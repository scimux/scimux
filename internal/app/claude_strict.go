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

	"codeberg.org/chrberger/scimux/internal/dialoghint"
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
	claudeUnsupportedExplain  = "This Claude session is not using scimux's current hook bundle, so chat binding, permission dialogs, and auto-approve are unavailable. Fork or relaunch it from scimux."
	claudeStartTimeoutExplain = "Claude did not start. SessionStart never arrived, so Remote Control is not ready."
	claudeStartUnknownExplain = "Claude did not start. SessionStart never arrived. Inspect Claude outside scimux to see why; scimux cannot identify this startup dialog."
	claudeTrustExplain        = "Claude stopped on a workspace-trust dialog. Trust this directory once in Claude Code outside scimux, then relaunch. scimux does not write Claude trust-state files, and --add-dir does not bypass trust."
	claudeDeliveryExplain     = "The first prompt could not be confirmed in the transcript. It has been kept as a draft and was not retried."
	claudeResumeExplain       = "Claude could not rebind the new transcript. The previous transcript is still attached. Inspect Claude outside scimux, or fork."
	claudePasteExplain        = "The first prompt could not be pasted into Claude. It has been kept as a draft and was not retried."
	claudeTurnFenceExplain    = "Claude supervision could not publish the turn fence, so the prompt was kept as a draft and was not sent. Relaunch this node from scimux."
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
}

func (a *app) claudeHookAckedLocked(nodeID string) bool {
	return a.claudeAck[nodeID]
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
}

func (a *app) claudeLaunchError(nodeID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claudeLaunchErr[nodeID]
}

func (a *app) clearClaudeLaunchError(nodeID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.claudeLaunchErr, nodeID)
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
		filepath.Join("perm", claudeAskedDirName), filepath.Join("perm", claudeShownDirName),
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
	if a.claudeLaunchErr[n.ID] != "" {
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

func (a *app) claudeGated(n *Node) bool {
	if n == nil || n.Agent != "claude" || n.transport() != "tmux" {
		return false
	}
	return true
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

func (a *app) claudeSessionStartReady(n *Node) bool {
	if n == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claudeHookAckedLocked(n.ID) && n.Transcript != "" && n.SessionID != ""
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

func (a *app) retireClaudeRequest(n *Node, requestID string) bool {
	if n == nil || requestID == "" || !safePathComponent(requestID) {
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
	removed := false
	if err := os.Remove(claudeAskedPath(perm, requestID)); err == nil {
		removed = true
	}
	_ = os.Remove(claudeShownPath(perm, requestID))
	markClaudeRequestAnswered(perm, requestID)
	return removed || claudeRequestAnswered(perm, requestID)
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

// claudeVisibleDialog is the only automatic terminal/attention source for a
// strict hooked Claude node. Notification(permission_prompt) proves a dialog
// is visible and mints a server epoch; it does not identify which
// PermissionRequest is on screen. Question tools raise attention but are not
// a permission dialog for the forced-terminal rule.
type claudeVisibleDialog struct {
	Attn     string
	DialogID string // server-minted visible-dialog epoch, not a Claude request id
	Title    string
	Tool     string
	Question bool
	Armed    bool
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
	if sup == claudeSupStrict {
		bundle = a.claudePermBundleLocked(n.ID)
	}
	a.mu.Unlock()
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
// current AX menu. The pane is consulted only after Notification proved a
// dialog exists (dlg.DialogID). Failure is empty options — the chat shows a
// manual-response state instead of invented Yes/No buttons.
func (a *app) claudeDialogOptions(n *Node, dlg claudeVisibleDialog) []PermOption {
	if n == nil || dlg.DialogID == "" || a.server == nil {
		return nil
	}
	vis, err := a.server.Session(n.ID).CaptureVisible()
	if err != nil || vis == "" {
		return nil
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

// resetClaudePermissionTurn tombstones unresolved asked/shown/epoch state
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
		clearClaudeShownMarkers(perm)
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
