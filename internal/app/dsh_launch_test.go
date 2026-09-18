package app

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/scimux/scimux/internal/acp"
)

// A model or thought level dsh refuses is the user's input being wrong, not the
// server's: launchNode must answer 400 so the browser can say which choice was
// rejected instead of showing a generic failure. The rejection also arrives as
// plain text when it crosses the session-worker boundary, so both the wrapped
// error and its serialised form have to classify the same way.
func TestLaunchNodeMapsARejectedConfigurationTo400(t *testing.T) {
	wrapped := fmt.Errorf("acp launch %s: %w", "dsh", acp.ErrConfigRejected)
	overIPC := errors.New("launch dsh: " + acp.ErrConfigRejected.Error() + ": model \"nope\" is not on the agent's menu")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"wrapped sentinel", wrapped},
		{"text across the worker boundary", overIPC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t, &fakeTmux{})
			proc := &countingProc{launchErr: tc.err}
			n := &Node{
				ID: "dsh-reject", Title: "dsh", Prompt: "hi", Agent: "dsh",
				Dir: a.home, CreatedAt: "2026-09-16T00:00:00Z",
			}
			status, err := a.launchNode(n, proc)
			if err == nil {
				t.Fatal("expected the rejected configuration to fail the launch")
			}
			if status != 400 {
				t.Fatalf("rejected configuration must be a client error: status=%d err=%v", status, err)
			}
			for _, r := range keyRecords(t, a.storePath) {
				if r.Type == "node" {
					t.Fatalf("a refused launch must not persist a node record, got %+v", r.Node)
				}
			}
		})
	}
}

// Every other launch failure stays a server error: a dead binary or a broken
// pipe is not something the user can fix by picking a different model. The RPC
// cases matter as much as the spawn case — they are the ones that happen while
// a model the user chose correctly is being applied, and the message they
// arrive as must not carry the rejection sentinel across the worker boundary.
func TestLaunchNodeKeepsOtherFailures500(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			"spawn failure",
			errors.New("spawn dsh: exec: \"dsh\": executable file not found in $PATH"),
		},
		{
			"agent internal error mid-apply",
			errors.New(`launch dsh: agent failed to set model "ds/b": {"code":-32603,"message":"Internal error"}`),
		},
		{
			"transport died mid-apply",
			fmt.Errorf(`launch dsh: agent failed to set model "ds/b": %w`, io.ErrUnexpectedEOF),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t, &fakeTmux{})
			proc := &countingProc{launchErr: tc.err}
			n := &Node{
				ID: "dsh-broken", Title: "dsh", Prompt: "hi", Agent: "dsh",
				Dir: a.home, CreatedAt: "2026-09-16T00:00:00Z",
			}
			status, err := a.launchNode(n, proc)
			if err == nil {
				t.Fatal("expected the launch to fail")
			}
			if status != 500 {
				t.Fatalf("a broken agent is a server error, got status=%d err=%v", status, err)
			}
		})
	}
}
