package sessionlog

import (
	"os"
	"strings"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/transcript"
)

// Segment is the log's tail after its last source seam — "the current
// conversation" in every transport's terms. A /clear (or a transcript
// relink/rollover on a tmux node) appends a source record; everything after
// it is the fresh chat surface, everything before it stays in the same file
// as prior history. A log that never rolled over is one segment.
type Segment struct {
	Turns      []transcript.Turn
	PriorTurns int // turns before the last seam; >0 means "render a divider"
	// StartTime is when the current chat surface began: the last seam's own
	// timestamp, or — for a log that never rolled over — the first timed
	// record's (normally the meta header). Every chat gets a "chat started"
	// header from it, not just post-/clear segments; "" only for a missing
	// or empty log.
	StartTime string
	// Usage folded from records within the segment only: a rollover starts a
	// fresh context, so the gauge must not carry the dead session's fill.
	Used, Size int64
	// ClearTimes are the timestamps of the log's clear-tagged seams (deliberate
	// /clear page-turns), oldest first — the metro map's stop boundaries. Only
	// reason:"clear" seams count: mechanical binds (relink, rotation, adoption)
	// also write source records, and a heavily relinked thread must not sprout
	// bogus stations. Whole-log data, not segment-scoped, riding here so the
	// existing per-node cache serves it for free.
	ClearTimes []string
}

func segmentOf(evs []Event) Segment {
	var seg Segment
	seg.Turns = []transcript.Turn{}
	for _, ev := range evs {
		if seg.StartTime == "" && ev.Time != "" {
			seg.StartTime = ev.Time // first-segment fallback; a seam overwrites
		}
		switch ev.T {
		case "source":
			seg.PriorTurns += len(seg.Turns)
			seg.Turns = seg.Turns[:0]
			seg.StartTime = ev.Time
			seg.Used, seg.Size = 0, 0
			if ev.Source != nil && ev.Source.Reason == "clear" {
				seg.ClearTimes = append(seg.ClearTimes, ev.Time)
			}
		case "user", "assistant":
			// Same filter as ReadTurns: whitespace-only records render nothing.
			if strings.TrimSpace(ev.Text) != "" {
				seg.Turns = append(seg.Turns, transcript.Turn{Role: ev.T, Text: ev.Text, Time: ev.Time})
			}
		case "usage":
			if ev.Usage != nil {
				if ev.Usage.Used > 0 {
					seg.Used = int64(ev.Usage.Used)
				}
				if ev.Usage.Size > 0 {
					seg.Size = int64(ev.Usage.Size)
				}
			}
		}
	}
	// A mirror/adopt/import seam is stamped at bind time, but the turns it
	// precedes can be far older (an adopted claude session, or one of the
	// imported archives). Dating the surface at the seam then renders "chat
	// started 07-20" above turns visibly from 2025 (R20.9). When the first turn
	// of the segment predates its StartTime, prefer the turn's own timestamp;
	// a genuine /clear, whose fresh turns all follow the seam, is unaffected.
	if len(seg.Turns) > 0 {
		if t0 := seg.Turns[0].Time; t0 != "" && earlier(t0, seg.StartTime) {
			seg.StartTime = t0
		}
	}
	return seg
}

// earlier reports whether timestamp a is chronologically before b. Both are
// raw logged strings (RFC3339 from seams, whatever the CLI wrote for turns);
// parse defensively and, if either is unparseable, make no claim (false) so a
// fabricated stamp is only ever corrected on solid evidence.
func earlier(a, b string) bool {
	ta, ea := parseStamp(a)
	tb, eb := parseStamp(b)
	if ea != nil || eb != nil {
		return false
	}
	return ta.Before(tb)
}

func parseStamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// ReadSegment parses the log and returns its current segment.
func ReadSegment(path string) Segment {
	return segmentOf(ReadEvents(path))
}

// HistorySegment is one chat surface of the whole log, oldest first — the
// on-demand read behind the chat's "show earlier history" divider and the
// metro map's earlier stops, where Segment is only the live tail. Seam is the
// opening seam's raw timestamp (for clear seams this is the map's stop time,
// so the UI can match a tapped stop to its surface); Start is the display
// time, backdated to the first turn when the turns predate the seam (same
// R20.9 rule as Segment.StartTime — adopted/imported turns can be far older
// than their bind seam).
type HistorySegment struct {
	Start  string            `json:"start"`
	Seam   string            `json:"seam"`
	Reason string            `json:"reason,omitempty"` // opening seam's reason; "" for the log's first surface
	Turns  []transcript.Turn `json:"turns"`
}

// ReadHistory parses the log into all its surfaces, oldest first. Surfaces
// with no readable turns (back-to-back mechanical seams: detach + relink, or
// a /clear nothing was said after) render nothing and are dropped. Turns can
// repeat across mechanical seams — a rotation re-mirrors the transcript from
// turn zero behind a fresh seam — and history is a reading surface, so the
// honest repeat beats a dedup guess. Not cached: this is a per-tap read, not
// a poll path.
func ReadHistory(path string) []HistorySegment {
	segs := []HistorySegment{}
	cur := HistorySegment{Turns: []transcript.Turn{}}
	flush := func() {
		if len(cur.Turns) == 0 {
			return
		}
		if t0 := cur.Turns[0].Time; t0 != "" && earlier(t0, cur.Start) {
			cur.Start = t0
		}
		segs = append(segs, cur)
	}
	for _, ev := range ReadEvents(path) {
		// first-surface fallback: date it at the first timed record (normally
		// the meta header), exactly like segmentOf; a seam overwrites
		if cur.Start == "" && ev.Time != "" {
			cur.Start, cur.Seam = ev.Time, ev.Time
		}
		switch ev.T {
		case "source":
			flush()
			cur = HistorySegment{Start: ev.Time, Seam: ev.Time, Turns: []transcript.Turn{}}
			if ev.Source != nil {
				cur.Reason = ev.Source.Reason
			}
		case "user", "assistant":
			// Same filter as segmentOf: whitespace-only records render nothing.
			if strings.TrimSpace(ev.Text) != "" {
				cur.Turns = append(cur.Turns, transcript.Turn{Role: ev.T, Text: ev.Text, Time: ev.Time})
			}
		}
	}
	flush()
	return segs
}

// Cache memoizes one node's parsed segment keyed by the file's (size, mtime).
// The chat endpoint polls every second while logs change only at turn/tool
// granularity, so the common case is a stat plus a cache hit; a change
// re-parses the whole file. (Records are appended, never rewritten, so size
// alone almost suffices — mtime guards the delete-and-reissue case where a
// fresh log happens to match the old length.)
type Cache struct {
	mu    sync.Mutex
	path  string
	size  int64
	mtime time.Time
	seg   Segment
}

func (c *Cache) Segment(path string) Segment {
	st, err := os.Stat(path)
	if err != nil {
		return Segment{Turns: []transcript.Turn{}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.path == path && c.size == st.Size() && c.mtime.Equal(st.ModTime()) {
		return c.seg
	}
	seg := ReadSegment(path)
	c.path, c.size, c.mtime, c.seg = path, st.Size(), st.ModTime(), seg
	return seg
}
