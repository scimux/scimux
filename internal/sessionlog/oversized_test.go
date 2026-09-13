package sessionlog

import (
	"path/filepath"
	"strings"
	"testing"
)

// A record too large for a reader's buffer is not a reason to forget the rest
// of somebody's history. The writer accepts a record of any size, so a log can
// hold one -- a pasted file, an agent dumping a build log -- and every reader
// has to walk past it. The 8 MiB scanner stopped there instead, and because
// ReadEvents dropped the scanner's error it returned what it had read so far
// with no sign that anything was missing: for a log whose oversized record
// came early, that is the entire chat. The cache reads the same file with a
// bufio.Reader and has no such limit, so the two disagreed about one file.
func TestAnOversizedRecordDoesNotHideTheHistoryBehindIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	for _, ev := range []Event{
		NewMeta("oversized", "codex", "gpt", "high", "/tmp/wd"),
		{T: "user", Text: "before the giant"},
		{T: "assistant", Text: strings.Repeat("x", 9<<20)},
		{T: "user", Text: "after the giant"},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	var texts []string
	for _, ev := range ReadEvents(path) {
		if ev.T == "user" || ev.T == "assistant" {
			texts = append(texts, ev.Text[:min(len(ev.Text), 40)])
		}
	}
	if want := []string{"before the giant", "after the giant"}; !equalStrings(texts, want) {
		t.Errorf("ReadEvents replayed %q, want %q", texts, want)
	}

	// The search index and the tap that follows a hit back to its turn must
	// agree on record ordinals with each other, or a hit opens the wrong turn.
	res := ScanLog(path, "after the giant", ScanOptions{Before: 1, After: 1})
	if len(res.Hits) != 1 {
		t.Fatalf("search found %d hits for a turn behind the giant, want 1", len(res.Hits))
	}
	window, anchor, _, _, ok := ReadTurnWindow(path, res.Hits[0].Segment, res.Hits[0].Record, 1, 1)
	if !ok || anchor >= len(window) || window[anchor].Text != "after the giant" {
		t.Errorf("the hit's identity resolved to %+v (ok=%v, anchor=%d), want the turn it was cut from",
			window, ok, anchor)
	}

	// The cache is the chat read path; a reader that skips the giant and one
	// that does not would show the user two different conversations.
	var cached []string
	for _, turn := range (&LogCache{}).Segment(path).Turns {
		cached = append(cached, turn.Text[:min(len(turn.Text), 40)])
	}
	if !equalStrings(cached, texts) {
		t.Errorf("the cache serves %q while ReadEvents replays %q", cached, texts)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
