package acp

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

// fakeAgent is an in-process ACP agent driven over pipes by the SDK, so tests
// exercise the real framing/unions with no agent CLI and no tokens.
type fakeAgent struct {
	conn *sdk.AgentSideConnection
	// prompt scripts one turn; it may stream updates and request permission
	// via a. Returning end_turn (or an error) ends the turn.
	prompt      func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error)
	newSession  func() sdk.NewSessionResponse
	mu          sync.Mutex
	lastConfig  *sdk.SetSessionConfigOptionRequest
	lastModeSet *sdk.SetSessionModeRequest
}

var _ sdk.Agent = (*fakeAgent)(nil)

func (a *fakeAgent) Initialize(ctx context.Context, _ sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	return sdk.InitializeResponse{ProtocolVersion: sdk.ProtocolVersionNumber}, nil
}
func (a *fakeAgent) NewSession(ctx context.Context, _ sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
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
