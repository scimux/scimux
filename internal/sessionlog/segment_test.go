package sessionlog

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLog(t *testing.T, evs []Event) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.jsonl")
	w := &Writer{Path: path}
	for _, ev := range evs {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestSegmentNoSeam(t *testing.T) {
	// A log that never rolled over is one segment: all turns, no divider,
	// and the chat's start stamp comes from the meta header (every chat
	// renders a "chat started" header, not just post-/clear segments).
	meta := NewMeta("n1", "codex", "gpt", "/tmp")
	path := writeLog(t, []Event{
		meta,
		{T: "user", Text: "hello"},
		{T: "assistant", Text: "hi"},
		{T: "usage", Usage: &UsageEvent{Used: 100, Size: 1000}},
	})
	seg := ReadSegment(path)
	if len(seg.Turns) != 2 || seg.PriorTurns != 0 || seg.StartTime != meta.Time {
		t.Fatalf("got turns=%d prior=%d start=%q (want start %q)", len(seg.Turns), seg.PriorTurns, seg.StartTime, meta.Time)
	}
	if seg.Used != 100 || seg.Size != 1000 {
		t.Fatalf("usage got %d/%d", seg.Used, seg.Size)
	}
}

func TestSegmentFirstBindIsNoRollover(t *testing.T) {
	// A tmux node's first source seam precedes any turns — no divider.
	path := writeLog(t, []Event{
		NewMeta("n1", "claude", "", "/tmp"),
		NewSource("/t/s1.jsonl", "s1"),
		{T: "user", Text: "q"},
		{T: "assistant", Text: "a"},
	})
	seg := ReadSegment(path)
	if len(seg.Turns) != 2 || seg.PriorTurns != 0 {
		t.Fatalf("got turns=%d prior=%d", len(seg.Turns), seg.PriorTurns)
	}
	if seg.StartTime == "" {
		t.Fatal("seam time should be recorded")
	}
}

func TestSegmentRollover(t *testing.T) {
	// /clear: turns after the last seam only; prior count and seam time kept;
	// usage resets at the seam (fresh context, fresh gauge).
	path := writeLog(t, []Event{
		NewMeta("n1", "codex", "", "/tmp"),
		{T: "user", Text: "old q"},
		{T: "assistant", Text: "old a"},
		{T: "usage", Usage: &UsageEvent{Used: 900, Size: 1000}},
		{T: "source", Time: "2026-07-17T10:00:00Z", Source: &SourceEvent{SessionID: "s2"}},
		{T: "user", Text: "new q"},
	})
	seg := ReadSegment(path)
	if len(seg.Turns) != 1 || seg.Turns[0].Text != "new q" {
		t.Fatalf("turns: %+v", seg.Turns)
	}
	if seg.PriorTurns != 2 || seg.StartTime != "2026-07-17T10:00:00Z" {
		t.Fatalf("prior=%d start=%q", seg.PriorTurns, seg.StartTime)
	}
	if seg.Used != 0 || seg.Size != 0 {
		t.Fatalf("usage must reset at the seam, got %d/%d", seg.Used, seg.Size)
	}
}

func TestSegmentFreshClear(t *testing.T) {
	// Seam as the last record: the fresh surface is empty but the divider data
	// (prior turns, start time) is there.
	path := writeLog(t, []Event{
		NewMeta("n1", "claude", "", "/tmp"),
		NewSource("/t/s1.jsonl", "s1"),
		{T: "user", Text: "q"},
		NewSource("", ""), // detached seam at /clear time
	})
	seg := ReadSegment(path)
	if len(seg.Turns) != 0 || seg.PriorTurns != 1 || seg.StartTime == "" {
		t.Fatalf("got turns=%d prior=%d start=%q", len(seg.Turns), seg.PriorTurns, seg.StartTime)
	}
}

func TestSegmentCache(t *testing.T) {
	path := writeLog(t, []Event{
		NewMeta("n1", "codex", "", "/tmp"),
		{T: "user", Text: "one"},
	})
	var c Cache
	if got := c.Segment(path); len(got.Turns) != 1 {
		t.Fatalf("first read: %+v", got.Turns)
	}
	// Unchanged file: served from cache (observable only as equality here).
	if got := c.Segment(path); len(got.Turns) != 1 {
		t.Fatalf("cached read: %+v", got.Turns)
	}
	// Append → cache must notice.
	w := &Writer{Path: path}
	if err := w.Append(Event{T: "assistant", Text: "two"}); err != nil {
		t.Fatal(err)
	}
	if got := c.Segment(path); len(got.Turns) != 2 {
		t.Fatalf("after append: %+v", got.Turns)
	}
	// Missing file degrades to an empty segment.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := c.Segment(path); len(got.Turns) != 0 {
		t.Fatalf("after remove: %+v", got.Turns)
	}
}
