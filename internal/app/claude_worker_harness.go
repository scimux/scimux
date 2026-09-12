package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/sessionworker"
	"codeberg.org/chrberger/scimux/internal/tmuxsession"
)

var errClaudeWorkerConflict = errors.New("claude session worker: state conflict")

type preparedClaudeAnswer struct {
	dialogID string
	keys     []string
}

// claudeSessionHarness puts the existing one-node Claude controller behind
// the same semantic operations as the structured harnesses. The controller's
// journal is private to this worker; the shared session log remains the sole
// conversation read path used by the muxer.
type claudeSessionHarness struct {
	nodeID   string
	app      *app
	stateDir string

	pollMu   sync.Mutex
	answerMu sync.Mutex
	answers  map[string]preparedClaudeAnswer
	start    sync.Once
	stop     sync.Once
	stopCh   chan struct{}
	done     chan struct{}
}

func newClaudeSessionHarness(config sessionWorkerConfig) (*claudeSessionHarness, error) {
	if config.Home == "" || config.Socket == "" || !safePathComponent(config.NodeID) {
		return nil, errors.New("claude session worker: incomplete home, socket, or node identity")
	}
	stateDir := filepath.Join(config.DataDir, "control", "worker-state", config.NodeID)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("claude session worker: create state directory: %w", err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("claude session worker: protect state directory: %w", err)
	}
	controller, err := newApp(Config{
		Home: config.Home, DataDir: config.DataDir, Socket: config.Socket,
	}, appDeps{
		Server:          tmuxsession.NewServer(config.Socket),
		StorePath:       filepath.Join(stateDir, "harness.jsonl"),
		ClaudeHooksDir:  filepath.Join(config.DataDir, "claude-hooks"),
		SkipHookCleanup: true,
	})
	if err != nil {
		return nil, err
	}
	h := &claudeSessionHarness{
		nodeID: config.NodeID, app: controller, stateDir: stateDir,
		answers: map[string]preparedClaudeAnswer{}, stopCh: make(chan struct{}), done: make(chan struct{}),
	}
	h.startLanes()
	return h, nil
}

func (h *claudeSessionHarness) startLanes() {
	h.start.Do(func() {
		go func() {
			defer close(h.done)
			poll := time.NewTicker(2 * time.Second)
			permission := time.NewTicker(claudePermLaneEvery)
			defer poll.Stop()
			defer permission.Stop()
			for {
				select {
				case <-poll.C:
					h.poll()
				case <-permission.C:
					h.app.resolveClaudePermissions()
				case <-h.stopCh:
					return
				}
			}
		}()
	})
}

func (h *claudeSessionHarness) poll() {
	h.pollMu.Lock()
	h.app.poll()
	h.pollMu.Unlock()
}

func (h *claudeSessionHarness) node() *Node {
	h.app.mu.Lock()
	defer h.app.mu.Unlock()
	return h.app.byID[h.nodeID]
}

func (h *claudeSessionHarness) Launch(_ context.Context, req sessionworker.LaunchRequest) (string, error) {
	if req.NodeID != h.nodeID || req.Agent != "claude" || req.Dir == "" || req.Title == "" || (!req.Existing && req.Prompt == "") {
		return "", errors.New("claude session worker: invalid launch request")
	}
	h.app.mu.Lock()
	if existing := h.app.byID[h.nodeID]; existing != nil {
		sid := existing.SessionID
		h.app.mu.Unlock()
		h.startLanes()
		return sid, nil
	}
	h.app.mu.Unlock()
	sid := req.SessionID
	if sid == "" {
		var err error
		sid, err = newUUID()
		if err != nil {
			return "", err
		}
	}
	n := &Node{
		ID: h.nodeID, Parent: req.Parent, Title: req.Title, Prompt: req.Prompt,
		Description: req.Description, Rationale: req.Rationale, LaneID: req.LaneID, ForkKind: req.ForkKind,
		Agent: "claude", Model: req.Model, Effort: req.Effort, Dir: req.Dir,
		SessionID: sid, Transport: req.Transport, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if n.Description == "" {
		n.Description = n.Prompt
	}
	if n.Transport == "" {
		n.Transport = "tmux"
	}
	if req.CreatedAt != "" {
		n.CreatedAt = req.CreatedAt
	}
	n.Transcript, n.Adopted, n.AXScreenReader = req.Transcript, req.Adopted, req.AXScreenReader
	if req.Existing {
		if !h.app.server.Session(n.ID).Alive() {
			return "", fmt.Errorf("%w: existing tmux session is not alive", errClaudeWorkerConflict)
		}
		if err := h.app.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
			return "", err
		}
		h.app.mu.Lock()
		h.app.nodes = append(h.app.nodes, n)
		h.app.byID[n.ID] = n
		h.app.mu.Unlock()
		if safePathComponent(req.HookID) {
			gen := req.HookGeneration
			if gen < 1 {
				gen = 1
			}
			if err := h.app.appendRecord(storeRecord{Type: "claude-hook", ID: n.ID, HookID: req.HookID, Generation: gen}); err != nil {
				return "", err
			}
			h.app.mu.Lock()
			h.app.claudeHooks[n.ID], h.app.claudeGens[n.ID] = req.HookID, gen
			if n.Transcript != "" {
				h.app.claudeAck[n.ID] = true
			}
			h.app.mu.Unlock()
			h.app.noteClaudeHookCapabilitiesForNode(n.ID)
		}
		h.startLanes()
		return sid, nil
	}
	if status, err := h.app.launchNode(n, nil); err != nil {
		return "", fmt.Errorf("claude session worker: launch (%d): %w", status, err)
	}
	h.app.mu.Lock()
	h.app.nodes = append(h.app.nodes, n)
	h.app.byID[n.ID] = n
	h.app.mu.Unlock()
	if hookID := h.app.takePendingClaudeHook(n.ID); hookID != "" {
		if err := h.app.appendRecord(storeRecord{Type: "claude-hook", ID: n.ID, HookID: hookID, Generation: 1}); err != nil {
			_ = h.app.server.Session(n.ID).Kill()
			h.app.archiveHookBundle(hookID)
			return "", err
		}
		h.app.mu.Lock()
		h.app.claudeHooks[n.ID], h.app.claudeGens[n.ID] = hookID, 1
		h.app.sendState[n.ID] = sendSubmitting
		h.app.mu.Unlock()
	}
	h.startLanes()
	go h.app.runClaudeInitialDelivery(n)
	return sid, nil
}

func (h *claudeSessionHarness) Send(_ context.Context, text string) (sessionworker.Delivery, error) {
	n := h.node()
	if n == nil {
		return sessionworker.Delivery{}, errClaudeWorkerConflict
	}
	status, delivery, err := h.app.sendTmuxPrompt(n, text, false)
	if err != nil {
		if status == 409 {
			return sessionworker.Delivery{}, fmt.Errorf("%w: %v", errClaudeWorkerConflict, err)
		}
		return sessionworker.Delivery{}, err
	}
	return sessionworker.Delivery{Status: string(delivery), Text: deliveryText(delivery, text)}, nil
}

func (h *claudeSessionHarness) Clear(context.Context) (sessionworker.Delivery, error) {
	n := h.node()
	if n == nil {
		return sessionworker.Delivery{}, errClaudeWorkerConflict
	}
	status, delivery, err := h.app.sendTmuxPrompt(n, "/clear", true)
	if err != nil {
		if status == 409 {
			return sessionworker.Delivery{}, fmt.Errorf("%w: %v", errClaudeWorkerConflict, err)
		}
		return sessionworker.Delivery{}, err
	}
	return sessionworker.Delivery{Status: string(delivery), Text: deliveryText(delivery, "/clear")}, nil
}

func deliveryText(delivery initialDelivery, text string) string {
	if delivery == initialUnconfirmed {
		return text
	}
	return ""
}

func (h *claudeSessionHarness) ResolveDelivery(context.Context) error {
	h.app.mu.Lock()
	defer h.app.mu.Unlock()
	if h.app.sendState[h.nodeID] == sendSubmitting {
		return errClaudeWorkerConflict
	}
	delete(h.app.sendState, h.nodeID)
	return nil
}

func (h *claudeSessionHarness) Interrupt(context.Context) (sessionworker.ActionEvidence, error) {
	n := h.node()
	if n == nil {
		return sessionworker.ActionEvidence{}, errClaudeWorkerConflict
	}
	s := h.app.server.Session(n.ID)
	cap, err := s.Capture()
	if err != nil {
		return sessionworker.ActionEvidence{}, fmt.Errorf("refusing interrupt without pane evidence (capture failed): %w", err)
	}
	if err := s.SendKey("Escape"); err != nil {
		return sessionworker.ActionEvidence{}, err
	}
	h.app.mu.Lock()
	if h.app.sendState[n.ID] != sendSubmitting {
		delete(h.app.sendState, n.ID)
	}
	h.app.mu.Unlock()
	h.app.endClaudePermissionTurn(n)
	return sessionworker.ActionEvidence{Evidence: "interrupt: " + lastLines(cap, 12), Keys: []string{"Escape"}}, nil
}

func (h *claudeSessionHarness) PreparePermission(_ context.Context, decision sessionworker.PermissionDecision) (sessionworker.PreparedPermission, error) {
	n := h.node()
	if n == nil {
		return sessionworker.PreparedPermission{}, errClaudeWorkerConflict
	}
	dlg := h.app.claudeVisibleDialog(n)
	if decision.RequestID == "" || decision.RequestID != dlg.DialogID {
		return sessionworker.PreparedPermission{}, errClaudeWorkerConflict
	}
	options := h.app.claudeDialogOptions(n, dlg)
	allowed := false
	for _, option := range options {
		if option.Key == decision.Key {
			allowed = true
			break
		}
	}
	if !allowed {
		return sessionworker.PreparedPermission{}, errors.New("key is not an option on this dialog")
	}
	cap, err := h.app.server.Session(n.ID).Capture()
	if err != nil {
		return sessionworker.PreparedPermission{}, fmt.Errorf("refusing keypress without pane evidence (capture failed): %w", err)
	}
	keys := tmuxKeySequence(n, decision.Key)
	token := newWorkerInstanceID()
	h.answerMu.Lock()
	h.answers[token] = preparedClaudeAnswer{dialogID: dlg.DialogID, keys: keys}
	h.answerMu.Unlock()
	return sessionworker.PreparedPermission{Token: token, Evidence: lastLines(cap, 12), Keys: keys}, nil
}

func (h *claudeSessionHarness) DeliverPermission(_ context.Context, token string) error {
	h.answerMu.Lock()
	prepared, ok := h.answers[token]
	delete(h.answers, token)
	h.answerMu.Unlock()
	if !ok {
		return errClaudeWorkerConflict
	}
	n := h.node()
	if n == nil || h.app.claudeVisibleDialog(n).DialogID != prepared.dialogID {
		return errClaudeWorkerConflict
	}
	if err := h.app.server.Session(n.ID).SendKeys(prepared.keys...); err != nil {
		return err
	}
	h.app.retireClaudeVisibleDialog(n, prepared.dialogID)
	h.app.mu.Lock()
	prev := h.app.attn[n.ID]
	h.app.attn[n.ID] = ""
	delete(h.app.attnAt, n.ID)
	h.app.mu.Unlock()
	h.app.persistAttentionTransition(n, prev, "")
	return nil
}

func (h *claudeSessionHarness) State(context.Context) sessionworker.State {
	h.poll()
	h.app.mu.Lock()
	n := h.app.byID[h.nodeID]
	if n == nil {
		h.app.mu.Unlock()
		return sessionworker.State{Live: "exited"}
	}
	copyNode := *n
	launch := sessionworker.LaunchRequest{
		NodeID: n.ID, Agent: n.Agent, Parent: n.Parent, Title: n.Title, Prompt: n.Prompt,
		Description: n.Description, Rationale: n.Rationale, LaneID: n.LaneID, ForkKind: n.ForkKind,
		Dir: n.Dir, Model: n.Model, Effort: n.Effort, Transport: n.Transport, SessionID: n.SessionID,
		Transcript: n.Transcript, Adopted: n.Adopted, AXScreenReader: n.AXScreenReader, CreatedAt: n.CreatedAt,
		HookID: h.app.claudeHookIDLocked(n.ID), HookGeneration: h.app.claudeGenerationLocked(n.ID),
	}
	state := sessionworker.State{
		Launch: &launch, SessionID: n.SessionID, Live: h.app.live[n.ID], Attention: h.app.attn[n.ID], LastError: h.app.claudeLaunchErr[n.ID],
		Transcript: n.Transcript, AXScreenReader: n.AXScreenReader, TurnDone: h.app.turnDone[n.ID],
		Delivery: h.app.sendState[n.ID], Supervision: string(h.app.claudeSupervisionOf(n)),
		HookID: h.app.claudeHookIDLocked(n.ID), HookGeneration: h.app.claudeGenerationLocked(n.ID),
	}
	aa := h.app.autoApproveViewOf(n)
	state.AutoApprove = sessionworker.AutoApprove{Supported: aa.Supported, Enabled: aa.Enabled, Phase: aa.Phase, Count: aa.Count, Error: aa.Error}
	h.app.mu.Unlock()
	// tmux is an external process. Never hold the application state lock while
	// waiting on it; a stalled socket must not block hooks or permission state.
	state.HasSession = h.app.server.Session(copyNode.ID).Alive()

	// Reuse the characterized Claude projection itself. The worker protocol is
	// typed, but its values are obtained from the same function the monolith
	// uses, avoiding a second implementation of delivery/attention semantics.
	overlay := map[string]any{}
	h.app.tmuxChatInto(overlay, &copyNode, h.app.segment(&copyNode))
	state.Live, _ = overlay["live"].(string)
	state.Attention, _ = overlay["attention"].(string)
	state.Delivery, _ = overlay["delivery"].(string)
	state.Supervision, _ = overlay["supervision"].(string)
	state.Pending, _ = overlay["pending"].(bool)
	state.Fallback, _ = overlay["fallback"].(bool)
	state.Source, _ = overlay["source"].(string)
	state.Reason, _ = overlay["reason"].(string)
	state.Watermark, _ = overlay["watermark"].(int64)
	state.Progress, _ = overlay["progress"].(int)
	state.PendingCalls, _ = overlay["pending_calls"].(int)
	state.WaitingOn, _ = overlay["waiting_on"].(string)
	state.ReplyReady, _ = overlay["reply_ready"].(bool)
	state.TurnInFlight, _ = overlay["turn_in_flight"].(bool)
	if message, ok := overlay["error"].(string); ok {
		state.LastError = message
	}
	if dialogID, _ := overlay["perm_dialog_id"].(string); dialogID != "" {
		opts, _ := overlay["perm_options"].([]PermOption)
		converted := make([]sessionworker.PermissionOption, len(opts))
		for i, option := range opts {
			converted[i] = sessionworker.PermissionOption{Key: option.Key, Name: option.Name, Kind: option.Kind}
		}
		state.Permission = &sessionworker.PendingPermission{
			RequestID: dialogID, Title: stringValue(overlay["perm_title"]), ToolKind: stringValue(overlay["perm_tool_kind"]),
			Reason: stringValue(overlay["perm_reason"]), Options: converted, Dialog: true, Manual: boolValue(overlay["perm_manual"]),
		}
	}
	state.Compacting, _ = overlay["compacting"].(bool)
	state.CompactTrigger, _ = overlay["compact_trigger"].(string)
	state.ElicitationCount, _ = overlay["elicitation_count"].(int)
	views, _ := overlay["elicitations"].([]claudeElicitationView)
	for _, view := range views {
		state.Elicitations = append(state.Elicitations, sessionworker.Elicitation{Server: view.Server, Message: view.Message, Mode: view.Mode, URL: view.URL})
	}
	return state
}

func stringValue(value any) string {
	out, _ := value.(string)
	return out
}

func boolValue(value any) bool {
	out, _ := value.(bool)
	return out
}

func (h *claudeSessionHarness) Peek(_ context.Context, mode string) string {
	n := h.node()
	if n == nil {
		return "(session exited or unavailable)"
	}
	var out string
	var err error
	if mode == "visible" {
		out, err = h.app.server.Session(n.ID).CaptureVisible()
	} else {
		out, err = h.app.server.Session(n.ID).Capture()
	}
	if err != nil {
		return "(session exited or unavailable)\n\n" + err.Error()
	}
	return out
}

func (h *claudeSessionHarness) SetAutoApprove(_ context.Context, enabled bool) (sessionworker.AutoApprove, error) {
	h.app.mu.Lock()
	n := h.app.byID[h.nodeID]
	live := h.app.live[h.nodeID]
	h.app.mu.Unlock()
	if n == nil {
		return sessionworker.AutoApprove{}, errClaudeWorkerConflict
	}
	view := h.app.setAutoApproveEnabled(h.nodeID, enabled, live, "", 0)
	return sessionworker.AutoApprove{Supported: view.Supported, Enabled: view.Enabled, Phase: view.Phase, Count: view.Count, Error: view.Error}, nil
}

func (h *claudeSessionHarness) RecordStartFailure(_ context.Context, message string) error {
	if message == "" {
		return nil
	}
	return h.AppendSessionEvent(context.Background(), sessionlog.Event{T: "error", Error: message})
}

func (h *claudeSessionHarness) AppendSessionEvent(_ context.Context, event sessionlog.Event) error {
	return (&sessionlog.Writer{Path: filepath.Join(h.app.sessionsDir, h.nodeID+".jsonl")}).Append(event)
}

func (h *claudeSessionHarness) Stop(_ context.Context, terminateSession bool) error {
	n := h.node()
	var stopErr error
	h.stop.Do(func() {
		close(h.stopCh)
		<-h.done
		if n == nil {
			return
		}
		if !terminateSession {
			return
		}
		stopErr = h.app.closeOwned(n)
		if hookID := h.app.claudeHookID(n.ID); hookID != "" {
			h.app.archiveHookBundle(hookID)
		}
		if err := h.app.appendRecord(storeRecord{Type: "delete", ID: n.ID, Time: time.Now().UTC().Format(time.RFC3339)}); stopErr == nil {
			stopErr = err
		}
	})
	return stopErr
}

func (h *claudeSessionHarness) Conflict(err error) bool {
	return errors.Is(err, errClaudeWorkerConflict) || errors.Is(err, errClaudeTurnInFlight)
}
