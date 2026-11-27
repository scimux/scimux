package app

import (
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/transcript"
)

type chatWindow struct {
	start      int
	windowHead *transcript.Turn
	decisions  []sessionlog.DecisionSurface
	leaseBase  map[string]int
}

func chatTailWindow(turns []transcript.Turn, decisions []sessionlog.DecisionSurface, n int) chatWindow {
	return chatWindow{start: 0}
}
