package app

import (
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/storagebudget"
	"github.com/scimux/scimux/internal/team"
)

func (h *teamHandler) retireAbsentPeople(w http.ResponseWriter, r *http.Request, id team.CopyID, key string) {
	if !h.peopleFilesEnabled() {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 1 || len(query["copy_instance"]) != 1 {
		writeTeamError(w, http.StatusBadRequest, "invalid_query")
		return
	}
	var one [1]byte
	if n, err := r.Body.Read(one[:]); n != 0 || err != io.EOF {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !h.requirePeopleCopy(w, r) {
		return
	}
	author, err := identity.LoadInstallation(h.dataDir)
	if err != nil {
		writeTeamError(w, http.StatusConflict, "identity_unavailable")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.peopleFilesEnabled() || !peopleCopyCurrentLocked(r) {
		writeTeamError(w, http.StatusConflict, "copy_changed")
		return
	}
	op := r.Context().Value(peopleCopyOperationKey{}).(*peopleCopyOperation)
	err = team.RetireAbsentPeopleKey(h.dataDir, id, op.instance, string(author), key)
	if err != nil {
		switch {
		case errors.Is(err, team.ErrPeopleReviewRequired):
			writeTeamError(w, http.StatusConflict, "review_required")
		case errors.Is(err, storagebudget.ErrBudget):
			writeTeamError(w, http.StatusInsufficientStorage, "storage_full")
		default:
			writeTeamError(w, http.StatusConflict, "publication_unavailable")
		}
		return
	}
	for other := range op.state.operations {
		if other != op && other.instance == op.instance && other.publicationKey == key {
			other.cancel()
		}
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{"state": "locally_discarded"})
}
