package acp

import (
	"context"
	"errors"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/scimux/scimux/internal/sessionlog"
)

// Vibe is its own ACP harness. A pi-acp symlink used during a throwaway
// prototype must not become the production command.
func TestVibeArgvIsTheInstalledBinary(t *testing.T) {
	got, err := agentArgv("vibe", "synthetic-model", "high")
	if err != nil {
		t.Fatalf("agentArgv(vibe): %v", err)
	}
	want := []string{"vibe-acp"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("agentArgv(vibe) = %v, want %v", got, want)
	}
	pi, err := agentArgv("pi", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pi) != 1 || pi[0] != "pi-acp" {
		t.Fatalf("pi argv = %v, want pi-acp only", pi)
	}
	if got[0] == pi[0] {
		t.Fatal("vibe launch reused the pi transport binary")
	}
}

func TestVibeLaunchSendsTheFirstPromptOnItsOwnSession(t *testing.T) {
	var prompts int
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			return sdk.NewSessionResponse{SessionId: "vibe-sess"}
		},
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			prompts++
			if p.SessionId != "vibe-sess" {
				t.Errorf("prompt session = %q", p.SessionId)
			}
			if len(p.Prompt) != 1 || p.Prompt[0].Text == nil || p.Prompt[0].Text.Text != "hello vibe" {
				t.Errorf("prompt = %+v", p.Prompt)
			}
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{
				SessionId: p.SessionId, Update: sdk.UpdateAgentMessageText("ready"),
			})
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		},
	}
	var launched []string
	runner := func(nodeID, name, dir, model, effort string) (Process, error) {
		launched = append(launched, name)
		return fakeRunner(agent)(nodeID, name, dir, model, effort)
	}
	m := NewManagerWithRunner(t.TempDir(), runner)
	if _, err := m.Launch("v1", "vibe", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if len(launched) != 1 || launched[0] != "vibe" {
		t.Fatalf("launch names = %v, want one private vibe process", launched)
	}
	if err := m.Send("v1", "hello vibe"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn quiet", func() bool { return m.Live("v1") == "quiet" })
	turns := m.Turns("v1")
	if len(turns) != 2 || turns[0].Text != "hello vibe" || turns[1].Text != "ready" {
		t.Fatalf("turns = %+v", turns)
	}
	if prompts != 1 {
		t.Fatalf("prompts = %d, want the one user turn", prompts)
	}
	if err := m.Kill("v1"); err != nil {
		t.Fatal(err)
	}
	if m.HasSession("v1") {
		t.Fatal("deleted node left its vibe session")
	}
}

func TestVibeClearReplacesTheProcessAndForkStartsEmpty(t *testing.T) {
	var n int
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		n++
		return sdk.NewSessionResponse{SessionId: sdk.SessionId("sess-" + string(rune('0'+n)))}
	}}
	var runs int
	runner := func(nodeID, name, dir, model, effort string) (Process, error) {
		runs++
		if name != "vibe" {
			return nil, errors.New("unexpected agent")
		}
		return fakeRunner(agent)(nodeID, name, dir, model, effort)
	}
	dir := t.TempDir()
	m := NewManagerWithRunner(dir, runner)
	if _, err := m.Launch("parent", "vibe", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("parent", "remember this"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "parent quiet", func() bool { return m.Live("parent") == "quiet" })
	if err := m.Clear("parent"); err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Fatalf("processes = %d, want a replacement for /clear", runs)
	}
	evs := sessionlog.ReadEvents(m.logPath("parent"))
	var seams int
	for _, ev := range evs {
		if ev.T == "source" {
			seams++
		}
	}
	if seams != 1 {
		t.Fatalf("clear seams = %d, want 1", seams)
	}
	// A fork is a new node: same runner, no copied turns.
	if _, err := m.Launch("child", "vibe", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if got := m.Turns("child"); len(got) != 0 {
		t.Fatalf("fork turns = %+v, want none", got)
	}
	if runs != 3 {
		t.Fatalf("processes = %d, want a third process for the fork", runs)
	}
	if m.SessionID("parent") == m.SessionID("child") {
		t.Fatal("fork reused the parent's ACP session")
	}
}
