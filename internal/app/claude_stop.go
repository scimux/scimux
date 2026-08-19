package app

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// claudeStopHookCmd is the hidden helper argv token for Claude's official
// Stop / StopFailure events — the deterministic current-turn boundary.
const claudeStopHookCmd = "__claude-stop-hook"

// claudeStopEvent is the documented Stop / StopFailure stdin payload.
// Unknown fields are ignored. last_assistant_message is deliberately not
// persisted: the helper only needs the event name, session, and whether
// another Stop hook already asked Claude to continue.
type claudeStopEvent struct {
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// claudeStopNotice is what the helper leaves for the running scimux process.
// Lease is the arm marker current when Stop fired: drain disarms only if
// that same lease is still current, so a late Stop from turn N cannot
// revoke turn N+1.
type claudeStopNotice struct {
	Lease         string `json:"lease"`
	Turn          string `json:"turn,omitempty"`
	TurnGen       int    `json:"turn_gen,omitempty"`
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	At            string `json:"at,omitempty"`
}

// claudeStopNoticeTTL is the leak backstop for a Stop file that is never
// drained (retired binding, scimux down). Past it the notice is quarantined
// and cannot settle a later lease.
const claudeStopNoticeTTL = 30 * time.Minute

func claudeStopHookCommand(execPath, hookDir string) (string, error) {
	if execPath == "" || hookDir == "" {
		return "", errClaudeHookRejected
	}
	return shellQuote(execPath) + " " + claudeStopHookCmd + " --dir " + shellQuote(hookDir), nil
}

// RunClaudeStopHook is the hidden helper body. It never writes stdout: a
// Stop hook that prints decision:"block" (or exits 2) would keep the turn
// alive. Invalid input fails closed. An unarmed session, or a Stop that is
// only continuing a previous stop hook, is a silent success — there is no
// lease to end.
func RunClaudeStopHook(dir string, r io.Reader, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if dir == "" || !filepath.IsAbs(dir) || dir != filepath.Clean(dir) || !safePathComponent(filepath.Base(dir)) {
		return errClaudeHookRejected
	}
	stopDir := filepath.Join(dir, "stop")
	st, err := os.Stat(stopDir)
	if err != nil || !st.IsDir() {
		return errClaudeHookRejected
	}
	limited := io.LimitReader(r, claudeHookStdinLimit+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return errClaudeHookRejected
	}
	if len(payload) > claudeHookStdinLimit {
		return errClaudeHookRejected
	}
	var ev claudeStopEvent
	if json.Unmarshal(payload, &ev) != nil {
		return errClaudeHookRejected
	}
	if ev.HookEventName != "Stop" && ev.HookEventName != "StopFailure" {
		return errClaudeHookRejected
	}
	if ev.HookEventName == "Stop" && ev.StopHookActive {
		return nil
	}
	lease := ""
	var turn claudeAcceptedTurn
	if perm, err := claudePermDir(dir); err == nil {
		lease, _ = readClaudePermLease(perm, time.Now())
		turn, _ = readClaudeAcceptedTurn(perm)
	}
	// Always emit a session-scoped Stop notice, armed or not, so drain can
	// clear epochs/asks/attention even when no lease exists. A late Stop
	// from another session or another accepted turn is quarantined on drain.
	raw, err := json.Marshal(claudeStopNotice{
		Lease:         lease,
		Turn:          turn.Turn,
		TurnGen:       turn.Gen,
		HookEventName: ev.HookEventName,
		SessionID:     ev.SessionID,
		At:            time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return errClaudeHookRejected
	}
	return writeClaudeHookInboxFile(stopDir, raw)
}

func runClaudeStopHookMain(args []string) int {
	dir := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			dir = args[i+1]
			i++
		}
	}
	_ = RunClaudeStopHook(dir, os.Stdin, io.Discard, io.Discard)
	return 0
}

func (a *app) drainClaudeStopInbox(nodeID, hookID string) {
	if !safePathComponent(hookID) {
		return
	}
	stopDir := filepath.Join(a.claudeHooksDir(), hookID, "stop")
	ents, err := os.ReadDir(stopDir)
	if err != nil {
		return
	}
	a.mu.Lock()
	sid := ""
	if n := a.byID[nodeID]; n != nil {
		sid = n.SessionID
	}
	a.mu.Unlock()
	now := time.Now()
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || !safePathComponent(name) {
			continue
		}
		path := filepath.Join(stopDir, name)
		if filepath.Dir(path) != stopDir {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil || len(b) > claudeHookStdinLimit {
			a.quarantineClaudeHookEvent(hookID, path, name)
			continue
		}
		var n claudeStopNotice
		if json.Unmarshal(b, &n) != nil ||
			(n.HookEventName != "Stop" && n.HookEventName != "StopFailure") {
			a.quarantineClaudeHookEvent(hookID, path, name)
			continue
		}
		if n.At != "" {
			at, err := time.Parse(time.RFC3339Nano, n.At)
			if err != nil || now.Sub(at) > claudeStopNoticeTTL {
				a.quarantineClaudeHookEvent(hookID, path, name)
				continue
			}
		}
		if sid != "" && n.SessionID != "" && n.SessionID != sid {
			a.quarantineClaudeHookEvent(hookID, path, name)
			continue
		}
		g := a.autoGateFor(nodeID)
		g.Lock()
		current := a.claudeAcceptedTurnOf(nodeID)
		closing := a.claudeClosingTurnOf(nodeID)
		if !claudeStopMatchesTurn(n, current, closing) {
			// Late Stop from turn N after N+1 began: consume it, mutate nothing.
			a.finishClaudeHookEvent(hookID, path, name, "stop")
			g.Unlock()
			continue
		}
		// Lease settle is an additional fence. Permission cleanup is
		// unconditional for the matching turn, even when no lease was armed.
		if n.Lease != "" {
			a.settleAutoApproveAfterTurnLocked(nodeID, n.Lease)
		}
		a.mu.Lock()
		node := a.byID[nodeID]
		a.mu.Unlock()
		if node != nil {
			a.resetClaudePermissionTurnLocked(node)
		}
		a.finishClaudeHookEvent(hookID, path, name, "stop")
		g.Unlock()
	}
}

// bundleSupportsStop reports whether a bundle was prepared with the Stop hook
// layout. A pre-Stop launch lacks the member and stays on the transcript
// end_turn fallback, which is visible from disk.
func bundleSupportsStop(bundle string) bool {
	caps, ok := readClaudeHookCapabilities(bundle)
	if !ok || caps.Stop < 1 {
		return false
	}
	if !claudeBundleExecUsable(caps) {
		return false
	}
	st, err := os.Stat(filepath.Join(bundle, "stop"))
	return err == nil && st.IsDir()
}
