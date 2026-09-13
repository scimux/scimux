// Package sessionlog owns scimux's unified per-node session history: one
// append-only JSONL file per node under <data>/sessions/<node-id>.jsonl,
// shared by every transport (ACP, codex app-server, and — via the transcript
// mirror — tmux-wrapped claude). The filename is the node's human-friendly
// slug; identity across slug reuse comes from the meta header record inside
// the file. Records are plain-text JSON lines on purpose: the corpus must
// stay grep/sed/awk/jq-able for future search/consolidation/sharing readers.
//
// # The corpus is the user's history and nothing else
//
// Taken together these files are an append-only, never-deleted, multi-vendor
// record of model output. Heuristics may read it freely — that is what
// search, the fare layer and needs-input detection do. No model may be
// trained, fine-tuned or distilled from it, ever, and no part of it may be
// collected into a training dataset. Every vendor whose harness scimux
// supervises prohibits exactly that (Anthropic Consumer Terms and Commercial
// D.4, OpenAI Terms of Use, SpaceXAI AUP, Meta Model API ToS 10.1(ix)), so a
// single change here breaches several agreements at once, on the user's
// account rather than the project's.
//
// This is written at the store rather than in any one transport because the
// realistic failure is not an adapter: it is a well-meant commit to
// internal/dialoghint, to search ranking or to the fare layer that reads like
// an accuracy improvement. If a change would make any of those learn from
// this directory, it is out of scope for scimux regardless of how well it
// works.
package sessionlog

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
	T          string          `json:"t"` // "meta" | "source" | "user" | "assistant" | "tool" | "usage" | "mark" | "stop" | "station" | "error" | "asset" | "attention" | "decision"
	Time       string          `json:"time"`
	Text       string          `json:"text,omitempty"`       // user / assistant
	Tool       *ToolEvent      `json:"tool,omitempty"`       // tool
	Usage      *UsageEvent     `json:"usage,omitempty"`      // usage
	Meta       *MetaEvent      `json:"meta,omitempty"`       // meta
	Source     *SourceEvent    `json:"source,omitempty"`     // source
	Mark       *MarkEvent      `json:"mark,omitempty"`       // mark
	Asset      *AssetEvent     `json:"asset,omitempty"`      // asset
	Station    *StationEvent   `json:"station,omitempty"`    // station
	Attention  *AttentionEvent `json:"attention,omitempty"`  // attention (needs-input edge)
	Decision   *DecisionEvent  `json:"decision,omitempty"`   // decision (auto-approval audit; P3)
	StopReason string          `json:"stopReason,omitempty"` // stop
	Error      string          `json:"error,omitempty"`      // error
}

// DecisionEvent is an audited automatic (or future manual) permission choice.
// Appended and fsynced before the structured transport delivers the selected
// option. Additive schema: old readers ignore t:"decision"; it is never a
// turn, seam, fare hit, tool interval, attention edge, or asset anchor.
type DecisionEvent struct {
	Source    string      `json:"source"`              // "auto"
	LeaseID   string      `json:"lease_id"`            // opaque lease that authorized this decision
	RequestID string      `json:"request_id"`          // stable pending-request identity
	Agent     string      `json:"agent,omitempty"`     // node agent at decision time
	ToolKind  string      `json:"tool_kind,omitempty"` // structured tool kind when known
	Title     string      `json:"title,omitempty"`     // full pending title/command
	Reason    string      `json:"reason,omitempty"`    // full reason when known
	Options   []DecOption `json:"options,omitempty"`   // every offered option
	Selected  DecOption   `json:"selected"`            // the option scimux authorized
}

// DecOption is one permission choice in a DecisionEvent: key, display name,
// and semantic kind ("allow" | "allow_always" | "reject" | "reject_always" | "").
type DecOption struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

// NewDecision builds a t:"decision" audit record. Time is stamped on Append.
func NewDecision(d DecisionEvent) Event {
	return Event{T: "decision", Decision: &d}
}

// DecisionSurface is one decision projected for chat audit rendering, keyed by
// durable ReadEvents record index so P4 can merge it into timeline order without
// turning it into a fake assistant turn.
type DecisionSurface struct {
	Record   int           `json:"record"`
	Time     string        `json:"time"`
	Decision DecisionEvent `json:"decision"`
}

// AttentionEvent brackets a needs-input interval (approval / question /
// inspect / dialog). Status is "start" or "end". Kind is the attention
// class from the existing mechanical signal — never a new attention source
// (fare-design.md V2-P2; reuses poller's a.attn / ACP Attention()).
// Append-only edges; historical wait is unrecoverable without these.
type AttentionEvent struct {
	Kind   string `json:"kind,omitempty"` // "approval" | "question" | "inspect" | "dialog"
	Status string `json:"status"`         // "start" | "end"
}

// NewAttentionEdge builds a durable needs-input start or end record.
func NewAttentionEdge(kind, status string) Event {
	return Event{T: "attention", Attention: &AttentionEvent{Kind: kind, Status: status}}
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
//
// CacheCreationTokens, ReasoningTokens, Model, and TurnID are additive
// (fare-design.md §4.1): all omitempty so legacy {"used":N} records stay
// greppable and parse without migration.
type UsageEvent struct {
	Used             int     `json:"used,omitempty"`
	Size             int     `json:"size,omitempty"`
	CostAmount       float64 `json:"costAmount,omitempty"`
	CostCurrency     string  `json:"costCurrency,omitempty"`
	InputTokens      int     `json:"inputTokens,omitempty"`
	OutputTokens     int     `json:"outputTokens,omitempty"`
	CachedReadTokens int     `json:"cachedReadTokens,omitempty"`
	TotalTokens      int     `json:"totalTokens,omitempty"`
	// Fare-oriented extensions (Phase 1). Existing fields keep their meaning.
	CacheCreationTokens int    `json:"cacheCreationTokens,omitempty"` // claude, pi, opencode-DB, grok(≈0)
	ReasoningTokens     int    `json:"reasoningTokens,omitempty"`     // pi, grok, claude
	Model               string `json:"model,omitempty"`               // per-turn (D6)
	TurnID              string `json:"turnId,omitempty"`              // dedup identity (D3)
}

// String formats occupancy fields for peek/log lines. Model is included only
// when set so empty Model leaves the pre-Phase-1 output unchanged.
func (u UsageEvent) String() string {
	s := "used=" + itoa(u.Used) + " size=" + itoa(u.Size)
	if u.Model != "" {
		s += " model=" + u.Model
	}
	return s
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
	Effort  string `json:"effort,omitempty"` // optional agent reasoning-effort selection
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
	// Reason distinguishes a deliberate /clear page-turn ("clear") from the
	// mechanical (re)binds — initial mirror bind, relink, rotation — that also
	// emit a source seam. Empty means a mechanical bind. Only "clear" seams are
	// genuine stop boundaries a chain renderer should split on; without this the
	// two are indistinguishable (a heavily relinked thread would draw dozens of
	// bogus stops). Absent on every pre-existing seam, so replay is unchanged.
	Reason string `json:"reason,omitempty"`
}

// NewSource builds a seam record for a (re)bound transcript file (mechanical).
func NewSource(path, sessionID string) Event {
	return Event{T: "source", Source: &SourceEvent{Path: path, SessionID: sessionID}}
}

// NewClearSource builds the path-less "detached" seam appended when a /clear
// turns the page, tagged so a chain renderer can tell it from a relink/rotation.
// Claude's rollover has no new session id yet at retire time and passes ""; the
// structured transports (ACP, codex) already hold the fresh session/thread id
// from the protocol call and pass it, so their clear seams are both tagged AND
// carry the new session — the seam stays uniform across every transport.
func NewClearSource(sessionID string) Event {
	return Event{T: "source", Source: &SourceEvent{SessionID: sessionID, Reason: "clear"}}
}

// StationEvent is a per-station label snapshot: one metro-map stop's name and
// description, frozen so a later rename of the active chat (which moves only the
// node's live Title/Description = the head station) cannot rewrite a closed
// station's label. Seam is the station's start-time key — the node's created_at
// for the first station, or the /clear seam's RFC3339 time for later ones —
// matching the strings the map already uses as stop keys. Append-only like every
// record: a manual edit of a station is just a newer StationEvent for the same
// Seam, latest wins. Unknown to old readers, so it degrades to the pre-feature
// behaviour (node title on every stop).
type StationEvent struct {
	Seam  string `json:"seam"`
	Title string `json:"title,omitempty"`
	Desc  string `json:"desc,omitempty"`
}

// NewStation builds a per-station label snapshot for the stop that starts at seam.
func NewStation(seam, title, desc string) Event {
	return Event{T: "station", Station: &StationEvent{Seam: seam, Title: title, Desc: desc}}
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
func NewMeta(node, agent, model, effort, dir string) Event {
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
		Effort: effort, Dir: dir, Created: now,
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

// UnterminatedTail reports whether path ends mid-line — bytes with no closing
// newline. That is what an interrupted append leaves behind: a short write
// under ENOSPC returns an error having already put bytes on disk, and a crash
// truncates wherever it lands.
//
// An append-only file is repaired by appending, so a writer that sees this
// closes the fragment with a newline and leaves it there as the evidence it
// is. Without that, the next record welds onto the fragment and replay drops
// one malformed line — losing a record whose append reported success.
//
// Unreadable means "no": every caller's fallback is today's behaviour, and a
// spurious newline in a file we cannot inspect would be its own corruption.
func UnterminatedTail(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return false
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], st.Size()-1); err != nil {
		return false
	}
	return last[0] != '\n'
}

// SyncParentDir fsyncs the directory that holds path so a newly created file's
// directory entry survives a crash. On POSIX, fsyncing a file does not
// necessarily make its new dirent durable; the parent directory must be synced
// too. Call this only after the file itself has been synced, and only when the
// file was newly created — an append to an existing file needs no dirent update.
func SyncParentDir(path string) error {
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

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
	// A first append to this node's log creates the file; its dirent is not
	// durable until the parent directory is also synced (see SyncParentDir).
	// Detect that under the same per-path lock that serializes the write.
	_, statErr := os.Stat(w.Path)
	created := os.IsNotExist(statErr)
	if UnterminatedTail(w.Path) {
		b = append([]byte{'\n'}, b...)
	}
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
	if err == nil && created {
		err = SyncParentDir(w.Path)
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
	case "attention":
		if ev.Attention != nil {
			return "· wait " + ev.Attention.Kind + " [" + ev.Attention.Status + "]"
		}
		return "· wait"
	case "usage":
		if ev.Usage != nil {
			return "· usage " + ev.Usage.String()
		}
		return "· usage"
	case "stop":
		return "— stop (" + ev.StopReason + ")"
	case "error":
		return "! error: " + oneLine(ev.Error)
	case "decision":
		if ev.Decision != nil {
			return "· auto " + oneLine(ev.Decision.Title) + " → " + oneLine(ev.Decision.Selected.Name)
		}
		return "· decision"
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
	return strconv.Itoa(n)
}
