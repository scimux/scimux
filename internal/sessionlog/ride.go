package sessionlog

import (
	"codeberg.org/chrberger/scimux/internal/fare"
)

// STUBS for V2-P2 red commit — real fold lands in the green commit.

func ReadRidesBySegment(path string) []fare.Ride { return nil }
func ReadRide(path string) fare.Ride {
	return fare.Ride{Totals: fare.FareTotals{ReportedCostComplete: true}}
}

// foldFareFromEvents keeps ReadFare working during the red phase.
func foldFareFromEvents(evs []Event) (fare.FareTotals, []fare.FareTotals) {
	agent, hits, openReasons := collectFareHits(evs)
	nSeg := len(openReasons)
	if nSeg == 0 {
		z := fare.FareTotals{ReportedCostComplete: true}
		return z, nil
	}
	count := markCounted(hits, openReasons)
	segs := make([]fare.FareTotals, nSeg)
	for i := range segs {
		segs[i].ReportedCostComplete = true
	}
	whole := fare.FareTotals{ReportedCostComplete: true}
	for i, h := range hits {
		if !count[i] {
			continue
		}
		raw := rawFromUsage(h.u)
		can := fare.Normalize(agent, raw)
		addCanonical(&whole, can, h.u)
		addCanonical(&segs[h.segIdx], can, h.u)
	}
	return whole, segs
}
