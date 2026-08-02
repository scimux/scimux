package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp/codex"
	"codeberg.org/chrberger/scimux/internal/tmuxsession"
)

func writeClaudeTranscript(t *testing.T, home, sessionID string) string {
	t.Helper()
	proj := filepath.Join(home, ".claude", "projects", "-w-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

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
	killErr    bool // kill-session returns an error (delete-durability tests)
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
	case "kill-session":
		if f.killErr {
			return "", errors.New("kill failed")
		}
		return "", nil
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
	root := t.TempDir()
	home := filepath.Join(root, "home")
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := newApp(Config{
		Home:    home,
		DataDir: data,
		// Tiny launch-grace window so failure-detection tests run fast; a marker
		// in the fake pane is seen on the first poll, success falls through at once.
		LaunchGrace: 40 * time.Millisecond,
		LaunchPoll:  5 * time.Millisecond,
	}, appDeps{Server: tmuxsession.NewServerWithRunner("testsock", f.run)})
	if err != nil {
		t.Fatal(err)
	}
	return a
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

// --- /api/adopt ---

func adopt(a *app, bodyJSON string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.handleAdopt(rec, httptest.NewRequest("POST", "/api/adopt", strings.NewReader(bodyJSON)))
	return rec
}

// --- /api/nodes ---

func newNode(a *app, bodyJSON string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.handleNewNode(rec, httptest.NewRequest("POST", "/api/nodes", strings.NewReader(bodyJSON)))
	return rec
}

func containsSub(subs []string, want string) bool {
	for _, s := range subs {
		if s == want {
			return true
		}
	}
	return false
}

// --- /api/nodes/{id}/key (tmux audit ordering) ---

func keyReq(a *app, id, bodyJSON string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/nodes/"+id+"/key", strings.NewReader(bodyJSON))
	r.SetPathValue("id", id)
	a.handleKey(rec, r)
	return rec
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

func (t *fakeCodexTransport) Stdout() io.Reader { return t.stdout }

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
