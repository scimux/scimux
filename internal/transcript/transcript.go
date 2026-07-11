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
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// Tailer incrementally reads one transcript file, remembering its byte
// offset between polls and buffering partial trailing lines.
type Tailer struct {
	Path   string
	Turns  []Turn
	offset int64
	buf    []byte
}

// Poll reads newly appended bytes and returns the accumulated turn list.
// All I/O failures are absorbed: the previous turn list is returned and the
// next poll retries. A shrunken file (rotation) resets the tailer.
func (t *Tailer) Poll() []Turn {
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
		t.offset, t.buf, t.Turns = 0, nil, nil
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
	esc := regexp.MustCompile(`[^a-zA-Z0-9-]`).ReplaceAllString(dir, "-")
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", esc, "*.jsonl"))
	if err != nil || len(matches) == 0 {
		return "", "", false
	}
	var bestTime time.Time
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil {
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

// FindCodexRollout locates the newest Codex rollout file under root
// (normally ~/.codex/sessions) created at or after since (with slack for
// clock skew) whose recorded cwd matches dir. Codex offers no way to pin a
// session id at launch, so discovery-by-cwd-and-time is the correlation.
func FindCodexRollout(root, dir string, since time.Time) (string, bool) {
	var best string
	var bestTime time.Time
	slack := since.Add(-10 * time.Second)
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(slack) {
			return nil
		}
		if rolloutCwd(path) != dir {
			return nil
		}
		if best == "" || info.ModTime().After(bestTime) {
			best, bestTime = path, info.ModTime()
		}
		return nil
	})
	return best, best != ""
}

// rolloutCwd extracts the working directory recorded in a rollout file's
// leading session_meta record, checking the first few lines defensively.
func rolloutCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 64*1024)
	n, _ := f.Read(head)
	for i, line := range bytes.Split(head[:n], []byte("\n")) {
		if i >= 5 {
			break
		}
		var rec struct {
			Payload struct {
				Cwd  string `json:"cwd"`
				Meta struct {
					Cwd string `json:"cwd"`
				} `json:"meta"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec.Payload.Cwd != "" {
			return rec.Payload.Cwd
		}
		if rec.Payload.Meta.Cwd != "" {
			return rec.Payload.Meta.Cwd
		}
	}
	return ""
}
