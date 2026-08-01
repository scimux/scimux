package main

// Packet 4A public-route coverage for node-management HTTP bindings through
// NewHandler. Complements — does not replace — the detailed direct-handler
// matrices in main_http_test.go or the Packet 2E durable-ordering tests in
// node_lifecycle_order_test.go.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/asset"
)

// nodeAPIHandler builds the production router for public-route assertions.
func nodeAPIHandler(t *testing.T, a *app) http.Handler {
	t.Helper()
	h, err := NewHandler(a, webFS)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// ---------- create / fork ----------

func TestPublicRouteCreateValidationAndLaunchFailure(t *testing.T) {
	// Validation failures never reach tmux; launch failure surfaces the CLI
	// message and leaves no published node — both via the public POST /api/nodes
	// binding (CSRF + Content-Type included).
	f := &fakeTmux{}
	a := newTestApp(t, f)
	h := nodeAPIHandler(t, a)

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
	h2 := nodeAPIHandler(t, a2)
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
	h := nodeAPIHandler(t, a)

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
	h := nodeAPIHandler(t, a)

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
	if a.byID["live1"] == nil {
		t.Fatal("adopt did not register the node")
	}
}

// ---------- update (metadata + immutable lane) ----------

func TestPublicRouteUpdateMetadataAndImmutableLane(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	h := nodeAPIHandler(t, a)

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
		h := nodeAPIHandler(t, a)
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
		h := nodeAPIHandler(t, a)
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
		h := nodeAPIHandler(t, a)
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
		h := nodeAPIHandler(t, a)
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
		h := nodeAPIHandler(t, a)

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
		h := nodeAPIHandler(t, a)
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
		h := nodeAPIHandler(t, a)
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
		h := nodeAPIHandler(t, a)
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
	h := nodeAPIHandler(t, a)
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
