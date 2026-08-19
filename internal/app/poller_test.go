package app

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/dialoghint"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
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
	a.initMaps()
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

// A deferred Claude prompt can land in the transcript after the synchronous
// create deadline. That is late evidence, not permanent ambiguity: the poller
// must release the initial-only send gate as soon as the mirrored user turn
// matches the durable Node.Prompt. Ordinary unconfirmed follow-up sends remain
// manual, and a different user turn proves nothing about the initial prompt.
func TestPollReconcilesLateClaudeInitialDelivery(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      string
		turn       string
		wantLocked bool
	}{
		{"matching initial turn self-heals", "initial_unconfirmed", "irreplaceable prompt", false},
		{"different turn stays uncertain", "initial_unconfirmed", "different prompt", true},
		{"ordinary unconfirmed send stays manual", "unconfirmed", "irreplaceable prompt", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTmux{list: []string{"cl1"}, capture: "working"}
			a := newTestApp(t, f)
			if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "claude.jsonl")
			appendFile(t, path, claudeTurn("user", tc.turn, "2026-08-14T12:00:00Z"))
			n := &Node{
				ID: "cl1", Title: "Claude", Agent: "claude", Dir: a.home,
				Prompt: "irreplaceable prompt", Transcript: path,
			}
			a.nodes = []*Node{n}
			a.byID[n.ID] = n
			a.sendState[n.ID] = tc.state

			a.poll()

			a.mu.Lock()
			_, locked := a.sendState[n.ID]
			a.mu.Unlock()
			if locked != tc.wantLocked {
				t.Fatalf("send gate locked = %v, want %v", locked, tc.wantLocked)
			}
		})
	}
}

func TestReconcileClaudeInitialDeliveryCanonicalNewlines(t *testing.T) {
	// AT-CR-03/04: late reconciliation uses the same canonical compare as the
	// synchronous create path. A CR-only mismatch must self-heal; different
	// text must not.
	cases := []struct {
		at, name, prompt, turn string
		wantLocked             bool
	}{
		{"AT-CR-03", "cr turn heals lf prompt", "irreplaceable\nprompt", "irreplaceable\rprompt", false},
		{"AT-CR-03", "crlf turn heals lf prompt", "irreplaceable\nprompt", "irreplaceable\r\nprompt", false},
		{"AT-CR-04", "different late turn stays uncertain", "irreplaceable\nprompt", "different prompt", true},
	}
	for _, tc := range cases {
		t.Run(tc.at+"/"+tc.name, func(t *testing.T) {
			f := &fakeTmux{list: []string{"cl1"}, capture: "working"}
			a := newTestApp(t, f)
			if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "claude.jsonl")
			appendFile(t, path, claudeTurn("user", tc.turn, "2026-08-14T12:00:00Z"))
			n := &Node{
				ID: "cl1", Title: "Claude", Agent: "claude", Dir: a.home,
				Prompt: tc.prompt, Transcript: path,
			}
			a.nodes = []*Node{n}
			a.byID[n.ID] = n
			a.sendState[n.ID] = sendInitialUnconfirmed

			a.poll()

			a.mu.Lock()
			_, locked := a.sendState[n.ID]
			a.mu.Unlock()
			if locked != tc.wantLocked {
				t.Fatalf("send gate locked = %v, want %v", locked, tc.wantLocked)
			}
		})
	}
}

func newTailerTestApp() *app {
	a := &app{tailers: map[string]*transcript.Tailer{},
		chatMark: map[string]chatMark{}, staleChat: map[string]bool{}}
	a.initMaps()
	return a
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
				sub := ""
				if len(args) >= 3 {
					sub = args[2]
				}
				switch sub {
				case "list-sessions":
					return "pi1", nil
				case "has-session":
					return "", nil // session alive
				case "capture-pane":
					if captureErr {
						return "", fmt.Errorf("capture failed")
					}
					return tc.pane, nil
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
			a.initMaps()

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
			if arg == "list-sessions" {
				return "cl1\npi1", nil
			}
			if arg == "has-session" {
				return "", nil
			}
		}
		return "", nil
	}

	n := tmuxFallbackNode("cl1", path)
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
	a.initMaps()

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
					return dialog, nil
				}
				return "", nil
			}
			n := tmuxFallbackNode("cl1", path)
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
			a.initMaps()
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
			if arg == "list-sessions" {
				return "cl1\npi1", nil
			}
			if arg == "has-session" {
				return "", nil
			}
		}
		return "", nil
	}
	n := tmuxFallbackNode("cl1", path)
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
	a.initMaps()
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
			if arg == "list-sessions" {
				return "cl1\npi1", nil
			}
			if arg == "has-session" {
				return "", nil
			}
		}
		return "", nil
	}
	n := tmuxFallbackNode("cl1", path)
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
	a.initMaps()
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
			if arg == "list-sessions" {
				return "cl1\npi1", nil
			}
			if arg == "has-session" {
				return "", nil
			}
		}
		return "", nil
	}
	n := tmuxFallbackNode("cl1", path)
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
	a.initMaps()
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
	a.initMaps()
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
	c := a.logCache["c1"]
	a.mu.Unlock()
	if c == nil {
		t.Fatal("warm startup did not populate the log cache")
	}
	seg := a.segment(n)
	if len(seg.Turns) != 2 || seg.Turns[0].Text != "question" || seg.Turns[1].Text != "answer" {
		t.Fatalf("warm startup segment = %+v", seg.Turns)
	}
}

// TestMaybeRelinkTranscriptAfterSessionRollover: a stale linked transcript
// after /clear must not be replaced by the newest file in the directory.
// Legacy (no hook) nodes detach; hook-owned nodes wait for SessionStart.
func TestMaybeRelinkTranscriptAfterSessionRollover(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}}
	a := newTestApp(t, f)
	proj := filepath.Join(a.home, ".claude", "projects", "-w-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(proj, "old-session.jsonl")
	newPath := filepath.Join(proj, "new-session.jsonl")
	oldTS := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	newTS := time.Now().UTC().Format(time.RFC3339Nano)
	appendLines(t, oldPath, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"old"}}`, oldTS))
	appendLines(t, newPath, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"new"}}`, newTS))
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldPath, past, past); err != nil {
		t.Fatal(err)
	}

	n := &Node{ID: "c1", Agent: "claude", Dir: "/w/proj", Transcript: oldPath, SessionID: "old-session"}
	a.nodes = append(a.nodes, n)
	a.byID["c1"] = n
	a.activeSince["c1"] = time.Now().Add(-30 * time.Second)
	// The prompt this file never recorded is the staleness evidence; a bare
	// pane phase proves nothing (D1/D2).
	a.noteDelivery("c1", time.Now().Add(-time.Minute))

	a.maybeRelinkTranscript(n)

	if n.Transcript == newPath || n.SessionID == "new-session" {
		t.Fatalf("newest-file guess rebound %q / %q", n.Transcript, n.SessionID)
	}
	if n.Transcript != "" || n.SessionID != "" {
		t.Fatalf("legacy stale link must detach, got %q / %q", n.Transcript, n.SessionID)
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
	oldTS := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	newTS := time.Now().UTC().Format(time.RFC3339Nano)
	appendLines(t, oldPath, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"old"}}`, oldTS))
	appendLines(t, newPath, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"new"}}`, newTS))
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

	if n.Transcript != "" || n.SessionID != "" {
		t.Fatalf("detached node must stay detached without a hook, got %q / %q", n.Transcript, n.SessionID)
	}
}

// A linked transcript that carried the phase (content time after phase start)
// is healthy — a newer sibling session file must not steal the link.
func TestMaybeRelinkTranscriptKeepsHealthyLink(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}}
	a := newTestApp(t, f)
	proj := filepath.Join(a.home, ".claude", "projects", "-w-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(proj, "linked.jsonl")
	other := filepath.Join(proj, "other.jsonl")
	// Linked file's content is after the phase start — it carried the work.
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	appendLines(t, linked, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"linked"}}`, ts))
	appendLines(t, other, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"other"}}`, ts))

	n := &Node{ID: "c1", Agent: "claude", Dir: "/w/proj", Transcript: linked}
	a.nodes = append(a.nodes, n)
	a.byID["c1"] = n
	a.activeSince["c1"] = time.Now().Add(-30 * time.Second)

	a.maybeRelinkTranscript(n)

	if n.Transcript != linked {
		t.Fatalf("healthy link was stolen: %q", n.Transcript)
	}
}

// P5 item 10 — turn_done is derived in the quiet branch with zero extra I/O:
// quiet + Delivered + no pending call + no attention + not ended. ACP nodes
// (no Tailer) never raise it — absent means UNKNOWN, not "not finished".
func TestTurnDoneQuietDelivered(t *testing.T) {
	plainPane := "working…\n$"
	quietRunner := func(pane string, sessions ...string) tmuxsession.Runner {
		if len(sessions) == 0 {
			sessions = []string{"cl1"}
		}
		listOut := strings.Join(sessions, "\n")
		return func(ctx context.Context, stdin string, args ...string) (string, error) {
			sub := ""
			if len(args) >= 3 {
				sub = args[2]
			}
			switch sub {
			case "list-sessions":
				return listOut, nil
			case "has-session":
				return "", nil
			case "capture-pane":
				return pane, nil
			}
			return "", nil
		}
	}
	// Delivery age decides prominence (P5 review): fixtures stamp their turns
	// with real RFC3339 times so each subtest fails for its own reason and not
	// because an unparseable "t1" left the delivery undated.
	stampAgo := func(d time.Duration) string {
		return time.Now().Add(-d).UTC().Format(time.RFC3339Nano)
	}
	mk := func(t *testing.T, path string, extra func(*app, *Node)) *app {
		t.Helper()
		n := tmuxFallbackNode("cl1", path)
		a := &app{
			byID:      map[string]*Node{"cl1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			turnDone:  map[string]bool{},
			prevCap:   map[string]string{"cl1": plainPane},
			lastChg:   map[string]time.Time{"cl1": time.Now().Add(-10 * time.Second)},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			server:    tmuxsession.NewServerWithRunner("testsock", quietRunner(plainPane)),
		}
		a.initMaps()
		if extra != nil {
			extra(a, n)
		}
		return a
	}

	t.Run("finished_when_quiet_delivered_no_pending", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"`+stampAgo(90*time.Second)+`","message":{"role":"user","content":"hi"}}`,
			`{"type":"assistant","timestamp":"`+stampAgo(30*time.Second)+`","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
		a := mk(t, path, nil)
		a.poll()
		if got := a.live["cl1"]; got != "quiet" {
			t.Fatalf("live = %q, want quiet", got)
		}
		if got := a.attn["cl1"]; got != "" {
			t.Fatalf("attention = %q, want none", got)
		}
		if !a.turnDone["cl1"] {
			t.Fatal("turnDone = false, want true for quiet+delivered+no pending")
		}
	})

	t.Run("not_finished_when_delivery_is_old", func(t *testing.T) {
		// Prominence is for a turn you might have just missed. An idle live
		// session that answered hours ago is still honestly idle, but it must
		// not hold a top-tier slot and a pulsing ring forever (P5 review).
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"`+stampAgo(3*time.Hour)+`","message":{"role":"user","content":"hi"}}`,
			`{"type":"assistant","timestamp":"`+stampAgo(2*time.Hour)+`","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
		a := mk(t, path, nil)
		a.poll()
		if got := a.attn["cl1"]; got != "" {
			t.Fatalf("attention = %q, want none", got)
		}
		if a.turnDone["cl1"] {
			t.Fatal("turnDone = true, want false once the delivery is older than turnDoneWindow")
		}
	})

	t.Run("not_finished_when_delivery_is_undated", func(t *testing.T) {
		// No parseable timestamp = no provable freshness. Claim nothing rather
		// than claim "just finished" (same rule as the ACP seam).
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","message":{"role":"user","content":"hi"}}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
		a := mk(t, path, nil)
		a.poll()
		if a.turnDone["cl1"] {
			t.Fatal("turnDone = true, want false when the delivery cannot be dated")
		}
	})

	t.Run("not_finished_while_owing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"`+stampAgo(90*time.Second)+`","message":{"role":"user","content":"hi"}}`)
		a := mk(t, path, nil)
		// Keep lastChg recent enough that the owing stall has not fired inspect.
		a.lastChg["cl1"] = time.Now().Add(-10 * time.Second)
		a.poll()
		if a.turnDone["cl1"] {
			t.Fatal("turnDone = true, want false while agent still owes output")
		}
	})

	t.Run("not_finished_with_pending_tool_call", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"`+stampAgo(90*time.Second)+`","message":{"role":"user","content":"run it"}}`,
			`{"type":"assistant","timestamp":"`+stampAgo(30*time.Second)+`","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`)
		a := mk(t, path, nil)
		a.poll()
		// Pending Bash may classify as attention via WaitingOn; either way not finished.
		if a.turnDone["cl1"] {
			t.Fatal("turnDone = true, want false with unresolved tool call")
		}
	})

	t.Run("not_finished_when_attention_classified", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"`+stampAgo(90*time.Second)+`","message":{"role":"user","content":"hi"}}`,
			`{"type":"assistant","timestamp":"`+stampAgo(30*time.Second)+`","message":{"role":"assistant","content":[{"type":"text","text":"done"},{"type":"tool_use","id":"tu1","name":"AskUserQuestion","input":{}}]}}`)
		a := mk(t, path, nil)
		a.poll()
		if a.attn["cl1"] == "" {
			t.Fatal("expected attention from AskUserQuestion, got none")
		}
		if a.turnDone["cl1"] {
			t.Fatal("turnDone must not fire when attention is classified")
		}
	})

	t.Run("not_finished_when_dialog_without_pending", func(t *testing.T) {
		// Isolates the attn == "" clause. The two subtests above cannot: both
		// of their fixtures also carry an unresolved tool call, so dropping
		// attn == "" leaves PendingCount() covering them (and vice versa).
		// This is P1's exact case — Claude Code flushes the tool_use record
		// late, so the newest record is the assistant's text with nothing
		// pending while an approval dialog sits on the pane. Without the
		// clause the map would call that blocked chat "Ready".
		dialogPane := "Do you want to proceed?\n  1. Yes\n  2. No\n  Esc to cancel"
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"`+stampAgo(90*time.Second)+`","message":{"role":"user","content":"run sweep"}}`,
			`{"type":"assistant","timestamp":"`+stampAgo(30*time.Second)+`","message":{"role":"assistant","content":[{"type":"text","text":"about to run it"}]}}`)
		a := mk(t, path, func(a *app, n *Node) {
			a.prevCap["cl1"] = dialogPane // unchanged capture = quiet pane
			a.server = tmuxsession.NewServerWithRunner("testsock", quietRunner(dialogPane))
		})
		a.poll()
		if got := a.attn["cl1"]; got != "dialog" {
			t.Fatalf("attention = %q, want dialog from the matcher", got)
		}
		if a.turnDone["cl1"] {
			t.Fatal("turnDone must not fire while any attention is classified")
		}
	})

	t.Run("not_finished_when_ended", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"`+stampAgo(90*time.Second)+`","message":{"role":"user","content":"hi"}}`,
			`{"type":"assistant","timestamp":"`+stampAgo(30*time.Second)+`","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
		a := mk(t, path, func(a *app, n *Node) {
			n.EndedAt = "2026-01-01T00:00:00Z"
		})
		a.poll()
		if a.turnDone["cl1"] {
			t.Fatal("turnDone = true, want false on ended node")
		}
	})

	t.Run("cleared_when_active", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"`+stampAgo(90*time.Second)+`","message":{"role":"user","content":"hi"}}`,
			`{"type":"assistant","timestamp":"`+stampAgo(30*time.Second)+`","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
		a := mk(t, path, nil)
		a.turnDone["cl1"] = true      // stale flag from a previous quiet tick
		a.lastChg["cl1"] = time.Now() // pane just changed → active
		a.prevCap["cl1"] = "different\npane"
		a.server = tmuxsession.NewServerWithRunner("testsock", quietRunner("fresh\npane"))
		a.poll()
		if got := a.live["cl1"]; got != "active" {
			t.Fatalf("live = %q, want active", got)
		}
		if a.turnDone["cl1"] {
			t.Fatal("turnDone must clear when the pane is active")
		}
	})

	t.Run("acp_no_tailer_stays_unknown", func(t *testing.T) {
		// tailerFor returns nil when Transcript == ""; ACP nodes never reach
		// turn_done in this phase. Absent must mean UNKNOWN.
		n := &Node{ID: "pi1", Agent: "pi", Transcript: ""}
		a := &app{
			byID:      map[string]*Node{"pi1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			turnDone:  map[string]bool{},
			prevCap:   map[string]string{},
			lastChg:   map[string]time.Time{},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			// No proc manager → falls through to tmux path; no session → exited.
			server: tmuxsession.NewServerWithRunner("testsock", func(ctx context.Context, stdin string, args ...string) (string, error) {
				for _, arg := range args {
					if arg == "has-session" {
						return "", fmt.Errorf("no session")
					}
				}
				return "", nil
			}),
		}
		a.initMaps()
		a.poll()
		if a.turnDone["pi1"] {
			t.Fatal("ACP/no-transcript node must not set turnDone")
		}
	})
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

	quietRunner := func(pane string, sessions ...string) tmuxsession.Runner {
		if len(sessions) == 0 {
			sessions = []string{"cl1"}
		}
		listOut := strings.Join(sessions, "\n")
		return func(ctx context.Context, stdin string, args ...string) (string, error) {
			sub := ""
			if len(args) >= 3 {
				sub = args[2]
			}
			switch sub {
			case "list-sessions":
				return listOut, nil
			case "has-session":
				return "", nil
			case "capture-pane":
				return pane, nil
			}
			return "", nil
		}
	}

	// (a) quiet + owing + past the stall ⇒ inspect, no unresolved call.
	t.Run("owing_past_stall_inspect", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"edit hello.txt"}}`)
		n := tmuxFallbackNode("cl1", path)
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
		a.initMaps()
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
		n := tmuxFallbackNode("cl1", path)
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
		a.initMaps()
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
		n := tmuxFallbackNode("cl1", path)
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
		a.initMaps()
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
		n := tmuxFallbackNode("cl1", path)
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
		a.initMaps()
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
		n := tmuxFallbackNode("cl1", path)
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
		a.initMaps()
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
		n := tmuxFallbackNode("cl1", path)
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
		a.initMaps()
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

	// (g) the "esc to cancel" anchor corroborates an owing stall: same neutral
	// inspect, reached at owedStallCorroborated instead of owedStallAfter. The
	// pane carries the anchor but no numbered options, so the matcher does not
	// classify it — this is the shortening path, not the dialog path.
	anchorPane := "Waiting for your answer\n\n Esc to cancel · Tab to amend"
	t.Run("anchor_shortens_owing_stall", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"edit hello.txt"}}`)
		n := tmuxFallbackNode("cl1", path)
		a := &app{
			byID:      map[string]*Node{"cl1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			prevCap:   map[string]string{"cl1": anchorPane},
			lastChg:   map[string]time.Time{"cl1": time.Now().Add(-15 * time.Second)},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			server:    tmuxsession.NewServerWithRunner("testsock", quietRunner(anchorPane)),
		}
		a.initMaps()
		a.poll()
		if got := a.live["cl1"]; got != "quiet" {
			t.Fatalf("live = %q, want quiet", got)
		}
		if got := a.attn["cl1"]; got != "inspect" {
			t.Errorf("attention = %q, want inspect at the corroborated stall", got)
		}
	})

	// (h) the anchor never creates attention on its own: a finished turn owes
	// nothing, so no stall runs however long the pane sits with the phrase on
	// screen (a scimux session discussing dialoghint is exactly this pane).
	t.Run("anchor_without_owing_never_raises", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"hi"}}`,
			`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"text","text":"the matcher wants esc to cancel"}]}}`)
		n := tmuxFallbackNode("cl1", path)
		a := &app{
			byID:      map[string]*Node{"cl1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			prevCap:   map[string]string{"cl1": anchorPane},
			lastChg:   map[string]time.Time{"cl1": time.Now().Add(-4 * owedStallAfter)},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			server:    tmuxsession.NewServerWithRunner("testsock", quietRunner(anchorPane)),
		}
		a.initMaps()
		a.poll()
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention = %q, want none: the anchor corroborates, it never raises", got)
		}
	})

	// (i) without the anchor the full stall still applies — (b) pins the same
	// window from the other side, this one pins that the shortening is what
	// makes the difference at 15s.
	t.Run("no_anchor_keeps_full_stall", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"edit hello.txt"}}`)
		n := tmuxFallbackNode("cl1", path)
		a := &app{
			byID:      map[string]*Node{"cl1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			prevCap:   map[string]string{"cl1": plainPane},
			lastChg:   map[string]time.Time{"cl1": time.Now().Add(-15 * time.Second)},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			server:    tmuxsession.NewServerWithRunner("testsock", quietRunner(plainPane)),
		}
		a.initMaps()
		a.poll()
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention = %q, want none without the anchor at 15s", got)
		}
	})
}

// TestAXScreenReaderPollerPins locks screen-reader (AX) pane shapes into the
// existing poller paths: structural quiet-branch dialog fallback, mechanical
// elapsed-time activity, cancel-anchor vs interrupt, completed tools, and
// prose safety. Production poller logic is unchanged — only the cancel-anchor
// spelling compatibility in dialoghint is new. Synthetic panes only.
func TestAXScreenReaderPollerPins(t *testing.T) {
	// Flat AX permission menu: full-word Escape, no abbreviated Esc line.
	axPermission := `Permission Required: Create file
hello.txt

  1. Yes
  2. Yes, and don't ask again for this session
  3. No

Enter selection [1-3], or Escape to cancel:`

	// Flat AX question menu (AskUserQuestion-shaped).
	axQuestion := `Which approach should we take?

  1. Keep the poller mechanical
  2. Parse the TUI
  3. Other
  4. Chat about this

Enter selection [1-4], or Escape to cancel:`

	// Abbreviated variant — ordinary TUI spelling still classifies.
	axPermissionEsc := `Permission Required: Create file
hello.txt

  1. Yes
  2. Yes, and don't ask again for this session
  3. No

Enter selection [1-3], or Esc to cancel:`

	// Running silent tool: only the elapsed timer changes between captures.
	running12 := "Running… (12s)\n$"
	running13 := "Running… (13s)\n$"

	// Interrupt chrome is not the dialog cancel anchor.
	interruptPane := "Running tool…\n esc to interrupt\n$"

	// Completed tool output: no live menu.
	completedPane := "Wrote hello.txt (42 bytes)\n\n✓ Done\n$"

	// Agent prose discussing the screen-reader prompt — no structural menu.
	prosePane := "Screen-reader menus end with Escape to cancel after Enter selection.\n" +
		"dialoghint must not treat this sentence as a live dialog.\n$"

	quietRunner := func(pane string, sessions ...string) tmuxsession.Runner {
		if len(sessions) == 0 {
			sessions = []string{"cl1"}
		}
		listOut := strings.Join(sessions, "\n")
		return func(ctx context.Context, stdin string, args ...string) (string, error) {
			sub := ""
			if len(args) >= 3 {
				sub = args[2]
			}
			switch sub {
			case "list-sessions":
				return listOut, nil
			case "has-session":
				return "", nil
			case "capture-pane":
				return pane, nil
			}
			return "", nil
		}
	}
	// quietApp builds a Claude node with a static pane older than paneQuietAfter.
	// lastChg is 10s ago: past quiet (8s) but inside owedStallAfter (45s), so only
	// the structural matcher can raise attention — not the mechanical owing stall.
	quietApp := func(t *testing.T, pane string, transcriptLines ...string) *app {
		t.Helper()
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		if len(transcriptLines) > 0 {
			appendLines(t, path, transcriptLines...)
		} else {
			// Empty file so the tailer exists but reports nothing pending/owing.
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		n := tmuxFallbackNode("cl1", path)
		a := &app{
			byID:      map[string]*Node{"cl1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			turnDone:  map[string]bool{},
			prevCap:   map[string]string{"cl1": pane},
			lastChg:   map[string]time.Time{"cl1": time.Now().Add(-10 * time.Second)},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			server:    tmuxsession.NewServerWithRunner("testsock", quietRunner(pane)),
		}
		a.initMaps()
		return a
	}

	// (A) Static AX permission pane older than paneQuietAfter raises dialog
	// even when Claude has not flushed the tool_use record (late flush).
	t.Run("static_ax_permission_dialog_without_tool_use", func(t *testing.T) {
		a := quietApp(t, axPermission,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"create hello.txt"}}`)
		a.poll()
		if got := a.live["cl1"]; got != "quiet" {
			t.Fatalf("live = %q, want quiet", got)
		}
		if got := a.attn["cl1"]; got != "dialog" {
			t.Errorf("attention = %q, want dialog from AX permission menu", got)
		}
	})

	// Abbreviated footer still classifies on the same path.
	t.Run("static_ax_permission_abbreviated_esc", func(t *testing.T) {
		a := quietApp(t, axPermissionEsc,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"create hello.txt"}}`)
		a.poll()
		if got := a.attn["cl1"]; got != "dialog" {
			t.Errorf("attention = %q, want dialog from abbreviated Esc menu", got)
		}
	})

	// (B) Static AX question pane with only the human turn raises hard attention
	// through the structural fallback (no unresolved AskUserQuestion yet).
	t.Run("static_ax_question_dialog_human_turn_only", func(t *testing.T) {
		a := quietApp(t, axQuestion,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"which approach?"}}`)
		a.poll()
		if got := a.live["cl1"]; got != "quiet" {
			t.Fatalf("live = %q, want quiet", got)
		}
		if got := a.attn["cl1"]; got != "dialog" {
			t.Errorf("attention = %q, want dialog from AX question menu", got)
		}
	})

	// (C) Silent tool whose pane only ticks Running… (12s) → (13s) stays active
	// with no attention. Mechanical pane-change only — no text special-casing.
	t.Run("running_elapsed_time_stays_active_no_attention", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"write it"}}`,
			`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Write","input":{}}]}}`)
		n := tmuxFallbackNode("cl1", path)
		a := &app{
			byID:      map[string]*Node{"cl1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			prevCap:   map[string]string{"cl1": running12},
			lastChg:   map[string]time.Time{},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			anim:      map[string]*animState{},
			server:    tmuxsession.NewServerWithRunner("testsock", quietRunner(running13)),
		}
		a.initMaps()
		a.poll()
		if got := a.live["cl1"]; got != "active" {
			t.Fatalf("live = %q, want active (elapsed timer is pane change)", got)
		}
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention = %q, want none while tool runs with no dialog", got)
		}
	})

	// (D) "esc to interrupt" is not the dialog cancel anchor and must not
	// raise dialog attention on a quiet running pane without menu structure.
	t.Run("esc_to_interrupt_not_dialog", func(t *testing.T) {
		a := quietApp(t, interruptPane,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"run"}}`,
			`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`)
		a.poll()
		// Unresolved Bash → structured approval, not dialog-from-interrupt text.
		if got := a.attn["cl1"]; got != "approval" {
			t.Errorf("attention = %q, want approval from unresolved tool (not interrupt text)", got)
		}
		// Explicit: interrupt chrome alone does not classify as dialog.
		if dialoghint.ClassifyVisible(interruptPane) {
			t.Error("esc to interrupt pane must not ClassifyVisible as dialog")
		}
		if dialoghint.HasCancelAnchor(interruptPane) {
			t.Error("esc to interrupt must not satisfy HasCancelAnchor")
		}
		if !dialoghint.HasInterruptAnchor(interruptPane) {
			t.Error("esc to interrupt must satisfy the suppression-only working anchor")
		}
	})

	// (E) Completed tool with resolved transcript and stable pane → quiet,
	// finished, no dialog attention.
	t.Run("completed_tool_quiet_no_dialog", func(t *testing.T) {
		stamp := time.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339Nano)
		userStamp := time.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339Nano)
		a := quietApp(t, completedPane,
			`{"type":"user","timestamp":"`+userStamp+`","message":{"role":"user","content":"write hello"}}`,
			`{"type":"assistant","timestamp":"`+stamp+`","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Write","input":{}}]}}`,
			`{"type":"user","timestamp":"`+stamp+`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}}`,
			`{"type":"assistant","timestamp":"`+stamp+`","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
		a.poll()
		if got := a.live["cl1"]; got != "quiet" {
			t.Fatalf("live = %q, want quiet", got)
		}
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention = %q, want none after completed tool", got)
		}
		if !a.turnDone["cl1"] {
			t.Error("turnDone = false, want true for quiet delivered completed tool")
		}
	})

	// (F) Prose discussing Escape to cancel / screen-reader menus must not
	// raise attention without mechanical+structural preconditions.
	t.Run("prose_escape_to_cancel_no_attention", func(t *testing.T) {
		// Finished assistant turn: agent does not owe output; prose alone is
		// insufficient even past the longest stall.
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"hi"}}`,
			`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"text","text":"Escape to cancel is the AX footer; dialoghint is corroboration only"}]}}`)
		n := tmuxFallbackNode("cl1", path)
		a := &app{
			byID:      map[string]*Node{"cl1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			prevCap:   map[string]string{"cl1": prosePane},
			lastChg:   map[string]time.Time{"cl1": time.Now().Add(-4 * owedStallAfter)},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			server:    tmuxsession.NewServerWithRunner("testsock", quietRunner(prosePane)),
		}
		a.initMaps()
		a.poll()
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention = %q, want none: prose is not a dialog", got)
		}
	})
}

// TestAXQuietAttentionSafety pins P1 (fixes-2.md): under --ax-screen-reader,
// quiet unresolved tools and Owing() stalls are not hard/neutral attention
// without visible-dialog corroboration. Non-AX and missing-transcript paths
// stay as before. Liveness remains mechanical.
func TestAXQuietAttentionSafety(t *testing.T) {
	plainPane := "I have the evidence I need. Writing the updated review.\n$"
	// Recognized permission menu (same structural shape as dialoghint fixtures).
	permDialog := `Permission Required: Create file
hello.txt

  1. Yes
  2. Yes, and don't ask again for this session
  3. No

Enter selection [1-3], or Escape to cancel:`

	quietRunner := func(pane string) tmuxsession.Runner {
		return func(ctx context.Context, stdin string, args ...string) (string, error) {
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
	}
	// quietNode is past paneQuietAfter with a static capture. ax selects the
	// owned Claude renderer metadata that production launch sets.
	quietNode := func(t *testing.T, pane string, ax bool, transcriptLines ...string) (*app, time.Time) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		if len(transcriptLines) > 0 {
			appendLines(t, path, transcriptLines...)
		} else {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		n := &Node{ID: "cl1", Agent: "claude", Transcript: path, AXScreenReader: ax}
		quietSince := time.Now().Add(-10 * time.Second)
		a := &app{
			byID:      map[string]*Node{"cl1": n},
			nodes:     []*Node{n},
			live:      map[string]string{},
			attn:      map[string]string{},
			turnDone:  map[string]bool{},
			prevCap:   map[string]string{"cl1": pane},
			lastChg:   map[string]time.Time{"cl1": quietSince},
			tailers:   map[string]*transcript.Tailer{},
			chatMark:  map[string]chatMark{},
			staleChat: map[string]bool{},
			server:    tmuxsession.NewServerWithRunner("testsock", quietRunner(pane)),
		}
		a.initMaps()
		return a, quietSince
	}
	unresolvedBash := []string{
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"run tests"}}`,
		`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`,
	}
	// Human turn only — agent owes the next output (Owing), no tool_use yet.
	owingUser := []string{
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"write the review"}}`,
	}

	// 1. AX + quiet + unresolved Bash + no recognized dialog → no approval.
	t.Run("ax_unresolved_bash_no_dialog_no_approval", func(t *testing.T) {
		a, quietSince := quietNode(t, plainPane, true, unresolvedBash...)
		a.poll()
		if got := a.attn["cl1"]; got == "approval" || got == "question" {
			t.Errorf("attention = %q, want no hard attention without visible dialog under AX", got)
		}
		if got := a.live["cl1"]; got != "quiet" {
			t.Errorf("live = %q, want quiet (attention must not feed liveness)", got)
		}
		if !a.lastChg["cl1"].Equal(quietSince) {
			t.Errorf("lastChg moved by attention path: got %v want %v", a.lastChg["cl1"], quietSince)
		}
	})

	// 2. AX + quiet + unresolved Bash + recognized permission dialog → approval.
	t.Run("ax_unresolved_bash_with_dialog_approval", func(t *testing.T) {
		a, _ := quietNode(t, permDialog, true, unresolvedBash...)
		a.poll()
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention = %q, want none: Claude never raises from pane/transcript fallback", got)
		}
		if got := a.live["cl1"]; got != "quiet" {
			t.Errorf("live = %q, want quiet", got)
		}
	})

	// 3. AX + quiet + Owing past owedStallAfter but short of owedStallAX + no
	//    matcher → still no inspect. AX sits one rung slower than the legacy
	//    renderer; it does raise the neutral inspect eventually, which
	//    TestQuietAttentionFallbackAXOwingFloor pins from the other side.
	t.Run("ax_owing_past_legacy_stall_not_yet_inspect", func(t *testing.T) {
		a, _ := quietNode(t, plainPane, true, owingUser...)
		// Stretch quietSince past owedStallAfter (quietNode defaults to 10s).
		quiet := 2 * owedStallAfter
		if quiet >= owedStallAX {
			t.Fatalf("test is vacuous: %v is already past owedStallAX (%v)", quiet, owedStallAX)
		}
		a.lastChg["cl1"] = time.Now().Add(-quiet)
		a.poll()
		if got := a.attn["cl1"]; got == "inspect" {
			t.Errorf("attention = %q, AX must not reach inspect on the legacy 45s rung", got)
		}
		if got := a.live["cl1"]; got != "quiet" {
			t.Errorf("live = %q, want quiet", got)
		}
	})

	// 4. Non-AX + quiet + unresolved call keeps hard attention (legacy semantics).
	t.Run("non_ax_unresolved_bash_approval", func(t *testing.T) {
		a, _ := quietNode(t, plainPane, false, unresolvedBash...)
		a.poll()
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention = %q, want none: Claude never raises from unresolved-call + quiet", got)
		}
	})

	// 5. Missing/stale/unparseable transcript still yields neutral inspect on AX.
	//    (Feature narrowed, not deleted — same seams as TestPollInspectFallbacks.)
	t.Run("ax_missing_transcript_still_inspect", func(t *testing.T) {
		n := &Node{ID: "cl1", Agent: "claude", Transcript: "", AXScreenReader: true}
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
		a.initMaps()
		a.poll()
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention = %q, want none: Claude never inspects from a missing transcript", got)
		}
	})
	t.Run("ax_stale_transcript_still_inspect", func(t *testing.T) {
		a, _ := quietNode(t, plainPane, true,
			`{"type":"assistant","timestamp":"t","message":{"role":"assistant","content":"ok"}}`)
		// Install the tailer first: a fresh tailerFor clears staleChat (relink
		// contract). Then re-flag stale so the quiet inspect path sees it.
		_ = a.tailerFor(a.byID["cl1"])
		a.staleChat["cl1"] = true
		a.live["cl1"] = "quiet"
		a.poll()
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention = %q, want none: Claude never inspects from a stale transcript", got)
		}
	})
	t.Run("ax_unparseable_transcript_still_inspect", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"hi"}}`,
			`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":"hello"}}`)
		n := &Node{ID: "cl1", Agent: "claude", Transcript: path, AXScreenReader: true}
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
		a.initMaps()
		_ = a.tailerFor(n)
		junk := make([]string, 10)
		for i := range junk {
			junk[i] = "not-json-line"
		}
		appendLines(t, path, junk...)
		a.live["cl1"] = "quiet"
		a.poll()
		tl := a.tailers["cl1"]
		if tl == nil || !tl.Unparseable() {
			t.Fatalf("setup: tailer unparseable = %v", tl != nil && tl.Unparseable())
		}
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention = %q, want none: Claude never inspects from an unparseable transcript", got)
		}
	})

	// 6. New attention branches do not rewrite live timestamps (lastChg).
	t.Run("ax_attention_paths_leave_lastchg", func(t *testing.T) {
		a, quietSince := quietNode(t, plainPane, true, unresolvedBash...)
		a.poll()
		if !a.lastChg["cl1"].Equal(quietSince) {
			t.Errorf("lastChg rewritten: got %v want %v", a.lastChg["cl1"], quietSince)
		}
		if got := a.live["cl1"]; got != "quiet" {
			t.Errorf("live = %q, want quiet", got)
		}
	})
}

// workspaceTrustPane is independently authored to exercise the lettered
// y/n grammar before a transcript exists. No live CLI output is included.
const workspaceTrustPane = `Synthetic workspace menu
Permission Required: Accessing workspace:
/fixture/workspace
Fixture workspace access request.
Fixture details deliberately span another line.
Fixture help
y. Yes, I trust this folder
n. No, exit
Enter y/n:
Enter to confirm · Esc to cancel`

// launchedTrustNode is a just-launched Claude node sitting on the workspace-
// trust dialog: no transcript yet (discovery pending), pane static past
// paneQuietAfter. Shape follows axNodeWithTranscript.
func launchedTrustNode(t *testing.T, pane string, quiet time.Duration) *app {
	t.Helper()
	n := &Node{ID: "cl1", Agent: "claude", Transcript: "", AXScreenReader: true}
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

// A launched Claude node with no transcript on a lettered workspace-trust
// dialog must classify as dialog (keypad), not the inspect that the
// noEvidence branch would otherwise raise. No poller change: ClassifyVisible
// is already consulted first.
func TestLaunchedClaudeTrustDialogNoTranscript(t *testing.T) {
	a := launchedTrustNode(t, workspaceTrustPane, paneQuietAfter+time.Second)
	a.poll()
	if got := a.attn["cl1"]; got != "" {
		t.Errorf("attention = %q, want none: a Claude trust dialog is a launch error, not poll inspect", got)
	}
	if got := a.live["cl1"]; got != "quiet" {
		t.Errorf("live = %q, want quiet", got)
	}
}

// Liveness stays mechanical: the lettered matcher never turns a static pane
// active, and a changing pane stays active with no attention raised.
func TestLetteredTrustDialogDoesNotFeedLiveness(t *testing.T) {
	t.Run("static pane stays quiet", func(t *testing.T) {
		a := launchedTrustNode(t, workspaceTrustPane, paneQuietAfter+time.Second)
		a.poll()
		if got := a.live["cl1"]; got != "quiet" {
			t.Errorf("live = %q, want quiet — matcher must not feed liveness", got)
		}
		if got := a.attn["cl1"]; got == "dialog" && a.live["cl1"] == "active" {
			t.Error("matcher turned the pane active")
		}
	})
	t.Run("changing pane stays active with no attention", func(t *testing.T) {
		a := launchedTrustNode(t, workspaceTrustPane, paneQuietAfter+time.Second)
		a.prevCap["cl1"] = "Synthetic workspace menu\nstarting…\n"
		a.poll()
		if got := a.live["cl1"]; got != "active" {
			t.Errorf("live = %q, want active on a changing pane", got)
		}
		if got := a.attn["cl1"]; got != "" {
			t.Errorf("attention = %q, want none while the pane is still changing", got)
		}
	})
}

// handlePeek / notePeekDialog share quietAttentionFallback, so a one-shot
// peek on the same no-transcript trust dialog must classify identically.
func TestHandlePeekLetteredTrustDialog(t *testing.T) {
	a := launchedTrustNode(t, workspaceTrustPane, paneQuietAfter+time.Second)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/cl1/peek", nil)
	r.SetPathValue("id", "cl1")
	a.handlePeek(rec, r)
	if rec.Code != 200 {
		t.Fatalf("peek = %d", rec.Code)
	}
	if got := a.attn["cl1"]; got != "" {
		t.Errorf("peek attention = %q, want none: Claude peek does not raise fallback attention", got)
	}
	if got := a.live["cl1"]; got != "" {
		t.Errorf("peek must not write liveness: live = %q", got)
	}
}

// P2 — /clear must not rebind a retired Claude transcript (ux-fixes-2.md).
// (a) tombstone, (b) content-time not mtime, (c) genuine new session, (d) cur
// health ignores metadata-only touches.
func TestPollFreshClearDoesNotRaiseInspect(t *testing.T) {
	f := &fakeTmux{
		list:    []string{"c1"},
		alive:   map[string]bool{"c1": true},
		capture: "fresh prompt",
	}
	a := newTestApp(t, f)
	n := &Node{ID: "c1", Agent: "claude", CreatedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}
	a.nodes = []*Node{n}
	a.byID = map[string]*Node{n.ID: n}
	a.prevCap[n.ID] = f.capture
	a.lastChg[n.ID] = time.Now().Add(-time.Minute)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	for _, ev := range []sessionlog.Event{
		sessionlog.NewMeta(n.ID, n.Agent, "", "", a.home),
		{T: "user", Text: "before clear"},
		{T: "assistant", Text: "old answer"},
		sessionlog.NewClearSource(""),
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	a.poll()
	if got := a.live[n.ID]; got != "quiet" {
		t.Fatalf("live = %q, want quiet", got)
	}
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("post-clear attention = %q, want none", got)
	}

	// Once the fresh segment contains a turn, an actually missing transcript
	// regains the ordinary neutral inspect fallback.
	if err := w.Append(sessionlog.Event{T: "user", Text: "after clear"}); err != nil {
		t.Fatal(err)
	}
	a.poll()
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("post-prompt attention = %q, want none: Claude never inspects from a missing transcript", got)
	}
}

func TestMaybeRelinkTranscriptP2ClearStaysCleared(t *testing.T) {
	claudeUser := func(text, ts string) string {
		return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":%q}}`, ts, text)
	}
	// (a) a tombstoned path is never re-bound even when its mtime and its
	// newest content record are both newer than the phase start.
	t.Run("tombstoned_path_never_rebound", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"c1": true}}
		a := newTestApp(t, f)
		proj := filepath.Join(a.home, ".claude", "projects", "-w-proj")
		if err := os.MkdirAll(proj, 0o755); err != nil {
			t.Fatal(err)
		}
		oldPath := filepath.Join(proj, "old-session.jsonl")
		// Content newer than the phase start — without a tombstone this would
		// look like a live session that carried the phase.
		now := time.Now().UTC()
		appendLines(t, oldPath, claudeUser("pre-clear turn", now.Format(time.RFC3339Nano)))
		// Phase started 30s ago; content is "now".
		n := &Node{ID: "c1", Agent: "claude", Dir: "/w/proj", Transcript: oldPath, SessionID: "old-session"}
		a.nodes = append(a.nodes, n)
		a.byID["c1"] = n
		a.retireTranscript(n)
		if n.Transcript != "" || n.SessionID != "" {
			t.Fatalf("retire left link: transcript=%q session=%q", n.Transcript, n.SessionID)
		}
		// Pane cmdline still advertises the launch (dead) session; content and
		// mtime are both after the phase watermark.
		a.paneSession = func(pid string) string { return "old-session" }
		a.activeSince["c1"] = now.Add(-30 * time.Second)
		a.maybeRelinkTranscript(n)
		if n.Transcript == oldPath || n.SessionID == "old-session" {
			t.Fatalf("tombstoned transcript re-bound: transcript=%q session=%q", n.Transcript, n.SessionID)
		}
	})

	// (b) newest content predates the phase start, mtime is newer — the
	// exact bridge-session shape from the report. Refuse.
	t.Run("content_time_not_mtime_refuses_bridge_session", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"c1": true}}
		a := newTestApp(t, f)
		proj := filepath.Join(a.home, ".claude", "projects", "-w-proj")
		if err := os.MkdirAll(proj, 0o755); err != nil {
			t.Fatal(err)
		}
		deadPath := filepath.Join(proj, "dead-session.jsonl")
		// Content from an hour ago; a trailing metadata record bumps mtime.
		oldTS := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339Nano)
		appendLines(t, deadPath,
			claudeUser("old turn", oldTS),
			`{"type":"bridge-session","timestamp":"2026-08-10T08:04:55.000Z"}`)
		// Touch mtime to "now" so a pure-mtime gate would accept it.
		now := time.Now()
		if err := os.Chtimes(deadPath, now, now); err != nil {
			t.Fatal(err)
		}
		n := &Node{ID: "c1", Agent: "claude", Dir: "/w/proj"}
		a.nodes = append(a.nodes, n)
		a.byID["c1"] = n
		a.paneSession = func(pid string) string { return "dead-session" }
		a.activeSince["c1"] = time.Now().Add(-30 * time.Second)
		a.maybeRelinkTranscript(n)
		if n.Transcript == deadPath || n.SessionID == "dead-session" {
			t.Fatalf("bridge-session mtime bump re-bound dead file: transcript=%q session=%q",
				n.Transcript, n.SessionID)
		}
	})

	// (c) a genuine new session file is not guessed from the directory.
	t.Run("genuine_new_session_not_guessed", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"c1": true}}
		a := newTestApp(t, f)
		proj := filepath.Join(a.home, ".claude", "projects", "-w-proj")
		if err := os.MkdirAll(proj, 0o755); err != nil {
			t.Fatal(err)
		}
		oldPath := filepath.Join(proj, "old-session.jsonl")
		newPath := filepath.Join(proj, "new-session.jsonl")
		oldTS := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
		newTS := time.Now().UTC().Format(time.RFC3339Nano)
		appendLines(t, oldPath, claudeUser("pre-clear", oldTS))
		appendLines(t, newPath, claudeUser("post-clear prompt", newTS))
		past := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(oldPath, past, past); err != nil {
			t.Fatal(err)
		}
		n := &Node{ID: "c1", Agent: "claude", Dir: "/w/proj", Transcript: oldPath, SessionID: "old-session"}
		a.nodes = append(a.nodes, n)
		a.byID["c1"] = n
		a.activeSince["c1"] = time.Now().Add(-30 * time.Second)
		a.maybeRelinkTranscript(n)
		if n.Transcript == newPath || n.SessionID == "new-session" {
			t.Fatalf("newest-file guess rebound %q / %q", n.Transcript, n.SessionID)
		}
	})

	// (d) the cur health check no longer treats a metadata-only touch as
	// "carried the phase" — so a linked dead file with only a late mtime bump
	// is not considered healthy and relink can proceed to a real new file.
	t.Run("cur_health_ignores_metadata_only_touch", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"c1": true}}
		a := newTestApp(t, f)
		proj := filepath.Join(a.home, ".claude", "projects", "-w-proj")
		if err := os.MkdirAll(proj, 0o755); err != nil {
			t.Fatal(err)
		}
		linked := filepath.Join(proj, "linked.jsonl")
		newer := filepath.Join(proj, "fresh-session.jsonl")
		oldTS := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
		newTS := time.Now().UTC().Format(time.RFC3339Nano)
		appendLines(t, linked,
			claudeUser("old content", oldTS),
			`{"type":"bridge-session","timestamp":"2026-08-10T08:04:55.000Z"}`)
		appendLines(t, newer, claudeUser("real new turn", newTS))
		// Bump linked mtime so the old mtime-based health check would keep it.
		now := time.Now()
		if err := os.Chtimes(linked, now, now); err != nil {
			t.Fatal(err)
		}
		n := &Node{ID: "c1", Agent: "claude", Dir: "/w/proj", Transcript: linked, SessionID: "linked"}
		a.nodes = append(a.nodes, n)
		a.byID["c1"] = n
		a.activeSince["c1"] = time.Now().Add(-30 * time.Second)
		// The unanswered prompt is the staleness evidence (D1/D2).
		a.noteDelivery("c1", time.Now().Add(-time.Minute))
		a.maybeRelinkTranscript(n)
		if n.Transcript == linked {
			t.Fatalf("metadata-only mtime touch kept dead link healthy; transcript still %q", linked)
		}
		if n.Transcript == newer {
			t.Fatalf("newest-file guess rebound %q", n.Transcript)
		}
	})
}
