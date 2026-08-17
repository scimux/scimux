// claude_permission.go — the PermissionRequest hook transport: the hidden
// helper that Claude runs in front of every permission decision, and the
// rendezvous files it shares with the scimux process.
//
// Claude offers this hook no menu. The payload describes the tool call; the
// once/always distinction lives entirely on the answer side, where
// `{"behavior":"allow"}` grants exactly the one call being decided and
// `updatedPermissions` would persist a rule. scimux never writes that member,
// so "allow once only" is an omission, not a policy check.
//
// Emitting no decision is the escalate: the normal permission flow proceeds
// and the human answers the dialog in the pane, exactly as today.
package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// claudePermissionHookCmd is the hidden helper argv token, beside
// claudeSessionHookCmd. cmd/scimux only dispatches.
const claudePermissionHookCmd = "__claude-permission-hook"

const (
	// permHookDeadline bounds how long a tool call may wait on scimux before
	// escalating to the dialog. Claude's own hook timeout is far longer; this
	// is scimux's promise, not Claude's.
	permHookDeadline = 2 * time.Second
	// permHookPollEvery is the rendezvous poll interval inside the helper.
	permHookPollEvery = 25 * time.Millisecond
)

// claudePermAllowJSON is the only thing this helper ever prints. Pinned
// byte-exact: `hookSpecificOutput.decision` is an object union in the CLI's
// schema (the published docs' string form is wrong), and the two optional
// members it omits — updatedInput, updatedPermissions — are precisely what
// would rewrite the call or persist a permission rule.
const claudePermAllowJSON = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`

// claudePermissionEvent is the PermissionRequest stdin payload. Unlike
// PreToolUse there is no tool_use_id: the CLI passes the tool-use id to its
// hook runner for telemetry only, never into the hook's stdin. Unknown fields
// are ignored at decode time.
type claudePermissionEvent struct {
	HookEventName  string          `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Cwd            string          `json:"cwd"`
	PromptID       string          `json:"prompt_id"`
	PermissionMode string          `json:"permission_mode"`
	AgentID        string          `json:"agent_id"`
	AgentType      string          `json:"agent_type"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	// HasSuggestions records that Claude offered persistent rule/mode
	// updates for this call (the "don't ask again" payload). scimux never
	// echoes them; it is kept as decision evidence.
	HasSuggestions bool `json:"has_permission_suggestions"`
}

// claudePermLease is the arm marker. Its presence is necessary but never
// sufficient: the app re-checks the lease before answering, so a stale marker
// can only ever cost a tool call the deadline, never authorize one.
type claudePermLease struct {
	Lease   string `json:"lease"`
	Expires string `json:"exp"`
}

// claudePermRequest is what the blocked helper leaves for the app. The id is
// minted here because the payload carries none — and because the helper
// process *is* the request, one nonce maps to exactly one blocked call.
type claudePermRequest struct {
	Lease string                `json:"lease"`
	ID    string                `json:"id"`
	At    string                `json:"at"`
	Event claudePermissionEvent `json:"event"`
}

// claudePermAnswer is the app's reply. "allow" is the only decision scimux
// ever writes; anything else (including nothing) escalates.
type claudePermAnswer struct {
	Lease    string `json:"lease"`
	ID       string `json:"id"`
	Decision string `json:"decision"`
}

func (a claudePermAnswer) allows(req claudePermRequest) bool {
	return a.Decision == "allow" && a.ID != "" && a.ID == req.ID && a.Lease != "" && a.Lease == req.Lease
}

// claudePermContext is the app-side state an eligibility decision is made
// against. Kept separate from autoApproveState so the policy stays a pure
// function of proven facts.
//
// TurnPromptID is the current turn's prompt_id when the app knows it (the
// protocol's own turn identity). Empty means unfenced: the lease is then the
// only turn boundary, which is the same guarantee the structured transports
// have.
type claudePermContext struct {
	Armed        bool
	LeaseID      string
	SessionID    string
	TurnPromptID string
	Now          time.Time
}

// claudeQuestionTools are the tools whose "permission" is really a question
// to the human. fixes-2 forbids auto-answering those, and Claude itself
// discards a hook allow for them, so this fence is belt-and-braces.
var claudeQuestionTools = map[string]bool{
	"ExitPlanMode":    true,
	"AskUserQuestion": true,
}

// claudeAutoModes are the permission modes in which an ask is an ordinary
// tool approval. plan is excluded because a plan-mode approval is a question;
// auto/dontAsk/bypassPermissions are excluded because Claude's own automation
// already owns the decision; anything unrecognized is excluded because
// unknown is unknown.
var claudeAutoModes = map[string]bool{
	"default":     true,
	"acceptEdits": true,
}

// eligibleClaudePermission is the pure Claude-side analogue of
// eligibleAutoAllow. There is no menu to inspect, so the fence is rebuilt
// from what the hook does prove: an armed lease this request echoes, the
// node's own bound session, an ordinary approval mode, a tool that is not a
// question, and a request whose hook is still waiting.
//
// The "request-scoped only" property is not checked here — it is structural:
// the answer reaches exactly the one blocked hook process that asked, and the
// allow JSON carries no updatedPermissions.
func eligibleClaudePermission(ctx claudePermContext, req claudePermRequest) bool {
	if !ctx.Armed || ctx.LeaseID == "" || req.Lease == "" || req.Lease != ctx.LeaseID {
		return false
	}
	if req.ID == "" {
		return false
	}
	ev := req.Event
	if ev.HookEventName != "PermissionRequest" {
		return false
	}
	if ctx.SessionID == "" || ev.SessionID == "" || ev.SessionID != ctx.SessionID {
		return false
	}
	if !claudeAutoModes[ev.PermissionMode] {
		return false
	}
	if ev.ToolName == "" || claudeQuestionTools[ev.ToolName] {
		return false
	}
	if ctx.TurnPromptID != "" && ev.PromptID != ctx.TurnPromptID {
		return false
	}
	return claudePermRequestPending(req, ctx.Now)
}

// claudePermRequestPending reports whether the helper that wrote req can
// still be waiting. Answering an already-escalated request would append a
// decision record for an approval that never happened — the audit must
// describe reality.
func claudePermRequestPending(req claudePermRequest, now time.Time) bool {
	at, err := time.Parse(time.RFC3339Nano, req.At)
	if err != nil {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	return now.Sub(at) < permHookDeadline
}

// RunClaudePermissionHook is the hidden helper body.
func RunClaudePermissionHook(dir string, r io.Reader, stdout, stderr io.Writer) error {
	return runClaudePermissionHook(dir, r, stdout, stderr, permHookDeadline, permHookPollEvery)
}

func runClaudePermissionHook(dir string, r io.Reader, stdout, stderr io.Writer, deadline, every time.Duration) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	perm, err := claudePermDir(dir)
	if err != nil {
		return err
	}
	ev, err := readClaudePermissionEvent(r)
	if err != nil {
		return err
	}

	// Fast path: no armed lease is the state every session is in unless a
	// human has just toggled auto-approve on. One open, one read, no wait,
	// and behaviour indistinguishable from having no hook at all.
	lease, ok := readClaudePermLease(perm, time.Now())
	if !ok {
		return nil
	}

	id, err := newPermRequestID()
	if err != nil {
		return nil
	}
	req := claudePermRequest{
		Lease: lease,
		ID:    id,
		At:    time.Now().UTC().Format(time.RFC3339Nano),
		Event: ev,
	}
	reqPath := filepath.Join(perm, "req", id+".json")
	if err := writeClaudePermFile(reqPath, req); err != nil {
		return nil
	}
	defer os.Remove(reqPath)

	ansPath := filepath.Join(perm, "ans", id+".json")
	defer os.Remove(ansPath)
	stop := time.Now().Add(deadline)
	for {
		if ans, ok := readClaudePermAnswer(ansPath); ok {
			if ans.allows(req) {
				_, _ = io.WriteString(stdout, claudePermAllowJSON)
			}
			return nil
		}
		if !time.Now().Before(stop) {
			return nil // deadline: escalate to the dialog
		}
		time.Sleep(every)
	}
}

// claudePermDir validates the bundle path with the same fences the
// SessionStart helper uses and returns its perm/ rendezvous directory. A
// bundle prepared before this feature has no perm/ and is rejected.
func claudePermDir(dir string) (string, error) {
	if dir == "" || !filepath.IsAbs(dir) || dir != filepath.Clean(dir) || !safePathComponent(filepath.Base(dir)) {
		return "", errClaudeHookRejected
	}
	perm := filepath.Join(dir, "perm")
	for _, sub := range []string{"", "req", "ans"} {
		st, err := os.Stat(filepath.Join(perm, sub))
		if err != nil || !st.IsDir() {
			return "", errClaudeHookRejected
		}
	}
	return perm, nil
}

func readClaudePermissionEvent(r io.Reader) (claudePermissionEvent, error) {
	payload, err := io.ReadAll(io.LimitReader(r, claudeHookStdinLimit+1))
	if err != nil || len(payload) > claudeHookStdinLimit {
		return claudePermissionEvent{}, errClaudeHookRejected
	}
	var ev claudePermissionEvent
	if json.Unmarshal(payload, &ev) != nil {
		return claudePermissionEvent{}, errClaudeHookRejected
	}
	// A PreToolUse payload arriving here is a bug or an attack, never a
	// decision: PreToolUse fires for every call, decision needed or not.
	if ev.HookEventName != "PermissionRequest" || ev.ToolName == "" || ev.SessionID == "" {
		return claudePermissionEvent{}, errClaudeHookRejected
	}
	var probe struct {
		Suggestions []json.RawMessage `json:"permission_suggestions"`
	}
	if json.Unmarshal(payload, &probe) == nil {
		ev.HasSuggestions = len(probe.Suggestions) > 0
	}
	return ev, nil
}

func readClaudePermLease(perm string, now time.Time) (string, bool) {
	b, err := os.ReadFile(filepath.Join(perm, "lease"))
	if err != nil || len(b) > claudeHookStdinLimit {
		return "", false
	}
	var l claudePermLease
	if json.Unmarshal(b, &l) != nil || l.Lease == "" {
		return "", false
	}
	exp, err := time.Parse(time.RFC3339Nano, l.Expires)
	if err != nil || !now.Before(exp) {
		return "", false
	}
	return l.Lease, true
}

func readClaudePermAnswer(path string) (claudePermAnswer, bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 || len(b) > claudeHookStdinLimit {
		return claudePermAnswer{}, false
	}
	var ans claudePermAnswer
	if json.Unmarshal(b, &ans) != nil {
		return claudePermAnswer{}, false
	}
	return ans, true
}

func newPermRequestID() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// writeClaudePermFile publishes a rendezvous file atomically: the reader on
// the other side must never observe a half-written JSON document.
func writeClaudePermFile(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(raw)
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
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return sessionlog.SyncParentDir(path)
}

// bundleSupportsPermission reports whether a hook bundle on disk was prepared
// with the permission rendezvous. Durable across a scimux restart on purpose:
// a pane launched before this feature shipped stays unsupported until it is
// relaunched, and says so rather than silently never arming.
func bundleSupportsPermission(bundle string) bool {
	if bundle == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(bundle, "capabilities.json"))
	if err != nil {
		return false
	}
	var caps struct {
		Permission int `json:"permission"`
	}
	if json.Unmarshal(b, &caps) != nil {
		return false
	}
	if caps.Permission < 1 {
		return false
	}
	_, err = claudePermDir(bundle)
	return err == nil
}

// runClaudePermissionHookMain always exits 0. A non-zero exit from a
// permission hook reads as hook failure to the CLI; scimux escalates by
// staying silent instead.
func runClaudePermissionHookMain(args []string) int {
	dir := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			dir = args[i+1]
			i++
		}
	}
	_ = RunClaudePermissionHook(dir, os.Stdin, os.Stdout, io.Discard)
	return 0
}

// ---------- app side ----------

// claudeLeaseTTL bounds a published marker's life. It is a backstop, not the
// lease policy: the turn's end disarms first (Stop hook, pane quiet, exit).
// It exists so a scimux that dies mid-turn cannot leave an armed marker
// behind for a pane that outlives it.
const claudeLeaseTTL = 30 * time.Minute

// claudePermBundle returns the on-disk bundle directory for a node, or "".
func (a *app) claudePermBundleLocked(nodeID string) string {
	hookID := a.claudeHookIDLocked(nodeID)
	if !safePathComponent(hookID) {
		return ""
	}
	root := a.claudeHooksDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, hookID)
}

func (a *app) claudePermBundle(nodeID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claudePermBundleLocked(nodeID)
}

// noteClaudePermCapability records that a bundle carries the permission
// rendezvous, so the supported predicate stays a map lookup under a.mu
// instead of filesystem I/O on every poll projection.
func (a *app) noteClaudePermCapability(hookID string) {
	if hookID == "" {
		return
	}
	a.mu.Lock()
	if a.claudePermCap == nil {
		a.claudePermCap = map[string]bool{}
	}
	a.claudePermCap[hookID] = true
	a.mu.Unlock()
}

// refreshClaudePermCaps rebuilds the capability map from disk at startup.
// A pane that survived a scimux restart keeps whatever its bundle proves.
func (a *app) refreshClaudePermCaps() {
	root := a.claudeHooksDir()
	if root == "" {
		return
	}
	a.mu.Lock()
	ids := make([]string, 0, len(a.claudeHooks))
	for _, hookID := range a.claudeHooks {
		ids = append(ids, hookID)
	}
	a.mu.Unlock()
	caps := map[string]bool{}
	for _, hookID := range ids {
		if !safePathComponent(hookID) {
			continue
		}
		if bundleSupportsPermission(filepath.Join(root, hookID)) {
			caps[hookID] = true
		}
	}
	a.mu.Lock()
	a.claudePermCap = caps
	a.mu.Unlock()
}

// claudePermSupportedLocked reports whether this node can auto-approve via
// the hook. Caller may hold a.mu.
func (a *app) claudePermSupportedLocked(n *Node) bool {
	if n == nil || n.Agent != "claude" || n.transport() != "tmux" {
		return false
	}
	hookID := a.claudeHookIDLocked(n.ID)
	if hookID == "" {
		return false
	}
	return a.claudePermCap[hookID]
}

// publishClaudeLease writes the arm marker the hook reads. Ordering rule:
// arm flips in-memory state first and publishes second, disarm removes the
// marker first and clears state second — so a marker can only ever cost a
// tool call the helper's deadline, never authorize one.
func (a *app) publishClaudeLease(bundle, leaseID string) {
	if bundle == "" || leaseID == "" {
		return
	}
	_ = writeClaudePermFile(filepath.Join(bundle, "perm", "lease"), claudePermLease{
		Lease:   leaseID,
		Expires: time.Now().Add(claudeLeaseTTL).UTC().Format(time.RFC3339Nano),
	})
}

func (a *app) retractClaudeLease(bundle string) {
	if bundle == "" {
		return
	}
	path := filepath.Join(bundle, "perm", "lease")
	if err := os.Remove(path); err == nil {
		_ = sessionlog.SyncParentDir(path)
	}
}

// syncClaudeLeaseMarker brings the on-disk marker in line with the node's
// current phase. Only an armed lease publishes.
func (a *app) syncClaudeLeaseMarker(n *Node) {
	if n == nil {
		return
	}
	a.mu.Lock()
	supported := a.claudePermSupportedLocked(n)
	bundle := a.claudePermBundleLocked(n.ID)
	leaseID := ""
	if st := a.autoApprove[n.ID]; st != nil && st.Phase == autoPhaseArmed {
		leaseID = st.LeaseID
	}
	a.mu.Unlock()
	if !supported || bundle == "" {
		return
	}
	if leaseID == "" {
		a.retractClaudeLease(bundle)
		return
	}
	a.publishClaudeLease(bundle, leaseID)
}

// claudePermLaneEvery paces the permission lane. A blocked tool call is a
// human-visible stall, so this runs far faster than the 2 s poll — it can
// afford to, because a tick with no armed Claude lease is a single map walk
// under a.mu and touches no filesystem at all.
const claudePermLaneEvery = 100 * time.Millisecond

// claudePermissionLane is the fast lane started by Run. It is deliberately
// separate from poll(): the poll tick owes capture, transcript and mirror
// work that must not be run ten times a second, and a permission answer owes
// none of it.
func (a *app) claudePermissionLane() {
	for {
		a.resolveClaudePermissions()
		time.Sleep(claudePermLaneEvery)
	}
}

// resolveClaudePermissions serves every node whose lease is armed right now.
// Returns the number of approvals delivered across all of them.
func (a *app) resolveClaudePermissions() int {
	a.mu.Lock()
	var armed []*Node
	for _, n := range a.nodes {
		st := a.autoApprove[n.ID]
		if st == nil || st.Phase != autoPhaseArmed {
			continue
		}
		if !a.claudePermSupportedLocked(n) {
			continue
		}
		armed = append(armed, n)
	}
	a.mu.Unlock()
	total := 0
	for _, n := range armed {
		total += a.resolveClaudePermissionsFor(n)
	}
	return total
}

// resolveClaudePermissionsFor drains one node's permission rendezvous. It is
// the Claude analogue of maybeAutoApprove: same per-node auto-gate, same
// audit-before-delivery ordering, same fail-closed error handling. Returns
// the number of approvals actually delivered.
func (a *app) resolveClaudePermissionsFor(n *Node) int {
	if n == nil {
		return 0
	}
	a.mu.Lock()
	supported := a.claudePermSupportedLocked(n)
	st := a.autoApprove[n.ID]
	armed := st != nil && st.Phase == autoPhaseArmed
	bundle := a.claudePermBundleLocked(n.ID)
	a.mu.Unlock()
	if !supported || !armed || bundle == "" {
		// Toggle off: never touch the rendezvous. A request left by an
		// earlier turn ages out on the helper's own deadline.
		return 0
	}

	reqDir := filepath.Join(bundle, "perm", "req")
	ents, err := os.ReadDir(reqDir)
	if err != nil {
		return 0
	}
	delivered := 0
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || !safePathComponent(name) {
			continue
		}
		path := filepath.Join(reqDir, name)
		req, ok := readClaudePermRequestFile(path)
		if !ok {
			a.finishClaudePermRequest(bundle, path, name, "rejected")
			continue
		}
		if a.resolveOneClaudePermission(n, bundle, path, name, req) {
			delivered++
		}
	}
	return delivered
}

func readClaudePermRequestFile(path string) (claudePermRequest, bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 || len(b) > 2*claudeHookStdinLimit {
		return claudePermRequest{}, false
	}
	var req claudePermRequest
	if json.Unmarshal(b, &req) != nil || req.ID == "" {
		return claudePermRequest{}, false
	}
	return req, true
}

// resolveOneClaudePermission commits (or declines) a single request under the
// per-node auto-gate, which enable/disable/re-arm also wait on: once a
// disable has returned, no decision belonging to the prior lease can still be
// delivered.
func (a *app) resolveOneClaudePermission(n *Node, bundle, path, name string, req claudePermRequest) bool {
	g := a.autoGateFor(n.ID)
	g.Lock()
	defer g.Unlock()

	a.mu.Lock()
	st := a.autoApprove[n.ID]
	ctx := claudePermContext{
		SessionID:    n.SessionID,
		TurnPromptID: a.claudeTurnPrompt[n.ID],
		Now:          time.Now(),
	}
	if st != nil {
		ctx.Armed = st.Phase == autoPhaseArmed
		ctx.LeaseID = st.LeaseID
	}
	leaseID := ctx.LeaseID
	agent := n.Agent
	a.mu.Unlock()

	if !eligibleClaudePermission(ctx, req) {
		// Declined: the helper escalates on its own deadline and the human
		// sees the dialog Claude was about to draw anyway.
		a.finishClaudePermRequest(bundle, path, name, "manual")
		return false
	}

	// Claim the request by moving it out of req/ before any audit. The rename
	// is the single-delivery guarantee: a concurrent lane pass cannot pick up
	// a request that has already been claimed.
	claimed := filepath.Join(bundle, "perm", "processed", "allowed", name)
	if err := movePermFile(path, claimed); err != nil {
		return false
	}

	dec := sessionlog.DecisionEvent{
		Source:    "auto",
		LeaseID:   leaseID,
		RequestID: req.ID,
		Agent:     agent,
		ToolKind:  claudePermToolKind(req.Event.ToolName),
		Title:     claudePermTitle(req.Event),
		Reason:    claudePermReason(req.Event),
		// Claude offers scimux no menu, so the audit records none. The UI
		// renders that honestly rather than as an empty option list.
		Options:  nil,
		Selected: sessionlog.DecOption{Name: "Allow once", Kind: "allow"},
	}
	if err := a.appendSessionEvent(n.ID, sessionlog.NewDecision(dec)); err != nil {
		a.mu.Lock()
		if cur := a.autoApprove[n.ID]; cur != nil && cur.LeaseID == leaseID {
			cur.Error = "auto-approve audit failed: " + err.Error()
		}
		a.mu.Unlock()
		return false
	}

	ans := claudePermAnswer{Lease: leaseID, ID: req.ID, Decision: "allow"}
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "ans", req.ID+".json"), ans); err != nil {
		_ = a.appendSessionEvent(n.ID, sessionlog.Event{
			T: "error",
			Error: fmt.Sprintf("auto-approve delivery failed (lease=%s request=%s): %v",
				leaseID, req.ID, err),
		})
		a.mu.Lock()
		if cur := a.autoApprove[n.ID]; cur != nil && cur.LeaseID == leaseID {
			cur.Error = "auto-approve delivery failed: " + err.Error()
		}
		a.mu.Unlock()
		return false
	}

	a.mu.Lock()
	if cur := a.autoApprove[n.ID]; cur != nil && cur.LeaseID == leaseID && cur.Phase == autoPhaseArmed {
		cur.Count++
		cur.Error = ""
	}
	a.mu.Unlock()
	return true
}

// claudePermTitle is the human-facing decision evidence: the tool and, when
// the input has an obvious one-line subject, that subject.
// claudePermToolKinds maps Claude's tool names onto the audit's shared
// rendering vocabulary (the UI's PERM_CODE_KINDS), the same way codex's
// mapApprovalToolKind classifies its approval methods. The raw tool name is
// never lost — it leads the decision title. Unknown tools map to "" and are
// rendered as prose rather than guessed into a code block.
var claudePermToolKinds = map[string]string{
	"Bash":         "execute",
	"BashOutput":   "execute",
	"KillShell":    "execute",
	"Edit":         "edit",
	"Write":        "edit",
	"NotebookEdit": "edit",
	"Read":         "read",
	"Glob":         "search",
	"Grep":         "search",
}

func claudePermToolKind(tool string) string { return claudePermToolKinds[tool] }

func claudePermTitle(ev claudePermissionEvent) string {
	if subj := claudePermSubject(ev.ToolInput); subj != "" {
		return ev.ToolName + ": " + subj
	}
	return ev.ToolName
}

func claudePermReason(ev claudePermissionEvent) string {
	if ev.AgentID == "" {
		return ""
	}
	kind := ev.AgentType
	if kind == "" {
		kind = "subagent"
	}
	return "subagent " + kind + " (" + ev.AgentID + ")"
}

// claudePermSubject pulls a short subject out of an undocumented tool_input
// shape. Unknown shapes yield "" — the tool name alone is still honest.
func claudePermSubject(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, key := range []string{"command", "file_path", "path", "url", "pattern"} {
		var s string
		if v, ok := m[key]; ok && json.Unmarshal(v, &s) == nil && s != "" {
			return truncateOneLine(s, 200)
		}
	}
	return ""
}

func truncateOneLine(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " "))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func (a *app) finishClaudePermRequest(bundle, src, name, kind string) {
	dest := filepath.Join(bundle, "perm", "processed", kind, name)
	_ = movePermFile(src, dest)
}

func movePermFile(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	return os.Rename(src, dest)
}

func claudePermissionHookCommand(execPath, hookDir string) (string, error) {
	if execPath == "" || hookDir == "" {
		return "", fmt.Errorf("claude permission hook: %w", errClaudeHookRejected)
	}
	return shellQuote(execPath) + " " + claudePermissionHookCmd + " --dir " + shellQuote(hookDir), nil
}
