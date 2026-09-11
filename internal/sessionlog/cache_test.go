package sessionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"codeberg.org/chrberger/scimux/internal/transcript"
)

// Stage A tests for LogCache: one walk per (path, size, mtime) feeding all
// four poll products. Behaviour must match the standalone readers.

// richLog writes a log exercising every record type the four products care
// about: meta, user, assistant, clear source, mechanical source, usage with
// and without TurnID, station, asset with and without SourcePath.
func richLog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rich.jsonl")
	w := &Writer{Path: path}
	for _, ev := range []Event{
		NewMeta("n1", "claude", "sonnet", "", "/tmp"),
		{T: "user", Time: "2026-07-15T09:00:00Z", Text: "hello"},
		{T: "assistant", Time: "2026-07-15T09:00:01Z", Text: "hi there"},
		{T: "usage", Time: "2026-07-15T09:00:02Z", Usage: &UsageEvent{
			Used: 100, Size: 200_000,
			InputTokens: 10, OutputTokens: 20, TurnID: "t1", CostAmount: 0.01, Model: "sonnet",
		}},
		{T: "asset", Time: "2026-07-15T09:00:03Z", Asset: &AssetEvent{
			ID: "a_with", Name: "pic.png", Mime: "image/png", Size: 12,
			Storage: "inline", Bytes: "AAAA", SourceKind: "upload", SourcePath: "/tmp/pic.png",
		}},
		{T: "asset", Time: "2026-07-15T09:00:04Z", Asset: &AssetEvent{
			ID: "a_nopath", Name: "blob.bin", Mime: "application/octet-stream", Size: 4,
			Storage: "blob", BlobPath: "n1/a_nopath",
		}},
		NewClearSource("s2"),
		{T: "user", Time: "2026-07-15T10:00:00Z", Text: "after clear"},
		{T: "usage", Time: "2026-07-15T10:00:01Z", Usage: &UsageEvent{
			// no TurnID — seam-watermark path
			Used: 50, Size: 200_000, InputTokens: 5, OutputTokens: 6,
		}},
		{T: "station", Time: "2026-07-15T10:00:02Z", Station: &StationEvent{
			Seam: "2026-07-15T10:00:00Z", Title: "stop-1", Desc: "after clear",
		}},
		// Mechanical source (empty reason) — continues the clear-bounded epoch.
		NewSource("/t/s3.jsonl", "s3"),
		{T: "user", Time: "2026-07-15T11:00:00Z", Text: "after mech"},
		{T: "assistant", Time: "2026-07-15T11:00:01Z", Text: "ok"},
		{T: "usage", Time: "2026-07-15T11:00:02Z", Usage: &UsageEvent{
			Used: 60, Size: 200_000, InputTokens: 1, OutputTokens: 2, TurnID: "t2",
		}},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// A1. One walk per change. All four products share one parse; unchanged
// re-reads cost zero additional walks; an append costs exactly one more.
func TestLogCache_OneWalkPerChange(t *testing.T) {
	path := richLog(t)
	c := &LogCache{}

	// First round: all four products → one walk.
	_ = c.Segment(path)
	_, _ = c.FareAndRides(path)
	_ = c.AnchoredAssets(path)
	_ = c.Assets(path)
	if got := c.Walks(); got != 1 {
		t.Fatalf("after first four products: Walks() = %d, want 1", got)
	}

	// Unchanged: still one walk.
	_ = c.Segment(path)
	_, _ = c.FareAndRides(path)
	_ = c.AnchoredAssets(path)
	_ = c.Assets(path)
	if got := c.Walks(); got != 1 {
		t.Fatalf("after unchanged re-read: Walks() = %d, want 1", got)
	}

	// Append one record → second walk.
	w := &Writer{Path: path}
	if err := w.Append(Event{T: "user", Time: "2026-07-15T12:00:00Z", Text: "appended"}); err != nil {
		t.Fatal(err)
	}
	_ = c.Segment(path)
	_, _ = c.FareAndRides(path)
	_ = c.AnchoredAssets(path)
	_ = c.Assets(path)
	if got := c.Walks(); got != 2 {
		t.Fatalf("after append: Walks() = %d, want 2", got)
	}
}

// A2. Product equivalence: each cached product deep-equals the standalone
// path-taking function. Guard that Stage A changed nothing observable.
func TestLogCache_ProductEquivalence(t *testing.T) {
	path := richLog(t)
	c := &LogCache{}

	gotSeg := c.Segment(path)
	wantSeg := ReadSegment(path)
	if !reflect.DeepEqual(gotSeg, wantSeg) {
		t.Errorf("Segment mismatch:\n got = %+v\nwant = %+v", gotSeg, wantSeg)
	}

	gotFare, gotRides := c.FareAndRides(path)
	wantFare := ReadFare(path)
	wantRides := ReadRidesBySegment(path)
	if !reflect.DeepEqual(gotFare, wantFare) {
		t.Errorf("Fare mismatch:\n got = %+v\nwant = %+v", gotFare, wantFare)
	}
	if !reflect.DeepEqual(gotRides, wantRides) {
		t.Errorf("Rides mismatch:\n got = %+v\nwant = %+v", gotRides, wantRides)
	}

	gotAnch := c.AnchoredAssets(path)
	wantAnch := ReadAnchoredAssets(path)
	if !reflect.DeepEqual(gotAnch, wantAnch) {
		t.Errorf("AnchoredAssets mismatch:\n got = %+v\nwant = %+v", gotAnch, wantAnch)
	}

	gotAssets := c.Assets(path)
	wantAssets := ReadAssets(path)
	if !reflect.DeepEqual(gotAssets, wantAssets) {
		t.Errorf("Assets mismatch:\n got = %+v\nwant = %+v", gotAssets, wantAssets)
	}
}

// A4. Missing / unreadable / empty log yields the same defensive zero values
// every current caller expects. Absent is not zero; missing must never error.
func TestLogCache_MissingEmptyDefensive(t *testing.T) {
	c := &LogCache{}

	// Missing path.
	missing := filepath.Join(t.TempDir(), "no-such.jsonl")
	seg := c.Segment(missing)
	if seg.Turns == nil || len(seg.Turns) != 0 {
		t.Errorf("missing Segment.Turns = %v, want empty non-nil slice", seg.Turns)
	}
	f, rides := c.FareAndRides(missing)
	if f.Turns != 0 || !f.ReportedCostComplete {
		t.Errorf("missing fare = %+v, want Turns==0 complete", f)
	}
	if rides != nil {
		t.Errorf("missing rides = %v, want nil", rides)
	}
	if a := c.AnchoredAssets(missing); a != nil {
		t.Errorf("missing anchored = %v, want nil", a)
	}
	// ReadAssets always returns a non-nil map (even empty); match that.
	if m := c.Assets(missing); m == nil || len(m) != 0 {
		t.Errorf("missing assets = %v, want empty non-nil map", m)
	}
	if got := c.Walks(); got != 0 {
		t.Errorf("missing file Walks() = %d, want 0 (nothing to parse)", got)
	}

	// Empty file (exists, zero bytes).
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c2 := &LogCache{}
	seg = c2.Segment(empty)
	if !reflect.DeepEqual(seg, ReadSegment(empty)) {
		t.Errorf("empty Segment = %+v, want cold %+v", seg, ReadSegment(empty))
	}
	f, rides = c2.FareAndRides(empty)
	wantRide, wantRides := foldRides(empty)
	if !reflect.DeepEqual(f, wantRide.Totals) || !reflect.DeepEqual(rides, wantRides) {
		t.Errorf("empty fare/rides = (%+v, %v), want (%+v, %v)", f, rides, wantRide.Totals, wantRides)
	}
	if a := c2.AnchoredAssets(empty); !reflect.DeepEqual(a, ReadAnchoredAssets(empty)) {
		t.Errorf("empty anchored = %v, want cold %v", a, ReadAnchoredAssets(empty))
	}
	if m := c2.Assets(empty); !reflect.DeepEqual(m, ReadAssets(empty)) {
		t.Errorf("empty assets = %v, want cold %v", m, ReadAssets(empty))
	}

	// Cross-check: cold readers agree on missing.
	if !reflect.DeepEqual(ReadSegment(missing).Turns, []transcript.Turn{}) &&
		len(ReadSegment(missing).Turns) != 0 {
		t.Error("cold ReadSegment(missing) unexpected")
	}
	if cold := ReadFare(missing); cold.Turns != 0 {
		t.Errorf("cold ReadFare(missing).Turns = %d", cold.Turns)
	}
}

func TestLogCacheSegmentMatchesColdForProvAndAgent(t *testing.T) {
	prov1 := synthProvJSON("cache-initial")
	path := writeLog(t, []Event{
		NewMeta("n1", "codex", "", "", "/tmp"),
		{T: "user", Time: "2026-07-15T09:00:00Z", Text: "hello"},
		{T: "assistant", Time: "2026-07-15T09:00:01Z", Text: "hi there", Prov: prov1},
	})
	c := &LogCache{}
	got := c.Segment(path)
	want := ReadSegment(path)
	if !reflect.DeepEqual(got.Turns, want.Turns) {
		t.Fatalf("initial scan mismatch:\n got = %+v\nwant = %+v", got.Turns, want.Turns)
	}
	if len(got.Turns) != 2 || got.Turns[1].Agent != "codex" {
		t.Fatalf("initial agent = %+v", got.Turns)
	}
	assertRawEqual(t, got.Turns[1].Prov, prov1)

	prov2 := synthProvJSON("cache-append")
	w := &Writer{Path: path}
	if err := w.Append(Event{T: "assistant", Time: "2026-07-15T09:00:02Z", Text: "more", Prov: prov2}); err != nil {
		t.Fatal(err)
	}
	got = c.Segment(path)
	want = ReadSegment(path)
	if !reflect.DeepEqual(got.Turns, want.Turns) {
		t.Fatalf("incremental append mismatch:\n got = %+v\nwant = %+v", got.Turns, want.Turns)
	}
	if got.Turns[2].Agent != "codex" {
		t.Errorf("appended Agent = %q", got.Turns[2].Agent)
	}
	assertRawEqual(t, got.Turns[2].Prov, prov2)

	// Replacement shorter than watermark forces a cold rebuild.
	short := writeLog(t, []Event{
		NewMeta("n1", "codex", "", "", "/tmp"),
		{T: "assistant", Time: "2026-07-15T09:00:00Z", Text: "rebuilt", Prov: synthProvJSON("cache-rebuild")},
	})
	body, err := os.ReadFile(short)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got = c.Segment(path)
	want = ReadSegment(path)
	if !reflect.DeepEqual(got.Turns, want.Turns) {
		t.Fatalf("rebuild mismatch:\n got = %+v\nwant = %+v", got.Turns, want.Turns)
	}
	if len(got.Turns) != 1 || got.Turns[0].Agent != "codex" || got.Turns[0].Text != "rebuilt" {
		t.Fatalf("rebuild turns = %+v", got.Turns)
	}
	assertRawEqual(t, got.Turns[0].Prov, synthProvJSON("cache-rebuild"))
}

func TestLogCacheSegmentProvIsCallerPrivate(t *testing.T) {
	prov := json.RawMessage(`[{"loc":"message","key":"_meta","v":{"src":"cache-leak","kids":[1,{"n":2}]}},{"loc":"envelope","key":"provenance","v":null}]`)
	saved := append(json.RawMessage(nil), prov...)
	path := writeLog(t, []Event{
		NewMeta("n1", "claude", "", "", "/tmp"),
		{T: "assistant", Time: "2026-07-15T09:00:01Z", Text: "visible", Prov: prov},
	})
	c := &LogCache{}
	first := c.Segment(path)
	if len(first.Turns) != 1 || first.Turns[0].Text != "visible" {
		t.Fatalf("first segment = %+v", first.Turns)
	}
	if len(first.Turns[0].Prov) == 0 {
		t.Fatal("first Segment dropped provenance")
	}
	for i := range first.Turns[0].Prov {
		first.Turns[0].Prov[i] = 'X'
	}
	second := c.Segment(path)
	if len(second.Turns) != 1 {
		t.Fatalf("second segment = %+v", second.Turns)
	}
	assertRawEqual(t, second.Turns[0].Prov, saved)
	cold := ReadSegment(path)
	if len(cold.Turns) != 1 {
		t.Fatalf("cold segment = %+v", cold.Turns)
	}
	assertRawEqual(t, second.Turns[0].Prov, cold.Turns[0].Prov)
	for i := range second.Turns[0].Prov {
		second.Turns[0].Prov[i] = 'Y'
	}
	third := c.Segment(path)
	assertRawEqual(t, third.Turns[0].Prov, saved)
}
