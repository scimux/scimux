// mirror.go — the transcript mirror: projects what the tailer extracts from a
// tmux node's agent-CLI transcript (Claude session logs, adopted Codex
// rollouts) into the node's unified session log under <data>/sessions/. This
// is what makes every harness equal on disk: structured transports write the
// log directly as their primary history; tmux transports arrive at the same
// file, same schema, through this mirror. Readers (search, consolidation,
// sharing) never learn which transport produced a file.
//
// The mirror is write-only and transcript-driven — it ingests only from the
// tailer (never from handleSend), so prompts typed into an attached pane are
// captured identically to prompts sent through the web UI. The UI's read
// path is untouched: chat still renders straight from the tailer.
package main

import (
	"os"
	"path/filepath"
	"strings"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// mirror is one node's projection state. All fields are owned by the poller
// goroutine (syncMirror is only called from poll); nothing here needs a lock
// beyond the app-level map access.
type mirror struct {
	logw *sessionlog.Writer
	// path is the transcript file the current source segment mirrors from;
	// mirrored counts the turns of that segment already in the log. Both are
	// recovered from the log itself on the first sync after a restart, so a
	// restart can never duplicate or drop turns.
	path     string
	mirrored int
	lastUsed int64
	lastWin  int64
}

// syncMirror advances a tmux node's session log to match its transcript.
// Called once per poll tick; every step is incremental and cheap when nothing
// changed. Structured-transport nodes never reach this (poll continues before
// the tmux branch).
func (a *app) syncMirror(n *Node) {
	if a.sessionsDir == "" {
		return // no session store configured (bare test apps)
	}
	tl := a.tailerFor(n)
	if tl == nil {
		return // no transcript linked yet; nothing to mirror
	}
	turns := tl.Poll()
	used, win := tl.Usage()

	a.mu.Lock()
	if a.mirrors == nil {
		a.mirrors = map[string]*mirror{}
	}
	m := a.mirrors[n.ID]
	if m == nil {
		m = &mirror{}
		a.mirrors[n.ID] = m
	}
	logPath := filepath.Join(a.sessionsDir, n.ID+".jsonl")
	a.mu.Unlock()

	m.sync(logPath, n, tl.Path, turns, used, win)
}

func (m *mirror) sync(logPath string, n *Node, tpath string, turns []transcript.Turn, used, win int64) {
	if tpath == "" {
		return
	}
	// First sync (process start or a node whose log path we haven't touched):
	// recover the watermark from the log itself.
	if m.logw == nil || m.logw.Path != logPath {
		m.logw = &sessionlog.Writer{Path: logPath}
		st := replayMirrorState(logPath)
		m.path, m.mirrored, m.lastUsed, m.lastWin = st.path, st.mirrored, st.used, st.win
		if !st.hasMeta {
			if err := m.logw.Append(sessionlog.NewMeta(n.ID, n.Agent, n.Model, n.Dir)); err != nil {
				m.logw = nil // retry next tick; don't advance state past a failed write
				return
			}
		}
	}
	// A different transcript file (first link, /clear rollover, corrected
	// adoption, relink) or a rebuilt one (rotation: the tailer reset and now
	// reports fewer turns than we mirrored) starts a new source segment.
	if tpath != m.path || len(turns) < m.mirrored {
		sid := strings.TrimSuffix(filepath.Base(tpath), ".jsonl")
		if err := m.logw.Append(sessionlog.NewSource(tpath, sid)); err != nil {
			return
		}
		m.path, m.mirrored = tpath, 0
	}
	for _, t := range turns[m.mirrored:] {
		// Role is "user"/"assistant" (transcript.ParseLine yields nothing
		// else), matching the log's record types; the transcript's own
		// timestamp rides along.
		if err := m.logw.Append(sessionlog.Event{T: t.Role, Text: t.Text, Time: t.Time}); err != nil {
			return // watermark stays; the failed turn is retried next tick
		}
		m.mirrored++
	}
	if used > 0 && (used != m.lastUsed || win != m.lastWin) {
		if err := m.logw.Append(sessionlog.Event{T: "usage", Usage: &sessionlog.UsageEvent{
			Used: int(used), Size: int(win),
		}}); err != nil {
			return
		}
		m.lastUsed, m.lastWin = used, win
	}
}

// mirrorState is what replayMirrorState recovers from an existing log: the
// last source segment and how far mirroring got within it.
type mirrorState struct {
	hasMeta  bool
	path     string
	mirrored int
	used     int64
	win      int64
}

// replayMirrorState derives the mirror's resume point from the log file —
// the log is the only durable watermark, so restarts are idempotent by
// construction. A missing or unreadable file yields the zero state (fresh
// log, meta pending).
func replayMirrorState(logPath string) mirrorState {
	var st mirrorState
	if _, err := os.Stat(logPath); err != nil {
		return st
	}
	for _, ev := range sessionlog.ReadEvents(logPath) {
		switch ev.T {
		case "meta":
			st.hasMeta = true
		case "source":
			if ev.Source != nil {
				st.path, st.mirrored = ev.Source.Path, 0
			}
		case "user", "assistant":
			st.mirrored++
		case "usage":
			if ev.Usage != nil {
				st.used, st.win = int64(ev.Usage.Used), int64(ev.Usage.Size)
			}
		}
	}
	// A log that has meta but never a source (shouldn't occur for mirrored
	// nodes) resumes with no bound path; the next sync writes a source seam.
	return st
}
