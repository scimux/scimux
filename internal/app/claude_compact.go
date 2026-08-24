// claude_compact.go — PreCompact / PostCompact hook transport.
//
// These events are passive UX evidence: PreCompact writes a session-scoped
// marker so the selected chat can say Claude is compacting; PostCompact
// removes it. The helper never blocks compaction, never retains custom
// compact instructions or compact summaries, and never writes session-log
// events or source seams.
package app

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// claudeCompactHookCmd is the hidden helper argv token for both PreCompact
// and PostCompact. cmd/scimux only dispatches.
const claudeCompactHookCmd = "__claude-compact-hook"

// claudeCompactStdinLimit is larger than the other hook stdin caps because
// PostCompact may carry a compact_summary. The helper skips that value and
// never persists it; the bound is a leak backstop, not a place to store the
// summary.
const claudeCompactStdinLimit = 4 << 20

// claudeCompactTTL is the leak backstop for an active marker that is never
// cleared (PostCompact missed, scimux down). Past it the chat ignores the
// marker rather than leaving a permanent "compacting" status.
const claudeCompactTTL = 30 * time.Minute

const claudeCompactMarkerMax = 4096

type claudeCompactEvent struct {
	HookEventName string
	SessionID     string
	Trigger       string
}

type claudeCompactMarker struct {
	SessionID string `json:"session_id"`
	Trigger   string `json:"trigger"`
	At        string `json:"at"`
}

func claudeCompactHookCommand(execPath, hookDir string) (string, error) {
	if execPath == "" || hookDir == "" {
		return "", errClaudeHookRejected
	}
	return shellQuote(execPath) + " " + claudeCompactHookCmd + " --dir " + shellQuote(hookDir), nil
}

func claudeCompactDir(bundle string) (string, error) {
	if bundle == "" {
		return "", errClaudeHookRejected
	}
	dir := filepath.Join(bundle, "compact")
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", errClaudeHookRejected
	}
	return dir, nil
}

func claudeCompactActivePath(bundle string) string {
	return filepath.Join(bundle, "compact", "active.json")
}

func bundleSupportsCompact(bundle string) bool {
	caps, ok := readClaudeHookCapabilities(bundle)
	if !ok || caps.Compact < 1 {
		return false
	}
	if !claudeBundleExecUsable(caps) {
		return false
	}
	_, err := claudeCompactDir(bundle)
	return err == nil
}

func allowedClaudeCompactTrigger(trigger string) bool {
	return trigger == "manual" || trigger == "auto"
}

func decodeClaudeCompactEvent(r io.Reader) (claudeCompactEvent, error) {
	var empty claudeCompactEvent
	limited := &io.LimitedReader{R: r, N: claudeCompactStdinLimit + 1}
	dec := json.NewDecoder(limited)
	tok, err := dec.Token()
	if err != nil {
		return empty, errClaudeHookRejected
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '{' {
		return empty, errClaudeHookRejected
	}
	var ev claudeCompactEvent
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return empty, errClaudeHookRejected
		}
		key, ok := keyTok.(string)
		if !ok {
			return empty, errClaudeHookRejected
		}
		switch key {
		case "hook_event_name":
			ev.HookEventName, err = decodeJSONStringToken(dec)
		case "session_id":
			ev.SessionID, err = decodeJSONStringToken(dec)
		case "trigger":
			ev.Trigger, err = decodeJSONStringToken(dec)
		default:
			// compact_summary / custom_instructions and any other extra
			// members are skipped and never stored.
			err = skipJSONValue(dec)
		}
		if err != nil {
			return empty, errClaudeHookRejected
		}
	}
	if _, err := dec.Token(); err != nil {
		return empty, errClaudeHookRejected
	}
	// Require a true end of input. A second JSON value, trailing garbage,
	// or anything other than whitespace must not be treated as a valid event.
	// Token skips whitespace, so a single object plus ordinary trailing
	// newlines still succeeds.
	if _, err := dec.Token(); err != io.EOF {
		return empty, errClaudeHookRejected
	}
	// Decoder buffering can hide unread stdin behind the first object.
	// Drain the capped reader so the limit applies to the whole payload,
	// not just the bytes the decoder happened to pull while parsing.
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return empty, errClaudeHookRejected
	}
	if limited.N <= 0 {
		return empty, errClaudeHookRejected
	}
	return ev, nil
}

func decodeJSONStringToken(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	s, ok := tok.(string)
	if !ok {
		return "", errClaudeHookRejected
	}
	return s, nil
}

func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		for dec.More() {
			if _, err := dec.Token(); err != nil {
				return err
			}
			if err := skipJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := skipJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	default:
		return errClaudeHookRejected
	}
}

// RunClaudeCompactHook is the hidden helper body. It never writes stdout or
// stderr: a PreCompact hook that exits non-zero or returns decision:"block"
// would stop compaction, which scimux must never do. Invalid input fails
// closed without mutating the marker. The command-main wrapper always exits
// zero even when this returns an error.
func RunClaudeCompactHook(dir string, r io.Reader, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if dir == "" || !filepath.IsAbs(dir) || dir != filepath.Clean(dir) || !safePathComponent(filepath.Base(dir)) {
		return errClaudeHookRejected
	}
	if _, err := claudeCompactDir(dir); err != nil {
		return errClaudeHookRejected
	}
	ev, err := decodeClaudeCompactEvent(r)
	if err != nil {
		return errClaudeHookRejected
	}
	if ev.HookEventName != "PreCompact" && ev.HookEventName != "PostCompact" {
		return errClaudeHookRejected
	}
	if ev.SessionID == "" || !allowedClaudeCompactTrigger(ev.Trigger) {
		return errClaudeHookRejected
	}
	switch ev.HookEventName {
	case "PreCompact":
		return writeClaudePermFile(claudeCompactActivePath(dir), claudeCompactMarker{
			SessionID: ev.SessionID,
			Trigger:   ev.Trigger,
			At:        time.Now().UTC().Format(time.RFC3339Nano),
		})
	case "PostCompact":
		cur, ok := readClaudeCompactMarkerFile(dir)
		if !ok {
			return nil
		}
		if cur.SessionID != ev.SessionID {
			return errClaudeHookRejected
		}
		return removeClaudeCompactMarker(dir)
	}
	return errClaudeHookRejected
}

func removeClaudeCompactMarker(bundle string) error {
	path := claudeCompactActivePath(bundle)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errClaudeHookRejected
	}
	_ = sessionlog.SyncParentDir(path)
	return nil
}

func readClaudeCompactMarkerFile(bundle string) (claudeCompactMarker, bool) {
	var empty claudeCompactMarker
	if bundle == "" {
		return empty, false
	}
	path := claudeCompactActivePath(bundle)
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > claudeCompactMarkerMax {
		return empty, false
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 || len(b) > claudeCompactMarkerMax {
		return empty, false
	}
	var m claudeCompactMarker
	if json.Unmarshal(b, &m) != nil {
		return empty, false
	}
	if m.SessionID == "" || !allowedClaudeCompactTrigger(m.Trigger) {
		return empty, false
	}
	if m.At == "" {
		return empty, false
	}
	if _, err := time.Parse(time.RFC3339Nano, m.At); err != nil {
		if _, err2 := time.Parse(time.RFC3339, m.At); err2 != nil {
			return empty, false
		}
	}
	return m, true
}

// claudeCompactingState is the chat-server projection of compact/active.json.
// It is evidence for the selected chat only: never attention, never liveness,
// never a terminal-forcing signal, never a session-log event.
func (a *app) claudeCompactingState(n *Node) (bool, string) {
	if a == nil || n == nil || n.Agent != "claude" || n.transport() != "tmux" {
		return false, ""
	}
	a.mu.Lock()
	ended := n.EndedAt != ""
	live := a.live[n.ID]
	sup := a.claudeSupervisionOf(n)
	hookID := a.claudeHookIDLocked(n.ID)
	sid := n.SessionID
	a.mu.Unlock()
	if ended || live == "exited" || sup != claudeSupStrict {
		return false, ""
	}
	bundle := a.claudeHookBundlePath(hookID)
	if !bundleSupportsCompact(bundle) {
		return false, ""
	}
	m, ok := liveClaudeCompactMarker(bundle, sid, time.Now())
	if !ok {
		return false, ""
	}
	return true, m.Trigger
}

func liveClaudeCompactMarker(bundle, session string, now time.Time) (claudeCompactMarker, bool) {
	m, ok := readClaudeCompactMarkerFile(bundle)
	if !ok {
		return m, false
	}
	if session == "" || m.SessionID != session {
		return claudeCompactMarker{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, m.At)
	if err != nil {
		at, err = time.Parse(time.RFC3339, m.At)
		if err != nil {
			return claudeCompactMarker{}, false
		}
	}
	if now.IsZero() {
		now = time.Now()
	}
	if now.Sub(at) > claudeCompactTTL || at.After(now.Add(time.Minute)) {
		return claudeCompactMarker{}, false
	}
	return m, true
}

func runClaudeCompactHookMain(args []string) int {
	dir := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			dir = args[i+1]
			i++
		}
	}
	_ = RunClaudeCompactHook(dir, os.Stdin, io.Discard, io.Discard)
	return 0
}
