package sessionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplicationOutcomeRoundTripAndProjection(t *testing.T) {
	// This schema is independently synthetic. An application outcome is an
	// audit surface, never another human decision, conversation turn, or seam.
	raw := []byte(`{"t":"decision","decision":{"source":"application","request_id":"req-synthetic","agent":"claude","selected":{"key":"","name":"Not delivered","kind":"not_delivered"},"outcome":{"delivery":"not_delivered","app_context":"Application delivery outcome."}}}`)
	var ev Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := &Writer{Path: path}
	if err := w.Append(ev); err != nil {
		t.Fatal(err)
	}
	seg := ReadSegment(path)
	if len(seg.Decisions) != 1 || len(seg.Turns) != 0 || len(seg.ClearTimes) != 0 {
		t.Fatalf("outcome became conversation content: %+v", seg)
	}
	got, err := json.Marshal(seg.Decisions[0].Decision)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"source":"application"`) || !strings.Contains(string(got), `"outcome":{"delivery":"not_delivered","app_context":"Application delivery outcome."}`) || strings.Contains(string(got), `"manual"`) {
		t.Fatalf("application provenance lost in projection: %s", got)
	}
	if turns := ReadTurns(path); len(turns) != 0 {
		t.Fatalf("outcome became a turn: %+v", turns)
	}
}

func TestManualDecisionRoundTripStaysDistinctFromAuto(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.jsonl")
	w := &Writer{Path: path}
	auto := NewDecision(DecisionEvent{
		Source:    "auto",
		LeaseID:   "lease-1",
		RequestID: "req-auto",
		Agent:     "claude",
		ToolKind:  "execute",
		Title:     "Bash: ls",
		Selected:  DecOption{Name: "Allow once", Kind: "allow"},
	})
	if err := w.Append(auto); err != nil {
		t.Fatal(err)
	}
	manual := NewDecision(DecisionEvent{
		Source:    "manual",
		RequestID: "req-manual",
		Agent:     "claude",
		Title:     "AskUserQuestion",
		Selected:  DecOption{Name: "Answered", Kind: "answer"},
		Manual: &ManualInteraction{
			Action:      "answer",
			Tool:        "AskUserQuestion",
			InputDigest: "abc",
			Feedback:    "",
			AppContext:  "",
			Delivery:    "recorded",
			SessionID:   "sid-synthetic",
			Selections: []ManualSelection{{
				QuestionID: "q1",
				Question:   "Pick a letter?",
				OptionIDs:  []string{"q1o1"},
				Labels:     []string{"Alpha"},
				Kind:       "option",
			}},
		},
	})
	if err := w.Append(manual); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d", len(lines))
	}
	if strings.Contains(lines[0], `"manual"`) || strings.Contains(lines[0], `"source":"manual"`) {
		t.Fatalf("auto row gained manual data: %s", lines[0])
	}
	if !strings.Contains(lines[0], `"source":"auto"`) || !strings.Contains(lines[0], `"Allow once"`) {
		t.Fatalf("auto row changed: %s", lines[0])
	}
	events := ReadEvents(path)
	if len(events) != 2 || events[0].Decision == nil || events[0].Decision.Manual != nil {
		t.Fatalf("auto read = %+v", events[0].Decision)
	}
	got := events[1].Decision
	if got == nil || got.Source != "manual" || got.Manual == nil {
		t.Fatalf("manual read = %+v", got)
	}
	if got.Manual.Feedback != "" || got.Manual.Selections[0].Kind != "option" || got.Manual.Selections[0].Labels[0] != "Alpha" {
		t.Fatalf("manual payload = %+v", got.Manual)
	}
	if got.Manual.Delivery != "recorded" || got.LeaseID != "" {
		t.Fatalf("delivery/lease = %+v", got)
	}
	if events[1].T != "decision" {
		t.Fatalf("t = %q", events[1].T)
	}
}
