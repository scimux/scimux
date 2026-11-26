package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/sessionlog"
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
	channels      time.Time
	seats         time.Time
	roster        time.Time
	rosterBinding string
	names         map[string]string
	unnamed       map[string]struct{}
	delivery      string
	truncated     bool
	peopleFiles   string
	peopleBinding string
	peopleStarted time.Time
	peopleProbe   uint64
}

type subOutcome struct {
	entry       team.Entry
	started     time.Time
	delivery    string
	channels    []team.Channel
	err         error
	peopleFiles string
	peopleProbe uint64
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
	Server             string              `json:"server"`
	ServerName         string              `json:"server_name"`
	Owner              string              `json:"owner"`
	OwnerName          string              `json:"owner_name"`
	Chat               string              `json:"chat"`
	Generation         string              `json:"generation"`
	Title              string              `json:"title"`
	Access             string              `json:"access"`
	State              string              `json:"state"`
	MirrorLast         uint64              `json:"mirror_last"`
	SeenMirror         uint64              `json:"seen_mirror"`
	CommentsLast       uint64              `json:"comments_last"`
	SeenComments       uint64              `json:"seen_comments"`
	Unread             bool                `json:"unread"`
	FirstSeen          string              `json:"first_seen"`
	LocalNode          string              `json:"local_node,omitempty"`
	LocalOrder         *int                `json:"local_order,omitempty"`
	LocalFrontier      *team.OwnerFrontier `json:"local_frontier,omitempty"`
	PeopleCopyInstance string              `json:"people_copy_instance,omitempty"`
}

type subscriptionServerView struct {
	ID                string `json:"id"`
	Delivery          string `json:"delivery"`
	PeopleFiles       string `json:"people_files,omitempty"`
	PeopleFileBinding string `json:"people_file_binding,omitempty"`
}

type peopleAttachmentCapabilities struct {
	Version       int `json:"version"`
	MaxFileBytes  int `json:"max_file_bytes"`
	MaxFiles      int `json:"max_files"`
	MaxTotalBytes int `json:"max_total_bytes"`
}

type subListResponse struct {
	Subscriptions     []subscriptionView            `json:"subscriptions"`
	Servers           []subscriptionServerView      `json:"servers"`
	Truncated         bool                          `json:"truncated"`
	PeopleAttachments *peopleAttachmentCapabilities `json:"people_attachments,omitempty"`
}

// serveSubscriptions handles /api/team/subscriptions and paths below it.
func (h *teamHandler) serveSubscriptions(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/team/subscriptions" && r.Method == http.MethodGet {
		h.listSubscriptions(w, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/team/subscriptions/")
	parts := strings.Split(rest, "/")
	if !ok {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	if len(parts) != 4 {
		if len(parts) == 7 && (parts[4] == "attachments" || (parts[4] == "pending" && parts[6] == "retire-absent")) {
			// The sequence and attachment identity are validated below.
		} else if len(parts) == 6 && parts[4] == "pending" {
			// The pending key is validated below.
		} else if len(parts) != 5 {
			writeTeamError(w, http.StatusNotFound, "not_found")
			return
		} else {
			switch parts[4] {
			case "seen", "comments", "reactions", "attachments", "local":
			default:
				writeTeamError(w, http.StatusNotFound, "not_found")
				return
			}
		}
	}
	for i, size := range []int{32, 32, 16, 16} {
		if !subHex(parts[i], size) {
			writeTeamError(w, http.StatusNotFound, "not_found")
			return
		}
	}
	id := team.CopyID{Server: parts[0], Owner: parts[1], Chat: parts[2], Generation: parts[3]}
	if (len(parts) == 4 && (r.Method == http.MethodGet || r.Method == http.MethodDelete)) || (len(parts) >= 5 && (parts[4] == "attachments" || parts[4] == "comments" || parts[4] == "reactions" || parts[4] == "pending" || (parts[4] == "local" && r.Method == http.MethodGet) || (parts[4] == "seen" && r.Method == http.MethodPut))) {
		stageID := ""
		if len(parts) == 7 && parts[5] == "stage" {
			stageID = parts[6]
		} else if len(parts) == 5 && parts[4] == "attachments" && r.Method == http.MethodPost {
			stageID = r.URL.Query().Get("stage_id")
		}
		var release func()
		r, release = h.beginPeopleCopyOperation(r, id, stageID)
		defer release()
	}
	if len(parts) == 5 && parts[4] == "local" && r.Method == http.MethodGet {
		h.localPeopleInventory(w, r, id)
		return
	}
	if len(parts) == 7 && parts[4] == "pending" && parts[6] == "retire-absent" && r.Method == http.MethodPost {
		if !subHex(parts[5], 16) {
			writeTeamError(w, http.StatusNotFound, "not_found")
			return
		}
		h.retireAbsentPeople(w, r, id, parts[5])
		return
	}
	if len(parts) == 6 && parts[4] == "pending" && r.Method == http.MethodDelete {
		if !subHex(parts[5], 16) {
			writeTeamError(w, http.StatusNotFound, "not_found")
			return
		}
		h.discardPeoplePending(w, r, id, parts[5])
		return
	}
	if len(parts) == 6 && parts[4] == "pending" && r.Method == http.MethodGet && r.URL.RawQuery != "" {
		if !subHex(parts[5], 16) {
			writeTeamError(w, http.StatusNotFound, "not_found")
			return
		}
		h.localPeoplePublicationState(w, r, id, parts[5])
		return
	}
	if len(parts) == 7 && parts[5] == "stage" && (r.Method == http.MethodDelete || r.Method == http.MethodGet) {
		if !subHex(parts[6], 16) {
			writeTeamError(w, http.StatusNotFound, "not_found")
			return
		}
		if r.Method == http.MethodDelete {
			h.removePeopleStage(w, r, id, parts[6])
		} else {
			h.readPeopleStage(w, r, id, parts[6])
		}
		return
	}
	if len(parts) == 7 && r.Method == http.MethodGet {
		seq, err := strconv.ParseUint(parts[5], 10, 64)
		if err != nil || seq == 0 || strconv.FormatUint(seq, 10) != parts[5] || !subHex(parts[6], 16) {
			writeTeamError(w, http.StatusNotFound, "not_found")
			return
		}
		h.readPeopleAttachment(w, r, id, seq, parts[6])
		return
	}
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
		if parts[4] == "seen" {
			h.seeSubscription(w, r, id)
			return
		}
	}
	if len(parts) == 5 && parts[4] == "attachments" && r.Method == http.MethodPost {
		h.stagePeopleAttachment(w, r, id)
		return
	}
	if len(parts) == 5 {
		if parts[4] == "reactions" {
			h.serveReactions(w, r, id)
			return
		}
		if parts[4] == "comments" {
			switch r.Method {
			case http.MethodGet:
				h.readSubscriptionLog(w, r, id, true)
				return
			case http.MethodPost:
				h.postComment(w, r, id)
				return
			}
		}
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
	for key, removed := range h.removedSubs {
		if now.Sub(removed) > 60*time.Second {
			delete(h.removedSubs, key)
		}
	}
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
	out := subOutcome{entry: entry, started: h.now().UTC()}
	h.mu.Lock()
	h.peopleProbeSerial++
	out.peopleProbe = h.peopleProbeSerial
	h.mu.Unlock()
	hello, err := h.client.Hello(ctx, entry.URL)
	if err == nil && hello.ID != entry.ID {
		err = errHelloMismatch
	}
	if err != nil {
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
	if hello.BlobGeneration {
		out.peopleFiles = "supported"
	} else {
		out.peopleFiles = "unsupported"
	}
	out.channels = channels
	return out
}

func (h *teamHandler) askSeats(ctx context.Context, entry team.Entry) seatOutcome {
	h.mu.Lock()
	if !h.serverCurrentLocked(entry) {
		h.mu.Unlock()
		return seatOutcome{id: entry.ID, entry: entry, err: team.ErrUnreachable}
	}
	ask := bindRoster(h.subAsks[entry.ID], entry)
	ask.roster = h.now().UTC()
	h.subAsks[entry.ID] = ask
	h.mu.Unlock()
	seats, err := h.client.Seats(ctx, entry.URL, entry.Token)
	if err == nil {
		h.storeRosterCurrent(ctx, entry, seats)
	}
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
		if current, ok := joined[outcome.entry.ID]; !ok || !peopleSameServer(current, outcome.entry) {
			continue
		}
		h.subAsks[outcome.entry.ID] = bindRoster(h.subAsks[outcome.entry.ID], outcome.entry)
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
	for i := range outcomes {
		channels := outcomes[i].channels[:0]
		for _, channel := range outcomes[i].channels {
			key := team.CopyID{Server: outcomes[i].entry.ID, Owner: channel.Owner, Chat: channel.Chat, Generation: channel.Generation}
			if removed, ok := h.removedSubs[key]; ok {
				if !removed.Before(outcomes[i].started) {
					continue
				}
			}
			channels = append(channels, channel)
		}
		outcomes[i].channels = channels
	}
	subs, truncation, changed := mergeSubs(subs, entries, outcomes, inst, now)
	if h.captureOwnerFrontiers(subs, inst) {
		changed = true
	}
	joined := entryMap(entries)
	for _, outcome := range outcomes {
		current, ok := joined[outcome.entry.ID]
		if !ok || !peopleSameServer(current, outcome.entry) {
			continue
		}
		h.remember(outcome.entry.ID, outcome.delivery)
		ask := h.subAsks[outcome.entry.ID]
		if (outcome.peopleProbe != 0 && outcome.peopleProbe > ask.peopleProbe) || (outcome.peopleProbe == 0 && !outcome.started.Before(ask.peopleStarted)) {
			ask.peopleFiles = outcome.peopleFiles
			ask.peopleBinding = peopleServerFingerprint(outcome.entry)
			ask.peopleStarted = outcome.started
			ask.peopleProbe = outcome.peopleProbe
			h.subAsks[outcome.entry.ID] = ask
		}
		if value, ok := truncation[outcome.entry.ID]; ok {
			ask := h.subAsks[outcome.entry.ID]
			ask.truncated = value
			h.subAsks[outcome.entry.ID] = ask
		}
	}
	for _, seat := range seats {
		if current, ok := joined[seat.id]; !ok || !peopleSameServer(current, seat.entry) {
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
			if sub.Access != "owner" {
				return true
			}
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
		if current, ok := joined[outcome.entry.ID]; !ok || !peopleSameServer(current, outcome.entry) {
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
		if channel.Owner == inst {
			if channel.Access != "owner" {
				continue
			}
		} else {
			if channel.Access != "read" && channel.Access != "write" {
				continue
			}
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
			if subs[idx].CommentsLast != channel.CommentsLast {
				subs[idx].CommentsLast = channel.CommentsLast
				changed = true
			}
			if channel.Access == "owner" {
				if subs[idx].OwnerName != outcome.entry.SeatName {
					subs[idx].OwnerName = outcome.entry.SeatName
					changed = true
				}
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
		if channel.Access == "owner" {
			ownerName = outcome.entry.SeatName
		}
		subs = append(subs, team.Subscription{
			Server: id, Owner: channel.Owner, Chat: channel.Chat, Generation: channel.Generation,
			ServerName: outcome.entry.Name, OwnerName: ownerName, Title: channel.Title, Access: channel.Access,
			State: "live", MirrorLast: channel.MirrorLast, SeenMirror: 0, CommentsLast: channel.CommentsLast,
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
		if subs[i].Access == "owner" {
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
	peopleReady := h.peopleCapabilitiesReady
	views := make([]subscriptionView, 0, len(subs))
	for i, sub := range subs {
		var localOrder *int
		var copyInstance string
		if peopleReady {
			id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
			copyInstance, _ = team.EnsurePeopleCopyInstance(h.dataDir, id)
		}
		if sub.Access == "owner" {
			order := i
			localOrder = &order
		}
		views = append(views, subscriptionView{
			Server: sub.Server, ServerName: sub.ServerName, Owner: sub.Owner, OwnerName: sub.OwnerName,
			Chat: sub.Chat, Generation: sub.Generation, Title: sub.Title, Access: sub.Access, State: sub.State,
			MirrorLast: sub.MirrorLast, SeenMirror: sub.SeenMirror, CommentsLast: sub.CommentsLast, SeenComments: sub.SeenComments, Unread: subUnread(sub),
			FirstSeen: sub.FirstSeen, LocalNode: h.localOwnerNode(sub), LocalOrder: localOrder, LocalFrontier: h.localOwnerFrontier(sub), PeopleCopyInstance: copyInstance,
		})
	}
	sort.Slice(views, func(i, j int) bool { return subLess(views[i], views[j]) })
	servers := make([]subscriptionServerView, 0, len(entries))
	for _, entry := range entries {
		view := subscriptionServerView{ID: entry.ID, Delivery: h.deliveryWord(entry.ID)}
		if peopleReady {
			view.PeopleFiles = "unknown"
			ask := h.subAsks[entry.ID]
			if ask.peopleBinding == peopleServerFingerprint(entry) && ask.peopleFiles != "" {
				view.PeopleFiles = ask.peopleFiles
				view.PeopleFileBinding = ask.peopleBinding
			}
		}
		servers = append(servers, view)
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].ID < servers[j].ID })
	result := subListResponse{Subscriptions: views, Servers: servers, Truncated: truncated}
	if peopleReady {
		result.PeopleAttachments = &peopleAttachmentCapabilities{Version: 1, MaxFileBytes: 4194304, MaxFiles: 16, MaxTotalBytes: 16777216}
	}
	return result
}

// localOwnerNode verifies a retained mapping against the current log identity.
func (h *teamHandler) localOwnerNode(sub team.Subscription) string {
	if sub.Access != "owner" || sub.OwnerFrontier == nil {
		return ""
	}
	path := filepath.Join(h.dataDir, "sessions", sub.OwnerFrontier.Node+".jsonl")
	found := sessionlog.ReadLogIdentity(path)
	if found.Status != sessionlog.IdentityOK || found.UID != sub.Chat {
		return ""
	}
	return sub.OwnerFrontier.Node
}

// localOwnerFrontier exposes only a UID-verified owner frontier.
func (h *teamHandler) localOwnerFrontier(sub team.Subscription) *team.OwnerFrontier {
	if h.localOwnerNode(sub) == "" {
		return nil
	}
	return sub.OwnerFrontier
}

func subUnread(sub team.Subscription) bool {
	if sub.State != "live" {
		return false
	}
	if sub.CommentsLast > sub.SeenComments {
		return true
	}
	if sub.Access == "owner" {
		return false
	}
	return sub.MirrorLast > sub.SeenMirror
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

func (h *teamHandler) endSubscription(r *http.Request, id team.CopyID, server team.Entry, whole bool) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !peopleCopyCurrentLocked(r) {
		return false, nil
	}
	entries, subs, err := h.loadSubState()
	if err != nil {
		return false, err
	}
	currentServer, currentCopy := false, false
	for _, entry := range entries {
		if peopleSameServer(entry, server) {
			currentServer = true
			break
		}
	}
	for _, sub := range subs {
		if sameCopy(sub, id) {
			currentCopy = true
			break
		}
	}
	if !currentServer || !currentCopy {
		return false, nil
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
		return true, h.saveSubscriptionState(subs)
	}
	return true, nil
}

// subscriptionReadCurrentLocked checks the original read authority at its local write boundary.
func (h *teamHandler) subscriptionReadCurrentLocked(r *http.Request, id team.CopyID, original team.Subscription, server team.Entry) (bool, error) {
	if !peopleCopyCurrentLocked(r) {
		return false, nil
	}
	entries, subs, err := h.loadSubState()
	if err != nil {
		return false, err
	}
	serverOK := false
	for _, entry := range entries {
		if peopleSameServer(entry, server) {
			serverOK = true
			break
		}
	}
	if !serverOK {
		return false, nil
	}
	for _, sub := range subs {
		if sameCopy(sub, id) {
			return sub.State == "live" && sub.Access == original.Access, nil
		}
	}
	// An absent entry is handled by the existing serialized orphan cleanup.
	return true, nil
}

// appendSubscriptionPage serializes the original authority check with copying a relay page.
func (h *teamHandler) appendSubscriptionPage(r *http.Request, id team.CopyID, sub team.Subscription, server team.Entry, log string, entries []team.MirrorRecord) (copyCurrent, current bool, currentErr, appendErr error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	copyCurrent = peopleCopyCurrentLocked(r)
	current, currentErr = h.subscriptionReadCurrentLocked(r, id, sub, server)
	if !current || currentErr != nil {
		return
	}
	appendErr = team.AppendCopy(h.dataDir, id, log, entries)
	return
}

// cleanupMissingSubscription rechecks the missing copy before deleting its local data.
func (h *teamHandler) cleanupMissingSubscription(r *http.Request, id team.CopyID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if peopleCopyCurrentLocked(r) {
		_, current, stateErr := h.loadSubState()
		present := false
		for _, sub := range current {
			if sameCopy(sub, id) {
				present = true
				break
			}
		}
		if stateErr == nil && !present {
			_ = removeSubscriptionCopy(h.dataDir, id)
		}
	}
}

func writeRetiredSubscriptionPage(w http.ResponseWriter, copyCurrent bool) {
	if copyCurrent {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	writeTeamError(w, http.StatusNotFound, "not_found")
}

func (h *teamHandler) readSubscription(w http.ResponseWriter, r *http.Request, id team.CopyID) {
	h.readSubscriptionLog(w, r, id, false)
}

func (h *teamHandler) retiredSubscriptionRead(w http.ResponseWriter, r *http.Request) {
	if h.peopleCopyCurrent(r) {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	writeTeamError(w, http.StatusNotFound, "not_found")
}

func (h *teamHandler) readSubscriptionLog(w http.ResponseWriter, r *http.Request, id team.CopyID, comments bool) {
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
	if _, supplied := query["copy_instance"]; supplied && !h.requirePeopleCopy(w, r) {
		return
	}
	log := team.MirrorLog
	if comments {
		log = team.CommentsLog
	}
	_, last, err := team.ReadCopy(h.dataDir, id, log, ^uint64(0), 256, 4<<20)
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
				if comments {
					page, err = h.client.ReadComments(r.Context(), server.URL, server.Token, id.Owner, id.Chat, last)
				} else {
					page, err = h.client.ReadMirror(r.Context(), server.URL, server.Token, id.Owner, id.Chat, last)
				}
				if !h.peopleCopyCurrent(r) {
					writeTeamError(w, http.StatusNotFound, "not_found")
					return
				}
				if err != nil {
					break
				}
				if page.Generation != id.Generation {
					var current bool
					current, err = h.endSubscription(r, id, server, false)
					if !current && err == nil {
						h.retiredSubscriptionRead(w, r)
						return
					}
					if err != nil {
						writeTeamError(w, 500, "store_unavailable")
						return
					}
					sub.State = "ended"
					break
				}
				copyCurrent, current, currentErr, appendErr := h.appendSubscriptionPage(r, id, sub, server, log, page.Entries)
				if !current || currentErr != nil {
					if currentErr != nil {
						writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
						return
					}
					writeRetiredSubscriptionPage(w, copyCurrent)
					return
				}
				err = appendErr
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
		if !h.peopleCopyCurrent(r) {
			writeTeamError(w, http.StatusNotFound, "not_found")
			return
		}
		switch {
		case err == nil:
		case errors.Is(err, team.ErrNoChannel):
			var current bool
			current, err = h.endSubscription(r, id, server, false)
			if !current && err == nil {
				h.retiredSubscriptionRead(w, r)
				return
			}
			sub.State = "ended"
		case errors.Is(err, team.ErrUnauthorized):
			var current bool
			current, err = h.endSubscription(r, id, server, true)
			if !current && err == nil {
				h.retiredSubscriptionRead(w, r)
				return
			}
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
	_, _, stillFound, _, err := h.lookupSubscription(id)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	if !stillFound {
		h.cleanupMissingSubscription(r, id)
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	records, last, err := team.ReadCopy(h.dataDir, id, log, after, 256, 4<<20)
	if err != nil {
		writeTeamError(w, 500, "copy_damaged")
		return
	}
	if comments {
		h.answerComments(w, r, id, server, joined, records, last, after, stopped, full)
		return
	}
	var out subscriptionReadResponse
	out.View.subscriptionView = subscriptionView{Server: sub.Server, ServerName: sub.ServerName, Owner: sub.Owner, OwnerName: sub.OwnerName, Chat: sub.Chat, Generation: sub.Generation, Title: sub.Title, Access: sub.Access, State: sub.State, MirrorLast: sub.MirrorLast, SeenMirror: sub.SeenMirror, CommentsLast: sub.CommentsLast, SeenComments: sub.SeenComments, Unread: subUnread(sub), FirstSeen: sub.FirstSeen, LocalNode: h.localOwnerNode(sub), LocalFrontier: h.localOwnerFrontier(sub)}
	if h.peopleCapabilitiesReady {
		out.View.PeopleCopyInstance, _ = team.ReadPeopleCopyInstance(h.dataDir, id)
	}
	out.View.Delivery = delivery
	out.Last = last
	out.Entries = []map[string]any{}
	textBytes := 0
	out.Next = after
	if out.Next > last {
		out.Next = last
	}
	for _, record := range records {
		entry, size, ok := projectSubRecordForAccess(record, sub.Access)
		if ok && sub.Access == "owner" && record.Key == "start" {
			if segment, index, found := h.ownerStartAddress(id); found {
				entry["local_segment"], entry["local_record"] = segment, index
			}
		}
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

// captureOwnerFrontiers runs under the team-state lock during subscription refresh.
func (h *teamHandler) captureOwnerFrontiers(subs []team.Subscription, inst string) bool {
	shares, err := team.LoadShares(h.dataDir)
	if err != nil {
		return false
	}
	changed := false
	for i := range subs {
		sub := &subs[i]
		if sub.Access != "owner" || sub.Owner != inst || sub.OwnerFrontier != nil {
			continue
		}
		for _, share := range shares {
			if share.UID != sub.Chat || share.Server != sub.Server || share.Generation != sub.Generation {
				continue
			}
			path := filepath.Join(h.dataDir, "sessions", share.Node+".jsonl")
			id := sessionlog.ReadLogIdentity(path)
			if id.Status != sessionlog.IdentityOK || id.UID != sub.Chat {
				break
			}
			segment, record, ok := ownerStartPosition(sessionlog.ReadEvents(path), share.Start)
			if ok {
				sub.OwnerFrontier = &team.OwnerFrontier{Node: share.Node, Segment: segment, Record: record}
				changed = true
			}
			break
		}
	}
	return changed
}

// ownerStartPosition identifies the cursor immediately before a share starts.
func ownerStartPosition(events []sessionlog.Event, start int) (int, int, bool) {
	if start < 0 || start > len(events) {
		return 0, 0, false
	}
	segment := 0
	for _, ev := range events[:start] {
		if ev.T == "source" {
			segment++
		}
	}
	return segment, start - 1, true
}

func (h *teamHandler) ownerStartAddress(id team.CopyID) (int, int, bool) {
	inst, err := identity.LoadInstallation(h.dataDir)
	if err != nil || string(inst) != id.Owner {
		return 0, 0, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	subs, err := team.LoadSubs(h.dataDir)
	if err != nil {
		return 0, 0, false
	}
	for i := range subs {
		sub := &subs[i]
		if !sameCopy(*sub, id) || sub.Access != "owner" {
			continue
		}
		if f := sub.OwnerFrontier; f != nil {
			return f.Segment, f.Record, true
		}
		shares, err := team.LoadShares(h.dataDir)
		if err != nil {
			return 0, 0, false
		}
		for _, share := range shares {
			if share.UID != id.Chat || share.Server != id.Server || share.Generation != id.Generation {
				continue
			}
			path := filepath.Join(h.dataDir, "sessions", share.Node+".jsonl")
			found := sessionlog.ReadLogIdentity(path)
			if found.Status != sessionlog.IdentityOK || found.UID != id.Chat {
				return 0, 0, false
			}
			segment, index, ok := ownerStartPosition(sessionlog.ReadEvents(path), share.Start)
			if !ok {
				return 0, 0, false
			}
			sub.OwnerFrontier = &team.OwnerFrontier{Node: share.Node, Segment: segment, Record: index}
			if h.saveSubscriptionState(subs) != nil {
				return 0, 0, false
			}
			return segment, index, true
		}
	}
	return 0, 0, false
}

// projectSubRecordForAccess adds the original event address only for the owner.
// Mirror keys identify that event without changing the relay contract.
func projectSubRecordForAccess(record team.MirrorRecord, access string) (map[string]any, int, bool) {
	entry, size, ok := projectSubRecord(record)
	if !ok || access != "owner" {
		return entry, size, ok
	}
	segmentText, recordText, found := strings.Cut(record.Key, "/")
	if !found {
		return entry, size, ok
	}
	segment, err1 := strconv.Atoi(segmentText)
	index, err2 := strconv.Atoi(recordText)
	if err1 != nil || err2 != nil || segment < 0 || index < 0 {
		return entry, size, ok
	}
	entry["local_segment"], entry["local_record"] = segment, index
	return entry, size, ok
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
		Mirror   json.RawMessage `json:"mirror"`
		Comments json.RawMessage `json:"comments"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil {
		writeTeamError(w, 400, "bad_request")
		return
	}
	if req.Mirror == nil && req.Comments == nil {
		writeTeamError(w, 400, "bad_request")
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		writeTeamError(w, 400, "bad_request")
		return
	}
	var mirror, comments *uint64
	if req.Mirror != nil {
		if bytes.Equal(bytes.TrimSpace(req.Mirror), []byte("null")) {
			writeTeamError(w, 400, "bad_request")
			return
		}
		var value uint64
		if json.Unmarshal(req.Mirror, &value) != nil {
			writeTeamError(w, 400, "bad_request")
			return
		}
		mirror = &value
	}
	if req.Comments != nil {
		if bytes.Equal(bytes.TrimSpace(req.Comments), []byte("null")) {
			writeTeamError(w, 400, "bad_request")
			return
		}
		var value uint64
		if json.Unmarshal(req.Comments, &value) != nil {
			writeTeamError(w, 400, "bad_request")
			return
		}
		comments = &value
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	_, bound := query["copy_instance"]
	if bound && !h.requirePeopleCopy(w, r) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !peopleCopyCurrentLocked(r) {
		writeTeamError(w, http.StatusConflict, "copy_changed")
		return
	}
	subs, err := team.LoadSubs(h.dataDir)
	if err != nil {
		writeTeamError(w, 500, "store_unavailable")
		return
	}
	for i := range subs {
		if sameCopy(subs[i], id) {
			changed := false
			if mirror != nil {
				_, last, err := team.ReadCopy(h.dataDir, id, team.MirrorLog, ^uint64(0), 256, 4<<20)
				if err != nil {
					writeTeamError(w, 500, "store_unavailable")
					return
				}
				value := *mirror
				if value > last {
					value = last
				}
				if value > subs[i].SeenMirror {
					subs[i].SeenMirror = value
					changed = true
				}
			}
			if comments != nil {
				_, last, err := team.ReadCopy(h.dataDir, id, team.CommentsLog, ^uint64(0), 256, 4<<20)
				if err != nil {
					writeTeamError(w, 500, "store_unavailable")
					return
				}
				value := *comments
				if value > last {
					value = last
				}
				if value > subs[i].SeenComments {
					subs[i].SeenComments = value
					changed = true
				}
			}
			if changed {
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
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if _, bound := query["copy_instance"]; bound && !h.requirePeopleCopy(w, r) {
		return
	}
	sub, server, found, joined, err := h.lookupSubscription(id)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	if !found {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	if sub.Access == "owner" {
		if sub.State == "live" {
			writeTeamError(w, http.StatusConflict, "own_share")
			return
		}
	}
	if sub.State == "live" {
		if joined {
			err = h.confirmed(r.Context(), server)
			if err == nil {
				err = h.client.LeaveChannel(r.Context(), server.URL, server.Token, id.Owner, id.Chat)
			}
			if err != nil {
				if !errors.Is(err, team.ErrNoChannel) {
					if !errors.Is(err, team.ErrUnauthorized) {
						if errors.Is(err, team.ErrUnreachable) {
							writeTeamError(w, http.StatusBadGateway, "unreachable")
						} else {
							writeTeamError(w, http.StatusBadGateway, "bad_server")
						}
						return
					}
				}
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !peopleCopyCurrentLocked(r) {
		writeTeamError(w, http.StatusConflict, "copy_changed")
		return
	}
	entries, err := team.Load(h.dataDir)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	currentServer, exists := team.Entry{}, false
	for _, entry := range entries {
		if entry.ID == id.Server {
			currentServer, exists = entry, true
			break
		}
	}
	if exists != joined || (joined && !peopleSameServer(currentServer, server)) {
		writeTeamError(w, http.StatusConflict, "copy_changed")
		return
	}
	if err := removeSubscriptionCopy(h.dataDir, id); err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	// The router retains this copy operation until the handler returns.
	epoch := r.Context().Value(peopleCopyOperationKey{}).(*peopleCopyOperation).state
	epoch.epoch++
	for op := range epoch.operations {
		op.cancel()
	}
	subs, err := team.LoadSubs(h.dataDir)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	idx := subIndex(subs, id.Server, subKey{owner: id.Owner, chat: id.Chat, gen: id.Generation})
	if idx < 0 {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	subs = append(subs[:idx], subs[idx+1:]...)
	if err := h.saveSubscriptionState(subs); err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	lastServer := true
	for _, remaining := range subs {
		if remaining.Server == id.Server {
			lastServer = false
			break
		}
	}
	if lastServer {
		if ask, ok := h.subAsks[id.Server]; ok {
			ask.unnamed = nil
			h.subAsks[id.Server] = ask
		}
		if people, err := team.LoadPeople(h.dataDir); err == nil {
			kept := make([]team.Person, 0, len(people))
			for _, person := range people {
				if person.Server != id.Server {
					kept = append(kept, person)
				}
			}
			if len(kept) != len(people) {
				_ = team.SavePeople(h.dataDir, kept)
			}
		}
	}
	if h.removedSubs == nil {
		h.removedSubs = make(map[team.CopyID]time.Time)
	}
	h.removedSubs[id] = h.now().UTC()
	w.WriteHeader(http.StatusNoContent)
}

type commentReadResponse struct {
	Entries   []map[string]any     `json:"entries"`
	Reactions []team.ReactionEvent `json:"reactions"`
	Names     map[string]string    `json:"names"`
	Last      uint64               `json:"last"`
	Next      uint64               `json:"next"`
	More      bool                 `json:"more"`
}

func (h *teamHandler) answerComments(w http.ResponseWriter, r *http.Request, id team.CopyID, server team.Entry, joined bool, records []team.MirrorRecord, last, after uint64, stopped, full bool) {
	inst, err := identity.LoadInstallation(h.dataDir)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "identity_unavailable")
		return
	}
	out := commentReadResponse{Entries: []map[string]any{}, Reactions: []team.ReactionEvent{}, Last: last, Next: after}
	if out.Next > last {
		out.Next = last
	}
	textBytes := 0
	for _, record := range records {
		var reactionBody struct {
			Target *uint64 `json:"target"`
		}
		var kind struct {
			Type string `json:"t"`
		}
		if json.Unmarshal(record.Body, &kind) == nil && kind.Type == "reaction" && json.Unmarshal(record.Body, &reactionBody) == nil && reactionBody.Target != nil && *reactionBody.Target > 0 && *reactionBody.Target < record.Seq {
			targets, _, readErr := team.ReadCopy(h.dataDir, id, team.CommentsLog, *reactionBody.Target-1, 1, 4<<20)
			if readErr != nil {
				writeTeamError(w, http.StatusInternalServerError, "copy_damaged")
				return
			}
			if len(targets) == 1 && targets[0].Seq == *reactionBody.Target {
				if event, valid := team.ParseReactionEvent(record, targets[0]); valid {
					event.Mine = event.Author == string(inst)
					out.Reactions = append(out.Reactions, event)
				}
			}
		}
		entry, size, ok := projectCommentRecord(record, string(inst))
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
	if stopped {
		out.More = true
	}
	if out.Next < last {
		out.More = true
	}
	if full {
		if len(records) == 0 {
			writeTeamError(w, http.StatusInsufficientStorage, "storage_full")
			return
		}
	}
	out.Names = h.commentNames(r.Context(), id.Server, server, joined, string(inst), out.Entries)
	writeJSONStatus(w, http.StatusOK, out)
}

func projectCommentRecord(record team.MirrorRecord, inst string) (map[string]any, int, bool) {
	var body struct {
		Version *int    `json:"v"`
		Type    *string `json:"t"`
		Text    *string `json:"text"`
		Anchor  *uint64 `json:"anchor"`
	}
	if json.Unmarshal(record.Body, &body) != nil {
		return nil, 0, false
	}
	if body.Version == nil {
		return nil, 0, false
	}
	if *body.Version != 1 {
		return nil, 0, false
	}
	if body.Type == nil {
		return nil, 0, false
	}
	if *body.Type != "comment" {
		return nil, 0, false
	}
	if body.Text == nil {
		return nil, 0, false
	}
	if *body.Text == "" {
		return nil, 0, false
	}
	if body.Anchor == nil {
		return nil, 0, false
	}
	entry := map[string]any{"seq": record.Seq, "at": record.At, "author": record.Author, "mine": record.Author == inst, "text": *body.Text, "anchor": *body.Anchor}
	projectPeopleAttachments(record.Body, *body.Text, entry)
	return entry, len(*body.Text), true
}

func (h *teamHandler) storeRoster(server string, seats []team.Seat) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ask := h.subAsks[server]
	if ask.names == nil {
		ask.names = make(map[string]string)
	}
	for _, seat := range seats {
		ask.names[seat.ID] = seat.Name
	}
	h.subAsks[server] = ask
}

func bindRoster(ask subAsk, server team.Entry) subAsk {
	binding := peopleServerFingerprint(server)
	if ask.rosterBinding != "" && ask.rosterBinding != binding {
		ask.roster, ask.seats = time.Time{}, time.Time{}
		ask.names, ask.unnamed = nil, nil
	}
	ask.rosterBinding = binding
	return ask
}

// serverCurrentLocked compares the captured connection with current configuration under h.mu.
func (h *teamHandler) serverCurrentLocked(server team.Entry) bool {
	entries, err := team.Load(h.dataDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if peopleSameServer(entry, server) {
			return true
		}
	}
	return false
}

func (h *teamHandler) storeRosterCurrent(ctx context.Context, server team.Entry, seats []team.Seat) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ctx.Err() != nil || !h.serverCurrentLocked(server) {
		return
	}
	if _, ok := ctx.Value(peopleCopyOperationKey{}).(*peopleCopyOperation); ok && !peopleCopyContextCurrentLocked(ctx) {
		return
	}
	ask := bindRoster(h.subAsks[server.ID], server)
	if ask.names == nil {
		ask.names = make(map[string]string)
	}
	for _, seat := range seats {
		ask.names[seat.ID] = seat.Name
	}
	h.subAsks[server.ID] = ask
}

// rememberUnnamed adds a server and seat only while the handler-wide bound permits it.
// The caller holds h.mu and writes ask back to h.subAsks.
func (h *teamHandler) rememberUnnamed(id string, ask *subAsk, seat string) {
	if _, ok := ask.unnamed[seat]; ok {
		return
	}
	count := len(ask.unnamed)
	for server, other := range h.subAsks {
		if server != id {
			count += len(other.unnamed)
		}
	}
	if count >= 4096 {
		return
	}
	if ask.unnamed == nil {
		ask.unnamed = make(map[string]struct{})
	}
	ask.unnamed[seat] = struct{}{}
}

func (h *teamHandler) commentNames(ctx context.Context, id string, server team.Entry, joined bool, inst string, entries []map[string]any) map[string]string {
	h.mu.Lock()
	if ctx.Err() != nil || (joined && !h.serverCurrentLocked(server)) {
		h.mu.Unlock()
		return map[string]string{}
	}
	if h.subAsks == nil {
		h.subAsks = make(map[string]subAsk)
	}
	ask := h.subAsks[id]
	if joined {
		ask = bindRoster(ask, server)
		h.subAsks[id] = ask
	}
	due := false
	if joined {
		if ask.roster.IsZero() {
			due = true
		} else if h.now().UTC().Sub(ask.roster) > 60*time.Second {
			due = true
		}
	}
	if due {
		ask.roster = h.now().UTC()
		h.subAsks[id] = ask
	}
	h.mu.Unlock()
	if due {
		if h.confirmed(ctx, server) == nil {
			seats, err := h.client.Seats(ctx, server.URL, server.Token)
			if err == nil {
				h.storeRosterCurrent(ctx, server, seats)
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if ctx.Err() != nil || (joined && !h.serverCurrentLocked(server)) {
		return map[string]string{}
	}
	if _, ok := ctx.Value(peopleCopyOperationKey{}).(*peopleCopyOperation); ok && !peopleCopyContextCurrentLocked(ctx) {
		return map[string]string{}
	}
	ask = h.subAsks[id]
	names := make(map[string]string)
	people, loadErr := team.LoadPeople(h.dataDir)
	stored := make(map[string]int)
	if loadErr == nil {
		for i, person := range people {
			if person.Server == id {
				names[person.Seat] = person.Name
				stored[person.Seat] = i
			}
		}
	}
	authors := make(map[string]bool)
	for _, entry := range entries {
		if author, ok := entry["author"].(string); ok {
			authors[author] = true
		}
	}
	for seat := range authors {
		if _, ok := stored[seat]; ok {
			continue
		}
		if _, ok := ask.names[seat]; !ok {
			h.rememberUnnamed(id, &ask, seat)
			continue
		}
		if loadErr != nil {
			h.rememberUnnamed(id, &ask, seat)
		}
	}
	if loadErr == nil {
		for seat := range stored {
			delete(ask.unnamed, seat)
		}
	}
	changed := false
	var added []string
	for seat, name := range ask.names {
		names[seat] = name
		if loadErr != nil {
			continue
		}
		if i, ok := stored[seat]; ok {
			if people[i].Name != name {
				people[i].Name = name
				changed = true
			}
			continue
		}
		candidate := authors[seat]
		if _, ok := ask.unnamed[seat]; ok {
			candidate = true
		}
		if !candidate {
			continue
		}
		h.rememberUnnamed(id, &ask, seat)
		if len(people) >= 4096 {
			continue
		}
		people = append(people, team.Person{Server: id, Seat: seat, Name: name})
		added = append(added, seat)
		changed = true
	}
	if changed {
		if err := team.SavePeople(h.dataDir, people); err == nil {
			for _, seat := range added {
				delete(ask.unnamed, seat)
			}
		}
	}
	h.subAsks[id] = ask
	if joined {
		names[inst] = server.SeatName
	}
	return names
}

func (h *teamHandler) postComment(w http.ResponseWriter, r *http.Request, id team.CopyID) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !utf8.Valid(raw) {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	var req struct {
		Text        *string   `json:"text"`
		Anchor      *uint64   `json:"anchor"`
		Key         *string   `json:"key"`
		Attachments *[]string `json:"attachments,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.Text == nil {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.Attachments != nil && (len(*req.Attachments) == 0 || len(*req.Attachments) > 16) {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if strings.TrimSpace(*req.Text) == "" && req.Attachments == nil {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if len(*req.Text) > 16384 {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.Anchor == nil {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.Key == nil {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !subHex(*req.Key, 16) {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	sub, server, found, joined, err := h.lookupSubscription(id)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	if !found {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	if req.Attachments != nil {
		if !h.requirePeopleCopy(w, r, *req.Key) {
			return
		}
		if h.tryPeopleCompletion(w, r, id, sub, server, *req.Text, *req.Anchor, *req.Key, *req.Attachments) {
			return
		}
	}
	if sub.Access == "read" {
		writeTeamError(w, http.StatusForbidden, "read_only")
		return
	}
	if sub.State != "live" {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	if !joined {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	_, last, err := team.ReadCopy(h.dataDir, id, team.MirrorLog, ^uint64(0), 256, 4<<20)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "copy_damaged")
		return
	}
	if *req.Anchor > last {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.Attachments != nil {
		h.postPeopleFileComment(w, r, id, sub, server, *req.Text, *req.Anchor, *req.Key, *req.Attachments)
		return
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		Version int    `json:"v"`
		Type    string `json:"t"`
		Text    string `json:"text"`
		Anchor  uint64 `json:"anchor"`
	}{1, "comment", *req.Text, *req.Anchor})
	err = h.confirmed(r.Context(), server)
	var seq uint64
	if err == nil {
		seq, err = h.client.AppendComment(r.Context(), server.URL, server.Token, id.Owner, id.Chat, id.Generation, "c/"+*req.Key, bytes.TrimSpace(body.Bytes()))
	}
	switch {
	case err == nil:
		writeJSONStatus(w, http.StatusOK, struct {
			Seq uint64 `json:"seq"`
		}{seq})
	case errors.Is(err, team.ErrStaleGeneration), errors.Is(err, team.ErrNoChannel):
		current, endErr := h.endSubscription(r, id, server, false)
		if endErr != nil {
			writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
			return
		}
		if !current {
			writeTeamError(w, http.StatusConflict, "not_active")
			return
		}
		writeTeamError(w, http.StatusConflict, "not_active")
	case errors.Is(err, team.ErrForbidden):
		writeTeamError(w, http.StatusForbidden, "read_only")
	case errors.Is(err, team.ErrUnauthorized):
		current, endErr := h.endSubscription(r, id, server, true)
		if endErr != nil {
			writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
			return
		}
		if !current {
			writeTeamError(w, http.StatusConflict, "not_active")
			return
		}
		writeTeamError(w, http.StatusBadGateway, "refused")
	case errors.Is(err, team.ErrUnreachable):
		writeTeamError(w, http.StatusBadGateway, "unreachable")
	case errors.Is(err, team.ErrQuota):
		writeTeamError(w, http.StatusInsufficientStorage, "server_full")
	case errors.Is(err, team.ErrTooLarge):
		writeTeamError(w, http.StatusRequestEntityTooLarge, "too_large")
	default:
		writeTeamError(w, http.StatusBadGateway, "bad_server")
	}
}
