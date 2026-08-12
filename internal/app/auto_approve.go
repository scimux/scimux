package app

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// autoGateFor returns the per-node mutex that linearizes automatic decision
// commitment against enable/disable/re-arm and other lease-ending transitions.
// Callers must not hold a.mu when acquiring the returned lock (lock order:
// auto-gate, then a.mu briefly).
func (a *app) autoGateFor(id string) *sync.Mutex {
	a.autoGateMu.Lock()
	defer a.autoGateMu.Unlock()
	if a.autoGate == nil {
		a.autoGate = map[string]*sync.Mutex{}
	}
	g := a.autoGate[id]
	if g == nil {
		g = &sync.Mutex{}
		a.autoGate[id] = g
	}
	return g
}

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
//
// EnableIncarn + EnableMaxSeq is the authoritative enable cutoff: a request
// with the same session incarnation and sequence <= EnableMaxSeq was already
// issued at enable time (head or queued) and must stay manual. Empty
// EnableIncarn means no trustworthy boundary is bound yet — eligibility fails
// closed until arm rebinds a real PermissionBoundary (or enable saw one).
type autoApproveState struct {
	Phase        autoApprovePhase
	LeaseID      string
	Count        int
	EnableIncarn string
	EnableMaxSeq uint64
	Attempted    map[string]bool // RequestIDs already tried (fail-closed no-retry)
	Error        string          // concise auto-approval error for chat
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
// is eligible only when the lease is armed, the request was created after the
// enable cutoff (afterEnable), and exactly one offered option has semantic
// kind "allow" (one-time / request-scoped). That option's actual key is
// returned regardless of display position. Never infers semantics from labels;
// never automates allow_always, reject, reject_always, missing/unknown kinds,
// or ambiguous multi-allow menus.
func eligibleAutoAllow(armed, afterEnable bool, opts []PermOption) (selected PermOption, ok bool) {
	if !armed || !afterEnable || len(opts) == 0 {
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

// requestCreatedAfterEnable reports whether requestID was issued after the
// enable cutoff (incarnation + sequence watermark). Fail closed on:
//   - empty/malformed request IDs
//   - empty enable boundary (no trustworthy session incarnation bound yet)
//   - incarnation mismatch (session replaced without a clean disarm)
//
// Eligibility must be proven; when it cannot, leave the request manual.
func requestCreatedAfterEnable(enableIncarn string, enableMaxSeq uint64, requestID string) bool {
	if requestID == "" || enableIncarn == "" {
		return false
	}
	incarn, seq, ok := parsePermissionRequestID(requestID)
	if !ok {
		return false
	}
	if incarn != enableIncarn {
		return false
	}
	return seq > enableMaxSeq
}

// parsePermissionRequestID splits "incarn:seq" (hex incarn, decimal seq).
func parsePermissionRequestID(id string) (incarn string, seq uint64, ok bool) {
	i := strings.IndexByte(id, ':')
	if i <= 0 || i == len(id)-1 {
		return "", 0, false
	}
	n, err := strconv.ParseUint(id[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return id[:i], n, true
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

// disableAutoApprove turns the lease off under the per-node gate. Fast path:
// no Live/Pending manager snapshot — disable must not wait on manager I/O
// before reaching the barrier.
func (a *app) disableAutoApprove(id string) autoApproveView {
	g := a.autoGateFor(id)
	g.Lock()
	defer g.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.autoApprove, id)
	n := a.byID[id]
	if n == nil || !autoApproveSupported(n) {
		return autoApproveView{Supported: false, Enabled: false, Phase: string(autoPhaseOff)}
	}
	return autoApproveView{Supported: true, Enabled: false, Phase: string(autoPhaseOff)}
}

// enableAutoApprove arms or primes a lease. It wins the per-node gate *before*
// reading Live/PermissionBoundary so a concurrent decision cannot finish and
// advance the queue between the enable snapshot and the lease commit. The
// cutoff is session incarnation + highest issued sequence (covers Codex queue).
// Rejects enable while a node is mid-delete (gate held through teardown) by
// virtue of waiting on the same gate; after delete the node is gone.
func (a *app) enableAutoApprove(id string, pm procManager) autoApproveView {
	g := a.autoGateFor(id)
	g.Lock()
	defer g.Unlock()

	// Authoritative snapshot only after winning the gate.
	live, enableIncarn, enableMaxSeq := "", "", uint64(0)
	if pm != nil {
		live = pm.Live(id)
		if incarn, maxSeq, ok := pm.PermissionBoundary(id); ok {
			enableIncarn, enableMaxSeq = incarn, maxSeq
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.byID[id]
	if n == nil || !autoApproveSupported(n) {
		return autoApproveView{Supported: false, Enabled: false, Phase: string(autoPhaseOff)}
	}
	st := &autoApproveState{
		LeaseID:      newLeaseID(),
		Count:        0,
		EnableIncarn: enableIncarn,
		EnableMaxSeq: enableMaxSeq,
		Attempted:    map[string]bool{},
	}
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

// setAutoApproveEnabled is the test-friendly mutation that applies a pre-known
// live/cutoff snapshot under the gate. Production HTTP uses enableAutoApprove
// / disableAutoApprove so the snapshot is taken after winning the gate.
func (a *app) setAutoApproveEnabled(id string, enabled bool, live, enableIncarn string, enableMaxSeq uint64) autoApproveView {
	if !enabled {
		return a.disableAutoApprove(id)
	}
	g := a.autoGateFor(id)
	g.Lock()
	defer g.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.byID[id]
	if n == nil || !autoApproveSupported(n) {
		return autoApproveView{Supported: false, Enabled: false, Phase: string(autoPhaseOff)}
	}
	st := &autoApproveState{
		LeaseID:      newLeaseID(),
		Count:        0,
		EnableIncarn: enableIncarn,
		EnableMaxSeq: enableMaxSeq,
		Attempted:    map[string]bool{},
	}
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
// exit, delete, manager loss, manual disable). Safe if already off. Takes the
// per-node auto-gate so any in-flight automatic decision for this lease
// finishes (or is refused) before the barrier completes.
func (a *app) disarmAutoApprove(id string) {
	g := a.autoGateFor(id)
	g.Lock()
	defer g.Unlock()
	a.mu.Lock()
	delete(a.autoApprove, id)
	a.mu.Unlock()
}

// disarmAutoApproveIfLease clears the lease only when it still matches the
// leaseID observed when the poller decided to disarm. A concurrent re-arm
// that installed a new LeaseID is left intact (stale-disarm linearization).
// Empty leaseID means no lease was present at observation — do not wipe a
// later re-arm.
func (a *app) disarmAutoApproveIfLease(id, leaseID string) {
	if leaseID == "" {
		return
	}
	g := a.autoGateFor(id)
	g.Lock()
	defer g.Unlock()
	a.mu.Lock()
	st := a.autoApprove[id]
	if st != nil && st.LeaseID == leaseID {
		delete(a.autoApprove, id)
	}
	a.mu.Unlock()
}

// armAutoApproveOnPrompt transitions primed → armed after a prompt is accepted.
// boundaryIncarn/maxSeq must be the pre-send PermissionBoundary (sampled before
// the turn starts), never a post-Send sample — otherwise the turn's first
// permission can race into the cutoff and become falsely pre-enable.
//
// If hasBound is false, any prior cutoff is cleared so eligibility fails closed.
// Already-armed leases are left untouched (a concurrent enable must not be
// mistaken for stale state and rotated). Cross-turn armed cleanup is
// clearStaleArmedBeforePrompt / the poller, not this path.
//
// Caller may already hold the auto-gate (acceptStructuredPrompt); if not,
// this takes it.
func (a *app) armAutoApproveOnPrompt(id string, boundaryIncarn string, boundaryMaxSeq uint64, hasBound bool) {
	g := a.autoGateFor(id)
	g.Lock()
	defer g.Unlock()
	a.armAutoApproveOnPromptLocked(id, "", boundaryIncarn, boundaryMaxSeq, hasBound)
}

// armAutoApproveOnPromptLocked is the gate-held core. primedLeaseID, when
// non-empty, requires the primed lease still match that id before arming so a
// concurrent enable (if the gate was released) cannot be overwritten.
func (a *app) armAutoApproveOnPromptLocked(id, primedLeaseID, boundaryIncarn string, boundaryMaxSeq uint64, hasBound bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.autoApprove[id]
	if st == nil {
		return
	}
	if st.Phase != autoPhasePrimed {
		// Armed or off: do not rotate lease ID / reset count.
		return
	}
	if primedLeaseID != "" && st.LeaseID != primedLeaseID {
		// A different lease was installed; leave it alone.
		return
	}
	st.Phase = autoPhaseArmed
	st.Count = 0
	st.Error = ""
	// Always clear first: unavailable pre-send boundary must not retain an
	// older incarnation fence from a previous enable.
	st.EnableIncarn = ""
	st.EnableMaxSeq = 0
	if hasBound {
		st.EnableIncarn = boundaryIncarn
		st.EnableMaxSeq = boundaryMaxSeq
	}
}

// clearStaleArmedBeforePrompt removes an armed lease that survived past turn
// completion before accepting a new idle prompt (spec: lease cannot cross a turn).
func (a *app) clearStaleArmedBeforePrompt(id string, live string) {
	if live == "active" {
		return // still in a turn; leave armed lease alone
	}
	g := a.autoGateFor(id)
	g.Lock()
	defer g.Unlock()
	a.clearStaleArmedBeforePromptLocked(id)
}

func (a *app) clearStaleArmedBeforePromptLocked(id string) {
	a.mu.Lock()
	st := a.autoApprove[id]
	if st != nil && st.Phase == autoPhaseArmed {
		delete(a.autoApprove, id)
	}
	a.mu.Unlock()
}

// acceptStructuredPrompt linearizes pre-send boundary sample → Send → primed
// arm under the per-node auto-gate. The cutoff is the PermissionBoundary
// before the turn starts, so the first permission issued by that turn remains
// eligible after arm. Concurrent enable/disable wait on the same gate and
// cannot install a lease between Send and arm.
func (a *app) acceptStructuredPrompt(n *Node, pm procManager, text string) error {
	g := a.autoGateFor(n.ID)
	g.Lock()
	defer g.Unlock()

	live := pm.Live(n.ID)
	if live != "active" {
		a.clearStaleArmedBeforePromptLocked(n.ID)
	}

	// Pre-send boundary: permissions with seq > maxSeq (same incarn) are
	// post-enable once armed — including those this turn is about to issue.
	boundaryIncarn, boundaryMaxSeq, hasBound := "", uint64(0), false
	if incarn, maxSeq, ok := pm.PermissionBoundary(n.ID); ok {
		boundaryIncarn, boundaryMaxSeq, hasBound = incarn, maxSeq, true
	}

	a.mu.Lock()
	primedLeaseID := ""
	if st := a.autoApprove[n.ID]; st != nil && st.Phase == autoPhasePrimed {
		primedLeaseID = st.LeaseID
	}
	a.mu.Unlock()

	if err := pm.Send(n.ID, text); err != nil {
		return err
	}

	if primedLeaseID != "" {
		a.armAutoApproveOnPromptLocked(n.ID, primedLeaseID, boundaryIncarn, boundaryMaxSeq, hasBound)
	}
	if a.acceptArmHook != nil {
		a.acceptArmHook(n.ID)
	}
	return nil
}

// maybeAutoApprove resolves an eligible structured pending request in the
// poll path before attention is published. Audit-before-delivery; fail-closed
// on append failure; honest error on delivery failure after a successful audit.
//
// Decision commitment is linearized with enable/disable/re-arm via the
// per-node auto-gate: once a disable/re-arm response has completed, no
// decision belonging to the prior lease may subsequently be delivered. The
// manager (not a second app-side snapshot) is the final authority that the
// request is still current — PrepareResolve binds expectedRequestID.
func (a *app) maybeAutoApprove(n *Node, pm procManager) {
	if n == nil || pm == nil || !autoApproveSupported(n) {
		return
	}

	// 1. Snapshot pending outside locks (manager call).
	pending, hasPerm := pm.Pending(n.ID)
	if !hasPerm || pending.RequestID == "" {
		return
	}

	// 2. Cheap pre-check under a.mu (no gate yet — avoid holding a.mu while
	// waiting on an in-flight decision). Eligibility is re-checked under the
	// gate before commit.
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
	after := requestCreatedAfterEnable(st.EnableIncarn, st.EnableMaxSeq, pending.RequestID)
	sel, ok := eligibleAutoAllow(true, after, pending.Options)
	if !ok {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()

	// 3. Acquire the per-node decision gate. Disable/re-arm wait on this lock,
	// so after they return no old-lease delivery can still be in flight.
	g := a.autoGateFor(n.ID)
	g.Lock()
	defer g.Unlock()

	// 4. Re-check lease + eligibility under a.mu while holding the gate.
	// Mark attempted so concurrent poll ticks cannot double-commit.
	a.mu.Lock()
	st = a.autoApprove[n.ID]
	if st == nil || st.Phase != autoPhaseArmed {
		a.mu.Unlock()
		return
	}
	if st.Attempted[pending.RequestID] {
		a.mu.Unlock()
		return
	}
	after = requestCreatedAfterEnable(st.EnableIncarn, st.EnableMaxSeq, pending.RequestID)
	leaseID := st.LeaseID
	agent := n.Agent
	sel, ok = eligibleAutoAllow(true, after, pending.Options)
	if !ok {
		a.mu.Unlock()
		return
	}
	if st.Attempted == nil {
		st.Attempted = map[string]bool{}
	}
	st.Attempted[pending.RequestID] = true
	a.mu.Unlock()

	// 5. Prepare against the exact request the policy evaluated. The manager
	// compares expectedRequestID under its session lock; if A was replaced by
	// B, B is not approved merely because it shares A's key layout.
	optID, _, err := pm.PrepareResolve(n.ID, pending.RequestID, sel.Key)
	if err != nil {
		a.mu.Lock()
		if cur := a.autoApprove[n.ID]; cur != nil && cur.LeaseID == leaseID {
			cur.Error = "auto-approve prepare failed: " + err.Error()
		}
		a.mu.Unlock()
		return
	}

	// 6. Append and fsync the session-log decision event (outside a.mu).
	// All audit fields describe the request accepted by prepare (pending snapshot).
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

	// 7. Deliver the prepared option (still under the auto-gate).
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

	// 8. Increment count only after successful delivery, and only if this
	// lease is still the armed one (disable/re-arm after we won the gate is
	// impossible while we hold it; this is defense in depth).
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
	if !*body.Enabled {
		// Fast path: no Live/Pending snapshot before the barrier.
		writeJSON(w, a.disableAutoApprove(n.ID))
		return
	}
	// Enable/re-arm: gate first, then authoritative Live + PermissionBoundary.
	writeJSON(w, a.enableAutoApprove(n.ID, a.proc(n)))
}
