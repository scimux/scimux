package app

// Phase 4 (F-11) — the attention thresholds in poller.go are ordered, and the
// order is the design decision. Their magnitudes are adjustable; their
// sequence is not, because each rung encodes how much a particular kind of
// evidence is worth relative to the next. A tweak that inverts two of them
// produces no compile error, no test failure anywhere else, and a supervisor
// that reaches a confident verdict on weaker evidence than a cautious one.

import (
	"testing"
	"time"
)

func TestAttentionLadderIsOrdered(t *testing.T) {
	// The quiet-pane chain, ordered by how much a static pane is worth as
	// evidence: the shared precondition, then the anchor-corroborated rung,
	// then the bare owing stall, then AX — flatter and quieter, so slowest.
	quiet := []struct {
		name string
		d    time.Duration
	}{
		{"paneQuietAfter", paneQuietAfter},
		{"owedStallCorroborated", owedStallCorroborated},
		{"owedStallAfter", owedStallAfter},
		{"owedStallAX", owedStallAX},
	}
	for i := 1; i < len(quiet); i++ {
		if quiet[i-1].d >= quiet[i].d {
			t.Errorf("quiet-pane ladder inverted: %s (%s) must be strictly less than %s (%s).\n"+
				"Each rung is slower because its evidence is weaker; equal or reversed "+
				"means the weaker evidence now reaches a verdict at least as fast.",
				quiet[i-1].name, quiet[i-1].d, quiet[i].name, quiet[i].d)
		}
	}

	// The cross-branch rule: a pane that is still changing is weaker evidence
	// of a wait than one that stopped, so the active-pane backstop must be
	// slower than the quiet-pane one.
	if owedStallAfter >= animStallAfter {
		t.Errorf("owedStallAfter (%s) must be strictly less than animStallAfter (%s): "+
			"a confined animation strip is weaker evidence of a wait than a pane that "+
			"stopped changing at all", owedStallAfter, animStallAfter)
	}
}
