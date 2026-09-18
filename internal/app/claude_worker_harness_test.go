package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/sessionworker"
	"github.com/scimux/scimux/internal/tmuxsession"
)

func testClaudeSessionHarness(a *app, id string) *claudeSessionHarness {
	h := &claudeSessionHarness{
		nodeID: id, app: a, answers: map[string]preparedClaudeAnswer{},
		stopCh: make(chan struct{}), done: make(chan struct{}),
	}
	h.refreshState()
	return h
}

// One test crosses the whole Claude adapter surface with existing synthetic
// hook and tmux fixtures. The large Claude suites continue to contain policy;
// this test contains only boundary regressions (typed state, epoch fencing,
// evidence-before-delivery, exactly-once token, and renderer key sequence).
func TestClaudeSessionHarnessDescribesAndAnswersCurrentDialog(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: axPermissionPane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	n.AXScreenReader = true
	perm := filepath.Join(bundle, "perm")
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "worker-dialog"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "req"), note); err != nil {
		t.Fatal(err)
	}
	epoch, err := mintClaudeVisibleEpoch(perm, hookSIDOwn)
	if err != nil {
		t.Fatal(err)
	}
	elicitation := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "Need a token", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(elicitation), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	h := testClaudeSessionHarness(a, n.ID)

	// The worker-owned ticker produces supervision state. Reading that state
	// may project it, but must not run another supervision cycle at browser
	// request frequency.
	h.poll()
	f.mu.Lock()
	callsBeforeState := len(f.calls)
	f.mu.Unlock()
	state := h.State(context.Background())
	f.mu.Lock()
	stateCalls := append([][]string(nil), f.calls[callsBeforeState:]...)
	f.mu.Unlock()
	for _, call := range stateCalls {
		if len(call) >= 3 && (call[2] == "list-sessions" || call[2] == "capture-pane") {
			t.Fatalf("State ran supervision command %q", call[2])
		}
	}
	if state.Supervision != string(claudeSupStrict) || !state.HasSession || !state.AXScreenReader || !state.AutoApprove.Supported {
		t.Fatalf("State = %#v", state)
	}
	if state.Permission == nil || !state.Permission.Dialog || state.Permission.RequestID != epoch.Epoch || len(state.Permission.Options) == 0 {
		t.Fatalf("dialog state = %#v", state.Permission)
	}
	if len(state.Elicitations) != 1 || state.Elicitations[0].Server != "docs-server" {
		t.Fatalf("elicitation state = %#v", state.Elicitations)
	}
	if visible := h.Peek(context.Background(), "visible"); !strings.Contains(visible, "Permission Required") {
		t.Fatalf("visible snapshot = %q", visible)
	}
	prepared, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch.Epoch, Key: "1"})
	if err != nil || prepared.Token == "" || len(prepared.Keys) != 2 || prepared.Keys[0] != "1" || prepared.Keys[1] != "Enter" || prepared.Evidence == "" {
		t.Fatalf("PreparePermission = %#v, %v", prepared, err)
	}
	if containsSub(f.subcommands(), "send-keys") {
		t.Fatal("prepare delivered before the muxer could persist its audit")
	}
	if err := h.DeliverPermission(context.Background(), prepared.Token); err != nil {
		t.Fatal(err)
	}
	if !containsSub(f.subcommands(), "send-keys") {
		t.Fatal("deliver did not send the prepared terminal choice")
	}
	if err := h.DeliverPermission(context.Background(), prepared.Token); !errors.Is(err, errClaudeWorkerConflict) {
		t.Fatalf("reused prepared token = %v", err)
	}
}

func TestClaudeSessionHarnessRoutesWorkspaceTrustDecision(t *testing.T) {
	f := &fakeTmux{
		list: []string{"n1"}, alive: map[string]bool{"n1": true},
		capture: workspaceTrustPane,
	}
	a := newTestApp(t, f)
	n := &Node{ID: "n1", Agent: "claude", SessionID: hookSIDOwn, AXScreenReader: true}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.claudeHooks[n.ID] = "hook-n1"
	a.claudeStrictCap["hook-n1"] = true
	a.prevCap[n.ID] = workspaceTrustPane
	a.lastChg[n.ID] = time.Now().Add(-paneQuietAfter - time.Second)
	h := testClaudeSessionHarness(a, n.ID)
	h.poll()

	state := h.State(context.Background())
	if state.Supervision != string(claudeSupStarting) || state.Attention != "dialog" ||
		state.Permission == nil || state.Permission.RequestID == "" || len(state.Permission.Options) != 2 {
		t.Fatalf("workspace trust state = %#v", state)
	}
	if _, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{
		RequestID: state.Permission.RequestID, Key: "Enter",
	}); err == nil {
		t.Fatal("trust decision accepted a key outside y/n")
	}
	if _, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{
		RequestID: "workspace-trust:stale", Key: "y",
	}); !errors.Is(err, errClaudeWorkerConflict) {
		t.Fatalf("stale trust decision = %v", err)
	}
	prepared, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{
		RequestID: state.Permission.RequestID, Key: "y",
	})
	if err != nil || prepared.Token == "" || strings.Join(prepared.Keys, " ") != "y Enter" ||
		!strings.Contains(prepared.Evidence, "Accessing workspace") {
		t.Fatalf("prepared trust decision = %#v, %v", prepared, err)
	}
	if containsSub(f.subcommands(), "send-keys") {
		t.Fatal("prepare delivered before the muxer could persist its audit")
	}
	f.mu.Lock()
	f.capture = "Claude prompt after trust dialog disappeared"
	f.mu.Unlock()
	if err := h.DeliverPermission(context.Background(), prepared.Token); !errors.Is(err, errClaudeWorkerConflict) {
		t.Fatalf("stale prepared trust decision = %v", err)
	}
	if containsSub(f.subcommands(), "send-keys") {
		t.Fatal("stale trust decision sent keys into Claude's prompt")
	}
	f.mu.Lock()
	f.capture = workspaceTrustPane
	f.mu.Unlock()
	prepared, err = h.PreparePermission(context.Background(), sessionworker.PermissionDecision{
		RequestID: state.Permission.RequestID, Key: "y",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.DeliverPermission(context.Background(), prepared.Token); err != nil {
		t.Fatal(err)
	}
	if calls := sendKeysCalls(f); len(calls) != 1 || strings.Join(calls[0][2:], " ") != "send-keys -t =n1: y Enter" {
		t.Fatalf("trust delivery = %v", calls)
	}
}

func TestClaudeSessionHarnessDeliveryResolutionAndAutoApprove(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "idle"}
	a := newTestApp(t, f)
	n, _ := seedPermClaude(t, a, "n1", hookSIDOwn)
	h := testClaudeSessionHarness(a, n.ID)
	a.mu.Lock()
	a.sendState[n.ID] = sendUnconfirmed
	a.mu.Unlock()
	if _, err := h.Send(context.Background(), "second"); !errors.Is(err, errClaudeWorkerConflict) {
		t.Fatalf("send across unresolved delivery = %v", err)
	}
	if err := h.ResolveDelivery(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.sendState[n.ID] = sendSubmitting
	a.mu.Unlock()
	if err := h.ResolveDelivery(context.Background()); !errors.Is(err, errClaudeWorkerConflict) {
		t.Fatalf("ResolveDelivery while submitting = %v", err)
	}
	if _, err := h.Clear(context.Background()); !errors.Is(err, errClaudeWorkerConflict) {
		t.Fatalf("Clear while submitting = %v", err)
	}
	a.mu.Lock()
	delete(a.sendState, n.ID)
	a.mu.Unlock()
	if auto, err := h.SetAutoApprove(context.Background(), true); err != nil || !auto.Supported || !auto.Enabled {
		t.Fatalf("enable auto approve = %#v, %v", auto, err)
	}
	if auto, err := h.SetAutoApprove(context.Background(), false); err != nil || auto.Enabled || !auto.Supported {
		t.Fatalf("disable auto approve = %#v, %v", auto, err)
	}
}

func TestClaudeSessionHarnessCurrentTransportOperations(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "idle pane"}
	a := newTestApp(t, f)
	n, _ := seedPermClaude(t, a, "n1", hookSIDOwn)
	h := testClaudeSessionHarness(a, n.ID)
	h.startLanes()

	delivery, err := h.Clear(context.Background())
	if err != nil || (delivery.Status != sessionworker.DeliveryAcknowledged && delivery.Status != sessionworker.DeliveryUnconfirmed) {
		t.Fatalf("Clear = %#v, %v", delivery, err)
	}
	if delivery.Status == sessionworker.DeliveryUnconfirmed {
		if delivery.Text != "/clear" {
			t.Fatalf("unconfirmed clear text = %q", delivery.Text)
		}
		if err := h.ResolveDelivery(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	evidence, err := h.Interrupt(context.Background())
	if err != nil || len(evidence.Keys) != 1 || evidence.Keys[0] != "Escape" || !strings.Contains(evidence.Evidence, "idle pane") {
		t.Fatalf("Interrupt = %#v, %v", evidence, err)
	}
	if got := h.Peek(context.Background(), "full"); got != "idle pane" {
		t.Fatalf("full Peek = %q", got)
	}
	if delivery, err := h.Send(context.Background(), "next turn"); err != nil || delivery.Status == "" {
		t.Fatalf("Send = %#v, %v", delivery, err)
	}
	if err := h.RecordStartFailure(context.Background(), "visible start failure"); err != nil {
		t.Fatal(err)
	}
	if err := h.RecordStartFailure(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if err := h.AppendSessionEvent(context.Background(), sessionlog.NewAttentionEdge("approval", "start")); err != nil {
		t.Fatal(err)
	}
	if events := sessionlog.ReadEvents(a.sessionLogPath(n.ID)); len(events) < 2 {
		t.Fatalf("worker-owned events = %#v", events)
	}
	if err := h.Stop(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !containsSub(f.subcommands(), "kill-session") {
		t.Fatal("chat deletion did not issue a kill for the worker-owned pane")
	}
}

func TestClaudeSessionHarnessActionsFailClosed(t *testing.T) {
	unpolled := &claudeSessionHarness{}
	if state := unpolled.State(context.Background()); state.Live != "exited" {
		t.Fatalf("unpolled state = %#v", state)
	}
	empty := testClaudeSessionHarness(newTestApp(t, &fakeTmux{}), "missing")
	if _, err := empty.Send(context.Background(), "x"); err == nil {
		t.Fatal("Send accepted a missing chat")
	}
	if _, err := empty.Clear(context.Background()); err == nil {
		t.Fatal("Clear accepted a missing chat")
	}
	if _, err := empty.Interrupt(context.Background()); err == nil {
		t.Fatal("Interrupt accepted a missing chat")
	}
	if _, err := empty.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: "stale", Key: "1"}); err == nil {
		t.Fatal("PreparePermission accepted a missing chat")
	}
	if _, err := empty.SetAutoApprove(context.Background(), true); err == nil {
		t.Fatal("SetAutoApprove accepted a missing chat")
	}
	if state := empty.State(context.Background()); state.Live != "exited" {
		t.Fatalf("missing state = %#v", state)
	}
	if peek := empty.Peek(context.Background(), "visible"); !strings.Contains(peek, "exited") {
		t.Fatalf("missing Peek = %q", peek)
	}
	if !empty.Conflict(errClaudeWorkerConflict) || empty.Conflict(errors.New("other")) {
		t.Fatal("worker conflict classification drifted")
	}

	f := &fakeTmux{list: []string{"guarded"}, alive: map[string]bool{"guarded": true}, capture: axPermissionPane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "guarded", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "guarded-dialog"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "req"), note); err != nil {
		t.Fatal(err)
	}
	epoch, err := mintClaudeVisibleEpoch(perm, hookSIDOwn)
	if err != nil {
		t.Fatal(err)
	}
	h := testClaudeSessionHarness(a, n.ID)
	if _, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: "stale", Key: "1"}); err == nil {
		t.Fatal("PreparePermission accepted a stale dialog")
	}
	if _, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch.Epoch, Key: "not-an-option"}); err == nil {
		t.Fatal("PreparePermission accepted a key outside the visible dialog")
	}
	f.captureErr = true
	if _, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch.Epoch, Key: "1"}); err == nil {
		t.Fatal("PreparePermission acted without capture evidence")
	}
	if _, err := h.Interrupt(context.Background()); err == nil {
		t.Fatal("Interrupt acted without capture evidence")
	}
	if peek := h.Peek(context.Background(), "visible"); !strings.Contains(peek, "capture failed") {
		t.Fatalf("failed Peek = %q", peek)
	}
	f.captureErr = false
	prepared, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch.Epoch, Key: "1"})
	if err != nil {
		t.Fatal(err)
	}
	f.sendKeysErr = true
	if err := h.DeliverPermission(context.Background(), prepared.Token); err == nil {
		t.Fatal("DeliverPermission hid terminal delivery failure")
	}
	if _, err := h.Send(context.Background(), "next"); err == nil {
		t.Fatal("Send hid terminal delivery failure")
	}
	if _, err := h.Clear(context.Background()); err == nil {
		t.Fatal("Clear hid terminal delivery failure")
	}
	h.answerMu.Lock()
	h.answers["stale-dialog"] = preparedClaudeAnswer{dialogID: "another-epoch", keys: []string{"1", "Enter"}}
	h.answerMu.Unlock()
	if err := h.DeliverPermission(context.Background(), "stale-dialog"); !errors.Is(err, errClaudeWorkerConflict) {
		t.Fatalf("stale prepared dialog = %v", err)
	}
}

func TestClaudeWorkerDeliveryTextOnlyRestoresUnconfirmedDraft(t *testing.T) {
	for _, tc := range []struct {
		status initialDelivery
		want   string
	}{{initialAcknowledged, ""}, {initialPending, ""}, {initialNotSent, ""}, {initialUnconfirmed, "draft"}} {
		if got := deliveryText(tc.status, "draft"); got != tc.want {
			t.Errorf("deliveryText(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestClaudeSessionHarnessRejectsIncompleteConstructionAndLaunch(t *testing.T) {
	if _, err := newClaudeSessionHarness(sessionWorkerConfig{DataDir: t.TempDir(), NodeID: "../escape"}); err == nil {
		t.Fatal("constructor accepted unsafe incomplete configuration")
	}
	a := newTestApp(t, &fakeTmux{})
	h := testClaudeSessionHarness(a, "n1")
	if _, err := h.Launch(context.Background(), sessionworker.LaunchRequest{NodeID: "other", Agent: "claude"}); err == nil {
		t.Fatal("launch accepted another node")
	}
	if err := h.ResolveDelivery(context.Background()); err != nil {
		t.Fatalf("idle ResolveDelivery = %v", err)
	}
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "control"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newClaudeSessionHarness(sessionWorkerConfig{
		DataDir: data, Home: t.TempDir(), Socket: "worker-test", NodeID: "blocked",
		Identity: sessionworker.Identity{WorkerID: "worker-blocked", Agent: "claude"},
	}); err == nil {
		t.Fatal("constructor accepted a non-directory control path")
	}

	failing := newTestApp(t, &fakeTmux{})
	failing.server = tmuxsession.NewServerWithRunner("worker-test", func(context.Context, string, ...string) (string, error) {
		return "", errors.New("tmux unavailable")
	})
	failedLaunch := testClaudeSessionHarness(failing, "launch-failure")
	if _, err := failedLaunch.Launch(context.Background(), sessionworker.LaunchRequest{
		NodeID: "launch-failure", Agent: "claude", Title: "Failure", Prompt: "hello", Dir: failing.home,
	}); err == nil {
		t.Fatal("launch hid tmux failure")
	}
}

func TestClaudeSessionHarnessLaunchesNewPaneWithRecoverableMetadata(t *testing.T) {
	f := &fakeTmux{capture: "starting"}
	a := newTestApp(t, f)
	a.claudeReadyTimeout = 0 // the delivery lane terminates without a hook event
	h := testClaudeSessionHarness(a, "fresh-worker")
	request := sessionworker.LaunchRequest{
		NodeID: "fresh-worker", Agent: "claude", Parent: "parent", Title: "Fresh", Prompt: "hello",
		Rationale: "fork reason", LaneID: "lane", ForkKind: "y-stay", Dir: a.home, Model: "sonnet", Effort: "low",
	}
	sid, err := h.Launch(context.Background(), request)
	if err != nil || sid == "" {
		t.Fatalf("Launch = %q, %v", sid, err)
	}
	if !containsSub(f.subcommands(), "new-session") {
		t.Fatal("ordinary Claude worker did not launch a tmux pane")
	}
	state := h.State(context.Background())
	if state.Launch == nil || state.Launch.SessionID != sid || state.Launch.Description != request.Prompt ||
		state.Launch.Transport != "tmux" || state.Launch.Parent != request.Parent || state.Launch.Rationale != request.Rationale ||
		state.HookID == "" || state.HookGeneration != 1 {
		t.Fatalf("new worker recovery state = %#v", state)
	}
	if again, err := h.Launch(context.Background(), request); err != nil || again != sid {
		t.Fatalf("idempotent Launch = %q, %v; want %q", again, err, sid)
	}
	if err := h.Stop(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeSessionHarnessRejectsExternalPaneWithoutRelaunch(t *testing.T) {
	f := &fakeTmux{list: []string{"existing"}, alive: map[string]bool{"existing": true}, capture: "idle"}
	a := newTestApp(t, f)
	h := testClaudeSessionHarness(a, "existing")
	request := sessionworker.LaunchRequest{
		NodeID: "existing", Agent: "claude", Parent: "parent", Title: "Existing", Prompt: "history",
		Description: "kept description", Rationale: "fork reason", LaneID: "lane", ForkKind: "y-stay",
		Dir: a.home, SessionID: hookSIDOwn, Existing: true, Adopted: true,
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Transcript: "/tmp/existing.jsonl",
		HookID: "hook-existing", HookGeneration: 4,
	}
	if sid, err := h.Launch(context.Background(), request); err == nil || sid != "" {
		t.Fatalf("external Launch = %q, %v, want rejection", sid, err)
	}
	if containsSub(f.subcommands(), "new-session") {
		t.Fatal("rejecting an external pane relaunched it")
	}
	if !f.alive["existing"] {
		t.Fatal("rejecting an external pane killed it")
	}
}

func TestClaudeSessionHarnessOwnedRecoveryRequiresLivePaneAndDurableHook(t *testing.T) {
	request := sessionworker.LaunchRequest{
		NodeID: "existing", Agent: "claude", Title: "Existing", Prompt: "history",
		Dir: t.TempDir(), SessionID: hookSIDOwn, Existing: true, HookID: "hook-existing",
	}
	dead := testClaudeSessionHarness(newTestApp(t, &fakeTmux{}), request.NodeID)
	if _, err := dead.Launch(context.Background(), request); !errors.Is(err, errClaudeWorkerConflict) {
		t.Fatalf("dead-pane recovery = %v", err)
	}

	f := &fakeTmux{list: []string{request.NodeID}, alive: map[string]bool{request.NodeID: true}}
	a := newTestApp(t, f)
	a.storePath = t.TempDir()
	brokenStore := testClaudeSessionHarness(a, request.NodeID)
	if _, err := brokenStore.Launch(context.Background(), request); err == nil {
		t.Fatal("recovery succeeded without durable node publication")
	}

	a = newTestApp(t, f)
	h := testClaudeSessionHarness(a, request.NodeID)
	if _, err := h.Launch(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := h.State(context.Background()); got.HookID != request.HookID || got.HookGeneration != 1 {
		t.Fatalf("zero-generation recovery = %#v", got)
	}
	if err := h.Stop(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeSessionHarnessShutdownRetiresOnlyWorker(t *testing.T) {
	f := &fakeTmux{list: []string{"owned"}, alive: map[string]bool{"owned": true}}
	a := newTestApp(t, f)
	n := seedOwnedClaude(t, a, "owned", hookSIDOwn, "")
	h := testClaudeSessionHarness(a, n.ID)
	h.startLanes()
	hookID := a.claudeHookID(n.ID)

	if err := h.Stop(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !f.alive[n.ID] {
		t.Fatal("whole-program shutdown killed an owned Claude tmux pane")
	}
	if a.byID[n.ID] == nil || a.claudeHookID(n.ID) != hookID {
		t.Fatal("whole-program shutdown deleted re-adoption state")
	}
}
