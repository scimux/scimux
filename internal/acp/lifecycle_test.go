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
// opencode exposes ACP as a subcommand, and anything else has no ACP transport
// and must be rejected rather than silently launched.
func TestAgentArgv(t *testing.T) {
	cases := []struct {
		agent   string
		want    []string
		wantErr bool
	}{
		{"pi", []string{"pi-acp"}, false},
		{"opencode", []string{"opencode", "acp"}, false},
		{"claude", nil, true},
		{"", nil, true},
	}
	for _, c := range cases {
		got, err := agentArgv(c.agent)
		if c.wantErr {
			if err == nil {
				t.Errorf("agentArgv(%q): want error, got %v", c.agent, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("agentArgv(%q): unexpected error %v", c.agent, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("agentArgv(%q) = %v, want %v", c.agent, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("agentArgv(%q) = %v, want %v", c.agent, got, c.want)
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
	return func(nodeID, agentName, dir string) (Process, error) {
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
	s := &Session{done: make(chan struct{}), logw: &logWriter{path: filepath.Join(t.TempDir(), "n.jsonl")}}
	respCh := make(chan sdk.RequestPermissionResponse, 1)
	go func() {
		resp, _ := s.RequestPermission(context.Background(), sdk.RequestPermissionRequest{
			ToolCall: sdk.ToolCallUpdate{ToolCallId: "t1", Title: sdk.Ptr("run bash")},
			Options:  []sdk.PermissionOption{{OptionId: "opt_allow", Name: "Allow"}},
		})
		respCh <- resp
	}()
	waitFor(t, "pending registered", func() bool { _, _, ok := s.pendingInfo(); return ok })

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
