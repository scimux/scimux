package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	// shareOps serializes share mutations with outbox work across network calls.
	shareOps sync.Mutex
	// statusBound is the time limit for one status check. Tests set it.
	// Production sets eight seconds.
	statusBound time.Duration
	// shareDelivery is the outbox's last result for one server in this
	// process. Nil means no attempt yet, which a view reports as ok.
	shareDelivery func(serverID string) string
	// removeShare replaces the share-file delete. Nil uses the real one.
	// A test sets it when the second write of one request has to fail.
	removeShare func(dataDir, uid string) error
	// saveShare and saveSubs replace metadata persistence in tests.
	saveShare func(string, team.Share) error
	saveSubs  func(string, []team.Subscription) error
	// seatOpen and seatRename let tests force atomic publication failures.
	seatOpen   func(string) (*os.File, error)
	seatRename func(string, string) error
	// subAsks is the last channels ask, the last seats ask, and the delivery
	// word for one server in this process. It is not stored in the state file.
	subAsks     map[string]subAsk
	removedSubs map[team.CopyID]time.Time
	// copyRemovalEpoch tracks only copies with in-flight attachment operations.
	copyRemovalEpoch map[team.CopyID]*peopleCopyRemovalEpoch
	// ownerInstance checks the current native node through the owning app.
	ownerInstance func(nodeID, instance string) bool
	// Stage storage defaults to the managed team store; tests may inject failures.
	stageRead func(string, team.CopyID, string) (team.PeopleStage, error)
	stageSave func(string, team.CopyID, team.PeopleStage) error
	// localDiscardBeforeDecision allows tests to interpose a copy replacement.
	localDiscardBeforeDecision func()
	// peopleCapabilitiesReady controls the advertised attachment capability.
	peopleCapabilitiesReady bool
	peopleProbeSerial       uint64
}

type peopleCopyRemovalEpoch struct {
	epoch      uint64
	active     int
	stages     map[string]uint64
	operations map[*peopleCopyOperation]struct{}
}

func newTeamHandler(dataDir string, now func() time.Time, client *team.Client) http.Handler {
	return &teamHandler{dataDir: dataDir, now: now, client: client, statusBound: 8 * time.Second, peopleCapabilitiesReady: true}
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
	case action == "seat" && r.Method == http.MethodPut:
		h.renameSeat(w, r, id)
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
	if strings.Contains(action, "/") || (hasAction && action != "status" && action != "leave" && action != "seats" && action != "seat") {
		return "", "", false
	}
	return id, action, true
}

func (h *teamHandler) renameSeat(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if decodeTeamJoin(w, r, &req) != nil || !validTeamSeatName(req.Name) {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
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
	if err := h.client.Rename(r.Context(), entry.URL, entry.Token, req.Name); err != nil {
		switch {
		case errors.Is(err, team.ErrUnreachable):
			writeTeamError(w, http.StatusBadGateway, "unreachable")
		case errors.Is(err, team.ErrUnauthorized):
			writeTeamError(w, http.StatusBadGateway, "refused")
		case errors.Is(err, team.ErrRejected):
			writeTeamError(w, http.StatusUnprocessableEntity, "name_refused")
		default:
			writeTeamError(w, http.StatusBadGateway, "bad_server")
		}
		return
	}
	updated, err := h.saveSeatName(entry, req.Name)
	if err != nil {
		if errors.Is(err, team.ErrNotFound) {
			writeTeamError(w, http.StatusNotFound, "not_found")
			return
		}
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	writeJSONStatus(w, http.StatusOK, teamViews([]team.Entry{updated})[0])
}

func (h *teamHandler) saveSeatName(original team.Entry, name string) (team.Entry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entries, err := team.Load(h.dataDir)
	if err != nil {
		return team.Entry{}, err
	}
	for i := range entries {
		if entries[i].ID != original.ID {
			continue
		}
		if entries[i].Token != original.Token || entries[i].URL != original.URL {
			return team.Entry{}, team.ErrNotFound
		}
		entries[i].SeatName = name
		body, _ := json.Marshal(struct {
			V       int          `json:"v"`
			Servers []team.Entry `json:"servers"`
		}{V: 1, Servers: entries})
		if len(body)+1 > 64<<10 {
			return team.Entry{}, team.ErrDamaged
		}
		open := h.seatOpen
		if open == nil {
			open = func(dir string) (*os.File, error) { return os.CreateTemp(dir, ".team-*.tmp") }
		}
		f, err := open(h.dataDir)
		if err != nil {
			return team.Entry{}, err
		}
		tmp := f.Name()
		defer os.Remove(tmp)
		_, writeErr := f.Write(append(body, '\n'))
		syncErr := f.Sync()
		closeErr := f.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			return team.Entry{}, err
		}
		rename := h.seatRename
		if rename == nil {
			rename = os.Rename
		}
		if err := rename(tmp, filepath.Join(h.dataDir, team.File)); err != nil {
			return team.Entry{}, err
		}
		return entries[i], nil
	}
	return team.Entry{}, team.ErrNotFound
}

func (h *teamHandler) setSeatIO(open func(string) (*os.File, error), rename func(string, string) error) {
	h.seatOpen = open
	h.seatRename = rename
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
