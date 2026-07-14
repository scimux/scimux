package acp

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/transcript"
)

// Event is one record of the append-only ACP session log
// (~/.scimux/acp/<node-id>.jsonl). It mirrors the plan's schema: one record
// per observed event, with only the fields relevant to that record's type set.
// The log is the authoritative history for an ACP node — there is no pane
// photo — so a reader reconstructs the chat, liveness, usage, and peek from it.
type Event struct {
	T          string      `json:"t"` // "user" | "assistant" | "tool" | "usage" | "stop" | "error"
	Time       string      `json:"time"`
	Text       string      `json:"text,omitempty"`       // user / assistant
	Tool       *ToolEvent  `json:"tool,omitempty"`       // tool
	Usage      *UsageEvent `json:"usage,omitempty"`      // usage
	StopReason string      `json:"stopReason,omitempty"` // stop
	Error      string      `json:"error,omitempty"`      // error
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

// logWriter serializes append-only writes to one node's log file. Writes are
// small and infrequent (turn boundaries, tool state, usage), so a fresh
// O_APPEND handle per write keeps the file crash-safe without a long-lived fd.
type logWriter struct {
	mu   sync.Mutex
	path string
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (w *logWriter) append(ev Event) error {
	if ev.Time == "" {
		ev.Time = nowStamp()
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// 0600: rawInput and prompt text are as sensitive as pane-excerpt evidence.
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// readEvents replays the log file. Defensive like the transcript parser:
// unreadable file yields nothing, unparseable lines are skipped.
func readEvents(path string) []Event {
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

// readTurns yields the chat turns (user/assistant) from the log — the ACP
// equivalent of transcript.Tailer.Poll for tmux nodes.
func readTurns(path string) []transcript.Turn {
	turns := []transcript.Turn{}
	for _, ev := range readEvents(path) {
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

// latestUsage folds the log's usage records into the newest known values.
// used/size feed the context gauge; either is 0 when never reported (pi).
func latestUsage(path string) (used, size int64) {
	for _, ev := range readEvents(path) {
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

// peekLog renders the tail of the raw event log as plain text — the ACP
// analogue of a pane photo (no terminal to capture). At most maxLines lines.
func peekLog(path string, maxLines int) string {
	evs := readEvents(path)
	var lines []string
	for _, ev := range evs {
		lines = append(lines, formatEvent(ev))
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	if len(lines) == 0 {
		return "(no ACP events yet)"
	}
	return strings.Join(lines, "\n")
}

func formatEvent(ev Event) string {
	switch ev.T {
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
	return strings.TrimSpace(jsonNum(n))
}

func jsonNum(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
