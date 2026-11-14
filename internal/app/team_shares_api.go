package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	Server  string         `json:"server"`
	Scope   string         `json:"scope"`
	Members *[]team.Member `json:"members"`
}

type teamShareView struct {
	UID      string        `json:"uid"`
	Node     string        `json:"node"`
	Server   string        `json:"server"`
	Scope    string        `json:"scope"`
	State    string        `json:"state"`
	Delivery string        `json:"delivery"`
	Members  []team.Member `json:"members"`
	SharedAt string        `json:"shared_at"`
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
		h.putShare(w, r, node)
	case http.MethodDelete:
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
		UID: s.UID, Node: s.Node, Server: s.Server, Scope: s.Scope,
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
	if req.Members == nil || len(*req.Members) > maxShareRequestMembers {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	seen := make(map[string]struct{}, len(*req.Members))
	for _, m := range *req.Members {
		if !validShareSeat(m.Seat) || (m.Access != "read" && m.Access != "write") {
			writeTeamError(w, http.StatusBadRequest, "bad_request")
			return
		}
		if _, ok := seen[m.Seat]; ok {
			writeTeamError(w, http.StatusBadRequest, "bad_request")
			return
		}
		seen[m.Seat] = struct{}{}
	}
	inst, err := identity.LoadInstallation(h.dataDir)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "identity_unavailable")
		return
	}
	for _, m := range *req.Members {
		if m.Seat == string(inst) {
			writeTeamError(w, http.StatusBadRequest, "bad_request")
			return
		}
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
		h.updateShare(w, r, entry, string(inst), existing, req.Scope, *req.Members)
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

// createShare sends Hello, then the channel, then the members. A channel
// that already exists is a leftover: it is deleted and created once more.
// A second answer that it exists is bad_server. Any failure after the
// channel exists deletes it again, best effort, and stores nothing.
func (h *teamHandler) createShare(w http.ResponseWriter, r *http.Request, entry team.Entry, inst, node, uid, title string, records int, scope string, members []team.Member) {
	ctx := r.Context()
	if err := h.confirmed(ctx, entry); err != nil {
		writeShareCallError(w, err)
		return
	}
	sentTitle := channelTitle(title)
	gen, created, err := h.client.PutChannel(ctx, entry.URL, entry.Token, inst, uid, sentTitle)
	if err != nil {
		writeShareCallError(w, err)
		return
	}
	if !created {
		if err := h.client.DeleteChannel(ctx, entry.URL, entry.Token, inst, uid); err != nil {
			writeShareCallError(w, err)
			return
		}
		gen, created, err = h.client.PutChannel(ctx, entry.URL, entry.Token, inst, uid, sentTitle)
		if err != nil {
			h.bestEffortDelete(ctx, entry, inst, uid)
			writeShareCallError(w, err)
			return
		}
		if !created {
			h.bestEffortDelete(ctx, entry, inst, uid)
			writeTeamError(w, http.StatusBadGateway, "bad_server")
			return
		}
	}
	if err := h.client.SetMembers(ctx, entry.URL, entry.Token, inst, uid, members); err != nil {
		h.bestEffortDelete(ctx, entry, inst, uid)
		writeShareCallError(w, err)
		return
	}
	start := 0
	if scope == "from_now" {
		start = records
	}
	share := team.Share{
		UID: uid, Node: node, Server: entry.ID, Scope: scope,
		Start: start, Next: start, StartSent: false,
		Title: sentTitle, Generation: gen, Members: members,
		State: "active", SharedAt: h.now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if err := h.writeShare(share); err != nil {
		h.bestEffortDelete(ctx, entry, inst, uid)
		writeShareSaveError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, h.shareAPI(share))
}

func (h *teamHandler) bestEffortDelete(ctx context.Context, entry team.Entry, inst, chat string) {
	_ = h.client.DeleteChannel(ctx, entry.URL, entry.Token, inst, chat)
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
			h.finishStop(w, share.UID)
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
	err = h.client.DeleteChannel(r.Context(), entry.URL, entry.Token, string(inst), share.UID)
	if err == nil || errors.Is(err, team.ErrNoChannel) || errors.Is(err, team.ErrUnauthorized) {
		h.finishStop(w, share.UID)
		return
	}
	if errors.Is(err, team.ErrUnreachable) {
		writeJSONStatus(w, http.StatusAccepted, h.shareAPI(share))
		return
	}
	writeTeamError(w, http.StatusBadGateway, "bad_server")
}

func (h *teamHandler) finishStop(w http.ResponseWriter, uid string) {
	err := h.deleteShareFile(uid)
	if err == nil || errors.Is(err, team.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
}

func (h *teamHandler) confirmed(ctx context.Context, entry team.Entry) error {
	hello, err := h.client.Hello(ctx, entry.URL)
	if err != nil {
		return err
	}
	if hello.ID != entry.ID {
		return errHelloMismatch
	}
	return nil
}

func (h *teamHandler) readShares() ([]team.Share, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return team.LoadShares(h.dataDir)
}

func (h *teamHandler) writeShare(s team.Share) error {
	h.mu.Lock()
	defer h.mu.Unlock()
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
		if err := team.SaveShare(h.dataDir, shares[i]); err != nil {
			return team.Share{}, err
		}
		return shares[i], nil
	}
	return team.Share{}, team.ErrNotFound
}

func (h *teamHandler) deleteShareFile(uid string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
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
	return title, found, nil
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
