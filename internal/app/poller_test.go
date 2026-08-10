package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/tmuxsession"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// noteChatProgress turns transcript progress into the stale-chat signal:
// growth across a whole working phase (judge=true, the active→quiet
// transition) without one chat record sets it; recognized chat progress
// clears it the moment it arrives, even on an ordinary quiet tick; quiet
// ticks otherwise only advance the baseline (findings 21, 24).
func TestNoteChatProgress(t *testing.T) {
	a := &app{chatMark: map[string]chatMark{}, staleChat: map[string]bool{}}
	a.noteChatProgress("n", 100, 2, false) // quiet tick: baseline
	if a.staleChat["n"] {
		t.Fatal("baseline observation must not mark stale")
	}
	a.noteChatProgress("n", 300, 2, true) // phase ended: bytes grew, no chat record
	if !a.staleChat["n"] {
		t.Fatal("growth without chat progress across a phase must mark stale")
	}
	a.noteChatProgress("n", 320, 2, false) // quiet growth: baseline moves, no judgment
	if !a.staleChat["n"] {
		t.Fatal("stale must persist while no chat progress arrives")
	}
	a.noteChatProgress("n", 400, 3, false) // split record completed while quiet
	if a.staleChat["n"] {
		t.Fatal("chat progress must clear stale without another activity cycle")
	}
	a.noteChatProgress("n", 400, 3, true) // idle phase end: nothing grew
	if a.staleChat["n"] {
		t.Fatal("idle cycle must not mark stale")
	}
	a.noteChatProgress("n", 900, 3, false) // benign growth while quiet: no phase, no stale
	if a.staleChat["n"] {
		t.Fatal("quiet growth must not mark stale without a working phase")
	}
}

func newTailerTestApp() *app {
	return &app{tailers: map[string]*transcript.Tailer{},
		chatMark: map[string]chatMark{}, staleChat: map[string]bool{}}
}

// A (re)built tailer takes its baseline from the file's existing history —
// catch-up reading is calibration, never progress attributed to the current
// pane phase. So after a link, a relink, or a scimux restart, the very first
// active→quiet judgment sees only what arrived afterwards, and historical
// chat records cannot mask a first-phase incompatible record (findings 24, 28).
func TestTailerForBaselineFromHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jsonl")
	appendLines(t, path,
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"old question"}}`,
		`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":"old answer"}}`)

	// Fresh app: the restart/adoption case — history exists before any tailer.
	a := newTailerTestApp()
	n := &Node{ID: "n", Transcript: path}
	tl := a.tailerFor(n)
	if tl == nil {
		t.Fatal("no tailer built")
	}
	off, prog := tl.Progress()
	if prog != 1 {
		t.Fatalf("history agent records = %d, want 1", prog)
	}
	if m := a.chatMark["n"]; !m.seen || m.off != off || m.prog != prog {
		t.Fatalf("baseline must equal the catch-up watermark: mark %+v, tailer %d/%d", m, off, prog)
	}
	if a.tailerFor(n) != tl {
		t.Fatal("repeated calls must return the installed tailer")
	}

	// First phase after the (re)link appends only an incompatible assistant
	// shape: historical records must not clear the judgment.
	appendLines(t, path,
		`{"type":"assistant","timestamp":"t3","message":{"role":"assistant","content":{"v2_rich":"moved"}}}`)
	tl.Poll()
	off2, prog2 := tl.Progress()
	a.noteChatProgress("n", off2, prog2, true)
	if !a.staleChat["n"] {
		t.Fatal("first post-link phase with an incompatible record must mark stale despite valid history")
	}

	// Relinking to a different file resets staleness and re-baselines there.
	n.Transcript = filepath.Join(dir, "b.jsonl")
	if a.tailerFor(n) == tl {
		t.Fatal("relink must build a new tailer")
	}
	if a.staleChat["n"] {
		t.Fatal("relink must clear staleness measured against the old file")
	}
	if m := a.chatMark["n"]; !m.seen || m.off != 0 || m.prog != 0 {
		t.Fatalf("empty new file must baseline at zero, got %+v", m)
	}
}

// User-side records logged during a phase — the prompt itself, meta records,
// injected scaffolding — must not vouch for the assistant's response format:
// a phase whose only interpretable records are user-side ends stale when the
// assistant shape is unknown (finding 27).
func TestStaleChatNotMaskedByUserRecords(t *testing.T) {
	cases := map[string][]string{
		"meta and scaffolding plus unknown assistant": {
			`{"type":"user","isMeta":true,"message":{"role":"user","content":"<command-name>/clear</command-name>"}}`,
			`{"type":"response_item","timestamp":"t","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>…</environment_context>"}]}}`,
			`{"type":"assistant","timestamp":"t","message":{"role":"assistant","content":{"v2_rich":"moved"}}}`,
		},
		"valid user prompt plus unknown assistant": {
			`{"type":"user","timestamp":"t","message":{"role":"user","content":"sweep the thresholds"}}`,
			`{"type":"assistant","timestamp":"t","message":{"role":"assistant","content":{"v2_rich":"moved"}}}`,
		},
	}
	for name, lines := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.jsonl")
			appendLines(t, path) // create empty: node linked before the phase
			a := newTailerTestApp()
			n := &Node{ID: "n", Transcript: path}
			tl := a.tailerFor(n)
			appendLines(t, path, lines...)
			tl.Poll()
			off, prog := tl.Progress()
			a.noteChatProgress("n", off, prog, true)
			if !a.staleChat["n"] {
				t.Fatal("user-side records masked an unknown assistant shape")
			}
			// A later understood assistant record clears the signal.
			appendLines(t, path,
				`{"type":"assistant","timestamp":"t","message":{"role":"assistant","content":"back to a known shape"}}`)
			tl.Poll()
			off, prog = tl.Progress()
			a.noteChatProgress("n", off, prog, false)
			if a.staleChat["n"] {
				t.Fatal("recognized assistant progress must clear stale")
			}
		})
	}
}

func TestAttentionKind(t *testing.T) {
	cases := map[string]string{
		"AskUserQuestion": "question",
		"ExitPlanMode":    "question",
		"Bash":            "approval",
		"Edit":            "approval",
		"exec_command":    "approval",
		"":                "approval",
	}
	for tool, want := range cases {
		if got := attentionKind(tool); got != want {
			t.Errorf("attentionKind(%q) = %q, want %q", tool, got, want)
		}
	}
}

// TestPollerDialogDetection verifies that the poller's dialoghint integration
// fires attention independently of structured transcript parsing, covering
// terminal-only harnesses, format changes, and discovery failures (feedback round).
func TestPollerDialogDetection(t *testing.T) {
	// Approval dialog pane
	dialogPane := `Allow WebFetch to fetch https://example.com?
  1. Allow once
  2. Allow for this session
  3. Deny
  Esc to cancel`

	// Rate limit pane
	rateLimitPane := `You've hit your usage limit.
  1. Stop and wait for limit to reset
  2. Switch model`

	normalPane := "Agent working..."

	cases := []struct {
		name       string
		pane       string
		hasCapt    bool
		transcript string
		wantAttn   string
	}{
		{"approval dialog visible", dialogPane, true, "", "dialog"},
		{"rate limit visible", rateLimitPane, true, "", "dialog"},
		{"normal quiet pane", normalPane, true, "", "inspect"},        // no transcript → inspect
		{"capture error sets unavailable", dialogPane, false, "", ""}, // state=unavailable, not quiet
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captureErr := !tc.hasCapt
			runner := func(ctx context.Context, stdin string, args ...string) (string, error) {
				for _, arg := range args {
					if arg == "has-session" {
						return "", nil // session alive
					}
					if arg == "capture-pane" {
						if captureErr {
							return "", fmt.Errorf("capture failed")
						}
						return tc.pane, nil
					}
				}
				return "", nil
			}

			n := &Node{ID: "pi1", Agent: "pi", Transcript: tc.transcript}
			a := &app{
				byID:      map[string]*Node{"pi1": n},
				nodes:     []*Node{n},
				live:      map[string]string{},
				attn:      map[string]string{},
				prevCap:   map[string]string{"pi1": tc.pane}, // same content = no change = quiet
				lastChg:   map[string]time.Time{"pi1": time.Now().Add(-10 * time.Second)},
				tailers:   map[string]*transcript.Tailer{},
				chatMark:  map[string]chatMark{},
				staleChat: map[string]bool{},
				server:    tmuxsession.NewServerWithRunner("testsock", runner),
			}

			a.poll()

			if got := a.attn["pi1"]; got != tc.wantAttn {
				t.Errorf("attention = %q, want %q", got, tc.wantAttn)
			}
		})
	}
}

// TestDialogDetectionOrWithStructured verifies that regex and structured
// attention paths are independent: either can fire.
func TestDialogDetectionOrWithStructured(t *testing.T) {
	dialogPane := `Do you want to proceed?
  1. Yes
  2. No
  Esc to cancel`

	dir := t.TempDir()
	path := filepath.Join(dir, "tx.jsonl")
	appendLines(t, path,
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"run sweep"}}`,
		`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"tool_use","id":"call1","name":"Bash","input":{}}]}}`)

	runner := func(ctx context.Context, stdin string, args ...string) (string, error) {
		for _, arg := range args {
			if arg == "capture-pane" {
				return dialogPane, nil
			}
			if arg == "has-session" {
				return "", nil
			}
		}
		return "", nil
	}

	n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
	a := &app{
		byID:      map[string]*Node{"cl1": n},
		nodes:     []*Node{n},
		live:      map[string]string{},
		attn:      map[string]string{},
		prevCap:   map[string]string{"cl1": dialogPane}, // same content = quiet
		lastChg:   map[string]time.Time{"cl1": time.Now().Add(-10 * time.Second)},
		tailers:   map[string]*transcript.Tailer{},
		chatMark:  map[string]chatMark{},
		staleChat: map[string]bool{},
		server:    tmuxsession.NewServerWithRunner("testsock", runner),
	}

	a.poll()

	// Structured path fires first (unresolved Bash tool_use)
	if got := a.attn["cl1"]; got != "approval" {
		t.Fatalf("structured attention = %q, want approval", got)
	}

	// Now resolve the tool call, regex should fire
	appendLines(t, path,
		`{"type":"assistant","timestamp":"t3","message":{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"call1","content":"ok"}]}}`)

	a.poll()
	if got := a.attn["cl1"]; got != "dialog" {
		t.Fatalf("after structured resolved, regex attention = %q, want dialog", got)
	}
}

// TestActivePaneDialogCorroboration covers the quiet-gate blind spot: an
// approval dialog with a parallel tool call queued behind it animates the
// queued call's spinner, so the pane never goes quiet. When the transcript
// shows an unresolved call and the pane's changes stay confined to an
// animation strip, the dialoghint matcher classifies the wait; a pane whose
// changes are unconfined (streaming output) must stay unflagged even with a
// dialog on screen — that geometry means a running tool.
func TestActivePaneDialogCorroboration(t *testing.T) {
	dialog := "Bash command\n  go test ./...\nDo you want to proceed?\n  1. Yes\n  2. No\n  Esc to cancel\n  · queued: grep (12s)"
	confinedPrev := strings.Replace(dialog, "(12s)", "(11s)", 1) // only the spinner line differs
	streamingPrev := "aa\nbb\ncc\ndd\nee\nff\ngg"

	cases := []struct {
		name     string
		prev     string
		wantAttn string
	}{
		{"confined animation + dialog", confinedPrev, "approval"},
		{"unconfined changes stay running", streamingPrev, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tx.jsonl")
			appendLines(t, path,
				`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{}}]}}`,
				`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c2","name":"Bash","input":{}}]}}`)
			runner := func(ctx context.Context, stdin string, args ...string) (string, error) {
				for _, arg := range args {
					if arg == "capture-pane" {
						return dialog, nil
					}
					if arg == "has-session" {
						return "", nil
					}
				}
				return "", nil
			}
			n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
			a := &app{
				byID:      map[string]*Node{"cl1": n},
				nodes:     []*Node{n},
				live:      map[string]string{},
				attn:      map[string]string{},
				prevCap:   map[string]string{"cl1": tc.prev},
				lastChg:   map[string]time.Time{},
				tailers:   map[string]*transcript.Tailer{},
				chatMark:  map[string]chatMark{},
				staleChat: map[string]bool{},
				server:    tmuxsession.NewServerWithRunner("testsock", runner),
			}
			a.poll()
			if got := a.live["cl1"]; got != "active" {
				t.Fatalf("live = %q, want active", got)
			}
			if got := a.attn["cl1"]; got != tc.wantAttn {
				t.Errorf("attention = %q, want %q", got, tc.wantAttn)
			}
		})
	}
}

// TestActiveConfinedStallBackstop: a wait the matcher does not recognize
// (future TUI wording) still degrades to the neutral "inspect" once the pane
// has been animating in place with an unresolved call and a stalled
// transcript for animStallAfter — never a classified dialog.
func TestActiveConfinedStallBackstop(t *testing.T) {
	pane := "Pick an action\n  1. Continue\n  ▸ waiting (11s)"
	prev := strings.Replace(pane, "(11s)", "(10s)", 1)
	path := filepath.Join(t.TempDir(), "tx.jsonl")
	appendLines(t, path,
		`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{}}]}}`)
	runner := func(ctx context.Context, stdin string, args ...string) (string, error) {
		for _, arg := range args {
			if arg == "capture-pane" {
				return pane, nil
			}
			if arg == "has-session" {
				return "", nil
			}
		}
		return "", nil
	}
	n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
	a := &app{
		byID:      map[string]*Node{"cl1": n},
		nodes:     []*Node{n},
		live:      map[string]string{},
		attn:      map[string]string{},
		prevCap:   map[string]string{"cl1": prev},
		lastChg:   map[string]time.Time{},
		tailers:   map[string]*transcript.Tailer{},
		chatMark:  map[string]chatMark{},
		staleChat: map[string]bool{},
		server:    tmuxsession.NewServerWithRunner("testsock", runner),
	}
	a.poll() // establishes the confined-animation state and its offset
	if got := a.attn["cl1"]; got != "" {
		t.Fatalf("fresh stall window: attention = %q, want none", got)
	}
	a.mu.Lock()
	st := a.anim["cl1"]
	if st == nil {
		a.mu.Unlock()
		t.Fatal("expected confined-animation state after poll")
	}
	st.since = time.Now().Add(-2 * animStallAfter)
	a.mu.Unlock()
	a.poll()
	if got := a.attn["cl1"]; got != "inspect" {
		t.Errorf("stalled wait: attention = %q, want inspect", got)
	}
}

// TestActivePaneKeepsPeekSetAttention: attention set by the one-shot peek
// path (notePeekDialog) must survive the poller's active branch while the
// corroborated check is indeterminate — an unresolved call but no confined-
// animation state (a full-pane redraw deleted it). Without preservation the
// human-confirmed approval is wiped one tick later (R20.5). It still clears
// mechanically once the tool call resolves.
func TestActivePaneKeepsPeekSetAttention(t *testing.T) {
	pane := "line0\nline1\nline2\nline3\nline4\nline5\nline6"
	prev := "aa\nbb\ncc\ndd\nee\nff\ngg" // unconfined diff: anim state stays nil
	path := filepath.Join(t.TempDir(), "tx.jsonl")
	appendLines(t, path,
		`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{}}]}}`)
	runner := func(ctx context.Context, stdin string, args ...string) (string, error) {
		for _, arg := range args {
			if arg == "capture-pane" {
				return pane, nil
			}
			if arg == "has-session" {
				return "", nil
			}
		}
		return "", nil
	}
	n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
	a := &app{
		byID:      map[string]*Node{"cl1": n},
		nodes:     []*Node{n},
		live:      map[string]string{},
		attn:      map[string]string{"cl1": "approval"},    // set by notePeekDialog
		attnAt:    map[string]time.Time{"cl1": time.Now()}, // freshly, this instant
		prevCap:   map[string]string{"cl1": prev},
		lastChg:   map[string]time.Time{},
		tailers:   map[string]*transcript.Tailer{},
		chatMark:  map[string]chatMark{},
		staleChat: map[string]bool{},
		server:    tmuxsession.NewServerWithRunner("testsock", runner),
	}
	a.poll()
	if got := a.live["cl1"]; got != "active" {
		t.Fatalf("live = %q, want active", got)
	}
	if got := a.attn["cl1"]; got != "approval" {
		t.Errorf("peek-set attention wiped by the active branch: %q, want approval", got)
	}
	// Once the call resolves, the preserved attention clears mechanically.
	appendLines(t, path,
		`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}`)
	a.poll()
	if got := a.attn["cl1"]; got != "" {
		t.Errorf("attention not cleared after the call resolved: %q", got)
	}
}

// TestPeekSetAttentionExpiresAfterStall: a dialog answered in the terminal
// touches no server state, so the tool call stays unresolved (its result record
// lands only when the tool completes) and the active branch keeps preserving
// attention. Bound that: once the last fresh classification is older than
// animStallAfter, the preserved attention must age out rather than pin the card
// on "approval" for the whole runtime of the approved tool (R21.2).
func TestPeekSetAttentionExpiresAfterStall(t *testing.T) {
	pane := "line0\nline1\nline2\nline3\nline4\nline5\nline6"
	prev := "aa\nbb\ncc\ndd\nee\nff\ngg" // unconfined diff: anim state stays nil
	path := filepath.Join(t.TempDir(), "tx.jsonl")
	appendLines(t, path,
		`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{}}]}}`)
	runner := func(ctx context.Context, stdin string, args ...string) (string, error) {
		for _, arg := range args {
			if arg == "capture-pane" {
				return pane, nil
			}
			if arg == "has-session" {
				return "", nil
			}
		}
		return "", nil
	}
	n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
	a := &app{
		byID:  map[string]*Node{"cl1": n},
		nodes: []*Node{n},
		live:  map[string]string{},
		attn:  map[string]string{"cl1": "approval"},
		// Classified more than the stall window ago (answered in the terminal
		// since; nothing refreshed the stamp).
		attnAt:    map[string]time.Time{"cl1": time.Now().Add(-animStallAfter - time.Second)},
		prevCap:   map[string]string{"cl1": prev},
		lastChg:   map[string]time.Time{},
		tailers:   map[string]*transcript.Tailer{},
		chatMark:  map[string]chatMark{},
		staleChat: map[string]bool{},
		server:    tmuxsession.NewServerWithRunner("testsock", runner),
	}
	a.poll()
	if got := a.attn["cl1"]; got != "" {
		t.Errorf("stale peek-set attention not aged out: %q, want empty", got)
	}
}

// TestNoteAnimSpillKeepsStallWindow: when the accumulated line-union spills
// past animMaxLines but the instantaneous diff is still confined (the
// animation strip drifted position), the stall window must carry over —
// resetting since/off on every drift would postpone the 90s inspect backstop
// forever (R20.6).
func TestNoteAnimSpillKeepsStallWindow(t *testing.T) {
	a := &app{anim: map[string]*animState{}}
	base := "l0\nl1\nl2\nl3\nl4\nl5"
	step1 := "l0\nx1\nx2\nx3\nl4\nl5" // lines 1,2,3 change: confined
	a.noteAnim("n", base, step1)
	st := a.anim["n"]
	if st == nil {
		t.Fatal("expected confined-animation state")
	}
	old := time.Now().Add(-time.Hour)
	st.since, st.off = old, 42

	step2 := "l0\nx1\nx2\nx3\nl4\ny5" // line 5 changes: union {1,2,3,5} spills, diff confined
	a.noteAnim("n", step1, step2)
	st2 := a.anim["n"]
	if st2 == nil {
		t.Fatal("re-confined state missing after spill")
	}
	if !st2.since.Equal(old) || st2.off != 42 {
		t.Errorf("stall window reset on drift: since=%v off=%d, want carried over", st2.since, st2.off)
	}
	if len(st2.lines) != 1 || st2.lines[0] != 5 {
		t.Errorf("re-confined lines = %v, want [5]", st2.lines)
	}
	// Genuinely unconfined output still clears the state.
	a.noteAnim("n", step2, base)
	if a.anim["n"] != nil {
		t.Error("unconfined diff should clear the anim state")
	}
}

func TestWarmStartupMirrorsAndCachesSegments(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}, capture: "ready"}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sess-1.jsonl")
	appendFile(t, tp, claudeTurn("user", "question", "t1")+claudeTurn("assistant", "answer", "t2"))
	n := &Node{ID: "c1", Title: "c1", Agent: "claude", Model: "opus", Dir: "/wd",
		Transcript: tp, CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes, a.byID["c1"] = []*Node{n}, n

	a.warmStartup()

	evs := contentEvents(logEvents(t, a, "c1"))
	if got, want := kinds(evs), []string{"meta", "source", "user", "assistant"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("warm startup log events = %v, want %v", got, want)
	}
	a.mu.Lock()
	c := a.segCache["c1"]
	a.mu.Unlock()
	if c == nil {
		t.Fatal("warm startup did not populate the segment cache")
	}
	seg := a.segment(n)
	if len(seg.Turns) != 2 || seg.Turns[0].Text != "question" || seg.Turns[1].Text != "answer" {
		t.Fatalf("warm startup segment = %+v", seg.Turns)
	}
}

// TestMaybeRelinkTranscriptAfterSessionRollover: a /clear or relaunch inside
// the pane starts a new session file; once the pane finishes a working phase
// the stale link never carried, discovery must re-run and relink — otherwise
// the chat freezes on the old conversation forever (restarts replay the store
// and change nothing).
func TestMaybeRelinkTranscriptAfterSessionRollover(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}}
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
	a.activeSince["c1"] = time.Now().Add(-30 * time.Second)

	a.maybeRelinkTranscript(n)

	if n.Transcript != newPath {
		t.Fatalf("transcript = %q, want relink to %q", n.Transcript, newPath)
	}
	if n.SessionID != "new-session" {
		t.Fatalf("session id = %q, want new-session", n.SessionID)
	}
	b, err := os.ReadFile(a.storePath)
	if err != nil || !strings.Contains(string(b), newPath) {
		t.Fatalf("relink not persisted to store: %v\n%s", err, b)
	}
}

// TestMaybeRelinkTranscriptIgnoresStaleCmdlineSession: the pane cmdline names
// the session claude was *launched* with; after an in-pane /clear the process
// keeps that argv while writing a new session file. Relinking must reject a
// cmdline-derived transcript that did not carry the finished phase — trusting
// it rebinds the dead pre-/clear file and needs-input (the approval watcher)
// goes permanently blind on that node.
func TestMaybeRelinkTranscriptIgnoresStaleCmdlineSession(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}}
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
	// The pane process still advertises the retired launch session.
	a.paneSession = func(pid string) string { return "old-session" }

	// /clear through scimux retired the link; the first phase of the new
	// session just ended.
	n := &Node{ID: "c1", Agent: "claude", Dir: "/w/proj"}
	a.nodes = append(a.nodes, n)
	a.byID["c1"] = n
	a.activeSince["c1"] = time.Now().Add(-30 * time.Second)

	a.maybeRelinkTranscript(n)

	if n.Transcript != newPath {
		t.Fatalf("transcript = %q, want %q (stale cmdline session id must not win)", n.Transcript, newPath)
	}
	if n.SessionID != "new-session" {
		t.Fatalf("session id = %q, want new-session", n.SessionID)
	}
}

// A linked transcript that carried the phase (mtime after phase start) is
// healthy — a newer sibling session file must not steal the link.
func TestMaybeRelinkTranscriptKeepsHealthyLink(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}}
	a := newTestApp(t, f)
	proj := filepath.Join(a.home, ".claude", "projects", "-w-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(proj, "linked.jsonl")
	other := filepath.Join(proj, "other.jsonl")
	for _, p := range []string{linked, other} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	n := &Node{ID: "c1", Agent: "claude", Dir: "/w/proj", Transcript: linked}
	a.nodes = append(a.nodes, n)
	a.byID["c1"] = n
	a.activeSince["c1"] = time.Now().Add(-30 * time.Second)

	a.maybeRelinkTranscript(n)

	if n.Transcript != linked {
		t.Fatalf("healthy link was stolen: %q", n.Transcript)
	}
}

// P1b/P1c — quiet-pane owing stall and structural dialog matcher. The exact
// reported case: Write/Edit approval on screen, no unresolved tool_use in the
// transcript (Claude Code flushes late), healthy transcript, quiet pane.
func TestQuietOwingStallAndStructuralDialog(t *testing.T) {
	// Non-matching pane text so only the mechanical backstop can fire.
	plainPane := "working…\n$"
	// Current Write/Edit dialog wording — no "proceed"/"allow" verbs.
	editDialog := ` Do you want to make this edit to hello.txt?
 ❯ 1. Yes
   2. Yes, allow all edits during this session (shift+tab)
   3. No

 Esc to cancel · Tab to amend`
	// Active pane: content differs from prevCap each observation.
	activePrev := "tick-a\nline2\nline3\nline4\nline5\nline6\nline7"
	activeCur := "tick-b\nline2\nline3\nline4\nline5\nline6\nline7"

	quietRunner := func(pane string) tmuxsession.Runner {
		return func(ctx context.Context, stdin string, args ...string) (string, error) {
			for _, arg := range args {
				if arg == "has-session" {
					return "", nil
				}
				if arg == "capture-pane" {
					return pane, nil
				}
			}
			return "", nil
		}
	}

	// (a) quiet + owing + past the stall ⇒ inspect, no unresolved call.
	t.Run("owing_past_stall_inspect", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"edit hello.txt"}}`)
		n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
		a := &app{
			byID:      map[string]*Node{"cl1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			prevCap:   map[string]string{"cl1": plainPane},
			lastChg:   map[string]time.Time{"cl1": time.Now().Add(-2 * owedStallAfter)},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			server:    tmuxsession.NewServerWithRunner("testsock", quietRunner(plainPane)),
		}
		a.poll()
		if got := a.live["cl1"]; got != "quiet" {
			t.Fatalf("live = %q, want quiet", got)
		}
		if got := a.attn["cl1"]; got != "inspect" {
			t.Errorf("attention = %q, want inspect", got)
		}
	})

	// (b) quiet + owing + inside the stall ⇒ none.
	t.Run("owing_inside_stall_none", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"edit hello.txt"}}`)
		n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
		a := &app{
			byID:      map[string]*Node{"cl1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			prevCap:   map[string]string{"cl1": plainPane},
			lastChg:   map[string]time.Time{"cl1": time.Now().Add(-10 * time.Second)},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			server:    tmuxsession.NewServerWithRunner("testsock", quietRunner(plainPane)),
		}
		a.poll()
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention = %q, want none inside stall", got)
		}
	})

	// (c) quiet with newest turn an assistant turn ⇒ none, however long.
	t.Run("finished_turn_never_inspect", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"hi"}}`,
			`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
		n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
		a := &app{
			byID:      map[string]*Node{"cl1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			prevCap:   map[string]string{"cl1": plainPane},
			lastChg:   map[string]time.Time{"cl1": time.Now().Add(-2 * owedStallAfter)},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			server:    tmuxsession.NewServerWithRunner("testsock", quietRunner(plainPane)),
		}
		a.poll()
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention = %q, want none on finished turn", got)
		}
	})

	// (d) matcher hit classifies as dialog immediately, before the stall elapses.
	// Quiet needs lastChg ≥ 8s; owedStallAfter is 45s — use 10s to sit between.
	t.Run("structural_matcher_dialog_immediate", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"edit hello.txt"}}`)
		n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
		a := &app{
			byID:      map[string]*Node{"cl1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			prevCap:   map[string]string{"cl1": editDialog},
			lastChg:   map[string]time.Time{"cl1": time.Now().Add(-10 * time.Second)},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			server:    tmuxsession.NewServerWithRunner("testsock", quietRunner(editDialog)),
		}
		a.poll()
		if got := a.live["cl1"]; got != "quiet" {
			t.Fatalf("live = %q, want quiet", got)
		}
		if got := a.attn["cl1"]; got != "dialog" {
			t.Errorf("attention = %q, want dialog from structural matcher", got)
		}
	})

	// (e) an active pane takes no new path (owing backstop is quiet-only).
	t.Run("active_pane_no_owing_path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"edit hello.txt"}}`)
		n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
		a := &app{
			byID:      map[string]*Node{"cl1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			prevCap:   map[string]string{"cl1": activePrev},
			lastChg:   map[string]time.Time{"cl1": time.Now().Add(-2 * owedStallAfter)},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			anim:      map[string]*animState{},
			server:    tmuxsession.NewServerWithRunner("testsock", quietRunner(activeCur)),
		}
		a.poll()
		if got := a.live["cl1"]; got != "active" {
			t.Fatalf("live = %q, want active", got)
		}
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention = %q, want none on active pane", got)
		}
	})

	// (f) attention clears once the transcript grows (assistant turn lands).
	t.Run("clears_when_transcript_grows", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"edit hello.txt"}}`)
		n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
		a := &app{
			byID:      map[string]*Node{"cl1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			prevCap:   map[string]string{"cl1": plainPane},
			lastChg:   map[string]time.Time{"cl1": time.Now().Add(-2 * owedStallAfter)},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			server:    tmuxsession.NewServerWithRunner("testsock", quietRunner(plainPane)),
		}
		a.poll()
		if got := a.attn["cl1"]; got != "inspect" {
			t.Fatalf("setup: attention = %q, want inspect", got)
		}
		appendLines(t, path,
			`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"text","text":"edited"}]}}`)
		a.poll()
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention after growth = %q, want cleared", got)
		}
	})
}
