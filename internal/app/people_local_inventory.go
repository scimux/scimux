package app

import (
	"net/http"
	"net/url"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

func (h *teamHandler) localPeopleInventory(w http.ResponseWriter, r *http.Request, id team.CopyID) {
	if !h.peopleFilesEnabled() {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["cursor"]) > 1 || len(query["copy_instance"]) != 1 || len(query) > 2 || (len(query) == 2 && len(query["cursor"]) != 1) {
		writeTeamError(w, http.StatusBadRequest, "invalid_inventory_query")
		return
	}
	cursor := query.Get("cursor")
	if cursor != "" && (len(cursor) != 17 || (cursor[0] != 'p' && cursor[0] != 's') || !subHex(cursor[1:], 16)) {
		writeTeamError(w, http.StatusBadRequest, "invalid_inventory_query")
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
	inventory, err := team.ReadPeopleInventory(h.dataDir, id, op.instance, string(author), cursor)
	if err != nil {
		writeTeamError(w, http.StatusConflict, "inventory_unavailable")
		return
	}
	writeJSONStatus(w, http.StatusOK, inventory)
}
