package app

import (
	"os"
	"path/filepath"
	"testing"
)

// The Claude CLI creates its transcript file lazily: an idle launch has no
// record to write, so SessionStart names a path that does not exist yet and
// the *first prompt* is what brings the file into being. Observed live on
// claude 2.1.266 (2026-09-09): the hook fired 1 s after launch, but its inbox
// event stayed pending for 85 s — until the user typed a prompt by hand — and
// the launch reported "SessionStart never arrived" 15 s in. Gating the paste
// on the transcript file is therefore a deadlock: scimux waits for a file the
// prompt it is holding back would create.
//
// These tests pin the split the fix rests on: SessionStart acknowledges the
// *hook*, the transcript file is what binds the *link*, and only the first is
// a precondition for pasting the first prompt.

// claudeTranscriptPathOnly returns the transcript path a launch will
// eventually own, with the project directory present and the file
// deliberately absent — the state SessionStart actually reports on an idle
// launch.
func claudeTranscriptPathOnly(t *testing.T, home, sid string) string {
	t.Helper()
	proj := filepath.Join(home, ".claude", "projects", "-w-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(proj, sid+".jsonl")
}

// prepareOwnedClaudeBundle installs a real hook bundle for n without
// acknowledging a SessionStart, so the drain under test is the only thing
// that can release the launch gate.
func prepareOwnedClaudeBundle(t *testing.T, a *app, n *Node) string {
	t.Helper()
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatalf("prepare Claude hook bundle: %v", err)
	}
	a.mu.Lock()
	a.claudeHooks[n.ID] = hookID
	a.mu.Unlock()
	return hookID
}

func TestClaudeLaunchPastesBeforeTheTranscriptFileExists(t *testing.T) {
	f := &fakeTmux{captureAfterEnter: "pane"}
	a := newTestApp(t, f)
	a.claudeReadyTimeout = testReadyBudget
	a.claudeDeliveryTimeout = testDeliverBudget
	a.claudeInitialPoll = testInitialPoll

	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID := prepareOwnedClaudeBundle(t, a, n)
	path := claudeTranscriptPathOnly(t, a.home, hookSIDOwn)
	hookInboxReady(t, a, hookID, "startup", hookSessionJSON("startup", hookSIDOwn, path))
	// The paste is what makes Claude write the file for the very first time.
	f.appendOnEnter(t, path,
		`{"type":"user","timestamp":"2026-09-09T00:00:00Z","message":{"role":"user","content":"first"}}`)

	if got := a.deliverClaudeInitialPrompt(n); got != initialAcknowledged {
		t.Fatalf("delivery = %q, want acknowledged: SessionStart arrived, the transcript was merely not written yet", got)
	}
	if !f.didSendEnter() {
		t.Fatal("the first prompt must be pasted once SessionStart has arrived")
	}
	if msg := a.claudeLaunchError(n.ID); msg != "" {
		t.Fatalf("launch error = %q, want none: the hook did arrive", msg)
	}
	a.mu.Lock()
	bound := n.Transcript
	a.mu.Unlock()
	if bound != path {
		t.Fatalf("transcript = %q, want %q bound once the paste created the file", bound, path)
	}
}

func TestClaudeSessionStartReleasesThePasteGateWithoutATranscript(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	if a.claudeSessionStartReady(n) {
		t.Fatal("the paste gate must stay shut before any SessionStart")
	}
	a.markClaudeStartPending(n.ID, hookSIDOwn)
	if !a.claudeSessionStartReady(n) {
		t.Fatal("a SessionStart whose transcript is not written yet must still release the paste gate")
	}
}

func TestClaudeStartPendingIsFencedBySession(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	// A mark left by an earlier launch names an earlier session id. This
	// launch minted its own, so that mark must not release its gate.
	a.markClaudeStartPending(n.ID, hookSIDOther)
	if a.claudeSessionStartReady(n) {
		t.Fatal("a pending SessionStart from another session must not release this launch's gate")
	}
}

func TestPendingSessionStartStaysInTheInboxUntilTheFileAppears(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID := prepareOwnedClaudeBundle(t, a, n)
	path := claudeTranscriptPathOnly(t, a.home, hookSIDOwn)
	event := hookInboxReady(t, a, hookID, "startup", hookSessionJSON("startup", hookSIDOwn, path))

	a.drainClaudeHooks()
	if _, err := os.Stat(event); err != nil {
		t.Fatalf("a SessionStart whose transcript is not written yet must stay in the inbox: %v", err)
	}
	a.mu.Lock()
	bound := n.Transcript
	acked := a.claudeHookAckedLocked(n.ID)
	a.mu.Unlock()
	if bound != "" {
		t.Fatalf("transcript = %q, want no binding until the file exists", bound)
	}
	if acked {
		t.Fatal("the full acknowledgement belongs to the bind, not to a pending event")
	}
	if !a.claudeSessionStartReady(n) {
		t.Fatal("the drain must record the pending SessionStart so the paste gate opens")
	}
	if recs := bindingRecs(t, a, n.ID); len(recs) != 0 {
		t.Fatalf("want no binding record while pending, got %+v", recs)
	}

	// Claude finally writes the file (in production: because the prompt
	// landed). The same inbox event now binds, with every filesystem check
	// applied to the file that actually exists.
	writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn)
	a.drainClaudeHooks()
	a.mu.Lock()
	bound = n.Transcript
	acked = a.claudeHookAckedLocked(n.ID)
	a.mu.Unlock()
	if bound != path {
		t.Fatalf("transcript = %q, want %q once the file appeared", bound, path)
	}
	if !acked {
		t.Fatal("the bind must acknowledge the hook")
	}
	if _, err := os.Stat(event); !os.IsNotExist(err) {
		t.Fatalf("the bound event must leave the inbox: %v", err)
	}
	if recs := bindingRecs(t, a, n.ID); len(recs) != 1 {
		t.Fatalf("want exactly one binding record, got %+v", recs)
	}
}
