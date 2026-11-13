package app

import (
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
}

func newTeamHandler(dataDir string, now func() time.Time, client *team.Client) http.Handler {
	return &teamHandler{dataDir: dataDir, now: now, client: client}
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
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/team/servers":
		h.list(w)
	case r.Method == http.MethodPost && r.URL.Path == "/api/team/servers":
		h.join(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/team/servers/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/team/servers/")
		if id == "" || strings.Contains(id, "/") {
			writeTeamError(w, http.StatusNotFound, "not_found")
			return
		}
		h.forget(w, id)
	default:
		writeTeamError(w, http.StatusNotFound, "not_found")
	}
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
