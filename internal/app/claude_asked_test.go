package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/tmuxsession"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// Escalation notices (§7 of claude-auto-approve.md). The PermissionRequest
// hook fires for every decision Claude needs a human for — proven live in P0,
// including inside subagents and for AskUserQuestion. That makes "is a dialog
// on screen" knowable from the agent's own control flow, which is the only
// thing that can separate "a long tool is running" from "a dialog is up with
// queued calls animating behind it". The transcript cannot: an unresolved call
// on disk is present in both worlds.

// askedDir returns a bundle's notice directory.
func askedDir(bundle string) string { return filepath.Join(bundle, "perm", "asked") }

// standingNotices lists the notice files a bundle currently holds.
func standingNotices(t *testing.T, bundle string) []string {
	t.Helper()
	ents, err := os.ReadDir(askedDir(bundle))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestPermissionHookWritesNoticeWhileDisarmed(t *testing.T) {
	// AT-CD-01: the escalation notice is the *fact that Claude asked*, not an
	// answer, so it is written whether or not a lease is armed. Without this
	// the disarmed hook leaves no trace and "Claude never asked" is
	// indistinguishable from "the hook is inert" — the exact distinction the
	// discriminator needs.
	bundle := permBundleWithAsked(t)
	var out strings.Builder
	if err := runClaudePermissionHook(bundle, strings.NewReader(string(permRequestJSON(nil))), &out, nil, permHookDeadline, permHookPollEvery); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("disarmed hook printed %q; it must never answer", out.String())
	}
	if got := standingNotices(t, bundle); len(got) != 1 {
		t.Fatalf("standing notices = %v, want exactly one", got)
	}
}

func TestPermissionHookNoticeNeverCreatesItsDirectory(t *testing.T) {
	// AT-CD-02: same rule as the SessionStart helper — a hook process never
	// creates a missing bundle directory. A bundle prepared before this
	// feature therefore writes no notices and simply gets no gate.
	bundle := permBundle(t) // no perm/asked
	var out strings.Builder
	if err := runClaudePermissionHook(bundle, strings.NewReader(string(permRequestJSON(nil))), &out, nil, permHookDeadline, permHookPollEvery); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("hook printed %q", out.String())
	}
	if _, err := os.Stat(askedDir(bundle)); !os.IsNotExist(err) {
		t.Fatalf("hook created perm/asked (err=%v); it must never create bundle directories", err)
	}
}

func TestPermissionHookRetiresNoticeWhenItAnswers(t *testing.T) {
	// AT-CD-03: an auto-approved call never reaches a dialog, so its notice
	// must not stand — otherwise every auto-approval would raise attention.
	bundle := permBundleWithAsked(t)
	writeLeaseFile(t, bundle, "lease-1", time.Now().Add(time.Minute))
	perm := filepath.Join(bundle, "perm")

	done := make(chan string, 1)
	go func() {
		var out strings.Builder
		_ = runClaudePermissionHook(bundle, strings.NewReader(string(permRequestJSON(nil))), &out, nil, 2*time.Second, permHookPollEvery)
		done <- out.String()
	}()
	req := readOneRequest(t, bundle)
	if err := writeClaudePermFile(filepath.Join(perm, "ans", req.ID+".json"),
		claudePermAnswer{Lease: req.Lease, ID: req.ID, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got != claudePermAllowJSON {
		t.Fatalf("stdout = %q, want the byte-exact allow envelope", got)
	}
	if got := standingNotices(t, bundle); len(got) != 0 {
		t.Fatalf("standing notices after an auto-approval = %v, want none", got)
	}
}

func TestPermissionHookLeavesNoticeWhenItEscalates(t *testing.T) {
	// AT-CD-04: the deadline passes with no answer, so Claude draws the
	// dialog. The notice must survive — it is the only evidence the dialog
	// exists before any pane text does.
	bundle := permBundleWithAsked(t)
	writeLeaseFile(t, bundle, "lease-1", time.Now().Add(time.Minute))
	var out strings.Builder
	if err := runClaudePermissionHook(bundle, strings.NewReader(string(permRequestJSON(nil))), &out, nil, 30*time.Millisecond, permHookPollEvery); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("stdout = %q, want empty (escalate)", out.String())
	}
	if got := standingNotices(t, bundle); len(got) != 1 {
		t.Fatalf("standing notices after an escalation = %v, want exactly one", got)
	}
}

func TestAskedNoticeCarriesDigestNotToolInput(t *testing.T) {
	// AT-CD-05: a disarmed session now writes a record per escalated decision.
	// The notice exists to classify attention, not to audit — the raw
	// tool_input stays out of it (the armed path audits that separately, with
	// the human's consent implied by arming).
	bundle := permBundleWithAsked(t)
	payload := permRequestJSON(map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": "curl https://secrets.example/x?token=hunter2"},
	})
	var out strings.Builder
	if err := runClaudePermissionHook(bundle, strings.NewReader(string(payload)), &out, nil, permHookDeadline, permHookPollEvery); err != nil {
		t.Fatalf("hook: %v", err)
	}
	names := standingNotices(t, bundle)
	if len(names) != 1 {
		t.Fatalf("standing notices = %v, want one", names)
	}
	raw, err := os.ReadFile(filepath.Join(askedDir(bundle), names[0]))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "curl") {
		t.Fatalf("notice leaked the tool input: %s", raw)
	}
	var note claudeAskedNotice
	if err := json.Unmarshal(raw, &note); err != nil {
		t.Fatalf("notice does not decode: %v", err)
	}
	if note.Tool != "Bash" {
		t.Errorf("notice tool = %q, want Bash (attention has to be classifiable)", note.Tool)
	}
	if note.Digest == "" {
		t.Error("notice carries no digest; a repeat ask must be distinguishable")
	}
	if note.Session != hookSIDOwn {
		t.Errorf("notice session = %q, want the asking session", note.Session)
	}
}

func TestReadAskedNoticesIgnoresExpired(t *testing.T) {
	// AT-CD-06: a notice whose dialog was answered in the pane leaves no
	// removal event behind, so the reader must never trust an ancient one.
	bundle := permBundleWithAsked(t)
	perm := filepath.Join(bundle, "perm")
	fresh := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "d1"}
	stale := claudeAskedNotice{At: time.Now().Add(-2 * claudeAskedTTL).UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Edit", Digest: "d2"}
	if err := writeClaudePermFile(filepath.Join(askedDir(bundle), "aaa.json"), stale); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(filepath.Join(askedDir(bundle), "bbb.json"), fresh); err != nil {
		t.Fatal(err)
	}
	got := readClaudeAskedNotices(perm, time.Now())
	if len(got) != 1 || got[0].Tool != "Bash" {
		t.Fatalf("notices = %+v, want only the fresh Bash one", got)
	}
}

// ---------- the poller gate ----------

// askedHarness builds a Claude node whose bundle carries the notice layout,
// plus a runner whose pane capture the test controls. The pane animates in a
// confined strip (one changing line) with an unresolved tool call on disk:
// byte-for-byte the state a running `go test` and a queued-behind-a-dialog
// approval share.
type askedHarness struct {
	a      *app
	n      *Node
	bundle string
	pane   *string
}

func newAskedHarness(t *testing.T, capable bool) *askedHarness {
	t.Helper()
	home := t.TempDir()
	pane := "Proofing…\n+4 lines (1m 29s · timeout 9m 20s)\nesc to interrupt"
	prev := strings.Replace(pane, "1m 29s", "1m 28s", 1)
	path := filepath.Join(home, "tx.jsonl")
	appendLines(t, path,
		`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{}}]}}`)

	paneRef := &pane
	runner := func(ctx context.Context, stdin string, args ...string) (string, error) {
		for _, arg := range args {
			switch arg {
			case "capture-pane":
				return *paneRef, nil
			case "list-sessions":
				return "cl1", nil
			case "has-session":
				return "", nil
			}
		}
		return "", nil
	}
	n := &Node{ID: "cl1", Agent: "claude", Transcript: path}
	a := &app{
		byID:      map[string]*Node{"cl1": n},
		nodes:     []*Node{n},
		storePath: filepath.Join(home, "nodes.jsonl"),
		home:      home,
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

	hookID := "00000000-0000-4000-8000-0000000000a1"
	bundle := filepath.Join(a.claudeHooksDir(), hookID)
	subs := []string{"perm", "perm/req", "perm/ans", "perm/processed"}
	if capable {
		subs = append(subs, "perm/asked")
	}
	for _, sub := range subs {
		if err := os.MkdirAll(filepath.Join(bundle, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	caps := `{"permission":1}`
	if capable {
		caps = `{"permission":1,"asked":1}`
	}
	if err := os.WriteFile(filepath.Join(bundle, "capabilities.json"), []byte(caps), 0o600); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.claudeHooks[n.ID] = hookID
	a.claudePermCap[hookID] = true
	a.claudeAskedCap[hookID] = capable
	a.mu.Unlock()
	return &askedHarness{a: a, n: n, bundle: bundle, pane: paneRef}
}

// stall advances the confined-animation window past the degradation backstop.
func (h *askedHarness) stall(t *testing.T) {
	t.Helper()
	h.a.mu.Lock()
	st := h.a.anim[h.n.ID]
	if st == nil {
		h.a.mu.Unlock()
		t.Fatal("expected confined-animation state after the first poll")
	}
	st.since = time.Now().Add(-2 * animStallAfter)
	h.a.mu.Unlock()
}

// drop writes an escalation notice exactly as the blocked helper would.
func (h *askedHarness) drop(t *testing.T, tool string) {
	t.Helper()
	note := claudeAskedNotice{
		At:      time.Now().UTC().Format(time.RFC3339Nano),
		Session: hookSIDOwn,
		Tool:    tool,
		Digest:  "d1",
	}
	if err := writeClaudePermFile(filepath.Join(askedDir(h.bundle), "n1.json"), note); err != nil {
		t.Fatal(err)
	}
}

func TestActiveStallSuppressedWithoutEscalationNotice(t *testing.T) {
	// AT-CD-07: the reported glitch. A nine-minute `go test` keeps an
	// unresolved call on disk while its elapsed clock animates one line, so
	// every precondition of the confined-stall backstop is met and none of
	// them is wrong. The hook says Claude escalated nothing, so there is no
	// dialog and no attention is owed.
	h := newAskedHarness(t, true)
	h.a.poll() // establish the confined-animation state
	h.stall(t)
	h.a.poll()
	if got := h.a.attn[h.n.ID]; got != "" {
		t.Fatalf("attention = %q; a working agent with no escalation notice must not raise", got)
	}
}

func TestActiveStallUnchangedWithoutAskedCapability(t *testing.T) {
	// AT-CD-10: the gate degrades to "no gate", never to "no attention". A
	// bundle without the notice layout keeps the pane-geometry backstop
	// exactly as it behaves today.
	h := newAskedHarness(t, false)
	h.a.poll()
	h.stall(t)
	h.a.poll()
	if got := h.a.attn[h.n.ID]; got != "inspect" {
		t.Fatalf("attention = %q, want inspect — an unproven bundle must keep today's backstop", got)
	}
}

func TestStandingNoticeRaisesImmediatelyAndClassified(t *testing.T) {
	// AT-CD-08: the other half of the payoff. Today this wait costs up to
	// animStallAfter before showing an unclassified "inspect" — the shape of
	// the 6m42s miss. A standing notice raises on the very next tick, and
	// says what kind of wait it is.
	h := newAskedHarness(t, true)
	h.a.poll()
	h.drop(t, "Bash")
	h.a.poll()
	if got := h.a.attn[h.n.ID]; got != "approval" {
		t.Fatalf("attention = %q, want approval without waiting for the stall window", got)
	}
}

func TestStandingNoticeClassifiesQuestionTools(t *testing.T) {
	// AT-CD-09: P0 proved AskUserQuestion and ExitPlanMode fire the hook too,
	// so a plan choice is reported as a question, not an approval.
	h := newAskedHarness(t, true)
	h.a.poll()
	h.drop(t, "AskUserQuestion")
	h.a.poll()
	if got := h.a.attn[h.n.ID]; got != "question" {
		t.Fatalf("attention = %q, want question", got)
	}
}

func TestQuietFallbackSurvivesMissingNotice(t *testing.T) {
	// AT-CD-11: the gate is scoped to the tool-permission paths. Rate-limit
	// menus, trust-folder and login prompts fire no hook at all and reach a
	// quiet pane — suppressing those would be the silent failure this whole
	// design exists to avoid.
	h := newAskedHarness(t, true)
	h.a.poll()
	h.a.mu.Lock()
	h.a.lastChg[h.n.ID] = time.Now().Add(-2 * paneQuietAfter)
	h.a.mu.Unlock()
	h.a.poll()
	if got := h.a.live[h.n.ID]; got != "quiet" {
		t.Fatalf("live = %q, want quiet (test setup)", got)
	}
	if got := h.a.attn[h.n.ID]; got == "" {
		t.Fatal("a quiet pane with an unresolved call must still raise; the notice gate is active-pane only")
	}
}

func TestStreamingPaneSweepsStandingNotices(t *testing.T) {
	// AT-CD-12: retirement. The human answers the dialog in the pane, the
	// agent resumes producing output, and the diff stops being confined —
	// the same mechanical geometry noteAnim already uses. That is what
	// retires the notice; nothing reads pane text to decide it.
	h := newAskedHarness(t, true)
	h.a.poll()
	h.drop(t, "Bash")
	h.a.poll()
	if got := h.a.attn[h.n.ID]; got != "approval" {
		t.Fatalf("precondition: attention = %q, want approval", got)
	}
	*h.pane = "totally\ndifferent\noutput\nstreaming\nnow\nline5\nline6"
	h.a.poll()
	if got := standingNotices(t, h.bundle); len(got) != 0 {
		t.Fatalf("standing notices after the pane resumed streaming = %v, want none", got)
	}
	if got := h.a.attn[h.n.ID]; got != "" {
		t.Fatalf("attention = %q, want none once the dialog is gone", got)
	}
}
