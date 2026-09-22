// claude_strict.go — supervision policy for scimux-owned Claude nodes.
//
// A newly launched Claude node enters the strict hooked path only after a
// valid SessionStart from that exact process has been processed against a
// complete current hook bundle (including PreCompact/PostCompact and
// Elicitation/ElicitationResult). Until then
// it is "starting". Adopted panes,
// pre-feature bundles, a moved/missing scimux executable, and any other
// incomplete contract are "unsupported": they never receive inspect/fallback
// terminal supervision and never enter the hooked permission path.
//
// Codex, ACP, pi, opencode, and grok are out of scope here.
package app

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/scimux/scimux/internal/dialoghint"
)

// claudeSupervision is the Claude-only UI/server contract. Empty means the
// node is not a Claude tmux session (other agents keep their own policy).
type claudeSupervision string

const (
	claudeSupNone        claudeSupervision = ""
	claudeSupStarting    claudeSupervision = "claude_starting"
	claudeSupStrict      claudeSupervision = "claude_strict"
	claudeSupUnsupported claudeSupervision = "claude_unsupported"
	claudeSupFailed      claudeSupervision = "claude_failed"
)

const (
	claudeUnsupportedExplain  = "This chat uses an older scimux setup. Fork or relaunch it to use all features."
	claudeStartTimeoutExplain = "Claude did not start. SessionStart never arrived, so Remote Control is not ready."
	claudeStartUnknownExplain = "Claude did not start. SessionStart never arrived. Inspect Claude outside scimux to see why; scimux cannot identify this startup dialog."
	claudeTrustExplain        = "Claude's workspace-trust prompt expired before it was answered. Relaunch and choose y or n in scimux. scimux does not write Claude trust-state files."
	claudeDeliveryExplain     = "Claude started, but never logged the first prompt — something on screen may have swallowed it. Open the terminal to check. The prompt has been kept as a draft and was not retried."
	claudeResumeExplain       = "Claude could not rebind the new transcript. The previous transcript is still attached. Inspect Claude outside scimux, or fork."
	claudePasteExplain        = "The first prompt could not be pasted into Claude. It has been kept as a draft and was not retried."
	claudeTurnFenceExplain    = "Claude supervision could not publish the turn fence, so the prompt was kept as a draft and was not sent. Relaunch this node from scimux."
	claudeClearExplain        = "The /clear reached Claude but no fresh session followed, so this chat was left as it was — something on screen may have swallowed it. Open the terminal to check. It was not retried."
)

// claudePermissionOptionKind classifies a label taken from a proven AX
// permission menu. Question menus never call this — their labels are
// arbitrary and must not be tagged allow/allow_always.
func claudePermissionOptionKind(label string) string {
	n := strings.ToLower(strings.TrimSpace(label))
	switch {
	case n == "yes":
		return "allow"
	case strings.Contains(n, "don't ask") || strings.Contains(n, "dont ask") || strings.Contains(n, "do not ask"):
		return "allow_always"
	case n == "no":
		return "reject"
	}
	return ""
}

func (a *app) markClaudeHookAck(nodeID string) {
	if nodeID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.claudeAck == nil {
		a.claudeAck = map[string]bool{}
	}
	a.claudeAck[nodeID] = true
	// An acknowledged node is bound, so it has no start left pending.
	delete(a.claudeStartPending, nodeID)
}

func (a *app) claudeHookAckedLocked(nodeID string) bool {
	return a.claudeAck[nodeID]
}

// markClaudeStartPending records a SessionStart that named a transcript the
// CLI has not written yet. The event itself stays in the inbox and binds when
// the file appears; this mark exists so the launch does not sit behind a file
// that only its own first prompt can create.
func (a *app) markClaudeStartPending(nodeID, sessionID string) {
	if nodeID == "" || sessionID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.claudeStartPending == nil {
		a.claudeStartPending = map[string]string{}
	}
	a.claudeStartPending[nodeID] = sessionID
}

// claudeStartPendingLocked reports whether the pending mark belongs to this
// launch. The session id is the fence: each launch mints its own, so a mark
// left by an earlier process can never release a later one's gate.
func (a *app) claudeStartPendingLocked(nodeID, sessionID string) bool {
	if nodeID == "" || sessionID == "" {
		return false
	}
	return a.claudeStartPending[nodeID] == sessionID
}

func (a *app) recordClaudeLaunchError(nodeID, msg string) {
	if nodeID == "" || msg == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.claudeLaunchErr == nil {
		a.claudeLaunchErr = map[string]string{}
	}
	a.claudeLaunchErr[nodeID] = msg
	delete(a.claudeRecoverableErr, nodeID)
}

// recordClaudeRecoverableError keeps an inline delivery diagnostic without
// invalidating a SessionStart-acknowledged hook bundle. If the turn truly is
// still outstanding, the accepted-turn nonce independently refuses the next
// send; if Claude finished and only transcript parsing missed the prompt, the
// node remains usable instead of becoming permanently claude_failed.
func (a *app) recordClaudeRecoverableError(nodeID, msg string) {
	if nodeID == "" || msg == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.claudeLaunchErr == nil {
		a.claudeLaunchErr = map[string]string{}
	}
	if a.claudeRecoverableErr == nil {
		a.claudeRecoverableErr = map[string]bool{}
	}
	a.claudeLaunchErr[nodeID] = msg
	a.claudeRecoverableErr[nodeID] = true
}

// claudeLaunchError returns a recorded launch error. Test seam: production
// reads a.claudeLaunchErr[...] with a.mu already held.
func (a *app) claudeLaunchError(nodeID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claudeLaunchErr[nodeID]
}

func (a *app) clearClaudeLaunchError(nodeID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.claudeLaunchErr, nodeID)
	delete(a.claudeRecoverableErr, nodeID)
}

// clearClaudeRecoverableError retires only the older delivery diagnostic. A
// fatal binding/launch error can be recorded concurrently while pane delivery
// is in flight; successful keystroke delivery must not erase that newer fault.
func (a *app) clearClaudeRecoverableError(nodeID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.claudeRecoverableErr[nodeID] {
		return
	}
	delete(a.claudeLaunchErr, nodeID)
	delete(a.claudeRecoverableErr, nodeID)
}

// bundleCompleteCurrent reports whether a hook bundle on disk is the complete
// current contract: SessionStart, PermissionRequest, Notification, Stop,
// StopFailure, PreCompact, PostCompact, Elicitation, ElicitationResult,
// notice layout, and a still-runnable baked executable.
func bundleCompleteCurrent(bundle string) bool {
	caps, ok := readClaudeHookCapabilities(bundle)
	if !ok || caps.Permission < 1 || caps.Asked < 1 || caps.Stop < 1 || caps.Notify < 1 || caps.Compact < 1 || caps.Elicitation < 1 {
		return false
	}
	if !claudeBundleExecUsable(caps) {
		return false
	}
	for _, sub := range []string{
		"inbox", "processed", "stop", "notify", "compact",
		"elicitation", filepath.Join("elicitation", "active"),
		"perm", filepath.Join("perm", "req"), filepath.Join("perm", "ans"),
		filepath.Join("perm", claudeAskedDirName),
	} {
		st, err := os.Stat(filepath.Join(bundle, sub))
		if err != nil || !st.IsDir() {
			return false
		}
	}
	return true
}

func (a *app) noteClaudeStrictCapability(hookID string) {
	if hookID == "" {
		return
	}
	ok := bundleCompleteCurrent(a.claudeHookBundlePath(hookID))
	a.mu.Lock()
	if a.claudeStrictCap == nil {
		a.claudeStrictCap = map[string]bool{}
	}
	if ok {
		a.claudeStrictCap[hookID] = true
	} else {
		delete(a.claudeStrictCap, hookID)
	}
	a.mu.Unlock()
}

// claudeSupervisionOf is the single gate every shared poll/chat/UI path uses
// before applying Claude-only policy. Caller may hold a.mu.
func (a *app) claudeSupervisionOf(n *Node) claudeSupervision {
	if n == nil || n.Agent != "claude" || n.transport() != "tmux" {
		return claudeSupNone
	}
	if a.claudeLaunchErr[n.ID] != "" && !a.claudeRecoverableErr[n.ID] {
		return claudeSupFailed
	}
	hookID := a.claudeHookIDLocked(n.ID)
	if n.Adopted || hookID == "" {
		return claudeSupUnsupported
	}
	// Disk completeness of a current bundle is enough to call the node
	// "starting". Permission and auto-approve capability stay gated on
	// SessionStart (noteClaudePermCapability / noteClaudeAskedCapability).
	if !a.claudeStrictCap[hookID] {
		return claudeSupUnsupported
	}
	if !a.claudeHookAckedLocked(n.ID) {
		return claudeSupStarting
	}
	return claudeSupStrict
}

func (a *app) claudeStrictSupervised(n *Node) bool {
	if n == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claudeSupervisionOf(n) == claudeSupStrict
}

func (a *app) claudeAutoApproveArmedLocked(n *Node) bool {
	if n == nil {
		return false
	}
	st := a.autoApprove[n.ID]
	return st != nil && st.Phase == autoPhaseArmed
}

func claudeSupervisionExplain(sup claudeSupervision, launchErr string) string {
	switch sup {
	case claudeSupUnsupported:
		return claudeUnsupportedExplain
	case claudeSupFailed:
		if launchErr != "" {
			return launchErr
		}
		return claudeStartTimeoutExplain
	}
	return ""
}

// claudeChatSupervisionExplain suppresses compatibility guidance after a
// thread has ended. The node's Exited state already explains why controls are
// unavailable; an upgrade instruction at that point is redundant and reads
// like a fresh problem.
func claudeChatSupervisionExplain(sup claudeSupervision, launchErr string, ended bool) string {
	if ended {
		return ""
	}
	if launchErr != "" {
		return launchErr
	}
	return claudeSupervisionExplain(sup, launchErr)
}

func (a *app) diagnoseClaudeStartFailure(n *Node) string {
	if n == nil || a.server == nil {
		return claudeStartTimeoutExplain
	}
	visible, err := a.server.Session(n.ID).CaptureVisible()
	if err != nil {
		return claudeStartTimeoutExplain
	}
	if dialoghint.LooksLikeWorkspaceTrust(visible) {
		return claudeTrustExplain
	}
	if dialoghint.ClassifyVisible(visible) {
		return claudeStartUnknownExplain
	}
	return claudeStartTimeoutExplain
}

func (a *app) claudeWorkspaceTrustVisible(n *Node) bool {
	if n == nil || a.server == nil {
		return false
	}
	visible, err := a.server.Session(n.ID).CaptureVisible()
	return err == nil && dialoghint.LooksLikeWorkspaceTrust(visible)
}

// claudeSessionStartReady is the first-prompt paste gate: has this exact
// launched process delivered a valid SessionStart? A bound transcript proves
// it. So does a pending one — the CLI writes the transcript file lazily, so an
// idle launch has no file for the hook to name, and the prompt this gate holds
// back is what would create it. Waiting for the file here deadlocked the
// launch (observed on claude 2.1.266); the binding still waits for the real
// file, and the delivery-confirmation loop picks it up from the same event.
func (a *app) claudeSessionStartReady(n *Node) bool {
	if n == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if n.SessionID == "" {
		return false
	}
	if a.claudeHookAckedLocked(n.ID) && n.Transcript != "" {
		return true
	}
	return a.claudeStartPendingLocked(n.ID, n.SessionID)
}

func (a *app) clearClaudeAttention(n *Node) {
	if n == nil {
		return
	}
	a.mu.Lock()
	prev := a.attn[n.ID]
	a.attn[n.ID] = ""
	delete(a.attnAt, n.ID)
	a.mu.Unlock()
	if prev != "" {
		a.persistAttentionTransition(n, prev, "")
	}
}

// retireClaudeVisibleDialog retires only the server-minted visible-dialog
// epoch. It does not delete any asked notice: Notification has no request
// id, so a standing ask is not that epoch's identity. Asks retire through
// matching transcript/tool evidence or an explicit turn boundary.
func (a *app) retireClaudeVisibleDialog(n *Node, epoch string) bool {
	if n == nil || epoch == "" {
		return false
	}
	a.mu.Lock()
	capable := a.claudeSupervisionOf(n) == claudeSupStrict
	bundle := ""
	if capable {
		bundle = a.claudePermBundleLocked(n.ID)
	}
	a.mu.Unlock()
	if bundle == "" {
		return false
	}
	perm := filepath.Join(bundle, "perm")
	cur, ok := readClaudeVisibleEpoch(perm)
	if !ok || cur.Epoch != epoch {
		return false
	}
	clearClaudeVisibleEpoch(perm)
	return true
}

func claudeRequestIsQuestion(tool string) bool {
	return claudeQuestionTools[tool]
}

// claudeVisibleDialog is the only actionable terminal source for an owned
// Claude node: the exact pre-session trust prompt while starting, or a
// Notification(permission_prompt) epoch after SessionStart. Neither identity
// is a Claude PermissionRequest id.
type claudeVisibleDialog struct {
	Attn     string
	DialogID string // supervisor identity, never a Claude request id
	Title    string
	Tool     string
	Question bool
	Armed    bool
}

const claudeWorkspaceTrustPrefix = "workspace-trust:"

func claudeWorkspaceTrustRequestID(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return claudeWorkspaceTrustPrefix + sessionID
}

func isClaudeWorkspaceTrustRequestID(id string) bool {
	return strings.HasPrefix(id, claudeWorkspaceTrustPrefix) && len(id) > len(claudeWorkspaceTrustPrefix)
}

func (a *app) claudeVisibleDialog(n *Node) claudeVisibleDialog {
	var empty claudeVisibleDialog
	if n == nil {
		return empty
	}
	a.mu.Lock()
	sup := a.claudeSupervisionOf(n)
	armed := a.claudeAutoApproveArmedLocked(n)
	bundle := ""
	sid := n.SessionID
	attn := a.attn[n.ID]
	if sup == claudeSupStrict {
		bundle = a.claudePermBundleLocked(n.ID)
	}
	a.mu.Unlock()
	if sup == claudeSupStarting && attn == "dialog" {
		return claudeVisibleDialog{
			Attn: "dialog", DialogID: claudeWorkspaceTrustRequestID(sid),
			Title: "Trust this directory?", Tool: "workspace",
		}
	}
	if sup != claudeSupStrict || bundle == "" {
		return empty
	}
	perm := filepath.Join(bundle, "perm")
	ep, ok := liveClaudeVisibleEpoch(perm, sid)
	if !ok || ep.Epoch == "" {
		return empty
	}
	// Standing ask tool is the classifier. A generic Notification title
	// ("Permission Required") must not override AskUserQuestion / ExitPlanMode.
	tool := ep.HintTool
	title := ep.Title
	if title == "" {
		title = tool
	}
	if title == "" {
		title = "Permission"
	}
	question := claudeRequestIsQuestion(tool)
	if armed && !question {
		// An armed lease that still left a visible permission dialog failed
		// to cover it (deadline, policy, competing hook, or Claude's own
		// ask/deny rule). Record an inline error. Questions/plan choices are
		// never auto-answered, so they use the manual-response surface
		// without crying wolf on every AskUserQuestion.
		a.mu.Lock()
		if cur := a.autoApprove[n.ID]; cur != nil && cur.Phase == autoPhaseArmed && cur.Error == "" {
			cur.Error = "auto-approve could not answer this permission request; choose below or open Terminal"
		}
		a.mu.Unlock()
	}
	kind := attentionKind(tool)
	if tool == "" {
		kind = "approval"
	}
	return claudeVisibleDialog{
		Attn:     kind,
		DialogID: ep.Epoch,
		Title:    title,
		Tool:     tool,
		Question: question,
		Armed:    armed,
	}
}

// claudeDialogOptions returns action keys only from a structurally validated
// current AX menu. The pane must still show either the exact workspace-trust
// grammar or the numbered menu paired with a Notification epoch. Failure is
// empty options — the chat shows a manual-response state instead of invented
// choices.
func (a *app) claudeDialogOptions(n *Node, dlg claudeVisibleDialog) []PermOption {
	if n == nil || dlg.DialogID == "" || a.server == nil {
		return nil
	}
	vis, err := a.server.Session(n.ID).CaptureVisible()
	if err != nil || vis == "" {
		return nil
	}
	a.mu.Lock()
	trustID := claudeWorkspaceTrustRequestID(n.SessionID)
	a.mu.Unlock()
	if dlg.DialogID == trustID {
		if !dialoghint.LooksLikeWorkspaceTrust(vis) {
			return nil
		}
		return []PermOption{
			{Key: "y", Name: "Trust directory", Kind: "allow"},
			{Key: "n", Name: "Cancel launch", Kind: "reject"},
		}
	}
	items, ok := dialoghint.NumberedOptions(vis)
	if !ok {
		return nil
	}
	out := make([]PermOption, 0, len(items))
	for _, it := range items {
		kind := ""
		if !dlg.Question {
			kind = claudePermissionOptionKind(it.Label)
		}
		out = append(out, PermOption{
			Key:  strconv.Itoa(it.N),
			Name: it.Label,
			Kind: kind,
		})
	}
	return out
}

// resetClaudePermissionTurn tombstones unresolved asked/epoch state
// belonging to the ending session so a later turn cannot pair with it.
// It runs at every real turn boundary even when no lease is armed.
// Unrelated session files are left standing. Callers disarm the lease
// separately when that is part of the boundary.
func (a *app) resetClaudePermissionTurn(n *Node) {
	if n == nil {
		return
	}
	g := a.autoGateFor(n.ID)
	g.Lock()
	defer g.Unlock()
	a.resetClaudePermissionTurnLocked(n)
}

// endClaudePermissionTurn is the atomic explicit-boundary path: retract the
// lease, clear in-memory authority, tombstone dialog state, and close the turn
// token under one gate. A concurrent send can run only before or after the
// whole boundary, never between disarm and token closure.
func (a *app) endClaudePermissionTurn(n *Node) {
	if n == nil {
		return
	}
	g := a.autoGateFor(n.ID)
	g.Lock()
	defer g.Unlock()
	a.retractClaudeLease(a.claudePermBundle(n.ID))
	a.mu.Lock()
	delete(a.autoApprove, n.ID)
	a.mu.Unlock()
	a.resetClaudePermissionTurnLocked(n)
}

// resetClaudePermissionTurnLocked is the gate-held core used by Stop, clear,
// interrupt, and delete. Keeping cleanup and token closure in the same
// critical section prevents a new prompt from entering half-way through the
// old turn's tombstoning.
func (a *app) resetClaudePermissionTurnLocked(n *Node) {
	if n == nil {
		return
	}
	a.clearClaudeAttention(n)
	a.mu.Lock()
	bundle := a.claudePermBundleLocked(n.ID)
	hookID := a.claudeHookIDLocked(n.ID)
	sid := n.SessionID
	turn := a.claudeTurns[n.ID]
	a.mu.Unlock()
	if bundle != "" {
		perm := filepath.Join(bundle, "perm")
		tombstoneClaudeAskedForSession(perm, sid)
		clearClaudeVisibleEpoch(perm)
		markClaudeTurnClosed(perm, sid, turn.Turn, turn.Gen)
		clearClaudeElicitationsForSession(bundle, sid)
	}
	if hookID != "" {
		a.dropClaudeNotifyInbox(hookID)
	}
	a.closeClaudeAcceptedTurnLocked(n.ID)
	a.clearClaudeDialogNote(n.ID)
}

// publishClaudeVisibleAttention installs map/card attention from a still-
// current visible dialog after auto-approve is disabled. No-op while armed
// or when the epoch has already been retired.
func (a *app) publishClaudeVisibleAttention(n *Node) {
	if n == nil {
		return
	}
	dlg := a.claudeVisibleDialog(n)
	if dlg.DialogID == "" || dlg.Armed || dlg.Attn == "" {
		return
	}
	a.mu.Lock()
	if a.claudeAutoApproveArmedLocked(n) {
		a.mu.Unlock()
		return
	}
	prev := a.attn[n.ID]
	a.attn[n.ID] = dlg.Attn
	a.attnAt[n.ID] = time.Now()
	a.mu.Unlock()
	if prev != dlg.Attn {
		a.persistAttentionTransition(n, prev, dlg.Attn)
	}
}
