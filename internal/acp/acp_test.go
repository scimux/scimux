package acp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	return func(nodeID, agentName, dir, model, effort string) (Process, error) {
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

// The two-step answer path main.go actually uses — PrepareResolve to map the
// whitelisted key and capture audit evidence, then Deliver only after the record
// is written (finding 53) — driven end to end over the real SDK connection with
// no mock: a real fakeAgent asks for permission and receives the delivered
// option. TestPermissionApprove covers the one-shot Resolve; this covers the
// split API and its intermediate invariants.
func TestPermissionPrepareResolveAndDeliver(t *testing.T) {
	got := make(chan sdk.PermissionOptionId, 1)
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
			if resp.Outcome.Selected != nil {
				got <- resp.Outcome.Selected.OptionId
				_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.UpdateAgentMessageText("approved")})
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

	// Step 1: map the key to an option and capture evidence, without delivering —
	// the agent stays blocked and the request stays pending (PrepareResolve is
	// read-only). Digit "1" selects the first option.
	optID, evidence, err := m.PrepareResolve("n1", "1")
	if err != nil {
		t.Fatalf("PrepareResolve: %v", err)
	}
	if optID != "opt_allow" {
		t.Errorf("key %q mapped to %q, want opt_allow (first option)", "1", optID)
	}
	if !strings.Contains(evidence, "run bash") {
		t.Errorf("evidence = %q, want it to carry the tool title as decision context", evidence)
	}
	if _, _, ok := m.Pending("n1"); !ok {
		t.Error("PrepareResolve must not consume the pending request")
	}

	// A delivery of an option that isn't the mapped/pending one is refused and
	// leaves the request pending (finding 53) — then the real option goes through.
	if err := m.Deliver("n1", "opt_bogus"); err == nil {
		t.Error("Deliver of an unmapped option should be refused")
	}
	if _, _, ok := m.Pending("n1"); !ok {
		t.Error("a refused delivery must leave the request pending")
	}

	// Step 2: deliver the mapped option, unblocking the agent's RequestPermission.
	if err := m.Deliver("n1", optID); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	select {
	case sel := <-got:
		if sel != "opt_allow" {
			t.Errorf("agent received option %q, want opt_allow", sel)
		}
	case <-time.After(time.Second):
		t.Fatal("agent never received the delivered option")
	}
	waitFor(t, "turn to finish", func() bool { return m.Live("n1") == "quiet" && m.Attention("n1") == "" })

	// The request is consumed and a second answer has nothing to answer.
	if _, _, err := m.PrepareResolve("n1", "1"); err != ErrNoPending {
		t.Errorf("PrepareResolve after deliver = %v, want ErrNoPending", err)
	}
	turns := m.Turns("n1")
	if len(turns) == 0 || turns[len(turns)-1].Text != "approved" {
		t.Fatalf("expected approved output, got %+v", turns)
	}
	// Peek renders the log tail (the ACP analogue of a pane photo): it carries the
	// turn's content without a live subprocess dependency.
	if peek := m.Peek("n1"); !strings.Contains(peek, "approved") {
		t.Errorf("Peek should surface the turn's output; got %q", peek)
	}
}

// PrepareResolve/Deliver against a node with no live session report ErrNoSession
// rather than panicking — the after-restart / never-launched case.
func TestPrepareResolveNoSession(t *testing.T) {
	m := newManager(t, &fakeAgent{})
	if _, _, err := m.PrepareResolve("ghost", "1"); err != ErrNoSession {
		t.Errorf("PrepareResolve(no session) = %v, want ErrNoSession", err)
	}
	if err := m.Deliver("ghost", "opt_allow"); err != ErrNoSession {
		t.Errorf("Deliver(no session) = %v, want ErrNoSession", err)
	}
}

// mapKeyToOption resolves the whitelisted dialog keys to permission options: a
// digit selects the Nth option, y/Enter the first allow, n/Escape the first
// reject, with documented fallbacks. Pure logic — no session needed.
func TestMapKeyToOption(t *testing.T) {
	opts := []sdk.PermissionOption{
		{OptionId: "allow", Kind: sdk.PermissionOptionKindAllowOnce},
		{OptionId: "reject", Kind: sdk.PermissionOptionKindRejectOnce},
	}
	cases := []struct {
		key  string
		want sdk.PermissionOptionId
		ok   bool
	}{
		{"1", "allow", true},  // digit → Nth option
		{"2", "reject", true}, //
		{"0", "", false},      // out of range low
		{"3", "", false},      // out of range high
		{"y", "allow", true},  // first allow-kind
		{"Enter", "allow", true},
		{"n", "reject", true}, // first reject-kind
		{"Escape", "reject", true},
		{"x", "", false}, // not a whitelisted key
	}
	for _, c := range cases {
		id, ok := mapKeyToOption(c.key, opts)
		if id != c.want || ok != c.ok {
			t.Errorf("mapKeyToOption(%q) = (%q, %v), want (%q, %v)", c.key, id, ok, c.want, c.ok)
		}
	}
	// No options never map, whatever the key.
	if _, ok := mapKeyToOption("y", nil); ok {
		t.Error("empty options must never map")
	}
	// Fallbacks when no option carries an allow/reject kind: y → first, n → last.
	noKind := []sdk.PermissionOption{{OptionId: "first"}, {OptionId: "last"}}
	if id, ok := mapKeyToOption("y", noKind); !ok || id != "first" {
		t.Errorf(`"y" fallback = (%q, %v), want ("first", true)`, id, ok)
	}
	if id, ok := mapKeyToOption("n", noKind); !ok || id != "last" {
		t.Errorf(`"n" fallback = (%q, %v), want ("last", true)`, id, ok)
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

// Independently synthetic Grok usage examples. Invented counters and identities
// exercise the two token layers without retaining any captured session data.

func mockGrokMetaN1() map[string]any { // single-call, top 121 vs nest 120
	return map[string]any{"modelId": "grok-4.5",
		"totalTokens": float64(121), "inputTokens": float64(100),
		"outputTokens": float64(20), "cachedReadTokens": float64(40),
		"reasoningTokens": float64(5),
		"usage": map[string]any{"inputTokens": float64(100), "outputTokens": float64(20),
			"totalTokens": float64(120), "cachedReadTokens": float64(40),
			"cacheCreationTokens": float64(0), "reasoningTokens": float64(5),
			"modelCalls": float64(1), "numTurns": float64(1), "costUsdTicks": float64(125000000)}}
}

func mockGrokMetaN3() map[string]any { // spontaneous tool, nest≈2×top
	return map[string]any{"modelId": "grok-4.5",
		"totalTokens": float64(241), "inputTokens": float64(220),
		"outputTokens": float64(20), "cachedReadTokens": float64(100), "reasoningTokens": float64(7),
		"usage": map[string]any{"inputTokens": float64(420), "outputTokens": float64(40),
			"totalTokens": float64(460), "cachedReadTokens": float64(200),
			"cacheCreationTokens": float64(0), "modelCalls": float64(2), "costUsdTicks": float64(250000000)}}
}

func mockGrokMetaT1() map[string]any { // primary red fixture — forced tool, dual-layer
	return map[string]any{"sessionId": "synthetic-grok-session",
		"requestId": "synthetic-grok-request", "modelId": "grok-4.5",
		"totalTokens": float64(501), "inputTokens": float64(480),
		"outputTokens": float64(20), "cachedReadTokens": float64(100), "reasoningTokens": float64(5),
		"usage": map[string]any{"inputTokens": float64(900), "outputTokens": float64(60),
			"totalTokens": float64(960), "cachedReadTokens": float64(200),
			"cacheCreationTokens": float64(0), "reasoningTokens": float64(12),
			"modelCalls": float64(2), "numTurns": float64(2), "costUsdTicks": float64(625000000)}}
}

func mockGrokMetaT6() map[string]any { // top-level cache 0
	return map[string]any{"modelId": "grok-4.5",
		"totalTokens": float64(701), "inputTokens": float64(680),
		"outputTokens": float64(20), "cachedReadTokens": float64(0), "reasoningTokens": float64(9),
		"usage": map[string]any{"inputTokens": float64(1300), "outputTokens": float64(40),
			"totalTokens": float64(1340), "cachedReadTokens": float64(600),
			"modelCalls": float64(2), "costUsdTicks": float64(375000000)}}
}

func mockGrokMetaT10() map[string]any {
	return map[string]any{"modelId": "grok-4.5",
		"totalTokens": float64(901), "inputTokens": float64(880),
		"outputTokens": float64(20), "cachedReadTokens": float64(800), "reasoningTokens": float64(3),
		"usage": map[string]any{"inputTokens": float64(1700), "outputTokens": float64(40),
			"totalTokens": float64(1740), "cachedReadTokens": float64(1500),
			"modelCalls": float64(2), "costUsdTicks": float64(500000000)}}
}

func mockGrokMetaNestedOnlyMega() map[string]any { // hardening: no top-level → Used=0
	return map[string]any{"modelId": "grok-4.5",
		"usage": map[string]any{"inputTokens": float64(2_000_000), "outputTokens": float64(50_000),
			"totalTokens": float64(2_050_000), "cachedReadTokens": float64(1_000_000),
			"modelCalls": float64(10), "numTurns": float64(10)}}
}

func mockGrokMetaTopOnly() map[string]any {
	return map[string]any{"totalTokens": float64(100), "inputTokens": float64(90), "outputTokens": float64(10)}
}

// Grok reports two token layers in PromptResponse._meta (fare-design §2.5 / D10):
// Layer A top-level last-call → gauge Used; Layer B nested per-prompt spend →
// breakdown+cost (fare). usageFromMeta must never drive Used from nested spend.
func TestUsageFromPromptMeta(t *testing.T) {
	// Single-call off-by-one lineage: top 81 vs nest 80 → Used is top.
	meta := map[string]any{
		"totalTokens": float64(81),
		"usage": map[string]any{
			"inputTokens":      float64(70),
			"outputTokens":     float64(10),
			"totalTokens":      float64(80),
			"cachedReadTokens": float64(30),
		},
	}
	ev := usageFromMeta(meta, 500_000)
	if ev == nil {
		t.Fatal("expected usage event")
	}
	if ev.Used != 81 || ev.Size != 500_000 {
		t.Fatalf("occupancy = %d/%d, want 81/500000 (top-level last-call)", ev.Used, ev.Size)
	}
	if ev.InputTokens != 70 || ev.OutputTokens != 10 || ev.CachedReadTokens != 30 {
		t.Fatalf("breakdown = %+v", ev)
	}
	if ev.TotalTokens != 80 {
		t.Fatalf("TotalTokens (Layer B spend) = %d, want 80", ev.TotalTokens)
	}
	// Top-level totals alone also work.
	ev = usageFromMeta(map[string]any{"totalTokens": float64(100)}, 200)
	if ev == nil || ev.Used != 100 || ev.Size != 200 {
		t.Fatalf("top-level meta = %+v", ev)
	}
	if usageFromMeta(nil, 0) != nil || usageFromMeta(map[string]any{"foo": 1}, 0) != nil {
		t.Fatal("empty meta must yield nil")
	}
	// Standard Usage field: TotalTokens becomes Used when no usage_update ran.
	total := 42
	ev = usageFromPrompt(sdk.PromptResponse{Usage: &sdk.Usage{
		InputTokens: 40, OutputTokens: 2, TotalTokens: total,
	}}, 1000)
	if ev == nil || ev.Used != 42 || ev.Size != 1000 {
		t.Fatalf("prompt usage = %+v", ev)
	}
}

// TestUsageFromMetaLastCallNotPromptSpend distinguishes last-call occupancy
// (501) from accumulated prompt spend (960), which is retained for fare.
func TestUsageFromMetaLastCallNotPromptSpend(t *testing.T) {
	ev := usageFromMeta(mockGrokMetaT1(), 500_000)
	if ev == nil {
		t.Fatal("expected usage event")
	}
	if ev.Used != 501 {
		t.Fatalf("Used = %d, want 501 (top-level last-call, not nested spend 960)", ev.Used)
	}
	if ev.Size != 500_000 {
		t.Fatalf("Size = %d, want 500000", ev.Size)
	}
	// Layer B spend telemetry still present for fare.
	if ev.TotalTokens != 960 {
		t.Fatalf("TotalTokens (nested spend) = %d, want 960", ev.TotalTokens)
	}
	if ev.InputTokens != 900 || ev.OutputTokens != 60 || ev.CachedReadTokens != 200 {
		t.Fatalf("nested breakdown = in=%d out=%d cache=%d",
			ev.InputTokens, ev.OutputTokens, ev.CachedReadTokens)
	}
	wantCost := 625000000.0 / 1e9
	if ev.CostAmount != wantCost {
		t.Fatalf("CostAmount = %v, want %v", ev.CostAmount, wantCost)
	}
	if ev.CostCurrency != "USD" {
		t.Fatalf("CostCurrency = %q, want USD", ev.CostCurrency)
	}

	// Off-by-one (N1): top 121 vs nested 120 → top wins.
	ev = usageFromMeta(mockGrokMetaN1(), 500_000)
	if ev == nil || ev.Used != 121 {
		t.Fatalf("N1 Used = %v, want 121 (top wins off-by-one)", ev)
	}

	// Nested-only mega: no top-level → Used must stay 0 (never 2_050_000).
	ev = usageFromMeta(mockGrokMetaNestedOnlyMega(), 500_000)
	if ev == nil {
		t.Fatal("nested-only mega: expected event with spend telemetry")
	}
	if ev.Used != 0 {
		t.Fatalf("nested-only mega Used = %d, want 0 (never fall back to nested for gauge)", ev.Used)
	}
	if ev.TotalTokens != 2_050_000 {
		t.Fatalf("nested-only mega TotalTokens = %d, want 2050000 (spend retained)", ev.TotalTokens)
	}
	if ev.Size != 500_000 {
		t.Fatalf("nested-only mega Size = %d, want 500000 (window still known)", ev.Size)
	}
	// Gauge must not clamp to 100%: Used/Size with Used=0 is empty, not full.
	if ev.Size > 0 && ev.Used >= ev.Size {
		t.Fatalf("gauge would paint full: Used=%d Size=%d", ev.Used, ev.Size)
	}
}

// Occupancy comes from Layer A only across synthetic usage examples.
func TestUsageFromMetaTable(t *testing.T) {
	cases := []struct {
		name               string
		meta               map[string]any
		ctxSize            int
		wantUsed, wantSize int
		// wantNil when meta carries nothing usable.
		wantNil bool
	}{
		{"N1_single", mockGrokMetaN1(), 500_000, 121, 500_000, false},
		{"N3_tool_double", mockGrokMetaN3(), 500_000, 241, 500_000, false},
		{"T1_tool_double", mockGrokMetaT1(), 500_000, 501, 500_000, false},
		{"T6_zero_top_cache", mockGrokMetaT6(), 500_000, 701, 500_000, false},
		{"T10_late_tool", mockGrokMetaT10(), 500_000, 901, 500_000, false},
		{"nested_only_mega", mockGrokMetaNestedOnlyMega(), 500_000, 0, 500_000, false},
		{"top_only", mockGrokMetaTopOnly(), 200, 100, 200, false},
		{"empty", map[string]any{"foo": 1}, 500_000, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := usageFromMeta(tc.meta, tc.ctxSize)
			if tc.wantNil {
				if ev != nil {
					t.Fatalf("got %+v, want nil", ev)
				}
				return
			}
			if ev == nil {
				t.Fatal("expected usage event")
			}
			if ev.Used != tc.wantUsed || ev.Size != tc.wantSize {
				t.Fatalf("occupancy = %d/%d, want %d/%d", ev.Used, ev.Size, tc.wantUsed, tc.wantSize)
			}
		})
	}
}

// Used > Size must never paint the gauge full (D10 hardening).
func TestUsageFromMetaUsedExceedsSizeCleared(t *testing.T) {
	ev := usageFromMeta(map[string]any{
		"totalTokens":  float64(600_000),
		"inputTokens":  float64(599_000),
		"outputTokens": float64(1000),
	}, 500_000)
	if ev == nil {
		t.Fatal("expected event")
	}
	if ev.Used != 0 {
		t.Fatalf("Used = %d, want 0 when Used>Size (never paint full)", ev.Used)
	}
	if ev.Size != 500_000 {
		t.Fatalf("Size = %d, want 500000", ev.Size)
	}
}

func TestContextSizeFromMeta(t *testing.T) {
	meta := map[string]any{
		"modelState": map[string]any{
			"currentModelId": "grok-4.5",
			"availableModels": []any{
				map[string]any{
					"modelId": "other",
					"_meta":   map[string]any{"totalContextTokens": float64(100_000)},
				},
				map[string]any{
					"modelId": "grok-4.5",
					"_meta":   map[string]any{"totalContextTokens": float64(500_000)},
				},
			},
		},
	}
	if n := contextSizeFromMeta(meta, "grok-4.5"); n != 500_000 {
		t.Fatalf("matched model size = %d", n)
	}
	if n := contextSizeFromMeta(meta, ""); n != 500_000 {
		// empty model → currentModelId path
		t.Fatalf("currentModelId size = %d", n)
	}
	if n := contextSizeFromMeta(meta, "missing"); n != 100_000 {
		t.Fatalf("fallback size = %d", n)
	}
	if n := contextSizeFromMeta(nil, "x"); n != 0 {
		t.Fatalf("nil meta = %d", n)
	}
}

// Phase 6 — Model + TurnID identity fields on grok UsageEvents (D3, D6).

// TestGrok_ModelFromMeta: usageFromMeta sets Model from _meta.modelId.
// The synthetic dual-layer mock carries "modelId":"grok-4.5".
func TestGrok_ModelFromMeta(t *testing.T) {
	ev := usageFromMeta(mockGrokMetaT1(), 500_000)
	if ev == nil {
		t.Fatal("expected usage event")
	}
	if ev.Model != "grok-4.5" {
		t.Fatalf("Model = %q, want %q (_meta.modelId)", ev.Model, "grok-4.5")
	}
	// Occupancy must stay Layer-A (Phase G) — Model is additive only.
	if ev.Used != 501 {
		t.Fatalf("Used = %d, want 501 (Phase-G occupancy must not change)", ev.Used)
	}
}

// TestGrok_TurnIDFromRequestId: usageFromMeta sets TurnID from _meta.requestId
// when present; absent → empty (seam fallback), never a crash.
func TestGrok_TurnIDFromRequestId(t *testing.T) {
	ev := usageFromMeta(mockGrokMetaT1(), 500_000)
	if ev == nil {
		t.Fatal("expected usage event")
	}
	wantID := "synthetic-grok-request"
	if ev.TurnID != wantID {
		t.Fatalf("TurnID = %q, want %q (_meta.requestId)", ev.TurnID, wantID)
	}

	// N1 has modelId but no requestId → empty TurnID (D3 seam fallback).
	ev = usageFromMeta(mockGrokMetaN1(), 500_000)
	if ev == nil {
		t.Fatal("N1: expected usage event")
	}
	if ev.TurnID != "" {
		t.Fatalf("TurnID = %q, want empty when requestId absent (seam fallback)", ev.TurnID)
	}
}

// TestGrok_ReportedCostFlowsToFare: a grok turn projected into the session log
// reaches ReadFare with ReportedCostUSD (costUsdTicks/1e9) and PerModel["grok-4.5"]
// (D6 + D8). Extends Phase-G Layer-B cost linkage.
func TestGrok_ReportedCostFlowsToFare(t *testing.T) {
	ev := usageFromMeta(mockGrokMetaT1(), 500_000)
	if ev == nil {
		t.Fatal("expected usage event")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "grok-fare.jsonl")
	w := &sessionlog.Writer{Path: path}
	if err := w.Append(sessionlog.NewMeta("g1", "grok", "grok-4.5", "low", dir)); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(sessionlog.Event{T: "usage", Usage: ev}); err != nil {
		t.Fatal(err)
	}

	f := sessionlog.ReadFare(path)
	// Layer B T1: fresh_in = 900 − 200 = 700; out=60; cache_read=200
	if f.FreshIn != 700 {
		t.Errorf("FreshIn = %d, want 700 (900−200 subtract)", f.FreshIn)
	}
	if f.CacheRead != 200 {
		t.Errorf("CacheRead = %d, want 200", f.CacheRead)
	}
	if f.Out != 60 {
		t.Errorf("Out = %d, want 60", f.Out)
	}
	wantCost := 625000000.0 / 1e9
	if f.ReportedCostUSD < wantCost-1e-12 || f.ReportedCostUSD > wantCost+1e-12 {
		t.Errorf("ReportedCostUSD = %v, want %v (costUsdTicks/1e9)", f.ReportedCostUSD, wantCost)
	}
	if !f.ReportedCostComplete {
		t.Error("ReportedCostComplete = false, want true")
	}
	pm, ok := f.PerModel["grok-4.5"]
	if !ok {
		t.Fatal(`PerModel["grok-4.5"] missing (D6: Model must land on UsageEvent)`)
	}
	if pm.FreshIn != 700 || pm.Out != 60 || pm.CacheRead != 200 {
		t.Errorf("PerModel[grok-4.5] = fresh=%d cache=%d out=%d, want 700/200/60",
			pm.FreshIn, pm.CacheRead, pm.Out)
	}
	if pm.ReportedCostUSD < wantCost-1e-12 || pm.ReportedCostUSD > wantCost+1e-12 {
		t.Errorf("PerModel cost = %v, want %v", pm.ReportedCostUSD, wantCost)
	}
	// Occupancy is never summed into fare (D1).
	if f.Turns != 1 {
		t.Errorf("Turns = %d, want 1", f.Turns)
	}
}

// End-to-end: synthetic dual-layer Grok _meta (modelCalls:2) must land
// last-call Used + window Size in Manager.Usage — not nested spend.
func TestGrokMetaUsageEndToEnd(t *testing.T) {
	agent := &fakeAgent{
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.UpdateAgentMessageText("pong")})
			return sdk.PromptResponse{
				StopReason: sdk.StopReasonEndTurn,
				Meta:       mockGrokMetaT1(),
			}, nil
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "grok", t.TempDir(), "grok-4.5", "low"); err != nil {
		t.Fatal(err)
	}
	// Inject the window the real Grok CLI advertises at initialize — the fake
	// agent has no modelState meta.
	if s := m.session("n1"); s != nil {
		s.ctxSize = 500_000
	}
	if err := m.Send("n1", "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn to finish", func() bool { return m.Live("n1") == "quiet" })
	used, window := m.Usage("n1")
	if used != 501 || window != 500_000 {
		t.Fatalf("usage = %d/%d, want 501/500000 (last-call, not nested 960)", used, window)
	}
}

// Clear must re-launch with the same model/effort the node was created with
// (fork is the only path that changes launch config).
func TestClearPropagatesModelEffort(t *testing.T) {
	var launches [][2]string
	agent := &fakeAgent{}
	runner := func(nodeID, agentName, dir, model, effort string) (Process, error) {
		launches = append(launches, [2]string{model, effort})
		return fakeRunner(agent)(nodeID, agentName, dir, model, effort)
	}
	m := NewManagerWithRunner(t.TempDir(), runner)
	if _, err := m.Launch("n1", "grok", t.TempDir(), "grok-4.5", "high"); err != nil {
		t.Fatal(err)
	}
	if err := m.Clear("n1"); err != nil {
		t.Fatal(err)
	}
	if len(launches) != 2 {
		t.Fatalf("launches = %d, want 2 (Launch+Clear)", len(launches))
	}
	for i, got := range launches {
		if got[0] != "grok-4.5" || got[1] != "high" {
			t.Errorf("launch %d model/effort = %v, want grok-4.5/high", i, got)
		}
	}
	s := m.session("n1")
	if s == nil || s.model != "grok-4.5" || s.effort != "high" {
		t.Fatalf("session after Clear = %+v", s)
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
