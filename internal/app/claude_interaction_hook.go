package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/transcript"
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
	filepath.Join("perm", "interact", "confirmed"),
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

func bundleSupportsInteraction(bundle string) bool {
	caps, ok := readClaudeHookCapabilities(bundle)
	if !ok || caps.Interaction < 1 || !claudeBundleExecUsable(caps) {
		return false
	}
	return true
}

func addPermissionRequestTimeout(raw []byte, timeout int) []byte {
	out := append([]byte(nil), raw...)
	if timeout <= 0 {
		return out
	}
	needle := []byte(`"PermissionRequest":[{"hooks":[{"command":`)
	i := bytes.Index(out, needle)
	if i < 0 {
		return out
	}
	marker := []byte(`"type":"command"}`)
	j := bytes.Index(out[i:], marker)
	if j < 0 {
		return out
	}
	end := i + j + len(marker) - 1
	extra := []byte(`,"timeout":` + fmt.Sprintf("%d", timeout))
	rewritten := make([]byte, 0, len(out)+len(extra))
	rewritten = append(rewritten, out[:end]...)
	rewritten = append(rewritten, extra...)
	rewritten = append(rewritten, out[end:]...)
	return rewritten
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
		interactionFileExists(dir, "closed", id) ||
		interactionFileExists(dir, "confirmed", id)
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

func submitClaudeInteractionClock(bundle string, caller claudeInteractionCaller, requestID string, reply claudeInteractionReply, now func() time.Time, audit func(sessionlog.Event) error) (claudeInteractionReceipt, error) {
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

func noteClaudeInteractionObservation(bundle, id, observed string) (string, error) {
	rec, err := readInteractionRequest(bundle, id)
	if err != nil {
		return "", err
	}
	model, _ := classifyClaudeInteraction(rec.Tool, rec.Original)
	if model.Tool != "ExitPlanMode" || model.Native {
		return interactionDeliveryUnconfirmed, nil
	}
	word := confirmClaudePlanDelivery(model.Plan, observed)
	if word != interactionDeliveryConfirmed {
		return interactionDeliveryUnconfirmed, nil
	}
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "confirmed", id), []byte("confirmed")); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("%w: confirmed", errInteractionIO)
	}
	return interactionDeliveryConfirmed, nil
}

func claudeInteractionPhase(bundle, id string, now time.Time) string {
	if !safePathComponent(id) {
		return ""
	}
	if interactionFileExists(bundle, "interrupted", id) {
		return "interrupted"
	}
	if interactionFileExists(bundle, "confirmed", id) {
		return "confirmed"
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

func claimClaudeInteractionOwner(bundle, instance string) error {
	if !safePathComponent(instance) {
		return errInteractionBadRequest
	}
	path := filepath.Join(bundle, "perm", "interact", "owner")
	if b, err := os.ReadFile(path); err == nil {
		if strings.TrimSpace(string(b)) == instance {
			return nil
		}
		return fmt.Errorf("%w: owner", errInteractionConflict)
	}
	if err := exclWrite(path, []byte(instance)); err != nil {
		return fmt.Errorf("%w: owner", errInteractionIO)
	}
	return nil
}

func retireForeignClaudeInteraction(bundle, instance string, now time.Time) (string, error) {
	_ = now
	path := filepath.Join(bundle, "perm", "interact", "owner")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "absent", nil
		}
		return "", fmt.Errorf("%w: owner", errInteractionIO)
	}
	if strings.TrimSpace(string(b)) == instance {
		return "kept", nil
	}
	active, ok := readInteractionActive(bundle)
	if ok {
		if ans, aerr := readInteractionAns(bundle, active); aerr == nil && len(ans) > 0 {
			return "foreign-answered", nil
		}
		closed, cerr := interactionChild(bundle, "closed", active)
		if cerr == nil {
			_ = exclWrite(closed, []byte("closed"))
		}
		ansPath, aerr := interactionChild(bundle, "ans", active+".json")
		if aerr == nil {
			_ = exclWrite(ansPath, []byte(interactionTerminalJSON))
		}
	}
	return "released", nil
}

func retireOwnClaudeInteractionScratch(bundle, instance string, now time.Time) error {
	owner := filepath.Join(bundle, "perm", "interact", "owner")
	b, err := os.ReadFile(owner)
	if err != nil || strings.TrimSpace(string(b)) != instance {
		return fmt.Errorf("%w: owner", errInteractionConflict)
	}
	return retireExpiredInteractionPayloads(bundle, now)
}

// recoverClaudeInteractionBundle releases authority the helper can no longer
// exercise. A foreign owner file does not skip the sweep: the worker that
// polls the bundle is the lifecycle owner after a restart.
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
	_ = claimClaudeInteractionOwner(bundle, claudeInteractionOwnerID())
	_ = retireExpiredInteractionPayloads(bundle, now)
}

func claudeInteractionOwnerID() string {
	return fmt.Sprintf("w-%d", os.Getpid())
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
	if interactionDeadlineOpen(now, parseInteractionTime(rec.HelperUntil)) {
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

func claudeInteractionCancelMaySettle(emitted, endTurn bool, cancelTurn string, current, closing claudeAcceptedTurn) (bool, string) {
	if !emitted {
		return false, "not emitted"
	}
	if !endTurn {
		return false, "no interruption evidence"
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

func (a *app) settleClaudeInteractionCancel(n *Node, requestID string, endTurn bool) (bool, string) {
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
	ok, why := claudeInteractionCancelMaySettle(emitted, endTurn, rec.Turn, current, closing)
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

func (a *app) noteClaudeInteractionInterrupt(n *Node, tl *transcript.Tailer) {
	if a == nil || n == nil || tl == nil || !tl.EndTurn() {
		return
	}
	a.mu.Lock()
	bundle := a.claudePermBundleLocked(n.ID)
	a.mu.Unlock()
	if bundle == "" {
		return
	}
	for _, id := range pendingCancelInteractions(bundle) {
		rec, err := readInteractionRequest(bundle, id)
		if err != nil {
			fence, ok := readInteractionCancelFence(bundle, id)
			if !ok {
				continue
			}
			rec.Turn = fence.Turn
		}
		a.mu.Lock()
		current := a.claudeTurns[n.ID]
		closing := a.claudeClosing[n.ID]
		a.mu.Unlock()
		if !interactionCancelTargetsTurn(rec.Turn, current, closing) {
			continue
		}
		target := current
		if current.Turn == "" {
			target = closing
		}
		if !interactionInterruptIsFresh(tl, target, claudeInterruptWatermarkOf(a, n.ID)) {
			continue
		}
		ok, _ := a.settleClaudeInteractionCancel(n, id, true)
		if ok {
			return
		}
	}
}

// claudeInterruptWatermark is the transcript size already on disk when the
// accepted turn began. An undated interruption is new only when this tailer
// has consumed bytes past that size and those earlier bytes do not already
// end on the same interruption notice. The mark is absent when the turn has
// no transcript path. A missing timestamp is not freshness by itself.
type claudeInterruptWatermark struct {
	path   string
	offset int64
}

type claudeInterruptWatermarkKey struct {
	app    *app
	nodeID string
}

var claudeInterruptWatermarks sync.Map

func rememberClaudeInterruptWatermark(a *app, n *Node) {
	if a == nil || n == nil {
		return
	}
	key := claudeInterruptWatermarkKey{app: a, nodeID: n.ID}
	if n.Transcript == "" {
		claudeInterruptWatermarks.Delete(key)
		return
	}
	st, err := os.Stat(n.Transcript)
	if err != nil {
		claudeInterruptWatermarks.Delete(key)
		return
	}
	claudeInterruptWatermarks.Store(key, claudeInterruptWatermark{path: n.Transcript, offset: st.Size()})
}

func claudeInterruptWatermarkOf(a *app, nodeID string) claudeInterruptWatermark {
	v, ok := claudeInterruptWatermarks.Load(claudeInterruptWatermarkKey{app: a, nodeID: nodeID})
	if !ok {
		return claudeInterruptWatermark{}
	}
	mark, _ := v.(claudeInterruptWatermark)
	return mark
}

// interactionInterruptIsFresh requires the interruption itself to be new for
// this accepted turn. The boundary is the tailer's last visible turn, and only
// when that turn is an interruption notice. An assistant end_turn, an
// unparseable tail, a missing turn stamp, or a notice dated before the turn
// was accepted belongs to an earlier turn. Another record's timestamp does
// not refresh it. An undated notice is evidence only when the file can be
// read, was modified after the turn was accepted, this tailer has consumed
// that file, and the notice was not already the terminal boundary in the
// bytes present when the turn began. A failed or lagged read keeps the
// boundary the tailer already had.
func interactionInterruptIsFresh(tl *transcript.Tailer, turn claudeAcceptedTurn, mark claudeInterruptWatermark) bool {
	if tl == nil {
		return false
	}
	// Poll can run concurrently in prompt delivery. Its exported Turns slice
	// cannot be inspected safely, and separate status methods are not a
	// coherent snapshot. Reuse the parser on a private copy of the committed
	// prefix, then refuse an incomplete copy or a concurrently advanced tail.
	offset, _ := tl.Progress()
	snapshot, err := interruptPrefixTailer(tl.Path, offset)
	if err != nil {
		return false
	}
	copied, _ := snapshot.Progress()
	current, _ := tl.Progress()
	if copied != offset || current != offset {
		return false
	}
	snapshot.Path = tl.Path
	tl = snapshot
	if !tl.EndTurn() || tl.Delivered() || tl.Unparseable() {
		return false
	}
	at := parseInteractionTime(turn.At)
	if at.IsZero() {
		return false
	}
	boundary, ok := interruptBoundaryTurn(tl)
	if !ok {
		return false
	}
	if stamp := parseInteractionTime(boundary.Time); !stamp.IsZero() {
		return !stamp.Before(at)
	}
	return undatedInterruptNewlyObserved(tl, at, mark)
}

func interruptNoticeText(text string) bool {
	switch strings.TrimSpace(text) {
	case "[Request interrupted by user]", "[Request interrupted by user for tool use]":
		return true
	default:
		return false
	}
}

func interruptBoundaryTurn(tl *transcript.Tailer) (transcript.Turn, bool) {
	// Callers inspect only their private prefix tailer, never a shared tailer.
	if tl == nil || len(tl.Turns) == 0 {
		return transcript.Turn{}, false
	}
	last := tl.Turns[len(tl.Turns)-1]
	if !interruptNoticeText(last.Text) {
		return transcript.Turn{}, false
	}
	return last, true
}

func undatedInterruptNewlyObserved(tl *transcript.Tailer, at time.Time, mark claudeInterruptWatermark) bool {
	if tl.Path == "" {
		return false
	}
	f, err := os.Open(tl.Path)
	if err != nil {
		return false
	}
	st, err := f.Stat()
	_ = f.Close()
	if err != nil || !st.ModTime().After(at) {
		return false
	}
	off, _ := tl.Progress()
	if off != st.Size() {
		return false
	}
	if mark.path == tl.Path && off <= mark.offset {
		return false
	}
	if mark.path == tl.Path && interruptBoundaryAlreadyPresent(tl.Path, mark.offset, tl) {
		return false
	}
	return true
}

// interruptPrefixTempDir is empty in production. A test points it at a file
// so the prefix copy cannot be created and freshness fails closed.
var interruptPrefixTempDir string

// writeInterruptPrefix copies the bytes that were already on disk when the
// turn began. A test replaces it to force that copy to fail.
var writeInterruptPrefix = func(f *os.File, b []byte) error {
	_, err := f.Write(b)
	return err
}

// interruptBoundaryAlreadyPresent reports whether the live interruption is
// the same terminal notice already contained in path's prefix. A prefix that
// cannot be read fails closed: the caller must not treat the boundary as new.
func interruptBoundaryAlreadyPresent(path string, offset int64, live *transcript.Tailer) bool {
	if offset <= 0 {
		return false
	}
	earlier, err := interruptPrefixTailer(path, offset)
	if err != nil {
		return true
	}
	if earlier.Unparseable() {
		return true
	}
	if !earlier.EndTurn() || earlier.Delivered() {
		return false
	}
	before, beforeOK := interruptBoundaryTurn(earlier)
	if !beforeOK {
		return false
	}
	after, afterOK := interruptBoundaryTurn(live)
	if !afterOK {
		return true
	}
	return len(earlier.Turns) == len(live.Turns) && before.Text == after.Text && before.Time == after.Time
}

// interruptPrefixTailer reuses the transcript tailer on a copy of the bytes
// that were already present. It does not parse the file a second way.
func interruptPrefixTailer(path string, offset int64) (*transcript.Tailer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, offset)
	n, _ := f.Read(buf)
	if n == 0 {
		return nil, errors.New("interrupt prefix unreadable")
	}
	buf = buf[:n]
	if len(bytes.TrimSpace(buf)) == 0 {
		return &transcript.Tailer{}, nil
	}
	tmp, err := os.CreateTemp(interruptPrefixTempDir, "interrupt-prefix-*.jsonl")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	werr := writeInterruptPrefix(tmp, buf)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, werr
	}
	tl := &transcript.Tailer{Path: tmpPath}
	_ = tl.Poll()
	return tl, nil
}

func interactionCancelTargetsTurn(cancelTurn string, current, closing claudeAcceptedTurn) bool {
	if cancelTurn == "" {
		return false
	}
	if current.Turn != "" {
		return cancelTurn == current.Turn
	}
	return closing.Turn != "" && cancelTurn == closing.Turn
}

func pendingCancelInteractions(bundle string) []string {
	dir := filepath.Join(bundle, "perm", "interact", "ack")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var ids []string
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(ent.Name(), ".json")
		if interactionFileExists(bundle, "interrupted", id) {
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
		name = "Answered"
	case "discuss":
		name = "Discuss"
	case "request_changes":
		name = "Request changes"
	case "approve":
		name = modeLabel
	case "cancel":
		name = "Interrupted"
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
