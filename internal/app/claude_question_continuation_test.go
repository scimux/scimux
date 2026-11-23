package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionworker"
)

// Synthetic panes. Invented question text; not a captured session.
const (
	contFirstPart = `Questions: Colour, Size
What colour?

  1. Teal
  2. Amber
  3. Other
  4. Chat about this

Escape to cancel`

	contSecondPart = `Questions: Colour (answered), Size
What size?

  1. Small
  2. Large
  3. Other
  4. Chat about this

Escape to cancel`

	contFreeText = `Type a colour in your own words.

Escape to cancel`

	contWorking = `Working on the palette.
esc to interrupt`

	contProse = `Notes mention esc to cancel before any menu.
1. first idea
2. second idea
esc to interrupt`
)

func contClaude(t *testing.T) (*fakeTmux, *app, *Node, string) {
	t.Helper()
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	n.AXScreenReader = true
	return f, a, n, filepath.Join(bundle, "perm")
}

func contPane(f *fakeTmux, capture, after string) {
	f.mu.Lock()
	f.capture, f.captureAfterEnter, f.captureErr = capture, after, false
	f.mu.Unlock()
}

func contAsk(t *testing.T, perm, id, tool, session string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(perm, claudeAskedDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	note := claudeAskedNotice{
		At: time.Now().UTC().Format(time.RFC3339Nano), Session: session, Tool: tool, Digest: "cont-" + id,
	}
	if err := writeClaudePermFile(claudeAskedPath(perm, id), note); err != nil {
		t.Fatal(err)
	}
}

func contAskAt(t *testing.T, perm, id, tool, session string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(perm, claudeAskedDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	note := claudeAskedNotice{At: at.UTC().Format(time.RFC3339Nano), Session: session, Tool: tool, Digest: "cont-" + id}
	if err := writeClaudePermFile(claudeAskedPath(perm, id), note); err != nil {
		t.Fatal(err)
	}
}

// contEpochMap reads the visible-dialog file as raw JSON. A missing file is
// the red failure for a multi-part question: the key cleared the epoch
// instead of continuing it.
func contEpochMap(t *testing.T, perm string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(claudeVisibleEpochPath(perm))
	if err != nil {
		t.Fatalf("visible.json missing; a multi-part question must keep a continued epoch: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["continued"] != true {
		t.Fatalf("visible.json epoch %v is not continued; a multi-part question must keep a continued epoch", raw["epoch"])
	}
	return raw
}

func contOptionNames(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["perm_options"].([]any)
	if !ok {
		t.Fatalf("perm_options = %#v, want part 2 labels", body["perm_options"])
	}
	names := make([]string, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]any)
		name, _ := m["name"].(string)
		names = append(names, name)
	}
	return names
}

func contNamesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

var contPartTwo = []string{"Small", "Large", "Other", "Chat about this"}

func contMintQuestion(t *testing.T, perm string) claudeVisibleEpoch {
	t.Helper()
	contAsk(t, perm, "ask-colour", "AskUserQuestion", hookSIDOwn)
	ep, err := mintClaudeVisibleEpochLabeled(perm, hookSIDOwn, "Colour", "")
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func TestClaudeQuestionKeyKeepsNextPartAnswerable(t *testing.T) {
	f, a, n, perm := contClaude(t)
	contPane(f, contFirstPart, contSecondPart)
	ep := contMintQuestion(t, perm)
	old := ep.Epoch

	rec := httptestPostKey(t, a, n.ID, "1", old)
	if rec.Code != 200 {
		t.Fatalf("key on the open part: %d %s", rec.Code, rec.Body.String())
	}
	raw := contEpochMap(t, perm)
	if raw["epoch"] == old {
		t.Fatal("continued epoch reused the answered id; the next part needs a fresh id")
	}
	if raw["hint_tool"] != "AskUserQuestion" || raw["session"] != hookSIDOwn {
		t.Fatalf("continued epoch = %#v, want the same hint_tool and session", raw)
	}
	a.poll()
	if got := a.attn[n.ID]; got != "question" {
		t.Fatalf("attn after the continued part = %q, want question", got)
	}
	body := chatBody(t, a, n.ID)
	if body["perm_dialog_id"] != raw["epoch"] {
		t.Fatalf("perm_dialog_id = %v, want continued epoch %v", body["perm_dialog_id"], raw["epoch"])
	}
	if got := contOptionNames(t, body); !contNamesEqual(got, contPartTwo) {
		t.Fatalf("perm_options = %v, want %v", got, contPartTwo)
	}
	before := len(sendKeysCalls(f))
	stale := httptestPostKey(t, a, n.ID, "1", old)
	if stale.Code != 409 {
		t.Fatalf("old epoch key = %d, want 409", stale.Code)
	}
	if got := len(sendKeysCalls(f)); got != before {
		t.Fatalf("send-keys count = %d, want %d after the stale epoch", got, before)
	}
	nextID, _ := raw["epoch"].(string)
	next := httptestPostKey(t, a, n.ID, "1", nextID)
	if next.Code != 200 {
		t.Fatalf("key on the continued epoch: %d %s", next.Code, next.Body.String())
	}
	again := contEpochMap(t, perm)
	if again["epoch"] == nextID {
		t.Fatal("a second part did not continue the epoch")
	}
}

func TestClaudeQuestionHarnessKeyKeepsNextPartAnswerable(t *testing.T) {
	f, a, n, perm := contClaude(t)
	contPane(f, contFirstPart, contSecondPart)
	ep := contMintQuestion(t, perm)
	h := testClaudeSessionHarness(a, n.ID)
	ctx := context.Background()
	prepared, err := h.PreparePermission(ctx, sessionworker.PermissionDecision{RequestID: ep.Epoch, Key: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.DeliverPermission(ctx, prepared.Token); err != nil {
		t.Fatal(err)
	}
	raw := contEpochMap(t, perm)
	h.poll()
	state := h.State(ctx)
	nextID, _ := raw["epoch"].(string)
	if state.Permission == nil || state.Permission.RequestID != nextID {
		t.Fatalf("harness permission = %#v, want continued epoch %s", state.Permission, nextID)
	}
	var names []string
	for _, opt := range state.Permission.Options {
		names = append(names, opt.Name)
	}
	if !contNamesEqual(names, contPartTwo) {
		t.Fatalf("harness options = %v, want %v", names, contPartTwo)
	}
}

func TestClaudeContinuedQuestionEndsWithItsAsk(t *testing.T) {
	f, a, n, perm := contClaude(t)
	contPane(f, contFirstPart, contSecondPart)
	ep := contMintQuestion(t, perm)
	rec := httptestPostKey(t, a, n.ID, "1", ep.Epoch)
	if rec.Code != 200 {
		t.Fatalf("key: %d %s", rec.Code, rec.Body.String())
	}
	contEpochMap(t, perm) // red: the key cleared the epoch instead of continuing it
	if err := os.Remove(claudeAskedPath(perm, "ask-colour")); err != nil {
		t.Fatal(err)
	}
	contPane(f, contWorking, contWorking)
	a.poll()
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("attn after the ask ended = %q, want none", got)
	}
	if _, err := os.Stat(claudeVisibleEpochPath(perm)); !os.IsNotExist(err) {
		t.Fatalf("visible.json after the ask ended: %v, want gone", err)
	}
	if id, ok := chatBody(t, a, n.ID)["perm_dialog_id"]; ok {
		t.Fatalf("perm_dialog_id = %v after the ask ended", id)
	}
}

func TestClaudeContinuedQuestionNeedsDialogOnScreen(t *testing.T) {
	f, a, n, perm := contClaude(t)
	contAsk(t, perm, "ask-size", "AskUserQuestion", hookSIDOwn)
	if err := writeClaudePermFile(claudeVisibleEpochPath(perm), map[string]any{
		"epoch": "cont-epoch-1", "session": hookSIDOwn, "hint_tool": "AskUserQuestion", "continued": true,
	}); err != nil {
		t.Fatal(err)
	}
	partTwo := func(t *testing.T) {
		t.Helper()
		a.poll()
		if got := a.attn[n.ID]; got != "question" {
			t.Fatalf("attn = %q, want question", got)
		}
		body := chatBody(t, a, n.ID)
		if got := contOptionNames(t, body); !contNamesEqual(got, contPartTwo) {
			t.Fatalf("options = %v, want %v", got, contPartTwo)
		}
	}
	quiet := func(t *testing.T) {
		t.Helper()
		a.poll()
		if got := a.attn[n.ID]; got != "" {
			t.Fatalf("continued epoch over a working pane raised attention %q; pane corroboration is required", got)
		}
		if id, ok := chatBody(t, a, n.ID)["perm_dialog_id"]; ok {
			t.Fatalf("continued epoch over a working pane set perm_dialog_id %v; pane corroboration is required", id)
		}
		if _, err := os.Stat(claudeVisibleEpochPath(perm)); err != nil {
			t.Fatalf("visible.json = %v; a continued epoch stays until its ask ends", err)
		}
	}
	t.Run("numbered", func(t *testing.T) {
		contPane(f, contSecondPart, "")
		partTwo(t)
	})
	t.Run("free text", func(t *testing.T) {
		contPane(f, contFreeText, "")
		a.poll()
		if got := a.attn[n.ID]; got != "question" {
			t.Fatalf("attn = %q, want question", got)
		}
		body := chatBody(t, a, n.ID)
		if body["perm_manual"] != true {
			t.Fatalf("perm_manual = %v, want true", body["perm_manual"])
		}
		if _, ok := body["perm_options"]; ok {
			t.Fatalf("free-text dialog published options %#v", body["perm_options"])
		}
	})
	t.Run("working", func(t *testing.T) {
		contPane(f, contWorking, "")
		quiet(t)
	})
	t.Run("prose", func(t *testing.T) {
		contPane(f, contProse, "")
		quiet(t)
	})
	t.Run("capture error", func(t *testing.T) {
		f.mu.Lock()
		f.captureErr = true
		f.mu.Unlock()
		if got := a.claudeVisibleDialog(n).DialogID; got != "" {
			t.Fatalf("continued epoch stayed visible as %q after capture failed; pane corroboration is required", got)
		}
		if _, err := os.Stat(claudeVisibleEpochPath(perm)); err != nil {
			t.Fatalf("visible.json = %v after a failed capture", err)
		}
	})
}

func TestClaudeOriginalEpochIgnoresPaneCorroboration(t *testing.T) {
	f, a, n, perm := contClaude(t)
	contPane(f, contWorking, "")
	ep := contMintQuestion(t, perm)
	body := chatBody(t, a, n.ID)
	if body["perm_dialog_id"] != ep.Epoch {
		t.Fatalf("perm_dialog_id = %v, want original epoch %s", body["perm_dialog_id"], ep.Epoch)
	}
	if body["perm_manual"] != true {
		t.Fatalf("perm_manual = %v, want true", body["perm_manual"])
	}
}

func TestClaudeKeyRetiresEpochWhenQuestionCannotContinue(t *testing.T) {
	if (&app{}).retireClaudeVisibleDialog(nil, "epoch") {
		t.Fatal("nil node retired an epoch")
	}
	bare := &app{}
	node := &Node{ID: "x", Agent: "claude"}
	if bare.retireClaudeVisibleDialog(node, "epoch-x") {
		t.Fatal("non-strict node retired an epoch")
	}
	if bare.retireClaudeVisibleDialog(node, "") {
		t.Fatal("empty epoch retired")
	}

	f, a, n, perm := contClaude(t)
	contPane(f, contFirstPart, "")
	contAsk(t, perm, "ask-live", "AskUserQuestion", hookSIDOwn)
	ep, err := mintClaudeVisibleEpochLabeled(perm, hookSIDOwn, "Colour", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.retireClaudeVisibleDialog(n, "not-the-epoch") {
		t.Fatal("mismatched epoch retired the current dialog")
	}
	if _, err := os.Stat(claudeVisibleEpochPath(perm)); err != nil {
		t.Fatalf("visible.json after a mismatched epoch: %v", err)
	}
	// No file: the epoch is already gone, so retirement has nothing to do.
	clearClaudeVisibleEpoch(perm)
	if a.retireClaudeVisibleDialog(n, ep.Epoch) {
		t.Fatal("missing epoch file retired")
	}

	cases := []struct {
		name, epoch, hint, askID, askTool string
	}{
		{"exit plan", "epoch-exitplan", "ExitPlanMode", "ask-plan", "ExitPlanMode"},
		{"bash only", "epoch-askbash", "AskUserQuestion", "ask-bash", "Bash"},
		{"edit", "epoch-edit", "Edit", "ask-edit", "Edit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, a, n, perm := contClaude(t)
			contPane(f, contFirstPart, "")
			contAsk(t, perm, tc.askID, tc.askTool, hookSIDOwn)
			if err := writeClaudePermFile(claudeVisibleEpochPath(perm), map[string]any{
				"epoch": tc.epoch, "session": hookSIDOwn, "hint_tool": tc.hint,
			}); err != nil {
				t.Fatal(err)
			}
			a.mu.Lock()
			a.attn[n.ID] = "question"
			a.mu.Unlock()
			rec := httptestPostKey(t, a, n.ID, "1", tc.epoch)
			if rec.Code != 200 {
				t.Fatalf("key: %d %s", rec.Code, rec.Body.String())
			}
			if _, err := os.Stat(claudeVisibleEpochPath(perm)); !os.IsNotExist(err) {
				t.Fatalf("visible.json = %v, want gone", err)
			}
			if got := a.attn[n.ID]; got != "" {
				t.Fatalf("attn = %q, want cleared", got)
			}
		})
	}
}

func TestClaudeQuestionKeyFailsClosedWhenSuccessorCannotBeWritten(t *testing.T) {
	f, a, n, perm := contClaude(t)
	contPane(f, contFirstPart, "")
	ep := contMintQuestion(t, perm)
	prev := claudeContinuedEpochID
	claudeContinuedEpochID = func() (string, error) { return "", errors.New("forced id error") }
	t.Cleanup(func() { claudeContinuedEpochID = prev })
	a.mu.Lock()
	a.attn[n.ID] = "question"
	a.mu.Unlock()
	rec := httptestPostKey(t, a, n.ID, "1", ep.Epoch)
	if rec.Code != 200 {
		t.Fatalf("key: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(claudeVisibleEpochPath(perm)); !os.IsNotExist(err) {
		t.Fatalf("visible.json = %v, want gone when the successor cannot be written", err)
	}
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("attn = %q, want cleared", got)
	}
}

func TestStandingClaudeAskFor(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	match := func(t *testing.T, perm, session string) {
		t.Helper()
		if !standingClaudeAskFor(perm, session, "AskUserQuestion", now) {
			t.Fatal("a standing AskUserQuestion notice must match")
		}
	}
	t.Run("same session", func(t *testing.T) {
		perm := filepath.Join(t.TempDir(), "perm")
		contAskAt(t, perm, "ask-1", "AskUserQuestion", hookSIDOwn, now)
		match(t, perm, hookSIDOwn)
	})
	t.Run("notice without session", func(t *testing.T) {
		perm := filepath.Join(t.TempDir(), "perm")
		contAskAt(t, perm, "ask-1", "AskUserQuestion", "", now)
		match(t, perm, hookSIDOwn)
	})
	t.Run("empty session argument", func(t *testing.T) {
		perm := filepath.Join(t.TempDir(), "perm")
		contAskAt(t, perm, "ask-1", "AskUserQuestion", hookSIDOwn, now)
		match(t, perm, "")
	})
	falseRow := func(t *testing.T, perm, session string) {
		t.Helper()
		if standingClaudeAskFor(perm, session, "AskUserQuestion", now) {
			t.Fatal("standingClaudeAskFor matched a notice it must ignore")
		}
	}
	t.Run("other tool", func(t *testing.T) {
		perm := filepath.Join(t.TempDir(), "perm")
		contAskAt(t, perm, "ask-1", "Bash", hookSIDOwn, now)
		falseRow(t, perm, hookSIDOwn)
	})
	t.Run("other session", func(t *testing.T) {
		perm := filepath.Join(t.TempDir(), "perm")
		contAskAt(t, perm, "ask-1", "AskUserQuestion", hookSIDSuccessor, now)
		falseRow(t, perm, hookSIDOwn)
	})
	t.Run("answered", func(t *testing.T) {
		perm := filepath.Join(t.TempDir(), "perm")
		contAskAt(t, perm, "ask-1", "AskUserQuestion", hookSIDOwn, now)
		markClaudeRequestAnswered(perm, "ask-1")
		falseRow(t, perm, hookSIDOwn)
	})
	t.Run("older than ttl", func(t *testing.T) {
		perm := filepath.Join(t.TempDir(), "perm")
		contAskAt(t, perm, "ask-1", "AskUserQuestion", hookSIDOwn, now.Add(-claudeAskedTTL-time.Minute))
		falseRow(t, perm, hookSIDOwn)
	})
	t.Run("missing asked dir", func(t *testing.T) {
		falseRow(t, filepath.Join(t.TempDir(), "perm"), hookSIDOwn)
	})
	t.Run("unsafe names", func(t *testing.T) {
		perm := filepath.Join(t.TempDir(), "perm")
		contAskAt(t, perm, "archive", "AskUserQuestion", hookSIDOwn, now)
		if err := os.WriteFile(filepath.Join(perm, claudeAskedDirName, "has..dot.json"), []byte(`{"tool":"AskUserQuestion","at":"2026-03-01T12:00:00Z","session":"`+hookSIDOwn+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		falseRow(t, perm, hookSIDOwn)
	})
}

func TestContinueClaudeVisibleEpoch(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 123, time.UTC)
	perm := filepath.Join(t.TempDir(), "perm")
	if err := os.MkdirAll(perm, 0o700); err != nil {
		t.Fatal(err)
	}
	cur := claudeVisibleEpoch{Epoch: "epoch-old", Session: hookSIDOwn, Title: "Colour", HintTool: "AskUserQuestion", At: "old"}
	if err := writeClaudePermFile(claudeVisibleEpochPath(perm), cur); err != nil {
		t.Fatal(err)
	}
	next, ok := continueClaudeVisibleEpoch(perm, cur, now)
	if !ok {
		t.Fatal("continueClaudeVisibleEpoch must write a successor epoch")
	}
	if next.Epoch == "" || next.Epoch == cur.Epoch || !next.Continued {
		t.Fatalf("successor = %#v, want a fresh continued id", next)
	}
	if next.Session != cur.Session || next.Title != cur.Title || next.HintTool != cur.HintTool {
		t.Fatalf("successor = %#v, want Session, Title and HintTool copied", next)
	}
	if next.At != now.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("At = %q, want %q", next.At, now.UTC().Format(time.RFC3339Nano))
	}
	want, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(claudeVisibleEpochPath(perm))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("visible.json = %s, want %s", got, want)
	}

	kept, err := os.ReadFile(claudeVisibleEpochPath(perm))
	if err != nil {
		t.Fatal(err)
	}
	prev := claudeContinuedEpochID
	claudeContinuedEpochID = func() (string, error) { return "", errors.New("forced id error") }
	t.Cleanup(func() { claudeContinuedEpochID = prev })
	if _, ok := continueClaudeVisibleEpoch(perm, cur, now); ok {
		t.Fatal("id error wrote a successor")
	}
	after, err := os.ReadFile(claudeVisibleEpochPath(perm))
	if err != nil || string(after) != string(kept) {
		t.Fatalf("visible.json changed on id error: %s", after)
	}
	claudeContinuedEpochID = prev

	if err := os.Mkdir(claudeVisibleEpochPath(perm)+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeVisibleEpochPath(perm)+".tmp", "block"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := continueClaudeVisibleEpoch(perm, next, now); ok {
		t.Fatal("write error reported a successor")
	}
	after, err = os.ReadFile(claudeVisibleEpochPath(perm))
	if err != nil || string(after) != string(kept) {
		t.Fatalf("visible.json changed on write error: %s", after)
	}
}

func TestClaudeContinuedDialogCorroborated(t *testing.T) {
	cases := []struct {
		name, pane string
		want       bool
	}{
		{"part 1", contFirstPart, true},
		{"part 2", contSecondPart, true},
		{"free text", contFreeText, true},
		{"working", contWorking, false},
		{"prose", contProse, false},
		{"empty", "", false},
		{"ax ask", axAskUserQuestionPane, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeContinuedDialogCorroborated(tc.pane)
			if got == tc.want {
				return
			}
			if tc.want {
				t.Fatalf("claudeContinuedDialogCorroborated(%s) = false, want true", tc.name)
			}
			t.Fatalf("claudeContinuedDialogCorroborated(%s) = true, want false", tc.name)
		})
	}
}

func TestClaudeContinuedDialogVisibleNeedsServer(t *testing.T) {
	n := &Node{ID: "n1", Agent: "claude"}
	if (&app{}).claudeContinuedDialogVisible(n) {
		t.Fatal("missing server corroborated a continued dialog")
	}
	f := &fakeTmux{capture: contSecondPart}
	a := newTestApp(t, f)
	a.byID[n.ID] = n
	if !a.claudeContinuedDialogVisible(n) {
		t.Fatal("a visible numbered dialog must corroborate a continued epoch")
	}
}

const (
	contUserLine = `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"pick a colour"}}`
	contMetaLine = `{"type":"user","timestamp":"2026-01-01T00:00:01Z","isMeta":true,"message":{"role":"user","content":"local note"}}`
)

// contGrowWithoutAgent appends a user prompt and a meta record, then polls
// the active→quiet edge. prog does not move. An ask, when requested, is
// stamped after the transcript times so the tick still sees it.
func contGrowWithoutAgent(t *testing.T, withAsk bool) (*app, *Node) {
	t.Helper()
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: contWorking}
	a := newTestApp(t, f)
	n, bundle := seedPermClaudeWithTranscript(t, a, "n1", hookSIDOwn, contUserLine)
	a.mu.Lock()
	a.prevCap[n.ID] = contWorking
	a.lastChg[n.ID] = time.Now().Add(-paneQuietAfter - time.Second)
	a.live[n.ID] = "quiet"
	a.mu.Unlock()
	a.poll()
	appendLines(t, n.Transcript, contUserLine, contMetaLine)
	if withAsk {
		contAsk(t, filepath.Join(bundle, "perm"), "ask-colour", "AskUserQuestion", hookSIDOwn)
	}
	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.poll()
	return a, n
}

func TestClaudeStandingAskSuspendsStaleJudgment(t *testing.T) {
	a, n := contGrowWithoutAgent(t, true)
	if a.staleChat[n.ID] {
		t.Fatal("a standing AskUserQuestion must suspend the stale-transcript judgment")
	}
	if reason, _ := chatBody(t, a, n.ID)["reason"].(string); reason == "claude_transcript_fault" {
		t.Fatal("a standing AskUserQuestion must not be reported as a transcript fault")
	}
}

func TestClaudeStaleJudgmentWithoutAskStillFaults(t *testing.T) {
	a, n := contGrowWithoutAgent(t, false)
	if !a.staleChat[n.ID] {
		t.Fatal("growth without an ask must mark the transcript stale")
	}
	reason, _ := chatBody(t, a, n.ID)["reason"].(string)
	if reason != "claude_transcript_fault" {
		t.Fatalf("reason = %q, want claude_transcript_fault", reason)
	}
}

func contForcedStaleReason(t *testing.T, hint, askTool string) string {
	t.Helper()
	f, a, n, perm := contClaude(t)
	contPane(f, contFirstPart, "")
	if hint != "" {
		contAsk(t, perm, "ask-1", askTool, hookSIDOwn)
		if err := writeClaudePermFile(claudeVisibleEpochPath(perm), map[string]any{
			"epoch": "epoch-wait", "session": hookSIDOwn, "hint_tool": hint,
		}); err != nil {
			t.Fatal(err)
		}
	}
	a.poll()
	a.mu.Lock()
	a.staleChat[n.ID] = true
	a.mu.Unlock()
	reason, _ := chatBody(t, a, n.ID)["reason"].(string)
	return reason
}

func TestClaudeWaitingReasonOutranksTranscriptFault(t *testing.T) {
	t.Run("question", func(t *testing.T) {
		if got := contForcedStaleReason(t, "AskUserQuestion", "AskUserQuestion"); got != "waiting_question" {
			t.Fatalf("question epoch reason = %q, want waiting_question; an asked dialog is not a transcript fault", got)
		}
	})
	t.Run("approval", func(t *testing.T) {
		if got := contForcedStaleReason(t, "Bash", "Bash"); got != "waiting_approval" {
			t.Fatalf("approval epoch reason = %q, want waiting_approval; an asked dialog is not a transcript fault", got)
		}
	})
	t.Run("no epoch", func(t *testing.T) {
		if got := contForcedStaleReason(t, "", ""); got != "claude_transcript_fault" {
			t.Fatalf("reason = %q, want claude_transcript_fault", got)
		}
	})
}

func TestNonClaudeReasonPrecedenceUnchanged(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := tmuxFallbackNode("pi1", "")
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	a.mu.Lock()
	a.live[n.ID] = "active"
	a.attn[n.ID] = "question"
	a.mu.Unlock()
	if reason, _ := chatBody(t, a, n.ID)["reason"].(string); reason != "pane_active" {
		t.Fatalf("pi reason = %q, want pane_active", reason)
	}
}
