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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/dialoghint"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/tmuxsession"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// segment returns the node's current conversation — the session log's tail
// after its last source seam. A /clear appends a seam, so this is what makes
// "fresh chat surface under the same activity" uniform across transports.
func (a *app) segment(n *Node) sessionlog.Segment {
	if a.sessionsDir == "" {
		return sessionlog.Segment{Turns: []transcript.Turn{}} // bare test apps
	}
	a.mu.Lock()
	if a.segCache == nil {
		a.segCache = map[string]*sessionlog.Cache{}
	}
	c := a.segCache[n.ID]
	if c == nil {
		c = &sessionlog.Cache{}
		a.segCache[n.ID] = c
	}
	a.mu.Unlock()
	return c.Segment(a.sessionLogPath(n.ID))
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
	atts, err := a.resolveAttachments(n.ID, body.Attachments)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// delivered is what the transport receives and records: the user's text with
	// a plain-text reference to each uploaded file appended (extendPrompt). The
	// raw text is kept only for /clear detection and unconfirmed-draft echo.
	delivered := extendPrompt(body.Text, atts)
	// Structured-protocol delivery (ACP, codex) is reliable (no pane-ack race,
	// so no "unconfirmed" state), but the preflight still holds: refuse a second
	// turn while one is in flight, and refuse entirely if the subprocess is
	// gone. The manager records the user turn before prompting (a log-append
	// failure refuses the send) — see the Send contract on each manager.
	if pm := a.proc(n); pm != nil {
		if pm.Live(n.ID) == "active" {
			http.Error(w, "a turn to this node is still in flight", 409)
			return
		}
		// The structured transports have no TUI to interpret slash commands,
		// so scimux implements /clear itself: a fresh protocol session on the
		// same process, recorded as a source seam — same page-turn semantics
		// as Claude's /clear, same log file, same node.
		if strings.TrimSpace(body.Text) == "/clear" {
			// Freeze the closing station's label before the page turns, so a
			// later rename only moves the fresh head (spec A/B/C). The manager
			// appends the clear seam inside Clear; this reads the pre-clear stop
			// chain, so it must run first.
			a.snapshotClosingStation(n)
			if err := pm.Clear(n.ID); err != nil {
				code := 500
				if pm.Conflict(err) {
					code = 409
				}
				http.Error(w, err.Error(), code)
				return
			}
			writeJSON(w, map[string]string{"status": "acknowledged"})
			return
		}
		if err := pm.Send(n.ID, delivered); err != nil {
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
	a.mu.Lock()
	switch a.sendState[n.ID] {
	case "submitting":
		a.mu.Unlock()
		http.Error(w, "a send to this node is still in flight", 409)
		return
	case "unconfirmed":
		a.mu.Unlock()
		http.Error(w, "the previous send is unconfirmed — check the terminal, then recheck", 409)
		return
	}
	a.sendState[n.ID] = "submitting"
	a.mu.Unlock()

	// Transcript watermark before the send: a new user turn appearing is the
	// structured acknowledgement (slash commands may never enter the
	// transcript — for those the mechanical pane change has to carry it).
	turnsBefore := -1
	tl := a.tailerFor(n)
	if tl != nil {
		turnsBefore = len(tl.Poll())
	}
	acked, err := a.server.Session(n.ID).SendAck(delivered)
	if err != nil {
		a.mu.Lock()
		delete(a.sendState, n.ID)
		a.mu.Unlock()
		http.Error(w, err.Error(), 500)
		return
	}
	if !acked && tl != nil && len(tl.Poll()) > turnsBefore {
		acked = true
	}
	a.mu.Lock()
	if acked {
		delete(a.sendState, n.ID)
	} else {
		a.sendState[n.ID] = "unconfirmed"
	}
	a.mu.Unlock()
	// SendAck succeeded, so the prompt was delivered to the pane (acked or
	// unconfirmed — both consume budget). /clear is a page turn, not a turn.
	if strings.TrimSpace(body.Text) != "/clear" {
		a.noteUsagePrompt(n.Agent)
	}
	// /clear delivered through scimux is a *known* session rollover: Claude
	// Code starts a fresh session file and the linked transcript goes dead.
	// Retire the link right away — the UI degrades honestly to peek (the old
	// conversation is gone from the pane too), and the phase-end relink picks
	// up the new session file after the next turn. A /clear typed directly
	// into an attached pane still relies on the mtime heuristic.
	if acked && n.Agent == "claude" && strings.TrimSpace(body.Text) == "/clear" {
		a.retireTranscript(n)
	}
	if !acked {
		writeJSON(w, map[string]string{"status": "unconfirmed", "text": body.Text})
		return
	}
	writeJSON(w, map[string]string{"status": "acknowledged"})
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
	key := n.CreatedAt
	if ct := a.segment(n).ClearTimes; len(ct) > 0 {
		key = ct[len(ct)-1]
	}
	if key == "" {
		return
	}
	w := &sessionlog.Writer{Path: logPath}
	if err := w.Append(sessionlog.NewStation(key, n.Title, n.Description)); err != nil {
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
	// Deliberate non-poller reset after durable retirement only: poller-owned
	// maps must drop the old file's tailer/progress/mirror so the next segment
	// cannot inherit them. Persist failure returns above without touching these.
	a.mu.Lock()
	n.Transcript, n.SessionID = "", ""
	delete(a.tailers, n.ID)
	delete(a.chatMark, n.ID)
	delete(a.staleChat, n.ID)
	// Drop the mirror's in-memory watermark too: it rebuilds from the log
	// (which now ends in the seam below), exactly like after a restart — so
	// a relink can never attribute pre-/clear turns to the fresh segment.
	delete(a.mirrors, n.ID)
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
	a.mu.Lock()
	if a.sendState[n.ID] == "submitting" {
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
	if a.refuseEnded(w, n) {
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
		writeJSON(w, map[string]string{"ok": "interrupted"})
		return
	}
	// tmux path: the interrupt is a remote keypress and follows the SendKey
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
		Excerpt: "interrupt: " + excerpt, Time: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: interrupt sent to %s but audit record failed: %v\n", n.ID, err)
		http.Error(w, "the interrupt was sent, but the audit record failed: "+err.Error(), 500)
		return
	}
	// Interrupting also withdraws an unconfirmed prior send — the supervisor
	// has taken over. A send still in flight ("submitting") keeps its state:
	// clearing it here would let a second send race the in-flight paste.
	a.mu.Lock()
	if a.sendState[n.ID] != "submitting" {
		delete(a.sendState, n.ID)
	}
	a.mu.Unlock()
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
	if assets != nil {
		resp["assets"] = assets
	}
	if pm := a.proc(n); pm != nil {
		a.procChatInto(resp, n, pm, seg)
	} else {
		a.tmuxChatInto(resp, n, seg)
	}
	writeJSON(w, resp)
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
	byPath := sessionlog.ReadAssetsByPath(logPath)
	out := make([]transcript.Turn, len(turns))
	referenced := map[string]bool{}
	for i, t := range turns {
		t.Text = asset.Project(t.Text, byPath)
		t.Text = asset.ProjectAgentPaths(t.Text, byPath)
		for _, id := range asset.ReferencedIDs(t.Text) {
			referenced[id] = true
		}
		out[i] = t
	}
	if len(referenced) == 0 {
		return out, nil
	}
	idx := sessionlog.ReadAssets(logPath)
	assets := map[string]any{}
	for id := range referenced {
		if rec, ok := idx[id]; ok {
			assets[id] = a.assetSummary(nodeID, rec)
		}
	}
	return out, assets
}

// assetSummary builds one entry of the chat response's "assets" map (see
// upload-design.md "Rendering API"): inline assets carry their bytes as a
// data: URI so the client never round-trips to the download endpoint just
// to paint a thumbnail; blob assets carry the download URL instead.
func (a *app) assetSummary(nodeID string, rec sessionlog.AssetEvent) map[string]any {
	m := map[string]any{
		"name": rec.Name, "mime": rec.Mime, "size": rec.Size, "sha256": rec.SHA256,
	}
	if rec.Storage == "inline" {
		m["inline"] = true
		m["data"] = "data:" + rec.Mime + ";base64," + rec.Bytes
	} else {
		m["inline"] = false
		m["url"] = "/api/nodes/" + url.PathEscape(nodeID) + "/assets/" + url.PathEscape(rec.ID)
	}
	return m
}

// tmuxChatInto overlays the tmux-transport state onto the shared chat
// response: liveness, attention, delivery, and the fallback decision — the
// mechanics stay with the pane and the transcript tailer even though the
// turns themselves now come from the session log (the mirror keeps the log
// at most one poll tick behind the transcript).
func (a *app) tmuxChatInto(resp map[string]any, n *Node, seg sessionlog.Segment) {
	tl := a.tailerFor(n) // may reset staleness on a relink; read flags after
	a.mu.Lock()
	pending := n.Transcript == ""
	live := a.live[n.ID]
	stale := a.staleChat[n.ID]
	attn := a.attn[n.ID]
	delivery := a.sendState[n.ID]
	agent, model := n.Agent, n.Model
	a.mu.Unlock()
	turns := seg.Turns
	// fallback signals "the transcript is not (or no longer) making sense" —
	// missing, not yet populated (or not yet mirrored), unreadable,
	// format-incompatible from the start, structurally broken after valid
	// turns (Unparseable), or still growing through whole pane-activity
	// cycles without one recognizable chat record (staleChat: a typed future
	// format). A path string existing does not mean the transcript is usable;
	// the client must degrade to the pane snapshot in every one of these cases.
	fallback := len(turns) == 0 || (tl != nil && tl.Unparseable()) || stale

	// Diagnostics: where the chat content comes from and why attention (or
	// its absence) looks the way it does — so a missed question is
	// distinguishable from "no transcript" versus "no structured request".
	source := "transcript"
	switch {
	case agent == "pi" || agent == "opencode":
		source = "terminal_only" // supported via pane peek + send only
	case pending:
		source = "none"
	case fallback:
		source = "peek"
	}
	reason := ""
	switch {
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
	resp["ctx_used"] = ctxUsed
	resp["ctx_window"] = ctxWindow
	resp["ctx_pct"] = ctxPct
}

// procChatInto overlays the structured-transport (ACP or codex app-server)
// state onto the shared chat response. No pane and no tailer: liveness and
// any pending permission come from the manager, while history and usage came
// from the same session log the manager writes. The tmux-only fields are
// neutralized (fallback false, delivery "", no watermark/pending_calls) so
// the web client's rendering path is shared; source "acp" selects the
// structured-node rendering path for both bridges; perm_* carries the pending
// approval and error a failed/empty turn.
func (a *app) procChatInto(resp map[string]any, n *Node, pm procManager, seg sessionlog.Segment) {
	live := pm.Live(n.ID)
	lastErr := pm.LastError(n.ID)
	permTitle, permOptions, hasPerm := pm.Pending(n.ID)
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
	resp["waiting_on"] = permTitle
	resp["ctx_used"] = seg.Used
	resp["ctx_window"] = seg.Size
	resp["ctx_pct"] = ctxPct
	resp["error"] = lastErr
	resp["perm_title"] = permTitle
	resp["perm_options"] = permOptions
}

// handleKey presses one whitelisted key in the node's pane — answering an
// approval prompt or question menu remotely — and appends the pane's bottom
// lines plus the key to the store as decision evidence.
func (a *app) handleKey(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if a.refuseEnded(w, n) {
		return
	}
	var body struct{ Key string }
	if err := decodeJSON(w, r, &body); err != nil || body.Key == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if !tmuxsession.AllowedKey(body.Key) {
		http.Error(w, fmt.Sprintf("key %q not allowed", body.Key), 400)
		return
	}
	// Structured protocols (ACP, codex): the key answers a structured permission
	// request (there is no pane to press it into). Because resolution is
	// structured, the key can be mapped to an option and audited *before* the
	// agent is told the answer — unlike a tmux keypress, this need not be a
	// post-send best-effort write. Persist the decision first, then deliver, so
	// a store failure can never leave an unaudited permission answer already
	// acted on (finding 53). The tool title stands in for the pane excerpt as
	// decision evidence.
	if pm := a.proc(n); pm != nil {
		optID, evidence, err := pm.PrepareResolve(n.ID, body.Key)
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
	if err := s.SendKey(body.Key); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Deliberate non-poller write: successful key delivery clears attn/attnAt
	// now rather than waiting for the tool-call record to resolve (it only lands
	// when the approved tool completes, which for a long tool is minutes away —
	// R21.2). Failures before SendKey must not reach here. The mechanical
	// pipeline re-raises on the next tick if the dialog is still up, mirroring
	// what the structured path gets for free from pm.Attention.
	a.mu.Lock()
	a.attn[n.ID] = ""
	delete(a.attnAt, n.ID)
	a.mu.Unlock()
	// The audit record is part of the operation's success contract: a keypress
	// whose evidence cannot be persisted must not report plain success. The key
	// is already delivered (cannot be unsent), so say exactly that.
	if err := a.appendRecord(storeRecord{Type: "key", ID: n.ID, Key: body.Key,
		Excerpt: excerpt, Time: time.Now().UTC().Format(time.RFC3339)}); err != nil {
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
	if pm := a.proc(n); pm != nil {
		// No pane to photograph: peek renders a tail of the raw event log.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, pm.Peek(n.ID))
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

// notePeekDialog runs the corroborated dialog check when a human opens the
// terminal view. Peeking is exactly the manual gesture that catches a dialog
// the quiet gate cannot see (parallel calls queued behind it keep the pane
// animating), so automate it at the same moment: one visible capture,
// transcript corroboration required, attention only ever added — the poller
// keeps owning clearing. Deliberate non-poller raise: only unresolved
// structured evidence plus a visible dialog matcher may set attn/attnAt, and
// existing attention is never overwritten or restamped. Unlike the poller's
// tick path this is one-shot and human-triggered, so it skips the
// confined-animation gate.
func (a *app) notePeekDialog(n *Node, s *tmuxsession.Session) {
	tl := a.tailerFor(n)
	if tl == nil {
		return
	}
	tl.Poll()
	name, ok := tl.WaitingOn()
	if !ok {
		return
	}
	// Skip the visible-pane capture entirely when attention is already set — the
	// poller has classified this node and notePeekDialog only ever raises fresh
	// attention, never overwrites (efficiency, 2026-07-20 batch).
	a.mu.Lock()
	already := a.attn[n.ID] != ""
	a.mu.Unlock()
	if already {
		return
	}
	visible, err := s.CaptureVisible()
	if err != nil || !dialoghint.ClassifyVisible(visible) {
		return
	}
	a.mu.Lock()
	if a.attn[n.ID] == "" {
		a.attn[n.ID] = attentionKind(name)
		if a.attnAt == nil { // tests build app literals without the map
			a.attnAt = map[string]time.Time{}
		}
		a.attnAt[n.ID] = time.Now() // fresh evidence: start the preserve window (R21.2)
	}
	a.mu.Unlock()
}
