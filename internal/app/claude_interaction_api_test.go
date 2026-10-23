package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/sessionworker"
)

func postInteraction(h http.Handler, id, body string, csrf bool) *httptest.ResponseRecorder {
	return routeRequest(h, http.MethodPost, "/api/nodes/"+id+"/claude-interaction", body, csrf)
}

func publishSyntheticInteraction(t *testing.T, bundle, requestID, tool string, raw []byte, sessionID string) {
	t.Helper()
	model, err := classifyClaudeInteraction(tool, raw)
	if err != nil || model.Native {
		t.Fatalf("classify %s: native=%v err=%v", tool, model.Native, err)
	}
	ev := claudePermissionEvent{SessionID: sessionID, ToolName: tool, PromptID: "prompt-synthetic", PermissionMode: "plan"}
	if err := publishInteractionRequest(bundle, ev, requestID, model, time.Now().UTC(), interactionHelperDeadline); err != nil {
		t.Fatal(err)
	}
}

func attachInteractionWorker(t *testing.T, a *app, n *Node) *claudeSessionHarness {
	t.Helper()
	h := testClaudeSessionHarness(a, n.ID)
	identity := sessionworker.Identity{WorkerID: "worker-" + n.ID, Agent: "claude", Build: "test"}
	dir, err := os.MkdirTemp("/tmp", "sciw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	server, err := sessionworker.Listen(dir, identity, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client, err := sessionworker.NewClient(server.Link())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	hello, err := client.Hello(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	caps := map[string]bool{}
	for _, capability := range hello.Capabilities {
		caps[capability] = true
	}
	if a.workers == nil {
		a.workers = &workerManager{entries: map[string]*workerEntry{}, dataDir: t.TempDir()}
	}
	a.workers.entries[n.ID] = &workerEntry{client: client, identity: identity, capabilities: caps}
	return h
}

func TestClaudeInteractionAPIPublicContract(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: axPermissionPane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	n.AXScreenReader = true
	publishSyntheticInteraction(t, bundle, "req-letter", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	before, err := os.ReadFile(filepath.Join(bundle, "perm", "interact", "req", "req-letter.json"))
	if err != nil {
		t.Fatal(err)
	}
	attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)

	if rec := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"answer","answers":[{"question_id":"q1","option_ids":["q1o1"]}]}`, false); rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF = %d %s", rec.Code, rec.Body.String())
	}
	origin := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/nodes/"+n.ID+"/claude-interaction", strings.NewReader(`{"request_id":"req-letter","action":"cancel"}`))
	origin.Host = "evil.example"
	origin.Header.Set("Content-Type", "application/json")
	origin.Header.Set("X-Scimux-CSRF", csrfToken)
	origin.Header.Set("Origin", "http://evil.example")
	originRec := httptest.NewRecorder()
	handler.ServeHTTP(originRec, origin)
	if originRec.Code != http.StatusForbidden {
		t.Fatalf("hostile host/origin = %d %s", originRec.Code, originRec.Body.String())
	}
	if rec := routeRequest(handler, http.MethodGet, "/api/nodes/"+n.ID+"/claude-interaction", "", false); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	if rec := postInteraction(handler, "missing", `{"request_id":"req-letter","action":"cancel"}`, true); rec.Code != http.StatusNotFound || strings.TrimSpace(rec.Body.String()) != "not found" {
		t.Fatalf("unknown node = %d %q", rec.Code, rec.Body.String())
	}
	n.EndedAt = "2026-10-21T00:00:00Z"
	if rec := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"cancel"}`, true); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "closed") {
		t.Fatalf("ended = %d %s", rec.Code, rec.Body.String())
	}
	n.EndedAt = ""
	foreign := seedTmuxNode(a, "codex-1")
	foreign.Agent = "codex"
	if _, ok := a.claudeInteractionView(foreign); ok {
		t.Fatal("non-claude node projected an interaction")
	}
	if rec := postInteraction(handler, foreign.ID, `{"request_id":"req-letter","action":"cancel"}`, true); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unsupported") {
		t.Fatalf("non-claude = %d %s", rec.Code, rec.Body.String())
	}
	plain := seedTmuxNode(a, "plain-claude")
	if _, ok := a.claudeInteractionView(plain); ok {
		t.Fatal("claude node without a hook bundle projected an interaction")
	}
	n.Adopted = true
	if rec := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"cancel"}`, true); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "retired") {
		t.Fatalf("adopted = %d %s", rec.Code, rec.Body.String())
	}
	n.Adopted = false
	saved := a.workers.entries[n.ID]
	delete(a.workers.entries, n.ID)
	if rec := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"cancel"}`, true); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unsupported") {
		t.Fatalf("unmanaged = %d %s", rec.Code, rec.Body.String())
	}
	a.workers.entries[n.ID] = saved
	delete(a.workers.entries[n.ID].capabilities, sessionworker.CapabilityClaudeInteractionV1)
	if rec := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"cancel"}`, true); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unsupported") {
		t.Fatalf("capability off = %d %s", rec.Code, rec.Body.String())
	}
	a.workers.entries[n.ID].capabilities[sessionworker.CapabilityClaudeInteractionV1] = true

	oversized := `{"request_id":"req-letter","action":"discuss","feedback":"` + strings.Repeat("x", interactionPostMax) + `"}`
	if rec := postInteraction(handler, n.ID, oversized, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("over 32KiB = %d %s", rec.Code, rec.Body.String())
	}
	hugeSend := `{"text":"` + strings.Repeat("y", 1<<20) + `"}`
	if rec := routeRequest(handler, http.MethodPost, "/api/nodes/"+n.ID+"/send", hugeSend, true); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("send ceiling = %d, want 413", rec.Code)
	}
	for _, body := range []string{
		`{`,
		`{"request_id":"req-letter"}`,
		`{"request_id":"req-letter","action":"nope"}`,
		`{"request_id":"","action":"cancel"}`,
		`{"request_id":" req-letter","action":"cancel"}`,
		`{"request_id":"req-letter","action":"cancel"}{}`,
		`{"request_id":"req-letter","action":"answer","answers":[{"question_id":"q1","option_ids":["missing"]}]}`,
		`{"request_id":"req-letter","action":"answer","updatedInput":{"questions":[]}}`,
		`{"request_id":"req-letter","action":"approve","mode":"bypassPermissions"}`,
		`{"request_id":"req-letter","action":"discuss","feedback":"  "}`,
		`{"request_id":"req-letter","action":"answer","answers":[{"question_id":"q1","option_ids":["q1o1"]}],"updatedPermissions":[{"type":"setMode"}]}`,
	} {
		if rec := postInteraction(handler, n.ID, body, true); rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	afterReject, err := os.ReadFile(filepath.Join(bundle, "perm", "interact", "req", "req-letter.json"))
	if err != nil || !bytes.Equal(before, afterReject) {
		t.Fatal("rejected bodies mutated the stored request")
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "ans", "req-letter.json")); !os.IsNotExist(err) {
		t.Fatal("rejected body wrote an answer")
	}

	first := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"answer","answers":[{"question_id":"q1","option_ids":["q1o1"]}]}`, true)
	if first.Code != http.StatusAccepted {
		t.Fatalf("answer = %d %s", first.Code, first.Body.String())
	}
	var accepted map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted["status"] != "accepted" || accepted["delivery"] != "recorded" || accepted["emitted"] != false || accepted["delivered"] == true {
		t.Fatalf("receipt = %#v", accepted)
	}
	if strings.Contains(first.Body.String(), `"delivered":true`) {
		t.Fatalf("queued reply claimed delivery: %s", first.Body.String())
	}
	ans, err := os.ReadFile(filepath.Join(bundle, "perm", "interact", "ans", "req-letter.json"))
	if err != nil || !bytes.Contains(ans, []byte(`"answers"`)) || bytes.Contains(ans, []byte("updatedPermissions")) {
		t.Fatalf("answer file = %s %v", ans, err)
	}
	stored, err := os.ReadFile(filepath.Join(bundle, "perm", "interact", "req", "req-letter.json"))
	if err != nil || !bytes.Equal(stored, before) {
		t.Fatal("accepted answer rewrote the original request")
	}
	second := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"answer","answers":[{"question_id":"q1","option_ids":["q1o2"]}]}`, true)
	if second.Code != http.StatusConflict {
		t.Fatalf("duplicate = %d %s", second.Code, second.Body.String())
	}
	logRaw, err := os.ReadFile(a.sessionLogPath(n.ID))
	if err != nil || !strings.Contains(string(logRaw), `"source":"manual"`) || strings.Contains(string(logRaw), "Auto-approved") {
		t.Fatalf("manual audit = %s %v", logRaw, err)
	}
	events := sessionlog.ReadEvents(a.sessionLogPath(n.ID))
	var manual *sessionlog.DecisionEvent
	for i := range events {
		if events[i].Decision != nil && events[i].Decision.Source == "manual" {
			manual = events[i].Decision
		}
	}
	if manual == nil || manual.Manual == nil || manual.Manual.Feedback != "" || len(manual.Manual.Selections) != 1 || manual.Manual.Selections[0].Kind != "option" {
		t.Fatalf("manual event = %+v", manual)
	}

	stale := postInteraction(handler, n.ID, `{"request_id":"req-other","action":"cancel"}`, true)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale id = %d %s", stale.Code, stale.Body.String())
	}
}

func TestClaudeInteractionAPIPlanModesAndNativeHandoff(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	f := &fakeTmux{list: []string{"plan-1"}, alive: map[string]bool{"plan-1": true}, capture: axPermissionPane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "plan-1", hookSIDOwn)
	n.AXScreenReader = true
	perm := filepath.Join(bundle, "perm")
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: n.SessionID, Tool: "Bash", Digest: "worker-dialog"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "req-dialog"), note); err != nil {
		t.Fatal(err)
	}
	epoch, err := mintClaudeVisibleEpoch(perm, n.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(filepath.Join(perm, "generation.json"), map[string]any{"generation": 3, "node_id": n.ID}); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(filepath.Join(perm, "turn.json"), map[string]any{"turn": "turn-synth", "gen": 2}); err != nil {
		t.Fatal(err)
	}
	publishSyntheticInteraction(t, bundle, "req-plan", "ExitPlanMode", tinyPlanRaw(), n.SessionID)
	h := attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)

	chat := routeRequest(handler, http.MethodGet, "/api/nodes/"+n.ID+"/chat", "", false)
	var body map[string]any
	if err := json.Unmarshal(chat.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	interaction, _ := body["claude_interaction"].(map[string]any)
	if interaction["request_id"] != "req-plan" || interaction["tool"] != "ExitPlanMode" || interaction["native_keys"] != false {
		t.Fatalf("snapshot = %#v", body["claude_interaction"])
	}
	if strings.Contains(chat.Body.String(), "planFilePath") || strings.Contains(chat.Body.String(), "/plans/not-read.md") {
		t.Fatalf("snapshot leaked a plan path: %s", chat.Body.String())
	}
	if body["perm_dialog_id"] != epoch.Epoch {
		t.Fatalf("perm_dialog_id = %#v, want the visible epoch %s", body["perm_dialog_id"], epoch.Epoch)
	}
	direct, err := h.ClaudeInteraction(context.Background())
	if err != nil || direct.RequestID != "req-plan" || direct.Plan == "" || direct.NativeKeys {
		t.Fatalf("worker view = %#v %v", direct, err)
	}
	etag := chat.Header().Get("ETag")
	again := routeRequest(handler, http.MethodGet, "/api/nodes/"+n.ID+"/chat", "", false)
	if again.Header().Get("ETag") != etag || etag == "" {
		t.Fatal("interaction snapshot was not a stable ETag")
	}
	history := routeRequest(handler, http.MethodGet, "/api/nodes/"+n.ID+"/chat?history=1", "", false)
	if history.Code != http.StatusOK || strings.Contains(history.Body.String(), "claude_interaction") || !strings.Contains(history.Body.String(), "segments") {
		t.Fatalf("history = %d %s", history.Code, history.Body.String())
	}

	key := routeRequest(handler, http.MethodPost, "/api/nodes/"+n.ID+"/key", `{"key":"1","dialog_id":"`+epoch.Epoch+`"}`, true)
	if key.Code != http.StatusConflict || containsSub(f.subcommands(), "send-keys") {
		t.Fatalf("native key while owned = %d sends=%v %s", key.Code, f.subcommands(), key.Body.String())
	}
	if _, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch.Epoch, Key: "1"}); err == nil {
		t.Fatal("worker prepare accepted a key the structured interaction owns")
	}
	if err := h.DeliverPermission(context.Background(), "not-a-token"); err == nil || containsSub(f.subcommands(), "send-keys") {
		t.Fatal("worker deliver sent a key the structured interaction owns")
	}

	approved := postInteraction(handler, n.ID, `{"request_id":"req-plan","action":"approve","mode":"acceptEdits"}`, true)
	if approved.Code != http.StatusAccepted {
		t.Fatalf("approve = %d %s", approved.Code, approved.Body.String())
	}
	ans, err := os.ReadFile(filepath.Join(bundle, "perm", "interact", "ans", "req-plan.json"))
	if err != nil || bytes.Contains(ans, []byte("bypassPermissions")) || bytes.Count(ans, []byte("setMode")) != 1 || !bytes.Contains(ans, []byte(`"acceptEdits"`)) {
		t.Fatalf("plan answer = %s %v", ans, err)
	}
	if bytes.Contains(ans, []byte("userSettings")) || bytes.Contains(ans, []byte("allow_always")) {
		t.Fatalf("plan answer persisted a rule: %s", ans)
	}
}

func TestClaudeInteractionPollAttentionAndMonolithKey(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	f := &fakeTmux{list: []string{"attn-1"}, alive: map[string]bool{"attn-1": true}, capture: axPermissionPane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "attn-1", hookSIDOwn)
	n.AXScreenReader = true
	perm := filepath.Join(bundle, "perm")
	if err := writeClaudePermFile(claudeAskedPath(perm, "ask"), claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: n.SessionID, Tool: "Bash", Digest: "attn"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := mintClaudeVisibleEpoch(perm, n.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	publishSyntheticInteraction(t, bundle, "req-letter", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	if sup := a.claudeSupervisionOf(n); sup != claudeSupStrict {
		t.Fatalf("supervision = %s", sup)
	}
	a.poll()
	a.mu.Lock()
	attn, live := a.attn[n.ID], a.live[n.ID]
	a.mu.Unlock()
	if attn != "question" || live == "" {
		t.Fatalf("attention = %q live = %q", attn, live)
	}
	handler := newTestHandler(t, a)
	if rec := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"cancel"}`, true); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unsupported") {
		t.Fatalf("monolith without a worker = %d %s", rec.Code, rec.Body.String())
	}
	key := routeRequest(handler, http.MethodPost, "/api/nodes/"+n.ID+"/key", `{"key":"1","dialog_id":"`+epoch.Epoch+`"}`, true)
	if key.Code != http.StatusConflict || containsSub(f.subcommands(), "send-keys") {
		t.Fatalf("monolith key = %d sends=%v %s", key.Code, f.subcommands(), key.Body.String())
	}
	chat := routeRequest(handler, http.MethodGet, "/api/nodes/"+n.ID+"/chat", "", false)
	if !strings.Contains(chat.Body.String(), `"request_id":"req-letter"`) || strings.Contains(chat.Body.String(), "planFilePath") {
		t.Fatalf("monolith chat = %s", chat.Body.String())
	}
}

func TestClaudeInteractionAPIGateOffAndSnapshot(t *testing.T) {
	f := &fakeTmux{list: []string{"off-1"}, alive: map[string]bool{"off-1": true}, capture: "idle\n"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "off-1", hookSIDOwn)
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact")); !os.IsNotExist(err) {
		t.Fatal("gate-off bundle created an interaction directory")
	}
	h := attachInteractionWorker(t, a, n)
	if _, err := h.ClaudeInteraction(context.Background()); !errors.Is(err, sessionworker.ErrInteractionUnsupported) {
		t.Fatalf("gate-off view = %v", err)
	}
	if _, err := h.SubmitClaudeInteraction(context.Background(), sessionworker.InteractionAction{RequestID: "req-letter", Action: "cancel"}); !errors.Is(err, sessionworker.ErrInteractionUnsupported) {
		t.Fatalf("gate-off submit = %v", err)
	}
	handler := newTestHandler(t, a)
	etag1 := routeRequest(handler, http.MethodGet, "/api/nodes/"+n.ID+"/chat", "", false).Header().Get("ETag")
	if rec := postInteraction(handler, n.ID, `{"request_id":"req-letter","action":"cancel"}`, true); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unsupported") {
		t.Fatalf("gate off = %d %s", rec.Code, rec.Body.String())
	}
	etag2 := routeRequest(handler, http.MethodGet, "/api/nodes/"+n.ID+"/chat", "", false).Header().Get("ETag")
	if etag1 == "" || etag1 != etag2 {
		t.Fatalf("gate-off chat ETag changed: %q %q", etag1, etag2)
	}
	a.poll()
	a.mu.Lock()
	attn := a.attn[n.ID]
	live := a.live[n.ID]
	a.mu.Unlock()
	if attn == "question" {
		t.Fatalf("gate-off poll raised interaction attention, live=%s", live)
	}
	a.mu.Lock()
	delete(a.byID, n.ID)
	a.mu.Unlock()
	if _, err := h.ClaudeInteraction(context.Background()); !errors.Is(err, sessionworker.ErrInteractionUnsupported) {
		t.Fatalf("missing node view = %v", err)
	}
	if _, err := h.SubmitClaudeInteraction(context.Background(), sessionworker.InteractionAction{RequestID: "req-letter", Action: "cancel"}); !errors.Is(err, errClaudeWorkerConflict) {
		t.Fatalf("missing node submit = %v", err)
	}
}

func TestClaudeInteractionViewRejectsUnownedAndUnsafePaths(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "view-1", hookSIDOwn)
	if _, ok := a.claudeInteractionView(n); ok {
		t.Fatal("capable bundle without an active request projected a view")
	}
	active := filepath.Join(bundle, "perm", "interact", "active.json")
	if err := os.WriteFile(active, []byte(`{"id":"req-missing"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.claudeInteractionView(n); ok {
		t.Fatal("active id without a request projected a view")
	}
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "req", "req-missing.json"), claudeInteractionRequest{
		ID: "req-missing", Tool: "Bash", Original: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.claudeInteractionView(n); ok {
		t.Fatal("native tool projected a structured view")
	}
	a.mu.Lock()
	a.claudeHooks[n.ID] = ".."
	a.mu.Unlock()
	if got := a.claudeHookBundle(n); got != "" {
		t.Fatalf("unsafe hook id bundle = %q", got)
	}
	a.mu.Lock()
	a.claudeHooks[n.ID] = "hook-ok"
	a.mu.Unlock()
	a.storePath = ""
	a.claudeHooksRoot = ""
	if got := a.claudeHookBundle(n); got != "" {
		t.Fatalf("empty hook root bundle = %q", got)
	}
}

func TestClaudeInteractionTerminalHandoffReenablesNativeKey(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	f := &fakeTmux{list: []string{"hand-1"}, alive: map[string]bool{"hand-1": true}, capture: axPermissionPane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "hand-1", hookSIDOwn)
	n.AXScreenReader = true
	perm := filepath.Join(bundle, "perm")
	if err := writeClaudePermFile(claudeAskedPath(perm, "ask"), claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: n.SessionID, Tool: "Bash", Digest: "handoff"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := mintClaudeVisibleEpoch(perm, n.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	publishSyntheticInteraction(t, bundle, "req-hand", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	h := attachInteractionWorker(t, a, n)
	handler := newTestHandler(t, a)

	blocked := routeRequest(handler, http.MethodPost, "/api/nodes/"+n.ID+"/key", `{"key":"1","dialog_id":"`+epoch.Epoch+`"}`, true)
	if blocked.Code != http.StatusConflict || containsSub(f.subcommands(), "send-keys") {
		t.Fatalf("key before handoff = %d sends=%v %s", blocked.Code, f.subcommands(), blocked.Body.String())
	}
	terminal := postInteraction(handler, n.ID, `{"request_id":"req-hand","action":"terminal"}`, true)
	if terminal.Code != http.StatusAccepted {
		t.Fatalf("terminal = %d %s", terminal.Code, terminal.Body.String())
	}
	ans, err := os.ReadFile(filepath.Join(bundle, "perm", "interact", "ans", "req-hand.json"))
	if err != nil || string(ans) != `{"terminal":true}` {
		t.Fatalf("terminal answer = %s %v", ans, err)
	}
	view, err := h.ClaudeInteraction(context.Background())
	if err != nil || view.Phase != "handoff" || !view.NativeKeys {
		t.Fatalf("handoff view = %#v %v", view, err)
	}
	stale := postInteraction(handler, n.ID, `{"request_id":"req-hand","action":"cancel"}`, true)
	if stale.Code != http.StatusConflict {
		t.Fatalf("reply after handoff = %d %s", stale.Code, stale.Body.String())
	}
	key := routeRequest(handler, http.MethodPost, "/api/nodes/"+n.ID+"/key", `{"key":"1","dialog_id":"`+epoch.Epoch+`"}`, true)
	if key.Code != http.StatusOK || !containsSub(f.subcommands(), "send-keys") {
		t.Fatalf("native key after handoff = %d sends=%v %s", key.Code, f.subcommands(), key.Body.String())
	}
}

func TestClaudeInteractionAPIAuditFailureEmitsNothing(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	f := &fakeTmux{list: []string{"audit-1"}, alive: map[string]bool{"audit-1": true}, capture: "idle\n"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "audit-1", hookSIDOwn)
	publishSyntheticInteraction(t, bundle, "req-letter", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	before, err := os.ReadFile(filepath.Join(bundle, "perm", "interact", "req", "req-letter.json"))
	if err != nil {
		t.Fatal(err)
	}
	attachInteractionWorker(t, a, n)
	a.sessionsDir = ""
	rec := postInteraction(newTestHandler(t, a), n.ID, `{"request_id":"req-letter","action":"answer","answers":[{"question_id":"q1","option_ids":["q1o1"]}]}`, true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("audit failure = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "ans", "req-letter.json")); !os.IsNotExist(err) {
		t.Fatal("audit failure wrote an answer")
	}
	stored, err := os.ReadFile(filepath.Join(bundle, "perm", "interact", "req", "req-letter.json"))
	if err != nil || !bytes.Equal(stored, before) {
		t.Fatal("audit failure rewrote the request")
	}
}
