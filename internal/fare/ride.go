package fare

import (
	"sort"
	"time"
)

// Ride is one journey segment's combined cost: token meter (FareTotals) plus
// the wall-clock time model (fare-design.md v2.1, V2-P2).
//
// Time is a partition of Real (agent + tools + wait ≡ real) when HasSplit is
// true. Agent is the residual Real − Tools − Wait, clamped ≥0 — never a
// measurement that can exceed the whole (v2-D1).
//
// HasSplit is false when the segment lacks recoverable tool/wait timing
// (complete-only tool stamps, pre-V2-P2 historical logs, empty surfaces):
// only Real is meaningful then; Agent/Tools/Wait are absent, not zero.
type Ride struct {
	Totals   FareTotals
	Real     time.Duration
	Agent    time.Duration
	Tools    time.Duration
	Wait     time.Duration
	HasSplit bool
}

// Interval is a half-open wall-clock bracket [Start, End) for union math.
// End must be strictly after Start to contribute duration.
type Interval struct {
	Start, End time.Time
}

// UnionDuration merges overlapping/adjacent intervals and returns the total
// covered length. Parallel tools (or concurrent wait edges) must not
// double-count — this is the tools/wait side of v2-D1.
func UnionDuration(ivs []Interval) time.Duration {
	if len(ivs) == 0 {
		return 0
	}
	sorted := make([]Interval, 0, len(ivs))
	for _, iv := range ivs {
		if iv.Start.IsZero() || iv.End.IsZero() || !iv.End.After(iv.Start) {
			continue
		}
		sorted = append(sorted, iv)
	}
	if len(sorted) == 0 {
		return 0
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Start.Before(sorted[j].Start)
	})
	cur := sorted[0]
	var total time.Duration
	for _, iv := range sorted[1:] {
		if !iv.Start.After(cur.End) {
			// Overlap or touch: extend.
			if iv.End.After(cur.End) {
				cur.End = iv.End
			}
			continue
		}
		total += cur.End.Sub(cur.Start)
		cur = iv
	}
	total += cur.End.Sub(cur.Start)
	return total
}

// ResidualAgent is Real − Tools − Wait, clamped at zero so the three parts
// always partition Real (agent+tools+wait ≡ real) even when the measured
// unions overlap or slightly overshoot due to timestamp granularity.
func ResidualAgent(real, tools, wait time.Duration) time.Duration {
	if real < 0 {
		real = 0
	}
	if tools < 0 {
		tools = 0
	}
	if wait < 0 {
		wait = 0
	}
	a := real - tools - wait
	if a < 0 {
		return 0
	}
	return a
}

// BuildRide assembles a Ride from pre-folded totals and measured intervals.
// hasToolTiming / hasWaitTiming gate the split: either is enough to project
// agent·tools·wait (missing side contributes 0); neither → Real only.
func BuildRide(totals FareTotals, real time.Duration, toolIvs, waitIvs []Interval, hasToolTiming, hasWaitTiming bool) Ride {
	r := Ride{Totals: totals, Real: real}
	if real < 0 {
		r.Real = 0
	}
	if !hasToolTiming && !hasWaitTiming {
		return r
	}
	r.HasSplit = true
	if hasToolTiming {
		r.Tools = UnionDuration(toolIvs)
	}
	if hasWaitTiming {
		r.Wait = UnionDuration(waitIvs)
	}
	r.Agent = ResidualAgent(r.Real, r.Tools, r.Wait)
	return r
}
