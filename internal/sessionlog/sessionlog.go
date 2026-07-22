// Package sessionlog owns scimux's unified per-node session history: one
// append-only JSONL file per node under <data>/sessions/<node-id>.jsonl,
// shared by every transport (ACP, codex app-server, and — via the transcript
// mirror — tmux-wrapped claude). The filename is the node's human-friendly
// slug; identity across slug reuse comes from the meta header record inside
// the file. Records are plain-text JSON lines on purpose: the corpus must
// stay grep/sed/awk/jq-able for future search/consolidation/sharing readers.
package sessionlog

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/transcript"
)

// Event is one record of the append-only session log. It mirrors the ACP
// plan's schema: one record per observed event, with only the fields relevant
// to that record's type set. The log is the authoritative history for a
// structured-transport node — there is no pane photo — so a reader
// reconstructs the chat, liveness, usage, and peek from it. Readers skip
// record types they don't know, so the schema can grow without breaking
// old binaries or old files.
type Event struct {
	T          string       `json:"t"` // "meta" | "source" | "user" | "assistant" | "tool" | "usage" | "mark" | "stop" | "error"
	Time       string       `json:"time"`
	Text       string       `json:"text,omitempty"`       // user / assistant
	Tool       *ToolEvent   `json:"tool,omitempty"`       // tool
	Usage      *UsageEvent  `json:"usage,omitempty"`      // usage
	Meta       *MetaEvent   `json:"meta,omitempty"`       // meta
	Source     *SourceEvent `json:"source,omitempty"`     // source
	Mark       *MarkEvent   `json:"mark,omitempty"`       // mark
	StopReason string       `json:"stopReason,omitempty"` // stop
	Error      string       `json:"error,omitempty"`      // error
}

// ToolEvent is the tool-call state assembled by toolCallId. rawInput carries
// paths, diffs and command text — the same sensitivity as tmux pane excerpts,
// which is why the log file is 0600.
type ToolEvent struct {
	ID       string `json:"id"`
	Title    string `json:"title,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Status   string `json:"status,omitempty"`
	RawInput any    `json:"rawInput,omitempty"`
}

// UsageEvent carries both observed usage shapes (opencode streams the first
// four fields via usage_update; the token breakdown arrives only in the final
// PromptResponse.usage). Any subset may be present.
type UsageEvent struct {
	Used             int     `json:"used,omitempty"`
	Size             int     `json:"size,omitempty"`
	CostAmount       float64 `json:"costAmount,omitempty"`
	CostCurrency     string  `json:"costCurrency,omitempty"`
	InputTokens      int     `json:"inputTokens,omitempty"`
	OutputTokens     int     `json:"outputTokens,omitempty"`
	CachedReadTokens int     `json:"cachedReadTokens,omitempty"`
	TotalTokens      int     `json:"totalTokens,omitempty"`
}

// MetaEvent is the self-describing header written as a log's first record.
// The filename is only a friendly slug that can be reissued after a delete;
// UID is the collision-proof identity, and the launch config makes a log
// file meaningful on its own (shared, archived, or searched outside the
// node store's context). Old logs without a header stay valid — every
// reader treats meta as just another skippable record type.
type MetaEvent struct {
	Node    string `json:"node"`
	UID     string `json:"uid"`
	Agent   string `json:"agent"`
	Model   string `json:"model,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Created string `json:"created"`
}

// SourceEvent marks the seam where a transcript-mirrored node (re)binds to an
// agent-CLI session file: first link, adoption correction, /clear rollover,
// rotation. The turns that follow a source record were mirrored from that
// file, which is also the mirror's dedupe anchor — on startup it replays the
// log, finds the last source, and counts the turns after it to know where to
// resume. Structured transports write source records only as /clear page-turn
// seams (their log is the primary history, not a mirror), with an empty path.
type SourceEvent struct {
	Path      string `json:"path"`
	SessionID string `json:"sessionId,omitempty"`
}

// NewSource builds a seam record for a (re)bound transcript file.
func NewSource(path, sessionID string) Event {
	return Event{T: "source", Source: &SourceEvent{Path: path, SessionID: sessionID}}
}

// MarkEvent records how far the transcript mirror has consumed its current
// source file, as a byte size. It is pure mirror bookkeeping — the restart
// fast path: an append-only transcript whose size still equals the last mark
// has nothing new, so it is skipped without re-parsing. It carries no chat
// content; segment and chat readers ignore it like any unknown record.
type MarkEvent struct {
	Off int64 `json:"off"`
}

// NewMark builds a mirror-watermark record for the given transcript byte size.
func NewMark(off int64) Event {
	return Event{T: "mark", Mark: &MarkEvent{Off: off}}
}

// NewMeta builds the header record for a fresh node log.
func NewMeta(node, agent, model, dir string) Event {
	b := make([]byte, 8)
	var uid string
	if _, err := rand.Read(b); err == nil {
		uid = fmt.Sprintf("%x", b)
	} else {
		// The UID exists to be collision-proof across slug reuse; a silent
		// all-zero value on RNG failure would defeat exactly that. A
		// nanosecond stamp cannot collide with another log minted by this
		// process, and the "t" prefix keeps it distinguishable from hex UIDs.
		uid = fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	now := nowStamp()
	return Event{T: "meta", Time: now, Meta: &MetaEvent{
		Node: node, UID: uid, Agent: agent, Model: model,
		Dir: dir, Created: now,
	}}
}

// Writer serializes append-only writes to one node's log file. Writes are
// small and infrequent (turn boundaries, tool state, usage), so a fresh
// O_APPEND handle per write keeps the file crash-safe without a long-lived fd.
type Writer struct {
	Path string
}

// fileLocks serializes appends *per path*, across every Writer instance that
// targets it. A per-Writer mutex is not enough: /clear seams and start-failure
// records mint fresh Writers against a live node's file, and two unsynchronized
// O_APPEND writers can interleave a partial line if one crashes mid-write.
var fileLocks sync.Map // path -> *sync.Mutex

func lockPath(path string) func() {
	m, _ := fileLocks.LoadOrStore(path, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (w *Writer) Append(ev Event) error {
	if ev.Time == "" {
		ev.Time = nowStamp()
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	unlock := lockPath(w.Path)
	defer unlock()
	// 0600: rawInput and prompt text are as sensitive as pane-excerpt evidence.
	f, err := os.OpenFile(w.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := f.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	// The seam and turn records are the node's durable history; fsync so a
	// crash right after a "done" append cannot lose the last page turn.
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ReadEvents replays the log file. Defensive like the transcript parser:
// unreadable file yields nothing, unparseable lines are skipped.
func ReadEvents(path string) []Event {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// ReadTurns yields the chat turns (user/assistant) from the log — the
// structured-transport equivalent of transcript.Tailer.Poll for tmux nodes.
func ReadTurns(path string) []transcript.Turn {
	turns := []transcript.Turn{}
	for _, ev := range ReadEvents(path) {
		switch ev.T {
		case "user":
			if strings.TrimSpace(ev.Text) != "" {
				turns = append(turns, transcript.Turn{Role: "user", Text: ev.Text, Time: ev.Time})
			}
		case "assistant":
			if strings.TrimSpace(ev.Text) != "" {
				turns = append(turns, transcript.Turn{Role: "assistant", Text: ev.Text, Time: ev.Time})
			}
		}
	}
	return turns
}

// LatestUsage folds the log's usage records into the newest known values.
// used/size feed the context gauge; either is 0 when never reported (pi).
func LatestUsage(path string) (used, size int64) {
	for _, ev := range ReadEvents(path) {
		if ev.T != "usage" || ev.Usage == nil {
			continue
		}
		if ev.Usage.Used > 0 {
			used = int64(ev.Usage.Used)
		}
		if ev.Usage.Size > 0 {
			size = int64(ev.Usage.Size)
		}
	}
	return used, size
}

// PeekLog renders the tail of the raw event log as plain text — the
// structured-transport analogue of a pane photo (no terminal to capture).
// Scoped to the current segment: a /clear appends a source seam and turns the
// page, so the peek must not resurface the dead conversation behind it (the
// seam line itself is kept as the visible boundary). At most maxLines lines;
// empty is shown for a missing or event-free log.
func PeekLog(path string, maxLines int, empty string) string {
	evs := ReadEvents(path)
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].T == "source" {
			evs = evs[i:]
			break
		}
	}
	var lines []string
	for _, ev := range evs {
		lines = append(lines, formatEvent(ev))
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	if len(lines) == 0 {
		return empty
	}
	return strings.Join(lines, "\n")
}

func formatEvent(ev Event) string {
	switch ev.T {
	case "meta":
		if ev.Meta != nil {
			return "— session " + ev.Meta.Node + " (" + ev.Meta.Agent +
				strings.TrimRight(" "+ev.Meta.Model, " ") + ")"
		}
		return "— session"
	case "source":
		if ev.Source != nil {
			return "— source " + ev.Source.Path
		}
		return "— source"
	case "user":
		return "» " + oneLine(ev.Text)
	case "assistant":
		return "« " + oneLine(ev.Text)
	case "tool":
		if ev.Tool != nil {
			return "· tool " + ev.Tool.Title + " [" + ev.Tool.Status + "]"
		}
		return "· tool"
	case "usage":
		if ev.Usage != nil {
			return "· usage used=" + itoa(ev.Usage.Used) + " size=" + itoa(ev.Usage.Size)
		}
		return "· usage"
	case "stop":
		return "— stop (" + ev.StopReason + ")"
	case "error":
		return "! error: " + oneLine(ev.Error)
	}
	return "· " + ev.T
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return strings.TrimSpace(string(b))
}
