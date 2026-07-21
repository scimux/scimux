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
