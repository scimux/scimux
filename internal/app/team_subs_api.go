package app

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/scimux/scimux/internal/identity"
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
	if r.URL.Path != "/api/team/subscriptions" {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != http.MethodGet {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	h.listSubscriptions(w, r)
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
		if needsSeats(subs, outcome.entry.ID, h.subAsks[outcome.entry.ID], now) {
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
		h.rememberSeats(seat.id, now)
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
	has := false
	empty := false
	for _, sub := range subs {
		if sub.Server != server {
			continue
		}
		has = true
		if sub.OwnerName == "" {
			empty = true
		}
	}
	if !has {
		return false
	}
	if empty {
		return true
	}
	return ask.seats.IsZero() || now.Sub(ask.seats) >= subSeatsEvery
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
			if subs[idx].State != "live" {
				continue
			}
			if subs[idx].Title != channel.Title || subs[idx].Access != channel.Access || subs[idx].MirrorLast != channel.MirrorLast {
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
		subs = append(subs, team.Subscription{
			Server: id, Owner: channel.Owner, Chat: channel.Chat, Generation: channel.Generation,
			ServerName: outcome.entry.Name, OwnerName: "", Title: channel.Title, Access: channel.Access,
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
			MirrorLast: sub.MirrorLast, SeenMirror: sub.SeenMirror, Unread: sub.MirrorLast > sub.SeenMirror,
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
