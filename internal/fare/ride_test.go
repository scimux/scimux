package fare

import (
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			panic(err)
		}
	}
	return t
}

func TestUnionDuration_Overlapping(t *testing.T) {
	// Two tools: [0, 5s] and [3s, 8s] → union 8s, not 10s.
	ivs := []Interval{
		{Start: ts("2026-08-01T10:00:00Z"), End: ts("2026-08-01T10:00:05Z")},
		{Start: ts("2026-08-01T10:00:03Z"), End: ts("2026-08-01T10:00:08Z")},
	}
	got := UnionDuration(ivs)
	want := 8 * time.Second
	if got != want {
		t.Errorf("UnionDuration(overlap) = %v, want %v (must not double-count)", got, want)
	}
}

func TestUnionDuration_Disjoint(t *testing.T) {
	ivs := []Interval{
		{Start: ts("2026-08-01T10:00:00Z"), End: ts("2026-08-01T10:00:02Z")},
		{Start: ts("2026-08-01T10:00:05Z"), End: ts("2026-08-01T10:00:07Z")},
	}
	got := UnionDuration(ivs)
	want := 4 * time.Second
	if got != want {
		t.Errorf("UnionDuration(disjoint) = %v, want %v", got, want)
	}
}

func TestUnionDuration_EmptyAndOpen(t *testing.T) {
	if got := UnionDuration(nil); got != 0 {
		t.Errorf("nil = %v, want 0", got)
	}
	// Zero/inverted intervals contribute nothing.
	if got := UnionDuration([]Interval{
		{Start: ts("2026-08-01T10:00:05Z"), End: ts("2026-08-01T10:00:01Z")},
		{},
	}); got != 0 {
		t.Errorf("invalid intervals = %v, want 0", got)
	}
}

func TestResidualAgent_Partition(t *testing.T) {
	real := 10 * time.Second
	tools := 3 * time.Second
	wait := 2 * time.Second
	agent := ResidualAgent(real, tools, wait)
	if agent != 5*time.Second {
		t.Errorf("agent = %v, want 5s", agent)
	}
	if agent+tools+wait != real {
		t.Errorf("agent+tools+wait = %v, want real %v", agent+tools+wait, real)
	}
}

func TestResidualAgent_ClampWhenPartsExceedReal(t *testing.T) {
	// Overlapping tools∪wait can exceed real; residual clamps so parts never
	// report a negative agent. Sum is still real only when tools+wait ≤ real;
	// when they exceed, agent=0 and tools+wait may exceed real — residual
	// still guarantees agent ≥ 0 (v2-D1 "never exceeds the whole" for agent).
	agent := ResidualAgent(5*time.Second, 4*time.Second, 3*time.Second)
	if agent != 0 {
		t.Errorf("agent = %v, want 0 (clamped)", agent)
	}
}

func TestBuildRide_PartitionIdentity(t *testing.T) {
	// Overlapping tools + a wait interval; agent is residual.
	// real = 20s; tools union [0,5]∪[3,8]=8s; wait [10,14]=4s; agent=8s.
	totals := FareTotals{FreshIn: 1, Out: 1, Turns: 1, ReportedCostComplete: true}
	real := 20 * time.Second
	toolIvs := []Interval{
		{Start: ts("2026-08-01T10:00:00Z"), End: ts("2026-08-01T10:00:05Z")},
		{Start: ts("2026-08-01T10:00:03Z"), End: ts("2026-08-01T10:00:08Z")},
	}
	waitIvs := []Interval{
		{Start: ts("2026-08-01T10:00:10Z"), End: ts("2026-08-01T10:00:14Z")},
	}
	r := BuildRide(totals, real, toolIvs, waitIvs, true, true)
	if !r.HasSplit {
		t.Fatal("HasSplit = false, want true")
	}
	if r.Tools != 8*time.Second {
		t.Errorf("Tools = %v, want 8s", r.Tools)
	}
	if r.Wait != 4*time.Second {
		t.Errorf("Wait = %v, want 4s", r.Wait)
	}
	if r.Agent != 8*time.Second {
		t.Errorf("Agent = %v, want 8s", r.Agent)
	}
	if r.Agent+r.Tools+r.Wait != r.Real {
		t.Errorf("agent+tools+wait = %v ≠ real %v", r.Agent+r.Tools+r.Wait, r.Real)
	}
	if r.Totals.Turns != 1 {
		t.Errorf("Totals.Turns = %d, want 1", r.Totals.Turns)
	}
}

func TestBuildRide_NoTiming_SplitAbsent(t *testing.T) {
	// Transport with no recoverable tool/wait timing (e.g. codex app-server
	// complete-only, pre-V2-P2 historical): Real present, split absent ≠ zero.
	r := BuildRide(FareTotals{Turns: 1, FreshIn: 10, ReportedCostComplete: true},
		30*time.Second, nil, nil, false, false)
	if r.HasSplit {
		t.Error("HasSplit = true, want false (absent split)")
	}
	if r.Real != 30*time.Second {
		t.Errorf("Real = %v, want 30s", r.Real)
	}
	if r.Agent != 0 || r.Tools != 0 || r.Wait != 0 {
		t.Errorf("split parts = %v/%v/%v, want 0/0/0 when absent (not projected)",
			r.Agent, r.Tools, r.Wait)
	}
}

func TestBuildRide_WaitOnly_SplitPresent(t *testing.T) {
	// Attention edges without closed tool intervals still enable the split
	// (tools contribute 0, not absent).
	waitIvs := []Interval{
		{Start: ts("2026-08-01T10:00:00Z"), End: ts("2026-08-01T10:00:05Z")},
	}
	r := BuildRide(FareTotals{}, 10*time.Second, nil, waitIvs, false, true)
	if !r.HasSplit {
		t.Fatal("HasSplit = false, want true (wait timing present)")
	}
	if r.Wait != 5*time.Second || r.Tools != 0 || r.Agent != 5*time.Second {
		t.Errorf("agent/tools/wait = %v/%v/%v, want 5s/0/5s", r.Agent, r.Tools, r.Wait)
	}
	if r.Agent+r.Tools+r.Wait != r.Real {
		t.Errorf("partition broken: sum=%v real=%v", r.Agent+r.Tools+r.Wait, r.Real)
	}
}
