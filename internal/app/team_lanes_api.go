package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

// readChatEntropy fills the index channel id. Tests replace it.
var readChatEntropy = rand.Read

type lanePutRequest struct {
	Server         string         `json:"server"`
	Scope          string         `json:"scope"`
	Members        *[]team.Member `json:"members"`
	MembershipOnly *shareIdentity `json:"membership_only,omitempty"`
	Name           string         `json:"name"`
	Color          string         `json:"color"`
	Excluded       *[]string      `json:"excluded"`
}

type laneShareView struct {
	Lane     string        `json:"lane"`
	ID       string        `json:"id"`
	Server   string        `json:"server"`
	Scope    string        `json:"scope"`
	Name     string        `json:"name"`
	Color    string        `json:"color"`
	Members  []team.Member `json:"members"`
	State    string        `json:"state"`
	Delivery string        `json:"delivery"`
	Chats    int           `json:"chats"`
	Excluded int           `json:"excluded"`
	SharedAt string        `json:"shared_at"`
}

func (h *teamHandler) serveLaneShares(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/team/lane-shares" {
		if r.Method != http.MethodGet {
			writeTeamError(w, http.StatusNotFound, "not_found")
			return
		}
		h.listLanes(w)
		return
	}
	lane, ok := laneSharePath(r.URL.Path)
	if !ok {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.getLane(w, lane)
	case http.MethodPut:
		h.shareOps.Lock()
		defer h.shareOps.Unlock()
		h.putLane(w, r, lane)
	case http.MethodDelete:
		h.shareOps.Lock()
		defer h.shareOps.Unlock()
		h.deleteLane(w, lane)
	default:
		writeTeamError(w, http.StatusNotFound, "not_found")
	}
}

func laneSharePath(path string) (string, bool) {
	rest := strings.TrimPrefix(path, "/api/team/lane-shares/")
	if rest == "" || strings.Contains(rest, "/") || !team.ValidLaneID(rest) {
		return "", false
	}
	return rest, true
}

func (h *teamHandler) listLanes(w http.ResponseWriter) {
	lanes, shares, err := h.laneSnapshot()
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	views := make([]laneShareView, 0, len(lanes))
	for _, lane := range lanes {
		views = append(views, h.laneView(lane, shares))
	}
	writeJSONStatus(w, http.StatusOK, struct {
		LaneShares []laneShareView `json:"lane_shares"`
	}{views})
}

func (h *teamHandler) getLane(w http.ResponseWriter, lane string) {
	lanes, shares, err := h.laneSnapshot()
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	found, ok := laneByLocal(lanes, lane)
	if !ok {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSONStatus(w, http.StatusOK, h.laneView(found, shares))
}

func (h *teamHandler) putLane(w http.ResponseWriter, r *http.Request, lane string) {
	var req lanePutRequest
	if decodeSharePut(w, r, &req) != nil || (req.Scope != "from_now" && req.Scope != "all") || req.Excluded == nil || !team.ValidLaneName(req.Name) || !team.ValidLaneColor(req.Color) || !team.ValidExcluded(*req.Excluded) {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	base := sharePutRequest{Server: req.Server, Scope: req.Scope, Members: req.Members, MembershipOnly: req.MembershipOnly}
	baseline, seen, code := screenShareMembers(base)
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
	entry, ok := h.beginServer(w, req.Server)
	if !ok {
		return
	}
	lanes, err := h.readLanes()
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	if existing, found := laneByLocal(lanes, lane); found {
		h.reviseLane(w, entry, existing, req, baseline, seen)
		return
	}
	if req.MembershipOnly != nil {
		writeTeamError(w, http.StatusConflict, "share_changed")
		return
	}
	h.openLane(w, r, entry, string(inst), lane, req)
}

func (h *teamHandler) reviseLane(w http.ResponseWriter, entry team.Entry, existing team.LaneShare, req lanePutRequest, baseline map[string]bool, seen map[string]struct{}) {
	if existing.Server != entry.ID || existing.Scope != req.Scope {
		writeTeamError(w, http.StatusConflict, "share_fixed")
		return
	}
	if existing.State != "active" {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	members := *req.Members
	if req.MembershipOnly != nil {
		if req.MembershipOnly.UID != existing.ID || req.MembershipOnly.Generation == "" || req.MembershipOnly.Generation != existing.Generation {
			writeTeamError(w, http.StatusConflict, "share_changed")
			return
		}
		folded, ok := foldMembership(existing.Members, members, seen, baseline)
		if !ok {
			writeTeamError(w, http.StatusBadRequest, "bad_request")
			return
		}
		members = folded
	}
	if !sameMembers(existing.Members, members) {
		existing.Rev++
	}
	existing.Name, existing.Color, existing.Members = req.Name, req.Color, members
	if err := h.writeLane(existing); err != nil {
		writeLaneSaveError(w, err)
		return
	}
	h.kickSharesNow()
	_, shares, err := h.laneSnapshot()
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	writeJSONStatus(w, http.StatusOK, h.laneView(existing, shares))
}

func (h *teamHandler) openLane(w http.ResponseWriter, r *http.Request, entry team.Entry, inst, lane string, req lanePutRequest) {
	id, err := h.freshIndexID(lane)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	starts, err := h.laneStarts(lane, req.Scope)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	gen, _, err := openChannel(r.Context(), h.client, entry, inst, id, "Shared lane · "+req.Name, *req.Members)
	if err != nil {
		writeShareCallError(w, err)
		return
	}
	share := team.LaneShare{
		ID: id, Lane: lane, Server: entry.ID, Scope: req.Scope, Name: req.Name, Color: req.Color,
		Members: *req.Members, Rev: 1, Starts: starts, Excluded: append([]string{}, (*req.Excluded)...),
		Generation: gen, IndexRev: 1, State: "active", SharedAt: h.now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if err := h.writeLane(share); err != nil {
		_ = h.client.DeleteChannel(r.Context(), entry.URL, entry.Token, inst, id, gen)
		writeLaneSaveError(w, err)
		return
	}
	h.kickSharesNow()
	writeJSONStatus(w, http.StatusCreated, h.laneView(share, nil))
}

func (h *teamHandler) deleteLane(w http.ResponseWriter, lane string) {
	lanes, shares, err := h.laneSnapshot()
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	existing, ok := laneByLocal(lanes, lane)
	if !ok {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	if err := h.markLaneChatsStopping(existing.ID); err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	existing.State = "stopping"
	if err := h.writeLane(existing); err != nil {
		writeLaneSaveError(w, err)
		return
	}
	h.kickSharesNow()
	writeJSONStatus(w, http.StatusAccepted, h.laneView(existing, shares))
}

func (h *teamHandler) laneView(lane team.LaneShare, shares []team.Share) laneShareView {
	n := 0
	for _, s := range shares {
		if s.Lane == lane.ID {
			n++
		}
	}
	return laneShareView{
		Lane: lane.Lane, ID: lane.ID, Server: lane.Server, Scope: lane.Scope, Name: lane.Name, Color: lane.Color,
		Members: lane.Members, State: lane.State, Delivery: h.deliveryOf(lane.Server), Chats: n, Excluded: len(lane.Excluded),
		SharedAt: lane.SharedAt,
	}
}

func (h *teamHandler) freshIndexID(lane string) (string, error) {
	var id string
	var err error
	if h.mintChat != nil {
		id, err = h.mintChat()
	} else {
		var buf [8]byte
		_, err = readChatEntropy(buf[:])
		id = hex.EncodeToString(buf[:])
	}
	if err != nil || len(id) != 16 || id == lane {
		return "", errors.New("team: index id")
	}
	return id, nil
}

func (h *teamHandler) laneStarts(lane, scope string) (map[string]int, error) {
	out := map[string]int{}
	if scope != "from_now" {
		return out, nil
	}
	raw, err := os.ReadFile(filepath.Join(h.dataDir, "nodes.jsonl"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}
	for id, snap := range parseNodeLog(raw) {
		if !snap.live || snap.lane != lane {
			continue
		}
		if _, n, ok := h.chatCursor(id); ok {
			out[id] = n
		}
	}
	return out, nil
}

func (h *teamHandler) laneSnapshot() ([]team.LaneShare, []team.Share, error) {
	lanes, err := h.readLanes()
	if err != nil {
		return nil, nil, err
	}
	shares, err := h.readShares()
	if err != nil {
		return nil, nil, err
	}
	return lanes, shares, nil
}

func (h *teamHandler) readLanes() ([]team.LaneShare, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return team.LoadLaneShares(h.dataDir)
}

func (h *teamHandler) writeLane(s team.LaneShare) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return team.SaveLaneShare(h.dataDir, s)
}

func (h *teamHandler) removeLane(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	err := team.RemoveLaneShare(h.dataDir, id)
	if errors.Is(err, team.ErrNotFound) {
		return nil
	}
	return err
}

func (h *teamHandler) markLaneChatsStopping(indexID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	shares, err := team.LoadShares(h.dataDir)
	if err != nil {
		return err
	}
	for _, s := range shares {
		if s.Lane != indexID || s.State == "stopping" {
			continue
		}
		s.State = "stopping"
		if err := h.saveShareState(s); err != nil {
			return err
		}
	}
	return nil
}

func (h *teamHandler) kickSharesNow() {
	if h.kickShares != nil {
		h.kickShares()
	}
}

func laneByLocal(lanes []team.LaneShare, lane string) (team.LaneShare, bool) {
	for _, s := range lanes {
		if s.Lane == lane {
			return s, true
		}
	}
	return team.LaneShare{}, false
}

func sameMembers(a, b []team.Member) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func writeLaneSaveError(w http.ResponseWriter, err error) {
	if errors.Is(err, team.ErrTooManyLaneShares) {
		writeTeamError(w, http.StatusInsufficientStorage, "too_many")
		return
	}
	writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
}
