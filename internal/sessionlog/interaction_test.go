package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
