package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/transcript"
)

// verifyClaudeFrontier compares every parsed visible transcript turn with the
// current source segment. A mirror mark is not a parsed-byte watermark.
func verifyClaudeFrontier(raw []byte, events []sessionlog.Event, path, session string) bool {
	if len(raw) == 0 || raw[len(raw)-1] != '\n' || path == "" || session == "" {
		return false
	}
	uid := ""
	source := -1
	for i, ev := range events {
		switch ev.T {
		case "meta":
			if ev.Meta == nil || ev.Meta.UID == "" || uid != "" {
				return false
			}
			uid = ev.Meta.UID
		case "source":
			source = i
		}
	}
	if uid == "" || source < 0 || events[source].Source == nil || events[source].Source.Path != path || events[source].Source.SessionID != session {
		return false
	}
	var turns []transcript.Turn
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var envelope struct {
			Type    string          `json:"type"`
			IsMeta  bool            `json:"isMeta"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &envelope) != nil || envelope.Type == "" {
			return false
		}
		switch envelope.Type {
		case "user", "assistant":
			if !envelope.IsMeta {
				var msg struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				}
				if json.Unmarshal(envelope.Message, &msg) != nil || (msg.Role != "" && msg.Role != envelope.Type) || len(msg.Content) == 0 {
					return false
				}
				var text string
				var blocks []json.RawMessage
				if json.Unmarshal(msg.Content, &text) != nil && json.Unmarshal(msg.Content, &blocks) != nil {
					return false
				}
				if bytes.Equal(bytes.TrimSpace(msg.Content), []byte("null")) {
					return false
				}
			}
		case "summary", "system", "progress", "file-history-snapshot", "queue-operation", "custom-title", "pr-link":
		default:
			return false
		}
		if turn, ok := transcript.ParseLine(line); ok {
			turns = append(turns, turn)
		}
	}
	var mirrored []sessionlog.Event
	for _, ev := range events[source+1:] {
		if ev.T == "user" || ev.T == "assistant" {
			mirrored = append(mirrored, ev)
		}
	}
	if len(turns) != len(mirrored) {
		return false
	}
	for i, turn := range turns {
		ev := mirrored[i]
		if turn.Role != ev.T || turn.Text != ev.Text || turn.Time != ev.Time || !sameClaudeProvenance(turn.Prov, ev.Prov) {
			return false
		}
	}
	return true
}

// sameClaudeProvenance compares JSON values without changing stored bytes.
func sameClaudeProvenance(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	return da.Decode(&x) == nil && db.Decode(&y) == nil && reflect.DeepEqual(x, y)
}

// claudeTraceFrontier is a read-only snapshot. The caller must revalidate the
// owning node and worker binding before and after using it.
type claudeTraceFrontier struct {
	UID            string
	Transcript     []byte
	Events         []sessionlog.Event
	Path           string
	Session        string
	HookID         string
	HookGeneration int
	Worker         *workerEntry
}

func captureClaudeTraceFrontier(transcriptPath, logPath, nodeID, session string) (claudeTraceFrontier, bool) {
	return captureClaudeTraceFrontierWithRead(transcriptPath, logPath, nodeID, session, os.ReadFile)
}

func captureClaudeTraceFrontierWithRead(transcriptPath, logPath, nodeID, session string, readFile func(string) ([]byte, error)) (claudeTraceFrontier, bool) {
	var empty claudeTraceFrontier
	if transcriptPath == "" || logPath == "" || nodeID == "" || session == "" {
		return empty, false
	}
	raw, err := readFile(transcriptPath)
	if err != nil {
		return empty, false
	}
	log, err := readFile(logPath)
	if err != nil || len(log) == 0 || log[len(log)-1] != '\n' {
		return empty, false
	}
	rawAgain, err := readFile(transcriptPath)
	if err != nil || !bytes.Equal(raw, rawAgain) {
		return empty, false
	}
	logAgain, err := readFile(logPath)
	if err != nil || !bytes.Equal(log, logAgain) {
		return empty, false
	}
	var events []sessionlog.Event
	for _, line := range bytes.Split(log, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev sessionlog.Event
		if json.Unmarshal(line, &ev) != nil || ev.T == "" {
			return empty, false
		}
		events = append(events, ev)
	}
	if !verifyClaudeFrontier(raw, events, transcriptPath, session) {
		return empty, false
	}
	// verifyClaudeFrontier established exactly one meta record.
	for _, ev := range events {
		if ev.T == "meta" {
			if ev.Meta.Node != nodeID || ev.Meta.Agent != "claude" {
				return empty, false
			}
			empty.UID = ev.Meta.UID
			break
		}
	}
	return claudeTraceFrontier{UID: empty.UID, Transcript: raw, Events: events, Path: transcriptPath, Session: session}, true
}

// captureOwnedClaudeFrontier validates ownership and the worker's current
// binding on both sides of the read. No filesystem or worker I/O holds a.mu.
func (a *app) captureOwnedClaudeFrontier(ctx context.Context, n *Node) (claudeTraceFrontier, bool) {
	return a.captureOwnedClaudeFrontierWithRead(ctx, n, os.ReadFile)
}

func (a *app) captureOwnedClaudeFrontierWithRead(ctx context.Context, n *Node, readFile func(string) ([]byte, error)) (claudeTraceFrontier, bool) {
	var empty claudeTraceFrontier
	if n == nil || a.sessionsDir == "" || ctx.Err() != nil {
		return empty, false
	}
	var entry *workerEntry
	if a.workers != nil {
		entry = a.workers.entry(n.ID)
	}
	var hookID string
	var hookGeneration int
	a.mu.Lock()
	valid := a.byID[n.ID] == n && n.Agent == "claude" && !n.Adopted
	path, session := n.Transcript, n.SessionID
	if valid && entry == nil {
		hookID, hookGeneration = a.claudeHookIDLocked(n.ID), a.claudeGenerationLocked(n.ID)
		valid = hookID != "" && a.claudeSupervisionOf(n) == claudeSupStrict
	}
	a.mu.Unlock()
	if !valid {
		return empty, false
	}
	if entry != nil {
		state, err := entry.client.State(ctx)
		if err != nil || !state.HasSession || state.Supervision != string(claudeSupStrict) || state.HookID == "" {
			return empty, false
		}
		path, session = state.Transcript, state.SessionID
		hookID, hookGeneration = state.HookID, state.HookGeneration
	}
	snap, ok := captureClaudeTraceFrontierWithRead(path, a.sessionLogPath(n.ID), n.ID, session, readFile)
	if !ok {
		return empty, false
	}
	if entry != nil {
		state, err := entry.client.State(ctx)
		if err != nil || a.workers.entry(n.ID) != entry || !state.HasSession || state.Supervision != string(claudeSupStrict) || state.Transcript != path || state.SessionID != session || state.HookID != hookID || state.HookGeneration != hookGeneration {
			return empty, false
		}
	}
	if ctx.Err() != nil || (entry == nil && a.workers != nil && a.workers.entry(n.ID) != nil) {
		return empty, false
	}
	a.mu.Lock()
	valid = a.byID[n.ID] == n && n.Agent == "claude" && !n.Adopted
	if entry == nil {
		valid = valid && n.Transcript == path && n.SessionID == session && a.claudeHookIDLocked(n.ID) == hookID && a.claudeGenerationLocked(n.ID) == hookGeneration && a.claudeSupervisionOf(n) == claudeSupStrict
	}
	a.mu.Unlock()
	if !valid {
		return empty, false
	}
	snap.HookID, snap.HookGeneration, snap.Worker = hookID, hookGeneration, entry
	return snap, true
}

// claudeFrontierContinues requires the same binding and complete immutable prefix.
func claudeFrontierContinues(before, after claudeTraceFrontier) bool {
	return before.UID != "" && before.UID == after.UID && before.Path == after.Path && before.Session == after.Session && before.Worker == after.Worker && before.HookID == after.HookID && before.HookGeneration == after.HookGeneration && bytes.HasPrefix(after.Transcript, before.Transcript) && len(after.Events) >= len(before.Events) && reflect.DeepEqual(before.Events, after.Events[:len(before.Events)])
}

// claudePromptAddress requires append-only snapshots and a unique new native
// user record dated after admission. The response, not this row, confirms it.
func claudePromptAddress(before, after claudeTraceFrontier, delivered string, admission time.Time) (promptAddress, bool) {
	var zero promptAddress
	if !claudeFrontierContinues(before, after) {
		return zero, false
	}
	address, ok := queuedPromptAddress(before.Events, after.Events, delivered)
	if !ok || address.UID != before.UID {
		return zero, false
	}
	timestamp, err := time.Parse(time.RFC3339Nano, after.Events[address.Record].Time)
	if err != nil || !timestamp.After(admission) {
		return zero, false
	}
	return address, true
}

func (a *app) awaitClaudePromptAddress(ctx context.Context, n *Node, before claudeTraceFrontier, delivered string, admission time.Time) (promptAddress, bool) {
	return awaitClaudePromptAddressWithCapture(ctx, before, delivered, admission, func() (claudeTraceFrontier, bool) { return a.captureOwnedClaudeFrontier(ctx, n) })
}

func awaitClaudePromptAddressWithCapture(ctx context.Context, before claudeTraceFrontier, delivered string, admission time.Time, capture func() (claudeTraceFrontier, bool)) (promptAddress, bool) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return promptAddress{}, false
		}
		if after, ok := capture(); ok {
			if !claudeFrontierContinues(before, after) {
				return promptAddress{}, false
			}
			if address, ok := claudePromptAddress(before, after, delivered, admission); ok {
				return address, true
			}
		}
		if !waitClaudeTracePoll(ctx, ticker.C) {
			return promptAddress{}, false
		}
	}
}

// waitClaudeTracePoll waits for another read or request cancellation.
func waitClaudeTracePoll(ctx context.Context, tick <-chan time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case <-tick:
		return true
	}
}
