package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/scimux/scimux/internal/asset"
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
	// maxLogRecord is the largest record ReadEvents keeps. sessionlog does
	// not export that cap. A resumed read uses the same one.
	maxLogRecord = 8 << 20
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
	// cursor reads one node's session log for lane enroll. Nil uses chatCursor.
	cursor func(node string) (string, int, bool)
	// laneLook is the session-log size last parsed while enrolling a node
	// that has no share yet. A missing log is not a look.
	laneLook map[string]int64
	// readNodes reads nodes.jsonl. A test counts the calls. Nil reads the file.
	readNodes func(dir string) ([]byte, error)
	// readFrom reads a session log from an offset. A test records the offsets.
	// Nil reads the file from that offset.
	readFrom func(path string, offset int64) ([]byte, error)
	// readLogAt reads a session log from an offset through the end. A test
	// counts every read. Nil reads the file. readFrom, when set, replaces it.
	readLogAt func(path string, offset int64) ([]byte, error)
	// statLog is the size and identity check for a session log. Nil uses os.Stat.
	statLog func(path string) (os.FileInfo, error)
	// files is the process lock for team-shares.json, shared with the routes.
	// A test that builds an outbox alone leaves it nil.
	files    *sync.Mutex
	shareOps *sync.Mutex

	mu      sync.Mutex
	running bool
	hold    map[string]serverHold
	looks   map[string]logLook
	scanned map[string]logScan
}

// serverHold is one server's backoff and the last delivery word.
type serverHold struct {
	until     time.Time
	delay     time.Duration
	confirmed bool
	delivery  string
}

// logLook is the cursor of the last look whose size check succeeded.
// The byte fields are the last look that finished its batch. A partial
// batch keeps them so the next tick reads the same tail again.
type logLook struct {
	size      int64
	next      int
	startSent bool
	done      bool
	offset    int64
	records   int
	segment   int
	agent     string
	info      os.FileInfo
	haveInfo  bool
}

// logScan is one parse of a session log, kept until remember stores it.
type logScan struct {
	offset  int64
	records int
	segment int
	agent   string
	info    os.FileInfo
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
		lanes, _ := team.LoadLaneShares(o.dataDir)
		if len(lanes) == 0 {
			return
		}
	}
	inst, err := identity.LoadInstallation(o.dataDir)
	if err != nil {
		return
	}
	owner := string(inst)
	nodes, err := o.nodeLog()
	if err != nil {
		return
	}
	o.reconcileLanes(ctx, entries, owner, nodes)
	for _, share := range shares {
		o.one(ctx, entries, owner, share, nodes)
	}
}

func (o *shareOutbox) one(ctx context.Context, entries []team.Entry, owner string, share team.Share, nodes map[string]nodeSnap) {
	if o.shareOps != nil {
		o.shareOps.Lock()
		defer o.shareOps.Unlock()
		// The tick may have waited behind a share mutation after taking its
		// snapshot. Refresh under the gate before doing any network work.
		var current team.Share
		found := false
		err := o.withFiles(func() error {
			shares, err := team.LoadShares(o.dataDir)
			if err != nil {
				return err
			}
			current, found = shareByUID(shares, share.UID)
			return nil
		})
		if err != nil || !found || current.Generation != share.Generation || current.Server != share.Server || current.Node != share.Node {
			return
		}
		share = current
	}
	entry, ok := serverFor(entries, share.Server)
	if !ok {
		o.deleteStored(share.UID)
		return
	}
	if o.waiting(entry.ID) {
		return
	}
	if share.State == "lost" || share.State == "full" {
		return
	}
	title, live := o.hasChat(share, nodes)
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
	err := o.client.DeleteChannel(ctx, entry.URL, entry.Token, owner, share.UID, share.Generation)
	if err == nil || errors.Is(err, team.ErrNoChannel) || errors.Is(err, team.ErrStaleGeneration) || errors.Is(err, team.ErrUnauthorized) {
		o.deleteStoredGeneration(share)
		o.noteSuccess(entry.ID)
		return
	}
	o.noteFailure(entry.ID, err)
}

func (o *shareOutbox) send(ctx context.Context, entry team.Entry, owner string, share team.Share, title string) {
	born := share.Generation
	path := shareLogPath(o.dataDir, share.Node)
	o.dropScan(share.UID)
	var (
		events        []sessionlog.Event
		size          int64
		sizeErr       error
		base, segment int
		agent         string
		conflict      bool
	)
	if o.readLog != nil {
		size, sizeErr = o.fileSize(path)
		if sizeErr != nil || !o.sameLook(share, size) {
			events = o.readLog(path)
		}
	} else {
		events, size, sizeErr, base, segment, agent, conflict = o.grownLog(share, path)
		if conflict {
			// The log belongs to another chat. Stop the share and send nothing.
			o.stop(ctx, entry, owner, share)
			return
		}
	}
	fresh := channelTitle(title)
	batch, next, done := mirrorFrom(share, events, base, segment, agent)
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
		gen, created, err := o.client.PutChannel(ctx, entry.URL, entry.Token, owner, share.UID, fresh)
		if err != nil {
			o.noteFailure(entry.ID, err)
			return
		}
		if created || gen != born {
			if created {
				_ = o.client.DeleteChannel(ctx, entry.URL, entry.Token, owner, share.UID, gen)
			}
			share.State = "lost"
			_ = o.persist(share, born)
			o.clearConfirmed(entry.ID)
			return
		}
		share.Title = fresh
		share.Generation = gen
		_ = o.persist(share, born)
	}
	if len(batch) == 0 {
		remember()
		o.noteSuccess(entry.ID)
		return
	}
	gen, _, err := o.client.AppendMirror(ctx, entry.URL, entry.Token, owner, share.UID, share.Generation, batch)
	if err != nil {
		if errors.Is(err, team.ErrNoChannel) || errors.Is(err, team.ErrStaleGeneration) {
			share.State = "lost"
			_ = o.persist(share, born)
			o.clearConfirmed(entry.ID)
			return
		}
		if errors.Is(err, team.ErrQuota) {
			share.State = "full"
			_ = o.persist(share, born)
			o.clearConfirmed(entry.ID)
			return
		}
		o.noteFailure(entry.ID, err)
		return
	}
	if gen != share.Generation {
		share.State = "lost"
		_ = o.persist(share, born)
		return
	}
	share.Next = next
	share.StartSent = true
	share.Generation = gen
	_ = o.persist(share, born)
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

// hasChat reports whether the node is still live. It does not read the
// session log. grownLog checks the uid on the bytes it already reads.
func (o *shareOutbox) hasChat(share team.Share, nodes map[string]nodeSnap) (string, bool) {
	snap, ok := nodes[share.Node]
	if !ok || !snap.live {
		return "", false
	}
	return snap.title, true
}

func (o *shareOutbox) nodeLog() (map[string]nodeSnap, error) {
	raw, err := o.readNodeBytes()
	if err != nil {
		return nil, err
	}
	return parseNodeLog(raw), nil
}

func (o *shareOutbox) readNodeBytes() ([]byte, error) {
	if o.readNodes != nil {
		return o.readNodes(o.dataDir)
	}
	return os.ReadFile(filepath.Join(o.dataDir, "nodes.jsonl"))
}

// nodeSnap is the latest node record in nodes.jsonl.
type nodeSnap struct {
	title     string
	live      bool
	lane      string
	createdAt string
}

// parseNodeLog is the one parser for nodes.jsonl. A blank line, a line that
// is not JSON, and a node record with no node are skipped. A later node
// record restores a chat that a delete removed.
func parseNodeLog(raw []byte) map[string]nodeSnap {
	nodes := map[string]nodeSnap{}
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
			if rec.Node == nil {
				continue
			}
			nodes[rec.Node.ID] = nodeSnap{title: rec.Node.Title, live: true, lane: rec.Node.LaneID, createdAt: rec.Node.CreatedAt}
			continue
		}
		if rec.Type == "delete" {
			nodes[rec.ID] = nodeSnap{live: false}
		}
	}
	return nodes
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

// withFiles runs a read-change-write of team-shares.json under the lock
// shared with the routes. No lock is held while the caller talks to a server.
func (o *shareOutbox) withFiles(fn func() error) error {
	if o.files != nil {
		o.files.Lock()
		defer o.files.Unlock()
	}
	return fn()
}

// preserveOwnerFrontiers runs with the shared team-file lock held.
func (o *shareOutbox) preserveOwnerFrontiers(share team.Share) error {
	subs, err := team.LoadSubs(o.dataDir)
	if err != nil {
		return err
	}
	found := false
	for _, sub := range subs {
		if sub.Access == "owner" && sub.Server == share.Server && sub.Chat == share.UID && sub.Generation == share.Generation {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	inst, err := identity.LoadInstallation(o.dataDir)
	if err != nil {
		return err
	}
	helper := &teamHandler{dataDir: o.dataDir}
	changed := helper.captureOwnerFrontiers(subs, string(inst))
	for _, sub := range subs {
		if sub.Access == "owner" && sub.Owner == string(inst) && sub.Server == share.Server && sub.Chat == share.UID && sub.Generation == share.Generation && sub.OwnerFrontier == nil {
			return errors.New("owner frontier unavailable")
		}
	}
	if changed {
		return team.SaveSubs(o.dataDir, subs)
	}
	return nil
}

func (o *shareOutbox) deleteStored(uid string) {
	_ = o.withFiles(func() error {
		shares, err := team.LoadShares(o.dataDir)
		if err != nil {
			return err
		}
		for _, share := range shares {
			if share.UID == uid {
				if err := o.preserveOwnerFrontiers(share); err != nil {
					return err
				}
				break
			}
		}
		return team.RemoveShare(o.dataDir, uid)
	})
}

// deleteStoredGeneration removes only the share the remote delete meant.
func (o *shareOutbox) deleteStoredGeneration(share team.Share) {
	_ = o.withFiles(func() error {
		shares, err := team.LoadShares(o.dataDir)
		if err != nil {
			return err
		}
		for _, stored := range shares {
			if stored.UID == share.UID && stored.Generation == share.Generation {
				if err := o.preserveOwnerFrontiers(stored); err != nil {
					return err
				}
				return team.RemoveShare(o.dataDir, share.UID)
			}
		}
		return nil
	})
}

// persist writes the outbox fields onto the share stored now. born is the
// generation the tick loaded. A missing share, a share that is no longer
// active, or a different generation is left untouched. The outbox does not
// create a share.
func (o *shareOutbox) persist(share team.Share, born string) error {
	return o.withFiles(func() error {
		shares, err := team.LoadShares(o.dataDir)
		if err != nil {
			return err
		}
		for i := range shares {
			if shares[i].UID != share.UID {
				continue
			}
			stored := shares[i]
			if stored.State != "active" || stored.Generation != born {
				return nil
			}
			stored.Next = share.Next
			stored.StartSent = share.StartSent
			stored.Title = share.Title
			stored.Generation = share.Generation
			if share.State == "lost" || share.State == "full" {
				stored.State = share.State
			}
			if o.save != nil {
				return o.save(o.dataDir, stored)
			}
			return team.SaveShare(o.dataDir, stored)
		}
		return nil
	})
}

func mirrorBatch(share team.Share, events []sessionlog.Event) ([]team.MirrorEntry, int, bool) {
	return mirrorFrom(share, events, 0, 0, "")
}

func mirrorFrom(share team.Share, events []sessionlog.Event, base, segment int, agent string) ([]team.MirrorEntry, int, bool) {
	batch := make([]team.MirrorEntry, 0)
	sum := 0
	if !share.StartSent {
		raw := rawJSON(mirrorStart{V: 1, T: "start", Scope: share.Scope, Time: share.SharedAt})
		batch = append(batch, team.MirrorEntry{Key: "start", Body: raw})
		sum += len(raw)
	}
	next := share.Next
	for i, ev := range events {
		idx := base + i
		if ev.T == "meta" && ev.Meta != nil && ev.Meta.Agent != "" {
			agent = ev.Meta.Agent
		}
		if idx >= share.Start && idx >= share.Next {
			body, ok := mirrorBody(ev, agent)
			if ok {
				if len(batch) > 0 && (len(batch) >= maxOutboxEntries || sum+len(body) > maxOutboxBytes) {
					return batch, idx, false
				}
				batch = append(batch, team.MirrorEntry{Key: fmt.Sprintf("%d/%d", segment, idx), Body: body})
				sum += len(body)
			}
			next = idx + 1
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
		} else {
			// Delivered attachment markers name the owner's local files.
			body.Text = asset.ShareNames(ev.Text)
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
	prev := o.looks[share.UID]
	look := logLook{size: size, next: share.Next, startSent: share.StartSent, done: done}
	if scan, ok := o.scanned[share.UID]; ok && done {
		look.offset = scan.offset
		look.records = scan.records
		look.segment = scan.segment
		look.agent = scan.agent
		look.info = scan.info
		look.haveInfo = true
	} else {
		look.offset = prev.offset
		look.records = prev.records
		look.segment = prev.segment
		look.agent = prev.agent
		look.info = prev.info
		look.haveInfo = prev.haveInfo
	}
	o.looks[share.UID] = look
}

func (o *shareOutbox) dropScan(uid string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.scanned, uid)
}

func (o *shareOutbox) stash(uid string, scan logScan) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.scanned == nil {
		o.scanned = map[string]logScan{}
	}
	o.scanned[uid] = scan
}

// grownLog reads the new tail when the log only grew. A first look, a
// shorter file, and a replaced file are read from the start. The entries
// are numbered as ReadEvents numbers a full read. conflict means the log
// is not this share's chat: the caller stops the share and sends nothing.
func (o *shareOutbox) grownLog(share team.Share, path string) (events []sessionlog.Event, size int64, sizeErr error, base, segment int, agent string, conflict bool) {
	info, err := o.statPath(path)
	if err != nil {
		// Stat can fail while the file is still readable. A missing file
		// has no identity, so the share stops. A matching file is copied.
		if !readIdentityMatches(path, share.UID) {
			return nil, 0, err, 0, 0, "", true
		}
		return sessionlog.ReadEvents(path), 0, err, 0, 0, "", false
	}
	size = info.Size()
	o.mu.Lock()
	prev, ok := o.looks[share.UID]
	o.mu.Unlock()
	start := int64(0)
	can := ok && prev.haveInfo && os.SameFile(prev.info, info) && size >= prev.size
	if can && size == prev.size && prev.done && prev.next == share.Next && prev.startSent == share.StartSent {
		return nil, size, nil, 0, 0, "", false
	}
	// A dropped save leaves the stored share behind the next this look
	// saved. Read the whole log again so the record numbers stay the ones
	// ReadEvents uses. A record that sends nothing, such as a tool record,
	// does not move that next, and the following tick still reads the tail.
	if can && share.StartSent == prev.startSent && share.Next == prev.next {
		start = prev.offset
		base = prev.records
		segment = prev.segment
		agent = prev.agent
	}
	raw, err := o.readRange(path, start, size)
	if err != nil {
		return nil, size, nil, 0, 0, "", false
	}
	parsed, watermark, records, seg, ag := parseLogTail(bytes.NewReader(raw), base, segment, agent)
	if start == 0 {
		if !logIdentityOK(parsed, share.UID) {
			return nil, size, nil, 0, 0, "", true
		}
	} else if tailHasOtherUID(parsed, share.UID) {
		return nil, size, nil, 0, 0, "", true
	}
	o.stash(share.UID, logScan{
		offset:  start + watermark,
		records: records,
		segment: seg,
		agent:   ag,
		info:    info,
	})
	return parsed, size, nil, base, segment, agent, false
}

func (o *shareOutbox) statPath(path string) (os.FileInfo, error) {
	if o.statLog != nil {
		return o.statLog(path)
	}
	return os.Stat(path)
}

func (o *shareOutbox) readRange(path string, offset, size int64) ([]byte, error) {
	if o.readFrom != nil {
		return o.readFrom(path, offset)
	}
	if o.readLogAt != nil {
		return o.readLogAt(path, offset)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.NewSectionReader(f, offset, size-offset))
}

// readIdentityMatches is ReadLogIdentity for a stat that failed. The file
// may still be readable, and a missing file is not this share's chat.
func readIdentityMatches(path, uid string) bool {
	id := sessionlog.ReadLogIdentity(path)
	if id.Status != sessionlog.IdentityOK {
		return false
	}
	return id.UID == uid
}

// metaUID is a meta record's non-empty uid. Anything else is not a header,
// which is the rule ReadLogIdentity uses.
func metaUID(ev sessionlog.Event) (string, bool) {
	if ev.T != "meta" || ev.Meta == nil || ev.Meta.UID == "" {
		return "", false
	}
	return ev.Meta.UID, true
}

// logIdentityOK reports whether a whole log of parsed records is this
// share's chat. The first non-empty meta uid wins. A later different uid
// is a conflict. A log with no header is not the chat.
func logIdentityOK(events []sessionlog.Event, uid string) bool {
	seen := ""
	have := false
	for _, ev := range events {
		id, ok := metaUID(ev)
		if !ok {
			continue
		}
		if !have {
			seen, have = id, true
			continue
		}
		if id != seen {
			return false
		}
	}
	if !have || seen != uid {
		return false
	}
	return true
}

// tailHasOtherUID reports a header in the grown part whose uid is not the
// share's. The bytes before that part were already accepted.
func tailHasOtherUID(events []sessionlog.Event, uid string) bool {
	for _, ev := range events {
		id, ok := metaUID(ev)
		if !ok {
			continue
		}
		if id != uid {
			return true
		}
	}
	return false
}

// parseLogTail parses one slice of a session log. base, segment, and agent
// are the cursor from the bytes already consumed. records counts the same
// lines ReadEvents counts.
func parseLogTail(r io.Reader, base, segment int, agent string) (events []sessionlog.Event, watermark int64, records, seg int, ag string) {
	records = base
	seg = segment
	ag = agent
	watermark = scanLogLines(r, func(rec []byte) {
		var ev sessionlog.Event
		if json.Unmarshal(rec, &ev) != nil {
			return
		}
		events = append(events, ev)
		if ev.T == "meta" && ev.Meta != nil && ev.Meta.Agent != "" {
			ag = ev.Meta.Agent
		}
		if ev.T == "source" {
			seg++
		}
		records++
	})
	return events, watermark, records, seg, ag
}

// scanLogLines walks complete newline-terminated records. Blank lines and
// records longer than maxLogRecord are skipped. A line with no newline yet
// stays unread. The returned offset is the byte after the last complete
// line, never inside an unfinished line.
func scanLogLines(r io.Reader, fn func(rec []byte)) int64 {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		off     int64
		pend    int64
		carry   []byte
		toolong bool
	)
	for {
		chunk, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// Count these bytes only when the line's newline arrives.
			pend += int64(len(chunk))
			if !toolong {
				if len(carry)+len(chunk) > maxLogRecord {
					toolong, carry = true, nil
				} else {
					carry = append(carry, chunk...)
				}
			}
			continue
		}
		if err != nil {
			// EOF, or a read error. ReadEvents drops the error too, and a
			// partial line is not a record. pend is the unfinished line.
			return off
		}
		off += pend + int64(len(chunk))
		pend = 0
		if !toolong {
			rec := chunk[:len(chunk)-1]
			if len(carry) > 0 {
				rec = append(carry, rec...)
			}
			if len(rec) > 0 && rec[len(rec)-1] == '\r' {
				rec = rec[:len(rec)-1]
			}
			if len(bytes.TrimSpace(rec)) > 0 {
				fn(rec)
			}
		}
		carry, toolong = carry[:0], false
	}
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
	_, box := a.teamComponents()
	if box == nil {
		return
	}
	box.kick()
}

// teamComponents returns the muxer's installed sharing components.
func (a *app) teamComponents() (http.Handler, *shareOutbox) {
	return a.teamHandler, a.shareOutbox
}

// installShareOutbox builds the muxer's share copy and the delivery hook
// the share routes read. The handler constructor returns this concrete type.
func installShareOutbox(a *app, dataDir string, now func() time.Time) {
	client := newTeamClient()
	h := newTeamHandler(dataDir, now, client)
	th := h.(*teamHandler)
	th.ownerInstance = func(nodeID, instance string) bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		n := a.byID[nodeID]
		return n != nil && n.InstanceID != "" && n.InstanceID == instance && n.EndedAt == ""
	}
	box := &shareOutbox{dataDir: dataDir, now: now, client: client, files: &th.mu, shareOps: &th.shareOps}
	th.kickShares = box.kick
	th.shareDelivery = box.delivery
	a.teamHandler = h
	a.shareOutbox = box
}
