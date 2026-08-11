package app

// fare_projection.go — whole-journey fare + per-segment rides onto the
// /api/state poll payload (fare-design.md Phase 7 + V2-P2, D1, D8). Cached
// fold-on-growth mirrors segCache: ReadFare/ReadRidesBySegment only re-run
// when the session log's size or mtime advances. Occupancy (ctx_pct) stays
// on the segment path and is never merged with fare. UI: wall heat (V2-P3)
// + selected-segment capsule/callout (V2-P4); v1 fareLineHTML is retired.

import (
	"codeberg.org/chrberger/scimux/internal/fare"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// fareAndRides returns whole-journey totals and per-segment rides (V2-P2).
func (a *app) fareAndRides(n *Node) (fare.FareTotals, []fare.Ride) {
	if a.sessionsDir == "" {
		return fare.FareTotals{ReportedCostComplete: true}, nil
	}
	a.mu.Lock()
	if a.fareCache == nil {
		a.fareCache = map[string]*sessionlog.FareCache{}
	}
	c := a.fareCache[n.ID]
	if c == nil {
		c = &sessionlog.FareCache{}
		a.fareCache[n.ID] = c
	}
	a.mu.Unlock()
	return c.FareAndRides(a.sessionLogPath(n.ID))
}

// fareCacheFolds is the test spy seam for fold-on-growth (Phase 7).
func fareCacheFolds(a *app, id string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fareCache == nil {
		return 0
	}
	c := a.fareCache[id]
	if c == nil {
		return 0
	}
	return c.Folds()
}

// applyFare projects whole-journey fare onto the state node view.
// Turns==0 → leave fields unset (omit from JSON). Never writes zeros for an
// empty ride. fare_cost is always paired with fare_cost_complete (D8).
func applyFare(v *nodeView, f fare.FareTotals) {
	if f.Turns == 0 {
		return
	}
	fi, cr, cw, out := f.FreshIn, f.CacheRead, f.CacheWrite, f.Out
	total, turns := f.Total(), f.Turns
	cost, complete := f.ReportedCostUSD, f.ReportedCostComplete
	v.FareFreshIn = &fi
	v.FareCacheRead = &cr
	v.FareCacheWrite = &cw
	v.FareOut = &out
	v.FareTotal = &total
	v.FareTurns = &turns
	v.FareCost = &cost
	v.FareCostComplete = &complete
	if m := dominantFareModel(f); m != "" {
		v.FareModel = m
	}
}

// fareSegView is one segment's projected ride for the poll payload (V2-P2).
// Time fields are milliseconds. agent/tools/wait are omitted when HasSplit
// is false (absent ≠ zero — historical / complete-only transports).
type fareSegView struct {
	FreshIn      int      `json:"fresh_in"`
	CacheRead    int      `json:"cache_read"`
	CacheWrite   int      `json:"cache_write"`
	Out          int      `json:"out"`
	Total        int      `json:"total"`
	Turns        int      `json:"turns"`
	Cost         *float64 `json:"cost,omitempty"`
	CostComplete *bool    `json:"cost_complete,omitempty"`
	RealMS       int64    `json:"real_ms"`
	AgentMS      *int64   `json:"agent_ms,omitempty"`
	ToolsMS      *int64   `json:"tools_ms,omitempty"`
	WaitMS       *int64   `json:"wait_ms,omitempty"`
}

// applyFareRides projects per-segment rides. Segments with neither tokens nor
// real duration are dropped (empty pre-source shells). Cost only when the
// segment's ReportedCostComplete is meaningful (turns > 0); D8 still applies
// at the whole-journey level for fare_cost.
func applyFareRides(v *nodeView, rides []fare.Ride) {
	if len(rides) == 0 {
		return
	}
	out := make([]fareSegView, 0, len(rides))
	for _, r := range rides {
		if r.Totals.Turns == 0 && r.Real == 0 {
			continue
		}
		seg := fareSegView{
			FreshIn:    r.Totals.FreshIn,
			CacheRead:  r.Totals.CacheRead,
			CacheWrite: r.Totals.CacheWrite,
			Out:        r.Totals.Out,
			Total:      r.Totals.Total(),
			Turns:      r.Totals.Turns,
			RealMS:     r.Real.Milliseconds(),
		}
		if r.Totals.Turns > 0 {
			cost, complete := r.Totals.ReportedCostUSD, r.Totals.ReportedCostComplete
			seg.Cost = &cost
			seg.CostComplete = &complete
		}
		if r.HasSplit {
			a, t, w := r.Agent.Milliseconds(), r.Tools.Milliseconds(), r.Wait.Milliseconds()
			seg.AgentMS = &a
			seg.ToolsMS = &t
			seg.WaitMS = &w
		}
		out = append(out, seg)
	}
	if len(out) > 0 {
		v.FareSegments = out
	}
}

// dominantFareModel picks the model with the most counted turns (stable tie:
// lexicographically greater name). Empty when no turn carried a Model.
func dominantFareModel(f fare.FareTotals) string {
	best, bestTurns := "", 0
	for m, pm := range f.PerModel {
		if pm.Turns > bestTurns || (pm.Turns == bestTurns && m > best) {
			best, bestTurns = m, pm.Turns
		}
	}
	return best
}
