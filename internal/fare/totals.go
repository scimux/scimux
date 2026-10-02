package fare

// FareTotals is the journey (or segment) token-meter fold: the four canonical
// billable quantities summed across counted turns, plus optional reported cost
// and per-model breakdown. See fare-design.md §4.2, D1.
//
// Occupancy (used/size) is intentionally not here — it is a tank, not a meter.
type FareTotals struct {
	FreshIn, CacheRead, CacheWrite, Out int
	UnsplitIn                           int // input with no reported fresh/cache split
	Turns                               int
	ReportedCostUSD                     float64
	ReportedCostComplete                bool // true iff every counted turn reported cost
	PerModel                            map[string]FareTotals
}

// Total is the sum of the canonical billable quantities plus unsplit input.
// Never includes occupancy (Used) — D1. Agents that report a split leave
// UnsplitIn at zero, so their total is unchanged.
func (f FareTotals) Total() int {
	return f.FreshIn + f.CacheRead + f.CacheWrite + f.Out + f.UnsplitIn
}

// Occupancy is the context tank (used/size): latest level, segment-scoped.
// Distinct from FareTotals so the two gauges cannot be conflated (D1).
type Occupancy struct {
	Used, Size int
}
