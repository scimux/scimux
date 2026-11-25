package app

import (
	"net/http"
	"net/url"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

func (h *teamHandler) localPeoplePublicationState(w http.ResponseWriter, r *http.Request, id team.CopyID, key string) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 1 || len(query["copy_instance"]) != 1 {
		writeTeamError(w, http.StatusBadRequest, "invalid_state_query")
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
	if !peopleCopyCurrentLocked(r) {
		writeTeamError(w, http.StatusConflict, "copy_changed")
		return
	}
	op := r.Context().Value(peopleCopyOperationKey{}).(*peopleCopyOperation)
	state, err := team.ReadPeopleLocalPublicationState(h.dataDir, id, op.instance, string(author), key)
	if err != nil {
		writeTeamError(w, http.StatusConflict, "publication_unavailable")
		return
	}
	writeJSONStatus(w, http.StatusOK, state)
}
