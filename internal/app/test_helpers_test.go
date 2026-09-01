package app

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

// Timeout budgets for the Claude first-turn delivery tests.
//
// These serve two different purposes and must not be conflated — doing so is
// what made TestDeliverClaudeInitialPromptCanonicalNewlines flake:
//
//   - On a path that expects delivery to SUCCEED, the budget is only a race
//     window. It is never reached in a passing run, so it must be generous
//     enough to survive a loaded machine, a `-race` build, or a `-count=N`
//     stress run. Use testDeliverBudget / testReadyBudget.
//   - On a path that expects delivery to TIME OUT, the budget *is* the test's
//     runtime and the assertion itself. It must stay short. Use
//     testTimeoutBudget.
//
// The ordering constraint ("the confirming transcript turn is appended only
// after the paste") is expressed as an event via fakeTmux.onEnter, never as a
// second wall-clock deadline racing the first.
const (
	// testReadyBudget / testDeliverBudget bound the SessionStart wait and the
	// delivery-confirmation wait on success paths.
	testReadyBudget   = 10 * time.Second
	testDeliverBudget = 10 * time.Second
	// testTimeoutBudget is used where the timeout is the expected outcome.
	testTimeoutBudget = 25 * time.Millisecond
	// testInitialPoll is the poll interval; small so success paths are fast.
	testInitialPoll = 2 * time.Millisecond
	// testEnterWait bounds awaitEnter. Like the success budgets it is a race
	// window, not an expected duration.
	testEnterWait = 10 * time.Second
)

// AX pane fixtures used by Claude dialog-control tests. Labels must match
// the live menu; they are not the invented Yes / don't-ask / No row.
const axPermissionPane = `Permission Required: Create file
hello.txt

  1. Yes
  2. Yes, and don't ask again for this session
  3. No

Enter selection [1-3], or Escape to cancel:`

const axAskUserQuestionPane = `Which approach should we take?

  1. Keep the poller mechanical
  2. Parse the TUI
  3. Other
  4. Chat about this

Enter selection [1-4], or Escape to cancel:`

// tmuxFallbackNode is a non-Claude tmux subject. Inspect, owing, and
// quiet-fallback supervision stay on this path; Claude is gated off it.
func tmuxFallbackNode(id, path string) *Node {
	return &Node{ID: id, Agent: "pi", Transport: "tmux", Transcript: path}
}

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
	if err := appendLinesErr(path, lines...); err != nil {
		t.Fatal(err)
	}
}

// appendLinesErr is the *testing.T-free core of appendLines, for the callers
// that run off the test goroutine (where t.Fatal is not allowed).
func appendLinesErr(path string, lines ...string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			return err
		}
	}
	return nil
}

// webSourcePath resolves a repository-root browser file from this package's
// test working directory. The production copy is embedded through webFS.
func webSourcePath(path string) string {
	return filepath.Join("..", "..", path)
}

// fakeTmux is a scriptable tmux runner: it records every invocation and answers
// per subcommand, so the HTTP handlers can be driven with no real tmux server.
// The runner receives the full argv the Server builds ("-L <sock> <subcmd> …").
type fakeTmux struct {
	mu         sync.Mutex
	calls      [][]string
	list       []string        // list-sessions output (session names)
	listErr    bool            // list-sessions returns an error (poll snapshot-failure tests)
	alive      map[string]bool // has-session result per exact name
	capture    string          // capture-pane output
	captureErr bool
	killErr    bool // kill-session returns an error (delete-durability tests)
	// sendKeysErr makes send-keys fail after the call is recorded, so tests
	// can assert pre-delivery failure paths without a real tmux server.
	sendKeysErr bool
	// captureAfterEnter, when set, is returned by capture-pane once an Enter
	// keypress was sent — it lets SendAck observe a pane "reaction".
	captureAfterEnter string
	enterSent         bool
	// enterCh is closed when Enter is first sent, so a test can wait for the
	// paste as an *event* rather than guessing how long it takes (awaitEnter).
	// Created lazily because every fakeTmux is built as a struct literal.
	enterCh chan struct{}
	// enterAppend* let a test hand the fake the transcript lines to append at
	// the moment of the paste; see appendOnEnter.
	enterAppendPath  string
	enterAppendLines []string
	enterAppendErr   error
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
		if f.listErr {
			return "error connecting to /tmp/tmux-0/default", errors.New("exit status 1")
		}
		// Explicit list only (including empty). Do not derive from alive:
		// handleNewNode consults list-sessions to pick a free title slug, and
		// many tests pre-seed alive with the intended id before create — that
		// must not make the name look taken.
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
		if f.sendKeysErr {
			return "", errors.New("send-keys failed")
		}
		// Compound AX sequences end with Enter; single Escape/Tab/etc. do not.
		if lastArg(args) == "Enter" && !f.enterSent {
			f.enterSent = true
			if f.enterAppendPath != "" {
				f.enterAppendErr = appendLinesErr(f.enterAppendPath, f.enterAppendLines...)
			}
			close(f.enterSignalLocked())
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

func (f *fakeTmux) didSendEnter() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enterSent
}

// enterSignalLocked returns the channel closed on the first Enter, creating it
// on demand. The caller must hold f.mu.
func (f *fakeTmux) enterSignalLocked() chan struct{} {
	if f.enterCh == nil {
		f.enterCh = make(chan struct{})
	}
	return f.enterCh
}

// enterSignal returns a channel that is closed once Enter has been sent (and is
// already closed if it was sent before the call).
func (f *fakeTmux) enterSignal() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enterSignalLocked()
}

// awaitEnter blocks until the paste's Enter has been sent, and fails the test if
// it never is. Use it from the test goroutine in place of a polling loop: the
// delivery of Enter is an event, and waiting for the event rather than for a
// wall-clock budget is what keeps these tests from racing a loaded machine.
func (f *fakeTmux) awaitEnter(t *testing.T) {
	t.Helper()
	select {
	case <-f.enterSignal():
	case <-time.After(testEnterWait):
		t.Fatal("tmux Enter was never sent")
	}
}

// appendOnEnter makes the fake append lines to the transcript at the instant
// the paste's Enter is sent — synchronously, inside the tmux call, before Send
// returns.
//
// This is what makes the Claude first-turn delivery tests deterministic. The
// production code takes its `before` watermark and pastes, then scans for a new
// user turn until claudeDeliveryTimeout. Appending from a background goroutine
// that had been polling for the paste made the turn's arrival a second race
// against that same budget: when the machine was loaded the turn landed after
// the deadline and a test expecting confirmation saw "not_sent". Appending on
// the Enter itself puts the evidence on disk before the confirmation loop runs
// its first poll, so a matching turn always confirms and a mismatching one
// always fails to — neither outcome depends on the clock.
//
// The append happens with f.mu held; nothing here may call back into fakeTmux.
func (f *fakeTmux) appendOnEnter(t *testing.T, path string, lines ...string) {
	t.Helper()
	f.mu.Lock()
	f.enterAppendPath = path
	f.enterAppendLines = lines
	f.mu.Unlock()
	t.Cleanup(func() {
		f.mu.Lock()
		err := f.enterAppendErr
		f.mu.Unlock()
		if err != nil {
			t.Errorf("appending the transcript turn on Enter failed: %v", err)
		}
	})
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
	}, appDeps{
		Server:               tmuxsession.NewServerWithRunner("testsock", f.run),
		DeliverClaudeInitial: func(*Node) initialDelivery { return initialAcknowledged },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Most application tests exercise request, lifecycle, and rendering paths,
	// not provider-account discovery. Leave usage disabled unless a test opts in
	// with installTestUsage: otherwise a successful fake prompt starts a
	// background walk of the developer's real ~/.codex history (and may also
	// probe other provider credentials), making the suite depend on private data
	// volume and leaving work alive after the test that started it.
	a.usage = nil
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
	// Claude create returns before the background delivery goroutine finishes.
	// Tests that send immediately after newNode need the instant-ack stub to
	// have released the send gate; yield briefly. Tests that keep delivery
	// outstanding (SessionStart wait) still see submitting after this cap.
	if rec.Code == 200 && a != nil {
		var n Node
		if json.Unmarshal(rec.Body.Bytes(), &n) == nil && n.Agent == "claude" && n.ID != "" {
			waitClaudeInitialGateFor(a, n.ID, 80*time.Millisecond)
		}
	}
	return rec
}

func waitClaudeInitialGate(t *testing.T, a *app, id string) {
	t.Helper()
	// Generous: every caller expects the gate to be released, so this bound is
	// a race window and is never reached in a passing run.
	waitClaudeInitialGateFor(a, id, testDeliverBudget)
	a.mu.Lock()
	st := a.sendState[id]
	a.mu.Unlock()
	if st == sendSubmitting {
		t.Fatal("initial delivery still submitting")
	}
}

func waitClaudeInitialGateFor(a *app, id string, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		st := a.sendState[id]
		a.mu.Unlock()
		if st != sendSubmitting {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func installPreparedClaudeHook(t *testing.T, a *app, n *Node) string {
	t.Helper()
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatalf("prepare Claude hook bundle: %v", err)
	}
	a.mu.Lock()
	a.claudeHooks[n.ID] = hookID
	a.mu.Unlock()
	a.markClaudeHookAck(n.ID)
	a.noteClaudeHookCapabilitiesForNode(n.ID)
	return filepath.Join(a.claudeHooksDir(), hookID)
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
