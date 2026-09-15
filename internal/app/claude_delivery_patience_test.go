package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Because the CLI writes the transcript lazily (see
// claude_lazy_transcript_test.go), silence after the paste is no longer
// evidence that the prompt was lost — it is evidence that Claude has not
// logged it yet. SessionStart proves the *launch* is healthy; only the
// transcript proves the prompt *arrived*, and those two facts now settle on
// different timescales.
//
// So an unconfirmed first prompt is a neutral pending state that self-clears
// when the mirror catches up. The error must nevertheless stay reachable at an
// outer bound: a paste swallowed by a startup, trust, login, or rate-limit
// dialog never reaches Claude's prompt, and no transcript will ever appear for
// it. Patience without a floor would turn that into a silent hang, which is
// worse than the red box it replaced.

func TestUnconfirmedFirstPromptIsNotAnErrorWhileTheLaunchIsHealthy(t *testing.T) {
	f := &fakeTmux{captureAfterEnter: "pane"}
	a := newTestApp(t, f)
	a.claudeReadyTimeout = testReadyBudget
	// The delivery clock is the subject here: it must run out without a verdict.
	a.claudeDeliveryTimeout = testTimeoutBudget
	a.claudeInitialPoll = testInitialPoll

	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID := prepareOwnedClaudeBundle(t, a, n)
	path := claudeTranscriptPathOnly(t, a.home, hookSIDOwn)
	hookInboxReady(t, a, hookID, "startup", hookSessionJSON("startup", hookSIDOwn, path))
	// Deliberately no appendOnEnter: Claude never gets round to writing the file.

	if got := a.deliverClaudeInitialPrompt(n); got != initialUnconfirmed {
		t.Fatalf("delivery = %q, want %q: the hook arrived, so this is patience, not failure",
			got, initialUnconfirmed)
	}
	if !f.didSendEnter() {
		t.Fatal("the prompt must still have been pasted exactly once")
	}
	if msg := a.claudeLaunchError(n.ID); msg != "" {
		t.Fatalf("launch error = %q, want none while the launch is provably healthy", msg)
	}
}

func TestUnconfirmedFirstPromptKeepsTheBubbleAndWithholdsTheDraft(t *testing.T) {
	f := &fakeTmux{captureAfterEnter: "pane"}
	a := newTestApp(t, f)
	a.deliverClaudeInitial = a.deliverClaudeInitialPrompt
	a.claudeReadyTimeout = testReadyBudget
	a.claudeDeliveryTimeout = testTimeoutBudget
	a.claudeInitialPoll = testInitialPoll
	rec := newNode(a, `{"title":"Patient","prompt":"hello patience","agent":"claude","dir":`+strconv.Quote(a.home)+`}`)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	n := a.byID[created.ID]
	path := claudeTranscriptPathOnly(t, a.home, n.SessionID)
	if err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "startup",
		SessionID: n.SessionID, TranscriptPath: path, Cwd: a.home,
	}); !errors.Is(err, errClaudeHookPending) {
		t.Fatalf("SessionStart naming an unwritten transcript = %v, want it held pending", err)
	}
	waitClaudeInitialGate(t, a, created.ID)

	body := chatBody(t, a, created.ID)
	if body["pending_prompt"] != "hello patience" {
		t.Fatalf("pending_prompt = %v, want the bubble still pale", body["pending_prompt"])
	}
	if draft, ok := body["restore_draft"]; ok {
		t.Fatalf("restore_draft = %v, want none: a prompt still in flight must not "+
			"also sit in the composer, or the user sends it twice", draft)
	}
	if msg, _ := body["error"].(string); msg != "" {
		t.Fatalf("error = %q, want none: SessionStart arrived, so nothing has failed", msg)
	}
	if got := body["delivery"]; got != "delivering" {
		t.Fatalf("delivery = %v, want %q — the browser's \"unconfirmed\" presentation says "+
			"the send could not be confirmed, which is untrue here", got, "delivering")
	}
}

func TestLateTranscriptRetiresTheUnconfirmedFirstPromptCleanly(t *testing.T) {
	f := &fakeTmux{list: []string{"cl1"}, capture: "working"}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "claude.jsonl")
	appendFile(t, path, claudeTurn("user", "irreplaceable", "2026-09-09T12:00:00Z"))
	n := &Node{
		ID: "cl1", Title: "Claude", Agent: "claude", Dir: a.home,
		Prompt: "irreplaceable", Transcript: path,
	}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.sendState[n.ID] = sendInitialUnconfirmed
	// The delivery watermark lets reconciliation confirm this late transcript.
	a.noteDelivery(n.ID, time.Now())

	a.poll()

	a.mu.Lock()
	_, locked := a.sendState[n.ID]
	a.mu.Unlock()
	if locked {
		t.Fatal("the late turn must release the first-prompt gate")
	}
	if msg := a.claudeLaunchError(n.ID); msg != "" {
		t.Fatalf("launch error = %q, want none: the prompt did land, merely late", msg)
	}
	body := chatBody(t, a, n.ID)
	if draft, ok := body["restore_draft"]; ok {
		t.Fatalf("restore_draft = %v, want none: the prompt is in the transcript", draft)
	}
	if _, ok := body["pending_prompt"]; ok {
		t.Fatalf("pending_prompt = %v, want the bubble solid now", body["pending_prompt"])
	}
}

func TestUnconfirmedFirstPromptGivesUpAtTheOuterBound(t *testing.T) {
	// A prompt pasted into a dialog that never reaches Claude's prompt leaves no
	// transcript at all, so the neutral wait has to end somewhere.
	for _, tc := range []struct {
		name    string
		since   time.Duration
		wantErr bool
	}{
		{"patient inside the bound", 179 * time.Second, false},
		{"gives up past the bound", 181 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t, &fakeTmux{})
			n := &Node{
				ID: "cl1", Title: "Claude", Agent: "claude", Dir: a.home,
				Prompt: "irreplaceable",
			}
			a.nodes = []*Node{n}
			a.byID[n.ID] = n
			a.sendState[n.ID] = sendInitialUnconfirmed
			a.noteDelivery(n.ID, time.Now().Add(-tc.since))

			a.reconcileClaudeInitialDelivery(n)

			msg := a.claudeLaunchError(n.ID)
			a.mu.Lock()
			_, locked := a.sendState[n.ID]
			a.mu.Unlock()
			if !tc.wantErr {
				if msg != "" {
					t.Fatalf("launch error = %q, want none inside the outer bound", msg)
				}
				if !locked {
					t.Fatal("the first-prompt gate must stay held while scimux is still waiting")
				}
				return
			}
			if msg != claudeDeliveryExplain {
				t.Fatalf("launch error = %q, want %q", msg, claudeDeliveryExplain)
			}
			if locked {
				t.Fatal("giving up must release the gate so the user can send again")
			}
			body := chatBody(t, a, n.ID)
			if body["restore_draft"] != "irreplaceable" {
				t.Fatalf("restore_draft = %v, want the prompt back in the composer once "+
					"scimux has genuinely given up", body["restore_draft"])
			}
		})
	}
}

func TestFirstPromptDeliveryGiveUpIsThreeMinutes(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if a.claudeDeliveryGiveUp != 180*time.Second {
		t.Fatalf("claudeDeliveryGiveUp = %v, want 3m", a.claudeDeliveryGiveUp)
	}
	if a.claudeDeliveryGiveUp <= a.claudeDeliveryTimeout {
		t.Fatalf("the outer bound (%v) must be strictly longer than the neutral wait (%v), "+
			"or the neutral state can never be observed", a.claudeDeliveryGiveUp, a.claudeDeliveryTimeout)
	}
}

func TestPasteFailureStaysAnImmediateError(t *testing.T) {
	// Patience is for a transcript that has not caught up. tmux refusing the
	// keystroke is a failure scimux can see at once, and it stays one.
	f := &fakeTmux{sendKeysErr: true}
	a := newTestApp(t, f)
	a.claudeReadyTimeout = testReadyBudget
	a.claudeDeliveryTimeout = testDeliverBudget
	a.claudeInitialPoll = testInitialPoll

	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID := prepareOwnedClaudeBundle(t, a, n)
	path := claudeTranscriptPathOnly(t, a.home, hookSIDOwn)
	hookInboxReady(t, a, hookID, "startup", hookSessionJSON("startup", hookSIDOwn, path))

	if got := a.deliverClaudeInitialPrompt(n); got != initialNotSent {
		t.Fatalf("delivery = %q, want %q when the paste itself failed", got, initialNotSent)
	}
	if msg := a.claudeLaunchError(n.ID); msg != claudePasteExplain {
		t.Fatalf("launch error = %q, want %q", msg, claudePasteExplain)
	}
}
