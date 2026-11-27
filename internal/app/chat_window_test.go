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
		hasHead    bool
	}{
		{name: "empty segment", turns: []transcript.Turn{}, n: 2, start: 0},
		{name: "at or below tail", turns: addressedTurns("user", "assistant"), n: 2, start: 0},
		{name: "snaps to user turn", turns: addressedTurns("user", "assistant", "user", "assistant", "user", "assistant"), n: 2, start: 4},
		{name: "hard cut records leading assistant", turns: addressedTurns("user", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant"), n: 2, start: 10, windowHead: 2, hasHead: true},
		{name: "first assistant is inside window", turns: []transcript.Turn{{Role: "user", Record: 0}, {Role: "tool", Record: 2}, {Role: "tool", Record: 4}, {Role: "tool", Record: 6}, {Role: "tool", Record: 8}, {Role: "tool", Record: 10}, {Role: "tool", Record: 12}, {Role: "tool", Record: 14}, {Role: "tool", Record: 16}, {Role: "tool", Record: 18}, {Role: "assistant", Record: 20}, {Role: "assistant", Record: 22}}, n: 2, start: 10},
		{name: "no user turn", turns: addressedTurns("assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant"), n: 2, start: 6, windowHead: 0, hasHead: true},
		{name: "cycle exactly at cap snaps", turns: addressedTurns("user", "assistant", "user", "assistant", "assistant", "assistant", "assistant", "assistant"), n: 2, start: 2},
		{name: "cycle one over cap hard cuts", turns: addressedTurns("user", "assistant", "user", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant"), n: 2, start: 7, windowHead: 6, hasHead: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chatTailWindow(tc.turns, nil, tc.n)
			if got.start != tc.start {
				t.Errorf("start = %d, want %d", got.start, tc.start)
			}
			if !tc.hasHead && got.windowHead != nil {
				t.Errorf("window_head = %+v, want none", got.windowHead)
			} else if tc.hasHead && (got.windowHead == nil || got.windowHead.Record != tc.windowHead) {
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

func TestChatTailWindowKeepsAllDecisionsWhenNothingIsOmitted(t *testing.T) {
	decisions := []sessionlog.DecisionSurface{
		{Record: 2, Decision: sessionlog.DecisionEvent{LeaseID: "lease-a"}},
		{Record: 4, Decision: sessionlog.DecisionEvent{LeaseID: "lease-a"}},
	}
	t.Run("empty segment", func(t *testing.T) {
		got := chatTailWindow(nil, decisions, 50)
		if !reflect.DeepEqual(got.decisions, decisions) {
			t.Errorf("decisions = %+v, want both %+v", got.decisions, decisions)
		}
		if len(got.leaseBase) != 0 {
			t.Errorf("lease_base = %#v, want empty", got.leaseBase)
		}
	})
	t.Run("tail contains full segment", func(t *testing.T) {
		turns := []transcript.Turn{{Role: "user", Record: 10}, {Role: "assistant", Record: 11}}
		got := chatTailWindow(turns, decisions[:1], 50)
		if !reflect.DeepEqual(got.decisions, decisions[:1]) {
			t.Errorf("decision before first turn = %+v, want retained %+v", got.decisions, decisions[:1])
		}
		if len(got.leaseBase) != 0 {
			t.Errorf("lease_base = %#v, want empty", got.leaseBase)
		}
	})
}

func TestChatStartWindowBoundaries(t *testing.T) {
	turns := addressedTurns("user", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant", "assistant")
	turns[1].UID, turns[1].Segment = "uid-a", 3
	for _, tc := range []struct {
		name      string
		start     int
		wantStart int
		wantHead  int
		hasHead   bool
	}{
		{name: "zero", start: 0, wantStart: 0},
		{name: "mid-cycle", start: 4, wantStart: 4, wantHead: 2, hasHead: true},
		{name: "at length", start: 8, wantStart: 8},
		{name: "past length clamps", start: 99, wantStart: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := chatStartWindow(turns, nil, tc.start)
			if got.start != tc.wantStart {
				t.Errorf("start=%d, want %d", got.start, tc.wantStart)
			}
			if tc.hasHead {
				if got.windowHead == nil || got.windowHead.Record != tc.wantHead || got.windowHead.UID != "uid-a" || got.windowHead.Segment != 3 {
					t.Errorf("window_head=%+v, want uid-a/3/%d", got.windowHead, tc.wantHead)
				}
			} else if got.windowHead != nil {
				t.Errorf("window_head=%+v, want none", got.windowHead)
			}
		})
	}
}

func TestChatStartWindowDecisionsAndLeaseBase(t *testing.T) {
	turns := addressedTurns("user", "assistant", "user", "assistant", "user", "assistant", "user", "assistant")
	decisions := []sessionlog.DecisionSurface{
		{Record: 1, Decision: sessionlog.DecisionEvent{LeaseID: "lease-a"}},
		{Record: 5, Decision: sessionlog.DecisionEvent{LeaseID: "lease-a"}},
		{Record: 11, Decision: sessionlog.DecisionEvent{LeaseID: "dropped-only"}},
		{Record: 12, Decision: sessionlog.DecisionEvent{LeaseID: "lease-a"}},
		{Record: 13, Decision: sessionlog.DecisionEvent{LeaseID: "lease-b"}},
	}
	got := chatStartWindow(turns, decisions, 6)
	if !reflect.DeepEqual(got.decisions, decisions[3:]) || !reflect.DeepEqual(got.leaseBase, map[string]int{"lease-a": 2}) {
		t.Errorf("start decisions/base=%+v/%v", got.decisions, got.leaseBase)
	}
	zero := chatStartWindow(turns, decisions, 0)
	if !reflect.DeepEqual(zero.decisions, decisions) || len(zero.leaseBase) != 0 {
		t.Errorf("zero start decisions/base=%+v/%v", zero.decisions, zero.leaseBase)
	}
	end := chatStartWindow(turns, decisions, len(turns))
	if len(end.decisions) != 0 || end.windowHead != nil {
		t.Errorf("end start retained decisions/head=%+v/%+v", end.decisions, end.windowHead)
	}
}
