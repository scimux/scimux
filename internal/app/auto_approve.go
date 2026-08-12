package app

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// autoApprovePhase is the server-owned one-turn lease phase.
//
//	off  → primed                 enabled while idle
//	off  → armed(lease,count=0)   enabled during an active turn
//	armed → armed(count+1)        eligible decision audited then delivered
//	armed → off                   manual disable, completion, interrupt,
//	                              /clear, /exit, delete, or process/session loss
//	primed → armed                next prompt accepted successfully
//	primed → off                  manual disable / exit / delete / manager loss
type autoApprovePhase string

const (
	autoPhaseOff    autoApprovePhase = "off"
	autoPhasePrimed autoApprovePhase = "primed"
	autoPhaseArmed  autoApprovePhase = "armed"
)

// autoApproveState is in-memory per-node lease state. Never persisted;
// restart is always off. Protected by a.mu; never hold a.mu across manager
// calls or session-log I/O.
type autoApproveState struct {
	Phase           autoApprovePhase
	LeaseID         string
	Count           int
	PendingAtEnable map[string]bool // RequestIDs already pending when enabled
	Attempted       map[string]bool // RequestIDs already tried (fail-closed no-retry)
	Error           string          // concise auto-approval error for chat
}

// autoApproveView is the authoritative JSON shape for the mutation endpoint
// and the polled chat response.
type autoApproveView struct {
	Supported bool   `json:"supported"`
	Enabled   bool   `json:"enabled"`
	Phase     string `json:"phase"`
	Count     int    `json:"count"`
	Error     string `json:"error,omitempty"`
}

// eligibleAutoAllow is the pure shared policy for ACP and Codex. An approval
// is eligible only when the lease is armed, the request was not already
// pending at enable, and exactly one offered option has semantic kind
// "allow" (one-time / request-scoped). That option's actual key is returned
// regardless of display position. Never infers semantics from labels; never
// automates allow_always, reject, reject_always, missing/unknown kinds, or
// ambiguous multi-allow menus.
func eligibleAutoAllow(armed, requestAtEnable bool, opts []PermOption) (selected PermOption, ok bool) {
	if !armed || requestAtEnable || len(opts) == 0 {
		return PermOption{}, false
	}
	var allows []PermOption
	for _, o := range opts {
		if o.Kind == "allow" {
			allows = append(allows, o)
		}
	}
	if len(allows) != 1 {
		return PermOption{}, false
	}
	return allows[0], true
}

// autoApproveSupported reports whether the node uses a structured transport
// that can prove permission options (codex app-server, ACP for grok/opencode/pi).
func autoApproveSupported(n *Node) bool {
	if n == nil {
		return false
	}
	switch n.transport() {
	case "acp", "codex":
		return true
	}
	return false
}

func newLeaseID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// autoApproveViewOf projects in-memory state for one node. Caller may hold a.mu.
func (a *app) autoApproveViewOf(n *Node) autoApproveView {
	if !autoApproveSupported(n) {
		return autoApproveView{Supported: false, Enabled: false, Phase: string(autoPhaseOff)}
	}
	st := a.autoApprove[n.ID]
	if st == nil || st.Phase == autoPhaseOff || st.Phase == "" {
		return autoApproveView{Supported: true, Enabled: false, Phase: string(autoPhaseOff)}
	}
	return autoApproveView{
		Supported: true,
		Enabled:   true,
		Phase:     string(st.Phase),
		Count:     st.Count,
		Error:     st.Error,
	}
}

// setAutoApproveEnabled applies the mutation. pendingAtEnable is the current
// pending RequestID (empty if none); live is the manager's Live() snapshot.
// Must not be called while holding a.mu across manager I/O — callers snapshot
// outside and pass values in.
func (a *app) setAutoApproveEnabled(id string, enabled bool, live, pendingRequestID string) autoApproveView {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.byID[id]
	if n == nil || !autoApproveSupported(n) {
		return autoApproveView{Supported: false, Enabled: false, Phase: string(autoPhaseOff)}
	}
	if !enabled {
		delete(a.autoApprove, id)
		return autoApproveView{Supported: true, Enabled: false, Phase: string(autoPhaseOff)}
	}
	st := &autoApproveState{
		LeaseID:         newLeaseID(),
		Count:           0,
		PendingAtEnable: map[string]bool{},
		Attempted:       map[string]bool{},
	}
	if pendingRequestID != "" {
		st.PendingAtEnable[pendingRequestID] = true
	}
	// Active turn → arm immediately; idle → primed until next prompt.
	if live == "active" {
		st.Phase = autoPhaseArmed
	} else {
		st.Phase = autoPhasePrimed
	}
	if a.autoApprove == nil {
		a.autoApprove = map[string]*autoApproveState{}
	}
	a.autoApprove[id] = st
	return a.autoApproveViewOf(n)
}

// disarmAutoApprove resets the lease to off (completion, interrupt, clear,
// exit, delete, manager loss, manual disable). Safe if already off.
func (a *app) disarmAutoApprove(id string) {
	a.mu.Lock()
	delete(a.autoApprove, id)
	a.mu.Unlock()
}

// armAutoApproveOnPrompt transitions primed → armed when a new prompt is
// accepted on an idle structured node. Also clears any stale armed state from
// a previous turn before accepting (safety net).
func (a *app) armAutoApproveOnPrompt(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.autoApprove[id]
	if st == nil {
		return
	}
	switch st.Phase {
	case autoPhasePrimed:
		st.Phase = autoPhaseArmed
		st.Count = 0
		st.Error = ""
		// Fresh arm for this turn: only pre-prompt pending (if any) is excluded.
		// Nothing is typically pending while idle/primed.
	case autoPhaseArmed:
		// Stale armed lease from a previous turn that was never disarmed —
		// refuse to leak into this prompt: reset count/lease.
		st.LeaseID = newLeaseID()
		st.Count = 0
		st.PendingAtEnable = map[string]bool{}
		st.Attempted = map[string]bool{}
		st.Error = ""
	}
}

// clearStaleArmedBeforePrompt removes an armed lease that survived past turn
// completion before accepting a new idle prompt (spec: lease cannot cross a turn).
func (a *app) clearStaleArmedBeforePrompt(id string, live string) {
	if live == "active" {
		return // still in a turn; leave armed lease alone
	}
	a.mu.Lock()
	st := a.autoApprove[id]
	if st != nil && st.Phase == autoPhaseArmed {
		// Previous turn ended without a clean disarm — drop the lease.
		delete(a.autoApprove, id)
	}
	a.mu.Unlock()
}

// maybeAutoApprove resolves an eligible structured pending request in the
// poll path before attention is published. Audit-before-delivery; fail-closed
// on append failure; honest error on delivery failure after a successful audit.
func (a *app) maybeAutoApprove(n *Node, pm procManager) {
	if n == nil || pm == nil || !autoApproveSupported(n) {
		return
	}

	// 1. Snapshot lease + pending (manager outside a.mu; lease under a.mu).
	pending, hasPerm := pm.Pending(n.ID)
	if !hasPerm || pending.RequestID == "" {
		return
	}

	a.mu.Lock()
	st := a.autoApprove[n.ID]
	if st == nil || st.Phase != autoPhaseArmed {
		a.mu.Unlock()
		return
	}
	if st.Attempted[pending.RequestID] {
		a.mu.Unlock()
		return
	}
	atEnable := st.PendingAtEnable[pending.RequestID]
	leaseID := st.LeaseID
	agent := n.Agent
	// 2. Pure eligibility (still under lock so Attempted marking is atomic
	// with the eligibility decision for this request id).
	sel, ok := eligibleAutoAllow(true, atEnable, pending.Options)
	if !ok {
		a.mu.Unlock()
		return
	}
	// Mark attempted before releasing the lock so concurrent poll ticks cannot
	// double-deliver the same request.
	if st.Attempted == nil {
		st.Attempted = map[string]bool{}
	}
	st.Attempted[pending.RequestID] = true
	a.mu.Unlock()

	// 3. Prepare resolution against the exact request using the selected
	// option's actual key (never hardcode "1" — Grok often puts allow at "2").
	optID, _, err := pm.PrepareResolve(n.ID, sel.Key)
	if err != nil {
		a.mu.Lock()
		if cur := a.autoApprove[n.ID]; cur != nil && cur.LeaseID == leaseID {
			cur.Error = "auto-approve prepare failed: " + err.Error()
		}
		a.mu.Unlock()
		return
	}

	// 4. Append and fsync the session-log decision event (outside a.mu).
	dec := sessionlog.DecisionEvent{
		Source:    "auto",
		LeaseID:   leaseID,
		RequestID: pending.RequestID,
		Agent:     agent,
		ToolKind:  pending.ToolKind,
		Title:     pending.Title,
		Reason:    pending.Reason,
		Options:   decOptionsFrom(pending.Options),
		Selected:  sessionlog.DecOption{Key: sel.Key, Name: sel.Name, Kind: sel.Kind},
	}
	if err := a.appendSessionEvent(n.ID, sessionlog.NewDecision(dec)); err != nil {
		// Fail closed: do not Deliver, do not increment. Already marked attempted.
		a.mu.Lock()
		if cur := a.autoApprove[n.ID]; cur != nil && cur.LeaseID == leaseID {
			cur.Error = "auto-approve audit failed: " + err.Error()
		}
		a.mu.Unlock()
		return
	}

	// 5. Deliver the prepared option.
	if err := pm.Deliver(n.ID, optID); err != nil {
		// Leave the decision audit intact; append a normal error event.
		_ = a.appendSessionEvent(n.ID, sessionlog.Event{
			T: "error",
			Error: fmt.Sprintf("auto-approve delivery failed (lease=%s request=%s): %v",
				leaseID, pending.RequestID, err),
		})
		a.mu.Lock()
		if cur := a.autoApprove[n.ID]; cur != nil && cur.LeaseID == leaseID {
			cur.Error = "auto-approve delivery failed: " + err.Error()
		}
		a.mu.Unlock()
		return
	}

	// 6. Increment count only after successful delivery.
	a.mu.Lock()
	if cur := a.autoApprove[n.ID]; cur != nil && cur.LeaseID == leaseID && cur.Phase == autoPhaseArmed {
		cur.Count++
		cur.Error = ""
	}
	a.mu.Unlock()
}

func decOptionsFrom(opts []PermOption) []sessionlog.DecOption {
	out := make([]sessionlog.DecOption, len(opts))
	for i, o := range opts {
		out[i] = sessionlog.DecOption{Key: o.Key, Name: o.Name, Kind: o.Kind}
	}
	return out
}

// handleAutoApprove is POST /api/nodes/{id}/auto-approve {"enabled":bool}.
func (a *app) handleAutoApprove(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if a.refuseEnded(w, n) {
		return
	}
	if !autoApproveSupported(n) {
		http.Error(w, "auto-approval is not available for this transport", http.StatusBadRequest)
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.Enabled == nil {
		http.Error(w, "bad request", 400)
		return
	}
	// Snapshot live/pending outside a.mu (manager calls).
	live, pendingID := "", ""
	if pm := a.proc(n); pm != nil {
		live = pm.Live(n.ID)
		if p, ok := pm.Pending(n.ID); ok {
			pendingID = p.RequestID
		}
	}
	// Supported transport without a live session may still prime (lease is
	// server-owned; process may return). live stays "" → primed.
	view := a.setAutoApproveEnabled(n.ID, *body.Enabled, live, pendingID)
	writeJSON(w, view)
}
