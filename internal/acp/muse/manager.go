package muse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/agentperm"
	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// SpawnFunc launches one Muse transport for a node. Tests inject in-process
// pipes; production uses defaultSpawn (`muse serve` from PATH).
type SpawnFunc func(nodeID, dir string) (Transport, error)

// logAppender is the session-log write seam. Production uses sessionlog.Writer.
type logAppender interface {
	Append(Event) error
}

// Manager owns one Muse process, one MSP client, and one current MSP session
// per node. It writes the unified session log and presents the structured
// manager surface used by ACP and Codex. It must not import internal/app.
type Manager struct {
	mu        sync.Mutex
	sessions  map[string]*nodeSession
	launching map[string]struct{}
	spawn     SpawnFunc
	logDir    string
	closing   bool
	assetHook asset.IngestFunc
	opCtx     context.Context
	opCancel  context.CancelFunc
	opTimeout time.Duration
	wg        sync.WaitGroup
}

var (
	ErrNotAlive        = errors.New("muse subprocess has exited")
	ErrTurnActive      = errors.New("a turn is already in flight")
	ErrNoPending       = errors.New("no permission request is pending")
	ErrStalePermission = errors.New("pending permission request has changed")
	ErrNoTurn          = errors.New("no turn is in flight")
	ErrLaunchConflict  = errors.New("muse: launch already in progress or session exists")
)

const defaultOpTimeout = 30 * time.Second

const defaultApprovalMode = "promptUnmatched"

func NewManager(logDir string) *Manager {
	return NewManagerWithSpawn(logDir, defaultSpawn)
}

func NewManagerWithSpawn(logDir string, spawn SpawnFunc) *Manager {
	if spawn == nil {
		spawn = defaultSpawn
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		sessions:  map[string]*nodeSession{},
		launching: map[string]struct{}{},
		spawn:     spawn,
		logDir:    logDir,
		opCtx:     ctx,
		opCancel:  cancel,
	}
}

func defaultSpawn(nodeID, dir string) (Transport, error) {
	// Working directory is MSP workspaceRoot, never process cwd.
	_ = nodeID
	_ = dir
	return Spawn(context.Background(), "muse")
}

func (m *Manager) timeout() time.Duration {
	if m.opTimeout > 0 {
		return m.opTimeout
	}
	return defaultOpTimeout
}

func (m *Manager) SetAssetHook(f asset.IngestFunc) {
	m.mu.Lock()
	m.assetHook = f
	m.mu.Unlock()
}

func (m *Manager) logPath(nodeID string) string {
	return filepath.Join(m.logDir, nodeID+".jsonl")
}

func (m *Manager) session(nodeID string) *nodeSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[nodeID]
}

func (m *Manager) Launch(nodeID, agent, dir, model, effort string) (string, error) {
	hook, err := m.reserveLaunch(nodeID)
	if err != nil {
		return "", err
	}
	defer m.finishLaunch(nodeID)

	if err := os.MkdirAll(m.logDir, 0o700); err != nil {
		return "", fmt.Errorf("muse session log dir: %w", err)
	}
	tr, err := m.spawn(nodeID, dir)
	if err != nil {
		return "", fmt.Errorf("muse spawn: %w", err)
	}

	logPath := m.logPath(nodeID)
	_, statErr := os.Stat(logPath)
	s := &nodeSession{
		nodeID:    nodeID,
		tr:        tr,
		logw:      &sessionlog.Writer{Path: logPath},
		logPath:   logPath,
		dir:       dir,
		model:     model,
		effort:    effort,
		assetHook: hook,
		incarn:    newSessionIncarn(),
		procAlive: true,
		opCtx:     m.opCtx,
		opTimeout: m.timeout(),
		done:      make(chan struct{}),
	}
	discardLog := func() {
		if os.IsNotExist(statErr) {
			_ = os.Remove(logPath)
		}
	}
	fail := func(err error) (string, error) {
		cerr := s.stop()
		discardLog()
		return "", wrapCleanup(err, cerr)
	}

	if err := s.logw.Append(sessionlog.NewMeta(nodeID, agent, model, effort, dir)); err != nil {
		return fail(fmt.Errorf("session log: %w", err))
	}
	s.client = newClient(tr, nil, s.onClientEvent, nil)
	s.client.SetApprovalHandler(s.onApproval)

	ctx, cancel := context.WithTimeout(m.opCtx, m.timeout())
	defer cancel()
	if err := s.client.Initialize(ctx, "scimux", "1"); err != nil {
		return fail(fmt.Errorf("muse initialize: %w", err))
	}
	if err := s.client.StartSession(ctx, StartParams{
		Cwd: dir, Model: model, ApprovalMode: defaultApprovalMode,
	}); err != nil {
		return fail(fmt.Errorf("muse session/start: %w", err))
	}
	sid := s.client.SessionID()
	if sid == "" {
		return fail(fmt.Errorf("muse session/start: empty session id"))
	}
	s.mu.Lock()
	s.sessionID = sid
	s.mu.Unlock()
	if s.client.Live() != nil {
		return fail(fmt.Errorf("muse launch: client exited before publication"))
	}

	if err := m.publish(nodeID, s); err != nil {
		return fail(err)
	}
	if s.client.Live() != nil {
		s.markExited()
	}
	return sid, nil
}

func wrapCleanup(err, closeErr error) error {
	if err == nil {
		return closeErr
	}
	if closeErr == nil {
		return err
	}
	return fmt.Errorf("%w (cleanup: %v)", err, closeErr)
}

func (m *Manager) reserveLaunch(nodeID string) (asset.IngestFunc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return nil, ErrNotAlive
	}
	if _, ok := m.sessions[nodeID]; ok {
		return nil, ErrLaunchConflict
	}
	if _, ok := m.launching[nodeID]; ok {
		return nil, ErrLaunchConflict
	}
	if m.launching == nil {
		m.launching = map[string]struct{}{}
	}
	m.launching[nodeID] = struct{}{}
	m.wg.Add(1)
	return m.assetHook, nil
}

func (m *Manager) finishLaunch(nodeID string) {
	m.mu.Lock()
	delete(m.launching, nodeID)
	m.mu.Unlock()
	m.wg.Done()
}

func (m *Manager) publish(nodeID string, s *nodeSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return ErrNotAlive
	}
	if _, ok := m.sessions[nodeID]; ok {
		return ErrLaunchConflict
	}
	if s.client != nil && s.client.Live() != nil {
		return fmt.Errorf("muse launch: client exited before publication")
	}
	m.sessions[nodeID] = s
	delete(m.launching, nodeID)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		<-s.client.Done()
		s.markExited()
	}()
	return nil
}

func (m *Manager) beginOp(nodeID string) (*nodeSession, error) {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return nil, ErrNotAlive
	}
	s := m.sessions[nodeID]
	if s == nil {
		m.mu.Unlock()
		return nil, ErrNoSession
	}
	m.wg.Add(1)
	m.mu.Unlock()
	return s, nil
}

func (m *Manager) Send(nodeID, text string) error {
	s, err := m.beginOp(nodeID)
	if err != nil {
		return err
	}
	if err := s.admitWork(workTurn); err != nil {
		m.wg.Done()
		return err
	}
	if s.stopped() {
		s.abortTurn()
		s.releaseWork()
		m.wg.Done()
		return ErrNotAlive
	}
	if !s.persist(Event{T: "user", Text: text}) {
		s.abortTurn()
		s.releaseWork()
		m.wg.Done()
		return fmt.Errorf("record user turn: %s", s.LastError())
	}
	s.scanUser(nodeID, text)
	go func() {
		defer m.wg.Done()
		defer s.releaseWork()
		s.runTurn(text)
	}()
	return nil
}

func (m *Manager) Clear(nodeID string) error {
	s, err := m.beginOp(nodeID)
	if err != nil {
		return err
	}
	defer m.wg.Done()
	if err := s.admitWork(workTurn); err != nil {
		return err
	}
	defer s.releaseWork()
	defer s.abortTurn()

	// cbGate orders Client.Clear, the durable seam, and state reset ahead of
	// new-session callbacks. It is acquired on the caller goroutine, never on
	// the JSON-RPC dispatchLoop, so a blocked sink cannot stall Call().
	s.lockCallbacks()
	defer s.unlockCallbacks()

	ctx, cancel := context.WithTimeout(s.opCtx, s.opTimeout)
	defer cancel()
	if err := s.client.Clear(ctx, StartParams{
		Cwd: s.dir, Model: s.model, ApprovalMode: defaultApprovalMode,
	}); err != nil {
		s.setLastError("muse session/start: " + err.Error())
		return fmt.Errorf("muse session/start: %w", err)
	}
	newID := s.client.SessionID()
	if s.stopped() {
		s.resetAfterClear(newID)
		return ErrNotAlive
	}
	if !s.persist(sessionlog.NewClearSource(newID)) {
		s.resetAfterClear(newID)
		msg := s.LastError()
		if msg == "" {
			msg = "clear seam was not persisted"
		} else {
			msg = "clear seam was not persisted: " + msg
		}
		s.setLastError(msg)
		return fmt.Errorf("record clear seam: %s", msg)
	}
	s.resetAfterClear(newID)
	return nil
}

func (m *Manager) Interrupt(nodeID string) error {
	s, err := m.beginOp(nodeID)
	if err != nil {
		return err
	}
	defer m.wg.Done()
	if err := s.admitWork(workActiveTurn); err != nil {
		return err
	}
	defer s.releaseWork()
	ctx, cancel := context.WithTimeout(s.opCtx, s.opTimeout)
	defer cancel()
	if err := s.client.Interrupt(ctx); err != nil {
		s.noteErr(err.Error())
		return err
	}
	return nil
}

func (m *Manager) PrepareResolve(nodeID, expectedRequestID, key string) (optID, evidence string, err error) {
	s, err := m.beginOp(nodeID)
	if err != nil {
		return "", "", err
	}
	defer m.wg.Done()
	if err := s.admitWork(workPlain); err != nil {
		return "", "", err
	}
	defer s.releaseWork()
	return s.prepareResolve(expectedRequestID, key)
}

func (m *Manager) Deliver(nodeID, optID string) error {
	s, err := m.beginOp(nodeID)
	if err != nil {
		return err
	}
	defer m.wg.Done()
	if err := s.admitWork(workPlain); err != nil {
		return err
	}
	defer s.releaseWork()
	return s.deliver(optID)
}

func (m *Manager) Pending(nodeID string) (agentperm.Pending, bool) {
	s := m.session(nodeID)
	if s == nil {
		return agentperm.Pending{}, false
	}
	return s.pendingInfo()
}

func (m *Manager) PermissionBoundary(nodeID string) (incarn string, maxSeq uint64, ok bool) {
	s := m.session(nodeID)
	if s == nil {
		return "", 0, false
	}
	s.reconcileDead()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping || !s.procAlive || s.incarn == "" {
		return "", 0, false
	}
	return s.incarn, s.pendingSeq, true
}

func (m *Manager) Turns(nodeID string) []transcript.Turn { return readTurns(m.logPath(nodeID)) }

func (m *Manager) Peek(nodeID string) string { return peekLog(m.logPath(nodeID), 200) }

func (m *Manager) Usage(nodeID string) (used, window int64) {
	return latestSegmentUsage(m.logPath(nodeID))
}

func (m *Manager) Live(nodeID string) string {
	if s := m.session(nodeID); s != nil {
		return s.Live()
	}
	return "exited"
}

func (m *Manager) Attention(nodeID string) string {
	if s := m.session(nodeID); s != nil {
		return s.Attention()
	}
	return ""
}

func (m *Manager) LastError(nodeID string) string {
	if s := m.session(nodeID); s != nil {
		return s.LastError()
	}
	return ""
}

func (m *Manager) HasSession(nodeID string) bool { return m.session(nodeID) != nil }

func (m *Manager) Kill(nodeID string) error {
	m.mu.Lock()
	s := m.sessions[nodeID]
	delete(m.sessions, nodeID)
	m.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.stop()
}

func (m *Manager) RecordStartFailure(nodeID string, cause error) error {
	msg := "first prompt not delivered"
	if cause != nil {
		msg += ": " + cause.Error()
	}
	s, err := m.beginOp(nodeID)
	if err != nil {
		return nil
	}
	defer m.wg.Done()
	if err := s.admitFailureRecord(); err != nil {
		return nil
	}
	defer s.releaseWork()
	logw := s.logw
	if logw == nil {
		return nil
	}
	if err := logw.Append(Event{T: "error", Error: msg}); err != nil {
		s.noteErr("session log write failed: " + err.Error())
		return err
	}
	s.noteErr(msg)
	return nil
}

func (m *Manager) Models(nodeID string) ([]Model, error) {
	s, err := m.beginOp(nodeID)
	if err != nil {
		return nil, err
	}
	defer m.wg.Done()
	if err := s.admitWork(workPlain); err != nil {
		return nil, err
	}
	defer s.releaseWork()
	ctx, cancel := context.WithTimeout(s.opCtx, s.opTimeout)
	defer cancel()
	ms, err := s.client.Models(ctx)
	if err != nil {
		return nil, err
	}
	return cloneModels(ms), nil
}

func (m *Manager) CatalogSource(nodeID string) string {
	s := m.session(nodeID)
	if s == nil || s.client == nil {
		return ""
	}
	return s.client.CatalogSource()
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		m.wg.Wait()
		return
	}
	m.closing = true
	if m.opCancel != nil {
		m.opCancel()
	}
	ss := make([]*nodeSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		ss = append(ss, s)
	}
	m.sessions = map[string]*nodeSession{}
	m.launching = map[string]struct{}{}
	m.mu.Unlock()
	for _, s := range ss {
		_ = s.stop()
	}
	m.wg.Wait()
}

// ---------- Session ----------

type nodeSession struct {
	nodeID    string
	client    *Client
	tr        Transport
	logw      logAppender
	logPath   string
	dir       string
	model     string
	effort    string
	sessionID string
	assetHook asset.IngestFunc
	incarn    string
	opCtx     context.Context
	opTimeout time.Duration

	cbGate sync.Mutex

	mu            sync.Mutex
	procAlive     bool
	stopping      bool
	turnActive    bool
	turnHadOutput bool
	lastError     string
	pendingSeq    uint64
	pending       *displayedPending
	prepared      map[string]permFence
	work          sync.WaitGroup
	done          chan struct{}
	doneOnce      sync.Once
	exitOnce      sync.Once
}

type displayedPending struct {
	seq      uint64
	approval Approval
}

type permFence struct {
	Incarn     string `json:"incarn"`
	Seq        uint64 `json:"seq"`
	SessionID  string `json:"sid"`
	ApprovalID string `json:"aid"`
	ReqAID     string `json:"rid"`
	ReqIdx     int    `json:"ridx"`
	ChoiceID   string `json:"cid,omitempty"`
}

func newSessionIncarn() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func encodeFence(f permFence) string {
	b, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func decodeFence(s string) (permFence, error) {
	raw, err := hex.DecodeString(s)
	if err != nil {
		return permFence{}, err
	}
	var f permFence
	if err := json.Unmarshal(raw, &f); err != nil {
		return permFence{}, err
	}
	if f.Incarn == "" || f.ApprovalID == "" {
		return permFence{}, errors.New("malformed fence")
	}
	return f, nil
}

func (s *nodeSession) clientDead() bool {
	return s.client != nil && s.client.Live() != nil
}

func (s *nodeSession) reconcileDead() {
	if s.clientDead() {
		s.markExited()
	}
}

func (s *nodeSession) Live() string {
	s.reconcileDead()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.procAlive {
		return "exited"
	}
	if s.turnActive {
		return "active"
	}
	return "quiet"
}

func (s *nodeSession) Attention() string {
	if s.stopped() {
		return ""
	}
	s.mu.Lock()
	has := s.pending != nil
	s.mu.Unlock()
	if has {
		return "approval"
	}
	if s.client != nil {
		if a, ok := s.client.PendingApproval(); ok && s.approvalBelongs(a) {
			return "approval"
		}
	}
	return ""
}

const (
	workPlain = iota
	workTurn
	workActiveTurn
)

func (s *nodeSession) admitWork(kind int) error {
	s.reconcileDead()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping || !s.procAlive {
		return ErrNotAlive
	}
	switch kind {
	case workTurn:
		if s.turnActive {
			return ErrTurnActive
		}
		s.turnActive = true
		s.turnHadOutput = false
		s.lastError = ""
	case workActiveTurn:
		if !s.turnActive {
			return ErrNoTurn
		}
	}
	s.work.Add(1)
	return nil
}

func (s *nodeSession) releaseWork() { s.work.Done() }

func (s *nodeSession) stopped() bool {
	s.reconcileDead()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping || !s.procAlive
}

func (s *nodeSession) admitFailureRecord() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return ErrNotAlive
	}
	s.work.Add(1)
	return nil
}

func (s *nodeSession) lockCallbacks()   { s.cbGate.Lock() }
func (s *nodeSession) unlockCallbacks() { s.cbGate.Unlock() }

func (s *nodeSession) persist(ev Event) bool {
	logw := s.logw
	if logw == nil {
		return false
	}
	if err := logw.Append(ev); err != nil {
		s.mu.Lock()
		if s.lastError == "" {
			s.lastError = "session log write failed: " + err.Error()
		}
		s.mu.Unlock()
		fmt.Fprintf(os.Stderr, "scimux/muse: persisting %s for %s failed: %v\n", ev.T, s.nodeID, err)
		return false
	}
	return true
}

func (s *nodeSession) setLastError(msg string) {
	s.mu.Lock()
	s.lastError = msg
	s.mu.Unlock()
}

func (s *nodeSession) LastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastError
}

func (s *nodeSession) abortTurn() {
	s.mu.Lock()
	s.turnActive = false
	s.mu.Unlock()
}

func (s *nodeSession) noteErr(msg string) {
	s.mu.Lock()
	if s.lastError == "" {
		s.lastError = msg
	}
	s.mu.Unlock()
}

func (s *nodeSession) runTurn(text string) {
	if s.stopped() {
		s.failAdmission(ErrNotAlive)
		return
	}
	parent := s.opCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, s.opTimeout)
	defer cancel()
	if err := s.client.StartTurn(ctx, text); err != nil {
		s.failAdmission(err)
	}
}

func (s *nodeSession) failAdmission(err error) {
	s.mu.Lock()
	if !s.turnActive {
		s.mu.Unlock()
		return
	}
	msg := err.Error()
	s.lastError = msg
	s.turnActive = false
	s.mu.Unlock()
	s.persist(Event{T: "error", Error: msg})
}

func (s *nodeSession) scanUser(nodeID, text string) {
	hook := s.assetHook
	dir := s.dir
	if hook == nil {
		return
	}
	cands := asset.ScanMarkdown(text)
	if len(cands) == 0 {
		return
	}
	hook(nodeID, dir, cands)
}

func (s *nodeSession) onEvent(ev Event) {
	captured := ""
	if s.client != nil {
		captured = s.client.SessionID()
	}
	s.onClientEvent(captured, ev)
}

func (s *nodeSession) onClientEvent(captured string, ev Event) {
	if err := s.admitWork(workPlain); err != nil {
		return
	}
	defer s.releaseWork()
	s.lockCallbacks()
	defer s.unlockCallbacks()
	if s.stopped() {
		return
	}
	s.mu.Lock()
	cur := s.sessionID
	s.mu.Unlock()
	if captured != "" && cur != "" && captured != cur {
		return
	}
	if ev.T == "user" {
		return
	}
	ev = cloneEvent(ev)
	ok := s.persist(ev)
	var hook asset.IngestFunc
	var nodeID, dir string
	var cands []asset.Candidate
	var emptyStop string
	s.mu.Lock()
	if ok && (ev.T == "assistant" || isOutputTool(ev)) {
		s.turnHadOutput = true
		if s.assetHook != nil {
			hook = s.assetHook
			nodeID, dir = s.nodeID, s.dir
			cands = scanEvent(ev)
		}
	}
	if ok && ev.T == "stop" && s.turnActive {
		if !s.turnHadOutput {
			s.lastError = "agent produced no output this turn"
			emptyStop = s.lastError
		}
		s.turnActive = false
	} else if ok && ev.T == "error" && s.turnActive && s.client != nil && !s.client.TurnActive() {
		if s.lastError == "" {
			s.lastError = ev.Error
		}
		s.turnActive = false
	}
	s.mu.Unlock()
	if emptyStop != "" {
		s.persist(Event{T: "error", Error: emptyStop})
	}
	s.syncPendingFromClient()
	if hook != nil && len(cands) > 0 {
		hook(nodeID, dir, cands)
	}
}

func (s *nodeSession) onApproval(a Approval) {
	if err := s.admitWork(workPlain); err != nil {
		return
	}
	defer s.releaseWork()
	s.lockCallbacks()
	defer s.unlockCallbacks()
	if s.stopped() {
		return
	}
	if a.SessionID != "" {
		s.mu.Lock()
		cur := s.sessionID
		s.mu.Unlock()
		if cur != "" && a.SessionID != cur {
			return
		}
	}
	s.syncPendingFromClient()
}

func (s *nodeSession) approvalBelongs(a Approval) bool {
	if a.SessionID == "" {
		return true
	}
	s.mu.Lock()
	cur := s.sessionID
	s.mu.Unlock()
	return cur == "" || a.SessionID == cur
}

func (s *nodeSession) syncPendingFromClient() {
	if s.client == nil {
		return
	}
	cur, ok := s.client.PendingApproval()
	if ok && !s.approvalBelongs(cur) {
		ok = false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping || !s.procAlive {
		s.pending = nil
		s.prepared = nil
		return
	}
	s.applyPendingLocked(cur, ok)
}

func (s *nodeSession) applyPendingLocked(cur Approval, ok bool) {
	if !ok {
		s.pending = nil
		s.prepared = nil
		return
	}
	if s.pending != nil &&
		s.pending.approval.ApprovalID == cur.ApprovalID &&
		s.pending.approval.Requirement == cur.Requirement {
		s.pending.approval = cur
		return
	}
	s.pendingSeq++
	s.pending = &displayedPending{seq: s.pendingSeq, approval: cur}
	s.prepared = nil
}

func (s *nodeSession) pendingInfo() (agentperm.Pending, bool) {
	if s.stopped() {
		return agentperm.Pending{}, false
	}
	s.syncPendingFromClient()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping || !s.procAlive || s.pending == nil {
		return agentperm.Pending{}, false
	}
	a := s.pending.approval
	opts := make([]agentperm.Option, 0, len(a.Choices))
	for i, c := range a.Choices {
		name := c.Label
		if name == "" {
			name = c.ChoiceID
		}
		if name == "" {
			name = c.Decision
		}
		opts = append(opts, agentperm.Option{
			Key:  strconv.Itoa(i + 1),
			Name: name,
			Kind: choiceKind(c.Decision),
		})
	}
	return agentperm.Pending{
		RequestID: encodeFence(s.fenceOfLocked(s.pending)),
		Title:     a.Describe(),
		ToolKind:  mapSubjectToolKind(a.Subject.Kind),
		Options:   opts,
	}, true
}

func (s *nodeSession) fenceOfLocked(p *displayedPending) permFence {
	sid := s.sessionID
	if sid == "" && s.client != nil {
		sid = s.client.SessionID()
	}
	return permFence{
		Incarn:     s.incarn,
		Seq:        p.seq,
		SessionID:  sid,
		ApprovalID: p.approval.ApprovalID,
		ReqAID:     p.approval.Requirement.ApprovalID,
		ReqIdx:     p.approval.Requirement.SourceIndex,
	}
}

func (s *nodeSession) prepareResolve(expectedRequestID, key string) (optID, evidence string, err error) {
	if s.stopped() {
		return "", "", ErrNotAlive
	}
	s.syncPendingFromClient()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping || !s.procAlive {
		return "", "", ErrNotAlive
	}
	if s.pending == nil {
		return "", "", ErrNoPending
	}
	curID := encodeFence(s.fenceOfLocked(s.pending))
	if expectedRequestID == "" || expectedRequestID != curID {
		return "", "", ErrStalePermission
	}
	ch, err := mapKeyToChoice(key, s.pending.approval.Choices)
	if err != nil {
		return "", "", err
	}
	tok := s.fenceOfLocked(s.pending)
	tok.ChoiceID = ch.ChoiceID
	id := encodeFence(tok)
	if s.prepared == nil {
		s.prepared = map[string]permFence{}
	}
	s.prepared[id] = tok
	return id, "permission: " + s.pending.approval.Describe(), nil
}

func (s *nodeSession) deliver(optID string) error {
	if s.stopped() {
		return ErrNotAlive
	}
	s.syncPendingFromClient()
	s.mu.Lock()
	if s.stopping || !s.procAlive {
		s.mu.Unlock()
		return ErrNotAlive
	}
	if s.pending == nil {
		s.mu.Unlock()
		return ErrNoPending
	}
	f, err := decodeFence(optID)
	if err != nil {
		s.mu.Unlock()
		return ErrStalePermission
	}
	stored, ok := s.prepared[optID]
	if !ok || stored != f {
		s.mu.Unlock()
		return ErrStalePermission
	}
	cur := s.pending
	sid := s.sessionID
	if f.Incarn != s.incarn || f.Seq != cur.seq || f.SessionID != sid ||
		f.ApprovalID != cur.approval.ApprovalID ||
		f.ReqAID != cur.approval.Requirement.ApprovalID ||
		f.ReqIdx != cur.approval.Requirement.SourceIndex ||
		f.ChoiceID == "" {
		s.mu.Unlock()
		return ErrStalePermission
	}
	delete(s.prepared, optID)
	aid := f.ApprovalID
	req := RequirementRef{ApprovalID: f.ReqAID, SourceIndex: f.ReqIdx}
	cid := f.ChoiceID
	parent := s.opCtx
	timeout := s.opTimeout
	s.mu.Unlock()

	if live, ok := s.client.PendingApproval(); !ok || live.ApprovalID != aid || live.Requirement != req {
		return ErrStalePermission
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	err = s.client.Decide(ctx, aid, req, cid, "")
	if errors.Is(err, ErrStaleApproval) {
		return ErrStalePermission
	}
	return err
}

func (s *nodeSession) resetAfterClear(newID string) {
	s.mu.Lock()
	s.sessionID = newID
	s.incarn = newSessionIncarn()
	s.pendingSeq = 0
	s.pending = nil
	s.prepared = nil
	s.turnHadOutput = false
	s.mu.Unlock()
}

func (s *nodeSession) markExited() {
	s.exitOnce.Do(func() {
		s.mu.Lock()
		s.procAlive = false
		s.pending = nil
		s.prepared = nil
		needErr := s.turnActive
		msg := s.lastError
		if needErr {
			if msg == "" {
				msg = "muse subprocess exited during turn"
				s.lastError = msg
			}
			s.turnActive = false
		}
		s.mu.Unlock()
		if needErr {
			s.persist(Event{T: "error", Error: msg})
		}
		s.closeDone()
	})
	s.mu.Lock()
	s.procAlive = false
	s.mu.Unlock()
	s.closeDone()
}

func (s *nodeSession) closeDone() {
	if s.done == nil {
		return
	}
	s.doneOnce.Do(func() { close(s.done) })
}

func (s *nodeSession) stop() error {
	s.mu.Lock()
	s.stopping = true
	s.procAlive = false
	s.mu.Unlock()
	s.closeDone()
	var err error
	if s.client != nil {
		err = s.client.Close()
	} else if s.tr != nil {
		err = s.tr.Close()
	}
	s.markExited()
	s.work.Wait()
	return err
}

func mapKeyToChoice(key string, cs []Choice) (Choice, error) {
	if n, err := strconv.Atoi(key); err == nil {
		if n >= 1 && n <= len(cs) {
			return cs[n-1], nil
		}
		return Choice{}, fmt.Errorf("key %q maps to no choice", key)
	}
	switch key {
	case "y", "Enter":
		for _, c := range cs {
			if !c.IsRejection() {
				return c, nil
			}
		}
	case "n", "Escape":
		for _, c := range cs {
			if c.IsRejection() {
				return c, nil
			}
		}
	}
	return Choice{}, fmt.Errorf("key %q maps to no choice", key)
}

func choiceKind(decision string) string {
	switch decision {
	case "approved":
		return "allow"
	case "approvedForSession", "approvedPolicyAmendment":
		return "allow_always"
	}
	if IsRejection(decision) {
		return "reject"
	}
	return ""
}

func mapSubjectToolKind(kind string) string {
	switch strings.ToLower(kind) {
	case "command", "usershell", "execute", "shell":
		return "execute"
	case "file", "path", "edit", "write", "patch":
		return "edit"
	default:
		return ""
	}
}

func isOutputTool(ev Event) bool {
	return ev.T == "tool" && ev.Tool != nil && ev.Tool.Kind != "approval"
}

func scanEvent(ev Event) []asset.Candidate {
	switch {
	case ev.T == "assistant":
		return asset.ScanMarkdown(ev.Text)
	case ev.T == "tool" && ev.Tool != nil:
		return asset.ScanToolOutput(ev.Tool)
	}
	return nil
}

func cloneEvent(ev Event) Event {
	ev.Prov = copyRaw(ev.Prov)
	if ev.Tool != nil {
		t := *ev.Tool
		ev.Tool = &t
	}
	if ev.Usage != nil {
		u := *ev.Usage
		ev.Usage = &u
	}
	return ev
}

func cloneModels(in []Model) []Model {
	out := make([]Model, len(in))
	for i, m := range in {
		if m.ContextLimit != nil {
			v := *m.ContextLimit
			m.ContextLimit = &v
		}
		if m.OutputLimit != nil {
			v := *m.OutputLimit
			m.OutputLimit = &v
		}
		if m.ReleaseDate != nil {
			v := *m.ReleaseDate
			m.ReleaseDate = &v
		}
		out[i] = m
	}
	return out
}
