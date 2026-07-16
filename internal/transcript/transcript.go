// Package transcript extracts chat turns from the session logs that agent
// CLIs write to disk as they run: Claude Code under
// ~/.claude/projects/<escaped-cwd>/<session-id>.jsonl and Codex under
// ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl.
//
// These formats are undocumented internals of the respective CLIs and may
// change between releases, so parsing is defensive by contract: lines with a
// recognized shape yield turns, everything else is silently ignored, and an
// unreadable file yields no turns rather than an error. Callers degrade to
// the raw tmux pane snapshot when this package returns nothing useful.
package transcript

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Turn is one visible chat message.
type Turn struct {
	Role string `json:"role"` // "user" or "assistant"
	Text string `json:"text"`
	Time string `json:"time,omitempty"` // raw timestamp string as logged, if any
}

// ParseLine extracts a turn from one JSONL line of either format.
// ok is false for anything that is not a visible chat message: tool calls,
// tool results, reasoning items, meta records, or unparseable input.
func ParseLine(line []byte) (Turn, bool) {
	var generic struct {
		Type      string          `json:"type"`
		Timestamp string          `json:"timestamp"`
		Message   json.RawMessage `json:"message"`
		Payload   json.RawMessage `json:"payload"`
		IsMeta    bool            `json:"isMeta"`
	}
	if err := json.Unmarshal(line, &generic); err != nil {
		return Turn{}, false
	}
	switch generic.Type {
	case "user", "assistant": // Claude Code session log
		if generic.IsMeta || len(generic.Message) == 0 {
			return Turn{}, false
		}
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(generic.Message, &msg); err != nil {
			return Turn{}, false
		}
		role := msg.Role
		if role == "" {
			role = generic.Type
		}
		return makeTurn(role, contentText(msg.Content), generic.Timestamp)
	case "response_item": // Codex rollout log
		var payload struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(generic.Payload, &payload); err != nil || payload.Type != "message" {
			return Turn{}, false
		}
		if payload.Role != "user" && payload.Role != "assistant" {
			return Turn{}, false
		}
		return makeTurn(payload.Role, contentText(payload.Content), generic.Timestamp)
	}
	return Turn{}, false
}

func makeTurn(role, text, ts string) (Turn, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Turn{}, false
	}
	// Injected scaffolding (<user_instructions>, <environment_context>,
	// <command-name>, …) appears as user messages in both formats; real
	// prompts are exceedingly unlikely to begin with '<'.
	if role == "user" && strings.HasPrefix(text, "<") {
		return Turn{}, false
	}
	return Turn{Role: role, Text: text, Time: ts}, true
}

// contentText accepts both a plain string and a list of content blocks,
// concatenating the text-bearing, non-tool blocks (Claude "text", Codex
// "input_text"/"output_text", and future types that carry a "text" field).
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != "" && !strings.HasPrefix(b.Type, "tool") {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// pendingCall is a tool/exec call the agent has logged whose result has not
// been logged yet. Both CLIs write the call record the moment the agent asks
// for it — verified empirically: an approval-gated call sits unresolved in
// the file for exactly as long as the human takes to answer. An unresolved
// call plus a mechanically quiet pane is therefore the signature of "agent
// is awaiting user input", with no TUI string matching involved.
type pendingCall struct{ id, name string }

// Tailer incrementally reads one transcript file, remembering its byte
// offset between polls and buffering partial trailing lines. It is safe for
// concurrent use (the poller and the chat handler both poll it).
type Tailer struct {
	Path    string
	Turns   []Turn
	mu      sync.Mutex
	offset  int64
	buf     []byte
	pending []pendingCall
	// unknownStreak counts consecutive appended lines that are not even
	// structurally recognizable (valid JSON object with a "type"). Unknown
	// *typed* records are normal and never counted — this catches a file
	// that stopped being this transcript format at all (wrong file linked,
	// corruption, a non-JSONL future format), even after earlier valid turns.
	unknownStreak int
	// progress counts agent-side records ever seen (see agentShaped). The
	// caller compares it, together with the byte offset, across pane
	// activity cycles: bytes that advance while this count stands still mean
	// the file keeps growing but no longer carries an interpretable agent
	// response — the typed-format-change case unknownStreak cannot see.
	progress int
	// ctxUsed/ctxWindow track the latest context-window usage the transcript
	// reports (Claude assistant usage blocks, Codex token_count events).
	// Purely informational — never part of health or progress signals.
	ctxUsed   int64
	ctxWindow int64
}

// unparseableThreshold is how many consecutive structurally-unrecognizable
// lines mark a transcript as no longer making sense.
const unparseableThreshold = 10

// Unparseable reports that the transcript's recent appended data cannot be
// interpreted — the signal for degrading the UI to the pane snapshot even
// when older turns were parsed fine.
func (t *Tailer) Unparseable() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.unknownStreak >= unparseableThreshold
}

// recognizedLine is deliberately lenient: any JSON object carrying a type
// string counts as "still this format", so benign new record types never
// trip the health signal.
func recognizedLine(line []byte) bool {
	var g struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(line, &g) == nil && g.Type != ""
}

// agentShaped reports whether a line is an agent-side record whose inner
// shape this parser actually understands: a visible assistant message it can
// render text from, or a tool call/result it can track — not merely a record
// with the right outer type. This is the chat-progress signal, and it is
// deliberately directional: a user prompt, a meta record, or injected
// scaffolding must not vouch for the health of the *response* side of the
// transcript, because a normal phase logs the user record first — counting
// it would mask an assistant format that changed out from under the parser.
// The signal must never claim more understanding than the parser has, so
// text extraction goes through the same contentText that ParseLine renders
// with and tool records must carry the fields the pending-call tracking
// reads. Benign side records (titles, token counts, queue operations) are
// simply not progress; they never mark anything unhealthy on their own.
func agentShaped(line []byte) bool {
	var g struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
		Payload json.RawMessage `json:"payload"`
		IsMeta  bool            `json:"isMeta"`
	}
	if json.Unmarshal(line, &g) != nil || g.IsMeta {
		return false
	}
	switch g.Type {
	case "assistant": // Claude: visible text or a trackable tool call
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(g.Message, &msg) != nil {
			return false
		}
		if strings.TrimSpace(contentText(msg.Content)) != "" {
			return true
		}
		var blocks []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			return false
		}
		for _, b := range blocks {
			if b.Type == "tool_use" && b.Name != "" {
				return true
			}
		}
		return false
	case "user": // Claude: a tool result is agent-side work; a prompt is not
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(g.Message, &msg) != nil {
			return false
		}
		var blocks []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
		}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			return false
		}
		for _, b := range blocks {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				return true
			}
		}
		return false
	case "response_item": // Codex
		var p struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Name    string          `json:"name"`
			CallID  string          `json:"call_id"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(g.Payload, &p) != nil {
			return false
		}
		switch p.Type {
		case "function_call", "custom_tool_call":
			return p.Name != ""
		case "function_call_output", "custom_tool_call_output":
			return p.CallID != ""
		case "message":
			// User messages — real prompts and <environment_context>-style
			// scaffolding alike — say nothing about the response format.
			return p.Role == "assistant" && strings.TrimSpace(contentText(p.Content)) != ""
		}
	}
	return false
}

// Progress reports how far the tailer has consumed the file — the committed
// watermark behind any partial trailing line still buffered, since a
// half-written record is not data the parser failed on — and how many
// agent-side records it has seen in total. Both values only grow (except on
// file rotation, which resets the tailer).
func (t *Tailer) Progress() (offset int64, agentRecords int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.offset - int64(len(t.buf)), t.progress
}

// Usage reports the latest context-window usage the transcript recorded:
// tokens occupied and the model's window size. Either value is 0 when the
// transcript has not (or never) reported it — Claude logs per-turn usage but
// no window size, Codex logs both.
func (t *Tailer) Usage() (used, window int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ctxUsed, t.ctxWindow
}

// noteUsage extracts context usage from one JSONL line. Defensive like all
// other parsing: unknown shapes leave the previous values standing.
func (t *Tailer) noteUsage(line []byte) {
	var g struct {
		Type    string `json:"type"`
		Message struct {
			Usage struct {
				InputTokens              int64 `json:"input_tokens"`
				CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
				OutputTokens             int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Payload struct {
			Type string `json:"type"`
			Info struct {
				ModelContextWindow int64 `json:"model_context_window"`
				LastTokenUsage     struct {
					InputTokens       int64 `json:"input_tokens"`
					CachedInputTokens int64 `json:"cached_input_tokens"`
					OutputTokens      int64 `json:"output_tokens"`
				} `json:"last_token_usage"`
			} `json:"info"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &g) != nil {
		return
	}
	switch g.Type {
	case "assistant": // Claude: usage rides on every assistant record
		u := g.Message.Usage
		// Context occupancy after this turn: everything the request carried
		// in (fresh + cached) plus what the model produced.
		if used := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens + u.OutputTokens; used > 0 {
			t.ctxUsed = used
		}
	case "event_msg": // Codex: explicit token_count events
		if g.Payload.Type != "token_count" {
			return
		}
		u := g.Payload.Info.LastTokenUsage
		if used := u.InputTokens + u.CachedInputTokens + u.OutputTokens; used > 0 {
			t.ctxUsed = used
		}
		if g.Payload.Info.ModelContextWindow > 0 {
			t.ctxWindow = g.Payload.Info.ModelContextWindow
		}
	}
}

// PendingCount reports how many logged tool calls still lack a result.
func (t *Tailer) PendingCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}

// WaitingOn reports the most recent tool call without a logged result — the
// transcript half of needs-input detection. The caller must combine it with
// pane quietness: a pending call under an actively changing pane is just a
// long-running tool.
func (t *Tailer) WaitingOn() (name string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pending) == 0 {
		return "", false
	}
	return t.pending[len(t.pending)-1].name, true
}

// notePending updates the pending-call set from one JSONL line.
func (t *Tailer) notePending(line []byte) {
	var generic struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
		Payload json.RawMessage `json:"payload"`
		IsMeta  bool            `json:"isMeta"`
	}
	if json.Unmarshal(line, &generic) != nil {
		return
	}
	switch generic.Type {
	case "user", "assistant": // Claude Code
		if generic.IsMeta {
			return
		}
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(generic.Message, &msg) != nil {
			return
		}
		var blocks []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			ToolUseID string `json:"tool_use_id"`
		}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			// Plain-string content: a human prompt (or interrupt notice)
			// means the human already acted; stale pending calls are moot.
			if generic.Type == "user" {
				t.pending = nil
			}
			return
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				t.pending = append(t.pending, pendingCall{b.ID, b.Name})
			case "tool_result":
				t.resolve(b.ToolUseID)
			case "text":
				if generic.Type == "user" {
					t.pending = nil
				}
			}
		}
	case "response_item": // Codex
		var p struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		}
		if json.Unmarshal(generic.Payload, &p) != nil {
			return
		}
		switch p.Type {
		case "function_call", "custom_tool_call":
			t.pending = append(t.pending, pendingCall{p.CallID, p.Name})
		case "function_call_output", "custom_tool_call_output":
			t.resolve(p.CallID)
		}
	case "event_msg": // Codex marks turn boundaries explicitly
		var p struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(generic.Payload, &p) != nil {
			return
		}
		if p.Type == "task_complete" || p.Type == "turn_aborted" {
			t.pending = nil
		}
	}
}

func (t *Tailer) resolve(id string) {
	for i, c := range t.pending {
		if c.id == id {
			t.pending = append(t.pending[:i], t.pending[i+1:]...)
			return
		}
	}
}

// Poll reads newly appended bytes and returns the accumulated turn list.
// All I/O failures are absorbed: the previous turn list is returned and the
// next poll retries. A shrunken file (rotation) resets the tailer.
func (t *Tailer) Poll() []Turn {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, err := os.Open(t.Path)
	if err != nil {
		return t.Turns
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return t.Turns
	}
	if st.Size() < t.offset {
		t.offset, t.buf, t.Turns, t.pending = 0, nil, nil, nil
		t.unknownStreak, t.progress = 0, 0
		t.ctxUsed, t.ctxWindow = 0, 0
	}
	if st.Size() == t.offset {
		return t.Turns
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return t.Turns
	}
	chunk, err := io.ReadAll(f)
	if err != nil && len(chunk) == 0 {
		return t.Turns
	}
	t.offset += int64(len(chunk))
	t.buf = append(t.buf, chunk...)
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		line := t.buf[:i]
		t.buf = append([]byte(nil), t.buf[i+1:]...)
		if len(bytes.TrimSpace(line)) > 0 {
			if recognizedLine(line) {
				t.unknownStreak = 0
			} else {
				t.unknownStreak++
			}
			if agentShaped(line) {
				t.progress++
			}
		}
		t.notePending(line)
		t.noteUsage(line)
		if turn, ok := ParseLine(line); ok {
			t.Turns = append(t.Turns, turn)
		}
	}
	return t.Turns
}

// FindClaudeTranscript locates the session log for a Claude Code session id
// by globbing all project directories. This avoids re-implementing Claude's
// cwd-escaping scheme (an undocumented internal): the session id is a UUID
// we minted ourselves, so a filename match is unambiguous.
func FindClaudeTranscript(home, sessionID string) (string, bool) {
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return "", false
	}
	return matches[0], true
}

// FindClaudeNewestInDir locates the most recently modified Claude session
// log for a given working directory — used when adopting a session whose
// id we don't know (e.g. migrated via --resume). Returns path and the
// session id (the filename). Claude escapes the cwd into a project dir
// name by replacing path separators and other specials with '-'.
func FindClaudeNewestInDir(home, dir string) (path, sessionID string, ok bool) {
	return FindClaudeNewestInDirSince(home, dir, time.Time{})
}

// FindClaudeNewestInDirSince is FindClaudeNewestInDir restricted to session
// logs modified after since — used to re-run discovery when a pane finished a
// whole working phase that the linked transcript never carried (a /clear or
// relaunch inside the pane started a new session file). Only a file the
// phase actually wrote can be the pane's current session.
func FindClaudeNewestInDirSince(home, dir string, since time.Time) (path, sessionID string, ok bool) {
	esc := regexp.MustCompile(`[^a-zA-Z0-9-]`).ReplaceAllString(dir, "-")
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", esc, "*.jsonl"))
	if err != nil || len(matches) == 0 {
		return "", "", false
	}
	var bestTime time.Time
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil || !st.ModTime().After(since) {
			continue
		}
		if path == "" || st.ModTime().After(bestTime) {
			path, bestTime = m, st.ModTime()
		}
	}
	if path == "" {
		return "", "", false
	}
	return path, strings.TrimSuffix(filepath.Base(path), ".jsonl"), true
}
