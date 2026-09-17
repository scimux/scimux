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
	stopMu  sync.Mutex
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
	// catalog reads the installed-harness catalog, which cursor launches need
	// to turn a (model, effort) pair back into one model id. It is a field so
	// the suite can supply a fixture: probing it for real would run whichever
	// agent CLIs happen to be on the host.
	catalog func() map[string]agentInfo
}

func newWorkerManager(executable, dataDir, build string) *workerManager {
	return &workerManager{
		entries: map[string]*workerEntry{}, starting: map[string]bool{},
		executable: executable, dataDir: dataDir, build: build,
		nextID:       newWorkerInstanceID,
		catalog:      detectAgents,
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

func (m *workerManager) AttachOwnedClaudePane(n *Node, hookID string, generation int) (string, error) {
	if n == nil || n.Agent != "claude" || n.Adopted {
		return "", errors.New("session worker: only scimux-owned Claude panes can be attached")
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
	if err := m.resolveLaunchModel(&request); err != nil {
		return "", err
	}
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

// resolveLaunchModel rewrites a cursor launch into the one field cursor reads.
// Cursor has no effort flag: the level is part of the model id, so the pair the
// node stores is collapsed here and the effort is cleared rather than sent on
// as a second, unvalidated spelling of the same choice. Every other agent
// carries both fields through untouched.
func (m *workerManager) resolveLaunchModel(request *sessionworker.LaunchRequest) error {
	if request.Agent != "cursor" || m.catalog == nil {
		return nil
	}
	model, err := resolveCursorModel(m.catalog()["cursor"], request.Model, request.Effort)
	if err != nil {
		return err
	}
	request.Model, request.Effort = model, ""
	return nil
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
	return m.observeState(nodeID, true)
}

func (m *workerManager) observeFresh(nodeID string) sessionworker.State {
	return m.observeState(nodeID, false)
}

func (m *workerManager) observeState(nodeID string, cached bool) sessionworker.State {
	m.mu.Lock()
	entry := m.entries[nodeID]
	if entry == nil {
		m.mu.Unlock()
		return sessionworker.State{Live: "exited"}
	}
	if cached && !entry.observedAt.IsZero() && time.Since(entry.observedAt) < 10*time.Millisecond {
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
		if !reapStaleWorkerLocator(m.dataDir, nodeID) {
			return sessionworker.State{Live: "unavailable"}
		}
		if err := m.reapWorkerEntry(nodeID, entry, time.Now().Add(5*time.Second)); err != nil {
			m.invalidate(nodeID)
		}
		return sessionworker.State{Live: "exited"}
	}
	m.mu.Lock()
	if m.entries[nodeID] == entry {
		entry.observed, entry.observedAt = state, time.Now()
	}
	m.mu.Unlock()
	return state
}

// staleWorkerLockGrace bounds how long a refused claim is allowed to mean
// "the kernel has not caught up yet" instead of "a live owner holds this
// node". Linux publishes a killed process as a zombie before running the
// deferred final __fput that drops its flocks, so for about a jiffy after a
// worker is provably dead its lifetime lock still answers as held. The grace
// is orders of magnitude above that window and is spent only after an RPC has
// already failed, so the healthy path never pays it.
const staleWorkerLockGrace = 250 * time.Millisecond

func reapStaleWorkerLocator(dataDir, nodeID string) bool {
	deadline := time.Now().Add(staleWorkerLockGrace)
	for {
		registration, err := sessionworker.Claim(dataDir, nodeID)
		if err == nil {
			return registration.Close() == nil
		}
		// Only a refused claim can be the kernel lagging behind a dead owner.
		// Every other error describes the data directory or the node id and
		// will not improve by waiting.
		if !errors.Is(err, sessionworker.ErrWorkerOwned) || !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
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

func (m *workerManager) FingerprintMismatch(nodeID string) bool {
	return m.observe(nodeID).MuseSchemaMismatch
}

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

// RetireController stops a session worker while deliberately leaving its
// underlying pane alive. It is used for historical external integrations and
// whole-program Claude shutdown, never as ownership proof for a pane.
func (m *workerManager) RetireController(nodeID string) error {
	return m.stop(nodeID, false)
}

func (m *workerManager) stop(nodeID string, terminateSession bool) error {
	entry := m.entry(nodeID)
	if entry == nil {
		return nil
	}
	entry.stopMu.Lock()
	defer entry.stopMu.Unlock()
	if m.entry(nodeID) != entry {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := entry.client.Stop(ctx, terminateSession)
	cancel()
	if err != nil {
		// A timed-out Stop is not permission to forget ownership. If the
		// lifetime lock is still held, keep the entry so deletion can be retried
		// after a suspended or unreachable worker recovers.
		if !reapStaleWorkerLocator(m.dataDir, nodeID) {
			m.invalidate(nodeID)
			return err
		}
	} else if entry.process == nil {
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, discoverErr := sessionworker.Discover(m.dataDir, nodeID)
			if errors.Is(discoverErr, os.ErrNotExist) {
				break
			}
			if time.Now().After(deadline) {
				m.invalidate(nodeID)
				return errors.New("session worker: locator remained after stop")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := m.reapWorkerEntry(nodeID, entry, time.Now().Add(5*time.Second)); err != nil {
		m.invalidate(nodeID)
		return err
	}
	return nil
}

func (m *workerManager) reapWorkerEntry(nodeID string, entry *workerEntry, deadline time.Time) error {
	if entry.process != nil {
		select {
		case <-entry.process.wait:
		case <-time.After(time.Until(deadline)):
			return errors.New("session worker: did not exit after stop")
		}
	} else if err := reapReattachedWorker(entry.pid, deadline); err != nil {
		return err
	}
	m.mu.Lock()
	if m.entries[nodeID] == entry {
		delete(m.entries, nodeID)
	}
	m.mu.Unlock()
	_ = entry.client.Close()
	return nil
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

// RetireExternalController proves that a historical external node has no live
// session worker, or authenticates and stops that worker with Stop(false).
// The manager's connection map is only a cache: absence there says nothing
// about a worker that survived a muxer restart or a failed reconciliation.
func (m *workerManager) RetireExternalController(node *Node) error {
	if m == nil || node == nil || !node.Adopted {
		return errors.New("session worker: external retirement requires an adopted node")
	}
	if entry := m.entry(node.ID); entry != nil {
		if entry.identity.Agent != "" && entry.identity.Agent != node.Agent {
			return errors.New("session worker: worker identity does not match durable external node")
		}
		return m.RetireController(node.ID)
	}

	locator, discoverErr := sessionworker.Discover(m.dataDir, node.ID)
	if discoverErr != nil {
		// Claiming the lifetime lock is the only proof that no controller remains.
		// It also safely removes a stale regular locator. A held lock, unreadable
		// locator, or unusable control directory remains fail-closed.
		if reapStaleWorkerLocator(m.dataDir, node.ID) {
			return nil
		}
		return fmt.Errorf("session worker: cannot prove external controller absent: %w", discoverErr)
	}
	client, connectErr := connectWorker(locator)
	if connectErr != nil {
		if reapStaleWorkerLocator(m.dataDir, node.ID) {
			return nil
		}
		return fmt.Errorf("session worker: connect external controller: %w", connectErr)
	}
	if locator.NodeID != node.ID || locator.Agent != node.Agent {
		_ = client.Close()
		return errors.New("session worker: worker identity does not match durable external node")
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
	return m.RetireController(node.ID)
}

// Reconcile attaches only locators whose authenticated hello agrees with the
// durable node. Missing locators are the ordinary read-only-history state for
// sessions created before workers existed or workers that exited.
func (m *workerManager) Reconcile(nodes []*Node) error {
	var errs []error
	for _, node := range nodes {
		if node == nil || node.EndedAt != "" || (node.transport() != "acp" && node.transport() != "codex" && node.transport() != "muse" && node.Agent != "claude") {
			continue
		}
		if node.Adopted {
			if err := m.RetireExternalController(node); err != nil {
				errs = append(errs, fmt.Errorf("retire external worker %s: %w", node.ID, err))
			}
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
		knownNode := a.byID[locator.NodeID]
		known := knownNode != nil
		ended := known && knownNode.EndedAt != ""
		adopted := known && knownNode.Adopted
		knownAgent := ""
		if known {
			knownAgent = knownNode.Agent
		}
		deleted := a.deletedNodes[locator.NodeID]
		a.mu.Unlock()
		if known && !ended && !adopted {
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
		if ended && locator.Agent != knownAgent {
			_ = client.Close()
			errs = append(errs, fmt.Errorf("%s: worker identity does not match ended durable node", locator.NodeID))
			continue
		}
		m.mu.Lock()
		m.entries[locator.NodeID] = &workerEntry{client: client, pid: locator.PID, identity: locator.Identity}
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		state, stateErr := client.State(ctx)
		cancel()
		req := state.Launch
		external := adopted || (stateErr == nil && req != nil && req.Adopted)
		if external {
			if err := m.RetireController(locator.NodeID); err != nil {
				errs = append(errs, fmt.Errorf("retire external worker %s: %w", locator.NodeID, err))
			}
			continue
		}
		if deleted || ended {
			if stateErr != nil && deleted && locator.Agent == "claude" {
				m.mu.Lock()
				delete(m.entries, locator.NodeID)
				m.mu.Unlock()
				_ = client.Close()
				errs = append(errs, fmt.Errorf("finish durably stopped worker %s: cannot prove whether its pane is external: %w", locator.NodeID, stateErr))
				continue
			}
			if err := m.stop(locator.NodeID, true); err != nil {
				errs = append(errs, fmt.Errorf("finish durably stopped worker %s: %w", locator.NodeID, err))
			}
			continue
		}
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
			case "pi", "opencode", "grok", "cursor":
				node.Transport = "acp"
			case "codex":
				node.Transport = "codex"
			case "muse":
				node.Transport = "muse"
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

// RecoverOwnedClaudePanes gives scimux-owned pre-worker tmux chats a
// controller endpoint without restarting their pane. Historical external
// integrations are intentionally excluded.
func (m *workerManager) RecoverOwnedClaudePanes(a *app) error {
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
		if node.Agent != "claude" || node.Adopted || node.EndedAt != "" || m.manages(node.ID) {
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
		if _, err := m.AttachOwnedClaudePane(&candidate.node, candidate.hookID, candidate.generation); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", candidate.node.ID, err))
		}
	}
	return errors.Join(errs...)
}
