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
	PriorTurns int    // turns before the last seam; >0 means "render a divider"
	StartTime  string // the last seam's own timestamp ("" when no seam exists)
	// Usage folded from records within the segment only: a rollover starts a
	// fresh context, so the gauge must not carry the dead session's fill.
	Used, Size int64
}

func segmentOf(evs []Event) Segment {
	var seg Segment
	seg.Turns = []transcript.Turn{}
	for _, ev := range evs {
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
	return seg
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
