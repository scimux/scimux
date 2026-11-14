package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A record longer than the read buffer that is not complete yet must not move
// the returned offset: a reader that resumes there would start inside it.
func TestForEachRecordLeavesALongTornLineUnconsumed(t *testing.T) {
	head := `{"t":"user","text":"one"}` + "\n"
	for name, torn := range map[string]int{
		"over one buffer":     80_000,
		"over the record cap": maxRecordBytes + 3*64*1024,
	} {
		t.Run(name, func(t *testing.T) {
			records := 0
			off, err := forEachRecord(strings.NewReader(head+strings.Repeat("w", torn)), func([]byte, int64) bool {
				records++
				return true
			})
			if err != nil {
				t.Fatal(err)
			}
			if off != int64(len(head)) || records != 1 {
				t.Fatalf("offset = %d records = %d, want %d and 1", off, records, len(head))
			}
		})
	}
}

// The offset given to fn and the returned offset still count a long record
// once its newline has arrived.
func TestForEachRecordCountsALongCompleteLine(t *testing.T) {
	long := `{"t":"assistant","text":"` + strings.Repeat("w", 100_000) + `"}` + "\n"
	in := long + `{"t":"user","text":"two"}` + "\n"
	var offs []int64
	off, err := forEachRecord(strings.NewReader(in), func(_ []byte, o int64) bool {
		offs = append(offs, o)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if off != int64(len(in)) || len(offs) != 2 || offs[0] != int64(len(long)) || offs[1] != int64(len(in)) {
		t.Fatalf("offset = %d record offsets = %v", off, offs)
	}
}

// A running cache that looks at the log while a long record is half written
// shows the same chat as a cold read once the record is complete.
func TestLogCacheKeepsALongRecordSeenInTwoParts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	add := func(s string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	long := `{"t":"assistant","time":"2026-01-02T03:04:07Z","text":"` + strings.Repeat("w", 100_000) + `"}` + "\n"
	c := &LogCache{}
	add(`{"t":"meta","time":"2026-01-02T03:04:05Z","meta":{"node":"n","uid":"00112233aabbccdd","agent":"x"}}` + "\n")
	add(`{"t":"user","time":"2026-01-02T03:04:06Z","text":"one"}` + "\n")
	c.Segment(path)
	add(long[:80_000])
	c.Segment(path)
	add(long[80_000:])
	c.Segment(path)
	add(`{"t":"user","time":"2026-01-02T03:04:08Z","text":"after"}` + "\n")
	warm := c.Segment(path)
	cold := (&LogCache{}).Segment(path)
	if len(cold.Turns) != 3 || len(warm.Turns) != len(cold.Turns) {
		t.Fatalf("running cache has %d turns, cold read has %d, want 3", len(warm.Turns), len(cold.Turns))
	}
	if warm.Turns[1].Text != cold.Turns[1].Text || warm.Turns[2].Text != "after" {
		t.Fatal("running cache differs from the cold read")
	}
}
