package acp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	sdk "github.com/coder/acp-go-sdk"
)

// fakeAgent is an in-process ACP agent driven over pipes by the SDK, so tests
// exercise the real framing/unions with no agent CLI and no tokens.
type fakeAgent struct {
	conn *sdk.AgentSideConnection
	// prompt scripts one turn; it may stream updates and request permission
	// via a. Returning end_turn (or an error) ends the turn.
	prompt        func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error)
	newSession    func() sdk.NewSessionResponse
	newSessionErr error // when set, NewSession fails — drives the launch-failure reaping path
	mu            sync.Mutex
	lastConfig    *sdk.SetSessionConfigOptionRequest
	lastModeSet   *sdk.SetSessionModeRequest
}

var _ sdk.Agent = (*fakeAgent)(nil)

func (a *fakeAgent) Initialize(ctx context.Context, _ sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	return sdk.InitializeResponse{ProtocolVersion: sdk.ProtocolVersionNumber}, nil
}
func (a *fakeAgent) NewSession(ctx context.Context, _ sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	if a.newSessionErr != nil {
		return sdk.NewSessionResponse{}, a.newSessionErr
	}
	if a.newSession != nil {
		return a.newSession(), nil
	}
	return sdk.NewSessionResponse{SessionId: sdk.SessionId("sess_test")}, nil
}
func (a *fakeAgent) Prompt(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
	if a.prompt != nil {
		return a.prompt(a, ctx, p)
	}
	return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
}
func (a *fakeAgent) Authenticate(ctx context.Context, _ sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error) {
	return sdk.AuthenticateResponse{}, nil
}
func (a *fakeAgent) Logout(ctx context.Context, _ sdk.LogoutRequest) (sdk.LogoutResponse, error) {
	return sdk.LogoutResponse{}, nil
}
func (a *fakeAgent) Cancel(ctx context.Context, _ sdk.CancelNotification) error { return nil }
func (a *fakeAgent) CloseSession(ctx context.Context, _ sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	return sdk.CloseSessionResponse{}, nil
}
func (a *fakeAgent) ListSessions(ctx context.Context, _ sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	return sdk.ListSessionsResponse{}, nil
}
func (a *fakeAgent) ResumeSession(ctx context.Context, _ sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	return sdk.ResumeSessionResponse{}, nil
}
func (a *fakeAgent) SetSessionConfigOption(ctx context.Context, p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	a.mu.Lock()
	a.lastConfig = &p
	a.mu.Unlock()
	return sdk.SetSessionConfigOptionResponse{}, nil
}
func (a *fakeAgent) SetSessionMode(ctx context.Context, p sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	a.mu.Lock()
	a.lastModeSet = &p
	a.mu.Unlock()
	return sdk.SetSessionModeResponse{}, nil
}

// fakeProcess wires the client and agent connections over two pipes.
type fakeProcess struct {
	stdin  io.WriteCloser
	stdout io.Reader
	r1     *io.PipeReader
	w2     *io.PipeWriter
	closed chan struct{}
	once   sync.Once
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *fakeProcess) Stdout() io.Reader     { return p.stdout }
func (p *fakeProcess) Kill() error {
	p.once.Do(func() {
		p.stdin.Close()
		p.w2.Close()
		p.r1.Close()
		close(p.closed)
	})
	return nil
}
func (p *fakeProcess) Wait() error { <-p.closed; return nil }

// fakeRunner returns a Runner that stands up the given agent in-process.
func fakeRunner(agent *fakeAgent) Runner {
	return func(nodeID, agentName, dir string) (Process, error) {
		r1, w1 := io.Pipe() // client -> agent
		r2, w2 := io.Pipe() // agent -> client
		ac := sdk.NewAgentSideConnection(agent, w2, r1)
		agent.conn = ac
		return &fakeProcess{stdin: w1, stdout: r2, r1: r1, w2: w2, closed: make(chan struct{})}, nil
	}
}

// waitFor polls cond up to a timeout; fails the test otherwise.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newManager(t *testing.T, agent *fakeAgent) *Manager {
	t.Helper()
	return NewManagerWithRunner(t.TempDir(), fakeRunner(agent))
}

func TestBasicTurn(t *testing.T) {
	agent := &fakeAgent{
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.UpdateAgentMessageText("Hello ")})
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.UpdateAgentMessageText("world")})
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn to finish", func() bool { return m.Live("n1") == "quiet" })
	turns := m.Turns("n1")
	if len(turns) != 2 || turns[0].Role != "user" || turns[0].Text != "hi" ||
		turns[1].Role != "assistant" || turns[1].Text != "Hello world" {
		t.Fatalf("unexpected turns: %+v", turns)
	}
	if m.LastError("n1") != "" {
		t.Fatalf("unexpected error: %q", m.LastError("n1"))
	}
}

func TestInterruptCancelsActiveTurn(t *testing.T) {
	cancelled := make(chan struct{})
	agent := &fakeAgent{
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			<-ctx.Done()
			close(cancelled)
			return sdk.PromptResponse{}, ctx.Err()
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "long"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn to become active", func() bool { return m.Live("n1") == "active" })
	if err := m.Interrupt("n1"); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("prompt context was not cancelled")
	}
	waitFor(t, "turn to finish", func() bool { return m.Live("n1") == "quiet" })
	if m.LastError("n1") == "" {
		t.Fatal("interrupt should be visible as a turn error")
	}
}

func TestPermissionApprove(t *testing.T) {
	agent := &fakeAgent{
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			resp, err := a.conn.RequestPermission(ctx, sdk.RequestPermissionRequest{
				SessionId: p.SessionId,
				ToolCall:  sdk.ToolCallUpdate{ToolCallId: "t1", Title: sdk.Ptr("run bash")},
				Options: []sdk.PermissionOption{
					{OptionId: "opt_allow", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce},
					{OptionId: "opt_reject", Name: "Reject", Kind: sdk.PermissionOptionKindRejectOnce},
				},
			})
			if err != nil {
				return sdk.PromptResponse{}, err
			}
			if resp.Outcome.Selected != nil && resp.Outcome.Selected.OptionId == "opt_allow" {
				_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.UpdateAgentMessageText("done")})
			}
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "permission prompt", func() bool { return m.Attention("n1") == "approval" })
	title, opts, ok := m.Pending("n1")
	if !ok || title != "run bash" || len(opts) != 2 {
		t.Fatalf("unexpected pending: %q %+v ok=%v", title, opts, ok)
	}
	evidence, err := m.Resolve("n1", "y")
	if err != nil {
		t.Fatal(err)
	}
	if evidence == "" {
		t.Fatal("expected audit evidence")
	}
	waitFor(t, "turn to finish", func() bool { return m.Live("n1") == "quiet" && m.Attention("n1") == "" })
	turns := m.Turns("n1")
	if len(turns) == 0 || turns[len(turns)-1].Text != "done" {
		t.Fatalf("expected approved output, got %+v", turns)
	}
}

func TestUsageAndEmptyTurn(t *testing.T) {
	agent := &fakeAgent{
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.SessionUpdate{UsageUpdate: &sdk.SessionUsageUpdate{Used: 1200, Size: 200000}}})
			// No text, no tool: an empty turn.
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn to finish", func() bool { return m.Live("n1") == "quiet" })
	if used, window := m.Usage("n1"); used != 1200 || window != 200000 {
		t.Fatalf("usage = %d/%d, want 1200/200000", used, window)
	}
	if m.LastError("n1") == "" {
		t.Fatal("expected empty-turn error to be surfaced")
	}
}

func TestEffortConfigOption(t *testing.T) {
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			ung := sdk.SessionConfigSelectOptionsUngrouped{
				{Value: "low", Name: "Low"},
				{Value: "high", Name: "High"},
			}
			return sdk.NewSessionResponse{
				SessionId: "sess_test",
				ConfigOptions: []sdk.SessionConfigOption{
					{Select: &sdk.SessionConfigOptionSelect{
						Id: "effort", Name: "Effort", CurrentValue: "low",
						Options: sdk.SessionConfigSelectOptions{Ungrouped: &ung},
					}},
				},
			}
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", "high"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "config option set", func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return agent.lastConfig != nil && agent.lastConfig.ValueId != nil && agent.lastConfig.ValueId.Value == "high"
	})
}

func TestRestartHistoryReadOnly(t *testing.T) {
	agent := &fakeAgent{
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.UpdateAgentMessageText("remembered")})
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		},
	}
	dir := t.TempDir()
	m := NewManagerWithRunner(dir, fakeRunner(agent))
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn to finish", func() bool { return m.Live("n1") == "quiet" })
	m.Kill("n1")

	// A fresh Manager over the same log dir has no live session but still
	// serves read-only history and reports the node as exited.
	m2 := NewManagerWithRunner(dir, fakeRunner(agent))
	if m2.HasSession("n1") {
		t.Fatal("did not expect a live session after restart")
	}
	if m2.Live("n1") != "exited" {
		t.Fatalf("live = %q, want exited", m2.Live("n1"))
	}
	turns := m2.Turns("n1")
	if len(turns) < 2 || turns[1].Text != "remembered" {
		t.Fatalf("expected history from log, got %+v", turns)
	}
}

func TestSendToDeadSession(t *testing.T) {
	m := newManager(t, &fakeAgent{})
	if err := m.Send("missing", "hi"); err != ErrNoSession {
		t.Fatalf("err = %v, want ErrNoSession", err)
	}
}

// A log write failure must not be swallowed: the failing record does not count
// as turn output, and the failure becomes the session's visible error rather
// than a quiet, successful-looking empty turn (finding 51).
func TestAppendFailureIsVisible(t *testing.T) {
	dir := t.TempDir()
	// A directory where the log file should be makes every append fail.
	badPath := filepath.Join(dir, "n1.jsonl")
	if err := os.Mkdir(badPath, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Session{nodeID: "n1", logw: &logWriter{Path: badPath}}
	s.mu.Lock()
	s.assistant.WriteString("real output that cannot be persisted")
	s.flushAssistantLocked()
	hadOutput := s.turnHadOutput
	s.mu.Unlock()

	if hadOutput {
		t.Error("turnHadOutput set for an assistant record that did not persist")
	}
	if s.LastError() == "" {
		t.Error("expected a visible error after a log write failure")
	}
}

// After a node is published, a first-prompt delivery failure must be visible in
// the node's own history and error state, not a silent empty success
// (finding 52).
func TestRecordStartFailureSurfaces(t *testing.T) {
	agent := &fakeAgent{}
	dir := t.TempDir()
	m := NewManagerWithRunner(dir, fakeRunner(agent))
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	m.RecordStartFailure("n1", errors.New("subprocess exited before send"))

	if le := m.LastError("n1"); le == "" {
		t.Error("expected LastError after a start failure")
	}
	// The failure is also durable in the authoritative log (survives restart).
	found := false
	for _, ev := range readEvents(filepath.Join(dir, "n1.jsonl")) {
		if ev.T == "error" {
			found = true
		}
	}
	if !found {
		t.Error("expected an error event persisted to the log")
	}
}

// When the authoritative log is the failing component, RecordStartFailure
// cannot make the failure durable and must report that to the caller, so the
// creation path can avoid reporting a clean success (finding 59).
func TestRecordStartFailureReportsUndurableLog(t *testing.T) {
	dir := t.TempDir()
	m := NewManagerWithRunner(dir, fakeRunner(&fakeAgent{}))
	// Occupy the node's log path with a directory: any O_WRONLY append fails
	// with EISDIR, independent of the test's uid.
	if err := os.Mkdir(filepath.Join(dir, "n1.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordStartFailure("n1", errors.New("subprocess exited before send")); err == nil {
		t.Error("expected RecordStartFailure to report the log-write failure")
	}
}

// Delivering an option that no longer matches the current pending request is
// refused rather than answering a since-replaced prompt (finding 53).
func TestDeliverRejectsStaleOption(t *testing.T) {
	s := &Session{nodeID: "n1", logw: &logWriter{Path: filepath.Join(t.TempDir(), "n1.jsonl")}}
	s.pending = &pendingPermission{
		toolTitle: "run bash",
		options:   []sdk.PermissionOption{{OptionId: "opt_allow", Name: "Allow"}},
		ch:        make(chan sdk.PermissionOptionId, 1),
	}
	if err := s.deliver(sdk.PermissionOptionId("opt_gone")); err == nil {
		t.Error("expected delivery of an unknown option to be refused")
	}
	// A valid option is consumed and clears the pending request.
	if err := s.deliver(sdk.PermissionOptionId("opt_allow")); err != nil {
		t.Fatalf("valid deliver failed: %v", err)
	}
	if _, _, ok := s.pendingInfo(); ok {
		t.Error("pending should be cleared after a successful deliver")
	}
	if err := s.deliver(sdk.PermissionOptionId("opt_allow")); err != ErrNoPending {
		t.Errorf("second deliver err = %v, want ErrNoPending", err)
	}
}

// Clear = the structured /clear: a fresh session on the same subprocess,
// recorded as a path-less source seam appended only after session/new
// succeeded. The chat surface (the log's current segment) restarts; the
// prior conversation stays behind the seam in the same file.
func TestClearStartsFreshSegment(t *testing.T) {
	agent := &fakeAgent{
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.UpdateAgentMessageText("answer")})
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		},
	}
	dir := t.TempDir()
	m := NewManagerWithRunner(dir, fakeRunner(agent))
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn to finish", func() bool { return m.Live("n1") == "quiet" })

	if err := m.Clear("n1"); err != nil {
		t.Fatal(err)
	}
	seg := sessionlog.ReadSegment(filepath.Join(dir, "n1.jsonl"))
	if len(seg.Turns) != 0 || seg.PriorTurns != 2 || seg.StartTime == "" {
		t.Fatalf("post-clear segment: turns=%d prior=%d start=%q",
			len(seg.Turns), seg.PriorTurns, seg.StartTime)
	}
	// The /clear seam is tagged as a real page-turn boundary (reason "clear"),
	// not a mechanical rebind, and carries the fresh session id — uniform with
	// Claude and codex so a chain renderer never has to special-case transports.
	evs := sessionlog.ReadEvents(filepath.Join(dir, "n1.jsonl"))
	last := evs[len(evs)-1]
	if last.T != "source" || last.Source == nil || last.Source.Reason != "clear" || last.Source.SessionID == "" {
		t.Fatalf("clear seam = %+v, want reason=clear with a fresh session id", last.Source)
	}
	// The fresh session accepts the next turn (same process, new context).
	if err := m.Send("n1", "again"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second turn", func() bool { return m.Live("n1") == "quiet" })
	if turns := sessionlog.ReadSegment(filepath.Join(dir, "n1.jsonl")).Turns; len(turns) != 2 {
		t.Fatalf("fresh segment turns: %+v", turns)
	}
}

// After a /clear the retired session shares the log writer with its
// replacement; a straggler from the dying process (a late usage or tool
// event) must not land after the seam, where it would poison the fresh
// segment — most concretely its context gauge (finding 91).
func TestClearFencesRetiredSessionWrites(t *testing.T) {
	agent := &fakeAgent{}
	dir := t.TempDir()
	m := NewManagerWithRunner(dir, fakeRunner(agent))
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	old := m.session("n1")
	if err := m.Clear("n1"); err != nil {
		t.Fatal(err)
	}
	// The dying process reports usage after the page turn.
	old.mu.Lock()
	persisted := old.appendLocked(Event{T: "usage", Usage: &UsageEvent{Used: 9000, Size: 10000}})
	old.mu.Unlock()
	if persisted {
		t.Error("retired session's write claimed to persist")
	}
	seg := sessionlog.ReadSegment(filepath.Join(dir, "n1.jsonl"))
	if seg.Used != 0 || seg.Size != 0 {
		t.Fatalf("fresh segment gauge poisoned by retired session: used=%d size=%d", seg.Used, seg.Size)
	}
	// The replacement session still writes normally.
	fresh := m.session("n1")
	fresh.mu.Lock()
	ok := fresh.appendLocked(Event{T: "usage", Usage: &UsageEvent{Used: 1, Size: 100}})
	fresh.mu.Unlock()
	if !ok {
		t.Fatal("fresh session's write failed")
	}
	if seg := sessionlog.ReadSegment(filepath.Join(dir, "n1.jsonl")); seg.Used != 1 {
		t.Fatalf("fresh session's usage not visible: %+v", seg)
	}
}

// A launch that fails after the meta header was written must remove the
// meta-only log it just created: the leftover file would make the slug read
// as taken-by-dead-history forever, silently minting study-2, study-3, … on
// every retry against a broken agent (R20.3).
func TestLaunchFailureRemovesMetaOnlyLog(t *testing.T) {
	agent := &fakeAgent{newSessionErr: errors.New("agent not authenticated")}
	dir := t.TempDir()
	m := NewManagerWithRunner(dir, fakeRunner(agent))
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err == nil {
		t.Fatal("launch should have failed")
	}
	if _, err := os.Stat(filepath.Join(dir, "n1.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("meta-only log left behind by a failed launch: stat err = %v", err)
	}
	// The slug is reusable: a retry against a working agent launches cleanly
	// under the same node id.
	agent.newSessionErr = nil
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatalf("retry launch: %v", err)
	}
	defer m.Shutdown()
	if _, err := os.Stat(filepath.Join(dir, "n1.jsonl")); err != nil {
		t.Fatalf("retry did not create the log: %v", err)
	}
}

// RecordStartFailure must not recreate a session log that no longer exists:
// the node was deleted (its history archived) while the first prompt was in
// flight, and a fresh meta-less file would resurrect the dead slug as an
// orphan (R20.2).
func TestRecordStartFailureSkipsMissingLog(t *testing.T) {
	dir := t.TempDir()
	m := NewManagerWithRunner(dir, fakeRunner(&fakeAgent{}))
	if err := m.RecordStartFailure("gone", errors.New("subprocess killed by delete")); err != nil {
		t.Fatalf("RecordStartFailure on a deleted node: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("RecordStartFailure recreated a deleted node's log: stat err = %v", err)
	}
}

// The retire fence must hold for every write path of the dying session, not
// just the streamed-update ones: the duplicate-permission error was the one
// call site writing to the shared log directly, so a straggler permission
// request in the Clear window landed after the seam (R20.1).
func TestRetiredSessionDuplicatePermissionErrorFenced(t *testing.T) {
	agent := &fakeAgent{}
	dir := t.TempDir()
	m := NewManagerWithRunner(dir, fakeRunner(agent))
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	old := m.session("n1")
	if err := m.Clear("n1"); err != nil {
		t.Fatal(err)
	}
	// A straggler permission request arrives on the dying connection while an
	// earlier one is still marked pending: the duplicate is cancelled, and its
	// error record must be dropped by the fence, not written after the seam.
	old.mu.Lock()
	old.pending = &pendingPermission{toolTitle: "stale", ch: make(chan sdk.PermissionOptionId, 1)}
	old.mu.Unlock()
	resp, err := old.RequestPermission(context.Background(), sdk.RequestPermissionRequest{})
	if err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	if resp.Outcome.Cancelled == nil {
		t.Fatalf("duplicate permission request outcome = %+v, want cancelled", resp.Outcome)
	}
	evs := readEvents(filepath.Join(dir, "n1.jsonl"))
	seam := -1
	for i, ev := range evs {
		if ev.T == "source" {
			seam = i
		}
	}
	if seam < 0 {
		t.Fatal("no source seam after /clear")
	}
	for _, ev := range evs[seam:] {
		if ev.T == "error" {
			t.Fatalf("retired session's error record landed in the fresh segment: %+v", ev)
		}
	}
}

// A /clear racing an active turn is refused like a second Send: one turn at
// a time also guards the session swap.
func TestClearDuringActiveTurn(t *testing.T) {
	release := make(chan struct{})
	agent := &fakeAgent{
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn active", func() bool { return m.Live("n1") == "active" })
	if err := m.Clear("n1"); err != ErrTurnActive {
		t.Fatalf("clear during turn: err = %v, want ErrTurnActive", err)
	}
	close(release)
	waitFor(t, "turn to finish", func() bool { return m.Live("n1") == "quiet" })
}
