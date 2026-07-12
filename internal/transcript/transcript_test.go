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

	got, ok := FindCodexRollout(root, "/data/exp1", time.Now().Add(-time.Minute), nil)
	if !ok || got != match {
		t.Fatalf("got %q ok=%v, want %q", got, ok, match)
	}
	if _, ok := FindCodexRollout(root, "/data/nomatch", time.Now().Add(-time.Minute), nil); ok {
		t.Fatal("matched a rollout for the wrong cwd")
	}
}

// Two same-cwd nodes starting close together must resolve to two different
// rollouts: claimed paths are excluded, and the oldest-by-filename candidate
// wins, so discovery in node start order pairs each node with its own file
// (finding 3 of the 2026-07 review).
func TestFindCodexRolloutConcurrentSameDir(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "07", "12")
	os.MkdirAll(day, 0o755)
	meta := `{"timestamp":"x","type":"session_meta","payload":{"id":"i","cwd":"/data/exp1"}}` + "\n"
	first := filepath.Join(day, "rollout-2026-07-12T10-00-00-aaa.jsonl")
	second := filepath.Join(day, "rollout-2026-07-12T10-00-05-bbb.jsonl")
	os.WriteFile(first, []byte(meta), 0o644)
	os.WriteFile(second, []byte(meta), 0o644)
	since := time.Now().Add(-time.Minute)

	// Node A (started first, discovered first) takes the older rollout…
	gotA, ok := FindCodexRollout(root, "/data/exp1", since, nil)
	if !ok || gotA != first {
		t.Fatalf("node A got %q ok=%v, want %q", gotA, ok, first)
	}
	// …and node B, with A's path claimed, takes the remaining one.
	gotB, ok := FindCodexRollout(root, "/data/exp1", since, map[string]bool{gotA: true})
	if !ok || gotB != second {
		t.Fatalf("node B got %q ok=%v, want %q", gotB, ok, second)
	}
	// With both claimed, nothing is left to link.
	if _, ok := FindCodexRollout(root, "/data/exp1", since,
		map[string]bool{first: true, second: true}); ok {
		t.Fatal("returned a claimed rollout")
	}
}

// WaitingOn is the transcript half of needs-input detection: a logged tool
// call whose result has not been logged yet. Verified against real session
// logs: both CLIs write the call record when the agent asks, and the result
// record only after the human answers (gaps of minutes to hours observed).

func TestWaitingOnClaude(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "w.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tl := &Tailer{Path: path}

	// Agent issues a Bash call: pending until the result lands.
	f.WriteString(`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"ls"}}]}}` + "\n")
	tl.Poll()
	if name, ok := tl.WaitingOn(); !ok || name != "Bash" {
		t.Fatalf("WaitingOn = %q,%v, want Bash,true", name, ok)
	}
	f.WriteString(`{"type":"user","timestamp":"t2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}}` + "\n")
	tl.Poll()
	if name, ok := tl.WaitingOn(); ok {
		t.Fatalf("resolved call still pending: %q", name)
	}

	// An explicit question tool stays pending, keeping its name so the
	// caller can tell "question" from "approval".
	f.WriteString(`{"type":"assistant","timestamp":"t3","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu2","name":"AskUserQuestion","input":{}}]}}` + "\n")
	tl.Poll()
	if name, ok := tl.WaitingOn(); !ok || name != "AskUserQuestion" {
		t.Fatalf("WaitingOn = %q,%v, want AskUserQuestion,true", name, ok)
	}
	// A plain user turn (answer typed in the TUI, or an interrupt notice)
	// means the human already acted: nothing stays pending.
	f.WriteString(`{"type":"user","timestamp":"t4","message":{"role":"user","content":"[Request interrupted by user]"}}` + "\n")
	tl.Poll()
	if name, ok := tl.WaitingOn(); ok {
		t.Fatalf("user turn did not clear pending: %q", name)
	}
}

func TestWaitingOnCodex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tl := &Tailer{Path: path}

	f.WriteString(`{"timestamp":"t1","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"c1","arguments":"{}"}}` + "\n")
	tl.Poll()
	if name, ok := tl.WaitingOn(); !ok || name != "exec_command" {
		t.Fatalf("WaitingOn = %q,%v, want exec_command,true", name, ok)
	}
	f.WriteString(`{"timestamp":"t2","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"done"}}` + "\n")
	tl.Poll()
	if name, ok := tl.WaitingOn(); ok {
		t.Fatalf("resolved call still pending: %q", name)
	}

	// task_complete / turn_aborted are explicit turn boundaries: whatever is
	// still unresolved (e.g. after an interrupt) is moot.
	f.WriteString(`{"timestamp":"t3","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"c2","arguments":"{}"}}` + "\n")
	f.WriteString(`{"timestamp":"t4","type":"event_msg","payload":{"type":"turn_aborted","reason":"interrupted"}}` + "\n")
	tl.Poll()
	if name, ok := tl.WaitingOn(); ok {
		t.Fatalf("turn boundary did not clear pending: %q", name)
	}
}
