package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp"
	"codeberg.org/chrberger/scimux/internal/acp/codex"
	"codeberg.org/chrberger/scimux/internal/tmuxsession"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// fakeTmux is a scriptable tmux runner: it records every invocation and answers
// per subcommand, so the HTTP handlers can be driven with no real tmux server.
// The runner receives the full argv the Server builds ("-L <sock> <subcmd> …").
type fakeTmux struct {
	mu         sync.Mutex
	calls      [][]string
	list       []string        // list-sessions output (session names)
	alive      map[string]bool // has-session result per exact name
	capture    string          // capture-pane output
	captureErr bool
	// captureAfterEnter, when set, is returned by capture-pane once an Enter
	// keypress was sent — it lets SendAck observe a pane "reaction".
	captureAfterEnter string
	enterSent         bool
}

func (f *fakeTmux) run(ctx context.Context, stdin string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	sub := ""
	if len(args) >= 3 {
		sub = args[2]
	}
	switch sub {
	case "list-sessions":
		return strings.Join(f.list, "\n"), nil
	case "has-session":
		name := strings.TrimSuffix(strings.TrimPrefix(lastArg(args), "="), ":")
		if f.alive[name] {
			return "", nil
		}
		return "", errors.New("can't find session")
	case "capture-pane":
		if f.captureErr {
			return "", errors.New("capture failed")
		}
		if f.enterSent && f.captureAfterEnter != "" {
			return f.captureAfterEnter, nil
		}
		return f.capture, nil
	case "send-keys":
		if lastArg(args) == "Enter" {
			f.enterSent = true
		}
		return "", nil
	case "display-message":
		return "12345", nil // pane_pid / pane_current_path stand-in
	default:
		// new-session, load-buffer, paste-buffer, send-keys: accepted, no output.
		return "", nil
	}
}

func (f *fakeTmux) subcommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	subs := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		if len(c) >= 3 {
			subs = append(subs, c[2])
		}
	}
	return subs
}

func lastArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[len(args)-1]
}

func newTestApp(t *testing.T, f *fakeTmux) *app {
	t.Helper()
	dir := t.TempDir()
	return &app{
		byID:        map[string]*Node{},
		live:        map[string]string{},
		attn:        map[string]string{},
		prevCap:     map[string]string{},
		lastChg:     map[string]time.Time{},
		activeSince: map[string]time.Time{},
		tailers:     map[string]*transcript.Tailer{},
		mirrors:     map[string]*mirror{},
		pathClaims:  map[string]bool{},
		chatMark:    map[string]chatMark{},
		staleChat:   map[string]bool{},
		sendState:   map[string]string{},
		server:      tmuxsession.NewServerWithRunner("testsock", f.run),
		acp:         acpManager{acp.NewManager(filepath.Join(dir, "sessions"))},
		codex:       codexManager{codex.NewManager(filepath.Join(dir, "sessions"))},
		storePath:   filepath.Join(dir, "nodes.jsonl"),
		sessionsDir: filepath.Join(dir, "sessions"),
		home:        dir,
	}
}

func keyRecords(t *testing.T, path string) []storeRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var recs []storeRecord
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r storeRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			recs = append(recs, r)
		}
	}
	return recs
}

// --- /api/state ---

func TestHandleStateETagAndUnadopted(t *testing.T) {
	f := &fakeTmux{list: []string{"node-a", "ghost"}}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "node-a", Title: "A", Agent: "claude", Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z"}}
	a.byID["node-a"] = a.nodes[0]

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("state code = %d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag header")
	}
	var body struct {
		Unadopted []string `json:"unadopted"`
		Nodes     []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Nodes) != 1 || body.Nodes[0].ID != "node-a" {
		t.Errorf("nodes = %+v", body.Nodes)
	}
	if len(body.Unadopted) != 1 || body.Unadopted[0] != "ghost" {
		t.Errorf("unadopted = %v, want [ghost] (node-a is accounted for)", body.Unadopted)
	}

	// A matching If-None-Match short-circuits to 304 with no body.
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	a.handleState(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: code = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 must have empty body, got %q", rec2.Body.String())
	}
}

// --- /api/adopt ---

func adopt(a *app, bodyJSON string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.handleAdopt(rec, httptest.NewRequest("POST", "/api/adopt", strings.NewReader(bodyJSON)))
	return rec
}

func TestHandleAdoptValidation(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"live1": true, "dup": true, "claimer": true}}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "dup", Title: "dup", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"},
		{ID: "owner", Title: "owner", Agent: "codex", Transcript: "/t/shared.jsonl", CreatedAt: "2026-07-14T00:00:00Z"}}
	for _, n := range a.nodes {
		a.byID[n.ID] = n
	}

	if rec := adopt(a, `{}`); rec.Code != 400 {
		t.Errorf("missing session: code = %d, want 400", rec.Code)
	}
	if rec := adopt(a, `{"session":"ghost"}`); rec.Code != 404 {
		t.Errorf("dead session: code = %d, want 404", rec.Code)
	}
	if rec := adopt(a, `{"session":"dup"}`); rec.Code != 409 {
		t.Errorf("duplicate: code = %d, want 409", rec.Code)
	}
	// Codex uses the app-server protocol — adoption is rejected with a clear error.
	if rec := adopt(a, `{"session":"live1","agent":"codex","session_id":"x"}`); rec.Code != 400 {
		t.Errorf("codex adopt: code = %d, want 400", rec.Code)
	}
	// Path claim still enforced for claude.
	if rec := adopt(a, `{"session":"claimer","agent":"claude","transcript":"/t/shared.jsonl"}`); rec.Code != 409 {
		t.Errorf("path claim: code = %d, want 409", rec.Code)
	}

	// Success: a claude session with an explicit id and dir.
	rec := adopt(a, `{"session":"live1","agent":"claude","session_id":"sid","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("adopt success: code = %d body %q", rec.Code, rec.Body.String())
	}
	if _, ok := a.byID["live1"]; !ok {
		t.Error("adopted node not registered")
	}
}

// --- /api/nodes ---

func newNode(a *app, bodyJSON string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.handleNewNode(rec, httptest.NewRequest("POST", "/api/nodes", strings.NewReader(bodyJSON)))
	return rec
}

func TestHandleNewNodeValidationAndCreate(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)

	if rec := newNode(a, `{bad json`); rec.Code != 400 {
		t.Errorf("bad json: code = %d, want 400", rec.Code)
	}
	if rec := newNode(a, `{"agent":"claude","dir":"`+a.home+`"}`); rec.Code != 400 {
		t.Errorf("empty title: code = %d, want 400", rec.Code)
	}
	if rec := newNode(a, `{"title":"T","prompt":"hi","agent":"martian","dir":"`+a.home+`"}`); rec.Code != 400 {
		t.Errorf("unknown agent: code = %d, want 400", rec.Code)
	}
	// No failing request should have reached tmux new-session.
	for _, s := range f.subcommands() {
		if s == "new-session" {
			t.Fatal("a validation failure launched tmux")
		}
	}

	rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	if len(a.nodes) != 1 {
		t.Fatalf("want 1 node created, got %d", len(a.nodes))
	}
	sawNewSession := false
	for _, s := range f.subcommands() {
		if s == "new-session" {
			sawNewSession = true
		}
	}
	if !sawNewSession {
		t.Error("create did not start a tmux session")
	}
	if got := a.nodes[0].Prompt; got != "T" {
		t.Errorf("prompt default = %q, want title", got)
	}
	if got := a.nodes[0].Description; got != "T" {
		t.Errorf("description default = %q, want first prompt", got)
	}
}

func TestHandleNodeLaneAssignmentIsOneWay(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"T": true}}
	a := newTestApp(t, f)
	rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	id := a.nodes[0].ID

	req := httptest.NewRequest("PATCH", "/api/nodes/"+id, strings.NewReader(`{"lane_id":"lane-a"}`))
	req.SetPathValue("id", id)
	up := httptest.NewRecorder()
	a.handleUpdateNode(up, req)
	if up.Code != 200 {
		t.Fatalf("assign lane: code = %d body %q", up.Code, up.Body.String())
	}
	if got := a.nodes[0].LaneID; got != "lane-a" {
		t.Fatalf("lane = %q, want lane-a", got)
	}

	req = httptest.NewRequest("PATCH", "/api/nodes/"+id, strings.NewReader(`{"lane_id":"lane-b"}`))
	req.SetPathValue("id", id)
	up = httptest.NewRecorder()
	a.handleUpdateNode(up, req)
	if up.Code != 409 {
		t.Fatalf("reassign lane: code = %d body %q, want 409", up.Code, up.Body.String())
	}
	if got := a.nodes[0].LaneID; got != "lane-a" {
		t.Fatalf("lane changed to %q", got)
	}
}

func TestHandleNewNodeInheritsParentLane(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	parent := newNode(a, `{"title":"Parent","agent":"claude","dir":"`+a.home+`","lane_id":"lane-a"}`)
	if parent.Code != 200 {
		t.Fatalf("parent: code = %d body %q", parent.Code, parent.Body.String())
	}
	child := newNode(a, `{"title":"Child","parent":"`+a.nodes[0].ID+`","description":"follow","agent":"claude","dir":"`+a.home+`"}`)
	if child.Code != 200 {
		t.Fatalf("child: code = %d body %q", child.Code, child.Body.String())
	}
	if got := a.nodes[1].LaneID; got != "lane-a" {
		t.Fatalf("child lane = %q, want lane-a", got)
	}
}

func TestHandleUpdateAndDeleteNode(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"T": true}}
	a := newTestApp(t, f)
	rec := newNode(a, `{"title":"T","description":"first","agent":"claude","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	id := a.nodes[0].ID

	req := httptest.NewRequest("PATCH", "/api/nodes/"+id, strings.NewReader(`{"description":"edited"}`))
	req.SetPathValue("id", id)
	up := httptest.NewRecorder()
	a.handleUpdateNode(up, req)
	if up.Code != 200 {
		t.Fatalf("update: code = %d body %q", up.Code, up.Body.String())
	}
	if got := a.nodes[0].Description; got != "edited" {
		t.Fatalf("description = %q, want edited", got)
	}
	req = httptest.NewRequest("PATCH", "/api/nodes/"+id, strings.NewReader(`{"title":"Renamed"}`))
	req.SetPathValue("id", id)
	up = httptest.NewRecorder()
	a.handleUpdateNode(up, req)
	if up.Code != 200 {
		t.Fatalf("title update: code = %d body %q", up.Code, up.Body.String())
	}
	if got := a.nodes[0].Title; got != "Renamed" {
		t.Fatalf("title = %q, want Renamed", got)
	}

	delReq := httptest.NewRequest("DELETE", "/api/nodes/"+id, nil)
	delReq.SetPathValue("id", id)
	del := httptest.NewRecorder()
	a.handleDeleteNode(del, delReq)
	if del.Code != 200 {
		t.Fatalf("delete: code = %d body %q", del.Code, del.Body.String())
	}
	if len(a.nodes) != 0 {
		t.Fatalf("nodes after delete = %d, want 0", len(a.nodes))
	}
	sawKill := false
	for _, s := range f.subcommands() {
		if s == "kill-session" {
			sawKill = true
		}
	}
	if !sawKill {
		t.Fatal("delete did not close owned tmux session")
	}
}

// --- /api/nodes/{id}/key (tmux audit ordering) ---

func keyReq(a *app, id, bodyJSON string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/nodes/"+id+"/key", strings.NewReader(bodyJSON))
	r.SetPathValue("id", id)
	a.handleKey(rec, r)
	return rec
}

func TestHandleKeyTmuxEvidenceBeforeAction(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"k1": true}, capture: "some output\nApprove? (y/n)\n"}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "k1", Title: "k1", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}}
	a.byID["k1"] = a.nodes[0]

	// Disallowed key rejected before any tmux contact.
	if rec := keyReq(a, "k1", `{"key":"q"}`); rec.Code != 400 {
		t.Errorf("disallowed key: code = %d, want 400", rec.Code)
	}
	if rec := keyReq(a, "missing", `{"key":"y"}`); rec.Code != 404 {
		t.Errorf("missing node: code = %d, want 404", rec.Code)
	}

	// Happy path: capture-pane (evidence) must precede send-keys (action), and
	// the audit record must carry the pane excerpt.
	rec := keyReq(a, "k1", `{"key":"y"}`)
	if rec.Code != 200 {
		t.Fatalf("key: code = %d body %q", rec.Code, rec.Body.String())
	}
	subs := f.subcommands()
	capIdx, keyIdx := -1, -1
	for i, s := range subs {
		if s == "capture-pane" && capIdx == -1 {
			capIdx = i
		}
		if s == "send-keys" && keyIdx == -1 {
			keyIdx = i
		}
	}
	if capIdx == -1 || keyIdx == -1 || capIdx > keyIdx {
		t.Fatalf("capture must precede send-keys, got order %v", subs)
	}
	recs := keyRecords(t, a.storePath)
	var got *storeRecord
	for i := range recs {
		if recs[i].Type == "key" {
			got = &recs[i]
		}
	}
	if got == nil || got.Key != "y" || !strings.Contains(got.Excerpt, "Approve?") {
		t.Fatalf("audit record = %+v, want key y with pane excerpt", got)
	}
}

func TestHandleKeyRefusesWithoutEvidence(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"k2": true}, captureErr: true}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "k2", Title: "k2", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}}
	a.byID["k2"] = a.nodes[0]

	rec := keyReq(a, "k2", `{"key":"y"}`)
	if rec.Code != 500 {
		t.Fatalf("capture failure: code = %d, want 500", rec.Code)
	}
	for _, s := range f.subcommands() {
		if s == "send-keys" {
			t.Fatal("send-keys must not run when evidence capture failed")
		}
	}
}

// --- /api/nodes/{id}/peek ---

func TestHandlePeekTmux(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"p1": true}, capture: "PANE CONTENT"}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "p1", Title: "p1", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}}
	a.byID["p1"] = a.nodes[0]

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/p1/peek", nil)
	r.SetPathValue("id", "p1")
	a.handlePeek(rec, r)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "PANE CONTENT") {
		t.Fatalf("peek = %d %q", rec.Code, rec.Body.String())
	}

	f.captureErr = true
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/api/nodes/p1/peek", nil)
	r2.SetPathValue("id", "p1")
	a.handlePeek(rec2, r2)
	if !strings.Contains(rec2.Body.String(), "session exited or unavailable") {
		t.Errorf("peek on dead pane = %q", rec2.Body.String())
	}
}

// --- /api/nodes/{id}/chat (tmux fallback) ---

func TestHandleChatTmuxFallbackNoTranscript(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "c1", Title: "c1", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}}
	a.byID["c1"] = a.nodes[0]

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/c1/chat", nil)
	r.SetPathValue("id", "c1")
	a.handleChat(rec, r)
	if rec.Code != 200 {
		t.Fatalf("chat code = %d", rec.Code)
	}
	var body struct {
		Pending  bool   `json:"pending"`
		Fallback bool   `json:"fallback"`
		Source   string `json:"source"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Pending || !body.Fallback || body.Source != "none" {
		t.Errorf("no-transcript chat = %+v, want pending+fallback, source none", body)
	}
}

// --- Codex app-server HTTP integration tests (finding 78) ---
//
// These tests drive handleNewNode, handleChat, handleSend, and handleKey for
// nodes with transport:"codex". They use a fake spawn function that returns an
// in-process transport backed by a scripted goroutine, so no real codex CLI is
// needed and no tokens are consumed.

// fakeCodexTransport implements codex.Transport over two pipes. The test's
// fake-server goroutine owns the server side; the codex client owns the client
// side.
type fakeCodexTransport struct {
	stdin     *io.PipeWriter
	stdout    *io.PipeReader
	closeOnce sync.Once
}

func (t *fakeCodexTransport) Stdin() io.WriteCloser { return t.stdin }
func (t *fakeCodexTransport) Stdout() io.Reader     { return t.stdout }
func (t *fakeCodexTransport) Close() error {
	t.closeOnce.Do(func() {
		_ = t.stdin.Close()
		_ = t.stdout.Close()
	})
	return nil
}

// fakeCodexServer is shared state for the inline fake app-server goroutine.
// All access to methods goes through locks so tests can read safely under race.
type fakeCodexServer struct {
	mu   sync.Mutex
	reqs []string
}

func (s *fakeCodexServer) record(method string) {
	s.mu.Lock()
	s.reqs = append(s.reqs, method)
	s.mu.Unlock()
}

// requests returns a snapshot of all recorded method names under the lock,
// safe to call concurrently with the server goroutine (finding 82).
func (s *fakeCodexServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]string, len(s.reqs))
	copy(cp, s.reqs)
	return cp
}

// fakeCodexSpawnBase is the shared server loop for the two spawn variants.
// unblock, if non-nil, is a channel the fake blocks on before completing
// turn/start; turnStarted, if non-nil, is closed once turn/start is received.
func fakeCodexSpawnBase(srv *fakeCodexServer, threadID, rolloutPath string, turnStarted chan<- struct{}, unblock <-chan struct{}) codex.SpawnFunc {
	return func(nodeID, dir string) (codex.Transport, error) {
		serverR, clientW := io.Pipe()
		clientR, serverW := io.Pipe()
		tr := &fakeCodexTransport{stdin: clientW, stdout: clientR}
		go func() {
			defer serverW.Close()
			sc := bufio.NewScanner(serverR)
			for sc.Scan() {
				line := sc.Text()
				var req map[string]json.RawMessage
				if json.Unmarshal([]byte(line), &req) != nil {
					continue
				}
				method := strings.Trim(string(req["method"]), `"`)
				srv.record(method)
				rawID := req["id"]
				switch method {
				case "initialize":
					fmt.Fprintf(serverW, `{"id":%s,"result":{"userAgent":"fake"}}`+"\n", rawID)
				case "thread/start":
					fmt.Fprintf(serverW, `{"id":%s,"result":{"thread":{"id":%q,"path":%q},"model":"fake-model","approvalPolicy":"on-request"}}`+"\n",
						rawID, threadID, rolloutPath)
				case "turn/start":
					fmt.Fprintf(serverW, `{"id":%s,"result":{"turn":{}}}`+"\n", rawID)
					if turnStarted != nil {
						close(turnStarted)
						turnStarted = nil // prevent double-close on subsequent turn/start
					}
					if unblock != nil {
						<-unblock
					}
					fmt.Fprintf(serverW, `{"method":"item/completed","params":{"item":{"type":"agentMessage","text":"pong"}}}`+"\n")
					fmt.Fprintf(serverW, `{"method":"turn/completed","params":{}}`+"\n")
				}
			}
		}()
		return tr, nil
	}
}

// newFakeCodexSpawn returns a SpawnFunc that completes turns immediately and
// a thread-safe requests() accessor for assertions (finding 82).
func newFakeCodexSpawn(t *testing.T, threadID, rolloutPath string) (codex.SpawnFunc, func() []string) {
	t.Helper()
	srv := &fakeCodexServer{}
	return fakeCodexSpawnBase(srv, threadID, rolloutPath, nil, nil), srv.requests
}

// newBlockingFakeCodexSpawn returns a SpawnFunc that signals turnStarted
// when it receives turn/start and then blocks until unblock is closed —
// giving the test a reliable window where the first turn is provably in flight.
func newBlockingFakeCodexSpawn(t *testing.T, threadID, rolloutPath string) (codex.SpawnFunc, func() []string, <-chan struct{}, chan struct{}) {
	t.Helper()
	srv := &fakeCodexServer{}
	turnStarted := make(chan struct{})
	unblock := make(chan struct{})
	return fakeCodexSpawnBase(srv, threadID, rolloutPath, turnStarted, unblock), srv.requests, turnStarted, unblock
}

func newCodexTestApp(t *testing.T, threadID, rolloutPath string) (*app, func() []string) {
	t.Helper()
	f := &fakeTmux{}
	a := newTestApp(t, f)
	spawn, requests := newFakeCodexSpawn(t, threadID, rolloutPath)
	logDir := filepath.Join(filepath.Dir(a.storePath), "codex")
	a.codex = codexManager{codex.NewManagerWithSpawn(logDir, spawn)}
	t.Cleanup(a.codex.Shutdown)
	return a, requests
}

func TestHandleNewNodeCodexCreatesNode(t *testing.T) {
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	a, requests := newCodexTestApp(t, "THREAD-HTTP", rollout)

	rec := newNode(a, `{"prompt":"research this","title":"R","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("codex create: code = %d body %q", rec.Code, rec.Body.String())
	}
	// Node must be registered with transport:"codex".
	if len(a.nodes) != 1 || a.nodes[0].Transport != "codex" {
		t.Fatalf("node not registered or wrong transport: %+v", a.nodes)
	}
	// Manager must have run both initialize AND thread/start (finding 83):
	// Launch is synchronous for both calls, so both are recorded before
	// newNode returns 200.
	rlist := requests()
	var sawInit, sawThread bool
	for _, m := range rlist {
		if m == "initialize" {
			sawInit = true
		}
		if m == "thread/start" {
			sawThread = true
		}
	}
	if !sawInit || !sawThread {
		t.Fatalf("want both initialize and thread/start, got: %v", rlist)
	}
	// SessionID must be the thread id from thread/start.
	if a.nodes[0].SessionID != "THREAD-HTTP" {
		t.Errorf("session id = %q, want THREAD-HTTP", a.nodes[0].SessionID)
	}
}

func TestHandleChatCodexSourceACP(t *testing.T) {
	// procChat must set source:"acp" so the browser renders the structured view.
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	a, _ := newCodexTestApp(t, "THREAD-CHAT", rollout) // requests not needed here

	// Create the node first.
	rec := newNode(a, `{"title":"Q","prompt":"q","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d %s", rec.Code, rec.Body)
	}
	n := a.nodes[0]

	// Wait briefly for the turn goroutine (first prompt) to complete.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && a.codex.Live(n.ID) == "active" {
		time.Sleep(10 * time.Millisecond)
	}

	req := httptest.NewRequest("GET", "/api/nodes/"+n.ID+"/chat", nil)
	req.SetPathValue("id", n.ID)
	rec2 := httptest.NewRecorder()
	a.handleChat(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("chat: code = %d %s", rec2.Code, rec2.Body)
	}
	var body struct {
		Source  string       `json:"source"`
		Turns   []any        `json:"turns"`
		Pending bool         `json:"pending"`
		Perms   []PermOption `json:"perm_options"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Source != "acp" {
		t.Errorf("source = %q, want acp", body.Source)
	}
	if body.Pending {
		t.Error("pending must be false for structured node")
	}
}

func TestHandleSendCodexConflict(t *testing.T) {
	// A second /send while a Codex turn is in flight must return exactly 409
	// (not 200, not 500). We use a blocking fake server so the first turn is
	// provably still active when we issue the second request (finding 83).
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	spawn, _, turnStarted, unblock := newBlockingFakeCodexSpawn(t, "THREAD-SEND", rollout)
	f := &fakeTmux{}
	a := newTestApp(t, f)
	logDir := filepath.Join(filepath.Dir(a.storePath), "codex")
	a.codex = codexManager{codex.NewManagerWithSpawn(logDir, spawn)}
	t.Cleanup(a.codex.Shutdown)

	rec := newNode(a, `{"title":"X","prompt":"do x","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d", rec.Code)
	}
	n := a.nodes[0]

	// Wait until the fake server has acknowledged turn/start so we know the
	// first turn is provably in flight (turnActive == true).
	select {
	case <-turnStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first turn to start")
	}

	// Second send while first turn is active must return 409.
	sendReq := httptest.NewRequest("POST", "/api/nodes/"+n.ID+"/send", strings.NewReader(`{"text":"second"}`))
	sendReq.SetPathValue("id", n.ID)
	sendRec := httptest.NewRecorder()
	a.handleSend(sendRec, sendReq)
	if sendRec.Code != 409 {
		t.Fatalf("second send: code = %d, want 409", sendRec.Code)
	}

	// Unblock the first turn so the manager can clean up before temp-dir removal.
	close(unblock)
}

// newApprovalFakeCodexSpawn builds a SpawnFunc whose fake server sends
// item/commandExecution/requestApproval after acknowledging turn/start, then
// waits for the client's decision response before sending turn/completed.
// approvalDispatched is closed once the approval request is written to the
// pipe; decisionDelivered is closed once the peer's response is read back.
func newApprovalFakeCodexSpawn(t *testing.T, threadID, rolloutPath string) (
	codex.SpawnFunc, <-chan struct{}, <-chan struct{},
) {
	t.Helper()
	approvalDispatched := make(chan struct{})
	decisionDelivered := make(chan struct{})
	spawn := func(nodeID, dir string) (codex.Transport, error) {
		serverR, clientW := io.Pipe()
		clientR, serverW := io.Pipe()
		tr := &fakeCodexTransport{stdin: clientW, stdout: clientR}
		go func() {
			defer serverW.Close()
			sc := bufio.NewScanner(serverR)
			for sc.Scan() {
				var req map[string]json.RawMessage
				if json.Unmarshal([]byte(sc.Text()), &req) != nil {
					continue
				}
				method := strings.Trim(string(req["method"]), `"`)
				rawID := req["id"]
				switch method {
				case "initialize":
					fmt.Fprintf(serverW, `{"id":%s,"result":{"userAgent":"fake"}}`+"\n", rawID)
				case "thread/start":
					fmt.Fprintf(serverW, `{"id":%s,"result":{"thread":{"id":%q,"path":%q},"model":"fake-model","approvalPolicy":"on-request"}}`+"\n",
						rawID, threadID, rolloutPath)
				case "turn/start":
					fmt.Fprintf(serverW, `{"id":%s,"result":{"turn":{}}}`+"\n", rawID)
					// Send an approval request to the client and wait for its response.
					fmt.Fprintf(serverW, `{"id":"srv-1","method":"item/commandExecution/requestApproval","params":{"command":"rm -rf /","reason":"test","availableDecisions":["accept","cancel"]}}`+"\n")
					close(approvalDispatched)
					if sc.Scan() { // reads the peer's approval-response line
						close(decisionDelivered)
					}
					fmt.Fprintf(serverW, `{"method":"item/completed","params":{"item":{"type":"agentMessage","text":"approved"}}}`+"\n")
					fmt.Fprintf(serverW, `{"method":"turn/completed","params":{}}`+"\n")
				}
			}
		}()
		return tr, nil
	}
	return spawn, approvalDispatched, decisionDelivered
}

func TestHandleKeyCodexAuditBeforeDeliver(t *testing.T) {
	// For a codex node with a pending approval, the audit record must be written
	// before Deliver is called (finding 53). We verify by injecting a node whose
	// Manager.Pending returns an option and checking the store record exists.
	// Because injecting a live pending permission requires a running goroutine,
	// we verify the simpler case: a missing session returns 400/409 (not 500 and
	// not an unaudited deliver).
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	a, _ := newCodexTestApp(t, "THREAD-KEY", rollout) // requests not needed here

	// Register a codex node that has no live session (was never launched via
	// HTTP, so the manager has no session) to exercise PrepareResolve → ErrNoSession.
	n := &Node{ID: "phantom", Title: "phantom", Agent: "codex", Transport: "codex",
		Dir: a.home, CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n

	req := httptest.NewRequest("POST", "/api/nodes/phantom/key", strings.NewReader(`{"key":"y"}`))
	req.SetPathValue("id", "phantom")
	rec := httptest.NewRecorder()
	a.handleKey(rec, req)
	// ErrNoSession is a Conflict → 400/409 (not 500 which would indicate a key
	// was delivered without evidence).
	if rec.Code == 500 {
		t.Fatalf("handleKey: unexpected 500 on missing session: %q", rec.Body)
	}
	if rec.Code == 200 {
		t.Fatal("handleKey must not succeed when PrepareResolve fails")
	}
	// No "key" audit record should exist — the store must not have been written
	// without a successful PrepareResolve.
	recs := keyRecords(t, a.storePath)
	for _, r := range recs {
		if r.Type == "key" {
			t.Fatalf("key audit record written despite PrepareResolve failure: %+v", r)
		}
	}
}

func TestHandleKeyCodexAuditPositive(t *testing.T) {
	// Positive /key path for a live Codex turn: the audit record must be written
	// to the store BEFORE the decision is delivered to the agent (finding 53/86).
	// Verified by observing that the fake server receives the decision response
	// only after /key has returned 200 and the audit record already exists.
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	spawn, approvalDispatched, decisionDelivered := newApprovalFakeCodexSpawn(t, "THREAD-KEY-POS", rollout)
	f := &fakeTmux{}
	a := newTestApp(t, f)
	logDir := filepath.Join(filepath.Dir(a.storePath), "codex")
	a.codex = codexManager{codex.NewManagerWithSpawn(logDir, spawn)}
	t.Cleanup(a.codex.Shutdown)

	// Create the node; the first prompt triggers turn/start → approval request.
	rec := newNode(a, `{"prompt":"do it","title":"T","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d", rec.Code)
	}
	n := a.nodes[0]

	// Wait for the fake server to dispatch the approval request.
	select {
	case <-approvalDispatched:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for approval to be dispatched")
	}

	// Poll until the session's pending field is populated; there is a brief
	// scheduling gap between the pipe write completing and approve() setting
	// s.pending.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := a.codex.Pending(n.ID); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, ok := a.codex.Pending(n.ID); !ok {
		t.Fatal("timed out waiting for pending approval to appear")
	}

	// POST /key y: maps to the first non-rejecting decision ("accept"), writes
	// the audit record, then delivers the decision.
	keyReq := httptest.NewRequest("POST", "/api/nodes/"+n.ID+"/key",
		strings.NewReader(`{"key":"y"}`))
	keyReq.SetPathValue("id", n.ID)
	keyRec := httptest.NewRecorder()
	a.handleKey(keyRec, keyReq)
	if keyRec.Code != 200 {
		t.Fatalf("handleKey: code = %d body %q", keyRec.Code, keyRec.Body)
	}

	// The audit record must be in the store immediately after /key returns 200
	// (written before Deliver — the HTTP handler persists first, then delivers).
	recs := keyRecords(t, a.storePath)
	var found *storeRecord
	for i := range recs {
		if recs[i].Type == "key" {
			found = &recs[i]
			break
		}
	}
	if found == nil {
		t.Fatal("no key audit record in store after /key returned 200")
	}
	if found.Key != "y" {
		t.Errorf("audit key = %q, want y", found.Key)
	}
	if found.Excerpt == "" {
		t.Error("audit evidence (excerpt) must not be empty")
	}
	if found.ID != n.ID {
		t.Errorf("audit node id = %q, want %q", found.ID, n.ID)
	}

	// The fake server must receive the decision. Since store append happens before
	// Deliver, which happens before approve() returns, which happens before the
	// server reads the response, the ordering guarantee is transitive.
	select {
	case <-decisionDelivered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for decision to reach fake server")
	}
}

func TestCodexManagerConflictAndPending(t *testing.T) {
	// Verify the codexManager adapter methods (Conflict, Pending) are correct
	// (finding 78 — the adapter methods were 0% covered).
	cm := codexManager{codex.NewManager(t.TempDir())}

	// Conflict must return true for the sentinel errors.
	if !cm.Conflict(codex.ErrNoSession) {
		t.Error("Conflict(ErrNoSession) = false, want true")
	}
	if !cm.Conflict(codex.ErrNotAlive) {
		t.Error("Conflict(ErrNotAlive) = false, want true")
	}
	if !cm.Conflict(codex.ErrTurnActive) {
		t.Error("Conflict(ErrTurnActive) = false, want true")
	}
	if !cm.Conflict(codex.ErrNoPending) {
		t.Error("Conflict(ErrNoPending) = false, want true")
	}
	if cm.Conflict(errors.New("random")) {
		t.Error("Conflict(random) = true, want false")
	}

	// Pending on a node with no session must return ok=false.
	if title, opts, ok := cm.Pending("ghost"); ok {
		t.Errorf("Pending(ghost) = (%q, %v, true), want ok=false", title, opts)
	}
}

// TestMaybeRelinkTranscriptAfterSessionRollover: a /clear or relaunch inside
// the pane starts a new session file; once the pane finishes a working phase
// the stale link never carried, discovery must re-run and relink — otherwise
// the chat freezes on the old conversation forever (restarts replay the store
// and change nothing).
func TestMaybeRelinkTranscriptAfterSessionRollover(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}}
	a := newTestApp(t, f)
	proj := filepath.Join(a.home, ".claude", "projects", "-w-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(proj, "old-session.jsonl")
	newPath := filepath.Join(proj, "new-session.jsonl")
	for _, p := range []string{oldPath, newPath} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldPath, past, past); err != nil {
		t.Fatal(err)
	}

	n := &Node{ID: "c1", Agent: "claude", Dir: "/w/proj", Transcript: oldPath, SessionID: "old-session"}
	a.nodes = append(a.nodes, n)
	a.byID["c1"] = n
	a.activeSince["c1"] = time.Now().Add(-30 * time.Second)

	a.maybeRelinkTranscript(n)

	if n.Transcript != newPath {
		t.Fatalf("transcript = %q, want relink to %q", n.Transcript, newPath)
	}
	if n.SessionID != "new-session" {
		t.Fatalf("session id = %q, want new-session", n.SessionID)
	}
	b, err := os.ReadFile(a.storePath)
	if err != nil || !strings.Contains(string(b), newPath) {
		t.Fatalf("relink not persisted to store: %v\n%s", err, b)
	}
}

// A linked transcript that carried the phase (mtime after phase start) is
// healthy — a newer sibling session file must not steal the link.
func TestMaybeRelinkTranscriptKeepsHealthyLink(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}}
	a := newTestApp(t, f)
	proj := filepath.Join(a.home, ".claude", "projects", "-w-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(proj, "linked.jsonl")
	other := filepath.Join(proj, "other.jsonl")
	for _, p := range []string{linked, other} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	n := &Node{ID: "c1", Agent: "claude", Dir: "/w/proj", Transcript: linked}
	a.nodes = append(a.nodes, n)
	a.byID["c1"] = n
	a.activeSince["c1"] = time.Now().Add(-30 * time.Second)

	a.maybeRelinkTranscript(n)

	if n.Transcript != linked {
		t.Fatalf("healthy link was stolen: %q", n.Transcript)
	}
}

// TestHandleSendInterruptTmux: the tmux interrupt is a remote keypress and
// follows the SendKey contract — Escape (whitelisted, Claude Code's turn
// interrupt), recorded in the store with pane evidence. An unconfirmed prior
// send is withdrawn; a send still in flight keeps its state.
func TestHandleSendInterruptTmux(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"T": true}, capture: "esc to interrupt"}
	a := newTestApp(t, f)
	if rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`"}`); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := a.nodes[0].ID
	a.sendState[id] = "unconfirmed"

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/nodes/"+id+"/send/interrupt", nil)
		req.SetPathValue("id", id)
		w := httptest.NewRecorder()
		a.handleSendInterrupt(w, req)
		return w
	}
	if w := post(); w.Code != 200 {
		t.Fatalf("interrupt: %d %s", w.Code, w.Body.String())
	}

	var sentKey string
	f.mu.Lock()
	for _, c := range f.calls {
		if len(c) >= 3 && c[2] == "send-keys" {
			sentKey = c[len(c)-1]
		}
	}
	f.mu.Unlock()
	if sentKey != "Escape" {
		t.Fatalf("interrupt key = %q, want Escape", sentKey)
	}
	b, err := os.ReadFile(a.storePath)
	if err != nil || !strings.Contains(string(b), `"key":"Escape"`) || !strings.Contains(string(b), "interrupt: ") {
		t.Fatalf("interrupt not audited: %v\n%s", err, b)
	}
	if _, ok := a.sendState[id]; ok {
		t.Fatal("unconfirmed send should be withdrawn by the interrupt")
	}

	a.sendState[id] = "submitting"
	if w := post(); w.Code != 200 {
		t.Fatalf("interrupt while submitting: %d", w.Code)
	}
	if a.sendState[id] != "submitting" {
		t.Fatalf("in-flight send state = %q, want submitting kept", a.sendState[id])
	}
}

// TestHandleSendClearRetiresTranscript: /clear delivered through scimux is a
// known session rollover — the transcript link and session id are retired
// (persisted as new records), so the UI degrades to peek immediately and the
// phase-end relink can adopt the fresh session file. Ordinary prompts must
// not retire anything.
func TestHandleSendClearRetiresTranscript(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"T": true}, capture: "idle", captureAfterEnter: "cleared"}
	a := newTestApp(t, f)
	a.server.PasteDelay, a.server.AckPoll = time.Millisecond, time.Millisecond
	if rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`"}`); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	n := a.nodes[0]
	tx := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(tx, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n.Transcript, n.SessionID = tx, "sess"

	send := func(text string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/nodes/"+n.ID+"/send", strings.NewReader(`{"text":`+text+`}`))
		req.SetPathValue("id", n.ID)
		w := httptest.NewRecorder()
		a.handleSend(w, req)
		return w
	}

	if w := send(`"  /clear  "`); w.Code != 200 || !strings.Contains(w.Body.String(), "acknowledged") {
		t.Fatalf("send /clear: %d %s", w.Code, w.Body.String())
	}
	if n.Transcript != "" || n.SessionID != "" {
		t.Fatalf("link not retired: transcript=%q session=%q", n.Transcript, n.SessionID)
	}

	// The retirement must survive a restart (replay).
	a2 := &app{byID: map[string]*Node{}, storePath: a.storePath}
	if err := a2.loadStore(); err != nil {
		t.Fatal(err)
	}
	if got := a2.byID[n.ID]; got == nil || got.Transcript != "" || got.SessionID != "" {
		t.Fatalf("replay resurrected the link: %+v", got)
	}

	// An ordinary prompt never retires a link.
	n.Transcript, n.SessionID = tx, "sess"
	if w := send(`"hello"`); w.Code != 200 {
		t.Fatalf("send hello: %d %s", w.Code, w.Body.String())
	}
	if n.Transcript != tx || n.SessionID != "sess" {
		t.Fatalf("ordinary send retired the link: transcript=%q session=%q", n.Transcript, n.SessionID)
	}
}
