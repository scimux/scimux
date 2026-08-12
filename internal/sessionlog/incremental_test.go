package sessionlog

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"codeberg.org/chrberger/scimux/internal/fare"
)

// Stage B — incremental tail parse. Every product after every append must
// deep-equal a cold Read* of the same bytes. Watermark is the byte offset
// after the last consumed newline, never st.Size() of a torn tail.

func appendRaw(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertProductsEqual(t *testing.T, c *LogCache, path, label string) {
	t.Helper()
	gotSeg := c.Segment(path)
	wantSeg := ReadSegment(path)
	if !reflect.DeepEqual(gotSeg, wantSeg) {
		t.Errorf("%s Segment:\n got = %+v\nwant = %+v", label, gotSeg, wantSeg)
	}
	gotFare, gotRides := c.FareAndRides(path)
	wantFare := ReadFare(path)
	wantRides := ReadRidesBySegment(path)
	if !reflect.DeepEqual(gotFare, wantFare) {
		t.Errorf("%s Fare:\n got = %+v\nwant = %+v", label, gotFare, wantFare)
	}
	if !reflect.DeepEqual(gotRides, wantRides) {
		t.Errorf("%s Rides:\n got = %+v\nwant = %+v", label, gotRides, wantRides)
	}
	gotAnch := c.AnchoredAssets(path)
	wantAnch := ReadAnchoredAssets(path)
	if !reflect.DeepEqual(gotAnch, wantAnch) {
		t.Errorf("%s Anchored:\n got = %+v\nwant = %+v", label, gotAnch, wantAnch)
	}
	gotAssets := c.Assets(path)
	wantAssets := ReadAssets(path)
	if !reflect.DeepEqual(gotAssets, wantAssets) {
		t.Errorf("%s Assets:\n got = %+v\nwant = %+v", label, gotAssets, wantAssets)
	}
}

// B1. Byte accounting: after a warm load, appending one record must parse
// approximately the appended line's length — not the whole file.
func TestLogCache_BytesParsedOnAppend(t *testing.T) {
	path := richLog(t)
	c := &LogCache{}
	_ = c.Segment(path)
	full := c.BytesParsed()
	if full <= 0 {
		t.Fatalf("cold BytesParsed() = %d, want > 0", full)
	}

	line, err := json.Marshal(Event{T: "user", Time: "2026-07-15T12:00:00Z", Text: "appended-one"})
	if err != nil {
		t.Fatal(err)
	}
	line = append(line, '\n')
	appendRaw(t, path, line)

	_ = c.Segment(path)
	got := c.BytesParsed()
	// Tight enough that a full re-parse fails: must be under 4× the line and
	// well under the full file size.
	if got > int64(len(line))*4 {
		t.Fatalf("BytesParsed after append = %d, want ≈ %d (got full re-parse of %d?)", got, len(line), full)
	}
	if got >= full {
		t.Fatalf("BytesParsed after append = %d ≥ cold full %d — not incremental", got, full)
	}
	if got < int64(len(line)) {
		t.Fatalf("BytesParsed after append = %d, want ≥ line len %d", got, len(line))
	}
}

// B2. Torn tail: partial line is not consumed; completing it yields cold equality.
// Also covers a multi-byte UTF-8 character straddling the torn boundary.
func TestLogCache_TornTail(t *testing.T) {
	path := writeLog(t, []Event{
		NewMeta("n1", "claude", "sonnet", "", "/tmp"),
		{T: "user", Time: "2026-07-15T09:00:00Z", Text: "hello"},
	})
	c := &LogCache{}
	assertProductsEqual(t, c, path, "initial")

	// Build a complete record that includes a multi-byte rune (€ = e2 82 ac).
	ev := Event{T: "user", Time: "2026-07-15T09:01:00Z", Text: "price €12"}
	full, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	full = append(full, '\n')

	// Find the € bytes inside the JSON and tear mid-character.
	euro := []byte("€")
	idx := -1
	for i := 0; i+len(euro) <= len(full); i++ {
		if full[i] == euro[0] && full[i+1] == euro[1] && full[i+2] == euro[2] {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("fixture: euro rune not found in marshalled line")
	}
	// Tear after the first byte of the 3-byte UTF-8 sequence.
	tearAt := idx + 1
	appendRaw(t, path, full[:tearAt])
	assertProductsEqual(t, c, path, "after torn partial")

	// Complete the line.
	appendRaw(t, path, full[tearAt:])
	assertProductsEqual(t, c, path, "after torn completed")

	// Also a simple ASCII torn line (no UTF-8 mid-char).
	ev2 := Event{T: "assistant", Time: "2026-07-15T09:02:00Z", Text: "done"}
	full2, err := json.Marshal(ev2)
	if err != nil {
		t.Fatal(err)
	}
	full2 = append(full2, '\n')
	mid := len(full2) / 2
	appendRaw(t, path, full2[:mid])
	assertProductsEqual(t, c, path, "ascii torn partial")
	appendRaw(t, path, full2[mid:])
	assertProductsEqual(t, c, path, "ascii torn completed")
}

// B3. Seam in the tail: clear and mechanical (empty-reason) source seams
// after existing turns must match a cold read for PriorTurns, Turns,
// StartTime, Used/Size, ClearTimes.
func TestLogCache_SeamInTail(t *testing.T) {
	path := writeLog(t, []Event{
		NewMeta("n1", "codex", "gpt", "", "/tmp"),
		{T: "user", Time: "2026-07-15T09:00:00Z", Text: "old q"},
		{T: "assistant", Time: "2026-07-15T09:00:01Z", Text: "old a"},
		{T: "usage", Time: "2026-07-15T09:00:02Z", Usage: &UsageEvent{Used: 900, Size: 1000, InputTokens: 1, OutputTokens: 2, TurnID: "u1"}},
	})
	c := &LogCache{}
	assertProductsEqual(t, c, path, "before clear")

	w := &Writer{Path: path}
	if err := w.Append(Event{T: "source", Time: "2026-07-15T10:00:00Z", Source: &SourceEvent{SessionID: "s2", Reason: "clear"}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Event{T: "user", Time: "2026-07-15T10:00:01Z", Text: "new q"}); err != nil {
		t.Fatal(err)
	}
	assertProductsEqual(t, c, path, "after clear seam")

	seg := c.Segment(path)
	if seg.PriorTurns != 2 || len(seg.Turns) != 1 || seg.Turns[0].Text != "new q" {
		t.Fatalf("clear seam segment = prior=%d turns=%+v", seg.PriorTurns, seg.Turns)
	}
	if seg.Used != 0 || seg.Size != 0 {
		t.Fatalf("usage must reset at clear, got %d/%d", seg.Used, seg.Size)
	}
	if len(seg.ClearTimes) != 1 || seg.ClearTimes[0] != "2026-07-15T10:00:00Z" {
		t.Fatalf("ClearTimes = %v", seg.ClearTimes)
	}

	// Mechanical empty-reason seam.
	if err := w.Append(NewSource("/t/s3.jsonl", "s3")); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Event{T: "user", Time: "2026-07-15T11:00:00Z", Text: "after mech"}); err != nil {
		t.Fatal(err)
	}
	assertProductsEqual(t, c, path, "after mechanical seam")
}

// B4. Fare retroactivity — the killer. A no-TurnID usage in segment k is
// counted; appending an empty-reason source seam must UN-count it.
// Mutation proof: a monotonic accumulator fails this.
func TestLogCache_FareRetroactivity(t *testing.T) {
	path := writeLog(t, []Event{
		NewMeta("n1", "claude", "sonnet", "", "/tmp"),
		{T: "user", Time: "2026-07-15T09:00:00Z", Text: "q"},
		{T: "usage", Time: "2026-07-15T09:00:01Z", Usage: &UsageEvent{
			// no TurnID — watermark path
			InputTokens: 100, OutputTokens: 50,
		}},
	})
	c := &LogCache{}
	f1, _ := c.FareAndRides(path)
	cold1 := ReadFare(path)
	if !reflect.DeepEqual(f1, cold1) {
		t.Fatalf("before seam: cached %+v != cold %+v", f1, cold1)
	}
	if f1.Turns != 1 || f1.FreshIn != 100 || f1.Out != 50 {
		t.Fatalf("before seam: fare = %+v, want counted 100/50 turns=1", f1)
	}

	// Mechanical source opens a new segment in the same clear-bounded epoch;
	// lastSegInEpoch moves forward → the earlier no-id hit is un-counted.
	w := &Writer{Path: path}
	if err := w.Append(NewSource("/t/rot.jsonl", "rot")); err != nil {
		t.Fatal(err)
	}
	f2, _ := c.FareAndRides(path)
	cold2 := ReadFare(path)
	if !reflect.DeepEqual(f2, cold2) {
		t.Fatalf("after seam: cached %+v != cold %+v", f2, cold2)
	}
	if cold2.Turns != 0 {
		t.Fatalf("precondition: cold after seam Turns=%d, want 0 (un-counted)", cold2.Turns)
	}
	if f2.FreshIn != 0 || f2.Out != 0 || f2.Turns != 0 {
		t.Fatalf("after seam: fare = %+v, want zero (retroactive un-count)", f2)
	}
	// ReportedCostComplete: no counted turns → complete stays true (zero ride).
	if !f2.ReportedCostComplete {
		t.Errorf("after seam: ReportedCostComplete = false, want true (no turns)")
	}
}

// B5. Record identity: after N incremental appends, every turn's
// (UID, Segment, Record) triple equals the cold read.
func TestLogCache_RecordIdentity(t *testing.T) {
	path := writeLog(t, []Event{
		NewMeta("n1", "claude", "sonnet", "", "/tmp"),
	})
	c := &LogCache{}
	_ = c.Segment(path)
	w := &Writer{Path: path}
	for i := 0; i < 10; i++ {
		if err := w.Append(Event{T: "user", Time: fmt.Sprintf("2026-07-15T09:00:%02dZ", i), Text: fmt.Sprintf("q%d", i)}); err != nil {
			t.Fatal(err)
		}
		if err := w.Append(Event{T: "assistant", Time: fmt.Sprintf("2026-07-15T09:00:%02dZ", i), Text: fmt.Sprintf("a%d", i)}); err != nil {
			t.Fatal(err)
		}
		if i == 4 {
			if err := w.Append(NewClearSource("s2")); err != nil {
				t.Fatal(err)
			}
		}
		got := c.Segment(path)
		want := ReadSegment(path)
		if len(got.Turns) != len(want.Turns) {
			t.Fatalf("step %d: turns %d vs %d", i, len(got.Turns), len(want.Turns))
		}
		for j := range got.Turns {
			gt, wt := got.Turns[j], want.Turns[j]
			if gt.UID != wt.UID || gt.Segment != wt.Segment || gt.Record != wt.Record {
				t.Fatalf("step %d turn %d: got (uid=%s seg=%d rec=%d) want (uid=%s seg=%d rec=%d)",
					i, j, gt.UID, gt.Segment, gt.Record, wt.UID, wt.Segment, wt.Record)
			}
		}
	}
}

// B6. Replacement and shrinkage: shorter replacement and truncate both cold-parse.
func TestLogCache_ReplacementAndShrink(t *testing.T) {
	path := writeLog(t, []Event{
		NewMeta("n1", "claude", "sonnet", "", "/tmp"),
		{T: "user", Time: "2026-07-15T09:00:00Z", Text: "long conversation turn one"},
		{T: "assistant", Time: "2026-07-15T09:00:01Z", Text: "long conversation turn two"},
		{T: "user", Time: "2026-07-15T09:00:02Z", Text: "long conversation turn three"},
	})
	c := &LogCache{}
	assertProductsEqual(t, c, path, "initial long")
	if len(c.Segment(path).Turns) != 3 {
		t.Fatal("precondition: 3 turns")
	}

	// Replace with a shorter log at the same path.
	short := []Event{
		NewMeta("n1", "claude", "sonnet", "", "/tmp"),
		{T: "user", Time: "2026-08-01T00:00:00Z", Text: "fresh"},
	}
	// Write via temp + rename so size shrinks atomically.
	tmp := path + ".new"
	w := &Writer{Path: tmp}
	for _, ev := range short {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	assertProductsEqual(t, c, path, "after shorter replace")
	if got := c.Segment(path); len(got.Turns) != 1 || got.Turns[0].Text != "fresh" {
		t.Fatalf("after replace: %+v", got.Turns)
	}

	// Truncate to empty.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	assertProductsEqual(t, c, path, "after truncate")
	if got := c.Segment(path); len(got.Turns) != 0 {
		t.Fatalf("after truncate: %+v", got.Turns)
	}
}

// B7. Equivalence property: pseudo-random log from the full record-type
// alphabet, fed in K random append chunks; after every chunk all four
// products deep-equal a cold read.
func TestLogCache_EquivalenceProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	// Build a pool of records covering the alphabet.
	records := propertyAlphabet()
	// Shuffle into a sequence.
	seq := make([]Event, len(records))
	copy(seq, records)
	rng.Shuffle(len(seq), func(i, j int) { seq[i], seq[j] = seq[j], seq[i] })

	// Also force some ordering-sensitive shapes at the end:
	// seam-then-usage, usage-then-seam, asset-before-first-turn, whitespace
	// turns, malformed (via raw append), station after seam.
	forced := []Event{
		NewMeta("prop", "codex", "gpt", "", "/tmp"),
		{T: "asset", Time: "2026-01-01T00:00:00Z", Asset: &AssetEvent{
			ID: "a_early", Name: "early.png", Mime: "image/png", Size: 1,
			Storage: "inline", Bytes: "AQ==", SourcePath: "/tmp/early.png",
		}},
		{T: "user", Time: "2026-01-01T00:00:01Z", Text: "   "}, // whitespace-only
		{T: "user", Time: "2026-01-01T00:00:02Z", Text: "real"},
		{T: "usage", Time: "2026-01-01T00:00:03Z", Usage: &UsageEvent{InputTokens: 7, OutputTokens: 3}}, // no TurnID
		NewSource("/t/m.jsonl", "m"),                                                                    // empty reason
		{T: "usage", Time: "2026-01-01T00:00:04Z", Usage: &UsageEvent{InputTokens: 1, OutputTokens: 1, TurnID: "x"}},
		NewClearSource("clr"),
		{T: "station", Time: "2026-01-01T00:00:05Z", Station: &StationEvent{Seam: "clear-seam", Title: "S", Desc: "d"}},
		{T: "user", Time: "2026-01-01T00:00:06Z", Text: "after clear"},
		{T: "usage", Time: "2026-01-01T00:00:07Z", Usage: &UsageEvent{InputTokens: 2, OutputTokens: 2}}, // no-id after clear
	}
	seq = append(forced, seq...)

	path := filepath.Join(t.TempDir(), "prop.jsonl")
	// Ensure file exists.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &LogCache{}
	assertProductsEqual(t, c, path, "empty start")

	// Feed in K random chunks.
	i := 0
	chunkN := 0
	for i < len(seq) {
		n := 1 + rng.Intn(5)
		if i+n > len(seq) {
			n = len(seq) - i
		}
		w := &Writer{Path: path}
		for _, ev := range seq[i : i+n] {
			if err := w.Append(ev); err != nil {
				t.Fatal(err)
			}
		}
		// Occasionally inject a malformed line mid-file (complete line, skipped).
		if chunkN%7 == 3 {
			appendRaw(t, path, []byte("{not json at all}\n"))
		}
		// Occasionally inject whitespace-only complete line.
		if chunkN%11 == 5 {
			appendRaw(t, path, []byte("   \n"))
		}
		assertProductsEqual(t, c, path, fmt.Sprintf("chunk %d (records %d..%d)", chunkN, i, i+n-1))
		i += n
		chunkN++
	}
}

func propertyAlphabet() []Event {
	return []Event{
		{T: "user", Time: "2026-02-01T00:00:00Z", Text: "u1"},
		{T: "assistant", Time: "2026-02-01T00:00:01Z", Text: "a1"},
		{T: "usage", Time: "2026-02-01T00:00:02Z", Usage: &UsageEvent{InputTokens: 5, OutputTokens: 5, TurnID: "p1", CostAmount: 0.01, Model: "m"}},
		{T: "usage", Time: "2026-02-01T00:00:03Z", Usage: &UsageEvent{InputTokens: 3, OutputTokens: 3}}, // no TurnID
		{T: "usage", Time: "2026-02-01T00:00:04Z", Usage: &UsageEvent{Used: 100, Size: 200}},            // occupancy shell
		{T: "tool", Time: "2026-02-01T00:00:05Z", Tool: &ToolEvent{ID: "t1", Title: "Read", Status: "pending"}},
		{T: "tool", Time: "2026-02-01T00:00:06Z", Tool: &ToolEvent{ID: "t1", Title: "Read", Status: "completed"}},
		{T: "attention", Time: "2026-02-01T00:00:07Z", Attention: &AttentionEvent{Kind: "approval", Status: "start"}},
		{T: "attention", Time: "2026-02-01T00:00:08Z", Attention: &AttentionEvent{Kind: "approval", Status: "end"}},
		{T: "asset", Time: "2026-02-01T00:00:09Z", Asset: &AssetEvent{ID: "a_p", Name: "f.png", Mime: "image/png", Size: 2, Storage: "inline", Bytes: "AAA=", SourcePath: "/p/f.png"}},
		{T: "asset", Time: "2026-02-01T00:00:10Z", Asset: &AssetEvent{ID: "a_np", Name: "g.bin", Mime: "application/octet-stream", Size: 1, Storage: "blob", BlobPath: "x/g"}},
		NewClearSource("c1"),
		NewSource("/t/x.jsonl", "x"),
		{T: "station", Time: "2026-02-01T00:00:11Z", Station: &StationEvent{Seam: "s1", Title: "T", Desc: "D"}},
		{T: "stop", Time: "2026-02-01T00:00:12Z", StopReason: "end_turn"},
		{T: "error", Time: "2026-02-01T00:00:13Z", Error: "boom"},
		{T: "mark", Time: "2026-02-01T00:00:14Z", Mark: &MarkEvent{Off: 99}},
		{T: "user", Time: "2025-01-01T00:00:00Z", Text: "old-dated"}, // R20.9 backdating candidate
	}
}

// Same-size rewrite then grow: the size/mtime key misses the rewrite, but a
// later append must still equal a cold read of the rewritten body + append
// (prefix signature forces cold when the watermarked prefix changed).
func TestLogCache_SameSizeRewriteThenGrow(t *testing.T) {
	// One meta for both logs: NewMeta's RFC3339Nano stamp is not zero-padded,
	// so two independent headers can differ in length and flip which file is
	// longer. Shared header + shorter usage digits keeps alt pad-able.
	meta := NewMeta("n1", "codex", "gpt", "", "/tmp")
	path := writeLog(t, []Event{
		meta,
		{T: "usage", Time: "2026-07-15T09:00:00Z", Usage: &UsageEvent{
			InputTokens: 50, OutputTokens: 10, TurnID: "c1",
		}},
	})
	c := &LogCache{}
	f1, _ := c.FareAndRides(path)
	if f1.FreshIn != 50 || f1.Out != 10 {
		t.Fatalf("initial fare = %+v", f1)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	origSize, origMT := st.Size(), st.ModTime()

	// Rewrite with different totals, pad to same byte length, restore mtime.
	alt := writeLog(t, []Event{
		meta,
		{T: "usage", Time: "2026-07-15T09:00:00Z", Usage: &UsageEvent{
			InputTokens: 9, OutputTokens: 8, TurnID: "x",
		}},
	})
	altBytes, err := os.ReadFile(alt)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(altBytes)) > origSize {
		t.Fatalf("alt longer than original; cannot pad")
	}
	for int64(len(altBytes)) < origSize {
		altBytes = append(altBytes, ' ') // pad — whitespace-only line fragment is fine until \n
	}
	// Ensure we end with a newline so the file is well-formed JSONL + spaces.
	// Simpler: pad with a whitespace complete line.
	pad := make([]byte, int(origSize)-len(altBytes))
	for i := range pad {
		pad[i] = ' '
	}
	if len(pad) > 0 {
		pad[len(pad)-1] = '\n'
	}
	body := append(altBytes, pad...)
	if int64(len(body)) != origSize {
		// Rebuild: write alt then pad spaces to exact size with trailing newline in pad.
		body = altBytes
		for int64(len(body)) < origSize-1 {
			body = append(body, ' ')
		}
		if int64(len(body)) < origSize {
			body = append(body, '\n')
		}
		for int64(len(body)) < origSize {
			body = append(body, ' ')
		}
		body = body[:origSize]
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, origMT, origMT); err != nil {
		t.Fatal(err)
	}
	// Key unchanged → still stale 50/10.
	f2, _ := c.FareAndRides(path)
	if f2.FreshIn != 50 {
		t.Fatalf("same size/mtime should stay stale, got %+v", f2)
	}

	// Grow: append a turn. Must equal cold of true on-disk content.
	w := &Writer{Path: path}
	if err := w.Append(Event{T: "usage", Time: "2026-07-15T10:00:00Z", Usage: &UsageEvent{
		InputTokens: 1, OutputTokens: 2, TurnID: "c2",
	}}); err != nil {
		t.Fatal(err)
	}
	got, _ := c.FareAndRides(path)
	want := ReadFare(path)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after rewrite+grow:\n got = %+v\nwant = %+v", got, want)
	}
}

// B8. Allocation guard: incremental path allocates ≥10× less than cold for
// the same appended record. Pass/fail, not just a benchmark number.
func TestLogCache_AllocGuard(t *testing.T) {
	// Build a moderately large log so cold is expensive enough to measure.
	dir := t.TempDir()
	path := filepath.Join(dir, "alloc.jsonl")
	w := &Writer{Path: path}
	if err := w.Append(NewMeta("alloc", "claude", "sonnet", "", "/tmp")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		if err := w.Append(Event{T: "user", Time: "2026-07-15T09:00:00Z", Text: fmt.Sprintf("turn %d padding text here", i)}); err != nil {
			t.Fatal(err)
		}
		if err := w.Append(Event{T: "assistant", Time: "2026-07-15T09:00:01Z", Text: fmt.Sprintf("reply %d more padding for size", i)}); err != nil {
			t.Fatal(err)
		}
		if err := w.Append(Event{T: "usage", Time: "2026-07-15T09:00:02Z", Usage: &UsageEvent{
			InputTokens: 10, OutputTokens: 20, TurnID: fmt.Sprintf("t%d", i),
		}}); err != nil {
			t.Fatal(err)
		}
	}

	// Cold path allocs: fresh LogCache each time after an append (forces full parse).
	cold := testing.AllocsPerRun(20, func() {
		// Append outside would accumulate — instead measure cold parse of full file.
		c := &LogCache{}
		_ = c.Segment(path)
		_, _ = c.FareAndRides(path)
	})

	// Warm incremental cache, then measure allocs of one-append + re-read.
	c := &LogCache{}
	_ = c.Segment(path)
	var n int
	inc := testing.AllocsPerRun(20, func() {
		n++
		line, _ := json.Marshal(Event{T: "user", Time: "2026-08-01T00:00:00Z", Text: fmt.Sprintf("x%d", n)})
		line = append(line, '\n')
		// Best-effort append; ignore error in alloc loop.
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.Write(line)
			_ = f.Close()
		}
		_ = c.Segment(path)
		_, _ = c.FareAndRides(path)
	})

	if inc == 0 {
		t.Fatal("incremental AllocsPerRun = 0; spy/path not instrumented?")
	}
	if cold/inc < 10 {
		t.Fatalf("alloc guard: cold=%.0f inc=%.0f ratio=%.1f, want ≥10×", cold, inc, cold/inc)
	}
	t.Logf("alloc guard: cold=%.0f inc=%.0f ratio=%.1f×", cold, inc, cold/inc)
}

// Ensure fare.FareTotals is used so the import stays if tests change.
var _ = fare.FareTotals{}
