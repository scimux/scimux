package app

import (
	"reflect"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/transcript"
)

func addressedTurns(roles ...string) []transcript.Turn {
	turns := make([]transcript.Turn, len(roles))
	for i, role := range roles {
		turns[i] = transcript.Turn{Role: role, Record: i * 2}
	}
	return turns
}

func TestChatTailWindowTurnBoundaries(t *testing.T) {
	cases := []struct {
		name       string
		turns      []transcript.Turn
		n          int
		start      int
		windowHead int
	}{
		{name: "at or below tail", turns: addressedTurns("user", "assistant"), n: 2, start: 0},
		{name: "snaps to user turn", turns: addressedTurns("user", "assistant", "user", "assistant", "user", "assistant"), n: 2, start: 4},
		{name: "hard cut records leading assistant", turns: addressedTurns("user", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant"), n: 2, start: 10, windowHead: 2},
		{name: "first assistant is inside window", turns: []transcript.Turn{{Role: "user", Record: 0}, {Role: "assistant", Record: 2}, {Role: "user", Record: 4}, {Role: "user", Record: 6}, {Role: "user", Record: 8}, {Role: "user", Record: 10}, {Role: "user", Record: 12}, {Role: "user", Record: 14}, {Role: "user", Record: 16}, {Role: "user", Record: 18}, {Role: "assistant", Record: 20}, {Role: "assistant", Record: 22}}, n: 2, start: 10},
		{name: "no user turn", turns: addressedTurns("assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant"), n: 2, start: 6, windowHead: 0},
		{name: "cycle exactly at cap snaps", turns: addressedTurns("user", "assistant", "assistant", "assistant", "assistant", "assistant", "user", "assistant"), n: 2, start: 6},
		{name: "cycle one over cap hard cuts", turns: addressedTurns("user", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "user", "assistant"), n: 2, start: 7, windowHead: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chatTailWindow(tc.turns, nil, tc.n)
			if got.start != tc.start {
				t.Errorf("start = %d, want %d", got.start, tc.start)
			}
			if tc.windowHead == 0 && got.windowHead != nil {
				t.Errorf("window_head = %+v, want none", got.windowHead)
			} else if tc.windowHead != 0 && (got.windowHead == nil || got.windowHead.Record != tc.windowHead) {
				t.Errorf("window_head = %+v, want record %d", got.windowHead, tc.windowHead)
			}
		})
	}
}

func TestChatTailWindowDecisionsAndLeaseBase(t *testing.T) {
	turns := addressedTurns("user", "assistant", "user", "assistant", "user", "assistant", "user", "assistant")
	decisions := []sessionlog.DecisionSurface{
		{Record: 1, Decision: sessionlog.DecisionEvent{LeaseID: "lease-a"}},
		{Record: 5, Decision: sessionlog.DecisionEvent{LeaseID: "lease-a"}},
		{Record: 11, Decision: sessionlog.DecisionEvent{LeaseID: "lease-dropped-only"}},
		{Record: 12, Decision: sessionlog.DecisionEvent{LeaseID: "lease-a"}},
		{Record: 13, Decision: sessionlog.DecisionEvent{LeaseID: "lease-b"}},
		{Record: 14, Decision: sessionlog.DecisionEvent{LeaseID: ""}},
	}
	got := chatTailWindow(turns, decisions, 2)
	if got.start != 6 {
		t.Fatalf("start = %d, want 6", got.start)
	}
	wantDecisions := decisions[3:]
	if !reflect.DeepEqual(got.decisions, wantDecisions) {
		t.Errorf("kept decisions = %+v, want %+v", got.decisions, wantDecisions)
	}
	wantBase := map[string]int{"lease-a": 2}
	if !reflect.DeepEqual(got.leaseBase, wantBase) {
		t.Errorf("lease_base = %#v, want %#v", got.leaseBase, wantBase)
	}
	if empty := chatTailWindow(turns, nil, 2); len(empty.decisions) != 0 || len(empty.leaseBase) != 0 {
		t.Errorf("empty decisions produced decisions=%v lease_base=%v", empty.decisions, empty.leaseBase)
	}
}
