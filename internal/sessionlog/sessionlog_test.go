package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetaHeaderRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	if err := w.Append(NewMeta("review-parser", "claude", "opus", "/tmp/wd")); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Event{T: "user", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	evs := ReadEvents(path)
	if len(evs) != 2 || evs[0].T != "meta" || evs[0].Meta == nil {
		t.Fatalf("want meta header first, got %+v", evs)
	}
	m := evs[0].Meta
	if m.Node != "review-parser" || m.Agent != "claude" || m.Model != "opus" ||
		m.Dir != "/tmp/wd" || m.UID == "" || m.Created == "" {
		t.Fatalf("meta fields incomplete: %+v", m)
	}
	if NewMeta("a", "b", "", "").Meta.UID == m.UID {
		t.Fatal("meta UIDs must differ between calls")
	}
}

func TestReadTurnsSkipsNonChatRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	for _, ev := range []Event{
		NewMeta("n", "claude", "", ""),
		{T: "user", Text: "question"},
		{T: "tool", Tool: &ToolEvent{ID: "t1", Title: "ls", Status: "completed"}},
		{T: "usage", Usage: &UsageEvent{Used: 10, Size: 100}},
		{T: "assistant", Text: "answer"},
		{T: "stop", StopReason: "end_turn"},
		{T: "user", Text: "  "}, // blank text never becomes a turn
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	turns := ReadTurns(path)
	if len(turns) != 2 || turns[0].Role != "user" || turns[1].Role != "assistant" {
		t.Fatalf("want 2 chat turns, got %+v", turns)
	}
	if used, size := LatestUsage(path); used != 10 || size != 100 {
		t.Fatalf("usage = %d/%d, want 10/100", used, size)
	}
}

func TestReadersAreDefensive(t *testing.T) {
	if got := ReadEvents(filepath.Join(t.TempDir(), "missing.jsonl")); got != nil {
		t.Fatalf("missing file: want nil, got %v", got)
	}
	path := filepath.Join(t.TempDir(), "n.jsonl")
	content := `{"t":"user","text":"ok"}` + "\n" +
		"not json at all\n" +
		`{"t":"future-type","x":1}` + "\n" +
		`{"t":"assistant","text":"torn` // no trailing newline, truncated JSON
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	turns := ReadTurns(path)
	if len(turns) != 1 || turns[0].Text != "ok" {
		t.Fatalf("want the one valid turn, got %+v", turns)
	}
}

func TestPeekLog(t *testing.T) {
	dir := t.TempDir()
	if got := PeekLog(filepath.Join(dir, "none.jsonl"), 10, "(empty)"); got != "(empty)" {
		t.Fatalf("empty peek = %q", got)
	}
	path := filepath.Join(dir, "n.jsonl")
	w := &Writer{Path: path}
	w.Append(NewMeta("n1", "codex", "gpt-5.5", "/wd"))
	w.Append(Event{T: "user", Text: "line one\nline two"})
	got := PeekLog(path, 10, "(empty)")
	if !strings.Contains(got, "session n1 (codex gpt-5.5)") {
		t.Fatalf("peek misses meta line: %q", got)
	}
	if !strings.Contains(got, "» line one line two") {
		t.Fatalf("peek misses flattened user line: %q", got)
	}
}

// A /clear appends a source seam and turns the page; the peek must show only
// the current segment, never resurface the dead conversation behind the seam
// (finding 90).
func TestPeekLogScopedToCurrentSegment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	w.Append(NewMeta("n1", "opencode", "", "/wd"))
	w.Append(Event{T: "user", Text: "old question"})
	w.Append(Event{T: "assistant", Text: "old answer"})
	w.Append(NewSource("", "fresh-session"))
	got := PeekLog(path, 10, "(empty)")
	if strings.Contains(got, "old question") || strings.Contains(got, "old answer") {
		t.Fatalf("peek resurfaces pre-seam history: %q", got)
	}
	if !strings.Contains(got, "— source") {
		t.Fatalf("peek should keep the seam line as the boundary: %q", got)
	}
	w.Append(Event{T: "user", Text: "new question"})
	got = PeekLog(path, 10, "(empty)")
	if !strings.Contains(got, "» new question") || strings.Contains(got, "old answer") {
		t.Fatalf("post-seam peek wrong: %q", got)
	}
}

func TestSourceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	w.Append(NewMeta("n", "claude", "", ""))
	w.Append(NewSource("/x/aaa.jsonl", "aaa"))
	w.Append(Event{T: "user", Text: "hi"})
	evs := ReadEvents(path)
	if len(evs) != 3 || evs[1].T != "source" || evs[1].Source == nil {
		t.Fatalf("events: %+v", evs)
	}
	if evs[1].Source.Path != "/x/aaa.jsonl" || evs[1].Source.SessionID != "aaa" {
		t.Fatalf("source fields: %+v", evs[1].Source)
	}
	if turns := ReadTurns(path); len(turns) != 1 {
		t.Fatalf("source records must not become turns: %+v", turns)
	}
	if peek := PeekLog(path, 10, ""); !strings.Contains(peek, "— source /x/aaa.jsonl") {
		t.Fatalf("peek misses source seam: %q", peek)
	}
}
