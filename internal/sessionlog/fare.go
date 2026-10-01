package sessionlog

import (
	"github.com/scimux/scimux/internal/fare"
)

// ReadFare folds the whole session-log journey into canonical fare totals:
// §2.3 normalization via meta.agent, TurnID dedup (D3) with source-seam
// watermark fallback when ids are absent, occupancy shells/empties skipped,
// Used never summed. Defensive: missing/garbled files yield zero totals.
func ReadFare(path string) fare.FareTotals {
	whole, _ := foldFareFromEvents(ReadEvents(path))
	return whole
}

// ReadFareBySegment returns per-segment fare totals split on source seams
// (same delimiters as segment.go / ReadHistory). Dedup is consistent with
// ReadFare: the sum of segment Total()s equals ReadFare().Total().
// Kept (not production-called) so TestReadFareBySegment_SplitsOnSourceSeams
// can assert the whole-vs-segments reconciliation invariant no other test states.
func ReadFareBySegment(path string) []fare.FareTotals {
	_, segs := foldFareFromEvents(ReadEvents(path))
	return segs
}

// usageHit is one billable usage record located in a source-delimited segment.
type usageHit struct {
	u      UsageEvent
	segIdx int
}

// collectFareHits walks the log once: meta.agent, source-seam segment index,
// and billable usage records. Segment indexing matches segment.go — content
// before the first source is segment 0; each source opens the next segment.
func collectFareHits(evs []Event) (agent string, hits []usageHit, openReasons []string) {
	openReasons = []string{""} // segment 0 (pre-first-source surface)
	segIdx := 0
	for _, ev := range evs {
		switch ev.T {
		case "meta":
			if ev.Meta != nil && ev.Meta.Agent != "" {
				agent = ev.Meta.Agent
			}
		case "source":
			reason := ""
			if ev.Source != nil {
				reason = ev.Source.Reason
			}
			openReasons = append(openReasons, reason)
			segIdx = len(openReasons) - 1
		case "usage":
			if ev.Usage == nil || (!hasFareData(ev.Usage) &&
				!(agent == "vibe" && ev.Usage.CostAmount > 0 && ev.Usage.CostCurrency == "USD")) {
				continue
			}
			hits = append(hits, usageHit{u: *ev.Usage, segIdx: segIdx})
		}
	}
	return agent, hits, openReasons
}

// markCounted decides which hits contribute to the fold (D3).
func markCounted(hits []usageHit, openReasons []string) []bool {
	n := len(hits)
	out := make([]bool, n)
	seenID := map[string]bool{}
	nSeg := len(openReasons)
	// Precompute last segment index of each epoch for no-id watermark.
	epochLast := make([]int, nSeg)
	for s := 0; s < nSeg; s++ {
		epochLast[s] = lastSegInEpoch(s, openReasons)
	}
	for i, h := range hits {
		if id := h.u.TurnID; id != "" {
			if seenID[id] {
				continue
			}
			seenID[id] = true
			out[i] = true
			continue
		}
		// No TurnID: seam-watermark fallback — only the last segment of the
		// current clear-bounded epoch counts (mechanical re-mirror supersedes).
		if h.segIdx >= 0 && h.segIdx < nSeg && h.segIdx == epochLast[h.segIdx] {
			out[i] = true
		}
	}
	return out
}

// lastSegInEpoch returns the highest segment index in the same clear-bounded
// epoch as segIdx. A segment opened with reason "clear" starts a new epoch;
// mechanical sources (empty reason) continue the epoch so a rotation's
// re-mirror replaces the prior no-id contribution.
func lastSegInEpoch(segIdx int, openReasons []string) int {
	n := len(openReasons)
	if segIdx < 0 || segIdx >= n {
		return segIdx
	}
	start := segIdx
	for start > 0 && openReasons[start] != "clear" {
		start--
	}
	end := start
	for s := start + 1; s < n; s++ {
		if openReasons[s] == "clear" {
			break
		}
		end = s
	}
	return end
}

// hasFareData reports whether a usage record carries billable breakdown (or a
// totalTokens fallback). Occupancy shells ({used,size,…}) and empty {} are false.
func hasFareData(u *UsageEvent) bool {
	return u.InputTokens != 0 || u.OutputTokens != 0 || u.CachedReadTokens != 0 ||
		u.CacheCreationTokens != 0 || u.TotalTokens != 0
}

// rawFromUsage maps stored fields to fare.RawTokens, applying the total-token
// fallback: a total-only record (no split) attributes total → out when out==0
// so it is not silently dropped.
func rawFromUsage(u UsageEvent) fare.RawTokens {
	raw := fare.RawTokens{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CachedReadTokens,
		CacheWrite: u.CacheCreationTokens,
	}
	if raw.Input == 0 && raw.Output == 0 && raw.CacheRead == 0 && raw.CacheWrite == 0 && u.TotalTokens > 0 {
		raw.Output = u.TotalTokens
	}
	return raw
}

// addCanonical folds one counted turn into dst (tokens, cost, per-model).
func addCanonical(dst *fare.FareTotals, can fare.Canonical, u UsageEvent) {
	dst.FreshIn += can.FreshIn
	dst.CacheRead += can.CacheRead
	dst.CacheWrite += can.CacheWrite
	dst.Out += can.Out
	dst.UnsplitIn += can.UnsplitIn
	dst.Turns++
	if u.CostAmount != 0 {
		dst.ReportedCostUSD += u.CostAmount
	} else {
		// Absent cost on a counted turn: incomplete (absent ≠ zero).
		dst.ReportedCostComplete = false
	}
	if u.Model == "" {
		return
	}
	if dst.PerModel == nil {
		dst.PerModel = make(map[string]fare.FareTotals)
	}
	pm := dst.PerModel[u.Model]
	if pm.Turns == 0 {
		pm.ReportedCostComplete = true
	}
	pm.FreshIn += can.FreshIn
	pm.CacheRead += can.CacheRead
	pm.CacheWrite += can.CacheWrite
	pm.Out += can.Out
	pm.UnsplitIn += can.UnsplitIn
	pm.Turns++
	if u.CostAmount != 0 {
		pm.ReportedCostUSD += u.CostAmount
	} else {
		pm.ReportedCostComplete = false
	}
	dst.PerModel[u.Model] = pm
}
