// claude_notify.go — Notification(permission_prompt) transport.
//
// PermissionRequest fires before hook aggregation and writes an asked notice
// for every escalated decision. Notification(permission_prompt) fires only
// when a permission dialog actually remained visible. Claude's payload has
// no PermissionRequest id and no tool_use id, so a notification proves only
// that *a* dialog is on screen. scimux mints a visible-dialog epoch for the
// UI action; it does not claim exact request correlation.
package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const claudeNotifyHookCmd = "__claude-notify-hook"

// claudeAnsweredDirName holds request ids that were successfully answered
// through the action bar so a later poll cannot reopen them.
const claudeAnsweredDirName = "answered"

type claudeNotifyEvent struct {
	HookEventName    string `json:"hook_event_name"`
	NotificationType string `json:"notification_type"`
	SessionID        string `json:"session_id"`
	Message          string `json:"message"`
	Title            string `json:"title"`
}

// claudeNotifyNotice is what the helper leaves for drain. Turn/TurnGen are
// copied from the accepted-turn marker at fire time so a leftover
// notification cannot mint an epoch for the next turn.
type claudeNotifyNotice struct {
	HookEventName    string `json:"hook_event_name"`
	NotificationType string `json:"notification_type"`
	SessionID        string `json:"session_id"`
	Message          string `json:"message"`
	Title            string `json:"title"`
	Turn             string `json:"turn,omitempty"`
	TurnGen          int    `json:"turn_gen,omitempty"`
	At               string `json:"at,omitempty"`
}

func claudeNotifyHookCommand(execPath, hookDir string) (string, error) {
	if execPath == "" || hookDir == "" {
		return "", errClaudeHookRejected
	}
	return shellQuote(execPath) + " " + claudeNotifyHookCmd + " --dir " + shellQuote(hookDir), nil
}

func claudeAnsweredPath(perm, id string) string {
	return filepath.Join(perm, "processed", claudeAnsweredDirName, id+".json")
}

func claudeRequestAnswered(perm, id string) bool {
	if perm == "" || id == "" {
		return false
	}
	st, err := os.Stat(claudeAnsweredPath(perm, id))
	return err == nil && st.Mode().IsRegular()
}

func markClaudeRequestAnswered(perm, id string) {
	if perm == "" || id == "" || !safePathComponent(id) {
		return
	}
	path := claudeAnsweredPath(perm, id)
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = writeClaudePermFile(path, map[string]string{
		"id": id,
		"at": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// claudeVisibleEpoch is a server-minted token proving that
// Notification(permission_prompt) reported a visible dialog. Title comes
// from the notification payload when present. HintTool is an optional
// label from a standing ask at mint time — never a request identity,
// never a deletion target.
type claudeVisibleEpoch struct {
	Epoch    string `json:"epoch"`
	Session  string `json:"session,omitempty"`
	At       string `json:"at,omitempty"`
	Title    string `json:"title,omitempty"`
	HintTool string `json:"hint_tool,omitempty"`
}

type claudeTurnClosed struct {
	Session string `json:"session,omitempty"`
	At      string `json:"at,omitempty"`
	Gen     int    `json:"gen,omitempty"`
	Turn    string `json:"turn,omitempty"`
}

func claudeVisibleEpochPath(perm string) string {
	return filepath.Join(perm, "visible.json")
}

func readClaudeVisibleEpoch(perm string) (claudeVisibleEpoch, bool) {
	var empty claudeVisibleEpoch
	if perm == "" {
		return empty, false
	}
	b, err := os.ReadFile(claudeVisibleEpochPath(perm))
	if err != nil || len(b) == 0 {
		return empty, false
	}
	var ep claudeVisibleEpoch
	if json.Unmarshal(b, &ep) != nil || ep.Epoch == "" || !safePathComponent(ep.Epoch) {
		return empty, false
	}
	return ep, true
}

// liveClaudeVisibleEpoch returns the current epoch only if it still refers to
// a standing dialog. A retired hint ask means that Notification's dialog
// closed; leftover queued asks need a later Notification, not this epoch.
func liveClaudeVisibleEpoch(perm, session string) (claudeVisibleEpoch, bool) {
	ep, ok := readClaudeVisibleEpoch(perm)
	if !ok {
		return ep, false
	}
	if session != "" && ep.Session != "" && ep.Session != session {
		return claudeVisibleEpoch{}, false
	}
	if ep.HintTool != "" {
		if _, ok := newestStandingAsk(perm, session, time.Now()); !ok {
			// Transcript/tool evidence retired every standing ask. The epoch
			// is leftover visibility, not a request identity — drop it.
			clearClaudeVisibleEpoch(perm)
			return claudeVisibleEpoch{}, false
		}
	}
	return ep, true
}

func clearClaudeVisibleEpoch(perm string) {
	if perm == "" {
		return
	}
	_ = os.Remove(claudeVisibleEpochPath(perm))
}

func newClaudeDialogEpoch() (string, error) {
	var rnd [8]byte
	if _, err := io.ReadFull(rand.Reader, rnd[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(rnd[:]), nil
}

func newestStandingAsk(perm, session string, now time.Time) (askedFile, bool) {
	files := readClaudeAskedFiles(perm, now)
	for _, f := range files { // newest first
		id := strings.TrimSuffix(filepath.Base(f.path), ".json")
		if !safePathComponent(id) || claudeRequestAnswered(perm, id) {
			continue
		}
		if session != "" && f.note.Session != "" && f.note.Session != session {
			continue
		}
		return f, true
	}
	return askedFile{}, false
}

// mintClaudeVisibleEpoch mints an unlabeled visible-dialog epoch. Test seam:
// production always mints with a real title and message.
func mintClaudeVisibleEpoch(perm, session string) (claudeVisibleEpoch, error) {
	return mintClaudeVisibleEpochLabeled(perm, session, "", "")
}

func mintClaudeVisibleEpochLabeled(perm, session, title, message string) (claudeVisibleEpoch, error) {
	if perm == "" {
		return claudeVisibleEpoch{}, errClaudeHookRejected
	}
	if st, err := os.Stat(perm); err != nil || !st.IsDir() {
		return claudeVisibleEpoch{}, errClaudeHookRejected
	}
	epoch, err := newClaudeDialogEpoch()
	if err != nil {
		return claudeVisibleEpoch{}, err
	}
	label := strings.TrimSpace(title)
	if label == "" {
		label = strings.TrimSpace(message)
	}
	ep := claudeVisibleEpoch{
		Epoch:   epoch,
		Session: session,
		At:      time.Now().UTC().Format(time.RFC3339Nano),
		Title:   label,
	}
	if f, ok := newestStandingAsk(perm, session, time.Now()); ok {
		ep.HintTool = f.note.Tool
		if ep.Title == "" {
			ep.Title = f.note.Tool
		}
	}
	if err := writeClaudePermFile(claudeVisibleEpochPath(perm), ep); err != nil {
		return claudeVisibleEpoch{}, err
	}
	return ep, nil
}

func claudeTurnClosedPath(perm string) string {
	return filepath.Join(perm, "closed.json")
}

func readClaudeTurnClosed(perm string) (claudeTurnClosed, bool) {
	var empty claudeTurnClosed
	if perm == "" {
		return empty, false
	}
	b, err := os.ReadFile(claudeTurnClosedPath(perm))
	if err != nil || len(b) == 0 {
		return empty, false
	}
	var c claudeTurnClosed
	if json.Unmarshal(b, &c) != nil {
		return empty, false
	}
	return c, true
}

func markClaudeTurnClosed(perm, session, turn string, gen int) {
	if perm == "" {
		return
	}
	prev, _ := readClaudeTurnClosed(perm)
	next := prev.Gen + 1
	if gen > next {
		next = gen
	}
	_ = writeClaudePermFile(claudeTurnClosedPath(perm), claudeTurnClosed{
		Session: session,
		At:      time.Now().UTC().Format(time.RFC3339Nano),
		Gen:     next,
		Turn:    turn,
	})
}

func tombstoneClaudeAskedForSession(perm, session string) {
	if perm == "" {
		return
	}
	for _, f := range readClaudeAskedFiles(perm, time.Now()) {
		id := strings.TrimSuffix(filepath.Base(f.path), ".json")
		if !safePathComponent(id) {
			continue
		}
		if session != "" && f.note.Session != "" && f.note.Session != session {
			continue
		}
		markClaudeRequestAnswered(perm, id)
		_ = os.Remove(f.path)
	}
}

// RunClaudeNotifyHook is the hidden helper body. Notification is fire-and-
// forget: no stdout, no decision. Only permission_prompt is accepted.
func RunClaudeNotifyHook(dir string, r io.Reader, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if dir == "" || !filepath.IsAbs(dir) || dir != filepath.Clean(dir) || !safePathComponent(filepath.Base(dir)) {
		return errClaudeHookRejected
	}
	notifyDir := filepath.Join(dir, "notify")
	st, err := os.Stat(notifyDir)
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
	var ev claudeNotifyEvent
	if json.Unmarshal(payload, &ev) != nil {
		return errClaudeHookRejected
	}
	if ev.HookEventName != "Notification" || ev.NotificationType != "permission_prompt" {
		return errClaudeHookRejected
	}
	turn, _ := readClaudeAcceptedTurn(filepath.Join(dir, "perm"))
	notice := claudeNotifyNotice{
		HookEventName:    ev.HookEventName,
		NotificationType: ev.NotificationType,
		SessionID:        ev.SessionID,
		Message:          ev.Message,
		Title:            ev.Title,
		Turn:             turn.Turn,
		TurnGen:          turn.Gen,
		At:               time.Now().UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(notice)
	if err != nil {
		return errClaudeHookRejected
	}
	return writeClaudeHookInboxFile(notifyDir, raw)
}

func runClaudeNotifyHookMain(args []string) int {
	dir := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			dir = args[i+1]
			i++
		}
	}
	_ = RunClaudeNotifyHook(dir, os.Stdin, io.Discard, io.Discard)
	return 0
}

func (a *app) drainClaudeNotifyInbox(nodeID, hookID string) {
	if !safePathComponent(hookID) {
		return
	}
	notifyDir := filepath.Join(a.claudeHooksDir(), hookID, "notify")
	ents, err := os.ReadDir(notifyDir)
	if err != nil {
		return
	}
	a.mu.Lock()
	sid := ""
	if n := a.byID[nodeID]; n != nil {
		sid = n.SessionID
	}
	bundle := a.claudePermBundleLocked(nodeID)
	a.mu.Unlock()
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || !safePathComponent(name) {
			continue
		}
		path := filepath.Join(notifyDir, name)
		if filepath.Dir(path) != notifyDir {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil || len(b) > claudeHookStdinLimit {
			a.quarantineClaudeHookEvent(hookID, path, name)
			continue
		}
		var ev claudeNotifyNotice
		if json.Unmarshal(b, &ev) != nil || ev.HookEventName != "Notification" ||
			ev.NotificationType != "permission_prompt" {
			a.quarantineClaudeHookEvent(hookID, path, name)
			continue
		}
		if sid != "" && ev.SessionID != "" && ev.SessionID != sid {
			a.quarantineClaudeHookEvent(hookID, path, name)
			continue
		}
		perm := ""
		if bundle != "" {
			perm = filepath.Join(bundle, "perm")
		}
		g := a.autoGateFor(nodeID)
		g.Lock()
		current, _ := readClaudeAcceptedTurn(perm)
		if current.Turn == "" {
			current = a.claudeAcceptedTurnOf(nodeID)
		}
		closed, hadClosed := readClaudeTurnClosed(perm)
		if !claudeNotifyBelongsToCurrentTurn(ev, current, closed, hadClosed) {
			a.noteClaudeDialogAmbiguous(nodeID, "A permission prompt could not be matched to the current turn. Open Terminal if a dialog is still visible.")
			a.quarantineClaudeHookEvent(hookID, path, name)
			g.Unlock()
			continue
		}
		if hadClosed && (closed.Turn != "" || closed.Gen > 0) {
			if _, ok := newestStandingAsk(perm, ev.SessionID, time.Now()); !ok {
				// After a turn boundary, a notify with no live asked notice
				// is a late leftover from the completed turn — do not mint
				// or reopen a dialog epoch.
				a.quarantineClaudeHookEvent(hookID, path, name)
				g.Unlock()
				continue
			}
		}
		if _, err := mintClaudeVisibleEpochLabeled(perm, ev.SessionID, ev.Title, ev.Message); err != nil {
			// Bundle not ready: leave the notification for the next drain.
			g.Unlock()
			continue
		}
		a.clearClaudeDialogNote(nodeID)
		a.finishClaudeHookEvent(hookID, path, name, "notify")
		g.Unlock()
	}
}

func (a *app) dropClaudeNotifyInbox(hookID string) {
	if !safePathComponent(hookID) {
		return
	}
	notifyDir := filepath.Join(a.claudeHooksDir(), hookID, "notify")
	ents, err := os.ReadDir(notifyDir)
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
		path := filepath.Join(notifyDir, name)
		if filepath.Dir(path) != notifyDir {
			continue
		}
		a.quarantineClaudeHookEvent(hookID, path, name)
	}
}
