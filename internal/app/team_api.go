package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

const maxTeamJoinBody = 4 << 10

// teamHandler serves the experimental team routes. mu is held only around
// reads and writes of the server file.
type teamHandler struct {
	dataDir string
	now     func() time.Time
	client  *team.Client
	mu      sync.Mutex
	// statusBound is the time limit for one status check. Tests set it.
	// Production sets eight seconds.
	statusBound time.Duration
	// shareDelivery is the outbox's last result for one server in this
	// process. Nil means no attempt yet, which a view reports as ok.
	shareDelivery func(serverID string) string
	// removeShare replaces the share-file delete. Nil uses the real one.
	// A test sets it when the second write of one request has to fail.
	removeShare func(dataDir, uid string) error
	// saveSubs replaces subscription persistence in tests.
	saveSubs func(string, []team.Subscription) error
}

func newTeamHandler(dataDir string, now func() time.Time, client *team.Client) http.Handler {
	return &teamHandler{dataDir: dataDir, now: now, client: client, statusBound: 8 * time.Second}
}

// newTeamClient builds the client Run installs. Tests replace it.
var newTeamClient = func() *team.Client { return team.NewClient() }

type teamServerView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	SeatName string `json:"seat_name"`
	JoinedAt string `json:"joined_at"`
}

func (h *teamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/team/servers" {
		switch r.Method {
		case http.MethodGet:
			h.list(w)
		case http.MethodPost:
			h.join(w, r)
		default:
			writeTeamError(w, http.StatusNotFound, "not_found")
		}
		return
	}
	if r.URL.Path == "/api/team/shares" || strings.HasPrefix(r.URL.Path, "/api/team/shares/") {
		h.serveShares(w, r)
		return
	}
	if r.URL.Path == "/api/team/subscriptions" || strings.HasPrefix(r.URL.Path, "/api/team/subscriptions/") {
		h.serveSubscriptions(w, r)
		return
	}
	id, action, ok := splitTeamServerPath(r.URL.Path)
	if !ok {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	switch {
	case action == "" && r.Method == http.MethodDelete:
		h.forget(w, id)
	case action == "status" && r.Method == http.MethodGet:
		h.status(w, r, id)
	case action == "leave" && r.Method == http.MethodPost:
		h.leave(w, r, id)
	case action == "seats":
		h.seats(w, r, id)
	default:
		writeTeamError(w, http.StatusNotFound, "not_found")
	}
}

// splitTeamServerPath accepts /api/team/servers/{id} and the status, leave,
// and seats actions. Anything else, including a trailing slash, is refused.
func splitTeamServerPath(path string) (id, action string, ok bool) {
	rest, found := strings.CutPrefix(path, "/api/team/servers/")
	if !found || rest == "" {
		return "", "", false
	}
	id, action, hasAction := strings.Cut(rest, "/")
	if strings.Contains(action, "/") || (hasAction && action != "status" && action != "leave" && action != "seats") {
		return "", "", false
	}
	return id, action, true
}

func (h *teamHandler) list(w http.ResponseWriter) {
	entries, err := h.readServers()
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	writeJSONStatus(w, http.StatusOK, struct {
		Servers []teamServerView `json:"servers"`
	}{Servers: teamViews(entries)})
}

func (h *teamHandler) forget(w http.ResponseWriter, id string) {
	err := h.deleteServer(id)
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if errors.Is(err, team.ErrNotFound) {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
}

func (h *teamHandler) join(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Link string `json:"link"`
		Name string `json:"name"`
	}
	if err := decodeTeamJoin(w, r, &req); err != nil {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !validTeamSeatName(req.Name) {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	base, code, err := team.ParseJoinLink(req.Link)
	if err != nil {
		writeTeamError(w, http.StatusBadRequest, "invalid_link")
		return
	}
	id, err := identity.LoadInstallation(h.dataDir)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "identity_unavailable")
		return
	}
	entries, err := h.readServers()
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	hello, err := h.client.Hello(r.Context(), base)
	if err != nil {
		if errors.Is(err, team.ErrUnreachable) {
			writeTeamError(w, http.StatusBadGateway, "unreachable")
			return
		}
		writeTeamError(w, http.StatusBadGateway, "bad_server")
		return
	}
	for _, old := range entries {
		if old.ID == hello.ID {
			writeTeamError(w, http.StatusConflict, "already_joined")
			return
		}
	}
	token, registered, err := h.client.Register(r.Context(), base, code, string(id), req.Name)
	if err != nil {
		writeTeamRegisterError(w, err)
		return
	}
	if registered.ID != hello.ID {
		writeTeamError(w, http.StatusBadGateway, "bad_server")
		return
	}
	entry := team.Entry{
		ID:       hello.ID,
		Name:     registered.Name,
		URL:      base,
		SeatName: req.Name,
		Token:    token,
		JoinedAt: h.now().UTC().Format(time.RFC3339),
	}
	if err := h.writeServer(entry); err != nil {
		if errors.Is(err, team.ErrAlreadyJoined) {
			writeTeamError(w, http.StatusConflict, "already_joined")
			return
		}
		writeTeamError(w, http.StatusInternalServerError, "joined_but_not_saved")
		return
	}
	writeJSONStatus(w, http.StatusCreated, teamServerView{
		ID: entry.ID, Name: entry.Name, URL: entry.URL, SeatName: entry.SeatName, JoinedAt: entry.JoinedAt,
	})
}

func (h *teamHandler) readServers() ([]team.Entry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return team.Load(h.dataDir)
}

func (h *teamHandler) writeServer(e team.Entry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return team.Add(h.dataDir, e)
}

func (h *teamHandler) deleteServer(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return team.Remove(h.dataDir, id)
}

func (h *teamHandler) serverByID(id string) (team.Entry, error) {
	entries, err := h.readServers()
	if err != nil {
		return team.Entry{}, err
	}
	for _, e := range entries {
		if e.ID == id {
			return e, nil
		}
	}
	return team.Entry{}, team.ErrNotFound
}

func (h *teamHandler) beginServer(w http.ResponseWriter, id string) (team.Entry, bool) {
	entry, err := h.serverByID(id)
	if err == nil {
		return entry, true
	}
	if errors.Is(err, team.ErrNotFound) {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return team.Entry{}, false
	}
	writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
	return team.Entry{}, false
}

func (h *teamHandler) status(w http.ResponseWriter, r *http.Request, id string) {
	entry, ok := h.beginServer(w, id)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.statusBound)
	defer cancel()
	writeJSONStatus(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: h.serverState(ctx, entry)})
}

func (h *teamHandler) serverState(ctx context.Context, entry team.Entry) string {
	hello, err := h.client.Hello(ctx, entry.URL)
	if err != nil {
		if errors.Is(err, team.ErrUnreachable) {
			return "offline"
		}
		return "changed"
	}
	if hello.ID != entry.ID {
		return "changed"
	}
	err = h.client.Check(ctx, entry.URL, entry.Token)
	if err == nil {
		return "connected"
	}
	if errors.Is(err, team.ErrUnauthorized) {
		return "refused"
	}
	if errors.Is(err, team.ErrUnreachable) {
		return "offline"
	}
	return "changed"
}

func (h *teamHandler) leave(w http.ResponseWriter, r *http.Request, id string) {
	entry, ok := h.beginServer(w, id)
	if !ok {
		return
	}
	hello, err := h.client.Hello(r.Context(), entry.URL)
	if err != nil {
		if errors.Is(err, team.ErrUnreachable) {
			writeTeamError(w, http.StatusBadGateway, "unreachable")
			return
		}
		writeTeamError(w, http.StatusBadGateway, "bad_server")
		return
	}
	if hello.ID != entry.ID {
		writeTeamError(w, http.StatusBadGateway, "bad_server")
		return
	}
	err = h.client.Leave(r.Context(), entry.URL, entry.Token)
	if err != nil && !errors.Is(err, team.ErrUnauthorized) {
		if errors.Is(err, team.ErrUnreachable) {
			writeTeamError(w, http.StatusBadGateway, "unreachable")
			return
		}
		writeTeamError(w, http.StatusBadGateway, "bad_server")
		return
	}
	if err := h.deleteServer(id); err != nil && !errors.Is(err, team.ErrNotFound) {
		writeTeamError(w, http.StatusInternalServerError, "left_but_not_removed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeTeamRegisterError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, team.ErrJoinRefused):
		writeTeamError(w, http.StatusForbidden, "join_refused")
	case errors.Is(err, team.ErrAlreadyRegistered):
		writeTeamError(w, http.StatusConflict, "already_registered")
	case errors.Is(err, team.ErrServerFull):
		writeTeamError(w, http.StatusInsufficientStorage, "server_full")
	case errors.Is(err, team.ErrRejected):
		writeTeamError(w, http.StatusBadRequest, "bad_request")
	case errors.Is(err, team.ErrUnreachable):
		writeTeamError(w, http.StatusBadGateway, "unreachable")
	default:
		writeTeamError(w, http.StatusBadGateway, "bad_server")
	}
}

func decodeTeamJoin(w http.ResponseWriter, r *http.Request, dest any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxTeamJoinBody)
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

func validTeamSeatName(name string) bool {
	if name == "" || len(name) > 64 || name != strings.TrimSpace(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func teamViews(entries []team.Entry) []teamServerView {
	out := make([]teamServerView, 0, len(entries))
	for _, e := range entries {
		out = append(out, teamServerView{
			ID: e.ID, Name: e.Name, URL: e.URL, SeatName: e.SeatName, JoinedAt: e.JoinedAt,
		})
	}
	return out
}

func writeTeamError(w http.ResponseWriter, status int, code string) {
	writeJSONStatus(w, status, map[string]string{"error": code})
}
