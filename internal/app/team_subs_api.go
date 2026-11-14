package app

import "net/http"

// serveSubscriptions handles /api/team/subscriptions and paths below it.
func (h *teamHandler) serveSubscriptions(w http.ResponseWriter, r *http.Request) {
	writeTeamError(w, http.StatusNotFound, "not_found")
}
