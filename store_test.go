package main

// Packet 2A characterization tests for the append-only node store.
// These lock current loadStore / appendRecord / session-log archive behavior
// before the mechanical extraction in Packet 2B. They must not change
// production code or weaken the existing store tests in main_test.go /
// app_test.go.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeStoreLines builds a nodes.jsonl body. When finalNL is false, the last
// record has no trailing newline (EOF mid-file is still a valid last record).
func writeStoreLines(t *testing.T, path string, finalNL bool, lines ...string) {
	t.Helper()
	var b strings.Builder
	for i, line := range lines {
		b.WriteString(line)
		if i < len(lines)-1 || finalNL {
			b.WriteByte('\n')
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadFromLines(t *testing.T, finalNL bool, lines ...string) *app {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nodes.jsonl")
	writeStoreLines(t, path, finalNL, lines...)
	a := &app{byID: map[string]*Node{}, storePath: path}
	if err := a.loadStore(); err != nil {
		t.Fatalf("loadStore: %v", err)
	}
	return a
}

func nodeIDs(a *app) []string {
	ids := make([]string, len(a.nodes))
	for i, n := range a.nodes {
		ids[i] = n.ID
	}
	return ids
}

func storeNodeLine(t *testing.T, id, title, prompt, desc string) string {
	t.Helper()
	n := map[string]any{
		"id":         id,
		"title":      title,
		"prompt":     prompt,
		"agent":      "claude",
		"dir":        "/tmp",
		"created_at": "2026-07-01T08:00:00Z",
	}
	if desc != "" {
		n["description"] = desc
	}
	b, err := json.Marshal(map[string]any{"type": "node", "node": n})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func storeTxLine(id, path string) string {
	return fmt.Sprintf(`{"type":"transcript","id":%q,"path":%q}`, id, path)
}

func storeDelLine(id string) string {
	return fmt.Sprintf(`{"type":"delete","id":%q,"time":"2026-07-01T09:00:00Z"}`, id)
}

func TestLoadStoreReplayMatrix(t *testing.T) {
	t.Run("correction_replaces_without_duplicate_keeps_first_seen_order", func(t *testing.T) {
		// Complements TestLoadStoreReplayCorrections with an explicit
		// three-node first-seen check and a later middle-node correction.
		a := loadFromLines(t, true,
			storeNodeLine(t, "a", "A", "pa", "da"),
			storeNodeLine(t, "b", "B", "pb", "db"),
			storeNodeLine(t, "c", "C", "pc", "dc"),
			storeNodeLine(t, "b", "B-fixed", "pb", "db"),
		)
		if got := nodeIDs(a); strings.Join(got, ",") != "a,b,c" {
			t.Fatalf("order = %v, want [a b c]", got)
		}
		if a.byID["b"].Title != "B-fixed" {
			t.Fatalf("correction title = %q", a.byID["b"].Title)
		}
		if len(a.nodes) != 3 || a.nodes[1] != a.byID["b"] {
			t.Fatal("byID and nodes diverged after correction")
		}
	})

	t.Run("transcript_before_and_after_node_latest_wins", func(t *testing.T) {
		a := loadFromLines(t, true,
			storeTxLine("a", "/t/before.jsonl"),
			storeNodeLine(t, "a", "A", "p", "d"),
			storeTxLine("a", "/t/mid.jsonl"),
			storeNodeLine(t, "a", "A2", "p", "d"),
			storeTxLine("a", "/t/after.jsonl"),
		)
		if a.byID["a"].Transcript != "/t/after.jsonl" {
			t.Fatalf("transcript = %q, want latest /t/after.jsonl", a.byID["a"].Transcript)
		}
		if a.byID["a"].Title != "A2" {
			t.Fatalf("title = %q, want A2", a.byID["a"].Title)
		}
	})

	t.Run("delete_removes_node_and_pending_transcript", func(t *testing.T) {
		a := loadFromLines(t, true,
			storeNodeLine(t, "a", "A", "p", "d"),
			storeTxLine("a", "/t/a.jsonl"),
			storeNodeLine(t, "b", "B", "p", "d"),
			storeDelLine("a"),
		)
		if a.byID["a"] != nil {
			t.Fatal("deleted node still in byID")
		}
		if got := nodeIDs(a); strings.Join(got, ",") != "b" {
			t.Fatalf("nodes after delete = %v, want [b]", got)
		}
	})

	t.Run("delete_clears_transcript_association_for_reintro", func(t *testing.T) {
		// Pending transcript for a deleted ID must not stick to a later
		// reintroduction unless a new transcript record appears after delete.
		a := loadFromLines(t, true,
			storeNodeLine(t, "a", "A", "p", "d"),
			storeTxLine("a", "/t/old.jsonl"),
			storeDelLine("a"),
			storeNodeLine(t, "a", "A-new", "p", "d"),
		)
		if a.byID["a"] == nil {
			t.Fatal("reintroduced node missing")
		}
		if a.byID["a"].Transcript != "" {
			t.Fatalf("reintroduced node inherited deleted transcript %q", a.byID["a"].Transcript)
		}
		if a.byID["a"].Title != "A-new" {
			t.Fatalf("title = %q, want A-new", a.byID["a"].Title)
		}
	})

	t.Run("reintroduce_deleted_id_appends_at_end", func(t *testing.T) {
		// Current behavior: removeNodeLocked drops the ID from the slice;
		// a later node record appends it as first-seen-again at the end.
		a := loadFromLines(t, true,
			storeNodeLine(t, "a", "A", "p", "d"),
			storeNodeLine(t, "b", "B", "p", "d"),
			storeNodeLine(t, "c", "C", "p", "d"),
			storeDelLine("a"),
			storeNodeLine(t, "a", "A-again", "p", "d"),
		)
		if got := nodeIDs(a); strings.Join(got, ",") != "b,c,a" {
			t.Fatalf("reintro order = %v, want [b c a]", got)
		}
		if a.byID["a"].Title != "A-again" {
			t.Fatalf("reintro title = %q", a.byID["a"].Title)
		}
	})

	t.Run("transcript_around_delete_and_reintroduction", func(t *testing.T) {
		// transcript after delete but before reintro is pending; applied when
		// the node returns. A still-later transcript wins.
		a := loadFromLines(t, true,
			storeNodeLine(t, "a", "A", "p", "d"),
			storeTxLine("a", "/t/early.jsonl"),
			storeDelLine("a"),
			storeTxLine("a", "/t/pending.jsonl"),
			storeNodeLine(t, "a", "A2", "p", "d"),
			storeTxLine("a", "/t/final.jsonl"),
		)
		if a.byID["a"].Transcript != "/t/final.jsonl" {
			t.Fatalf("transcript = %q, want /t/final.jsonl", a.byID["a"].Transcript)
		}

		a2 := loadFromLines(t, true,
			storeNodeLine(t, "a", "A", "p", "d"),
			storeTxLine("a", "/t/early.jsonl"),
			storeDelLine("a"),
			storeTxLine("a", "/t/pending.jsonl"),
			storeNodeLine(t, "a", "A2", "p", "d"),
		)
		if a2.byID["a"].Transcript != "/t/pending.jsonl" {
			t.Fatalf("pending-after-delete transcript = %q, want /t/pending.jsonl", a2.byID["a"].Transcript)
		}
	})

	t.Run("silently_ignores_blank_malformed_unknown_key_unusable_node", func(t *testing.T) {
		a := loadFromLines(t, true,
			``,
			`   `,
			`{not json`,
			`{"type":"unknown","id":"x"}`,
			`{"type":"key","id":"a","key":"y","excerpt":"dialog","time":"2026-07-01T10:00:00Z"}`,
			`{"type":"node"}`, // unusable: missing node payload
			`{"type":"node","node":null}`,
			storeNodeLine(t, "a", "Keep", "p", "d"),
			storeTxLine("a", "/t/ok.jsonl"),
		)
		if len(a.nodes) != 1 || a.byID["a"] == nil {
			t.Fatalf("want only node a, got ids=%v byID=%v", nodeIDs(a), a.byID)
		}
		if a.byID["a"].Title != "Keep" || a.byID["a"].Transcript != "/t/ok.jsonl" {
			t.Fatalf("valid records disturbed: %+v", a.byID["a"])
		}
	})

	t.Run("valid_final_record_without_trailing_newline", func(t *testing.T) {
		a := loadFromLines(t, false,
			storeNodeLine(t, "a", "A", "p", "d"),
			storeNodeLine(t, "b", "B", "p", "d"),
		)
		if got := nodeIDs(a); strings.Join(got, ",") != "a,b" {
			t.Fatalf("EOF without NL: ids=%v", got)
		}
		if a.byID["b"].Title != "B" {
			t.Fatalf("final record not replayed: %+v", a.byID["b"])
		}
	})

	t.Run("description_falls_back_to_prompt", func(t *testing.T) {
		// loadStore path (constructor coverage lives in TestNewAppReplaysExistingStore).
		a := loadFromLines(t, true,
			`{"type":"node","node":{"id":"n1","title":"T","prompt":"the prompt","agent":"claude","dir":"/tmp","created_at":"2026-07-14T00:00:00Z"}}`,
		)
		if a.byID["n1"].Description != "the prompt" {
			t.Fatalf("description = %q, want prompt fallback", a.byID["n1"].Description)
		}
		// Explicit description is preserved.
		a2 := loadFromLines(t, true,
			`{"type":"node","node":{"id":"n2","title":"T","prompt":"p","description":"explicit","agent":"claude","dir":"/tmp","created_at":"2026-07-14T00:00:00Z"}}`,
		)
		if a2.byID["n2"].Description != "explicit" {
			t.Fatalf("description = %q, want explicit", a2.byID["n2"].Description)
		}
	})
}

func TestLoadStoreMissingAndReadFailure(t *testing.T) {
	// Missing-store success is also covered by TestLoadStoreMissingFile;
	// restate the error-path half so store read contracts live together.
	t.Run("missing_store_is_empty_not_error", func(t *testing.T) {
		a := &app{byID: map[string]*Node{}, storePath: filepath.Join(t.TempDir(), "absent.jsonl")}
		if err := a.loadStore(); err != nil {
			t.Fatalf("missing store: %v", err)
		}
		if len(a.nodes) != 0 {
			t.Fatalf("want 0 nodes, got %d", len(a.nodes))
		}
	})
	t.Run("genuine_read_failure_propagates", func(t *testing.T) {
		// Directory at storePath makes ReadFile fail with a non-IsNotExist error.
		path := filepath.Join(t.TempDir(), "nodes.jsonl")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		a := &app{byID: map[string]*Node{}, storePath: path}
		if err := a.loadStore(); err == nil {
			t.Fatal("want read error when storePath is a directory")
		}
	})
}

func TestRemoveNodeLocked(t *testing.T) {
	na := &Node{ID: "a", Title: "A"}
	nb := &Node{ID: "b", Title: "B"}
	now := time.Now()
	a := &app{
		nodes:       []*Node{na, nb},
		byID:        map[string]*Node{"a": na, "b": nb},
		live:        map[string]string{"a": "quiet", "b": "active"},
		attn:        map[string]string{"a": "approval"},
		attnAt:      map[string]time.Time{"a": now},
		prevCap:     map[string]string{"a": "cap"},
		lastChg:     map[string]time.Time{"a": now},
		activeSince: map[string]time.Time{"a": now},
		staleChat:   map[string]bool{"a": true},
		sendState:   map[string]string{"a": "submitting"},
	}
	a.removeNodeLocked("a")
	if a.byID["a"] != nil {
		t.Fatal("byID still has a")
	}
	if got := nodeIDs(a); strings.Join(got, ",") != "b" {
		t.Fatalf("nodes = %v, want [b]", got)
	}
	if _, ok := a.live["a"]; ok {
		t.Error("live not cleared")
	}
	if _, ok := a.attn["a"]; ok {
		t.Error("attn not cleared")
	}
	if _, ok := a.attnAt["a"]; ok {
		t.Error("attnAt not cleared")
	}
	if _, ok := a.prevCap["a"]; ok {
		t.Error("prevCap not cleared")
	}
	if _, ok := a.lastChg["a"]; ok {
		t.Error("lastChg not cleared")
	}
	if _, ok := a.activeSince["a"]; ok {
		t.Error("activeSince not cleared")
	}
	if _, ok := a.staleChat["a"]; ok {
		t.Error("staleChat not cleared")
	}
	if _, ok := a.sendState["a"]; ok {
		t.Error("sendState not cleared")
	}
	// Other node untouched.
	if a.byID["b"] != nb || a.live["b"] != "active" {
		t.Fatal("removeNodeLocked disturbed unrelated node")
	}
	// Missing id is a no-op.
	a.removeNodeLocked("missing")
	if got := nodeIDs(a); strings.Join(got, ",") != "b" {
		t.Fatalf("after missing delete: %v", got)
	}
}

func TestAppendRecordJSONLAndFailure(t *testing.T) {
	t.Run("appended_records_are_complete_jsonl", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nodes.jsonl")
		a := &app{byID: map[string]*Node{}, storePath: path}
		n := &Node{
			ID: "n1", Title: "T", Prompt: "p", Agent: "claude",
			Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z",
		}
		if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
			t.Fatal(err)
		}
		if err := a.appendRecord(storeRecord{Type: "transcript", ID: "n1", Path: "/t/x.jsonl"}); err != nil {
			t.Fatal(err)
		}
		if err := a.appendRecord(storeRecord{Type: "delete", ID: "n1", Time: "2026-07-14T01:00:00Z"}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(string(raw), "\n") {
			t.Fatal("store file must end with a newline after appends")
		}
		lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		if len(lines) != 3 {
			t.Fatalf("want 3 complete lines, got %d: %q", len(lines), raw)
		}
		var recs []storeRecord
		for i, line := range lines {
			if line == "" {
				t.Fatalf("empty line %d", i)
			}
			var rec storeRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("line %d not JSON: %v (%q)", i, err, line)
			}
			recs = append(recs, rec)
		}
		if recs[0].Type != "node" || recs[0].Node == nil || recs[0].Node.ID != "n1" {
			t.Fatalf("node record: %+v", recs[0])
		}
		if recs[1].Type != "transcript" || recs[1].ID != "n1" || recs[1].Path != "/t/x.jsonl" {
			t.Fatalf("transcript record: %+v", recs[1])
		}
		if recs[2].Type != "delete" || recs[2].ID != "n1" {
			t.Fatalf("delete record: %+v", recs[2])
		}
		// Append-only: a second process-style append extends rather than rewrites.
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.appendRecord(storeRecord{Type: "key", ID: "n1", Key: "y", Excerpt: "ok", Time: "t"}); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(after), string(before)) {
			t.Fatal("append rewrote earlier bytes instead of extending")
		}
	})

	t.Run("open_failure_returns_to_caller", func(t *testing.T) {
		// A directory at storePath cannot be opened for append.
		path := filepath.Join(t.TempDir(), "nodes.jsonl")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		a := &app{byID: map[string]*Node{}, storePath: path}
		err := a.appendRecord(storeRecord{Type: "delete", ID: "x", Time: "t"})
		if err == nil {
			t.Fatal("want append error when storePath is a directory")
		}
	})
}

// Concurrent appends through one app must serialize to complete JSONL lines:
// no partial, interleaved, missing, or duplicate records. Deterministic:
// unique IDs, wait for all workers, parse every line, compare sets/counts.
// Worker goroutines never call testing.T methods.
func TestAppendRecordConcurrentSerialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.jsonl")
	a := &app{byID: map[string]*Node{}, storePath: path}

	const workers = 32
	const perWorker = 8
	type result struct {
		id  string
		err error
	}
	results := make(chan result, workers*perWorker)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id := fmt.Sprintf("w%02d-i%02d", w, i)
				err := a.appendRecord(storeRecord{
					Type: "node",
					Node: &Node{
						ID: id, Title: id, Prompt: "p", Agent: "claude",
						Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z",
					},
				})
				results <- result{id: id, err: err}
			}
		}(w)
	}
	wg.Wait()
	close(results)

	want := map[string]bool{}
	for r := range results {
		if r.err != nil {
			t.Fatalf("append %s: %v", r.id, r.err)
		}
		want[r.id] = true
	}
	if len(want) != workers*perWorker {
		t.Fatalf("want %d unique IDs from workers, got %d", workers*perWorker, len(want))
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatal("store must be non-empty and newline-terminated")
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != workers*perWorker {
		t.Fatalf("line count = %d, want %d (missing/extra/merged records)", len(lines), workers*perWorker)
	}
	got := map[string]int{}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			t.Fatalf("blank line at %d (partial write?)", i)
		}
		var rec storeRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d not complete JSON: %v (%q)", i, err, line)
		}
		if rec.Type != "node" || rec.Node == nil || rec.Node.ID == "" {
			t.Fatalf("line %d unexpected record: %+v", i, rec)
		}
		got[rec.Node.ID]++
	}
	for id := range want {
		if got[id] != 1 {
			t.Errorf("id %s count = %d, want 1", id, got[id])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %d distinct ids on disk, want %d", len(got), len(want))
	}
}

func TestSessionLogPathAndExists(t *testing.T) {
	sessions := t.TempDir()
	a := &app{sessionsDir: sessions}

	if got, want := a.sessionLogPath("alpha"), filepath.Join(sessions, "alpha.jsonl"); got != want {
		t.Fatalf("sessionLogPath = %q, want %q", got, want)
	}
	if got, want := a.sessionLogPath("x/y"), filepath.Join(sessions, "x/y.jsonl"); got != want {
		// Characterize current spelling: id is joined as given (no sanitization here).
		t.Fatalf("sessionLogPath with slash = %q, want %q", got, want)
	}

	if a.sessionLogExists("missing") {
		t.Fatal("absent log reported present")
	}
	if err := os.WriteFile(a.sessionLogPath("live"), []byte("meta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !a.sessionLogExists("live") {
		t.Fatal("existing log not reported present")
	}

	// Empty sessionsDir: current behavior is "not present".
	empty := &app{}
	if empty.sessionLogExists("anything") {
		t.Fatal("empty sessionsDir must not claim logs exist")
	}
}

func TestArchiveSessionLogNamingAndSlugReuse(t *testing.T) {
	// TestArchiveSessionLog covers missing no-op, source removal, and bytes.
	// Here: naming shape and uniqueID dead-slug / post-archive reuse.
	sessions := t.TempDir()
	a := &app{
		byID:        map[string]*Node{},
		sessionsDir: sessions,
	}
	const slug = "demo"
	body := []byte(`{"t":"meta","id":"demo"}` + "\n")
	if err := os.WriteFile(a.sessionLogPath(slug), body, 0o600); err != nil {
		t.Fatal(err)
	}

	// Live leftover blocks the slug (dead-history safety).
	if got := a.uniqueID(slug, nil); got != slug+"-2" {
		t.Fatalf("uniqueID with live leftover log = %q, want %s-2", got, slug)
	}

	before := time.Now().UTC().Add(-time.Second)
	a.archiveSessionLog(slug)
	after := time.Now().UTC().Add(time.Second)

	if _, err := os.Stat(a.sessionLogPath(slug)); !os.IsNotExist(err) {
		t.Fatal("live path still present after archive")
	}
	matches, err := filepath.Glob(filepath.Join(sessions, "archive", slug+".*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("want one archive file, got %v", matches)
	}
	base := filepath.Base(matches[0])
	// Shape: <id>.<UTC stamp>.jsonl with production stamp layout.
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(slug) + `\.\d{8}T\d{6}\.\d{9}Z\.jsonl$`)
	if !re.MatchString(base) {
		t.Fatalf("archive name %q does not match <id>.<UTC timestamp>.jsonl", base)
	}
	// Stamp is parseable as the production layout and near "now".
	stamp := strings.TrimSuffix(strings.TrimPrefix(base, slug+"."), ".jsonl")
	ts, err := time.Parse("20060102T150405.000000000Z", stamp)
	if err != nil {
		t.Fatalf("archive stamp %q: %v", stamp, err)
	}
	if ts.Before(before) || ts.After(after) {
		t.Fatalf("archive stamp %v outside [%v, %v]", ts, before, after)
	}
	if got, err := os.ReadFile(matches[0]); err != nil || string(got) != string(body) {
		t.Fatalf("archived bytes = %q err=%v, want %q", got, err, body)
	}

	// After successful archive the live path is gone, so the bare slug is reusable.
	if got := a.uniqueID(slug, nil); got != slug {
		t.Fatalf("uniqueID after archive = %q, want reusable %q", got, slug)
	}
	// Archived file under archive/ must not count as a live leftover.
	if a.sessionLogExists(slug) {
		t.Fatal("sessionLogExists true after archive")
	}
}
