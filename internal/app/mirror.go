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
// captured identically to prompts sent through the web UI. Since phase 3 of
// the consolidation the mirror is also the chat's supply line: handleChat
// renders every transport from the session log (current segment = everything
// after the last source seam), so the log trails the transcript by at most
// one poll tick and the tailer serves only mechanics (needs-input,
// staleness, delivery confirmation).
package app

import (
	"os"
	"path/filepath"
	"strings"

	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// mirror is one node's projection state. All fields are owned by the poller
// goroutine: syncMirror is only called from poll (or warmStartup's one-shot
// poll), and a.mirrors[id] entries are created only on that path under a.mu.
// retireTranscript and removeNodeLocked deliberately delete map entries;
// nothing here needs a lock beyond the app-level map access.
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
	// lastTurnID is the TurnID of the last emitted usage record. Together with
	// lastUsed/lastWin it forms the emission gate: occupancy change still
	// fires, and a new TurnID at the same occupancy also fires so Claude fare
	// does not drop consecutive turns with an identical Used snapshot
	// (Phase 7 bundled fix; ReadFare dedups by TurnID so extras are harmless).
	lastTurnID string
	// tsize is the transcript byte size mirrored so far — the restart fast
	// path's watermark. Recovered from the last "mark" record and re-persisted
	// as mirroring advances; a stat equal to it means "nothing new, skip".
	tsize int64
}

// syncMirror advances a tmux node's session log to match its transcript.
// Called once per poll tick; every step is incremental and cheap when nothing
// changed. Structured-transport nodes never reach this (poll continues before
// the tmux branch).
func (a *app) syncMirror(n *Node) {
	if a.sessionsDir == "" {
		return // no session store configured (bare test apps)
	}
	a.mu.Lock()
	if a.mirrors == nil {
		a.mirrors = map[string]*mirror{}
	}
	m := a.mirrors[n.ID]
	if m == nil {
		m = &mirror{}
		a.mirrors[n.ID] = m
	}
	logPath := a.sessionLogPath(n.ID)
	tpath := n.Transcript
	a.mu.Unlock()

	if tpath == "" {
		return // no transcript linked yet; nothing to mirror, create no log
	}

	// Recover the durable watermark before touching the transcript. The log is
	// the only persistent mirror state, and reading it here — our own compact
	// JSONL, not the agent's large transcript — is what lets an unchanged
	// transcript skip the far more expensive tailer parse on restart. Re-run
	// when the log path changes (a retitled node's slug moves the file).
	if m.logw == nil || m.logw.Path != logPath {
		m.logw = &sessionlog.Writer{Path: logPath}
		st := replayMirrorState(logPath)
		m.path, m.mirrored, m.lastUsed, m.lastWin, m.lastTurnID, m.tsize =
			st.path, st.mirrored, st.used, st.win, st.turnID, st.size
		if !st.hasMeta {
			if err := m.logw.Append(sessionlog.NewMeta(n.ID, n.Agent, n.Model, n.Effort, n.Dir)); err != nil {
				m.logw = nil // retry next tick; don't advance state past a failed write
				return
			}
		}
	}

	// Fast path: transcripts are append-only, so a size still equal to the last
	// mirrored size means there is nothing new — one stat, no tailer, no parse.
	// This is the restart win: settled nodes (the common case) cost a stat each
	// instead of a full-file re-parse. A grown or shrunken file falls through to
	// the full parse below (correctness over the speed-up on change).
	if fi, err := os.Stat(tpath); err == nil && tpath == m.path && fi.Size() == m.tsize {
		return
	}

	tl := a.tailerFor(n)
	if tl == nil {
		return
	}
	turns := tl.Poll()
	used, win := tl.Usage()
	bd := tl.UsageBreakdown()
	m.sync(a, n, tl.Path, turns, used, win, bd)
}

// sync appends any new turns/usage to the log and re-persists the transcript
// size watermark. The caller has already recovered durable state and confirmed
// the transcript changed (or is new). Fields are owned by the poller goroutine.
func (m *mirror) sync(a *app, n *Node, tpath string, turns []transcript.Turn, used, win int64, bd transcript.UsageBreakdown) {
	if tpath == "" {
		return
	}
	// A different transcript file (first link, /clear rollover, corrected
	// adoption, relink) or a rebuilt one (rotation: the tailer reset and now
	// reports fewer turns than we mirrored) starts a new source segment.
	if tpath != m.path || len(turns) < m.mirrored {
		sid := strings.TrimSuffix(filepath.Base(tpath), ".jsonl")
		if err := m.logw.Append(sessionlog.NewSource(tpath, sid)); err != nil {
			return
		}
		m.path, m.mirrored, m.tsize = tpath, 0, 0
	}
	for _, t := range turns[m.mirrored:] {
		// Role is "user"/"assistant" (transcript.ParseLine yields nothing
		// else), matching the log's record types; the transcript's own
		// timestamp rides along.
		if err := m.logw.Append(sessionlog.Event{T: t.Role, Text: t.Text, Time: t.Time}); err != nil {
			return // watermark stays; the failed turn is retried next tick
		}
		m.mirrored++
		// Phase 4 ingestion point: tmux transcripts carry only chat text (the
		// tailer discards tool records, see transcript.ParseLine), so only
		// Markdown-mention scanning applies here — the tool-call scanner is
		// exercised by the ACP/codex managers, which log ToolEvent records
		// directly. Runs at turn-append time, matching the eager-ingestion
		// rule (upload-design.md, "Ingestion Timing").
		if a.assetHook != nil {
			if cands := asset.ScanMarkdown(t.Text); len(cands) > 0 {
				a.assetHook(n.ID, n.Dir, cands)
			}
		}
	}
	// Emission gate: occupancy change (used/win) OR a new TurnID at the same
	// occupancy. Phase 3 only gated on occupancy, which dropped Claude turns
	// whose Used snapshot matched the previous turn; Phase 7 widens the gate
	// so fare is complete. ReadFare dedups by TurnID — extra emissions safe.
	if used > 0 && (used != m.lastUsed || win != m.lastWin || bd.TurnID != m.lastTurnID) {
		if err := m.logw.Append(sessionlog.Event{T: "usage", Usage: &sessionlog.UsageEvent{
			Used:                int(used),
			Size:                int(win),
			InputTokens:         int(bd.InputTokens),
			OutputTokens:        int(bd.OutputTokens),
			CachedReadTokens:    int(bd.CachedReadTokens),
			CacheCreationTokens: int(bd.CacheCreationTokens),
			TurnID:              bd.TurnID,
		}}); err != nil {
			return
		}
		m.lastUsed, m.lastWin, m.lastTurnID = used, win, bd.TurnID
	}
	// Persist the consumed transcript size so a restart can skip this file if it
	// has not grown. Only when it advances, so a settled node writes no marks;
	// stat again here (rather than trust the tailer offset) so the watermark is
	// exactly what the restart fast path compares against.
	if fi, err := os.Stat(tpath); err == nil && fi.Size() > m.tsize {
		if err := m.logw.Append(sessionlog.NewMark(fi.Size())); err != nil {
			return
		}
		m.tsize = fi.Size()
	}
}

// mirrorState is what replayMirrorState recovers from an existing log: the
// last source segment and how far mirroring got within it.
type mirrorState struct {
	hasMeta  bool
	path     string
	mirrored int
	tools    int // tool records in the current source segment (V2-P2 watermark)
	used     int64
	win      int64
	turnID   string // last usage TurnID in the current segment (emission gate)
	size     int64  // transcript byte size at the last mark of the current segment
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
				// a new segment resets the size watermark with the turn count
				st.path, st.mirrored, st.size = ev.Source.Path, 0, 0
			}
		case "user", "assistant":
			st.mirrored++
		case "usage":
			if ev.Usage != nil {
				st.used, st.win = int64(ev.Usage.Used), int64(ev.Usage.Size)
				st.turnID = ev.Usage.TurnID
			}
		case "mark":
			if ev.Mark != nil {
				st.size = ev.Mark.Off
			}
		}
	}
	// A log that has meta but never a source (shouldn't occur for mirrored
	// nodes) resumes with no bound path; the next sync writes a source seam.
	return st
}
