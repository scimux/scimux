// conversation_api.go — HTTP orchestration for send/clear/resolve/interrupt/
// key/peek/chat and the segment/projection helpers they share.
//
// Ownership / locks (existing boundaries; this file does not change them):
//   - Segment-cache install and Node/poll/sendState snapshots use a.mu;
//     filesystem, tmux, session-log, and protocol calls stay outside a.mu.
//   - Send serialization remains per-node (a.sendState under a.mu).
//   - Durable transcript retirement (store appends) precedes in-memory
//     unlink/reset of Transcript/SessionID and poll maps.
//   - Structured key audit (store append) remains before permission delivery.
//   - Tmux capture remains a prerequisite for key/interrupt; the send-then-
//     audit success contract is unchanged.
//   - Peek may only raise corroborated attention (never overwrite/clear).
//   - Router registration stays in router.go; procManager owns protocol
//     branching; attachment helpers live in attachment_api.go; shared request
//     decoding and JSON response encoding live in security.go.
package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/scimux/scimux/internal/acp"
	"github.com/scimux/scimux/internal/asset"
	"github.com/scimux/scimux/internal/dialoghint"
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/sessionworker"
	"github.com/scimux/scimux/internal/tmuxsession"
	"github.com/scimux/scimux/internal/transcript"
)

// segment returns the node's current conversation — the session log's tail
// after its last source seam. A /clear appends a seam, so this is what makes
// "fresh chat surface under the same activity" uniform across transports.
func (a *app) segment(n *Node) sessionlog.Segment {
	if a.sessionsDir == "" {
		return sessionlog.Segment{Turns: []transcript.Turn{}} // bare test apps
	}
	return a.sessionLogCache(n.ID).Segment(a.sessionLogPath(n.ID))
}

// sessionLogCache returns the per-node LogCache, installing one if needed.
// a.mu is held only for the map lookup/create — never across file I/O.
func (a *app) sessionLogCache(id string) *sessionlog.LogCache {
	a.mu.Lock()
	c := a.logCache[id]
	if c == nil {
		c = &sessionlog.LogCache{}
		a.logCache[id] = c
	}
	a.mu.Unlock()
	return c
}

func isFreshSurface(seg sessionlog.Segment) bool {
	return len(seg.Turns) == 0 && seg.PriorTurns > 0
}

func (a *app) node(r *http.Request) (*Node, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, ok := a.byID[r.PathValue("id")]
	return n, ok
}

// refuseEnded reports whether the node is a deliberately closed ("ended")
// thread and, if so, writes a 409. /exit makes the ended head an immutable
// dead-end cap; every mutation endpoint (send, /clear, upload, interrupt,
// remote key) refuses it so a "Closed" thread can never accept new history —
// including an adopted thread whose process outlived the cap, or one whose
// teardown failed. Read paths (peek, chat) and the allowed post-end
// operations (fork, edit, archive, delete) are deliberately not guarded:
// pick-up is via fork, not by reopening the dead-end.
func (a *app) refuseEnded(w http.ResponseWriter, n *Node) bool {
	a.mu.Lock()
	ended := n.EndedAt != ""
	a.mu.Unlock()
	if ended {
		http.Error(w, "thread is closed; fork to continue", http.StatusConflict)
	}
	return ended
}

// refuseRetiredExternal fences every control path for historical nodes that
// scimux once attached to but never owned. Their saved history remains a read
// surface; continuing work requires a fresh scimux-owned fork/chat.
func (a *app) refuseRetiredExternal(w http.ResponseWriter, n *Node) bool {
	if n == nil || !n.Adopted {
		return false
	}
	http.Error(w, "external session integration is retired; start a fresh chat to continue", http.StatusConflict)
	return true
}

// handleSend delivers a web prompt and reports what the delivery evidence
// supports: "acknowledged" when the pane visibly reacted to Enter or a new
// transcript turn appeared, "unconfirmed" otherwise. {ok:"sent"} alone would
// only mean "tmux accepted the keystrokes" — the TUI may have held the text
// in its editor (e.g. a pre-existing draft), and the next blind send would
// concatenate with it. Sends are serialized per node; an unresolved delivery
// holds further sends until the supervisor rechecks (POST …/send/resolve).
func (a *app) handleSend(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if a.refuseRetiredExternal(w, n) {
		return
	}
	if a.refuseEnded(w, n) {
		return
	}
	var body struct {
		Text        string       `json:"text"`
		Attachments []Attachment `json:"attachments"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		// The body cap is a deliberate defense (R18.6), but a bare "bad
		// request" for an oversized prompt gives the user no size hint and
		// invites retries that can never succeed — name the limit. (Files ride
		// the separate multipart upload, not this JSON body.)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, fmt.Sprintf("prompt too large: the request body is capped at %d bytes (%d MiB); shorten the prompt or point the agent at a file instead", jsonBodyMax, jsonBodyMax>>20), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", 400)
		return
	}
	// A send may carry text, attachments, or both — an image-only turn is valid.
	if strings.TrimSpace(body.Text) == "" && len(body.Attachments) == 0 {
		http.Error(w, "bad request", 400)
		return
	}
	// Claude's native /fork is not a scimux SessionStart source. Forking is
	// scimux's own create-with-parent action. HTTP is authoritative: the GUI
	// also blocks this, but a raw POST must still 400.
	if n.Agent == "claude" && claudeNativeForkCommand(body.Text) {
		http.Error(w, "Claude /fork is not supported; use scimux's Fork action to create a fresh node that inherits launch configuration but not conversation history", http.StatusBadRequest)
		return
	}
	atts, err := a.resolveAttachments(n.ID, body.Attachments)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// delivered is what the transport receives and records: the user's text with
	// a plain-text reference to each uploaded file appended (extendPrompt). The
	// raw text is kept only for /clear detection and unconfirmed-draft echo.
	delivered := extendPrompt(body.Text, atts)
	if a.workers != nil && n.Agent == "claude" && a.workers.manages(n.ID) {
		var delivery sessionworker.Delivery
		var err error
		if strings.TrimSpace(body.Text) == "/clear" {
			delivery, err = a.workers.ClearDelivery(n.ID)
		} else {
			delivery, err = a.workers.SendDelivery(n.ID, delivered)
		}
		if err != nil {
			code := http.StatusInternalServerError
			if a.workers.Conflict(err) {
				code = http.StatusConflict
			}
			http.Error(w, err.Error(), code)
			return
		}
		if strings.TrimSpace(body.Text) != "/clear" {
			a.noteUsagePrompt(n.Agent)
		}
		response := map[string]string{"status": delivery.Status}
		if delivery.Status == sessionworker.DeliveryUnconfirmed {
			response["text"] = body.Text
		}
		writeJSON(w, response)
		return
	}
	// Structured-protocol delivery (ACP, codex, Muse) is reliable (no pane-ack race,
	// so no "unconfirmed" state), but the preflight still holds: refuse a second
	// turn while one is in flight, and refuse entirely if the subprocess is
	// gone. The manager records the user turn before prompting (a log-append
	// failure refuses the send) — see the Send contract on each manager.
	if pm := a.proc(n); pm != nil {
		live := pm.Live(n.ID)
		if live == "active" {
			http.Error(w, "a turn to this node is still in flight", 409)
			return
		}
		// The structured transports have no TUI to interpret slash commands,
		// so scimux implements /clear itself: a fresh protocol session on the
		// same process, recorded as a source seam — same page-turn semantics
		// as Claude's /clear, same log file, same node.
		if strings.TrimSpace(body.Text) == "/clear" {
			if (n.Agent == "muse" || n.transport() == "muse") && !a.settings().MuseApprovalJudgeConsent {
				http.Error(w, errMuseConsentRequired.Error(), http.StatusBadRequest)
				return
			}
			// Freeze the closing station's label before the page turns, so a
			// later rename only moves the fresh head (spec A/B/C). The manager
			// appends the clear seam inside Clear; this reads the pre-clear stop
			// chain, so it must run first.
			a.snapshotClosingStation(n)
			if err := pm.Clear(n.ID); err != nil {
				code := http.StatusInternalServerError
				message := err.Error()
				if pm.Conflict(err) {
					code = http.StatusConflict
				} else if acp.IsConfigRejection(err) {
					code = http.StatusBadRequest
					message += ". Fork with a model the agent still offers."
				}
				http.Error(w, message, code)
				return
			}
			// /clear ends the turn lease (page turn is a hard reset).
			a.disarmAutoApprove(n.ID)
			// Keep node.session_id on the post-clear ACP/thread id so the pi
			// native fare join (session-map.json) stays one hop (Phase 4). The
			// correction is a new node record: the worker can outlive this muxer,
			// so an in-memory update would regress after its next restart.
			if sid := a.liveSessionID(n); sid != "" {
				var err error
				a.mu.Lock()
				if sid != n.SessionID {
					updated := *n
					updated.SessionID = sid
					err = a.appendRecord(storeRecord{Type: "node", Node: &updated})
					if err == nil {
						n.SessionID = sid
					}
				}
				a.mu.Unlock()
				if err != nil {
					http.Error(w, "persist clear session: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
			writeJSON(w, map[string]string{"status": "acknowledged"})
			return
		}
		// Boundary sample → Send → primed arm under the auto-gate so the
		// first permission of this turn cannot race into the cutoff, and a
		// concurrent enable cannot install between Send and arm.
		if err := a.acceptStructuredPrompt(n, pm, delivered); err != nil {
			code := 500
			if pm.Conflict(err) {
				code = 409
			}
			http.Error(w, err.Error(), code)
			return
		}
		a.noteUsagePrompt(n.Agent)
		writeJSON(w, map[string]string{"status": "acknowledged"})
		return
	}
	status, delivery, err := a.sendTmuxPrompt(n, delivered, strings.TrimSpace(body.Text) == "/clear")
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	response := map[string]string{"status": string(delivery)}
	if delivery == initialUnconfirmed {
		response["text"] = body.Text
	}
	writeJSON(w, response)
}

// sendTmuxPrompt is the established pane-delivery contract shared by the
// legacy in-process path and the Claude session worker. It returns the HTTP
// status that the existing public endpoint assigns to a refusal, so moving
// the caller across a process boundary cannot blur 409 conflicts into 500s.
func (a *app) sendTmuxPrompt(n *Node, delivered string, isClear bool) (int, initialDelivery, error) {
	a.mu.Lock()
	switch a.sendState[n.ID] {
	case sendSubmitting:
		a.mu.Unlock()
		return http.StatusConflict, "", errors.New("a send to this node is still in flight")
	case sendUnconfirmed, sendInitialUnconfirmed:
		a.mu.Unlock()
		return http.StatusConflict, "", errors.New("the previous send is unconfirmed — check the terminal, then recheck")
	}
	if n.Agent == "claude" {
		sup := a.claudeSupervisionOf(n)
		launchErr := a.claudeLaunchErr[n.ID]
		if sup != claudeSupStrict {
			a.mu.Unlock()
			return http.StatusConflict, "", errors.New(claudeSupervisionExplain(sup, launchErr))
		}
	}
	a.sendState[n.ID] = sendSubmitting
	a.mu.Unlock()

	turnsBefore := -1
	tl := a.tailerFor(n)
	if tl != nil {
		turnsBefore = len(tl.Poll())
	}
	pasted := time.Now()
	var clearBind *sync.Mutex
	if n.Agent == "claude" && isClear {
		clearBind = a.claudeBindLock(n.ID)
		clearBind.Lock()
		defer clearBind.Unlock()
	}
	acked, err := a.acceptTmuxPrompt(n, isClear, func() (bool, error) {
		return a.server.Session(n.ID).SendAck(delivered)
	})
	if err != nil {
		a.mu.Lock()
		delete(a.sendState, n.ID)
		a.mu.Unlock()
		status := http.StatusInternalServerError
		if errors.Is(err, errClaudeTurnInFlight) {
			status = http.StatusConflict
		}
		return status, "", err
	}
	// Reaching this point proves the new prompt was accepted by the pane. Any
	// older recoverable first-prompt confirmation error is now obsolete. Clear
	// only that diagnostic: a fatal launch/binding error recorded concurrently
	// with delivery must remain fail-closed.
	a.clearClaudeRecoverableError(n.ID)
	if !acked && tl != nil && len(tl.Poll()) > turnsBefore {
		acked = true
	}
	a.noteDelivery(n.ID, pasted)
	a.mu.Lock()
	if acked {
		delete(a.sendState, n.ID)
	} else {
		a.sendState[n.ID] = sendUnconfirmed
	}
	a.mu.Unlock()
	if !isClear {
		a.noteUsagePrompt(n.Agent)
	}
	// A pasted /clear is only a request. SessionStart source:"clear" remains
	// the sole author of retirement, tombstones, and the source seam.
	if n.Agent == "claude" && isClear {
		a.noteClaudeClearSent(n.ID)
	}
	if !acked {
		return 0, initialUnconfirmed, nil
	}
	return 0, initialAcknowledged, nil
}

// claudeNativeForkCommand reports whether text is Claude's native /fork
// invocation: the trimmed prompt is exactly "/fork" or begins with "/fork"
// followed by whitespace. Ordinary prose that merely contains the token
// "/fork" is not a command.
func claudeNativeForkCommand(text string) bool {
	s := strings.TrimSpace(text)
	if s == "/fork" {
		return true
	}
	if !strings.HasPrefix(s, "/fork") {
		return false
	}
	rest := s[len("/fork"):]
	if rest == "" {
		return true
	}
	switch rest[0] {
	case ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

// snapshotClosingStation freezes the label of the station a /clear is about to
// close, so a later rename of the active chat (which moves only the node's live
// Title/Description = the fresh head) can never rewrite the closed station's
// name on the map. The key is the closing station's start time: the last
// existing /clear seam, or the node's creation when this is the first page-turn.
// Call it BEFORE the clear seam is appended, so the key is read from the pre-clear
// stop chain. Best-effort: an append failure is logged, not fatal (the map
// degrades to the node title, the pre-feature behaviour). Guarded like the seam
// write — a node that never mirrored has no station to freeze.
func (a *app) snapshotClosingStation(n *Node) {
	if a.sessionsDir == "" {
		return
	}
	logPath := a.sessionLogPath(n.ID)
	if _, err := os.Stat(logPath); err != nil {
		return
	}
	seg := a.segment(n)
	key := n.CreatedAt
	if ct := seg.ClearTimes; len(ct) > 0 {
		key = ct[len(ct)-1]
	}
	if key == "" {
		return
	}
	// A clear the agent refuses answers 400, which the user retries — and a
	// refused clear appends no seam, so the retry lands on the same key. The
	// newest snapshot per seam wins, so a record identical to the one already
	// held says nothing new; writing it anyway would grow a store that is never
	// rewritten with copies of one label. Skip it, and let a changed label land.
	if prev, ok := seg.Stations[key]; ok && prev.Title == n.Title && prev.Desc == n.Description {
		return
	}
	if err := a.appendSessionEvent(n.ID, sessionlog.NewStation(key, n.Title, n.Description)); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: station snapshot for %s: %v\n", n.ID, err)
	}
}

// retireTranscript unlinks a node's transcript (and session id) after a known
// session rollover. Corrections are new records: a node record persists the
// cleared session id so a young node's discovery cannot resurrect the dead
// file, and an empty-path transcript record retires the link across restarts
// (the latest transcript record wins at replay). The node record goes first:
// if only one append lands, a cleared session id with a stale path is inert,
// while a cleared path with a live session id lets discovery relink the dead
// transcript. Record first, publish second, as everywhere.
func (a *app) retireTranscript(n *Node) {
	a.mu.Lock()
	if n.Transcript == "" && n.SessionID == "" {
		a.mu.Unlock()
		return
	}
	oldPath, oldSID := n.Transcript, n.SessionID
	cp := *n
	cp.Transcript, cp.SessionID = "", ""
	a.mu.Unlock()
	if err := a.appendRecord(storeRecord{Type: "node", Node: &cp}); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: retire session id for %s: %v\n", n.ID, err)
		return
	}
	if err := a.appendRecord(storeRecord{Type: "transcript", ID: n.ID, Path: ""}); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: retire transcript for %s: %v\n", n.ID, err)
	}
	// Tombstone the retired path/session so discoverTranscript and a late
	// hook event refuse them even when the pane cmdline still names the
	// launch session or a late metadata touch bumps the dead file's mtime.
	// Append-only: a new record, never an edit of the node/transcript lines.
	if err := a.appendRecord(storeRecord{
		Type: "transcript-retired", ID: n.ID, Path: oldPath, SessionID: oldSID,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: tombstone transcript for %s: %v\n", n.ID, err)
	}
	// Deliberate non-poller reset after durable retirement only: poller-owned
	// maps must drop the old file's tailer/progress/mirror so the next segment
	// cannot inherit them. Persist failure returns above without touching these.
	a.mu.Lock()
	a.markDeadTranscriptLocked(n.ID, oldPath, oldSID)
	n.Transcript, n.SessionID = "", ""
	delete(a.tailers, n.ID)
	delete(a.chatMark, n.ID)
	delete(a.staleChat, n.ID)
	// Drop the mirror's in-memory watermark too: it rebuilds from the log
	// (which now ends in the seam below), exactly like after a restart — so
	// a relink can never attribute pre-/clear turns to the fresh segment.
	delete(a.mirrors, n.ID)
	delete(a.piMirrors, n.ID)
	a.mu.Unlock()
	// A known rollover also turns the page in the session log: a path-less
	// "detached" seam makes the fresh chat surface immediate (the reader
	// renders since-last-source), while the prior conversation stays behind
	// it in the same file. The relink after the next turn appends the real
	// source seam with the new transcript path — two seams, both true. Only
	// an existing log gets one: a node that never mirrored has no page to
	// turn. Append failures are logged, not fatal: the UI degrades to peek
	// either way and the mirror's next seam still separates the segments.
	if a.sessionsDir != "" {
		logPath := a.sessionLogPath(n.ID)
		if _, err := os.Stat(logPath); err == nil {
			// Freeze the closing station's label before the seam turns the page,
			// so a later rename only moves the fresh head (spec A/B/C).
			a.snapshotClosingStation(n)
			w := &sessionlog.Writer{Path: logPath}
			if err := w.Append(sessionlog.NewClearSource("")); err != nil {
				fmt.Fprintf(os.Stderr, "scimux: clear seam for %s: %v\n", n.ID, err)
			}
		}
	}
}

// handleSendResolve is the supervisor's "I checked (or fixed) this in the
// terminal" acknowledgement: it clears an unconfirmed delivery so sending
// can resume. It never re-presses Enter.
func (a *app) handleSendResolve(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if a.refuseRetiredExternal(w, n) {
		return
	}
	if a.workers != nil && n.Agent == "claude" && a.workers.manages(n.ID) {
		if err := a.workers.ResolveDelivery(n.ID); err != nil {
			code := http.StatusInternalServerError
			if a.workers.Conflict(err) {
				code = http.StatusConflict
			}
			http.Error(w, err.Error(), code)
			return
		}
		writeJSON(w, map[string]string{"ok": "resolved"})
		return
	}
	a.mu.Lock()
	if a.sendState[n.ID] == sendSubmitting {
		a.mu.Unlock()
		http.Error(w, "a send is still in flight", 409)
		return
	}
	delete(a.sendState, n.ID)
	a.mu.Unlock()
	writeJSON(w, map[string]string{"ok": "resolved"})
}

func (a *app) handleSendInterrupt(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if a.refuseRetiredExternal(w, n) {
		return
	}
	if a.refuseEnded(w, n) {
		return
	}
	if a.workers != nil && n.Agent == "claude" && a.workers.manages(n.ID) {
		evidence, err := a.workers.InterruptEvidence(n.ID)
		if err != nil {
			code := http.StatusInternalServerError
			if a.workers.Conflict(err) {
				code = http.StatusConflict
			}
			http.Error(w, err.Error(), code)
			return
		}
		if err := a.appendRecord(storeRecord{Type: "key", ID: n.ID, Key: "Escape", Keys: evidence.Keys,
			Excerpt: evidence.Evidence, Time: time.Now().UTC().Format(time.RFC3339)}); err != nil {
			fmt.Fprintf(os.Stderr, "scimux: interrupt sent to %s but audit record failed: %v\n", n.ID, err)
			http.Error(w, "the interrupt was sent, but the audit record failed: "+err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"ok": "interrupted"})
		return
	}
	if pm := a.proc(n); pm != nil {
		if err := pm.Interrupt(n.ID); err != nil {
			code := 500
			if pm.Conflict(err) {
				code = 409
			}
			http.Error(w, err.Error(), code)
			return
		}
		if n.Agent == "claude" {
			a.endClaudePermissionTurn(n)
		} else {
			a.disarmAutoApprove(n.ID)
		}
		writeJSON(w, map[string]string{"ok": "interrupted"})
		return
	}
	// tmux path: the interrupt is a remote keypress and follows the SendKeys
	// contract — whitelisted key only (Escape is Claude Code's documented
	// turn interrupt; C-c on an idle pane clears input and a double press
	// exits the CLI), recorded in the store with pane evidence. Evidence
	// capture is a prerequisite, exactly as in handleKey.
	s := a.server.Session(n.ID)
	cap, err := s.Capture()
	if err != nil {
		http.Error(w, "refusing interrupt without pane evidence (capture failed): "+err.Error(), 500)
		return
	}
	excerpt := lastLines(cap, 12)
	if err := s.SendKey("Escape"); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := a.appendRecord(storeRecord{Type: "key", ID: n.ID, Key: "Escape",
		Keys: []string{"Escape"}, Excerpt: "interrupt: " + excerpt,
		Time: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: interrupt sent to %s but audit record failed: %v\n", n.ID, err)
		http.Error(w, "the interrupt was sent, but the audit record failed: "+err.Error(), 500)
		return
	}
	// Interrupting also withdraws an unconfirmed prior send — the supervisor
	// has taken over. A send still in flight ("submitting") keeps its state:
	// clearing it here would let a second send race the in-flight paste.
	a.mu.Lock()
	if a.sendState[n.ID] != sendSubmitting {
		delete(a.sendState, n.ID)
	}
	a.mu.Unlock()
	// Interrupt is the human taking control back. It ends the lease on every
	// agent; Claude's official Stop hook does not fire on user interrupt.
	if n.Agent == "claude" {
		a.endClaudePermissionTurn(n)
	} else {
		a.disarmAutoApprove(n.ID)
	}
	writeJSON(w, map[string]string{"ok": "interrupted"})
}

// handleChat renders the chat view for any node from its unified session log:
// the store is the read path for conversation history on every transport
// (phase 3 of the session-log consolidation). Transport machinery contributes
// only overlays — the tmux branch derives fallback/diagnostics/needs-input
// from the transcript tailer and pane mechanics; the structured branch adds
// the pending permission and last error from its manager. chat_started and
// prior_turns carry the /clear divider: a source seam in the log starts a
// fresh chat surface under the same activity, with the prior conversation
// preserved behind the seam in the same file.
func (a *app) handleChat(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	// ?history=1: the whole log as ordered surfaces — the on-demand read
	// behind the chat's "show earlier history" divider and the map's earlier
	// stops ("ride back through the journey"). A per-tap read, not a poll
	// path, so it is parsed fresh and uncached; the polled response below
	// stays segment-scoped.
	if r.URL.Query().Get("history") == "1" {
		segs := []sessionlog.HistorySegment{}
		assets := map[string]any{}
		if a.sessionsDir != "" {
			segs = sessionlog.ReadHistory(a.sessionLogPath(n.ID))
			for i := range segs {
				var segAssets map[string]any
				segs[i].Turns, segAssets = a.projectTurns(n.ID, segs[i].Turns)
				for id, v := range segAssets {
					assets[id] = v
				}
			}
		}
		resp := map[string]any{"segments": segs}
		resp["allow_external_attachments"] = a.settings().AllowExternalAttachments
		if len(assets) > 0 {
			resp["assets"] = assets
		}
		writeJSON(w, resp)
		return
	}
	seg := a.segment(n)
	a.mu.Lock()
	var lastMS int64
	if t, ok := a.lastChg[n.ID]; ok {
		lastMS = t.UnixMilli()
	}
	a.mu.Unlock()
	turns, assets := a.projectTurns(n.ID, seg.Turns)
	resp := map[string]any{
		"turns": turns, "last_change": lastMS,
		"chat_started": seg.StartTime, "prior_turns": seg.PriorTurns,
	}
	resp["allow_external_attachments"] = a.settings().AllowExternalAttachments
	// fresh: zero-turn current segment with prior history behind a source
	// seam — a /clear'ed chat. The client trusts this over fallback so a
	// cleared Claude node says "fresh chat — send a prompt" instead of
	// dropping to a terminal peek (P2c). A never-started node has no prior
	// turns and must not claim fresh.
	if isFreshSurface(seg) {
		resp["fresh"] = true
	}
	if assets != nil {
		resp["assets"] = assets
	}
	// Current-segment decision audit surfaces (P3); separate from turns.
	if len(seg.Decisions) > 0 {
		resp["decisions"] = seg.Decisions
	}
	workerClaude := !n.Adopted && a.workers != nil && n.Agent == "claude" && a.workers.manages(n.ID)
	var workerState sessionworker.State
	// Authoritative auto-approval state participates in the full-body ETag.
	var aaView autoApproveView
	if workerClaude {
		workerState = a.workers.State(n.ID)
		aa := workerState.AutoApprove
		aaView = autoApproveView{Supported: aa.Supported, Enabled: aa.Enabled, Phase: aa.Phase, Count: aa.Count, Error: aa.Error}
	} else {
		a.mu.Lock()
		aaView = a.autoApproveViewOf(n)
		a.mu.Unlock()
	}
	resp["auto_approve"] = aaView
	if n.Adopted {
		resp["live"] = "unavailable"
		resp["reply_ready"] = false
		resp["source"] = "transcript"
		resp["error"] = "This external session integration is retired. Saved history remains available; start a fresh chat to continue."
	} else if workerClaude {
		a.claudeWorkerChatInto(resp, n, workerState, seg)
	} else if pm := a.proc(n); pm != nil {
		a.procChatInto(resp, n, pm, seg)
	} else {
		a.tmuxChatInto(resp, n, seg)
	}
	// Polled path only: ETag short-circuit hashes the fully marshalled body
	// (same as /api/state). Hashing log identity alone would miss needs-input,
	// attention, last_change, and other mechanics that change without a log
	// write — the trap that hides an unanswered approval dialog.
	body, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	h := fnv.New64a()
	h.Write(body)
	etag := fmt.Sprintf(`"%x"`, h.Sum64())
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func (a *app) claudeWorkerChatInto(resp map[string]any, n *Node, state sessionworker.State, seg sessionlog.Segment) {
	resp["pending"] = state.Pending
	resp["live"] = state.Live
	resp["fallback"] = state.Fallback
	resp["attention"] = state.Attention
	resp["delivery"] = state.Delivery
	resp["source"] = state.Source
	resp["reason"] = state.Reason
	resp["watermark"] = state.Watermark
	resp["progress"] = state.Progress
	resp["pending_calls"] = state.PendingCalls
	resp["waiting_on"] = state.WaitingOn
	resp["reply_ready"] = state.ReplyReady
	resp["turn_in_flight"] = state.TurnInFlight
	resp["ctx_used"] = seg.Used
	resp["ctx_window"] = ctxWindowFor(seg.Used, seg.Size, n.Model)
	resp["ctx_pct"] = ctxPctOf(seg.Used, ctxWindowFor(seg.Used, seg.Size, n.Model))
	resp["supervision"] = state.Supervision
	if state.LastError != "" {
		resp["error"] = state.LastError
	}
	if state.Delivery == sendSubmitting || state.Delivery == sendUnconfirmed || state.Delivery == sendDelivering {
		resp["pending_prompt"] = n.Prompt
	} else if state.LastError != "" {
		resp["restore_draft"] = n.Prompt
	}
	if state.Compacting {
		resp["compacting"] = true
		if state.CompactTrigger != "" {
			resp["compact_trigger"] = state.CompactTrigger
		}
	}
	if state.ElicitationCount > 0 {
		resp["elicitation_waiting"] = true
		resp["elicitation_count"] = state.ElicitationCount
		resp["elicitations"] = state.Elicitations
	}
	if pending := state.Permission; pending != nil && pending.Dialog {
		resp["perm_dialog_id"] = pending.RequestID
		resp["perm_title"] = pending.Title
		resp["perm_tool_kind"] = pending.ToolKind
		if len(pending.Options) > 0 {
			resp["perm_options"] = pending.Options
		}
		if pending.Manual {
			resp["perm_manual"] = true
		}
		if pending.Reason != "" {
			resp["perm_reason"] = pending.Reason
		}
	}
}

// projectTurns rewrites each turn's attachment markers (internal/asset.Project)
// and general agent-generated Markdown local-path references
// (internal/asset.ProjectAgentPaths, Phase 4) into scimux-asset:<id>
// references, and returns the projected turns plus the response's "assets"
// map, built from only the asset IDs actually referenced after projection —
// never the whole node's asset set. Read-time only: the stored log is never
// touched (upload-design.md "Render-Time Projection, Not Log Rewrite").
// ProjectAgentPaths runs even when byPath is empty — a Markdown path
// reference with no matching ingested asset still needs to become a defined
// "unavailable" chip rather than raw path text (Guaranteed Outcome), so it
// can't be skipped just because nothing has been ingested yet. turns is
// returned unmodified (assets nil) when the node has no session log at all.
func (a *app) projectTurns(nodeID string, turns []transcript.Turn) ([]transcript.Turn, map[string]any) {
	if a.sessionsDir == "" || len(turns) == 0 {
		return turns, nil
	}
	logPath := a.sessionLogPath(nodeID)
	dir := a.nodeDir(nodeID)
	// Anchored assets and the ID index share the node's LogCache with
	// segment/fare — an idle chat poll no longer re-walks the log every tick.
	c := a.sessionLogCache(nodeID)
	anchored := c.AnchoredAssets(logPath)
	blockedByTurn := map[int][]sessionlog.AssetImportEvent{}
	for _, ref := range c.AssetImports(logPath) {
		blockedByTurn[ref.TurnRecord] = append(blockedByTurn[ref.TurnRecord], ref)
	}
	out := make([]transcript.Turn, len(turns))
	referenced := map[string]bool{}
	for i, t := range turns {
		byPath := assetsAsOf(anchored, t.Record)
		bound := assetsBoundTo(anchored, t.Record)
		t.Text = asset.Project(t.Text, byPath)
		t.Text = asset.ProjectVisibleBlocked(t.Text, bound, blockedByTurn[t.Record], dir)
		t.Text = asset.ProjectAgentPaths(t.Text, byPath)
		for _, id := range asset.ReferencedIDs(t.Text) {
			referenced[id] = true
		}
		out[i] = t
	}
	if len(referenced) == 0 {
		return out, nil
	}
	idx := c.Assets(logPath)
	assets := map[string]any{}
	for id := range referenced {
		if rec, ok := idx[id]; ok {
			assets[id] = a.assetSummary(nodeID, rec)
		}
	}
	return out, assets
}

// assetsAsOf builds the SourcePath->asset resolution a turn at record index
// `record` sees: last-wins over every anchored asset owned at or before it.
// anchored is in log order, so anchors are non-decreasing and the last write
// for each path within the window is its newest content. This is the temporal
// anchor — an earlier turn resolves a path to the content current when it was
// written, even after a later turn re-ingests the same path with new bytes.
func assetsAsOf(anchored []sessionlog.AnchoredAsset, record int) map[string]sessionlog.AssetEvent {
	byPath := map[string]sessionlog.AssetEvent{}
	for _, aa := range anchored {
		if aa.Anchor <= record && aa.Asset.AnchorOccurrence == nil {
			byPath[aa.Asset.SourcePath] = aa.Asset
		}
	}
	return byPath
}

func (a *app) nodeDir(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n := a.byID[id]; n != nil {
		return n.Dir
	}
	return ""
}

func assetsBoundTo(anchored []sessionlog.AnchoredAsset, record int) map[int]sessionlog.AssetEvent {
	bound := map[int]sessionlog.AssetEvent{}
	for _, aa := range anchored {
		if aa.Anchor == record && aa.Asset.AnchorOccurrence != nil {
			bound[*aa.Asset.AnchorOccurrence] = aa.Asset
		}
	}
	return bound
}

// assetSummary builds one entry of the chat response's "assets" map.
// Every asset — inline or blob — exposes the same canonical same-origin URL.
// Recorded MIME and inline bytes are never interpolated into a data: URI:
// the asset endpoint is the sole byte-serving and rendering URL, and it
// decides inline-vs-download from the file extension, not from rec.Mime.
func (a *app) assetSummary(nodeID string, rec sessionlog.AssetEvent) map[string]any {
	return map[string]any{
		"name":   rec.Name,
		"mime":   rec.Mime,
		"size":   rec.Size,
		"sha256": rec.SHA256,
		"inline": rec.Storage == "inline",
		"url":    "/api/nodes/" + url.PathEscape(nodeID) + "/assets/" + url.PathEscape(rec.ID),
	}
}

// tmuxChatInto overlays the tmux-transport state onto the shared chat
// response: liveness, attention, delivery, and the fallback decision — the
// mechanics stay with the pane and the transcript tailer even though the
// turns themselves now come from the session log (the mirror keeps the log
// at most one poll tick behind the transcript).
func (a *app) tmuxChatInto(resp map[string]any, n *Node, seg sessionlog.Segment) {
	tl := a.tailerFor(n) // may reset staleness on a relink; read flags after
	if tl != nil {
		// The chat poll may race ahead of the supervisor tick. Refresh the
		// lightweight tailer here so reply_ready is withdrawn immediately by a
		// new user/tool record and does not leave a stale send button enabled.
		tl.Poll()
	}
	a.mu.Lock()
	pending := n.Transcript == ""
	live := a.live[n.ID]
	stale := a.staleChat[n.ID]
	attn := a.attn[n.ID]
	delivery := a.sendState[n.ID]
	if delivery == sendInitialUnconfirmed {
		// Initial delivery has a distinct internal state so the poller can
		// reconcile late transcript evidence without weakening the manual gate
		// for ordinary sends. It gets its own browser value rather than the
		// ordinary "unconfirmed": that presentation tells the user the send
		// could not be confirmed and offers a resume button, but here
		// SessionStart has arrived and only the lazily written transcript is
		// outstanding, so there is nothing yet to doubt.
		delivery = sendDelivering
	}
	agent, model, ended := n.Agent, n.Model, n.EndedAt != ""
	a.mu.Unlock()
	turns := seg.Turns
	a.mu.Lock()
	sup := a.claudeSupervisionOf(n)
	launchErr := a.claudeLaunchErr[n.ID]
	armed := a.claudeAutoApproveArmedLocked(n)
	a.mu.Unlock()
	dlg := a.claudeVisibleDialog(n)

	// fallback signals "the transcript is not (or no longer) making sense" —
	// missing, not yet populated (or not yet mirrored), unreadable,
	// format-incompatible from the start, structurally broken after valid
	// turns (Unparseable), or still growing through whole pane-activity
	// cycles without one recognizable chat record (staleChat: a typed future
	// format). A path string existing does not mean the transcript is usable;
	// the client must degrade to the pane snapshot in every one of these cases.
	// Claude (strict, starting, unsupported, or failed) never uses that
	// degradation to force the terminal.
	fallback := len(turns) == 0 || (tl != nil && tl.Unparseable()) || stale
	if agent == "claude" {
		fallback = false
		if attn == "inspect" {
			attn = ""
		}
	}

	// Diagnostics: where the chat content comes from and why attention (or
	// its absence) looks the way it does — so a missed question is
	// distinguishable from "no transcript" versus "no structured request".
	source := "transcript"
	switch {
	case agent == "pi" || agent == "opencode":
		source = "terminal_only" // supported via pane peek + send only
	case agent == "claude" && (sup == claudeSupStarting || pending):
		source = "none"
	case pending:
		source = "none"
	case fallback:
		source = "peek"
	}
	reason := ""
	switch {
	case agent == "claude" && sup == claudeSupUnsupported:
		reason = "claude_unsupported"
	case agent == "claude" && sup == claudeSupFailed:
		reason = "claude_launch_error"
	case agent == "claude" && sup == claudeSupStarting:
		reason = "claude_starting"
	case agent == "claude" && (tl != nil && tl.Unparseable() || stale):
		reason = "claude_transcript_fault"
	case live == "active":
		reason = "pane_active"
	case attn == "question":
		reason = "waiting_question"
	case attn == "approval":
		reason = "waiting_approval"
	case attn == "inspect":
		reason = "quiet_inspect"
	case pending:
		reason = "no_transcript"
	case fallback:
		reason = "no_structured_request"
	}
	var watermark int64
	var prog, pendCalls int
	var waiting string
	if tl != nil {
		watermark, prog = tl.Progress()
		pendCalls = tl.PendingCount()
		waiting, _ = tl.WaitingOn()
	}
	replyReady := tl != nil && tl.EndTurn() && pendCalls == 0 && attn == "" && !ended
	// Claude's Stop/StopFailure hook, not pane activity or an early transcript
	// boundary, owns the turn lease.  Keep the composer interruptible for the
	// whole accepted turn; closeClaudeAcceptedTurnLocked clears this only after
	// the matching hook has been drained (or an explicit interrupt/reset ends
	// the turn).
	turnInFlight := false
	if agent == "claude" {
		turnInFlight = a.claudeAcceptedTurnOf(n.ID).Turn != ""
	}
	compacting, compactTrigger := a.claudeCompactingState(n)
	if compacting {
		replyReady = false
	}
	elicitationWaiting, elicitationCount, elicitations := a.claudeElicitationChatState(n)
	if elicitationWaiting {
		replyReady = false
	}
	// Usage is segment-scoped: a /clear seam resets the gauge together with
	// the context it measures.
	ctxUsed := seg.Used
	ctxWindow := ctxWindowFor(seg.Used, seg.Size, model)
	ctxPct := ctxPctOf(ctxUsed, ctxWindow)
	resp["pending"] = pending
	resp["live"] = live
	resp["fallback"] = fallback
	resp["attention"] = attn
	resp["delivery"] = delivery
	resp["source"] = source
	resp["reason"] = reason
	resp["watermark"] = watermark
	resp["progress"] = prog
	resp["pending_calls"] = pendCalls
	resp["waiting_on"] = waiting
	resp["reply_ready"] = replyReady
	resp["turn_in_flight"] = turnInFlight
	resp["ctx_used"] = ctxUsed
	resp["ctx_window"] = ctxWindow
	resp["ctx_pct"] = ctxPct
	if agent == "claude" {
		resp["supervision"] = string(sup)
		if explain := claudeChatSupervisionExplain(sup, launchErr, ended); explain != "" {
			resp["error"] = explain
		}
		if delivery == sendSubmitting || delivery == sendUnconfirmed || delivery == sendDelivering {
			resp["pending_prompt"] = n.Prompt
		}
		if launchErr != "" && delivery != sendSubmitting && delivery != sendUnconfirmed && delivery != sendDelivering {
			resp["restore_draft"] = n.Prompt
		}
		if compacting {
			resp["compacting"] = true
			if compactTrigger != "" {
				resp["compact_trigger"] = compactTrigger
			}
		}
		if elicitationWaiting {
			resp["elicitation_waiting"] = true
			resp["elicitation_count"] = elicitationCount
			resp["elicitations"] = elicitations
		}
		if note := a.claudeDialogNoteOf(n.ID); note != "" && dlg.DialogID == "" {
			resp["perm_manual"] = true
			resp["perm_reason"] = note
		}
		if dlg.DialogID != "" {
			// perm_dialog_id is a supervisor identity, not a PermissionRequest
			// id. Action keys come only from a validated current AX menu (or
			// the exact pre-session trust menu). Missing options → manual
			// response + Open Terminal, never invented choices.
			resp["perm_dialog_id"] = dlg.DialogID
			resp["perm_title"] = dlg.Title
			resp["perm_tool_kind"] = claudePermToolKind(dlg.Tool)
			opts := a.claudeDialogOptions(n, dlg)
			if len(opts) > 0 {
				resp["perm_options"] = opts
			} else {
				resp["perm_manual"] = true
			}
			if dlg.Question {
				resp["perm_reason"] = "This is a question or plan choice — auto-approve will not answer it."
			}
			if armed {
				resp["perm_manual"] = true
				if dlg.Question {
					if len(opts) > 0 {
						resp["perm_reason"] = "Auto-approve is armed and will not answer this. Choose below, or open Terminal."
					} else {
						resp["perm_reason"] = "Auto-approve is armed and will not answer this. Open Terminal to respond."
					}
				} else if resp["perm_reason"] == nil {
					if len(opts) > 0 {
						resp["perm_reason"] = "Auto-approve could not answer this permission request. Choose below, or open Terminal."
					} else {
						resp["perm_reason"] = "Auto-approve could not answer this permission request. Open Terminal to respond."
					}
				}
			} else if len(opts) == 0 && resp["perm_reason"] == nil {
				resp["perm_reason"] = "A dialog is visible but its options could not be read safely. Open Terminal to respond."
			}
		}
	}
}

// procChatInto overlays structured transport state (ACP, codex app-server, or Muse MSP)
// state onto the shared chat response. No pane and no tailer: liveness and
// any pending permission come from the manager, while history and usage came
// from the same session log the manager writes. The tmux-only fields are
// neutralized (fallback false, delivery "", no watermark/pending_calls) so
// the web client's rendering path is shared; source "acp" selects the
// structured-node rendering path for every bridge; perm_* carries the pending
// approval and error a failed/empty turn.
func (a *app) procChatInto(resp map[string]any, n *Node, pm procManager, seg sessionlog.Segment) {
	live := pm.Live(n.ID)
	lastErr := userFacingAgentError(n.Agent, pm.LastError(n.ID))
	pending, hasPerm := pm.Pending(n.ID)
	attn := ""
	if hasPerm {
		attn = "approval"
	}
	reason := ""
	switch {
	case live == "active":
		reason = "turn_active"
	case attn == "approval":
		reason = "waiting_approval"
	case lastErr != "":
		reason = "turn_error"
	}
	ctxPct := ctxPctOf(seg.Used, seg.Size)
	resp["pending"] = false
	resp["live"] = live
	resp["fallback"] = false
	resp["attention"] = attn
	resp["delivery"] = ""
	resp["source"] = "acp"
	resp["reason"] = reason
	resp["watermark"] = int64(0)
	resp["progress"] = len(seg.Turns)
	resp["pending_calls"] = 0
	resp["waiting_on"] = pending.Title
	resp["reply_ready"] = false
	// For ACP, codex app-server, and Muse MSP, Live==active is the manager's protected
	// turnActive latch. It falls only when Prompt returns / turn/completed (or
	// an interrupt/error closes the turn), so it is stronger than visual or
	// timing-based liveness.
	resp["turn_in_flight"] = live == "active"
	resp["ctx_used"] = seg.Used
	resp["ctx_window"] = seg.Size
	resp["ctx_pct"] = ctxPct
	resp["error"] = lastErr
	resp["perm_title"] = pending.Title
	resp["perm_options"] = pending.Options
	resp["perm_tool_kind"] = pending.ToolKind
	resp["perm_reason"] = pending.Reason
	// Opaque request identity only while a permission is actually pending.
	// Idle structured chats and tmux nodes must not invent a request id.
	if hasPerm && pending.RequestID != "" {
		resp["perm_request_id"] = pending.RequestID
	}
}

// tmuxKeySequence maps a semantic web choice to the physical tmux key
// sequence for one /key action. Claude nodes launched in screen-reader mode
// (AXScreenReader) need a digit or y/n followed immediately by Enter; every
// other allowed key, and every non-AX / non-Claude / non-tmux node, stays a
// single physical key. The renderer is taken from durable launch metadata —
// never from pane contents, process args, or transcript text. Nil n is
// treated as non-AX (single key).
func tmuxKeySequence(n *Node, key string) []string {
	if n != nil && n.Agent == "claude" && n.transport() == "tmux" && n.AXScreenReader {
		switch key {
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "y", "n":
			return []string{key, "Enter"}
		}
	}
	return []string{key}
}

// handleKey accepts one whitelisted semantic choice for an approval prompt or
// question menu. Structured transports resolve it through their protocol;
// tmux captures pane evidence, delivers the renderer-specific physical key
// sequence, and audits both the semantic choice and physical keys.
func (a *app) handleKey(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if a.refuseRetiredExternal(w, n) {
		return
	}
	if a.refuseEnded(w, n) {
		return
	}
	var body struct {
		Key       string `json:"key"`
		RequestID string `json:"request_id"`
		DialogID  string `json:"dialog_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.Key == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if !tmuxsession.AllowedKey(body.Key) {
		http.Error(w, fmt.Sprintf("key %q not allowed", body.Key), 400)
		return
	}
	if a.workers != nil && n.Agent == "claude" && a.workers.manages(n.ID) {
		epoch := body.DialogID
		if epoch == "" {
			epoch = body.RequestID
		}
		if epoch == "" {
			http.Error(w, "dialog_id is required for Claude permission decisions", http.StatusBadRequest)
			return
		}
		prepared, err := a.workers.PreparePermission(n.ID, epoch, body.Key)
		if err != nil {
			code := http.StatusBadRequest
			if a.workers.Conflict(err) {
				code = http.StatusConflict
			}
			http.Error(w, err.Error(), code)
			return
		}
		if err := a.appendRecord(storeRecord{Type: "key", ID: n.ID, Key: body.Key,
			Keys: prepared.Keys, Excerpt: prepared.Evidence, Time: time.Now().UTC().Format(time.RFC3339)}); err != nil {
			http.Error(w, "refusing to answer without an audit record: "+err.Error(), 500)
			return
		}
		if err := a.workers.Deliver(n.ID, prepared.Token); err != nil {
			fmt.Fprintf(os.Stderr, "scimux: key %q audited on %s but delivery failed: %v\n", body.Key, n.ID, err)
			http.Error(w, "the decision was recorded, but delivering it to the agent failed: "+err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"ok": "sent"})
		return
	}
	// Structured protocols (ACP, codex, Muse): the key answers a structured permission
	// request (there is no pane to press it into). Because resolution is
	// structured, the key can be mapped to an option and audited *before* the
	// agent is told the answer — unlike a tmux keypress, this need not be a
	// post-send best-effort write. Persist the decision first, then deliver, so
	// a store failure can never leave an unaudited permission answer already
	// acted on (finding 53). The tool title stands in for the pane excerpt as
	// decision evidence. Keys stays absent: no terminal keys were pressed, and
	// this branch must never receive a synthetic Enter.
	//
	// request_id binds the decision to the exact pending request the UI (or
	// auto-approver) evaluated. Missing → 400; stale → 409. Neither writes an
	// audit record nor delivers. tmux/Claude keeps the {key}-only body.
	if pm := a.proc(n); pm != nil {
		if body.RequestID == "" {
			http.Error(w, "request_id is required for structured permission decisions", 400)
			return
		}
		optID, evidence, err := pm.PrepareResolve(n.ID, body.RequestID, body.Key)
		if err != nil {
			code := 400
			if pm.Conflict(err) {
				code = 409
			}
			http.Error(w, err.Error(), code)
			return
		}
		if err := a.appendRecord(storeRecord{Type: "key", ID: n.ID, Key: body.Key,
			Excerpt: evidence, Time: time.Now().UTC().Format(time.RFC3339)}); err != nil {
			http.Error(w, "refusing to answer without an audit record: "+err.Error(), 500)
			return
		}
		if err := pm.Deliver(n.ID, optID); err != nil {
			fmt.Fprintf(os.Stderr, "scimux: key %q audited on %s but delivery failed: %v\n", body.Key, n.ID, err)
			http.Error(w, "the decision was recorded, but delivering it to the agent failed: "+err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"ok": "sent"})
		return
	}
	s := a.server.Session(n.ID)
	// Evidence capture is a prerequisite, not best-effort: a keypress whose
	// pane context cannot be photographed would produce an audit record that
	// cannot distinguish "empty pane" from "no evidence". Refuse and let the
	// supervisor retry (or attach) instead of acting unauditably.
	cap, err := s.Capture()
	if err != nil {
		http.Error(w, "refusing keypress without pane evidence (capture failed): "+err.Error(), 500)
		return
	}
	excerpt := lastLines(cap, 12)
	// Semantic web choice → physical sequence (AX Claude: choice + Enter).
	// One browser action becomes one validated SendKeys delivery.
	seq := tmuxKeySequence(n, body.Key)
	claudeEpoch := ""
	if n.Agent == "claude" {
		dlg := a.claudeVisibleDialog(n)
		// A dialog_id in the body is fail-closed: send nothing unless it is
		// exactly the live identity. A missing, retired, or mismatched identity
		// must never type a digit/Enter into Claude's prompt.
		if body.DialogID != "" {
			if dlg.DialogID == "" || body.DialogID != dlg.DialogID {
				http.Error(w, "stale permission dialog", http.StatusConflict)
				return
			}
			claudeEpoch = dlg.DialogID
		} else if dlg.DialogID != "" {
			if body.RequestID != "" && body.RequestID == dlg.DialogID {
				claudeEpoch = dlg.DialogID
			} else {
				http.Error(w, "dialog_id is required for Claude permission decisions", 400)
				return
			}
		}
		if claudeEpoch != "" {
			opts := a.claudeDialogOptions(n, dlg)
			if len(opts) == 0 {
				http.Error(w, "dialog options are not available; open the terminal", http.StatusConflict)
				return
			}
			allowed := false
			for _, o := range opts {
				if o.Key == body.Key {
					allowed = true
					break
				}
			}
			if !allowed {
				http.Error(w, "key is not an option on this dialog", 400)
				return
			}
		}
	}
	if err := s.SendKeys(seq...); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Deliberate non-poller write: successful key delivery clears attn/attnAt
	// now rather than waiting for the tool-call record to resolve (it only lands
	// when the approved tool completes, which for a long tool is minutes away —
	// R21.2). Failures before SendKeys must not reach here. The mechanical
	// pipeline re-raises on the next tick if the dialog is still up, mirroring
	// what the structured path gets for free from pm.Attention.
	//
	// For a strict hooked Claude node, retire only the visible-dialog epoch.
	// Standing asks are not a guessed Notification identity and stay until
	// transcript/tool evidence or a turn boundary retires them.
	if n.Agent == "claude" && claudeEpoch != "" {
		a.retireClaudeVisibleDialog(n, claudeEpoch)
	}
	a.mu.Lock()
	prevAttn := a.attn[n.ID]
	a.attn[n.ID] = ""
	delete(a.attnAt, n.ID)
	a.mu.Unlock()
	// V2-P2: closing the wait edge on web-key answer (same mechanical signal).
	a.persistAttentionTransition(n, prevAttn, "")
	// The audit record is part of the operation's success contract: a keypress
	// whose evidence cannot be persisted must not report plain success. The
	// physical sequence is already delivered (cannot be unsent), so say exactly
	// that. Key = semantic choice; Keys = physical tmux sequence.
	if err := a.appendRecord(storeRecord{Type: "key", ID: n.ID, Key: body.Key,
		Keys: seq, Excerpt: excerpt, Time: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: key %q sent to %s but audit record failed: %v\n", body.Key, n.ID, err)
		http.Error(w, "key was sent, but persisting the audit record failed: "+err.Error(), 500)
		return
	}
	writeJSON(w, map[string]string{"ok": "sent"})
}

// lastLines returns the last n lines of s with trailing blank lines removed —
// the visible bottom of a pane, where approval dialogs live.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, " \t\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// handlePeek returns the pane snapshot: the last 200 scrollback lines by
// default, or only the currently rendered screen with ?mode=visible — the
// decision view, where an approval dialog is not buried under history.
func (a *app) handlePeek(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if a.refuseRetiredExternal(w, n) {
		return
	}
	if pm := a.proc(n); pm != nil {
		// No pane to photograph: peek renders a tail of the raw event log.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		peek := pm.Peek(n.ID)
		workers, workerBacked := pm.(*workerManager)
		if workerBacked && n.Agent == "claude" {
			peek = workers.PeekMode(n.ID, r.URL.Query().Get("mode"))
		} else if workerBacked && peek == "" {
			// A dead structured worker has no RPC endpoint, but its canonical
			// session log remains the same Inspect source the monolith uses.
			switch n.transport() {
			case "acp":
				peek = a.acp.Peek(n.ID)
			case "codex":
				peek = a.codex.Peek(n.ID)
			case "muse":
				peek = a.muse.Peek(n.ID)
			}
		}
		fmt.Fprint(w, peek)
		return
	}
	s := a.server.Session(n.ID)
	var cap string
	var err error
	if r.URL.Query().Get("mode") == "visible" {
		cap, err = s.CaptureVisible()
	} else {
		cap, err = s.Capture()
	}
	if err != nil {
		cap = "(session exited or unavailable)\n\n" + err.Error()
	} else {
		a.notePeekDialog(n, s)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, cap)
}

// notePeekDialog runs the dialog check when a human opens the terminal view.
// Peeking is exactly the manual gesture that catches a dialog the quiet gate
// cannot see (parallel calls queued behind it keep the pane animating), so
// automate it at the same moment: one visible capture, attention only ever
// added — the poller keeps owning clearing. Existing attention is never
// overwritten or restamped. Unlike the poller's tick path this is one-shot
// and human-triggered, so it skips the confined-animation gate.
//
// Two raise paths share the quiet-branch predicate (P1c):
//  1. unresolved tool call + dialoghint matcher → attentionKind (approval/question);
//  2. quietAttentionFallback — structural matcher (dialog) or owing stall
//     (inspect) — for the Claude late-flush case where WaitingOn is false.
func (a *app) notePeekDialog(n *Node, s *tmuxsession.Session) {
	// Claude never gains inspect/owing attention from a peek. Strict nodes
	// only raise from a proven permission dialog; unsupported nodes do not
	// enter fallback supervision.
	if n != nil && n.Agent == "claude" {
		return
	}
	// Skip work when attention is already set — only ever raises fresh
	// attention, never overwrites (efficiency, 2026-07-20 batch).
	a.mu.Lock()
	already := a.attn[n.ID] != ""
	quietSince := a.lastChg[n.ID]
	a.mu.Unlock()
	if already {
		return
	}
	tl := a.tailerFor(n)
	if tl != nil {
		tl.Poll()
	}
	visible, err := s.CaptureVisible()
	if err != nil {
		visible = ""
	}
	kind := ""
	if tl != nil {
		if name, ok := tl.WaitingOn(); ok && visible != "" && dialoghint.ClassifyVisible(visible) {
			kind = attentionKind(name)
		}
	}
	if kind == "" {
		kind = quietAttentionFallback(tl, visible, quietSince, n.AXScreenReader)
	}
	if kind == "" {
		return
	}
	a.mu.Lock()
	if a.attn[n.ID] == "" {
		a.attn[n.ID] = kind
		a.attnAt[n.ID] = time.Now() // fresh evidence: start the preserve window (R21.2)
	}
	a.mu.Unlock()
}
