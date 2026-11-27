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
	window := chatWindow{decisions: []sessionlog.DecisionSurface{}}
	if len(turns) > n {
		start := len(turns) - n
		userStart := 0
		for i := 0; i <= start; i++ {
			if turns[i].Role == "user" {
				userStart = i
			}
		}
		if len(turns)-userStart <= 3*n {
			window.start = userStart
		} else {
			window.start = start
			for i := userStart; i < window.start; i++ {
				if turns[i].Role == "assistant" {
					window.windowHead = &chatWindowHead{
						UID: turns[i].UID, Segment: turns[i].Segment, Record: turns[i].Record,
					}
					break
				}
			}
		}
	}

	if window.start == 0 {
		window.decisions = decisions
		return window
	}
	firstRecord := turns[window.start].Record
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

func chatStartWindow(turns []transcript.Turn, decisions []sessionlog.DecisionSurface, start int) chatWindow {
	return chatWindow{}
}
