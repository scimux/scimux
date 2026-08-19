package app

// P1 follow-up: AX attention degrades to the neutral inspect, never to silence.
//
// fixes-2 P1 stopped a spurious 45-second `inspect` from inviting a stray `1`
// on an --ax-screen-reader node. It did that twice over: the UI stopped giving
// AX `inspect` a remote keypad (keyRowHTML: Dismiss only; a stray "1" would
// become 1+Enter), and the poller stopped raising it for AX at all. Only the
// first was needed — an AX-keyless `inspect` cannot submit a prompt. The
// second removed AX's floor: with the quiet WaitingOn branch fenced behind
// ClassifyVisible *and* the Owing backstop disabled, one regex family became
// the sole route to any AX attention. Six of seven plausible AX pane shapes
// (reworded footer, footer-less menu, y/n confirm, free-text prompt,
// single-option menu) do not match it, and in that state an AX node sitting
// on a real approval raised nothing, indefinitely — while a non-AX node in
// the identical state still degrades to `inspect` at 45s. Non-AX inspect
// keeps the full keypad plus Dismiss so a misclassified dialog can be
// answered; that keypad must not appear on AX.
//
// These tests pin the floor: AX stays strictly more conservative than non-AX at
// every rung, but it does eventually say "no visible progress".

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/tmuxsession"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// The stall ladder: AX is exactly one rung slower than non-AX, and the cancel
// anchor sharpens each by one rung. Constants, not behavior — a regression here
// silently changes every case below.
func TestAXStallLadderOrdering(t *testing.T) {
	if !(owedStallCorroborated < owedStallAfter && owedStallAfter < owedStallAX) {
		t.Fatalf("stall ladder broken: corroborated=%v owed=%v ax=%v; want corroborated < owed < ax",
			owedStallCorroborated, owedStallAfter, owedStallAX)
	}
	// Bounded on both sides. Every case below states its quiet duration
	// relative to owedStallAX, so without a ceiling they would all still pass
	// with an absurd constant that means the floor never fires in practice —
	// the vacuous "guard" this arc keeps finding. The ceiling is set by what
	// the floor is for: AGENTS.md records a 6m42s approval wait going
	// unnoticed, so the neutral note has to arrive well inside that.
	if owedStallAX < 2*time.Minute || owedStallAX > 6*time.Minute {
		t.Errorf("owedStallAX = %v, want between 2m and 6m — long enough that a silent AX turn does not trip it, short enough to still be a floor",
			owedStallAX)
	}
}

// quietAttentionFallback directly: the AX owing backstop is delayed, not deleted.
func TestQuietAttentionFallbackAXOwingFloor(t *testing.T) {
	// Anchor present, dialog unrecognized — the probe's "AX free-text prompt".
	anchorPane := "The agent asks: which branch should I target?\n\nType a reply, or Escape to cancel:"
	plainPane := "I have the evidence I need. Writing the updated review.\n$"
	workingPane := "Running tool…\n esc to interrupt\n$"

	tl := owingTailer(t)

	cases := []struct {
		what   string
		pane   string
		quiet  time.Duration
		ax     bool
		want   string
		reason string
	}{
		{"non-AX past owed stall", plainPane, owedStallAfter + time.Second, false, "inspect",
			"legacy behavior must be untouched"},
		{"AX just past owed stall", plainPane, owedStallAfter + time.Second, true, "",
			"AX is one rung slower — 45s is not yet enough"},
		{"AX past AX stall", plainPane, owedStallAX + time.Second, true, "inspect",
			"the floor: AX eventually says no visible progress"},
		{"AX working footer suppresses floor", workingPane, owedStallAX + time.Second, true, "",
			"the ordinary working footer is a suppression hint, never a dialog"},
		{"AX with anchor past owed stall", anchorPane, owedStallAfter + time.Second, true, "inspect",
			"anchor sharpens AX by one rung, to the non-AX timing"},
		{"AX with anchor before owed stall", anchorPane, owedStallCorroborated + time.Second, true, "",
			"the anchor may not shorten AX all the way to the non-AX corroborated rung"},
	}
	for _, tc := range cases {
		got := quietAttentionFallback(tl, tc.pane, time.Now().Add(-tc.quiet), tc.ax)
		if got != tc.want {
			t.Errorf("%s: quietAttentionFallback = %q, want %q (%s)", tc.what, got, tc.want, tc.reason)
		}
	}
}

// The quiet WaitingOn branch: an AX pane the matcher cannot classify must fall
// through to the neutral inspect, never to nothing and never to a classified
// approval it has no evidence for.
func TestAXUnrecognizedDialogDegradesToInspect(t *testing.T) {
	// Structurally a real permission menu, but the footer is reworded — exactly
	// the TUI-churn case. ClassifyVisible does not match it.
	rewordedDialog := "Permission Required: Create file\n\n  1. Yes\n  2. No\n\n" +
		"Enter selection [1-2], or press Escape to go back:"
	permDialog := "Permission Required: Create file\n\n  1. Yes\n  2. No\n\n" +
		"Enter selection [1-2], or Escape to cancel:"
	unresolvedBash := []string{
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"run tests"}}`,
		`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`,
	}

	cases := []struct {
		what  string
		pane  string
		quiet time.Duration
		want  string
	}{
		{"recognized dialog does not auto-open Claude", permDialog, 10 * time.Second, ""},
		{"recognized dialog still does not inspect past the AX stall", permDialog, owedStallAX + time.Second, ""},
		{"unrecognized dialog is not inspect", rewordedDialog, 10 * time.Second, ""},
		{"unrecognized dialog does not degrade to inspect on Claude", rewordedDialog, owedStallAX + time.Second, ""},
		{"working footer stays silent", "Running tool…\n esc to interrupt", owedStallAX + time.Second, ""},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			a := axNodeWithTranscript(t, tc.pane, tc.quiet, unresolvedBash...)
			a.poll()
			if got := a.attn["cl1"]; got != tc.want {
				t.Errorf("attention = %q, want %q", got, tc.want)
			}
			// Attention never feeds liveness, and never rewrites the clock it reads.
			if got := a.live["cl1"]; got != "quiet" {
				t.Errorf("live = %q, want quiet", got)
			}
		})
	}
}

// axNodeWithTranscript builds a quiet AX Claude node whose pane has been static
// for `quiet`, with the given transcript lines. Synthetic panes only.
func axNodeWithTranscript(t *testing.T, pane string, quiet time.Duration, lines ...string) *app {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tx.jsonl")
	if len(lines) > 0 {
		appendLines(t, path, lines...)
	} else if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "cl1", Agent: "claude", Transcript: path, AXScreenReader: true}
	runner := func(ctx context.Context, stdin string, args ...string) (string, error) {
		sub := ""
		if len(args) >= 3 {
			sub = args[2]
		}
		switch sub {
		case "list-sessions":
			return "cl1", nil
		case "has-session":
			return "", nil
		case "capture-pane":
			return pane, nil
		}
		return "", nil
	}
	a := &app{
		byID:      map[string]*Node{"cl1": n},
		nodes:     []*Node{n},
		live:      map[string]string{},
		attn:      map[string]string{},
		prevCap:   map[string]string{"cl1": pane},
		lastChg:   map[string]time.Time{"cl1": time.Now().Add(-quiet)},
		tailers:   map[string]*transcript.Tailer{},
		chatMark:  map[string]chatMark{},
		staleChat: map[string]bool{},
		server:    tmuxsession.NewServerWithRunner("testsock", runner),
	}
	a.initMaps()
	return a
}

// owingTailer returns a tailer whose newest recognized record is a human turn,
// so Owing() reports the agent owes the next output.
func owingTailer(t *testing.T) *transcript.Tailer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "owing.jsonl")
	appendLines(t, path,
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"write the review"}}`)
	tl := &transcript.Tailer{Path: path}
	tl.Poll()
	if !tl.Owing() {
		t.Fatalf("setup: tailer must be Owing")
	}
	return tl
}
