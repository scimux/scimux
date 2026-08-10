package app

// Packet 3D: Phase 3 concurrency/ownership boundary. Complements — does not
// replace — Packet 3A poll characterization, Packet 3C state projection,
// Packet 2E retire/delete ordering, TestHandleKeyTmuxEvidenceBeforeAction,
// TestHandleKeyRefusesWithoutEvidence, TestHandlePeekSpotsDialog,
// TestRemoveNodeLocked, and mirror tests. Production behavior is unchanged;
// these tests only close genuine ownership/reset and concurrent-read gaps.

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestPollConcurrentWithHandleState races the real poll loop against repeated
// handleState reads. It uses only the thread-safe fakeTmux runner, synthetic
// transcript/session-log files under t.TempDir, and never writes app maps or
// Node fields from outside a.mu merely to provoke the race detector.
func TestPollConcurrentWithHandleState(t *testing.T) {
	f := &fakeTmux{
		alive: map[string]bool{
			"n-active":   true,
			"n-quiet":    true,
			"n-discover": true,
			"n-mirror":   true,
		},
		capture: "pane line 0\nworking… 1s",
		list:    []string{"n-active", "n-quiet", "n-discover", "n-mirror"},
	}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Quiet-path attention + tailer: unresolved tool call + aged lastChg.
	quietTx := filepath.Join(t.TempDir(), "quiet.jsonl")
	appendLines(t, quietTx,
		`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{}}]}}`)

	// Mirror path: linked Claude transcript with turns the poller projects.
	mirrorTx := filepath.Join(t.TempDir(), "mirror.jsonl")
	appendFile(t, mirrorTx, claudeTurn("user", "q", "t1")+claudeTurn("assistant", "a", "t2"))

	// Discovery path: young node with SessionID; plant file under home layout.
	discoverSID := "sess-discover-3d"
	discoverPath := writeClaudeTranscript(t, a.home, discoverSID)
	appendFile(t, discoverPath, claudeTurn("user", "hi", "t0")+claudeTurn("assistant", "yo", "t1"))

	now := time.Now().UTC().Format(time.RFC3339)
	nodes := []*Node{
		{ID: "n-active", Title: "Active", Agent: "claude", Model: "opus", CreatedAt: now},
		{ID: "n-quiet", Title: "Quiet", Agent: "claude", Model: "opus", Transcript: quietTx, CreatedAt: now},
		{ID: "n-discover", Title: "Discover", Agent: "claude", Model: "opus", SessionID: discoverSID, Dir: "/w/proj", CreatedAt: now},
		{ID: "n-mirror", Title: "Mirror", Agent: "claude", Model: "opus", Transcript: mirrorTx, Dir: "/wd", CreatedAt: now},
	}
	a.mu.Lock()
	a.nodes = nodes
	for _, n := range nodes {
		a.byID[n.ID] = n
	}
	// Seed quiet/active baselines so the first ticks already exercise both
	// liveness branches, attention, prevCap, and lastChg without waiting for
	// the 8s quiet gate on every node.
	a.prevCap["n-quiet"] = "static quiet pane\nDo you want to proceed?\n  1. Yes"
	a.lastChg["n-quiet"] = time.Now().Add(-10 * time.Second)
	a.live["n-quiet"] = "quiet"
	a.prevCap["n-active"] = "pane line 0\nworking… 0s"
	a.lastChg["n-active"] = time.Now()
	a.live["n-active"] = "active"
	a.prevCap["n-mirror"] = "mirror pane"
	a.lastChg["n-mirror"] = time.Now().Add(-2 * time.Second)
	a.live["n-mirror"] = "active"
	a.mu.Unlock()

	const iterations = 80
	wantIDs := map[string]string{
		"n-active":   "Active",
		"n-quiet":    "Quiet",
		"n-discover": "Discover",
		"n-mirror":   "Mirror",
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			// Flip capture under the fake's own lock so poll sees alternate
			// pane bytes (prevCap/lastChg/live) without unsafe app-map writes.
			f.mu.Lock()
			if i%2 == 0 {
				f.capture = "pane line 0\nworking… " + time.Now().Format("15:04:05.000")
			} else {
				f.capture = "static quiet pane\nDo you want to proceed?\n  1. Yes\n  2. No"
			}
			f.mu.Unlock()
			a.poll()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/api/state", nil)
			a.handleState(rec, req)
			if rec.Code != 200 {
				errCh <- fmt.Errorf("handleState code=%d body=%q", rec.Code, rec.Body.String())
				return
			}
			var body struct {
				Nodes []struct {
					ID    string `json:"id"`
					Title string `json:"title"`
					Agent string `json:"agent"`
					Live  string `json:"live"`
				} `json:"nodes"`
				Socket   string `json:"socket"`
				Hostname string `json:"hostname"`
				Version  string `json:"version"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				errCh <- fmt.Errorf("state JSON: %v body=%q", err, rec.Body.String())
				return
			}
			if len(body.Nodes) != len(wantIDs) {
				errCh <- fmt.Errorf("nodes len=%d want %d", len(body.Nodes), len(wantIDs))
				return
			}
			seen := map[string]bool{}
			for _, n := range body.Nodes {
				title, ok := wantIDs[n.ID]
				if !ok {
					errCh <- fmt.Errorf("unexpected node id %q", n.ID)
					return
				}
				if n.Title != title {
					errCh <- fmt.Errorf("node %s title=%q want %q (partial/racy copy?)", n.ID, n.Title, title)
					return
				}
				if n.Agent != "claude" {
					errCh <- fmt.Errorf("node %s agent=%q (incomplete snapshot)", n.ID, n.Agent)
					return
				}
				// Live may race between ticks; only require a complete identity.
				_ = n.Live
				seen[n.ID] = true
			}
			for id := range wantIDs {
				if !seen[id] {
					errCh <- fmt.Errorf("missing node identity %q in state response", id)
					return
				}
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	// Postconditions prove the poll path exercised the intended fields without
	// requiring any particular interleaving of the concurrent state reads.
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.live["n-active"] == "" && a.live["n-quiet"] == "" {
		t.Fatal("poll never wrote live for active/quiet nodes")
	}
	if a.prevCap["n-active"] == "" && a.prevCap["n-quiet"] == "" {
		t.Fatal("poll never updated prevCap")
	}
	if _, ok := a.lastChg["n-active"]; !ok {
		if _, ok2 := a.lastChg["n-quiet"]; !ok2 {
			t.Fatal("poll never wrote lastChg")
		}
	}
	if a.byID["n-discover"].Transcript == "" {
		t.Fatal("discovery never linked transcript for n-discover")
	}
	if a.mirrors["n-mirror"] == nil && a.tailers["n-mirror"] == nil {
		// Mirror map entry is created on first syncMirror; tailer also installs.
		// Either proves the mirror/session-log path ran for the linked node.
		t.Fatal("neither mirror nor tailer installed for n-mirror")
	}
	// Attention may clear if capture flips on the last tick; require that the
	// quiet-path structured path at least installed a tailer or set attention.
	if a.tailers["n-quiet"] == nil && a.attn["n-quiet"] == "" {
		t.Fatal("quiet node never gained a tailer or attention")
	}
}

// ---------- handleKey: deliberate attn/attnAt clear after successful delivery ----------

// TestHandleKeyClearsAttentionOnSuccess locks the deliberate non-poller write:
// successful tmux key delivery clears attn and attnAt so the card does not
// keep claiming "approval" for the whole tool runtime (R21.2).
func TestHandleKeyClearsAttentionOnSuccess(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"k1": true}, capture: "Approve?\n  1. Yes\n"}
	a := newTestApp(t, f)
	n := &Node{ID: "k1", Title: "k1", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes, a.byID["k1"] = []*Node{n}, n
	stamp := time.Now().Add(-30 * time.Second)
	a.attn["k1"] = "approval"
	a.attnAt["k1"] = stamp

	rec := keyReq(a, "k1", `{"key":"y"}`)
	if rec.Code != 200 {
		t.Fatalf("key: code = %d body %q", rec.Code, rec.Body.String())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if got := a.attn["k1"]; got != "" {
		t.Errorf("attn after successful key = %q, want cleared", got)
	}
	if _, ok := a.attnAt["k1"]; ok {
		t.Error("attnAt still present after successful key")
	}
}

// TestHandleKeyPreservesAttentionBeforeDelivery: capture failure refuses the
// key before send-keys, so the deliberate attn clear must not run.
func TestHandleKeyPreservesAttentionBeforeDelivery(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"k2": true}, captureErr: true}
	a := newTestApp(t, f)
	n := &Node{ID: "k2", Title: "k2", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes, a.byID["k2"] = []*Node{n}, n
	stamp := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	a.attn["k2"] = "approval"
	a.attnAt["k2"] = stamp

	rec := keyReq(a, "k2", `{"key":"y"}`)
	if rec.Code != 500 {
		t.Fatalf("capture failure: code = %d, want 500", rec.Code)
	}
	for _, s := range f.subcommands() {
		if s == "send-keys" {
			t.Fatal("send-keys must not run when evidence capture failed")
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if got := a.attn["k2"]; got != "approval" {
		t.Errorf("attn after pre-delivery failure = %q, want approval", got)
	}
	if !a.attnAt["k2"].Equal(stamp) {
		t.Errorf("attnAt restamped or cleared on pre-delivery failure: %v", a.attnAt["k2"])
	}
}

// ---------- notePeekDialog: raise-only, fresh stamp, evidence required ----------

// TestNotePeekDialogOwnership covers the deliberate non-poller attention raise:
// WaitingOn+matcher raises approval/question; quietAttentionFallback raises
// dialog/inspect without WaitingOn (P1c); fresh attention stamps attnAt;
// existing attention is not overwritten or restamped; matcher failure with an
// unresolved call and no owing stall leaves attention unchanged.
func TestNotePeekDialogOwnership(t *testing.T) {
	dialogPane := "Do you want to proceed?\n  1. Yes\n  2. No\n  Esc to cancel"
	normalPane := "Agent working on files…"

	t.Run("raises_and_stamps_fresh_attention", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"p1": true}, capture: dialogPane}
		a := newTestApp(t, f)
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{}}]}}`)
		n := &Node{ID: "p1", Title: "p1", Agent: "claude", Transcript: path, CreatedAt: "2026-07-18T00:00:00Z"}
		a.nodes, a.byID["p1"] = []*Node{n}, n

		before := time.Now()
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/nodes/p1/peek", nil)
		r.SetPathValue("id", "p1")
		a.handlePeek(rec, r)
		if rec.Code != 200 {
			t.Fatalf("peek = %d", rec.Code)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if got := a.attn["p1"]; got != "approval" {
			t.Errorf("attention = %q, want approval", got)
		}
		stamped, ok := a.attnAt["p1"]
		if !ok {
			t.Fatal("attnAt not stamped for fresh attention")
		}
		if stamped.Before(before) {
			t.Errorf("attnAt %v is before peek started %v", stamped, before)
		}
	})

	t.Run("does_not_overwrite_or_restamp_existing", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"p2": true}, capture: dialogPane}
		a := newTestApp(t, f)
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"AskUserQuestion","input":{}}]}}`)
		n := &Node{ID: "p2", Title: "p2", Agent: "claude", Transcript: path, CreatedAt: "2026-07-18T00:00:00Z"}
		a.nodes, a.byID["p2"] = []*Node{n}, n
		prior := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		a.attn["p2"] = "inspect" // existing classification must stand
		a.attnAt["p2"] = prior

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/nodes/p2/peek", nil)
		r.SetPathValue("id", "p2")
		a.handlePeek(rec, r)
		if rec.Code != 200 {
			t.Fatalf("peek = %d", rec.Code)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if got := a.attn["p2"]; got != "inspect" {
			t.Errorf("attention overwritten: got %q, want inspect", got)
		}
		if !a.attnAt["p2"].Equal(prior) {
			t.Errorf("attnAt restamped: got %v want %v", a.attnAt["p2"], prior)
		}
	})

	t.Run("dialog_matcher_without_waiting_on_raises_dialog", func(t *testing.T) {
		// P1c: peek shares the quiet-branch predicate — structural/legacy
		// dialoghint alone raises "dialog" even with no unresolved tool call
		// (Claude late tool_use flush). Was "leaves_unchanged" pre-P1. The
		// predicate still requires a static pane; see
		// TestNotePeekDialogActivePaneNeedsStructuredEvidence.
		f := &fakeTmux{alive: map[string]bool{"p3": true}, capture: dialogPane}
		a := newTestApp(t, f)
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"user","timestamp":"t1","message":{"role":"user","content":"hi"}}`)
		n := &Node{ID: "p3", Title: "p3", Agent: "claude", Transcript: path, CreatedAt: "2026-07-18T00:00:00Z"}
		a.nodes, a.byID["p3"] = []*Node{n}, n
		a.lastChg["p3"] = time.Now().Add(-time.Minute)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/nodes/p3/peek", nil)
		r.SetPathValue("id", "p3")
		a.handlePeek(rec, r)
		a.mu.Lock()
		defer a.mu.Unlock()
		if got := a.attn["p3"]; got != "dialog" {
			t.Errorf("attention = %q, want dialog from matcher without WaitingOn", got)
		}
	})

	t.Run("matcher_failure_leaves_unchanged", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{"p4": true}, capture: normalPane}
		a := newTestApp(t, f)
		path := filepath.Join(t.TempDir(), "tx.jsonl")
		appendLines(t, path,
			`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{}}]}}`)
		n := &Node{ID: "p4", Title: "p4", Agent: "claude", Transcript: path, CreatedAt: "2026-07-18T00:00:00Z"}
		a.nodes, a.byID["p4"] = []*Node{n}, n

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/nodes/p4/peek", nil)
		r.SetPathValue("id", "p4")
		a.handlePeek(rec, r)
		a.mu.Lock()
		defer a.mu.Unlock()
		if got := a.attn["p4"]; got != "" {
			t.Errorf("attention raised when matcher fails: %q", got)
		}
		if _, ok := a.attnAt["p4"]; ok {
			t.Error("attnAt set when matcher fails")
		}
	})

	t.Run("missing_transcript_matcher_still_raises_dialog", func(t *testing.T) {
		// Same quiet-branch path as poller's terminal-only harness: matcher
		// alone classifies "dialog" with no transcript (P1c parity), on a
		// static pane.
		f := &fakeTmux{alive: map[string]bool{"p5": true}, capture: dialogPane}
		a := newTestApp(t, f)
		n := &Node{ID: "p5", Title: "p5", Agent: "claude", CreatedAt: "2026-07-18T00:00:00Z"}
		a.nodes, a.byID["p5"] = []*Node{n}, n
		a.lastChg["p5"] = time.Now().Add(-time.Minute)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/nodes/p5/peek", nil)
		r.SetPathValue("id", "p5")
		a.handlePeek(rec, r)
		a.mu.Lock()
		defer a.mu.Unlock()
		if got := a.attn["p5"]; got != "dialog" {
			t.Errorf("attention without transcript = %q, want dialog", got)
		}
	})
}
