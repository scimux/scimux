package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// Manager owns every codex node's app-server subprocess and JSON-RPC client.
// It is the codex-app-server sibling of acp.Manager: same public surface, same
// contract with the rest of scimux (one node = one subprocess = one thread; the
// append-only session log is the authoritative history; unknown protocol
// records are ignored, never errors; the node degrades to read-only log history
// when no live subprocess backs it). The only difference is the wire underneath
// — codex's own app-server protocol (see peer.go), not the ACP SDK.
type Manager struct {
	mu        sync.Mutex
	sessions  map[string]*Session
	spawn     SpawnFunc
	logDir    string
	closing   bool
	assetHook asset.IngestFunc
	// wg tracks in-flight runTurn goroutines so Shutdown can wait for them
	// before returning, preventing writes to a deleted log directory.
	// Add(1) is always called while holding mu (before Shutdown can set
	// closing=true and reach Wait), so Wait() never misses a concurrent Add.
	wg sync.WaitGroup
}

// SpawnFunc launches the app-server transport for a node. Injectable so unit
// tests point at an in-process fake over pipes (no codex CLI, no tokens), the
// codex analogue of acp's Runner seam.
type SpawnFunc func(nodeID, dir string) (Transport, error)

// Errors surfaced to the HTTP layer so it can pick a status code. They mirror
// acp's sentinels one-for-one (ErrTurnActive lives in client.go).
var (
	ErrNoSession = errors.New("no live codex session for node")
	ErrNotAlive  = errors.New("codex subprocess has exited")
	ErrNoPending = errors.New("no permission request is pending")
	ErrNoTurn    = errors.New("no turn is in flight")
)

// launchTimeout bounds initialize + thread/start so a hung app-server (e.g. one
// blocked on an auth prompt it never surfaces) can't wedge Launch.
const launchTimeout = 30 * time.Second

// defaultApprovalPolicy asks the agent to request approval for risky actions so
// the supervisor answers them through the UI. The server may downgrade it; the
// effective value is read back from thread/start (never assumed).
const defaultApprovalPolicy = "on-request"

// NewManager builds a Manager that spawns real `codex app-server` subprocesses.
func NewManager(logDir string) *Manager {
	return &Manager{sessions: map[string]*Session{}, spawn: defaultSpawn, logDir: logDir}
}

// NewManagerWithSpawn is the test constructor: the SpawnFunc supplies an
// in-process fake server, so tests need no codex CLI and no tokens.
func NewManagerWithSpawn(logDir string, s SpawnFunc) *Manager {
	return &Manager{sessions: map[string]*Session{}, spawn: s, logDir: logDir}
}

// SetAssetHook installs the Phase 4 turn-append asset-ingestion hook (see
// asset.IngestFunc). Every session launched after this call carries it;
// main.go calls this once at startup, so in practice every session does.
func (m *Manager) SetAssetHook(f asset.IngestFunc) {
	m.mu.Lock()
	m.assetHook = f
	m.mu.Unlock()
}

func defaultSpawn(nodeID, dir string) (Transport, error) {
	// The working directory is delivered to codex via thread/start's cwd, so the
	// subprocess itself needs no special cwd here.
	return Spawn("codex")
}

func (m *Manager) logPath(nodeID string) string {
	return filepath.Join(m.logDir, nodeID+".jsonl")
}

func (m *Manager) session(nodeID string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[nodeID]
}

// Launch spawns the app-server, negotiates initialize, opens a thread (reading
// back the effective approval policy/model) and applies effort. It does NOT
// send the first prompt — that is an ordinary Send fired after the node is
// persisted, mirroring the ACP path. On any failure the subprocess (and its
// group) is closed before returning, so a failed launch never orphans. Returns
// the codex thread id for storage as the node's session id.
func (m *Manager) Launch(nodeID, agent, dir, model, effort string) (string, error) {
	if err := os.MkdirAll(m.logDir, 0o700); err != nil {
		return "", err
	}
	tr, err := m.spawn(nodeID, dir)
	if err != nil {
		return "", err
	}
	s := &Session{
		nodeID:    nodeID,
		tr:        tr,
		logw:      &logWriter{Path: m.logPath(nodeID)},
		dir:       dir,
		model:     model,
		effort:    effort,
		assetHook: m.assetHook,
		procAlive: true,
		done:      make(chan struct{}),
	}
	// Self-describing header first: the filename is a reusable slug, so the
	// log carries its own identity and launch config (see sessionlog.MetaEvent).
	// Remember whether the file pre-existed: a launch that fails after this
	// point must remove the meta-only log it just created, or the leftover
	// header makes the slug read as taken-by-dead-history forever (R20.3). A
	// pre-existing file is never ours to remove.
	_, statErr := os.Stat(s.logw.Path)
	discardLog := func() {
		if os.IsNotExist(statErr) {
			_ = os.Remove(s.logw.Path)
		}
	}
	if err := s.logw.Append(sessionlog.NewMeta(nodeID, agent, model, effort, dir)); err != nil {
		_ = tr.Close()
		discardLog()
		return "", fmt.Errorf("session log: %w", err)
	}
	s.client = NewClient(tr, s.onEvent, nil)
	s.client.SetApprovalHandler(s.approve)

	ctx, cancel := context.WithTimeout(context.Background(), launchTimeout)
	defer cancel()
	if _, err := s.client.Initialize(ctx, "scimux", "1"); err != nil {
		_ = tr.Close()
		discardLog()
		return "", fmt.Errorf("codex initialize: %w", err)
	}
	info, err := s.client.StartThread(ctx, StartThreadParams{
		Cwd: dir, ApprovalPolicy: defaultApprovalPolicy, Model: model, Effort: effort,
	})
	if err != nil {
		_ = tr.Close()
		discardLog()
		return "", fmt.Errorf("codex thread/start: %w", err)
	}
	s.threadID = info.ID

	m.mu.Lock()
	m.sessions[nodeID] = s
	m.mu.Unlock()

	// Flip liveness to "exited" the moment the subprocess dies (its stdout
	// closing ends the read loop), the analogue of reaping a tmux pane.
	go func() {
		<-s.client.Done()
		s.markExited()
	}()
	return info.ID, nil
}

// Send delivers a prompt turn. Preflight mirrors acp.Manager.Send: the session
// must be alive, only one turn at a time, and the user turn is recorded before
// prompting — a log-append failure refuses the send rather than prompting
// without a durable record. The turn itself runs in a goroutine.
func (m *Manager) Send(nodeID, text string) error {
	m.mu.Lock()
	s := m.sessions[nodeID]
	if s == nil {
		m.mu.Unlock()
		return ErrNoSession
	}
	if m.closing {
		m.mu.Unlock()
		return ErrNotAlive
	}
	// Add while holding the lock so Shutdown's wg.Wait() can never observe a
	// zero count between the session lookup and the goroutine launch (finding 87).
	m.wg.Add(1)
	m.mu.Unlock()

	if !s.alive() {
		m.wg.Done()
		return ErrNotAlive
	}
	ctx, ok := s.reserveTurn()
	if !ok {
		m.wg.Done()
		return ErrTurnActive
	}
	if err := s.logw.Append(Event{T: "user", Text: text}); err != nil {
		s.abortTurn()
		m.wg.Done()
		return fmt.Errorf("record user turn: %w", err)
	}
	if s.assetHook != nil {
		if cands := asset.ScanMarkdown(text); len(cands) > 0 {
			s.assetHook(nodeID, s.dir, cands)
		}
	}
	go func() { defer m.wg.Done(); s.runTurn(ctx, text) }()
	return nil
}

// Clear opens a fresh thread for the node — the structured-transport /clear.
// Unlike acp.Manager.Clear (which replaces the subprocess, because a second
// session/new on one ACP connection is unproven upstream), codex keeps the
// same PID: multiple threads per app-server process are a first-class
// concept of its protocol. thread/start is negotiated first and the source
// seam is appended only after it succeeded, so the log records what actually
// happened; the prior conversation stays behind the seam in the same file.
// One turn at a time applies: a /clear racing an active turn is refused like
// a second Send.
func (m *Manager) Clear(nodeID string) error {
	m.mu.Lock()
	s := m.sessions[nodeID]
	closing := m.closing
	m.mu.Unlock()
	if s == nil {
		return ErrNoSession
	}
	if closing || !s.alive() {
		return ErrNotAlive
	}
	if _, ok := s.reserveTurn(); !ok {
		return ErrTurnActive
	}
	defer s.abortTurn() // the reservation only guarded the swap; no prompt ran
	ctx, cancel := context.WithTimeout(context.Background(), launchTimeout)
	defer cancel()
	info, err := s.client.StartThread(ctx, StartThreadParams{
		Cwd: s.dir, ApprovalPolicy: defaultApprovalPolicy, Model: s.model, Effort: s.effort,
	})
	if err != nil {
		return fmt.Errorf("codex thread/start: %w", err)
	}
	if err := s.logw.Append(sessionlog.NewClearSource(info.ID)); err != nil {
		return fmt.Errorf("record clear seam: %w", err)
	}
	s.mu.Lock()
	s.threadID = info.ID
	s.lastError = ""
	s.mu.Unlock()
	return nil
}

// PrepareResolve maps a whitelisted key to a pending decision and returns an
// opaque option id (the decision's index) plus audit evidence WITHOUT
// delivering it, so the HTTP layer can persist the decision before the agent is
// answered (finding 53). Deliver completes the answer.
func (m *Manager) PrepareResolve(nodeID, key string) (optID, evidence string, err error) {
	s := m.session(nodeID)
	if s == nil {
		return "", "", ErrNoSession
	}
	return s.prepareResolve(key)
}

// Deliver hands a previously mapped decision to the pending approval. It fails
// if the pending request changed or was already answered, so a stale answer is
// never delivered to a fresh request.
func (m *Manager) Deliver(nodeID, optID string) error {
	s := m.session(nodeID)
	if s == nil {
		return ErrNoSession
	}
	return s.deliver(optID)
}

// Pending reports the outstanding approval, if any: the tool/command title and
// the server-enumerated decisions rendered as answerable options.
func (m *Manager) Pending(nodeID string) (PendingPermission, bool) {
	s := m.session(nodeID)
	if s == nil {
		return PendingPermission{}, false
	}
	return s.pendingInfo()
}

// PendingPermission is the UI-facing view of one outstanding approval: a title,
// the tool's kind when known, and the answerable options. Empty ToolKind or
// option Kind means "unknown" — never an error, never a guess.
type PendingPermission struct {
	Title    string
	ToolKind string
	Options  []PermOption
}

// PermOption is one answerable decision: the key a supervisor presses, the
// decision's name (the codex decision enum, e.g. "accept"), and the option's
// role kind when known ("allow" | "reject" | ""). Shape matches acp.PermOption
// so the HTTP/UI layer treats both transports identically.
type PermOption struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

// RecordStartFailure marks a node whose first prompt could not be delivered
// (finding 52): it writes the failure to the authoritative log and, if a
// session still exists, surfaces it as the node's LastError.
func (m *Manager) RecordStartFailure(nodeID string, cause error) error {
	msg := "first prompt not delivered: " + cause.Error()
	// If the log is gone, the node was deleted (and its history archived)
	// while the first prompt was in flight. Recreating the file here would
	// resurrect a dead slug as an orphan that burns the name forever (R20.2);
	// with the node gone there is nothing left to record the failure for.
	if _, err := os.Stat(m.logPath(nodeID)); err != nil {
		return nil
	}
	logErr := (&logWriter{Path: m.logPath(nodeID)}).Append(Event{T: "error", Error: msg})
	if s := m.session(nodeID); s != nil {
		s.mu.Lock()
		if s.lastError == "" {
			s.lastError = msg
		}
		s.mu.Unlock()
	}
	return logErr
}

// HasSession reports whether a live subprocess backs this node.
func (m *Manager) HasSession(nodeID string) bool { return m.session(nodeID) != nil }

// Turns yields the chat history from the node's log (with or without a live
// subprocess).
func (m *Manager) Turns(nodeID string) []transcript.Turn { return readTurns(m.logPath(nodeID)) }

// Peek renders the tail of the raw event log — the app-server analogue of a
// pane photo.
func (m *Manager) Peek(nodeID string) string { return peekLog(m.logPath(nodeID), 200) }

// Usage reports context occupancy for the gauge (used, window), read from the
// log so it survives restarts.
func (m *Manager) Usage(nodeID string) (used, window int64) { return latestUsage(m.logPath(nodeID)) }

// Live reports liveness: "exited" without a subprocess, else "active" during a
// turn or "quiet" when idle.
func (m *Manager) Live(nodeID string) string {
	if s := m.session(nodeID); s != nil {
		return s.Live()
	}
	return "exited"
}

// Interrupt cancels the active prompt turn, if any.
func (m *Manager) Interrupt(nodeID string) error {
	s := m.session(nodeID)
	if s == nil {
		return ErrNoSession
	}
	return s.Interrupt()
}

// Attention is "approval" while a permission request is outstanding, else "".
func (m *Manager) Attention(nodeID string) string {
	if s := m.session(nodeID); s != nil {
		return s.Attention()
	}
	return ""
}

// LastError surfaces the most recent failed/empty turn for the chat view.
func (m *Manager) LastError(nodeID string) string {
	if s := m.session(nodeID); s != nil {
		return s.LastError()
	}
	return ""
}

// Kill stops one node's subprocess (group) and forgets it.
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

// Shutdown stops every codex subprocess. Like ACP children (and unlike tmux
// sessions), these are ours and must not orphan. Wire it to a signal handler.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.closing = true
	ss := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		ss = append(ss, s)
	}
	m.sessions = map[string]*Session{}
	m.mu.Unlock()
	for _, s := range ss {
		_ = s.stop()
	}
	// Wait for in-flight runTurn goroutines to finish. They write to the session
	// log; without this wait, callers (e.g. tests using t.TempDir) can delete
	// the log directory before the goroutine is done, producing write errors.
	m.wg.Wait()
}

// ---------- Session ----------

// Session is one node's codex client and turn state. The client drives the wire
// (streaming, turn completion, approval blocking); the Session owns the
// authoritative log and the acp-style bookkeeping (liveness, last error,
// "no output this turn", pending approvals) the fire-and-forget event
// sink cannot express on its own.
type Session struct {
	nodeID   string
	client   *Client
	tr       Transport
	logw     *logWriter
	threadID string
	// dir/model/effort are kept from Launch so Clear can open a fresh thread
	// under the same conditions (/clear never changes launch config — fork is
	// the path that can).
	dir       string
	model     string
	effort    string
	assetHook asset.IngestFunc

	mu            sync.Mutex
	procAlive     bool
	turnActive    bool
	turnCancel    context.CancelFunc
	turnHadOutput bool
	lastError     string
	pending       []*pendingPermission
	pendingSeq    uint64

	done     chan struct{} // closed on exit/stop; unblocks a pending approval
	doneOnce sync.Once
}

// pendingPermission is one outstanding approval. Codex can issue more than one
// server request while the same turn is active, so scimux queues them and lets
// the supervisor answer the head of the queue. seq identifies the request
// across the prepare→deliver gap (a bare queue index could name a different
// approval after the head changes).
type pendingPermission struct {
	seq      uint64
	approval Approval
	ch       chan chosen
}

type chosen struct {
	key     string
	payload json.RawMessage
	ok      bool
}

// onEvent is the client's Event sink: it persists every decoded record and
// tracks whether the turn produced any real output. A write failure is folded
// into the session's visible error state and stops the record from counting as
// output — the log is authoritative, so an event that did not persist must not
// let a turn look successful (findings 51/59). The first failure wins.
func (s *Session) onEvent(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appendLocked(ev) && (ev.T == "assistant" || ev.T == "tool") {
		s.turnHadOutput = true
	}
}

func (s *Session) appendLocked(ev Event) bool {
	if err := s.logw.Append(ev); err != nil {
		if s.lastError == "" {
			s.lastError = "session log write failed: " + err.Error()
		}
		fmt.Fprintf(os.Stderr, "scimux/codex: persisting %s for %s failed: %v\n", ev.T, s.nodeID, err)
		return false
	}
	s.scanAssetsLocked(ev)
	return true
}

// scanAssetsLocked is Phase 4's turn-append ingestion point for
// assistant/tool records (the user-turn equivalent lives in Manager.Send,
// which persists outside s.mu). It only detects candidates; asset.IngestFunc
// (main.go) owns eligibility, storage, and the log-append of the asset event
// itself. Callers hold s.mu.
func (s *Session) scanAssetsLocked(ev Event) {
	if s.assetHook == nil {
		return
	}
	var cands []asset.Candidate
	switch {
	case ev.T == "assistant":
		cands = asset.ScanMarkdown(ev.Text)
	case ev.T == "tool" && ev.Tool != nil:
		cands = asset.ScanToolOutput(ev.Tool)
	}
	if len(cands) > 0 {
		s.assetHook(s.nodeID, s.dir, cands)
	}
}

// approve is the injected ApprovalFunc. It queues a pending request and blocks
// until a supervisor answers it (via Deliver) or the turn/session is
// cancelled (done closes → fail-closed rejection). The pending approval is
// already logged as tool evidence by the client before this is called.
func (s *Session) approve(a Approval) (string, json.RawMessage, bool) {
	ch := make(chan chosen, 1)
	s.mu.Lock()
	s.pendingSeq++
	p := &pendingPermission{seq: s.pendingSeq, approval: a, ch: ch}
	s.pending = append(s.pending, p)
	s.mu.Unlock()

	defer s.removePending(p)
	select {
	case c := <-ch:
		return c.key, c.payload, c.ok
	case <-s.done:
		return "", nil, false
	}
}

// prepareResolve maps a whitelisted key to a pending decision and returns an
// opaque token naming both the approval (by identity) and the chosen decision
// index, plus audit evidence, without delivering. The token pins deliver to
// this exact approval: with a queue, the head can change between the audit
// write and the delivery (turn end, cancellation), and a bare index would
// silently answer a different request than the one recorded as evidence.
func (s *Session) prepareResolve(key string) (optID, evidence string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return "", "", ErrNoPending
	}
	p := s.pending[0]
	idx, ok := mapKeyToDecision(key, p.approval.AvailableDecisions)
	if !ok {
		return "", "", fmt.Errorf("key %q maps to no decision", key)
	}
	return fmt.Sprintf("%d:%d", p.seq, idx), "permission: " + approvalTitle(p.approval), nil
}

// deliver hands the mapped decision to the blocked approve goroutine. It holds
// s.mu across the check-and-send and consumes the pending request atomically,
// so a decision mapped against a since-replaced request is refused rather than
// misdelivered (the finding-53 race, mirrored from acp.deliver). The token's
// sequence number must still name the queue head.
func (s *Session) deliver(optID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return ErrNoPending
	}
	p := s.pending[0]
	var seq uint64
	var idx int
	if n, err := fmt.Sscanf(optID, "%d:%d", &seq, &idx); n != 2 || err != nil {
		return errors.New("malformed decision token")
	}
	if seq != p.seq || idx < 0 || idx >= len(p.approval.AvailableDecisions) {
		return errors.New("pending permission changed before the answer was delivered")
	}
	d := p.approval.AvailableDecisions[idx]
	s.pending = s.pending[1:] // consume; approve's deferred removePending is now a no-op
	select {
	case p.ch <- chosen{key: d.Key, payload: d.Payload, ok: true}:
		return nil
	default:
		return errors.New("permission request was already answered")
	}
}

func (s *Session) pendingInfo() (PendingPermission, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return PendingPermission{}, false
	}
	a := s.pending[0].approval
	ds := a.AvailableDecisions
	opts := make([]PermOption, 0, len(ds))
	for i, d := range ds {
		// RED stub: Kind left empty until feat maps Decision.IsRejection().
		opts = append(opts, PermOption{Key: strconv.Itoa(i + 1), Name: d.Key})
	}
	return PendingPermission{
		Title:    approvalTitle(a),
		ToolKind: mapApprovalToolKind(a),
		Options:  opts,
	}, true
}

// mapDecisionKind projects a codex decision into scimux option vocabulary.
// Rejections become "reject"; everything else is "allow". Always/session
// variants are intentionally not distinguished — no grounded evidence for that
// enum yet, and a wrong guess would mislabel a persistent grant as a one-shot.
func mapDecisionKind(d Decision) string {
	// RED stub: always unknown until the green commit.
	_ = d
	return ""
}

// mapApprovalToolKind is "execute" when the approval carries a command, else "".
func mapApprovalToolKind(a Approval) string {
	// RED stub: always unknown until the green commit.
	_ = a
	return ""
}

func (s *Session) removePending(p *pendingPermission) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cur := range s.pending {
		if cur == p {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return
		}
	}
}

// mapKeyToDecision resolves a whitelisted dialog key to a decision index: a
// digit selects the Nth decision; y/Enter picks the first non-rejecting one;
// n/Escape picks the first rejecting one (or the last decision).
func mapKeyToDecision(key string, ds []Decision) (int, bool) {
	if len(ds) == 0 {
		return 0, false
	}
	if n, err := strconv.Atoi(key); err == nil {
		if n >= 1 && n <= len(ds) {
			return n - 1, true
		}
		return 0, false
	}
	switch key {
	case "y", "Enter":
		for i, d := range ds {
			if !d.IsRejection() {
				return i, true
			}
		}
		return 0, true
	case "n", "Escape":
		for i, d := range ds {
			if d.IsRejection() {
				return i, true
			}
		}
		return len(ds) - 1, true
	}
	return 0, false
}

// approvalTitle is the one-line label of an approval: the proposed command when
// there is one, otherwise the request method.
func approvalTitle(a Approval) string {
	if a.Command != "" {
		return a.Command
	}
	return a.Method
}

// --- turn lifecycle ---

func (s *Session) reserveTurn() (context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnActive {
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.turnActive = true
	s.turnCancel = cancel
	s.turnHadOutput = false
	s.lastError = ""
	return ctx, true
}

func (s *Session) abortTurn() {
	s.mu.Lock()
	if s.turnCancel != nil {
		s.turnCancel()
		s.turnCancel = nil
	}
	s.turnActive = false
	s.mu.Unlock()
}

func (s *Session) Interrupt() error {
	s.mu.Lock()
	if !s.procAlive {
		s.mu.Unlock()
		return ErrNotAlive
	}
	cancel := s.turnCancel
	if !s.turnActive || cancel == nil {
		s.mu.Unlock()
		return ErrNoTurn
	}
	s.mu.Unlock()
	cancel()
	return nil
}

func (s *Session) runTurn(ctx context.Context, text string) {
	err := s.client.RunTurn(ctx, s.threadID, text)
	if errors.Is(err, context.Canceled) {
		err = errors.New("turn interrupted by supervisor")
	}
	s.endTurn(err)
}

// endTurn closes out a turn: a RunTurn error (or a transport that closed
// mid-turn) is recorded; otherwise a stop record is written, and a turn that
// produced no assistant text or tool output at all is flagged as failed/empty
// (mirrors acp.endTurn). Usage records arrive on the stream and are already
// logged by onEvent.
func (s *Session) endTurn(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err != nil:
		s.lastError = err.Error()
		s.appendLocked(Event{T: "error", Error: err.Error()})
	default:
		s.appendLocked(Event{T: "stop", StopReason: "turn/completed"})
		if !s.turnHadOutput {
			if s.lastError == "" {
				s.lastError = "agent produced no output this turn"
			}
			s.appendLocked(Event{T: "error", Error: s.lastError})
		}
	}
	s.turnActive = false
	if s.turnCancel != nil {
		s.turnCancel()
	}
	s.turnCancel = nil
	s.turnHadOutput = false
	// Approvals cannot outlive their turn: after an interrupt the server
	// requests that were blocking on a human are moot — unblock them
	// fail-closed (the client answers "rejected by handler") instead of
	// leaving attention stuck on a dead question.
	for _, p := range s.pending {
		select {
		case p.ch <- chosen{}:
		default:
		}
	}
	s.pending = nil
}

// --- liveness / lifecycle ---

func (s *Session) Live() string {
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

func (s *Session) Attention() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) != 0 {
		return "approval"
	}
	return ""
}

func (s *Session) LastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastError
}

func (s *Session) alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procAlive
}

func (s *Session) markExited() {
	s.mu.Lock()
	s.procAlive = false
	s.mu.Unlock()
	s.closeDone()
}

func (s *Session) closeDone() { s.doneOnce.Do(func() { close(s.done) }) }

// stop tears a session down: unblock any pending approval (→ fail-closed
// rejection) and close the transport (which signals the process group and
// reaps it). Closing stdout ends the read loop, which fails any in-flight turn.
func (s *Session) stop() error {
	s.closeDone()
	return s.tr.Close()
}
