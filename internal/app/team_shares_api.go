package app

import "net/http"

// serveShares and seats are stubs. The route tests assert the real answers,
// so each of them fails until the routes exist.
func (h *teamHandler) serveShares(w http.ResponseWriter, r *http.Request) {
	_, _ = h, r
	writeTeamError(w, http.StatusInternalServerError, "share_stub")
}

func (h *teamHandler) seats(w http.ResponseWriter, r *http.Request, id string) {
	_, _, _ = h, r, id
	writeTeamError(w, http.StatusInternalServerError, "share_stub")
}
