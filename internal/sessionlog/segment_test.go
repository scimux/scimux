package sessionlog

import (
	"os"
	"path/filepath"
	"testing"

	"codeberg.org/chrberger/scimux/internal/transcript"
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

func TestSegmentTurnAddresses(t *testing.T) {
	// Every rendered turn carries the durable address (UID, Segment, Record) —
	// the same positional identity ScanLog assigns — so a capture can name the
	// exact chat turn it came from. Record is the 0-based index among
	// successfully-parsed records (tool/asset/usage records advance it too);
	// Segment is the source seams seen before the record; UID is the meta uid.
	meta := NewMeta("n1", "claude", "", "", "/tmp")
	path := writeLog(t, []Event{
		meta,
		{T: "user", Text: "seg0 question", Time: "2026-07-01T09:00:00Z"},
		{T: "tool", Tool: &ToolEvent{ID: "t1", Title: "Read", Status: "completed"}},
		{T: "asset", Asset: &AssetEvent{ID: "a1", Name: "diagram.png", Storage: "inline"}},
		{T: "assistant", Text: "seg0 answer", Time: "2026-07-01T09:01:00Z"},
		{T: "usage", Usage: &UsageEvent{Used: 100, Size: 1000}},
		NewClearSource("s2"), // /clear seam: segment 1 begins
		{T: "user", Text: "seg1 followup", Time: "2026-07-02T10:00:00Z"},
	})
	uid := meta.Meta.UID

	seg := ReadSegment(path)
	// The current segment is everything after the last seam: only the followup.
	if len(seg.Turns) != 1 || seg.Turns[0].Text != "seg1 followup" {
		t.Fatalf("segment turns = %+v", seg.Turns)
	}
	tn := seg.Turns[0]
	if tn.UID != uid || tn.Segment != 1 {
		t.Fatalf("followup address = uid %q seg %d, want %q seg 1", tn.UID, tn.Segment, uid)
	}
	// Cross-check the ordinal against ScanLog: a hit on the same text must land
	// on the same (Segment, Record). This is the load-bearing invariant — the
	// read path and the search path must agree numerically or jump-back breaks.
	res := ScanLog(path, "seg1 followup", ScanOptions{Before: 5, After: 5})
	if len(res.Hits) != 1 {
		t.Fatalf("ScanLog hits = %+v", res.Hits)
	}
	if res.Hits[0].Segment != tn.Segment || res.Hits[0].Record != tn.Record || res.Hits[0].UID != tn.UID {
		t.Fatalf("address mismatch: turn (uid %q seg %d rec %d) vs ScanLog (uid %q seg %d rec %d)",
			tn.UID, tn.Segment, tn.Record, res.Hits[0].UID, res.Hits[0].Segment, res.Hits[0].Record)
	}
	// Round-trip: the turn's own address resolves back to that exact turn.
	win, anchor, _, _, ok := ReadTurnWindow(path, tn.Segment, tn.Record, 0, 0)
	if !ok || win[anchor].Text != tn.Text {
		t.Fatalf("round-trip: ok=%v anchor=%q, want %q", ok, mustText(win, anchor), tn.Text)
	}
}

func TestReadHistoryTurnAddresses(t *testing.T) {
	// The same durable address rides every history surface, not just the live
	// tail — an embedded reference into an earlier /clear surface needs it too.
	meta := NewMeta("n1", "codex", "", "", "/tmp")
	path := writeLog(t, []Event{
		meta,
		{T: "user", Text: "old question", Time: "2026-07-01T09:00:00Z"},
		{T: "assistant", Text: "old answer", Time: "2026-07-01T09:01:00Z"},
		NewClearSource("s2"),
		{T: "user", Text: "new question", Time: "2026-07-02T10:00:00Z"},
	})
	uid := meta.Meta.UID
	segs := ReadHistory(path)
	if len(segs) != 2 {
		t.Fatalf("surfaces = %d", len(segs))
	}
	// Surface 0 turns are in segment 0; surface 1's are in segment 1. Each turn
	// round-trips through its own address and reports the meta uid.
	for si, s := range segs {
		for _, tn := range s.Turns {
			if tn.UID != uid || tn.Segment != si {
				t.Fatalf("surface %d turn %q: uid %q seg %d, want %q seg %d", si, tn.Text, tn.UID, tn.Segment, uid, si)
			}
			win, anchor, _, _, ok := ReadTurnWindow(path, tn.Segment, tn.Record, 0, 0)
			if !ok || win[anchor].Text != tn.Text {
				t.Fatalf("surface %d turn %q did not round-trip (ok=%v)", si, tn.Text, ok)
			}
		}
	}
}

func TestSegmentTurnAddressesSkipTornLines(t *testing.T) {
	// A torn/blank line between two turns is not a parsed record, so it must not
	// advance the record ordinal — the address stays aligned with ScanLog, which
	// skips the same lines. Write a good log, then splice a garbage line in.
	meta := NewMeta("n1", "claude", "", "", "/tmp")
	path := writeLog(t, []Event{
		meta,
		{T: "user", Text: "before torn", Time: "2026-07-01T09:00:00Z"},
	})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n{not valid json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	w := &Writer{Path: path}
	if err := w.Append(Event{T: "assistant", Text: "after torn", Time: "2026-07-01T09:02:00Z"}); err != nil {
		t.Fatal(err)
	}
	seg := ReadSegment(path)
	if len(seg.Turns) != 2 {
		t.Fatalf("turns = %+v", seg.Turns)
	}
	for _, tn := range seg.Turns {
		res := ScanLog(path, tn.Text, ScanOptions{Before: 5, After: 5})
		if len(res.Hits) != 1 || res.Hits[0].Segment != tn.Segment || res.Hits[0].Record != tn.Record {
			t.Fatalf("turn %q address drifted from ScanLog: turn(seg %d rec %d) scan %+v", tn.Text, tn.Segment, tn.Record, res.Hits)
		}
	}
}

// mustText is a test helper: the text of window[i], or a marker when out of range.
func mustText(window []transcript.Turn, i int) string {
	if i < 0 || i >= len(window) {
		return "<no anchor>"
	}
	return window[i].Text
}

func TestSegmentCache(t *testing.T) {
	path := writeLog(t, []Event{
		NewMeta("n1", "codex", "", "", "/tmp"),
		{T: "user", Text: "one"},
	})
	var c LogCache
	if got := c.Segment(path); len(got.Turns) != 1 {
		t.Fatalf("first read: %+v", got.Turns)
	}
	// Unchanged file: served from cache (observable only as equality here).
	if got := c.Segment(path); len(got.Turns) != 1 {
		t.Fatalf("cached read: %+v", got.Turns)
	}
	if c.Walks() != 1 {
		t.Fatalf("unchanged re-read Walks() = %d, want 1", c.Walks())
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

func TestSegmentStations(t *testing.T) {
	// Per-station label snapshots fold into Segment.Stations keyed by the
	// station's start-time seam; the latest record for a seam wins (a manual
	// edit is just another snapshot). Whole-log data, like ClearTimes.
	path := writeLog(t, []Event{
		NewMeta("n1", "claude", "", "", "/tmp"),
		NewStation("2026-07-01T09:00:00Z", "Alpha", "first station"),
		NewStation("2026-07-02T10:00:00Z", "Beta", "second station"),
		// a later edit of the first station overrides the earlier snapshot
		NewStation("2026-07-01T09:00:00Z", "Alpha renamed", "edited"),
	})
	seg := ReadSegment(path)
	if seg.Stations == nil {
		t.Fatal("Stations must be a non-nil map")
	}
	if got := seg.Stations["2026-07-01T09:00:00Z"]; got.Title != "Alpha renamed" || got.Desc != "edited" {
		t.Fatalf("first station = %+v, want latest override", got)
	}
	if got := seg.Stations["2026-07-02T10:00:00Z"]; got.Title != "Beta" || got.Desc != "second station" {
		t.Fatalf("second station = %+v", got)
	}
}

func TestSegmentStationsEmpty(t *testing.T) {
	// A log with no station records yields an empty (non-nil) map — legacy
	// safety, so the projection never nil-panics and old stops fall back cleanly.
	path := writeLog(t, []Event{
		NewMeta("n1", "codex", "", "", "/tmp"),
		{T: "user", Text: "hi"},
	})
	seg := ReadSegment(path)
	if seg.Stations == nil || len(seg.Stations) != 0 {
		t.Fatalf("Stations = %+v, want empty non-nil map", seg.Stations)
	}
}

func TestNewStationRoundTrip(t *testing.T) {
	// NewStation survives marshal → append → ReadEvents unchanged.
	path := writeLog(t, []Event{NewStation("2026-07-01T09:00:00Z", "Name", "Desc")})
	evs := ReadEvents(path)
	if len(evs) != 1 || evs[0].T != "station" || evs[0].Station == nil {
		t.Fatalf("got %+v", evs)
	}
	if s := evs[0].Station; s.Seam != "2026-07-01T09:00:00Z" || s.Title != "Name" || s.Desc != "Desc" {
		t.Fatalf("station = %+v", s)
	}
}
