package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/storagebudget"
	"github.com/scimux/scimux/internal/team"
)

const (
	subListCap    = 4096
	subAskWindow  = 10 * time.Second
	subSeatsEvery = 10 * time.Minute
)

// subAsk is one server's refresh clock in this process.
type subAsk struct {
	channels  time.Time
	seats     time.Time
	delivery  string
	truncated bool
}

type subOutcome struct {
	entry    team.Entry
	delivery string
	channels []team.Channel
	err      error
}

type seatOutcome struct {
	id    string
	entry team.Entry
	seats []team.Seat
	err   error
}

type subKey struct {
	owner, chat, gen string
}

type subscriptionView struct {
	Server     string `json:"server"`
	ServerName string `json:"server_name"`
	Owner      string `json:"owner"`
	OwnerName  string `json:"owner_name"`
	Chat       string `json:"chat"`
	Generation string `json:"generation"`
	Title      string `json:"title"`
	Access     string `json:"access"`
	State      string `json:"state"`
	MirrorLast uint64 `json:"mirror_last"`
	SeenMirror uint64 `json:"seen_mirror"`
	Unread     bool   `json:"unread"`
	FirstSeen  string `json:"first_seen"`
}

type subscriptionServerView struct {
	ID       string `json:"id"`
	Delivery string `json:"delivery"`
}

type subListResponse struct {
	Subscriptions []subscriptionView       `json:"subscriptions"`
	Servers       []subscriptionServerView `json:"servers"`
	Truncated     bool                     `json:"truncated"`
}

// serveSubscriptions handles /api/team/subscriptions and paths below it.
func (h *teamHandler) serveSubscriptions(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/team/subscriptions" && r.Method == http.MethodGet {
		h.listSubscriptions(w, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/team/subscriptions/")
	parts := strings.Split(rest, "/")
	if !ok || (len(parts) != 4 && (len(parts) != 5 || parts[4] != "seen")) || !subHex(parts[0], 32) || !subHex(parts[1], 32) || !subHex(parts[2], 16) || !subHex(parts[3], 16) {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	id := team.CopyID{Server: parts[0], Owner: parts[1], Chat: parts[2], Generation: parts[3]}
	if len(parts) == 4 {
		if r.Method == http.MethodDelete {
			h.removeSubscription(w, r, id)
			return
		}
	}
	if len(parts) == 4 && r.Method == http.MethodGet {
		h.readSubscription(w, r, id)
		return
	}
	if len(parts) == 5 && r.Method == http.MethodPut {
		h.seeSubscription(w, r, id)
		return
	}
	writeTeamError(w, http.StatusNotFound, "not_found")
}

func subHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func (h *teamHandler) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	inst, err := identity.LoadInstallation(h.dataDir)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "identity_unavailable")
		return
	}
	entries, err := h.readServers()
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	if _, err := h.readSubs(); err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	now := h.now().UTC()
	due := h.dueServers(entries, now)
	outcomes := make([]subOutcome, 0, len(due))
	for _, entry := range due {
		outcomes = append(outcomes, h.askChannels(r.Context(), entry))
	}
	seatDue := h.planSeats(string(inst), outcomes, now)
	seats := make([]seatOutcome, 0, len(seatDue))
	for _, entry := range seatDue {
		seats = append(seats, h.askSeats(r.Context(), entry))
	}
	view, err := h.commitSubs(string(inst), outcomes, seats, now)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	writeJSONStatus(w, http.StatusOK, view)
}

func (h *teamHandler) readSubs() ([]team.Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return team.LoadSubs(h.dataDir)
}

func (h *teamHandler) dueServers(entries []team.Entry, now time.Time) []team.Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subAsks == nil {
		h.subAsks = map[string]subAsk{}
	}
	due := make([]team.Entry, 0, len(entries))
	for _, entry := range entries {
		ask := h.subAsks[entry.ID]
		if !ask.channels.IsZero() && now.Sub(ask.channels) < subAskWindow {
			continue
		}
		ask.channels = now
		h.subAsks[entry.ID] = ask
		due = append(due, entry)
	}
	return due
}

func (h *teamHandler) askChannels(ctx context.Context, entry team.Entry) subOutcome {
	out := subOutcome{entry: entry}
	if err := h.confirmed(ctx, entry); err != nil {
		out.err = err
		if errors.Is(err, team.ErrUnreachable) {
			out.delivery = "offline"
			return out
		}
		out.delivery = "changed"
		return out
	}
	channels, err := h.client.Channels(ctx, entry.URL, entry.Token, true)
	if err != nil {
		out.err = err
		switch {
		case errors.Is(err, team.ErrUnauthorized):
			out.delivery = "refused"
		case errors.Is(err, team.ErrUnreachable):
			out.delivery = "offline"
		default:
			out.delivery = "changed"
		}
		return out
	}
	out.delivery = "ok"
	out.channels = channels
	return out
}

func (h *teamHandler) askSeats(ctx context.Context, entry team.Entry) seatOutcome {
	seats, err := h.client.Seats(ctx, entry.URL, entry.Token)
	return seatOutcome{id: entry.ID, entry: entry, seats: seats, err: err}
}

func (h *teamHandler) planSeats(inst string, outcomes []subOutcome, now time.Time) []team.Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	entries, subs, err := h.loadSubState()
	if err != nil {
		return nil
	}
	known := make(map[team.CopyID]bool, len(subs))
	for _, sub := range subs {
		known[team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}] = true
	}
	subs, _, _ = mergeSubs(subs, entries, outcomes, inst, now)
	joined := entryMap(entries)
	due := []team.Entry{}
	for _, outcome := range outcomes {
		if outcome.delivery != "ok" {
			continue
		}
		if _, ok := joined[outcome.entry.ID]; !ok {
			continue
		}
		newUnnamed := false
		for _, sub := range subs {
			if sub.Server == outcome.entry.ID && sub.OwnerName == "" && !known[team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}] {
				newUnnamed = true
				break
			}
		}
		if newUnnamed || needsSeats(subs, outcome.entry.ID, h.subAsks[outcome.entry.ID], now) {
			h.rememberSeats(outcome.entry.ID, now)
			due = append(due, outcome.entry)
		}
	}
	return due
}

func (h *teamHandler) commitSubs(inst string, outcomes []subOutcome, seats []seatOutcome, now time.Time) (subListResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entries, subs, err := h.loadSubState()
	if err != nil {
		return subListResponse{}, err
	}
	subs, truncation, changed := mergeSubs(subs, entries, outcomes, inst, now)
	joined := entryMap(entries)
	for _, outcome := range outcomes {
		if _, ok := joined[outcome.entry.ID]; !ok {
			continue
		}
		h.remember(outcome.entry.ID, outcome.delivery)
		if value, ok := truncation[outcome.entry.ID]; ok {
			ask := h.subAsks[outcome.entry.ID]
			ask.truncated = value
			h.subAsks[outcome.entry.ID] = ask
		}
	}
	for _, seat := range seats {
		if _, ok := joined[seat.id]; !ok {
			continue
		}
		if errors.Is(seat.err, team.ErrUnauthorized) {
			if endServer(subs, seat.id) {
				changed = true
			}
			h.remember(seat.id, "refused")
			continue
		}
		if seat.err != nil {
			continue
		}
		if applySeatNames(subs, seat.id, seat.seats) {
			changed = true
		}
	}
	if changed {
		save := h.saveSubs
		if save == nil {
			save = team.SaveSubs
		}
		if err := save(h.dataDir, subs); err != nil {
			return subListResponse{}, err
		}
	}
	truncated := false
	for _, entry := range entries {
		truncated = truncated || h.subAsks[entry.ID].truncated
	}
	return h.subscriptionView(subs, entries, truncated), nil
}

func (h *teamHandler) loadSubState() ([]team.Entry, []team.Subscription, error) {
	entries, err := team.Load(h.dataDir)
	if err != nil {
		return nil, nil, err
	}
	subs, err := team.LoadSubs(h.dataDir)
	if err != nil {
		return nil, nil, err
	}
	return entries, subs, nil
}

func (h *teamHandler) remember(id, delivery string) {
	ask := h.subAsks[id]
	ask.delivery = delivery
	h.subAsks[id] = ask
}

func (h *teamHandler) rememberSeats(id string, now time.Time) {
	ask := h.subAsks[id]
	ask.seats = now
	h.subAsks[id] = ask
}

func (h *teamHandler) deliveryWord(id string) string {
	ask, ok := h.subAsks[id]
	if !ok || ask.delivery == "" {
		return "ok"
	}
	return ask.delivery
}

func needsSeats(subs []team.Subscription, server string, ask subAsk, now time.Time) bool {
	if !ask.seats.IsZero() && now.Sub(ask.seats) < subSeatsEvery {
		return false
	}
	for _, sub := range subs {
		if sub.Server == server {
			return true
		}
	}
	return false
}

func mergeSubs(subs []team.Subscription, entries []team.Entry, outcomes []subOutcome, inst string, now time.Time) ([]team.Subscription, map[string]bool, bool) {
	joined := entryMap(entries)
	changed := false
	for i := range subs {
		if _, ok := joined[subs[i].Server]; ok {
			continue
		}
		if markEnded(&subs[i]) {
			changed = true
		}
	}
	truncation := make(map[string]bool)
	for _, outcome := range outcomes {
		if _, ok := joined[outcome.entry.ID]; !ok {
			continue
		}
		if outcome.delivery == "refused" {
			if endServer(subs, outcome.entry.ID) {
				changed = true
			}
			continue
		}
		if outcome.delivery != "ok" {
			continue
		}
		var grew bool
		var truncated bool
		subs, grew, truncated = applyChannels(subs, outcome, inst, now, false)
		truncation[outcome.entry.ID] = truncated
		if grew {
			changed = true
		}
	}
	return subs, truncation, changed
}

func applyChannels(subs []team.Subscription, outcome subOutcome, inst string, now time.Time, truncated bool) ([]team.Subscription, bool, bool) {
	listed := map[subKey]team.Channel{}
	order := make([]subKey, 0, len(outcome.channels))
	for _, channel := range outcome.channels {
		if channel.Owner == inst || (channel.Access != "read" && channel.Access != "write") {
			continue
		}
		key := subKey{owner: channel.Owner, chat: channel.Chat, gen: channel.Generation}
		listed[key] = channel
		order = append(order, key)
	}
	changed := false
	id := outcome.entry.ID
	for _, key := range order {
		for i := range subs {
			if subs[i].Server != id || subs[i].Owner != key.owner || subs[i].Chat != key.chat || subs[i].Generation == key.gen {
				continue
			}
			if markEnded(&subs[i]) {
				changed = true
			}
		}
	}
	for _, key := range order {
		channel := listed[key]
		idx := subIndex(subs, id, key)
		if idx >= 0 {
			if subs[idx].State != "live" || subs[idx].Title != channel.Title || subs[idx].Access != channel.Access || subs[idx].MirrorLast != channel.MirrorLast {
				subs[idx].State = "live"
				subs[idx].Title = channel.Title
				subs[idx].Access = channel.Access
				subs[idx].MirrorLast = channel.MirrorLast
				changed = true
			}
			continue
		}
		if len(subs) >= subListCap {
			truncated = true
			continue
		}
		ownerName := ""
		for _, prior := range subs {
			if prior.Server == id && prior.Owner == channel.Owner && prior.OwnerName != "" {
				ownerName = prior.OwnerName
				break
			}
		}
		subs = append(subs, team.Subscription{
			Server: id, Owner: channel.Owner, Chat: channel.Chat, Generation: channel.Generation,
			ServerName: outcome.entry.Name, OwnerName: ownerName, Title: channel.Title, Access: channel.Access,
			State: "live", MirrorLast: channel.MirrorLast, SeenMirror: 0,
			FirstSeen: now.Format(time.RFC3339),
		})
		changed = true
	}
	for i := range subs {
		if subs[i].Server != id || subs[i].State != "live" {
			continue
		}
		if _, ok := listed[subKey{subs[i].Owner, subs[i].Chat, subs[i].Generation}]; ok {
			continue
		}
		if markEnded(&subs[i]) {
			changed = true
		}
	}
	return subs, changed, truncated
}

func applySeatNames(subs []team.Subscription, server string, seats []team.Seat) bool {
	names := make(map[string]string, len(seats))
	for _, seat := range seats {
		names[seat.ID] = seat.Name
	}
	changed := false
	for i := range subs {
		if subs[i].Server != server {
			continue
		}
		name, ok := names[subs[i].Owner]
		if !ok || subs[i].OwnerName == name {
			continue
		}
		subs[i].OwnerName = name
		changed = true
	}
	return changed
}

func endServer(subs []team.Subscription, id string) bool {
	changed := false
	for i := range subs {
		if subs[i].Server == id && markEnded(&subs[i]) {
			changed = true
		}
	}
	return changed
}

func markEnded(sub *team.Subscription) bool {
	if sub.State == "ended" {
		return false
	}
	sub.State = "ended"
	return true
}

func subIndex(subs []team.Subscription, server string, key subKey) int {
	for i := range subs {
		if subs[i].Server == server && subs[i].Owner == key.owner && subs[i].Chat == key.chat && subs[i].Generation == key.gen {
			return i
		}
	}
	return -1
}

func entryMap(entries []team.Entry) map[string]team.Entry {
	out := make(map[string]team.Entry, len(entries))
	for _, entry := range entries {
		out[entry.ID] = entry
	}
	return out
}

func (h *teamHandler) subscriptionView(subs []team.Subscription, entries []team.Entry, truncated bool) subListResponse {
	views := make([]subscriptionView, 0, len(subs))
	for _, sub := range subs {
		views = append(views, subscriptionView{
			Server: sub.Server, ServerName: sub.ServerName, Owner: sub.Owner, OwnerName: sub.OwnerName,
			Chat: sub.Chat, Generation: sub.Generation, Title: sub.Title, Access: sub.Access, State: sub.State,
			MirrorLast: sub.MirrorLast, SeenMirror: sub.SeenMirror, Unread: sub.State == "live" && sub.MirrorLast > sub.SeenMirror,
			FirstSeen: sub.FirstSeen,
		})
	}
	sort.Slice(views, func(i, j int) bool { return subLess(views[i], views[j]) })
	servers := make([]subscriptionServerView, 0, len(entries))
	for _, entry := range entries {
		servers = append(servers, subscriptionServerView{ID: entry.ID, Delivery: h.deliveryWord(entry.ID)})
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].ID < servers[j].ID })
	return subListResponse{Subscriptions: views, Servers: servers, Truncated: truncated}
}

func subLess(a, b subscriptionView) bool {
	if a.Server != b.Server {
		return a.Server < b.Server
	}
	if a.Owner != b.Owner {
		return a.Owner < b.Owner
	}
	if a.Chat != b.Chat {
		return a.Chat < b.Chat
	}
	return a.FirstSeen < b.FirstSeen
}

type subscriptionReadResponse struct {
	View struct {
		subscriptionView
		Delivery string `json:"delivery"`
	} `json:"view"`
	Entries []map[string]any `json:"entries"`
	Last    uint64           `json:"last"`
	Next    uint64           `json:"next"`
	More    bool             `json:"more"`
}

func sameCopy(s team.Subscription, id team.CopyID) bool {
	return s.Server == id.Server && s.Owner == id.Owner && s.Chat == id.Chat && s.Generation == id.Generation
}

func (h *teamHandler) lookupSubscription(id team.CopyID) (team.Subscription, team.Entry, bool, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entries, subs, err := h.loadSubState()
	if err != nil {
		return team.Subscription{}, team.Entry{}, false, false, err
	}
	for _, s := range subs {
		if sameCopy(s, id) {
			for _, e := range entries {
				if e.ID == id.Server {
					return s, e, true, true, nil
				}
			}
			return s, team.Entry{}, true, false, nil
		}
	}
	return team.Subscription{}, team.Entry{}, false, false, nil
}

func (h *teamHandler) endSubscription(id team.CopyID, whole bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	subs, err := team.LoadSubs(h.dataDir)
	if err != nil {
		return err
	}
	changed := false
	for i := range subs {
		if (whole && subs[i].Server == id.Server) || (!whole && sameCopy(subs[i], id)) {
			if markEnded(&subs[i]) {
				changed = true
			}
		}
	}
	if changed {
		return h.saveSubscriptionState(subs)
	}
	return nil
}

func (h *teamHandler) readSubscription(w http.ResponseWriter, r *http.Request, id team.CopyID) {
	var after uint64
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeTeamError(w, 400, "bad_request")
		return
	}
	vals, exists := query["after"]
	if exists {
		if len(vals) != 1 || vals[0] == "" {
			writeTeamError(w, 400, "bad_request")
			return
		}
		var err error
		after, err = strconv.ParseUint(vals[0], 10, 64)
		if err != nil {
			writeTeamError(w, 400, "bad_request")
			return
		}
	}
	sub, server, found, joined, err := h.lookupSubscription(id)
	if err != nil {
		writeTeamError(w, 500, "store_unavailable")
		return
	}
	if !found {
		writeTeamError(w, 404, "not_found")
		return
	}
	_, last, err := team.ReadCopy(h.dataDir, id, ^uint64(0), 256, 4<<20)
	if err != nil {
		writeTeamError(w, 500, "copy_damaged")
		return
	}
	delivery := "ok"
	stopped := false
	full := false
	if sub.State == "live" && joined {
		err = h.confirmed(r.Context(), server)
		if err == nil {
			for pageNo := 0; pageNo < 8; pageNo++ {
				var page team.MirrorPage
				page, err = h.client.ReadMirror(r.Context(), server.URL, server.Token, id.Owner, id.Chat, last)
				if err != nil {
					break
				}
				if page.Generation != id.Generation {
					err = h.endSubscription(id, false)
					if err != nil {
						writeTeamError(w, 500, "store_unavailable")
						return
					}
					sub.State = "ended"
					break
				}
				err = team.AppendCopy(h.dataDir, id, page.Entries)
				if err != nil {
					break
				}
				if len(page.Entries) > 0 {
					last = page.Entries[len(page.Entries)-1].Seq
				}
				if !page.More {
					break
				}
				if pageNo == 7 {
					stopped = true
				}
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, team.ErrNoChannel):
			err = h.endSubscription(id, false)
			sub.State = "ended"
		case errors.Is(err, team.ErrUnauthorized):
			err = h.endSubscription(id, true)
			sub.State = "ended"
			delivery = "refused"
		case errors.Is(err, team.ErrUnreachable):
			delivery = "offline"
			err = nil
		case errors.Is(err, storagebudget.ErrBudget):
			full = true
			err = nil
		case errors.Is(err, team.ErrCopyDamaged):
			writeTeamError(w, 500, "copy_damaged")
			return
		case errors.Is(err, team.ErrCopyRange), errors.Is(err, team.ErrProtocol):
			delivery = "changed"
			err = nil
		default:
			delivery = "changed"
			err = nil
		}
		if err != nil {
			writeTeamError(w, 500, "store_unavailable")
			return
		}
	}
	records, last, err := team.ReadCopy(h.dataDir, id, after, 256, 4<<20)
	if err != nil {
		writeTeamError(w, 500, "copy_damaged")
		return
	}
	var out subscriptionReadResponse
	out.View.subscriptionView = subscriptionView{Server: sub.Server, ServerName: sub.ServerName, Owner: sub.Owner, OwnerName: sub.OwnerName, Chat: sub.Chat, Generation: sub.Generation, Title: sub.Title, Access: sub.Access, State: sub.State, MirrorLast: sub.MirrorLast, SeenMirror: sub.SeenMirror, Unread: sub.State == "live" && sub.MirrorLast > sub.SeenMirror, FirstSeen: sub.FirstSeen}
	out.View.Delivery = delivery
	out.Last = last
	out.Entries = []map[string]any{}
	textBytes := 0
	out.Next = after
	if out.Next > last {
		out.Next = last
	}
	for _, record := range records {
		entry, size, ok := projectSubRecord(record)
		if !ok {
			out.Next = record.Seq
			continue
		}
		if textBytes+size > 4<<20 {
			out.More = true
			break
		}
		out.Entries = append(out.Entries, entry)
		out.Next = record.Seq
		textBytes += size
	}
	out.More = stopped || out.Next < last
	if full && len(records) == 0 {
		writeTeamError(w, 507, "storage_full")
		return
	}
	writeJSONStatus(w, 200, out)
}

func projectSubRecord(record team.MirrorRecord) (map[string]any, int, bool) {
	var body map[string]json.RawMessage
	if json.Unmarshal(record.Body, &body) != nil || body == nil {
		return nil, 0, false
	}
	var version int
	var kind string
	if json.Unmarshal(body["v"], &version) != nil || version != 1 || json.Unmarshal(body["t"], &kind) != nil {
		return nil, 0, false
	}
	var fields []string
	switch kind {
	case "start":
		fields = []string{"scope", "time"}
	case "user":
		fields = []string{"time", "text", "prov", "omitted"}
	case "assistant":
		fields = []string{"time", "text", "agent", "prov", "omitted"}
	case "source":
		fields = []string{"time", "reason"}
	default:
		return nil, 0, false
	}
	out := map[string]any{"seq": record.Seq, "at": record.At, "t": kind}
	size := 0
	for _, field := range fields {
		raw, ok := body[field]
		if !ok {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, 0, false
		}
		if field == "prov" {
			var b bool
			if json.Unmarshal(raw, &b) != nil {
				return nil, 0, false
			}
			out[field] = b
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, 0, false
		}
		out[field] = s
		if field == "text" {
			size += len(s)
		}
	}
	return out, size, true
}

func (h *teamHandler) seeSubscription(w http.ResponseWriter, r *http.Request, id team.CopyID) {
	var req struct {
		Mirror *uint64 `json:"mirror"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || req.Mirror == nil {
		writeTeamError(w, 400, "bad_request")
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		writeTeamError(w, 400, "bad_request")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	subs, err := team.LoadSubs(h.dataDir)
	if err != nil {
		writeTeamError(w, 500, "store_unavailable")
		return
	}
	for i := range subs {
		if sameCopy(subs[i], id) {
			_, last, err := team.ReadCopy(h.dataDir, id, ^uint64(0), 256, 4<<20)
			if err != nil {
				writeTeamError(w, 500, "store_unavailable")
				return
			}
			value := *req.Mirror
			if value > last {
				value = last
			}
			if value > subs[i].SeenMirror {
				subs[i].SeenMirror = value
				if h.saveSubscriptionState(subs) != nil {
					writeTeamError(w, 500, "store_unavailable")
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	writeTeamError(w, 404, "not_found")
}

func (h *teamHandler) saveSubscriptionState(subs []team.Subscription) error {
	if h.saveSubs != nil {
		return h.saveSubs(h.dataDir, subs)
	}
	return team.SaveSubs(h.dataDir, subs)
}

// removeSubscriptionCopy is a filesystem seam for removal tests.
var removeSubscriptionCopy = team.RemoveCopy

func (h *teamHandler) removeSubscription(w http.ResponseWriter, r *http.Request, id team.CopyID) {
	writeTeamError(w, http.StatusNotFound, "not_found")
}
