package acp

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

// agentArgv is the pure command-selection seam: pi ships a dedicated binary,
// opencode exposes ACP as a subcommand, grok as `grok agent stdio` (with
// optional model/effort flags), cursor as `cursor-agent acp` (with an
// optional model flag), dsh as `dsh --profile acp`, and anything else
// has no ACP transport and must be rejected rather than silently launched.
func TestAgentArgv(t *testing.T) {
	cases := []struct {
		agent, model, effort string
		want                 []string
		wantErr              bool
	}{
		{agent: "pi", want: []string{"pi-acp"}},
		{agent: "opencode", want: []string{"opencode", "acp"}},
		{agent: "grok", want: []string{"grok", "--no-auto-update", "agent", "stdio"}},
		{agent: "grok", model: "grok-4.5",
			want: []string{"grok", "--no-auto-update", "agent", "-m", "grok-4.5", "stdio"}},
		{agent: "grok", effort: "high",
			want: []string{"grok", "--no-auto-update", "agent", "--reasoning-effort", "high", "stdio"}},
		{agent: "grok", model: "grok-4.5", effort: "low",
			want: []string{"grok", "--no-auto-update", "agent", "-m", "grok-4.5", "--reasoning-effort", "low", "stdio"}},
		// pi/opencode ignore model/effort at argv (session options own them).
		{agent: "pi", model: "x", effort: "high", want: []string{"pi-acp"}},
		{agent: "opencode", model: "y", effort: "low", want: []string{"opencode", "acp"}},
		// cursor exposes ACP as `cursor-agent acp` and takes the model as a
		// launch flag, because its session-level model option is a closed enum
		// whose values are a different namespace from the CLI's model ids.
		{agent: "cursor", want: []string{"cursor-agent", "acp"}},
		{agent: "cursor", model: "claude-opus-5-thinking-high",
			want: []string{"cursor-agent", "--model", "claude-opus-5-thinking-high", "acp"}},
		// Effort is never an argv flag for cursor: the level is already folded
		// into the model id by the catalog, so passing it here would be a
		// second, unvalidated spelling of the same choice.
		{agent: "cursor", effort: "high", want: []string{"cursor-agent", "acp"}},
		{agent: "cursor", model: "auto", effort: "xhigh",
			want: []string{"cursor-agent", "--model", "auto", "acp"}},
		// dsh selects its ACP transport with a profile name, not a subcommand;
		// the profile is what binds the ACP bundle. Model/effort are session
		// config options over the wire, never argv, so they change nothing here.
		{agent: "dsh", want: []string{"dsh", "--profile", "acp"}},
		{agent: "dsh", model: "deepseek-v4-flash", effort: "high",
			want: []string{"dsh", "--profile", "acp"}},
		{agent: "claude", wantErr: true},
		{agent: "", wantErr: true},
	}
	for _, c := range cases {
		got, err := agentArgv(c.agent, c.model, c.effort)
		if c.wantErr {
			if err == nil {
				t.Errorf("agentArgv(%q): want error, got %v", c.agent, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("agentArgv(%q,%q,%q): unexpected error %v", c.agent, c.model, c.effort, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("agentArgv(%q,%q,%q) = %v, want %v", c.agent, c.model, c.effort, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("agentArgv(%q,%q,%q) = %v, want %v", c.agent, c.model, c.effort, got, c.want)
				break
			}
		}
	}
}

// countingProcess counts Kill/Wait so a test can prove a subprocess was reaped.
type countingProcess struct {
	*fakeProcess
	kills int32
	waits int32
}

func (p *countingProcess) Kill() error { atomic.AddInt32(&p.kills, 1); return p.fakeProcess.Kill() }
func (p *countingProcess) Wait() error { atomic.AddInt32(&p.waits, 1); return p.fakeProcess.Wait() }

// countingRunner mirrors fakeRunner but hands back a countingProcess so the
// caller can observe the launch-failure reaping path.
func countingRunner(agent *fakeAgent, cp **countingProcess) Runner {
	return func(nodeID, agentName, dir, model, effort string) (Process, error) {
		r1, w1 := io.Pipe()
		r2, w2 := io.Pipe()
		agent.conn = sdk.NewAgentSideConnection(agent, w2, r1)
		p := &countingProcess{fakeProcess: &fakeProcess{stdin: w1, stdout: r2, r1: r1, w2: w2, closed: make(chan struct{})}}
		*cp = p
		return p, nil
	}
}

// A launch that fails after the process starts (here: NewSession errors) must
// kill and reap the subprocess before returning, so a failed launch never
// leaves an orphan (finding 57).
func TestLaunchReapsOnSessionFailure(t *testing.T) {
	agent := &fakeAgent{newSessionErr: errors.New("no session for you")}
	var cp *countingProcess
	m := NewManagerWithRunner(t.TempDir(), countingRunner(agent, &cp))

	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err == nil {
		t.Fatal("expected Launch to fail when NewSession errors")
	}
	if cp == nil {
		t.Fatal("runner was never invoked")
	}
	waitFor(t, "subprocess killed and reaped", func() bool {
		return atomic.LoadInt32(&cp.kills) >= 1 && atomic.LoadInt32(&cp.waits) >= 1
	})
	if m.HasSession("n1") {
		t.Error("a failed launch must not register a session")
	}
}

// A pending permission request blocks an SDK dispatch goroutine until a human
// answers. Session teardown (closeDone, driven by stop/Shutdown) must unblock
// it as cancelled rather than leaking the goroutine forever.
func TestRequestPermissionCancelledOnDone(t *testing.T) {
	s := &Session{done: make(chan struct{}), logw: &logWriter{Path: filepath.Join(t.TempDir(), "n.jsonl")}}
	respCh := make(chan sdk.RequestPermissionResponse, 1)
	go func() {
		resp, _ := s.RequestPermission(context.Background(), sdk.RequestPermissionRequest{
			ToolCall: sdk.ToolCallUpdate{ToolCallId: "t1", Title: sdk.Ptr("run bash")},
			Options:  []sdk.PermissionOption{{OptionId: "opt_allow", Name: "Allow"}},
		})
		respCh <- resp
	}()
	waitFor(t, "pending registered", func() bool { _, ok := s.pendingInfo(); return ok })

	s.closeDone() // what stop()/Shutdown() do on teardown

	select {
	case resp := <-respCh:
		if resp.Outcome.Cancelled == nil {
			t.Errorf("teardown should cancel a pending permission, got %+v", resp.Outcome)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RequestPermission stayed parked after done was closed")
	}
}

// Shutdown must stop and reap every live subprocess and drop its session, so no
// ACP child outlives scimux (plan §4.9).
func TestShutdownStopsSubprocess(t *testing.T) {
	agent := &fakeAgent{}
	var cp *countingProcess
	m := NewManagerWithRunner(t.TempDir(), countingRunner(agent, &cp))
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if !m.HasSession("n1") {
		t.Fatal("session should be live after Launch")
	}

	m.Shutdown()

	if m.HasSession("n1") {
		t.Error("Shutdown must drop the session")
	}
	waitFor(t, "subprocess killed", func() bool { return atomic.LoadInt32(&cp.kills) >= 1 })
}

// dshModelID turns one dsh select-option value into the "provider/model" id
// scimux already uses for pi and opencode. dsh encodes the pair as a JSON
// array string, which is dsh's own identity for the pair — so the id is
// decoded from the wire rather than invented, and re-encodes exactly.
func TestDshModelID(t *testing.T) {
	cases := []struct{ value, want string }{
		{`["deepseek-official","deepseek-v4-flash"]`, "deepseek-official/deepseek-v4-flash"},
		// A local provider's model name may itself contain a slash. The id
		// keeps it verbatim: splitting on the last slash instead of the first
		// would move the provider boundary and name a provider that does not
		// exist.
		{`["synthetic-local","synth/local-1"]`, "synthetic-local/synth/local-1"},
		// Anything that is not a two-element array of strings is not a pair
		// and must not be guessed at.
		{`"plain-string"`, ""},
		{`["only-one"]`, ""},
		{`["a","b","c"]`, ""},
		{`[1,2]`, ""},
		{``, ""},
		{`not json`, ""},
		// An empty half would produce "/x" or "x/", neither of which round-trips.
		{`["","m"]`, ""},
		{`["p",""]`, ""},
	}
	for _, c := range cases {
		if got := dshModelID(c.value); got != c.want {
			t.Errorf("dshModelID(%q) = %q, want %q", c.value, got, c.want)
		}
	}
}

// applyModel is applyEffort's sibling: it puts a supervisor's model choice on
// the agent's own control surface after the session exists, because dsh takes
// no model on argv. Matching by the decoded pair is what makes the id the
// dialog shows and the value the agent wants the same thing.
func TestApplyModelSelectsTheGroupedPair(t *testing.T) {
	grouped := sdk.SessionConfigSelectOptionsGrouped{
		{Group: "deepseek-official", Name: "DeepSeek", Options: []sdk.SessionConfigSelectOption{
			{Value: `["deepseek-official","synth-flash"]`, Name: "Synth-Flash"},
			{Value: `["deepseek-official","synth-pro"]`, Name: "Synth-Pro"},
		}},
		{Group: "synthetic-local", Name: "synthetic-local", Options: []sdk.SessionConfigSelectOption{
			{Value: `["synthetic-local","synth/local-1"]`, Name: "synth/local-1"},
		}},
	}
	newSession := func() sdk.NewSessionResponse {
		return sdk.NewSessionResponse{
			SessionId: "sess_test",
			ConfigOptions: []sdk.SessionConfigOption{
				{Select: &sdk.SessionConfigOptionSelect{
					Id: "model", Name: "Model", CurrentValue: `["synthetic-local","synth/local-1"]`,
					Options: sdk.SessionConfigSelectOptions{Grouped: &grouped},
				}},
			},
		}
	}

	t.Run("picks the option whose decoded pair matches", func(t *testing.T) {
		agent := &fakeAgent{newSession: newSession}
		m := newManager(t, agent)
		if _, err := m.Launch("n1", "dsh", t.TempDir(), "deepseek-official/synth-pro", ""); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "model config option set", func() bool {
			agent.mu.Lock()
			defer agent.mu.Unlock()
			return agent.lastConfig != nil && agent.lastConfig.ValueId != nil &&
				string(agent.lastConfig.ValueId.Value) == `["deepseek-official","synth-pro"]` &&
				string(agent.lastConfig.ValueId.ConfigId) == "model"
		})
	})

	t.Run("an unknown model refuses the launch", func(t *testing.T) {
		// Launching on the agent's default instead would leave the node record
		// and the log header naming a model nothing is running. dsh's model
		// lives only in the session, so the launch is the last moment this can
		// be said out loud.
		agent := &fakeAgent{newSession: newSession}
		m := newManager(t, agent)
		_, err := m.Launch("n1", "dsh", t.TempDir(), "nobody/nothing", "")
		if err == nil || !IsConfigRejection(err) {
			t.Fatalf("launch error = %v, want a rejected configuration", err)
		}
		agent.mu.Lock()
		defer agent.mu.Unlock()
		if agent.lastConfig != nil {
			t.Fatalf("set config to %+v for a model the agent never offered", agent.lastConfig.ValueId)
		}
	})

	t.Run("no model means no request at all", func(t *testing.T) {
		agent := &fakeAgent{newSession: newSession}
		m := newManager(t, agent)
		if _, err := m.Launch("n1", "dsh", t.TempDir(), "", ""); err != nil {
			t.Fatal(err)
		}
		agent.mu.Lock()
		defer agent.mu.Unlock()
		if agent.lastConfig != nil {
			t.Fatalf("set config to %+v with no model requested", agent.lastConfig.ValueId)
		}
	})

	t.Run("a non-model select is never touched", func(t *testing.T) {
		// applyEffort owns the effort select. If applyModel matched on value
		// alone it could set an effort option to a model id.
		agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
			ung := sdk.SessionConfigSelectOptionsUngrouped{
				{Value: "deepseek-official/synth-pro", Name: "High"},
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
		}}
		m := newManager(t, agent)
		_, err := m.Launch("n1", "dsh", t.TempDir(), "deepseek-official/synth-pro", "")
		if err == nil || !IsConfigRejection(err) {
			t.Fatalf("launch error = %v, want a rejected configuration", err)
		}
		agent.mu.Lock()
		defer agent.mu.Unlock()
		if agent.lastConfig != nil {
			t.Fatalf("model choice reached the %q select", agent.lastConfig.ValueId.ConfigId)
		}
	})
}
