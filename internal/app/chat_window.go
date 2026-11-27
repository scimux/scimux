package app

import (
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/transcript"
)

type chatWindow struct {
	start      int
	windowHead *chatWindowHead
	decisions  []sessionlog.DecisionSurface
	leaseBase  map[string]int
}

type chatWindowHead struct {
	UID     string `json:"uid"`
	Segment int    `json:"segment"`
	Record  int    `json:"record"`
}

func chatTailWindow(turns []transcript.Turn, decisions []sessionlog.DecisionSurface, n int) chatWindow {
	start := 0
	if len(turns) > n {
		start = len(turns) - n
		userStart := 0
		for i := 0; i <= start; i++ {
			if turns[i].Role == "user" {
				userStart = i
			}
		}
		if len(turns)-userStart <= 3*n {
			start = userStart
		}
	}
	return chatStartWindow(turns, decisions, start)
}

func chatStartWindow(turns []transcript.Turn, decisions []sessionlog.DecisionSurface, start int) chatWindow {
	if start < 0 {
		start = 0
	}
	if start > len(turns) {
		start = len(turns)
	}
	window := chatWindow{start: start, decisions: []sessionlog.DecisionSurface{}}
	if start > 0 && start < len(turns) && turns[start].Role != "user" {
		userStart := 0
		for i := 0; i <= start; i++ {
			if turns[i].Role == "user" {
				userStart = i
			}
		}
		for i := userStart; i < start; i++ {
			if turns[i].Role == "assistant" {
				window.windowHead = &chatWindowHead{UID: turns[i].UID, Segment: turns[i].Segment, Record: turns[i].Record}
				break
			}
		}
	}
	if start == 0 {
		window.decisions = decisions
		return window
	}
	if start == len(turns) {
		return window
	}
	firstRecord := turns[start].Record
	keptLeases := make(map[string]bool)
	for _, decision := range decisions {
		if decision.Record >= firstRecord {
			window.decisions = append(window.decisions, decision)
			if decision.Decision.LeaseID != "" {
				keptLeases[decision.Decision.LeaseID] = true
			}
		}
	}
	for _, decision := range decisions {
		leaseID := decision.Decision.LeaseID
		if decision.Record < firstRecord && keptLeases[leaseID] && leaseID != "" {
			if window.leaseBase == nil {
				window.leaseBase = make(map[string]int)
			}
			window.leaseBase[leaseID]++
		}
	}
	return window
}
