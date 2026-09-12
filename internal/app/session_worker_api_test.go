package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/sessionworker"
)

func attachSyntheticWorker(t *testing.T, a *app, nodeID string, harness *syntheticSessionHarness) {
	t.Helper()
	identity := sessionworker.Identity{WorkerID: "worker-" + nodeID, Agent: "claude", Build: "test"}
	server, err := sessionworker.Listen("", identity, harness)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client, err := sessionworker.NewClient(server.Link())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	a.workers = &workerManager{entries: map[string]*workerEntry{
		nodeID: {client: client, identity: identity},
	}, dataDir: t.TempDir()}
}

// One HTTP test crosses every Claude-specific worker branch. Harness policy
// remains contained by the existing Claude suites; this test defends only the
// public projection and dispatch across the new process boundary.
func TestClaudeWorkerPublicConversationContract(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedTmuxNode(a, "worker-chat")
	n.Prompt, n.Model = "first prompt", "sonnet"
	harness := &syntheticSessionHarness{launched: true, state: sessionworker.State{
		HasSession: true, SessionID: "session-worker", Live: "active", Attention: "approval",
		Transcript: "/worker/transcript.jsonl", AXScreenReader: true, TurnDone: true,
		Delivery: sendUnconfirmed, Supervision: string(claudeSupStrict), Pending: true,
		Fallback: true, Source: "transcript", Reason: "tool", Watermark: 7, Progress: 2,
		PendingCalls: 1, WaitingOn: "permission", ReplyReady: true, TurnInFlight: true,
		Compacting: true, CompactTrigger: "auto", AutoApprove: sessionworker.AutoApprove{Supported: true, Phase: "off"},
		ElicitationCount: 1, Elicitations: []sessionworker.Elicitation{{Server: "docs", Message: "choose"}},
		PermissionBoundary: &sessionworker.PermissionBoundary{Incarnation: "synthetic", MaxSequence: 0},
		Permission: &sessionworker.PendingPermission{
			RequestID: "dialog-1", Title: "run tests", ToolKind: "Bash", Reason: "needs access", Dialog: true,
			Options: []sessionworker.PermissionOption{{Key: "1", Name: "Allow once", Kind: "allow"}},
		},
	}}
	attachSyntheticWorker(t, a, n.ID, harness)
	handler := newTestHandler(t, a)

	chat := routeRequest(handler, http.MethodGet, "/api/nodes/worker-chat/chat", "", false)
	if chat.Code != http.StatusOK {
		t.Fatalf("chat = %d: %s", chat.Code, chat.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(chat.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"live": "active", "attention": "approval", "delivery": sendUnconfirmed,
		"supervision": string(claudeSupStrict), "perm_dialog_id": "dialog-1",
		"perm_title": "run tests", "pending_prompt": "first prompt", "compacting": true,
		"elicitation_waiting": true,
	} {
		if got := body[key]; got != want {
			t.Errorf("chat[%q] = %#v, want %#v", key, got, want)
		}
	}

	state := routeRequest(handler, http.MethodGet, "/api/state", "", false)
	if state.Code != http.StatusOK || !strings.Contains(state.Body.String(), "session-worker") || !strings.Contains(state.Body.String(), "/worker/transcript.jsonl") {
		t.Fatalf("state = %d: %s", state.Code, state.Body.String())
	}

	peek := routeRequest(handler, http.MethodGet, "/api/nodes/worker-chat/peek?mode=visible", "", false)
	if peek.Code != http.StatusOK || peek.Body.String() != "synthetic event log" {
		t.Fatalf("peek = %d %q", peek.Code, peek.Body.String())
	}
	harness.mu.Lock()
	peekMode := harness.peekMode
	harness.mu.Unlock()
	if peekMode != "visible" {
		t.Fatalf("peek mode = %q", peekMode)
	}

	if rec := routeRequest(handler, http.MethodPost, "/api/nodes/worker-chat/send/resolve", "", true); rec.Code != http.StatusOK {
		t.Fatalf("resolve = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := routeRequest(handler, http.MethodPost, "/api/nodes/worker-chat/send", `{"text":"next"}`, true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "acknowledged") {
		t.Fatalf("send = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := routeRequest(handler, http.MethodPost, "/api/nodes/worker-chat/key", `{"key":"1","dialog_id":"synthetic:1"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("key = %d: %s", rec.Code, rec.Body.String())
	}
	if records := keyRecords(t, a.storePath); len(records) == 0 || records[len(records)-1].Type != "key" || len(records[len(records)-1].Keys) != 1 {
		t.Fatalf("permission audit records = %#v", records)
	}
	if rec := routeRequest(handler, http.MethodPost, "/api/nodes/worker-chat/send", `{"text":"/clear"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := routeRequest(handler, http.MethodPost, "/api/nodes/worker-chat/send/interrupt", "", true); rec.Code != http.StatusOK {
		t.Fatalf("interrupt = %d: %s", rec.Code, rec.Body.String())
	}
	if records := keyRecords(t, a.storePath); len(records) == 0 || records[len(records)-1].Key != "Escape" || len(records[len(records)-1].Keys) != 1 {
		t.Fatalf("interrupt audit records = %#v", records)
	}
	if rec := routeRequest(handler, http.MethodPost, "/api/nodes/worker-chat/auto-approve", `{"enabled":true}`, true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "armed") {
		t.Fatalf("auto approve = %d: %s", rec.Code, rec.Body.String())
	}

	// The worker is a client of the same append-only store; no shadow chat
	// payload is introduced by the RPC contract.
	if _, err := os.Stat(a.sessionLogPath(n.ID)); err == nil {
		t.Fatal("read-only worker projections unexpectedly created a session log")
	}
}

func TestMuxerPollDoesNotDuplicateClaudeWorkerAttentionAudit(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedTmuxNode(a, "worker-attention")
	harness := &syntheticSessionHarness{launched: true, state: sessionworker.State{
		HasSession: true, Live: "quiet", Attention: "approval", Supervision: string(claudeSupStrict),
	}}
	attachSyntheticWorker(t, a, n.ID, harness)

	a.poll()
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if len(harness.events) != 0 {
		t.Fatalf("muxer duplicated worker-owned attention events: %#v", harness.events)
	}
}

func TestClaudeWorkerIsSoleSessionEventWriter(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedTmuxNode(a, "worker-writer")
	harness := &syntheticSessionHarness{launched: true}
	attachSyntheticWorker(t, a, n.ID, harness)
	event := sessionlog.NewAttentionEdge("approval", "start")

	if err := a.appendSessionEvent(n.ID, event); err != nil {
		t.Fatal(err)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if len(harness.events) != 1 {
		t.Fatalf("worker events = %#v", harness.events)
	}
	if _, err := os.Stat(a.sessionLogPath(n.ID)); !os.IsNotExist(err) {
		t.Fatalf("muxer wrote a shadow session log: %v", err)
	}
}

func TestPollProjectsChangedClaudeWorkerHookIdentity(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "worker-hook-rotation", hookSIDOwn, "")
	a.mu.Lock()
	a.claudeHooks[n.ID], a.claudeGens[n.ID] = "hook-old", 1
	a.mu.Unlock()
	attachSyntheticWorker(t, a, n.ID, &syntheticSessionHarness{launched: true, state: sessionworker.State{
		HasSession: true, Live: "quiet", HookID: "hook-next", HookGeneration: 2,
	}})

	a.poll()
	if got, generation := a.claudeHookID(n.ID), a.claudeGeneration(n.ID); got != "hook-next" || generation != 2 {
		t.Fatalf("projected hook = %q generation %d", got, generation)
	}
	records := keyRecords(t, a.storePath)
	last := records[len(records)-1]
	if last.Type != "claude-hook" || last.ID != n.ID || last.HookID != "hook-next" || last.Generation != 2 {
		t.Fatalf("last durable record = %#v", last)
	}
}

func TestWorkerClaudeHookProjectionIsMonotonicAndFailsClosed(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.projectWorkerClaudeHook("node", sessionworker.State{HookID: "../unsafe", HookGeneration: 2})
	if got := keyRecords(t, a.storePath); len(got) != 0 {
		t.Fatalf("unsafe hook was persisted: %#v", got)
	}

	a.projectWorkerClaudeHook("node", sessionworker.State{HookID: "hook-zero", HookGeneration: 0})
	if got, generation := a.claudeHookID("node"), a.claudeGeneration("node"); got != "hook-zero" || generation != 1 {
		t.Fatalf("normalized hook = %q generation %d", got, generation)
	}
	a.projectWorkerClaudeHook("node", sessionworker.State{HookID: "hook-zero", HookGeneration: 1})
	a.projectWorkerClaudeHook("node", sessionworker.State{HookID: "hook-stale", HookGeneration: 0})
	if got := keyRecords(t, a.storePath); len(got) != 1 {
		t.Fatalf("idempotent or stale projection appended records: %#v", got)
	}

	a.storePath = t.TempDir()
	a.projectWorkerClaudeHook("node", sessionworker.State{HookID: "hook-new", HookGeneration: 2})
	if got, generation := a.claudeHookID("node"), a.claudeGeneration("node"); got != "hook-zero" || generation != 1 {
		t.Fatalf("failed persistence changed hook = %q generation %d", got, generation)
	}
}

func TestClaudeWorkerAndMonolithPublicReadModelsMatchExactly(t *testing.T) {
	f := &fakeTmux{list: []string{"parity"}, alive: map[string]bool{"parity": true}, capture: axPermissionPane}
	a := newTestApp(t, f)
	n, _ := seedPermClaude(t, a, "parity", hookSIDOwn)
	n.AXScreenReader = true
	controller := testClaudeSessionHarness(a, n.ID)
	workerState := controller.State(t.Context())
	handler := newTestHandler(t, a)

	legacyChat := routeRequest(handler, http.MethodGet, "/api/nodes/parity/chat", "", false)
	legacyState := routeRequest(handler, http.MethodGet, "/api/state", "", false)
	attachSyntheticWorker(t, a, n.ID, &syntheticSessionHarness{launched: true, state: workerState})
	workerChat := routeRequest(handler, http.MethodGet, "/api/nodes/parity/chat", "", false)
	workerStateHTTP := routeRequest(handler, http.MethodGet, "/api/state", "", false)

	for name, pair := range map[string][2][]byte{
		"chat":  {legacyChat.Body.Bytes(), workerChat.Body.Bytes()},
		"state": {legacyState.Body.Bytes(), workerStateHTTP.Body.Bytes()},
	} {
		oldBody, newBody := canonicalJSON(t, pair[0]), canonicalJSON(t, pair[1])
		if !bytes.Equal(oldBody, newBody) {
			t.Fatalf("%s read model differs\nmonolith=%s\nworker=%s", name, oldBody, newBody)
		}
	}
}

func canonicalJSON(t *testing.T, body []byte) []byte {
	t.Helper()
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
