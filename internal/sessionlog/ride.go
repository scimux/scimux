package sessionlog

import (
	"strings"
	"time"

	"codeberg.org/chrberger/scimux/internal/fare"
)

// ReadRidesBySegment folds per-segment token fare + the v2 time model into
// one Ride per source-delimited segment (fare-design.md V2-P2).
//
// Real = first→last Event.Time in the segment.
// Tools = union of closed tool call→result intervals (pending/start →
// completed/failed per tool.id). Complete-only stamps form no interval
// (absent tools timing — never fabricated).
// Wait = union of closed attention start→end edges.
// Agent = residual Real − Tools − Wait (clamped ≥0).
//
// HasSplit is true only when the segment carries V2-P2-quality timing
// (closed tool intervals and/or attention edges). Pre-V2-P2 / historical
// segments with neither render Real-only (absent ≠ zero for the split).
func ReadRidesBySegment(path string) []fare.Ride {
	_, rides := foldRides(path)
	return rides
}

// ReadRide folds the whole journey into one Ride (segment-agnostic time
// model over the entire file). Token totals match ReadFare.
func ReadRide(path string) fare.Ride {
	whole, _ := foldRides(path)
	return whole
}

func foldRides(path string) (fare.Ride, []fare.Ride) {
	return foldRidesFromEvents(ReadEvents(path))
}

// foldRidesFromEvents is the pure core of foldRides; LogCache calls it on the
// single in-memory event slice shared with the other poll products.
func foldRidesFromEvents(evs []Event) (fare.Ride, []fare.Ride) {
	totals, segs := foldFareFromEvents(evs)
	nSeg := len(segs)
	if nSeg == 0 {
		z := fare.Ride{Totals: fare.FareTotals{ReportedCostComplete: true}}
		return z, nil
	}

	// Collect per-segment time material in one pass.
	type segTime struct {
		first, last  time.Time
		hasFirst     bool
		toolStarts   map[string]time.Time // id → first start
		toolEnds     map[string]time.Time // id → first terminal
		hasToolStart bool                 // any non-terminal tool row
		toolIvs      []fare.Interval
		waitOpen     time.Time
		waitOpenSet  bool
		waitIvs      []fare.Interval
		hasWaitEdge  bool // any attention record
	}
	st := make([]segTime, nSeg)
	for i := range st {
		st[i].toolStarts = map[string]time.Time{}
		st[i].toolEnds = map[string]time.Time{}
	}

	openReasons := []string{""}
	segIdx := 0
	for _, ev := range evs {
		switch ev.T {
		case "source":
			reason := ""
			if ev.Source != nil {
				reason = ev.Source.Reason
			}
			openReasons = append(openReasons, reason)
			segIdx = len(openReasons) - 1
			if segIdx >= len(st) {
				// foldFareFromEvents and this walk must agree on segment count;
				// grow defensively if a log is read mid-write.
				for len(st) <= segIdx {
					st = append(st, segTime{
						toolStarts: map[string]time.Time{},
						toolEnds:   map[string]time.Time{},
					})
				}
			}
		}
		if segIdx < 0 || segIdx >= len(st) {
			continue
		}
		s := &st[segIdx]
		// Real = min→max wall-clock of content events in the segment.
		// Bookkeeping (meta/source/mark/station/asset) is excluded: meta and
		// source stamp scimux append-time, while mirrored turns/tools carry
		// CLI timestamps — mixing them inflates Real on adopted sessions.
		if countsTowardReal(ev.T) {
			if t, ok := parseEventTime(ev.Time); ok {
				if !s.hasFirst || t.Before(s.first) {
					s.first, s.hasFirst = t, true
				}
				if t.After(s.last) {
					s.last = t
				}
			}
		}
		switch ev.T {
		case "tool":
			if ev.Tool == nil || ev.Tool.ID == "" {
				continue
			}
			// Approval-kind rows are wait-related on codex app-server, not
			// tool-exec intervals (V2-P1 grounding).
			if strings.EqualFold(ev.Tool.Kind, "approval") {
				continue
			}
			t, ok := parseEventTime(ev.Time)
			if !ok {
				continue
			}
			id := ev.Tool.ID
			status := strings.ToLower(ev.Tool.Status)
			if isTerminalToolStatus(status) {
				if _, seen := s.toolEnds[id]; !seen {
					s.toolEnds[id] = t
				}
				// A lone completion without a prior start is complete-only:
				// do not invent a start (absent ≠ zero).
			} else {
				// Any non-terminal status is a start candidate (pending,
				// in_progress, "", started, …). First wins.
				s.hasToolStart = true
				if _, seen := s.toolStarts[id]; !seen {
					s.toolStarts[id] = t
				}
			}
		case "attention":
			if ev.Attention == nil {
				continue
			}
			s.hasWaitEdge = true
			t, ok := parseEventTime(ev.Time)
			if !ok {
				continue
			}
			status := strings.ToLower(ev.Attention.Status)
			switch status {
			case "start":
				if !s.waitOpenSet {
					s.waitOpen, s.waitOpenSet = t, true
				}
			case "end":
				if s.waitOpenSet {
					s.waitIvs = append(s.waitIvs, fare.Interval{Start: s.waitOpen, End: t})
					s.waitOpenSet = false
				}
			}
		}
	}

	// Close tool intervals per id: need both start and terminal.
	for i := range st {
		s := &st[i]
		for id, start := range s.toolStarts {
			end, ok := s.toolEnds[id]
			if !ok || !end.After(start) {
				continue // open interval → omit from union
			}
			s.toolIvs = append(s.toolIvs, fare.Interval{Start: start, End: end})
		}
		// Unpaired wait start: open → omit (still marks hasWaitEdge).
	}

	rides := make([]fare.Ride, nSeg)
	for i := 0; i < nSeg; i++ {
		s := st[i]
		var real time.Duration
		if s.hasFirst && s.last.After(s.first) {
			real = s.last.Sub(s.first)
		}
		hasToolTiming := len(s.toolIvs) > 0
		// Wait timing present when any attention edge was logged — even if
		// the open wait has no end yet (wait duration 0 until closed). A
		// segment with attention edges is post-V2-P2.
		hasWaitTiming := s.hasWaitEdge
		// Complete-only tool rows alone do not enable the tools split
		// (hasToolTiming false). hasToolStart without a closed interval
		// also does not enable it (open tools omit).
		_ = s.hasToolStart
		// Clip intervals to the Real window so tools+wait cannot exceed real
		// (v2.5 ①: agent+tools+wait ≡ real via residual).
		toolIvs, waitIvs := s.toolIvs, s.waitIvs
		if s.hasFirst && s.last.After(s.first) {
			toolIvs = clipIntervals(toolIvs, s.first, s.last)
			waitIvs = clipIntervals(waitIvs, s.first, s.last)
		}
		rides[i] = fare.BuildRide(segs[i], real, toolIvs, waitIvs, hasToolTiming, hasWaitTiming)
	}

	// Whole-journey ride: re-fold intervals across all segments (same rules).
	var allTool, allWait []fare.Interval
	var hasTool, hasWait bool
	var first, last time.Time
	var hasFirst bool
	for _, s := range st {
		if s.hasFirst {
			if !hasFirst || s.first.Before(first) {
				first = s.first
			}
			if !hasFirst || s.last.After(last) {
				last = s.last
			}
			hasFirst = true
		}
		if len(s.toolIvs) > 0 {
			hasTool = true
			allTool = append(allTool, s.toolIvs...)
		}
		if s.hasWaitEdge {
			hasWait = true
			allWait = append(allWait, s.waitIvs...)
		}
	}
	var real time.Duration
	if hasFirst && last.After(first) {
		real = last.Sub(first)
		allTool = clipIntervals(allTool, first, last)
		allWait = clipIntervals(allWait, first, last)
	}
	whole := fare.BuildRide(totals, real, allTool, allWait, hasTool, hasWait)
	return whole, rides
}

// clipIntervals intersects each interval with [lo, hi]. Empty results drop.
func clipIntervals(ivs []fare.Interval, lo, hi time.Time) []fare.Interval {
	if lo.IsZero() || !hi.After(lo) || len(ivs) == 0 {
		return ivs
	}
	out := make([]fare.Interval, 0, len(ivs))
	for _, iv := range ivs {
		start, end := iv.Start, iv.End
		if start.Before(lo) {
			start = lo
		}
		if end.After(hi) {
			end = hi
		}
		if end.After(start) {
			out = append(out, fare.Interval{Start: start, End: end})
		}
	}
	return out
}

// foldFareFromEvents is the shared reduce for ReadFare, ReadFareBySegment,
// and the ride fold (so tokens and times share one segment layout).
//
// Dedup (D3):
//   - TurnID present → first occurrence wins globally (rotation re-mirror safe).
//   - TurnID absent  → source-seam watermark: within each clear-bounded epoch,
//     only the last segment's no-id records count (mechanical re-mirrors
//     supersede; /clear starts a fresh additive epoch).
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

func isTerminalToolStatus(status string) bool {
	switch status {
	case "completed", "failed", "complete", "error", "cancelled", "canceled":
		return true
	}
	return false
}

// countsTowardReal reports whether an event type contributes to the segment's
// wall-clock span. Pure bookkeeping is excluded so scimux append stamps on
// meta/source/mark never mix with CLI transcript times.
func countsTowardReal(t string) bool {
	switch t {
	case "user", "assistant", "tool", "usage", "attention", "stop", "error":
		return true
	}
	return false
}

func parseEventTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}
