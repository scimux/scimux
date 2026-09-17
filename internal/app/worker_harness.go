package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp"
	"codeberg.org/chrberger/scimux/internal/acp/codex"
	"codeberg.org/chrberger/scimux/internal/acp/muse"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/sessionworker"
)

// oneSessionHarness narrows the existing multi-node manager interface to the
// one node owned by a session-worker process. Harness policy and protocol state
// machines remain in their existing managers; only reconnect-stable lifecycle
// observations, such as the bounded completion latch, live at this boundary.
type oneSessionHarness struct {
	nodeID      string
	manager     procManager
	stateMu     sync.Mutex
	afterLaunch func(sessionworker.LaunchRequest, string)
	beforeState func()
	logw        *sessionlog.Writer
	launch      *sessionworker.LaunchRequest
	lastLive    string
	lastChg     time.Time
	turnDone    bool
}

func newOneSessionHarness(nodeID string, manager procManager) *oneSessionHarness {
	return &oneSessionHarness{nodeID: nodeID, manager: manager}
}

// newProductionSessionHarness constructs exactly one existing structured
// manager behind the per-chat boundary. It intentionally contains no new
// harness policy: ACP, Codex, and Muse keep their already-tested state
// machines and session-log writers; the worker only changes their lifetime
// and address.
func newProductionSessionHarness(config sessionWorkerConfig) (sessionworker.Harness, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if config.Identity.Agent == "claude" {
		return newClaudeSessionHarness(config)
	}
	sessionsDir := filepath.Join(config.DataDir, "sessions")
	assetOwner := &app{
		sessionsDir: sessionsDir,
		assetsDir:   filepath.Join(config.DataDir, "assets"),
	}
	var manager procManager
	switch config.Identity.Agent {
	case "pi", "opencode", "grok", "dsh":
		m := acpManager{acp.NewManager(sessionsDir)}
		m.SetAssetHook(assetOwner.ingestAssetHook)
		manager = m
		assetOwner.acp = m
	case "codex":
		m := codexManager{codex.NewManager(sessionsDir)}
		m.SetAssetHook(assetOwner.ingestAssetHook)
		manager = m
	case "muse":
		m := museManager{muse.NewManager(sessionsDir)}
		m.SetAssetHook(assetOwner.ingestAssetHook)
		manager = m
	default:
		return nil, fmt.Errorf("session worker: unsupported agent %q", config.Identity.Agent)
	}
	harness := newOneSessionHarness(config.NodeID, manager)
	harness.logw = &sessionlog.Writer{Path: filepath.Join(sessionsDir, config.NodeID+".jsonl")}
	if config.Identity.Agent == "pi" {
		assetOwner.home, _ = os.UserHomeDir()
		node := &Node{ID: config.NodeID, Agent: "pi", Transport: "acp"}
		harness.afterLaunch = func(req sessionworker.LaunchRequest, sessionID string) {
			node.SessionID, node.Dir, node.Model, node.Effort = sessionID, req.Dir, req.Model, req.Effort
		}
		harness.beforeState = func() { assetOwner.syncPiFare(node) }
	}
	return harness, nil
}

func (h *oneSessionHarness) Launch(_ context.Context, req sessionworker.LaunchRequest) (string, error) {
	if req.NodeID != h.nodeID {
		return "", errors.New("session worker launch is for another node")
	}
	sid, err := h.manager.Launch(h.nodeID, req.Agent, req.Dir, req.Model, req.Effort)
	if err == nil {
		h.stateMu.Lock()
		launch := req
		h.launch = &launch
		h.lastLive = h.manager.Live(h.nodeID)
		h.lastChg = time.Time{}
		h.turnDone = false
		h.stateMu.Unlock()
		if h.afterLaunch != nil {
			h.afterLaunch(req, sid)
		}
	}
	return sid, err
}

func (h *oneSessionHarness) Send(_ context.Context, text string) (sessionworker.Delivery, error) {
	err := h.manager.Send(h.nodeID, text)
	if err == nil {
		// Send acceptance is the mechanical start edge. Record it here even if
		// a very fast harness is quiet again before the next State request.
		h.stateMu.Lock()
		h.lastLive = "active"
		h.lastChg = time.Now()
		h.turnDone = false
		h.stateMu.Unlock()
	}
	return sessionworker.Delivery{Status: sessionworker.DeliveryAcknowledged}, err
}

func (h *oneSessionHarness) Clear(context.Context) (sessionworker.Delivery, error) {
	err := h.manager.Clear(h.nodeID)
	if err == nil {
		h.stateMu.Lock()
		h.lastLive = h.manager.Live(h.nodeID)
		h.lastChg = time.Time{}
		h.turnDone = false
		h.stateMu.Unlock()
	}
	return sessionworker.Delivery{Status: sessionworker.DeliveryAcknowledged}, err
}

func (h *oneSessionHarness) ResolveDelivery(context.Context) error { return nil }

func (h *oneSessionHarness) Interrupt(context.Context) (sessionworker.ActionEvidence, error) {
	return sessionworker.ActionEvidence{}, h.manager.Interrupt(h.nodeID)
}

func (h *oneSessionHarness) PreparePermission(_ context.Context, decision sessionworker.PermissionDecision) (sessionworker.PreparedPermission, error) {
	token, evidence, err := h.manager.PrepareResolve(h.nodeID, decision.RequestID, decision.Key)
	return sessionworker.PreparedPermission{Token: token, Evidence: evidence}, err
}

func (h *oneSessionHarness) DeliverPermission(_ context.Context, token string) error {
	return h.manager.Deliver(h.nodeID, token)
}

func (h *oneSessionHarness) State(context.Context) sessionworker.State {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	if h.beforeState != nil {
		h.beforeState()
	}
	state := sessionworker.State{
		Launch:     h.launch,
		HasSession: h.manager.HasSession(h.nodeID),
		Live:       h.manager.Live(h.nodeID),
		Attention:  h.manager.Attention(h.nodeID),
		LastError:  h.manager.LastError(h.nodeID),
	}
	now := time.Now()
	state.TurnDone = structuredTurnDone(state.Live, state.Attention, h.lastLive, state.LastError, "", h.turnDone, h.lastChg, now)
	if state.Live == "active" {
		h.lastChg = now
	}
	h.lastLive, h.turnDone = state.Live, state.TurnDone
	if provider, ok := h.manager.(interface{ SessionID(string) string }); ok {
		state.SessionID = provider.SessionID(h.nodeID)
	}
	if pending, ok := h.manager.Pending(h.nodeID); ok {
		options := make([]sessionworker.PermissionOption, len(pending.Options))
		for i, option := range pending.Options {
			options[i] = sessionworker.PermissionOption{Key: option.Key, Name: option.Name, Kind: option.Kind}
		}
		state.Permission = &sessionworker.PendingPermission{
			RequestID: pending.RequestID, Title: pending.Title, ToolKind: pending.ToolKind,
			Reason: pending.Reason, Options: options,
		}
	}
	if incarnation, maxSequence, ok := h.manager.PermissionBoundary(h.nodeID); ok {
		state.PermissionBoundary = &sessionworker.PermissionBoundary{Incarnation: incarnation, MaxSequence: maxSequence}
	}
	if reporter, ok := h.manager.(museFingerprintReporter); ok {
		state.MuseSchemaMismatch = reporter.FingerprintMismatch(h.nodeID)
	}
	return state
}

func (h *oneSessionHarness) Peek(context.Context, string) string {
	return h.manager.Peek(h.nodeID)
}

// SetAutoApprove reports unsupported because structured auto-approval remains
// muxer policy: its incarnation/sequence
// fence is already transport-independent. Claude uses this operation because
// its hook rendezvous must be mutated by the process that owns that harness.
func (h *oneSessionHarness) SetAutoApprove(context.Context, bool) (sessionworker.AutoApprove, error) {
	return sessionworker.AutoApprove{Supported: false, Phase: "off"}, nil
}

func (h *oneSessionHarness) RecordStartFailure(_ context.Context, message string) error {
	return h.manager.RecordStartFailure(h.nodeID, errors.New(message))
}

func (h *oneSessionHarness) AppendSessionEvent(_ context.Context, event sessionlog.Event) error {
	if h.logw == nil {
		return errors.New("session worker: session log writer unavailable")
	}
	return h.logw.Append(event)
}

func (h *oneSessionHarness) Stop(context.Context, bool) error {
	return h.manager.Kill(h.nodeID)
}

func (h *oneSessionHarness) Conflict(err error) bool { return h.manager.Conflict(err) }
