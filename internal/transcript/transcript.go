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
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Turn is one visible chat message.
//
// UID/Segment/Record are the turn's durable source address — the same positional
// identity sessionlog.ScanLog assigns (UID = the log's meta uid, Segment = source
// seams seen before the record, Record = 0-based index among successfully-parsed
// records). They are populated only when a turn is read from a unified session log
// (sessionlog.segmentOf / ReadHistory); the raw transcript tailer leaves them zero,
// since a CLI transcript file has no scimux uid and no session-log record ordinals.
// A capture stamps this triple so it can name the exact chat turn it came from and
// resolve jump-back across /clear seams, rotation, slug reuse, and node deletion.
type Turn struct {
	Role    string `json:"role"` // "user" or "assistant"
	Text    string `json:"text"`
	Time    string `json:"time,omitempty"` // raw timestamp string as logged, if any
	UID     string `json:"uid,omitempty"`
	Segment int    `json:"segment,omitempty"`
	Record  int    `json:"record,omitempty"`
}

// ToolStamp is one tool call or result extracted from a transcript line for
// the session-log mirror (fare-design.md V2-P2 / O6). Status is "pending"
// (call) or "completed" (result). Time is the CLI's line timestamp so tool
// intervals match agent time, not scimux append clock.
type ToolStamp struct {
	ID     string // join key: claude tool_use.id / codex call_id
	Title  string // tool name when known (call side)
	Status string // "pending" | "completed"
	Time   string // raw transcript timestamp
	// Digest is the first 16 hex of sha256 over the call's raw input bytes,
	// set on the call side only. It joins a record to the Claude escalation
	// notice that announced the same call (internal/app/claude_asked.go):
	// Claude writes the record only after a human answers, so a match is proof
	// that one specific dialog closed. Verified against real data 2026-08-18 —
	// the hook's tool_input and the transcript's input hash identically, so no
	// canonicalization is involved on either side.
	Digest string
}

// inputDigest is the shared spelling of that hash. Empty input yields "", so a
// record with nothing to hash never collides with a notice.
func inputDigest(input []byte) string {
	if len(input) == 0 {
		return ""
	}
	sum := sha256.Sum256(input)
	return hex.EncodeToString(sum[:])[:16]
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

// NewestContentTime returns the newest timestamp among records the existing
// parser turns into a visible turn (ParseLine/makeTurn). Metadata and unknown
// shapes — including Claude Code's trailing bridge-session records — are
// silently ignored. ok is false when the file is unreadable or has no turns.
// Callers use this instead of mtime when judging whether a transcript carried
// a working phase: mtime is not evidence of content.
func NewestContentTime(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	var newest time.Time
	found := false
	sc := bufio.NewScanner(f)
	// Claude/Codex lines can be large (tool results); raise the token limit
	// so a long line is still considered rather than aborting the scan.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		turn, ok := ParseLine(line)
		if !ok || turn.Time == "" {
			continue
		}
		t, ok := parseStamp(turn.Time)
		if !ok {
			continue
		}
		if !found || t.After(newest) {
			newest, found = t, true
		}
	}
	return newest, found
}

// parseStamp reads a CLI line timestamp, tolerating both RFC3339 forms the
// agents emit. Absent or unparseable reads as unknown, never as zero time.
func parseStamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return ts, true
	}
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts, true
	}
	return time.Time{}, false
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
	Path  string
	Turns []Turn
	// Tools accumulates call+result stamps in file order for the mirror's
	// V2-P2 tool projection. Parallel to Turns — does not affect Poll's
	// return value or the turn watermark.
	Tools   []ToolStamp
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
	// last* retain the latest Claude (assistant) usage breakdown for fare
	// projection (fare-design.md Phase 3). Occupancy (ctxUsed) is still the
	// flat sum only; these fields ride alongside and never feed liveness.
	lastIn          int64
	lastOut         int64
	lastCacheRead   int64
	lastCacheCreate int64
	lastTurnID      string
	// owing is true when the newest recognized agent-facing record leaves the
	// agent owing the next output: a human prompt or a tool result. False after
	// an assistant record (visible text or tool_use). Unknown record types leave
	// it unchanged. Quiet-pane stall backstop input only (P1b) — never liveness.
	owing bool
	// lastRole is "" | "user" | "assistant": the role of the newest recognized
	// agent-facing record. Empty before anything is recognized — that is the
	// case Owing cannot distinguish (Owing is also false then). Assigned at
	// every site that assigns owing. Map "turn finished" input only (P5) —
	// never liveness.
	lastRole string
	// lastTurnAt is the CLI's own stamp on the newest parsed turn, kept in
	// memory so the poller can date a delivery without re-reading the file.
	// Zero means "cannot be dated" (nothing parsed yet, undated records, or a
	// rotation reset) — never "old". Map freshness input only, never liveness.
	lastTurnAt time.Time
	// endTurn is Claude's explicit stop_reason:"end_turn" boundary, or its
	// explicit interrupted-message record. Unlike Delivered, it is false for an
	// assistant tool_use or a partial assistant record. The chat composer uses it
	// to become ready as soon as Claude has committed the completed/aborted turn,
	// without waiting for the pane quiet timer.
	// Structured transcript data only; never feeds mechanical liveness.
	endTurn bool
}

// UsageBreakdown is the latest per-turn token split retained from a Claude
// assistant usage block (or zero when none has been seen / shape unknown).
// InputTokens is Claude-fresh; CacheCreationTokens covers both the flat
// cache_creation_input_tokens field and the nested ephemeral object.
// TurnID is message.id + ":" + requestId (ccusage/D3 dedup identity).
type UsageBreakdown struct {
	InputTokens         int64
	OutputTokens        int64
	CachedReadTokens    int64
	CacheCreationTokens int64
	TurnID              string
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

// UsageBreakdown reports the latest Claude per-turn token split retained by
// noteUsage. Zero values mean "not reported" — never an error. Callers
// (mirror projection) copy these into sessionlog.UsageEvent; occupancy
// still comes from Usage().
func (t *Tailer) UsageBreakdown() UsageBreakdown {
	t.mu.Lock()
	defer t.mu.Unlock()
	return UsageBreakdown{
		InputTokens:         t.lastIn,
		OutputTokens:        t.lastOut,
		CachedReadTokens:    t.lastCacheRead,
		CacheCreationTokens: t.lastCacheCreate,
		TurnID:              t.lastTurnID,
	}
}

// ToolStamps returns a snapshot of tool call/result stamps accumulated by
// Poll (V2-P2 mirror widen). The slice is a copy — safe after unlock.
func (t *Tailer) ToolStamps() []ToolStamp {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.Tools) == 0 {
		return nil
	}
	out := make([]ToolStamp, len(t.Tools))
	copy(out, t.Tools)
	return out
}

// noteUsage extracts context usage from one JSONL line. Defensive like all
// other parsing: unknown shapes leave the previous values standing.
func (t *Tailer) noteUsage(line []byte) {
	var g struct {
		Type      string `json:"type"`
		RequestID string `json:"requestId"`
		Message   struct {
			ID    string `json:"id"`
			Usage struct {
				InputTokens              int64           `json:"input_tokens"`
				CacheReadInputTokens     int64           `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int64           `json:"cache_creation_input_tokens"`
				OutputTokens             int64           `json:"output_tokens"`
				CacheCreation            json.RawMessage `json:"cache_creation"`
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
		// in (fresh + cached) plus what the model produced. Flat
		// cache_creation_input_tokens only — nested ephemeral does not
		// change this sum (Phase 3: occupancy formula byte-for-byte).
		if used := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens + u.OutputTokens; used > 0 {
			t.ctxUsed = used
		}
		// Fare breakdown rides alongside occupancy. Cache-creation prefers
		// the flat field; if absent/zero, sum nested ephemeral shapes
		// (ephemeral_5m / ephemeral_1h or *_input_tokens variants).
		create := u.CacheCreationInputTokens
		if create == 0 {
			create = sumCacheCreationNested(u.CacheCreation)
		}
		if u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheReadInputTokens != 0 || create != 0 {
			t.lastIn = u.InputTokens
			t.lastOut = u.OutputTokens
			t.lastCacheRead = u.CacheReadInputTokens
			t.lastCacheCreate = create
			if g.Message.ID != "" && g.RequestID != "" {
				t.lastTurnID = g.Message.ID + ":" + g.RequestID
			} else if g.Message.ID != "" {
				t.lastTurnID = g.Message.ID
			} else if g.RequestID != "" {
				t.lastTurnID = g.RequestID
			} else {
				t.lastTurnID = ""
			}
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

// sumCacheCreationNested sums numeric values inside Claude's nested
// cache_creation object (ephemeral_5m / ephemeral_1h, or the longer
// ephemeral_*_input_tokens names seen on the wire). Unknown shapes yield 0.
func sumCacheCreationNested(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var m map[string]int64
	if json.Unmarshal(raw, &m) != nil {
		return 0
	}
	var sum int64
	for _, v := range m {
		sum += v
	}
	return sum
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

// Owing reports whether the newest recognized transcript record leaves the
// agent owing the next output (human prompt or tool result). The quiet-pane
// stall backstop combines this with pane quietness only — no pane text.
func (t *Tailer) Owing() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.owing
}

// Delivered reports whether the newest recognized agent-facing record is an
// assistant turn (text or tool_use / function_call). False before anything is
// recognized and after a user/tool_result record. Sibling of Owing — not its
// inverse: both are false on an empty tailer. Map turn-finished input only.
func (t *Tailer) Delivered() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastRole == "assistant"
}

// EndTurn reports Claude's explicit completed-turn boundary. It is kept
// separate from Delivered because an assistant record can instead be a tool
// request that leaves the agent blocked on permission or still working.
func (t *Tailer) EndTurn() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.endTurn
}

// NewestTurnTime reports the CLI's stamp on the newest parsed turn. ok is
// false when no turn has been dated — undated records must read as unknown,
// so a caller gating on freshness declines rather than guesses.
func (t *Tailer) NewestTurnTime() (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastTurnAt, !t.lastTurnAt.IsZero()
}

// notePending updates the pending-call set from one JSONL line and appends
// ToolStamp entries for the mirror (V2-P2). Pending tracking and tool
// stamping share one parse so join keys stay identical.
func (t *Tailer) notePending(line []byte) {
	var generic struct {
		Type                 string          `json:"type"`
		Timestamp            string          `json:"timestamp"`
		Message              json.RawMessage `json:"message"`
		Payload              json.RawMessage `json:"payload"`
		IsMeta               bool            `json:"isMeta"`
		InterruptedMessageID string          `json:"interruptedMessageId"`
	}
	if json.Unmarshal(line, &generic) != nil {
		return
	}
	switch generic.Type {
	case "user", "assistant": // Claude Code
		if generic.IsMeta {
			return
		}
		// Agent-facing record: user (prompt or tool_result) leaves the agent
		// owing; assistant (text or tool_use) clears the debt. Set before the
		// content parse so tool_result-only user records still mark owing.
		// lastRole tracks the same record so Delivered can distinguish
		// "assistant just spoke" from "nothing recognized yet".
		t.lastRole = generic.Type
		t.owing = generic.Type == "user"
		t.endTurn = false
		var msg struct {
			Content    json.RawMessage `json:"content"`
			StopReason string          `json:"stop_reason"`
		}
		if json.Unmarshal(generic.Message, &msg) != nil {
			return
		}
		// Claude records an interrupted turn as a user-role message even though
		// it is a terminal boundary: the agent does not owe another response and
		// the next prompt is valid immediately. Treating it like an ordinary user
		// prompt leaves Owing true forever, eventually raises the AX inspect floor,
		// and withdraws reply_ready. The structural interruptedMessageId is the
		// primary signal; the exact bracketed text keeps older captures working.
		if generic.Type == "user" && claudeInterruptedTurn(generic.InterruptedMessageID, msg.Content) {
			t.pending = nil
			t.owing = false
			t.endTurn = true
			return
		}
		// Every recognized Claude-facing record replaces the boundary state:
		// a new user turn or tool-use/partial assistant record withdraws the
		// prior completion; only the explicit final boundary raises it.
		t.endTurn = generic.Type == "assistant" && msg.StopReason == "end_turn"
		var blocks []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			ToolUseID string          `json:"tool_use_id"`
			Input     json.RawMessage `json:"input"`
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
				if b.ID != "" {
					t.Tools = append(t.Tools, ToolStamp{
						ID: b.ID, Title: b.Name, Status: "pending",
						Time: generic.Timestamp, Digest: inputDigest(b.Input),
					})
				}
			case "tool_result":
				t.resolve(b.ToolUseID)
				if b.ToolUseID != "" {
					t.Tools = append(t.Tools, ToolStamp{
						ID: b.ToolUseID, Status: "completed", Time: generic.Timestamp,
					})
				}
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
			t.owing = false
			t.lastRole = "assistant"
			if p.CallID != "" {
				t.Tools = append(t.Tools, ToolStamp{
					ID: p.CallID, Title: p.Name, Status: "pending", Time: generic.Timestamp,
				})
			}
		case "function_call_output", "custom_tool_call_output":
			t.resolve(p.CallID)
			t.owing = true
			t.lastRole = "user"
			if p.CallID != "" {
				t.Tools = append(t.Tools, ToolStamp{
					ID: p.CallID, Status: "completed", Time: generic.Timestamp,
				})
			}
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

func claudeInterruptedTurn(interruptedMessageID string, content json.RawMessage) bool {
	if interruptedMessageID != "" {
		return true
	}
	isInterruptText := func(s string) bool {
		s = strings.TrimSpace(s)
		return s == "[Request interrupted by user]" ||
			s == "[Request interrupted by user for tool use]"
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		return isInterruptText(text)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "text" && isInterruptText(b.Text) {
			return true
		}
	}
	return false
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
		t.offset, t.buf, t.Turns, t.Tools, t.pending = 0, nil, nil, nil, nil
		t.unknownStreak, t.progress = 0, 0
		t.ctxUsed, t.ctxWindow = 0, 0
		t.lastIn, t.lastOut, t.lastCacheRead, t.lastCacheCreate = 0, 0, 0, 0
		t.lastTurnID = ""
		t.owing = false
		t.lastRole = ""
		t.lastTurnAt = time.Time{}
		t.endTurn = false
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
			// Newest, not last-parsed: Claude flushes some records late, so a
			// trailing older stamp must not age a fresh delivery.
			if ts, ok := parseStamp(turn.Time); ok && ts.After(t.lastTurnAt) {
				t.lastTurnAt = ts
			}
			// Visible turns also set owing (Codex message items reach here
			// without going through the function_call branches above). An explicit
			// Claude completion/interrupt boundary was already interpreted by
			// notePending and must not be overwritten merely because the visible
			// interrupt notice itself carries role=user.
			if !t.endTurn {
				if turn.Role == "user" || turn.Role == "assistant" {
					t.lastRole = turn.Role
				}
				t.owing = turn.Role == "user"
			}
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
	if err != nil || len(matches) != 1 {
		return "", false
	}
	return matches[0], true
}

// ClaudeBridgeReady reports whether Claude has recorded that Remote Control is
// active for sessionID. The prose in content is deliberately ignored: it is UI
// copy (and may be localized or reworded), while type/subtype/sessionId are the
// narrow structural signal observed in Claude's JSONL. As with every transcript
// helper, unknown shapes and read errors degrade to false rather than errors.
func ClaudeBridgeReady(path, sessionID string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var r struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Type == "system" &&
			r.Subtype == "bridge_status" && r.SessionID == sessionID {
			return true
		}
	}
	return false
}
