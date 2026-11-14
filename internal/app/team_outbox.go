package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/team"
)

const (
	maxOutboxEntries = 64
	maxOutboxBytes   = 2 << 20
	maxOutboxText    = 1 << 20
	outboxBackoff    = 4 * time.Second
	outboxBackoffCap = 5 * time.Minute
)

// shareOutbox copies shared chats to their team servers. The poll loop
// calls kick. A tick that is still running makes the next kick return.
type shareOutbox struct {
	dataDir string
	now     func() time.Time
	client  *team.Client
	save    func(dir string, s team.Share) error
	readLog func(path string) []sessionlog.Event
	logSize func(path string) (int64, error)
	// files is the process lock for team-shares.json, shared with the routes.
	// A test that builds an outbox alone leaves it nil.
	files *sync.Mutex

	mu      sync.Mutex
	running bool
	hold    map[string]serverHold
	looks   map[string]logLook
}

// serverHold is one server's backoff and the last delivery word.
type serverHold struct {
	until     time.Time
	delay     time.Duration
	confirmed bool
	delivery  string
}

// logLook is the cursor of the last look whose size check succeeded.
type logLook struct {
	size      int64
	next      int
	startSent bool
	done      bool
}

type mirrorStart struct {
	V     int    `json:"v"`
	T     string `json:"t"`
	Scope string `json:"scope"`
	Time  string `json:"time"`
}

type mirrorText struct {
	V       int    `json:"v"`
	T       string `json:"t"`
	Time    string `json:"time"`
	Text    string `json:"text,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Prov    bool   `json:"prov,omitempty"`
	Omitted string `json:"omitted,omitempty"`
}

type mirrorSource struct {
	V      int    `json:"v"`
	T      string `json:"t"`
	Time   string `json:"time"`
	Reason string `json:"reason,omitempty"`
}

func (o *shareOutbox) kick() {
	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		return
	}
	o.running = true
	o.mu.Unlock()
	go o.run()
}

func (o *shareOutbox) run() {
	defer func() {
		o.mu.Lock()
		o.running = false
		o.mu.Unlock()
	}()
	o.tick(context.Background())
}

func (o *shareOutbox) tick(ctx context.Context) {
	entries, err := team.Load(o.dataDir)
	if err != nil {
		return
	}
	shares, err := team.LoadShares(o.dataDir)
	if err != nil {
		return
	}
	if len(shares) == 0 {
		return
	}
	inst, err := identity.LoadInstallation(o.dataDir)
	if err != nil {
		return
	}
	owner := string(inst)
	for _, share := range shares {
		o.one(ctx, entries, owner, share)
	}
}

func (o *shareOutbox) one(ctx context.Context, entries []team.Entry, owner string, share team.Share) {
	entry, ok := serverFor(entries, share.Server)
	if !ok {
		_ = team.RemoveShare(o.dataDir, share.UID)
		return
	}
	if o.waiting(entry.ID) {
		return
	}
	if share.State == "lost" || share.State == "full" {
		return
	}
	title, live := o.hasChat(share)
	if share.State == "stopping" || !live {
		o.stop(ctx, entry, owner, share)
		return
	}
	o.send(ctx, entry, owner, share, title)
}

func (o *shareOutbox) stop(ctx context.Context, entry team.Entry, owner string, share team.Share) {
	if !o.open(ctx, entry) {
		return
	}
	err := o.client.DeleteChannel(ctx, entry.URL, entry.Token, owner, share.UID)
	if err == nil || errors.Is(err, team.ErrNoChannel) || errors.Is(err, team.ErrUnauthorized) {
		_ = team.RemoveShare(o.dataDir, share.UID)
		o.noteSuccess(entry.ID)
		return
	}
	o.noteFailure(entry.ID, err)
}

func (o *shareOutbox) send(ctx context.Context, entry team.Entry, owner string, share team.Share, title string) {
	path := shareLogPath(o.dataDir, share.Node)
	size, sizeErr := o.fileSize(path)
	var events []sessionlog.Event
	if sizeErr != nil || !o.sameLook(share, size) {
		events = o.events(path)
	}
	fresh := channelTitle(title)
	batch, next, done := mirrorBatch(share, events)
	remember := func() {
		if sizeErr != nil {
			return
		}
		o.remember(share, size, done)
	}
	if len(batch) == 0 && fresh == share.Title {
		remember()
		return
	}
	if !o.open(ctx, entry) {
		return
	}
	if fresh != share.Title {
		gen, _, err := o.client.PutChannel(ctx, entry.URL, entry.Token, owner, share.UID, fresh)
		if err != nil {
			o.noteFailure(entry.ID, err)
			return
		}
		share.Title = fresh
		share.Generation = gen
		_ = o.persist(share)
	}
	if len(batch) == 0 {
		remember()
		o.noteSuccess(entry.ID)
		return
	}
	gen, _, err := o.client.AppendMirror(ctx, entry.URL, entry.Token, owner, share.UID, batch)
	if err != nil {
		if errors.Is(err, team.ErrNoChannel) {
			share.State = "lost"
			_ = o.persist(share)
			o.clearConfirmed(entry.ID)
			return
		}
		if errors.Is(err, team.ErrQuota) {
			share.State = "full"
			_ = o.persist(share)
			o.clearConfirmed(entry.ID)
			return
		}
		o.noteFailure(entry.ID, err)
		return
	}
	if gen != share.Generation {
		share.State = "lost"
		_ = o.persist(share)
		return
	}
	share.Next = next
	share.StartSent = true
	share.Generation = gen
	_ = o.persist(share)
	remember()
	o.noteSuccess(entry.ID)
}

func (o *shareOutbox) open(ctx context.Context, entry team.Entry) bool {
	if err := o.confirm(ctx, entry); err != nil {
		o.noteFailure(entry.ID, err)
		return false
	}
	return true
}

func (o *shareOutbox) confirm(ctx context.Context, entry team.Entry) error {
	if o.confirmed(entry.ID) {
		return nil
	}
	srv, err := o.client.Hello(ctx, entry.URL)
	if err != nil {
		return err
	}
	if srv.ID != entry.ID {
		return errHelloMismatch
	}
	o.update(entry.ID, func(h *serverHold) { h.confirmed = true })
	return nil
}

func (o *shareOutbox) hasChat(share team.Share) (string, bool) {
	title, found := readNodeTitle(o.dataDir, share.Node)
	if !found {
		return "", false
	}
	id := sessionlog.ReadLogIdentity(shareLogPath(o.dataDir, share.Node))
	if id.Status != sessionlog.IdentityOK || id.UID != share.UID {
		return "", false
	}
	return title, true
}

func readNodeTitle(dir, id string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "nodes.jsonl"))
	if err != nil {
		return "", false
	}
	title := ""
	found := false
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec storeRecord
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec.Type == "node" {
			if rec.Node == nil || rec.Node.ID != id {
				continue
			}
			title = rec.Node.Title
			found = true
			continue
		}
		if rec.Type == "delete" && rec.ID == id {
			title = ""
			found = false
		}
	}
	return title, found
}

func shareLogPath(dir, node string) string {
	return filepath.Join(dir, "sessions", node+".jsonl")
}

func serverFor(entries []team.Entry, id string) (team.Entry, bool) {
	for _, entry := range entries {
		if entry.ID == id {
			return entry, true
		}
	}
	return team.Entry{}, false
}

func (o *shareOutbox) fileSize(path string) (int64, error) {
	if o.logSize != nil {
		return o.logSize(path)
	}
	return defaultLogSize(path)
}

// defaultLogSize is the size check used when a test does not replace it.
func defaultLogSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func (o *shareOutbox) events(path string) []sessionlog.Event {
	if o.readLog != nil {
		return o.readLog(path)
	}
	return sessionlog.ReadEvents(path)
}

func (o *shareOutbox) persist(share team.Share) error {
	if o.save != nil {
		return o.save(o.dataDir, share)
	}
	return team.SaveShare(o.dataDir, share)
}

func mirrorBatch(share team.Share, events []sessionlog.Event) ([]team.MirrorEntry, int, bool) {
	batch := make([]team.MirrorEntry, 0)
	sum := 0
	if !share.StartSent {
		raw := rawJSON(mirrorStart{V: 1, T: "start", Scope: share.Scope, Time: share.SharedAt})
		batch = append(batch, team.MirrorEntry{Key: "start", Body: raw})
		sum += len(raw)
	}
	segment := 0
	agent := ""
	next := share.Next
	for i, ev := range events {
		if ev.T == "meta" && ev.Meta != nil && ev.Meta.Agent != "" {
			agent = ev.Meta.Agent
		}
		if i >= share.Start && i >= share.Next {
			body, ok := mirrorBody(ev, agent)
			if ok {
				if len(batch) > 0 && (len(batch) >= maxOutboxEntries || sum+len(body) > maxOutboxBytes) {
					return batch, i, false
				}
				batch = append(batch, team.MirrorEntry{Key: fmt.Sprintf("%d/%d", segment, i), Body: body})
				sum += len(body)
			}
			next = i + 1
		}
		if ev.T == "source" {
			segment++
		}
	}
	return batch, next, true
}

func mirrorBody(ev sessionlog.Event, agent string) (json.RawMessage, bool) {
	switch ev.T {
	case "user", "assistant":
		if strings.TrimSpace(ev.Text) == "" {
			return nil, false
		}
		if len(ev.Text) > maxOutboxText {
			return rawJSON(mirrorText{V: 1, T: ev.T, Time: ev.Time, Omitted: "too_large"}), true
		}
		body := mirrorText{V: 1, T: ev.T, Time: ev.Time, Text: ev.Text, Prov: len(ev.Prov) > 0}
		if ev.T == "assistant" {
			body.Agent = agent
		}
		return rawJSON(body), true
	case "source":
		body := mirrorSource{V: 1, T: "source", Time: ev.Time}
		if ev.Source != nil && ev.Source.Reason != "" {
			body.Reason = ev.Source.Reason
		}
		return rawJSON(body), true
	default:
		return nil, false
	}
}

func rawJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

func (o *shareOutbox) sameLook(share team.Share, size int64) bool {
	o.mu.Lock()
	prev, ok := o.looks[share.UID]
	o.mu.Unlock()
	if !ok || !prev.done {
		return false
	}
	return prev.size == size && prev.next == share.Next && prev.startSent == share.StartSent
}

func (o *shareOutbox) remember(share team.Share, size int64, done bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.looks == nil {
		o.looks = map[string]logLook{}
	}
	o.looks[share.UID] = logLook{size: size, next: share.Next, startSent: share.StartSent, done: done}
}

func (o *shareOutbox) waiting(id string) bool {
	o.mu.Lock()
	until := o.hold[id].until
	o.mu.Unlock()
	if until.IsZero() {
		return false
	}
	return o.now().Before(until)
}

func (o *shareOutbox) confirmed(id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hold[id].confirmed
}

func (o *shareOutbox) delivery(id string) string {
	o.mu.Lock()
	word := o.hold[id].delivery
	o.mu.Unlock()
	if word == "" {
		return "ok"
	}
	return word
}

func (o *shareOutbox) noteSuccess(id string) {
	o.update(id, func(h *serverHold) {
		h.confirmed = true
		h.delivery = "ok"
		h.delay = 0
		h.until = time.Time{}
	})
}

func (o *shareOutbox) noteFailure(id string, err error) {
	o.update(id, func(h *serverHold) {
		h.confirmed = false
		h.delivery = deliveryWord(err)
		if h.delay == 0 {
			h.delay = outboxBackoff
		} else {
			h.delay *= 2
		}
		if h.delay > outboxBackoffCap {
			h.delay = outboxBackoffCap
		}
		h.until = o.now().Add(h.delay)
	})
}

func (o *shareOutbox) clearConfirmed(id string) {
	o.update(id, func(h *serverHold) { h.confirmed = false })
}

func (o *shareOutbox) update(id string, fn func(*serverHold)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.hold == nil {
		o.hold = map[string]serverHold{}
	}
	h := o.hold[id]
	fn(&h)
	o.hold[id] = h
}

func deliveryWord(err error) string {
	switch {
	case errors.Is(err, team.ErrUnreachable):
		return "offline"
	case errors.Is(err, team.ErrUnauthorized):
		return "refused"
	default:
		return "changed"
	}
}

// copySharedChats is the poll loop's hook. A nil outbox does nothing.
func (a *app) copySharedChats() {
	if a.shareOutbox == nil {
		return
	}
	a.shareOutbox.kick()
}

// installShareOutbox builds the muxer's share copy and the delivery hook
// the share routes read. The handler constructor returns this concrete type.
func installShareOutbox(a *app, dataDir string, now func() time.Time) {
	client := newTeamClient()
	h := newTeamHandler(dataDir, now, client)
	box := &shareOutbox{dataDir: dataDir, now: now, client: client}
	h.(*teamHandler).shareDelivery = box.delivery
	a.teamHandler = h
	a.shareOutbox = box
}
