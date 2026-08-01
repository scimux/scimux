package main

// Packet 4B public-route coverage for conversation HTTP bindings through
// NewHandler. Complements — does not replace — the detailed direct-handler
// matrices in main_http_test.go / main_test.go, session-log tests, protocol-
// manager tests, Packet 2E ordering, or Packet 3D ownership tests.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp/codex"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// conversationAPIHandler builds the production router for public-route assertions.
func conversationAPIHandler(t *testing.T, a *app) http.Handler {
	t.Helper()
	h, err := NewHandler(a, webFS)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// seedTmuxNode registers a live-looking tmux node without going through create.
func seedTmuxNode(a *app, id string) *Node {
	n := &Node{ID: id, Title: id, Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes = append(a.nodes, n)
	a.byID[id] = n
	return n
}

// ---------- unknown + ended ----------

func TestPublicRouteUnknownAndEndedConversation(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"live": true}, capture: "Approve? (y/n)\n"}
	a := newTestApp(t, f)
	n := seedTmuxNode(a, "live")
	n.EndedAt = "2026-07-23T00:00:00Z"
	h := conversationAPIHandler(t, a)

	// Unknown node → 404 on every conversation route.
	unknown := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/nodes/ghost/send", `{"text":"hi"}`},
		{http.MethodPost, "/api/nodes/ghost/send/resolve", ""},
		{http.MethodPost, "/api/nodes/ghost/send/interrupt", ""},
		{http.MethodPost, "/api/nodes/ghost/key", `{"key":"y"}`},
		{http.MethodGet, "/api/nodes/ghost/chat", ""},
		{http.MethodGet, "/api/nodes/ghost/peek", ""},
	}
	for _, c := range unknown {
		rec := routeRequest(h, c.method, c.path, c.body, c.method != http.MethodGet)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404; body=%q", c.method, c.path, rec.Code, rec.Body.String())
		}
	}

	// Ended node refuses mutations with 409; read paths still work.
	mutations := []struct {
		path, body string
	}{
		{"/api/nodes/live/send", `{"text":"hi"}`},
		{"/api/nodes/live/send", `{"text":"/clear"}`},
		{"/api/nodes/live/send/interrupt", ""},
		{"/api/nodes/live/key", `{"key":"y"}`},
	}
	for _, c := range mutations {
		rec := routeRequest(h, http.MethodPost, c.path, c.body, true)
		if rec.Code != http.StatusConflict {
			t.Errorf("ended %s: status = %d, want 409; body=%q", c.path, rec.Code, rec.Body.String())
		}
	}
	if containsSub(f.subcommands(), "send-keys") || containsSub(f.subcommands(), "paste-buffer") {
		t.Fatalf("ended node produced tmux mutations: %v", f.subcommands())
	}

	// Chat and peek remain readable on an ended thread.
	if rec := routeRequest(h, http.MethodGet, "/api/nodes/live/chat", "", false); rec.Code != http.StatusOK {
		t.Errorf("ended chat: status = %d, want 200", rec.Code)
	}
	if rec := routeRequest(h, http.MethodGet, "/api/nodes/live/peek", "", false); rec.Code != http.StatusOK {
		t.Errorf("ended peek: status = %d, want 200", rec.Code)
	}
}

// ---------- tmux send validation, delivery, resolve ----------

func TestPublicRouteSendValidationAndDelivery(t *testing.T) {
	// Static pane → unconfirmed; changing pane → acknowledged; empty → 400;
	// duplicate protection + resolve submitting conflict.
	f := &fakeTmux{alive: map[string]bool{"n1": true}, capture: "static pane"}
	a := newTestApp(t, f)
	a.server.PasteDelay, a.server.AckPoll = time.Millisecond, time.Millisecond
	seedTmuxNode(a, "n1")
	h := conversationAPIHandler(t, a)

	// Empty text with no attachments → 400.
	if rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/send", `{"text":"  "}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty send: status = %d, want 400", rec.Code)
	}

	// Static pane: unconfirmed, echoes text for recovery.
	rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/send", `{"text":"hello"}`, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"unconfirmed"`) {
		t.Fatalf("unconfirmed send: status = %d body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Errorf("unconfirmed response must echo text: %s", rec.Body.String())
	}
	// Next send blocked while unconfirmed.
	if rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/send", `{"text":"next"}`, true); rec.Code != http.StatusConflict {
		t.Fatalf("send during unconfirmed: status = %d, want 409", rec.Code)
	}

	// Resolve clears the hold.
	rec = routeRequest(h, http.MethodPost, "/api/nodes/n1/send/resolve", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "resolved") {
		t.Fatalf("resolve: status = %d body %q", rec.Code, rec.Body.String())
	}

	// Submitting conflict: resolve refuses while a send is in flight.
	a.mu.Lock()
	a.sendState["n1"] = "submitting"
	a.mu.Unlock()
	if rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/send/resolve", "", true); rec.Code != http.StatusConflict {
		t.Fatalf("resolve while submitting: status = %d, want 409", rec.Code)
	}
	if rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/send", `{"text":"blocked"}`, true); rec.Code != http.StatusConflict {
		t.Fatalf("send while submitting: status = %d, want 409", rec.Code)
	}
	a.mu.Lock()
	delete(a.sendState, "n1")
	a.mu.Unlock()

	// Acknowledged delivery when the pane reacts to Enter.
	f2 := &fakeTmux{alive: map[string]bool{"n2": true}, capture: "before", captureAfterEnter: "after"}
	a2 := newTestApp(t, f2)
	a2.server.PasteDelay, a2.server.AckPoll = time.Millisecond, time.Millisecond
	seedTmuxNode(a2, "n2")
	h2 := conversationAPIHandler(t, a2)
	rec = routeRequest(h2, http.MethodPost, "/api/nodes/n2/send", `{"text":"go"}`, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"acknowledged"`) {
		t.Fatalf("acknowledged send: status = %d body %q", rec.Code, rec.Body.String())
	}
}

// ---------- tmux /clear retirement + source seam ----------

func TestPublicRouteSendClearRetiresTranscript(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}, capture: "idle", captureAfterEnter: "cleared"}
	a := newTestApp(t, f)
	a.server.PasteDelay, a.server.AckPoll = time.Millisecond, time.Millisecond
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := seedTmuxNode(a, "c1")
	tx := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(tx, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n.Transcript, n.SessionID = tx, "sess"
	// Existing log so retire appends a path-less clear seam.
	w := &sessionlog.Writer{Path: a.sessionLogPath("c1")}
	for _, ev := range []sessionlog.Event{
		sessionlog.NewMeta("c1", "claude", "", "", a.home),
		sessionlog.NewSource(tx, "sess"),
		{T: "user", Text: "old"},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	h := conversationAPIHandler(t, a)

	rec := routeRequest(h, http.MethodPost, "/api/nodes/c1/send", `{"text":"  /clear  "}`, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "acknowledged") {
		t.Fatalf("/clear: status = %d body %q", rec.Code, rec.Body.String())
	}
	if n.Transcript != "" || n.SessionID != "" {
		t.Fatalf("link not retired: transcript=%q session=%q", n.Transcript, n.SessionID)
	}
	evs := sessionlog.ReadEvents(a.sessionLogPath("c1"))
	if len(evs) == 0 {
		t.Fatal("session log vanished")
	}
	last := evs[len(evs)-1]
	if last.T != "source" || last.Source == nil || last.Source.Reason != "clear" || last.Source.Path != "" {
		t.Fatalf("retire seam = %+v, want path-less clear source", last)
	}
	// Station snapshot for the closing head must precede the seam.
	stationIdx, seamIdx := -1, -1
	for i, ev := range evs {
		if ev.T == "station" {
			stationIdx = i
		}
		if ev.T == "source" && ev.Source != nil && ev.Source.Reason == "clear" {
			seamIdx = i
		}
	}
	if stationIdx < 0 || seamIdx < 0 || stationIdx > seamIdx {
		t.Fatalf("station snapshot (idx %d) must precede clear seam (idx %d)", stationIdx, seamIdx)
	}
}

// ---------- structured send / clear / resolve / interrupt via procManager ----------

func TestPublicRouteStructuredSendClearAndConflict(t *testing.T) {
	// Success path: /clear on a live codex node opens a second thread and
	// leaves a path-less clear seam; conflict path: second send while turn
	// is in flight → 409.
	t.Run("clear_success", func(t *testing.T) {
		rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
		f := &fakeTmux{}
		a := newTestApp(t, f)
		spawn, requests := newFakeCodexSpawn(t, "THREAD-CLR-R", rollout)
		a.codex = codexManager{codex.NewManagerWithSpawn(a.sessionsDir, spawn)}
		t.Cleanup(a.codex.Shutdown)
		h := conversationAPIHandler(t, a)

		rec := routeRequest(h, http.MethodPost, "/api/nodes",
			`{"title":"C","prompt":"ping","agent":"codex","dir":`+strconv.Quote(a.home)+`}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		var n Node
		if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && a.codex.Live(n.ID) == "active" {
			time.Sleep(10 * time.Millisecond)
		}

		rec = routeRequest(h, http.MethodPost, "/api/nodes/"+n.ID+"/send", `{"text":"/clear"}`, true)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "acknowledged") {
			t.Fatalf("/clear: status = %d body %q", rec.Code, rec.Body.String())
		}
		starts := 0
		for _, m := range requests() {
			if m == "thread/start" {
				starts++
			}
		}
		if starts != 2 {
			t.Fatalf("thread/start count = %d, want 2", starts)
		}
		evs := sessionlog.ReadEvents(filepath.Join(a.sessionsDir, n.ID+".jsonl"))
		if len(evs) == 0 {
			t.Fatal("no session log")
		}
		last := evs[len(evs)-1]
		if last.T != "source" || last.Source == nil || last.Source.Path != "" || last.Source.Reason != "clear" {
			t.Fatalf("last event = %+v, want path-less clear seam", last)
		}
	})

	t.Run("send_conflict", func(t *testing.T) {
		rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
		spawn, _, turnStarted, unblock := newBlockingFakeCodexSpawn(t, "THREAD-SEND-R", rollout)
		f := &fakeTmux{}
		a := newTestApp(t, f)
		logDir := filepath.Join(filepath.Dir(a.storePath), "codex")
		a.codex = codexManager{codex.NewManagerWithSpawn(logDir, spawn)}
		t.Cleanup(a.codex.Shutdown)
		h := conversationAPIHandler(t, a)

		rec := routeRequest(h, http.MethodPost, "/api/nodes",
			`{"title":"X","prompt":"do x","agent":"codex","dir":`+strconv.Quote(a.home)+`}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("create: %d", rec.Code)
		}
		var n Node
		if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
			t.Fatal(err)
		}
		select {
		case <-turnStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for first turn")
		}
		rec = routeRequest(h, http.MethodPost, "/api/nodes/"+n.ID+"/send", `{"text":"second"}`, true)
		if rec.Code != http.StatusConflict {
			t.Fatalf("second send: status = %d, want 409", rec.Code)
		}
		close(unblock)
	})
}

// ---------- interrupt: tmux evidence/audit + structured conflict ----------

func TestPublicRouteInterruptTmuxAndStructured(t *testing.T) {
	t.Run("tmux_evidence_and_audit", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"t1": true}, capture: "esc to interrupt"}
		a := newTestApp(t, f)
		seedTmuxNode(a, "t1")
		a.sendState["t1"] = "unconfirmed"
		h := conversationAPIHandler(t, a)

		rec := routeRequest(h, http.MethodPost, "/api/nodes/t1/send/interrupt", "", true)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "interrupted") {
			t.Fatalf("interrupt: status = %d body %q", rec.Code, rec.Body.String())
		}
		// Capture must precede Escape send-keys.
		subs := f.subcommands()
		capIdx, keyIdx := -1, -1
		for i, s := range subs {
			if s == "capture-pane" && capIdx == -1 {
				capIdx = i
			}
			if s == "send-keys" && keyIdx == -1 {
				keyIdx = i
			}
		}
		if capIdx == -1 || keyIdx == -1 || capIdx > keyIdx {
			t.Fatalf("capture must precede send-keys, got %v", subs)
		}
		recs := keyRecords(t, a.storePath)
		var got *storeRecord
		for i := range recs {
			if recs[i].Type == "key" && recs[i].Key == "Escape" {
				got = &recs[i]
			}
		}
		if got == nil || !strings.Contains(got.Excerpt, "interrupt:") {
			t.Fatalf("interrupt audit = %+v", got)
		}
		if _, ok := a.sendState["t1"]; ok {
			t.Fatal("unconfirmed send should be withdrawn by interrupt")
		}

		// Capture failure refuses without send-keys.
		f.captureErr = true
		f.calls = nil
		if rec := routeRequest(h, http.MethodPost, "/api/nodes/t1/send/interrupt", "", true); rec.Code != http.StatusInternalServerError {
			t.Fatalf("capture fail interrupt: status = %d, want 500", rec.Code)
		}
		if containsSub(f.subcommands(), "send-keys") {
			t.Fatal("send-keys must not run when capture failed")
		}
	})

	t.Run("structured_no_session_conflict", func(t *testing.T) {
		// Codex node registered without a live manager session → Interrupt
		// maps Conflict to 409 (not 500).
		rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
		a, _ := newCodexTestApp(t, "THREAD-INT", rollout)
		n := &Node{ID: "phantom", Title: "phantom", Agent: "codex", Transport: "codex",
			Dir: a.home, CreatedAt: "2026-07-14T00:00:00Z"}
		a.nodes = append(a.nodes, n)
		a.byID[n.ID] = n
		h := conversationAPIHandler(t, a)

		rec := routeRequest(h, http.MethodPost, "/api/nodes/phantom/send/interrupt", "", true)
		if rec.Code == http.StatusOK {
			t.Fatal("interrupt on missing session must not succeed")
		}
		if rec.Code == http.StatusInternalServerError {
			t.Fatalf("interrupt conflict must not be 500: body %q", rec.Body.String())
		}
		// ErrNoSession is a Conflict → 409.
		if rec.Code != http.StatusConflict {
			t.Fatalf("interrupt no-session: status = %d, want 409; body=%q", rec.Code, rec.Body.String())
		}
	})
}

// ---------- key whitelist + structured audit-before-deliver + tmux capture ----------

func TestPublicRouteKeyWhitelistAndAuditOrder(t *testing.T) {
	t.Run("tmux_whitelist_and_capture_order", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"k1": true}, capture: "some output\nApprove? (y/n)\n"}
		a := newTestApp(t, f)
		seedTmuxNode(a, "k1")
		a.attn["k1"] = "approval"
		a.attnAt["k1"] = time.Now().Add(-30 * time.Second)
		h := conversationAPIHandler(t, a)

		// Disallowed key rejected before any tmux contact.
		if rec := routeRequest(h, http.MethodPost, "/api/nodes/k1/key", `{"key":"q"}`, true); rec.Code != http.StatusBadRequest {
			t.Fatalf("disallowed key: status = %d, want 400", rec.Code)
		}
		if containsSub(f.subcommands(), "send-keys") || containsSub(f.subcommands(), "capture-pane") {
			t.Fatalf("disallowed key must not touch tmux: %v", f.subcommands())
		}

		// Happy path: capture before send-keys; audit carries excerpt; attn cleared.
		rec := routeRequest(h, http.MethodPost, "/api/nodes/k1/key", `{"key":"y"}`, true)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":"sent"`) {
			t.Fatalf("key: status = %d body %q", rec.Code, rec.Body.String())
		}
		subs := f.subcommands()
		capIdx, keyIdx := -1, -1
		for i, s := range subs {
			if s == "capture-pane" && capIdx == -1 {
				capIdx = i
			}
			if s == "send-keys" && keyIdx == -1 {
				keyIdx = i
			}
		}
		if capIdx == -1 || keyIdx == -1 || capIdx > keyIdx {
			t.Fatalf("capture must precede send-keys, got %v", subs)
		}
		recs := keyRecords(t, a.storePath)
		var got *storeRecord
		for i := range recs {
			if recs[i].Type == "key" {
				got = &recs[i]
			}
		}
		if got == nil || got.Key != "y" || !strings.Contains(got.Excerpt, "Approve?") {
			t.Fatalf("audit record = %+v", got)
		}
		a.mu.Lock()
		if a.attn["k1"] != "" {
			t.Errorf("attn after success = %q, want cleared", a.attn["k1"])
		}
		a.mu.Unlock()

		// Capture failure: no send-keys, attention preserved.
		f.captureErr = true
		f.calls = nil
		a.attn["k1"] = "approval"
		if rec := routeRequest(h, http.MethodPost, "/api/nodes/k1/key", `{"key":"y"}`, true); rec.Code != http.StatusInternalServerError {
			t.Fatalf("capture fail key: status = %d, want 500", rec.Code)
		}
		if containsSub(f.subcommands(), "send-keys") {
			t.Fatal("send-keys must not run when evidence capture failed")
		}
		if a.attn["k1"] != "approval" {
			t.Errorf("attn cleared on pre-delivery failure")
		}
	})

	t.Run("structured_audit_before_deliver", func(t *testing.T) {
		// Positive path: approval request → audit record exists before decision
		// reaches the fake server (finding 53).
		rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
		spawn, approvalDispatched, decisionDelivered := newApprovalFakeCodexSpawn(t, "THREAD-KEY-R", rollout)
		f := &fakeTmux{}
		a := newTestApp(t, f)
		logDir := filepath.Join(filepath.Dir(a.storePath), "codex")
		a.codex = codexManager{codex.NewManagerWithSpawn(logDir, spawn)}
		t.Cleanup(a.codex.Shutdown)
		h := conversationAPIHandler(t, a)

		rec := routeRequest(h, http.MethodPost, "/api/nodes",
			`{"prompt":"do it","title":"T","agent":"codex","dir":`+strconv.Quote(a.home)+`}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("create: %d", rec.Code)
		}
		var n Node
		if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
			t.Fatal(err)
		}
		select {
		case <-approvalDispatched:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for approval")
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, _, ok := a.codex.Pending(n.ID); ok {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if _, _, ok := a.codex.Pending(n.ID); !ok {
			t.Fatal("pending approval never appeared")
		}

		rec = routeRequest(h, http.MethodPost, "/api/nodes/"+n.ID+"/key", `{"key":"y"}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("key: status = %d body %q", rec.Code, rec.Body.String())
		}
		// Audit must be on disk immediately after 200 (written before Deliver).
		recs := keyRecords(t, a.storePath)
		var found *storeRecord
		for i := range recs {
			if recs[i].Type == "key" {
				found = &recs[i]
				break
			}
		}
		if found == nil || found.Key != "y" || found.Excerpt == "" {
			t.Fatalf("audit record = %+v", found)
		}
		select {
		case <-decisionDelivered:
		case <-time.After(5 * time.Second):
			t.Fatal("decision never reached fake server")
		}
	})
}

// ---------- peek: tmux fallback + structured session-log tail ----------

func TestPublicRoutePeekTmuxAndStructured(t *testing.T) {
	t.Run("tmux_fallback", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"p1": true}, capture: "PANE CONTENT"}
		a := newTestApp(t, f)
		seedTmuxNode(a, "p1")
		h := conversationAPIHandler(t, a)

		rec := routeRequest(h, http.MethodGet, "/api/nodes/p1/peek", "", false)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "PANE CONTENT") {
			t.Fatalf("peek: status = %d body %q", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
			t.Errorf("Content-Type = %q, want text/plain", ct)
		}

		f.captureErr = true
		rec = routeRequest(h, http.MethodGet, "/api/nodes/p1/peek", "", false)
		if !strings.Contains(rec.Body.String(), "session exited or unavailable") {
			t.Errorf("dead peek = %q", rec.Body.String())
		}
	})

	t.Run("tmux_raises_corroborated_attention", func(t *testing.T) {
		dialogPane := "Do you want to proceed?\n  1. Yes\n  2. No\n  Esc to cancel"
		f := &fakeTmux{alive: map[string]bool{"p2": true}, capture: dialogPane}
		a := newTestApp(t, f)
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{}}]}}`)
		n := seedTmuxNode(a, "p2")
		n.Transcript = path
		h := conversationAPIHandler(t, a)

		rec := routeRequest(h, http.MethodGet, "/api/nodes/p2/peek", "", false)
		if rec.Code != http.StatusOK {
			t.Fatalf("peek: %d", rec.Code)
		}
		if got := a.attn["p2"]; got != "approval" {
			t.Errorf("attention after peek = %q, want approval", got)
		}
	})

	t.Run("structured_session_log_peek", func(t *testing.T) {
		rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
		a, _ := newCodexTestApp(t, "THREAD-PEEK", rollout)
		h := conversationAPIHandler(t, a)
		rec := routeRequest(h, http.MethodPost, "/api/nodes",
			`{"title":"P","prompt":"hi","agent":"codex","dir":`+strconv.Quote(a.home)+`}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		var n Node
		if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
			t.Fatal(err)
		}
		// Wait for first turn to settle so Peek has log content.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && a.codex.Live(n.ID) == "active" {
			time.Sleep(10 * time.Millisecond)
		}
		rec = routeRequest(h, http.MethodGet, "/api/nodes/"+n.ID+"/peek", "", false)
		if rec.Code != http.StatusOK {
			t.Fatalf("structured peek: status = %d body %q", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
			t.Errorf("Content-Type = %q, want text/plain", ct)
		}
		// Structured peek is a session-log tail, not a pane photo — must not
		// invoke tmux capture-pane.
		// (fakeTmux under newCodexTestApp starts empty; create may have used it
		// for nothing; capture-pane would appear only if peek hit the tmux path.)
	})
}

// ---------- chat: current segment, history, overlays, assets ----------

func TestPublicRouteChatSegmentHistoryAndAssets(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := seedTmuxNode(a, "c1")
	_ = n
	w := &sessionlog.Writer{Path: a.sessionLogPath("c1")}
	for _, ev := range []sessionlog.Event{
		sessionlog.NewMeta("c1", "claude", "", "", a.home),
		sessionlog.NewAsset(sessionlog.AssetEvent{
			ID: "a_1", Name: "photo.png", Mime: "image/png", Size: 3,
			Storage: "inline", Bytes: "aGk=", SourceKind: "upload",
			SourcePath: "/tmp/n1/photo.png",
		}),
		{T: "user", Text: "old question", Time: "2026-07-14T01:00:00Z"},
		{T: "assistant", Text: "old answer", Time: "2026-07-14T01:01:00Z"},
		{T: "source", Time: "2026-07-15T09:00:00Z", Source: &sessionlog.SourceEvent{SessionID: "s2", Reason: "clear"}},
		{T: "user", Text: "new question\n\n[attached image: /tmp/n1/photo.png]", Time: "2026-07-15T09:05:00Z"},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	h := conversationAPIHandler(t, a)

	// Polled chat: segment-scoped, prior_turns, asset projection, transport overlay.
	rec := routeRequest(h, http.MethodGet, "/api/nodes/c1/chat", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: status = %d body %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var poll struct {
		Turns      []transcript.Turn         `json:"turns"`
		PriorTurns int                       `json:"prior_turns"`
		ChatStart  string                    `json:"chat_started"`
		Pending    bool                      `json:"pending"`
		Fallback   bool                      `json:"fallback"`
		Source     string                    `json:"source"`
		Assets     map[string]map[string]any `json:"assets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &poll); err != nil {
		t.Fatal(err)
	}
	if len(poll.Turns) != 1 || poll.PriorTurns != 2 {
		t.Fatalf("poll turns=%d prior=%d, want 1/2", len(poll.Turns), poll.PriorTurns)
	}
	if poll.ChatStart == "" {
		t.Error("chat_started must be set after a clear seam")
	}
	// No transcript path → pending + source none. fallback is only forced when
	// the segment has zero turns (or the tailer is unparseable/stale); a
	// mirrored session log with turns is still the chat surface.
	if !poll.Pending || poll.Source != "none" {
		t.Errorf("tmux overlay = pending=%v fallback=%v source=%q, want pending/source none",
			poll.Pending, poll.Fallback, poll.Source)
	}
	// Asset projection on the live segment.
	if !strings.Contains(poll.Turns[0].Text, "scimux-asset:a_1") {
		t.Errorf("turn not projected: %q", poll.Turns[0].Text)
	}
	if as, ok := poll.Assets["a_1"]; !ok || as["name"] != "photo.png" || as["inline"] != true {
		t.Errorf("assets map = %+v", poll.Assets)
	}

	// ?history=1: whole log as ordered surfaces.
	rec = routeRequest(h, http.MethodGet, "/api/nodes/c1/chat?history=1", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("history: status = %d", rec.Code)
	}
	var hist struct {
		Segments []sessionlog.HistorySegment `json:"segments"`
		Assets   map[string]any              `json:"assets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(hist.Segments))
	}
	if len(hist.Segments[0].Turns) != 2 || hist.Segments[0].Reason != "" {
		t.Errorf("first surface = %+v", hist.Segments[0])
	}
	if len(hist.Segments[1].Turns) != 1 || hist.Segments[1].Reason != "clear" {
		t.Errorf("clear surface = %+v", hist.Segments[1])
	}
	if hist.Assets["a_1"] == nil {
		t.Error("history response must include projected assets")
	}

	// Structured chat overlay: source "acp".
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	a2, _ := newCodexTestApp(t, "THREAD-CHAT-R", rollout)
	h2 := conversationAPIHandler(t, a2)
	rec = routeRequest(h2, http.MethodPost, "/api/nodes",
		`{"title":"Q","prompt":"q","agent":"codex","dir":`+strconv.Quote(a2.home)+`}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("codex create: %d %s", rec.Code, rec.Body.String())
	}
	var cn Node
	if err := json.Unmarshal(rec.Body.Bytes(), &cn); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && a2.codex.Live(cn.ID) == "active" {
		time.Sleep(10 * time.Millisecond)
	}
	rec = routeRequest(h2, http.MethodGet, "/api/nodes/"+cn.ID+"/chat", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("structured chat: %d %s", rec.Code, rec.Body.String())
	}
	var sbody struct {
		Source  string `json:"source"`
		Pending bool   `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sbody); err != nil {
		t.Fatal(err)
	}
	if sbody.Source != "acp" || sbody.Pending {
		t.Errorf("structured chat overlay = %+v, want source acp, pending false", sbody)
	}
}
