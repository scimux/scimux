package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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
	// The exec path is part of the proof (AT-CD-16): the test binary is a real
	// executable, so a capable fixture records one.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	caps := `{"permission":1,"exec":` + strconv.Quote(self) + `}`
	if capable {
		caps = `{"permission":1,"asked":1,"exec":` + strconv.Quote(self) + `}`
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
	h.dropAt(t, tool, "n1.json", time.Now())
}

func (h *askedHarness) dropAt(t *testing.T, tool, name string, at time.Time) {
	t.Helper()
	note := claudeAskedNotice{
		At:      at.UTC().Format(time.RFC3339Nano),
		Session: hookSIDOwn,
		Tool:    tool,
		Digest:  "d1",
	}
	if err := writeClaudePermFile(filepath.Join(askedDir(h.bundle), name), note); err != nil {
		t.Fatal(err)
	}
}

// grow appends a recognized transcript record — what reaching the agent's own
// control flow looks like on disk when a tool finally runs.
func (h *askedHarness) grow(t *testing.T, line string) {
	t.Helper()
	appendLines(t, h.n.Transcript, line)
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

func TestDialogDrawMustNotRetireItsOwnNotice(t *testing.T) {
	// AT-CD-12: observed live in P1 (case B). Drawing the dialog *is* an
	// unconfined pane diff — a permission box replaces most of the screen — so
	// retiring notices on pane geometry destroyed the notice one tick after it
	// arrived, and the dialog sat there raising nothing at all. Pane geometry
	// cannot tell "a dialog appeared" from "output resumed": that is the same
	// ambiguity the notice exists to settle, so it must not be the retirement
	// signal.
	h := newAskedHarness(t, true)
	h.a.poll()
	h.drop(t, "Bash")
	*h.pane = "Permission Required: Bash command\ndate > stamp2.txt\nDo you want to proceed?\n1. Yes\n2. Yes, and always allow\n3. No\nEnter selection [1-3]"
	h.a.poll()
	if got := standingNotices(t, h.bundle); len(got) != 1 {
		t.Fatalf("standing notices after the dialog was drawn = %v, want the one that announced it", got)
	}
	if got := h.a.attn[h.n.ID]; got != "approval" {
		t.Fatalf("attention = %q, want approval — the dialog is on screen", got)
	}
}

func TestNoticeNewerThanItsEvidenceSurvives(t *testing.T) {
	// AT-CD-14: the ordering clause. An agent that narrates and *then* asks for
	// permission produces transcript growth whose newest record predates the
	// ask, so growth alone must not retire it — retiring on "something moved"
	// would erase the announcement of a dialog that is about to be drawn.
	h := newAskedHarness(t, true)
	h.a.poll()
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	// The harness's own pending call is resolved here so the *ordering* clause is
	// the only thing keeping this notice alive (AT-CD-15 covers the pending one).
	h.grow(t, `{"type":"user","timestamp":"`+past+`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}`)
	h.grow(t, `{"type":"assistant","timestamp":"`+past+`","message":{"role":"assistant","content":[{"type":"text","text":"about to run it"}]}}`)
	h.drop(t, "Bash")
	*h.pane = strings.Replace(*h.pane, "1m 29s", "1m 30s", 1)
	h.a.poll()
	if got := standingNotices(t, h.bundle); len(got) != 1 {
		t.Fatalf("standing notices = %v, want the ask that came after the newest record", got)
	}
	if got := h.a.attn[h.n.ID]; got != "approval" {
		t.Fatalf("attention = %q, want approval", got)
	}
}

func TestAnnouncingTheBlockedCallMustNotRetireItsNotice(t *testing.T) {
	// AT-CD-15: growth is not resolution while a call is still unresolved. The
	// parallel-call case makes this concrete and it is the one the whole
	// discriminator exists for: Claude asks for A and B in one turn, the human
	// answers A in the pane, and A's records — a tool_result and the agent
	// talking again — carry stamps *newer* than B's notice. Both clauses of the
	// retirement rule (growth, notice older than the evidence) are then satisfied
	// for B, whose dialog is on screen right now. So the gate is the transcript's
	// own pending set: while any call is unresolved, a resolution cannot be
	// claimed, no matter how much newer the newest record is.
	h := newAskedHarness(t, true)
	h.a.poll()
	h.dropAt(t, "Bash", "n1.json", time.Now().Add(-time.Second))
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	h.grow(t, `{"type":"assistant","timestamp":"`+stamp+`","message":{"role":"assistant","content":[{"type":"text","text":"the first one is done"}]}}`)
	*h.pane = strings.Replace(*h.pane, "1m 29s", "1m 30s", 1)
	h.a.poll()
	if got := standingNotices(t, h.bundle); len(got) != 1 {
		t.Fatalf("standing notices = %v, want the ask that is still waiting", got)
	}
	if got := h.a.attn[h.n.ID]; got != "approval" {
		t.Fatalf("attention = %q, want approval — that dialog is on screen", got)
	}
}

func TestTranscriptResolutionRetiresOneNoticeOldestFirst(t *testing.T) {
	// AT-CD-13: retirement, corrected. The human answers in the pane, Claude
	// runs the tool and only then writes its records — so recognized transcript
	// growth on a producing pane is the mechanical proof that an ask was
	// resolved. Exactly one ask is retired per resolution, oldest first: Claude
	// draws queued dialogs in order, and a second standing ask must survive its
	// predecessor's answer or the wait it announces goes unnoticed.
	h := newAskedHarness(t, true)
	h.a.poll()
	h.dropAt(t, "Bash", "old.json", time.Now().Add(-time.Minute))
	h.dropAt(t, "Bash", "new.json", time.Now())
	h.a.poll()
	if got := h.a.attn[h.n.ID]; got != "approval" {
		t.Fatalf("precondition: attention = %q, want approval", got)
	}
	// Real stamps: retirement compares the notice against the CLI's own record
	// time, and an undated transcript must retire nothing. The record that
	// proves an answer is the agent talking again after the tool ran.
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	h.grow(t, `{"type":"user","timestamp":"`+stamp+`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}`)
	h.grow(t, `{"type":"assistant","timestamp":"`+stamp+`","message":{"role":"assistant","content":[{"type":"text","text":"ran it"}]}}`)
	*h.pane = strings.Replace(*h.pane, "1m 29s", "1m 31s", 1)
	h.a.poll()
	got := standingNotices(t, h.bundle)
	if len(got) != 1 || got[0] != "new.json" {
		t.Fatalf("standing notices = %v, want only new.json (one resolution retires one ask, oldest first)", got)
	}
	if got := h.a.attn[h.n.ID]; got != "approval" {
		t.Fatalf("attention = %q, want approval — the second ask is still waiting", got)
	}
	stamp2 := time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)
	h.grow(t, `{"type":"assistant","timestamp":"`+stamp2+`","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
	*h.pane = strings.Replace(*h.pane, "1m 31s", "1m 33s", 1)
	h.a.poll()
	if got := standingNotices(t, h.bundle); len(got) != 0 {
		t.Fatalf("standing notices = %v, want none once both asks resolved", got)
	}
	if got := h.a.attn[h.n.ID]; got != "" {
		t.Fatalf("attention = %q, want none once no ask is standing", got)
	}
}

func TestCapabilityRequiresTheBakedExecPath(t *testing.T) {
	// AT-CD-16 (gap 5): settings.json bakes os.Executable() at launch, so a pane
	// that outlives a *move* of the scimux binary keeps a settings file pointing
	// nowhere. Its hooks can no longer run — and for the notice gate that failure
	// is silent in the dangerous direction: no hook means no notices, and no
	// notices reads as "Claude asked nothing", which suppresses the very
	// attention the pane needs. So both capabilities must be provable against
	// the baked path, not just against the bundle's layout: a bundle whose
	// binary is gone reports no capability, which restores the pane-geometry
	// backstop and shows the auto-approve toggle as unsupported.
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	_ = n
	if !bundleSupportsAsked(bundle) || !bundleSupportsPermission(bundle) {
		t.Fatal("a freshly prepared bundle must prove both capabilities")
	}

	capPath := filepath.Join(bundle, "capabilities.json")
	rewrite := func(t *testing.T, caps string) {
		t.Helper()
		if err := os.WriteFile(capPath, []byte(caps), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gone := filepath.Join(t.TempDir(), "moved-away", "scimux")
	rewrite(t, `{"permission":1,"asked":1,"exec":`+strconv.Quote(gone)+`}`)
	if bundleSupportsAsked(bundle) {
		t.Fatal("a bundle whose binary moved must not gate attention")
	}
	if bundleSupportsPermission(bundle) {
		t.Fatal("a bundle whose binary moved must not advertise auto-approval")
	}

	// Present but not executable is the same failure with a different cause.
	dud := filepath.Join(t.TempDir(), "scimux")
	if err := os.WriteFile(dud, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rewrite(t, `{"permission":1,"asked":1,"exec":`+strconv.Quote(dud)+`}`)
	if bundleSupportsAsked(bundle) || bundleSupportsPermission(bundle) {
		t.Fatal("a non-executable baked path proves nothing")
	}

	// A bundle written before the path was recorded cannot prove it either. The
	// unprovable case degrades exactly like the broken one — that is what keeps
	// the backstop instead of losing attention silently.
	rewrite(t, `{"permission":1,"asked":1}`)
	if bundleSupportsAsked(bundle) || bundleSupportsPermission(bundle) {
		t.Fatal("an unrecorded exec path must not count as proof")
	}
}

func TestLaunchCapabilityIsRecordedFromDiskNotAssumed(t *testing.T) {
	// AT-CD-17 (gap 5): the launch-time note must read the same proof the
	// restart path reads. Otherwise a running scimux would hold "capable" for a
	// bundle whose hooks cannot execute until someone restarts it.
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	hookID := a.claudeHooks[n.ID]
	a.mu.Unlock()

	if err := os.WriteFile(filepath.Join(bundle, "capabilities.json"),
		[]byte(`{"permission":1,"asked":1,"exec":"/nonexistent/scimux"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a.noteClaudeAskedCapability(hookID)
	a.noteClaudePermCapability(hookID)
	a.mu.Lock()
	asked, perm := a.claudeAskedCap[hookID], a.claudePermCap[hookID]
	a.mu.Unlock()
	if asked || perm {
		t.Fatalf("capabilities recorded from an unusable bundle: asked=%v permission=%v", asked, perm)
	}
}
