package app

// Packet 4A public-route coverage for node-management HTTP bindings through
// NewHandler. Complements — does not replace — the detailed direct-handler
// matrices consolidated below or the Packet 2E durable-ordering tests in
// node_lifecycle_order_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// ---------- create / fork ----------

func TestPublicRouteCreateValidationAndLaunchFailure(t *testing.T) {
	// Validation failures never reach tmux; launch failure surfaces the CLI
	// message and leaves no published node — both via the public POST /api/nodes
	// binding (CSRF + Content-Type included).
	f := &fakeTmux{}
	a := newTestApp(t, f)
	h := newTestHandler(t, a)

	// Empty body / missing required fields → 400, no new-session.
	rec := routeRequest(h, http.MethodPost, "/api/nodes", `{}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty create: status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	// Unknown agent → 400.
	rec = routeRequest(h, http.MethodPost, "/api/nodes",
		`{"title":"T","prompt":"hi","agent":"martian","dir":`+strconv.Quote(a.home)+`}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown agent: status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	// Fork of a missing parent → 400.
	rec = routeRequest(h, http.MethodPost, "/api/nodes",
		`{"title":"Child","prompt":"hi","agent":"claude","dir":`+strconv.Quote(a.home)+`,"parent":"no-such"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing parent: status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	for _, s := range f.subcommands() {
		if s == "new-session" {
			t.Fatal("validation failure launched tmux")
		}
	}

	// Launch failure: fake pane carries the sentinel so awaitLaunch reports the
	// CLI message and rolls the session back.
	f2 := &fakeTmux{capture: "API Error: model gone\n" + launchFailSentinel + " (status 1)\n"}
	a2 := newTestApp(t, f2)
	h2 := newTestHandler(t, a2)
	rec = routeRequest(h2, http.MethodPost, "/api/nodes",
		`{"title":"Fork","agent":"claude","dir":`+strconv.Quote(a2.home)+`}`, true)
	if rec.Code == http.StatusOK {
		t.Fatalf("failed launch reported success: body=%q", rec.Body.String())
	}
	if len(a2.nodes) != 0 {
		t.Fatalf("failed launch published %d nodes", len(a2.nodes))
	}
	if !strings.Contains(rec.Body.String(), "model gone") {
		t.Errorf("launch failure body must quote CLI message, got %q", rec.Body.String())
	}
}

func TestPublicRouteCreateSuccessAndFork(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	h := newTestHandler(t, a)

	// Root create succeeds through the public binding.
	rec := routeRequest(h, http.MethodPost, "/api/nodes",
		`{"title":"Root","agent":"claude","dir":`+strconv.Quote(a.home)+`,"lane_id":"lane-a"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status = %d body %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var created Node
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.LaneID != "lane-a" || created.Title != "Root" {
		t.Fatalf("created node = %+v", created)
	}
	if !created.AXScreenReader {
		t.Error("public create of owned Claude must return ax_screen_reader:true")
	}
	if len(a.nodes) != 1 || a.byID[created.ID] == nil {
		t.Fatal("create did not publish the node in memory")
	}

	// Fork inherits the parent lane when lane_id is omitted.
	rec = routeRequest(h, http.MethodPost, "/api/nodes",
		`{"title":"Child","agent":"claude","dir":`+strconv.Quote(a.home)+`,"parent":`+strconv.Quote(created.ID)+`}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("fork: status = %d body %q", rec.Code, rec.Body.String())
	}
	var child Node
	if err := json.Unmarshal(rec.Body.Bytes(), &child); err != nil {
		t.Fatal(err)
	}
	if child.Parent != created.ID || child.LaneID != "lane-a" {
		t.Fatalf("fork parent/lane = %q/%q, want %q/lane-a", child.Parent, child.LaneID, created.ID)
	}
	if !child.AXScreenReader {
		t.Error("public fork of owned Claude must return ax_screen_reader:true")
	}
}

func TestPublicRouteClaudeInitialDeliveryEvidence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		delivery   initialDelivery
		wantLocked bool
	}{
		{"not sent before readiness", initialNotSent, false},
		{"submitted but unconfirmed", initialUnconfirmed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTmux{}
			a := newTestApp(t, f)
			a.deliverClaudeInitial = func(*Node) initialDelivery { return tc.delivery }
			h := newTestHandler(t, a)
			rec := routeRequest(h, http.MethodPost, "/api/nodes",
				`{"title":"Keep","prompt":"irreplaceable","agent":"claude","dir":`+strconv.Quote(a.home)+`}`, true)
			if rec.Code != http.StatusOK {
				t.Fatalf("create: status = %d body %q", rec.Code, rec.Body.String())
			}
			var body struct {
				ID              string          `json:"id"`
				Prompt          string          `json:"prompt"`
				InitialDelivery initialDelivery `json:"initial_delivery"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.ID == "" || body.Prompt != "irreplaceable" || body.InitialDelivery != tc.delivery {
				t.Fatalf("response = %+v", body)
			}
			a.mu.Lock()
			locked := a.sendState[body.ID] == "unconfirmed"
			a.mu.Unlock()
			if locked != tc.wantLocked {
				t.Fatalf("send lock = %v, want %v", locked, tc.wantLocked)
			}
		})
	}
}

func TestClaudeCreateHoldsSendGateDuringDeferredInitialDelivery(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"Gate": true}}
	a := newTestApp(t, f)
	started := make(chan struct{})
	release := make(chan struct{})
	a.deliverClaudeInitial = func(*Node) initialDelivery {
		close(started)
		<-release
		return initialAcknowledged
	}
	h := newTestHandler(t, a)
	created := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		created <- routeRequest(h, http.MethodPost, "/api/nodes",
			`{"title":"Gate","prompt":"first","agent":"claude","dir":`+strconv.Quote(a.home)+`}`, true)
	}()
	<-started

	send := routeRequest(h, http.MethodPost, "/api/nodes/Gate/send", `{"text":"second"}`, true)
	if send.Code != http.StatusConflict || !strings.Contains(send.Body.String(), "still in flight") {
		t.Fatalf("concurrent send = %d %q, want in-flight 409", send.Code, send.Body.String())
	}
	close(release)
	if rec := <-created; rec.Code != http.StatusOK {
		t.Fatalf("create: %d %q", rec.Code, rec.Body.String())
	}
	a.mu.Lock()
	_, held := a.sendState["Gate"]
	a.mu.Unlock()
	if held {
		t.Fatal("acknowledged initial delivery left the send gate held")
	}
}

// ---------- adopt ----------

func TestPublicRouteAdoptSessionTransportAndTranscript(t *testing.T) {
	shared := ""
	f := &fakeTmux{alive: map[string]bool{"live1": true, "claimer": true, "pending": true}}
	a := newTestApp(t, f)
	shared = filepath.Join(a.home, ".claude", "projects", "proj", "shared.jsonl")
	a.nodes = []*Node{
		{ID: "owner", Title: "owner", Agent: "claude", Transcript: shared, CreatedAt: "2026-07-14T00:00:00Z"},
	}
	a.byID["owner"] = a.nodes[0]
	a.reserved = map[string]bool{"pending": true}
	h := newTestHandler(t, a)

	// Missing session → 400.
	if rec := routeRequest(h, http.MethodPost, "/api/adopt", `{}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing session: status = %d, want 400", rec.Code)
	}
	// Dead session → 404.
	if rec := routeRequest(h, http.MethodPost, "/api/adopt", `{"session":"ghost"}`, true); rec.Code != http.StatusNotFound {
		t.Fatalf("dead session: status = %d, want 404", rec.Code)
	}
	// Codex is not adoptable (app-server transport) → 400.
	if rec := routeRequest(h, http.MethodPost, "/api/adopt",
		`{"session":"live1","agent":"codex"}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("codex adopt: status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	// Grok is a structured ACP subprocess scimux owns — not adoptable.
	if rec := routeRequest(h, http.MethodPost, "/api/adopt",
		`{"session":"live1","agent":"grok"}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("grok adopt: status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	// Transcript owned by another node → 409.
	if rec := routeRequest(h, http.MethodPost, "/api/adopt",
		`{"session":"claimer","agent":"claude","transcript":`+strconv.Quote(shared)+`}`, true); rec.Code != http.StatusConflict {
		t.Fatalf("path claim: status = %d, want 409", rec.Code)
	}
	// Transcript outside ~/.claude/projects/ → 400.
	if rec := routeRequest(h, http.MethodPost, "/api/adopt",
		`{"session":"claimer","agent":"claude","transcript":"/tmp/elsewhere.jsonl"}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("out-of-root transcript: status = %d, want 400", rec.Code)
	}
	// Reserved in-flight create id → 409.
	if rec := routeRequest(h, http.MethodPost, "/api/adopt",
		`{"session":"pending","agent":"claude"}`, true); rec.Code != http.StatusConflict {
		t.Fatalf("reserved id: status = %d, want 409", rec.Code)
	}

	// Success: live session with explicit id/dir.
	rec := routeRequest(h, http.MethodPost, "/api/adopt",
		`{"session":"live1","agent":"claude","session_id":"sid","dir":`+strconv.Quote(a.home)+`}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("adopt success: status = %d body %q", rec.Code, rec.Body.String())
	}
	var n Node
	if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
		t.Fatal(err)
	}
	if n.ID != "live1" || !n.Adopted {
		t.Fatalf("adopted node = %+v", n)
	}
	if n.AXScreenReader {
		t.Error("adopted Claude must remain ax_screen_reader:false")
	}
	if a.byID["live1"] == nil {
		t.Fatal("adopt did not register the node")
	}
}

// ---------- update (metadata + immutable lane) ----------

func TestPublicRouteUpdateMetadataAndImmutableLane(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	h := newTestHandler(t, a)

	// Seed via public create so the id is real.
	rec := routeRequest(h, http.MethodPost, "/api/nodes",
		`{"title":"T","agent":"claude","dir":`+strconv.Quote(a.home)+`}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status = %d body %q", rec.Code, rec.Body.String())
	}
	var n Node
	if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
		t.Fatal(err)
	}
	id := n.ID

	// Unknown id → 404.
	if rec := routeRequest(h, http.MethodPatch, "/api/nodes/nope",
		`{"title":"X"}`, true); rec.Code != http.StatusNotFound {
		t.Fatalf("missing node: status = %d, want 404", rec.Code)
	}

	// Title + description persist through the public PATCH binding.
	rec = routeRequest(h, http.MethodPatch, "/api/nodes/"+id,
		`{"title":"Renamed","description":"why"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status = %d body %q", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
		t.Fatal(err)
	}
	if n.Title != "Renamed" || n.Description != "why" {
		t.Fatalf("updated node = title %q desc %q", n.Title, n.Description)
	}
	if a.byID[id].Title != "Renamed" {
		t.Fatal("update did not mutate in-memory node")
	}

	// First lane assignment succeeds; reassignment is immutable → 409.
	if rec := routeRequest(h, http.MethodPatch, "/api/nodes/"+id,
		`{"lane_id":"lane-a"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("assign lane: status = %d body %q", rec.Code, rec.Body.String())
	}
	if rec := routeRequest(h, http.MethodPatch, "/api/nodes/"+id,
		`{"lane_id":"lane-b"}`, true); rec.Code != http.StatusConflict {
		t.Fatalf("reassign lane: status = %d, want 409", rec.Code)
	}
	if got := a.byID[id].LaneID; got != "lane-a" {
		t.Fatalf("lane changed to %q after rejected reassignment", got)
	}
}

// ---------- exit (owned / adopted / structured) ----------

func TestPublicRouteExitOwnedAdoptedAndStructured(t *testing.T) {
	// Owned live tmux session: closed + stopped, kill-session issued.
	t.Run("owned", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{}}
		a := newTestApp(t, f)
		h := newTestHandler(t, a)
		rec := routeRequest(h, http.MethodPost, "/api/nodes",
			`{"title":"T","agent":"claude","dir":`+strconv.Quote(a.home)+`}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		var n Node
		if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
			t.Fatal(err)
		}
		f.alive[n.ID] = true

		rec = routeRequest(h, http.MethodPost, "/api/nodes/"+n.ID+"/exit", "", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("exit: status = %d body %q", rec.Code, rec.Body.String())
		}
		var body struct {
			Closed  bool   `json:"closed"`
			Stopped bool   `json:"stopped"`
			Reason  string `json:"reason"`
			Node    Node   `json:"node"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !body.Closed || !body.Stopped || body.Reason != "" || body.Node.EndedAt == "" {
			t.Fatalf("owned exit body = %+v", body)
		}
		if a.byID[n.ID] == nil || a.byID[n.ID].EndedAt == "" {
			t.Fatal("owned exit must keep the node and stamp ended_at")
		}
		if !containsSub(f.subcommands(), "kill-session") {
			t.Fatal("owned exit did not kill the tmux session")
		}
	})

	// Adopted session: closed but not stopped (reason adopted); no kill.
	t.Run("adopted", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"live1": true}}
		a := newTestApp(t, f)
		h := newTestHandler(t, a)
		rec := routeRequest(h, http.MethodPost, "/api/adopt",
			`{"session":"live1","agent":"claude","dir":`+strconv.Quote(a.home)+`}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("adopt: %d %s", rec.Code, rec.Body.String())
		}

		rec = routeRequest(h, http.MethodPost, "/api/nodes/live1/exit", "", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("exit: status = %d body %q", rec.Code, rec.Body.String())
		}
		var body struct {
			Closed  bool   `json:"closed"`
			Stopped bool   `json:"stopped"`
			Reason  string `json:"reason"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !body.Closed || body.Stopped || body.Reason != "adopted" {
			t.Fatalf("adopted exit body = %+v, want closed not-stopped adopted", body)
		}
		if containsSub(f.subcommands(), "kill-session") {
			t.Fatal("adopted exit must not kill-session")
		}
		if a.byID["live1"] == nil || a.byID["live1"].EndedAt == "" {
			t.Fatal("adopted exit must keep the node and stamp ended_at")
		}
	})

	// Structured (codex) node: closeOwned goes through the process manager,
	// not tmux kill-session; node stays registered with ended_at.
	t.Run("structured", func(t *testing.T) {
		rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
		a, _ := newCodexTestApp(t, "THREAD-EXIT", rollout)
		h := newTestHandler(t, a)
		rec := routeRequest(h, http.MethodPost, "/api/nodes",
			`{"title":"C","prompt":"hi","agent":"codex","dir":`+strconv.Quote(a.home)+`}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("codex create: %d %s", rec.Code, rec.Body.String())
		}
		var n Node
		if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
			t.Fatal(err)
		}
		if n.Transport != "codex" {
			t.Fatalf("transport = %q, want codex", n.Transport)
		}

		rec = routeRequest(h, http.MethodPost, "/api/nodes/"+n.ID+"/exit", "", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("exit: status = %d body %q", rec.Code, rec.Body.String())
		}
		var body struct {
			Closed  bool `json:"closed"`
			Stopped bool `json:"stopped"`
			Node    Node `json:"node"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !body.Closed || !body.Stopped || body.Node.EndedAt == "" {
			t.Fatalf("structured exit body = %+v", body)
		}
		if a.byID[n.ID] == nil || a.byID[n.ID].EndedAt == "" {
			t.Fatal("structured exit must keep the node and stamp ended_at")
		}
		// Structured close must not fall through to tmux kill-session.
		// (fakeTmux on the codex test app may still have other calls.)
		if a.proc(a.byID[n.ID]) != nil && a.codex.HasSession(n.ID) {
			t.Fatal("structured session still live after exit closeOwned")
		}
	})

	// Missing node → 404.
	t.Run("missing", func(t *testing.T) {
		a := newTestApp(t, &fakeTmux{})
		h := newTestHandler(t, a)
		if rec := routeRequest(h, http.MethodPost, "/api/nodes/nope/exit", "", true); rec.Code != http.StatusNotFound {
			t.Fatalf("missing exit: status = %d, want 404", rec.Code)
		}
	})
}

// ---------- delete (persist / close-reassert / remove / archive) ----------

func TestPublicRouteDeletePersistCloseArchive(t *testing.T) {
	// Happy path: durable delete intent, kill while still registered, memory
	// removal, then session-log / attachment / asset archives — through the
	// public DELETE binding. Detailed ordering remains in Packet 2E.
	t.Run("success remove and archive", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"n1": true}}
		a := newTestApp(t, f)
		n := &Node{ID: "n1", Title: "n1", Agent: "claude"}
		a.nodes = []*Node{n}
		a.byID["n1"] = n
		if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		logPath := a.sessionLogPath("n1")
		if err := os.WriteFile(logPath, []byte(`{"t":"meta","id":"n1"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := a.storeAttachment("n1", "note.txt", "text/plain", strings.NewReader("hi")); err != nil {
			t.Fatal(err)
		}
		if _, err := a.ingestAttachmentAsset("n1", "big.bin", "application/octet-stream", "/tmp/u", make([]byte, assetInlineCap+1)); err != nil {
			t.Fatal(err)
		}
		h := newTestHandler(t, a)

		rec := routeRequest(h, http.MethodDelete, "/api/nodes/n1", "", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("delete: status = %d body %q", rec.Code, rec.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["ok"] != "deleted" {
			t.Fatalf("body = %v, want ok=deleted", body)
		}
		if _, ok := a.byID["n1"]; ok {
			t.Fatal("node still registered after delete")
		}
		if !containsSub(f.subcommands(), "kill-session") {
			t.Fatal("delete did not kill owned session")
		}
		if _, err := os.Stat(logPath); !os.IsNotExist(err) {
			t.Errorf("live session log still present: %v", err)
		}
		if _, err := os.Stat(a.attachmentDir("n1")); !os.IsNotExist(err) {
			t.Errorf("live attachments still present: %v", err)
		}
		if _, err := os.Stat(asset.NodeDir(a.assetsDir, "n1")); !os.IsNotExist(err) {
			t.Errorf("live assets still present: %v", err)
		}
		sessArch, _ := filepath.Glob(filepath.Join(a.sessionsDir, "archive", "n1.*.jsonl"))
		if len(sessArch) != 1 {
			t.Errorf("session-log archive = %v, want 1", sessArch)
		}
		var sawDelete bool
		for _, r := range keyRecords(t, a.storePath) {
			if r.Type == "delete" && r.ID == "n1" {
				sawDelete = true
			}
		}
		if !sawDelete {
			t.Error("delete intent missing from store")
		}
	})

	// Persist failure of the delete intent blocks kill and archives.
	t.Run("persist failure blocks teardown", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"n1": true}}
		a := newTestApp(t, f)
		a.nodes = []*Node{{ID: "n1", Title: "n1", Agent: "claude"}}
		a.byID["n1"] = a.nodes[0]
		if err := os.Mkdir(a.storePath, 0o755); err != nil {
			t.Fatal(err)
		}
		h := newTestHandler(t, a)
		rec := routeRequest(h, http.MethodDelete, "/api/nodes/n1", "", true)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("delete with broken store: status = %d, want 500", rec.Code)
		}
		if _, ok := a.byID["n1"]; !ok {
			t.Error("node removed despite failed delete persist")
		}
		if containsSub(f.subcommands(), "kill-session") {
			t.Error("agent killed before durable delete intent")
		}
	})

	// Close failure re-asserts the node so replay keeps it live.
	t.Run("close failure reasserts", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"n1": true}, killErr: true}
		a := newTestApp(t, f)
		a.nodes = []*Node{{ID: "n1", Title: "n1", Agent: "claude"}}
		a.byID["n1"] = a.nodes[0]
		h := newTestHandler(t, a)
		rec := routeRequest(h, http.MethodDelete, "/api/nodes/n1", "", true)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("delete with kill failure: status = %d, want 500", rec.Code)
		}
		if _, ok := a.byID["n1"]; !ok {
			t.Error("node vanished after failed kill")
		}
		fresh := &app{byID: map[string]*Node{}, storePath: a.storePath}
		if err := fresh.loadStore(); err != nil {
			t.Fatal(err)
		}
		if _, ok := fresh.byID["n1"]; !ok {
			t.Error("replay dropped node whose kill failed (re-assert missing)")
		}
	})

	// Missing node → 404.
	t.Run("missing", func(t *testing.T) {
		a := newTestApp(t, &fakeTmux{})
		h := newTestHandler(t, a)
		if rec := routeRequest(h, http.MethodDelete, "/api/nodes/nope", "", true); rec.Code != http.StatusNotFound {
			t.Fatalf("missing delete: status = %d, want 404", rec.Code)
		}
	})
}

// ---------- GET /api/agents ----------

func TestPublicRouteAgents(t *testing.T) {
	// Discovery may invoke installed model-list commands. Keep this route test
	// deterministic and preserve the test-suite rule against real agent CLIs;
	// detailed discovery behavior is covered separately with explicit fakes.
	t.Setenv("PATH", t.TempDir())
	a := newTestApp(t, &fakeTmux{})
	h := newTestHandler(t, a)
	rec := routeRequest(h, http.MethodGet, "/api/agents", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/agents: status = %d body %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// Shape is a map of harness → agentInfo; decode must succeed. Contents
	// depend on which CLIs are on PATH (and a process-wide cache); the public
	// binding contract is status + JSON, not a specific inventory.
	var body map[string]agentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode agents payload: %v body=%q", err, rec.Body.String())
	}
	// Wrong method is rejected by the router (Allow lists GET).
	rec = routeRequest(h, http.MethodPost, "/api/agents", `{}`, true)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/agents: status = %d, want 405", rec.Code)
	}
}

func TestHandleAdoptValidation(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"live1": true, "dup": true, "claimer": true}}
	a := newTestApp(t, f)
	shared := filepath.Join(a.home, ".claude", "projects", "proj", "shared.jsonl")
	a.nodes = []*Node{{ID: "dup", Title: "dup", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"},
		{ID: "owner", Title: "owner", Agent: "claude", Transcript: shared, CreatedAt: "2026-07-14T00:00:00Z"}}
	for _, n := range a.nodes {
		a.byID[n.ID] = n
	}

	if rec := adopt(a, `{}`); rec.Code != 400 {
		t.Errorf("missing session: code = %d, want 400", rec.Code)
	}
	if rec := adopt(a, `{"session":"ghost"}`); rec.Code != 404 {
		t.Errorf("dead session: code = %d, want 404", rec.Code)
	}
	if rec := adopt(a, `{"session":"dup"}`); rec.Code != 409 {
		t.Errorf("duplicate: code = %d, want 409", rec.Code)
	}
	// Codex uses the app-server protocol — adoption is rejected with a clear error.
	if rec := adopt(a, `{"session":"live1","agent":"codex","session_id":"x"}`); rec.Code != 400 {
		t.Errorf("codex adopt: code = %d, want 400", rec.Code)
	}
	// Path claim still enforced for claude.
	if rec := adopt(a, `{"session":"claimer","agent":"claude","transcript":`+strconv.Quote(shared)+`}`); rec.Code != 409 {
		t.Errorf("path claim: code = %d, want 409", rec.Code)
	}
	// A transcript override outside the claude root is rejected before any bind.
	if rec := adopt(a, `{"session":"claimer","agent":"claude","transcript":"/t/elsewhere.jsonl"}`); rec.Code != 400 {
		t.Errorf("out-of-root transcript: code = %d, want 400", rec.Code)
	}

	// Success: a claude session with an explicit id and dir.
	rec := adopt(a, `{"session":"live1","agent":"claude","session_id":"sid","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("adopt success: code = %d body %q", rec.Code, rec.Body.String())
	}
	if _, ok := a.byID["live1"]; !ok {
		t.Error("adopted node not registered")
	}
}

// An id reserved by an in-flight create must be rejected for adoption, and a
// leftover session log under the slug is a dead node's history the adopt must
// not bind onto (R20.2, R20.4).
func TestHandleAdoptRejectsReservedAndDeadSlug(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"pending": true, "deadslug": true}}
	a := newTestApp(t, f)
	a.reserved = map[string]bool{"pending": true}
	if rec := adopt(a, `{"session":"pending","agent":"claude"}`); rec.Code != 409 {
		t.Errorf("reserved id: code = %d, want 409", rec.Code)
	}
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(a.sessionsDir, "deadslug.jsonl")
	if err := os.WriteFile(logPath, []byte(`{"t":"meta"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := adopt(a, `{"session":"deadslug","agent":"claude"}`)
	if rec.Code != 409 {
		t.Errorf("dead-history slug: code = %d body %q, want 409", rec.Code, rec.Body.String())
	}
	if _, ok := a.byID["deadslug"]; ok {
		t.Error("dead-history adopt still registered a node")
	}
	if b, err := os.ReadFile(logPath); err != nil || string(b) != `{"t":"meta"}`+"\n" {
		t.Errorf("dead node's log was touched: %q %v", b, err)
	}
}

func TestHandleNewNodeValidationAndCreate(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)

	if rec := newNode(a, `{bad json`); rec.Code != 400 {
		t.Errorf("bad json: code = %d, want 400", rec.Code)
	}
	if rec := newNode(a, `{"agent":"claude","dir":"`+a.home+`"}`); rec.Code != 400 {
		t.Errorf("empty title: code = %d, want 400", rec.Code)
	}
	if rec := newNode(a, `{"title":"T","prompt":"hi","agent":"martian","dir":"`+a.home+`"}`); rec.Code != 400 {
		t.Errorf("unknown agent: code = %d, want 400", rec.Code)
	}
	// No failing request should have reached tmux new-session.
	for _, s := range f.subcommands() {
		if s == "new-session" {
			t.Fatal("a validation failure launched tmux")
		}
	}

	rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	if len(a.nodes) != 1 {
		t.Fatalf("want 1 node created, got %d", len(a.nodes))
	}
	sawNewSession := false
	for _, s := range f.subcommands() {
		if s == "new-session" {
			sawNewSession = true
		}
	}
	if !sawNewSession {
		t.Error("create did not start a tmux session")
	}
	if got := a.nodes[0].Prompt; got != "T" {
		t.Errorf("prompt default = %q, want title", got)
	}
	if got := a.nodes[0].Description; got != "T" {
		t.Errorf("description default = %q, want first prompt", got)
	}
}

// The UI offers the family alias (opus); scimux resolves it to the concrete id
// the CLI accepts (claude-opus-4-8) at launch, working around the CLI's broken
// alias resolution. The stored node keeps the durable alias; only the launched
// command carries the resolved id.
func TestHandleNewNodeResolvesClaudeModelID(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	a.claudeIDs = map[string]string{"opus": "claude-opus-4-8"}

	if rec := newNode(a, `{"title":"Fork","agent":"claude","model":"opus","effort":"medium","dir":"`+a.home+`"}`); rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	// Stored node keeps the alias (durable across model-version bumps).
	if a.nodes[0].Model != "opus" {
		t.Errorf("stored model = %q, want the alias 'opus'", a.nodes[0].Model)
	}
	// The launched command carries the resolved concrete id and the effort flag.
	var launch string
	f.mu.Lock()
	for _, c := range f.calls {
		if len(c) >= 3 && c[2] == "new-session" {
			launch = strings.Join(c, " ")
		}
	}
	f.mu.Unlock()
	if !strings.Contains(launch, "--model 'claude-opus-4-8'") {
		t.Errorf("launch did not resolve the model id: %q", launch)
	}
	if !strings.Contains(launch, "--effort 'medium'") {
		t.Errorf("launch did not pass effort: %q", launch)
	}
	if c := strings.Count(launch, "--ax-screen-reader"); c != 1 {
		t.Errorf("resolved-model launch must contain exactly one --ax-screen-reader (got %d): %q", c, launch)
	}
	if !a.nodes[0].AXScreenReader {
		t.Error("resolved-model Claude create must set AXScreenReader=true")
	}
}

// A launch that dies before its interface is ready (the real-world case: a
// forked node whose --model the CLI rejects) must surface the agent's own error
// to the create caller and leave no phantom node behind — not the opaque "dead
// session that couldn't be adopted" that this replaces. The launch wrapper holds
// the CLI's message on the pane; awaitLaunch reads it during the grace window.
func TestHandleNewNodeSurfacesLaunchFailure(t *testing.T) {
	f := &fakeTmux{capture: "API Error: model claude-4-6-opus is not available\n" +
		launchFailSentinel + " (status 1)\n"}
	a := newTestApp(t, f)

	rec := newNode(a, `{"title":"Fork","agent":"claude","dir":"`+a.home+`"}`)
	if rec.Code == 200 {
		t.Fatalf("failed launch must not report success, got 200 body %q", rec.Body.String())
	}
	if len(a.nodes) != 0 {
		t.Fatalf("failed launch must not persist a node, got %d", len(a.nodes))
	}
	if !strings.Contains(rec.Body.String(), "claude-4-6-opus") {
		t.Errorf("error must quote the CLI's own message, got %q", rec.Body.String())
	}
	// The lingering session (held open by the wrapper) must be cleaned up.
	killed := false
	for _, s := range f.subcommands() {
		if s == "kill-session" {
			killed = true
		}
	}
	if !killed {
		t.Error("failed launch must kill the session the wrapper held open")
	}
}

// The launched tmux command must carry the failure-diagnostic wrapper so a
// launch that exits early is legible; a healthy launch is otherwise untouched.
func TestHandleNewNodeWrapsLaunchCommand(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if rec := newNode(a, `{"title":"OK","agent":"claude","dir":"`+a.home+`"}`); rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	wrapped := false
	var launch string
	f.mu.Lock()
	for _, c := range f.calls {
		if len(c) >= 3 && c[2] == "new-session" {
			joined := strings.Join(c, " ")
			launch = joined
			if strings.Contains(joined, launchFailSentinel) {
				wrapped = true
			}
		}
	}
	f.mu.Unlock()
	if !wrapped {
		t.Error("new-session command was not wrapped with launch diagnostics")
	}
	if c := strings.Count(launch, "--ax-screen-reader"); c != 1 {
		t.Errorf("create launch must contain exactly one --ax-screen-reader (got %d): %q", c, launch)
	}
}

// AXScreenReader ownership: owned Claude create/fork are true; adoption and
// non-Claude creation stay false; client-forged values cannot invent AX
// semantics; launch failure leaves nothing published or persisted.
func TestAXScreenReaderOwnershipCreateForkAdopt(t *testing.T) {
	// Successful create: returned and published node is AX-marked.
	f := &fakeTmux{}
	a := newTestApp(t, f)
	rec := newNode(a, `{"title":"Root","agent":"claude","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	var created Node
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.AXScreenReader {
		t.Error("create response: want AXScreenReader=true for owned Claude")
	}
	if n := a.byID[created.ID]; n == nil || !n.AXScreenReader {
		t.Error("published node: want AXScreenReader=true")
	}
	persisted := false
	for _, r := range keyRecords(t, a.storePath) {
		if r.Type == "node" && r.Node != nil && r.Node.ID == created.ID {
			persisted = true
			if !r.Node.AXScreenReader {
				t.Error("store node record must have ax_screen_reader:true")
			}
		}
	}
	if !persisted {
		t.Fatal("create did not persist a node record")
	}

	// Fork of a legacy parent (AX false) still launches with AX true.
	fFork := &fakeTmux{}
	aFork := newTestApp(t, fFork)
	parent := &Node{
		ID: "legacy-parent", Title: "Legacy", Prompt: "p", Agent: "claude",
		Dir: aFork.home, SessionID: "legacy-sid", CreatedAt: "2026-01-01T00:00:00Z",
		AXScreenReader: false,
	}
	aFork.nodes = []*Node{parent}
	aFork.byID = map[string]*Node{parent.ID: parent}
	rec = newNode(aFork, `{"title":"Child","agent":"claude","dir":"`+aFork.home+`","parent":"legacy-parent"}`)
	if rec.Code != 200 {
		t.Fatalf("fork: code = %d body %q", rec.Code, rec.Body.String())
	}
	var child Node
	if err := json.Unmarshal(rec.Body.Bytes(), &child); err != nil {
		t.Fatal(err)
	}
	if !child.AXScreenReader {
		t.Error("Claude fork of legacy parent must have AXScreenReader=true")
	}
	if parent.AXScreenReader {
		t.Error("fork must not mutate the legacy parent's AXScreenReader")
	}
	var forkLaunch string
	fFork.mu.Lock()
	for _, c := range fFork.calls {
		if len(c) >= 3 && c[2] == "new-session" {
			forkLaunch = strings.Join(c, " ")
		}
	}
	fFork.mu.Unlock()
	if c := strings.Count(forkLaunch, "--ax-screen-reader"); c != 1 {
		t.Errorf("fork launch must contain exactly one --ax-screen-reader (got %d): %q", c, forkLaunch)
	}

	// Adopted Claude remains false.
	fAdopt := &fakeTmux{alive: map[string]bool{"adopt-me": true}}
	aAdopt := newTestApp(t, fAdopt)
	rec = adopt(aAdopt, `{"session":"adopt-me","agent":"claude","dir":`+strconv.Quote(aAdopt.home)+`}`)
	if rec.Code != 200 {
		t.Fatalf("adopt: code = %d body %q", rec.Code, rec.Body.String())
	}
	var adopted Node
	if err := json.Unmarshal(rec.Body.Bytes(), &adopted); err != nil {
		t.Fatal(err)
	}
	if adopted.AXScreenReader {
		t.Error("adopted Claude must remain AXScreenReader=false")
	}
	if aAdopt.byID["adopt-me"].AXScreenReader {
		t.Error("published adopted node must remain AXScreenReader=false")
	}

	// Adopted non-Claude tmux sessions remain false too; an unknown request
	// field cannot opt a legacy pi pane into Claude AX key semantics.
	fAdoptPi := &fakeTmux{alive: map[string]bool{"adopt-pi": true}}
	aAdoptPi := newTestApp(t, fAdoptPi)
	rec = adopt(aAdoptPi, `{"session":"adopt-pi","agent":"pi","dir":`+strconv.Quote(aAdoptPi.home)+`,"ax_screen_reader":true}`)
	if rec.Code != 200 {
		t.Fatalf("adopt pi: code = %d body %q", rec.Code, rec.Body.String())
	}
	var adoptedPi Node
	if err := json.Unmarshal(rec.Body.Bytes(), &adoptedPi); err != nil {
		t.Fatal(err)
	}
	if adoptedPi.AXScreenReader {
		t.Error("adopted non-Claude node must remain AXScreenReader=false")
	}

	// Non-Claude create (codex) stays false even if the client supplies true.
	aCodex, _ := newCodexTestApp(t, "THREAD-AX", filepath.Join(t.TempDir(), "rollout.jsonl"))
	rec = newNode(aCodex, `{"title":"C","prompt":"hi","agent":"codex","dir":`+strconv.Quote(aCodex.home)+`,"ax_screen_reader":true}`)
	if rec.Code != 200 {
		t.Fatalf("codex create: code = %d body %q", rec.Code, rec.Body.String())
	}
	var codexNode Node
	if err := json.Unmarshal(rec.Body.Bytes(), &codexNode); err != nil {
		t.Fatal(err)
	}
	if codexNode.AXScreenReader {
		t.Error("codex create must not set AXScreenReader")
	}

	// Launch failure: no published node, no store record.
	fFail := &fakeTmux{capture: "API Error: model gone\n" + launchFailSentinel + " (status 1)\n"}
	aFail := newTestApp(t, fFail)
	rec = newNode(aFail, `{"title":"Boom","agent":"claude","dir":`+strconv.Quote(aFail.home)+`,"ax_screen_reader":true}`)
	if rec.Code == 200 {
		t.Fatal("failed launch must not succeed")
	}
	if len(aFail.nodes) != 0 {
		t.Fatalf("failed launch published %d nodes", len(aFail.nodes))
	}
	for _, r := range keyRecords(t, aFail.storePath) {
		if r.Type == "node" {
			t.Fatalf("failed launch must not persist a node, got AX=%v id=%q", r.Node.AXScreenReader, r.Node.ID)
		}
	}
}

// A create request supplies launch config only; identity, adoption, the ended
// cap, the linked transcript, and AX launch mode are server-owned and must
// never be trusted from the body (adopted → an owned session is never killed;
// ended_at → a node born closed; transcript → an arbitrary mirror bind with no
// pathClaimed check; ax_screen_reader → claim AX key semantics for an ordinary
// pane). For a successful owned Claude launch the marker is still true — set
// by launchNode, not by the scrubbed client claim.
func TestHandleNewNodeScrubsServerOwnedFields(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	body := `{"title":"Sneaky","agent":"claude","dir":"` + a.home + `",` +
		`"id":"pwned","adopted":true,"ended_at":"2020-01-01T00:00:00Z",` +
		`"transcript":"/etc/shadow","session_id":"forged","fork_kind":"s",` +
		`"ax_screen_reader":true}`
	rec := newNode(a, body)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	if len(a.nodes) != 1 {
		t.Fatalf("want 1 node, got %d", len(a.nodes))
	}
	n := a.nodes[0]
	if n.Adopted {
		t.Error("adopted trusted from body — an owned session would then never be killed")
	}
	if n.EndedAt != "" {
		t.Errorf("ended_at trusted from body: %q (node born closed)", n.EndedAt)
	}
	if n.Transcript != "" {
		t.Errorf("transcript trusted from body: %q (arbitrary mirror bind)", n.Transcript)
	}
	if n.ID == "pwned" {
		t.Error("id trusted from body instead of minted server-side")
	}
	if n.ForkKind != "" {
		t.Errorf("fork_kind trusted from body: %q (a root has no fork)", n.ForkKind)
	}
	// A claude node's session id is minted server-side, never the forged value.
	if n.SessionID == "forged" {
		t.Error("session_id trusted from body")
	}
	// Client-supplied ax_screen_reader cannot manufacture AX semantics on its
	// own; the authoritative launch still marks owned Claude true.
	if !n.AXScreenReader {
		t.Error("owned Claude launch must set AXScreenReader=true after scrubbing the client claim")
	}
	// Response body also reports the server-owned value.
	var resp Node
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.AXScreenReader {
		t.Error("create response must report ax_screen_reader:true for owned Claude")
	}
}

// forkKind is fixed at creation so the map cannot reclassify it later when a
// sibling is deleted: same lane as the parent → y-stay, a lane that already
// holds a station → s (crossover), an unused lane → y-new.
func TestForkKindRecordedAtCreation(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if rec := newNode(a, `{"title":"Root","agent":"claude","dir":"`+a.home+`","lane_id":"topic-x"}`); rec.Code != 200 {
		t.Fatalf("root: %d %s", rec.Code, rec.Body.String())
	}
	root := a.nodes[0]
	if root.ForkKind != "" {
		t.Errorf("root fork_kind = %q, want empty (a root has no fork)", root.ForkKind)
	}
	fork := func(lane string) *Node {
		body := `{"title":"F","agent":"claude","dir":"` + a.home + `","parent":"` + root.ID + `","lane_id":"` + lane + `"}`
		rec := newNode(a, body)
		if rec.Code != 200 {
			t.Fatalf("fork into %q: %d %s", lane, rec.Code, rec.Body.String())
		}
		return a.nodes[len(a.nodes)-1]
	}
	if k := fork("topic-x").ForkKind; k != "y-stay" {
		t.Errorf("fork into the parent's lane: fork_kind = %q, want y-stay", k)
	}
	if k := fork("topic-y").ForkKind; k != "y-new" {
		t.Errorf("fork into an unused lane: fork_kind = %q, want y-new", k)
	}
	if k := fork("topic-y").ForkKind; k != "s" {
		t.Errorf("fork into a lane that now holds a station: fork_kind = %q, want s", k)
	}
}

func TestHandleNodeLaneAssignmentIsOneWay(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"T": true}}
	a := newTestApp(t, f)
	rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	id := a.nodes[0].ID

	req := httptest.NewRequest("PATCH", "/api/nodes/"+id, strings.NewReader(`{"lane_id":"lane-a"}`))
	req.SetPathValue("id", id)
	up := httptest.NewRecorder()
	a.handleUpdateNode(up, req)
	if up.Code != 200 {
		t.Fatalf("assign lane: code = %d body %q", up.Code, up.Body.String())
	}
	if got := a.nodes[0].LaneID; got != "lane-a" {
		t.Fatalf("lane = %q, want lane-a", got)
	}

	req = httptest.NewRequest("PATCH", "/api/nodes/"+id, strings.NewReader(`{"lane_id":"lane-b"}`))
	req.SetPathValue("id", id)
	up = httptest.NewRecorder()
	a.handleUpdateNode(up, req)
	if up.Code != 409 {
		t.Fatalf("reassign lane: code = %d body %q, want 409", up.Code, up.Body.String())
	}
	if got := a.nodes[0].LaneID; got != "lane-a" {
		t.Fatalf("lane changed to %q", got)
	}
}

// The lane is set-once (immutable once assigned); title and description stay
// mutable. Interchange and Service are gone — the model derives topology from
// forks, not hand-tagged fields.
func TestHandleNodeLaneImmutableAndEditable(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`"}`) // no lane yet
	if rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	id := a.nodes[0].ID
	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PATCH", "/api/nodes/"+id, strings.NewReader(body))
		req.SetPathValue("id", id)
		up := httptest.NewRecorder()
		a.handleUpdateNode(up, req)
		return up
	}

	// First assignment sets the lane; a second, different value is rejected.
	if up := patch(`{"lane_id":"lane-a"}`); up.Code != 200 {
		t.Fatalf("assign lane: code = %d body %q", up.Code, up.Body.String())
	}
	if got := a.nodes[0].LaneID; got != "lane-a" {
		t.Fatalf("lane_id = %q, want lane-a", got)
	}
	if up := patch(`{"lane_id":"lane-b"}`); up.Code != 409 {
		t.Fatalf("reassign lane: code = %d, want 409", up.Code)
	}

	// Title and description remain mutable.
	if up := patch(`{"title":"Renamed","description":"why"}`); up.Code != 200 {
		t.Fatalf("edit title/desc: code = %d body %q", up.Code, up.Body.String())
	}
	if a.nodes[0].Title != "Renamed" || a.nodes[0].Description != "why" {
		t.Fatalf("title/desc = %q/%q", a.nodes[0].Title, a.nodes[0].Description)
	}
}

// /exit marks a thread ended (dead-end cap) and stops its process, but keeps
// the node on the map — unlike delete, which removes it.
func TestHandleExitNodeMarksEndedAndKeepsNode(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`","lane_id":"lane-a"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	id := a.nodes[0].ID

	req := httptest.NewRequest("POST", "/api/nodes/"+id+"/exit", nil)
	req.SetPathValue("id", id)
	up := httptest.NewRecorder()
	a.handleExitNode(up, req)
	if up.Code != 200 {
		t.Fatalf("exit: code = %d body %q", up.Code, up.Body.String())
	}
	n, ok := a.byID[id]
	if !ok {
		t.Fatal("node removed by /exit; want it to stay visible")
	}
	if n.EndedAt == "" {
		t.Fatal("ended_at not stamped")
	}
	// Idempotent: a second /exit keeps the original stamp and does not error.
	first := n.EndedAt
	up2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/api/nodes/"+id+"/exit", nil)
	req2.SetPathValue("id", id)
	a.handleExitNode(up2, req2)
	if up2.Code != 200 || a.byID[id].EndedAt != first {
		t.Fatalf("second exit: code = %d ended_at = %q want %q", up2.Code, a.byID[id].EndedAt, first)
	}
}

// /exit must tell the truth about whether it actually stopped the process, so
// the UI never claims a stop it did not deliver: an owned live tmux session is
// killed (stopped), an adopted one is deliberately left running, and a failed
// kill is reported as not stopped. All three still mark the node closed.
func TestHandleExitNodeReportsProcessOutcome(t *testing.T) {
	exit := func(a *app, id string) (closed, stopped bool, reason string) {
		req := httptest.NewRequest("POST", "/api/nodes/"+id+"/exit", nil)
		req.SetPathValue("id", id)
		up := httptest.NewRecorder()
		a.handleExitNode(up, req)
		if up.Code != 200 {
			t.Fatalf("exit: code = %d body %q", up.Code, up.Body.String())
		}
		var body struct {
			Closed  bool   `json:"closed"`
			Stopped bool   `json:"stopped"`
			Reason  string `json:"reason"`
		}
		if err := json.Unmarshal(up.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Closed, body.Stopped, body.Reason
	}
	t.Run("owned live session is killed", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{}}
		a := newTestApp(t, f)
		newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`","lane_id":"lane-a"}`)
		id := a.nodes[0].ID
		f.alive[id] = true
		closed, stopped, reason := exit(a, id)
		if !closed || !stopped || reason != "" {
			t.Fatalf("owned: closed=%v stopped=%v reason=%q, want closed stopped no-reason", closed, stopped, reason)
		}
		if !containsSub(f.subcommands(), "kill-session") {
			t.Fatal("owned live session was not killed")
		}
	})
	t.Run("adopted session is left running", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{}}
		a := newTestApp(t, f)
		newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`","lane_id":"lane-a"}`)
		id := a.nodes[0].ID
		a.byID[id].Adopted = true
		f.alive[id] = true
		closed, stopped, reason := exit(a, id)
		if !closed || stopped || reason != "adopted" {
			t.Fatalf("adopted: closed=%v stopped=%v reason=%q, want closed not-stopped adopted", closed, stopped, reason)
		}
		if containsSub(f.subcommands(), "kill-session") {
			t.Fatal("adopted session was killed; it must be left running")
		}
	})
	t.Run("failed kill is reported not stopped", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{}, killErr: true}
		a := newTestApp(t, f)
		newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`","lane_id":"lane-a"}`)
		id := a.nodes[0].ID
		f.alive[id] = true
		closed, stopped, reason := exit(a, id)
		if !closed || stopped || reason != "kill_failed" {
			t.Fatalf("kill fail: closed=%v stopped=%v reason=%q, want closed not-stopped kill_failed", closed, stopped, reason)
		}
	})
}

func TestHandleNewNodeInheritsParentLane(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	parent := newNode(a, `{"title":"Parent","agent":"claude","dir":"`+a.home+`","lane_id":"lane-a"}`)
	if parent.Code != 200 {
		t.Fatalf("parent: code = %d body %q", parent.Code, parent.Body.String())
	}
	child := newNode(a, `{"title":"Child","parent":"`+a.nodes[0].ID+`","description":"follow","agent":"claude","dir":"`+a.home+`"}`)
	if child.Code != 200 {
		t.Fatalf("child: code = %d body %q", child.Code, child.Body.String())
	}
	if got := a.nodes[1].LaneID; got != "lane-a" {
		t.Fatalf("child lane = %q, want lane-a", got)
	}
}

func TestHandleUpdateAndDeleteNode(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"T": true}}
	a := newTestApp(t, f)
	rec := newNode(a, `{"title":"T","description":"first","agent":"claude","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	id := a.nodes[0].ID

	req := httptest.NewRequest("PATCH", "/api/nodes/"+id, strings.NewReader(`{"description":"edited"}`))
	req.SetPathValue("id", id)
	up := httptest.NewRecorder()
	a.handleUpdateNode(up, req)
	if up.Code != 200 {
		t.Fatalf("update: code = %d body %q", up.Code, up.Body.String())
	}
	if got := a.nodes[0].Description; got != "edited" {
		t.Fatalf("description = %q, want edited", got)
	}
	req = httptest.NewRequest("PATCH", "/api/nodes/"+id, strings.NewReader(`{"title":"Renamed"}`))
	req.SetPathValue("id", id)
	up = httptest.NewRecorder()
	a.handleUpdateNode(up, req)
	if up.Code != 200 {
		t.Fatalf("title update: code = %d body %q", up.Code, up.Body.String())
	}
	if got := a.nodes[0].Title; got != "Renamed" {
		t.Fatalf("title = %q, want Renamed", got)
	}

	delReq := httptest.NewRequest("DELETE", "/api/nodes/"+id, nil)
	delReq.SetPathValue("id", id)
	del := httptest.NewRecorder()
	a.handleDeleteNode(del, delReq)
	if del.Code != 200 {
		t.Fatalf("delete: code = %d body %q", del.Code, del.Body.String())
	}
	if len(a.nodes) != 0 {
		t.Fatalf("nodes after delete = %d, want 0", len(a.nodes))
	}
	sawKill := false
	for _, s := range f.subcommands() {
		if s == "kill-session" {
			sawKill = true
		}
	}
	if !sawKill {
		t.Fatal("delete did not close owned tmux session")
	}
}

// Phase 6: deleting a node must archive its blob-stored session assets, not
// leave them dangling under the live assets directory, and the download
// endpoint must go 404 once the node record itself is gone.
func TestHandleDeleteNodeArchivesAssets(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "n1", Title: "n1", Agent: "claude"}}
	a.byID["n1"] = a.nodes[0]
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ev, err := a.ingestAttachmentAsset("n1", "big.bin", "application/octet-stream", "/tmp/uploads/big.bin", make([]byte, assetInlineCap+1))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Storage != "blob" {
		t.Fatalf("storage = %q, want blob", ev.Storage)
	}
	nodeDir := asset.NodeDir(a.assetsDir, "n1")
	if _, err := os.Stat(nodeDir); err != nil {
		t.Fatalf("precondition: node asset dir missing: %v", err)
	}

	del := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/nodes/n1", nil)
	r.SetPathValue("id", "n1")
	a.handleDeleteNode(del, r)
	if del.Code != 200 {
		t.Fatalf("delete: code = %d body %q", del.Code, del.Body.String())
	}

	if _, err := os.Stat(nodeDir); !os.IsNotExist(err) {
		t.Errorf("node asset dir still present after delete: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(a.assetsDir, "archive"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("archive entries = %d, err=%v, want 1", len(entries), err)
	}

	rec := serveAsset(a, "n1", ev.ID)
	if rec.Code != 404 {
		t.Errorf("download after delete: code = %d, want 404", rec.Code)
	}
}

func TestHandleNewNodeCodexCreatesNode(t *testing.T) {
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	a, requests := newCodexTestApp(t, "THREAD-HTTP", rollout)

	rec := newNode(a, `{"prompt":"research this","title":"R","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("codex create: code = %d body %q", rec.Code, rec.Body.String())
	}
	// Node must be registered with transport:"codex".
	if len(a.nodes) != 1 || a.nodes[0].Transport != "codex" {
		t.Fatalf("node not registered or wrong transport: %+v", a.nodes)
	}
	// Manager must have run both initialize AND thread/start (finding 83):
	// Launch is synchronous for both calls, so both are recorded before
	// newNode returns 200.
	rlist := requests()
	var sawInit, sawThread bool
	for _, m := range rlist {
		if m == "initialize" {
			sawInit = true
		}
		if m == "thread/start" {
			sawThread = true
		}
	}
	if !sawInit || !sawThread {
		t.Fatalf("want both initialize and thread/start, got: %v", rlist)
	}
	// SessionID must be the thread id from thread/start.
	if a.nodes[0].SessionID != "THREAD-HTTP" {
		t.Errorf("session id = %q, want THREAD-HTTP", a.nodes[0].SessionID)
	}
}

// Editing an OLD station on the map (PATCH with a station seam) writes a label
// snapshot for that station only and never touches the node record — so it
// cannot bleed into the head or any other station (spec E). A PATCH without a
// station still edits the node (the head path, unchanged).
func TestHandleUpdateNodeStationOverride(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := newNode(a, `{"title":"Head","description":"head desc","agent":"claude","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}
	id := a.nodes[0].ID
	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PATCH", "/api/nodes/"+id, strings.NewReader(body))
		req.SetPathValue("id", id)
		up := httptest.NewRecorder()
		a.handleUpdateNode(up, req)
		return up
	}

	// Edit an old station: writes a station snapshot, node untouched.
	if up := patch(`{"station":"2026-07-01T09:00:00Z","title":"Old renamed","description":"old desc"}`); up.Code != 200 {
		t.Fatalf("station edit: code = %d body %q", up.Code, up.Body.String())
	}
	if a.nodes[0].Title != "Head" || a.nodes[0].Description != "head desc" {
		t.Fatalf("node label changed by a station edit: %q/%q", a.nodes[0].Title, a.nodes[0].Description)
	}
	seg := sessionlog.ReadSegment(filepath.Join(a.sessionsDir, id+".jsonl"))
	if got := seg.Stations["2026-07-01T09:00:00Z"]; got.Title != "Old renamed" || got.Desc != "old desc" {
		t.Fatalf("station snapshot = %+v, want Old renamed/old desc", got)
	}

	// A PATCH without a station still edits the node (head path).
	if up := patch(`{"title":"Head2"}`); up.Code != 200 {
		t.Fatalf("head edit: code = %d body %q", up.Code, up.Body.String())
	}
	if a.nodes[0].Title != "Head2" {
		t.Fatalf("head title = %q, want Head2", a.nodes[0].Title)
	}
}
