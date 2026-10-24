package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
)

// Manual Claude question and plan replies. The ordinary auto-approval helper
// stays on its existing path unless a bundle was minted with the experimental
// gate and the tool is one of the two interaction tools.

const interactionFileMax = 96 * 1024

const interactionTerminalJSON = `{"terminal":true}`

var (
	errInteractionConflict   = errors.New("claude interaction: conflict")
	errInteractionAudit      = errors.New("claude interaction: audit failed")
	errInteractionIO         = errors.New("claude interaction: io")
	errInteractionBadRequest = errors.New("claude interaction: bad request")
)

// claudeInteractionSubdirs is created only for bundles minted with the gate on.
// It is not part of claudeHookBundleSubdirs, so gate-off layouts stay identical.
var claudeInteractionSubdirs = []string{
	filepath.Join("perm", "interact"),
	filepath.Join("perm", "interact", "req"),
	filepath.Join("perm", "interact", "ans"),
	filepath.Join("perm", "interact", "claim"),
	filepath.Join("perm", "interact", "recorded"),
	filepath.Join("perm", "interact", "ack"),
	filepath.Join("perm", "interact", "closed"),
	filepath.Join("perm", "interact", "expired"),
	filepath.Join("perm", "interact", "interrupted"),
	filepath.Join("perm", "interact", "waiter"),
	filepath.Join("perm", "interact", "cancel"),
}

type claudeInteractionCaller struct {
	NodeID     string
	SessionID  string
	BundleID   string
	Generation int
	Turn       string
	TurnGen    int
}

type claudeInteractionReceipt struct {
	Status    string
	Delivery  string
	RequestID string
	Emitted   bool
	Phase     string
}

type claudeInteractionAck struct {
	Emitted  bool   `json:"emitted"`
	Terminal bool   `json:"terminal,omitempty"`
	Error    string `json:"error,omitempty"`
}

// claudeInteractionRequest is the helper-published pending record.
type claudeInteractionRequest struct {
	ID             string          `json:"id"`
	SessionID      string          `json:"session_id"`
	Tool           string          `json:"tool"`
	PromptID       string          `json:"prompt_id"`
	PermissionMode string          `json:"permission_mode"`
	Turn           string          `json:"turn"`
	TurnGen        int             `json:"turn_gen"`
	BundleID       string          `json:"bundle_id"`
	Generation     int             `json:"generation"`
	NodeID         string          `json:"node_id"`
	At             string          `json:"at"`
	AnswerUntil    string          `json:"answer_until"`
	HelperUntil    string          `json:"helper_until"`
	Original       json.RawMessage `json:"original"`
	Digest         string          `json:"digest"`
	HelperWaiter   bool            `json:"helper_waiter,omitempty"`
}

type interactionSyncFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

func interactionWaitBudget(requested time.Duration) time.Duration {
	if requested > 0 && requested < permHookDeadline {
		return requested
	}
	return interactionHelperDeadline
}

func interactionAnswerDeadline(start time.Time, budget time.Duration) time.Time {
	if budget <= 0 {
		return start
	}
	margin := interactionRejectMargin
	if budget < interactionHelperDeadline {
		margin = budget / 3
	}
	if margin <= 0 || margin >= budget {
		return start
	}
	return start.Add(budget - margin)
}

var interactionReadCapabilities = readClaudeHookCapabilities

type interactionCapabilityCacheEntry struct {
	info os.FileInfo
	caps claudeHookCapabilities
}

var interactionCapabilityCache sync.Map

func bundleSupportsInteraction(bundle string) bool {
	info, err := os.Stat(filepath.Join(bundle, "capabilities.json"))
	if err != nil {
		interactionCapabilityCache.Delete(bundle)
		return false
	}
	var caps claudeHookCapabilities
	if value, ok := interactionCapabilityCache.Load(bundle); ok && sameInteractionFile(value.(interactionCapabilityCacheEntry).info, info) {
		caps = value.(interactionCapabilityCacheEntry).caps
	} else {
		var valid bool
		caps, valid = interactionReadCapabilities(bundle)
		if !valid {
			return false
		}
		interactionCapabilityCache.Store(bundle, interactionCapabilityCacheEntry{info: info, caps: caps})
	}
	// Capabilities are immutable after minting. Detect replacement anyway,
	// and keep checking the executable so a moved binary fails closed.
	if caps.Interaction < 1 || !claudeBundleExecUsable(caps) {
		return false
	}
	return true
}

func sameInteractionFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func maybeServeClaudeInteraction(dir string, ev claudePermissionEvent, id string, stdout io.Writer, deadline, every time.Duration) (bool, error) {
	if !bundleSupportsInteraction(dir) || !claudeQuestionTools[ev.ToolName] {
		return false, nil
	}
	return serveClaudeInteraction(dir, ev, id, stdout, interactionWaitBudget(deadline), every, time.Now, time.Sleep)
}

func serveClaudeInteraction(dir string, ev claudePermissionEvent, id string, stdout io.Writer, budget, every time.Duration, now func() time.Time, sleep func(time.Duration)) (bool, error) {
	if !bundleSupportsInteraction(dir) || !claudeQuestionTools[ev.ToolName] {
		return false, nil
	}
	if !safePathComponent(id) {
		return true, errInteractionBadRequest
	}
	model, _ := classifyClaudeInteraction(ev.ToolName, ev.ToolInput)
	if model.Native {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(dir, "perm", "interact")); err != nil {
		return true, fmt.Errorf("%w: interact directory", errInteractionIO)
	}
	_ = retireExpiredInteractionPayloads(dir, now())
	activeID, activeOK := readInteractionActive(dir)
	if activeOK && activeID != id {
		return true, nil
	}
	if interactionReplayBlocked(dir, id) {
		return true, nil
	}
	// id was already checked, so this path cannot escape the bundle.
	waiter := filepath.Join(dir, "perm", "interact", "waiter", id)
	if err := exclWrite(waiter, []byte(id)); err != nil {
		return true, nil
	}
	live, err := os.OpenFile(waiter, os.O_RDWR, 0)
	if err != nil {
		return true, err
	}
	defer live.Close()
	if err := interactionFlock(int(live.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true, err
	}
	start := now()
	if !activeOK {
		if err := publishInteractionRequest(dir, ev, id, model, start, budget); err != nil {
			return true, err
		}
	}
	helperUntil := start.Add(budget)
	for {
		if !helperMayContinue(dir, id, now(), helperUntil) {
			_ = expireInteraction(dir, id)
			return true, nil
		}
		ans, ansErr := readInteractionAns(dir, id)
		if ansErr == nil && interactionAnswerReady(ans) {
			return emitInteractionAnswer(dir, id, ans, stdout, now, helperUntil)
		}
		if interactionFileExists(dir, "closed", id) {
			return finishInteractionTerminal(dir, id)
		}
		sleep(every)
	}
}

// helperMayContinue is the deadline and expiry gate. It runs before an answer
// file is read so a late or expired request cannot be emitted.
func helperMayContinue(dir, id string, now, helperUntil time.Time) bool {
	if !interactionDeadlineOpen(now, helperUntil) || interactionFileExists(dir, "expired", id) {
		return false
	}
	rec, err := readInteractionRequest(dir, id)
	if err != nil {
		return false
	}
	return interactionDeadlineOpen(now, parseInteractionTime(rec.HelperUntil))
}

func interactionDeadlineOpen(now, until time.Time) bool {
	if until.IsZero() {
		return true
	}
	return now.Before(until)
}

// interactionAnswerReady accepts only a complete decision. An empty file, a
// partial document, or JSON without allow/deny or an explicit terminal flag
// is not a response the helper may acknowledge.
func interactionAnswerReady(raw []byte) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	if interactionAnsTerminal(raw) {
		return true
	}
	var doc struct {
		HookSpecificOutput struct {
			Decision struct {
				Behavior string `json:"behavior"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	switch doc.HookSpecificOutput.Decision.Behavior {
	case "allow", "deny":
		return true
	default:
		return false
	}
}

func interactionReplayBlocked(dir, id string) bool {
	if interactionFileExists(dir, "recorded", id) || interactionFileExists(dir, "waiter", id) {
		return true
	}
	if interactionFileExists(dir, "cancel", id) {
		return true
	}
	if interactionFileExists(dir, "ack", id+".json") {
		return true
	}
	return interactionFileExists(dir, "expired", id) ||
		interactionFileExists(dir, "interrupted", id) ||
		interactionFileExists(dir, "closed", id)
}

func finishInteractionTerminal(dir, id string) (bool, error) {
	_ = noteClaudeInteractionEmission(dir, id, claudeInteractionAck{Terminal: true})
	removeInteractionActive(dir, id)
	return true, nil
}

func emitInteractionAnswer(dir, id string, ans []byte, stdout io.Writer, now func() time.Time, helperUntil time.Time) (bool, error) {
	unlock, err := lockInteractionFence(dir)
	if err != nil {
		return true, err
	}
	defer unlock()
	if interactionFileExists(dir, "closed", id) {
		return finishInteractionTerminal(dir, id)
	}
	if !helperMayContinue(dir, id, now(), helperUntil) || !interactionTurnAuthoritative(dir, id) {
		_ = expireInteraction(dir, id)
		return true, nil
	}
	// Reading an answer before taking the fence is only a readiness hint.
	// Publication can still fail and withdraw that name before the fence is
	// released. Never emit bytes whose published file no longer matches.
	published, err := readInteractionAns(dir, id)
	if err != nil || !bytes.Equal(published, ans) {
		_ = expireInteraction(dir, id)
		return true, nil
	}
	if interactionAnsTerminal(ans) {
		return finishInteractionTerminal(dir, id)
	}
	n, werr := stdout.Write(ans)
	emitted := werr == nil && n == len(ans)
	ack := claudeInteractionAck{Emitted: emitted}
	if !emitted {
		ack.Error = "stdout"
		if werr == nil {
			werr = io.ErrShortWrite
		}
	}
	_ = noteClaudeInteractionEmission(dir, id, ack)
	removeInteractionActive(dir, id)
	if !emitted {
		return true, werr
	}
	return true, nil
}

func interactionTurnAuthoritative(dir, id string) bool {
	rec, err := readInteractionRequest(dir, id)
	if err != nil {
		return false
	}
	if !interactionGenerationStillCurrent(dir, rec) {
		return false
	}
	if rec.Turn == "" {
		return true
	}
	live, ok := readClaudeAcceptedTurn(filepath.Join(dir, "perm"))
	return ok && live.Turn == rec.Turn && live.Gen == rec.TurnGen
}

// claudeBindingOwner is the in-process view of one bundle's authoritative
// generation. The helper process cannot see it; perm/generation.json is the
// fence that process reads. A generation of zero is not a binding.
type claudeBindingOwner struct {
	app    *app
	nodeID string
}

var claudeBindingOwners sync.Map

func registerClaudeBindingOwner(bundle string, a *app, nodeID string) {
	if bundle == "" || a == nil || nodeID == "" {
		return
	}
	claudeBindingOwners.Store(bundle, &claudeBindingOwner{app: a, nodeID: nodeID})
}

func lookupClaudeBinding(bundle string) (int, string, bool) {
	v, ok := claudeBindingOwners.Load(bundle)
	if !ok {
		return 0, "", false
	}
	src, _ := v.(*claudeBindingOwner)
	if src == nil || src.app == nil {
		return 0, "", false
	}
	src.app.mu.Lock()
	defer src.app.mu.Unlock()
	if src.app.claudePermBundleLocked(src.nodeID) != bundle {
		return 0, "", false
	}
	gen := src.app.claudeGenerationLocked(src.nodeID)
	if gen < 1 {
		return 0, "", false
	}
	return gen, src.nodeID, true
}

func authoritativeInteractionGeneration(bundle string) (int, string, bool) {
	if gen, node, ok := lookupClaudeBinding(bundle); ok && gen > 0 {
		return gen, node, true
	}
	return readInteractionGeneration(bundle)
}

// claudeBindingFence is the helper-visible authority for one live bundle.
// The owning worker is the only writer. A muxer snapshot must not rewrite it.
type claudeBindingFence struct {
	Generation int    `json:"generation"`
	NodeID     string `json:"node_id"`
	SessionID  string `json:"session_id,omitempty"`
}

func publishClaudeBindingFenceFile(bundle, nodeID string, gen int, sessionID string) error {
	if gen < 1 || nodeID == "" || bundle == "" || !bundleSupportsInteraction(bundle) {
		return nil
	}
	perm := filepath.Join(bundle, "perm")
	st, err := os.Stat(perm)
	if err != nil || !st.IsDir() {
		if err == nil {
			err = fmt.Errorf("%w: generation", errInteractionIO)
		}
		return err
	}
	cur, ok := readClaudeBindingFence(bundle)
	writeSession := sessionID
	if writeSession == "" && ok {
		writeSession = cur.SessionID
	}
	if ok && cur.Generation == gen && cur.NodeID == nodeID && cur.SessionID == writeSession {
		return nil
	}
	return writeClaudePermFile(filepath.Join(perm, "generation.json"), claudeBindingFence{
		Generation: gen, NodeID: nodeID, SessionID: writeSession,
	})
}

// publishClaudeBindingFenceExclusive writes the helper fence while holding
// the same lock emission holds. A failed write removes the fence so a request
// that was already bound cannot keep emitting under the previous authority.
func publishClaudeBindingFenceExclusive(bundle, nodeID, sessionID string, gen int) error {
	if gen < 1 || nodeID == "" || bundle == "" || !bundleSupportsInteraction(bundle) {
		return nil
	}
	unlock, err := lockInteractionFence(bundle)
	if err != nil {
		// The same generation and session are already published. A lock glitch
		// must not revoke that authority. A binding change that cannot take
		// the lock removes the fence so the previous session cannot emit.
		if bindingFenceAlreadyAuthoritative(bundle, nodeID, sessionID, gen) {
			return nil
		}
		_ = os.Remove(filepath.Join(bundle, "perm", "generation.json"))
		return err
	}
	defer unlock()
	if err := publishClaudeBindingFenceFile(bundle, nodeID, gen, sessionID); err != nil {
		_ = os.Remove(filepath.Join(bundle, "perm", "generation.json"))
		return err
	}
	return nil
}

func bindingFenceAlreadyAuthoritative(bundle, nodeID, sessionID string, gen int) bool {
	cur, ok := readClaudeBindingFence(bundle)
	if !ok || cur.Generation != gen || cur.NodeID != nodeID {
		return false
	}
	if sessionID == "" {
		return true
	}
	return cur.SessionID == sessionID
}

func (a *app) publishClaudeBindingFence(nodeID string) {
	if a == nil || nodeID == "" {
		return
	}
	a.mu.Lock()
	bundle := a.claudePermBundleLocked(nodeID)
	gen := a.claudeGenerationLocked(nodeID)
	session := ""
	if n := a.byID[nodeID]; n != nil {
		session = n.SessionID
	}
	a.mu.Unlock()
	if bundle == "" {
		return
	}
	registerClaudeBindingOwner(bundle, a, nodeID)
	if gen < 1 {
		return
	}
	_ = publishClaudeBindingFenceExclusive(bundle, nodeID, session, gen)
}

func stampInteractionGeneration(dir string, rec *claudeInteractionRequest) error {
	if gen, node, ok := lookupClaudeBinding(dir); ok && gen > 0 {
		rec.Generation = gen
		rec.NodeID = node
		file, fileOK := readClaudeBindingFence(dir)
		// A helper stamp may fill a missing session. It must not replace the
		// session a binding commit already published.
		session := rec.SessionID
		if fileOK && file.SessionID != "" {
			session = file.SessionID
		}
		if !fileOK || file.Generation != gen || file.NodeID != node || file.SessionID != session {
			if err := publishClaudeBindingFenceFile(dir, node, gen, session); err != nil {
				return fmt.Errorf("%w: generation", errInteractionIO)
			}
		}
		return nil
	}
	if file, ok := readClaudeBindingFence(dir); ok {
		rec.Generation = file.Generation
		rec.NodeID = file.NodeID
	}
	return nil
}

func interactionGenerationStillCurrent(dir string, rec claudeInteractionRequest) bool {
	fence, fileOK := readClaudeBindingFence(dir)
	// The helper process has no owner registry. A request that was bound to a
	// generation loses its authority when that file is missing or unreadable.
	if rec.Generation > 0 && !fileOK {
		return false
	}
	if fileOK {
		if rec.Generation != fence.Generation {
			return false
		}
		if fence.NodeID != "" && rec.NodeID != "" && fence.NodeID != rec.NodeID {
			return false
		}
		if fence.SessionID != "" && rec.SessionID != "" && fence.SessionID != rec.SessionID {
			return false
		}
	} else if fileExists(filepath.Join(dir, "perm", "generation.json")) {
		return false
	}
	if liveGen, liveNode, liveOK := lookupClaudeBinding(dir); liveOK && liveGen > 0 {
		if rec.Generation != liveGen {
			return false
		}
		if liveNode != "" && rec.NodeID != "" && liveNode != rec.NodeID {
			return false
		}
	}
	return true
}

func expireInteraction(dir, id string) error {
	if path, err := interactionChild(dir, "expired", id); err == nil {
		if werr := exclWrite(path, []byte("expired")); werr != nil && !os.IsExist(werr) {
			return werr
		}
	}
	removeInteractionActive(dir, id)
	return nil
}

func expireClaudeInteractionSession(bundle, sessionID string) {
	unlock, err := lockInteractionFence(bundle)
	if err != nil {
		return
	}
	defer unlock()
	id, ok := readInteractionActive(bundle)
	if !ok {
		return
	}
	rec, err := readInteractionRequest(bundle, id)
	if err == nil && rec.SessionID == sessionID {
		_ = expireInteraction(bundle, id)
	}
}

func removeInteractionActive(dir, id string) {
	active, ok := readInteractionActive(dir)
	if ok && active != id {
		return
	}
	_ = os.Remove(filepath.Join(dir, "perm", "interact", "active.json"))
}

func publishInteractionRequest(dir string, ev claudePermissionEvent, id string, model claudeInteractionModel, start time.Time, budget time.Duration) error {
	unlock, err := lockInteractionFence(dir)
	if err != nil {
		return fmt.Errorf("%w: fence", errInteractionIO)
	}
	defer unlock()
	rec := claudeInteractionRequest{
		ID:             id,
		SessionID:      ev.SessionID,
		Tool:           ev.ToolName,
		PromptID:       ev.PromptID,
		PermissionMode: ev.PermissionMode,
		BundleID:       filepath.Base(dir),
		At:             start.UTC().Format(time.RFC3339Nano),
		AnswerUntil:    interactionAnswerDeadline(start, budget).UTC().Format(time.RFC3339Nano),
		HelperUntil:    start.Add(budget).UTC().Format(time.RFC3339Nano),
		Original:       append([]byte(nil), model.Original...),
		Digest:         model.Digest,
		HelperWaiter:   interactionFileExists(dir, "waiter", id),
	}
	if turn, ok := readClaudeAcceptedTurn(filepath.Join(dir, "perm")); ok {
		rec.Turn = turn.Turn
		rec.TurnGen = turn.Gen
	}
	if err := stampInteractionGeneration(dir, &rec); err != nil {
		return err
	}
	reqPath := filepath.Join(dir, "perm", "interact", "req", id+".json")
	if err := writeClaudePermFile(reqPath, rec); err != nil {
		return fmt.Errorf("%w: request", errInteractionIO)
	}
	active := filepath.Join(dir, "perm", "interact", "active.json")
	if err := exclWrite(active, []byte(`{"id":"`+id+`"}`)); err != nil {
		return fmt.Errorf("%w: active", errInteractionIO)
	}
	return nil
}

// submitClaudeInteraction freezes now for callers that already sampled a clock.
// Production uses submitClaudeInteractionClock so the deadline is read again
// after audit and immediately before the answer is published.
func submitClaudeInteraction(bundle string, caller claudeInteractionCaller, requestID string, reply claudeInteractionReply, now time.Time, audit func(sessionlog.Event) error) (claudeInteractionReceipt, error) {
	frozen := now
	return submitClaudeInteractionClock(bundle, caller, requestID, reply, func() time.Time { return frozen }, audit)
}

func submitClaudeInteractionClock(bundle string, caller claudeInteractionCaller, requestID string, reply claudeInteractionReply, now func() time.Time, audit func(sessionlog.Event) error) (receipt claudeInteractionReceipt, resultErr error) {
	var zero claudeInteractionReceipt
	if now == nil {
		now = time.Now
	}
	if audit == nil {
		return zero, fmt.Errorf("%w: writer missing", errInteractionAudit)
	}
	root, err := interactionRoot(bundle)
	if err != nil {
		return zero, err
	}
	if !safePathComponent(requestID) {
		return zero, errInteractionBadRequest
	}
	rec, err := readInteractionRequest(bundle, requestID)
	if err != nil {
		return zero, err
	}
	if interactionSubmissionExpired(bundle, requestID, rec, now()) {
		return zero, fmt.Errorf("%w: expired", errInteractionConflict)
	}
	if err := matchInteractionIdentity(bundle, caller, rec); err != nil {
		return zero, err
	}
	phase := claudeInteractionPhase(bundle, requestID, now())
	if phase != "pending" {
		if phase == "" {
			return zero, fmt.Errorf("%w: stale", errInteractionConflict)
		}
		return zero, fmt.Errorf("%w: %s", errInteractionConflict, phase)
	}
	claim := filepath.Join(root, "claim", requestID)
	if err := exclWrite(claim, []byte("claim")); err != nil {
		if os.IsExist(err) {
			return zero, fmt.Errorf("%w: claim", errInteractionConflict)
		}
		return zero, fmt.Errorf("%w: claim", errInteractionIO)
	}
	model, _ := classifyClaudeInteraction(rec.Tool, rec.Original)
	if model.Native {
		_ = os.Remove(claim)
		return zero, errInteractionBadRequest
	}
	env, err := buildClaudeInteractionReply(model, reply)
	if err != nil {
		_ = os.Remove(claim)
		return zero, fmt.Errorf("%w: %v", errInteractionBadRequest, err)
	}
	event := manualInteractionEvent(rec, env)
	// Audit stays outside the fence. Turn publication takes the same lock, and
	// holding it across the callback would deadlock a boundary change.
	if err := audit(event); err != nil {
		_ = os.Remove(claim)
		return zero, fmt.Errorf("%w: %v", errInteractionAudit, err)
	}
	// The first record is the human's intent, not a delivery claim. If anything
	// after that append fails, leave a correlated application-authored outcome.
	// Registered before the unlock defer, this runs after the fence is released.
	defer func() {
		if resultErr == nil {
			return
		}
		// Build a separate application record: cloning intent would repeat human
		// authorship and content. Raw errors can contain private filesystem paths.
		decision := sessionlog.DecisionEvent{
			Source:    "application",
			RequestID: requestID,
			Agent:     "claude",
			Selected:  sessionlog.DecOption{Name: "Not delivered", Kind: "not_delivered"},
			Outcome: &sessionlog.DeliveryOutcome{
				Delivery:   "not_delivered",
				AppContext: "Application delivery outcome: this submission was rejected before delivery.",
			},
		}
		if !errors.Is(resultErr, errInteractionConflict) {
			// An I/O error cannot attest withdrawal or non-consumption. Keep
			// uncertainty explicit instead of inventing a negative receipt.
			decision.Outcome.Delivery = "unconfirmed"
			decision.Outcome.AppContext = "Application delivery outcome: submission failed; delivery is unconfirmed."
			decision.Selected = sessionlog.DecOption{Name: "Delivery unconfirmed", Kind: "unconfirmed"}
		}
		if err := audit(sessionlog.NewDecision(decision)); err != nil {
			resultErr = fmt.Errorf("%w: outcome for %v: %v", errInteractionAudit, resultErr, err)
		}
	}()
	unlock, lockErr := lockInteractionFence(bundle)
	if lockErr != nil {
		_ = os.Remove(claim)
		return zero, fmt.Errorf("%w: fence", errInteractionIO)
	}
	defer unlock()
	if err := revalidateInteractionSubmission(bundle, caller, requestID, now()); err != nil {
		abortInteractionSubmission(bundle, requestID, err)
		return zero, err
	}
	payload := env.Stdout
	if env.Terminal {
		payload = []byte(interactionTerminalJSON)
	}
	recorded := filepath.Join(root, "recorded", requestID)
	if err := exclWrite(recorded, payload); err != nil {
		_ = os.Remove(claim)
		return zero, fmt.Errorf("%w: recorded", errInteractionIO)
	}
	if env.Terminal {
		closed := filepath.Join(root, "closed", requestID)
		if err := exclWrite(closed, []byte("closed")); err != nil && !os.IsExist(err) {
			_ = os.Remove(claim)
			return zero, fmt.Errorf("%w: closed", errInteractionIO)
		}
	}
	if err := revalidateInteractionSubmission(bundle, caller, requestID, now()); err != nil {
		_ = os.Remove(recorded)
		if env.Terminal {
			_ = os.Remove(filepath.Join(root, "closed", requestID))
		}
		abortInteractionSubmission(bundle, requestID, err)
		return zero, err
	}
	ans := filepath.Join(root, "ans", requestID+".json")
	if err := exclWrite(ans, payload); err != nil {
		return zero, fmt.Errorf("%w: answer", errInteractionIO)
	}
	// Leave active.json for the waiting helper. Removing it here would let a
	// parallel PermissionRequest publish a second interaction before the
	// helper has consumed this answer.
	return claudeInteractionReceipt{
		Status: "accepted", Delivery: "recorded", RequestID: requestID, Phase: "recorded",
	}, nil
}

func interactionSubmissionExpired(bundle, id string, rec claudeInteractionRequest, now time.Time) bool {
	if interactionHelperExited(bundle, rec) {
		return true
	}
	if interactionFileExists(bundle, "expired", id) {
		return true
	}
	if !interactionDeadlineOpen(now, parseInteractionTime(rec.AnswerUntil)) {
		return true
	}
	return !interactionDeadlineOpen(now, parseInteractionTime(rec.HelperUntil))
}

func revalidateInteractionSubmission(bundle string, caller claudeInteractionCaller, id string, now time.Time) error {
	rec, err := readInteractionRequest(bundle, id)
	if err != nil {
		return err
	}
	if interactionSubmissionExpired(bundle, id, rec, now) {
		return fmt.Errorf("%w: expired", errInteractionConflict)
	}
	if err := matchInteractionIdentity(bundle, caller, rec); err != nil {
		return err
	}
	if rec.Turn != "" {
		live, ok := readClaudeAcceptedTurn(filepath.Join(bundle, "perm"))
		if !ok || live.Turn != rec.Turn || live.Gen != rec.TurnGen {
			return fmt.Errorf("%w: turn", errInteractionConflict)
		}
	}
	return nil
}

func abortInteractionSubmission(bundle, id string, reason error) {
	if root, err := interactionRoot(bundle); err == nil {
		_ = os.Remove(filepath.Join(root, "claim", id))
	}
	if reason == nil || !errors.Is(reason, errInteractionConflict) || !strings.Contains(reason.Error(), "expired") {
		return
	}
	_ = markInteractionExpired(bundle, id)
}

func markInteractionExpired(bundle, id string) error {
	path, err := interactionChild(bundle, "expired", id)
	if err != nil {
		return err
	}
	if err := exclWrite(path, []byte("expired")); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

func noteClaudeInteractionEmission(bundle, id string, ack claudeInteractionAck) error {
	if !safePathComponent(id) {
		return errInteractionBadRequest
	}
	path, err := interactionChild(bundle, "ack", id+".json")
	if err != nil {
		return err
	}
	if interactionFileExists(bundle, "ack", id+".json") {
		return fmt.Errorf("%w: ack", errInteractionConflict)
	}
	// claudeInteractionAck is bools and a string. json.Marshal cannot fail,
	// and a second response must not be invented to cover that branch.
	raw, _ := json.Marshal(ack)
	if err := exclWrite(path, raw); err != nil {
		return fmt.Errorf("%w: ack", errInteractionIO)
	}
	return nil
}

func claudeInteractionPhase(bundle, id string, now time.Time) string {
	if !safePathComponent(id) {
		return ""
	}
	if interactionFileExists(bundle, "interrupted", id) {
		return "interrupted"
	}
	if interactionFileExists(bundle, "expired", id) {
		return "expired"
	}
	if ack, ok := readInteractionAck(bundle, id); ok && ack.Emitted && ack.Terminal {
		return "handoff"
	}
	if interactionFileExists(bundle, "closed", id) {
		return "handoff"
	}
	if ack, ok := readInteractionAck(bundle, id); ok && ack.Emitted {
		return "emitted"
	}
	rec, err := readInteractionRequest(bundle, id)
	if err != nil {
		if _, ok := readInteractionAck(bundle, id); ok {
			return "unconfirmed"
		}
		return ""
	}
	if interactionHelperExited(bundle, rec) {
		return "expired"
	}
	// A dead helper must not keep recorded, claimed, or unconfirmed authority
	// after its deadline. Successful emission stays emitted; everything else
	// releases native controls.
	if !interactionDeadlineOpen(now, parseInteractionTime(rec.HelperUntil)) {
		return "expired"
	}
	if _, ok := readInteractionAck(bundle, id); ok {
		return "unconfirmed"
	}
	if interactionFileExists(bundle, "recorded", id) {
		return "recorded"
	}
	if interactionFileExists(bundle, "claim", id) {
		return "claimed"
	}
	if active, ok := readInteractionActive(bundle); ok && active == id {
		if !interactionDeadlineOpen(now, parseInteractionTime(rec.AnswerUntil)) {
			return "expired"
		}
		return "pending"
	}
	return ""
}

// recoverClaudeInteractionBundle releases authority the helper can no longer
// exercise. The worker that polls the bundle already owns its lifecycle;
// no second PID-based owner claim is needed after a restart.
func recoverClaudeInteractionBundle(bundle string, now time.Time) {
	if bundle == "" || !bundleSupportsInteraction(bundle) {
		return
	}
	root, err := interactionRoot(bundle)
	if err != nil {
		return
	}
	st, statErr := os.Stat(root)
	if statErr != nil || !st.IsDir() {
		return
	}
	_ = retireExpiredInteractionPayloads(bundle, now)
}

func retireExpiredInteractionPayloads(bundle string, now time.Time) error {
	reqDir := filepath.Join(bundle, "perm", "interact", "req")
	ents, err := os.ReadDir(reqDir)
	if err != nil {
		return fmt.Errorf("%w: scratch", errInteractionIO)
	}
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		releaseExpiredInteraction(bundle, strings.TrimSuffix(ent.Name(), ".json"), now)
	}
	return nil
}

func releaseExpiredInteraction(bundle, id string, now time.Time) {
	rec, err := readInteractionRequest(bundle, id)
	if err != nil {
		return
	}
	if interactionDeadlineOpen(now, parseInteractionTime(rec.HelperUntil)) && !interactionHelperExited(bundle, rec) {
		return
	}
	ack, ackOK := readInteractionAck(bundle, id)
	if !ackOK || !ack.Emitted {
		marker := []byte("expired")
		if _, aerr := readInteractionAns(bundle, id); aerr == nil || interactionFileExists(bundle, "recorded", id) {
			marker = []byte("unconfirmed")
		}
		if path, perr := interactionChild(bundle, "expired", id); perr == nil {
			_ = exclWrite(path, marker)
		}
	}
	// A cancellation still needs its turn identity after the raw payload is
	// gone. Keep that fence before deletion, and keep the payload if the
	// fence cannot be retained.
	if err := retainInteractionCancelFence(bundle, rec); err != nil {
		return
	}
	removeInteractionActive(bundle, id)
	_ = os.Remove(filepath.Join(bundle, "perm", "interact", "req", id+".json"))
	_ = os.Remove(filepath.Join(bundle, "perm", "interact", "ans", id+".json"))
	_ = os.Remove(filepath.Join(bundle, "perm", "interact", "recorded", id))
	_ = os.Remove(filepath.Join(bundle, "perm", "interact", "claim", id))
}

// interactionHelperExited probes the lock held from before publication through
// the helper's last stdout write. The kernel releases it on exit, including SIGKILL. Legacy
// requests without the marker retain their deadline-only contract.
func interactionHelperExited(bundle string, rec claudeInteractionRequest) bool {
	if !rec.HelperWaiter {
		return false
	}
	f, err := os.OpenFile(filepath.Join(bundle, "perm", "interact", "waiter", rec.ID), os.O_RDWR, 0)
	if err != nil {
		return true
	}
	defer f.Close()
	if err := interactionFlock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN)
	}
	// Close releases the acquired probe lock. Successful stdout is still only
	// emission evidence; keep it until its ordinary payload-retirement deadline.
	ack, ok := readInteractionAck(bundle, rec.ID)
	return !ok || !ack.Emitted
}

// claudeInteractionCancelFence is the only state settlement needs once the
// request and answer files have been retired. It is not a hook answer and
// must not be written to the helper's stdout.
type claudeInteractionCancelFence struct {
	Action  string `json:"action"`
	Turn    string `json:"turn"`
	TurnGen int    `json:"turn_gen"`
}

func retainInteractionCancelFence(bundle string, rec claudeInteractionRequest) error {
	if rec.Turn == "" || interactionFileExists(bundle, "interrupted", rec.ID) {
		return nil
	}
	// A bundle that cannot name the fence is an error. Treating it as "no
	// cancel" would let retirement delete the payload the fence was meant to
	// preserve. The answer is read only after that path is usable.
	dir, err := interactionChild(bundle, "cancel")
	if err != nil {
		return err
	}
	ans, err := readInteractionAns(bundle, rec.ID)
	if err != nil || !interactionAnsCancels(ans) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if interactionFileExists(bundle, "cancel", rec.ID) {
		return nil
	}
	path, err := interactionChild(bundle, "cancel", rec.ID)
	if err != nil {
		return err
	}
	return writeClaudePermFile(path, claudeInteractionCancelFence{
		Action: "cancel", Turn: rec.Turn, TurnGen: rec.TurnGen,
	})
}

func readInteractionCancelFence(bundle, id string) (claudeInteractionCancelFence, bool) {
	var zero claudeInteractionCancelFence
	path, err := interactionChild(bundle, "cancel", id)
	if err != nil {
		return zero, false
	}
	b, err := readBounded(path, interactionFileMax)
	if err != nil || json.Unmarshal(b, &zero) != nil {
		return zero, false
	}
	if zero.Action != "cancel" || zero.Turn == "" {
		return zero, false
	}
	return zero, true
}

func claudeInteractionCancelMaySettle(emitted bool, cancelTurn string, current, closing claudeAcceptedTurn) (bool, string) {
	if !emitted {
		return false, "not emitted"
	}
	if cancelTurn == "" {
		return false, "turn mismatch"
	}
	if current.Turn != "" && current.Turn != cancelTurn {
		return false, "successor turn"
	}
	if current.Turn == cancelTurn || (current.Turn == "" && closing.Turn == cancelTurn) {
		return true, "settled"
	}
	return false, "turn mismatch"
}

func (a *app) settleClaudeInteractionCancel(n *Node, requestID string) (bool, string) {
	if a == nil || n == nil || !safePathComponent(requestID) {
		return false, "turn mismatch"
	}
	a.mu.Lock()
	bundle := a.claudePermBundleLocked(n.ID)
	a.mu.Unlock()
	if bundle == "" {
		return false, "turn mismatch"
	}
	rec, err := readInteractionRequest(bundle, requestID)
	fence, fenceOK := readInteractionCancelFence(bundle, requestID)
	if err != nil {
		if !fenceOK {
			return false, "turn mismatch"
		}
		rec.Turn = fence.Turn
		rec.TurnGen = fence.TurnGen
	}
	ans, ansErr := readInteractionAns(bundle, requestID)
	ack, ackOK := readInteractionAck(bundle, requestID)
	fromAnswer := ansErr == nil && interactionAnsCancels(ans)
	emitted := ackOK && ack.Emitted && (fromAnswer || (ansErr != nil && fenceOK))
	// Decide and close under the same auto gate that beginClaudeAcceptedTurn's
	// callers hold. endClaudePermissionTurn locks that gate itself, so the
	// boundary steps run inline here instead of through a second lock.
	g := a.autoGateFor(n.ID)
	g.Lock()
	defer g.Unlock()
	a.mu.Lock()
	current := a.claudeTurns[n.ID]
	closing := a.claudeClosing[n.ID]
	a.mu.Unlock()
	ok, why := claudeInteractionCancelMaySettle(emitted, rec.Turn, current, closing)
	if !ok {
		return false, why
	}
	a.retractClaudeLease(a.claudePermBundle(n.ID))
	a.mu.Lock()
	delete(a.autoApprove, n.ID)
	a.mu.Unlock()
	a.resetClaudePermissionTurnLocked(n)
	if path, err := interactionChild(bundle, "interrupted", requestID); err == nil {
		_ = exclWrite(path, []byte("interrupted"))
	}
	return true, "settled"
}

func (a *app) settleEmittedClaudeInteractionCancels(n *Node) {
	a.mu.Lock()
	bundle := a.claudePermBundleLocked(n.ID)
	a.mu.Unlock()
	for _, id := range pendingCancelInteractions(bundle) {
		if ok, _ := a.settleClaudeInteractionCancel(n, id); ok {
			return
		}
	}
}

func pendingCancelInteractions(bundle string) []string {
	// Requests are short lived; cancellation fences index only still-relevant
	// retired cancels. Historical acknowledgements are replay fences, not an
	// inbox to rescan on every node poll.
	candidates := map[string]bool{}
	for _, kind := range []string{"req", "cancel"} {
		ents, err := os.ReadDir(filepath.Join(bundle, "perm", "interact", kind))
		if err != nil {
			continue
		}
		for _, ent := range ents {
			if ent.IsDir() {
				continue
			}
			id := ent.Name()
			if kind == "req" {
				if !strings.HasSuffix(id, ".json") {
					continue
				}
				id = strings.TrimSuffix(id, ".json")
			}
			candidates[id] = true
		}
	}
	var ids []string
	for id := range candidates {
		if interactionFileExists(bundle, "interrupted", id) {
			_ = os.Remove(filepath.Join(bundle, "perm", "interact", "cancel", id))
			continue
		}
		ack, ok := readInteractionAck(bundle, id)
		if !ok || !ack.Emitted {
			continue
		}
		ans, aerr := readInteractionAns(bundle, id)
		if aerr != nil || !interactionAnsCancels(ans) {
			if _, ok := readInteractionCancelFence(bundle, id); !ok {
				continue
			}
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func manualInteractionEvent(rec claudeInteractionRequest, env claudeInteractionEnvelope) sessionlog.Event {
	manual := &sessionlog.ManualInteraction{
		Action:          env.Audit.Action,
		Tool:            env.Audit.Tool,
		InputDigest:     env.Audit.InputDigest,
		Feedback:        env.Audit.Feedback,
		AppContext:      env.Audit.AppContext,
		Mode:            env.Audit.Mode,
		ModeLabel:       env.Audit.ModeLabel,
		EstablishedMode: env.Audit.EstablishedMode,
		Delivery:        "recorded",
		Turn:            rec.Turn,
		TurnGen:         rec.TurnGen,
		SessionID:       rec.SessionID,
		BundleID:        rec.BundleID,
		Generation:      rec.Generation,
	}
	if rec.Tool == "ExitPlanMode" {
		model, _ := classifyClaudeInteraction(rec.Tool, rec.Original)
		manual.Reviewed = model.Plan
	}
	for _, sel := range env.Audit.Selections {
		manual.Selections = append(manual.Selections, sessionlog.ManualSelection{
			QuestionID: sel.QuestionID,
			Question:   sel.Question,
			OptionIDs:  append([]string(nil), sel.OptionIDs...),
			Labels:     append([]string(nil), sel.Labels...),
			FreeText:   sel.FreeText,
			Kind:       sel.Kind,
		})
	}
	return sessionlog.NewDecision(sessionlog.DecisionEvent{
		Source:    "manual",
		RequestID: rec.ID,
		Agent:     "claude",
		Title:     rec.Tool,
		Selected:  interactionSelected(env.Audit.Action, env.Audit.ModeLabel),
		Manual:    manual,
	})
}

func interactionSelected(action, modeLabel string) sessionlog.DecOption {
	name := action
	switch action {
	case "answer":
		name = "Answer recorded"
	case "discuss":
		name = "Discuss"
	case "request_changes":
		name = "Request changes"
	case "approve":
		name = "Approval recorded: " + modeLabel
	case "cancel":
		name = "Cancellation recorded"
	case "terminal":
		name = "Terminal handoff"
	}
	return sessionlog.DecOption{Name: name, Kind: action}
}

func matchInteractionIdentity(bundle string, caller claudeInteractionCaller, rec claudeInteractionRequest) error {
	if caller.BundleID == "" || caller.BundleID != rec.BundleID || caller.BundleID != filepath.Base(bundle) {
		return fmt.Errorf("%w: bundle", errInteractionConflict)
	}
	if caller.SessionID == "" || caller.SessionID != rec.SessionID {
		return fmt.Errorf("%w: session", errInteractionConflict)
	}
	if rec.NodeID != "" && caller.NodeID != rec.NodeID {
		return fmt.Errorf("%w: node", errInteractionConflict)
	}
	if fence, ok := readClaudeBindingFence(bundle); ok {
		if caller.Generation != fence.Generation || rec.Generation != fence.Generation {
			return fmt.Errorf("%w: generation", errInteractionConflict)
		}
		if fence.NodeID != "" && caller.NodeID != fence.NodeID {
			return fmt.Errorf("%w: node", errInteractionConflict)
		}
		if fence.SessionID != "" && (caller.SessionID != fence.SessionID || rec.SessionID != fence.SessionID) {
			return fmt.Errorf("%w: session", errInteractionConflict)
		}
	} else if fileExists(filepath.Join(bundle, "perm", "generation.json")) {
		return fmt.Errorf("%w: generation", errInteractionConflict)
	} else if caller.Generation != rec.Generation {
		return fmt.Errorf("%w: generation", errInteractionConflict)
	}
	live, liveOK := readClaudeAcceptedTurn(filepath.Join(bundle, "perm"))
	if rec.Turn != "" || caller.Turn != "" || liveOK {
		if caller.Turn != rec.Turn || caller.TurnGen != rec.TurnGen {
			return fmt.Errorf("%w: turn", errInteractionConflict)
		}
		if liveOK && (live.Turn != rec.Turn || live.Gen != rec.TurnGen) {
			return fmt.Errorf("%w: turn", errInteractionConflict)
		}
	}
	return nil
}

func readClaudeBindingFence(bundle string) (claudeBindingFence, bool) {
	var zero claudeBindingFence
	b, err := readBounded(filepath.Join(bundle, "perm", "generation.json"), interactionFileMax)
	if err != nil {
		return zero, false
	}
	var doc claudeBindingFence
	if json.Unmarshal(b, &doc) != nil {
		return zero, false
	}
	return doc, true
}

func readInteractionGeneration(bundle string) (int, string, bool) {
	doc, ok := readClaudeBindingFence(bundle)
	if !ok {
		return 0, "", false
	}
	return doc.Generation, doc.NodeID, true
}

func readInteractionRequest(bundle, id string) (claudeInteractionRequest, error) {
	var zero claudeInteractionRequest
	path, err := interactionChild(bundle, "req", id+".json")
	if err != nil {
		return zero, err
	}
	b, err := readBounded(path, interactionFileMax)
	if err != nil {
		if os.IsNotExist(err) {
			return zero, fmt.Errorf("%w: request", errInteractionConflict)
		}
		return zero, fmt.Errorf("%w: request", errInteractionIO)
	}
	var rec claudeInteractionRequest
	if json.Unmarshal(b, &rec) != nil || rec.ID != id {
		return zero, fmt.Errorf("%w: request", errInteractionBadRequest)
	}
	return rec, nil
}

func readInteractionAns(bundle, id string) ([]byte, error) {
	path, err := interactionChild(bundle, "ans", id+".json")
	if err != nil {
		return nil, err
	}
	return readBounded(path, interactionFileMax)
}

func readInteractionAck(bundle, id string) (claudeInteractionAck, bool) {
	var zero claudeInteractionAck
	path, err := interactionChild(bundle, "ack", id+".json")
	if err != nil {
		return zero, false
	}
	b, err := readBounded(path, interactionFileMax)
	if err != nil || json.Unmarshal(b, &zero) != nil {
		return claudeInteractionAck{}, false
	}
	return zero, true
}

func readInteractionActive(bundle string) (string, bool) {
	b, err := readBounded(filepath.Join(bundle, "perm", "interact", "active.json"), interactionFileMax)
	if err != nil {
		return "", false
	}
	var doc struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &doc) != nil || !safePathComponent(doc.ID) {
		return "", false
	}
	return doc.ID, true
}

func interactionAnsTerminal(raw []byte) bool {
	var doc struct {
		Terminal           bool            `json:"terminal"`
		HookSpecificOutput json.RawMessage `json:"hookSpecificOutput"`
	}
	return json.Unmarshal(raw, &doc) == nil && doc.Terminal && len(bytes.TrimSpace(doc.HookSpecificOutput)) == 0
}

func interactionAnsCancels(raw []byte) bool {
	var doc struct {
		HookSpecificOutput struct {
			Decision struct {
				Behavior  string `json:"behavior"`
				Interrupt *bool  `json:"interrupt"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.HookSpecificOutput.Decision.Interrupt == nil {
		return false
	}
	return doc.HookSpecificOutput.Decision.Behavior == "deny" && *doc.HookSpecificOutput.Decision.Interrupt
}

func interactionFileExists(bundle, kind, name string) bool {
	path, err := interactionChild(bundle, kind, name)
	if err != nil {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func interactionRoot(bundle string) (string, error) {
	if bundle == "" || !filepath.IsAbs(bundle) || filepath.Clean(bundle) != bundle {
		return "", errInteractionBadRequest
	}
	return filepath.Join(bundle, "perm", "interact"), nil
}

func interactionChild(bundle string, parts ...string) (string, error) {
	root, err := interactionRoot(bundle)
	if err != nil {
		return "", err
	}
	p := root
	for _, part := range parts {
		if !safePathComponent(part) {
			return "", errInteractionBadRequest
		}
		p = filepath.Join(p, part)
	}
	return p, nil
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func parseInteractionTime(v string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

func readBounded(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > max {
		return nil, fmt.Errorf("%w: bounded read", errInteractionIO)
	}
	return b, nil
}

var (
	interactionFlock        = syscall.Flock
	interactionPublishNonce = newClaudeTurnNonce
	interactionPublishSync  = sessionlog.SyncParentDir
)

func exclWrite(path string, data []byte) error {
	return publishExclusive(path, data, openExclusivePublish)
}

func openExclusivePublish(path string) (interactionSyncFile, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

// publishExclusive makes path appear only after data is complete. The
// temporary file is validated and linked onto the final name; readers that
// race the writer do not observe an empty or partial file.
func publishExclusive(path string, data []byte, open func(string) (interactionSyncFile, error)) error {
	if len(data) == 0 {
		return fmt.Errorf("%w: empty publish", errInteractionIO)
	}
	if strings.HasSuffix(path, ".json") && !json.Valid(data) {
		return fmt.Errorf("%w: invalid json", errInteractionIO)
	}
	nonce, err := interactionPublishNonce()
	if err != nil {
		return err
	}
	if open == nil {
		return fmt.Errorf("%w: publish", errInteractionIO)
	}
	tmp := filepath.Join(filepath.Dir(path), ".pub-"+nonce)
	f, err := open(tmp)
	if err != nil {
		return err
	}
	if err := finishExcl(f, tmp, data); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	got, err := os.ReadFile(tmp)
	if err != nil || !bytes.Equal(got, data) {
		_ = os.Remove(tmp)
		if err == nil {
			err = fmt.Errorf("%w: publish reread", errInteractionIO)
		}
		return err
	}
	if err := os.Link(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(tmp)
	if err := interactionPublishSync(path); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func lockInteractionFence(bundle string) (func(), error) {
	root, err := interactionRoot(bundle)
	if err != nil {
		return nil, err
	}
	st, statErr := os.Stat(root)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return func() {}, nil
		}
		return nil, statErr
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%w: fence", errInteractionIO)
	}
	f, err := os.OpenFile(filepath.Join(root, "fence.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := interactionFlock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = interactionFlock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func finishExcl(f interactionSyncFile, path string, data []byte) error {
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return err
	}
	if err := sessionlog.SyncParentDir(path); err != nil {
		return err
	}
	return nil
}
