package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// ---------- stub procManager for forced failures / policy tests ----------

type stubProc struct {
	mu           sync.Mutex
	live         string
	pending      PendingPermission
	hasPending   bool
	hasSession   bool
	prepareCalls int
	deliverCalls int
	prepareErr   error
	deliverErr   error
	// prepareTok returned by PrepareResolve when set
	prepareTok string
	// after Deliver succeeds, clear pending
	clearOnDeliver bool
	// record last deliver token
	lastDeliver string
}

func (s *stubProc) Launch(string, string, string, string, string) (string, error) {
	return "", nil
}
func (s *stubProc) Send(string, string) error      { return nil }
func (s *stubProc) Clear(string) error             { return nil }
func (s *stubProc) Interrupt(string) error         { return nil }
func (s *stubProc) Turns(string) []transcript.Turn { return nil }
func (s *stubProc) Peek(string) string             { return "" }
func (s *stubProc) Usage(string) (int64, int64)    { return 0, 0 }
func (s *stubProc) Live(string) string             { return s.live }
func (s *stubProc) Attention(string) string {
	if s.hasPending {
		return "approval"
	}
	return ""
}
func (s *stubProc) LastError(string) string { return "" }
func (s *stubProc) HasSession(string) bool  { return s.hasSession }
func (s *stubProc) Kill(string) error       { return nil }
func (s *stubProc) RecordStartFailure(string, error) error {
	return nil
}
func (s *stubProc) Shutdown()               {}
func (s *stubProc) Conflict(err error) bool { return false }

func (s *stubProc) Pending(string) (PendingPermission, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasPending {
		return PendingPermission{}, false
	}
	return s.pending, true
}

func (s *stubProc) PrepareResolve(nodeID, key string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prepareCalls++
	if s.prepareErr != nil {
		return "", "", s.prepareErr
	}
	tok := s.prepareTok
	if tok == "" {
		tok = "tok-" + key
	}
	return tok, "permission: " + s.pending.Title, nil
}

func (s *stubProc) Deliver(nodeID, optID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliverCalls++
	s.lastDeliver = optID
	if s.deliverErr != nil {
		return s.deliverErr
	}
	if s.clearOnDeliver {
		s.hasPending = false
	}
	return nil
}

func seedStructuredNode(t *testing.T, a *app, id, agent, transport string) *Node {
	t.Helper()
	n := &Node{
		ID: id, Title: id, Agent: agent, Transport: transport,
		CreatedAt: "2026-08-11T00:00:00Z", Dir: a.home,
	}
	a.mu.Lock()
	a.nodes = append(a.nodes, n)
	a.byID[id] = n
	a.live[id] = "quiet"
	a.mu.Unlock()
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: a.sessionLogPath(id)}
	if err := w.Append(sessionlog.NewMeta(id, agent, "m", "", a.home)); err != nil {
		t.Fatal(err)
	}
	return n
}

func allowPending(reqID string) PendingPermission {
	return PendingPermission{
		RequestID: reqID,
		Title:     "go test ./...",
		ToolKind:  "execute",
		Reason:    "Run the test suite",
		Options: []PermOption{
			{Key: "1", Name: "Allow once", Kind: "allow"},
			{Key: "2", Name: "Reject", Kind: "reject"},
		},
	}
}

// grokStylePending mirrors Grok's menu: option 1 is allow_always, the sole
// one-time allow is key "2". Delivery must use "2", not hardcoded "1".
func grokStylePending(reqID string) PendingPermission {
	return PendingPermission{
		RequestID: reqID,
		Title:     "go test ./...",
		ToolKind:  "execute",
		Reason:    "Run the test suite",
		Options: []PermOption{
			{Key: "1", Name: "Always allow", Kind: "allow_always"},
			{Key: "2", Name: "Allow once", Kind: "allow"},
			{Key: "3", Name: "Reject", Kind: "reject"},
		},
	}
}

// ---------- Slice 4: lease lifecycle ----------

func TestAutoApproveLeaseLifecycle(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "g1", "grok", "acp")

	// New node starts off.
	a.mu.Lock()
	v := a.autoApproveViewOf(n)
	a.mu.Unlock()
	if v.Enabled || v.Phase != "off" || !v.Supported {
		t.Fatalf("new node view = %+v, want supported off", v)
	}

	// Enable while idle → primed.
	v = a.setAutoApproveEnabled(n.ID, true, "quiet", "")
	if !v.Enabled || v.Phase != "primed" || v.Count != 0 {
		t.Fatalf("enable idle = %+v, want primed count 0", v)
	}
	a.mu.Lock()
	lease1 := a.autoApprove[n.ID].LeaseID
	a.mu.Unlock()
	if lease1 == "" {
		t.Fatal("lease id empty")
	}

	// Next prompt accepted → armed.
	a.armAutoApproveOnPrompt(n.ID)
	a.mu.Lock()
	v = a.autoApproveViewOf(n)
	a.mu.Unlock()
	if v.Phase != "armed" || v.Count != 0 {
		t.Fatalf("after prompt = %+v, want armed count 0", v)
	}

	// Enable during active turn → armed immediately; pending at enable recorded.
	a.disarmAutoApprove(n.ID)
	v = a.setAutoApproveEnabled(n.ID, true, "active", "req-old")
	if v.Phase != "armed" {
		t.Fatalf("enable active = %+v, want armed", v)
	}
	a.mu.Lock()
	if !a.autoApprove[n.ID].PendingAtEnable["req-old"] {
		t.Fatal("pending-at-enable not recorded")
	}
	a.mu.Unlock()

	// Manual disable → off.
	v = a.setAutoApproveEnabled(n.ID, false, "active", "")
	if v.Enabled || v.Phase != "off" {
		t.Fatalf("disable = %+v, want off", v)
	}

	// Restart semantics: no state after delete of map entry (process restart).
	a.setAutoApproveEnabled(n.ID, true, "quiet", "")
	a2 := newTestApp(t, &fakeTmux{})
	// New app has empty autoApprove → off.
	n2 := seedStructuredNode(t, a2, "g1", "grok", "acp")
	a2.mu.Lock()
	v = a2.autoApproveViewOf(n2)
	a2.mu.Unlock()
	if v.Enabled {
		t.Fatal("fresh process must start off")
	}

	// Fork/new ID starts off.
	fork := seedStructuredNode(t, a, "g1-fork", "grok", "acp")
	a.mu.Lock()
	v = a.autoApproveViewOf(fork)
	a.mu.Unlock()
	if v.Enabled {
		t.Fatal("fork starts off")
	}

	// Completion / interrupt / clear / exit / delete transitions.
	a.setAutoApproveEnabled(n.ID, true, "active", "")
	a.disarmAutoApprove(n.ID) // interrupt/clear/exit/delete path
	a.mu.Lock()
	if a.autoApprove[n.ID] != nil {
		t.Fatal("disarm must clear state")
	}
	a.mu.Unlock()

	// clearStaleArmedBeforePrompt drops armed when idle.
	a.setAutoApproveEnabled(n.ID, true, "active", "")
	a.clearStaleArmedBeforePrompt(n.ID, "quiet")
	a.mu.Lock()
	if a.autoApprove[n.ID] != nil {
		t.Fatal("stale armed must clear before idle prompt")
	}
	a.mu.Unlock()

	// Browser navigation has no server effect — no code path; state survives.
	a.setAutoApproveEnabled(n.ID, true, "quiet", "")
	a.mu.Lock()
	if a.autoApprove[n.ID] == nil || a.autoApprove[n.ID].Phase != autoPhasePrimed {
		t.Fatal("lease must remain without browser involvement")
	}
	a.mu.Unlock()
}

func TestAutoApproveUnsupportedClaude(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{ID: "c1", Title: "c1", Agent: "claude", Transport: "tmux", CreatedAt: "t"}
	a.nodes = append(a.nodes, n)
	a.byID["c1"] = n

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/nodes/c1/auto-approve", strings.NewReader(`{"enabled":true}`))
	r.SetPathValue("id", "c1")
	a.handleAutoApprove(rec, r)
	if rec.Code == 200 {
		t.Fatalf("claude must not succeed, code=%d body=%s", rec.Code, rec.Body.String())
	}
	a.mu.Lock()
	if a.autoApprove["c1"] != nil {
		t.Fatal("claude must create no auto-approve state")
	}
	// Claude never contacts tmux for this endpoint.
	a.mu.Unlock()
	// fakeTmux records calls on the server; nothing should have been sent for this.
}

// ---------- Slice 5: HTTP + ETag ----------

func TestAutoApproveHTTPAndETag(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "cx1", "codex", "codex")
	h := newTestHandler(t, a)

	// Missing enabled → 400
	rec := routeRequest(h, http.MethodPost, "/api/nodes/cx1/auto-approve", `{}`, true)
	if rec.Code != 400 {
		t.Fatalf("empty body code=%d", rec.Code)
	}

	// Enable → primed
	rec = routeRequest(h, http.MethodPost, "/api/nodes/cx1/auto-approve", `{"enabled":true}`, true)
	if rec.Code != 200 {
		t.Fatalf("enable code=%d body=%s", rec.Code, rec.Body.String())
	}
	var view autoApproveView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Supported || !view.Enabled || view.Phase != "primed" || view.Count != 0 {
		t.Fatalf("enable view = %+v", view)
	}

	// Chat includes auto_approve; ETag changes with state.
	rec1 := chatGET(t, a, "cx1", "")
	if rec1.Code != 200 {
		t.Fatalf("chat code=%d", rec1.Code)
	}
	etag1 := rec1.Header().Get("ETag")
	var chat map[string]any
	if err := json.Unmarshal(rec1.Body.Bytes(), &chat); err != nil {
		t.Fatal(err)
	}
	aa, ok := chat["auto_approve"].(map[string]any)
	if !ok || aa["phase"] != "primed" {
		t.Fatalf("chat auto_approve = %v", chat["auto_approve"])
	}

	// Unchanged → 304
	rec304 := chatGET(t, a, "cx1", etag1)
	if rec304.Code != 304 {
		t.Fatalf("unchanged code=%d, want 304", rec304.Code)
	}

	// Arm via prompt transition → ETag changes
	a.armAutoApproveOnPrompt(n.ID)
	rec2 := chatGET(t, a, "cx1", etag1)
	if rec2.Code != 200 {
		t.Fatalf("after arm code=%d", rec2.Code)
	}
	etag2 := rec2.Header().Get("ETag")
	if etag2 == "" || etag2 == etag1 {
		t.Fatalf("etag after arm = %q, was %q", etag2, etag1)
	}

	// Count change → ETag changes
	a.mu.Lock()
	a.autoApprove[n.ID].Count = 3
	a.mu.Unlock()
	rec3 := chatGET(t, a, "cx1", etag2)
	if rec3.Code != 200 {
		t.Fatalf("after count code=%d", rec3.Code)
	}
	if rec3.Header().Get("ETag") == etag2 {
		t.Fatal("count change must change ETag")
	}

	// Error change → ETag changes
	etag3 := rec3.Header().Get("ETag")
	a.mu.Lock()
	a.autoApprove[n.ID].Error = "auto-approve audit failed: boom"
	a.mu.Unlock()
	rec4 := chatGET(t, a, "cx1", etag3)
	if rec4.Code != 200 || rec4.Header().Get("ETag") == etag3 {
		t.Fatalf("error must change ETag; code=%d etag=%q was %q", rec4.Code, rec4.Header().Get("ETag"), etag3)
	}

	// Decision in log → ETag changes (new projected decision surface).
	etag4 := rec4.Header().Get("ETag")
	w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	if err := w.Append(sessionlog.NewDecision(sessionlog.DecisionEvent{
		Source: "auto", LeaseID: "L", RequestID: "R",
		Selected: sessionlog.DecOption{Key: "1", Name: "Allow once", Kind: "allow"},
	})); err != nil {
		t.Fatal(err)
	}
	rec5 := chatGET(t, a, "cx1", etag4)
	if rec5.Code != 200 || rec5.Header().Get("ETag") == etag4 {
		t.Fatalf("decision must change ETag; code=%d", rec5.Code)
	}
	var chat5 map[string]any
	_ = json.Unmarshal(rec5.Body.Bytes(), &chat5)
	if chat5["decisions"] == nil {
		t.Fatal("chat must project decisions")
	}

	// ?history=1 remains unconditional (no ETag short-circuit).
	rHist := httptest.NewRequest("GET", "/api/nodes/cx1/chat?history=1", nil)
	rHist.SetPathValue("id", "cx1")
	rHist.Header.Set("If-None-Match", rec5.Header().Get("ETag"))
	recH := httptest.NewRecorder()
	a.handleChat(recH, rHist)
	if recH.Code != 200 {
		t.Fatalf("history code=%d, want 200 unconditional", recH.Code)
	}

	// Missing node
	rec = routeRequest(h, http.MethodPost, "/api/nodes/missing/auto-approve", `{"enabled":true}`, true)
	if rec.Code != 404 {
		t.Fatalf("missing node code=%d", rec.Code)
	}

	// Ended node
	a.mu.Lock()
	n.EndedAt = time.Now().UTC().Format(time.RFC3339)
	a.mu.Unlock()
	rec = routeRequest(h, http.MethodPost, "/api/nodes/cx1/auto-approve", `{"enabled":true}`, true)
	if rec.Code != 409 {
		t.Fatalf("ended node code=%d, want 409", rec.Code)
	}
}

// ---------- Slice 6: audit-before-delivery ----------

func TestMaybeAutoApproveAuditBeforeDeliver(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "s1", "opencode", "acp")
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("req-1"), clearOnDeliver: true,
	}
	// Arm lease (active enable).
	a.setAutoApproveEnabled(n.ID, true, "active", "")

	a.maybeAutoApprove(n, stub)
	if stub.prepareCalls != 1 || stub.deliverCalls != 1 {
		t.Fatalf("prepare/deliver = %d/%d, want 1/1", stub.prepareCalls, stub.deliverCalls)
	}
	if stub.lastDeliver != "tok-1" {
		t.Fatalf("deliver token = %q, want tok-1 (key 1)", stub.lastDeliver)
	}
	// Decision on disk.
	evs := sessionlog.ReadEvents(a.sessionLogPath(n.ID))
	var dec *sessionlog.DecisionEvent
	for i := range evs {
		if evs[i].T == "decision" && evs[i].Decision != nil {
			dec = evs[i].Decision
		}
	}
	if dec == nil || dec.Source != "auto" || dec.RequestID != "req-1" || dec.Selected.Kind != "allow" {
		t.Fatalf("decision = %+v", dec)
	}
	if dec.Selected.Key != "1" || dec.Selected.Name != "Allow once" {
		t.Fatalf("selected = %+v, want key=1 Allow once", dec.Selected)
	}
	if len(dec.Options) != 2 {
		t.Fatalf("options = %+v", dec.Options)
	}
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	if st == nil || st.Count != 1 {
		t.Fatalf("count after success = %+v", st)
	}
	a.mu.Unlock()
	// No manual attention after successful resolve (pending cleared).
	if stub.Attention(n.ID) != "" {
		t.Fatal("attention should be empty after successful auto-approve")
	}
}

// Semantic policy: Grok puts allow_always at key 1 and the sole one-time allow
// at key 2. Delivery and the audit record must both use key "2".
func TestMaybeAutoApproveGrokStyleSelectsKey2(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "g-key2", "grok", "acp")
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: grokStylePending("req-g2"), clearOnDeliver: true,
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "")
	a.maybeAutoApprove(n, stub)
	if stub.prepareCalls != 1 || stub.deliverCalls != 1 {
		t.Fatalf("prepare/deliver = %d/%d, want 1/1", stub.prepareCalls, stub.deliverCalls)
	}
	if stub.lastDeliver != "tok-2" {
		t.Fatalf("deliver must use selected key 2 token, got %q", stub.lastDeliver)
	}
	evs := sessionlog.ReadEvents(a.sessionLogPath(n.ID))
	var dec *sessionlog.DecisionEvent
	for i := range evs {
		if evs[i].T == "decision" && evs[i].Decision != nil {
			dec = evs[i].Decision
		}
	}
	if dec == nil {
		t.Fatal("missing decision audit")
	}
	if dec.Selected.Key != "2" || dec.Selected.Kind != "allow" || dec.Selected.Name != "Allow once" {
		t.Fatalf("selected = %+v, want key=2 Allow once allow", dec.Selected)
	}
	if len(dec.Options) != 3 {
		t.Fatalf("audit must record all offered options, got %+v", dec.Options)
	}
	// All alternatives present with key/name/kind.
	wantKinds := map[string]string{"1": "allow_always", "2": "allow", "3": "reject"}
	for _, o := range dec.Options {
		if wantKinds[o.Key] != o.Kind {
			t.Fatalf("option %s kind = %q, want %q", o.Key, o.Kind, wantKinds[o.Key])
		}
	}
	a.mu.Lock()
	if a.autoApprove[n.ID].Count != 1 {
		t.Fatalf("count = %d, want 1 after successful key-2 delivery", a.autoApprove[n.ID].Count)
	}
	a.mu.Unlock()
}

func TestMaybeAutoApproveMultipleAllowsStayManual(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "ambig", "pi", "acp")
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: PendingPermission{
			RequestID: "r-ambig", Title: "x",
			Options: []PermOption{
				{Key: "1", Name: "Allow once", Kind: "allow"},
				{Key: "2", Name: "Allow this tool", Kind: "allow"},
				{Key: "3", Name: "Reject", Kind: "reject"},
			},
		},
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "")
	a.maybeAutoApprove(n, stub)
	if stub.prepareCalls != 0 || stub.deliverCalls != 0 {
		t.Fatalf("ambiguous allows must stay manual; prep/del=%d/%d",
			stub.prepareCalls, stub.deliverCalls)
	}
}

func TestMaybeAutoApproveAuditFailureFailClosed(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "s2", "pi", "acp")
	// Make append fail: replace session file with a directory.
	path := a.sessionLogPath(n.ID)
	_ = os.Remove(path)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("req-fail"),
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "")
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 0 {
		t.Fatalf("deliver called %d times after audit failure", stub.deliverCalls)
	}
	if stub.prepareCalls != 1 {
		t.Fatalf("prepare calls = %d", stub.prepareCalls)
	}
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	if st == nil || st.Count != 0 || st.Error == "" {
		t.Fatalf("state after audit fail = %+v", st)
	}
	if !st.Attempted["req-fail"] {
		t.Fatal("must mark attempted so poller does not retry every tick")
	}
	a.mu.Unlock()
	// Pending still for manual handling.
	if !stub.hasPending {
		t.Fatal("pending must remain for manual path")
	}
	// Second poll must not re-prepare.
	a.maybeAutoApprove(n, stub)
	if stub.prepareCalls != 1 {
		t.Fatalf("retry prepare calls = %d, want 1", stub.prepareCalls)
	}
}

func TestMaybeAutoApproveDeliveryFailureKeepsAudit(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "s3", "grok", "acp")
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("req-d"), deliverErr: errors.New("agent gone"),
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "")
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 1 {
		t.Fatal("deliver must be attempted")
	}
	evs := sessionlog.ReadEvents(a.sessionLogPath(n.ID))
	var sawDec, sawErr bool
	for _, ev := range evs {
		if ev.T == "decision" {
			sawDec = true
		}
		if ev.T == "error" && strings.Contains(ev.Error, "auto-approve delivery failed") {
			sawErr = true
		}
	}
	if !sawDec || !sawErr {
		t.Fatalf("want decision + error events; dec=%v err=%v", sawDec, sawErr)
	}
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	if st == nil || st.Count != 0 || st.Error == "" {
		t.Fatalf("delivery fail state = %+v", st)
	}
	a.mu.Unlock()
	if !stub.hasPending {
		t.Fatal("pending remains for honest manual handling")
	}
}

func TestMaybeAutoApproveSkipsPendingAtEnable(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "s4", "codex", "codex")
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("already-showing"),
	}
	// Enable while this request is already pending.
	a.setAutoApproveEnabled(n.ID, true, "active", "already-showing")
	a.maybeAutoApprove(n, stub)
	if stub.prepareCalls != 0 || stub.deliverCalls != 0 {
		t.Fatalf("must not auto-resolve pre-enable request; prep/del=%d/%d",
			stub.prepareCalls, stub.deliverCalls)
	}
}

func TestMaybeAutoApproveIneligibleStaysManual(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "s5", "opencode", "acp")
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: PendingPermission{
			RequestID: "r", Title: "x",
			Options: []PermOption{{Key: "1", Name: "Always", Kind: "allow_always"}},
		},
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "")
	a.maybeAutoApprove(n, stub)
	if stub.prepareCalls != 0 {
		t.Fatal("ineligible must not prepare")
	}
	if stub.Attention(n.ID) != "approval" {
		t.Fatal("ineligible remains on manual attention path")
	}
}

// ---------- Slice 7: poll integration + concurrency ----------

func TestPollAutoApproveBeforeAttention(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "p1", "grok", "acp")
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("req-poll"), clearOnDeliver: true,
	}
	// Inject stub via replacing acp manager adapter is hard; call maybeAutoApprove
	// then simulate poll attention publish order.
	a.setAutoApproveEnabled(n.ID, true, "active", "")
	a.maybeAutoApprove(n, stub)
	attn := stub.Attention(n.ID)
	a.mu.Lock()
	a.attn[n.ID] = attn
	a.mu.Unlock()
	if a.attn[n.ID] != "" {
		t.Fatalf("successfully auto-approved must not publish manual attention, got %q", a.attn[n.ID])
	}
}

func TestPollNoDoubleDeliver(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "p2", "pi", "acp")
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("req-once"), clearOnDeliver: true,
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "")
	// Concurrent polls.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.maybeAutoApprove(n, stub)
		}()
	}
	wg.Wait()
	if stub.deliverCalls != 1 {
		t.Fatalf("deliverCalls = %d, want exactly 1", stub.deliverCalls)
	}
	a.mu.Lock()
	if a.autoApprove[n.ID].Count != 1 {
		t.Fatalf("count = %d, want 1", a.autoApprove[n.ID].Count)
	}
	a.mu.Unlock()
}

func TestDisarmDuringAutoApproveCannotLeak(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "p3", "codex", "codex")
	// Slow deliver: hold until we disarm mid-flight.
	gate := make(chan struct{})
	var deliverStarted atomic.Int32
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("req-race"),
	}
	// Wrap deliver with gate via deliverErr path: custom type
	slow := &slowDeliverProc{stubProc: stub, gate: gate, started: &deliverStarted}
	a.setAutoApproveEnabled(n.ID, true, "active", "")

	done := make(chan struct{})
	go func() {
		a.maybeAutoApprove(n, slow)
		close(done)
	}()
	// Wait until deliver is about to run (after audit).
	deadline := time.Now().Add(2 * time.Second)
	for deliverStarted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if deliverStarted.Load() == 0 {
		close(gate)
		<-done
		t.Fatal("deliver never started")
	}
	// Concurrent disable.
	a.disarmAutoApprove(n.ID)
	close(gate)
	<-done
	// Count must not increment on a disarmed lease after delivery... actually
	// delivery may still succeed to the agent, but count check requires lease match.
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	a.mu.Unlock()
	if st != nil && st.Count > 0 {
		// If disarm deleted state, st is nil — good. If somehow still present with count, fail.
		t.Fatalf("leaked armed state with count: %+v", st)
	}
}

// slowDeliverProc blocks in Deliver until gate closes.
type slowDeliverProc struct {
	*stubProc
	gate    chan struct{}
	started *atomic.Int32
}

func (s *slowDeliverProc) Deliver(nodeID, optID string) error {
	s.started.Add(1)
	<-s.gate
	return s.stubProc.Deliver(nodeID, optID)
}

func TestClaudeAutoApproveNeverTouchesTmux(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c-claude": true}}
	a := newTestApp(t, f)
	n := &Node{ID: "c-claude", Title: "c", Agent: "claude", CreatedAt: "t"}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	// Baseline call count.
	before := len(f.subcommands())
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/nodes/c-claude/auto-approve", strings.NewReader(`{"enabled":true}`))
	r.SetPathValue("id", "c-claude")
	a.handleAutoApprove(rec, r)
	if rec.Code == 200 {
		t.Fatal("claude must not succeed")
	}
	after := len(f.subcommands())
	if after != before {
		t.Fatalf("claude auto-approve contacted tmux: calls before=%d after=%d subs=%v",
			before, after, f.subcommands())
	}
}

func TestAutoApproveAgentsShareEligibility(t *testing.T) {
	// Prove codex/grok/opencode/pi all use the same pure function (table already
	// covers options). Here we only assert each transport is supported and
	// Claude is not.
	agents := []struct {
		agent, transport string
		supported        bool
	}{
		{"codex", "codex", true},
		{"grok", "acp", true},
		{"opencode", "acp", true},
		{"pi", "acp", true},
		{"claude", "tmux", false},
		{"claude", "", false},
	}
	for _, ag := range agents {
		n := &Node{Agent: ag.agent, Transport: ag.transport}
		if got := autoApproveSupported(n); got != ag.supported {
			t.Errorf("%s/%s supported=%v, want %v", ag.agent, ag.transport, got, ag.supported)
		}
	}
}

// Call-order spy: append is sequenced relative to Deliver via sessionsDir
// observability — already covered by audit failure (prepare without deliver)
// and success (decision before pending clear). Explicit order counter:
func TestAutoApproveCallOrder(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "ord", "grok", "acp")
	var order []string
	var mu sync.Mutex
	stub := &orderSpyProc{
		stubProc: &stubProc{
			live: "active", hasSession: true, hasPending: true,
			pending: allowPending("req-ord"), clearOnDeliver: true,
		},
		onPrepare: func() {
			mu.Lock()
			order = append(order, "prepare")
			mu.Unlock()
		},
		onDeliver: func() {
			// Decision must already be on disk.
			for _, ev := range sessionlog.ReadEvents(a.sessionLogPath(n.ID)) {
				if ev.T == "decision" {
					mu.Lock()
					order = append(order, "audit-seen")
					mu.Unlock()
					break
				}
			}
			mu.Lock()
			order = append(order, "deliver")
			mu.Unlock()
		},
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "")
	a.maybeAutoApprove(n, stub)
	mu.Lock()
	got := strings.Join(order, ",")
	mu.Unlock()
	if got != "prepare,audit-seen,deliver" {
		t.Fatalf("order = %q, want prepare,audit-seen,deliver", got)
	}
}

type orderSpyProc struct {
	*stubProc
	onPrepare func()
	onDeliver func()
}

func (o *orderSpyProc) PrepareResolve(id, key string) (string, string, error) {
	if o.onPrepare != nil {
		o.onPrepare()
	}
	return o.stubProc.PrepareResolve(id, key)
}
func (o *orderSpyProc) Deliver(id, tok string) error {
	if o.onDeliver != nil {
		o.onDeliver()
	}
	return o.stubProc.Deliver(id, tok)
}

func TestDeleteClearsAutoApprove(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "del1", "pi", "acp")
	a.setAutoApproveEnabled(n.ID, true, "quiet", "")
	a.mu.Lock()
	a.removeNodeLocked(n.ID)
	if a.autoApprove[n.ID] != nil {
		t.Fatal("delete must clear auto-approve state")
	}
	a.mu.Unlock()
}
