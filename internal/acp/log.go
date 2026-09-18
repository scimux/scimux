package acp

import (
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/transcript"
)

// The session log machinery lives in internal/sessionlog — one unified
// per-node JSONL store shared by every transport. These aliases keep the
// manager code reading naturally in ACP terms.
type (
	Event      = sessionlog.Event
	ToolEvent  = sessionlog.ToolEvent
	UsageEvent = sessionlog.UsageEvent
	logWriter  = sessionlog.Writer
)

func readEvents(path string) []Event             { return sessionlog.ReadEvents(path) }
func readTurns(path string) []transcript.Turn    { return sessionlog.ReadTurns(path) }
func latestUsage(path string) (used, size int64) { return sessionlog.LatestUsage(path) }
func peekLog(path string, maxLines int) string {
	return sessionlog.PeekLog(path, maxLines, "(no ACP events yet)")
}
