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
	// lastPrepareReqID is the expectedRequestID last passed to PrepareResolve
	lastPrepareReqID string
	// prepareHook runs under the stub lock just before the prepare result
	// (tests use it to swap pending between snapshot and prepare)
	prepareHook func(*stubProc)
	// conflictErrors are classified as Conflict (HTTP 409)
	conflictErrors []error
	// boundary* override PermissionBoundary when boundarySet is true
	boundarySet    bool
	boundaryIncarn string
	boundaryMaxSeq uint64
	// sendHook runs under the stub lock during Send (simulates turn starting
	// and issuing a permission before Send returns).
	sendCalls int
	sendErr   error
	sendHook  func(*stubProc)
	// lastError is the structured LastError() projection (empty after a
	// successful turn; set for interrupt/empty/failed).
	lastError     string
	shutdownCalls atomic.Int32
}

func (s *stubProc) Launch(string, string, string, string, string) (string, error) {
	return "", nil
}
func (s *stubProc) Send(nodeID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendCalls++
	if s.sendHook != nil {
		s.sendHook(s)
	}
	return s.sendErr
}
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
func (s *stubProc) LastError(string) string { return s.lastError }
func (s *stubProc) HasSession(string) bool  { return s.hasSession }
func (s *stubProc) Kill(string) error       { return nil }
func (s *stubProc) RecordStartFailure(string, error) error {
	return nil
}
func (s *stubProc) Shutdown() { s.shutdownCalls.Add(1) }
func (s *stubProc) Conflict(err error) bool {
	for _, e := range s.conflictErrors {
		if err == e {
			return true
		}
	}
	return false
}

func (s *stubProc) Pending(string) (PendingPermission, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasPending {
		return PendingPermission{}, false
	}
	return s.pending, true
}

func (s *stubProc) PermissionBoundary(string) (string, uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasSession && s.boundaryIncarn == "" && s.boundaryMaxSeq == 0 {
		return "", 0, false
	}
	if s.boundarySet {
		return s.boundaryIncarn, s.boundaryMaxSeq, true
	}
	// Derive from pending RequestID when it uses incarn:seq form.
	if s.hasPending {
		if inc, seq, ok := parsePermissionRequestID(s.pending.RequestID); ok {
			return inc, seq, true
		}
	}
	if s.hasSession {
		return s.boundaryIncarn, s.boundaryMaxSeq, true
	}
	return "", 0, false
}

func (s *stubProc) PrepareResolve(nodeID, expectedRequestID, key string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prepareCalls++
	s.lastPrepareReqID = expectedRequestID
	if s.prepareHook != nil {
		s.prepareHook(s)
	}
	// Bind expected request ID to the current pending (mirrors real managers).
	if s.hasPending && expectedRequestID != "" && expectedRequestID != s.pending.RequestID {
		return "", "", errStubStale
	}
	if s.prepareErr != nil {
		return "", "", s.prepareErr
	}
	if !s.hasPending {
		return "", "", errStubNoPending
	}
	tok := s.prepareTok
	if tok == "" {
		tok = "tok-" + key
	}
	return tok, "permission: " + s.pending.Title, nil
}

var (
	errStubStale     = errors.New("stub: pending permission request has changed")
	errStubNoPending = errors.New("stub: no permission request is pending")
)

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
	v = a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0)
	if !v.Enabled || v.Phase != "primed" || v.Count != 0 {
		t.Fatalf("enable idle = %+v, want primed count 0", v)
	}
	a.mu.Lock()
	lease1 := a.autoApprove[n.ID].LeaseID
	a.mu.Unlock()
	if lease1 == "" {
		t.Fatal("lease id empty")
	}

	// Next prompt accepted → armed (pre-send boundary empty / fail-closed until bound).
	a.armAutoApproveOnPrompt(n.ID, "", 0, false)
	a.mu.Lock()
	v = a.autoApproveViewOf(n)
	a.mu.Unlock()
	if v.Phase != "armed" || v.Count != 0 {
		t.Fatalf("after prompt = %+v, want armed count 0", v)
	}

	// Enable during active turn → armed immediately; pending at enable recorded.
	a.disarmAutoApprove(n.ID)
	v = a.setAutoApproveEnabled(n.ID, true, "active", "inc", 1)
	if v.Phase != "armed" {
		t.Fatalf("enable active = %+v, want armed", v)
	}
	a.mu.Lock()
	if a.autoApprove[n.ID].EnableIncarn != "inc" || a.autoApprove[n.ID].EnableMaxSeq != 1 {
		t.Fatalf("enable cutoff = %q/%d, want inc/1", a.autoApprove[n.ID].EnableIncarn, a.autoApprove[n.ID].EnableMaxSeq)
	}
	a.mu.Unlock()

	// Manual disable → off.
	v = a.setAutoApproveEnabled(n.ID, false, "active", "", 0)
	if v.Enabled || v.Phase != "off" {
		t.Fatalf("disable = %+v, want off", v)
	}

	// Restart semantics: no state after delete of map entry (process restart).
	a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0)
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
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
	a.disarmAutoApprove(n.ID) // interrupt/clear/exit/delete path
	a.mu.Lock()
	if a.autoApprove[n.ID] != nil {
		t.Fatal("disarm must clear state")
	}
	a.mu.Unlock()

	// settleArmedBeforePrompt closes an armed structured-agent lease that
	// outlived its turn. Only Claude carries the enable across a turn.
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
	a.settleArmedBeforePrompt(n.ID, "quiet")
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	if st != nil {
		t.Fatalf("stale structured-agent lease = %+v, want off before the idle prompt", st)
	}
	a.mu.Unlock()
	a.disarmAutoApprove(n.ID)

	// Browser navigation has no server effect — no code path; state survives.
	a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0)
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
	a.armAutoApproveOnPrompt(n.ID, "inc", 0, true)
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
		pending: allowPending("inc:1"), clearOnDeliver: true,
	}
	// Arm lease (active enable).
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)

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
	if dec == nil || dec.Source != "auto" || dec.RequestID != "inc:1" || dec.Selected.Kind != "allow" {
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
		pending: grokStylePending("inc:1"), clearOnDeliver: true,
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
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
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
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
		pending: allowPending("inc:1"),
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
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
	if !st.Attempted["inc:1"] {
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
		pending: allowPending("inc:1"), deliverErr: errors.New("agent gone"),
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
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
		// Same incarnation, seq at the enable watermark.
		pending: allowPending("inc:5"),
	}
	// Enable cutoff: incarn=inc, maxSeq=5 → this request is pre-enable.
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 5)
	a.maybeAutoApprove(n, stub)
	if stub.prepareCalls != 0 || stub.deliverCalls != 0 {
		t.Fatalf("must not auto-resolve pre-enable request; prep/del=%d/%d",
			stub.prepareCalls, stub.deliverCalls)
	}
	// A later request (seq 6) on the same incarnation is eligible.
	stub.pending = allowPending("inc:6")
	stub.hasPending = true
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 1 {
		t.Fatalf("post-enable request should deliver; del=%d", stub.deliverCalls)
	}
}

// A primed lease — enabled while the node was idle — must not approve anything
// until a prompt arms it. Arming with the pre-send boundary makes the same
// request eligible (one-turn contract / C3).
func TestMaybeAutoApprovePrimedLeaseNeverApproves(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "s6", "grok", "acp")
	stub := &stubProc{
		live: "quiet", hasSession: true, hasPending: true,
		pending:        grokStylePending("inc:1"),
		clearOnDeliver: true,
		boundarySet:    true, boundaryIncarn: "inc", boundaryMaxSeq: 0,
	}
	if v := a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0); v.Phase != "primed" {
		t.Fatalf("setup: phase = %q, want primed (enabled while idle)", v.Phase)
	}
	// Primed must never approve, even if the request is otherwise eligible.
	a.maybeAutoApprove(n, stub)
	if stub.prepareCalls != 0 || stub.deliverCalls != 0 {
		t.Fatalf("primed lease auto-approved; prep/del=%d/%d, want 0/0",
			stub.prepareCalls, stub.deliverCalls)
	}
	// Pre-send boundary maxSeq=0: the turn's first permission (inc:1) is eligible after arm.
	a.armAutoApproveOnPrompt(n.ID, "inc", 0, true)
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	if st == nil || st.Phase != autoPhaseArmed || st.EnableIncarn != "inc" || st.EnableMaxSeq != 0 {
		t.Fatalf("after arm state = %+v, want armed inc/0", st)
	}
	a.mu.Unlock()
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 1 {
		t.Fatalf("armed lease delivered %d, want 1 — same request becomes eligible",
			stub.deliverCalls)
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
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
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
		pending: allowPending("inc:1"), clearOnDeliver: true,
	}
	// Inject stub via replacing acp manager adapter is hard; call maybeAutoApprove
	// then simulate poll attention publish order.
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
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
		pending: allowPending("inc:1"), clearOnDeliver: true,
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
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
	// Slow deliver: hold until the test releases it. Disable waits on the
	// per-node auto-gate, so it must be started concurrently (not before
	// release) — once disable returns, the prior lease is gone.
	gate := make(chan struct{})
	inDeliver := make(chan struct{})
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("inc:1"),
	}
	slow := &channelDeliverProc{
		stubProc: stub,
		onEnter:  func() { close(inDeliver); <-gate },
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)

	done := make(chan struct{})
	go func() {
		a.maybeAutoApprove(n, slow)
		close(done)
	}()
	select {
	case <-inDeliver:
	case <-time.After(3 * time.Second):
		close(gate)
		<-done
		t.Fatal("deliver never started")
	}
	// Concurrent disable waits behind the in-flight decision.
	disarmDone := make(chan struct{})
	go func() {
		a.disarmAutoApprove(n.ID)
		close(disarmDone)
	}()
	close(gate)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("maybeAutoApprove did not finish")
	}
	select {
	case <-disarmDone:
	case <-time.After(3 * time.Second):
		t.Fatal("disarm did not finish")
	}
	// After disable returns the lease is gone; no armed state may remain.
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	a.mu.Unlock()
	if st != nil {
		t.Fatalf("leaked armed state after disable: %+v", st)
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
	// The invariant guard: a Claude approval is answered through the hook
	// rendezvous or not at all. If anyone ever routes one through SendKeys —
	// which would make scimux press keys in a human's pane — this goes red.
	f := &fakeTmux{alive: map[string]bool{"c-claude": true}}
	a := newTestApp(t, f)
	n := &Node{ID: "c-claude", Title: "c", Agent: "claude", CreatedAt: "t"}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	// An unsupported Claude pane (no permission-capable bundle) is refused,
	// and the refusal costs no tmux call.
	before := len(f.subcommands())
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/nodes/c-claude/auto-approve", strings.NewReader(`{"enabled":true}`))
	r.SetPathValue("id", "c-claude")
	a.handleAutoApprove(rec, r)
	if rec.Code == 200 {
		t.Fatal("a claude pane without the permission hook must not succeed")
	}
	after := len(f.subcommands())
	if after != before {
		t.Fatalf("claude auto-approve contacted tmux: calls before=%d after=%d subs=%v",
			before, after, f.subcommands())
	}

	// A supported pane, armed and actually answering a request, is the case
	// that matters: still zero tmux traffic.
	armed, bundle := seedPermClaude(t, a, "c-armed", hookSIDOwn)
	a.setAutoApproveEnabled(armed.ID, true, "active", "", 0)
	dropRequest(t, bundle, armedRequest(a, armed, "nonce-1"))
	before = len(f.subcommands())
	if got := a.resolveClaudePermissions(); got != 1 {
		t.Fatalf("armed lane delivered %d approvals, want 1", got)
	}
	if got := len(f.subcommands()); got != before {
		t.Fatalf("answering a claude approval contacted tmux: before=%d after=%d subs=%v",
			before, got, f.subcommands())
	}
}

func TestAutoApproveAgentsShareEligibility(t *testing.T) {
	// Prove codex/grok/opencode/pi all use the same pure function (table already
	// covers options). Here we only assert which transports the *structured*
	// predicate covers: Claude is not one of them — it reaches auto-approval
	// through its own hook, checked separately below.
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

	// The endpoint's predicate is the union: structured transports plus a
	// Claude pane whose own bundle carries the permission rendezvous.
	a := newTestApp(t, &fakeTmux{})
	capable, _ := seedPermClaude(t, a, "hooked", hookSIDOwn)
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.autoApproveSupportedFor(capable) {
		t.Error("a hook-capable claude pane must be supported by the endpoint predicate")
	}
	for _, ag := range agents {
		n := &Node{ID: "x-" + ag.agent + ag.transport, Agent: ag.agent, Transport: ag.transport}
		if got := a.autoApproveSupportedFor(n); got != ag.supported {
			t.Errorf("endpoint predicate %s/%s = %v, want %v", ag.agent, ag.transport, got, ag.supported)
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
			pending: allowPending("inc:1"), clearOnDeliver: true,
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
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
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

func (o *orderSpyProc) PrepareResolve(id, expectedRequestID, key string) (string, string, error) {
	if o.onPrepare != nil {
		o.onPrepare()
	}
	return o.stubProc.PrepareResolve(id, expectedRequestID, key)
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
	a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0)
	a.mu.Lock()
	a.removeNodeLocked(n.ID)
	if a.autoApprove[n.ID] != nil {
		t.Fatal("delete must clear auto-approve state")
	}
	a.mu.Unlock()
}

// ---------- P1 security: request-ID bind + lease linearization ----------

// Snapshot A, replace with B (same key layout) before prepare. B must not be
// delivered and no decision event may be written.
func TestMaybeAutoApproveStaleRequestNotDelivered(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "race-a", "grok", "acp")
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending:        allowPending("inc:1"),
		conflictErrors: []error{errStubStale},
	}
	// When prepare runs with A's id, pending is already B (identical keys).
	stub.prepareHook = func(s *stubProc) {
		s.pending = allowPending("inc:2")
		s.hasPending = true
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 0 {
		t.Fatalf("deliverCalls = %d, want 0 (B must not be answered for A's evaluation)", stub.deliverCalls)
	}
	if stub.lastPrepareReqID != "inc:1" {
		t.Fatalf("prepare expected id = %q, want inc:1", stub.lastPrepareReqID)
	}
	for _, ev := range sessionlog.ReadEvents(a.sessionLogPath(n.ID)) {
		if ev.T == "decision" {
			t.Fatalf("decision audit written for stale prepare: %+v", ev.Decision)
		}
	}
	// B remains pending for manual resolution.
	if !stub.hasPending || stub.pending.RequestID != "inc:2" {
		t.Fatalf("B must remain pending; got has=%v pending=%+v", stub.hasPending, stub.pending)
	}
}

// Replacement B has allow_always at the key that was A's one-time allow.
// Option position is never authorization — B must not be delivered.
func TestMaybeAutoApproveReplacementAllowAlwaysNotDelivered(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "race-aa", "opencode", "acp")
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending:        allowPending("inc:1"), // key "1" is allow
		conflictErrors: []error{errStubStale},
	}
	stub.prepareHook = func(s *stubProc) {
		// B: key "1" is now allow_always (A's former key position).
		s.pending = PendingPermission{
			RequestID: "inc:2", Title: "go test ./...", ToolKind: "execute",
			Options: []PermOption{
				{Key: "1", Name: "Always allow", Kind: "allow_always"},
				{Key: "2", Name: "Reject", Kind: "reject"},
			},
		}
		s.hasPending = true
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 0 {
		t.Fatalf("deliverCalls = %d, want 0", stub.deliverCalls)
	}
	for _, ev := range sessionlog.ReadEvents(a.sessionLogPath(n.ID)) {
		if ev.T == "decision" {
			t.Fatalf("must not audit a decision for a replaced request: %+v", ev.Decision)
		}
	}
}

// Disable wins the per-node decision ordering: disable returns and the old
// lease never calls Deliver.
func TestAutoApproveDisableWinsPreventsDelivery(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "dis-first", "pi", "acp")
	inPending := make(chan struct{})
	releasePending := make(chan struct{})
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("inc:1"), clearOnDeliver: true,
	}
	// Block the decision path in Pending so disable can take the auto-gate first.
	blocking := &pendingBlockProc{
		stubProc: stub,
		onPending: func() {
			select {
			case <-inPending:
			default:
				close(inPending)
			}
			<-releasePending
		},
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)

	done := make(chan struct{})
	go func() {
		a.maybeAutoApprove(n, blocking)
		close(done)
	}()
	select {
	case <-inPending:
	case <-time.After(3 * time.Second):
		t.Fatal("decision never entered Pending")
	}
	// Disable while decision is still before the auto-gate.
	a.disarmAutoApprove(n.ID)
	close(releasePending)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("maybeAutoApprove did not return after disable")
	}
	if stub.deliverCalls != 0 || stub.prepareCalls != 0 {
		t.Fatalf("old lease must not prepare/deliver after disable; prep/del=%d/%d",
			stub.prepareCalls, stub.deliverCalls)
	}
	a.mu.Lock()
	if a.autoApprove[n.ID] != nil {
		t.Fatalf("lease must be off after disable: %+v", a.autoApprove[n.ID])
	}
	a.mu.Unlock()
}

// Automatic decision wins ordering: audit+delivery finish before disable
// returns; no old-lease delivery afterward.
func TestAutoApproveDecisionWinsBeforeDisable(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "dec-first", "codex", "codex")
	inDeliver := make(chan struct{})
	releaseDeliver := make(chan struct{})
	order := make(chan string, 4)
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("inc:1"), clearOnDeliver: true,
	}
	slow := &channelDeliverProc{
		stubProc: stub,
		onEnter: func() {
			close(inDeliver)
			<-releaseDeliver
			order <- "deliver-done"
		},
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)

	decDone := make(chan struct{})
	go func() {
		a.maybeAutoApprove(n, slow)
		close(decDone)
	}()
	select {
	case <-inDeliver:
	case <-time.After(3 * time.Second):
		t.Fatal("decision never reached Deliver")
	}
	// Disable must wait behind the in-flight decision (holds auto-gate).
	disarmDone := make(chan struct{})
	go func() {
		a.disarmAutoApprove(n.ID)
		order <- "disable-done"
		close(disarmDone)
	}()
	// Finish delivery; then disable may return. Order is recorded on channels.
	close(releaseDeliver)
	select {
	case <-decDone:
	case <-time.After(3 * time.Second):
		t.Fatal("decision did not finish")
	}
	select {
	case <-disarmDone:
	case <-time.After(3 * time.Second):
		t.Fatal("disable did not return after decision released the gate")
	}
	var seq []string
	for len(seq) < 2 {
		select {
		case s := <-order:
			seq = append(seq, s)
		case <-time.After(3 * time.Second):
			t.Fatalf("order incomplete: %v", seq)
		}
	}
	if seq[0] != "deliver-done" || seq[1] != "disable-done" {
		t.Fatalf("order = %v, want deliver-done then disable-done", seq)
	}
	if stub.deliverCalls != 1 {
		t.Fatalf("deliverCalls = %d, want 1", stub.deliverCalls)
	}
	// No second delivery after disable.
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 1 {
		t.Fatalf("post-disable deliverCalls = %d, want still 1", stub.deliverCalls)
	}
	// Decision audit exists (committed before disable returned).
	var saw bool
	for _, ev := range sessionlog.ReadEvents(a.sessionLogPath(n.ID)) {
		if ev.T == "decision" {
			saw = true
		}
	}
	if !saw {
		t.Fatal("decision audit missing after decision-first ordering")
	}
}

// Re-arm has the same barrier as disable and creates a distinct lease ID.
func TestAutoApproveRearmBarrierAndNewLease(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "rearm", "grok", "acp")
	inDeliver := make(chan struct{})
	releaseDeliver := make(chan struct{})
	order := make(chan string, 4)
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("inc:1"), clearOnDeliver: true,
	}
	slow := &channelDeliverProc{
		stubProc: stub,
		onEnter: func() {
			close(inDeliver)
			<-releaseDeliver
			order <- "deliver-done"
		},
	}
	v1 := a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
	if v1.Phase != "armed" || v1.Enabled != true {
		t.Fatalf("first enable = %+v", v1)
	}
	a.mu.Lock()
	lease1 := a.autoApprove[n.ID].LeaseID
	a.mu.Unlock()
	if lease1 == "" {
		t.Fatal("empty lease id")
	}

	decDone := make(chan struct{})
	go func() {
		a.maybeAutoApprove(n, slow)
		close(decDone)
	}()
	select {
	case <-inDeliver:
	case <-time.After(3 * time.Second):
		t.Fatal("decision never reached Deliver")
	}

	// Re-arm (enable again) must wait behind the in-flight decision.
	var v2 autoApproveView
	rearmDone := make(chan struct{})
	go func() {
		v2 = a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
		order <- "rearm-done"
		close(rearmDone)
	}()
	close(releaseDeliver)
	select {
	case <-decDone:
	case <-time.After(3 * time.Second):
		t.Fatal("decision did not finish")
	}
	select {
	case <-rearmDone:
	case <-time.After(3 * time.Second):
		t.Fatal("re-arm did not return")
	}
	var seq []string
	for len(seq) < 2 {
		select {
		case s := <-order:
			seq = append(seq, s)
		case <-time.After(3 * time.Second):
			t.Fatalf("order incomplete: %v", seq)
		}
	}
	if seq[0] != "deliver-done" || seq[1] != "rearm-done" {
		t.Fatalf("order = %v, want deliver-done then rearm-done", seq)
	}
	if !v2.Enabled || v2.Phase != "armed" {
		t.Fatalf("re-arm view = %+v", v2)
	}
	a.mu.Lock()
	lease2 := a.autoApprove[n.ID].LeaseID
	a.mu.Unlock()
	if lease2 == "" || lease2 == lease1 {
		t.Fatalf("re-arm lease = %q, want distinct from %q", lease2, lease1)
	}
	// Old lease must not deliver again under the new lease without a new request.
	// The old request was already delivered; pending cleared.
	if stub.deliverCalls != 1 {
		t.Fatalf("deliverCalls = %d, want 1", stub.deliverCalls)
	}
}

// pendingBlockProc runs onPending before returning Pending (race coordination).
type pendingBlockProc struct {
	*stubProc
	onPending func()
}

func (p *pendingBlockProc) Pending(id string) (PendingPermission, bool) {
	if p.onPending != nil {
		p.onPending()
	}
	return p.stubProc.Pending(id)
}

// channelDeliverProc runs onEnter inside Deliver before delegating.
type channelDeliverProc struct {
	*stubProc
	onEnter func()
}

func (c *channelDeliverProc) Deliver(nodeID, optID string) error {
	if c.onEnter != nil {
		c.onEnter()
	}
	return c.stubProc.Deliver(nodeID, optID)
}

// ---------- P1 review: re-arm cutoff + lifecycle barriers ----------

// HTTP re-arm while A is delivering: snapshot is taken after the gate, so B
// (which became current while re-arm waited) stays manual.
func TestHTTPRearmCutoffExcludesRequestThatArrivedDuringWait(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "rearm-http", "grok", "acp")
	inDeliver := make(chan struct{})
	releaseDeliver := make(chan struct{})
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		// First lease: A (seq 1) is post-enable (cutoff maxSeq 0).
		pending: allowPending("inc:1"), clearOnDeliver: true,
		boundarySet: true, boundaryIncarn: "inc", boundaryMaxSeq: 0,
	}
	v1 := a.enableAutoApprove(n.ID, stub)
	if v1.Phase != "armed" {
		t.Fatalf("first enable = %+v", v1)
	}
	// After enable, advance boundary so a concurrent re-arm can observe B.
	stub.mu.Lock()
	stub.boundaryMaxSeq = 1
	stub.mu.Unlock()
	// Old lease starts delivering A.
	slow := &channelDeliverProc{
		stubProc: stub,
		onEnter: func() {
			close(inDeliver)
			<-releaseDeliver
		},
	}
	decDone := make(chan struct{})
	go func() {
		a.maybeAutoApprove(n, slow)
		close(decDone)
	}()
	select {
	case <-inDeliver:
	case <-time.After(3 * time.Second):
		t.Fatal("decision never reached Deliver")
	}
	// While A is in deliver, B becomes the next pending and boundary advances.
	// Re-arm waits on the gate; when it wins, PermissionBoundary must see B.
	stub.mu.Lock()
	stub.pending = allowPending("inc:2")
	stub.hasPending = true
	stub.boundaryMaxSeq = 2
	stub.mu.Unlock()

	var v2 autoApproveView
	rearmDone := make(chan struct{})
	go func() {
		// Real HTTP handler path would call enableAutoApprove with a.proc;
		// exercise that function with the stub so the snapshot is gate-ordered.
		v2 = a.enableAutoApprove(n.ID, stub)
		close(rearmDone)
	}()
	close(releaseDeliver)
	select {
	case <-decDone:
	case <-time.After(3 * time.Second):
		t.Fatal("decision did not finish")
	}
	select {
	case <-rearmDone:
	case <-time.After(3 * time.Second):
		t.Fatal("re-arm did not finish")
	}
	if !v2.Enabled || v2.Phase != "armed" {
		t.Fatalf("re-arm view = %+v", v2)
	}
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	if st == nil || st.EnableIncarn != "inc" || st.EnableMaxSeq != 2 {
		t.Fatalf("re-arm cutoff = %+v, want inc/2 (includes B)", st)
	}
	a.mu.Unlock()
	// B must stay manual under the new lease.
	stub.deliverCalls = 0
	stub.prepareCalls = 0
	stub.clearOnDeliver = false
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 0 {
		t.Fatalf("B (seq 2 at re-arm cutoff) must stay manual; del=%d", stub.deliverCalls)
	}
	// C created after re-arm is eligible.
	stub.pending = allowPending("inc:3")
	stub.hasPending = true
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 1 {
		t.Fatalf("post-rearm request should deliver; del=%d", stub.deliverCalls)
	}
}

// Codex queue: B queued behind A before re-arm; after A finishes and re-arm
// returns, B stays manual (boundary maxSeq covers the whole queue).
func TestRearmExcludesQueuedBehindHead(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "queue-b", "codex", "codex")
	// At enable, pendingSeq is already 2 (A head + B queued).
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending:     allowPending("inc:1"), // head is A
		boundarySet: true, boundaryIncarn: "inc", boundaryMaxSeq: 2,
	}
	v := a.enableAutoApprove(n.ID, stub)
	if v.Phase != "armed" {
		t.Fatalf("enable = %+v", v)
	}
	a.mu.Lock()
	if a.autoApprove[n.ID].EnableMaxSeq != 2 {
		t.Fatalf("cutoff maxSeq = %d, want 2", a.autoApprove[n.ID].EnableMaxSeq)
	}
	a.mu.Unlock()
	// A is pre-enable.
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 0 {
		t.Fatal("A must stay manual")
	}
	// After A is answered externally, B becomes head with seq 2 — still pre-enable.
	stub.pending = allowPending("inc:2")
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 0 {
		t.Fatal("queued-at-enable B must stay manual after becoming head")
	}
}

// Delete during in-flight deliver: delete waits for the barrier, then no
// further decision appends land on an archived log.
func TestDeleteWaitsForAutoApproveBarrier(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "del-race", "pi", "acp")
	inDeliver := make(chan struct{})
	releaseDeliver := make(chan struct{})
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("inc:1"), clearOnDeliver: true,
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
	slow := &channelDeliverProc{
		stubProc: stub,
		onEnter:  func() { close(inDeliver); <-releaseDeliver },
	}
	decDone := make(chan struct{})
	go func() {
		a.maybeAutoApprove(n, slow)
		close(decDone)
	}()
	select {
	case <-inDeliver:
	case <-time.After(3 * time.Second):
		t.Fatal("decision never reached Deliver")
	}
	// Delete path: disarm first (what handleDeleteNode does before close/archive).
	disarmDone := make(chan struct{})
	go func() {
		a.disarmAutoApprove(n.ID)
		close(disarmDone)
	}()
	close(releaseDeliver)
	select {
	case <-decDone:
	case <-time.After(3 * time.Second):
		t.Fatal("decision did not finish")
	}
	select {
	case <-disarmDone:
	case <-time.After(3 * time.Second):
		t.Fatal("disarm did not finish after decision")
	}
	a.mu.Lock()
	if a.autoApprove[n.ID] != nil {
		t.Fatal("lease must be gone after delete barrier")
	}
	a.mu.Unlock()
	// No further auto delivery after barrier.
	stub.hasPending = true
	stub.pending = allowPending("inc:2")
	a.maybeAutoApprove(n, stub)
	// deliverCalls may be 1 from the first decision; must not grow.
	if stub.deliverCalls != 1 {
		t.Fatalf("post-delete deliverCalls = %d, want 1", stub.deliverCalls)
	}
}

// HTTP disable does not call Live/Pending (fast path).
func TestHTTPDisableSkipsManagerSnapshot(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "dis-fast", "grok", "acp")
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
	// No manager installed; disable must still succeed without panicking.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/nodes/"+n.ID+"/auto-approve", strings.NewReader(`{"enabled":false}`))
	r.SetPathValue("id", n.ID)
	a.handleAutoApprove(rec, r)
	if rec.Code != 200 {
		t.Fatalf("disable code = %d body %q", rec.Code, rec.Body)
	}
	a.mu.Lock()
	if a.autoApprove[n.ID] != nil {
		t.Fatal("lease must be off")
	}
	a.mu.Unlock()
}

// ---------- P1 re-review: delete barrier, empty boundary, poller lease-id ----------

// Malformed request IDs are always ineligible, even with an empty enable boundary.
func TestRequestCreatedAfterEnableFailClosed(t *testing.T) {
	// Empty boundary → never eligible.
	if requestCreatedAfterEnable("", 0, "inc:1") {
		t.Fatal("empty enable boundary must not treat any request as post-enable")
	}
	if requestCreatedAfterEnable("", 0, "malformed") {
		t.Fatal("malformed ID with empty boundary must be ineligible")
	}
	// Bound boundary → malformed still ineligible.
	if requestCreatedAfterEnable("inc", 0, "malformed") {
		t.Fatal("malformed ID must always be ineligible")
	}
	if requestCreatedAfterEnable("inc", 0, "") {
		t.Fatal("empty request ID must be ineligible")
	}
	if requestCreatedAfterEnable("inc", 0, "other:1") {
		t.Fatal("foreign incarnation must be ineligible")
	}
	if requestCreatedAfterEnable("inc", 5, "inc:5") {
		t.Fatal("seq at watermark must be pre-enable")
	}
	if !requestCreatedAfterEnable("inc", 5, "inc:6") {
		t.Fatal("seq after watermark must be post-enable")
	}
}

// Enable without a session (empty boundary), arm with pre-send incarnation A
// (maxSeq 0); A:1 is eligible, foreign incarnation B is not.
func TestPrimedArmBindsPresendBoundaryAndFencesIncarnation(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "bound-a", "grok", "acp")
	// Enable while idle with no manager boundary.
	if v := a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0); v.Phase != "primed" {
		t.Fatalf("enable = %+v", v)
	}
	// Pre-send bound to A/0; first permission A:1 is post-cutoff.
	a.armAutoApproveOnPrompt(n.ID, "A", 0, true)
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	if st == nil || st.EnableIncarn != "A" || st.EnableMaxSeq != 0 || st.Phase != autoPhaseArmed {
		t.Fatalf("arm bind = %+v, want armed A/0", st)
	}
	a.mu.Unlock()
	stubA := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("A:1"), clearOnDeliver: true,
	}
	a.maybeAutoApprove(n, stubA)
	if stubA.deliverCalls != 1 {
		t.Fatalf("A:1 after pre-send cutoff 0 must deliver; del=%d", stubA.deliverCalls)
	}
	// Session replaced by B (foreign incarnation) — must not auto-approve.
	stubB := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("B:1"),
	}
	a.maybeAutoApprove(n, stubB)
	if stubB.deliverCalls != 0 {
		t.Fatal("incarnation B must not qualify under lease bound to A")
	}
}

// Real handleDeleteNode holds the auto-gate through teardown: concurrent enable
// and poll-driven auto-approve cannot install a lease or deliver a decision
// that survives deletion, and the session log is not recreated after archival.
func TestHandleDeleteBlocksEnableAndDecision(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "del-live", "pi", "acp")
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("inc:1"), clearOnDeliver: true,
	}
	a.testProc = stub
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)

	inGate := make(chan struct{})
	releaseGate := make(chan struct{})
	a.deleteGateHook = func(id string) {
		if id != n.ID {
			t.Errorf("hook id = %q", id)
		}
		close(inGate)
		<-releaseGate
	}

	delDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("DELETE", "/api/nodes/"+n.ID, nil)
		r.SetPathValue("id", n.ID)
		a.handleDeleteNode(rec, r)
		delDone <- rec.Code
	}()

	select {
	case <-inGate:
	case <-time.After(3 * time.Second):
		t.Fatal("delete never acquired the auto-gate")
	}

	// Race enable (must wait on the gate) and maybeAutoApprove (pre-check
	// sees lease already cleared under the held gate → returns without deliver).
	enableDone := make(chan autoApproveView, 1)
	go func() {
		enableDone <- a.enableAutoApprove(n.ID, stub)
	}()
	// Spin maybeAutoApprove repeatedly while delete holds the gate.
	var pollDelivers atomic.Int32
	stopPoll := make(chan struct{})
	var pollWG sync.WaitGroup
	pollWG.Add(1)
	go func() {
		defer pollWG.Done()
		for {
			select {
			case <-stopPoll:
				return
			default:
				before := stub.deliverCalls
				a.maybeAutoApprove(n, stub)
				if stub.deliverCalls > before {
					pollDelivers.Add(1)
				}
			}
		}
	}()

	// Enable must not complete while the gate is held.
	select {
	case <-enableDone:
		t.Fatal("enable completed while delete still held the gate")
	case <-time.After(50 * time.Millisecond):
		// expected: blocked on gate
	}

	// Let delete finish teardown + archival.
	close(releaseGate)
	var code int
	select {
	case code = <-delDone:
	case <-time.After(3 * time.Second):
		t.Fatal("delete did not finish")
	}
	if code != 200 {
		t.Fatalf("delete code = %d, want 200", code)
	}
	close(stopPoll)
	pollWG.Wait()
	select {
	case v := <-enableDone:
		// After delete, node is gone → unsupported/off.
		if v.Enabled || v.Phase != "off" {
			t.Fatalf("post-delete enable = %+v, want off", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("enable did not unblock after delete")
	}
	if pollDelivers.Load() != 0 {
		t.Fatalf("poll auto-approve delivered during delete window: %d", pollDelivers.Load())
	}

	a.mu.Lock()
	if a.autoApprove[n.ID] != nil {
		t.Fatalf("lease survived delete: %+v", a.autoApprove[n.ID])
	}
	if a.byID[n.ID] != nil {
		t.Fatal("node still in byID after delete")
	}
	a.mu.Unlock()
	if stub.deliverCalls != 0 {
		t.Fatalf("no decision may deliver across delete; del=%d", stub.deliverCalls)
	}
	// Log archived: live path gone.
	if _, err := os.Stat(a.sessionLogPath(n.ID)); !os.IsNotExist(err) {
		t.Fatalf("live session log still present after delete: %v", err)
	}
	// No recreation of the live log after archival.
	// (maybeAutoApprove must not have appended post-archive.)
	if a.sessionLogExists(n.ID) {
		t.Fatal("session log recreated after archival")
	}
}

// Poller disarm-first then re-arm: the observed lease is cleared; re-arm installs a new one.
func TestPollerDisarmThenRearm(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "poll-1", "grok", "acp")
	v := a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
	if !v.Enabled {
		t.Fatal("setup")
	}
	a.mu.Lock()
	lease1 := a.autoApprove[n.ID].LeaseID
	a.mu.Unlock()

	// Disarm the observed lease (poller path).
	a.disarmAutoApproveIfLease(n.ID, lease1)
	a.mu.Lock()
	if a.autoApprove[n.ID] != nil {
		t.Fatal("lease should be cleared by matching disarm")
	}
	a.mu.Unlock()

	// Re-arm installs a distinct lease.
	v2 := a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
	if !v2.Enabled {
		t.Fatal("re-arm failed")
	}
	a.mu.Lock()
	lease2 := a.autoApprove[n.ID].LeaseID
	a.mu.Unlock()
	if lease2 == "" || lease2 == lease1 {
		t.Fatalf("re-arm lease = %q, want new (not %q)", lease2, lease1)
	}
}

// Re-arm first, then a stale poller disarm for the old lease ID must not erase the new lease.
func TestStalePollerDisarmDoesNotEraseRearm(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "poll-2", "codex", "codex")
	v1 := a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
	a.mu.Lock()
	lease1 := a.autoApprove[n.ID].LeaseID
	a.mu.Unlock()
	if lease1 == "" || !v1.Enabled {
		t.Fatal("setup")
	}

	// Re-arm (new lease) before the stale disarm runs.
	v2 := a.setAutoApproveEnabled(n.ID, true, "active", "inc", 1)
	a.mu.Lock()
	lease2 := a.autoApprove[n.ID].LeaseID
	a.mu.Unlock()
	if !v2.Enabled || lease2 == "" || lease2 == lease1 {
		t.Fatalf("re-arm lease = %q (old %q)", lease2, lease1)
	}

	// Stale poller disarm observed lease1 — must leave lease2 intact.
	a.disarmAutoApproveIfLease(n.ID, lease1)
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	a.mu.Unlock()
	if st == nil || st.LeaseID != lease2 {
		t.Fatalf("stale disarm wiped re-arm: %+v, want lease %q", st, lease2)
	}

	// Empty observation must not wipe either.
	a.disarmAutoApproveIfLease(n.ID, "")
	a.mu.Lock()
	st = a.autoApprove[n.ID]
	a.mu.Unlock()
	if st == nil || st.LeaseID != lease2 {
		t.Fatalf("empty-lease disarm wiped state: %+v", st)
	}
}

// Pre-send boundary is sampled before Send. If Send publishes inc:1 before
// returning, arm still uses the pre-send maxSeq (0) so inc:1 remains eligible.
func TestAcceptStructuredPromptPresendCutoffKeepsFirstPermissionEligible(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "send-race", "grok", "acp")
	stub := &stubProc{
		live: "quiet", hasSession: true,
		boundarySet: true, boundaryIncarn: "inc", boundaryMaxSeq: 0,
	}
	// During Send, the turn issues permission inc:1 and advances the boundary.
	stub.sendHook = func(s *stubProc) {
		s.pending = allowPending("inc:1")
		s.hasPending = true
		s.boundaryMaxSeq = 1
		s.live = "active"
	}
	if v := a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0); v.Phase != "primed" {
		t.Fatalf("enable = %+v", v)
	}
	if err := a.acceptStructuredPrompt(n, stub, "do it"); err != nil {
		t.Fatal(err)
	}
	if stub.sendCalls != 1 {
		t.Fatalf("sendCalls = %d", stub.sendCalls)
	}
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	if st == nil || st.Phase != autoPhaseArmed || st.EnableIncarn != "inc" || st.EnableMaxSeq != 0 {
		t.Fatalf("after accept state = %+v, want armed inc/0 (pre-send)", st)
	}
	a.mu.Unlock()
	// First permission of the turn is eligible under pre-send cutoff 0.
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 1 {
		t.Fatalf("inc:1 must deliver after pre-send arm; del=%d", stub.deliverCalls)
	}
}

// Real handleSend path: same pre-send cutoff guarantee via HTTP.
func TestHandleSendArmsWithPresendCutoff(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "http-send", "pi", "acp")
	stub := &stubProc{
		live: "quiet", hasSession: true,
		boundarySet: true, boundaryIncarn: "inc", boundaryMaxSeq: 0,
	}
	stub.sendHook = func(s *stubProc) {
		s.pending = allowPending("inc:1")
		s.hasPending = true
		s.boundaryMaxSeq = 1
		s.live = "active"
	}
	a.testProc = stub
	a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/nodes/"+n.ID+"/send",
		strings.NewReader(`{"text":"run tests"}`))
	r.SetPathValue("id", n.ID)
	a.handleSend(rec, r)
	if rec.Code != 200 {
		t.Fatalf("handleSend code = %d body %q", rec.Code, rec.Body)
	}
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	if st == nil || st.Phase != autoPhaseArmed || st.EnableMaxSeq != 0 {
		t.Fatalf("after send state = %+v, want armed maxSeq 0", st)
	}
	lease := st.LeaseID
	a.mu.Unlock()
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 1 {
		t.Fatalf("first permission must deliver; del=%d", stub.deliverCalls)
	}
	// Lease identity preserved through arm.
	a.mu.Lock()
	if a.autoApprove[n.ID] == nil || a.autoApprove[n.ID].LeaseID != lease {
		t.Fatalf("lease rotated unexpectedly: %+v", a.autoApprove[n.ID])
	}
	a.mu.Unlock()
}

// Arm must not reset a different primed lease (lease-ID mismatch guard).
// L2 stays primed under the gate while we invoke the locked helper with L1.
func TestArmDoesNotResetForeignLease(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "foreign-lease", "codex", "codex")
	// Primed lease L1.
	v1 := a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0)
	a.mu.Lock()
	l1 := a.autoApprove[n.ID].LeaseID
	a.mu.Unlock()
	if !v1.Enabled || l1 == "" {
		t.Fatal("setup primed")
	}
	// Concurrent enable installs a different primed lease L2 (still idle).
	v2 := a.setAutoApproveEnabled(n.ID, true, "quiet", "old", 3)
	a.mu.Lock()
	l2 := a.autoApprove[n.ID].LeaseID
	inc2 := a.autoApprove[n.ID].EnableIncarn
	max2 := a.autoApprove[n.ID].EnableMaxSeq
	phase := a.autoApprove[n.ID].Phase
	a.mu.Unlock()
	if !v2.Enabled || l2 == l1 || phase != autoPhasePrimed {
		t.Fatalf("setup L2 = %q phase %s (L1 %q)", l2, phase, l1)
	}
	// Hold the gate and arm as if finishing the old prompt with L1's id.
	g := a.autoGateFor(n.ID)
	g.Lock()
	a.armAutoApproveOnPromptLocked(n.ID, l1, "inc", 0, true)
	g.Unlock()
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	gotID, gotPhase, gotInc, gotMax := "", autoPhaseOff, "", uint64(0)
	if st != nil {
		gotID, gotPhase, gotInc, gotMax = st.LeaseID, st.Phase, st.EnableIncarn, st.EnableMaxSeq
	}
	a.mu.Unlock()
	if gotID != l2 || gotPhase != autoPhasePrimed || gotInc != inc2 || gotMax != max2 {
		t.Fatalf("arm reset foreign primed lease: id=%q phase=%s bound=%s/%d, want L2 %q primed %s/%d",
			gotID, gotPhase, gotInc, gotMax, l2, inc2, max2)
	}
}

// Concurrent enable waits behind acceptStructuredPrompt's gate. After both
// complete, enable's new lease is current (L2). Pre-send arming of L1 is
// covered by TestAcceptStructuredPromptPresendCutoff / TestHandleSendArms.
func TestEnableWaitsBehindAcceptStructuredPrompt(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "enable-wait", "grok", "acp")
	inSend := make(chan struct{})
	releaseSend := make(chan struct{})
	// Capture L1 after arm while accept still holds the gate (before unlock).
	armedL1 := make(chan struct {
		lease string
		phase autoApprovePhase
		max   uint64
	}, 1)
	stub := &stubProc{
		live: "quiet", hasSession: true,
		boundarySet: true, boundaryIncarn: "inc", boundaryMaxSeq: 0,
	}
	stub.sendHook = func(s *stubProc) {
		close(inSend)
		<-releaseSend
		s.pending = allowPending("inc:1")
		s.hasPending = true
		s.boundaryMaxSeq = 1
	}
	a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0)
	a.mu.Lock()
	l1 := a.autoApprove[n.ID].LeaseID
	a.mu.Unlock()

	a.acceptArmHook = func(id string) {
		if id != n.ID {
			return
		}
		// Still holding the auto-gate here (called from acceptStructuredPrompt).
		a.mu.Lock()
		st := a.autoApprove[id]
		var snap struct {
			lease string
			phase autoApprovePhase
			max   uint64
		}
		if st != nil {
			snap.lease, snap.phase, snap.max = st.LeaseID, st.Phase, st.EnableMaxSeq
		}
		a.mu.Unlock()
		select {
		case armedL1 <- snap:
		default:
		}
	}

	acceptDone := make(chan error, 1)
	go func() {
		acceptDone <- a.acceptStructuredPrompt(n, stub, "go")
	}()
	select {
	case <-inSend:
	case <-time.After(3 * time.Second):
		t.Fatal("Send never entered")
	}

	// Enable must wait on the auto-gate held across Send.
	enableDone := make(chan autoApproveView, 1)
	go func() {
		enableDone <- a.enableAutoApprove(n.ID, stub)
	}()
	select {
	case <-enableDone:
		t.Fatal("enable completed while accept still held the gate")
	case <-time.After(40 * time.Millisecond):
	}

	close(releaseSend)

	// Observe L1 while accept still holds the gate (hook fires before unlock).
	var mid struct {
		lease string
		phase autoApprovePhase
		max   uint64
	}
	select {
	case mid = <-armedL1:
	case <-time.After(3 * time.Second):
		t.Fatal("acceptArmHook never fired")
	}
	if mid.lease != l1 || mid.phase != autoPhaseArmed || mid.max != 0 {
		t.Fatalf("mid-accept under gate = %+v, want L1 %q armed maxSeq 0", mid, l1)
	}

	// Wait for both operations; order between acceptDone and enableDone is free
	// after the gate is released.
	var acceptErr error
	var v2 autoApproveView
	for acceptDone != nil || enableDone != nil {
		select {
		case err := <-acceptDone:
			acceptErr = err
			acceptDone = nil
		case v := <-enableDone:
			v2 = v
			enableDone = nil
		case <-time.After(3 * time.Second):
			t.Fatal("accept or enable did not finish")
		}
	}
	if acceptErr != nil {
		t.Fatal(acceptErr)
	}
	if !v2.Enabled {
		t.Fatalf("enable after accept = %+v", v2)
	}
	// Final state: enable's new lease (L2), not L1.
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	finalID := ""
	if st != nil {
		finalID = st.LeaseID
	}
	a.mu.Unlock()
	if finalID == "" || finalID == l1 {
		t.Fatalf("final lease = %q, want new L2 (not L1 %q)", finalID, l1)
	}
}

// Unavailable pre-send boundary clears any older cutoff so eligibility fails closed.
func TestArmWithoutBoundClearsPriorCutoff(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "clear-bound", "pi", "acp")
	// Prime with an older non-empty boundary (as if enable saw session A).
	a.setAutoApproveEnabled(n.ID, true, "quiet", "A", 7)
	a.mu.Lock()
	if a.autoApprove[n.ID].EnableIncarn != "A" || a.autoApprove[n.ID].EnableMaxSeq != 7 {
		t.Fatalf("setup bound = %+v", a.autoApprove[n.ID])
	}
	a.mu.Unlock()
	// Arm without a trustworthy pre-send boundary.
	a.armAutoApproveOnPrompt(n.ID, "", 0, false)
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	phase, inc, max := autoPhaseOff, "", uint64(0)
	if st != nil {
		phase, inc, max = st.Phase, st.EnableIncarn, st.EnableMaxSeq
	}
	a.mu.Unlock()
	if phase != autoPhaseArmed || inc != "" || max != 0 {
		t.Fatalf("after unbound arm = phase %s bound %q/%d, want armed empty", phase, inc, max)
	}
	// A:8 must stay manual under empty cutoff.
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("A:8"), clearOnDeliver: true,
	}
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 0 {
		t.Fatalf("A:8 must remain manual with empty cutoff; del=%d", stub.deliverCalls)
	}
}
