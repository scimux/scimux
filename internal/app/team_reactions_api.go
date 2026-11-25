package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"unicode/utf8"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

const reactionPageSize = 256

var readReactionCopy = team.ReadCopy
var ensureReactionCopyInstance = team.EnsurePeopleCopyInstance

func bindReactionCopyInstance(r *http.Request, op *peopleCopyOperation, instance string) error {
	op.instance = instance
	if !peopleCopyCurrentLocked(r) {
		return errors.New("copy changed")
	}
	return nil
}

type reactionPageResponse struct {
	Events       []team.ReactionEvent `json:"events"`
	CopyInstance string               `json:"copy_instance"`
	Last         uint64               `json:"last"`
	Next         uint64               `json:"next"`
	More         bool                 `json:"more"`
}

// serveReactions exposes the reaction-only view of the retained comments log.
// The cursor is a comments sequence; non-reaction records advance it too.
func (h *teamHandler) serveReactions(w http.ResponseWriter, r *http.Request, id team.CopyID) {
	switch r.Method {
	case http.MethodGet:
		h.readReactions(w, r, id)
	case http.MethodPost:
		h.postReaction(w, r, id)
	default:
		writeTeamError(w, http.StatusNotFound, "not_found")
	}
}

func (h *teamHandler) readReactions(w http.ResponseWriter, r *http.Request, id team.CopyID) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) > 2 {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	for key := range query {
		if key != "after" && key != "copy_instance" {
			writeTeamError(w, http.StatusBadRequest, "bad_request")
			return
		}
	}
	var after uint64
	if values, ok := query["after"]; ok {
		if len(values) != 1 || values[0] == "" {
			writeTeamError(w, http.StatusBadRequest, "bad_request")
			return
		}
		after, err = strconv.ParseUint(values[0], 10, 64)
		if err != nil || strconv.FormatUint(after, 10) != values[0] {
			writeTeamError(w, http.StatusBadRequest, "bad_request")
			return
		}
	}
	if values, ok := query["copy_instance"]; ok {
		if len(values) != 1 {
			writeTeamError(w, http.StatusConflict, "copy_instance_required")
			return
		}
		if !h.bindReactionCopy(w, r, id, values[0]) {
			return
		}
	}
	if _, _, found, _, lookupErr := h.lookupSubscription(id); lookupErr != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	} else if !found {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	instance, err := h.ensureReactionCopy(r, id)
	if err != nil {
		writeTeamError(w, http.StatusConflict, "copy_changed")
		return
	}
	records, last, err := readReactionCopy(h.dataDir, id, team.CommentsLog, after, reactionPageSize, 4<<20)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "copy_damaged")
		return
	}
	out := reactionPageResponse{Events: []team.ReactionEvent{}, CopyInstance: instance, Last: last, Next: after}
	inst, identityErr := identity.LoadInstallation(h.dataDir)
	if identityErr != nil {
		writeTeamError(w, http.StatusInternalServerError, "identity_unavailable")
		return
	}
	if out.Next > last {
		out.Next = last
	}
	for _, record := range records {
		out.Next = record.Seq
		var kind struct {
			Type string `json:"t"`
		}
		if json.Unmarshal(record.Body, &kind) != nil || kind.Type != "reaction" {
			continue
		}
		var body struct {
			Target *uint64 `json:"target"`
		}
		if json.Unmarshal(record.Body, &body) != nil || body.Target == nil || *body.Target == 0 || *body.Target >= record.Seq {
			continue
		}
		targets, _, readErr := readReactionCopy(h.dataDir, id, team.CommentsLog, *body.Target-1, 1, 4<<20)
		if readErr != nil {
			writeTeamError(w, http.StatusInternalServerError, "copy_damaged")
			return
		}
		if len(targets) != 1 || targets[0].Seq != *body.Target {
			continue
		}
		if event, valid := team.ParseReactionEvent(record, targets[0]); valid {
			event.Mine = event.Author == string(inst)
			out.Events = append(out.Events, event)
		}
	}
	out.More = out.Next < last
	writeJSONStatus(w, http.StatusOK, out)
}

// ensureReactionCopy establishes copy identity only on the reaction API path.
// Passive subscription and mirror reads must not create copy state.
func (h *teamHandler) ensureReactionCopy(r *http.Request, id team.CopyID) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !peopleCopyCurrentLocked(r) {
		return "", errors.New("copy operation retired")
	}
	instance, err := ensureReactionCopyInstance(h.dataDir, id)
	if err != nil {
		return "", err
	}
	op := r.Context().Value(peopleCopyOperationKey{}).(*peopleCopyOperation)
	if err := bindReactionCopyInstance(r, op, instance); err != nil {
		return "", err
	}
	return instance, nil
}

func (h *teamHandler) bindReactionCopy(w http.ResponseWriter, r *http.Request, id team.CopyID, instance string) bool {
	if !subHex(instance, 32) {
		writeTeamError(w, http.StatusConflict, "copy_changed")
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	op, ok := r.Context().Value(peopleCopyOperationKey{}).(*peopleCopyOperation)
	if !ok || !peopleCopyCurrentLocked(r) {
		writeTeamError(w, http.StatusConflict, "copy_changed")
		return false
	}
	current, err := team.ReadPeopleCopyInstance(h.dataDir, id)
	if err != nil || current != instance {
		writeTeamError(w, http.StatusConflict, "copy_changed")
		return false
	}
	op.instance = instance
	return true
}

func (h *teamHandler) postReaction(w http.ResponseWriter, r *http.Request, id team.CopyID) {
	if !h.bindReactionCopyFromQuery(w, r, id) {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil || !utf8.Valid(raw) {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	var req struct {
		Target *uint64 `json:"target"`
		Emoji  *string `json:"emoji"`
		Active *bool   `json:"active"`
		Key    *string `json:"key"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || req.Target == nil || req.Emoji == nil || req.Active == nil || req.Key == nil || !subHex(*req.Key, 16) {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	var extra json.RawMessage
	if dec.Decode(&extra) != io.EOF {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !validReactionID(*req.Emoji) || *req.Target == 0 {
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
	if sub.Access == "read" {
		writeTeamError(w, http.StatusForbidden, "read_only")
		return
	}
	if sub.State != "live" || !joined {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	inst, err := identity.LoadInstallation(h.dataDir)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "identity_unavailable")
		return
	}
	targets, last, err := readReactionCopy(h.dataDir, id, team.CommentsLog, *req.Target-1, 1, 4<<20)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "copy_damaged")
		return
	}
	if *req.Target > last || len(targets) != 1 || targets[0].Seq != *req.Target || !team.ValidPeopleCommentRecord(targets[0]) || targets[0].Author == string(inst) {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !h.peopleCopyCurrent(r) {
		writeTeamError(w, http.StatusConflict, "copy_changed")
		return
	}
	body, _ := json.Marshal(struct {
		Version int    `json:"v"`
		Type    string `json:"t"`
		Target  uint64 `json:"target"`
		Emoji   string `json:"emoji"`
		Active  bool   `json:"active"`
	}{1, "reaction", *req.Target, *req.Emoji, *req.Active})
	err = h.confirmed(r.Context(), server)
	var seq uint64
	if err == nil {
		current, currentErr := h.reactionAuthorityCurrent(r, id, sub, server)
		if currentErr != nil {
			writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
			return
		}
		if !current {
			writeTeamError(w, http.StatusConflict, "not_active")
			return
		}
		seq, err = h.client.AppendComment(r.Context(), server.URL, server.Token, id.Owner, id.Chat, id.Generation, "r/"+*req.Key, body)
	}
	current, currentErr := h.reactionAuthorityCurrent(r, id, sub, server)
	if currentErr != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	if !current {
		writeTeamError(w, http.StatusConflict, "copy_changed")
		return
	}
	switch {
	case err == nil:
		writeJSONStatus(w, http.StatusOK, struct {
			Seq uint64 `json:"seq"`
		}{seq})
	case errors.Is(err, team.ErrStaleGeneration), errors.Is(err, team.ErrNoChannel):
		writeTeamError(w, http.StatusConflict, "not_active")
	case errors.Is(err, team.ErrForbidden):
		writeTeamError(w, http.StatusForbidden, "read_only")
	case errors.Is(err, team.ErrUnauthorized):
		writeTeamError(w, http.StatusBadGateway, "refused")
	case errors.Is(err, team.ErrUnreachable):
		writeTeamError(w, http.StatusBadGateway, "offline")
	default:
		writeTeamError(w, http.StatusBadGateway, "changed")
	}
}

func (h *teamHandler) reactionAuthorityCurrent(r *http.Request, id team.CopyID, original team.Subscription, server team.Entry) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !peopleCopyCurrentLocked(r) {
		return false, nil
	}
	entries, subs, err := h.loadSubState()
	if err != nil {
		return false, err
	}
	serverCurrent := false
	for _, entry := range entries {
		if peopleSameServer(entry, server) {
			serverCurrent = true
			break
		}
	}
	if !serverCurrent {
		return false, nil
	}
	for _, sub := range subs {
		if sameCopy(sub, id) {
			return sub.State == "live" && sub.Access != "read" && original.State == sub.State && original.Access == sub.Access, nil
		}
	}
	return false, nil
}

func (h *teamHandler) bindReactionCopyFromQuery(w http.ResponseWriter, r *http.Request, id team.CopyID) bool {
	query, err := url.ParseQuery(r.URL.RawQuery)
	values := query["copy_instance"]
	if err != nil || len(values) != 1 || len(query) != 1 {
		writeTeamError(w, http.StatusConflict, "copy_instance_required")
		return false
	}
	return h.bindReactionCopy(w, r, id, values[0])
}

func validReactionID(value string) bool {
	switch value {
	case "celebration", "thinking", "laughing", "heart", "thumbs_down", "thumbs_up":
		return true
	default:
		return false
	}
}
