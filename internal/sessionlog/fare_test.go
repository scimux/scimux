package sessionlog

import (
	"path/filepath"
	"testing"

	"codeberg.org/chrberger/scimux/internal/fare"
)

// Phase 2 fold readers (fare-design.md §4.2, §4.3, D2, D3). Fixtures are
// synthetic and committed under testdata/fare/ — never real corpus content.

func fareFixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", "fare", name)
}

func TestReadFare_SumsCanonicalPerAgent(t *testing.T) {
	// Claude: input is fresh-only (direct). Two turns:
	//   t1: in=2, out=410, cacheWrite=25437
	//   t2: in=100, out=50, cacheWrite=200
	// → FreshIn=102, CacheWrite=25637, Out=460
	claude := ReadFare(fareFixture(t, "claude-sum.jsonl"))
	if claude.FreshIn != 102 {
		t.Errorf("claude FreshIn = %d, want 102 (direct sum)", claude.FreshIn)
	}
	if claude.CacheWrite != 25637 {
		t.Errorf("claude CacheWrite = %d, want 25637", claude.CacheWrite)
	}
	if claude.Out != 460 {
		t.Errorf("claude Out = %d, want 460", claude.Out)
	}
	if claude.CacheRead != 0 {
		t.Errorf("claude CacheRead = %d, want 0", claude.CacheRead)
	}
	if claude.Turns != 2 {
		t.Errorf("claude Turns = %d, want 2", claude.Turns)
	}
	if claude.Total() != 102+25637+460 {
		t.Errorf("claude Total() = %d, want %d", claude.Total(), 102+25637+460)
	}

	// Grok: input is whole-prompt → Normalize subtracts cache_read.
	//   t1: in=1000, cacheRead=300, out=50 → fresh=700
	//   t2: in=500,  cacheRead=100, out=25 → fresh=400
	// → FreshIn=1100, CacheRead=400, Out=75
	grok := ReadFare(fareFixture(t, "grok-sum.jsonl"))
	if grok.FreshIn != 1100 {
		t.Errorf("grok FreshIn = %d, want 1100 (1000−300)+(500−100)", grok.FreshIn)
	}
	if grok.CacheRead != 400 {
		t.Errorf("grok CacheRead = %d, want 400", grok.CacheRead)
	}
	if grok.Out != 75 {
		t.Errorf("grok Out = %d, want 75", grok.Out)
	}
	if grok.Turns != 2 {
		t.Errorf("grok Turns = %d, want 2", grok.Turns)
	}
	if grok.Total() != 1100+400+75 {
		t.Errorf("grok Total() = %d, want %d", grok.Total(), 1100+400+75)
	}
}

func TestReadFare_NeverSumsOccupancy(t *testing.T) {
	// Records carry large Used/Size (occupancy levels) plus small breakdowns.
	// Total() must reflect only the four canonical quantities, never Used.
	got := ReadFare(fareFixture(t, "occupancy-not-summed.jsonl"))
	// codex subtract: input is whole-prompt but cacheRead=0 → fresh = input
	// turns: (10+5) + (20+10) = 45
	want := 45
	if got.Total() != want {
		t.Errorf("Total() = %d, want %d (Used must not be summed)", got.Total(), want)
	}
	if got.FreshIn+got.CacheRead+got.CacheWrite+got.Out != want {
		t.Errorf("canonical parts = %d+%d+%d+%d, want sum %d",
			got.FreshIn, got.CacheRead, got.CacheWrite, got.Out, want)
	}
	// Guard against a buggy Total() that added Used: fixture Used values are
	// 50000 and 90000 — any inclusion would push Total well above 45.
	if got.Total() > 1000 {
		t.Errorf("Total() = %d looks like Used leaked into the fare sum", got.Total())
	}
}

func TestReadFare_DedupsByTurnID(t *testing.T) {
	// same-id appears twice; other-id once → two counted turns, not three.
	got := ReadFare(fareFixture(t, "dedup-turnid.jsonl"))
	if got.Turns != 2 {
		t.Errorf("Turns = %d, want 2 (same TurnID counted once)", got.Turns)
	}
	// first same-id (100+10) + other (50+5) = 165
	if got.Total() != 165 {
		t.Errorf("Total() = %d, want 165", got.Total())
	}
	if got.FreshIn != 150 || got.Out != 15 {
		t.Errorf("FreshIn/Out = %d/%d, want 150/15", got.FreshIn, got.Out)
	}
}

// Flagship: a rotation source seam followed by turns 0..n re-mirrored with the
// *same* TurnIDs must yield a single-pass sum, not a doubled one (D3).
func TestReadFare_RotationReplayNoDoubleCount(t *testing.T) {
	// Pre-rotation: t0(100+10), t1(200+20)
	// Post-rotation re-mirror: t0, t1 again + new t2(50+5)
	// Correct journey: t0+t1+t2 = 100+10+200+20+50+5 = 385, Turns=3
	// Naive sum would be 385+300 = 685 (or Turns=5).
	got := ReadFare(fareFixture(t, "rotation-replay.jsonl"))
	if got.Turns != 3 {
		t.Errorf("Turns = %d, want 3 (t0,t1 once each + t2); double-count would be 5", got.Turns)
	}
	if got.Total() != 385 {
		t.Errorf("Total() = %d, want 385; double-count of t0+t1 would be 685", got.Total())
	}
	if got.FreshIn != 350 || got.Out != 35 {
		t.Errorf("FreshIn/Out = %d/%d, want 350/35", got.FreshIn, got.Out)
	}
}

func TestReadFare_SeamFallbackWhenNoTurnID(t *testing.T) {
	// No TurnIDs: mechanical rotation re-mirrors the same usage. Dedup falls
	// back to the source-seam watermark (D3) — not naive summing.
	got := ReadFare(fareFixture(t, "seam-fallback.jsonl"))
	if got.Turns != 1 {
		t.Errorf("Turns = %d, want 1 (seam watermark; naive would be 2)", got.Turns)
	}
	if got.Total() != 110 {
		t.Errorf("Total() = %d, want 110 (100+10 once); naive double would be 220", got.Total())
	}
}

func TestReadFareBySegment_SplitsOnSourceSeams(t *testing.T) {
	// segments.jsonl:
	//   source → seg-a (100+10)
	//   clear  → seg-b (200+20)
	//   rotation re-mirror of seg-b + seg-c (50+5); seg-b TurnID deduped
	// Whole journey: a+b+c = 110+220+55 = 385
	path := fareFixture(t, "segments.jsonl")
	whole := ReadFare(path)
	segs := ReadFareBySegment(path)
	if len(segs) < 2 {
		t.Fatalf("ReadFareBySegment returned %d segments, want ≥2 (split on source seams)", len(segs))
	}
	// Segments must reconcile to the whole-journey total (dedup consistent).
	segSum := 0
	segTurns := 0
	for i, s := range segs {
		segSum += s.Total()
		segTurns += s.Turns
		t.Logf("segment[%d] Total=%d Turns=%d FreshIn=%d Out=%d", i, s.Total(), s.Turns, s.FreshIn, s.Out)
	}
	if segSum != whole.Total() {
		t.Errorf("sum of segment Totals() = %d, ReadFare().Total() = %d — must reconcile", segSum, whole.Total())
	}
	if segTurns != whole.Turns {
		t.Errorf("sum of segment Turns = %d, ReadFare().Turns = %d — must reconcile", segTurns, whole.Turns)
	}
	if whole.Total() != 385 {
		t.Errorf("whole Total() = %d, want 385 (a+b+c after dedup)", whole.Total())
	}
	if whole.Turns != 3 {
		t.Errorf("whole Turns = %d, want 3", whole.Turns)
	}
}

func TestReadFare_SkipsOpencodeOccupancyShells(t *testing.T) {
	// Occupancy shell {used,size,costCurrency} and empty {} contribute nothing.
	// Only the breakdown record is counted (opencode is direct for fresh).
	got := ReadFare(fareFixture(t, "opencode-shells.jsonl"))
	if got.Turns != 1 {
		t.Errorf("Turns = %d, want 1 (shells/empties skipped)", got.Turns)
	}
	if got.FreshIn != 100 {
		t.Errorf("FreshIn = %d, want 100 (direct, not subtract)", got.FreshIn)
	}
	if got.CacheRead != 50 {
		t.Errorf("CacheRead = %d, want 50", got.CacheRead)
	}
	if got.Out != 20 {
		t.Errorf("Out = %d, want 20", got.Out)
	}
	if got.Total() != 170 {
		t.Errorf("Total() = %d, want 170", got.Total())
	}
}

func TestReadFare_TotalTokenFallback(t *testing.T) {
	// {totalTokens:16209}-only (no split) must not be dropped: attribute via
	// total-token fallback (missing→out when out==0).
	got := ReadFare(fareFixture(t, "total-token-fallback.jsonl"))
	if got.Turns != 1 {
		t.Errorf("Turns = %d, want 1 (total-only record is billable)", got.Turns)
	}
	if got.Out != 16209 {
		t.Errorf("Out = %d, want 16209 (total→out fallback)", got.Out)
	}
	if got.FreshIn != 0 || got.CacheRead != 0 || got.CacheWrite != 0 {
		t.Errorf("non-out fields = %d/%d/%d, want 0/0/0", got.FreshIn, got.CacheRead, got.CacheWrite)
	}
	if got.Total() != 16209 {
		t.Errorf("Total() = %d, want 16209", got.Total())
	}
}

func TestReadFare_ReportedCostCompleteFlag(t *testing.T) {
	// Every counted turn carried cost → complete.
	complete := ReadFare(fareFixture(t, "cost-complete.jsonl"))
	if !complete.ReportedCostComplete {
		t.Error("ReportedCostComplete = false, want true (every turn had cost)")
	}
	if complete.ReportedCostUSD != 0.75 {
		t.Errorf("ReportedCostUSD = %v, want 0.75", complete.ReportedCostUSD)
	}
	if complete.Turns != 2 {
		t.Errorf("Turns = %d, want 2", complete.Turns)
	}

	// One cost-less counted turn flips the flag false; USD still sums present ones.
	incomplete := ReadFare(fareFixture(t, "cost-incomplete.jsonl"))
	if incomplete.ReportedCostComplete {
		t.Error("ReportedCostComplete = true, want false (one turn lacked cost; absent≠zero)")
	}
	if incomplete.ReportedCostUSD != 0.5 {
		t.Errorf("ReportedCostUSD = %v, want 0.5 (only the present cost summed)", incomplete.ReportedCostUSD)
	}
	if incomplete.Turns != 2 {
		t.Errorf("Turns = %d, want 2", incomplete.Turns)
	}
}

// Compile-time shape checks: FareTotals/Occupancy live in fare (leaf), and
// Total() is the four-quantity sum (D1 — never Used).
func TestFareTypes_LiveInFarePackage(t *testing.T) {
	var f fare.FareTotals
	_ = f.Total()
	var _ fare.Occupancy
}

// Fare and Rides are thin wrappers over FareAndRides; exercise the actual
// returned computation (not just "does not panic").
func TestFareCache_FareAndRidesWrappers(t *testing.T) {
	path := fareFixture(t, "claude-sum.jsonl")
	var c FareCache
	wantF, wantR := c.FareAndRides(path)
	gotF := c.Fare(path)
	if gotF.FreshIn != wantF.FreshIn || gotF.Out != wantF.Out || gotF.Turns != wantF.Turns {
		t.Errorf("Fare FreshIn/Out/Turns = %d/%d/%d, want %d/%d/%d",
			gotF.FreshIn, gotF.Out, gotF.Turns, wantF.FreshIn, wantF.Out, wantF.Turns)
	}
	if gotF.FreshIn != 102 || gotF.Out != 460 || gotF.Turns != 2 {
		t.Errorf("Fare FreshIn/Out/Turns = %d/%d/%d, want 102/460/2", gotF.FreshIn, gotF.Out, gotF.Turns)
	}
	gotR := c.Rides(path)
	if len(gotR) != len(wantR) {
		t.Fatalf("Rides len = %d, want %d", len(gotR), len(wantR))
	}
	for i := range wantR {
		if gotR[i].Totals.Turns != wantR[i].Totals.Turns || gotR[i].Real != wantR[i].Real {
			t.Errorf("Rides[%d] Turns/Real = %d/%v, want %d/%v",
				i, gotR[i].Totals.Turns, gotR[i].Real, wantR[i].Totals.Turns, wantR[i].Real)
		}
	}
}
