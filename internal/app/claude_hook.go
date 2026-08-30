// claude_hook.go — private SessionStart hook transport and generation-checked
// Claude transcript binding. Startup, clear, compact, and resume are hook-only;
// directory recency never selects a transcript. Claude's native fork
// (source:"fork" and the /fork command) is not accepted.
package app

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// claudeSessionHookCmd is the hidden helper argv token. Product behavior stays
// in this package; cmd/scimux only dispatches.
const claudeSessionHookCmd = "__claude-session-hook"

const claudeHookStdinLimit = 64 * 1024

var (
	// errClaudeHookNotImplemented remains for any still-unbuilt Phase 3 seam.
	errClaudeHookNotImplemented = errors.New("claude session hook: not implemented")
	errClaudeHookRejected       = errors.New("claude hook rejected")
	errClaudeHookPending        = errors.New("claude hook pending")
	errClaudeHookHeld           = errors.New("claude hook held")
)

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
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claudeHookIDLocked(nodeID)
}

func (a *app) claudeHookIDLocked(nodeID string) string {
	if a.claudeHooks == nil {
		return ""
	}
	return a.claudeHooks[nodeID]
}

func (a *app) claudeGeneration(nodeID string) int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claudeGenerationLocked(nodeID)
}

func (a *app) claudeGenerationLocked(nodeID string) int {
	if a.claudeGens == nil {
		return 0
	}
	return a.claudeGens[nodeID]
}

func (a *app) setPendingClaudeHook(nodeID, hookID string) {
	if nodeID == "" || hookID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingClaudeHooks == nil {
		a.pendingClaudeHooks = map[string]string{}
	}
	a.pendingClaudeHooks[nodeID] = hookID
}

func (a *app) takePendingClaudeHook(nodeID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingClaudeHooks == nil {
		return ""
	}
	id := a.pendingClaudeHooks[nodeID]
	delete(a.pendingClaudeHooks, nodeID)
	return id
}

func newHookID() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func safePathComponent(s string) bool {
	if s == "" || s == "." || s == ".." || s == "archive" {
		return false
	}
	if strings.ContainsAny(s, `/\`) || strings.Contains(s, "..") {
		return false
	}
	return true
}

// hookOps is the injectable filesystem for writeClaudeHookBundle.
// Unexported; exists so TestClaudeHookBundleRollsBackOnFailure can fail
// one operation at a time. The seam pins that a failed prepare leaves
// no bundle directory and does not call noteClaudeStrictCapability —
// an incomplete bundle must never mean the Claude node is supported.
type hookOps struct {
	MkdirAll   func(path string, perm os.FileMode) error
	Chmod      func(name string, mode os.FileMode) error
	WriteFile  func(name string, data []byte, perm os.FileMode) error
	OpenFile   func(name string, flag int, perm os.FileMode) (hookFile, error)
	Executable func() (string, error)
	RemoveAll  func(path string) error
	// Note, if set, is the success-path capability registration.
	// Tests supply a counter; production sets noteClaudeStrictCapability.
	Note func(hookID string)
}

// hookFile is the settings.json handle OpenFile returns. *os.File
// already satisfies it; tests substitute to fail Write, Chmod, or Close.
type hookFile interface {
	Write([]byte) (int, error)
	Chmod(os.FileMode) error
	Close() error
}

func defaultHookOps() hookOps {
	return hookOps{
		MkdirAll:  os.MkdirAll,
		Chmod:     os.Chmod,
		WriteFile: os.WriteFile,
		OpenFile: func(name string, flag int, perm os.FileMode) (hookFile, error) {
			return os.OpenFile(name, flag, perm)
		},
		Executable: os.Executable,
		RemoveAll:  os.RemoveAll,
	}
}

func (a *app) prepareClaudeHookBundle(nodeID string) (hookID, settingsPath string, err error) {
	ops := defaultHookOps()
	ops.Note = a.noteClaudeStrictCapability
	return a.prepareClaudeHookBundleWith(ops, nodeID)
}

func (a *app) prepareClaudeHookBundleWith(ops hookOps, nodeID string) (hookID, settingsPath string, err error) {
	_ = nodeID
	hookID, err = newHookID()
	if err != nil {
		return "", "", err
	}
	root := a.claudeHooksDir()
	if root == "" {
		return "", "", fmt.Errorf("claude hook root: %w", errClaudeHookRejected)
	}
	settingsPath, err = writeClaudeHookBundle(ops, root, hookID)
	if err != nil {
		return "", "", err
	}
	// Disk completeness is knowable at prepare time. Permission/auto-approve
	// capability still waits for SessionStart; this only lets supervision
	// report claude_starting instead of claude_unsupported.
	if ops.Note != nil {
		ops.Note(hookID)
	}
	return hookID, settingsPath, nil
}

// claudeHookBundleSubdirs is the layout writeClaudeHookBundle creates
// under a hook id. Tests range over the same list so a new subdirectory
// adds its two rollback rows automatically.
var claudeHookBundleSubdirs = []string{
	"inbox", "processed", "stop", "notify",
	"perm", filepath.Join("perm", "req"), filepath.Join("perm", "ans"),
	filepath.Join("perm", "processed"), filepath.Join("perm", claudeAskedDirName),
	filepath.Join("perm", claudeShownDirName),
}

func writeClaudeHookBundle(ops hookOps, root, hookID string) (settingsPath string, err error) {
	dir := filepath.Join(root, hookID)
	if err := ops.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := ops.Chmod(dir, 0o700); err != nil {
		ops.RemoveAll(dir)
		return "", err
	}
	for _, sub := range claudeHookBundleSubdirs {
		p := filepath.Join(dir, sub)
		if err := ops.MkdirAll(p, 0o700); err != nil {
			ops.RemoveAll(dir)
			return "", err
		}
		if err := ops.Chmod(p, 0o700); err != nil {
			ops.RemoveAll(dir)
			return "", err
		}
	}
	execPath, err := ops.Executable()
	if err != nil {
		ops.RemoveAll(dir)
		return "", err
	}
	raw, err := claudeHookSettingsJSON(execPath, dir)
	if err != nil {
		ops.RemoveAll(dir)
		return "", err
	}
	// capabilities.json makes support provable from disk: a bundle prepared
	// before a feature lacks its member and stays unsupported for it.
	// "permission" gates answering, "asked" gates the escalation-notice
	// discriminator; a bundle with only the former keeps the pane-geometry
	// attention backstop. "exec" records the binary settings.json just baked in,
	// as JSON rather than a shell string to re-parse, so both gates can check
	// that the hooks can still run at all (claudeBundleExecUsable).
	caps, err := json.Marshal(claudeHookCapabilities{Permission: 1, Asked: 1, Stop: 1, Notify: 1, Exec: execPath})
	if err != nil {
		ops.RemoveAll(dir)
		return "", err
	}
	if err := ops.WriteFile(filepath.Join(dir, "capabilities.json"), caps, 0o600); err != nil {
		ops.RemoveAll(dir)
		return "", err
	}
	settingsPath = filepath.Join(dir, "settings.json")
	f, err := ops.OpenFile(settingsPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		ops.RemoveAll(dir)
		return "", err
	}
	_, werr := f.Write(raw)
	if werr == nil {
		werr = f.Chmod(0o600)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		ops.RemoveAll(dir)
		return "", werr
	}
	return settingsPath, nil
}

func claudeHookSettingsJSON(execPath, hookDir string) ([]byte, error) {
	if execPath == "" || hookDir == "" {
		return nil, fmt.Errorf("claude hook settings: %w", errClaudeHookRejected)
	}
	cmd := shellQuote(execPath) + " " + claudeSessionHookCmd + " --dir " + shellQuote(hookDir)
	// PermissionRequest is registered on every owned launch and is inert
	// while no lease marker exists, so a session can be armed at any later
	// point in its life without a relaunch. PreToolUse is deliberately never
	// registered: it fires for every tool call, decision needed or not.
	// Stop / StopFailure are the official current-turn boundary (after the
	// tool loop; not on user interrupt). SubagentStop is not the main turn.
	permCmd, err := claudePermissionHookCommand(execPath, hookDir)
	if err != nil {
		return nil, err
	}
	stopCmd, err := claudeStopHookCommand(execPath, hookDir)
	if err != nil {
		return nil, err
	}
	notifyCmd, err := claudeNotifyHookCommand(execPath, hookDir)
	if err != nil {
		return nil, err
	}
	entry := func(command string) []any {
		return []any{
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": command},
				},
			},
		}
	}
	notifyEntry := []any{
		map[string]any{
			"matcher": "permission_prompt",
			"hooks": []any{
				map[string]any{"type": "command", "command": notifyCmd},
			},
		},
	}
	doc := map[string]any{
		"hooks": map[string]any{
			"SessionStart":      entry(cmd),
			"PermissionRequest": entry(permCmd),
			"Notification":      notifyEntry,
			"Stop":              entry(stopCmd),
			"StopFailure":       entry(stopCmd),
		},
	}
	return json.Marshal(doc)
}

// RunClaudeSessionHook is the hidden helper body: bounded stdin, one inbox
// file, no stdout. It never creates a missing hook directory.
func RunClaudeSessionHook(dir string, r io.Reader, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	// Claude injects SessionStart stdout into conversation context. Never
	// write a byte on the success or reject path.
	if dir == "" || !filepath.IsAbs(dir) || dir != filepath.Clean(dir) || !safePathComponent(filepath.Base(dir)) {
		return errClaudeHookRejected
	}
	inbox := filepath.Join(dir, "inbox")
	st, err := os.Stat(inbox)
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
	var ev claudeSessionStartEvent
	if json.Unmarshal(payload, &ev) != nil {
		return errClaudeHookRejected
	}
	if ev.HookEventName != "SessionStart" || !allowedClaudeHookSource(ev.Source) {
		return errClaudeHookRejected
	}
	return writeClaudeHookInboxFile(inbox, payload)
}

func allowedClaudeHookSource(src string) bool {
	switch src {
	case "startup", "clear", "compact", "resume":
		return true
	}
	return false
}

func writeClaudeHookInboxFile(inbox string, payload []byte) error {
	for i := 0; i < 8; i++ {
		var rnd [4]byte
		if _, err := io.ReadFull(rand.Reader, rnd[:]); err != nil {
			return err
		}
		name := fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(rnd[:]))
		tmp := filepath.Join(inbox, name+".tmp")
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return err
		}
		_, werr := f.Write(payload)
		if werr == nil {
			werr = f.Sync()
		}
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(tmp)
			return werr
		}
		ready := filepath.Join(inbox, name+".json")
		if err := os.Rename(tmp, ready); err != nil {
			os.Remove(tmp)
			return err
		}
		return sessionlog.SyncParentDir(ready)
	}
	return errClaudeHookRejected
}

func (a *app) drainClaudeHooks() {
	if a == nil || a.claudeHooksDir() == "" {
		return
	}
	a.mu.Lock()
	pairs := make([][2]string, 0, len(a.claudeHooks))
	for nodeID, hookID := range a.claudeHooks {
		pairs = append(pairs, [2]string{nodeID, hookID})
	}
	a.mu.Unlock()
	for _, p := range pairs {
		a.drainClaudeHookInbox(p[0], p[1])
		a.drainClaudeStopInbox(p[0], p[1])
		a.drainClaudeNotifyInbox(p[0], p[1])
	}
}

func (a *app) drainClaudeHookInbox(nodeID, hookID string) {
	if !safePathComponent(hookID) {
		return
	}
	inbox := filepath.Join(a.claudeHooksDir(), hookID, "inbox")
	ents, err := os.ReadDir(inbox)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || !safePathComponent(name) {
			continue
		}
		path := filepath.Join(inbox, name)
		if filepath.Dir(path) != inbox {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil || len(b) > claudeHookStdinLimit {
			a.quarantineClaudeHookEvent(hookID, path, name)
			continue
		}
		var ev claudeSessionStartEvent
		if json.Unmarshal(b, &ev) != nil {
			a.quarantineClaudeHookEvent(hookID, path, name)
			continue
		}
		a.mu.Lock()
		captured := a.claudeGenerationLocked(nodeID)
		a.mu.Unlock()
		err = a.processClaudeHookEventAt(nodeID, ev, captured)
		switch {
		case err == nil:
			a.finishClaudeHookEvent(hookID, path, name, "")
		case errors.Is(err, errClaudeHookRejected):
			a.finishClaudeHookEvent(hookID, path, name, "rejected")
		case errors.Is(err, errClaudeHookHeld):
			a.finishClaudeHookEvent(hookID, path, name, "held")
		default:
			// Persist failure and a not-yet-visible file stay in inbox.
		}
	}
}

func (a *app) finishClaudeHookEvent(hookID, src, name, kind string) {
	destDir := filepath.Join(a.claudeHooksDir(), hookID, "processed")
	if kind != "" {
		destDir = filepath.Join(destDir, kind)
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return
	}
	_ = os.Chmod(destDir, 0o700)
	_ = os.Rename(src, filepath.Join(destDir, name))
}

func (a *app) quarantineClaudeHookEvent(hookID, src, name string) {
	a.finishClaudeHookEvent(hookID, src, name, "rejected")
}

func (a *app) processClaudeHookEvent(nodeID string, ev claudeSessionStartEvent) error {
	a.mu.Lock()
	captured := a.claudeGenerationLocked(nodeID)
	a.mu.Unlock()
	return a.processClaudeHookEventAt(nodeID, ev, captured)
}

func (a *app) processClaudeHookEventAt(nodeID string, ev claudeSessionStartEvent, capturedGen int) error {
	if ev.HookEventName != "SessionStart" || !allowedClaudeHookSource(ev.Source) {
		return errClaudeHookRejected
	}
	lock := a.claudeBindLock(nodeID)
	lock.Lock()
	defer lock.Unlock()
	var err error
	switch ev.Source {
	case "startup":
		err = a.bindClaudeStartup(nodeID, ev, capturedGen)
	case "clear":
		err = a.bindClaudeClear(nodeID, ev, capturedGen)
	case "compact":
		err = a.applyClaudeCompact(nodeID, ev)
	case "resume":
		err = a.applyClaudeResume(nodeID, ev, capturedGen)
	default:
		return fmt.Errorf("claude hook source %s: %w", ev.Source, errClaudeHookRejected)
	}
	if err == nil {
		a.markClaudeHookAck(nodeID)
		a.noteClaudeHookCapabilitiesForNode(nodeID)
	}
	return err
}

// noteClaudeHookCapabilitiesForNode is the live acknowledgement seam. The
// bundle's disk layout is consulted only after Claude proved that this exact
// launch loaded it by running SessionStart successfully.
func (a *app) noteClaudeHookCapabilitiesForNode(nodeID string) {
	a.mu.Lock()
	hookID := a.claudeHookIDLocked(nodeID)
	a.mu.Unlock()
	if hookID == "" {
		return
	}
	a.noteClaudePermCapability(hookID)
	a.noteClaudeAskedCapability(hookID)
	a.noteClaudeStrictCapability(hookID)
}

func (a *app) bindClaudeStartup(nodeID string, ev claudeSessionStartEvent, capturedGen int) error {
	if err := a.validateClaudeHookEvent(nodeID, ev, true); err != nil {
		return err
	}
	a.mu.Lock()
	n := a.byID[nodeID]
	if n == nil {
		a.mu.Unlock()
		return errClaudeHookRejected
	}
	if n.Transcript == ev.TranscriptPath && n.SessionID == ev.SessionID {
		a.mu.Unlock()
		return nil
	}
	hookID := a.claudeHookIDLocked(nodeID)
	gen := a.claudeGenerationLocked(nodeID)
	if gen != capturedGen || n.SessionID != ev.SessionID {
		a.mu.Unlock()
		return errClaudeHookRejected
	}
	if a.pathClaimedLocked(ev.TranscriptPath, nodeID) {
		a.mu.Unlock()
		return errClaudeHookRejected
	}
	a.pathClaims[ev.TranscriptPath] = true
	a.mu.Unlock()

	if err := a.commitClaudeBinding(nodeID, hookID, gen, ev, "startup", "", ""); err != nil {
		a.mu.Lock()
		delete(a.pathClaims, ev.TranscriptPath)
		a.mu.Unlock()
		return err
	}
	return nil
}

func (a *app) bindClaudeClear(nodeID string, ev claudeSessionStartEvent, capturedGen int) error {
	if err := a.validateClaudeHookEvent(nodeID, ev, false); err != nil {
		return err
	}
	a.mu.Lock()
	n := a.byID[nodeID]
	if n == nil {
		a.mu.Unlock()
		return errClaudeHookRejected
	}
	hookID := a.claudeHookIDLocked(nodeID)
	gen := a.claudeGenerationLocked(nodeID)
	if hookID == "" || gen != capturedGen {
		a.mu.Unlock()
		return errClaudeHookRejected
	}
	if n.Transcript == ev.TranscriptPath && n.SessionID == ev.SessionID {
		a.mu.Unlock()
		return nil
	}
	if a.pathClaimedLocked(ev.TranscriptPath, nodeID) {
		a.mu.Unlock()
		a.recordClaudeLaunchError(nodeID, claudeResumeExplain)
		return errClaudeHookRejected
	}
	a.pathClaims[ev.TranscriptPath] = true
	oldPath, oldSID := n.Transcript, n.SessionID
	needPage := oldPath != "" || oldSID != ""
	a.mu.Unlock()

	if needPage {
		if err := a.persistClaudeClearGeneration(n.ID, hookID, capturedGen); err != nil {
			a.mu.Lock()
			delete(a.pathClaims, ev.TranscriptPath)
			a.mu.Unlock()
			a.recordClaudeLaunchError(nodeID, claudeResumeExplain)
			return err
		}
		capturedGen++
	}

	retirePath, retireSID := "", ""
	if needPage {
		retirePath, retireSID = oldPath, oldSID
	}
	if err := a.commitClaudeBinding(nodeID, hookID, capturedGen, ev, "clear", retirePath, retireSID); err != nil {
		a.mu.Lock()
		delete(a.pathClaims, ev.TranscriptPath)
		a.mu.Unlock()
		a.recordClaudeLaunchError(nodeID, claudeResumeExplain)
		return err
	}
	if needPage {
		a.applyRetiredClaudeTranscript(nodeID, oldPath, oldSID, true)
	}
	return nil
}

func (a *app) advanceClaudeClearAfterWeb(n *Node) error {
	if n == nil {
		return errClaudeHookRejected
	}
	lock := a.claudeBindLock(n.ID)
	lock.Lock()
	defer lock.Unlock()
	return a.advanceClaudeClearAfterWebLocked(n)
}

// advanceClaudeClearAfterWebLocked advances the detached generation while
// the caller holds claudeBindLock. The HTTP /clear path holds that lock from
// before Enter through transcript retirement and this append.
func (a *app) advanceClaudeClearAfterWebLocked(n *Node) error {
	if n == nil {
		return errClaudeHookRejected
	}
	a.mu.Lock()
	hookID := a.claudeHookIDLocked(n.ID)
	captured := a.claudeGenerationLocked(n.ID)
	a.mu.Unlock()
	if hookID == "" {
		return errClaudeHookRejected
	}
	return a.persistClaudeClearGeneration(n.ID, hookID, captured)
}

// persistClaudeClearGeneration is called only while claudeBindLock(nodeID) is
// held. Validate before append; after a successful append no competing bind or
// web clear can move the generation before memory is updated.
func (a *app) persistClaudeClearGeneration(nodeID, hookID string, captured int) error {
	next := captured + 1
	a.mu.Lock()
	valid := a.byID[nodeID] != nil && a.claudeHookIDLocked(nodeID) == hookID &&
		a.claudeGenerationLocked(nodeID) == captured
	a.mu.Unlock()
	if !valid {
		return errClaudeHookRejected
	}
	if err := a.appendRecord(storeRecord{Type: "claude-hook", ID: nodeID, HookID: hookID, Generation: next}); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.byID[nodeID] == nil || a.claudeHookIDLocked(nodeID) != hookID || a.claudeGenerationLocked(nodeID) != captured {
		return errClaudeHookRejected
	}
	if a.claudeGens == nil {
		a.claudeGens = map[string]int{}
	}
	a.claudeGens[nodeID] = next
	return nil
}

func (a *app) commitClaudeBinding(nodeID, hookID string, gen int, ev claudeSessionStartEvent, cause, retirePath, retireSID string) error {
	// Candidate first: replay ignores it. The committed claude-binding is
	// written only after generation/node validation still holds, and it
	// carries the old path/session tombstone so both land or neither does.
	candidate := storeRecord{
		Type:       "claude-binding-candidate",
		ID:         nodeID,
		HookID:     hookID,
		Generation: gen,
		SessionID:  ev.SessionID,
		Path:       ev.TranscriptPath,
		Cause:      cause,
	}
	if err := a.appendRecord(candidate); err != nil {
		return err
	}
	if a.claudeAfterCandidate != nil {
		a.claudeAfterCandidate(nodeID)
	}
	a.mu.Lock()
	if a.claudeHookIDLocked(nodeID) != hookID || a.claudeGenerationLocked(nodeID) != gen || a.byID[nodeID] == nil {
		delete(a.pathClaims, ev.TranscriptPath)
		a.mu.Unlock()
		return errClaudeHookRejected
	}
	a.mu.Unlock()

	rec := storeRecord{
		Type:          "claude-binding",
		ID:            nodeID,
		HookID:        hookID,
		Generation:    gen,
		SessionID:     ev.SessionID,
		Path:          ev.TranscriptPath,
		Cause:         cause,
		RetirePath:    retirePath,
		RetireSession: retireSID,
	}
	if err := a.appendRecord(rec); err != nil {
		a.mu.Lock()
		delete(a.pathClaims, ev.TranscriptPath)
		a.mu.Unlock()
		return err
	}
	// The committed record is durable. All generation mutations use the same
	// per-node bind lock, so the validated generation cannot move between the
	// append and this in-memory application.
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.byID[nodeID]
	if n == nil {
		delete(a.pathClaims, ev.TranscriptPath)
		return errClaudeHookRejected
	}
	n.Transcript = ev.TranscriptPath
	n.SessionID = ev.SessionID
	if a.claudeBoundAt == nil {
		a.claudeBoundAt = map[string]time.Time{}
	}
	a.claudeBoundAt[nodeID] = time.Now()
	if retirePath != "" || retireSID != "" {
		a.markDeadTranscriptLocked(nodeID, retirePath, retireSID)
	}
	delete(a.pathClaims, ev.TranscriptPath)
	return nil
}

func (a *app) applyClaudeCompact(nodeID string, ev claudeSessionStartEvent) error {
	if err := a.validateClaudeHookEvent(nodeID, ev, false); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.byID[nodeID]
	if n == nil {
		return errClaudeHookRejected
	}
	if n.SessionID != ev.SessionID || n.Transcript != ev.TranscriptPath {
		return errClaudeHookRejected
	}
	return nil
}

func (a *app) applyClaudeResume(nodeID string, ev claudeSessionStartEvent, capturedGen int) error {
	// Resume is accepted and never becomes inspect. Same identity is a
	// no-op. A valid changed session/path is rebound with the current
	// generation, path-claim, tombstone, persistence, and source-seam
	// rules. The new binding is committed before the old transcript is
	// retired. Failure is an inline error, not inspect, and keeps the
	// previous valid binding.
	if err := a.validateClaudeHookEvent(nodeID, ev, false); err != nil {
		a.recordClaudeLaunchError(nodeID, claudeResumeExplain)
		return err
	}
	a.mu.Lock()
	n := a.byID[nodeID]
	if n == nil {
		a.mu.Unlock()
		return errClaudeHookRejected
	}
	if n.SessionID == ev.SessionID && n.Transcript == ev.TranscriptPath {
		a.mu.Unlock()
		return nil
	}
	hookID := a.claudeHookIDLocked(nodeID)
	gen := a.claudeGenerationLocked(nodeID)
	if hookID == "" || gen != capturedGen {
		a.mu.Unlock()
		a.recordClaudeLaunchError(nodeID, claudeResumeExplain)
		return errClaudeHookRejected
	}
	if a.pathClaimedLocked(ev.TranscriptPath, nodeID) {
		a.mu.Unlock()
		a.recordClaudeLaunchError(nodeID, claudeResumeExplain)
		return errClaudeHookRejected
	}
	a.pathClaims[ev.TranscriptPath] = true
	oldPath, oldSID := n.Transcript, n.SessionID
	a.mu.Unlock()

	if err := a.commitClaudeBinding(nodeID, hookID, gen, ev, "resume", oldPath, oldSID); err != nil {
		a.mu.Lock()
		delete(a.pathClaims, ev.TranscriptPath)
		a.mu.Unlock()
		a.recordClaudeLaunchError(nodeID, claudeResumeExplain)
		return err
	}
	a.applyRetiredClaudeTranscript(nodeID, oldPath, oldSID, false)
	a.clearClaudeLaunchError(nodeID)
	return nil
}

// applyRetiredClaudeTranscript drops poller state for a path that the
// committed binding already tombstoned. It does not append store records:
// a failed extra append must not be the only tombstone, and a successful
// commit already persisted retire_path / retire_session.
func (a *app) applyRetiredClaudeTranscript(nodeID, oldPath, oldSID string, pageTurn bool) {
	if oldPath == "" && oldSID == "" {
		return
	}
	a.mu.Lock()
	n := a.byID[nodeID]
	if n != nil && n.Transcript != oldPath {
		delete(a.tailers, nodeID)
		delete(a.chatMark, nodeID)
		delete(a.staleChat, nodeID)
		delete(a.mirrors, nodeID)
		delete(a.piMirrors, nodeID)
		if n.Transcript != "" {
			if a.claudeBoundAt == nil {
				a.claudeBoundAt = map[string]time.Time{}
			}
			a.claudeBoundAt[nodeID] = time.Now()
		} else {
			delete(a.claudeBoundAt, nodeID)
		}
	}
	a.mu.Unlock()
	if !pageTurn || a.sessionsDir == "" || n == nil {
		return
	}
	logPath := a.sessionLogPath(nodeID)
	if _, err := os.Stat(logPath); err != nil {
		return
	}
	a.snapshotClosingStation(n)
	w := &sessionlog.Writer{Path: logPath}
	if err := w.Append(sessionlog.NewClearSource("")); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: clear seam for %s: %v\n", nodeID, err)
	}
}

func (a *app) validateClaudeHookEvent(nodeID string, ev claudeSessionStartEvent, startup bool) error {
	a.mu.Lock()
	n := a.byID[nodeID]
	hookID := a.claudeHookIDLocked(nodeID)
	a.mu.Unlock()
	if n == nil || !safePathComponent(hookID) {
		return errClaudeHookRejected
	}
	if ev.HookEventName != "SessionStart" || !allowedClaudeHookSource(ev.Source) {
		return errClaudeHookRejected
	}
	if ev.SessionID == "" || !sessionIDArgRe.MatchString(ev.SessionID) {
		return errClaudeHookRejected
	}
	if startup && n.SessionID != ev.SessionID {
		return errClaudeHookRejected
	}
	path := ev.TranscriptPath
	if path == "" || !filepath.IsAbs(path) || path != filepath.Clean(path) {
		return errClaudeHookRejected
	}
	if filepath.Base(path) != ev.SessionID+".jsonl" {
		return errClaudeHookRejected
	}
	root := filepath.Join(a.home, ".claude", "projects")
	if !withinDir(root, path) {
		return errClaudeHookRejected
	}
	if a.isDeadTranscript(nodeID, path, ev.SessionID) {
		return errClaudeHookRejected
	}
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return errClaudeHookPending
		}
		return errClaudeHookRejected
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return errClaudeHookRejected
	}
	rootResolved, rerr := filepath.EvalSymlinks(root)
	if rerr != nil {
		rootResolved = filepath.Clean(root)
	}
	if !withinDir(rootResolved, resolved) {
		return errClaudeHookRejected
	}
	st, err := os.Stat(resolved)
	if err != nil || !st.Mode().IsRegular() {
		return errClaudeHookRejected
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*", ev.SessionID+".jsonl"))
	if len(matches) != 1 {
		return errClaudeHookRejected
	}
	if transcriptContradictsSession(resolved, ev.SessionID) {
		return errClaudeHookRejected
	}
	return nil
}

const claudeTxSidScanLines = 64

func transcriptContradictsSession(path, sid string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 256*1024)
	for i := 0; i < claudeTxSidScanLines && sc.Scan(); i++ {
		var r struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		if r.SessionID != "" && r.SessionID != sid {
			return true
		}
	}
	return false
}

func (a *app) archiveClaudeHook(nodeID string) {
	a.mu.Lock()
	hookID := a.claudeHookIDLocked(nodeID)
	delete(a.claudeHooks, nodeID)
	delete(a.claudeGens, nodeID)
	delete(a.pendingClaudeHooks, nodeID)
	delete(a.claudeAck, nodeID)
	delete(a.claudeLaunchErr, nodeID)
	delete(a.claudeTurns, nodeID)
	delete(a.claudeClosing, nodeID)
	delete(a.claudeDialogNote, nodeID)
	a.mu.Unlock()
	a.archiveHookBundle(hookID)
}

func (a *app) archiveHookBundle(hookID string) {
	if !safePathComponent(hookID) {
		return
	}
	root := a.claudeHooksDir()
	if root == "" {
		return
	}
	src := filepath.Join(root, hookID)
	if _, err := os.Stat(src); err != nil {
		return
	}
	dir := filepath.Join(root, "archive")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: archive claude hook %s: %v\n", hookID, err)
		return
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(src, filepath.Join(dir, hookID+"."+stamp)); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: archive claude hook %s: %v\n", hookID, err)
	}
}

func runClaudeSessionHookMain(args []string) int {
	dir := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			dir = args[i+1]
			i++
		}
	}
	if err := RunClaudeSessionHook(dir, os.Stdin, io.Discard, io.Discard); err != nil {
		return 1
	}
	return 0
}

func (a *app) cleanupOrphanClaudeHooks() {
	root := a.claudeHooksDir()
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	inUse := map[string]bool{}
	a.mu.Lock()
	for _, hookID := range a.claudeHooks {
		inUse[hookID] = true
	}
	for _, hookID := range a.pendingClaudeHooks {
		inUse[hookID] = true
	}
	a.mu.Unlock()
	for _, e := range ents {
		if !e.IsDir() || e.Name() == "archive" {
			continue
		}
		if inUse[e.Name()] {
			continue
		}
		a.archiveHookBundle(e.Name())
	}
}

func (a *app) claudeReadyPath(n *Node) (string, bool) {
	if n == nil || n.SessionID == "" {
		return "", false
	}
	if n.Transcript != "" {
		return n.Transcript, true
	}
	return transcript.FindClaudeTranscript(a.home, n.SessionID)
}
