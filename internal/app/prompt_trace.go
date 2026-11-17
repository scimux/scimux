package app

import (
	"reflect"
	"strings"

	"github.com/scimux/scimux/internal/sessionlog"
)

// structuredTraceTransport reports workers with synchronous user-row admission. A mirrored
// tmux transcript can lag or include terminal-originated prompts.
func structuredTraceTransport(n *Node) bool {
	if n.Agent == "claude" {
		return false
	}
	switch n.transport() {
	case "codex", "acp", "muse":
		return true
	default:
		return false
	}
}

// promptAddress is a durable session-log address, not proof of delivery.
type promptAddress struct {
	UID     string `json:"uid"`
	Segment int    `json:"segment"`
	Record  int    `json:"record"`
}

// queuedPromptAddress compares complete append-only snapshots while owner sends
// and clears are serialized. More than one new user row, a seam, replacement,
// or an incomplete identity makes the result ambiguous.
func queuedPromptAddress(before, after []sessionlog.Event, delivered string) (promptAddress, bool) {
	var zero promptAddress
	if len(before) == 0 || len(after) < len(before) || !reflect.DeepEqual(before, after[:len(before)]) {
		return zero, false
	}
	uid := ""
	segment := 0
	for _, ev := range before {
		if ev.T == "meta" {
			if ev.Meta == nil || ev.Meta.UID == "" || uid != "" {
				return zero, false
			}
			uid = ev.Meta.UID
		}
		if ev.T == "source" {
			segment++
		}
	}
	if uid == "" {
		return zero, false
	}
	found := -1
	for i := len(before); i < len(after); i++ {
		switch after[i].T {
		case "meta", "source":
			return zero, false
		case "user":
			if found >= 0 || after[i].Text != delivered {
				return zero, false
			}
			found = i
		}
	}
	if found < 0 {
		return zero, false
	}
	return promptAddress{UID: uid, Segment: segment, Record: found}, true
}

// promptTraceOutcome only treats a genuine assistant response as positive
// evidence. Errors and later user/source boundaries cannot be crossed. Claude
// addresses are admitted only after verified transcript and mirror readback.
func promptTraceOutcome(events []sessionlog.Event, address promptAddress) string {
	if address.UID == "" || address.Segment < 0 || address.Record < 0 || address.Record >= len(events) {
		return "unknown"
	}
	uid := ""
	segment := 0
	for i := 0; i <= address.Record; i++ {
		ev := events[i]
		if ev.T == "meta" {
			if ev.Meta == nil || ev.Meta.UID == "" || uid != "" {
				return "unknown"
			}
			uid = ev.Meta.UID
		}
		if ev.T == "source" {
			segment++
		}
	}
	if uid != address.UID || segment != address.Segment || events[address.Record].T != "user" {
		return "unknown"
	}
	for _, ev := range events[address.Record+1:] {
		switch ev.T {
		case "meta", "source", "user":
			return "unknown"
		case "error":
			return "rejected"
		case "assistant":
			if strings.TrimSpace(ev.Text) != "" {
				return "confirmed"
			}
		}
	}
	return "pending"
}
