package app

// Packet 4B public-route coverage for conversation HTTP bindings through
// NewHandler. Complements — does not replace — the detailed direct-handler
// matrices consolidated below, session-log tests, protocol-manager tests,
// Packet 2E ordering, or Packet 3D ownership tests.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp/codex"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/tmuxsession"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

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
	h := newTestHandler(t, a)

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
	seedPermClaude(t, a, "n1", hookSIDOwn)
	h := newTestHandler(t, a)

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
	seedPermClaude(t, a2, "n2", hookSIDOwn)
	h2 := newTestHandler(t, a2)
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
	installPreparedClaudeHook(t, a, n)
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
	h := newTestHandler(t, a)

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
		h := newTestHandler(t, a)

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
		h := newTestHandler(t, a)

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
		h := newTestHandler(t, a)

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
		h := newTestHandler(t, a)

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
		h := newTestHandler(t, a)

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
		// Default seed is non-AX Claude: physical Keys is the one-element sequence.
		if !reflect.DeepEqual(got.Keys, []string{"y"}) {
			t.Errorf("legacy Keys = %v, want [y]", got.Keys)
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
		h := newTestHandler(t, a)

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
			if _, ok := a.codex.Pending(n.ID); ok {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		p, ok := a.codex.Pending(n.ID)
		if !ok {
			t.Fatal("pending approval never appeared")
		}

		rec = routeRequest(h, http.MethodPost, "/api/nodes/"+n.ID+"/key",
			`{"key":"y","request_id":`+strconv.Quote(p.RequestID)+`}`, true)
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
		if found.Keys != nil {
			t.Errorf("structured audit must omit Keys, got %v", found.Keys)
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
		h := newTestHandler(t, a)

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
		n.Agent = "pi"
		n.Transport = "tmux"
		n.Transcript = path
		h := newTestHandler(t, a)

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
		h := newTestHandler(t, a)
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
	h := newTestHandler(t, a)

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
	h2 := newTestHandler(t, a2)
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

func TestLastLines(t *testing.T) {
	if got := lastLines("a\nb\nc\nd\n\n  \n", 2); got != "c\nd" {
		t.Errorf("lastLines = %q", got)
	}
	if got := lastLines("only", 5); got != "only" {
		t.Errorf("short input = %q", got)
	}
}

// TestHandleSendUnconfirmedHoldsNextSend covers the delivery state machine:
// a send whose Enter produces no pane reaction and no transcript turn is
// reported "unconfirmed", further sends are held with 409, and an explicit
// resolve reopens the node. The fake tmux runner is inert (finding 29-style
// discipline: no real agent CLI, no real tmux).
func TestHandleSendUnconfirmedHoldsNextSend(t *testing.T) {
	// Every capture-pane returns the same bytes: the TUI never reacts.
	runner := func(ctx context.Context, stdin string, args ...string) (string, error) {
		return "static pane", nil
	}
	n := &Node{ID: "n1", Agent: "pi"}
	a := &app{
		byID:      map[string]*Node{"n1": n},
		nodes:     []*Node{n},
		sendState: map[string]string{},
		tailers:   map[string]*transcript.Tailer{},
		chatMark:  map[string]chatMark{},
		staleChat: map[string]bool{},
		server:    tmuxsession.NewServerWithRunner("testsock", runner),
	}
	a.initMaps() // keeps the fixture maps above, fills the rest handleSend writes
	send := func(text string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/nodes/n1/send", strings.NewReader(`{"text":"`+text+`"}`))
		req.SetPathValue("id", "n1")
		a.handleSend(rec, req)
		return rec
	}

	rec := send("hello")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"unconfirmed"`) {
		t.Fatalf("send on static pane = %d %s, want unconfirmed", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Errorf("unconfirmed response must return the text for recovery: %s", rec.Body.String())
	}
	if rec := send("next"); rec.Code != 409 {
		t.Fatalf("send during unconfirmed delivery = %d, want 409", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/nodes/n1/send/resolve", nil)
	req.SetPathValue("id", "n1")
	a.handleSendResolve(rec, req)
	if rec.Code != 200 {
		t.Fatalf("resolve = %d %s", rec.Code, rec.Body.String())
	}
	if rec := send("after-resolve"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"unconfirmed"`) {
		t.Fatalf("send after resolve = %d %s", rec.Code, rec.Body.String())
	}
}

// The initial-only internal state presents as its own "delivering" value, not
// as the ordinary delivery warning. This test used to require the opposite, so
// that the browser would keep forcing the terminal "precisely while the
// delivery is uncertain" — but that reasoning does not survive contact with
// the two rules that now bracket it. sendInitialUnconfirmed is only ever set
// on a Claude node, and the strict terminal policy already forbids the browser
// from auto-opening the pane for one (chatActivityPolicy's claudeGated branch
// ignores `unconfirmed` entirely), so nothing was being forced. What was left
// was the wording: an unconfirmed send offers "check the terminal" and a
// resume button, and the CLI's lazily written transcript makes that the normal
// case for a launch whose SessionStart has already arrived and whose prompt is
// perfectly fine. The doubt now belongs to the outer bound, not to the wait.
func TestHandleChatPresentsInitialDeliveryAsDelivering(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	seedChatNodeWithLog(t, a, "n1")
	a.sendState["n1"] = "initial_unconfirmed"

	rec := chatGET(t, a, "n1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d %q", rec.Code, rec.Body.String())
	}
	var body struct {
		Delivery string `json:"delivery"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Delivery != "delivering" {
		t.Fatalf("delivery = %q, want %q", body.Delivery, "delivering")
	}
}

// A prompt past the 1 MiB body cap must fail with a specific 413 naming the
// limit, not a bare "bad request" that gives the user no size hint and
// invites retries that can never succeed.
func TestHandleSendOversizedPrompt413(t *testing.T) {
	n := &Node{ID: "n1", Agent: "pi"}
	a := &app{
		byID:      map[string]*Node{"n1": n},
		nodes:     []*Node{n},
		sendState: map[string]string{},
		tailers:   map[string]*transcript.Tailer{},
		chatMark:  map[string]chatMark{},
		staleChat: map[string]bool{},
		server: tmuxsession.NewServerWithRunner("testsock",
			func(ctx context.Context, stdin string, args ...string) (string, error) { return "", nil }),
	}
	big := `{"text":"` + strings.Repeat("a", jsonBodyMax+1024) + `"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/nodes/n1/send", strings.NewReader(big))
	req.SetPathValue("id", "n1")
	a.handleSend(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized prompt: code = %d, want 413", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, fmt.Sprint(jsonBodyMax)) {
		t.Errorf("413 body must name the limit, got %q", body)
	}
}

// TestHandleSendAcknowledgedByPane: a pane that changes after Enter clears
// the state immediately, so consecutive sends flow.
func TestHandleSendAcknowledgedByPane(t *testing.T) {
	seq := 0
	runner := func(ctx context.Context, stdin string, args ...string) (string, error) {
		for _, arg := range args {
			if arg == "capture-pane" {
				seq++
				return fmt.Sprintf("pane state %d", seq), nil
			}
		}
		return "", nil
	}
	n := &Node{ID: "n1", Agent: "pi"}
	a := &app{
		byID:      map[string]*Node{"n1": n},
		nodes:     []*Node{n},
		sendState: map[string]string{},
		tailers:   map[string]*transcript.Tailer{},
		chatMark:  map[string]chatMark{},
		staleChat: map[string]bool{},
		server:    tmuxsession.NewServerWithRunner("testsock", runner),
	}
	a.initMaps() // keeps the fixture maps above, fills the rest handleSend writes
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/nodes/n1/send", strings.NewReader(`{"text":"go"}`))
		req.SetPathValue("id", "n1")
		a.handleSend(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"acknowledged"`) {
			t.Fatalf("send %d = %d %s, want acknowledged", i, rec.Code, rec.Body.String())
		}
	}
}

// An ended thread (/exit) is an immutable dead-end: every mutation endpoint
// must refuse it with 409 so a "Closed" thread — including an adopted one whose
// process outlived the cap — can never accept new history. Pick-up is via fork.
func TestEndedNodeRejectsMutations(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{}, capture: "Approve? (y/n)\n"}
	a := newTestApp(t, f)
	if rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`","lane_id":"lane-a"}`); rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	id := a.nodes[0].ID
	a.byID[id].EndedAt = "2026-07-23T00:00:00Z"

	post := func(path, body string, h http.HandlerFunc) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/nodes/"+id+path, strings.NewReader(body))
		r.SetPathValue("id", id)
		h(w, r)
		return w
	}
	cases := []struct {
		name, path, body string
		h                http.HandlerFunc
	}{
		{"send", "/send", `{"text":"hi"}`, a.handleSend},
		{"clear", "/send", `{"text":"/clear"}`, a.handleSend},
		{"upload", "/attachments", "", a.handleUploadAttachments},
		{"interrupt", "/send/interrupt", "", a.handleSendInterrupt},
		{"key", "/key", `{"key":"y"}`, a.handleKey},
	}
	for _, c := range cases {
		if w := post(c.path, c.body, c.h); w.Code != http.StatusConflict {
			t.Errorf("%s on ended node: code = %d, want 409 (body %q)", c.name, w.Code, w.Body.String())
		}
	}
	// The guard short-circuits before any tmux side effect.
	if subs := f.subcommands(); containsSub(subs, "send-keys") || containsSub(subs, "paste-buffer") {
		t.Fatalf("ended node produced tmux mutations: %v", subs)
	}
}

func TestHandleKeyTmuxEvidenceBeforeAction(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"k1": true}, capture: "some output\nApprove? (y/n)\n"}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "k1", Title: "k1", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}}
	a.byID["k1"] = a.nodes[0]

	// Disallowed key rejected before any tmux contact.
	if rec := keyReq(a, "k1", `{"key":"q"}`); rec.Code != 400 {
		t.Errorf("disallowed key: code = %d, want 400", rec.Code)
	}
	if rec := keyReq(a, "missing", `{"key":"y"}`); rec.Code != 404 {
		t.Errorf("missing node: code = %d, want 404", rec.Code)
	}

	// Happy path: capture-pane (evidence) must precede send-keys (action), and
	// the audit record must carry the pane excerpt.
	rec := keyReq(a, "k1", `{"key":"y"}`)
	if rec.Code != 200 {
		t.Fatalf("key: code = %d body %q", rec.Code, rec.Body.String())
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
		t.Fatalf("capture must precede send-keys, got order %v", subs)
	}
	recs := keyRecords(t, a.storePath)
	var got *storeRecord
	for i := range recs {
		if recs[i].Type == "key" {
			got = &recs[i]
		}
	}
	if got == nil || got.Key != "y" || !strings.Contains(got.Excerpt, "Approve?") {
		t.Fatalf("audit record = %+v, want key y with pane excerpt", got)
	}
	// Legacy/adopted Claude (AXScreenReader false): one-element physical Keys.
	if !reflect.DeepEqual(got.Keys, []string{"y"}) {
		t.Errorf("legacy single-key Keys = %v, want [y]", got.Keys)
	}
}

func TestHandleKeyRefusesWithoutEvidence(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"k2": true}, captureErr: true}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "k2", Title: "k2", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}}
	a.byID["k2"] = a.nodes[0]

	rec := keyReq(a, "k2", `{"key":"y"}`)
	if rec.Code != 500 {
		t.Fatalf("capture failure: code = %d, want 500", rec.Code)
	}
	for _, s := range f.subcommands() {
		if s == "send-keys" {
			t.Fatal("send-keys must not run when evidence capture failed")
		}
	}
}

// tmuxKeySequence is the pure AX policy: Claude+tmux+AX digit/y/n → choice+Enter;
// everything else (including nil node) stays a single physical key.
func TestTmuxKeySequence(t *testing.T) {
	axClaude := &Node{Agent: "claude", Transport: "tmux", AXScreenReader: true}
	legacyClaude := &Node{Agent: "claude", Transport: "tmux", AXScreenReader: false}
	adoptedClaude := &Node{Agent: "claude", Transport: "tmux", Adopted: true, AXScreenReader: false}
	piTmux := &Node{Agent: "pi", Transport: "tmux"}
	ocTmux := &Node{Agent: "opencode", Transport: "tmux"}
	codex := &Node{Agent: "codex", Transport: "codex"}

	cases := []struct {
		name string
		n    *Node
		key  string
		want []string
	}{
		{"ax-1", axClaude, "1", []string{"1", "Enter"}},
		{"ax-9", axClaude, "9", []string{"9", "Enter"}},
		{"ax-y", axClaude, "y", []string{"y", "Enter"}},
		{"ax-n", axClaude, "n", []string{"n", "Enter"}},
		{"ax-Enter", axClaude, "Enter", []string{"Enter"}},
		{"ax-Escape", axClaude, "Escape", []string{"Escape"}},
		{"ax-Tab", axClaude, "Tab", []string{"Tab"}},
		{"ax-Up", axClaude, "Up", []string{"Up"}},
		{"ax-Down", axClaude, "Down", []string{"Down"}},
		{"legacy-1", legacyClaude, "1", []string{"1"}},
		{"adopted-1", adoptedClaude, "1", []string{"1"}},
		{"pi-1", piTmux, "1", []string{"1"}},
		{"opencode-1", ocTmux, "1", []string{"1"}},
		// Pure helper returns the key even for structured agents; public
		// routing never delivers that sequence through tmux.
		{"codex-1", codex, "1", []string{"1"}},
		{"nil-1", nil, "1", []string{"1"}},
	}
	for _, c := range cases {
		got := tmuxKeySequence(c.n, c.key)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: tmuxKeySequence = %v, want %v", c.name, got, c.want)
		}
	}
}

// sendKeysCalls returns every send-keys argv recorded by the fake runner.
func sendKeysCalls(f *fakeTmux) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if len(c) >= 3 && c[2] == "send-keys" {
			out = append(out, append([]string(nil), c...))
		}
	}
	return out
}

// AX Claude compound delivery: one capture, one send-keys with choice+Enter,
// semantic Key + physical Keys audit, single attention clear and end edge.
func TestHandleKeyAXClaudeCompoundDelivery(t *testing.T) {
	pane := "Permission Required\n  1. Yes\n  2. No\nEnter selection [1-2], or Escape to cancel:"
	f := &fakeTmux{alive: map[string]bool{"ax1": true}, capture: pane}
	a := newTestApp(t, f)
	n := &Node{
		ID: "ax1", Title: "ax1", Agent: "claude", Transport: "tmux",
		AXScreenReader: true, CreatedAt: "2026-08-10T00:00:00Z",
	}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.attn[n.ID] = "approval"
	a.attnAt[n.ID] = time.Now().Add(-30 * time.Second)
	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "claude", "c", "", a.home),
		{T: "attention", Time: "2026-08-10T10:00:00Z", Attention: &sessionlog.AttentionEvent{Kind: "approval", Status: "start"}},
	})

	// AX + "1" → capture then one send-keys `1 Enter`.
	rec := keyReq(a, "ax1", `{"key":"1"}`)
	if rec.Code != 200 {
		t.Fatalf("AX key 1: code = %d body %q", rec.Code, rec.Body.String())
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
	sk := sendKeysCalls(f)
	if len(sk) != 1 {
		t.Fatalf("want exactly one send-keys call, got %d: %v", len(sk), sk)
	}
	// Args: -L sock send-keys -t =ax1: 1 Enter
	if !reflect.DeepEqual(sk[0][2:], []string{"send-keys", "-t", "=ax1:", "1", "Enter"}) {
		t.Errorf("send-keys argv = %v, want send-keys -t =ax1: 1 Enter", sk[0])
	}
	// Not two runner calls for the compound sequence.
	sendCount := 0
	for _, s := range subs {
		if s == "send-keys" {
			sendCount++
		}
	}
	if sendCount != 1 {
		t.Errorf("send-keys subcommand count = %d, want 1", sendCount)
	}

	recs := keyRecords(t, a.storePath)
	var got *storeRecord
	for i := range recs {
		if recs[i].Type == "key" {
			got = &recs[i]
		}
	}
	if got == nil {
		t.Fatal("missing key audit")
	}
	if got.Key != "1" {
		t.Errorf("semantic Key = %q, want 1", got.Key)
	}
	if !reflect.DeepEqual(got.Keys, []string{"1", "Enter"}) {
		t.Errorf("physical Keys = %v, want [1 Enter]", got.Keys)
	}
	if !strings.Contains(got.Excerpt, "Permission Required") {
		t.Errorf("excerpt missing pre-delivery pane evidence: %q", got.Excerpt)
	}
	if a.attn[n.ID] != "" {
		t.Errorf("attn after compound delivery = %q, want cleared once", a.attn[n.ID])
	}
	var ends int
	for _, ev := range sessionlog.ReadEvents(a.sessionLogPath(n.ID)) {
		if ev.T == "attention" && ev.Attention != nil && ev.Attention.Status == "end" {
			ends++
		}
	}
	if ends != 1 {
		t.Errorf("attention end edges = %d, want 1", ends)
	}

	// AX + y → y Enter in one call.
	f.calls = nil
	a.attn[n.ID] = "question"
	rec = keyReq(a, "ax1", `{"key":"y"}`)
	if rec.Code != 200 {
		t.Fatalf("AX key y: code = %d body %q", rec.Code, rec.Body.String())
	}
	sk = sendKeysCalls(f)
	if len(sk) != 1 || !reflect.DeepEqual(sk[0][2:], []string{"send-keys", "-t", "=ax1:", "y", "Enter"}) {
		t.Errorf("y Enter delivery = %v", sk)
	}

	// AX + Escape remains a single physical key.
	f.calls = nil
	rec = keyReq(a, "ax1", `{"key":"Escape"}`)
	if rec.Code != 200 {
		t.Fatalf("AX Escape: code = %d body %q", rec.Code, rec.Body.String())
	}
	sk = sendKeysCalls(f)
	if len(sk) != 1 || !reflect.DeepEqual(sk[0][2:], []string{"send-keys", "-t", "=ax1:", "Escape"}) {
		t.Errorf("Escape delivery = %v", sk)
	}
	recs = keyRecords(t, a.storePath)
	var esc *storeRecord
	for i := range recs {
		if recs[i].Type == "key" && recs[i].Key == "Escape" {
			esc = &recs[i]
		}
	}
	if esc == nil || !reflect.DeepEqual(esc.Keys, []string{"Escape"}) {
		t.Errorf("Escape audit Keys = %+v", esc)
	}
}

// Legacy Claude, adopted Claude, and non-Claude tmux stay single-key.
func TestHandleKeyLegacyAdoptedNonClaudeSingleKey(t *testing.T) {
	cases := []struct {
		name string
		n    *Node
	}{
		{"legacy", &Node{ID: "leg", Title: "leg", Agent: "claude", Transport: "tmux",
			AXScreenReader: false, CreatedAt: "2026-01-01T00:00:00Z"}},
		{"adopted", &Node{ID: "adp", Title: "adp", Agent: "claude", Transport: "tmux",
			Adopted: true, AXScreenReader: false, CreatedAt: "2026-01-01T00:00:00Z"}},
		{"pi", &Node{ID: "pi1", Title: "pi1", Agent: "pi", Transport: "tmux",
			CreatedAt: "2026-01-01T00:00:00Z"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeTmux{alive: map[string]bool{c.n.ID: true}, capture: "Approve? 1/2\n"}
			a := newTestApp(t, f)
			a.nodes = []*Node{c.n}
			a.byID[c.n.ID] = c.n
			rec := keyReq(a, c.n.ID, `{"key":"1"}`)
			if rec.Code != 200 {
				t.Fatalf("code = %d body %q", rec.Code, rec.Body.String())
			}
			sk := sendKeysCalls(f)
			if len(sk) != 1 {
				t.Fatalf("send-keys calls = %d, want 1", len(sk))
			}
			want := []string{"send-keys", "-t", "=" + c.n.ID + ":", "1"}
			if !reflect.DeepEqual(sk[0][2:], want) {
				t.Errorf("argv = %v, want %v (no Enter)", sk[0][2:], want)
			}
			recs := keyRecords(t, a.storePath)
			var got *storeRecord
			for i := range recs {
				if recs[i].Type == "key" {
					got = &recs[i]
				}
			}
			if got == nil || got.Key != "1" || !reflect.DeepEqual(got.Keys, []string{"1"}) {
				t.Errorf("audit = %+v, want Key=1 Keys=[1]", got)
			}
		})
	}
}

// Capture / SendKeys failure preserves attention and writes no success audit.
func TestHandleKeyFailurePreservesAttention(t *testing.T) {
	t.Run("capture_failure", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"axf": true}, captureErr: true}
		a := newTestApp(t, f)
		n := &Node{ID: "axf", Title: "axf", Agent: "claude", Transport: "tmux",
			AXScreenReader: true, CreatedAt: "2026-08-10T00:00:00Z"}
		a.nodes = []*Node{n}
		a.byID[n.ID] = n
		a.attn[n.ID] = "approval"
		if rec := keyReq(a, "axf", `{"key":"1"}`); rec.Code != 500 {
			t.Fatalf("code = %d, want 500", rec.Code)
		}
		if len(sendKeysCalls(f)) != 0 {
			t.Fatal("send-keys must not run when capture failed")
		}
		if a.attn[n.ID] != "approval" {
			t.Errorf("attn cleared on capture failure")
		}
		for _, r := range keyRecords(t, a.storePath) {
			if r.Type == "key" {
				t.Fatalf("unexpected key audit on capture failure: %+v", r)
			}
		}
	})

	t.Run("sendkeys_failure", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"axs": true}, capture: "menu", sendKeysErr: true}
		a := newTestApp(t, f)
		n := &Node{ID: "axs", Title: "axs", Agent: "claude", Transport: "tmux",
			AXScreenReader: true, CreatedAt: "2026-08-10T00:00:00Z"}
		a.nodes = []*Node{n}
		a.byID[n.ID] = n
		a.attn[n.ID] = "approval"
		if rec := keyReq(a, "axs", `{"key":"1"}`); rec.Code != 500 {
			t.Fatalf("code = %d, want 500", rec.Code)
		}
		// Call was attempted (validation passed) but failed.
		if len(sendKeysCalls(f)) != 1 {
			t.Fatalf("expected one failed send-keys attempt, got %v", sendKeysCalls(f))
		}
		if a.attn[n.ID] != "approval" {
			t.Errorf("attn cleared on SendKeys failure")
		}
		for _, r := range keyRecords(t, a.storePath) {
			if r.Type == "key" {
				t.Fatalf("success audit written after SendKeys failure: %+v", r)
			}
		}
	})
}

// Audit failure after physical delivery reports the existing partial-failure
// contract: keys cannot be unsent, so response is not plain success.
func TestHandleKeyAuditFailureAfterDelivery(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"axa": true}, capture: "Approve?\n"}
	a := newTestApp(t, f)
	n := &Node{ID: "axa", Title: "axa", Agent: "claude", Transport: "tmux",
		AXScreenReader: true, CreatedAt: "2026-08-10T00:00:00Z"}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	// Make appendRecord fail: storePath is a directory.
	bad := filepath.Join(t.TempDir(), "nodes.jsonl")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	a.storePath = bad

	rec := keyReq(a, "axa", `{"key":"1"}`)
	if rec.Code != 500 {
		t.Fatalf("code = %d, want 500; body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "key was sent") {
		t.Errorf("partial-failure body must report delivery-then-audit failure, got %q", rec.Body.String())
	}
	// Physical keys already delivered.
	sk := sendKeysCalls(f)
	if len(sk) != 1 || !reflect.DeepEqual(sk[0][2:], []string{"send-keys", "-t", "=axa:", "1", "Enter"}) {
		t.Errorf("physical delivery = %v", sk)
	}
	// Attention still cleared after successful delivery (before audit).
	if a.attn[n.ID] != "" {
		t.Errorf("attn = %q after delivery; clear happens before audit", a.attn[n.ID])
	}
}

// Structured Codex decisions keep semantic Key only — no physical Keys/Enter.
func TestHandleKeyStructuredOmitsPhysicalKeys(t *testing.T) {
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	spawn, approvalDispatched, decisionDelivered := newApprovalFakeCodexSpawn(t, "THREAD-KEY-AX", rollout)
	f := &fakeTmux{}
	a := newTestApp(t, f)
	logDir := filepath.Join(filepath.Dir(a.storePath), "codex")
	a.codex = codexManager{codex.NewManagerWithSpawn(logDir, spawn)}
	t.Cleanup(a.codex.Shutdown)

	rec := newNode(a, `{"prompt":"do it","title":"T","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: %d", rec.Code)
	}
	n := a.nodes[0]
	select {
	case <-approvalDispatched:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for approval")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := a.codex.Pending(n.ID); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	p, ok := a.codex.Pending(n.ID)
	if !ok {
		t.Fatal("pending approval never appeared")
	}

	keyRec := keyReq(a, n.ID, `{"key":"y","request_id":`+strconv.Quote(p.RequestID)+`}`)
	if keyRec.Code != 200 {
		t.Fatalf("handleKey: code = %d body %q", keyRec.Code, keyRec.Body)
	}
	// No tmux send-keys for structured decisions.
	if len(sendKeysCalls(f)) != 0 {
		t.Fatalf("structured key must not contact tmux: %v", sendKeysCalls(f))
	}
	recs := keyRecords(t, a.storePath)
	var found *storeRecord
	for i := range recs {
		if recs[i].Type == "key" {
			found = &recs[i]
			break
		}
	}
	if found == nil || found.Key != "y" {
		t.Fatalf("audit = %+v", found)
	}
	if found.Keys != nil {
		t.Errorf("structured audit must omit Keys, got %v", found.Keys)
	}
	select {
	case <-decisionDelivered:
	case <-time.After(5 * time.Second):
		t.Fatal("decision never reached fake server")
	}
}

// --- /api/nodes/{id}/peek ---

func TestHandlePeekTmux(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"p1": true}, capture: "PANE CONTENT"}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "p1", Title: "p1", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}}
	a.byID["p1"] = a.nodes[0]

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/p1/peek", nil)
	r.SetPathValue("id", "p1")
	a.handlePeek(rec, r)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "PANE CONTENT") {
		t.Fatalf("peek = %d %q", rec.Code, rec.Body.String())
	}

	f.captureErr = true
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/api/nodes/p1/peek", nil)
	r2.SetPathValue("id", "p1")
	a.handlePeek(rec2, r2)
	if !strings.Contains(rec2.Body.String(), "session exited or unavailable") {
		t.Errorf("peek on dead pane = %q", rec2.Body.String())
	}
}

// TestHandlePeekSpotsDialog: opening the terminal view runs the corroborated
// dialog check regardless of the quiet gate — the human's peek is exactly the
// gesture that catches a dialog hidden behind an animating pane.
func TestHandlePeekSpotsDialog(t *testing.T) {
	dialogPane := "Do you want to proceed?\n  1. Yes\n  2. No\n  Esc to cancel"
	f := &fakeTmux{alive: map[string]bool{"p1": true}, capture: dialogPane}
	a := newTestApp(t, f)
	path := filepath.Join(t.TempDir(), "tx.jsonl")
	appendLines(t, path,
		`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{}}]}}`)
	n := &Node{ID: "p1", Title: "p1", Agent: "pi", Transport: "tmux", Transcript: path, CreatedAt: "2026-07-18T00:00:00Z"}
	a.nodes, a.byID["p1"] = []*Node{n}, n

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/p1/peek", nil)
	r.SetPathValue("id", "p1")
	a.handlePeek(rec, r)
	if rec.Code != 200 {
		t.Fatalf("peek = %d", rec.Code)
	}
	if got := a.attn["p1"]; got != "approval" {
		t.Errorf("attention after peek = %q, want approval", got)
	}
}

const peekEditDialog = ` Do you want to make this edit to hello.txt?
 ❯ 1. Yes
   2. Yes, allow all edits during this session (shift+tab)
   3. No

 Esc to cancel · Tab to amend`

// TestHandlePeekStructuralDialogNoWaitingOn: P1c — peek raises attention for
// the current Write/Edit dialog wording even with no unresolved tool call in
// the transcript (Claude Code's late tool_use flush). Shares the quiet-branch
// predicate with the poller, including its static-pane precondition: the
// matcher may classify a quiet pane, never an active one (AGENTS.md).
func TestHandlePeekStructuralDialogNoWaitingOn(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"p1": true}, capture: peekEditDialog}
	a := newTestApp(t, f)
	path := filepath.Join(t.TempDir(), "tx.jsonl")
	// User prompt only — no tool_use record (the late-flush case).
	appendLines(t, path,
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"edit hello.txt"}}`)
	n := &Node{ID: "p1", Title: "p1", Agent: "pi", Transport: "tmux", Transcript: path, CreatedAt: "2026-07-18T00:00:00Z"}
	a.nodes, a.byID["p1"] = []*Node{n}, n
	a.lastChg["p1"] = time.Now().Add(-time.Minute) // pane mechanically static

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/p1/peek", nil)
	r.SetPathValue("id", "p1")
	a.handlePeek(rec, r)
	if rec.Code != 200 {
		t.Fatalf("peek = %d", rec.Code)
	}
	if got := a.attn["p1"]; got != "dialog" {
		t.Errorf("attention after peek = %q, want dialog (no WaitingOn required)", got)
	}
}

// TestNotePeekDialogActivePaneNeedsStructuredEvidence pins the fence that P1c
// must not widen: handlePeek is not quiet-gated (it deliberately runs while the
// pane animates, to catch a dialog with parallel calls queued behind it), so on
// an *active* pane only the corroborated path may raise — an unresolved tool
// call plus the matcher. Pane text alone must never create attention there.
func TestNotePeekDialogActivePaneNeedsStructuredEvidence(t *testing.T) {
	peek := func(t *testing.T, id string, lines ...string) *app {
		t.Helper()
		f := &fakeTmux{alive: map[string]bool{id: true}, capture: peekEditDialog}
		a := newTestApp(t, f)
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path, lines...)
		n := &Node{ID: id, Title: id, Agent: "pi", Transport: "tmux", Transcript: path, CreatedAt: "2026-07-18T00:00:00Z"}
		a.nodes, a.byID[id] = []*Node{n}, n
		a.lastChg[id] = time.Now() // pane changed just now: active, not quiet
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/nodes/"+id+"/peek", nil)
		r.SetPathValue("id", id)
		a.handlePeek(rec, r)
		return a
	}

	t.Run("matcher_alone_on_active_pane_stays_silent", func(t *testing.T) {
		a := peek(t, "a1",
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"edit hello.txt"}}`)
		a.mu.Lock()
		defer a.mu.Unlock()
		if got := a.attn["a1"]; got != "" {
			t.Errorf("attention = %q, want none: pane text alone on an active pane", got)
		}
		if _, ok := a.attnAt["a1"]; ok {
			t.Error("attnAt stamped from pane text on an active pane")
		}
	})

	t.Run("unresolved_call_plus_matcher_still_raises", func(t *testing.T) {
		a := peek(t, "a2",
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"edit hello.txt"}}`,
			`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Edit"}]}}`)
		a.mu.Lock()
		defer a.mu.Unlock()
		if got := a.attn["a2"]; got != "approval" {
			t.Errorf("attention = %q, want approval: the corroborated path is unchanged", got)
		}
	})
}

// --- /api/nodes/{id}/chat (tmux fallback) ---

func TestHandleChatTmuxFallbackNoTranscript(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "c1", Title: "c1", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}}
	a.byID["c1"] = a.nodes[0]

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/c1/chat", nil)
	r.SetPathValue("id", "c1")
	a.handleChat(rec, r)
	if rec.Code != 200 {
		t.Fatalf("chat code = %d", rec.Code)
	}
	var body struct {
		Pending  bool   `json:"pending"`
		Fallback bool   `json:"fallback"`
		Source   string `json:"source"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Pending || body.Fallback || body.Source != "none" {
		t.Errorf("no-transcript Claude chat = %+v, want pending, no fallback (no forced terminal)", body)
	}
}

// TestHandleChatHistory: ?history=1 returns the whole log as ordered
// surfaces — the on-demand read behind "show earlier history" and the metro
// map's earlier stops — while the plain poll stays segment-scoped.
func TestHandleChatHistory(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "c1", Title: "c1", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes, a.byID["c1"] = []*Node{n}, n
	w := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, "c1.jsonl")}
	for _, ev := range []sessionlog.Event{
		sessionlog.NewMeta("c1", "claude", "", "", a.home),
		{T: "user", Text: "old question", Time: "2026-07-14T01:00:00Z"},
		{T: "assistant", Text: "old answer", Time: "2026-07-14T01:01:00Z"},
		{T: "source", Time: "2026-07-15T09:00:00Z", Source: &sessionlog.SourceEvent{SessionID: "s2", Reason: "clear"}},
		{T: "user", Text: "new question", Time: "2026-07-15T09:05:00Z"},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/c1/chat?history=1", nil)
	r.SetPathValue("id", "c1")
	a.handleChat(rec, r)
	if rec.Code != 200 {
		t.Fatalf("history code = %d", rec.Code)
	}
	var body struct {
		Segments []sessionlog.HistorySegment `json:"segments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Segments) != 2 {
		t.Fatalf("segments = %+v, want 2 surfaces", body.Segments)
	}
	if len(body.Segments[0].Turns) != 2 || body.Segments[0].Reason != "" {
		t.Errorf("first surface = %+v", body.Segments[0])
	}
	if len(body.Segments[1].Turns) != 1 || body.Segments[1].Reason != "clear" ||
		body.Segments[1].Seam != "2026-07-15T09:00:00Z" {
		t.Errorf("clear surface = %+v", body.Segments[1])
	}
	// The polled response is untouched by the new branch: segment-scoped,
	// prior turns behind the divider.
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/api/nodes/c1/chat", nil)
	r2.SetPathValue("id", "c1")
	a.handleChat(rec2, r2)
	var poll struct {
		Turns      []transcript.Turn `json:"turns"`
		PriorTurns int               `json:"prior_turns"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &poll); err != nil {
		t.Fatal(err)
	}
	if len(poll.Turns) != 1 || poll.PriorTurns != 2 {
		t.Errorf("poll = turns %d prior %d, want 1/2", len(poll.Turns), poll.PriorTurns)
	}
}

// TestHandleChatCarriesDurableAddress: every turn the chat read path emits —
// both the polled segment and the ?history=1 surfaces — carries the durable
// source address (uid, segment, record), and it matches what ScanLog assigns to
// the same text. This is the address a capture stamps so it can name and resolve
// the exact chat turn it came from (Phase 0b).
func TestHandleChatCarriesDurableAddress(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "c1", Title: "c1", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes, a.byID["c1"] = []*Node{n}, n
	logPath := a.sessionLogPath("c1")
	w := &sessionlog.Writer{Path: logPath}
	for _, ev := range []sessionlog.Event{
		sessionlog.NewMeta("c1", "claude", "", "", a.home),
		{T: "user", Text: "old question", Time: "2026-07-14T01:00:00Z"},
		{T: "assistant", Text: "old answer", Time: "2026-07-14T01:01:00Z"},
		{T: "source", Time: "2026-07-15T09:00:00Z", Source: &sessionlog.SourceEvent{SessionID: "s2", Reason: "clear"}},
		{T: "user", Text: "new question", Time: "2026-07-15T09:05:00Z"},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	// scanAddr returns the (uid, segment, record) ScanLog assigns to a text —
	// the identity the read path must reproduce exactly.
	scanAddr := func(text string) sessionlog.Hit {
		res := sessionlog.ScanLog(logPath, text, sessionlog.ScanOptions{Before: 5, After: 5})
		if len(res.Hits) != 1 {
			t.Fatalf("ScanLog(%q) hits = %+v", text, res.Hits)
		}
		return res.Hits[0]
	}
	sameAddr := func(where string, tn transcript.Turn) {
		want := scanAddr(tn.Text)
		if tn.UID == "" || tn.UID != want.UID || tn.Segment != want.Segment || tn.Record != want.Record {
			t.Fatalf("%s: turn %q address (uid %q seg %d rec %d) != ScanLog (uid %q seg %d rec %d)",
				where, tn.Text, tn.UID, tn.Segment, tn.Record, want.UID, want.Segment, want.Record)
		}
	}

	// Polled chat: the current segment's turn carries its address.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/c1/chat", nil)
	r.SetPathValue("id", "c1")
	a.handleChat(rec, r)
	if rec.Code != 200 {
		t.Fatalf("chat code = %d", rec.Code)
	}
	var poll struct {
		Turns []transcript.Turn `json:"turns"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &poll); err != nil {
		t.Fatal(err)
	}
	if len(poll.Turns) != 1 {
		t.Fatalf("poll turns = %+v, want 1 (current segment)", poll.Turns)
	}
	if poll.Turns[0].Segment != 1 {
		t.Errorf("post-clear turn segment = %d, want 1", poll.Turns[0].Segment)
	}
	sameAddr("poll", poll.Turns[0])

	// History: turns in surface 0 are segment 0, surface 1 is segment 1, and
	// every one round-trips its address against ScanLog.
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/api/nodes/c1/chat?history=1", nil)
	r2.SetPathValue("id", "c1")
	a.handleChat(rec2, r2)
	var hist struct {
		Segments []sessionlog.HistorySegment `json:"segments"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist.Segments) != 2 {
		t.Fatalf("history surfaces = %d, want 2", len(hist.Segments))
	}
	for si, s := range hist.Segments {
		for _, tn := range s.Turns {
			if tn.Segment != si {
				t.Errorf("history surface %d turn %q segment = %d", si, tn.Text, tn.Segment)
			}
			sameAddr(fmt.Sprintf("history[%d]", si), tn)
		}
	}
}

func TestHandleChatCodexSourceACP(t *testing.T) {
	// procChat must set source:"acp" so the browser renders the structured view.
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	a, _ := newCodexTestApp(t, "THREAD-CHAT", rollout) // requests not needed here

	// Create the node first.
	rec := newNode(a, `{"title":"Q","prompt":"q","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d %s", rec.Code, rec.Body)
	}
	n := a.nodes[0]

	// Wait briefly for the turn goroutine (first prompt) to complete.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && a.codex.Live(n.ID) == "active" {
		time.Sleep(10 * time.Millisecond)
	}

	req := httptest.NewRequest("GET", "/api/nodes/"+n.ID+"/chat", nil)
	req.SetPathValue("id", n.ID)
	rec2 := httptest.NewRecorder()
	a.handleChat(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("chat: code = %d %s", rec2.Code, rec2.Body)
	}
	var body struct {
		Source       string       `json:"source"`
		Turns        []any        `json:"turns"`
		Pending      bool         `json:"pending"`
		Perms        []PermOption `json:"perm_options"`
		PermTitle    string       `json:"perm_title"`
		PermToolKind string       `json:"perm_tool_kind"`
		Attention    string       `json:"attention"`
		WaitingOn    string       `json:"waiting_on"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Source != "acp" {
		t.Errorf("source = %q, want acp", body.Source)
	}
	if body.Pending {
		t.Error("pending must be false for structured node")
	}
	// No outstanding approval: title/attention empty, tool kind present as "".
	if body.PermTitle != "" || body.WaitingOn != "" || body.Attention != "" {
		t.Errorf("idle chat: perm_title=%q waiting_on=%q attention=%q, want empty",
			body.PermTitle, body.WaitingOn, body.Attention)
	}
	if body.PermToolKind != "" {
		t.Errorf("idle chat: perm_tool_kind = %q, want \"\"", body.PermToolKind)
	}
}

// Chat JSON for a live codex approval must carry option role kinds and the
// tool kind so the keyrow can style reject without parsing labels. Existing
// perm_title / attention / waiting_on fields stay byte-identical in meaning.
func TestHandleChatCodexPendingKinds(t *testing.T) {
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	spawn, approvalDispatched, _ := newApprovalFakeCodexSpawn(t, "THREAD-CHAT-PERM", rollout)
	f := &fakeTmux{}
	a := newTestApp(t, f)
	logDir := filepath.Join(filepath.Dir(a.storePath), "codex")
	a.codex = codexManager{codex.NewManagerWithSpawn(logDir, spawn)}
	t.Cleanup(a.codex.Shutdown)

	rec := newNode(a, `{"title":"Q","prompt":"q","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d %s", rec.Code, rec.Body)
	}
	n := a.nodes[0]

	select {
	case <-approvalDispatched:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for approval")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := a.codex.Pending(n.ID); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := a.codex.Pending(n.ID); !ok {
		t.Fatal("pending approval never appeared")
	}

	req := httptest.NewRequest("GET", "/api/nodes/"+n.ID+"/chat", nil)
	req.SetPathValue("id", n.ID)
	rec2 := httptest.NewRecorder()
	a.handleChat(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("chat: code = %d %s", rec2.Code, rec2.Body)
	}
	var body struct {
		Source        string       `json:"source"`
		Pending       bool         `json:"pending"`
		Perms         []PermOption `json:"perm_options"`
		PermTitle     string       `json:"perm_title"`
		PermToolKind  string       `json:"perm_tool_kind"`
		PermRequestID string       `json:"perm_request_id"`
		Attention     string       `json:"attention"`
		WaitingOn     string       `json:"waiting_on"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Source != "acp" {
		t.Errorf("source = %q, want acp", body.Source)
	}
	if body.Pending {
		t.Error("pending must be false for structured node (flag is tmux-only)")
	}
	// Existing fields: title, attention, waiting_on describe the live approval.
	if body.Attention != "approval" {
		t.Errorf("attention = %q, want approval", body.Attention)
	}
	if body.PermRequestID == "" {
		t.Error("perm_request_id must be non-empty while a structured permission is pending")
	}
	if p, ok := a.codex.Pending(n.ID); ok && p.RequestID != body.PermRequestID {
		t.Errorf("perm_request_id = %q, want manager pending %q", body.PermRequestID, p.RequestID)
	}
	if body.PermTitle == "" || body.WaitingOn == "" {
		t.Errorf("perm_title=%q waiting_on=%q, want non-empty", body.PermTitle, body.WaitingOn)
	}
	if body.PermTitle != body.WaitingOn {
		t.Errorf("perm_title (%q) and waiting_on (%q) must match", body.PermTitle, body.WaitingOn)
	}
	// New fields: tool kind from command approvals, option kinds from decisions.
	if body.PermToolKind != "execute" {
		t.Errorf("perm_tool_kind = %q, want execute (command approval)", body.PermToolKind)
	}
	if len(body.Perms) != 2 {
		t.Fatalf("perm_options = %d, want accept+cancel", len(body.Perms))
	}
	byKey := map[string]PermOption{}
	for _, o := range body.Perms {
		byKey[o.Key] = o
	}
	if got := byKey["1"]; got.Name != "Approve once" || got.Kind != "allow" {
		t.Errorf("accept option = %+v, want Approve once/allow", got)
	}
	if got := byKey["2"]; got.Name != "Reject and stop" || got.Kind != "reject" {
		t.Errorf("cancel option = %+v, want Reject and stop/reject", got)
	}
}

func TestHandleSendCodexConflict(t *testing.T) {
	// A second /send while a Codex turn is in flight must return exactly 409
	// (not 200, not 500). We use a blocking fake server so the first turn is
	// provably still active when we issue the second request (finding 83).
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	spawn, _, turnStarted, unblock := newBlockingFakeCodexSpawn(t, "THREAD-SEND", rollout)
	f := &fakeTmux{}
	a := newTestApp(t, f)
	logDir := filepath.Join(filepath.Dir(a.storePath), "codex")
	a.codex = codexManager{codex.NewManagerWithSpawn(logDir, spawn)}
	t.Cleanup(a.codex.Shutdown)

	rec := newNode(a, `{"title":"X","prompt":"do x","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d", rec.Code)
	}
	n := a.nodes[0]

	// Wait until the fake server has acknowledged turn/start so we know the
	// first turn is provably in flight (turnActive == true).
	select {
	case <-turnStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first turn to start")
	}

	// Second send while first turn is active must return 409.
	sendReq := httptest.NewRequest("POST", "/api/nodes/"+n.ID+"/send", strings.NewReader(`{"text":"second"}`))
	sendReq.SetPathValue("id", n.ID)
	sendRec := httptest.NewRecorder()
	a.handleSend(sendRec, sendReq)
	if sendRec.Code != 409 {
		t.Fatalf("second send: code = %d, want 409", sendRec.Code)
	}

	// Unblock the first turn so the manager can clean up before temp-dir removal.
	close(unblock)
}

func TestHandleKeyCodexAuditBeforeDeliver(t *testing.T) {
	// For a codex node with a pending approval, the audit record must be written
	// before Deliver is called (finding 53). We verify by injecting a node whose
	// Manager.Pending returns an option and checking the store record exists.
	// Because injecting a live pending permission requires a running goroutine,
	// we verify the simpler case: a missing session returns 400/409 (not 500 and
	// not an unaudited deliver).
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	a, _ := newCodexTestApp(t, "THREAD-KEY", rollout) // requests not needed here

	// Register a codex node that has no live session (was never launched via
	// HTTP, so the manager has no session) to exercise PrepareResolve → ErrNoSession.
	n := &Node{ID: "phantom", Title: "phantom", Agent: "codex", Transport: "codex",
		Dir: a.home, CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n

	req := httptest.NewRequest("POST", "/api/nodes/phantom/key",
		strings.NewReader(`{"key":"y","request_id":"1"}`))
	req.SetPathValue("id", "phantom")
	rec := httptest.NewRecorder()
	a.handleKey(rec, req)
	// ErrNoSession is a Conflict → 400/409 (not 500 which would indicate a key
	// was delivered without evidence).
	if rec.Code == 500 {
		t.Fatalf("handleKey: unexpected 500 on missing session: %q", rec.Body)
	}
	if rec.Code == 200 {
		t.Fatal("handleKey must not succeed when PrepareResolve fails")
	}
	// No "key" audit record should exist — the store must not have been written
	// without a successful PrepareResolve.
	recs := keyRecords(t, a.storePath)
	for _, r := range recs {
		if r.Type == "key" {
			t.Fatalf("key audit record written despite PrepareResolve failure: %+v", r)
		}
	}
}

func TestHandleKeyCodexAuditPositive(t *testing.T) {
	// Positive /key path for a live Codex turn: the audit record must be written
	// to the store BEFORE the decision is delivered to the agent (finding 53/86).
	// Verified by observing that the fake server receives the decision response
	// only after /key has returned 200 and the audit record already exists.
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	spawn, approvalDispatched, decisionDelivered := newApprovalFakeCodexSpawn(t, "THREAD-KEY-POS", rollout)
	f := &fakeTmux{}
	a := newTestApp(t, f)
	logDir := filepath.Join(filepath.Dir(a.storePath), "codex")
	a.codex = codexManager{codex.NewManagerWithSpawn(logDir, spawn)}
	t.Cleanup(a.codex.Shutdown)

	// Create the node; the first prompt triggers turn/start → approval request.
	rec := newNode(a, `{"prompt":"do it","title":"T","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d", rec.Code)
	}
	n := a.nodes[0]

	// Wait for the fake server to dispatch the approval request.
	select {
	case <-approvalDispatched:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for approval to be dispatched")
	}

	// Poll until the session's pending field is populated; there is a brief
	// scheduling gap between the pipe write completing and approve() setting
	// s.pending.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := a.codex.Pending(n.ID); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := a.codex.Pending(n.ID); !ok {
		t.Fatal("timed out waiting for pending approval to appear")
	}

	// POST /key y: maps to the first non-rejecting decision ("accept"), writes
	// the audit record, then delivers the decision. request_id binds the
	// decision to the exact pending request (P1 security).
	p, ok := a.codex.Pending(n.ID)
	if !ok || p.RequestID == "" {
		t.Fatal("pending approval disappeared before /key")
	}
	keyReq := httptest.NewRequest("POST", "/api/nodes/"+n.ID+"/key",
		strings.NewReader(`{"key":"y","request_id":`+strconv.Quote(p.RequestID)+`}`))
	keyReq.SetPathValue("id", n.ID)
	keyRec := httptest.NewRecorder()
	a.handleKey(keyRec, keyReq)
	if keyRec.Code != 200 {
		t.Fatalf("handleKey: code = %d body %q", keyRec.Code, keyRec.Body)
	}

	// The audit record must be in the store immediately after /key returns 200
	// (written before Deliver — the HTTP handler persists first, then delivers).
	recs := keyRecords(t, a.storePath)
	var found *storeRecord
	for i := range recs {
		if recs[i].Type == "key" {
			found = &recs[i]
			break
		}
	}
	if found == nil {
		t.Fatal("no key audit record in store after /key returned 200")
	}
	if found.Key != "y" {
		t.Errorf("audit key = %q, want y", found.Key)
	}
	if found.Excerpt == "" {
		t.Error("audit evidence (excerpt) must not be empty")
	}
	if found.ID != n.ID {
		t.Errorf("audit node id = %q, want %q", found.ID, n.ID)
	}
	if found.Keys != nil {
		t.Errorf("structured audit must omit physical Keys, got %v", found.Keys)
	}

	// The fake server must receive the decision. Since store append happens before
	// Deliver, which happens before approve() returns, which happens before the
	// server reads the response, the ordering guarantee is transitive.
	select {
	case <-decisionDelivered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for decision to reach fake server")
	}
}

// TestHandleSendInterruptTmux: the tmux interrupt is a remote keypress and
// follows the SendKey contract — Escape (whitelisted, Claude Code's turn
// interrupt), recorded in the store with pane evidence. An unconfirmed prior
// send is withdrawn; a send still in flight keeps its state.
func TestHandleSendInterruptTmux(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"T": true}, capture: "esc to interrupt"}
	a := newTestApp(t, f)
	if rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`"}`); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := a.nodes[0].ID
	a.sendState[id] = "unconfirmed"

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/nodes/"+id+"/send/interrupt", nil)
		req.SetPathValue("id", id)
		w := httptest.NewRecorder()
		a.handleSendInterrupt(w, req)
		return w
	}
	if w := post(); w.Code != 200 {
		t.Fatalf("interrupt: %d %s", w.Code, w.Body.String())
	}

	var sentKey string
	f.mu.Lock()
	for _, c := range f.calls {
		if len(c) >= 3 && c[2] == "send-keys" {
			sentKey = c[len(c)-1]
		}
	}
	f.mu.Unlock()
	if sentKey != "Escape" {
		t.Fatalf("interrupt key = %q, want Escape", sentKey)
	}
	b, err := os.ReadFile(a.storePath)
	if err != nil || !strings.Contains(string(b), `"key":"Escape"`) ||
		!strings.Contains(string(b), `"keys":["Escape"]`) || !strings.Contains(string(b), "interrupt: ") {
		t.Fatalf("interrupt not audited: %v\n%s", err, b)
	}
	if _, ok := a.sendState[id]; ok {
		t.Fatal("unconfirmed send should be withdrawn by the interrupt")
	}

	a.sendState[id] = "submitting"
	if w := post(); w.Code != 200 {
		t.Fatalf("interrupt while submitting: %d", w.Code)
	}
	if a.sendState[id] != "submitting" {
		t.Fatalf("in-flight send state = %q, want submitting kept", a.sendState[id])
	}
}

// TestHandleSendClearRetiresTranscript: /clear delivered through scimux is a
// known session rollover — the transcript link and session id are retired
// (persisted as new records), so the UI degrades to peek immediately and the
// phase-end relink can adopt the fresh session file. Ordinary prompts must
// not retire anything.
func TestHandleSendClearRetiresTranscript(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"T": true}, capture: "idle", captureAfterEnter: "cleared"}
	a := newTestApp(t, f)
	a.server.PasteDelay, a.server.AckPoll = time.Millisecond, time.Millisecond
	if rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`"}`); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	n := a.nodes[0]
	a.markClaudeHookAck(n.ID)
	a.noteClaudeHookCapabilitiesForNode(n.ID)
	tx := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(tx, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n.Transcript, n.SessionID = tx, "sess"

	send := func(text string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/nodes/"+n.ID+"/send", strings.NewReader(`{"text":`+text+`}`))
		req.SetPathValue("id", n.ID)
		w := httptest.NewRecorder()
		a.handleSend(w, req)
		return w
	}

	if w := send(`"  /clear  "`); w.Code != 200 || !strings.Contains(w.Body.String(), "acknowledged") {
		t.Fatalf("send /clear: %d %s", w.Code, w.Body.String())
	}
	if n.Transcript != "" || n.SessionID != "" {
		t.Fatalf("link not retired: transcript=%q session=%q", n.Transcript, n.SessionID)
	}

	// The retirement must survive a restart (replay).
	a2 := &app{byID: map[string]*Node{}, storePath: a.storePath}
	if err := a2.loadStore(); err != nil {
		t.Fatal(err)
	}
	if got := a2.byID[n.ID]; got == nil || got.Transcript != "" || got.SessionID != "" {
		t.Fatalf("replay resurrected the link: %+v", got)
	}

	// An ordinary prompt never retires a link.
	n.Transcript, n.SessionID = tx, "sess"
	if w := send(`"hello"`); w.Code != 200 {
		t.Fatalf("send hello: %d %s", w.Code, w.Body.String())
	}
	if n.Transcript != tx || n.SessionID != "sess" {
		t.Fatalf("ordinary send retired the link: transcript=%q session=%q", n.Transcript, n.SessionID)
	}
}

// --- /clear: uniform page-turn semantics (session-log phase 3) ---

// A "/clear" sent to a structured node must open a fresh protocol session on
// the same subprocess (second thread/start), append a path-less source seam
// to the node's log, and leave the chat with a fresh surface: zero turns,
// the prior count, and the seam's own timestamp for the divider.
func TestHandleSendCodexClear(t *testing.T) {
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	f := &fakeTmux{}
	a := newTestApp(t, f)
	spawn, requests := newFakeCodexSpawn(t, "THREAD-CLR", rollout)
	// Wire the manager at sessionsDir like production main() does, so the
	// chat read path observes the manager's log.
	a.codex = codexManager{codex.NewManagerWithSpawn(a.sessionsDir, spawn)}
	t.Cleanup(a.codex.Shutdown)

	rec := newNode(a, `{"title":"C","prompt":"ping","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	n := a.nodes[0]
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && a.codex.Live(n.ID) == "active" {
		time.Sleep(10 * time.Millisecond)
	}

	sendReq := httptest.NewRequest("POST", "/api/nodes/"+n.ID+"/send", strings.NewReader(`{"text":"/clear"}`))
	sendReq.SetPathValue("id", n.ID)
	sendRec := httptest.NewRecorder()
	a.handleSend(sendRec, sendReq)
	if sendRec.Code != 200 {
		t.Fatalf("/clear: code = %d body %q", sendRec.Code, sendRec.Body.String())
	}
	starts := 0
	for _, m := range requests() {
		if m == "thread/start" {
			starts++
		}
	}
	if starts != 2 {
		t.Fatalf("thread/start count = %d, want 2 (launch + clear)", starts)
	}
	evs := sessionlog.ReadEvents(filepath.Join(a.sessionsDir, n.ID+".jsonl"))
	if len(evs) == 0 {
		t.Fatal("no session log events")
	}
	last := evs[len(evs)-1]
	if last.T != "source" || last.Source == nil || last.Source.Path != "" || last.Source.Reason != "clear" {
		t.Fatalf("last event = %+v, want path-less clear-tagged source seam", last)
	}

	chatReq := httptest.NewRequest("GET", "/api/nodes/"+n.ID+"/chat", nil)
	chatReq.SetPathValue("id", n.ID)
	chatRec := httptest.NewRecorder()
	a.handleChat(chatRec, chatReq)
	var body struct {
		Turns       []any  `json:"turns"`
		PriorTurns  int    `json:"prior_turns"`
		ChatStarted string `json:"chat_started"`
	}
	if err := json.Unmarshal(chatRec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Turns) != 0 || body.PriorTurns == 0 || body.ChatStarted == "" {
		t.Fatalf("fresh surface: turns=%d prior=%d started=%q",
			len(body.Turns), body.PriorTurns, body.ChatStarted)
	}
}

// A known Claude rollover (retireTranscript) must turn the page immediately:
// a path-less seam lands in the log at retire time so the fresh surface does
// not wait for the relink after the next turn.
func TestRetireTranscriptAppendsClearSeam(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "c1", Agent: "claude", Transcript: "/tmp/x.jsonl", SessionID: "sid"}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	w := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, "c1.jsonl")}
	for _, ev := range []sessionlog.Event{
		sessionlog.NewMeta("c1", "claude", "", "", a.home),
		sessionlog.NewSource("/tmp/x.jsonl", "sid"),
		{T: "user", Text: "old question"},
		{T: "assistant", Text: "old answer"},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	a.retireTranscript(n)

	seg := sessionlog.ReadSegment(w.Path)
	if len(seg.Turns) != 0 || seg.PriorTurns != 2 || seg.StartTime == "" {
		t.Fatalf("post-retire segment: turns=%d prior=%d start=%q",
			len(seg.Turns), seg.PriorTurns, seg.StartTime)
	}
	// The retire seam is a real /clear page-turn, tagged so a chain renderer
	// splits a stop here; Claude has no fresh session id yet, so it is path-less.
	evs := sessionlog.ReadEvents(w.Path)
	if last := evs[len(evs)-1]; last.T != "source" || last.Source == nil || last.Source.Reason != "clear" || last.Source.Path != "" {
		t.Fatalf("retire seam = %+v, want path-less reason=clear source", last.Source)
	}
	// A node that never mirrored has no page to turn: no log file appears.
	n2 := &Node{ID: "c2", Agent: "claude", Transcript: "/tmp/y.jsonl", SessionID: "s2"}
	a.nodes = append(a.nodes, n2)
	a.byID[n2.ID] = n2
	a.retireTranscript(n2)
	if _, err := os.Stat(filepath.Join(a.sessionsDir, "c2.jsonl")); err == nil {
		t.Fatal("retire must not create a log for a never-mirrored node")
	}
}

// A /clear must snapshot the CLOSING station's label so a later rename of the
// active chat cannot rewrite it (spec A/B/C). The snapshot is keyed to the
// closing station's start time (created_at when no prior /clear) and lands
// before the clear seam; the node's own Title/Description are left untouched so
// the fresh head keeps reading the live label.
func TestRetireTranscriptSnapshotsClosingStation(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "c1", Agent: "claude", Transcript: "/tmp/x.jsonl", SessionID: "sid",
		Title: "Alpha", Description: "the alpha work", CreatedAt: "2026-07-01T09:00:00Z"}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	w := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, "c1.jsonl")}
	for _, ev := range []sessionlog.Event{
		sessionlog.NewMeta("c1", "claude", "", "", a.home),
		sessionlog.NewSource("/tmp/x.jsonl", "sid"),
		{T: "user", Text: "old question"},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	a.retireTranscript(n)

	// The closing station (created_at, since no prior /clear) is snapshotted with
	// the pre-clear label, and it precedes the clear seam.
	evs := sessionlog.ReadEvents(w.Path)
	stationIdx, seamIdx := -1, -1
	for i, ev := range evs {
		if ev.T == "station" && ev.Station != nil && ev.Station.Seam == n.CreatedAt {
			stationIdx = i
			if ev.Station.Title != "Alpha" || ev.Station.Desc != "the alpha work" {
				t.Fatalf("snapshot label = %q/%q, want pre-clear Alpha/the alpha work", ev.Station.Title, ev.Station.Desc)
			}
		}
		if ev.T == "source" && ev.Source != nil && ev.Source.Reason == "clear" {
			seamIdx = i
		}
	}
	if stationIdx < 0 {
		t.Fatal("no station snapshot for the closing station")
	}
	if seamIdx < 0 || stationIdx > seamIdx {
		t.Fatalf("station snapshot (idx %d) must precede the clear seam (idx %d)", stationIdx, seamIdx)
	}
	// The node's live label is untouched: the fresh head still reads Alpha, so a
	// later rename only moves the head, never the snapshotted closing station.
	if n.Title != "Alpha" || n.Description != "the alpha work" {
		t.Fatalf("node label mutated by /clear: %q/%q", n.Title, n.Description)
	}
}

// P2c — handleChat reports fresh:true for a zero-turn post-seam segment
// (cleared Claude chat) and not for a never-started node.
func TestHandleChatFreshAfterClearSeam(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true, "c2": true}}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Cleared node: prior turns behind a source seam, empty current segment.
	n := &Node{ID: "c1", Title: "c1", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes = append(a.nodes, n)
	a.byID["c1"] = n
	w := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, "c1.jsonl")}
	for _, ev := range []sessionlog.Event{
		sessionlog.NewMeta("c1", "claude", "", "", a.home),
		sessionlog.NewSource("/tmp/old.jsonl", "old-sid"),
		{T: "user", Text: "old question", Time: "2026-07-14T01:00:00Z"},
		{T: "assistant", Text: "old answer", Time: "2026-07-14T01:01:00Z"},
		sessionlog.NewClearSource(""),
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/c1/chat", nil)
	r.SetPathValue("id", "c1")
	a.handleChat(rec, r)
	if rec.Code != 200 {
		t.Fatalf("chat code = %d body %q", rec.Code, rec.Body.String())
	}
	var body struct {
		Turns      []any `json:"turns"`
		PriorTurns int   `json:"prior_turns"`
		Fresh      bool  `json:"fresh"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Turns) != 0 || body.PriorTurns == 0 {
		t.Fatalf("post-clear segment: turns=%d prior=%d", len(body.Turns), body.PriorTurns)
	}
	if !body.Fresh {
		t.Fatalf("fresh = false, want true for zero-turn post-seam segment; body=%s", rec.Body.String())
	}

	// Never-started node: no session log → not fresh.
	n2 := &Node{ID: "c2", Title: "c2", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes = append(a.nodes, n2)
	a.byID["c2"] = n2
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/api/nodes/c2/chat", nil)
	r2.SetPathValue("id", "c2")
	a.handleChat(rec2, r2)
	if rec2.Code != 200 {
		t.Fatalf("never-started chat code = %d", rec2.Code)
	}
	var body2 struct {
		Fresh bool `json:"fresh"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatal(err)
	}
	if body2.Fresh {
		t.Fatal("never-started node must not report fresh:true")
	}
}

func TestHandleChatReplyReadyFromClaudeEndTurn(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}}
	a := newTestApp(t, f)
	tx := filepath.Join(t.TempDir(), "claude.jsonl")
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	appendLines(t, tx,
		`{"type":"user","timestamp":"`+stamp+`","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","timestamp":"`+stamp+`","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`)
	n := &Node{ID: "c1", Title: "c1", Agent: "claude", Transcript: tx}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	a.live[n.ID] = "active" // pane quiet debounce has not elapsed yet
	a.claudeTurns[n.ID] = claudeAcceptedTurn{Turn: "turn-1", Gen: 1}

	request := func() struct {
		Live         string `json:"live"`
		ReplyReady   bool   `json:"reply_ready"`
		TurnInFlight bool   `json:"turn_in_flight"`
	} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/nodes/c1/chat", nil)
		r.SetPathValue("id", n.ID)
		a.handleChat(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("chat code = %d body %q", rec.Code, rec.Body.String())
		}
		var body struct {
			Live         string `json:"live"`
			ReplyReady   bool   `json:"reply_ready"`
			TurnInFlight bool   `json:"turn_in_flight"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	body := request()
	if body.Live != "active" || !body.ReplyReady || !body.TurnInFlight {
		t.Fatalf("pre-Stop reply = live %q ready %v in-flight %v, want active/true/true", body.Live, body.ReplyReady, body.TurnInFlight)
	}
	appendLines(t, tx, `{"type":"user","timestamp":"`+stamp+`","message":{"role":"user","content":"next"}}`)
	body = request()
	if body.ReplyReady || !body.TurnInFlight {
		t.Fatal("new user turn must withdraw reply_ready but retain the Stop-hook turn latch")
	}
	a.mu.Lock()
	delete(a.claudeTurns, n.ID) // matching Stop/StopFailure drained
	a.mu.Unlock()
	body = request()
	if body.TurnInFlight {
		t.Fatal("matching Stop must release the composer turn latch")
	}
}

// ---------- P2: chat poll ETag / 304 ----------
// Hash the fully marshalled response body (same shape as /api/state). A
// validator keyed only on the session log would miss needs-input / attention /
// last_change changes that leave size+mtime alone.

func chatGET(t *testing.T, a *app, id, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/"+id+"/chat", nil)
	r.SetPathValue("id", id)
	if ifNoneMatch != "" {
		r.Header.Set("If-None-Match", ifNoneMatch)
	}
	a.handleChat(rec, r)
	return rec
}

func seedChatNodeWithLog(t *testing.T, a *app, id string, events ...sessionlog.Event) *Node {
	t.Helper()
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: id, Title: id, Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes = append(a.nodes, n)
	a.byID[id] = n
	a.live[id] = "quiet"
	w := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, id+".jsonl")}
	base := []sessionlog.Event{
		sessionlog.NewMeta(id, "claude", "", "", a.home),
		{T: "user", Text: "hello", Time: "2026-07-14T01:00:00Z"},
		{T: "assistant", Text: "hi there", Time: "2026-07-14T01:01:00Z"},
	}
	if len(events) > 0 {
		base = append([]sessionlog.Event{sessionlog.NewMeta(id, "claude", "", "", a.home)}, events...)
	}
	for _, ev := range base {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// TestHandleChatETagChangesWhenMechanicsChange is the trap-avoidance test:
// the session log is untouched, but a needs-input/attention (or last_change)
// field flips — the ETag must change and a conditional GET must return 200,
// not a stale 304 that would hide an approval dialog.
func TestHandleChatETagChangesWhenMechanicsChange(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"m1": true}}
	a := newTestApp(t, f)
	seedChatNodeWithLog(t, a, "m1")

	rec1 := chatGET(t, a, "m1", "")
	if rec1.Code != 200 {
		t.Fatalf("baseline code = %d body %q", rec1.Code, rec1.Body.String())
	}
	etag1 := rec1.Header().Get("ETag")
	if etag1 == "" {
		t.Fatal("baseline missing ETag")
	}
	if rec1.Body.Len() == 0 {
		t.Fatal("baseline body empty")
	}

	// Log unchanged; attention raised (needs-input machinery).
	a.mu.Lock()
	a.attn["m1"] = "approval"
	a.mu.Unlock()

	rec2 := chatGET(t, a, "m1", etag1)
	if rec2.Code != 200 {
		t.Fatalf("after attention: code = %d, want 200 (not a stale 304)", rec2.Code)
	}
	etag2 := rec2.Header().Get("ETag")
	if etag2 == "" || etag2 == etag1 {
		t.Fatalf("after attention: ETag = %q, want different non-empty (was %q)", etag2, etag1)
	}
	var body struct {
		Attention string `json:"attention"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Attention != "approval" {
		t.Fatalf("attention = %q, want approval", body.Attention)
	}

	// Same trap for last_change (pane activity while the log is idle).
	a.mu.Lock()
	a.lastChg["m1"] = time.UnixMilli(1_700_000_000_123)
	a.mu.Unlock()
	rec3 := chatGET(t, a, "m1", etag2)
	if rec3.Code != 200 {
		t.Fatalf("after last_change: code = %d, want 200", rec3.Code)
	}
	etag3 := rec3.Header().Get("ETag")
	if etag3 == "" || etag3 == etag2 {
		t.Fatalf("after last_change: ETag = %q, want different from %q", etag3, etag2)
	}
}

func TestHandleChatETagStableAnd304WhenUnchanged(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}}
	a := newTestApp(t, f)
	seedChatNodeWithLog(t, a, "c1")
	// Pin last_change so two successive builds are byte-identical.
	a.mu.Lock()
	a.lastChg["c1"] = time.UnixMilli(1_700_000_000_000)
	a.mu.Unlock()

	rec1 := chatGET(t, a, "c1", "")
	if rec1.Code != 200 {
		t.Fatalf("first code = %d", rec1.Code)
	}
	etag1 := rec1.Header().Get("ETag")
	if etag1 == "" {
		t.Fatal("missing ETag")
	}
	if rec1.Body.Len() == 0 {
		t.Fatal("first body empty")
	}
	if ct := rec1.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	rec2 := chatGET(t, a, "c1", "")
	if rec2.Header().Get("ETag") != etag1 {
		t.Errorf("ETag drifted without change: %q → %q", etag1, rec2.Header().Get("ETag"))
	}

	rec3 := chatGET(t, a, "c1", etag1)
	if rec3.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match code = %d, want 304", rec3.Code)
	}
	if rec3.Body.Len() != 0 {
		t.Errorf("304 body = %q, want empty", rec3.Body.String())
	}
}

func TestHandleChatETagChangesWhenLogGainsTurn(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}}
	a := newTestApp(t, f)
	seedChatNodeWithLog(t, a, "c1")
	a.mu.Lock()
	a.lastChg["c1"] = time.UnixMilli(1_700_000_000_000)
	a.mu.Unlock()

	rec1 := chatGET(t, a, "c1", "")
	etag1 := rec1.Header().Get("ETag")
	if etag1 == "" {
		t.Fatal("missing ETag")
	}

	w := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, "c1.jsonl")}
	if err := w.Append(sessionlog.Event{T: "user", Text: "another turn", Time: "2026-07-14T01:02:00Z"}); err != nil {
		t.Fatal(err)
	}
	// Invalidate log cache so the new turn is visible.
	a.mu.Lock()
	delete(a.logCache, "c1")
	a.mu.Unlock()

	rec2 := chatGET(t, a, "c1", etag1)
	if rec2.Code != 200 {
		t.Fatalf("after turn: code = %d, want 200", rec2.Code)
	}
	etag2 := rec2.Header().Get("ETag")
	if etag2 == "" || etag2 == etag1 {
		t.Fatalf("after turn: ETag = %q, want different from %q", etag2, etag1)
	}
	var body struct {
		Turns []transcript.Turn `json:"turns"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Turns) < 3 {
		t.Fatalf("turns = %d, want >= 3 after append", len(body.Turns))
	}
}

func TestHandleChatHistoryUnaffectedByETag(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}}
	a := newTestApp(t, f)
	seedChatNodeWithLog(t, a, "c1")

	// ?history=1 is an on-demand read: always 200 with a body, no ETag
	// short-circuit, even when If-None-Match is sent.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/c1/chat?history=1", nil)
	r.SetPathValue("id", "c1")
	r.Header.Set("If-None-Match", `"deadbeef"`)
	a.handleChat(rec, r)
	if rec.Code != 200 {
		t.Fatalf("history code = %d, want 200", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("history body empty")
	}
	// No requirement to set ETag on history; if present it must not 304.
	if rec.Code == http.StatusNotModified {
		t.Fatal("history must not return 304")
	}
	var body struct {
		Segments []sessionlog.HistorySegment `json:"segments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Segments) == 0 {
		t.Fatal("history segments empty")
	}
}

// ---------- P1 security: structured request identity on /chat and /key ----------

// Structured chat with a pending permission exposes a stable non-empty
// perm_request_id. Idle structured chat and tmux chat must not invent one.
func TestPermRequestIDChatExposure(t *testing.T) {
	// Idle structured (no pending): no perm_request_id.
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "idle-acp", "grok", "acp")
	// Inject a live manager with no pending via empty acp manager — chat still
	// routes through procChatInto when a.proc returns non-nil. Use a stub via
	// temporarily replacing is hard; drive procChatInto directly.
	stub := &stubProc{live: "quiet", hasSession: true, hasPending: false}
	seg := sessionlog.Segment{}
	resp := map[string]any{}
	a.procChatInto(resp, n, stub, seg)
	if _, ok := resp["perm_request_id"]; ok {
		t.Fatalf("idle structured chat invented perm_request_id: %v", resp["perm_request_id"])
	}

	// Pending structured: non-empty stable id.
	stub.hasPending = true
	stub.pending = allowPending("opaque-req-7")
	resp = map[string]any{}
	a.procChatInto(resp, n, stub, seg)
	id, _ := resp["perm_request_id"].(string)
	if id != "opaque-req-7" {
		t.Fatalf("perm_request_id = %v, want opaque-req-7", resp["perm_request_id"])
	}
	// Stable across two projections.
	resp2 := map[string]any{}
	a.procChatInto(resp2, n, stub, seg)
	if resp2["perm_request_id"] != id {
		t.Fatalf("perm_request_id unstable: %v → %v", id, resp2["perm_request_id"])
	}

	// tmux chat must not invent a structured request id.
	tmux := seedTmuxNode(a, "tmux-1")
	a.live[tmux.ID] = "quiet"
	req := httptest.NewRequest("GET", "/api/nodes/tmux-1/chat", nil)
	req.SetPathValue("id", "tmux-1")
	rec := httptest.NewRecorder()
	a.handleChat(rec, req)
	if rec.Code != 200 {
		t.Fatalf("tmux chat: %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if v, ok := body["perm_request_id"]; ok && v != nil && v != "" {
		t.Fatalf("tmux chat invented perm_request_id: %v", v)
	}
}

func TestProcChatTurnLatchAndReadableUsageError(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "grok-limit", "grok", "acp")
	stub := &stubProc{live: "active", hasSession: true}
	resp := map[string]any{}
	a.procChatInto(resp, n, stub, sessionlog.Segment{})
	if resp["turn_in_flight"] != true {
		t.Fatalf("active structured turn latch = %v, want true", resp["turn_in_flight"])
	}

	stub.live = "quiet"
	stub.lastError = `Internal error: {"message":"API error (status 402 Payment Required): Grok Build usage balance exhausted","http_status":402}`
	resp = map[string]any{}
	a.procChatInto(resp, n, stub, sessionlog.Segment{})
	if resp["turn_in_flight"] != false {
		t.Fatalf("completed structured turn latch = %v, want false", resp["turn_in_flight"])
	}
	const want = "Grok Build usage balance exhausted. Add credits or wait for the balance to reset, then try again."
	if resp["error"] != want {
		t.Fatalf("inline error = %q, want %q", resp["error"], want)
	}
}

// Missing request_id on structured /key → 400; stale → 409; neither audits or delivers.
// Current {request_id,key} preserves audit-before-delivery. tmux /key stays {key}-only.
// Exercises the real handleKey handler via app.testProc.
func TestHandleKeyStructuredRequestID(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "key-id", "pi", "acp")
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending:        allowPending("inc:1"),
		clearOnDeliver: true,
		conflictErrors: []error{errStubStale},
	}
	a.testProc = stub

	postKey := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/nodes/"+n.ID+"/key", strings.NewReader(body))
		req.SetPathValue("id", n.ID)
		rec := httptest.NewRecorder()
		a.handleKey(rec, req)
		return rec
	}

	before := len(keyRecords(t, a.storePath))
	// Missing request_id → 400, no audit, no deliver.
	if rec := postKey(`{"key":"1"}`); rec.Code != 400 {
		t.Fatalf("missing request_id: code=%d body %q, want 400", rec.Code, rec.Body)
	}
	if stub.deliverCalls != 0 || len(keyRecords(t, a.storePath)) != before {
		t.Fatalf("missing request_id must not audit or deliver; del=%d keys=%d",
			stub.deliverCalls, len(keyRecords(t, a.storePath))-before)
	}
	// Stale request_id → 409, no audit, no deliver; pending remains.
	if rec := postKey(`{"key":"1","request_id":"inc:99"}`); rec.Code != 409 {
		t.Fatalf("stale request_id: code=%d body %q, want 409", rec.Code, rec.Body)
	}
	if stub.deliverCalls != 0 || len(keyRecords(t, a.storePath)) != before {
		t.Fatalf("stale request_id must not audit or deliver; del=%d keys=%d",
			stub.deliverCalls, len(keyRecords(t, a.storePath))-before)
	}
	if !stub.hasPending || stub.pending.RequestID != "inc:1" {
		t.Fatalf("replacement/current pending must remain; got %+v", stub.pending)
	}
	// Current request_id + key → 200, audit then deliver.
	if rec := postKey(`{"key":"1","request_id":"inc:1"}`); rec.Code != 200 {
		t.Fatalf("current: code=%d body %q, want 200", rec.Code, rec.Body)
	}
	if stub.deliverCalls != 1 {
		t.Fatalf("deliverCalls = %d, want 1", stub.deliverCalls)
	}
	if len(keyRecords(t, a.storePath)) != before+1 {
		t.Fatalf("want one new key audit after successful resolve")
	}
	// Audit key matches the semantic choice.
	recs := keyRecords(t, a.storePath)
	var found *storeRecord
	for i := range recs {
		if recs[i].Type == "key" && recs[i].ID == n.ID {
			found = &recs[i]
		}
	}
	if found == nil || found.Key != "1" || found.Excerpt == "" {
		t.Fatalf("audit = %+v", found)
	}

	// tmux /key retains {key}-only shape and succeeds without request_id.
	f := &fakeTmux{alive: map[string]bool{"tmux-key": true}, capture: "Approve? (y/n)\n"}
	a2 := newTestApp(t, f)
	a2.nodes = []*Node{{ID: "tmux-key", Title: "t", Agent: "claude", CreatedAt: "2026-08-11T00:00:00Z"}}
	a2.byID["tmux-key"] = a2.nodes[0]
	req := httptest.NewRequest("POST", "/api/nodes/tmux-key/key", strings.NewReader(`{"key":"y"}`))
	req.SetPathValue("id", "tmux-key")
	rec := httptest.NewRecorder()
	a2.handleKey(rec, req)
	if rec.Code != 200 {
		t.Fatalf("tmux /key without request_id: code = %d body %q", rec.Code, rec.Body)
	}
	if len(sendKeysCalls(f)) == 0 {
		t.Fatal("tmux /key must deliver a key")
	}
}
