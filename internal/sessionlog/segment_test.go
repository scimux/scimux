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
	meta := NewMeta("n1", "codex", "gpt", "", "/tmp")
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
		NewMeta("n1", "claude", "", "", "/tmp"),
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
		NewMeta("n1", "codex", "", "", "/tmp"),
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

func TestSegmentMirroredOldTurnsDateAtTheTurn(t *testing.T) {
	// A mirror/adopt/import seam is stamped at bind time, but the turns it
	// precedes can be far older. The surface must date at the earliest turn, not
	// the seam, so the UI doesn't render "chat started" at import time above
	// turns visibly from a year earlier (R20.9).
	path := writeLog(t, []Event{
		NewMeta("n1", "claude", "", "", "/tmp"),
		{T: "source", Time: "2026-07-20T09:00:00Z", Source: &SourceEvent{SessionID: "s1"}},
		{T: "user", Text: "old q", Time: "2025-03-01T12:00:00Z"},
		{T: "assistant", Text: "old a", Time: "2025-03-01T12:01:00Z"},
	})
	seg := ReadSegment(path)
	if seg.StartTime != "2025-03-01T12:00:00Z" {
		t.Fatalf("start=%q, want the oldest turn's own time", seg.StartTime)
	}
}

func TestSegmentFreshClearKeepsSeamTime(t *testing.T) {
	// A genuine /clear: the fresh turns all follow the seam in time, so the seam
	// stamp is the true chat-start and must not be pulled back (R20.9 guard).
	path := writeLog(t, []Event{
		NewMeta("n1", "codex", "", "", "/tmp"),
		{T: "user", Text: "old q", Time: "2026-07-17T09:00:00Z"},
		{T: "source", Time: "2026-07-17T10:00:00Z", Source: &SourceEvent{SessionID: "s2"}},
		{T: "user", Text: "new q", Time: "2026-07-17T10:05:00Z"},
	})
	seg := ReadSegment(path)
	if seg.StartTime != "2026-07-17T10:00:00Z" {
		t.Fatalf("start=%q, want the seam time", seg.StartTime)
	}
}

func TestSegmentFreshClear(t *testing.T) {
	// Seam as the last record: the fresh surface is empty but the divider data
	// (prior turns, start time) is there.
	path := writeLog(t, []Event{
		NewMeta("n1", "claude", "", "", "/tmp"),
		NewSource("/t/s1.jsonl", "s1"),
		{T: "user", Text: "q"},
		NewSource("", ""), // detached seam at /clear time
	})
	seg := ReadSegment(path)
	if len(seg.Turns) != 0 || seg.PriorTurns != 1 || seg.StartTime == "" {
		t.Fatalf("got turns=%d prior=%d start=%q", len(seg.Turns), seg.PriorTurns, seg.StartTime)
	}
}

func TestReadHistory(t *testing.T) {
	// The whole log as ordered surfaces: first surface (dated at meta),
	// a /clear surface, and a trailing empty surface (detached seam) dropped.
	// Empty back-to-back mechanical seams are dropped too.
	meta := NewMeta("n1", "claude", "", "", "/tmp")
	path := writeLog(t, []Event{
		meta,
		{T: "user", Text: "old q", Time: "2026-07-01T09:00:00Z"},
		{T: "assistant", Text: "old a", Time: "2026-07-01T09:01:00Z"},
		{T: "assistant", Text: "   ", Time: "2026-07-01T09:02:00Z"}, // whitespace-only: never rendered
		{T: "source", Time: "2026-07-02T10:00:00Z", Source: &SourceEvent{SessionID: "s2", Reason: "clear"}},
		{T: "user", Text: "new q", Time: "2026-07-02T10:05:00Z"},
		NewSource("", ""), // detached seam, nothing after: no empty surface
	})
	segs := ReadHistory(path)
	if len(segs) != 2 {
		t.Fatalf("got %d surfaces: %+v", len(segs), segs)
	}
	// The turns predate the meta stamp (written now), so the backdate rule
	// dates the surface at the first turn; the seam keeps the meta time.
	if len(segs[0].Turns) != 2 || segs[0].Reason != "" ||
		segs[0].Start != "2026-07-01T09:00:00Z" || segs[0].Seam != meta.Time {
		t.Fatalf("first surface: %+v (want start at first turn, seam %q)", segs[0], meta.Time)
	}
	if len(segs[1].Turns) != 1 || segs[1].Reason != "clear" ||
		segs[1].Seam != "2026-07-02T10:00:00Z" || segs[1].Start != "2026-07-02T10:00:00Z" {
		t.Fatalf("clear surface: %+v", segs[1])
	}
	if ReadHistory(filepath.Join(t.TempDir(), "missing.jsonl")) == nil {
		t.Fatal("missing log must yield an empty, non-nil slice (JSON [])")
	}
}

func TestReadHistoryBackdatesMirroredTurns(t *testing.T) {
	// Same R20.9 rule as Segment.StartTime: a bind seam stamped now above
	// year-old adopted turns dates the surface at the first turn. Seam keeps
	// the raw time (the map's stop key must still match).
	path := writeLog(t, []Event{
		NewMeta("n1", "claude", "", "", "/tmp"),
		{T: "source", Time: "2026-07-20T09:00:00Z", Source: &SourceEvent{SessionID: "s1"}},
		{T: "user", Text: "old q", Time: "2025-03-01T12:00:00Z"},
	})
	segs := ReadHistory(path)
	if len(segs) != 1 || segs[0].Start != "2025-03-01T12:00:00Z" || segs[0].Seam != "2026-07-20T09:00:00Z" {
		t.Fatalf("got %+v", segs)
	}
}

func TestSegmentCache(t *testing.T) {
	path := writeLog(t, []Event{
		NewMeta("n1", "codex", "", "", "/tmp"),
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
