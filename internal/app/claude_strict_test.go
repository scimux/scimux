package app

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/dialoghint"
)

func TestClaudeLaunchDoesNotUseBridgeStatus(t *testing.T) {
	f := &fakeTmux{captureAfterEnter: "pane"}
	a := newTestApp(t, f)
	// The ready timeout IS the assertion: no SessionStart ever arrives.
	a.claudeReadyTimeout = testTimeoutBudget
	a.claudeDeliveryTimeout = testTimeoutBudget
	a.claudeInitialPoll = testInitialPoll
	n := &Node{ID: "n1", Agent: "claude", SessionID: hookSIDOwn, Prompt: "hello"}
	path := writeClaudeTranscript(t, a.home, n.SessionID)
	appendLines(t, path,
		`{"type":"system","subtype":"bridge_status","sessionId":"`+hookSIDOwn+`","content":"ready"}`)
	if got := a.deliverClaudeInitialPrompt(n); got != initialNotSent {
		t.Fatalf("delivery = %q, want not_sent: bridge_status is not readiness", got)
	}
	if f.didSendEnter() {
		t.Fatal("prompt must not be sent on bridge_status alone")
	}
	if a.claudeLaunchError(n.ID) == "" {
		t.Fatal("missing SessionStart must record an inline launch error")
	}
}

func TestClaudeLaunchWaitsForSessionStartThenConfirms(t *testing.T) {
	f := &fakeTmux{captureAfterEnter: "pane"}
	a := newTestApp(t, f)
	a.claudeReadyTimeout = testReadyBudget
	a.claudeDeliveryTimeout = testDeliverBudget
	a.claudeInitialPoll = testInitialPoll
	n := &Node{ID: "n1", Agent: "claude", SessionID: hookSIDOwn, Prompt: "hello"}
	path := writeClaudeTranscript(t, a.home, n.SessionID)
	n.Transcript = path
	a.byID[n.ID] = n
	a.nodes = append(a.nodes, n)
	installPreparedClaudeHook(t, a, n)
	f.appendOnEnter(t, path,
		`{"type":"user","timestamp":"2026-08-18T00:00:00Z","message":{"role":"user","content":"hello"}}`)
	if got := a.deliverClaudeInitialPrompt(n); got != initialAcknowledged {
		t.Fatalf("delivery = %q, want acknowledged after SessionStart", got)
	}
}

func TestClaudeLaunchConfirmsBracketedPasteTranscriptEnvelope(t *testing.T) {
	f := &fakeTmux{captureAfterEnter: "pane"}
	a := newTestApp(t, f)
	a.claudeReadyTimeout = testReadyBudget
	a.claudeDeliveryTimeout = testDeliverBudget
	a.claudeInitialPoll = testInitialPoll
	n := &Node{ID: "n1", Agent: "claude", SessionID: hookSIDOwn, Prompt: "Review the synthetic release checklist"}
	path := writeClaudeTranscript(t, a.home, n.SessionID)
	n.Transcript = path
	a.byID[n.ID] = n
	a.nodes = append(a.nodes, n)
	installPreparedClaudeHook(t, a, n)
	f.appendOnEnter(t, path,
		`{"type":"user","timestamp":"2027-01-02T03:04:05Z","message":{"role":"user","content":"\n\n<pasted_content id=\"p7_q-2\">\nReview the synthetic release checklist\n</pasted_content id=\"p7_q-2\">\n"}}`)
	if got := a.deliverClaudeInitialPrompt(n); got != initialAcknowledged {
		t.Fatalf("delivery = %q, want acknowledged for Claude's pasted_content envelope", got)
	}
}

func TestClaudeTrustDialogDiagnosesLaunchWithoutTerminal(t *testing.T) {
	f := &fakeTmux{capture: workspaceTrustPane}
	a := newTestApp(t, f)
	n := &Node{ID: "n1", Agent: "claude", SessionID: hookSIDOwn, Dir: "/new/proj"}
	msg := a.diagnoseClaudeStartFailure(n)
	if !strings.Contains(msg, "trust") {
		t.Fatalf("trust diagnosis = %q, want a trust explanation", msg)
	}
	if !dialoghint.LooksLikeWorkspaceTrust(workspaceTrustPane) {
		t.Fatal("LooksLikeWorkspaceTrust must recognize the lettered trust dialog")
	}
}

func TestClaudeWorkspaceTrustExtendsSessionStartWait(t *testing.T) {
	f := &fakeTmux{capture: workspaceTrustPane, captureAfterEnter: "Claude ready"}
	a := newTestApp(t, f)
	a.claudeReadyTimeout = testTimeoutBudget
	a.claudeDeliveryGiveUp = testReadyBudget
	a.claudeDeliveryTimeout = testDeliverBudget
	a.claudeInitialPoll = testInitialPoll
	n := &Node{
		ID: "n1", Agent: "claude", SessionID: hookSIDOwn,
		Dir: "/new/proj", Prompt: "continue after trust",
	}
	path := writeClaudeTranscript(t, a.home, n.SessionID)
	n.Transcript = path
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	f.appendOnEnter(t, path, claudeUserLine(n.Prompt, 0))

	done := make(chan initialDelivery, 1)
	go func() { done <- a.deliverClaudeInitialPrompt(n) }()
	deadline := time.Now().Add(2 * time.Second)
	for !containsSub(f.subcommands(), "capture-pane") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !containsSub(f.subcommands(), "capture-pane") {
		t.Fatal("startup timeout never inspected the trust prompt")
	}
	// Let the timeout branch finish. Without the trust-specific extension it
	// returns initialNotSent here and the later web decision cannot recover the
	// launch.
	time.Sleep(10 * time.Millisecond)
	select {
	case got := <-done:
		t.Fatalf("trust prompt ended startup wait early: %q", got)
	default:
	}

	installPreparedClaudeHook(t, a, n)
	select {
	case got := <-done:
		if got != initialAcknowledged {
			t.Fatalf("delivery after trust = %q, want acknowledged", got)
		}
	case <-time.After(testReadyBudget):
		t.Fatal("delivery did not resume after trust was accepted")
	}
}

func TestClaudeCapabilitiesWaitForSessionStart(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.claudeHooks[n.ID] = hookID
	a.mu.Unlock()
	if a.autoApproveSupportedFor(n) {
		t.Fatal("must not be hook-capable before SessionStart")
	}
	if a.claudeSupervisionOf(n) != claudeSupStarting {
		t.Fatalf("supervision = %q, want starting before SessionStart", a.claudeSupervisionOf(n))
	}
	a.markClaudeHookAck(n.ID)
	a.noteClaudeHookCapabilitiesForNode(n.ID)
	if !a.autoApproveSupportedFor(n) {
		t.Fatal("SessionStart must publish hook capability")
	}
	if a.claudeSupervisionOf(n) != claudeSupStrict {
		t.Fatalf("supervision = %q, want strict after SessionStart", a.claudeSupervisionOf(n))
	}
}

func TestClaudeUnsupportedNeverInspects(t *testing.T) {
	f := &fakeTmux{list: []string{"legacy"}, alive: map[string]bool{"legacy": true}, capture: "quiet pane"}
	a := newTestApp(t, f)
	n := &Node{ID: "legacy", Agent: "claude", Adopted: true, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.prevCap[n.ID] = "quiet pane"
	a.lastChg[n.ID] = time.Now().Add(-time.Minute)
	a.poll()
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("adopted Claude attention = %q, want none", got)
	}
	if a.claudeSupervisionOf(n) != claudeSupUnsupported {
		t.Fatalf("supervision = %q, want unsupported", a.claudeSupervisionOf(n))
	}
}

func TestClaudeUnsupportedExplanationIsPlainAndActionable(t *testing.T) {
	const want = "This chat uses an older scimux setup. Fork or relaunch it to use all features."
	if got := claudeSupervisionExplain(claudeSupUnsupported, ""); got != want {
		t.Fatalf("unsupported explanation = %q, want %q", got, want)
	}
	if got := claudeChatSupervisionExplain(claudeSupUnsupported, "", true); got != "" {
		t.Fatalf("ended chat explanation = %q, want no redundant compatibility notice", got)
	}
}

func TestClaudePermissionDialogRequiresNotification(t *testing.T) {
	h := newAskedHarness(t, true)
	// Write a notice without marking it shown — PermissionRequest alone is not
	// proof the dialog remained visible after hook aggregation.
	note := claudeAskedNotice{
		At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "d1",
	}
	if err := writeClaudePermFile(filepath.Join(askedDir(h.bundle), "only-ask.json"), note); err != nil {
		t.Fatal(err)
	}
	h.a.poll()
	if got := h.a.attn[h.n.ID]; got != "" {
		t.Fatalf("attention = %q, want none without Notification(permission_prompt)", got)
	}
	if _, err := mintClaudeVisibleEpoch(filepath.Join(h.bundle, "perm"), hookSIDOwn); err != nil {
		t.Fatal(err)
	}
	h.a.poll()
	if got := h.a.attn[h.n.ID]; got != "approval" {
		t.Fatalf("attention = %q, want approval once Notification minted a visible epoch", got)
	}
}

func TestClaudeKeyRetiresExactRequestAndLeavesQueued(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, capture: axPermissionPane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	old := claudeAskedNotice{At: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "d-old"}
	cur := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Edit", Digest: "d-cur"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "oldreq"), old); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(claudeAskedPath(perm, "curreq"), cur); err != nil {
		t.Fatal(err)
	}
	ep, err := mintClaudeVisibleEpoch(perm, hookSIDOwn)
	if err != nil {
		t.Fatal(err)
	}
	a.attn[n.ID] = "approval"

	rec := httptestPostKey(t, a, n.ID, "1", ep.Epoch)
	if rec.Code != 200 {
		t.Fatalf("key: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(claudeAskedPath(perm, "curreq")); err != nil {
		t.Fatal("answering an epoch must not delete a guessed standing ask")
	}
	if _, err := os.Stat(claudeAskedPath(perm, "oldreq")); err != nil {
		t.Fatal("queued request must remain")
	}
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("attention after key = %q, want none", got)
	}
	if _, ok := readClaudeVisibleEpoch(perm); ok {
		t.Fatal("visible epoch must be retired after a successful key")
	}
}

func TestClaudeKeyFailureKeepsRequest(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, capture: axPermissionPane, sendKeysErr: true}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "d1"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "req1"), note); err != nil {
		t.Fatal(err)
	}
	ep, err := mintClaudeVisibleEpoch(perm, hookSIDOwn)
	if err != nil {
		t.Fatal(err)
	}
	a.attn[n.ID] = "approval"
	rec := httptestPostKey(t, a, n.ID, "1", ep.Epoch)
	if rec.Code == 200 {
		t.Fatal("want key delivery failure")
	}
	if _, err := os.Stat(claudeAskedPath(perm, "req1")); err != nil {
		t.Fatal("failed key must leave the notice standing")
	}
	if got := a.attn[n.ID]; got != "approval" {
		t.Fatalf("attention after failed key = %q, want approval", got)
	}
}

func TestClaudeForkSourceIsRejected(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("keep", 0))
	next := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, cur)
	if err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "fork",
		SessionID: hookSIDSuccessor, TranscriptPath: next, Cwd: "/w/proj",
	}); err == nil {
		t.Fatal("SessionStart source:fork must be rejected")
	}
	if n.SessionID != hookSIDOwn || n.Transcript != cur {
		t.Fatalf("rejected fork rebound the node: %+v", n)
	}
	if a.attn[n.ID] == "inspect" {
		t.Fatal("rejected fork must not raise inspect")
	}
	if allowedClaudeHookSource("fork") {
		t.Fatal("fork is not an accepted SessionStart source")
	}
}

func TestPiTmuxInspectUnchanged(t *testing.T) {
	pane := "Agent finished.\n$ "
	n := tmuxFallbackNode("n", "")
	a := newPollApp(t, n, pollRunner([]string{n.ID}, pane, false, nil))
	a.prevCap[n.ID] = pane
	a.lastChg[n.ID] = time.Now().Add(-10 * time.Second)
	a.poll()
	if got := a.attn[n.ID]; got != "inspect" {
		t.Fatalf("pi tmux attention = %q, want inspect (non-Claude fallback unchanged)", got)
	}
}

func TestClaudeSettingsRegisterNotificationMatcher(t *testing.T) {
	raw, err := claudeHookSettingsJSON("/tmp/scimux", "/tmp/hooks/id")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	hooks := doc["hooks"].(map[string]any)
	notes, _ := hooks["Notification"].([]any)
	if len(notes) != 1 {
		t.Fatalf("Notification hooks = %v", raw)
	}
	if !strings.Contains(string(raw), "permission_prompt") {
		t.Fatal("Notification hook must match permission_prompt")
	}
	if !strings.Contains(string(raw), claudeNotifyHookCmd) {
		t.Fatal("Notification hook must invoke the notify helper")
	}
}

func httptestPostKey(t *testing.T, a *app, id, key, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]string{"key": key}
	if requestID != "" {
		body["request_id"] = requestID
		body["dialog_id"] = requestID
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return routeRequest(newTestHandler(t, a), "POST", "/api/nodes/"+id+"/key", string(raw), true)
}

func chatBody(t *testing.T, a *app, id string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/nodes/"+id+"/chat", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	a.handleChat(rec, req)
	if rec.Code != 200 {
		t.Fatalf("chat: %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestClaudeCreateReturnsPendingBeforeSessionStart(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	a.deliverClaudeInitial = a.deliverClaudeInitialPrompt
	// Generous: no SessionStart ever arrives, and the point is that delivery is
	// still pending while the assertions below run.
	a.claudeReadyTimeout = testReadyBudget
	a.claudeDeliveryTimeout = testDeliverBudget
	a.claudeInitialPoll = testInitialPoll
	start := time.Now()
	rec := newNode(a, `{"title":"Pale","prompt":"hello pale","agent":"claude","dir":`+strconv.Quote(a.home)+`}`)
	// A create that waited for SessionStart would take the whole ready budget;
	// the margin is a fraction of it so a loaded machine cannot fake the verdict.
	if blocked := time.Since(start); blocked > testReadyBudget/2 {
		t.Fatalf("create blocked for %s; must return before SessionStart", blocked)
	}
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID              string          `json:"id"`
		InitialDelivery initialDelivery `json:"initial_delivery"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.InitialDelivery != initialPending {
		t.Fatalf("initial_delivery = %q, want pending", created.InitialDelivery)
	}
	if f.didSendEnter() {
		t.Fatal("must not paste before SessionStart")
	}
	body := chatBody(t, a, created.ID)
	if body["pending_prompt"] != "hello pale" {
		t.Fatalf("pending_prompt = %v, want the launch prompt before SessionStart", body["pending_prompt"])
	}
	if body["supervision"] != string(claudeSupStarting) {
		t.Fatalf("supervision = %v, want starting", body["supervision"])
	}
	if attn, _ := body["attention"].(string); attn != "" {
		t.Fatalf("attention = %q, want none during startup", attn)
	}
	send := routeRequest(newTestHandler(t, a), "POST", "/api/nodes/"+created.ID+"/send", `{"text":"second"}`, true)
	if send.Code != 409 {
		t.Fatalf("send during first-prompt gate = %d, want 409", send.Code)
	}
}

func TestClaudePalePromptSolidAfterTranscriptConfirm(t *testing.T) {
	f := &fakeTmux{captureAfterEnter: "pane"}
	a := newTestApp(t, f)
	a.deliverClaudeInitial = a.deliverClaudeInitialPrompt
	a.claudeReadyTimeout = testReadyBudget
	a.claudeDeliveryTimeout = testDeliverBudget
	a.claudeInitialPoll = testInitialPoll
	rec := newNode(a, `{"title":"Solid","prompt":"hello pale","agent":"claude","dir":`+strconv.Quote(a.home)+`}`)
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
	path := writeClaudeTranscript(t, a.home, n.SessionID)
	if err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "startup",
		SessionID: n.SessionID, TranscriptPath: path, Cwd: a.home,
	}); err != nil {
		t.Fatalf("startup: %v", err)
	}
	// The paste is an event; wait for it rather than for a duration. This test
	// deliberately inspects the pale bubble in the window between the paste and
	// the confirming turn, so it cannot use appendOnEnter.
	f.awaitEnter(t)
	before := chatBody(t, a, n.ID)
	if before["pending_prompt"] != "hello pale" {
		t.Fatalf("bubble must stay pale after paste, pending_prompt=%v", before["pending_prompt"])
	}
	appendLines(t, path,
		`{"type":"user","timestamp":"2026-08-18T00:00:00Z","message":{"role":"user","content":"hello pale"}}`)
	waitClaudeInitialGate(t, a, n.ID)
	a.reconcileClaudeInitialDelivery(n)
	after := chatBody(t, a, n.ID)
	if _, ok := after["pending_prompt"]; ok {
		t.Fatalf("pending_prompt still set after transcript confirm: %v", after["pending_prompt"])
	}
	if after["supervision"] != string(claudeSupStrict) {
		t.Fatalf("supervision = %v, want strict", after["supervision"])
	}
}

func TestClaudeStartupFailureRestoresDraftWithoutTerminal(t *testing.T) {
	f := &fakeTmux{capture: "y. Continue\nn. Cancel\nEnter y/n:\n"}
	a := newTestApp(t, f)
	a.deliverClaudeInitial = a.deliverClaudeInitialPrompt
	// The ready timeout IS the assertion: the trust dialog means SessionStart
	// never arrives, and waitClaudeInitialGate below requires it to give up.
	a.claudeReadyTimeout = testTimeoutBudget
	a.claudeDeliveryTimeout = testTimeoutBudget
	a.claudeInitialPoll = testInitialPoll
	rec := newNode(a, `{"title":"FailStart","prompt":"keep me","agent":"claude","dir":`+strconv.Quote(a.home)+`}`)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	waitClaudeInitialGate(t, a, created.ID)
	if f.didSendEnter() {
		t.Fatal("failed SessionStart must not paste")
	}
	body := chatBody(t, a, created.ID)
	if body["restore_draft"] != "keep me" {
		t.Fatalf("restore_draft = %v", body["restore_draft"])
	}
	if _, ok := body["pending_prompt"]; ok {
		t.Fatalf("failed launch left pending_prompt: %v", body["pending_prompt"])
	}
	if attn, _ := body["attention"].(string); attn != "" {
		t.Fatalf("attention = %q, must not open the terminal", attn)
	}
	errText, _ := body["error"].(string)
	if strings.Contains(strings.ToLower(errText), "trust this directory") {
		t.Fatalf("generic lettered dialog must not be diagnosed as workspace trust: %q", errText)
	}
}

func TestClaudeStopClearsAskedAndLaterNotifyDoesNotPair(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "quiet"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	noteA := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "da"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "ask-a"), noteA); err != nil {
		t.Fatal(err)
	}
	epA, err := mintClaudeVisibleEpoch(perm, hookSIDOwn)
	if err != nil {
		t.Fatal(err)
	}
	a.poll()
	if got := a.attn[n.ID]; got != "approval" {
		t.Fatalf("attention after A = %q", got)
	}
	if epA.HintTool != "Bash" {
		t.Fatalf("label = %q, want Bash", epA.HintTool)
	}
	a.resetClaudePermissionTurn(n)
	if _, err := os.Stat(claudeAskedPath(perm, "ask-a")); !os.IsNotExist(err) {
		t.Fatal("stop must tombstone request A")
	}
	if !claudeRequestAnswered(perm, "ask-a") {
		t.Fatal("A must be answered/tombstoned")
	}
	if _, ok := readClaudeVisibleEpoch(perm); ok {
		t.Fatal("visible epoch must be cleared at the turn boundary")
	}
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("attention after stop = %q", got)
	}
	noteB := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Edit", Digest: "db"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "ask-b"), noteB); err != nil {
		t.Fatal(err)
	}
	epB, err := mintClaudeVisibleEpoch(perm, hookSIDOwn)
	if err != nil {
		t.Fatal(err)
	}
	if epB.Epoch == epA.Epoch {
		t.Fatal("later notification reused A's epoch")
	}
	if epB.Epoch == "ask-a" || epB.Epoch == epA.Epoch {
		t.Fatal("later notification reused A's identity")
	}
	body := chatBody(t, a, n.ID)
	if body["perm_dialog_id"] == "ask-a" || body["perm_request_id"] == "ask-a" {
		t.Fatal("chat reopened A as the current dialog")
	}
}

func TestClaudeNotifyDoesNotClaimExactRequestCorrelation(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: axPermissionPane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	old := claudeAskedNotice{At: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "d-old"}
	cur := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Edit", Digest: "d-cur"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "oldreq"), old); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(claudeAskedPath(perm, "curreq"), cur); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claudeNotifyEvent{
		HookEventName: "Notification", NotificationType: "permission_prompt", SessionID: hookSIDOwn,
	})
	if err := writeClaudeHookInboxFile(filepath.Join(bundle, "notify"), payload); err != nil {
		t.Fatal(err)
	}
	a.drainClaudeNotifyInbox(n.ID, filepath.Base(bundle))
	ep, ok := readClaudeVisibleEpoch(perm)
	if !ok {
		t.Fatal("notification must mint a visible-dialog epoch")
	}
	if ep.Epoch == "oldreq" || ep.Epoch == "curreq" {
		t.Fatalf("epoch %q claims a request nonce", ep.Epoch)
	}
	body := chatBody(t, a, n.ID)
	if id, _ := body["perm_dialog_id"].(string); id != ep.Epoch {
		t.Fatalf("perm_dialog_id = %v, want epoch %s", body["perm_dialog_id"], ep.Epoch)
	}
	if _, ok := body["perm_request_id"]; ok {
		t.Fatalf("Claude chat must not present perm_request_id as Claude's identity: %v", body["perm_request_id"])
	}
	if ep.HintTool != "Edit" {
		t.Fatalf("label = %q, want newest standing ask tool as a label only", ep.HintTool)
	}
	rec := httptestPostKey(t, a, n.ID, "1", ep.Epoch)
	if rec.Code != 200 {
		t.Fatalf("key: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(claudeAskedPath(perm, "oldreq")); err != nil {
		t.Fatal("queued older ask must remain")
	}
	if _, err := os.Stat(claudeAskedPath(perm, "curreq")); err != nil {
		t.Fatal("answering an epoch must not delete a guessed standing ask")
	}
}

func TestClaudeArmedAutoApproveFailureIsInlineOnly(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "1. Yes"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "d1"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "req1"), note); err != nil {
		t.Fatal(err)
	}
	if _, err := mintClaudeVisibleEpoch(perm, hookSIDOwn); err != nil {
		t.Fatal(err)
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	a.poll()
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("armed attention = %q, want none", got)
	}
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	errText := ""
	if st != nil {
		errText = st.Error
	}
	a.mu.Unlock()
	if errText == "" {
		t.Fatal("armed visible dialog must record an inline auto-approve error")
	}
	body := chatBody(t, a, n.ID)
	if body["perm_dialog_id"] == nil || body["perm_dialog_id"] == "" {
		t.Fatal("armed leftover dialog must still expose an epoch-bound action surface")
	}
	if body["perm_manual"] != true {
		t.Fatalf("perm_manual = %v, want true while armed", body["perm_manual"])
	}
	if attn, _ := body["attention"].(string); attn != "" {
		t.Fatalf("chat attention = %q", attn)
	}
	aa, _ := body["auto_approve"].(map[string]any)
	if aa["error"] == nil || aa["error"] == "" {
		t.Fatalf("auto_approve.error missing: %v", aa)
	}
}

func TestClaudeDisableAutoApproveExposesVisibleDialog(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "1. Yes"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "d1"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "req1"), note); err != nil {
		t.Fatal(err)
	}
	ep, err := mintClaudeVisibleEpoch(perm, hookSIDOwn)
	if err != nil {
		t.Fatal(err)
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	a.poll()
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("armed attention = %q", got)
	}
	a.disableAutoApprove(n.ID)
	if got := a.attn[n.ID]; got != "approval" {
		t.Fatalf("attention after disable = %q, want approval", got)
	}
	body := chatBody(t, a, n.ID)
	if body["perm_dialog_id"] != ep.Epoch {
		t.Fatalf("perm_dialog_id = %v, want %s", body["perm_dialog_id"], ep.Epoch)
	}
}

func TestClaudeCurrentBundleReportsStartingBeforeAck(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.claudeHooks[n.ID] = hookID
	a.mu.Unlock()
	if a.claudeSupervisionOf(n) != claudeSupStarting {
		t.Fatalf("supervision = %q, want starting for a current bundle awaiting SessionStart", a.claudeSupervisionOf(n))
	}
	if a.autoApproveSupportedFor(n) {
		t.Fatal("permission capability must wait for SessionStart")
	}
	a.markClaudeHookAck(n.ID)
	a.noteClaudeHookCapabilitiesForNode(n.ID)
	if a.claudeSupervisionOf(n) != claudeSupStrict {
		t.Fatalf("supervision = %q, want strict after ack", a.claudeSupervisionOf(n))
	}
}

func TestClaudeGenericLetteredDialogIsNotWorkspaceTrust(t *testing.T) {
	generic := "y. Continue\nn. Cancel\nEnter y/n:\n"
	if dialoghint.LooksLikeWorkspaceTrust(generic) {
		t.Fatal("generic lettered dialog must not be workspace trust")
	}
	if !dialoghint.LooksLikeWorkspaceTrust(workspaceTrustPane) {
		t.Fatal("real trust pane must still match")
	}
	f := &fakeTmux{capture: generic}
	a := newTestApp(t, f)
	n := &Node{ID: "n1", Agent: "claude"}
	msg := a.diagnoseClaudeStartFailure(n)
	if strings.Contains(strings.ToLower(msg), "trust this directory") ||
		strings.Contains(strings.ToLower(msg), "workspace-trust") {
		t.Fatalf("uncertain dialog used a trust-specific instruction: %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "outside scimux") {
		t.Fatalf("neutral diagnosis = %q, want inspect-outside-scimux", msg)
	}
}

func TestClaudeStaleDialogIDDoesNotSendKeys(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "1. Yes"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "d1"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "req1"), note); err != nil {
		t.Fatal(err)
	}
	ep, err := mintClaudeVisibleEpoch(perm, hookSIDOwn)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptestPostKey(t, a, n.ID, "1", "not-the-epoch")
	if rec.Code != 409 {
		t.Fatalf("mismatched dialog_id = %d, want 409", rec.Code)
	}
	if containsSub(f.subcommands(), "send-keys") {
		t.Fatal("stale dialog_id must not send keys")
	}

	clearClaudeVisibleEpoch(perm)
	rec = httptestPostKey(t, a, n.ID, "1", ep.Epoch)
	if rec.Code != 409 {
		t.Fatalf("retired epoch = %d, want 409", rec.Code)
	}
	if containsSub(f.subcommands(), "send-keys") {
		t.Fatal("retired dialog_id must not send keys")
	}
}

func TestClaudeQuestionDialogIsAnswerableWithoutAutoApprove(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "1. A"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "AskUserQuestion", Digest: "q1"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "qreq"), note); err != nil {
		t.Fatal(err)
	}
	ep, err := mintClaudeVisibleEpoch(perm, hookSIDOwn)
	if err != nil {
		t.Fatal(err)
	}
	a.poll()
	if got := a.attn[n.ID]; got != "question" {
		t.Fatalf("attention = %q, want question", got)
	}
	body := chatBody(t, a, n.ID)
	if body["perm_dialog_id"] != ep.Epoch {
		t.Fatalf("perm_dialog_id = %v, want epoch-bound question surface", body["perm_dialog_id"])
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	a.poll()
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("armed question attention = %q, want none (no auto-open)", got)
	}
	armed := chatBody(t, a, n.ID)
	if armed["perm_dialog_id"] != ep.Epoch {
		t.Fatal("armed question must keep an epoch-bound action surface")
	}
	if armed["perm_manual"] != true {
		t.Fatal("armed question must be marked perm_manual")
	}
}

func TestClaudeUnarmedStopClearsEpochAndAsks(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "quiet"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "d1"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "ask-a"), note); err != nil {
		t.Fatal(err)
	}
	if _, err := mintClaudeVisibleEpoch(perm, hookSIDOwn); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claudeStopEvent{HookEventName: "Stop", SessionID: hookSIDOwn})
	if err := RunClaudeStopHook(bundle, strings.NewReader(string(payload)), nil, nil); err != nil {
		t.Fatalf("unarmed Stop hook: %v", err)
	}
	a.drainClaudeStopInbox(n.ID, filepath.Base(bundle))
	if _, ok := readClaudeVisibleEpoch(perm); ok {
		t.Fatal("unarmed Stop must clear the visible epoch")
	}
	if _, err := os.Stat(claudeAskedPath(perm, "ask-a")); !os.IsNotExist(err) {
		t.Fatal("unarmed Stop must tombstone standing asks")
	}
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("attention after unarmed Stop = %q", got)
	}
}

func TestClaudeLateNotifyAfterStopDoesNotReopenEpoch(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "quiet"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	a.resetClaudePermissionTurn(n)
	payload, _ := json.Marshal(claudeNotifyEvent{
		HookEventName: "Notification", NotificationType: "permission_prompt", SessionID: hookSIDOwn,
	})
	if err := writeClaudeHookInboxFile(filepath.Join(bundle, "notify"), payload); err != nil {
		t.Fatal(err)
	}
	a.drainClaudeNotifyInbox(n.ID, filepath.Base(bundle))
	if _, ok := readClaudeVisibleEpoch(perm); ok {
		t.Fatal("late notification after turn close must not mint an epoch")
	}
}

func TestClaudeCreateMarshalsSnapshotNotLiveNode(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	started := make(chan struct{})
	release := make(chan struct{})
	a.deliverClaudeInitial = func(n *Node) initialDelivery {
		close(started)
		<-release
		return initialPending
	}
	rec := newNode(a, `{"title":"Race","prompt":"p","agent":"claude","dir":`+strconv.Quote(a.home)+`}`)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var body Node
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID == "" || body.SessionID == "" {
		t.Fatalf("snapshot missing identity: %+v", body)
	}
	allocated := body.SessionID
	a.mu.Lock()
	if live := a.byID[body.ID]; live != nil {
		live.SessionID = "mutated-after-http"
		live.Transcript = "/tmp/mutated.jsonl"
	}
	a.mu.Unlock()
	if body.SessionID != allocated || body.SessionID == "mutated-after-http" {
		t.Fatalf("HTTP snapshot followed a concurrent bind: %q", body.SessionID)
	}
	close(release)
	<-started
}

func TestClaudeResumeCommitFailureKeepsOldBinding(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("keep", 0))
	next := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, cur)
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.claudeHooks[n.ID] = hookID
	a.claudeGens[n.ID] = 1
	a.mu.Unlock()
	if err := os.Remove(a.storePath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(a.storePath, 0o700); err != nil {
		t.Fatal(err)
	}
	err = a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "resume",
		SessionID: hookSIDSuccessor, TranscriptPath: next, Cwd: "/w/proj",
	})
	if err == nil {
		t.Fatal("want resume commit failure")
	}
	if n.Transcript != cur || n.SessionID != hookSIDOwn {
		t.Fatalf("failed resume retired the old binding: %q / %q", n.Transcript, n.SessionID)
	}
	if a.attn[n.ID] == "inspect" {
		t.Fatal("failed resume must not raise inspect")
	}
	if a.claudeLaunchError(n.ID) == "" {
		t.Fatal("failed resume must publish an inline error")
	}
}
