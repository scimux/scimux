package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/team"
)

const (
	maxTeamShareBody       = 64 << 10
	maxShareRequestMembers = 200
)

// errHelloMismatch means Hello answered, and the server id was not the one
// stored for this seat. Delete keeps the share and answers 202. A start or
// an update answers bad_server and sends no token.
var errHelloMismatch = errors.New("team: hello named another server")

type sharePutRequest struct {
	Server         string         `json:"server"`
	Scope          string         `json:"scope"`
	Members        *[]team.Member `json:"members"`
	MembershipOnly *shareIdentity `json:"membership_only,omitempty"`
}

type shareIdentity struct {
	UID        string    `json:"uid"`
	Generation string    `json:"generation"`
	Baseline   *[]string `json:"baseline"`
}

type teamShareView struct {
	UID        string        `json:"uid"`
	Node       string        `json:"node"`
	Server     string        `json:"server"`
	Scope      string        `json:"scope"`
	Generation string        `json:"generation"`
	State      string        `json:"state"`
	Delivery   string        `json:"delivery"`
	Members    []team.Member `json:"members"`
	SharedAt   string        `json:"shared_at"`
}

type teamSeatView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (h *teamHandler) serveShares(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/team/shares" {
		if r.Method != http.MethodGet {
			writeTeamError(w, http.StatusNotFound, "not_found")
			return
		}
		h.listShares(w)
		return
	}
	node, ok := shareNodePath(r.URL.Path)
	if !ok {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.getShare(w, node)
	case http.MethodPut:
		h.shareOps.Lock()
		defer h.shareOps.Unlock()
		h.putShare(w, r, node)
	case http.MethodDelete:
		h.shareOps.Lock()
		defer h.shareOps.Unlock()
		h.stopShare(w, r, node)
	default:
		writeTeamError(w, http.StatusNotFound, "not_found")
	}
}

func shareNodePath(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/api/team/shares/")
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

func (h *teamHandler) seats(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	entry, ok := h.beginServer(w, id)
	if !ok {
		return
	}
	inst, err := identity.LoadInstallation(h.dataDir)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "identity_unavailable")
		return
	}
	if err := h.confirmed(r.Context(), entry); err != nil {
		writeShareCallError(w, err)
		return
	}
	seats, err := h.client.Seats(r.Context(), entry.URL, entry.Token)
	if err != nil {
		writeShareCallError(w, err)
		return
	}
	view := make([]teamSeatView, 0, len(seats))
	for _, seat := range seats {
		view = append(view, teamSeatView{ID: seat.ID, Name: seat.Name})
	}
	writeJSONStatus(w, http.StatusOK, struct {
		Self  string         `json:"self"`
		Seats []teamSeatView `json:"seats"`
	}{Self: string(inst), Seats: view})
}

func (h *teamHandler) listShares(w http.ResponseWriter) {
	shares, err := h.readShares()
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	views := make([]teamShareView, len(shares))
	for i, s := range shares {
		views[i] = h.shareAPI(s)
	}
	sortShareViews(views)
	writeJSONStatus(w, http.StatusOK, struct {
		Shares []teamShareView `json:"shares"`
	}{Shares: views})
}

func (h *teamHandler) getShare(w http.ResponseWriter, node string) {
	shares, err := h.readShares()
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	share, ok := h.pickShare(shares, node)
	if !ok {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSONStatus(w, http.StatusOK, h.shareAPI(share))
}

func sortShareViews(views []teamShareView) {
	for i := 1; i < len(views); i++ {
		j := i
		for j > 0 && shareViewLess(views[j], views[j-1]) {
			views[j], views[j-1] = views[j-1], views[j]
			j--
		}
	}
}

func shareViewLess(a, b teamShareView) bool {
	if a.Node != b.Node {
		return a.Node < b.Node
	}
	return a.UID < b.UID
}

func (h *teamHandler) shareAPI(s team.Share) teamShareView {
	return teamShareView{
		UID: s.UID, Node: s.Node, Server: s.Server, Scope: s.Scope, Generation: s.Generation,
		State: s.State, Delivery: h.deliveryOf(s.Server),
		Members: s.Members, SharedAt: s.SharedAt,
	}
}

func (h *teamHandler) deliveryOf(server string) string {
	word := "ok"
	if h.shareDelivery != nil {
		word = h.shareDelivery(server)
	}
	switch word {
	case "ok", "offline", "refused", "changed":
		return word
	default:
		return "ok"
	}
}

// putShare checks the body before the installation id, the node, and the
// server. The seat that is this installation is rejected only after the id
// is known. A chat that already has a share keeps its server and scope.
func (h *teamHandler) putShare(w http.ResponseWriter, r *http.Request, node string) {
	var req sharePutRequest
	if err := decodeSharePut(w, r, &req); err != nil {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if req.Scope != "from_now" && req.Scope != "all" {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	baseline, seen, code := screenShareMembers(req)
	if code != "" {
		writeTeamError(w, http.StatusBadRequest, code)
		return
	}
	inst, err := identity.LoadInstallation(h.dataDir)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "identity_unavailable")
		return
	}
	if shareIncludesSelf(*req.Members, string(inst)) {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	title, found, err := h.nodeTitle(node)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	if !found {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	uid, records, ok := h.chatCursor(node)
	if !ok {
		writeTeamError(w, http.StatusConflict, "no_history")
		return
	}
	entry, ok := h.beginServer(w, req.Server)
	if !ok {
		return
	}
	shares, err := h.readShares()
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	if existing, ok := shareByUID(shares, uid); ok {
		if req.MembershipOnly != nil {
			if req.MembershipOnly.UID != existing.UID || req.MembershipOnly.Generation == "" || req.MembershipOnly.Generation != existing.Generation {
				writeTeamError(w, http.StatusConflict, "share_changed")
				return
			}
			members, ok := foldMembership(existing.Members, *req.Members, seen, baseline)
			if !ok {
				writeTeamError(w, http.StatusBadRequest, "bad_request")
				return
			}
			*req.Members = members
		}
		h.updateShare(w, r, entry, string(inst), existing, req.Scope, *req.Members)
		return
	}
	if req.MembershipOnly != nil {
		writeTeamError(w, http.StatusConflict, "share_changed")
		return
	}
	h.createShare(w, r, entry, string(inst), node, uid, title, records, req.Scope, *req.Members)
}

func (h *teamHandler) updateShare(w http.ResponseWriter, r *http.Request, entry team.Entry, inst string, share team.Share, scope string, members []team.Member) {
	if share.Server != entry.ID || share.Scope != scope {
		writeTeamError(w, http.StatusConflict, "share_fixed")
		return
	}
	if share.State != "active" {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	if err := h.confirmed(r.Context(), entry); err != nil {
		writeShareCallError(w, err)
		return
	}
	if err := h.client.SetMembers(r.Context(), entry.URL, entry.Token, inst, share.UID, members); err != nil {
		writeShareCallError(w, err)
		return
	}
	updated, err := h.keepShareFields(share.UID, func(cur *team.Share) {
		cur.Members = members
	})
	if err != nil {
		writeShareSaveError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusOK, h.shareAPI(updated))
}

// errShareStore wraps a failure to save a share after the channel existed.
// errChannelLeftOver means the channel already existed after one replacement.
var (
	errShareStore      = errors.New("team: share store")
	errChannelLeftOver = errors.New("team: channel already exists")
)

// openedShare is the chat a provision creates. Channel is what SetMembers
// sends when it differs from the members stored on the share. Nil uses Members.
type openedShare struct {
	Node, UID, Title, Scope, Lane string
	Start, LaneRev                int
	Members                       []team.Member
	Channel                       []team.Member
}

// createShare sends Hello, then the channel, then the members. The stored
// cursor is records for from_now and 0 for all.
func (h *teamHandler) createShare(w http.ResponseWriter, r *http.Request, entry team.Entry, inst, node, uid, title string, records int, scope string, members []team.Member) {
	start := 0
	if scope == "from_now" {
		start = records
	}
	share, err := provisionChat(r.Context(), h.client, h.now, h.writeShare, entry, inst, openedShare{
		Node: node, UID: uid, Title: title, Scope: scope, Start: start, Members: members,
	})
	if err != nil {
		if errors.Is(err, errShareStore) {
			writeShareSaveError(w, err)
			return
		}
		writeShareCallError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, h.shareAPI(share))
}

// provisionChat confirms Hello, creates the channel, sets its members, and
// stores the share at spec.Start. A leftover channel is replaced once. A
// second answer that it exists, or any later failure, deletes it and stores
// nothing. The handler and the lane reconcile both use this function.
func provisionChat(ctx context.Context, client *team.Client, now func() time.Time, save func(team.Share) error, entry team.Entry, inst string, spec openedShare) (team.Share, error) {
	gen, sent, err := openChannel(ctx, client, entry, inst, spec.UID, spec.Title, channelMembers(spec))
	if err != nil {
		return team.Share{}, err
	}
	share := team.Share{
		UID: spec.UID, Node: spec.Node, Server: entry.ID, Scope: spec.Scope,
		Start: spec.Start, Next: spec.Start, StartSent: false,
		Title: sent, Generation: gen, Members: spec.Members,
		Lane: spec.Lane, LaneRev: spec.LaneRev,
		State: "active", SharedAt: now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if err := save(share); err != nil {
		_ = client.DeleteChannel(ctx, entry.URL, entry.Token, inst, spec.UID, gen)
		return team.Share{}, fmt.Errorf("%w: %w", errShareStore, err)
	}
	return share, nil
}

func channelMembers(spec openedShare) []team.Member {
	if spec.Channel != nil {
		return spec.Channel
	}
	return spec.Members
}

// openChannel creates a channel and sets its members. A channel that already
// exists is deleted and created once more. Failure after it exists deletes it.
func openChannel(ctx context.Context, client *team.Client, entry team.Entry, inst, chat, title string, members []team.Member) (string, string, error) {
	if err := confirmEntry(ctx, client, entry); err != nil {
		return "", "", err
	}
	sent := channelTitle(title)
	gen, created, err := client.PutChannel(ctx, entry.URL, entry.Token, inst, chat, sent)
	if err != nil {
		return "", "", err
	}
	if !created {
		if err := client.DeleteChannel(ctx, entry.URL, entry.Token, inst, chat, gen); err != nil {
			return "", "", err
		}
		gen, created, err = client.PutChannel(ctx, entry.URL, entry.Token, inst, chat, sent)
		if err != nil {
			return "", "", err
		}
		if !created {
			_ = client.DeleteChannel(ctx, entry.URL, entry.Token, inst, chat, gen)
			return "", "", errChannelLeftOver
		}
	}
	if err := client.SetMembers(ctx, entry.URL, entry.Token, inst, chat, members); err != nil {
		_ = client.DeleteChannel(ctx, entry.URL, entry.Token, inst, chat, gen)
		return "", "", err
	}
	return gen, sent, nil
}

func confirmEntry(ctx context.Context, client *team.Client, entry team.Entry) error {
	hello, err := client.Hello(ctx, entry.URL)
	if err != nil {
		return err
	}
	if hello.ID != entry.ID {
		return errHelloMismatch
	}
	return nil
}

// stopShare reads the installation and the server before it marks the share
// stopping, so a missing id or a damaged server file leaves the share active.
// A forgotten server has no URL, so the share is removed locally.
func (h *teamHandler) stopShare(w http.ResponseWriter, r *http.Request, node string) {
	shares, err := h.readShares()
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	share, ok := h.pickShare(shares, node)
	if !ok {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	inst, err := identity.LoadInstallation(h.dataDir)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "identity_unavailable")
		return
	}
	entry, err := h.serverByID(share.Server)
	if err != nil {
		if errors.Is(err, team.ErrNotFound) {
			h.finishStop(w, share)
			return
		}
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	stopped, err := h.keepShareFields(share.UID, func(cur *team.Share) {
		cur.State = "stopping"
	})
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	share = stopped
	if err := h.confirmed(r.Context(), entry); err != nil {
		if errors.Is(err, team.ErrUnreachable) || errors.Is(err, errHelloMismatch) {
			writeJSONStatus(w, http.StatusAccepted, h.shareAPI(share))
			return
		}
		writeTeamError(w, http.StatusBadGateway, "bad_server")
		return
	}
	err = h.client.DeleteChannel(r.Context(), entry.URL, entry.Token, string(inst), share.UID, share.Generation)
	if err == nil || errors.Is(err, team.ErrNoChannel) || errors.Is(err, team.ErrStaleGeneration) || errors.Is(err, team.ErrUnauthorized) {
		h.finishStop(w, share)
		return
	}
	if errors.Is(err, team.ErrUnreachable) {
		writeJSONStatus(w, http.StatusAccepted, h.shareAPI(share))
		return
	}
	writeTeamError(w, http.StatusBadGateway, "bad_server")
}

func (h *teamHandler) finishStop(w http.ResponseWriter, share team.Share) {
	inst, identityErr := identity.LoadInstallation(h.dataDir)
	if identityErr == nil {
		id := team.CopyID{Server: share.Server, Owner: string(inst), Chat: share.UID, Generation: share.Generation}
		sub, _, found, _, err := h.lookupSubscription(id)
		if err != nil {
			writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
			return
		}
		if found && sub.Access == "owner" {
			if _, _, ok := h.ownerStartAddress(id); !ok {
				writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
				return
			}
		}
	}
	err := h.deleteShareFile(share.UID, share.Generation)
	if err == nil || errors.Is(err, team.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
}

func (h *teamHandler) confirmed(ctx context.Context, entry team.Entry) error {
	return confirmEntry(ctx, h.client, entry)
}

// screenShareMembers checks seats, access, duplicates, and the membership
// baseline. It does not know the installation id. An empty code means the
// body can proceed. seen is the requested seats.
func screenShareMembers(req sharePutRequest) (baseline map[string]bool, seen map[string]struct{}, code string) {
	if req.Members == nil || len(*req.Members) > maxShareRequestMembers {
		return nil, nil, "bad_request"
	}
	baseline = map[string]bool{}
	if req.MembershipOnly != nil {
		if req.MembershipOnly.Baseline == nil || len(*req.MembershipOnly.Baseline) > maxShareRequestMembers {
			return nil, nil, "bad_request"
		}
		for _, seat := range *req.MembershipOnly.Baseline {
			if !validShareSeat(seat) || baseline[seat] {
				return nil, nil, "bad_request"
			}
			baseline[seat] = true
		}
	}
	seen = make(map[string]struct{}, len(*req.Members))
	for _, m := range *req.Members {
		if !validShareSeat(m.Seat) || (m.Access != "read" && m.Access != "write") {
			return nil, nil, "bad_request"
		}
		if _, ok := seen[m.Seat]; ok {
			return nil, nil, "bad_request"
		}
		seen[m.Seat] = struct{}{}
	}
	return baseline, seen, ""
}

func shareIncludesSelf(members []team.Member, self string) bool {
	for _, m := range members {
		if m.Seat == self {
			return true
		}
	}
	return false
}

// foldMembership applies a membership-only save. The bool is false when the
// merged list is over the member cap. A baseline seat that is no longer
// stored stays absent. A stored seat the request did not name is kept.
func foldMembership(existing, requested []team.Member, seen map[string]struct{}, baseline map[string]bool) ([]team.Member, bool) {
	roles := make(map[string]string, len(existing))
	for _, m := range existing {
		roles[m.Seat] = m.Access
	}
	members := make([]team.Member, 0, len(requested)+len(existing))
	for _, m := range requested {
		if role, found := roles[m.Seat]; found {
			m.Access = role
		} else if baseline[m.Seat] {
			continue
		} else {
			m.Access = "write"
		}
		members = append(members, m)
	}
	for _, m := range existing {
		if _, chosen := seen[m.Seat]; !chosen && !baseline[m.Seat] {
			members = append(members, m)
		}
	}
	if len(members) > maxShareRequestMembers {
		return nil, false
	}
	return members, true
}

func (h *teamHandler) readShares() ([]team.Share, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return team.LoadShares(h.dataDir)
}

func (h *teamHandler) writeShare(s team.Share) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.saveShareState(s)
}

func (h *teamHandler) saveShareState(s team.Share) error {
	if h.saveShare != nil {
		return h.saveShare(h.dataDir, s)
	}
	return team.SaveShare(h.dataDir, s)
}

// keepShareFields applies a route change to the share that is stored now.
// It does not create a share. The lock is the one the outbox uses too.
func (h *teamHandler) keepShareFields(uid string, apply func(*team.Share)) (team.Share, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	shares, err := team.LoadShares(h.dataDir)
	if err != nil {
		return team.Share{}, err
	}
	for i := range shares {
		if shares[i].UID != uid {
			continue
		}
		apply(&shares[i])
		if err := h.saveShareState(shares[i]); err != nil {
			return team.Share{}, err
		}
		return shares[i], nil
	}
	return team.Share{}, team.ErrNotFound
}

func (h *teamHandler) deleteShareFile(uid, generation string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	shares, err := team.LoadShares(h.dataDir)
	if err != nil {
		return err
	}
	matched := false
	for _, share := range shares {
		if share.UID == uid && share.Generation == generation {
			matched = true
			break
		}
	}
	if !matched {
		return nil
	}
	if h.removeShare != nil {
		return h.removeShare(h.dataDir, uid)
	}
	return team.RemoveShare(h.dataDir, uid)
}

// pickShare chooses the share for a node. The live session-log UID wins when
// it names one of them. Otherwise the least UID wins. The node record is not
// required: a share can outlive its node.
func (h *teamHandler) pickShare(shares []team.Share, node string) (team.Share, bool) {
	matches := make([]team.Share, 0)
	for _, s := range shares {
		if s.Node == node {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		return team.Share{}, false
	}
	id := sessionlog.ReadLogIdentity(filepath.Join(h.dataDir, "sessions", node+".jsonl"))
	if id.Status == sessionlog.IdentityOK {
		for _, s := range matches {
			if s.UID == id.UID {
				return s, true
			}
		}
	}
	best := matches[0]
	for _, s := range matches[1:] {
		if s.UID < best.UID {
			best = s
		}
	}
	return best, true
}

func shareByUID(shares []team.Share, uid string) (team.Share, bool) {
	for _, s := range shares {
		if s.UID == uid {
			return s, true
		}
	}
	return team.Share{}, false
}

func (h *teamHandler) nodeTitle(id string) (string, bool, error) {
	raw, err := os.ReadFile(filepath.Join(h.dataDir, "nodes.jsonl"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	snap, ok := parseNodeLog(raw)[id]
	if !ok || !snap.live {
		return "", false, nil
	}
	return snap.title, true, nil
}

func (h *teamHandler) chatCursor(node string) (string, int, bool) {
	path := filepath.Join(h.dataDir, "sessions", node+".jsonl")
	id := sessionlog.ReadLogIdentity(path)
	if id.Status != sessionlog.IdentityOK {
		return "", 0, false
	}
	return id.UID, len(sessionlog.ReadEvents(path)), true
}

func channelTitle(title string) string {
	var b strings.Builder
	for _, r := range title {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	s := b.String()
	if len(s) <= 200 {
		return s
	}
	cut := 0
	for i, r := range s {
		if i+utf8.RuneLen(r) > 200 {
			break
		}
		cut = i + utf8.RuneLen(r)
	}
	return s[:cut]
}

func validShareSeat(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte("0123456789abcdef", s[i]) < 0 {
			return false
		}
	}
	return true
}

func decodeSharePut(w http.ResponseWriter, r *http.Request, dest any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxTeamShareBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("team: trailing json")
	}
	return nil
}

func writeShareCallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, team.ErrUnreachable):
		writeTeamError(w, http.StatusBadGateway, "unreachable")
	case errors.Is(err, team.ErrUnauthorized):
		writeTeamError(w, http.StatusBadGateway, "refused")
	case errors.Is(err, team.ErrUnknownMember):
		writeTeamError(w, http.StatusUnprocessableEntity, "unknown_member")
	case errors.Is(err, team.ErrQuota):
		writeTeamError(w, http.StatusInsufficientStorage, "server_full")
	default:
		writeTeamError(w, http.StatusBadGateway, "bad_server")
	}
}

func writeShareSaveError(w http.ResponseWriter, err error) {
	if errors.Is(err, team.ErrTooManyShares) {
		writeTeamError(w, http.StatusInsufficientStorage, "too_many")
		return
	}
	writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
}
