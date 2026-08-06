package app

// Phase 7 — node fare projection onto the /api/state poll payload
// (fare-design.md Phase 7, D1, D8). Fare is whole-journey ReadFare, cached
// fold-on-growth; ctx_pct occupancy stays segment-scoped and untouched.

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// TestNodeProjection_FareFieldsPopulated: a node whose session log has usage
// projects all fare_* fields matching ReadFare (whole journey, not segment).
func TestNodeProjection_FareFieldsPopulated(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{
		ID: "fare-full", Title: "f", Agent: "claude", Model: "claude-sonnet",
		Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z",
	}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.live[n.ID] = "quiet"

	// Two turns: different models so fare_model dominance is pinned (m-main
	// has more turns). Cost present on both → fare_cost_complete true.
	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "claude", "claude-sonnet", "", a.home),
		{T: "usage", Time: "2026-07-15T09:00:00Z", Usage: &sessionlog.UsageEvent{
			Used: 1000, Size: 200_000,
			InputTokens: 10, OutputTokens: 20, CachedReadTokens: 5,
			CacheCreationTokens: 3, CostAmount: 0.01, Model: "m-main", TurnID: "t1",
		}},
		{T: "usage", Time: "2026-07-15T09:01:00Z", Usage: &sessionlog.UsageEvent{
			Used: 2000, Size: 200_000,
			InputTokens: 100, OutputTokens: 40, CachedReadTokens: 15,
			CacheCreationTokens: 7, CostAmount: 0.02, Model: "m-main", TurnID: "t2",
		}},
		{T: "usage", Time: "2026-07-15T09:02:00Z", Usage: &sessionlog.UsageEvent{
			Used: 2500, Size: 200_000,
			InputTokens: 1, OutputTokens: 1, CachedReadTokens: 0,
			CacheCreationTokens: 0, CostAmount: 0.001, Model: "m-other", TurnID: "t3",
		}},
	})

	want := sessionlog.ReadFare(a.sessionLogPath(n.ID))
	if want.Turns == 0 {
		t.Fatal("fixture precondition: ReadFare.Turns == 0")
	}

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d body %q", rec.Code, rec.Body.String())
	}
	node := stateNodeByID(t, rec.Body.Bytes(), n.ID)

	assertJSONInt(t, node, "fare_fresh_in", want.FreshIn)
	assertJSONInt(t, node, "fare_cache_read", want.CacheRead)
	assertJSONInt(t, node, "fare_cache_write", want.CacheWrite)
	assertJSONInt(t, node, "fare_out", want.Out)
	assertJSONInt(t, node, "fare_total", want.Total())
	assertJSONInt(t, node, "fare_turns", want.Turns)

	if _, ok := node["fare_cost"]; !ok {
		t.Fatalf("missing fare_cost; keys %v", keysOf(node))
	}
	if got := node["fare_cost"].(float64); got != want.ReportedCostUSD {
		t.Errorf("fare_cost = %v, want %v", got, want.ReportedCostUSD)
	}
	if _, ok := node["fare_cost_complete"]; !ok {
		t.Fatalf("missing fare_cost_complete; keys %v", keysOf(node))
	}
	if got := node["fare_cost_complete"].(bool); got != want.ReportedCostComplete {
		t.Errorf("fare_cost_complete = %v, want %v", got, want.ReportedCostComplete)
	}
	// Dominant model = most turns → m-main (2) over m-other (1).
	if got, _ := node["fare_model"].(string); got != "m-main" {
		t.Errorf("fare_model = %q, want %q (dominant by turn count)", got, "m-main")
	}
}

// TestNodeProjection_FareCachedUntilLogGrows: fold runs on first projection
// and after size/mtime advance, but not when the log is unchanged. Proven by
// mutating the file behind the cache (same size+mtime → stale value) then
// growing it (recompute). A fold-count spy on FareCache double-checks.
func TestNodeProjection_FareCachedUntilLogGrows(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{
		ID: "fare-cache", Title: "fc", Agent: "codex", Model: "gpt",
		Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z",
	}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.live[n.ID] = "active"

	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "codex", "gpt", "", a.home),
		{T: "usage", Time: "2026-07-15T09:00:00Z", Usage: &sessionlog.UsageEvent{
			Used: 500, Size: 100_000,
			InputTokens: 50, OutputTokens: 10, CachedReadTokens: 0, TurnID: "c1",
		}},
	})

	poll := func() map[string]any {
		rec := httptest.NewRecorder()
		a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
		if rec.Code != 200 {
			t.Fatalf("code = %d body %q", rec.Code, rec.Body.String())
		}
		return stateNodeByID(t, rec.Body.Bytes(), n.ID)
	}

	// First projection folds.
	node := poll()
	assertJSONInt(t, node, "fare_fresh_in", 50)
	assertJSONInt(t, node, "fare_out", 10)
	folds1 := fareFolds(t, a, n.ID)
	if folds1 < 1 {
		t.Fatalf("after first poll: folds = %d, want ≥1", folds1)
	}

	// Unchanged log: no re-fold.
	node = poll()
	assertJSONInt(t, node, "fare_fresh_in", 50)
	folds2 := fareFolds(t, a, n.ID)
	if folds2 != folds1 {
		t.Fatalf("unchanged poll re-folded: folds %d → %d", folds1, folds2)
	}

	// Mutate file behind the cache without growing size/mtime: still stale.
	path := a.sessionLogPath(n.ID)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	origSize, origMT := st.Size(), st.ModTime()
	// Rewrite with different totals but pad to the same byte length, then
	// restore mtime so the size/mtime key is unchanged.
	// Single-digit tokens so the rewritten body is shorter than the original
	// (50/10) and can pad up to the same byte length.
	alt := buildPaddedFareLog(t, n.ID, a.home, 9, 8, int(origSize))
	if err := os.WriteFile(path, alt, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, origMT, origMT); err != nil {
		t.Fatal(err)
	}
	st2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Size() != origSize || !st2.ModTime().Equal(origMT) {
		t.Fatalf("setup: size/mtime changed (size %d→%d, mt equal=%v)",
			origSize, st2.Size(), st2.ModTime().Equal(origMT))
	}

	node = poll()
	// Stale cached value — not the rewritten 999/888.
	assertJSONInt(t, node, "fare_fresh_in", 50)
	assertJSONInt(t, node, "fare_out", 10)
	folds3 := fareFolds(t, a, n.ID)
	if folds3 != folds1 {
		t.Fatalf("same size/mtime re-folded: folds %d → %d", folds1, folds3)
	}

	// Growth: append a usage record → re-fold with true on-disk content.
	// (File currently holds the rewritten body; append advances size.)
	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		{T: "usage", Time: "2026-07-15T10:00:00Z", Usage: &sessionlog.UsageEvent{
			Used: 600, Size: 100_000,
			InputTokens: 1, OutputTokens: 2, TurnID: "c2",
		}},
	})
	// Real on-disk fare after growth (rewritten body + appended turn).
	want := sessionlog.ReadFare(path)
	node = poll()
	assertJSONInt(t, node, "fare_fresh_in", want.FreshIn)
	assertJSONInt(t, node, "fare_out", want.Out)
	folds4 := fareFolds(t, a, n.ID)
	if folds4 <= folds1 {
		t.Fatalf("after growth: folds = %d, want > %d", folds4, folds1)
	}
}

// TestNodeProjection_CtxPctUnchanged: occupancy projection is independent of
// fare (D1). ctx_pct from LatestUsage / segment Used+Size is unchanged.
func TestNodeProjection_CtxPctUnchanged(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{
		ID: "fare-ctx", Title: "fc", Agent: "claude", Model: "claude-sonnet",
		Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z",
	}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.live[n.ID] = "quiet"

	// Occupancy used=50_000 of 200_000 → 25%. Fare totals are deliberately
	// different numbers so a merge bug would corrupt ctx_pct.
	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "claude", "claude-sonnet", "", a.home),
		{T: "usage", Time: "2026-07-15T09:00:00Z", Usage: &sessionlog.UsageEvent{
			Used: 50_000, Size: 200_000,
			InputTokens: 9000, OutputTokens: 1000, CachedReadTokens: 500,
			CacheCreationTokens: 100, CostAmount: 0.05, Model: "m", TurnID: "u1",
		}},
	})

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d body %q", rec.Code, rec.Body.String())
	}
	node := stateNodeByID(t, rec.Body.Bytes(), n.ID)

	if _, ok := node["ctx_pct"]; !ok {
		t.Fatalf("missing ctx_pct; keys %v", keysOf(node))
	}
	if got := int(node["ctx_pct"].(float64)); got != 25 {
		t.Errorf("ctx_pct = %d, want 25 (occupancy 50k/200k; fare must not perturb tank)", got)
	}
	// Fare is present and distinct from occupancy.
	assertJSONInt(t, node, "fare_fresh_in", 9000)
	assertJSONInt(t, node, "fare_out", 1000)
	if node["fare_fresh_in"] == node["ctx_pct"] {
		t.Error("fare_fresh_in equals ctx_pct — gauges appear merged (D1)")
	}
}

// TestNodeProjection_FareUnavailableForEmptyPi: no counted fare turns → omit
// fare_* fields entirely (absent ≠ zero).
func TestNodeProjection_FareUnavailableForEmptyPi(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{
		ID: "empty-pi", Title: "pi", Agent: "pi", Model: "mistral",
		Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z", Transport: "acp",
	}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.live[n.ID] = "quiet"

	// Meta + chat only — no billable usage (pi ACP often writes none).
	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "pi", "mistral", "", a.home),
		{T: "user", Text: "hello", Time: "2026-07-15T09:00:00Z"},
		{T: "assistant", Text: "hi", Time: "2026-07-15T09:00:01Z"},
		// Occupancy shell only (used/size) — not billable fare data.
		{T: "usage", Time: "2026-07-15T09:00:02Z", Usage: &sessionlog.UsageEvent{
			Used: 1000, Size: 128_000,
		}},
	})

	if got := sessionlog.ReadFare(a.sessionLogPath(n.ID)); got.Turns != 0 {
		t.Fatalf("precondition: ReadFare.Turns = %d, want 0", got.Turns)
	}

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d body %q", rec.Code, rec.Body.String())
	}
	node := stateNodeByID(t, rec.Body.Bytes(), n.ID)

	fareKeys := []string{
		"fare_fresh_in", "fare_cache_read", "fare_cache_write", "fare_out",
		"fare_total", "fare_turns", "fare_cost", "fare_cost_complete", "fare_model",
	}
	for _, k := range fareKeys {
		if v, ok := node[k]; ok {
			t.Errorf("%s = %v, want omitted (absent ≠ zero)", k, v)
		}
	}
}

// ---------- helpers ----------

func stateNodeByID(t *testing.T, body []byte, id string) map[string]any {
	t.Helper()
	for _, n := range decodeStateNodes(t, body) {
		if n["id"] == id {
			return n
		}
	}
	t.Fatalf("node %q not in state payload", id)
	return nil
}

func assertJSONInt(t *testing.T, m map[string]any, key string, want int) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("missing %s; keys %v", key, keysOf(m))
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s type %T = %v, want number", key, v, v)
	}
	if int(f) != want {
		t.Errorf("%s = %v, want %d", key, v, want)
	}
}

// fareFolds returns how many times the node's FareCache has re-folded.
func fareFolds(t *testing.T, a *app, id string) int {
	t.Helper()
	return fareCacheFolds(a, id)
}

// buildPaddedFareLog builds a session log with known fare totals, padded with
// a trailing whitespace-only comment line so the file is exactly wantSize
// bytes (for same-size cache-key tests).
func buildPaddedFareLog(t *testing.T, id, home string, freshIn, out, wantSize int) []byte {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pad.jsonl")
	w := &sessionlog.Writer{Path: path}
	for _, ev := range []sessionlog.Event{
		sessionlog.NewMeta(id, "codex", "gpt", "", home),
		{T: "usage", Time: "2026-07-15T09:00:00Z", Usage: &sessionlog.UsageEvent{
			Used: 1, Size: 100_000,
			InputTokens: freshIn, OutputTokens: out, TurnID: "alt",
		}},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > wantSize {
		t.Fatalf("fixture body %d bytes > wantSize %d; adjust pad strategy", len(b), wantSize)
	}
	// Pad with spaces+newline so the file remains valid JSONL (extra blank
	// lines are skipped by ReadEvents).
	for len(b) < wantSize {
		b = append(b, ' ')
	}
	// Ensure final byte is newline if we overwrote one — keep trailing \n.
	if wantSize > 0 {
		b[wantSize-1] = '\n'
	}
	return b[:wantSize]
}
