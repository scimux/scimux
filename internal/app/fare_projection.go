package app

// fare_projection.go — whole-journey fare onto the /api/state poll payload
// (fare-design.md Phase 7, D1, D8). Cached fold-on-growth mirrors segCache:
// ReadFare only re-runs when the session log's size or mtime advances.
// Occupancy (ctx_pct) stays on the segment path and is never merged with fare.

import (
	"codeberg.org/chrberger/scimux/internal/fare"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// fare returns the node's whole-journey FareTotals, cache-backed so an idle
// poll is a stat rather than a full-log fold. Defensive: bare test apps and
// missing logs yield Turns==0 (projector omits fare fields — absent ≠ zero).
func (a *app) fare(n *Node) fare.FareTotals {
	if a.sessionsDir == "" {
		return fare.FareTotals{ReportedCostComplete: true}
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
	return c.Fare(a.sessionLogPath(n.ID))
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
