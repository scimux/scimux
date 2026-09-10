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
//   - a.mu protects node fields and poll-state maps (live, attn, turnDone,
//     attnAt, prevCap, lastChg, activeSince, anim, chatMark, staleChat,
//     tailers, pathClaims, mirrors).
//   - tmux capture, transcript/filesystem work, appendRecord/store work, and
//     session-log/mirror work run without a.mu held.
//   - Durable transcript/store work completes before a.mu is reacquired to
//     publish in-memory links (discoverTranscript, hook-driven binding).
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

	// One list-sessions for the whole tick answers liveness for every tmux
	// node (set lookup), replacing N has-session forks. A failed snapshot
	// must not mass-flip every node to exited: leave their live[] alone for
	// this tick (branch a). Capture still runs only for names present in
	// the set; its own error still distinguishes unavailable from exited.
	sessionSet, sessionsOK := a.sessionSnapshot()
	a.drainClaudeHooks()

	for _, n := range nodes {
		// Structured-protocol nodes (ACP, codex app-server) carry no tmux pane:
		// liveness and needs-input come from the manager's structured state
		// (process alive, turn in flight, pending permission), not pane-change
		// detection. No capture, no transcript discovery.
		if pm := a.proc(n); pm != nil {
			// P3: auto-resolve eligible structured approvals before publishing
			// manual attention. Manager + session-log I/O stay outside a.mu.
			a.maybeAutoApprove(n, pm)

			// Snapshot manager state outside a.mu (HasSession/Live/Attention
			// are in-memory lookups, but keep the "no a.mu across manager
			// calls" discipline for consistency with Send/log paths).
			live := pm.Live(n.ID)
			attn := pm.Attention(n.ID)
			lastErr := pm.LastError(n.ID)
			a.mu.Lock()
			prevAttn := a.attn[n.ID]
			prevLive := a.live[n.ID]
			prevDone := a.turnDone[n.ID]
			a.live[n.ID] = live
			a.attn[n.ID] = attn
			if live == "active" {
				a.lastChg[n.ID] = time.Now()
			}
			a.turnDone[n.ID] = structuredTurnDone(live, attn, prevLive, lastErr, n.EndedAt, prevDone, a.lastChg[n.ID], time.Now())
			// An armed lease cannot cross a turn or a dead process. Every
			// agent turns it off at completion. Do not touch a primed
			// lease merely because the process is not yet up (enable while idle
			// is valid before the first prompt lands). Capture the current
			// LeaseID at decision time; under the gate, act only if that same
			// lease is still current so a completed re-arm is not erased by a
			// stale poller transition.
			disarmLeaseID, settleLeaseID := "", ""
			if live == "exited" && prevLive != "exited" {
				if st := a.autoApprove[n.ID]; st != nil {
					disarmLeaseID = st.LeaseID // process/session loss
				}
			} else if prevLive == "active" && live != "active" {
				if st := a.autoApprove[n.ID]; st != nil {
					settleLeaseID = st.LeaseID // turn completed
				}
			}
			a.mu.Unlock()
			if disarmLeaseID != "" {
				a.disarmAutoApproveIfLease(n.ID, disarmLeaseID)
			}
			if settleLeaseID != "" {
				a.settleAutoApproveAfterTurn(n.ID, settleLeaseID)
			}
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
		if !sessionsOK {
			// Transient list-sessions failure: do not rewrite live/attn for
			// tmux nodes this tick. discoverTranscript/syncMirror can wait.
			continue
		}
		s := a.server.Session(n.ID)
		a.mu.Lock()
		prev := a.live[n.ID] // not yet overwritten this tick
		a.mu.Unlock()
		state := "exited"
		if sessionSet[n.ID] {
			cap, err := s.Capture()
			if err == nil {
				a.mu.Lock()
				if pc := a.prevCap[n.ID]; cap != pc {
					a.noteAnim(n.ID, pc, cap)
					a.prevCap[n.ID] = cap
					a.lastChg[n.ID] = time.Now()
				}
				if time.Since(a.lastChg[n.ID]) < paneQuietAfter {
					state = "active"
				} else {
					state = "quiet"
				}
				if state == "active" && prev != "active" {
					a.activeSince[n.ID] = time.Now()
				}
				a.mu.Unlock()
			} else {
				state = "unavailable"
			}
		}
		// One read per tick of what Claude has actually escalated
		// (claude_asked.go). askCapable means the PermissionRequest hook is
		// authoritative for this node: it fires for every decision Claude needs a
		// human for, so "no standing notice" is positive evidence that no dialog
		// is up — which the pane-geometry backstops cannot know and must not
		// contradict.
		askAttn, askCapable := a.claudeAskState(n)
		claudeDlg := a.claudeVisibleDialog(n)
		a.mu.Lock()
		claudeSup := a.claudeSupervisionOf(n)
		claudeArmed := a.claudeAutoApproveArmedLocked(n)
		a.mu.Unlock()
		claudeGated := n.Agent == "claude"
		attn := ""
		// freshAttn distinguishes attention classified from evidence this tick
		// from attention merely preserved across an indeterminate tick: only the
		// former (re)stamps attnAt, so the preserve window ages out (R21.2).
		freshAttn := false
		// quietTl is the Tailer polled on this quiet tick (nil otherwise). The
		// turn_done predicate reuses it — no second tailerFor / file read.
		var quietTl *transcript.Tailer
		if claudeGated {
			// Strict hooked Claude: the terminal is never opened from quietness,
			// AX staticness, unresolved calls, missing/stale/unparseable
			// transcripts, fallback chat, or owing timeouts. Unsupported and
			// starting Claude nodes are equally forbidden from inspect.
			// Attention is a proven visible permission dialog (never while
			// auto-approve remains armed — that case is an inline error) or
			// a current MCP elicitation, which stays visible even when the
			// lease is armed because elicitation is never auto-answered.
			if claudeSup == claudeSupStrict && claudeDlg.Attn != "" && !claudeArmed {
				attn, freshAttn = claudeDlg.Attn, true
			}
			refreshClaudeDlg := func(tl *transcript.Tailer) {
				if claudeSup != claudeSupStrict || tl == nil {
					return
				}
				off, _ := tl.Progress()
				grew := a.noteClaudeAskedProgress(n.ID, off)
				retired := a.retireClaudeAskedByCalls(n, tl.ToolStamps()) > 0
				if grew && !retired && tl.PendingCount() == 0 {
					at, dated := tl.NewestTurnTime()
					retired = a.retireClaudeAskedOnProgress(n, at, dated)
				}
				if retired {
					claudeDlg = a.claudeVisibleDialog(n)
				}
				if claudeDlg.Attn != "" && !claudeDlg.Armed {
					attn, freshAttn = claudeDlg.Attn, true
				} else {
					attn, freshAttn = "", retired
				}
			}
			if state == "quiet" {
				quietTl = a.tailerFor(n)
				if quietTl != nil {
					quietTl.Poll()
					off, prog := quietTl.Progress()
					a.noteChatProgress(n.ID, off, prog, prev == "active")
					refreshClaudeDlg(quietTl)
				}
			} else if state == "active" {
				if tl := a.tailerFor(n); tl != nil {
					tl.Poll()
					refreshClaudeDlg(tl)
				}
			}
			askAttn, askCapable = "", false
			if claudeSup == claudeSupStrict && a.claudeElicitationWaiting(n) && attn == "" {
				attn, freshAttn = "question", true
			}
		} else if state == "quiet" {
			// A zero-turn segment immediately after /clear is deliberately idle:
			// the detached Claude transcript is expected, not missing evidence.
			// Suppress every attention fallback until the first post-clear turn
			// lands. Structured transports already have no pending request after
			// their successful Clear, so this gives every transport the same
			// fresh-page contract without weakening ordinary missing-transcript
			// diagnostics.
			freshSurface := isFreshSurface(a.segment(n))
			quietTl = a.tailerFor(n)
			if quietTl != nil {
				quietTl.Poll()
				if name, ok := quietTl.WaitingOn(); ok && !freshSurface {
					// Under --ax-screen-reader the pane is flatter and quieter,
					// so an unresolved tool call alone is weak evidence of a
					// permission wait (long-running tools can also sit static).
					// Require the same ClassifyVisible fence the active branch
					// already uses before claiming approval/question. Non-AX/
					// adopted Claude keep the legacy unresolved-call +
					// quiet-pane hard attention (fixes-2 P1).
					if n.AXScreenReader {
						a.mu.Lock()
						quietSince := a.lastChg[n.ID]
						a.mu.Unlock()
						visible, err := s.CaptureVisible()
						switch {
						case err == nil && dialoghint.ClassifyVisible(visible):
							attn = attentionKind(name)
						case err == nil && dialoghint.HasInterruptAnchor(visible):
							// Claude is displaying its ordinary working footer. This
							// suppresses only the neutral AX inspect floor; it is not
							// liveness and cannot override the classified-dialog case.
						case !quietSince.IsZero() && time.Since(quietSince) >= owedStallAX:
							// Matcher dark on a flat AX pane while the
							// transcript holds an unresolved call: degrade to
							// the neutral inspect. AX inspect has no remote
							// keypad (a stray "1" would become 1+Enter and
							// submit), so this cannot invite a prompt. Never
							// a classified dialog this has no evidence for,
							// and never silence, which is how a real approval
							// went unnoticed indefinitely.
							attn = "inspect"
						}
					} else {
						attn = attentionKind(name)
					}
				}
				// Judge the transcript only across a whole working phase (the
				// active→quiet transition, finding 21); on ordinary quiet ticks
				// the mark still advances — establishing the baseline before
				// the next phase — and newly recognized chat progress clears a
				// stale flag immediately, without waiting for another activity
				// cycle (finding 24).
				off, prog := quietTl.Progress()
				a.noteChatProgress(n.ID, off, prog, prev == "active")
				// Keep the notice watermark current without retiring anything: a
				// static pane is exactly what a waiting dialog looks like, so a
				// quiet tick is never evidence that an ask was answered.
				if askCapable {
					a.noteClaudeAskedProgress(n.ID, off)
					// Per-call retirement is safe here where growth is not:
					// it matches a notice to the one call it announced, so a
					// static pane proves nothing either way and is not asked
					// to. A dialog answered just before the pane settled
					// leaves its record behind, and this drains it.
					if a.retireClaudeAskedByCalls(n, quietTl.ToolStamps()) > 0 {
						askAttn, _ = a.claudeAskState(n)
					}
				}
			}
			// Dialoghint matcher + owing-stall backstop (P1a/P1b). Shared with
			// notePeekDialog via quietAttentionFallback — never feeds liveness.
			// AX nodes skip the uncorroborated Owing() inspect path (flat pane).
			if attn == "" && !freshSurface {
				a.mu.Lock()
				quietSince := a.lastChg[n.ID]
				a.mu.Unlock()
				visible, _ := s.CaptureVisible()
				attn = quietAttentionFallback(quietTl, visible, quietSince, n.AXScreenReader)
			}
			// Neutral needs-a-look state: the pane is quiet but there is no
			// trustworthy structured transcript to say whether the agent
			// finished or sits on a dialog this parser cannot see — no
			// transcript at all (terminal-only harnesses, discovery pending),
			// a stale one, or one whose recent data stopped parsing. Never
			// labeled question/approval (no structured evidence, and pane
			// regexes stay forbidden); the UI shows the terminal instead.
			if attn == "" && !freshSurface {
				a.mu.Lock()
				noEvidence := n.Transcript == "" || a.staleChat[n.ID]
				a.mu.Unlock()
				if noEvidence || (quietTl != nil && quietTl.Unparseable()) {
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
				// Retirement (claude_asked.go), two rules, exact first.
				//
				// Per-call: a notice carries the digest of the tool_input it
				// escalated, and Claude writes the matching tool_use record only
				// *after* the human answers — so that record retires that notice,
				// whatever else is in flight. This is the primary rule.
				//
				// Growth stays as the fallback for what the digest cannot join:
				// a dialog that is dismissed, or one raised for something that
				// never becomes a tool_use record (a plan choice, a question).
				// It keeps its old guard — an unresolved call anywhere means no
				// resolution can be claimed — because it is statistical, not a
				// join. That guard is precisely why it could not carry the load
				// alone: a working agent nearly always has a call pending, so
				// notices stood until their TTL and held hard attention over an
				// agent that was merely busy (16 of them across one 23-minute
				// turn, observed 2026-08-18). The watermark still advances on
				// every tick — a frozen mark cannot report the growth the *next*
				// resolution writes.
				if askCapable {
					off, _ := tl.Progress()
					grew := a.noteClaudeAskedProgress(n.ID, off)
					retired := a.retireClaudeAskedByCalls(n, tl.ToolStamps()) > 0
					if grew && !retired && tl.PendingCount() == 0 {
						at, dated := tl.NewestTurnTime()
						retired = a.retireClaudeAskedOnProgress(n, at, dated)
					}
					if retired {
						askAttn, _ = a.claudeAskState(n)
					}
				}
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
					switch {
					case askCapable:
						// The hook is the sole authority here. An unresolved call
						// on disk plus a confined animation is byte-identical
						// between "a long tool is running" and "a dialog is up
						// with parallel calls animating behind it", so neither the
						// matchers nor the stall backstop can discriminate — but
						// the hook already told us. A notice means a dialog (kind
						// from its tool name); no notice means the call is simply
						// still executing, and nothing is raised.
						attn = askAttn
					case st != nil:
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
					// A capable node needs no preservation window: the notice
					// itself persists across indeterminate ticks (it is a file,
					// not a per-tick classification) and its absence is evidence,
					// so preserving here would re-raise exactly the attention the
					// discriminator just retired.
					if attn == "" && !askCapable {
						a.mu.Lock()
						if prevAttn := a.attn[n.ID]; prevAttn != "" && time.Since(a.attnAt[n.ID]) < animStallAfter {
							attn = prevAttn
						}
						a.mu.Unlock()
					}
				}
			}
		}
		// A standing notice raises on its own, whichever branch ran and whether or
		// not the transcript holds an unresolved call. That last part is the
		// point: Claude flushes the tool_use record only *after* approval, so for
		// a first blocked call the helper is the only evidence the dialog exists
		// — the same late-flush case the owing-stall backstop covers in tens of
		// seconds, answered here in one tick. askAttn is non-empty only for a
		// capable node.
		if attn == "" && askAttn != "" {
			attn, freshAttn = askAttn, true
		}
		// P5 item 10 — turn finished: quiet pane, newest recognized record is
		// assistant (Delivered), no unresolved tool call, no attention, not
		// ended. Uses quietTl already polled this tick — zero extra I/O.
		// ACP nodes never reach this with a Tailer (tailerFor returns nil when
		// Transcript == ""); absent turn_done means UNKNOWN, not "not finished".
		// The claim expires: an idle session must not stay "finished" forever.
		// Undated turns read as unknown and decline rather than guess (the
		// dated check is deliberately redundant with the window — a zero time
		// is always outside it — and states the intent at the seam).
		turnDone := false
		if state == "quiet" && attn == "" && n.EndedAt == "" &&
			quietTl != nil && quietTl.Delivered() && quietTl.PendingCount() == 0 {
			if at, dated := quietTl.NewestTurnTime(); dated && time.Since(at) < turnDoneWindow {
				turnDone = true
			}
		}
		a.mu.Lock()
		prevAttn := a.attn[n.ID]
		a.live[n.ID] = state
		if freshAttn && attn != "" {
			a.attnAt[n.ID] = time.Now()
		}
		a.attn[n.ID] = attn
		a.turnDone[n.ID] = turnDone
		// An armed lease cannot cross a turn, and no lease survives a dead pane.
		// Unlike the structured branch above, Claude waits for an explicit
		// transcript boundary before disarming (Stop hook is the primary
		// signal; end_turn is the fallback). Capture the lease id here so a
		// re-arm that completed after this tick's observation is not erased
		// by a stale transition.
		disarmLeaseID, settleLeaseID := "", ""
		claudeLost := n.Agent == "claude" && state == "exited" && prev != "exited"
		if state == "exited" && prev != "exited" {
			if st := a.autoApprove[n.ID]; st != nil {
				disarmLeaseID = st.LeaseID
			}
		} else if prev == "active" && state != "active" {
			// Hooked Claude stays armed until Stop / interrupt / disable /
			// /clear / /exit / process loss. Pane quietness is not a turn
			// boundary. Other agents keep the existing mechanical settle.
			if n.Agent != "claude" {
				turnClosed := quietTl == nil || (quietTl.EndTurn() && quietTl.PendingCount() == 0)
				if st := a.autoApprove[n.ID]; st != nil && turnClosed {
					settleLeaseID = st.LeaseID
				}
			}
		}
		a.mu.Unlock()
		if disarmLeaseID != "" {
			a.disarmAutoApproveIfLease(n.ID, disarmLeaseID)
		}
		if settleLeaseID != "" {
			a.settleAutoApproveAfterTurn(n.ID, settleLeaseID)
		}
		if claudeLost {
			a.resetClaudePermissionTurn(n)
		}
		// An armed Claude marker expires after claudeLeaseTTL and is never
		// rewritten on its own; republish so Stop can still read it on a
		// long turn.
		a.refreshClaudeLeaseExpiry(n)
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
		a.reconcileClaudeInitialDelivery(n)
		a.reconcileClaudeClear(n)
	}
}

// reconcileClaudeClear gives up waiting for the SessionStart source:"clear"
// that would prove a pasted /clear turned the page. Expiry is a notice and
// nothing more: there is deliberately nothing to undo, because the delivery
// path no longer retires anything, and nothing to retry, because /clear is
// never retried. The same outer bound as the first prompt — a paste swallowed
// by a dialog is the shared cause, and a second clock would only invite the
// two to disagree.
func (a *app) reconcileClaudeClear(n *Node) {
	if n == nil || n.Agent != "claude" || a.claudeDeliveryGiveUp <= 0 {
		return
	}
	a.mu.Lock()
	sent, waiting := a.claudeClearSent[n.ID]
	expired := waiting && time.Since(sent) > a.claudeDeliveryGiveUp
	if expired {
		a.clearClaudeClearPendingLocked(n.ID)
	}
	a.mu.Unlock()
	if expired {
		a.recordClaudeLaunchError(n.ID, claudeClearExplain)
	}
}

// reconcileClaudeInitialDelivery releases only the initial-prompt uncertainty
// when Claude's transcript mirror catches up after the synchronous create
// deadline. It never retries the prompt and never clears an ordinary
// unconfirmed follow-up send: those still require the supervisor's explicit
// terminal check.
func (a *app) reconcileClaudeInitialDelivery(n *Node) {
	if n == nil || n.Agent != "claude" {
		return
	}
	a.mu.Lock()
	pending := a.sendState[n.ID] == sendInitialUnconfirmed
	a.mu.Unlock()
	if !pending {
		return
	}

	want := canonicalPrompt(n.Prompt)
	if want == "" {
		return
	}
	for _, turn := range a.segment(n).Turns {
		if turn.Role != "user" || canonicalPrompt(turn.Text) != want {
			continue
		}
		a.mu.Lock()
		if a.sendState[n.ID] == sendInitialUnconfirmed {
			delete(a.sendState, n.ID)
		}
		a.mu.Unlock()
		a.clearClaudeLaunchError(n.ID)
		return
	}
	// Still nothing. The transcript is written lazily, so keep waiting — but
	// not forever: a paste swallowed by a startup, trust, login, or rate-limit
	// dialog never reaches Claude's prompt, and no transcript turn will ever
	// arrive for it. At the outer bound the neutral wait becomes the inline
	// error, with the prompt handed back as a draft. The error is recorded
	// before the gate is released so a poll landing between the two shows the
	// explanation beside the pale bubble rather than a prompt that has briefly
	// vanished from both the chat and the composer.
	a.mu.Lock()
	deliv, delivered := a.lastDeliver[n.ID]
	a.mu.Unlock()
	if !delivered || a.claudeDeliveryGiveUp <= 0 ||
		time.Since(deliv) <= a.claudeDeliveryGiveUp {
		return
	}
	a.recordClaudeLaunchError(n.ID, claudeDeliveryExplain)
	a.mu.Lock()
	if a.sendState[n.ID] == sendInitialUnconfirmed {
		delete(a.sendState, n.ID)
	}
	a.mu.Unlock()
}

// sessionSnapshot runs one list-sessions and returns the name set. ok is
// false when the listing failed (as opposed to succeeding with zero
// sessions); callers must leave liveness unchanged on !ok.
func (a *app) sessionSnapshot() (map[string]bool, bool) {
	if a.server == nil {
		return nil, true // bare tests without a server: empty set, not a failure
	}
	names, err := a.server.ListSessions()
	if err != nil {
		return nil, false
	}
	set := make(map[string]bool, len(names))
	for _, name := range names {
		if name != "" {
			set[name] = true
		}
	}
	return set, true
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

// noteDelivery records that scimux pasted a prompt into a node's pane. It is
// the watermark the stale-link backstop judges against: after this instant the
// agent owes output, so a transcript that records nothing is not the file the
// pane is writing to. Callers pass the paste time rather than "now" so a slow
// confirmation wait cannot move the watermark forward past the answer.
func (a *app) noteDelivery(id string, at time.Time) {
	if id == "" {
		return
	}
	a.mu.Lock()
	a.lastDeliver[id] = at
	a.mu.Unlock()
}

// maybeRelinkTranscript is the active→quiet hook drain and stale-link
// backstop. It must not choose a transcript from cwd, mtime, pane cmdline,
// or newest-file. After a delivery the linked file did not carry, every Claude
// node detaches and shows the pane. A later validated hook event still binds
// the successor.
func (a *app) maybeRelinkTranscript(n *Node) {
	if n == nil || n.Agent != "claude" {
		return
	}
	a.drainClaudeHooks()
	a.mu.Lock()
	cur := n.Transcript
	deliv, delivered := a.lastDeliver[n.ID]
	bound := a.claudeBoundAt[n.ID]
	a.mu.Unlock()
	if cur == "" {
		return
	}
	// Staleness is judged against a delivery, never against a pane phase. A
	// phase proves only that the terminal repainted: the poller's very first
	// capture after a restart opens one (prevCap starts empty, so any live
	// pane reads as changed), and so does remote-control chrome redrawing
	// after a turn is already finished. Both were observed retiring healthy
	// transcripts — the restart case retired every idle Claude node at once.
	// A delivered prompt is different: the agent owes output for it.
	if !delivered {
		return
	}
	// The transcript write lags the paste; judging inside that window would
	// retire a link that is about to be answered.
	if time.Since(deliv) < deliveryGrace {
		return
	}
	// A link established after the prompt was pasted cannot have missed it.
	if bound.After(deliv) {
		return
	}
	// No recognized turn yet is absence of evidence, not proof of staleness.
	// Claude Code fills a fresh /clear successor with mode, bridge-session,
	// file-history-snapshot and an isMeta caveat record — ParseLine recognizes
	// none of them — so the file carries no content time until the next human
	// turn. Retiring on that tombstones the successor and strands the node in
	// peek for good (observed against claude 2.1.224).
	ct, ok := transcript.NewestContentTime(cur)
	if !ok || !ct.Before(deliv) {
		return
	}
	a.retireTranscript(n)
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
// owedStallAfter: quiet-pane backstop when the newest transcript turn is a
// user turn (agent owes output) and the pane has been static this long with
// no unresolved call and no matcher hit — Claude Code's late tool_use flush
// leaves WaitingOn blind for the whole approval wait (P1b).
// paneQuietAfter: a pane unchanged for this long is mechanically quiet. It is
// the liveness threshold, and also the precondition every quiet-branch
// attention source shares.
const animMaxLines = 3
const paneQuietAfter = 8 * time.Second

// The attention ladder. The absolute magnitudes below are conservative
// choices, not measurements — what is actually load-bearing is their order,
// and TestAttentionLadderIsOrdered pins it so a plausible-looking tweak to one
// constant cannot silently invert a rung.
//
// Two chains, on two different kinds of evidence:
//
//	paneQuietAfter < owedStallCorroborated < owedStallAfter < owedStallAX
//
// is the quiet-pane chain, ordered by how much the pane's staticness is worth.
// paneQuietAfter is the precondition every rung shares; the cancel anchor
// sharpens by exactly one rung; an AX pane renders flatter and quieter, so its
// staticness is the weakest evidence of all and it sits slowest.
//
//	owedStallAfter < animStallAfter
//
// is the cross-branch one: animStallAfter governs a pane that is still
// *changing*, where a confined animation strip is weaker evidence of a wait
// than a pane that stopped changing altogether, so the active-pane backstop
// must be the slower of the two. Both directions of error are bounded and
// neither is silent — too fast raises a neutral inspect at a busy agent (the
// user dismisses it), too slow leaves a human waiting, and the incident that
// motivated this whole branch was a 6m42s wait, which is the scale the numbers
// are chosen against.
//
// For an owned Claude launch whose bundle proves "asked", none of this decides
// anything: the escalation notice is the sole authority on the active-pane
// paths, and its absence suppresses the animStallAfter degradation outright.
// These rungs govern the agents that have no such notice — Codex, ACP, and
// bundles predating the gate.

// deliveryGrace is how long after a paste the stale-link backstop waits before
// a transcript with no new content counts as proof the link is dead. It covers
// the lag between the paste and the agent writing its user record; the pane
// can complete a whole active→quiet cycle on the paste echo alone inside it.
const deliveryGrace = 30 * time.Second
const animStallAfter = 90 * time.Second
const owedStallAfter = 45 * time.Second

// owedStallCorroborated: the owing stall when the pane also shows the dialog
// chrome anchor. The anchor cannot classify (it appears in agent prose too),
// but on a pane that is already quiet with the agent owing output it is strong
// enough to reach the same neutral verdict sooner.
const owedStallCorroborated = 10 * time.Second

// owedStallAX: the same stall on an --ax-screen-reader node, which renders a
// flatter and quieter pane, so staticness is weaker evidence of a wait. AX sits
// exactly one rung slower than the legacy renderer at every step —
// corroborated < owed < AX — and the cancel anchor sharpens AX by one rung, to
// the non-AX owed timing, never all the way to owedStallCorroborated.
//
// This is the *floor*, not a classifier: past it an AX node the matcher cannot
// read says the neutral "no visible progress", never approval or question.
// AX inspect is Dismiss-only in the UI (no digit/y-n keypad), so the floor
// cannot invite a stray "1"+"Enter" prompt. The floor exists because
// attention for AX otherwise rests entirely on one regex family — see
// ax_inspect_floor_test.go for the six AX pane shapes that family does not match.
const owedStallAX = 4 * time.Minute

// turnDoneWindow bounds how long a delivered turn keeps claiming "finished".
// The claim is dated by the CLI's own stamp on the newest parsed turn, not by
// scimux clocks, so it survives a restart and cannot relight every idle
// station. Past the window a node is simply quiet again: the reply is still
// there to read, but it is no longer news.
const turnDoneWindow = 30 * time.Minute

// structuredTurnDone is the P2 Ready latch for ACP/codex. Proof is the
// mechanical active→quiet edge with an empty LastError (endTurn writes
// LastError for interrupt/empty/failed; a successful end_turn /
// turn/completed leaves it empty). Idle-never-ran stays false. The latch
// holds across quiet ticks until attention, process death, a new turn, or
// turnDoneWindow — same bound as the Claude transcript claim.
func structuredTurnDone(live, attn, prevLive, lastErr, endedAt string, prevDone bool, lastChg, now time.Time) bool {
	if endedAt != "" || live != "quiet" || attn != "" || lastErr != "" {
		return false
	}
	if prevLive == "active" {
		return true
	}
	if !prevDone || lastChg.IsZero() {
		return false
	}
	return now.Sub(lastChg) < turnDoneWindow
}

// quietAttentionFallback is the quiet-branch attention sources that do not
// need WaitingOn: the dialoghint matcher (P1a, including the structural
// numbered-options shape) and the owing-stall mechanical backstop (P1b).
// Called from the poller's quiet branch and from notePeekDialog (P1c).
// Feeds attention only — never liveness. The backstop reads no pane text.
//
// Both sources require a mechanically static pane, checked here rather than
// left to the caller: the poller's quiet branch satisfies it by construction,
// but notePeekDialog runs on a human peek with no liveness gate at all, and
// pane text must never create attention on an active pane — that case is
// fenced to unresolved call + confined animation, which notePeekDialog's
// corroborated path handles before reaching this one.
//
// axScreenReader *delays* the uncorroborated Owing() inspect path rather than
// disabling it: under --ax-screen-reader the pane is deliberately flatter, so
// quietness past the legacy timeout is not yet a safe claim that the agent is
// waiting — but past owedStallAX it is, and the alternative is silence.
//
// fixes-2 P1 disabled this path for AX outright. That was one fix too many:
// the observed harm (a spurious inspect inviting a stray "1") is removed by
// keeping AX inspect keyless in the UI (Dismiss only; non-AX inspect keeps
// the full keypad plus Dismiss), while disabling the backstop left AX with
// a single regex family as its only route to any attention. When that family
// goes dark on a TUI rewording, an AX node on a real approval raised nothing
// at all, indefinitely — where AGENTS.md's rule for a dark matcher is
// degradation to the neutral inspect, never to a classified dialog and never
// to silence. AX may still raise "dialog" via ClassifyVisible, and
// missing/stale/unparseable transcript inspect still lives in the caller's
// separate noEvidence branch.
func quietAttentionFallback(tl *transcript.Tailer, visible string, quietSince time.Time, axScreenReader bool) string {
	if quietSince.IsZero() || time.Since(quietSince) < paneQuietAfter {
		return ""
	}
	if visible != "" && dialoghint.ClassifyVisible(visible) {
		return "dialog"
	}
	if visible != "" && dialoghint.HasInterruptAnchor(visible) {
		return ""
	}
	if tl != nil && tl.Owing() {
		// Ladder: corroborated < owed < AX. The base rung is the renderer's —
		// AX is slower because its flat pane makes staticness weaker evidence.
		// The cancel anchor sharpens by exactly one rung, so AX with an anchor
		// lands on the non-AX base timing and never on owedStallCorroborated:
		// the phrase occurs in ordinary agent prose, and AX must stay strictly
		// more conservative than the legacy renderer at every step.
		stall := owedStallAfter
		if axScreenReader {
			stall = owedStallAX
		}
		if visible != "" && dialoghint.HasCancelAnchor(visible) {
			if axScreenReader {
				stall = owedStallAfter
			} else {
				stall = owedStallCorroborated
			}
		}
		if time.Since(quietSince) >= stall {
			return "inspect"
		}
	}
	return ""
}

type animState struct {
	lines []int     // union of line indices seen changing, sorted
	since time.Time // when changes became confined; restarts on transcript growth
	off   int64     // transcript offset backing the stall window; -1 until read
}

// noteAnim classifies one pane change: streaming or redrawing output touches
// many lines and clears the state; an animation strip touches the same few
// lines tick after tick. Mechanical only — diff geometry, never text.
// Callers hold a.mu.
//
// Deliberately *not* used to decide that a dialog is gone: drawing a
// permission box is itself an unconfined diff, so geometry cannot separate
// "a dialog appeared" from "output resumed" — the very ambiguity the
// escalation notices exist to settle (claude_asked.go).
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
		a.anim[id] = &animState{lines: idx, since: time.Now(), off: -1}
		return
	}
	delete(a.anim, id)
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

// discoverTranscript attempts UUID-directory binding for a tmux node that
// still lacks a transcript path. Claude is excluded: SessionStart is its
// only ownership proof. Eligibility requires a SessionID and CreatedAt
// within 15 minutes; the path must not already be owned, claimed in-flight,
// or tombstoned. Persist-before-publish keeps a failed store write
// retryable (claim released, in-memory path empty).
//
// Reachability: structured transports never enter this poller branch, and
// no supported production SessionID for a non-Claude node names a file
// under ~/.claude/projects. The body after the Claude exit is retained as
// legacy defensive code — not a supported bind path.
func (a *app) discoverTranscript(n *Node) {
	if n == nil || n.Agent == "claude" {
		return
	}
	a.mu.Lock()
	done := n.Transcript != ""
	a.mu.Unlock()
	if done {
		return
	}
	if n.SessionID == "" {
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
	if !ok || excluded[path] || a.isDeadTranscript(n.ID, path, n.SessionID) {
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
	a.claudeBoundAt[n.ID] = time.Now()
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
