// Package acp is scimux's second transport: instead of driving pi and
// opencode through a tmux-wrapped TUI and photographing the pane, it speaks
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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"codeberg.org/chrberger/scimux/internal/transcript"
)

// Errors returned to the HTTP layer so it can pick a status code.
var (
	ErrNoSession  = errors.New("no live ACP session for node")
	ErrNotAlive   = errors.New("ACP subprocess has exited")
	ErrNoPending  = errors.New("no permission request is pending")
	ErrTurnActive = errors.New("a turn is already in flight")
)

// launchTimeout bounds initialize + new-session so a hung agent (e.g. one
// blocked on an auth prompt it never surfaces) can't wedge createNode.
const launchTimeout = 30 * time.Second

// Manager owns every node's ACP subprocess and connection. It is the seam the
// HTTP handlers talk to; the tmux Server has the analogous role for TUI nodes.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	runner   Runner
	logDir   string
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

func (m *Manager) logPath(nodeID string) string {
	return filepath.Join(m.logDir, nodeID+".jsonl")
}

func (m *Manager) session(nodeID string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[nodeID]
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
	proc, err := m.runner(nodeID, agent, dir)
	if err != nil {
		return "", err
	}
	s := &Session{
		nodeID:    nodeID,
		agent:     agent,
		proc:      proc,
		logw:      &logWriter{path: m.logPath(nodeID)},
		procAlive: true,
		done:      make(chan struct{}),
	}
	s.conn = sdk.NewClientSideConnection(s, proc.Stdin(), proc.Stdout())

	ctx, cancel := context.WithTimeout(context.Background(), launchTimeout)
	defer cancel()
	// Be explicit about capabilities: we decline fs and terminal (plan §2).
	if _, err := s.conn.Initialize(ctx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		ClientCapabilities: sdk.ClientCapabilities{
			Fs:       sdk.FileSystemCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: false,
		},
	}); err != nil {
		proc.Kill()
		return "", fmt.Errorf("acp initialize: %w", err)
	}
	resp, err := s.conn.NewSession(ctx, sdk.NewSessionRequest{Cwd: dir, McpServers: []sdk.McpServer{}})
	if err != nil {
		proc.Kill()
		return "", fmt.Errorf("acp new session: %w", err)
	}
	s.sessionID = resp.SessionId
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
	if !s.reserveTurn() {
		return ErrTurnActive
	}
	if err := s.logw.append(Event{T: "user", Text: text}); err != nil {
		s.abortTurn()
		return fmt.Errorf("record user turn: %w", err)
	}
	go s.runPrompt(text)
	return nil
}

// Resolve answers a pending permission request by mapping a whitelisted key to
// an option. Returns evidence (the tool title) for the audit record.
func (m *Manager) Resolve(nodeID, key string) (string, error) {
	s := m.session(nodeID)
	if s == nil {
		return "", ErrNoSession
	}
	return s.resolvePermission(key)
}

// Pending reports the outstanding permission request, if any, so the UI can
// render answer keys and the audit trail can capture the decision context.
func (m *Manager) Pending(nodeID string) (title string, opts []PermOption, ok bool) {
	s := m.session(nodeID)
	if s == nil {
		return "", nil, false
	}
	return s.pendingInfo()
}

// PermOption is one answerable permission choice: the key a supervisor presses
// and the agent's human-readable label for it.
type PermOption struct {
	Key  string `json:"key"`
	Name string `json:"name"`
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

	mu            sync.Mutex
	procAlive     bool
	turnActive    bool
	turnHadOutput bool
	curMsgID      string
	assistant     strings.Builder
	pending       *pendingPermission
	lastError     string
	banner        string
	bannerDone    bool

	done     chan struct{} // closed on exit/stop; unblocks a pending permission
	doneOnce sync.Once
}

// pendingPermission is the one outstanding permission request (ACP is one turn
// at a time, so at most one). RequestPermission blocks on ch until a key
// resolves it, or done closes (cancel/shutdown → cancelled outcome).
type pendingPermission struct {
	toolTitle string
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
		s.turnHadOutput = true
		_ = s.logw.append(Event{T: "tool", Tool: &ToolEvent{
			ID:       string(u.ToolCall.ToolCallId),
			Title:    u.ToolCall.Title,
			Kind:     string(u.ToolCall.Kind),
			Status:   string(u.ToolCall.Status),
			RawInput: u.ToolCall.RawInput,
		}})
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
		s.turnHadOutput = true
		_ = s.logw.append(Event{T: "tool", Tool: ev})
	case u.UsageUpdate != nil:
		uu := u.UsageUpdate
		ev := &UsageEvent{Used: uu.Used, Size: uu.Size}
		if uu.Cost != nil {
			ev.CostAmount = uu.Cost.Amount
			ev.CostCurrency = uu.Cost.Currency
		}
		_ = s.logw.append(Event{T: "usage", Usage: ev})
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
	ch := make(chan sdk.PermissionOptionId, 1)
	s.mu.Lock()
	if s.pending != nil {
		// One pending at a time; a second is an agent error → cancel it.
		s.mu.Unlock()
		_ = s.logw.append(Event{T: "error", Error: "duplicate permission request ignored"})
		return cancelledPermission(), nil
	}
	s.pending = &pendingPermission{toolTitle: title, options: p.Options, ch: ch}
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
func (s *Session) reserveTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnActive {
		return false
	}
	s.turnActive = true
	s.turnHadOutput = false
	s.curMsgID = ""
	s.lastError = ""
	s.assistant.Reset()
	return true
}

// abortTurn releases a reservation whose prompt was never fired (e.g. the user
// turn could not be recorded, so we refuse the send).
func (s *Session) abortTurn() {
	s.mu.Lock()
	s.turnActive = false
	s.mu.Unlock()
}

func (s *Session) runPrompt(text string) {
	resp, err := s.conn.Prompt(context.Background(), sdk.PromptRequest{
		SessionId: s.sessionID,
		Prompt:    []sdk.ContentBlock{sdk.TextBlock(text)},
	})
	s.endTurn(resp, err)
}

func (s *Session) endTurn(resp sdk.PromptResponse, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushAssistantLocked()
	if resp.Usage != nil {
		u := resp.Usage
		ev := &UsageEvent{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, TotalTokens: u.TotalTokens}
		if u.CachedReadTokens != nil {
			ev.CachedReadTokens = *u.CachedReadTokens
		}
		_ = s.logw.append(Event{T: "usage", Usage: ev})
	}
	switch {
	case err != nil:
		s.lastError = err.Error()
		_ = s.logw.append(Event{T: "error", Error: err.Error()})
	default:
		_ = s.logw.append(Event{T: "stop", StopReason: string(resp.StopReason)})
		// A turn that ended without any text or tool output is a failed/empty
		// turn (observed: a model failure can end_turn silently). Surface it.
		if !s.turnHadOutput {
			s.lastError = "agent produced no output this turn"
			_ = s.logw.append(Event{T: "error", Error: s.lastError})
		}
	}
	s.turnActive = false
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
	s.turnHadOutput = true
	_ = s.logw.append(Event{T: "assistant", Text: txt})
}

// --- permission answering ---

func (s *Session) resolvePermission(key string) (string, error) {
	s.mu.Lock()
	p := s.pending
	s.mu.Unlock()
	if p == nil {
		return "", ErrNoPending
	}
	id, ok := mapKeyToOption(key, p.options)
	if !ok {
		return "", fmt.Errorf("key %q maps to no permission option", key)
	}
	select {
	case p.ch <- id:
	default: // already answered/cancelled; harmless
	}
	return "permission: " + p.toolTitle, nil
}

func (s *Session) pendingInfo() (string, []PermOption, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return "", nil, false
	}
	opts := make([]PermOption, 0, len(s.pending.options))
	for i, o := range s.pending.options {
		opts = append(opts, PermOption{Key: strconv.Itoa(i + 1), Name: o.Name})
	}
	return s.pending.toolTitle, opts, true
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
