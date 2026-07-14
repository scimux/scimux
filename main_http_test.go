package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
		return f.capture, nil
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
		byID:         map[string]*Node{},
		live:         map[string]string{},
		attn:         map[string]string{},
		prevCap:      map[string]string{},
		lastChg:      map[string]time.Time{},
		tailers:      map[string]*transcript.Tailer{},
		rolloutSnaps: map[string]map[string]bool{},
		pathClaims:   map[string]bool{},
		chatMark:     map[string]chatMark{},
		staleChat:    map[string]bool{},
		sendState:    map[string]string{},
		server:       tmuxsession.NewServerWithRunner("testsock", f.run),
		storePath:    filepath.Join(dir, "nodes.jsonl"),
		home:         dir,
		codexRoot:    filepath.Join(dir, "codex"),
	}
}

func keyRecords(t *testing.T, path string) []storeRecord {
	t.Helper()
	b, err := os.ReadFile(path)
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
	if rec := adopt(a, `{"session":"claimer","agent":"codex","session_id":"x","transcript":"/t/shared.jsonl"}`); rec.Code != 409 {
		t.Errorf("path claim: code = %d, want 409", rec.Code)
	}

	// Success: a codex session with an explicit id and its own transcript path
	// skips the pane heuristics and persists.
	rec := adopt(a, `{"session":"live1","agent":"codex","session_id":"sid","transcript":"/t/own.jsonl"}`)
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
		t.Errorf("empty prompt: code = %d, want 400", rec.Code)
	}
	if rec := newNode(a, `{"prompt":"hi","agent":"martian","dir":"`+a.home+`"}`); rec.Code != 400 {
		t.Errorf("unknown agent: code = %d, want 400", rec.Code)
	}
	// No failing request should have reached tmux new-session.
	for _, s := range f.subcommands() {
		if s == "new-session" {
			t.Fatal("a validation failure launched tmux")
		}
	}

	rec := newNode(a, `{"prompt":"do the thing","title":"T","agent":"claude","dir":"`+a.home+`"}`)
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
