package transcript

// Phase 4 tests (fare-design.md): pi native session-map join + message usage
// parsing. Synthetic fixtures only — never a real pi transcript.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSessionMap(t *testing.T, dir string, sessions map[string]map[string]string) string {
	t.Helper()
	path := filepath.Join(dir, "session-map.json")
	body := map[string]any{"version": 1, "sessions": sessions}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPiSessionMap_ResolvesFileForSession(t *testing.T) {
	dir := t.TempDir()
	native := filepath.Join(dir, "native.jsonl")
	if err := os.WriteFile(native, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mapPath := writeSessionMap(t, dir, map[string]map[string]string{
		"sess-abc": {
			"sessionId":   "sess-abc",
			"cwd":         "/tmp/work",
			"sessionFile": native,
			"updatedAt":   "2026-08-04T12:00:00.000Z",
		},
	})
	got, ok := ResolvePiSessionFile(mapPath, "sess-abc")
	if !ok {
		t.Fatal("expected resolve ok")
	}
	if got != native {
		t.Fatalf("resolved %q, want %q", got, native)
	}
}

func TestPiSessionMap_StaleEntryTolerated(t *testing.T) {
	dir := t.TempDir()
	// Missing map file.
	if _, ok := ResolvePiSessionFile(filepath.Join(dir, "no-such-map.json"), "x"); ok {
		t.Fatal("missing map must not resolve")
	}
	// Map present but session id absent.
	mapPath := writeSessionMap(t, dir, map[string]map[string]string{
		"other": {"sessionId": "other", "sessionFile": "/tmp/gone.jsonl"},
	})
	if _, ok := ResolvePiSessionFile(mapPath, "missing-id"); ok {
		t.Fatal("absent session id must not resolve")
	}
	// Entry points at a missing file — still returns the path (caller may
	// degrade on open); never panics. Empty sessionFile is not a resolve.
	mapPath2 := writeSessionMap(t, dir, map[string]map[string]string{
		"stale": {"sessionId": "stale", "sessionFile": filepath.Join(dir, "deleted.jsonl")},
		"empty": {"sessionId": "empty", "sessionFile": ""},
	})
	if path, ok := ResolvePiSessionFile(mapPath2, "stale"); !ok || path == "" {
		t.Fatalf("stale path entry: ok=%v path=%q (want ok with non-empty path)", ok, path)
	}
	if _, ok := ResolvePiSessionFile(mapPath2, "empty"); ok {
		t.Fatal("empty sessionFile must not resolve")
	}
	// Corrupt JSON: degrade, no panic.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ResolvePiSessionFile(bad, "x"); ok {
		t.Fatal("corrupt map must not resolve")
	}
	// Empty session id: never resolves.
	if _, ok := ResolvePiSessionFile(mapPath, ""); ok {
		t.Fatal("empty session id must not resolve")
	}
}

func TestPiNative_ParsesUsageOnMessageRecords(t *testing.T) {
	// §2.2 live shape: usage on type:"message" assistant records.
	// Mapping (pinned): input→InputTokens (fresh, direct), output→OutputTokens,
	// cacheRead→CachedReadTokens, cacheWrite→CacheCreationTokens,
	// reasoning→ReasoningTokens, cost.total→CostAmount (+USD), Model.
	line := []byte(`{"type":"message","id":"msg-asst-1","timestamp":"2026-08-04T12:00:05.000Z","message":{"role":"assistant","model":"grok-4.5","usage":{"input":3808,"output":202,"cacheRead":128,"cacheWrite":512,"reasoning":77,"totalTokens":4138,"cost":{"input":0.003,"output":0.001,"cacheRead":0.0001,"cacheWrite":0.0005,"total":0.0046}}}}`)
	u, ok := ParsePiUsageLine(line)
	if !ok {
		t.Fatal("expected usage parse ok")
	}
	if u.InputTokens != 3808 {
		t.Errorf("InputTokens = %d, want 3808 (fresh, direct)", u.InputTokens)
	}
	if u.OutputTokens != 202 {
		t.Errorf("OutputTokens = %d, want 202", u.OutputTokens)
	}
	if u.CachedReadTokens != 128 {
		t.Errorf("CachedReadTokens = %d, want 128", u.CachedReadTokens)
	}
	if u.CacheCreationTokens != 512 {
		t.Errorf("CacheCreationTokens = %d, want 512 (cacheWrite)", u.CacheCreationTokens)
	}
	if u.ReasoningTokens != 77 {
		t.Errorf("ReasoningTokens = %d, want 77", u.ReasoningTokens)
	}
	if u.TotalTokens != 4138 {
		t.Errorf("TotalTokens = %d, want 4138", u.TotalTokens)
	}
	if u.CostAmount != 0.0046 {
		t.Errorf("CostAmount = %v, want 0.0046", u.CostAmount)
	}
	if u.CostCurrency != "USD" {
		t.Errorf("CostCurrency = %q, want USD", u.CostCurrency)
	}
	if u.Model != "grok-4.5" {
		t.Errorf("Model = %q, want grok-4.5", u.Model)
	}
}

func TestPiNative_TurnIDFromRecord(t *testing.T) {
	line := []byte(`{"type":"message","id":"msg-asst-1","message":{"role":"assistant","usage":{"input":1,"output":1},"model":"m"}}`)
	u, ok := ParsePiUsageLine(line)
	if !ok {
		t.Fatal("expected ok")
	}
	if u.TurnID != "msg-asst-1" {
		t.Errorf("TurnID = %q, want msg-asst-1 (top-level record id)", u.TurnID)
	}
}

func TestPiNative_IgnoresNonMessageRecords(t *testing.T) {
	// session / model_change / thinking_level_change / user messages without
	// usage / garbage: all must be silent no-ops (defensive parsing).
	for _, line := range []string{
		`{"type":"session","id":"s"}`,
		`{"type":"model_change","model":"x"}`,
		`{"type":"thinking_level_change","thinkingLevel":"low"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":"hi"}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":"no usage"}}`,
		`not json`,
		`{}`,
		``,
	} {
		if _, ok := ParsePiUsageLine([]byte(line)); ok {
			t.Errorf("ParsePiUsageLine(%q) ok=true, want false", line)
		}
	}
}

func TestPiNative_FixtureFileParsesBothTurns(t *testing.T) {
	// Full synthetic fixture: two assistant usage turns, ignores non-message.
	tl := &PiTailer{Path: "testdata/pi-native.jsonl"}
	got := tl.Poll()
	if len(got) != 2 {
		t.Fatalf("got %d usage events, want 2", len(got))
	}
	if got[0].TurnID != "msg-asst-1" || got[0].CacheCreationTokens != 512 {
		t.Errorf("turn0 = %+v", got[0])
	}
	if got[1].TurnID != "msg-asst-2" || got[1].InputTokens != 100 {
		t.Errorf("turn1 = %+v", got[1])
	}
	// Second poll of an unchanged file yields nothing new.
	if again := tl.Poll(); len(again) != 0 {
		t.Fatalf("idle poll returned %d events", len(again))
	}
}

// --- Phase 4a: pi models.json → contextWindow by model id ---

func TestPiModels_ParsesContextWindowByID(t *testing.T) {
	// Multi-provider fixture: join on id (not name). Local-endpoint models
	// included. Duplicate id → first-wins, no error. Missing/corrupt → empty map.
	path := "testdata/pi-models.json"
	got := LoadPiModelWindows(path)
	if got == nil {
		t.Fatal("LoadPiModelWindows returned nil; want empty-or-populated map")
	}
	// id→window (not name): smollm2:1.7b is the id; name is smollm2.
	if got["smollm2:1.7b"] != 8192 {
		t.Errorf("smollm2:1.7b = %d, want 8192 (local ollama model by id)", got["smollm2:1.7b"])
	}
	if _, ok := got["smollm2"]; ok {
		t.Error("must key by id, not name: found name key smollm2")
	}
	if got["gpt-oss-safeguard-120b-mlx"] != 32768 {
		t.Errorf("gpt-oss-safeguard-120b-mlx = %d, want 32768", got["gpt-oss-safeguard-120b-mlx"])
	}
	if got["mistral-small-latest"] != 32000 {
		t.Errorf("mistral-small-latest = %d, want 32000", got["mistral-small-latest"])
	}
	// Same id under two providers: first-wins (1000, not 9999).
	if got["dup-id"] != 1000 {
		t.Errorf("dup-id = %d, want 1000 (first-wins across providers)", got["dup-id"])
	}
	// contextWindow == 0 entries may be present or omitted; Lookup treats 0 as unknown.
	// Missing file → empty map, no crash.
	empty := LoadPiModelWindows(filepath.Join(t.TempDir(), "no-such-models.json"))
	if empty == nil {
		t.Fatal("missing file must return empty map, not nil")
	}
	if len(empty) != 0 {
		t.Errorf("missing file map len = %d, want 0", len(empty))
	}
	// Corrupt JSON → empty, no error.
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("not-json{"), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := LoadPiModelWindows(bad)
	if corrupt == nil || len(corrupt) != 0 {
		t.Errorf("corrupt JSON: got %#v, want empty map", corrupt)
	}
}

func TestPiModels_ReloadsOnChange(t *testing.T) {
	// Editing models.json (new mtime) must pick up a changed window.
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	write := func(window int) {
		t.Helper()
		body := map[string]any{
			"providers": map[string]any{
				"p": map[string]any{
					"models": []any{
						map[string]any{"id": "m1", "name": "m1", "contextWindow": window},
					},
				},
			},
		}
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(1000)
	cache := &PiModels{Path: path}
	if w := cache.Window("m1"); w != 1000 {
		t.Fatalf("initial Window(m1) = %d, want 1000", w)
	}
	// Ensure mtime advances on some filesystems (1s resolution).
	// Touch content change is enough on most systems; sleep if needed.
	write(2000)
	info1, _ := os.Stat(path)
	// Force a distinct mtime if the FS is coarse.
	future := info1.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if w := cache.Window("m1"); w != 2000 {
		t.Fatalf("after edit Window(m1) = %d, want 2000 (mtime reload)", w)
	}
	// Unknown model → 0.
	if w := cache.Window("nope"); w != 0 {
		t.Errorf("unknown model Window = %d, want 0", w)
	}
}
