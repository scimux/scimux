package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

// discardPeoplePending performs local maintenance only. The revision is a
// concurrency precondition for the immutable pending value, not a credential.
func (h *teamHandler) discardPeoplePending(w http.ResponseWriter, r *http.Request, id team.CopyID, key string) {
	if !h.peopleFilesEnabled() {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["revision"]) != 1 || !subHex(query.Get("revision"), 64) {
		writeTeamError(w, http.StatusConflict, "revision_required")
		return
	}
	if !h.requirePeopleCopy(w, r) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.peopleFilesEnabled() || !peopleCopyCurrentLocked(r) {
		writeTeamError(w, http.StatusConflict, "copy_changed")
		return
	}
	op := r.Context().Value(peopleCopyOperationKey{}).(*peopleCopyOperation)
	pending, err := team.ReadPeopleLocalPublication(h.dataDir, id, op.instance, key)
	if err != nil {
		writeTeamError(w, http.StatusConflict, "publication_unavailable")
		return
	}
	author, err := identity.LoadInstallation(h.dataDir)
	if err != nil || pending.Author != string(author) {
		writeTeamError(w, http.StatusConflict, "identity_unavailable")
		return
	}
	raw, _ := json.Marshal(pending) // Validated pending values are JSON-encodable.
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != query.Get("revision") {
		writeTeamError(w, http.StatusConflict, "revision_changed")
		return
	}
	if h.localDiscardBeforeDecision != nil {
		h.localDiscardBeforeDecision()
	}
	seq, err := team.DiscardPeoplePending(h.dataDir, id, op.instance, pending)
	if err != nil {
		writeTeamError(w, http.StatusConflict, "publication_conflict")
		return
	}
	for other := range op.state.operations {
		if other != op && other.instance == op.instance && other.publicationKey == key {
			other.cancel()
		}
	}
	if seq != 0 {
		writeJSONStatus(w, http.StatusOK, map[string]any{"state": "already_completed", "seq": seq})
		return
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{"state": "locally_discarded"})
}
