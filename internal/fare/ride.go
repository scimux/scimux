package fare

import "time"

// Ride is one journey segment's combined cost (fare-design.md v2.1, V2-P2).
// STUB for red commit — replaced by the green implementation.
type Ride struct {
	Totals   FareTotals
	Real     time.Duration
	Agent    time.Duration
	Tools    time.Duration
	Wait     time.Duration
	HasSplit bool
}

// Interval is a half-open wall-clock bracket [Start, End).
type Interval struct {
	Start, End time.Time
}

// UnionDuration STUB — always 0 so overlapping-tool tests fail (red).
func UnionDuration(ivs []Interval) time.Duration { return 0 }

// ResidualAgent STUB — always 0 so partition tests fail (red).
func ResidualAgent(real, tools, wait time.Duration) time.Duration { return 0 }

// BuildRide STUB — never sets HasSplit / split parts (red).
func BuildRide(totals FareTotals, real time.Duration, toolIvs, waitIvs []Interval, hasToolTiming, hasWaitTiming bool) Ride {
	return Ride{Totals: totals, Real: real}
}
