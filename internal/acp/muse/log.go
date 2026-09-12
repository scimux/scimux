// Package muse is the isolated Muse/MSP JSON-RPC wire client.
//
// It speaks newline-delimited JSON-RPC 2.0 over `muse serve` stdio using
// only the Go standard library and scimux internal packages. Command IDs
// are client-minted UUIDv7 values; item IDs stay opaque. Production spawn
// is exactly `<bin> serve`. This package must not read a credential store,
// call a Model API, or wire Muse into the application.
package muse

import (
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// Event aliases keep this package on the unified session-log schema.
// Muse writes the same records every other transport writes; there is no
// second event model. Event.Prov retains Phase 2 provenance passthrough.
type (
	Event      = sessionlog.Event
	ToolEvent  = sessionlog.ToolEvent
	UsageEvent = sessionlog.UsageEvent
	logWriter  = sessionlog.Writer
)

func readTurns(path string) []transcript.Turn { return sessionlog.ReadTurns(path) }

func peekLog(path string, maxLines int) string {
	return sessionlog.PeekLog(path, maxLines, "(no muse events yet)")
}

// latestSegmentUsage returns context occupancy from the current log segment
// only. A /clear seam resets the gauge; session/tokenUsage spend records do
// not populate Used/Size and therefore cannot become occupancy.
func latestSegmentUsage(path string) (used, window int64) {
	seg := sessionlog.ReadSegment(path)
	return seg.Used, seg.Size
}
