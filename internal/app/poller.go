// poller.go — mechanical poll loop: pane liveness, needs-input attention,
// transcript discovery/relink, animation geometry, and tailer ownership.
// Session-log projection lives in mirror.go; state HTTP projection lives in
// state_api.go.
package app

import (
	"fmt"
	"os"
	"strings"
	"time"

	"codeberg.org/chrberger/scimux/internal/dialoghint"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// Lock ownership for the poll path (existing behavior; do not change locking
// merely to match this comment):
//
//   - a.mu protects node fields and poll-state maps (live, attn, attnAt,
//     prevCap, lastChg, activeSince, anim, chatMark, staleChat, tailers,
//     pathClaims, mirrors).
//   - tmux capture, transcript/filesystem work, appendRecord/store work, and
//     session-log/mirror work run without a.mu held.
//   - Durable transcript/store work completes before a.mu is reacquired to
//     publish in-memory links (discoverTranscript, maybeRelinkTranscript).
//   - tailerFor performs catch-up transcript I/O without a.mu, then atomically
//     installs the tailer and progress baseline under a.mu.
//   - noteAnim and pathClaimedLocked require the caller to hold a.mu.

// poll refreshes liveness (mechanical only: pane changed recently, quiet, or
// session gone), detects needs-input, and runs one-time transcript discovery
// for young nodes.
//
// Needs-input = an unresolved tool call in the transcript AND a quiet pane.
// Both halves matter: while a tool actually runs, both TUIs animate a timer
// (pane keeps changing); an approval prompt or question menu is static. This
// keeps detection free of TUI string matching — the transcript side is
// structured data, the pane side is the same byte-compare liveness uses.
func (a *app) poll() {
	a.mu.Lock()
	nodes := make([]*Node, len(a.nodes))
	copy(nodes, a.nodes)
	a.mu.Unlock()

	for _, n := range nodes {
		// Structured-protocol nodes (ACP, codex app-server) carry no tmux pane:
		// liveness and needs-input come from the manager's structured state
		// (process alive, turn in flight, pending permission), not pane-change
		// detection. No capture, no transcript discovery.
		if pm := a.proc(n); pm != nil {
			attn := pm.Attention(n.ID)
			a.mu.Lock()
			prevAttn := a.attn[n.ID]
			a.live[n.ID] = pm.Live(n.ID)
			a.attn[n.ID] = attn
			if a.live[n.ID] == "active" {
				a.lastChg[n.ID] = time.Now()
			}
			a.mu.Unlock()
			// V2-P2: persist needs-input start→end edges from the existing
			// mechanical Attention() signal only — no new regex / source.
			a.persistAttentionTransition(n, prevAttn, attn)
			// Phase 4: re-source pi fare from the native JSONL (session-map
			// join). Other structured agents keep ACP/app-server usage only.
			if n.Agent == "pi" {
				a.syncPiFare(n)
			}
			continue
		}
		s := a.server.Session(n.ID)
		a.mu.Lock()
		prev := a.live[n.ID] // not yet overwritten this tick
		a.mu.Unlock()
		state := "exited"
		if s.Alive() {
			cap, err := s.Capture()
			if err == nil {
				a.mu.Lock()
				if pc := a.prevCap[n.ID]; cap != pc {
					a.noteAnim(n.ID, pc, cap)
					a.prevCap[n.ID] = cap
					a.lastChg[n.ID] = time.Now()
				}
				if time.Since(a.lastChg[n.ID]) < 8*time.Second {
					state = "active"
				} else {
					state = "quiet"
				}
				if state == "active" && prev != "active" {
					if a.activeSince == nil {
						a.activeSince = map[string]time.Time{}
					}
					a.activeSince[n.ID] = time.Now()
				}
				a.mu.Unlock()
			} else {
				state = "unavailable"
			}
		}
		attn := ""
		// freshAttn distinguishes attention classified from evidence this tick
		// from attention merely preserved across an indeterminate tick: only the
		// former (re)stamps attnAt, so the preserve window ages out (R21.2).
		freshAttn := false
		if state == "quiet" {
			tl := a.tailerFor(n)
			if tl != nil {
				tl.Poll()
				if name, ok := tl.WaitingOn(); ok {
					attn = attentionKind(name)
				}
				// Judge the transcript only across a whole working phase (the
				// active→quiet transition, finding 21); on ordinary quiet ticks
				// the mark still advances — establishing the baseline before
				// the next phase — and newly recognized chat progress clears a
				// stale flag immediately, without waiting for another activity
				// cycle (finding 24).
				off, prog := tl.Progress()
				a.noteChatProgress(n.ID, off, prog, prev == "active")
			}
			// Parallel regex-based dialog detection. Independent of structured
			// transcript parsing; fires on terminal-only harnesses, format
			// changes, or discovery failures. Only runs on quiet panes, only
			// examines visible screen (no scrollback mixed in).
			if attn == "" {
				if visible, err := s.CaptureVisible(); err == nil {
					if dialoghint.ClassifyVisible(visible) {
						attn = "dialog"
					}
				}
			}
			// Neutral needs-a-look state: the pane is quiet but there is no
			// trustworthy structured transcript to say whether the agent
			// finished or sits on a dialog this parser cannot see — no
			// transcript at all (terminal-only harnesses, discovery pending),
			// a stale one, or one whose recent data stopped parsing. Never
			// labeled question/approval (no structured evidence, and pane
			// regexes stay forbidden); the UI shows the terminal instead.
			if attn == "" {
				a.mu.Lock()
				noEvidence := n.Transcript == "" || a.staleChat[n.ID]
				a.mu.Unlock()
				if noEvidence || (tl != nil && tl.Unparseable()) {
					attn = "inspect"
				}
			}
			freshAttn = attn != "" // every quiet-branch assignment is fresh evidence
		} else if state == "active" {
			// Active-pane needs-input: only when the transcript shows an
			// unresolved call AND the pane's changes are confined to an
			// animation strip is one visible-capture spent on the dialoghint
			// matchers. A streaming pane never gets here (noteAnim cleared
			// the state), so running tools cost nothing and cannot false-fire
			// — their pane doesn't render an approval dialog. If the matcher
			// stays dark long enough while the transcript stalls, the wait
			// degrades to the neutral "inspect", never a classified dialog.
			if tl := a.tailerFor(n); tl != nil {
				tl.Poll()
				if name, ok := tl.WaitingOn(); ok {
					off, _ := tl.Progress()
					a.mu.Lock()
					st := a.anim[n.ID]
					var stalledSince time.Time
					if st != nil {
						if st.off != off {
							// Transcript progressed: the agent is producing,
							// not waiting. Restart the stall window.
							st.off, st.since = off, time.Now()
						}
						stalledSince = st.since
					}
					a.mu.Unlock()
					if st != nil {
						if visible, err := s.CaptureVisible(); err == nil && dialoghint.ClassifyVisible(visible) {
							attn = attentionKind(name)
						} else if time.Since(stalledSince) >= animStallAfter {
							attn = "inspect"
						}
					}
					freshAttn = attn != ""
					// While the call stays unresolved and this tick produced no
					// fresh classification, the check is indeterminate (most
					// concretely: a full-pane redraw deleted the anim state, so
					// st == nil) — preserve attention already set, e.g. by the
					// one-shot peek path, instead of wiping it a tick after a
					// human-confirmed dialog (R20.5). But bound the preservation:
					// a tool call stays unresolved until the tool *completes*
					// (both CLIs write the result record only then), and an
					// executing tool streams — deleting the anim state and making
					// every tick indeterminate. Without a bound, approving a
					// 3-minute Bash call in the terminal would leave the card on
					// "approval" for all 3 minutes (R21.2). Only preserve while
					// the last fresh classification is younger than animStallAfter;
					// a web-key answer clears it outright in handleKey.
					if attn == "" {
						a.mu.Lock()
						if prevAttn := a.attn[n.ID]; prevAttn != "" && time.Since(a.attnAt[n.ID]) < animStallAfter {
							attn = prevAttn
						}
						a.mu.Unlock()
					}
				}
			}
		}
		a.mu.Lock()
		prevAttn := a.attn[n.ID]
		a.live[n.ID] = state
		if freshAttn && attn != "" {
			if a.attnAt == nil { // tests build app literals without the map
				a.attnAt = map[string]time.Time{}
			}
			a.attnAt[n.ID] = time.Now()
		}
		a.attn[n.ID] = attn
		a.mu.Unlock()
		// V2-P2: durable wait edges from the existing mechanical needs-input
		// signal (WaitingOn + quiet/confined-anim; no new regex).
		a.persistAttentionTransition(n, prevAttn, attn)
		// A whole working phase just ended: if the linked transcript never
		// carried it, the pane's claude has moved to a new session file
		// (/clear, relaunch) — re-run discovery. Mechanical signal only.
		if n.Agent == "claude" && prev == "active" && state == "quiet" {
			a.maybeRelinkTranscript(n)
		}
		a.discoverTranscript(n)
		a.syncMirror(n)
	}
}

// persistAttentionTransition appends a session-log attention start/end edge
// when the mechanical needs-input signal transitions empty↔non-empty.
// Append-only; never rewrites. No-op when sessionsDir is unset (bare tests)
// or the signal is unchanged. Reuses only the existing attention class —
// never invents a new source (fare-design.md V2-P2, v2.5 ⑦).
func (a *app) persistAttentionTransition(n *Node, prev, next string) {
	if a.sessionsDir == "" || n == nil || prev == next {
		return
	}
	var ev sessionlog.Event
	switch {
	case prev == "" && next != "":
		ev = sessionlog.NewAttentionEdge(next, "start")
	case prev != "" && next == "":
		ev = sessionlog.NewAttentionEdge(prev, "end")
	default:
		// Kind change while still needing input (e.g. inspect→approval):
		// close the old interval and open the new one so wait union stays
		// honest. Two appends, still append-only.
		if err := a.appendSessionEvent(n.ID, sessionlog.NewAttentionEdge(prev, "end")); err != nil {
			return
		}
		ev = sessionlog.NewAttentionEdge(next, "start")
	}
	_ = a.appendSessionEvent(n.ID, ev)
}

// appendSessionEvent appends one record to the node's session log. Creates
// no meta/source — callers only fire after a log already exists (or the
// write is best-effort for wait edges on a young node).
func (a *app) appendSessionEvent(id string, ev sessionlog.Event) error {
	if a.sessionsDir == "" {
		return nil
	}
	w := &sessionlog.Writer{Path: a.sessionLogPath(id)}
	return w.Append(ev)
}

// maybeRelinkTranscript re-runs transcript discovery for a tmux claude node
// whose pane just finished a working phase the linked transcript did not
// carry. Claude Code starts a new session file on /clear or a relaunch inside
// the same pane; the old link then points at a file that stops growing, the
// chat silently freezes on the last linked conversation, and — because the
// store is replayed at startup — restarting scimux does not recover. The
// judgment is mechanical (pane went active→quiet while the linked file's
// mtime stayed before the phase start); a wrong or missing guess leaves peek
// and send working exactly as at adoption. Also links a node that never got
// a transcript (adoption guess failed) once its pane completes a phase.
func (a *app) maybeRelinkTranscript(n *Node) {
	a.mu.Lock()
	cur := n.Transcript
	since, ok := a.activeSince[n.ID]
	a.mu.Unlock()
	if !ok {
		return
	}
	// Transcript writes can precede the first observed pane change by up to a
	// poll tick; pad the phase-start watermark.
	since = since.Add(-10 * time.Second)
	if cur != "" {
		if st, err := os.Stat(cur); err == nil && st.ModTime().After(since) {
			return // the linked file carried this phase; the link is healthy
		}
	}
	// Prefer the pane process's own session id (deterministic even with many
	// sessions in one directory) — but only when the file it names carried
	// the phase that just ended. The cmdline holds the id claude was
	// *launched* with; after an in-pane /clear the process keeps that argv
	// while writing a brand-new session file, so a stale cmdline id must fall
	// through to the newest-file heuristic instead of relinking the dead
	// pre-/clear transcript (which would blind needs-input for good: the
	// dead file never grows, so no pending call and no staleness signal ever
	// appear). The fallback is ambiguous only when two panes in the same
	// directory finish concurrently, and path exclusivity bounds that damage.
	var path, sid string
	if pid, err := a.server.Session(n.ID).PanePID(); err == nil {
		if got := a.paneSessionID(pid); got != "" {
			if p, ok := transcript.FindClaudeTranscript(a.home, got); ok {
				if st, err := os.Stat(p); err == nil && st.ModTime().After(since) {
					path, sid = p, got
				}
			}
		}
	}
	if path == "" {
		if p, s, ok := transcript.FindClaudeNewestInDirSince(a.home, n.Dir, since); ok {
			path, sid = p, s
		}
	}
	if path == "" || path == cur {
		return
	}
	a.mu.Lock()
	if a.pathClaimedLocked(path, n.ID) {
		a.mu.Unlock()
		return
	}
	a.pathClaims[path] = true
	a.mu.Unlock()
	// Record first, publish second — same contract as discoverTranscript.
	if err := a.appendRecord(storeRecord{Type: "transcript", ID: n.ID, Path: path}); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: relink transcript for %s: %v (will retry)\n", n.ID, err)
		a.mu.Lock()
		delete(a.pathClaims, path)
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	n.Transcript = path
	var cp Node
	if sid != "" && sid != n.SessionID {
		n.SessionID = sid
		cp = *n
	}
	delete(a.pathClaims, path)
	a.mu.Unlock()
	// Persist the corrected session id as a fresh node record (append-only:
	// corrections are new records). Best-effort — the transcript record above
	// already carries the link across restarts.
	if cp.ID != "" {
		_ = a.appendRecord(storeRecord{Type: "node", Node: &cp})
	}
}

// The quiet gate is structurally blind to one case: an approval dialog with
// parallel tool calls queued behind it — the queued call's spinner keeps the
// pane "active" for as long as the human stays away (observed live: 6m42s
// unnoticed). The pieces below close it without weakening the mechanical
// core: line-diff geometry decides whether an "active" pane is really a
// static screen with an animation strip, and only then is the dialoghint
// matcher consulted to classify a transcript-evidenced wait. Text corroborates
// structured evidence; it never replaces it and never feeds liveness.

// animMaxLines: pane changes confined to this many distinct lines across
// ticks count as an in-place animation strip (spinner + timer rows), not
// real output. animStallAfter: how long a confined-animated pane with an
// unresolved tool call and a stalled transcript waits before the neutral
// "inspect" backstop fires — the safety net for dialogs the matcher no
// longer recognizes after a TUI rewording.
const animMaxLines = 3
const animStallAfter = 90 * time.Second

type animState struct {
	lines []int     // union of line indices seen changing, sorted
	since time.Time // when changes became confined; restarts on transcript growth
	off   int64     // transcript offset backing the stall window; -1 until read
}

// noteAnim classifies one pane change: streaming or redrawing output touches
// many lines and clears the state; an animation strip touches the same few
// lines tick after tick. Mechanical only — diff geometry, never text.
// Callers hold a.mu.
func (a *app) noteAnim(id, prev, cur string) {
	if prev == "" {
		return // first observation: no baseline to diff against
	}
	idx := changedLines(prev, cur, animMaxLines+1)
	if st := a.anim[id]; st != nil {
		if merged := unionInts(st.lines, idx); len(merged) <= animMaxLines {
			st.lines = merged
			return
		}
	}
	if len(idx) <= animMaxLines {
		if st := a.anim[id]; st != nil {
			// Re-confining after a union spill: the animation strip drifted
			// position (elapsed-time rewrapping, queue reordering) but this
			// tick's diff is still confined — the same wait continues. Carry
			// since and off over; resetting them would let a strip that
			// drifts more often than animStallAfter postpone the inspect
			// backstop forever (R20.6).
			st.lines = idx
			return
		}
		if a.anim == nil { // tests build app literals without the map
			a.anim = map[string]*animState{}
		}
		a.anim[id] = &animState{lines: idx, since: time.Now(), off: -1}
	} else {
		delete(a.anim, id)
	}
}

// changedLines reports the indices of lines that differ between two
// captures, giving up after max entries (the caller only distinguishes
// "confined" from "not confined").
func changedLines(prev, cur string, max int) []int {
	po, co := strings.Split(prev, "\n"), strings.Split(cur, "\n")
	n := len(po)
	if len(co) > n {
		n = len(co)
	}
	var idx []int
	for i := 0; i < n && len(idx) < max; i++ {
		var p, c string
		if i < len(po) {
			p = po[i]
		}
		if i < len(co) {
			c = co[i]
		}
		if p != c {
			idx = append(idx, i)
		}
	}
	return idx
}

// unionInts merges two sorted int slices without duplicates.
func unionInts(a, b []int) []int {
	out := make([]int, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case j == len(b) || (i < len(a) && a[i] < b[j]):
			out = append(out, a[i])
			i++
		case i == len(a) || b[j] < a[i]:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

// attentionKind classifies what the agent is waiting for, from the name of
// its unresolved tool call: Claude logs explicit question tools; everything
// else (Bash, Edit, codex exec_command, …) is a permission prompt.
func attentionKind(tool string) string {
	switch tool {
	case "AskUserQuestion", "ExitPlanMode":
		return "question"
	}
	return "approval"
}

func (a *app) discoverTranscript(n *Node) {
	a.mu.Lock()
	done := n.Transcript != ""
	a.mu.Unlock()
	if done {
		return
	}
	// Only claude (tmux) nodes discover a transcript file. The structured
	// transports keep their history in the manager's session log and never
	// reach here (the poller continues past them before discovery). An adopted
	// claude node without a known id already made its one guess at adoption.
	if n.Agent != "claude" || n.SessionID == "" {
		return
	}
	created, err := time.Parse(time.RFC3339, n.CreatedAt)
	if err != nil || time.Since(created) > 15*time.Minute {
		return // peek remains available
	}
	// Transcript paths are exclusive among nodes.
	excluded := map[string]bool{}
	a.mu.Lock()
	for _, o := range a.nodes {
		if o.ID != n.ID && o.Transcript != "" {
			excluded[o.Transcript] = true
		}
	}
	for p := range a.pathClaims {
		excluded[p] = true
	}
	a.mu.Unlock()
	path, ok := transcript.FindClaudeTranscript(a.home, n.SessionID)
	if !ok || excluded[path] {
		return
	}
	// Reserve the path under the lock before the store write, re-checking
	// against live state: a concurrent adoption may have published it since
	// the snapshot above.
	a.mu.Lock()
	if a.pathClaimedLocked(path, n.ID) {
		a.mu.Unlock()
		return
	}
	a.pathClaims[path] = true
	a.mu.Unlock()
	// Record first, publish second: if the store write fails, the in-memory
	// path stays empty and the poller retries on the next tick instead of
	// treating an unpersisted link as final.
	if err := a.appendRecord(storeRecord{Type: "transcript", ID: n.ID, Path: path}); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: record transcript for %s: %v (will retry)\n", n.ID, err)
		a.mu.Lock()
		delete(a.pathClaims, path)
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	n.Transcript = path
	delete(a.pathClaims, path)
	a.mu.Unlock()
}

// chatMark is one node's transcript progress as of the last active→quiet
// pane transition: bytes consumed and agent-side records seen.
type chatMark struct {
	seen bool
	off  int64
	prog int
}

// noteChatProgress updates the stale-transcript signal. Bytes that advanced
// over a whole active phase (judge is true exactly at the active→quiet
// transition) without one recognized *agent-side* record mean the linked
// file no longer carries an interpretable response — a typed future format
// the structural health check cannot see — so the UI degrades to peek
// alongside the retained turns. The progress count is directional by
// construction (transcript.Tailer counts only understood assistant messages
// and tool calls/results): a phase's own user prompt, meta records, or
// injected scaffolding cannot vouch for an assistant format that changed.
// Recognized progress clears the signal the moment it arrives, even if the
// pane never becomes active again (a record split across polls completes
// while quiet). Ordinary quiet ticks otherwise only advance the mark, so
// each phase is judged against a pre-phase baseline. Anchoring the stale
// judgment to pane activity cycles (not per-line thresholds) is what keeps
// benign non-chat records, however many, from ever tripping it on their own.
func (a *app) noteChatProgress(id string, off int64, prog int, judge bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.chatMark[id]
	switch {
	case m.seen && prog > m.prog:
		a.staleChat[id] = false
	case m.seen && judge && off > m.off:
		a.staleChat[id] = true
	}
	a.chatMark[id] = chatMark{seen: true, off: off, prog: prog}
}

// pathClaimedLocked reports whether a transcript path is already owned by
// another node or reserved by an in-flight discovery. Callers hold a.mu.
func (a *app) pathClaimedLocked(path, excludeID string) bool {
	if a.pathClaims[path] {
		return true
	}
	for _, o := range a.nodes {
		if o.ID != excludeID && o.Transcript == path {
			return true
		}
	}
	return false
}

// tailerFor returns the node's transcript tailer, (re)building it when the
// transcript appears or is relinked to a different file (e.g. a corrected
// adoption guess). Callers must not hold a.mu: a fresh tailer first consumes
// the file's existing history outside the lock, and its baseline is taken
// from that catch-up read — pre-link history is never attributed to the
// current pane phase, so the first active→quiet judgment after a link or a
// scimux restart sees only bytes and records that arrived afterwards
// (finding 28). The built tailer is installed atomically together with its
// watermark.
func (a *app) tailerFor(n *Node) *transcript.Tailer {
	a.mu.Lock()
	path := n.Transcript
	tl := a.tailers[n.ID]
	a.mu.Unlock()
	if path == "" {
		return nil
	}
	if tl != nil && tl.Path == path {
		return tl
	}
	nt := &transcript.Tailer{Path: path}
	nt.Poll() // catch up on existing history: the baseline, not phase progress
	off, prog := nt.Progress()
	a.mu.Lock()
	defer a.mu.Unlock()
	if n.Transcript != path {
		return nil // relinked while reading history; the next call rebuilds
	}
	if cur := a.tailers[n.ID]; cur != nil && cur.Path == path {
		return cur // a concurrent caller finished building first
	}
	a.tailers[n.ID] = nt
	// Staleness measured against the old file says nothing about this one.
	a.chatMark[n.ID] = chatMark{seen: true, off: off, prog: prog}
	delete(a.staleChat, n.ID)
	return nt
}
