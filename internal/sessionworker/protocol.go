// Package sessionworker defines the private, versioned control protocol between
// the replaceable muxer and one long-lived agent-session worker. The protocol
// contains only behaviours already exercised by scimux's current harnesses;
// conversation history remains the append-only session log, not an RPC.
package sessionworker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

const (
	ProtocolMajor = 1
	ProtocolMinor = 0

	headerToken = "X-Scimux-Worker-Token"
	headerMajor = "X-Scimux-Worker-Major"
	headerMinor = "X-Scimux-Worker-Minor"
	// The public JSON boundary is 1 MiB. With HTML escaping disabled, Go's JSON
	// encoder can still expand raw U+2028/U+2029 from three UTF-8 bytes to a
	// six-byte escape. Twice the public bound covers that worst case; the fixed
	// allowance covers the private envelope plus resolved Unix paths, UUIDs,
	// timestamps and enum labels without leaving the endpoint unbounded.
	publicJSONBodyMax          = 1 << 20
	privateEnvelopeMetadataMax = 64 << 10
	maxBody                    = 2*publicJSONBodyMax + privateEnvelopeMetadataMax
)

// versionOneCapabilities is immutable for the life of major 1. Capabilities
// appended to capabilities in a later minor are optional: callers feature-gate
// them instead of rejecting an older worker that owns a live conversation.
var versionOneCapabilities = []string{
	"launch-v1", "send-v1", "clear-v1", "interrupt-v1", "permission-v1",
	"state-v1", "peek-v1", "start-failure-v1", "stop-v1",
	"append-session-event-v1", "resolve-delivery-v1", "auto-approve-v1",
}

var capabilities = append([]string(nil), versionOneCapabilities...)

// CurrentCapabilities returns a copy so callers cannot alter the protocol's
// compatibility declaration.
func CurrentCapabilities() []string { return append([]string(nil), capabilities...) }

// CheckCompatibility names the v1 common denominator, not a build number.
// Newer workers may advertise a higher minor and extra capabilities; old and
// new muxers can therefore coexist as long as every action the current web UI
// already uses remains present. Removing or redefining one requires a major.
func CheckCompatibility(hello Hello) error {
	if hello.Major != ProtocolMajor || hello.Minor < 0 {
		return fmt.Errorf("session worker: incompatible protocol %d.%d", hello.Major, hello.Minor)
	}
	have := make(map[string]bool, len(hello.Capabilities))
	for _, capability := range hello.Capabilities {
		have[capability] = true
	}
	for _, required := range versionOneCapabilities {
		if !have[required] {
			return fmt.Errorf("session worker: missing capability %s", required)
		}
	}
	return nil
}

type Link struct {
	Socket string `json:"socket"`
	Token  string `json:"token"`
}

// Identity is descriptive compatibility data, not a harness operation. It
// lets a muxer reject a locator that points at the wrong session worker.
type Identity struct {
	WorkerID string `json:"worker_id"`
	Agent    string `json:"agent"`
	Build    string `json:"build,omitempty"`
}

type Hello struct {
	Identity
	Major        int      `json:"major"`
	Minor        int      `json:"minor"`
	Capabilities []string `json:"capabilities"`
}

type LaunchRequest struct {
	NodeID      string `json:"node_id"`
	Agent       string `json:"agent"`
	Parent      string `json:"parent,omitempty"`
	Title       string `json:"title,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	Description string `json:"description,omitempty"`
	Rationale   string `json:"rationale,omitempty"`
	LaneID      string `json:"lane_id,omitempty"`
	ForkKind    string `json:"fork_kind,omitempty"`
	Dir         string `json:"dir"`
	Model       string `json:"model,omitempty"`
	Effort      string `json:"effort,omitempty"`
	Transport   string `json:"transport,omitempty"`
	// SessionID is populated only when the caller already minted the identity,
	// as the Claude harness does before launching its tmux pane.
	SessionID string `json:"session_id,omitempty"`
	// Existing seeds a worker around a tmux pane that predates workers. These
	// fields are ignored by structured harnesses and on an ordinary launch.
	Existing       bool   `json:"existing,omitempty"`
	Transcript     string `json:"transcript,omitempty"`
	Adopted        bool   `json:"adopted,omitempty"`
	AXScreenReader bool   `json:"ax_screen_reader,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	HookID         string `json:"hook_id,omitempty"`
	HookGeneration int    `json:"hook_generation,omitempty"`
}

// canonicalLaunch avoids carrying the prompt twice when Description has its
// ordinary default. Recovery already defines an omitted description as the
// prompt, so this changes only the private representation, not node state.
func canonicalLaunch(req LaunchRequest) LaunchRequest {
	if req.Description == req.Prompt {
		req.Description = ""
	}
	return req
}

const (
	DeliveryAcknowledged = "acknowledged"
	DeliveryUnconfirmed  = "unconfirmed"
	DeliveryNotSent      = "not_sent"
	DeliveryPending      = "pending"
)

// Delivery reports what the harness can prove about a user action. A process
// accepting bytes is not necessarily an agent accepting a prompt: Claude's
// pane transport is the current consumer of the unconfirmed state.
type Delivery struct {
	Status string `json:"status"`
	Text   string `json:"text,omitempty"`
}

// ActionEvidence is populated when an operation emitted physical terminal
// keys. The muxer stores it in the canonical audit journal after the worker
// reports success; structured interrupts leave both fields empty.
type ActionEvidence struct {
	Evidence string   `json:"evidence,omitempty"`
	Keys     []string `json:"keys,omitempty"`
}

type PermissionOption struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

type PendingPermission struct {
	RequestID string             `json:"request_id"`
	Title     string             `json:"title"`
	ToolKind  string             `json:"tool_kind,omitempty"`
	Reason    string             `json:"reason,omitempty"`
	Options   []PermissionOption `json:"options"`
	// Dialog distinguishes a visible terminal epoch from a structured request
	// identity. Manual says no safe action list could be derived.
	Dialog bool `json:"dialog,omitempty"`
	Manual bool `json:"manual,omitempty"`
}

type PermissionBoundary struct {
	Incarnation string `json:"incarnation"`
	MaxSequence uint64 `json:"max_sequence"`
}

// State combines the bounded mechanical observations the current muxer polls.
// It intentionally excludes turns and usage: both already come from the
// durable session log and would create a second read contract over RPC.
type State struct {
	// Launch is the worker's durable recovery description. It lets a muxer
	// finish publication if its predecessor died after starting the worker but
	// before appending the global node record.
	Launch             *LaunchRequest      `json:"launch,omitempty"`
	HasSession         bool                `json:"has_session"`
	SessionID          string              `json:"session_id,omitempty"`
	Live               string              `json:"live"`
	Attention          string              `json:"attention,omitempty"`
	LastError          string              `json:"last_error,omitempty"`
	Transcript         string              `json:"transcript,omitempty"`
	HookID             string              `json:"hook_id,omitempty"`
	HookGeneration     int                 `json:"hook_generation,omitempty"`
	AXScreenReader     bool                `json:"ax_screen_reader,omitempty"`
	TurnDone           bool                `json:"turn_done,omitempty"`
	Delivery           string              `json:"delivery,omitempty"`
	Supervision        string              `json:"supervision,omitempty"`
	Pending            bool                `json:"pending,omitempty"`
	Fallback           bool                `json:"fallback,omitempty"`
	Source             string              `json:"source,omitempty"`
	Reason             string              `json:"reason,omitempty"`
	Watermark          int64               `json:"watermark,omitempty"`
	Progress           int                 `json:"progress,omitempty"`
	PendingCalls       int                 `json:"pending_calls,omitempty"`
	WaitingOn          string              `json:"waiting_on,omitempty"`
	ReplyReady         bool                `json:"reply_ready,omitempty"`
	TurnInFlight       bool                `json:"turn_in_flight,omitempty"`
	Compacting         bool                `json:"compacting,omitempty"`
	CompactTrigger     string              `json:"compact_trigger,omitempty"`
	AutoApprove        AutoApprove         `json:"auto_approve"`
	ElicitationCount   int                 `json:"elicitation_count,omitempty"`
	Elicitations       []Elicitation       `json:"elicitations,omitempty"`
	Permission         *PendingPermission  `json:"permission,omitempty"`
	PermissionBoundary *PermissionBoundary `json:"permission_boundary,omitempty"`
}

type PermissionDecision struct {
	RequestID string `json:"request_id"`
	Key       string `json:"key"`
}

type PreparedPermission struct {
	Token    string   `json:"token"`
	Evidence string   `json:"evidence"`
	Keys     []string `json:"keys,omitempty"`
}

type AutoApprove struct {
	Supported bool   `json:"supported"`
	Enabled   bool   `json:"enabled"`
	Phase     string `json:"phase"`
	Count     int    `json:"count"`
	Error     string `json:"error,omitempty"`
}

type Elicitation struct {
	Server  string `json:"server"`
	Message string `json:"message"`
	Mode    string `json:"mode,omitempty"`
	URL     string `json:"url,omitempty"`
}

// Harness is the complete per-session behaviour currently called by scimux.
// Conflict classifies an operation failure for the existing HTTP 409/500
// distinction; it is local policy, not a remotely callable action.
type Harness interface {
	Launch(context.Context, LaunchRequest) (string, error)
	Send(context.Context, string) (Delivery, error)
	Clear(context.Context) (Delivery, error)
	ResolveDelivery(context.Context) error
	Interrupt(context.Context) (ActionEvidence, error)
	PreparePermission(context.Context, PermissionDecision) (PreparedPermission, error)
	DeliverPermission(context.Context, string) error
	State(context.Context) State
	Peek(context.Context, string) string
	SetAutoApprove(context.Context, bool) (AutoApprove, error)
	RecordStartFailure(context.Context, string) error
	AppendSessionEvent(context.Context, sessionlog.Event) error
	// Stop always retires the worker. terminateSession distinguishes deleting
	// a chat from shutting down scimux: the monolith already left Claude's tmux
	// panes alive on shutdown, while a node deletion killed owned panes.
	Stop(context.Context, bool) error
	Conflict(error) bool
}

var ErrConflict = errors.New("session worker: conflict")

type remoteError struct {
	message  string
	conflict bool
}

func (e *remoteError) Error() string { return e.message }

func (e *remoteError) Is(target error) bool { return target == ErrConflict && e.conflict }

type wireError struct {
	Error string `json:"error"`
	Class string `json:"class,omitempty"`
}

type Server struct {
	link     Link
	dir      string
	ln       net.Listener
	http     *http.Server
	done     chan struct{}
	stop     chan struct{}
	once     sync.Once
	stopOnce sync.Once
	err      error
}

// Listen creates a short owner-only Unix socket directory. Its lifetime is
// owned by the worker, never by the muxer, so the endpoint survives muxer
// replacement and still avoids platform socket-path limits.
func Listen(parent string, identity Identity, harness Harness) (*Server, error) {
	if identity.WorkerID == "" || identity.Agent == "" {
		return nil, errors.New("session worker: incomplete identity")
	}
	if harness == nil {
		return nil, errors.New("session worker: nil harness")
	}
	dir, err := os.MkdirTemp(parent, "scimux-worker-")
	if err != nil {
		return nil, fmt.Errorf("session worker: create runtime directory: %w", err)
	}
	cleanup := func() { _ = os.Remove(dir) }
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return nil, fmt.Errorf("session worker: protect runtime directory: %w", err)
	}
	token, err := randomToken()
	if err != nil {
		cleanup()
		return nil, err
	}
	path := filepath.Join(dir, "worker.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("session worker: listen: %w", err)
	}

	s := &Server{link: Link{Socket: path, Token: token}, dir: dir, ln: ln, done: make(chan struct{}), stop: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hello", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, Hello{Identity: identity, Major: ProtocolMajor, Minor: ProtocolMinor, Capabilities: CurrentCapabilities()})
	})
	mux.HandleFunc("POST /v1/launch", func(w http.ResponseWriter, r *http.Request) {
		var req LaunchRequest
		if !readJSON(w, r, &req) {
			return
		}
		sid, err := harness.Launch(r.Context(), req)
		if writeHarnessError(w, harness, err) {
			return
		}
		writeJSON(w, http.StatusOK, struct {
			SessionID string `json:"session_id"`
		}{sid})
	})
	mux.HandleFunc("POST /v1/send", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text string `json:"text"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		delivery, err := harness.Send(r.Context(), req.Text)
		if writeHarnessError(w, harness, err) {
			return
		}
		writeJSON(w, http.StatusOK, delivery)
	})
	mux.HandleFunc("POST /v1/clear", func(w http.ResponseWriter, r *http.Request) {
		delivery, err := harness.Clear(r.Context())
		if writeHarnessError(w, harness, err) {
			return
		}
		writeJSON(w, http.StatusOK, delivery)
	})
	mux.HandleFunc("POST /v1/resolve-delivery", operation(harness, harness.ResolveDelivery))
	mux.HandleFunc("POST /v1/interrupt", func(w http.ResponseWriter, r *http.Request) {
		evidence, err := harness.Interrupt(r.Context())
		if writeHarnessError(w, harness, err) {
			return
		}
		writeJSON(w, http.StatusOK, evidence)
	})
	mux.HandleFunc("POST /v1/permission/prepare", func(w http.ResponseWriter, r *http.Request) {
		var req PermissionDecision
		if !readJSON(w, r, &req) {
			return
		}
		prepared, err := harness.PreparePermission(r.Context(), req)
		if writeHarnessError(w, harness, err) {
			return
		}
		writeJSON(w, http.StatusOK, prepared)
	})
	mux.HandleFunc("POST /v1/permission/deliver", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"token"`
		}
		if !readJSON(w, r, &req) || writeHarnessError(w, harness, harness.DeliverPermission(r.Context(), req.Token)) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/state", func(w http.ResponseWriter, r *http.Request) {
		state := harness.State(r.Context())
		if state.Launch != nil {
			launch := canonicalLaunch(*state.Launch)
			state.Launch = &launch
		}
		writeJSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("GET /v1/peek", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, harness.Peek(r.Context(), r.URL.Query().Get("mode")))
	})
	mux.HandleFunc("POST /v1/auto-approve", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		state, err := harness.SetAutoApprove(r.Context(), req.Enabled)
		if writeHarnessError(w, harness, err) {
			return
		}
		writeJSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("POST /v1/start-failure", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Message string `json:"message"`
		}
		if !readJSON(w, r, &req) || writeHarnessError(w, harness, harness.RecordStartFailure(r.Context(), req.Message)) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/session-event", func(w http.ResponseWriter, r *http.Request) {
		var event sessionlog.Event
		if !readJSON(w, r, &event) || writeHarnessError(w, harness, harness.AppendSessionEvent(r.Context(), event)) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/stop", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TerminateSession bool `json:"terminate_session"`
		}
		if !readJSON(w, r, &req) || writeHarnessError(w, harness, harness.Stop(r.Context(), req.TerminateSession)) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
		s.stopOnce.Do(func() { close(s.stop) })
	})
	s.http = &http.Server{Handler: authenticate(token, mux)}
	go func() {
		defer close(s.done)
		_ = s.http.Serve(ln)
	}()
	return s, nil
}

func operation(h Harness, fn func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if writeHarnessError(w, h, fn(r.Context())) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		http.Error(w, "invalid worker request", http.StatusBadRequest)
		return false
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid worker request", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
}

func writeHarnessError(w http.ResponseWriter, h Harness, err error) bool {
	if err == nil {
		return false
	}
	status, class := http.StatusInternalServerError, ""
	if h.Conflict(err) {
		status, class = http.StatusConflict, "conflict"
	}
	writeJSON(w, status, wireError{Error: err.Error(), Class: class})
	return true
}

func authenticate(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(headerToken)), []byte(token)) != 1 {
			http.Error(w, "worker capability rejected", http.StatusForbidden)
			return
		}
		major, err := strconv.Atoi(r.Header.Get(headerMajor))
		if err != nil || major != ProtocolMajor {
			w.Header().Set(headerMajor, strconv.Itoa(ProtocolMajor))
			http.Error(w, "worker protocol major mismatch", http.StatusUpgradeRequired)
			return
		}
		minor, err := strconv.Atoi(r.Header.Get(headerMinor))
		if err != nil || minor < 0 {
			http.Error(w, "invalid worker protocol minor", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session worker: generate capability: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) Link() Link {
	if s == nil {
		return Link{}
	}
	return s.link
}

// Stopped closes only after a successful protocol Stop has been answered.
// Muxer disconnects do not affect it: surviving that disconnect is the
// session-worker process's reason to exist.
func (s *Server) Stopped() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.stop
}

func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.http.Shutdown(ctx); err != nil {
			s.err = err
			_ = s.ln.Close()
		}
		<-s.done
		for _, path := range []string{s.link.Socket, s.dir} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && s.err == nil {
				s.err = err
			}
		}
	})
	return s.err
}

type Client struct {
	link      Link
	transport *http.Transport
	http      *http.Client
}

func NewClient(link Link) (*Client, error) {
	if link.Socket == "" {
		return nil, errors.New("session worker: empty socket path")
	}
	if link.Token == "" {
		return nil, errors.New("session worker: empty capability")
	}
	transport := &http.Transport{DisableCompression: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", link.Socket)
	}}
	return &Client{link: link, transport: transport, http: &http.Client{Transport: transport}}, nil
}

func (c *Client) request(ctx context.Context, method, path string, body []byte, major, minor int) (*http.Request, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("session worker: nil client")
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://scimux-worker"+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set(headerToken, c.link.Token)
	req.Header.Set(headerMajor, strconv.Itoa(major))
	req.Header.Set(headerMinor, strconv.Itoa(minor))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	var err error
	if in != nil {
		var encoded bytes.Buffer
		enc := json.NewEncoder(&encoded)
		enc.SetEscapeHTML(false)
		if err = enc.Encode(in); err != nil {
			return err
		}
		body = encoded.Bytes()
	}
	req, err := c.request(ctx, method, path, body, ProtocolMajor, ProtocolMinor)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure wireError
		if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&failure) != nil || failure.Error == "" {
			failure.Error = resp.Status
		}
		return &remoteError{message: failure.Error, conflict: resp.StatusCode == http.StatusConflict && failure.Class == "conflict"}
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) Hello(ctx context.Context) (Hello, error) {
	var out Hello
	err := c.do(ctx, http.MethodGet, "/v1/hello", nil, &out)
	if err == nil && out.Major != ProtocolMajor {
		err = fmt.Errorf("session worker: hello major %d, want %d", out.Major, ProtocolMajor)
	}
	return out, err
}

func (c *Client) Launch(ctx context.Context, req LaunchRequest) (string, error) {
	var out struct {
		SessionID string `json:"session_id"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/launch", canonicalLaunch(req), &out)
	return out.SessionID, err
}

func (c *Client) Send(ctx context.Context, text string) (Delivery, error) {
	var out Delivery
	err := c.do(ctx, http.MethodPost, "/v1/send", struct {
		Text string `json:"text"`
	}{text}, &out)
	return out, err
}

func (c *Client) Clear(ctx context.Context) (Delivery, error) {
	var out Delivery
	err := c.do(ctx, http.MethodPost, "/v1/clear", nil, &out)
	return out, err
}

func (c *Client) ResolveDelivery(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/resolve-delivery", nil, nil)
}

func (c *Client) Interrupt(ctx context.Context) (ActionEvidence, error) {
	var out ActionEvidence
	err := c.do(ctx, http.MethodPost, "/v1/interrupt", nil, &out)
	return out, err
}

func (c *Client) PreparePermission(ctx context.Context, decision PermissionDecision) (PreparedPermission, error) {
	var out PreparedPermission
	err := c.do(ctx, http.MethodPost, "/v1/permission/prepare", decision, &out)
	return out, err
}

func (c *Client) DeliverPermission(ctx context.Context, token string) error {
	return c.do(ctx, http.MethodPost, "/v1/permission/deliver", struct {
		Token string `json:"token"`
	}{token}, nil)
}

func (c *Client) State(ctx context.Context) (State, error) {
	var out State
	err := c.do(ctx, http.MethodGet, "/v1/state", nil, &out)
	return out, err
}

func (c *Client) Peek(ctx context.Context, mode string) (string, error) {
	path := "/v1/peek"
	if mode != "" {
		path += "?mode=" + url.QueryEscape(mode)
	}
	req, err := c.request(ctx, http.MethodGet, path, nil, ProtocolMajor, ProtocolMinor)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("session worker: peek: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return string(b), err
}

func (c *Client) SetAutoApprove(ctx context.Context, enabled bool) (AutoApprove, error) {
	var out AutoApprove
	err := c.do(ctx, http.MethodPost, "/v1/auto-approve", struct {
		Enabled bool `json:"enabled"`
	}{enabled}, &out)
	return out, err
}

func (c *Client) RecordStartFailure(ctx context.Context, message string) error {
	return c.do(ctx, http.MethodPost, "/v1/start-failure", struct {
		Message string `json:"message"`
	}{message}, nil)
}

func (c *Client) AppendSessionEvent(ctx context.Context, event sessionlog.Event) error {
	return c.do(ctx, http.MethodPost, "/v1/session-event", event, nil)
}

func (c *Client) Stop(ctx context.Context, terminateSession bool) error {
	return c.do(ctx, http.MethodPost, "/v1/stop", struct {
		TerminateSession bool `json:"terminate_session"`
	}{terminateSession}, nil)
}

func (c *Client) Close() error {
	if c == nil || c.transport == nil {
		return nil
	}
	c.transport.CloseIdleConnections()
	return nil
}
