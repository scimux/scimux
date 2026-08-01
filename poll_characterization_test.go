package main

// Packet 3A: characterization of poll() mechanical liveness, structured-transport
// bypass, active→quiet transcript judgment, inspect fallbacks, and pure poll
// helpers. Complements — does not replace — TestPollerDialogDetection,
// TestDialogDetectionOrWithStructured, TestActivePaneDialogCorroboration,
// TestActiveConfinedStallBackstop, attention preservation/expiry, NoteAnim spill,
// NoteChatProgress/stale-chat, MaybeRelinkTranscript*, RetireTranscript*,
// MirrorClearRollover, and Packet 2E discovery/relink ordering tests.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp/codex"
	"codeberg.org/chrberger/scimux/internal/tmuxsession"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// pollRunner builds a fake-tmux Runner that reports session liveness and pane
// capture without talking to a real tmux server. capture-pane invocations are
// counted so structured-transport tests can prove the tmux path is skipped.
func pollRunner(alive bool, pane string, captureErr bool, captureCalls *int) tmuxsession.Runner {
	return func(ctx context.Context, stdin string, args ...string) (string, error) {
		for _, arg := range args {
			switch arg {
			case "has-session":
				if alive {
					return "", nil
				}
				return "", fmt.Errorf("can't find session")
			case "capture-pane":
				if captureCalls != nil {
					*captureCalls++
				}
				if captureErr {
					return "", fmt.Errorf("capture failed")
				}
				return pane, nil
			}
		}
		return "", nil
	}
}

func newPollApp(t *testing.T, n *Node, runner tmuxsession.Runner) *app {
	t.Helper()
	data := t.TempDir()
	return &app{
		byID:        map[string]*Node{n.ID: n},
		nodes:       []*Node{n},
		live:        map[string]string{},
		attn:        map[string]string{},
		attnAt:      map[string]time.Time{},
		prevCap:     map[string]string{},
		lastChg:     map[string]time.Time{},
		activeSince: map[string]time.Time{},
		tailers:     map[string]*transcript.Tailer{},
		mirrors:     map[string]*mirror{},
		pathClaims:  map[string]bool{},
		chatMark:    map[string]chatMark{},
		staleChat:   map[string]bool{},
		anim:        map[string]*animState{},
		server:      tmuxsession.NewServerWithRunner("testsock", runner),
		home:        t.TempDir(),
		storePath:   filepath.Join(data, "nodes.jsonl"),
		sessionsDir: filepath.Join(data, "sessions"),
	}
}

// TestPollMechanicalLiveness locks the pane-change liveness matrix and the
// rule that pane *text* never chooses active vs quiet. activeSince is stamped
// only on entry into an active phase.
func TestPollMechanicalLiveness(t *testing.T) {
	const id = "n1"
	paneA := "line0\nline1\nworking"
	paneB := "line0\nline1\nworking!"
	// Dialog-shaped text must not flip liveness on its own.
	dialogPane := "Do you want to proceed?\n  1. Yes\n  2. No\n  Esc to cancel"

	t.Run("changed capture is active and stamps activeSince", func(t *testing.T) {
		n := &Node{ID: id, Agent: "claude"}
		a := newPollApp(t, n, pollRunner(true, paneB, false, nil))
		a.prevCap[id] = paneA
		a.live[id] = "quiet"
		before := time.Now()
		a.poll()
		if got := a.live[id]; got != "active" {
			t.Fatalf("live = %q, want active", got)
		}
		since, ok := a.activeSince[id]
		if !ok || since.Before(before) {
			t.Fatalf("activeSince missing or stale: ok=%v since=%v", ok, since)
		}
		if a.prevCap[id] != paneB {
			t.Fatalf("prevCap = %q, want updated capture", a.prevCap[id])
		}
	})

	t.Run("unchanged but recent lastChg stays active without restamping activeSince", func(t *testing.T) {
		n := &Node{ID: id, Agent: "claude"}
		a := newPollApp(t, n, pollRunner(true, paneA, false, nil))
		a.prevCap[id] = paneA
		a.lastChg[id] = time.Now().Add(-2 * time.Second) // within 8s quiet gate
		a.live[id] = "active"
		fixed := time.Now().Add(-30 * time.Second)
		a.activeSince[id] = fixed
		a.poll()
		if got := a.live[id]; got != "active" {
			t.Fatalf("live = %q, want active", got)
		}
		if !a.activeSince[id].Equal(fixed) {
			t.Fatalf("activeSince restamped while remaining active: %v vs %v", a.activeSince[id], fixed)
		}
	})

	t.Run("unchanged beyond quiet threshold is quiet", func(t *testing.T) {
		n := &Node{ID: id, Agent: "claude", Transcript: filepath.Join(t.TempDir(), "missing.jsonl")}
		// Healthy empty path absent → inspect; we only assert liveness here.
		a := newPollApp(t, n, pollRunner(true, paneA, false, nil))
		a.prevCap[id] = paneA
		a.lastChg[id] = time.Now().Add(-10 * time.Second)
		a.live[id] = "active"
		fixed := time.Now().Add(-40 * time.Second)
		a.activeSince[id] = fixed
		a.poll()
		if got := a.live[id]; got != "quiet" {
			t.Fatalf("live = %q, want quiet", got)
		}
		// Phase-start watermark is kept for the active→quiet transcript judgment.
		if !a.activeSince[id].Equal(fixed) {
			t.Fatalf("activeSince cleared on quiet: %v", a.activeSince[id])
		}
	})

	t.Run("missing session is exited", func(t *testing.T) {
		n := &Node{ID: id, Agent: "claude"}
		a := newPollApp(t, n, pollRunner(false, paneA, false, nil))
		a.live[id] = "active"
		a.poll()
		if got := a.live[id]; got != "exited" {
			t.Fatalf("live = %q, want exited", got)
		}
		if got := a.attn[id]; got != "" {
			t.Fatalf("attention on exited = %q, want empty", got)
		}
	})

	t.Run("capture failure is unavailable", func(t *testing.T) {
		n := &Node{ID: id, Agent: "claude"}
		a := newPollApp(t, n, pollRunner(true, dialogPane, true, nil))
		a.poll()
		if got := a.live[id]; got != "unavailable" {
			t.Fatalf("live = %q, want unavailable", got)
		}
		if got := a.attn[id]; got != "" {
			t.Fatalf("attention on unavailable = %q, want empty", got)
		}
	})

	t.Run("pane text alone never chooses active vs quiet", func(t *testing.T) {
		// Same mechanical inputs (alive, same capture as prevCap, aged lastChg)
		// with dialog-shaped vs ordinary text must both be quiet.
		for _, pane := range []string{paneA, dialogPane} {
			n := &Node{ID: id, Agent: "claude"}
			a := newPollApp(t, n, pollRunner(true, pane, false, nil))
			a.prevCap[id] = pane
			a.lastChg[id] = time.Now().Add(-10 * time.Second)
			a.poll()
			if got := a.live[id]; got != "quiet" {
				t.Fatalf("pane %q: live = %q, want quiet (text must not feed liveness)", pane, got)
			}
		}
		// And the same with a recent lastChg must both be active.
		for _, pane := range []string{paneA, dialogPane} {
			n := &Node{ID: id, Agent: "claude"}
			a := newPollApp(t, n, pollRunner(true, pane, false, nil))
			a.prevCap[id] = pane
			a.lastChg[id] = time.Now().Add(-time.Second)
			a.poll()
			if got := a.live[id]; got != "active" {
				t.Fatalf("pane %q: live = %q, want active", pane, got)
			}
		}
	})

	t.Run("activeSince set only when entering active from non-active", func(t *testing.T) {
		n := &Node{ID: id, Agent: "claude"}
		a := newPollApp(t, n, pollRunner(true, paneB, false, nil))
		a.prevCap[id] = paneA
		// prev live unset → entering active
		a.poll()
		first, ok := a.activeSince[id]
		if !ok {
			t.Fatal("activeSince not set on first active entry")
		}
		// Force another change while already active.
		time.Sleep(2 * time.Millisecond)
		a.prevCap[id] = paneB
		// Swap pane so capture differs again.
		a.server = tmuxsession.NewServerWithRunner("testsock", pollRunner(true, paneA, false, nil))
		a.poll()
		if got := a.live[id]; got != "active" {
			t.Fatalf("live = %q after second change, want active", got)
		}
		if !a.activeSince[id].Equal(first) {
			t.Fatalf("activeSince moved while staying active: first=%v now=%v", first, a.activeSince[id])
		}
	})
}

// TestPollStructuredTransportBypassesTmux: ACP/codex manager Live/Attention
// win; poll must not capture panes, discover transcripts, or run tmux quiet
// mechanics for structured nodes.
func TestPollStructuredTransportBypassesTmux(t *testing.T) {
	t.Run("no session yields manager exited without capture", func(t *testing.T) {
		var captures int
		// Runner would report a live dialog pane if consulted — structured path
		// must ignore it.
		n := &Node{ID: "cx1", Agent: "codex", Transport: "codex", Transcript: "/nope.jsonl"}
		a := newPollApp(t, n, pollRunner(true, "Do you want to proceed?\n  1. Yes", false, &captures))
		a.codex = codexManager{codex.NewManager(t.TempDir())}
		t.Cleanup(a.codex.Shutdown)
		a.prevCap[n.ID] = "prior" // would affect tmux quiet/active if read
		a.lastChg[n.ID] = time.Now().Add(-time.Hour)
		a.poll()
		if got := a.live[n.ID]; got != "exited" {
			t.Fatalf("live = %q, want exited from manager", got)
		}
		if got := a.attn[n.ID]; got != "" {
			t.Fatalf("attention = %q, want empty", got)
		}
		if captures != 0 {
			t.Fatalf("structured poll performed %d capture-pane calls, want 0", captures)
		}
		if a.prevCap[n.ID] != "prior" {
			t.Fatalf("prevCap mutated on structured path: %q", a.prevCap[n.ID])
		}
	})

	t.Run("launched codex supplies quiet and still skips capture", func(t *testing.T) {
		var captures int
		rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
		n := &Node{ID: "cx2", Agent: "codex", Transport: "codex", Dir: t.TempDir()}
		a := newPollApp(t, n, pollRunner(true, "pane that would be quiet or dialog", false, &captures))
		spawn, _ := newFakeCodexSpawn(t, "THREAD-POLL", rollout)
		a.codex = codexManager{codex.NewManagerWithSpawn(a.sessionsDir, spawn)}
		t.Cleanup(a.codex.Shutdown)
		if _, err := a.codex.Launch(n.ID, n.Agent, n.Dir, "", ""); err != nil {
			t.Fatal(err)
		}
		a.poll()
		if got := a.live[n.ID]; got != "quiet" {
			t.Fatalf("live = %q, want quiet from manager", got)
		}
		if captures != 0 {
			t.Fatalf("structured poll performed %d capture-pane calls, want 0", captures)
		}
		// Quiet structured path does not stamp lastChg (only active does).
		if _, ok := a.lastChg[n.ID]; ok {
			t.Fatalf("lastChg set on quiet structured node: %v", a.lastChg[n.ID])
		}
	})

	t.Run("active codex turn stamps lastChg and still skips capture", func(t *testing.T) {
		var captures int
		rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
		n := &Node{ID: "cx3", Agent: "codex", Transport: "codex", Dir: t.TempDir()}
		a := newPollApp(t, n, pollRunner(true, "ignored pane", false, &captures))
		spawn, _, turnStarted, unblock := newBlockingFakeCodexSpawn(t, "THREAD-ACT", rollout)
		a.codex = codexManager{codex.NewManagerWithSpawn(a.sessionsDir, spawn)}
		t.Cleanup(func() {
			close(unblock)
			a.codex.Shutdown()
		})
		if _, err := a.codex.Launch(n.ID, n.Agent, n.Dir, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := a.codex.Send(n.ID, "go"); err != nil {
			t.Fatal(err)
		}
		select {
		case <-turnStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for turn/start")
		}
		before := time.Now()
		a.poll()
		if got := a.live[n.ID]; got != "active" {
			t.Fatalf("live = %q, want active from manager", got)
		}
		if captures != 0 {
			t.Fatalf("structured poll performed %d capture-pane calls, want 0", captures)
		}
		chg, ok := a.lastChg[n.ID]
		if !ok || chg.Before(before) {
			t.Fatalf("lastChg not stamped on structured active: ok=%v chg=%v", ok, chg)
		}
		if got := a.attn[n.ID]; got != "" {
			t.Fatalf("attention during ordinary turn = %q, want empty", got)
		}
	})
}

// TestPollActiveToQuietTranscriptJudgment: the whole-phase (prev active → quiet)
// path marks stale when the linked file grew without recognized agent progress,
// and quiet ticks clear stale when progress finally arrives.
func TestPollActiveToQuietTranscriptJudgment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tx.jsonl")
	appendLines(t, path) // empty link

	n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
	pane := "static quiet pane"
	a := newPollApp(t, n, pollRunner(true, pane, false, nil))
	a.prevCap[n.ID] = pane
	a.lastChg[n.ID] = time.Now().Add(-10 * time.Second)
	// Establish the pre-phase baseline on a quiet tick (judge=false).
	a.live[n.ID] = "quiet"
	a.poll()
	if a.staleChat[n.ID] {
		t.Fatal("baseline quiet tick must not mark stale")
	}
	if !a.chatMark[n.ID].seen {
		t.Fatal("baseline mark not recorded")
	}

	// Phase growth: unknown assistant shape only (bytes grow, agent prog does not).
	appendLines(t, path,
		`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":{"v2_rich":"moved"}}}`)
	a.live[n.ID] = "active" // prev for the next tick
	a.poll()
	if !a.staleChat[n.ID] {
		t.Fatal("active→quiet growth without chat progress must mark stale")
	}
	if got := a.live[n.ID]; got != "quiet" {
		t.Fatalf("live = %q, want quiet", got)
	}
	if got := a.attn[n.ID]; got != "inspect" {
		t.Fatalf("stale quiet attention = %q, want inspect", got)
	}

	// Recognized progress on a later quiet tick clears stale without another active cycle.
	appendLines(t, path,
		`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":"back to known shape"}}`)
	a.live[n.ID] = "quiet"
	a.poll()
	if a.staleChat[n.ID] {
		t.Fatal("recognized progress must clear stale on quiet tick")
	}
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("after clear, attention = %q, want empty (healthy quiet transcript)", got)
	}
}

// TestPollInspectFallbacks: no-transcript, pre-flagged stale, and unparseable
// quiet panes degrade to neutral inspect — never approval/question from text.
func TestPollInspectFallbacks(t *testing.T) {
	pane := "Agent finished.\n$ "                       // ordinary, non-dialog text
	dialogish := "something about approval permissions" // not a dialoghint match

	t.Run("missing transcript is inspect", func(t *testing.T) {
		n := &Node{ID: "n", Agent: "claude"} // no Transcript
		a := newPollApp(t, n, pollRunner(true, pane, false, nil))
		a.prevCap[n.ID] = pane
		a.lastChg[n.ID] = time.Now().Add(-10 * time.Second)
		a.poll()
		if got := a.live[n.ID]; got != "quiet" {
			t.Fatalf("live = %q, want quiet", got)
		}
		if got := a.attn[n.ID]; got != "inspect" {
			t.Fatalf("attention = %q, want inspect", got)
		}
	})

	t.Run("staleChat is inspect", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"assistant","timestamp":"t","message":{"role":"assistant","content":"ok"}}`)
		n := &Node{ID: "n", Agent: "claude", Transcript: path}
		a := newPollApp(t, n, pollRunner(true, dialogish, false, nil))
		a.prevCap[n.ID] = dialogish
		a.lastChg[n.ID] = time.Now().Add(-10 * time.Second)
		// Install the tailer first: a fresh tailerFor clears staleChat (relink
		// contract). Then re-flag stale so the quiet inspect path sees it.
		_ = a.tailerFor(n)
		a.staleChat[n.ID] = true
		a.live[n.ID] = "quiet"
		a.poll()
		if got := a.attn[n.ID]; got != "inspect" {
			t.Fatalf("attention = %q, want inspect", got)
		}
		// Ordinary pane text must not upgrade inspect to approval/question.
		if got := a.attn[n.ID]; got == "approval" || got == "question" {
			t.Fatalf("pane text created classified attention: %q", got)
		}
	})

	t.Run("unparseable recent data is inspect", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		// Valid turns first so the tailer has a parseable prefix, then enough
		// consecutive unrecognized lines to trip Unparseable (threshold 10).
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"hi"}}`,
			`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":"hello"}}`)
		n := &Node{ID: "n", Agent: "claude", Transcript: path}
		a := newPollApp(t, n, pollRunner(true, pane, false, nil))
		a.prevCap[n.ID] = pane
		a.lastChg[n.ID] = time.Now().Add(-10 * time.Second)
		_ = a.tailerFor(n)
		junk := make([]string, 10)
		for i := range junk {
			junk[i] = "not-json-line"
		}
		appendLines(t, path, junk...)
		a.live[n.ID] = "quiet"
		a.poll()
		tl := a.tailers[n.ID]
		if tl == nil || !tl.Unparseable() {
			t.Fatalf("setup: tailer unparseable = %v", tl != nil && tl.Unparseable())
		}
		if got := a.attn[n.ID]; got != "inspect" {
			t.Fatalf("attention = %q, want inspect", got)
		}
	})
}

// TestPollQuietOrdinaryPaneNoClassifiedAttention: without structured WaitingOn
// evidence, ordinary or even dialoghint-matching pane text must never become
// approval/question. Dialoghint may classify "dialog"; plain text → inspect or "".
func TestPollQuietOrdinaryPaneNoClassifiedAttention(t *testing.T) {
	t.Run("ordinary text without transcript is inspect not approval", func(t *testing.T) {
		pane := "Bash completed successfully\npermission granted earlier\n$"
		n := &Node{ID: "n", Agent: "claude"}
		a := newPollApp(t, n, pollRunner(true, pane, false, nil))
		a.prevCap[n.ID] = pane
		a.lastChg[n.ID] = time.Now().Add(-10 * time.Second)
		a.poll()
		if got := a.attn[n.ID]; got != "inspect" {
			t.Fatalf("attention = %q, want inspect", got)
		}
		if got := a.live[n.ID]; got != "quiet" {
			t.Fatalf("live = %q, want quiet", got)
		}
	})

	t.Run("healthy transcript quiet with non-dialog text is unflagged", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"done?"}}`,
			`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":"all done"}}`)
		pane := "all done\n$"
		n := &Node{ID: "n", Agent: "claude", Transcript: path}
		a := newPollApp(t, n, pollRunner(true, pane, false, nil))
		a.prevCap[n.ID] = pane
		a.lastChg[n.ID] = time.Now().Add(-10 * time.Second)
		a.poll()
		if got := a.attn[n.ID]; got != "" {
			t.Fatalf("attention = %q, want empty", got)
		}
		if got := a.live[n.ID]; got != "quiet" {
			t.Fatalf("live = %q, want quiet", got)
		}
	})

	t.Run("dialoghint alone classifies dialog not approval or question", func(t *testing.T) {
		// Full matcher shape (Esc to cancel) — classification must stay the
		// fenced "dialog" label, never structured approval/question.
		dialogPane := `Allow WebFetch to fetch https://example.com?
  1. Allow once
  2. Allow for this session
  3. Deny
  Esc to cancel`
		n := &Node{ID: "n", Agent: "claude"}
		a := newPollApp(t, n, pollRunner(true, dialogPane, false, nil))
		a.prevCap[n.ID] = dialogPane
		a.lastChg[n.ID] = time.Now().Add(-10 * time.Second)
		a.poll()
		if got := a.attn[n.ID]; got != "dialog" {
			t.Fatalf("attention = %q, want dialog", got)
		}
	})

	t.Run("quiet unresolved structured call classifies via attentionKind", func(t *testing.T) {
		// Complements TestDialogDetectionOrWithStructured (Bash→approval) with
		// AskUserQuestion→question on a non-dialog pane (structured wins alone).
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{}}]}}`)
		pane := "waiting on a form...\n$"
		n := &Node{ID: "n", Agent: "claude", Transcript: path}
		a := newPollApp(t, n, pollRunner(true, pane, false, nil))
		a.prevCap[n.ID] = pane
		a.lastChg[n.ID] = time.Now().Add(-10 * time.Second)
		a.poll()
		if got := a.attn[n.ID]; got != "question" {
			t.Fatalf("attention = %q, want question", got)
		}
		if got := a.live[n.ID]; got != "quiet" {
			t.Fatalf("live = %q, want quiet", got)
		}
	})
}

// TestPollActiveTranscriptProgressRestartsStallWindow: while a confined
// animation + unresolved call is waiting, transcript growth restarts the
// animStallAfter window so a working agent is not forced to inspect.
func TestPollActiveTranscriptProgressRestartsStallWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tx.jsonl")
	appendLines(t, path,
		`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{}}]}}`)
	// Confined spinner change; text does not match dialoghint.
	// Runner returns a slight confined change each tick after the baseline.
	tick := 12
	runner := func(ctx context.Context, stdin string, args ...string) (string, error) {
		for _, arg := range args {
			if arg == "has-session" {
				return "", nil
			}
			if arg == "capture-pane" {
				return fmt.Sprintf("running tools\n  · Bash (%ds)\nidle", tick), nil
			}
		}
		return "", nil
	}
	n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
	a := newPollApp(t, n, runner)
	a.prevCap[n.ID] = "running tools\n  · Bash (11s)\nidle"
	a.poll() // establish confined anim + stamp stall offset
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("fresh stall window: attention = %q, want none", got)
	}
	a.mu.Lock()
	st := a.anim[n.ID]
	if st == nil {
		a.mu.Unlock()
		t.Fatal("expected confined-animation state")
	}
	st.since = time.Now().Add(-2 * animStallAfter)
	a.mu.Unlock()

	// Transcript progress while still unresolved: another tool_use grows prog/off.
	appendLines(t, path,
		`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"tool_use","id":"c2","name":"Bash","input":{}}]}}`)
	tick = 13
	a.poll()
	if got := a.attn[n.ID]; got == "inspect" {
		t.Fatal("transcript progress must restart stall window; got inspect")
	}
	a.mu.Lock()
	st = a.anim[n.ID]
	if st == nil {
		a.mu.Unlock()
		t.Fatal("anim state cleared unexpectedly")
	}
	if time.Since(st.since) >= animStallAfter/2 {
		a.mu.Unlock()
		t.Fatalf("stall since not restarted after progress: age=%v", time.Since(st.since))
	}
	// Age again without further progress → inspect backstop.
	st.since = time.Now().Add(-2 * animStallAfter)
	a.mu.Unlock()
	tick = 14
	a.poll()
	if got := a.attn[n.ID]; got != "inspect" {
		t.Fatalf("stalled after restart: attention = %q, want inspect", got)
	}
}

// TestPollActiveToQuietTriggersRelink: poll itself (not a direct unit call)
// re-runs discovery when a claude pane finishes a phase the linked transcript
// did not carry. Persistence/ordering stay covered by MaybeRelink* and Packet 2E.
func TestPollActiveToQuietTriggersRelink(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}, capture: "static quiet pane"}
	a := newTestApp(t, f)
	proj := filepath.Join(a.home, ".claude", "projects", "-w-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(proj, "old-session.jsonl")
	newPath := filepath.Join(proj, "new-session.jsonl")
	for _, p := range []string{oldPath, newPath} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldPath, past, past); err != nil {
		t.Fatal(err)
	}

	n := &Node{ID: "c1", Agent: "claude", Dir: "/w/proj", Transcript: oldPath, SessionID: "old-session"}
	a.nodes = append(a.nodes, n)
	a.byID["c1"] = n
	a.live["c1"] = "active"
	a.prevCap["c1"] = "static quiet pane"
	a.lastChg["c1"] = time.Now().Add(-10 * time.Second)
	a.activeSince["c1"] = time.Now().Add(-30 * time.Second)

	a.poll()

	if got := a.live["c1"]; got != "quiet" {
		t.Fatalf("live = %q, want quiet (active→quiet transition)", got)
	}
	if n.Transcript != newPath {
		t.Fatalf("transcript = %q, want poll-driven relink to %q", n.Transcript, newPath)
	}
	if n.SessionID != "new-session" {
		t.Fatalf("session id = %q, want new-session", n.SessionID)
	}
}

// TestPollActiveUnconfinedDialogTextStaysUnflagged is a narrow liveness+attention
// cross-check: streaming geometry keeps the pane active and ignores dialog-
// shaped text. Full confined/unconfined matrix lives in
// TestActivePaneDialogCorroboration; this only asserts live state alongside.
func TestPollActiveUnconfinedDialogTextStaysUnflagged(t *testing.T) {
	dialog := "Bash command\n  go test\nDo you want to proceed?\n  1. Yes\n  2. No\n  Esc to cancel\nlots\nof\nstreaming\nlines\nhere"
	streamingPrev := "aa\nbb\ncc\ndd\nee\nff\ngg\nhh\nii"
	path := filepath.Join(t.TempDir(), "tx.jsonl")
	appendLines(t, path,
		`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{}}]}}`)
	n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
	a := newPollApp(t, n, pollRunner(true, dialog, false, nil))
	a.prevCap[n.ID] = streamingPrev
	a.poll()
	if got := a.live[n.ID]; got != "active" {
		t.Fatalf("live = %q, want active", got)
	}
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("unconfined streaming flagged: attention = %q", got)
	}
	if a.anim[n.ID] != nil {
		t.Fatal("unconfined diff must clear/not establish anim state")
	}
}

// ---------- pure helpers (gaps only) ----------

func TestChangedLines(t *testing.T) {
	cases := []struct {
		name      string
		prev, cur string
		max       int
		want      []int
	}{
		{"identical", "a\nb\nc", "a\nb\nc", 10, nil},
		{"single change", "a\nb\nc", "a\nX\nc", 10, []int{1}},
		{"max truncates", "a\nb\nc\nd", "A\nB\nC\nD", 2, []int{0, 1}},
		{"longer cur", "a", "a\nb\nc", 10, []int{1, 2}},
		{"longer prev", "a\nb\nc", "a", 10, []int{1, 2}},
		{"both empty", "", "", 5, nil},
		{"empty to content", "", "x", 5, []int{0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := changedLines(tc.prev, tc.cur, tc.max)
			if len(got) != len(tc.want) {
				t.Fatalf("changedLines = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("changedLines = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestUnionInts(t *testing.T) {
	cases := []struct {
		name string
		a, b []int
		want []int
	}{
		{"both empty", nil, nil, []int{}},
		{"left only", []int{1, 3}, nil, []int{1, 3}},
		{"right only", nil, []int{2, 4}, []int{2, 4}},
		{"merge unique", []int{1, 3, 5}, []int{2, 3, 6}, []int{1, 2, 3, 5, 6}},
		{"identical", []int{1, 2}, []int{1, 2}, []int{1, 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unionInts(tc.a, tc.b)
			if len(got) != len(tc.want) {
				t.Fatalf("unionInts = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("unionInts = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestNoteAnimBasics covers first-observation skip, confined establish, and
// unconfined clear. Spill/carry-over is TestNoteAnimSpillKeepsStallWindow.
func TestNoteAnimBasics(t *testing.T) {
	a := &app{anim: map[string]*animState{}}
	base := "l0\nl1\nl2\nl3\nl4"
	a.noteAnim("n", "", base) // no baseline
	if a.anim["n"] != nil {
		t.Fatal("first observation must not establish anim state")
	}
	step := "l0\nx1\nl2\nl3\nl4" // one line: confined
	a.noteAnim("n", base, step)
	st := a.anim["n"]
	if st == nil || len(st.lines) != 1 || st.lines[0] != 1 || st.off != -1 {
		t.Fatalf("confined establish: %+v", st)
	}
	// Another confined tick on the same line keeps the state.
	step2 := "l0\ny1\nl2\nl3\nl4"
	a.noteAnim("n", step, step2)
	if a.anim["n"] == nil {
		t.Fatal("continued confinement cleared state")
	}
	// Unconfined multi-line streaming clears it.
	wide := "A\nB\nC\nD\nE\nF"
	a.noteAnim("n", step2, wide)
	if a.anim["n"] != nil {
		t.Fatal("unconfined diff must clear anim state")
	}
}
