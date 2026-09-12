package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/sessionworker"
)

var errNoSessionWorker = errors.New("no live session worker for node")

type workerEntry struct {
	client  *sessionworker.Client
	process *sessionWorkerProcess
	// pid is retained for a worker reattached after an in-place muxer exec.
	// The replacement has no exec.Cmd, but is still the OS parent and must reap
	// the child when an explicit Stop makes it exit.
	pid        int
	identity   sessionworker.Identity
	observed   sessionworker.State
	observedAt time.Time
}

// workerManager implements procManager by routing each node to its own Unix
// process. The map is only a connection registry: all irreplaceable harness
// state lives behind the per-session endpoint.
type workerManager struct {
	mu           sync.Mutex
	entries      map[string]*workerEntry
	starting     map[string]bool
	executable   string
	dataDir      string
	build        string
	home         string
	socket       string
	startOptions sessionWorkerStartOptions
	nextID       func() string
}

func newWorkerManager(executable, dataDir, build string) *workerManager {
	return &workerManager{
		entries: map[string]*workerEntry{}, starting: map[string]bool{},
		executable: executable, dataDir: dataDir, build: build,
		nextID:       newWorkerInstanceID,
		startOptions: sessionWorkerStartOptions{stderr: os.Stderr},
	}
}

func newWorkerInstanceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func (m *workerManager) beginLaunch(nodeID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries[nodeID] != nil || m.starting[nodeID] {
		return false
	}
	m.starting[nodeID] = true
	return true
}

func (m *workerManager) endLaunch(nodeID string) {
	m.mu.Lock()
	delete(m.starting, nodeID)
	m.mu.Unlock()
}

func (m *workerManager) Launch(nodeID, agent, dir, model, effort string) (string, error) {
	return m.launch(sessionworker.LaunchRequest{NodeID: nodeID, Agent: agent, Dir: dir, Model: model, Effort: effort})
}

func (m *workerManager) LaunchNode(n *Node, model string) (string, error) {
	if n == nil {
		return "", errors.New("session worker: nil node launch")
	}
	return m.launch(sessionworker.LaunchRequest{
		NodeID: n.ID, Agent: n.Agent, Parent: n.Parent, Title: n.Title, Prompt: n.Prompt,
		Description: n.Description, Rationale: n.Rationale, LaneID: n.LaneID, ForkKind: n.ForkKind,
		Dir: n.Dir, Model: model, Effort: n.Effort, Transport: n.Transport, SessionID: n.SessionID,
		Transcript: n.Transcript, Adopted: n.Adopted, AXScreenReader: n.AXScreenReader, CreatedAt: n.CreatedAt,
	})
}

func (m *workerManager) AdoptClaude(n *Node, hookID string, generation int) (string, error) {
	if n == nil || n.Agent != "claude" {
		return "", errors.New("session worker: only Claude tmux nodes can be adopted")
	}
	return m.launch(sessionworker.LaunchRequest{
		NodeID: n.ID, Agent: n.Agent, Parent: n.Parent, Title: n.Title, Prompt: n.Prompt,
		Description: n.Description, Rationale: n.Rationale, LaneID: n.LaneID, ForkKind: n.ForkKind,
		Dir: n.Dir, Model: n.Model, Effort: n.Effort, Transport: n.Transport, SessionID: n.SessionID,
		Existing: true, Transcript: n.Transcript, Adopted: n.Adopted,
		AXScreenReader: n.AXScreenReader, CreatedAt: n.CreatedAt,
		HookID: hookID, HookGeneration: generation,
	})
}

func (m *workerManager) launch(request sessionworker.LaunchRequest) (string, error) {
	nodeID, agent := request.NodeID, request.Agent
	if !m.beginLaunch(nodeID) {
		return "", errNoSessionWorker
	}
	defer m.endLaunch(nodeID)
	identity := sessionworker.Identity{WorkerID: m.nextID(), Agent: agent, Build: m.build}
	config := sessionWorkerConfig{DataDir: m.dataDir, Home: m.home, Socket: m.socket, NodeID: nodeID, Identity: identity}
	ctx, cancel := context.WithTimeout(context.Background(), workerReadinessTimeout)
	defer cancel()
	process, err := startSessionWorker(ctx, m.executable, config, m.startOptions)
	if err != nil {
		return "", err
	}
	sid, err := process.client.Launch(ctx, request)
	if err != nil {
		if request.Existing {
			// A lost response must never turn migration into ownership: killing
			// the controller process leaves the pre-existing tmux pane untouched.
			_ = process.client.Close()
			if p, findErr := os.FindProcess(process.pid); findErr == nil {
				_ = p.Kill()
			}
		} else {
			_ = process.client.Stop(context.Background(), true)
		}
		_ = process.waitForExit(5 * time.Second)
		return "", err
	}
	m.mu.Lock()
	m.entries[nodeID] = &workerEntry{client: process.client, process: process, identity: identity}
	m.mu.Unlock()
	return sid, nil
}

func (m *workerManager) manages(nodeID string) bool { return m.entry(nodeID) != nil }

func (m *workerManager) entry(nodeID string) *workerEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entries[nodeID]
}

func (m *workerManager) invalidate(nodeID string) {
	m.mu.Lock()
	if entry := m.entries[nodeID]; entry != nil {
		entry.observedAt = time.Time{}
	}
	m.mu.Unlock()
}

func (m *workerManager) observe(nodeID string) sessionworker.State {
	m.mu.Lock()
	entry := m.entries[nodeID]
	if entry == nil {
		m.mu.Unlock()
		return sessionworker.State{Live: "exited"}
	}
	if !entry.observedAt.IsZero() && time.Since(entry.observedAt) < 10*time.Millisecond {
		state := entry.observed
		m.mu.Unlock()
		return state
	}
	client := entry.client
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, err := client.State(ctx)
	if err != nil {
		// A failed RPC proves only that this client cannot reach the worker.
		// Claiming the lifetime lock distinguishes a dead owner from a live but
		// temporarily unreachable one, and removes the locator atomically only
		// in the former case.
		reapStaleWorkerLocator(m.dataDir, nodeID)
		return sessionworker.State{Live: "exited"}
	}
	m.mu.Lock()
	if m.entries[nodeID] == entry {
		entry.observed, entry.observedAt = state, time.Now()
	}
	m.mu.Unlock()
	return state
}

func reapStaleWorkerLocator(dataDir, nodeID string) bool {
	registration, err := sessionworker.Claim(dataDir, nodeID)
	if err != nil {
		return false
	}
	return registration.Close() == nil
}

func (m *workerManager) withClient(nodeID string, fn func(context.Context, *sessionworker.Client) error) error {
	entry := m.entry(nodeID)
	if entry == nil {
		return errNoSessionWorker
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := fn(ctx, entry.client)
	m.invalidate(nodeID)
	return err
}

func (m *workerManager) Send(nodeID, text string) error {
	_, err := m.SendDelivery(nodeID, text)
	return err
}

func (m *workerManager) SendDelivery(nodeID, text string) (sessionworker.Delivery, error) {
	var delivery sessionworker.Delivery
	err := m.withClient(nodeID, func(ctx context.Context, client *sessionworker.Client) error {
		var err error
		delivery, err = client.Send(ctx, text)
		return err
	})
	return delivery, err
}

func (m *workerManager) Clear(nodeID string) error {
	_, err := m.ClearDelivery(nodeID)
	return err
}

func (m *workerManager) ClearDelivery(nodeID string) (sessionworker.Delivery, error) {
	var delivery sessionworker.Delivery
	err := m.withClient(nodeID, func(ctx context.Context, client *sessionworker.Client) error {
		var err error
		delivery, err = client.Clear(ctx)
		return err
	})
	return delivery, err
}

func (m *workerManager) ResolveDelivery(nodeID string) error {
	return m.withClient(nodeID, func(ctx context.Context, client *sessionworker.Client) error {
		return client.ResolveDelivery(ctx)
	})
}

func (m *workerManager) Interrupt(nodeID string) error {
	_, err := m.InterruptEvidence(nodeID)
	return err
}

func (m *workerManager) InterruptEvidence(nodeID string) (sessionworker.ActionEvidence, error) {
	var evidence sessionworker.ActionEvidence
	err := m.withClient(nodeID, func(ctx context.Context, client *sessionworker.Client) error {
		var err error
		evidence, err = client.Interrupt(ctx)
		return err
	})
	return evidence, err
}

func (m *workerManager) PrepareResolve(nodeID, expectedRequestID, key string) (string, string, error) {
	prepared, err := m.PreparePermission(nodeID, expectedRequestID, key)
	return prepared.Token, prepared.Evidence, err
}

func (m *workerManager) PreparePermission(nodeID, expectedRequestID, key string) (sessionworker.PreparedPermission, error) {
	var prepared sessionworker.PreparedPermission
	err := m.withClient(nodeID, func(ctx context.Context, client *sessionworker.Client) error {
		var err error
		prepared, err = client.PreparePermission(ctx, sessionworker.PermissionDecision{RequestID: expectedRequestID, Key: key})
		return err
	})
	return prepared, err
}

func (m *workerManager) Deliver(nodeID, token string) error {
	return m.withClient(nodeID, func(ctx context.Context, client *sessionworker.Client) error {
		return client.DeliverPermission(ctx, token)
	})
}

func (m *workerManager) Pending(nodeID string) (PendingPermission, bool) {
	pending := m.observe(nodeID).Permission
	if pending == nil {
		return PendingPermission{}, false
	}
	options := make([]PermOption, len(pending.Options))
	for i, option := range pending.Options {
		options[i] = PermOption{Key: option.Key, Name: option.Name, Kind: option.Kind}
	}
	return PendingPermission{RequestID: pending.RequestID, Title: pending.Title, ToolKind: pending.ToolKind, Reason: pending.Reason, Options: options}, true
}

func (m *workerManager) PermissionBoundary(nodeID string) (string, uint64, bool) {
	boundary := m.observe(nodeID).PermissionBoundary
	if boundary == nil {
		return "", 0, false
	}
	return boundary.Incarnation, boundary.MaxSequence, true
}

func (m *workerManager) Peek(nodeID string) string {
	return m.PeekMode(nodeID, "")
}

func (m *workerManager) PeekMode(nodeID, mode string) string {
	entry := m.entry(nodeID)
	if entry == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peek, err := entry.client.Peek(ctx, mode)
	if err != nil {
		return "(session worker unavailable)\n\n" + err.Error()
	}
	return peek
}

func (m *workerManager) SetAutoApprove(nodeID string, enabled bool) (sessionworker.AutoApprove, error) {
	var state sessionworker.AutoApprove
	err := m.withClient(nodeID, func(ctx context.Context, client *sessionworker.Client) error {
		var err error
		state, err = client.SetAutoApprove(ctx, enabled)
		return err
	})
	return state, err
}

func (m *workerManager) State(nodeID string) sessionworker.State { return m.observe(nodeID) }

func (m *workerManager) Live(nodeID string) string { return m.observe(nodeID).Live }

func (m *workerManager) Attention(nodeID string) string { return m.observe(nodeID).Attention }

func (m *workerManager) LastError(nodeID string) string { return m.observe(nodeID).LastError }

func (m *workerManager) HasSession(nodeID string) bool { return m.observe(nodeID).HasSession }

func (m *workerManager) SessionID(nodeID string) string { return m.observe(nodeID).SessionID }

func (m *workerManager) RecordStartFailure(nodeID string, cause error) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	return m.withClient(nodeID, func(ctx context.Context, client *sessionworker.Client) error {
		return client.RecordStartFailure(ctx, message)
	})
}

func (m *workerManager) AppendSessionEvent(nodeID string, event sessionlog.Event) error {
	return m.withClient(nodeID, func(ctx context.Context, client *sessionworker.Client) error {
		return client.AppendSessionEvent(ctx, event)
	})
}

func (m *workerManager) Kill(nodeID string) error {
	return m.stop(nodeID, true)
}

func (m *workerManager) stop(nodeID string, terminateSession bool) error {
	m.mu.Lock()
	entry := m.entries[nodeID]
	delete(m.entries, nodeID)
	m.mu.Unlock()
	if entry == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := entry.client.Stop(ctx, terminateSession)
	cancel()
	_ = entry.client.Close()
	if entry.process != nil {
		if waitErr := entry.process.waitForExit(5 * time.Second); err == nil {
			err = waitErr
		}
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, discoverErr := sessionworker.Discover(m.dataDir, nodeID)
		if errors.Is(discoverErr, os.ErrNotExist) {
			if reapErr := reapReattachedWorker(entry.pid, deadline); err == nil {
				err = reapErr
			}
			return err
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return errors.New("session worker: locator remained after stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// reapReattachedWorker matters only across syscall.Exec. The replacement
// muxer keeps the same PID and therefore remains the worker's parent, but the
// Go exec.Cmd waiter vanished with the old program image. After an ordinary
// crash/restart the worker belongs to init (or a subreaper), so ECHILD is the
// expected no-op result.
func reapReattachedWorker(pid int, deadline time.Time) error {
	if pid <= 0 {
		return nil
	}
	for {
		var status syscall.WaitStatus
		waited, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.ECHILD) || waited == pid {
			return nil
		}
		if err != nil {
			return fmt.Errorf("session worker: reap pid %d: %w", pid, err)
		}
		if time.Now().After(deadline) {
			return errors.New("session worker: did not exit after stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (m *workerManager) Shutdown() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.entries))
	for id := range m.entries {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		entry := m.entry(id)
		terminateSession := entry == nil || entry.identity.Agent != "claude"
		_ = m.stop(id, terminateSession)
	}
}

// Detach closes muxer-owned connections without sending Stop. It is the
// critical difference between a muxer handoff and an explicit user shutdown.
func (m *workerManager) Detach() {
	m.mu.Lock()
	entries := m.entries
	m.entries = map[string]*workerEntry{}
	m.mu.Unlock()
	for _, entry := range entries {
		_ = entry.client.Close()
	}
}

func (m *workerManager) Conflict(err error) bool {
	return errors.Is(err, errNoSessionWorker) || errors.Is(err, sessionworker.ErrConflict)
}

func connectWorker(locator sessionworker.Locator) (*sessionworker.Client, error) {
	client, err := sessionworker.NewClient(locator.Link)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	hello, err := client.Hello(ctx)
	cancel()
	if err == nil {
		err = sessionworker.CheckCompatibility(hello)
	}
	if err == nil && hello.Identity != locator.Identity {
		err = errors.New("worker hello does not match locator identity")
	}
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// Reconcile attaches only locators whose authenticated hello agrees with the
// durable node. Missing locators are the ordinary read-only-history state for
// sessions created before workers existed or workers that exited.
func (m *workerManager) Reconcile(nodes []*Node) error {
	var errs []error
	for _, node := range nodes {
		if node == nil || node.EndedAt != "" || (node.transport() != "acp" && node.transport() != "codex" && node.Agent != "claude") {
			continue
		}
		locator, err := sessionworker.Discover(m.dataDir, node.ID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", node.ID, err))
			continue
		}
		client, connectErr := connectWorker(locator)
		if connectErr != nil {
			if reapStaleWorkerLocator(m.dataDir, node.ID) {
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %w", node.ID, connectErr))
			continue
		}
		if locator.NodeID != node.ID || locator.Agent != node.Agent {
			if client != nil {
				_ = client.Close()
			}
			connectErr = errors.New("worker identity does not match durable node")
			errs = append(errs, fmt.Errorf("%s: %w", node.ID, connectErr))
			continue
		}
		m.mu.Lock()
		if m.entries[node.ID] == nil {
			m.entries[node.ID] = &workerEntry{client: client, pid: locator.PID, identity: locator.Identity}
			client = nil
		}
		m.mu.Unlock()
		if client != nil {
			_ = client.Close()
		}
	}
	return errors.Join(errs...)
}

// RecoverUnknown finishes the two publication transactions that a killed
// muxer can interrupt. A worker with no node record is recovered from its own
// authenticated launch description. A worker whose latest durable record is
// delete is stopped, completing that deletion instead of resurrecting it.
func (m *workerManager) RecoverUnknown(a *app) error {
	if a == nil {
		return nil
	}
	locators, listErr := sessionworker.List(m.dataDir)
	errs := []error{listErr}
	for _, locator := range locators {
		if m.manages(locator.NodeID) {
			continue
		}
		a.mu.Lock()
		known := a.byID[locator.NodeID] != nil
		deleted := a.deletedNodes[locator.NodeID]
		a.mu.Unlock()
		if known {
			continue
		}
		client, err := connectWorker(locator)
		if err != nil {
			if reapStaleWorkerLocator(m.dataDir, locator.NodeID) {
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %w", locator.NodeID, err))
			continue
		}
		m.mu.Lock()
		m.entries[locator.NodeID] = &workerEntry{client: client, pid: locator.PID, identity: locator.Identity}
		m.mu.Unlock()
		if deleted {
			if err := m.stop(locator.NodeID, true); err != nil {
				errs = append(errs, fmt.Errorf("finish deleted worker %s: %w", locator.NodeID, err))
			}
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		state, stateErr := client.State(ctx)
		cancel()
		req := state.Launch
		if stateErr != nil || req == nil || !state.HasSession || req.NodeID != locator.NodeID || req.Agent != locator.Agent || req.Title == "" || req.Dir == "" || req.CreatedAt == "" {
			m.mu.Lock()
			delete(m.entries, locator.NodeID)
			m.mu.Unlock()
			_ = client.Close()
			if stateErr == nil {
				stateErr = errors.New("worker has no complete recovery description")
			}
			errs = append(errs, fmt.Errorf("%s: %w", locator.NodeID, stateErr))
			continue
		}
		node := &Node{
			ID: req.NodeID, Parent: req.Parent, Title: req.Title, Prompt: req.Prompt,
			Description: req.Description, Rationale: req.Rationale, LaneID: req.LaneID, ForkKind: req.ForkKind,
			Agent: req.Agent, Model: req.Model, Effort: req.Effort, Dir: req.Dir,
			SessionID: state.SessionID, Transcript: state.Transcript, Adopted: req.Adopted,
			AXScreenReader: state.AXScreenReader, Transport: req.Transport, CreatedAt: req.CreatedAt,
		}
		if node.Description == "" {
			node.Description = node.Prompt
		}
		if node.Transport == "" {
			switch node.Agent {
			case "pi", "opencode", "grok":
				node.Transport = "acp"
			case "codex":
				node.Transport = "codex"
			default:
				node.Transport = "tmux"
			}
		}
		if err := a.appendRecord(storeRecord{Type: "node", Node: node}); err != nil {
			m.mu.Lock()
			delete(m.entries, locator.NodeID)
			m.mu.Unlock()
			_ = client.Close()
			errs = append(errs, fmt.Errorf("recover worker %s: %w", locator.NodeID, err))
			continue
		}
		a.mu.Lock()
		a.nodes = append(a.nodes, node)
		a.byID[node.ID] = node
		a.deletedNodes[node.ID] = false
		if node.Agent == "claude" && safePathComponent(state.HookID) {
			a.claudeHooks[node.ID] = state.HookID
			a.claudeGens[node.ID] = state.HookGeneration
		}
		a.mu.Unlock()
		if node.Agent == "claude" && safePathComponent(state.HookID) {
			if err := a.appendRecord(storeRecord{Type: "claude-hook", ID: node.ID, HookID: state.HookID, Generation: state.HookGeneration}); err != nil {
				errs = append(errs, fmt.Errorf("recover worker hook %s: %w", locator.NodeID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// AdoptExistingClaude gives pre-worker tmux chats a controller endpoint
// without restarting their pane. Failure is per chat and non-destructive: the
// muxer simply keeps using the legacy in-process path for that node.
func (m *workerManager) AdoptExistingClaude(a *app) error {
	if a == nil || a.server == nil {
		return nil
	}
	type candidate struct {
		node       Node
		hookID     string
		generation int
	}
	a.mu.Lock()
	candidates := make([]candidate, 0)
	for _, node := range a.nodes {
		if node.Agent != "claude" || node.EndedAt != "" || m.manages(node.ID) {
			continue
		}
		candidates = append(candidates, candidate{node: *node, hookID: a.claudeHookIDLocked(node.ID), generation: a.claudeGenerationLocked(node.ID)})
	}
	a.mu.Unlock()
	var errs []error
	for _, candidate := range candidates {
		if !a.server.Session(candidate.node.ID).Alive() {
			continue
		}
		if _, err := m.AdoptClaude(&candidate.node, candidate.hookID, candidate.generation); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", candidate.node.ID, err))
		}
	}
	return errors.Join(errs...)
}
