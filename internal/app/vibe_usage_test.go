package app

import (
	"net/http/httptest"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
)

// Context occupancy, token spend, and Vibe-reported cost stay distinct fields
// on the state payload. The numbers are synthetic.
func TestVibeStateSeparatesOccupancyTokensAndCost(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{
		ID: "vibe-fare", Title: "v", Agent: "vibe", Model: "synthetic",
		Dir: a.home, CreatedAt: "2026-09-30T00:00:00Z",
	}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.live[n.ID] = "quiet"
	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "vibe", "synthetic", "", a.home),
		{T: "usage", Usage: &sessionlog.UsageEvent{Used: 1000, Size: 4000}},
		{T: "usage", Usage: &sessionlog.UsageEvent{
			Used: 1000, Size: 4000,
			InputTokens: 30, OutputTokens: 12, TotalTokens: 42,
			CostAmount: 1.5, CostCurrency: "USD",
		}},
	})
	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("state = %d %s", rec.Code, rec.Body)
	}
	node := stateNodeByID(t, rec.Body.Bytes(), n.ID)
	if got := int(node["ctx_pct"].(float64)); got != 25 {
		t.Fatalf("ctx_pct = %d, want 25 from occupancy 1000/4000", got)
	}
	assertJSONInt(t, node, "fare_fresh_in", 0)
	assertJSONInt(t, node, "fare_cache_read", 0)
	assertJSONInt(t, node, "fare_unsplit_in", 30)
	assertJSONInt(t, node, "fare_out", 12)
	assertJSONInt(t, node, "fare_total", 42)
	if got := node["fare_cost"].(float64); got != 1.5 {
		t.Fatalf("fare_cost = %v, want 1.5", got)
	}
	if node["fare_cost_complete"] != true {
		t.Fatalf("fare_cost_complete = %v", node["fare_cost_complete"])
	}
	if node["fare_total"] == node["ctx_pct"] {
		t.Fatal("token total and context percent are the same figure")
	}
}

// Vibe's ACP input includes cached prompt tokens and has no fresh/cache split.
// The stored meter keeps that quantity, the occupancy gauge, and the USD cost
// without publishing a fresh or cache count.
func TestVibeStoredInputIsUnsplit(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{
		ID: "vibe-unsplit", Title: "v", Agent: "vibe", Model: "synthetic",
		Dir: a.home, CreatedAt: "2026-09-30T00:00:00Z",
	}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.live[n.ID] = "quiet"
	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "vibe", "synthetic", "", a.home),
		{T: "usage", Usage: &sessionlog.UsageEvent{Used: 1000, Size: 4000}},
		{T: "usage", Usage: &sessionlog.UsageEvent{
			Used: 1000, Size: 4000,
			InputTokens: 30, OutputTokens: 12, TotalTokens: 42,
			CachedReadTokens: 80,
			CostAmount:       1.5, CostCurrency: "USD",
		}},
	})
	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("state = %d %s", rec.Code, rec.Body)
	}
	node := stateNodeByID(t, rec.Body.Bytes(), n.ID)
	if got := int(node["ctx_pct"].(float64)); got != 25 {
		t.Fatalf("ctx_pct = %d, want 25", got)
	}
	assertJSONInt(t, node, "fare_fresh_in", 0)
	assertJSONInt(t, node, "fare_cache_read", 0)
	assertJSONInt(t, node, "fare_cache_write", 0)
	assertJSONInt(t, node, "fare_unsplit_in", 30)
	assertJSONInt(t, node, "fare_out", 12)
	assertJSONInt(t, node, "fare_total", 42)
	if got := node["fare_cost"].(float64); got != 1.5 {
		t.Fatalf("fare_cost = %v, want 1.5", got)
	}
	if node["fare_cost_complete"] != true {
		t.Fatalf("fare_cost_complete = %v", node["fare_cost_complete"])
	}
	segs, ok := node["fare_segments"].([]any)
	if !ok || len(segs) == 0 {
		t.Fatal("fare segment missing")
	}
	seg, ok := segs[0].(map[string]any)
	if !ok {
		t.Fatal("fare segment shape")
	}
	assertJSONInt(t, seg, "fresh_in", 0)
	assertJSONInt(t, seg, "cache_read", 0)
	assertJSONInt(t, seg, "unsplit_in", 30)
	assertJSONInt(t, seg, "out", 12)
	assertJSONInt(t, seg, "total", 42)
}
