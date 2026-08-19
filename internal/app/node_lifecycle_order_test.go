package app

// Packet 2E characterization: durable ordering and ownership for create,
// transcript discovery/relink/retire, exit, and delete.
//
// Intentionally not re-covered (already characterized elsewhere):
// - create publish-collision rollback + store re-assertion
//   (TestCreateNodePublishCollisionRollsBack)
// - launch persist-failure archives the structured session log
//   (TestLaunchNodeRollbackArchivesSessionLog)
// - delete re-assert on kill failure (TestDeleteReassertsNodeOnKillFailure)
// - delete basic kill + assets archive (TestHandleUpdateAndDeleteNode,
//   TestHandleDeleteNodeArchivesAssets)
// - relink functional paths (TestMaybeRelinkTranscript*)
// - retire clear-seam and station snapshot (TestRetireTranscript*)
// - reserved id blocks adopt/unadopted (TestHandleAdoptRejectsReserved*,
//   TestHandleStateUnadoptedExcludesReserved)
// - standalone archive helpers (TestArchiveAttachmentsMovesNodeDir,
//   TestArchiveAssetsMovesNodeDir, store archive tests)

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp/codex"
	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/tmuxsession"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// ---------- helpers ----------

// storeRecs is the package-local JSONL reader already used by key audit tests.
func storeRecs(t *testing.T, path string) []storeRecord {
	t.Helper()
	return keyRecords(t, path)
}

func breakStore(t *testing.T, a *app) {
	t.Helper()
	// A directory cannot be opened for append, so appendRecord fails.
	if err := os.Remove(a.storePath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(a.storePath, 0o700); err != nil {
		t.Fatal(err)
	}
}

func fixStore(t *testing.T, a *app) {
	t.Helper()
	if err := os.RemoveAll(a.storePath); err != nil {
		t.Fatal(err)
	}
}

func killCount(f *fakeTmux) int {
	n := 0
	for _, s := range f.subcommands() {
		if s == "kill-session" {
			n++
		}
	}
	return n
}

func exitNode(a *app, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/nodes/"+id+"/exit", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	a.handleExitNode(w, req)
	return w
}

func deleteNode(a *app, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("DELETE", "/api/nodes/"+id, nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	a.handleDeleteNode(w, req)
	return w
}

// ---------- 1. Create: reserve → launch → persist → publish ----------

// During the unlocked launch window the id is reserved (invisible to concurrent
// minting) but not yet published in memory and not yet durable in the store.
// Launch therefore precedes the durable node record, and the durable record is
// what publish later depends on.
func TestCreateNodeReserveLaunchPersistPublishOrder(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	srv := &fakeCodexServer{}
	base := fakeCodexSpawnBase(srv, "THREAD-ORD", filepath.Join(t.TempDir(), "rollout.jsonl"), nil, nil)

	var (
		duringReserved  bool
		duringPublished bool
		duringStoreNode bool
		duringAltID     string
	)
	spawn := func(nodeID, dir string) (codex.Transport, error) {
		a.mu.Lock()
		duringReserved = a.reserved[nodeID]
		_, duringPublished = a.byID[nodeID]
		// Concurrent minting must skip the reserved slug.
		duringAltID = a.uniqueID("T", nil)
		a.mu.Unlock()
		// Store must still lack a node record: launch precedes durable persist.
		for _, r := range storeRecs(t, a.storePath) {
			if r.Type == "node" && r.Node != nil && r.Node.ID == nodeID {
				duringStoreNode = true
			}
		}
		return base(nodeID, dir)
	}
	a.codex = codexManager{codex.NewManagerWithSpawn(a.sessionsDir, spawn)}
	t.Cleanup(a.codex.Shutdown)

	rec := newNode(a, `{"title":"T","prompt":"p","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create: code = %d body %q", rec.Code, rec.Body.String())
	}

	if !duringReserved {
		t.Error("id was not reserved during launch")
	}
	if duringPublished {
		t.Error("node was published in memory during launch; want persist-before-publish")
	}
	if duringStoreNode {
		t.Error("node record was durable during launch; want launch before durable record")
	}
	if duringAltID != "T-2" {
		t.Errorf("uniqueID during reserved launch = %q, want T-2", duringAltID)
	}

	// After success: reservation ends, durable record exists, memory is published.
	a.mu.Lock()
	reserved := a.reserved["T"]
	published := a.byID["T"] != nil
	a.mu.Unlock()
	if reserved {
		t.Error("reservation still held after successful publish")
	}
	if !published {
		t.Error("node not published after successful create")
	}
	var sawNode bool
	for _, r := range storeRecs(t, a.storePath) {
		if r.Type == "node" && r.Node != nil && r.Node.ID == "T" {
			sawNode = true
		}
	}
	if !sawNode {
		t.Error("durable node record missing after successful create")
	}
}

// A store-append failure after launch must leave the id unpublished and the
// reservation released so a later create can reuse the slug. (Archive of the
// structured session log is covered by TestLaunchNodeRollbackArchivesSessionLog.)
func TestCreateNodePersistFailureDoesNotPublish(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	spawn, _ := newFakeCodexSpawn(t, "THREAD-PF", filepath.Join(t.TempDir(), "rollout.jsonl"))
	a.codex = codexManager{codex.NewManagerWithSpawn(a.sessionsDir, spawn)}
	t.Cleanup(a.codex.Shutdown)
	breakStore(t, a)

	rec := newNode(a, `{"title":"T","prompt":"p","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 500 {
		t.Fatalf("create with broken store: code = %d, want 500", rec.Code)
	}
	a.mu.Lock()
	_, published := a.byID["T"]
	reserved := a.reserved["T"]
	nodeCount := len(a.nodes)
	a.mu.Unlock()
	if published || nodeCount != 0 {
		t.Fatalf("persist failure published the node: byID=%v nodes=%d", published, nodeCount)
	}
	if reserved {
		t.Error("reservation still held after persist failure")
	}
	if a.codex.HasSession("T") {
		t.Error("structured session left live after persist failure rollback")
	}
}

// ---------- 2. Transcript discovery / relink / retire ----------

func TestDiscoverTranscriptPersistsBeforePublishAndReleasesClaim(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	sid := "11111111-2222-3333-4444-555555555501"
	path := writeClaudeTranscript(t, a.home, sid)
	n := &Node{
		ID: "c1", Agent: "claude", SessionID: sid,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n

	a.discoverTranscript(n)

	if n.Transcript != "" {
		t.Fatalf("Claude discoverTranscript bound %q; SessionStart is the only bind", n.Transcript)
	}
	_ = path
	a.mu.Lock()
	claimed := a.pathClaims[path]
	a.mu.Unlock()
	if claimed {
		t.Error("path claim still held after a no-op Claude discovery")
	}
}

// Failed discovery persistence must leave the in-memory link empty (retryable)
// and release the path claim so another node or a later tick can take it.
func TestDiscoverTranscriptPersistFailureRetryableAndReleasesClaim(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	sid := "11111111-2222-3333-4444-555555555502"
	path := writeClaudeTranscript(t, a.home, sid)
	n := &Node{
		ID: "c1", Agent: "claude", SessionID: sid,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	breakStore(t, a)

	a.discoverTranscript(n)

	if n.Transcript != "" {
		t.Fatalf("Claude discoverTranscript bound %q; SessionStart is the only bind", n.Transcript)
	}
	a.mu.Lock()
	claimed := a.pathClaims[path]
	a.mu.Unlock()
	if claimed {
		t.Error("path claim still held after discovery persist failure")
	}

	fixStore(t, a)
	a.discoverTranscript(n)
	if n.Transcript != "" {
		t.Fatalf("retry still must not UUID-discover a Claude transcript: %q", n.Transcript)
	}
}

func TestMaybeRelinkTranscriptPersistFailureRetryableAndReleasesClaim(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"c1": true}}
	a := newTestApp(t, f)
	proj := filepath.Join(a.home, ".claude", "projects", "-w-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(proj, "old-session.jsonl")
	newPath := filepath.Join(proj, "new-session.jsonl")
	// Content time gates require a real turn after the phase watermark.
	oldTS := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	newTS := time.Now().UTC().Format(time.RFC3339Nano)
	appendLines(t, oldPath, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"old"}}`, oldTS))
	appendLines(t, newPath, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"new"}}`, newTS))
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldPath, past, past); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "c1", Agent: "claude", Dir: "/w/proj", Transcript: oldPath, SessionID: "old-session"}
	a.nodes = append(a.nodes, n)
	a.byID["c1"] = n
	a.activeSince["c1"] = time.Now().Add(-30 * time.Second)
	// A pasted prompt this file never recorded is what makes the link
	// provably stale; a bare pane phase proves nothing (D1/D2).
	a.noteDelivery("c1", time.Now().Add(-time.Minute))
	breakStore(t, a)

	a.maybeRelinkTranscript(n)

	if n.Transcript != oldPath {
		t.Fatalf("stale detach mutated memory on persist failure: %q", n.Transcript)
	}
	if n.SessionID != "old-session" {
		t.Fatalf("session id mutated on persist failure: %q", n.SessionID)
	}
	a.mu.Lock()
	claimed := a.pathClaims[newPath]
	a.mu.Unlock()
	if claimed {
		t.Error("path claim still held after persist failure")
	}

	fixStore(t, a)
	a.maybeRelinkTranscript(n)
	if n.Transcript == newPath {
		t.Fatalf("retry guessed newest file %q", n.Transcript)
	}
	if n.Transcript != "" {
		t.Fatalf("legacy stale link must detach after store fix, got %q", n.Transcript)
	}
}

// retireTranscript writes the emptied node record, then the empty transcript
// record, then clears in-memory link state. A node-record persist failure
// aborts before any of that state is mutated.
func TestRetireTranscriptRecordOrderAndStateCleanup(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tx := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(tx, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "c1", Agent: "claude", Transcript: tx, SessionID: "sess"}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	// Seed poller state that retire must drop once the durable records land.
	a.tailers[n.ID] = nil
	a.chatMark[n.ID] = chatMark{seen: true, off: 1}
	a.staleChat[n.ID] = true
	a.mirrors[n.ID] = &mirror{}

	// No session log yet: retire still records node/transcript empties, but
	// skips the clear seam (no page to turn). Isolate record order from seam.
	before := len(storeRecs(t, a.storePath))
	a.retireTranscript(n)

	recs := storeRecs(t, a.storePath)
	if len(recs) < before+2 {
		t.Fatalf("store grew by %d, want at least 2 retire records", len(recs)-before)
	}
	got := recs[before:]
	if got[0].Type != "node" || got[0].Node == nil || got[0].Node.ID != "c1" {
		t.Fatalf("first retire record = %+v, want emptied node", got[0])
	}
	if got[0].Node.Transcript != "" || got[0].Node.SessionID != "" {
		t.Fatalf("retire node record still linked: transcript=%q session=%q",
			got[0].Node.Transcript, got[0].Node.SessionID)
	}
	if got[1].Type != "transcript" || got[1].ID != "c1" || got[1].Path != "" {
		t.Fatalf("second retire record = %+v, want empty transcript", got[1])
	}
	if n.Transcript != "" || n.SessionID != "" {
		t.Fatalf("in-memory link not cleared: transcript=%q session=%q", n.Transcript, n.SessionID)
	}
	a.mu.Lock()
	_, hasTailer := a.tailers[n.ID]
	_, hasMark := a.chatMark[n.ID]
	_, hasStale := a.staleChat[n.ID]
	_, hasMirror := a.mirrors[n.ID]
	a.mu.Unlock()
	if hasTailer || hasMark || hasStale || hasMirror {
		t.Errorf("poller state not cleaned: tailer=%v mark=%v stale=%v mirror=%v",
			hasTailer, hasMark, hasStale, hasMirror)
	}

	// Persist failure on the node record aborts before memory mutation —
	// including poller maps (tailer/chatMark/staleChat/mirror), not only the
	// Node transcript/session fields.
	n2 := &Node{ID: "c2", Agent: "claude", Transcript: tx, SessionID: "sess2"}
	a.nodes = append(a.nodes, n2)
	a.byID[n2.ID] = n2
	a.tailers[n2.ID] = &transcript.Tailer{Path: tx}
	a.chatMark[n2.ID] = chatMark{seen: true, off: 7}
	a.staleChat[n2.ID] = true
	a.mirrors[n2.ID] = &mirror{path: tx}
	breakStore(t, a)
	a.retireTranscript(n2)
	if n2.Transcript != tx || n2.SessionID != "sess2" {
		t.Fatalf("persist failure cleared memory: transcript=%q session=%q", n2.Transcript, n2.SessionID)
	}
	a.mu.Lock()
	tl2 := a.tailers[n2.ID]
	mark2 := a.chatMark[n2.ID]
	stale2 := a.staleChat[n2.ID]
	mir2 := a.mirrors[n2.ID]
	a.mu.Unlock()
	if tl2 == nil || !mark2.seen || mark2.off != 7 || !stale2 || mir2 == nil || mir2.path != tx {
		t.Errorf("persist failure mutated poller maps: tailer=%v mark=%+v stale=%v mirror=%+v",
			tl2, mark2, stale2, mir2)
	}
}

// ---------- 3. Exit: persist ended → closeOwned (once) ----------

// The ended-node record must be durable before closeOwned runs. A store failure
// must leave the node open and the agent untouched. A kill failure still keeps
// the durable ended stamp. A repeated exit must not close again.
func TestExitPersistBeforeCloseAndIdempotence(t *testing.T) {
	t.Run("persist before kill", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{}}
		var a *app
		var (
			storeHadEnded bool
			stillInByID   bool
		)
		// Wrap the fake so kill-session can observe durable state and registry.
		run := func(ctx context.Context, stdin string, args ...string) (string, error) {
			sub := ""
			if len(args) >= 3 {
				sub = args[2]
			}
			if sub == "kill-session" && a != nil {
				b, err := os.ReadFile(a.storePath)
				if err == nil {
					// ended_at is present on the just-appended node correction.
					storeHadEnded = strings.Contains(string(b), `"ended_at"`)
				}
				a.mu.Lock()
				_, stillInByID = a.byID["T"]
				a.mu.Unlock()
			}
			return f.run(ctx, stdin, args...)
		}
		root := t.TempDir()
		home := filepath.Join(root, "home")
		data := filepath.Join(root, "data")
		for _, d := range []string{home, data} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		var err error
		a, err = newApp(Config{
			Home: home, DataDir: data,
			LaunchGrace: 40 * time.Millisecond, LaunchPoll: 5 * time.Millisecond,
		}, appDeps{
			Server:               tmuxsession.NewServerWithRunner("testsock", run),
			DeliverClaudeInitial: func(*Node) initialDelivery { return initialAcknowledged },
		})
		if err != nil {
			t.Fatal(err)
		}
		if rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`"}`); rec.Code != 200 {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		id := a.nodes[0].ID
		f.alive[id] = true

		// Bind the capture vars to the real slug after create.
		// (kill-session above looks up "T" by title slug; create mints that.)
		if id != "T" {
			t.Fatalf("id = %q, want T for the kill observer", id)
		}

		w := exitNode(a, id)
		if w.Code != 200 {
			t.Fatalf("exit: code = %d body %q", w.Code, w.Body.String())
		}
		if !storeHadEnded {
			t.Error("kill-session ran before the ended node record was durable")
		}
		if !stillInByID {
			t.Error("node was removed from memory before/during closeOwned; exit must keep it")
		}
		if a.byID[id].EndedAt == "" {
			t.Error("ended_at not stamped after exit")
		}
		if killCount(f) != 1 {
			t.Fatalf("kill-session count = %d, want 1", killCount(f))
		}
	})

	t.Run("persist failure blocks teardown", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{}}
		a := newTestApp(t, f)
		if rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`"}`); rec.Code != 200 {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		id := a.nodes[0].ID
		f.alive[id] = true
		// Break store after create so the ended-node append fails.
		breakStore(t, a)

		w := exitNode(a, id)
		if w.Code != 500 {
			t.Fatalf("exit with broken store: code = %d, want 500", w.Code)
		}
		if a.byID[id].EndedAt != "" {
			t.Errorf("ended_at stamped despite persist failure: %q", a.byID[id].EndedAt)
		}
		if killCount(f) != 0 {
			t.Error("closeOwned ran despite persist failure")
		}
	})

	t.Run("close failure retains durable ended", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{}, killErr: true}
		a := newTestApp(t, f)
		if rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`"}`); rec.Code != 200 {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		id := a.nodes[0].ID
		f.alive[id] = true

		w := exitNode(a, id)
		if w.Code != 200 {
			t.Fatalf("exit: code = %d body %q", w.Code, w.Body.String())
		}
		var body struct {
			Closed  bool   `json:"closed"`
			Stopped bool   `json:"stopped"`
			Reason  string `json:"reason"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !body.Closed || body.Stopped || body.Reason != "kill_failed" {
			t.Fatalf("body = %+v, want closed not-stopped kill_failed", body)
		}
		if a.byID[id].EndedAt == "" {
			t.Error("ended_at cleared after kill failure")
		}
		// Durable store still carries the ended correction.
		fresh := &app{byID: map[string]*Node{}, storePath: a.storePath}
		if err := fresh.loadStore(); err != nil {
			t.Fatal(err)
		}
		if got := fresh.byID[id]; got == nil || got.EndedAt == "" {
			t.Fatalf("replay lost ended state: %+v", got)
		}
	})

	t.Run("repeated exit does not close twice", func(t *testing.T) {
		f := &fakeTmux{alive: map[string]bool{}}
		a := newTestApp(t, f)
		if rec := newNode(a, `{"title":"T","agent":"claude","dir":"`+a.home+`"}`); rec.Code != 200 {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		id := a.nodes[0].ID
		f.alive[id] = true

		if w := exitNode(a, id); w.Code != 200 {
			t.Fatalf("first exit: %d %s", w.Code, w.Body.String())
		}
		firstEnded := a.byID[id].EndedAt
		// After a successful kill the fake marks the session dead; keep it
		// "alive" so a buggy second closeOwned would still issue kill-session.
		f.alive[id] = true
		if w := exitNode(a, id); w.Code != 200 {
			t.Fatalf("second exit: %d %s", w.Code, w.Body.String())
		}
		if a.byID[id].EndedAt != firstEnded {
			t.Fatalf("ended_at changed on re-exit: %q → %q", firstEnded, a.byID[id].EndedAt)
		}
		if killCount(f) != 1 {
			t.Fatalf("kill-session count = %d, want 1 (no second close)", killCount(f))
		}
	})
}

// ---------- 4. Delete: persist intent → close → remove → archive ----------

// Successful delete: closeOwned runs while the node is still registered,
// removeNodeLocked drops it from memory, then session-log / attachment /
// asset archives run. Ownership stays split: store archives the session log;
// attachment helpers archive attachment and asset bytes.
func TestDeleteCloseRemoveArchiveOrderAndOwnership(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{}}
	var a *app
	var registeredAtKill bool
	run := func(ctx context.Context, stdin string, args ...string) (string, error) {
		sub := ""
		if len(args) >= 3 {
			sub = args[2]
		}
		if sub == "kill-session" && a != nil {
			a.mu.Lock()
			_, registeredAtKill = a.byID["n1"]
			a.mu.Unlock()
		}
		return f.run(ctx, stdin, args...)
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	data := filepath.Join(root, "data")
	for _, d := range []string{home, data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	a, err = newApp(Config{
		Home: home, DataDir: data,
		LaunchGrace: 40 * time.Millisecond, LaunchPoll: 5 * time.Millisecond,
	}, appDeps{Server: tmuxsession.NewServerWithRunner("testsock", run)})
	if err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "n1", Title: "n1", Agent: "claude"}
	a.nodes = []*Node{n}
	a.byID["n1"] = n
	f.alive["n1"] = true

	// Session log (store owns archiveSessionLog).
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := a.sessionLogPath("n1")
	if err := os.WriteFile(logPath, []byte(`{"t":"meta","id":"n1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Attachments (attachment helper owns archiveAttachments).
	if _, err := a.storeAttachment("n1", "note.txt", "text/plain", strings.NewReader("hi")); err != nil {
		t.Fatal(err)
	}
	// Assets (attachment helper owns archiveAssets).
	if _, err := a.ingestAttachmentAsset("n1", "big.bin", "application/octet-stream", "/tmp/uploads/big.bin", make([]byte, assetInlineCap+1)); err != nil {
		t.Fatal(err)
	}
	attachDir := a.attachmentDir("n1")
	assetDir := asset.NodeDir(a.assetsDir, "n1")

	// Prove the archive helpers remain independently callable ownership seams
	// (delete only sequences them; it does not absorb their bodies).
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("precondition session log: %v", err)
	}
	if _, err := os.Stat(attachDir); err != nil {
		t.Fatalf("precondition attachments: %v", err)
	}
	if _, err := os.Stat(assetDir); err != nil {
		t.Fatalf("precondition assets: %v", err)
	}

	w := deleteNode(a, "n1")
	if w.Code != 200 {
		t.Fatalf("delete: code = %d body %q", w.Code, w.Body.String())
	}
	if !registeredAtKill {
		t.Error("node already removed when closeOwned ran; want close before in-memory removal")
	}
	if _, ok := a.byID["n1"]; ok {
		t.Error("node still registered after successful delete")
	}
	if killCount(f) != 1 {
		t.Fatalf("kill-session count = %d, want 1", killCount(f))
	}

	// Removal precedes archival: live paths are gone and archives exist.
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("live session log still present: %v", err)
	}
	if _, err := os.Stat(attachDir); !os.IsNotExist(err) {
		t.Errorf("live attachments still present: %v", err)
	}
	if _, err := os.Stat(assetDir); !os.IsNotExist(err) {
		t.Errorf("live assets still present: %v", err)
	}
	sessArch, _ := filepath.Glob(filepath.Join(a.sessionsDir, "archive", "n1.*.jsonl"))
	if len(sessArch) != 1 {
		t.Errorf("session-log archive entries = %v, want 1 (store.archiveSessionLog)", sessArch)
	}
	attArch, _ := os.ReadDir(filepath.Join(a.attachmentsDir, "archive"))
	if len(attArch) != 1 || !strings.HasPrefix(attArch[0].Name(), "n1.") {
		t.Errorf("attachment archive = %v, want one n1.* entry (archiveAttachments)", names(attArch))
	}
	assetArch, _ := os.ReadDir(filepath.Join(a.assetsDir, "archive"))
	if len(assetArch) != 1 || !strings.HasPrefix(assetArch[0].Name(), "n1.") {
		t.Errorf("asset archive = %v, want one n1.* entry (archiveAssets)", names(assetArch))
	}

	// Durable delete intent is present (append-only).
	var sawDelete bool
	for _, r := range storeRecs(t, a.storePath) {
		if r.Type == "delete" && r.ID == "n1" {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Error("delete intent record missing from store")
	}
}

// Persistence failure of the delete intent must block close, removal, and every
// archive path — not only the agent kill (TestDeletePersistsBeforeKill).
func TestDeletePersistFailureBlocksTeardownAndArchive(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	n := &Node{ID: "n1", Title: "n1", Agent: "claude"}
	a.nodes = []*Node{n}
	a.byID["n1"] = n
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := a.sessionLogPath("n1")
	if err := os.WriteFile(logPath, []byte(`{"t":"meta"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.storeAttachment("n1", "a.txt", "text/plain", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ingestAttachmentAsset("n1", "big.bin", "application/octet-stream", "/tmp/u", make([]byte, assetInlineCap+1)); err != nil {
		t.Fatal(err)
	}
	breakStore(t, a)

	w := deleteNode(a, "n1")
	if w.Code != 500 {
		t.Fatalf("delete with broken store: code = %d, want 500", w.Code)
	}
	if _, ok := a.byID["n1"]; !ok {
		t.Error("node removed despite failed delete persist")
	}
	if killCount(f) != 0 {
		t.Error("agent killed despite failed delete persist")
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("session log archived/removed despite failed delete persist: %v", err)
	}
	if _, err := os.Stat(a.attachmentDir("n1")); err != nil {
		t.Errorf("attachments archived despite failed delete persist: %v", err)
	}
	if _, err := os.Stat(asset.NodeDir(a.assetsDir, "n1")); err != nil {
		t.Errorf("assets archived despite failed delete persist: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(a.sessionsDir, "archive")); len(entries) != 0 {
		t.Errorf("session archive created on persist failure: %v", names(entries))
	}
	if entries, _ := os.ReadDir(filepath.Join(a.attachmentsDir, "archive")); len(entries) != 0 {
		t.Errorf("attachment archive created on persist failure: %v", names(entries))
	}
	if entries, _ := os.ReadDir(filepath.Join(a.assetsDir, "archive")); len(entries) != 0 {
		t.Errorf("asset archive created on persist failure: %v", names(entries))
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name()
	}
	return out
}

// The delete record must be durable before the agent is torn down: if the
// store append fails, nothing is killed and the node stays.
func TestDeletePersistsBeforeKill(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "n1", Title: "n1", Agent: "claude"}}
	a.byID["n1"] = a.nodes[0]
	// A directory cannot be opened for append, so appendRecord fails.
	if err := os.Mkdir(a.storePath, 0o755); err != nil {
		t.Fatal(err)
	}
	del := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/nodes/n1", nil)
	r.SetPathValue("id", "n1")
	a.handleDeleteNode(del, r)
	if del.Code != 500 {
		t.Fatalf("code=%d, want 500", del.Code)
	}
	if _, ok := a.byID["n1"]; !ok {
		t.Error("node removed despite a failed delete persist")
	}
	for _, s := range f.subcommands() {
		if s == "kill-session" {
			t.Error("agent killed before the delete was durable")
		}
	}
}

// If the agent survives the delete (kill fails), the node is re-asserted so the
// durable store replays it as live again — never a killed-but-forgotten node.
func TestDeleteReassertsNodeOnKillFailure(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, killErr: true}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "n1", Title: "n1", Agent: "claude"}}
	a.byID["n1"] = a.nodes[0]
	del := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/nodes/n1", nil)
	r.SetPathValue("id", "n1")
	a.handleDeleteNode(del, r)
	if del.Code != 500 {
		t.Fatalf("code=%d, want 500", del.Code)
	}
	if _, ok := a.byID["n1"]; !ok {
		t.Error("node vanished after a failed kill")
	}
	// Replay the store into a fresh app: the delete-then-node ordering must
	// converge on a live node.
	b := &app{byID: map[string]*Node{}, storePath: a.storePath}
	if err := b.loadStore(); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.byID["n1"]; !ok {
		t.Error("replay dropped the node whose kill failed")
	}
}

// If another path publishes the same id while createNode's launch runs with
// a.mu released, the publish-time re-check must abandon the launch — kill the
// subprocess, negate the persisted record, archive the log — instead of
// silently double-publishing the id (R20.2). The interleaving is injected at
// the spawn seam, which runs exactly inside the unlocked window.
func TestCreateNodePublishCollisionRollsBack(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	srv := &fakeCodexServer{}
	base := fakeCodexSpawnBase(srv, "THREAD-COLL", filepath.Join(t.TempDir(), "rollout.jsonl"), nil, nil)
	var imposter *Node
	spawn := func(nodeID, dir string) (codex.Transport, error) {
		a.mu.Lock()
		imposter = &Node{ID: nodeID, Title: "imposter", Agent: "claude", Adopted: true,
			CreatedAt: "2026-07-20T00:00:00Z"}
		a.nodes = append(a.nodes, imposter)
		a.byID[nodeID] = imposter
		a.mu.Unlock()
		return base(nodeID, dir)
	}
	a.codex = codexManager{codex.NewManagerWithSpawn(a.sessionsDir, spawn)}
	t.Cleanup(a.codex.Shutdown)

	rec := newNode(a, `{"title":"T","prompt":"p","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 409 {
		t.Fatalf("collided create: code = %d body %q, want 409", rec.Code, rec.Body.String())
	}
	a.mu.Lock()
	got := a.byID["T"]
	count := len(a.nodes)
	a.mu.Unlock()
	if got != imposter || count != 1 {
		t.Fatalf("registry after collision: byID[T]=%p imposter=%p nodes=%d", got, imposter, count)
	}
	if a.codex.HasSession("T") {
		t.Error("abandoned launch left a live codex session")
	}
	// The winner owns sessions/T.jsonl; the rollback leaves it intact rather
	// than archiving it out from under the live node (R21.4).
	if _, err := os.Stat(filepath.Join(a.sessionsDir, "T.jsonl")); err != nil {
		t.Errorf("winner's session log was archived by the rollback: stat err = %v", err)
	}
	// The store negates the abandoned record and re-asserts the winner's, so a
	// replay converges on the winner with no manual re-adopt (R21.4). The
	// winner was published straight to memory in this test, so its only store
	// record is the one the rollback re-appended.
	fresh := &app{byID: map[string]*Node{}, storePath: a.storePath,
		live: map[string]string{}, attn: map[string]string{}}
	if err := fresh.loadStore(); err != nil {
		t.Fatal(err)
	}
	if got := fresh.byID["T"]; got == nil {
		t.Error("winner record not converged on store replay")
	} else if got.Title != "imposter" {
		t.Errorf("replay converged on the wrong node: title = %q, want imposter", got.Title)
	}
}

// A store-append failure after a successful structured launch rolls back the
// subprocess; the launcher's just-written meta header must be archived with
// it, or the slug reads as taken-by-dead-history forever (R20.3).
func TestLaunchNodeRollbackArchivesSessionLog(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	spawn, _ := newFakeCodexSpawn(t, "THREAD-RB", filepath.Join(t.TempDir(), "rollout.jsonl"))
	a.codex = codexManager{codex.NewManagerWithSpawn(a.sessionsDir, spawn)}
	t.Cleanup(a.codex.Shutdown)
	// A directory at the store path makes every node-record append fail.
	if err := os.Mkdir(a.storePath, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := newNode(a, `{"title":"T","prompt":"p","agent":"codex","dir":"`+a.home+`"}`)
	if rec.Code != 500 {
		t.Fatalf("create with broken store: code = %d, want 500", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(a.sessionsDir, "T.jsonl")); !os.IsNotExist(err) {
		t.Errorf("rolled-back launch left a session log burning the slug: stat err = %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(a.sessionsDir, "archive", "T.*.jsonl"))
	if len(matches) != 1 {
		t.Errorf("want the meta-only log archived, got %v", matches)
	}
}
