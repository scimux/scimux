package muse

import (
	"encoding/json"
	"testing"
)

func TestEmitTurnCompletedIgnoresPreviousSession(t *testing.T) {
	var events []Event
	c := &Client{
		session:    Session{SessionID: "synthetic-current"},
		activeTurn: "synthetic-active",
		fold:       newFold(),
		sink:       func(event Event) { events = append(events, event) },
	}
	c.recent.add("synthetic-completed")
	c.fold.stream["synthetic-item"] = "synthetic partial"
	params := json.RawMessage(`{"turnId":"synthetic-completed","terminal":"failed","reason":"synthetic failure"}`)

	c.emitTurnCompleted("synthetic-old", params)
	if len(events) != 0 || c.session.SessionID != "synthetic-current" || c.activeTurn != "synthetic-active" {
		t.Fatalf("stale completion changed current session or emitted an event: session=%q active=%q events=%v", c.session.SessionID, c.activeTurn, events)
	}
	if !c.recent.has("synthetic-completed") || len(c.recent.emitted) != 0 || c.fold.stream["synthetic-item"] != "synthetic partial" {
		t.Fatal("stale completion consumed current completion or stream")
	}

	c.emitTurnCompleted("synthetic-current", params)
	if len(events) != 1 || events[0].T != "error" || events[0].Error == "" {
		t.Fatalf("current completion event=%v", events)
	}
	if _, ok := c.recent.emitted["synthetic-completed"]; !ok || len(c.fold.stream) != 0 || c.activeTurn != "synthetic-active" {
		t.Fatal("current completion did not mark and fold the intended turn")
	}
	c.emitTurnCompleted("synthetic-current", params)
	if len(events) != 1 {
		t.Fatalf("current completion emitted twice: %v", events)
	}
}
