// node_api.go — HTTP orchestration for node create/update/exit/delete.
//
// Ownership (existing boundaries; this file does not change locks):
//   - Router registration stays in router.go.
//   - Validation, resolve/create/launch, process/session discovery, and
//     closeOwned stay in node_lifecycle.go; these handlers only sequence them.
//   - Store append/replay and session-log archival stay in store.go.
//   - Attachment/asset archive helpers live in attachment_api.go; delete only
//     sequences a.archiveAttachments/Assets after removeNodeLocked.
//   - a.mu: create releases before createNode (which manages its own lock);
//     update holds through node mutation/persist except the station
//     path; exit/delete persist under a.mu then release before closeOwned
//     (and delete re-locks for re-assert or removeNodeLocked). Never call
//     segment() under a.mu from these handlers.
package app

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
)

func (a *app) handleNewNode(w http.ResponseWriter, r *http.Request) {
	var n Node
	if err := decodeJSON(w, r, &n); err != nil {
		http.Error(w, "bad request: "+err.Error(), 400)
		return
	}
	// Scrub every server-owned field before validation: a create request only
	// supplies launch config (title/description/prompt/agent/model/effort/dir/
	// parent/lane_id/rationale). Identity, external ownership, liveness, the linked
	// transcript, and AX launch mode are all minted or managed by the server,
	// and trusting them from the body would let a client be born "Closed"
	// (ended_at), keep an owned tmux session alive forever (adopted → closeOwned
	// never kills it), bind the mirror to an arbitrary transcript path without
	// an ownership check, or claim AX key
	// semantics for an ordinary external pane. The UI never sends these; this
	// closes the gap for any other client. ForkKind is recomputed in
	// resolveNode; AXScreenReader is set only at the owned-Claude launch seam.
	n.ID, n.SessionID, n.Transcript, n.CreatedAt, n.EndedAt, n.ForkKind, n.Adopted, n.AXScreenReader = "", "", "", "", "", "", false, false
	// Resolve and validate the launch configuration first: no request that
	// fails validation (empty prompt, unknown parent, bad agent or dir) ever
	// reaches createNode, and the transport decision reads the *resolved* agent
	// (finding 25).
	a.mu.Lock()
	status, err := a.resolveNode(&n)
	a.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	// Snapshot current session names before the critical section (tmux is
	// slow); the ID allocator must avoid foreign session names too.
	taken := map[string]bool{}
	for _, s := range a.server.Sessions() {
		taken[s] = true
	}
	// createNode manages a.mu itself: the id reservation happens under the
	// lock, the external launch outside it.
	status, initial, err := a.createNode(&n, taken)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	// Structured transports accepted their first turn inside createNode.
	// Claude pastes after SessionStart on a background goroutine and notes
	// usage there only if the paste happened.
	if n.Agent != "claude" && strings.TrimSpace(n.Prompt) != "" {
		a.noteUsagePrompt(n.Agent)
	}
	// Snapshot under the lock so SessionStart cannot mutate SessionID /
	// Transcript / delivery fields while this response is marshaled.
	a.mu.Lock()
	snap := n
	launchErr := a.claudeLaunchErr[n.ID]
	a.mu.Unlock()
	writeJSON(w, struct {
		*Node
		InitialDelivery initialDelivery `json:"initial_delivery,omitempty"`
		InitialError    string          `json:"initial_error,omitempty"`
	}{Node: &snap, InitialDelivery: initial, InitialError: launchErr})
	if snap.Agent == "claude" && (a.workers == nil || !a.workers.manages(snap.ID)) {
		a.startClaudeInitialDelivery(snap.ID)
	}
}

func (a *app) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		LaneID      *string `json:"lane_id"`
		// Station, when set, targets an OLD map station (its start-time key)
		// instead of the node: the edit is written as a per-station label
		// snapshot and the node record is left untouched, so renaming a closed
		// station never bleeds into the head or its siblings (spec E). The head
		// station has no key here — it is edited through the node fields above.
		Station *string `json:"station"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), 400)
		return
	}
	a.mu.Lock()
	n, ok := a.byID[id]
	if !ok {
		a.mu.Unlock()
		http.Error(w, "not found", 404)
		return
	}
	// Old-station edit: snapshot the label for that stop only, node untouched.
	if body.Station != nil {
		a.mu.Unlock()
		seam := strings.TrimSpace(*body.Station)
		if seam == "" {
			http.Error(w, "station must not be empty", 400)
			return
		}
		title, desc := "", ""
		if body.Title != nil {
			title = strings.TrimSpace(*body.Title)
		}
		if body.Description != nil {
			desc = strings.TrimSpace(*body.Description)
		}
		if title == "" {
			http.Error(w, "title must not be empty", 400)
			return
		}
		if a.sessionsDir == "" {
			http.Error(w, "no session store", 409)
			return
		}
		if err := a.appendSessionEvent(id, sessionlog.NewStation(seam, title, desc)); err != nil {
			http.Error(w, "persist station: "+err.Error(), 500)
			return
		}
		writeJSON(w, sessionlog.StationLabel{Title: title, Desc: desc})
		return
	}
	next := *n
	if body.Title != nil {
		title := strings.TrimSpace(*body.Title)
		if title == "" {
			a.mu.Unlock()
			http.Error(w, "title must not be empty", 400)
			return
		}
		next.Title = title
	}
	if body.Description != nil {
		next.Description = strings.TrimSpace(*body.Description)
	}
	if body.LaneID != nil {
		laneID := strings.TrimSpace(*body.LaneID)
		switch {
		case n.LaneID == "" && laneID != "":
			next.LaneID = laneID
		case n.LaneID != laneID:
			a.mu.Unlock()
			http.Error(w, "lane assignment is immutable", 409)
			return
		}
	}
	if next.Description == "" {
		next.Description = next.Prompt
	}
	if err := a.appendRecord(storeRecord{Type: "node", Node: &next}); err != nil {
		a.mu.Unlock()
		http.Error(w, "persist node: "+err.Error(), 500)
		return
	}
	*n = next
	a.mu.Unlock()
	writeJSON(w, next)
}

// handleExitNode implements /exit: the deliberate "ended" head state. Unlike
// delete, the node stays on the map (dead-end ⊣ cap, visible and rideable); unlike
// a mechanical process exit, this is a user decision recorded durably. Order
// mirrors delete — persist the intent (the ended node record) before tearing the
// process down, so a crash between the two never loses the closed marker; then
// stop the owned process via the same closeOwned path delete uses.
func (a *app) handleExitNode(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	a.mu.Lock()
	cur, ok := a.byID[n.ID]
	if !ok {
		a.mu.Unlock()
		http.Error(w, "not found", 404)
		return
	}
	firstExit := cur.EndedAt == ""
	if firstExit {
		next := *cur
		next.EndedAt = time.Now().UTC().Format(time.RFC3339)
		if err := a.appendRecord(storeRecord{Type: "node", Node: &next}); err != nil {
			a.mu.Unlock()
			http.Error(w, "persist node: "+err.Error(), 500)
			return
		}
		*cur = next
	}
	a.mu.Unlock()
	// The response reports whether the underlying process actually stopped so the
	// UI can tell the truth instead of promising a stop it may not have
	// delivered: a historical external tmux session is deliberately left running
	// (closeOwned returns nil without killing it), and a kill can fail. In both
	// cases the node is "closed in scimux" but the agent is still alive.
	stopped, reason := true, ""
	if firstExit {
		// Best-effort process teardown, once. The thread is already marked ended
		// in the durable store, so a failure to reach the agent must not un-end
		// it — the node stays visibly closed regardless.
		if err := a.closeOwned(n); err != nil {
			stopped, reason = false, "kill_failed"
			fmt.Fprintf(os.Stderr, "scimux: exit of %s marked ended but failed to close the agent: %v\n", n.ID, err)
		} else if a.proc(n) == nil && n.Adopted {
			stopped, reason = false, "adopted"
		}
		// /exit ends the auto-approval lease (process/session loss).
		if n.Agent == "claude" {
			a.endClaudePermissionTurn(n)
		} else {
			a.disarmAutoApprove(n.ID)
		}
	} else {
		// Idempotent re-exit (only reachable by a direct API call — the UI hides
		// the control once ended): do not tear the process down a second time.
		// Report the agent's current standing from mechanical liveness instead of
		// attempting a fresh kill that a first, already-successful exit made moot.
		a.mu.Lock()
		live := a.live[n.ID]
		a.mu.Unlock()
		switch {
		case a.proc(n) == nil && n.Adopted:
			stopped, reason = false, "adopted"
		case live == "active" || live == "quiet":
			stopped, reason = false, "running"
		}
	}
	writeJSON(w, map[string]any{"node": *n, "closed": true, "stopped": stopped, "reason": reason})
}

func (a *app) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	// Persist the delete before tearing anything down. The store is the
	// supervisor's durable truth; killing an owned agent first and only then
	// recording the delete risks a killed agent the store still replays as a
	// live node. Order is persist-intent → close → finalize, and the node is
	// re-asserted if the close fails so the durable record matches reality.
	a.mu.Lock()
	if _, ok := a.byID[n.ID]; !ok {
		a.mu.Unlock()
		http.Error(w, "not found", 404)
		return
	}
	if err := a.appendRecord(storeRecord{Type: "delete", ID: n.ID, Time: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		a.mu.Unlock()
		http.Error(w, "persist delete: "+err.Error(), 500)
		return
	}
	a.mu.Unlock()

	// Hold the auto-approve gate through lease clear, manager teardown, node
	// removal, and archival so a concurrent enable cannot install a new lease
	// (and a decision cannot deliver) during the delete window.
	if err := a.finalizeDeleteWithAutoBarrier(n); err != nil {
		// The agent outlived the delete record. Re-assert the node so replay
		// (and this process) keep showing it live: a later node record wins
		// over the delete, exactly as the concurrent-launch path relies on.
		a.mu.Lock()
		if _, ok := a.byID[n.ID]; ok {
			if rerr := a.appendRecord(storeRecord{Type: "node", Node: n}); rerr != nil {
				fmt.Fprintf(os.Stderr, "scimux: delete of %s failed to close the agent and failed to re-assert the node: %v\n", n.ID, rerr)
			}
		}
		a.mu.Unlock()
		http.Error(w, "close session: "+err.Error(), 500)
		return
	}
	writeJSON(w, map[string]string{"ok": "deleted"})
}

// finalizeDeleteWithAutoBarrier holds the per-node auto-approve gate from
// lease removal through manager kill, in-memory node drop, and session-log
// archival. Concurrent enable/decision paths wait on the same gate and cannot
// survive past a completed delete.
func (a *app) finalizeDeleteWithAutoBarrier(n *Node) error {
	g := a.autoGateFor(n.ID)
	g.Lock()
	defer g.Unlock()

	if n.Agent == "claude" {
		a.retractClaudeLease(a.claudePermBundle(n.ID))
	}
	a.mu.Lock()
	delete(a.autoApprove, n.ID)
	a.mu.Unlock()
	if n.Agent == "claude" {
		a.resetClaudePermissionTurnLocked(n)
	}

	if a.deleteGateHook != nil {
		a.deleteGateHook(n.ID)
	}

	if err := a.closeOwned(n); err != nil {
		return err
	}
	hookID := a.claudeHookID(n.ID)
	a.mu.Lock()
	a.removeNodeLocked(n.ID)
	a.mu.Unlock()
	a.archiveHookBundle(hookID)
	a.archiveSessionLog(n.ID)
	a.archiveAttachments(n.ID)
	a.archiveAssets(n.ID)
	return nil
}
