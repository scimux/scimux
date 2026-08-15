// claude_hook.go — private SessionStart hook transport and generation-checked
// Claude transcript binding. Phase 1 of claude-fixes.md: symbols and explicit
// not-implemented errors only. Do not generate settings, bind paths, or change
// launch/clear/relink behavior here until the red suite is accepted.
package app

import (
	"errors"
	"io"
	"path/filepath"
)

// claudeSessionHookCmd is the hidden helper argv token. Product behavior stays
// in this package; cmd/scimux only dispatches.
const claudeSessionHookCmd = "__claude-session-hook"

// errClaudeHookNotImplemented is the Phase 1 seam: tests compile against the
// hook API and fail for missing behavior, not missing symbols.
var errClaudeHookNotImplemented = errors.New("claude session hook: not implemented")

// claudeSessionStartEvent is the documented SessionStart stdin payload.
// Unknown JSON fields are ignored at decode time.
type claudeSessionStartEvent struct {
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	Source         string `json:"source"`
}

func (a *app) claudeHooksDir() string {
	if a == nil || a.storePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(a.storePath), "claude-hooks")
}

func (a *app) claudeHookID(nodeID string) string {
	if a == nil || a.claudeHooks == nil {
		return ""
	}
	return a.claudeHooks[nodeID]
}

func (a *app) claudeGeneration(nodeID string) int {
	if a == nil || a.claudeGens == nil {
		return 0
	}
	return a.claudeGens[nodeID]
}

func (a *app) prepareClaudeHookBundle(nodeID string) (hookID, settingsPath string, err error) {
	return "", "", errClaudeHookNotImplemented
}

func claudeHookSettingsJSON(execPath, hookDir string) ([]byte, error) {
	return nil, errClaudeHookNotImplemented
}

// RunClaudeSessionHook is the hidden helper body: bounded stdin, one inbox
// file, no stdout. Phase 1 returns not implemented and must not create dirs.
func RunClaudeSessionHook(dir string, r io.Reader, stdout, stderr io.Writer) error {
	return errClaudeHookNotImplemented
}

func (a *app) drainClaudeHooks() {}

func (a *app) processClaudeHookEvent(nodeID string, ev claudeSessionStartEvent) error {
	return errClaudeHookNotImplemented
}

func (a *app) archiveClaudeHook(nodeID string) {}
