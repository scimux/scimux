// Package acp is scimux's second transport: instead of driving pi, opencode,
// and grok through a tmux-wrapped TUI and photographing the pane, it speaks
// the Agent Client Protocol (ACP) to them over stdio as local child
// processes. Streaming updates, tool state, usage and structured permission
// prompts arrive as typed records — no pane snapshots, no TUI string matching.
//
// This package is the single, deliberate exception to scimux's stdlib-only
// invariant (see AGENTS.md): it depends on github.com/coder/acp-go-sdk for the
// bidirectional JSON-RPC peer. The dependency is scoped here; the tmux and
// transcript core stay dependency-free.
//
// Contract with the rest of scimux, mirroring the tmux path's spirit:
//   - one node = one subprocess = one ACP session,
//   - the append-only session log (log.go) is the authoritative history —
//     when there is no live subprocess (after a scimux restart) the node
//     degrades to read-only history from that log,
//   - unknown session-update variants are ignored, never errors,
//   - filesystem/terminal methods are declined by design (scimux is a
//     supervisor, not the agent's filesystem).
package acp

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

	sdk "github.com/coder/acp-go-sdk"

	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// Errors returned to the HTTP layer so it can pick a status code.
var (
	ErrNoSession = errors.New("no live ACP session for node")
	ErrNotAlive  = errors.New("ACP subprocess has exited")
	ErrNoPending = errors.New("no permission request is pending")
	// ErrStalePermission means PrepareResolve was called with an expected
	// request ID that is not the current pending request (replaced, cleared,
	// or never matched). HTTP maps it to 409; the replacement stays pending.
	ErrStalePermission = errors.New("pending permission request has changed")
	ErrTurnActive      = errors.New("a turn is already in flight")
	ErrNoTurn          = errors.New("no turn is in flight")
)

// launchTimeout bounds initialize + new-session so a hung agent (e.g. one
// blocked on an auth prompt it never surfaces) can't wedge createNode.
const launchTimeout = 30 * time.Second

// Manager owns every node's ACP subprocess and connection. It is the seam the
// HTTP handlers talk to; the tmux Server has the analogous role for TUI nodes.
type Manager struct {
	mu        sync.Mutex
	sessions  map[string]*Session
	runner    Runner
	logDir    string
	assetHook asset.IngestFunc
}

// NewManager builds a Manager launching real agent subprocesses.
func NewManager(logDir string) *Manager {
	return &Manager{sessions: map[string]*Session{}, runner: execRunner, logDir: logDir}
}

// NewManagerWithRunner is the test constructor: the Runner supplies an
// in-process fake agent, so tests need no agent CLI and no tokens.
func NewManagerWithRunner(logDir string, r Runner) *Manager {
	return &Manager{sessions: map[string]*Session{}, runner: r, logDir: logDir}
}

// SetAssetHook installs the Phase 4 turn-append asset-ingestion hook (see
// asset.IngestFunc). Every session launched after this call carries it;
// main.go calls this once at startup, so in practice every session does.
func (m *Manager) SetAssetHook(f asset.IngestFunc) {
	m.mu.Lock()
	m.assetHook = f
	m.mu.Unlock()
}

// killAndReap kills a started subprocess and drains its exit in the background
// so a launch that fails after Start (initialize/new-session error) never leaks
// a zombie (finding 57). The success path installs its own reaper instead.
func killAndReap(p Process) {
	_ = p.Kill()
	go func() { _ = p.Wait() }()
}

func (m *Manager) logPath(nodeID string) string {
	return filepath.Join(m.logDir, nodeID+".jsonl")
}

func (m *Manager) session(nodeID string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[nodeID]
}

// SessionID returns the live ACP session id for nodeID, or "" if none.
// Used by the pi native fare re-source (fare-design Phase 4) so the join
// key stays correct after /clear even when the node record is not rewritten.
func (m *Manager) SessionID(nodeID string) string {
	s := m.session(nodeID)
	if s == nil {
		return ""
	}
	return string(s.sessionID)
}

// Launch spawns the agent, negotiates the protocol, opens a session, and
// applies effort. It does NOT send the first prompt — ACP's first prompt is
// an ordinary Send, fired after the node is persisted. On any failure the
// subprocess (and its group) is killed before returning, so a failed launch
// never leaves an orphan. Returns the ACP session id for storage.
func (m *Manager) Launch(nodeID, agent, dir, model, effort string) (string, error) {
	if err := os.MkdirAll(m.logDir, 0o700); err != nil {
		return "", err
	}
	proc, err := m.runner(nodeID, agent, dir, model, effort)
	if err != nil {
		return "", err
	}
	s := &Session{
		nodeID:    nodeID,
		agent:     agent,
		proc:      proc,
		logw:      &logWriter{Path: m.logPath(nodeID)},
		dir:       dir,
		model:     model,
		effort:    effort,
		assetHook: m.assetHook,
		incarn:    newSessionIncarn(),
		procAlive: true,
		done:      make(chan struct{}),
	}
	// Self-describing header first: the filename is a reusable slug, so the
	// log carries its own identity and launch config (see sessionlog.MetaEvent).
	// Remember whether the file pre-existed: a launch that fails after this
	// point must remove the meta-only log it just created, or the leftover
	// header makes sessionLogExists treat the slug as taken-by-dead-history
	// forever (R20.3). A pre-existing file is never ours to remove.
	_, statErr := os.Stat(s.logw.Path)
	discardLog := func() {
		if os.IsNotExist(statErr) {
			_ = os.Remove(s.logw.Path)
		}
	}
	if err := s.logw.Append(sessionlog.NewMeta(nodeID, agent, model, effort, dir)); err != nil {
		killAndReap(proc)
		discardLog()
		return "", fmt.Errorf("session log: %w", err)
	}
	s.conn = sdk.NewClientSideConnection(s, proc.Stdin(), proc.Stdout())

	ctx, cancel := context.WithTimeout(context.Background(), launchTimeout)
	defer cancel()
	// Be explicit about capabilities: we decline fs and terminal (plan §2).
	initResp, err := s.conn.Initialize(ctx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		ClientCapabilities: sdk.ClientCapabilities{
			Fs:       sdk.FileSystemCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: false,
		},
	})
	if err != nil {
		killAndReap(proc)
		discardLog()
		return "", fmt.Errorf("acp initialize: %w", err)
	}
	// Context window for the gauge: Grok (and some others) advertise
	// totalContextTokens on initialize modelState but never stream
	// usage_update Used/Size — so remember it for end-of-turn usage records.
	s.ctxSize = contextSizeFromMeta(initResp.Meta, model)
	resp, err := s.conn.NewSession(ctx, sdk.NewSessionRequest{Cwd: dir, McpServers: []sdk.McpServer{}})
	if err != nil {
		killAndReap(proc)
		discardLog()
		return "", fmt.Errorf("acp new session: %w", err)
	}
	s.sessionID = resp.SessionId
	if s.ctxSize == 0 {
		s.ctxSize = contextSizeFromMeta(resp.Meta, model)
	}
	s.captureBanner(resp)
	s.applyEffort(ctx, effort, resp)

	m.mu.Lock()
	m.sessions[nodeID] = s
	m.mu.Unlock()
	// Reap the subprocess so liveness flips to "exited" the moment it dies.
	go func() {
		_ = proc.Wait()
		s.markExited()
	}()
	return string(resp.SessionId), nil
}

// Send delivers a prompt turn. Preflight (plan §4.7): the session must be
// alive, and the user turn must be recorded before we prompt — a log-append
// failure refuses the send rather than prompting without a record. The prompt
// itself runs in a goroutine; a Prompt error after start is recorded as a
// visible error record, not swallowed.
func (m *Manager) Send(nodeID, text string) error {
	s := m.session(nodeID)
	if s == nil {
		return ErrNoSession
	}
	if !s.alive() {
		return ErrNotAlive
	}
	// Reserve the turn atomically: ACP is one turn at a time, so a second Send
	// racing the first must be refused, not silently interleaved.
	ctx, ok := s.reserveTurn()
	if !ok {
		return ErrTurnActive
	}
	if err := s.logw.Append(Event{T: "user", Text: text}); err != nil {
		s.abortTurn()
		return fmt.Errorf("record user turn: %w", err)
	}
	if s.assetHook != nil {
		if cands := asset.ScanMarkdown(text); len(cands) > 0 {
			s.assetHook(nodeID, s.dir, cands)
		}
	}
	go s.runPrompt(ctx, text)
	return nil
}

// Clear is the /clear for ACP nodes, and it is deliberately a process
// replacement: the old subprocess is killed and a fresh one is negotiated
// under the same node. ACP permits a second session/new over one connection,
// but the agents in the wild are driven one-session-per-process by their
// usual clients — that code path is unproven upstream, and /clear must be
// deterministic. A fresh PID self-evidently carries no prior context. Same
// node, same log file: the source seam is appended only after the
// replacement session exists, so the log records what actually happened; on
// any failure the old conversation is left untouched (/clear simply
// failed). One turn at a time applies: a /clear racing an active turn is
// refused like a second Send. (codex differs: its app-server hosts multiple
// threads per process as a first-class concept, so codex.Manager.Clear
// opens a thread on the same PID.)
func (m *Manager) Clear(nodeID string) error {
	old := m.session(nodeID)
	if old == nil {
		return ErrNoSession
	}
	if !old.alive() {
		return ErrNotAlive
	}
	// Fence the swap like a turn: refuse a racing /clear-vs-turn, and keep
	// new turns out while the replacement is negotiated.
	if _, ok := old.reserveTurn(); !ok {
		return ErrTurnActive
	}
	proc, err := m.runner(nodeID, old.agent, old.dir, old.model, old.effort)
	if err != nil {
		old.abortTurn()
		return err
	}
	s := &Session{
		nodeID:    nodeID,
		agent:     old.agent,
		proc:      proc,
		logw:      old.logw, // same file, same writer: stragglers stay serialized
		dir:       old.dir,
		model:     old.model,
		effort:    old.effort,
		ctxSize:   old.ctxSize, // fall back to prior window until initialize re-probes
		assetHook: old.assetHook,
		incarn:    newSessionIncarn(), // fresh identity: request IDs must not collide with pre-clear
		procAlive: true,
		done:      make(chan struct{}),
	}
	s.conn = sdk.NewClientSideConnection(s, proc.Stdin(), proc.Stdout())
	ctx, cancel := context.WithTimeout(context.Background(), launchTimeout)
	defer cancel()
	initResp, err := s.conn.Initialize(ctx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		ClientCapabilities: sdk.ClientCapabilities{
			Fs:       sdk.FileSystemCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: false,
		},
	})
	if err != nil {
		killAndReap(proc)
		old.abortTurn()
		return fmt.Errorf("acp initialize: %w", err)
	}
	if n := contextSizeFromMeta(initResp.Meta, s.model); n > 0 {
		s.ctxSize = n
	}
	resp, err := s.conn.NewSession(ctx, sdk.NewSessionRequest{Cwd: s.dir, McpServers: []sdk.McpServer{}})
	if err != nil {
		killAndReap(proc)
		old.abortTurn()
		return fmt.Errorf("acp new session: %w", err)
	}
	s.sessionID = resp.SessionId
	if n := contextSizeFromMeta(resp.Meta, s.model); n > 0 {
		s.ctxSize = n
	}
	s.captureBanner(resp)
	s.applyEffort(ctx, s.effort, resp)
	// Retire the old session's writer before the seam goes in: from here its
	// stragglers are dropped rather than landing inside the fresh segment
	// (finding 91). Reverted if the seam append fails, so that failure path
	// leaves the old session fully intact.
	old.setRetired(true)
	// Seam before the visible flip — record first, publish second.
	if err := s.logw.Append(sessionlog.NewClearSource(string(resp.SessionId))); err != nil {
		old.setRetired(false)
		killAndReap(proc)
		old.abortTurn()
		return fmt.Errorf("record clear seam: %w", err)
	}
	m.mu.Lock()
	m.sessions[nodeID] = s
	m.mu.Unlock()
	// Reap the replacement like Launch does, then retire the old process
	// (cancel, close session, kill the group). Its turn fence dies with it.
	go func() {
		_ = proc.Wait()
		s.markExited()
	}()
	_ = old.stop()
	return nil
}

// Resolve answers a pending permission request by mapping a whitelisted key to
// an option and delivering it in one call. Returns evidence (the tool title)
// for the audit record. The HTTP path uses the split PrepareResolve/Deliver so
// it can persist the audit record between mapping and delivery; this composed
// form is retained for tests and callers that do not audit separately.
func (m *Manager) Resolve(nodeID, key string) (string, error) {
	s := m.session(nodeID)
	if s == nil {
		return "", ErrNoSession
	}
	p, ok := s.pendingInfo()
	if !ok {
		return "", ErrNoPending
	}
	id, evidence, err := s.prepareResolve(p.RequestID, key)
	if err != nil {
		return "", err
	}
	if err := s.deliver(id); err != nil {
		return "", err
	}
	return evidence, nil
}

// PrepareResolve maps the pending key to a permission option and returns the
// option id plus audit evidence WITHOUT delivering it, so the HTTP layer can
// persist the decision before the agent ever receives the answer (finding 53:
// no unaudited permission decision). expectedRequestID must name the request
// the caller evaluated; a mismatch returns ErrStalePermission while the
// current pending request remains actionable. Deliver completes the answer
// and re-checks the prepared token against the live request.
func (m *Manager) PrepareResolve(nodeID, expectedRequestID, key string) (optID, evidence string, err error) {
	s := m.session(nodeID)
	if s == nil {
		return "", "", ErrNoSession
	}
	id, ev, err := s.prepareResolve(expectedRequestID, key)
	return string(id), ev, err
}

// Deliver sends a previously mapped permission option to the pending request.
// It fails if the pending request has changed or already been answered, so a
// stale answer is never delivered to a fresh request.
func (m *Manager) Deliver(nodeID, optID string) error {
	s := m.session(nodeID)
	if s == nil {
		return ErrNoSession
	}
	return s.deliver(sdk.PermissionOptionId(optID))
}

// RecordStartFailure marks a node whose first prompt could not be delivered
// (finding 52). It writes the failure to the authoritative log — so it survives
// a restart and renders in the peek/chat view — and, when a session still
// exists, surfaces it as the node's LastError. This upholds the creation
// invariant: once a node is published, its first prompt is either in the
// agent's context or visibly failed in the node's own history.
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

// Pending reports the outstanding permission request, if any, so the UI can
// render answer keys and the audit trail can capture the decision context.
func (m *Manager) Pending(nodeID string) (PendingPermission, bool) {
	s := m.session(nodeID)
	if s == nil {
		return PendingPermission{}, false
	}
	return s.pendingInfo()
}

// PendingPermission is the UI-facing view of one outstanding approval: a title,
// the tool's kind when known, an optional reason, and the answerable options.
// Empty ToolKind, option Kind, or Reason means "unknown" — never an error,
// never a guess. Shape matches codex.PendingPermission so the HTTP/UI layer
// treats both transports identically. ACP RequestPermission has no reason
// field; Reason stays empty there.
//
// RequestID is an opaque stable identity for the current pending request
// (monotonic per-session sequence). It stays fixed while the same request is
// pending and advances for the next request even when title/options match.
type PendingPermission struct {
	RequestID string // opaque; stable while this request is pending
	Title     string
	ToolKind  string
	Reason    string // why the agent is asking; empty when unknown (ACP always empty)
	Options   []PermOption
}

// PermOption is one answerable permission choice: the key a supervisor presses,
// the agent's human-readable label, and the option's role kind when known
// ("allow" | "allow_always" | "reject" | "reject_always" | "").
type PermOption struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

// HasSession reports whether a live subprocess backs this node (vs read-only
// history after a restart).
func (m *Manager) HasSession(nodeID string) bool { return m.session(nodeID) != nil }

// Turns yields the chat history from the node's log (works with or without a
// live subprocess).
func (m *Manager) Turns(nodeID string) []transcript.Turn { return readTurns(m.logPath(nodeID)) }

// Peek renders the tail of the raw event log — the ACP analogue of a pane
// photo.
func (m *Manager) Peek(nodeID string) string { return peekLog(m.logPath(nodeID), 200) }

// Usage reports context occupancy for the gauge (used, window). Read from the
// log so it survives restarts; 0 when the agent never reported it (pi).
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

// LastError surfaces the most recent failed/empty turn for the chat view
// (plan §4.6); "" when the last turn was fine.
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

// Shutdown stops every ACP subprocess. Unlike tmux sessions (which
// deliberately survive scimux exit), ACP children are ours and must not
// orphan (plan §4.9). Wire it to a signal handler.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	ss := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		ss = append(ss, s)
	}
	m.sessions = map[string]*Session{}
	m.mu.Unlock()
	for _, s := range ss {
		_ = s.stop()
	}
}

// ---------- Session ----------

// Session is one node's ACP connection and turn state. It implements the SDK's
// Client interface; the SDK dispatches inbound notifications and requests
// (SessionUpdate, RequestPermission, fs/*, terminal/*) onto it.
type Session struct {
	nodeID    string
	agent     string
	conn      *sdk.ClientSideConnection
	proc      Process
	sessionID sdk.SessionId
	logw      *logWriter
	// dir, model, and effort are kept from Launch so Clear can renegotiate a
	// fresh session under the same conditions (/clear never changes launch
	// config — fork is the path that can). model matters for agents that take
	// it as a process flag (grok); pi/opencode keep it for the meta header only.
	// ctxSize is the model context window (tokens) when advertised at
	// initialize/new-session. Grok never sends usage_update with size; the
	// gauge Size comes from this window while Used comes from _meta Layer A
	// last-call tokens (fare-design §2.5 / D10) — not nested spend.
	dir       string
	model     string
	effort    string
	ctxSize   int
	assetHook asset.IngestFunc

	mu            sync.Mutex
	procAlive     bool
	retired       bool // /clear replaced this session: its writes must not land after the seam
	turnActive    bool
	turnCancel    context.CancelFunc
	turnHadOutput bool
	curMsgID      string
	assistant     strings.Builder
	pending       *pendingPermission
	pendingSeq    uint64 // monotonic per-session; pairs with incarn in RequestID
	// incarn is a collision-resistant identity for this Session value. Request
	// IDs and prepared tokens include it so a post-/clear (or kill/relaunch)
	// session cannot accept a stale decision from a prior incarnation that
	// reused sequence numbers.
	incarn     string
	lastError  string
	banner     string
	bannerDone bool
	// shellUsed/shellSize remember the latest usage_update occupancy for the
	// in-flight turn so endTurn can pair it with PromptResponse.usage
	// (opencode two-shape; fare-design §2.1 / D5). Cleared at turn end.
	shellUsed int
	shellSize int

	done     chan struct{} // closed on exit/stop; unblocks a pending permission
	doneOnce sync.Once
}

// newSessionIncarn mints an opaque per-Session identity for request/token binding.
func newSessionIncarn() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// formatRequestID builds the opaque pending-request identity: incarn:seq.
func formatRequestID(incarn string, seq uint64) string {
	return incarn + ":" + strconv.FormatUint(seq, 10)
}

// ParseRequestID splits an opaque request id into incarnation and sequence.
// Exported so the app layer can apply auto-approve enable cutoffs.
func ParseRequestID(id string) (incarn string, seq uint64, ok bool) {
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

// PermissionBoundary returns the session incarnation and the highest request
// sequence issued so far. Auto-approve treats requests with the same
// incarnation and seq <= maxSeq as pre-enable (including any still queued).
func (m *Manager) PermissionBoundary(nodeID string) (incarn string, maxSeq uint64, ok bool) {
	s := m.session(nodeID)
	if s == nil {
		return "", 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.incarn == "" {
		return "", 0, false
	}
	return s.incarn, s.pendingSeq, true
}

// pendingPermission is the one outstanding permission request (ACP is one turn
// at a time, so at most one). RequestPermission blocks on ch until a key
// resolves it, or done closes (cancel/shutdown → cancelled outcome). seq
// identifies the request across the prepare→deliver gap so a stale answer
// cannot be delivered to a replacement request with the same option ids.
type pendingPermission struct {
	seq       uint64
	toolTitle string
	toolKind  string // ACP ToolKind string, or "" when unknown
	options   []sdk.PermissionOption
	ch        chan sdk.PermissionOptionId
}

var _ sdk.Client = (*Session)(nil)

// --- SDK Client: inbound from the agent ---

// SessionUpdate assembles streamed output into the log: agent text is buffered
// and flushed as assistant turns at message/tool/turn boundaries; tool calls
// and usage are recorded as they arrive; unknown variants are ignored.
func (s *Session) SessionUpdate(ctx context.Context, n sdk.SessionNotification) error {
	u := n.Update
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case u.AgentMessageChunk != nil:
		s.appendAssistantLocked(blockText(u.AgentMessageChunk.Content), u.AgentMessageChunk.MessageId)
	case u.ToolCall != nil:
		s.flushAssistantLocked()
		if s.appendLocked(Event{T: "tool", Tool: &ToolEvent{
			ID:       string(u.ToolCall.ToolCallId),
			Title:    u.ToolCall.Title,
			Kind:     string(u.ToolCall.Kind),
			Status:   string(u.ToolCall.Status),
			RawInput: u.ToolCall.RawInput,
		}}) {
			s.turnHadOutput = true
		}
	case u.ToolCallUpdate != nil:
		tu := u.ToolCallUpdate
		ev := &ToolEvent{ID: string(tu.ToolCallId), RawInput: tu.RawInput}
		if tu.Title != nil {
			ev.Title = *tu.Title
		}
		if tu.Kind != nil {
			ev.Kind = string(*tu.Kind)
		}
		if tu.Status != nil {
			ev.Status = string(*tu.Status)
		}
		if s.appendLocked(Event{T: "tool", Tool: ev}) {
			s.turnHadOutput = true
		}
	case u.UsageUpdate != nil:
		// Occupancy shell (opencode streams these; see fare-design §2.1).
		// Drop empty {} — contribute nothing. Cost amount is only recorded
		// when >0 (currency-only is fine; never fabricate a dollar figure).
		uu := u.UsageUpdate
		if uu.Used == 0 && uu.Size == 0 {
			break
		}
		ev := &UsageEvent{Used: uu.Used, Size: uu.Size}
		if uu.Cost != nil {
			if uu.Cost.Amount > 0 {
				ev.CostAmount = uu.Cost.Amount
			}
			if uu.Cost.Currency != "" {
				ev.CostCurrency = uu.Cost.Currency
			}
		}
		// Stash for turn-end pairing with PromptResponse.usage (opencode).
		s.shellUsed = uu.Used
		s.shellSize = uu.Size
		s.appendLocked(Event{T: "usage", Usage: ev})
	default:
		// Thoughts, plans, mode/config/info updates, and any future variant:
		// ignored, never an error (defensive-parsing contract).
	}
	return nil
}

// RequestPermission is the structured needs-input path. It runs on an SDK
// dispatch goroutine and blocks until a supervisor answers (via Resolve) or
// the turn/session is cancelled. There is deliberately no timeout: an approval
// may legitimately wait for a human.
func (s *Session) RequestPermission(ctx context.Context, p sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	title := ""
	if p.ToolCall.Title != nil {
		title = *p.ToolCall.Title
	}
	toolKind := ""
	if p.ToolCall.Kind != nil {
		toolKind = string(*p.ToolCall.Kind)
	}
	ch := make(chan sdk.PermissionOptionId, 1)
	s.mu.Lock()
	if s.pending != nil {
		// One pending at a time; a second is an agent error → cancel it. The
		// error record goes through appendLocked, never s.logw directly, so
		// the /clear retire fence applies (R20.1): a straggler request on the
		// dying connection must be dropped, not written after the seam into
		// the fresh segment.
		s.appendLocked(Event{T: "error", Error: "duplicate permission request ignored"})
		s.mu.Unlock()
		return cancelledPermission(), nil
	}
	s.pendingSeq++
	s.pending = &pendingPermission{
		seq: s.pendingSeq, toolTitle: title, toolKind: toolKind, options: p.Options, ch: ch,
	}
	s.mu.Unlock()

	defer s.clearPending()
	select {
	case id := <-ch:
		return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeSelected(id)}, nil
	case <-ctx.Done():
		return cancelledPermission(), nil
	case <-s.done:
		return cancelledPermission(), nil
	}
}

func cancelledPermission() sdk.RequestPermissionResponse {
	return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeCancelled()}
}

// Filesystem and terminal requests are declined by design: scimux supervises,
// it is not the agent's filesystem or terminal. Agents with local fs access
// (the common case) fall back to acting directly.
func (s *Session) ReadTextFile(ctx context.Context, p sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	return sdk.ReadTextFileResponse{}, sdk.NewMethodNotFound("fs/read_text_file")
}

func (s *Session) WriteTextFile(ctx context.Context, p sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	return sdk.WriteTextFileResponse{}, sdk.NewMethodNotFound("fs/write_text_file")
}

func (s *Session) CreateTerminal(ctx context.Context, p sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	return sdk.CreateTerminalResponse{}, sdk.NewMethodNotFound("terminal/create")
}

func (s *Session) KillTerminal(ctx context.Context, p sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	return sdk.KillTerminalResponse{}, sdk.NewMethodNotFound("terminal/kill")
}

func (s *Session) TerminalOutput(ctx context.Context, p sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	return sdk.TerminalOutputResponse{}, sdk.NewMethodNotFound("terminal/output")
}

func (s *Session) ReleaseTerminal(ctx context.Context, p sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	return sdk.ReleaseTerminalResponse{}, sdk.NewMethodNotFound("terminal/release")
}

func (s *Session) WaitForTerminalExit(ctx context.Context, p sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	return sdk.WaitForTerminalExitResponse{}, sdk.NewMethodNotFound("terminal/wait_for_exit")
}

// --- turn lifecycle ---

// reserveTurn atomically marks a turn in flight, resetting per-turn state. It
// returns false if a turn is already active (one turn at a time).
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
	s.curMsgID = ""
	s.lastError = ""
	s.assistant.Reset()
	return ctx, true
}

// abortTurn releases a reservation whose prompt was never fired (e.g. the user
// turn could not be recorded, so we refuse the send).
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

func (s *Session) runPrompt(ctx context.Context, text string) {
	// On cancellation the SDK sends session/cancel to the agent for us
	// (Prompt's ctx.Err() path), so the wire side of an interrupt is covered.
	resp, err := s.conn.Prompt(ctx, sdk.PromptRequest{
		SessionId: s.sessionID,
		Prompt:    []sdk.ContentBlock{sdk.TextBlock(text)},
	})
	if errors.Is(err, context.Canceled) {
		err = errors.New("turn interrupted by supervisor")
	}
	s.endTurn(resp, err)
}

func (s *Session) endTurn(resp sdk.PromptResponse, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushAssistantLocked()
	// Context occupancy for the gauge: opencode streams usage_update with
	// used/size; codex uses its own event; Grok lands here at turn end via
	// PromptResponse._meta. usageFromPrompt splits Grok's two layers
	// (fare-design §2.5 / D10): Layer A last-call → Used, nested Layer B →
	// spend breakdown/cost; Size from ctxSize captured at initialize.
	//
	// opencode (D5): two-shape pairing — shell feeds Used/Size, breakdown
	// feeds fare. Shell occupancy was already written for the live gauge;
	// the turn-end record must not overwrite Used with totalTokens.
	shellUsed, shellSize := s.shellUsed, s.shellSize
	s.shellUsed, s.shellSize = 0, 0
	if ev := usageFromPrompt(resp, s.ctxSize); ev != nil {
		if s.agent == "opencode" {
			ev = applyOpencodeUsagePairing(ev, shellUsed, shellSize)
		}
		if ev != nil {
			s.appendLocked(Event{T: "usage", Usage: ev})
		}
	}
	switch {
	case err != nil:
		s.lastError = err.Error()
		s.appendLocked(Event{T: "error", Error: err.Error()})
	default:
		s.appendLocked(Event{T: "stop", StopReason: string(resp.StopReason)})
		// A turn that ended without any text or tool output is a failed/empty
		// turn (observed: a model failure can end_turn silently). A persistence
		// failure (appendLocked already set lastError) also lands here because
		// turnHadOutput is only claimed for records that reached the log, so
		// keep the earlier, more specific error rather than masking it.
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
	s.curMsgID = ""
	s.turnHadOutput = false
}

// appendAssistantLocked accumulates streamed agent text, flushing at a message
// boundary. Callers hold s.mu.
func (s *Session) appendAssistantLocked(text string, msgID *string) {
	if text == "" {
		return
	}
	id := ""
	if msgID != nil {
		id = *msgID
	}
	if s.assistant.Len() > 0 && id != s.curMsgID {
		s.flushAssistantLocked()
	}
	s.curMsgID = id
	// pi emits a static startup banner as its first agent text; strip only an
	// exact match of the session's advertised banner so real content is never
	// dropped (adapter-specific, best-effort — see captureBanner).
	if !s.bannerDone {
		s.bannerDone = true
		if s.banner != "" && strings.TrimSpace(text) == strings.TrimSpace(s.banner) {
			return
		}
	}
	s.assistant.WriteString(text)
}

func (s *Session) flushAssistantLocked() {
	if s.assistant.Len() == 0 {
		return
	}
	txt := s.assistant.String()
	s.assistant.Reset()
	if s.appendLocked(Event{T: "assistant", Text: txt}) {
		s.turnHadOutput = true
	}
}

// setRetired fences this session's writer off the shared log (or lifts the
// fence again when a /clear fails after retiring).
func (s *Session) setRetired(v bool) {
	s.mu.Lock()
	s.retired = v
	s.mu.Unlock()
}

// appendLocked persists one event and folds a write failure into the session's
// visible error state (finding 51). Callers hold s.mu. It returns true only
// when the record actually reached the log: the ACP log is the authoritative
// history, so callers must not claim turnHadOutput for an event that did not
// persist, and a silent write failure must not let the agent produce output
// that is neither stored nor surfaced. The first failure wins so the chat view
// shows the original cause.
func (s *Session) appendLocked(ev Event) bool {
	if s.retired {
		// A /clear turned the page: the shared log's tail is now the fresh
		// segment, and a straggler from the dying process (a late usage or
		// tool event, a duplicate-permission error) would land after the seam
		// and poison the new chat surface — most concretely its context
		// gauge. Drop it visibly on stderr instead (finding 91).
		fmt.Fprintf(os.Stderr, "scimux/acp: dropping %s from retired session %s\n", ev.T, s.nodeID)
		return false
	}
	if err := s.logw.Append(ev); err != nil {
		if s.lastError == "" {
			s.lastError = "session log write failed: " + err.Error()
		}
		fmt.Fprintf(os.Stderr, "scimux/acp: persisting %s for %s failed: %v\n", ev.T, s.nodeID, err)
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

// --- permission answering ---

// prepareResolve maps a whitelisted key to the pending option and returns an
// opaque token naming incarnation, request sequence, and option id, plus audit
// evidence, without delivering. expectedRequestID is compared to the current
// pending request under the same lock that selects the option, so a key
// evaluated against request A cannot map against a replacement B. The token
// still pins deliver to this exact request and session incarnation.
func (s *Session) prepareResolve(expectedRequestID, key string) (sdk.PermissionOptionId, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return "", "", ErrNoPending
	}
	if s.incarn == "" {
		s.incarn = newSessionIncarn()
	}
	curID := formatRequestID(s.incarn, s.pending.seq)
	if expectedRequestID == "" || expectedRequestID != curID {
		return "", "", ErrStalePermission
	}
	id, ok := mapKeyToOption(key, s.pending.options)
	if !ok {
		return "", "", fmt.Errorf("key %q maps to no permission option", key)
	}
	// Token format: "<incarn>:<seq>:<optionId>" — incarnation fences /clear
	// and kill/relaunch; seq fences in-session replacements that reuse option ids.
	tok := sdk.PermissionOptionId(fmt.Sprintf("%s:%d:%s", s.incarn, s.pending.seq, id))
	return tok, "permission: " + s.pending.toolTitle, nil
}

// deliver hands the mapped option to the blocked RequestPermission goroutine.
// It holds s.mu across the check-and-send and consumes the pending request
// (pending = nil) atomically, closing the finding-53 race where a copy of
// pending could be sent into a stale channel after clearPending had run. The
// token's incarnation and sequence must still name the current pending request.
func (s *Session) deliver(tok sdk.PermissionOptionId) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending
	if p == nil {
		return ErrNoPending
	}
	incarn, seq, optID, ok := parseDeliverToken(string(tok))
	if !ok || incarn != s.incarn || seq != p.seq {
		return errors.New("pending permission changed before the answer was delivered")
	}
	valid := false
	for _, o := range p.options {
		if o.OptionId == optID {
			valid = true
			break
		}
	}
	if !valid {
		return errors.New("pending permission changed before the answer was delivered")
	}
	s.pending = nil // consume; RequestPermission's deferred clearPending is now a no-op
	select {
	case p.ch <- optID: // buffered (cap 1) and empty: completes without blocking
		return nil
	default:
		return errors.New("permission request was already answered")
	}
}

// parseDeliverToken splits a prepareResolve token into (incarn, seq, optionId).
// Accepts only the "<incarn>:<seq>:<optionId>" form; option ids may contain colons.
func parseDeliverToken(tok string) (incarn string, seq uint64, optID sdk.PermissionOptionId, ok bool) {
	i := strings.IndexByte(tok, ':')
	if i <= 0 {
		return "", 0, "", false
	}
	rest := tok[i+1:]
	j := strings.IndexByte(rest, ':')
	if j <= 0 || j == len(rest)-1 {
		return "", 0, "", false
	}
	n, err := strconv.ParseUint(rest[:j], 10, 64)
	if err != nil {
		return "", 0, "", false
	}
	return tok[:i], n, sdk.PermissionOptionId(rest[j+1:]), true
}

func (s *Session) pendingInfo() (PendingPermission, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return PendingPermission{}, false
	}
	if s.incarn == "" {
		s.incarn = newSessionIncarn()
	}
	opts := make([]PermOption, 0, len(s.pending.options))
	for i, o := range s.pending.options {
		opts = append(opts, PermOption{
			Key:  strconv.Itoa(i + 1),
			Name: o.Name,
			Kind: mapOptionKind(o.Kind),
		})
	}
	return PendingPermission{
		RequestID: formatRequestID(s.incarn, s.pending.seq),
		Title:     s.pending.toolTitle,
		ToolKind:  s.pending.toolKind,
		Options:   opts,
	}, true
}

// mapOptionKind projects an SDK permission-option kind into scimux vocabulary.
// Unknown/empty values stay "" (defensive-parsing contract).
func mapOptionKind(k sdk.PermissionOptionKind) string {
	switch k {
	case sdk.PermissionOptionKindAllowOnce:
		return "allow"
	case sdk.PermissionOptionKindAllowAlways:
		return "allow_always"
	case sdk.PermissionOptionKindRejectOnce:
		return "reject"
	case sdk.PermissionOptionKindRejectAlways:
		return "reject_always"
	default:
		return ""
	}
}

func (s *Session) clearPending() {
	s.mu.Lock()
	s.pending = nil
	s.mu.Unlock()
}

// mapKeyToOption resolves a whitelisted dialog key to a permission option:
// a digit selects the Nth option; y/Enter picks the first allow option (or the
// first option); n/Escape picks the first reject option (or the last).
func mapKeyToOption(key string, options []sdk.PermissionOption) (sdk.PermissionOptionId, bool) {
	if len(options) == 0 {
		return "", false
	}
	if n, err := strconv.Atoi(key); err == nil {
		if n >= 1 && n <= len(options) {
			return options[n-1].OptionId, true
		}
		return "", false
	}
	switch key {
	case "y", "Enter":
		for _, o := range options {
			if strings.HasPrefix(string(o.Kind), "allow") {
				return o.OptionId, true
			}
		}
		return options[0].OptionId, true
	case "n", "Escape":
		for _, o := range options {
			if strings.HasPrefix(string(o.Kind), "reject") {
				return o.OptionId, true
			}
		}
		return options[len(options)-1].OptionId, true
	}
	return "", false
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
	if s.pending != nil {
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

// stop tears a session down in order: cancel an in-flight turn, close the ACP
// session, unblock any pending permission (→ cancelled), then kill the group.
func (s *Session) stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.conn.Cancel(ctx, sdk.CancelNotification{SessionId: s.sessionID})
	_, _ = s.conn.CloseSession(ctx, sdk.CloseSessionRequest{SessionId: s.sessionID})
	s.closeDone()
	return s.proc.Kill()
}

// --- adapters ---

// captureBanner records pi's startup banner (advertised in the new-session
// _meta) so appendAssistantLocked can strip it. Best-effort and pi-specific.
func (s *Session) captureBanner(resp sdk.NewSessionResponse) {
	if s.agent != "pi" || resp.Meta == nil {
		return
	}
	if si, ok := resp.Meta["startupInfo"]; ok {
		if str, ok := si.(string); ok {
			s.banner = str
		}
	}
}

// usageFromPrompt builds a sessionlog usage record from a PromptResponse.
// Prefers the unstable top-level Usage field (opencode-style); falls back to
// Grok-style dual-layer _meta (usageFromMeta). Standard Usage still maps
// TotalTokens → Used via fillUsageOccupancy; Grok meta does not — see D10.
// Empty {} Usage is dropped (nil). Returns nil when nothing usable is present.
//
// Deliberately does not map Usage.CachedWriteTokens: opencode's ACP path has
// no cache-write (fare-design D5 — baseline is 3-quantity fare; Phase 5b
// SQLite enrichment for cache-write/cost is paused and must not be re-opened
// here).
func usageFromPrompt(resp sdk.PromptResponse, ctxSize int) *UsageEvent {
	if resp.Usage != nil {
		u := resp.Usage
		ev := &UsageEvent{
			InputTokens:  u.InputTokens,
			OutputTokens: u.OutputTokens,
			TotalTokens:  u.TotalTokens,
		}
		if u.CachedReadTokens != nil {
			ev.CachedReadTokens = *u.CachedReadTokens
		}
		// Drop empty {} — no billable fields at all.
		if ev.InputTokens == 0 && ev.OutputTokens == 0 &&
			ev.CachedReadTokens == 0 && ev.TotalTokens == 0 {
			return nil
		}
		fillUsageOccupancy(ev, ctxSize)
		return ev
	}
	return usageFromMeta(resp.Meta, ctxSize)
}

// applyOpencodeUsagePairing implements fare-design D5 / Phase 5 for the
// opencode ACP path (Tier B→B+, zero dependency).
//
// opencode emits two shapes per turn (§2.1): a streamed occupancy shell
// {used,size,costCurrency} and a turn-end breakdown
// {inputTokens,outputTokens,cachedReadTokens,totalTokens}. Pair them so:
//   - Used/Size (tank) come from the shell, never from totalTokens
//   - fare quantities come from the breakdown only
//   - never double-count (shell has no breakdown fields; this record is the
//     single fare hit for the turn)
//   - no cache-write and no cost amount are invented (absent, not zero-as-real)
//
// On unexpected/missing breakdown the caller already left the shell in the
// log (occupancy-only degrade); we never error. SQLite / opencode.db is out
// of scope here — that is the paused Phase 5b enrichment, not correctness.
func applyOpencodeUsagePairing(breakdown *UsageEvent, shellUsed, shellSize int) *UsageEvent {
	if breakdown == nil {
		return nil
	}
	// Never invent cache-write or reported cost on the ACP baseline.
	breakdown.CacheCreationTokens = 0
	breakdown.CostAmount = 0
	// CostCurrency alone is not a fabricated dollar figure; leave empty on
	// the breakdown so the fare hit is clearly "no cost reported".
	breakdown.CostCurrency = ""

	// Tank from shell when present. totalTokens is a spend sum, not occupancy;
	// fillUsageOccupancy would otherwise paint Used = totalTokens.
	if shellUsed > 0 {
		breakdown.Used = shellUsed
	}
	if shellSize > 0 {
		breakdown.Size = shellSize
	}
	return breakdown
}

// usageFromMeta reads Grok's PromptResponse._meta two-layer shape
// (fare-design §2.5 / D10):
//
//	Layer A (top-level): totalTokens / inputTokens / outputTokens
//	  → last model call of this prompt → gauge Used (occupancy tank)
//	Layer B (nested usage{}): same fields + costUsdTicks + modelCalls
//	  → per-prompt spend summed over internal rounds → breakdown + cost
//	  (fare meter telemetry; never drives Used)
//
// Size comes from ctxSize (initialize totalContextTokens). Used is set only
// from top-level; if top-level is empty we do not fall back to nested for
// Used. If Used > Size > 0 the occupancy is cleared (never paint full).
// Nested totals are never routed through fillUsageOccupancy.
func usageFromMeta(meta map[string]any, ctxSize int) *UsageEvent {
	if len(meta) == 0 {
		return nil
	}
	ev := &UsageEvent{}
	hasNested := false
	if raw, ok := meta["usage"]; ok {
		if m, ok := raw.(map[string]any); ok {
			// Layer B — spend telemetry / future fare source.
			ev.InputTokens = intFromAny(m["inputTokens"])
			ev.OutputTokens = intFromAny(m["outputTokens"])
			ev.TotalTokens = intFromAny(m["totalTokens"])
			ev.CachedReadTokens = intFromAny(m["cachedReadTokens"])
			if amount := floatFromAny(m["costAmount"]); amount > 0 {
				ev.CostAmount = amount
				ev.CostCurrency = "USD"
			}
			// costUsdTicks is 1e-9 USD units observed on grok agent stdio.
			if ticks := floatFromAny(m["costUsdTicks"]); ticks > 0 && ev.CostAmount == 0 {
				ev.CostAmount = ticks / 1e9
				ev.CostCurrency = "USD"
			}
			if ev.TotalTokens > 0 || ev.InputTokens > 0 || ev.OutputTokens > 0 {
				hasNested = true
			}
		}
	}

	// Layer A — last-call occupancy for the context gauge.
	topTotal := intFromAny(meta["totalTokens"])
	topIn := intFromAny(meta["inputTokens"])
	topOut := intFromAny(meta["outputTokens"])
	if topTotal > 0 {
		ev.Used = topTotal
	} else if n := topIn + topOut; n > 0 {
		ev.Used = n
	}
	// When nested Layer B is absent, top-level fields also fill breakdown
	// (top-only meta shapes). Never overwrite nested spend with last-call.
	if !hasNested {
		if ev.TotalTokens == 0 {
			ev.TotalTokens = topTotal
		}
		if ev.InputTokens == 0 {
			ev.InputTokens = topIn
		}
		if ev.OutputTokens == 0 {
			ev.OutputTokens = topOut
		}
		if ev.CachedReadTokens == 0 {
			ev.CachedReadTokens = intFromAny(meta["cachedReadTokens"])
		}
	}

	// Phase 6 identity (D3/D6, O4): additive only — do not touch Layer A/B split.
	// modelId is per-turn on the wire; requestId (==promptId) is the turn id.
	// Absent → empty (ReadFare seam fallback / no PerModel key); never error.
	if s, ok := meta["modelId"].(string); ok {
		ev.Model = s
	}
	if s, ok := meta["requestId"].(string); ok {
		ev.TurnID = s
	}

	if ev.Used == 0 && ev.TotalTokens == 0 && ev.InputTokens == 0 && ev.OutputTokens == 0 {
		return nil
	}
	if ctxSize > 0 {
		ev.Size = ctxSize
	}
	// Nonsense occupancy must not paint the gauge full (D10).
	if ev.Used > 0 && ev.Size > 0 && ev.Used > ev.Size {
		ev.Used = 0
	}
	return ev
}

// fillUsageOccupancy maps token totals onto the Used/Size fields the context
// gauge reads (LatestUsage / segment only look at Used and Size). Used for
// the standard PromptResponse.Usage path and usage_update-shaped records —
// not for Grok dual-layer _meta (usageFromMeta handles that split itself).
func fillUsageOccupancy(ev *UsageEvent, ctxSize int) {
	if ev.Used == 0 {
		if ev.TotalTokens > 0 {
			ev.Used = ev.TotalTokens
		} else if n := ev.InputTokens + ev.OutputTokens; n > 0 {
			ev.Used = n
		}
	}
	if ev.Size == 0 && ctxSize > 0 {
		ev.Size = ctxSize
	}
}

// contextSizeFromMeta pulls totalContextTokens out of an initialize or
// new-session _meta payload. Grok advertises modelState.availableModels[].
// _meta.totalContextTokens (and a currentModelId); other agents may omit it.
func contextSizeFromMeta(meta map[string]any, model string) int {
	if len(meta) == 0 {
		return 0
	}
	// Direct field (defensive — some agents may put it at the top level).
	if n := intFromAny(meta["totalContextTokens"]); n > 0 {
		return n
	}
	ms, _ := meta["modelState"].(map[string]any)
	if ms == nil {
		return 0
	}
	current := model
	if current == "" {
		if s, ok := ms["currentModelId"].(string); ok {
			current = s
		}
	}
	models, _ := ms["availableModels"].([]any)
	var fallback int
	for _, raw := range models {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		id, _ := m["modelId"].(string)
		mm, _ := m["_meta"].(map[string]any)
		n := intFromAny(m["totalContextTokens"])
		if n == 0 && mm != nil {
			n = intFromAny(mm["totalContextTokens"])
		}
		if n <= 0 {
			continue
		}
		if id == current && current != "" {
			return n
		}
		if fallback == 0 {
			fallback = n
		}
	}
	return fallback
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func floatFromAny(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// applyEffort maps a scimux effort level onto the agent's own control surface:
// pi exposes effort as a session mode; opencode as a select config option.
// Best-effort and tolerant — a mismatch leaves the agent's default standing.
func (s *Session) applyEffort(ctx context.Context, effort string, resp sdk.NewSessionResponse) {
	if effort == "" {
		return
	}
	if resp.Modes != nil {
		for _, mode := range resp.Modes.AvailableModes {
			if strings.EqualFold(string(mode.Id), effort) || strings.EqualFold(mode.Name, effort) {
				_, _ = s.conn.SetSessionMode(ctx, sdk.SetSessionModeRequest{SessionId: s.sessionID, ModeId: mode.Id})
				return
			}
		}
	}
	for _, opt := range resp.ConfigOptions {
		sel := opt.Select
		if sel == nil {
			continue
		}
		if !strings.Contains(strings.ToLower(string(sel.Id)), "effort") &&
			!strings.Contains(strings.ToLower(sel.Name), "effort") {
			continue
		}
		if val, ok := matchSelectValue(sel, effort); ok {
			_, _ = s.conn.SetSessionConfigOption(ctx, sdk.SetSessionConfigOptionRequest{
				ValueId: &sdk.SetSessionConfigOptionValueId{ConfigId: sel.Id, SessionId: s.sessionID, Value: val},
			})
			return
		}
	}
}

func matchSelectValue(sel *sdk.SessionConfigOptionSelect, effort string) (sdk.SessionConfigValueId, bool) {
	var opts []sdk.SessionConfigSelectOption
	if sel.Options.Ungrouped != nil {
		opts = append(opts, (*sel.Options.Ungrouped)...)
	}
	if sel.Options.Grouped != nil {
		for _, g := range *sel.Options.Grouped {
			opts = append(opts, g.Options...)
		}
	}
	for _, o := range opts {
		if strings.EqualFold(string(o.Value), effort) || strings.EqualFold(o.Name, effort) {
			return o.Value, true
		}
	}
	return "", false
}

// blockText extracts plain text from a content block (the only variant we
// render into the chat log).
func blockText(c sdk.ContentBlock) string {
	if c.Text != nil {
		return c.Text.Text
	}
	return ""
}
