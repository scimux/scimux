package transcript

// The fixtures in testdata/ were hand-written from known samples of both
// CLIs' log formats. Regenerate real ones with scripts/capture-fixtures.sh
// whenever a CLI update changes its format.

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func parseFile(t *testing.T, path string) []Turn {
	t.Helper()
	tl := &Tailer{Path: path}
	return tl.Poll()
}

func TestParseClaudeFixture(t *testing.T) {
	turns := parseFile(t, "testdata/claude-session.jsonl")
	want := []Turn{
		{Role: "user", Text: "Hello agent, analyze run 42", Time: "2026-07-11T09:00:00.000Z"},
		{Role: "assistant", Text: "Looking at run 42 now.", Time: "2026-07-11T09:00:05.000Z"},
		{Role: "assistant", Text: "Run 42 shows 12% false positives on low-speed maneuvers.", Time: "2026-07-11T09:00:15.000Z"},
	}
	if !reflect.DeepEqual(turns, want) {
		t.Fatalf("turns = %#v\nwant %#v", turns, want)
	}
}

func TestParseCodexFixture(t *testing.T) {
	turns := parseFile(t, "testdata/codex-rollout.jsonl")
	want := []Turn{
		{Role: "user", Text: "Hello codex, sweep the detector thresholds", Time: "2026-07-11T10:00:02.000Z"},
		{Role: "assistant", Text: "Sweep started; 5 threshold values queued.", Time: "2026-07-11T10:00:06.000Z"},
	}
	if !reflect.DeepEqual(turns, want) {
		t.Fatalf("turns = %#v\nwant %#v", turns, want)
	}
}

// The real-* fixtures were captured from actual CLI runs (claude 2026-07,
// codex 0.139.0) via scripts/capture-fixtures.sh. They pin the parser to
// formats observed in the wild, including record types the hand-written
// fixtures don't model (queue-operation, ai-title, developer-role messages,
// thinking blocks, token_count events).

func TestParseRealClaudeFixture(t *testing.T) {
	if _, err := os.Stat("testdata/real-claude-session.jsonl"); err != nil {
		t.Skip("real fixture not captured yet (run scripts/capture-fixtures.sh)")
	}
	turns := parseFile(t, "testdata/real-claude-session.jsonl")
	if len(turns) != 2 {
		t.Fatalf("want 2 turns, got %d: %#v", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[0].Text != "Reply with the single word: pong" {
		t.Errorf("user turn = %#v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Text != "pong" {
		t.Errorf("assistant turn = %#v", turns[1])
	}
}

func TestParseRealCodexFixture(t *testing.T) {
	if _, err := os.Stat("testdata/real-codex-rollout.jsonl"); err != nil {
		t.Skip("real fixture not captured yet (run scripts/capture-fixtures.sh)")
	}
	turns := parseFile(t, "testdata/real-codex-rollout.jsonl")
	if len(turns) != 2 {
		t.Fatalf("want 2 turns (env context and developer msg filtered), got %d: %#v", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[0].Text != "Reply with the single word: pong" {
		t.Errorf("user turn = %#v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Text != "pong" {
		t.Errorf("assistant turn = %#v", turns[1])
	}
	// Discovery relies on session_meta carrying the cwd.
	if cwd := rolloutCwd("testdata/real-codex-rollout.jsonl"); cwd == "" {
		t.Error("rolloutCwd found no cwd in real session_meta")
	}
}

func TestGarbageInEmptyOutNoPanic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "garbage.jsonl")
	os.WriteFile(path, []byte("{{{{\n\x00\xff binary\n{\"type\":\"unknown_future_type\",\"payload\":{}}\n"), 0o644)
	if turns := parseFile(t, path); len(turns) != 0 {
		t.Fatalf("garbage produced turns: %#v", turns)
	}
	// Missing file: no turns, no error, no panic.
	missing := &Tailer{Path: filepath.Join(dir, "does-not-exist.jsonl")}
	if turns := missing.Poll(); len(turns) != 0 {
		t.Fatalf("missing file produced turns: %#v", turns)
	}
}

func TestTailerIncrementalAndPartialLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tl := &Tailer{Path: path}

	f.WriteString(`{"type":"user","timestamp":"t1","message":{"role":"user","content":"first"}}` + "\n")
	if turns := tl.Poll(); len(turns) != 1 || turns[0].Text != "first" {
		t.Fatalf("after first line: %#v", turns)
	}
	// Append half a line: must not be parsed yet…
	half := `{"type":"assistant","timestamp":"t2","message":{"role":"assist`
	f.WriteString(half)
	if turns := tl.Poll(); len(turns) != 1 {
		t.Fatalf("partial line parsed prematurely: %#v", turns)
	}
	// …until completed.
	f.WriteString(`ant","content":[{"type":"text","text":"second"}]}}` + "\n")
	turns := tl.Poll()
	if len(turns) != 2 || turns[1].Text != "second" {
		t.Fatalf("after completing line: %#v", turns)
	}
	// Idempotent when nothing new arrived.
	if turns := tl.Poll(); len(turns) != 2 {
		t.Fatalf("re-poll changed result: %#v", turns)
	}
}

func TestFindClaudeTranscript(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-data-exp1")
	os.MkdirAll(proj, 0o755)
	id := "11111111-2222-3333-4444-555555555555"
	os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "other.jsonl"), []byte("{}\n"), 0o644)

	got, ok := FindClaudeTranscript(home, id)
	if !ok || got != filepath.Join(proj, id+".jsonl") {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	if _, ok := FindClaudeTranscript(home, "not-a-real-id"); ok {
		t.Fatal("found transcript for unknown session id")
	}
}

func TestFindClaudeNewestInDir(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-data-my-exp-1")
	os.MkdirAll(proj, 0o755)
	older := filepath.Join(proj, "aaaaaaaa-0000-0000-0000-000000000001.jsonl")
	newer := filepath.Join(proj, "bbbbbbbb-0000-0000-0000-000000000002.jsonl")
	os.WriteFile(older, []byte("{}\n"), 0o644)
	os.WriteFile(newer, []byte("{}\n"), 0o644)
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(older, past, past)

	path, sid, ok := FindClaudeNewestInDir(home, "/data/my/exp_1")
	if !ok || path != newer || sid != "bbbbbbbb-0000-0000-0000-000000000002" {
		t.Fatalf("got %q sid=%q ok=%v", path, sid, ok)
	}
	if _, _, ok := FindClaudeNewestInDir(home, "/data/unknown"); ok {
		t.Fatal("unknown dir must not match")
	}
}

func TestFindCodexRollout(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "07", "11")
	os.MkdirAll(day, 0o755)
	meta := func(cwd string) string {
		return `{"timestamp":"x","type":"session_meta","payload":{"id":"i","cwd":"` + cwd + `"}}` + "\n"
	}
	old := filepath.Join(day, "rollout-old.jsonl")
	match := filepath.Join(day, "rollout-match.jsonl")
	otherCwd := filepath.Join(day, "rollout-other.jsonl")
	os.WriteFile(old, []byte(meta("/data/exp1")), 0o644)
	os.WriteFile(match, []byte(meta("/data/exp1")), 0o644)
	os.WriteFile(otherCwd, []byte(meta("/data/exp2")), 0o644)
	// Make "old" predate the search window.
	past := time.Now().Add(-1 * time.Hour)
	os.Chtimes(old, past, past)

	got, ok := FindCodexRollout(root, "/data/exp1", time.Now().Add(-time.Minute))
	if !ok || got != match {
		t.Fatalf("got %q ok=%v, want %q", got, ok, match)
	}
	if _, ok := FindCodexRollout(root, "/data/nomatch", time.Now().Add(-time.Minute)); ok {
		t.Fatal("matched a rollout for the wrong cwd")
	}
}
