package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp"
	"codeberg.org/chrberger/scimux/internal/tmuxsession"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// ---------- node lifecycle ----------

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// newUUID mints a v4 UUID used as a Claude session id. randSource is a seam so
// TestNewUUID can force the RNG-failure path. A silent zero/partial UUID on RNG
// failure would be exactly the kind of identity bug the rest of the code avoids
// (cf. sessionlog.NewMeta), so a read error is a hard failure, not ignored.
var randSource io.Reader = rand.Reader

func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(randSource, b); err != nil {
		return "", fmt.Errorf("uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// canonicalPrompt is the single equality helper for sent-prompt versus
// transcript-turn confirmation. Only line endings are normalized.
func canonicalPrompt(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}

// slugStrip collapses any run of characters not allowed in a tmux session name
// into a single '-'. '.' is excluded from the allow-set on purpose: it is tmux's
// window.pane target separator, so a slug containing it yields an unaddressable
// session (see tmuxsession.nameRe). dashRun then squeezes the '-' runs this can
// leave (e.g. from " - ") so names stay tidy.
var slugStrip = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
var dashRun = regexp.MustCompile(`-{2,}`)

// uniqueID allocates a slug that collides neither with registered nodes nor
// with any name in taken — the current tmux inventory, so a foreign session
// with the same slug cannot make new-session fail. A session log left on disk
// under the slug also counts as taken: it means a dead node's archive rename
// failed, and reissuing the slug would append new history onto dead history.
func (a *app) uniqueID(title string, taken map[string]bool) string {
	slug := strings.Trim(dashRun.ReplaceAllString(slugStrip.ReplaceAllString(title, "-"), "-"), "-")
	if slug == "" || !tmuxsession.ValidName(slug) {
		slug = "chat"
	}
	if len(slug) > 40 {
		slug = slug[:40]
	}
	id := slug
	for i := 2; ; i++ {
		if _, used := a.byID[id]; !used && !taken[id] && !a.reserved[id] && !a.sessionLogExists(id) {
			return id
		}
		id = fmt.Sprintf("%s-%d", slug, i)
	}
}

// agentCommandClaude builds the owned-Claude argv. --settings sits before
// --remote-control so an empty title cannot swallow the path. Tests that do
// not pass a settings file keep the historical flag order.
func agentCommandClaude(n *Node, addDirs []string, settingsPath string) string {
	// --ax-screen-reader is a hard default for every scimux-owned Claude
	// launch: stable flat-text menus for supervision. Exactly one flag;
	// placed before --remote-control so an empty title cannot treat the
	// flag as that option's value. Pair with n.AXScreenReader=true at the
	// launchNode seam so the stored marker matches the launched argv.
	//
	// It is not only a rendering preference, so do not make it one. The
	// Anthropic Consumer Terms bar accessing the Services "through automated
	// or non-human means, whether through a bot, script, or otherwise",
	// except via an API key "or where we otherwise explicitly permit it". A
	// first-party flag whose entire purpose is to make the pane legible to an
	// external reader is that explicit permission, and because this is the
	// mode every supervised pane actually runs in, it is a stronger answer
	// than pointing at hooks or headless mode. Demoting it to an option would
	// quietly move scimux's Claude integration off the permitted footing for
	// whoever turned it off.
	parts := []string{"claude", "--session-id", n.SessionID, "--ax-screen-reader"}
	if settingsPath != "" {
		parts = append(parts, "--settings", shellQuote(settingsPath))
	}
	parts = append(parts, "--remote-control")
	if n.Title != "" {
		parts = append(parts, shellQuote(n.Title))
	}
	if n.Model != "" {
		parts = append(parts, "--model", shellQuote(n.Model))
	}
	if n.Effort != "" {
		parts = append(parts, "--effort", shellQuote(n.Effort))
	}
	for _, dir := range addDirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		parts = append(parts, "--add-dir", shellQuote(dir))
	}
	return strings.Join(parts, " ")
}

// agentCommand builds the launch command. Structured transports keep their
// established first-prompt paths. Owned Claude is the exception: SessionStart
// from this launch is the readiness acknowledgement, so its prompt is omitted
// here and delivered by deliverClaudeInitialPrompt after that hook event binds
// the transcript.
func agentCommand(n *Node, addDirs []string) (string, error) {
	return agentCommandSettings(n, addDirs, "")
}

func agentCommandSettings(n *Node, addDirs []string, settingsPath string) (string, error) {
	switch n.Agent {
	case "claude":
		return agentCommandClaude(n, addDirs, settingsPath), nil
	// pi, opencode, and grok reach agentCommand only as a legacy/forced tmux
	// fallback (new nodes resolve to the ACP transport). pi/opencode take the
	// "provider/model" form their list commands emit; grok takes -m and
	// --reasoning-effort as on its ACP launch line.
	case "pi":
		parts := []string{"pi"}
		if n.Model != "" {
			parts = append(parts, "--model", shellQuote(n.Model))
		}
		return strings.Join(append(parts, shellQuote(n.Prompt)), " "), nil
	case "opencode":
		parts := []string{"opencode"}
		if n.Model != "" {
			parts = append(parts, "--model", shellQuote(n.Model))
		}
		return strings.Join(append(parts, "--prompt", shellQuote(n.Prompt)), " "), nil
	case "grok":
		parts := []string{"grok"}
		if n.Model != "" {
			parts = append(parts, "-m", shellQuote(n.Model))
		}
		if n.Effort != "" {
			parts = append(parts, "--reasoning-effort", shellQuote(n.Effort))
		}
		return strings.Join(append(parts, shellQuote(n.Prompt)), " "), nil
	}
	// dsh is deliberately absent: `dsh --profile acp` is the only launch line
	// scimux knows, and it speaks ACP on stdio rather than to a terminal. A
	// bare `dsh <prompt>` is a different program with a different profile, so
	// a tmux fallback here would launch something the node did not ask for;
	// cursor and muse are likewise structured-only and have no tmux fallback.
	return "", fmt.Errorf("unknown agent %q (want claude, pi, opencode, or grok)", n.Agent)
}

// launchFailSentinel is printed on the pane by wrapLaunch when the launched
// process exits non-zero, and is what awaitLaunch greps for. It doubles as a
// human-legible line for a supervisor peeking at the pane.
const launchFailSentinel = "SCIMUX: agent process exited before its interface started"

// launchHoldSeconds is how long the wrapper keeps a failed launch's pane open so
// awaitLaunch (and a slower poll, or a human peek) can read the error. awaitLaunch
// kills the session as soon as it detects the sentinel, cutting this short; the
// hold only matters if that detection is missed, after which the pane self-cleans.
const launchHoldSeconds = 30

// wrapLaunch wraps a tmux launch command so that a process which exits before
// its interface is ready leaves its error on the pane instead of the session
// vanishing into an unexplained dead node. The original command runs verbatim
// first, and the
// sentinel is emitted only on a non-zero exit, so a clean quit falls through
// untouched (pane closes, session dies, exactly as before). tmux runs this
// whole string via `sh -c`.
func wrapLaunch(cmd string) string {
	return fmt.Sprintf(`%s; __ec=$?; if [ "$__ec" != 0 ]; then printf '\n%s (status %%d)\n' "$__ec"; sleep %d; fi`,
		cmd, launchFailSentinel, launchHoldSeconds)
}

// awaitLaunch watches a freshly-launched tmux session for an immediate failure.
// It polls the pane over a.launchGrace; if wrapLaunch's sentinel appears the
// launch failed, and the agent's own error (the pane text above the sentinel)
// is returned as the reason. A launch that stays up past the window is taken to
// be healthy. Zero grace disables the check (returns not-failed at once).
func (a *app) awaitLaunch(name string) (reason string, failed bool) {
	if a.launchGrace <= 0 {
		return "", false
	}
	poll := a.launchPoll
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	deadline := time.Now().Add(a.launchGrace)
	for {
		out, err := a.server.Session(name).CaptureVisible()
		if err == nil {
			if i := strings.Index(out, launchFailSentinel); i >= 0 {
				return launchFailReason(out[:i]), true
			}
		}
		if time.Now().After(deadline) {
			return "", false
		}
		time.Sleep(poll)
	}
}

// launchFailReason distills the pane text above the sentinel into a one-line
// reason: the last few non-empty lines (where a CLI prints its error), joined.
func launchFailReason(paneAbove string) string {
	var lines []string
	for _, ln := range strings.Split(paneAbove, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			lines = append(lines, ln)
		}
	}
	if len(lines) == 0 {
		return "the process exited immediately (no output); check the agent and --model options"
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return strings.Join(lines, " / ")
}

// resolveClaudeModel maps a family alias to the concrete id the CLI accepts. It
// returns "" when there is no mapping — the probe is absent or failed, or the
// value is not a known family (e.g. it is already a concrete id) — leaving the
// caller to pass the value through unchanged.
func (a *app) resolveClaudeModel(model string) string {
	a.claudeMu.Lock()
	defer a.claudeMu.Unlock()
	return a.claudeIDs[model]
}

func (a *app) setClaudeIDs(ids map[string]string) {
	a.claudeMu.Lock()
	a.claudeIDs = ids
	a.claudeMu.Unlock()
}

// refreshClaudeModels populates the family->id map, preferring a stored answer
// over a fresh probe. The order is deliberate: read the cache, then ask the CLI
// its version (a local subprocess that spends nothing), and only when those two
// disagree spend the throwaway sessions. Best-effort throughout — a probe that
// answers nothing leaves any stale ids in place and writes nothing, so the
// retry is simply the next trigger.
func (a *app) refreshClaudeModels(ctx context.Context) {
	cache := readClaudeCache(a.claudeCachePath)
	serveCache := func() {
		if len(cache.IDs) > 0 {
			a.setClaudeIDs(cache.IDs)
		}
	}
	// No resolver means this app was not built to probe (every test, and any
	// caller that is not the serve path). The cache is still worth serving.
	if a.claudeResolveModels == nil {
		serveCache()
		return
	}
	version := ""
	if a.claudeVersion != nil {
		version = a.claudeVersion(ctx)
	}
	if claudeCacheUsable(cache, version, time.Now()) {
		a.setClaudeIDs(cache.IDs)
		return
	}
	ids := a.claudeResolveModels(ctx)
	if len(ids) == 0 {
		serveCache()
		return
	}
	a.setClaudeIDs(ids)
	if err := writeClaudeCache(a.claudeCachePath, version, ids); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: cache claude models: %v\n", err)
	}
}

// claudeModelRefreshTimeout bounds one whole refresh: up to four candidate
// probes and one picker capture, each a session start rather than a round trip.
const claudeModelRefreshTimeout = 120 * time.Second

// ensureClaudeModels asks for a refresh in the background and returns at once.
// It is what the triggers call — startup, the new-activity dialog, the burger
// menu's update check — because a long-lived scimux must notice a claude it did
// not install itself. Overlapping triggers collapse into one run: the work is
// idempotent, but four throwaway sessions are not something to do twice.
func (a *app) ensureClaudeModels() {
	if a == nil || a.claudeResolveModels == nil {
		return
	}
	if !a.claudeRefreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer a.claudeRefreshing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), claudeModelRefreshTimeout)
		defer cancel()
		a.refreshClaudeModels(ctx)
	}()
}

// resolveNode validates a new-node request and resolves its launch
// configuration in place: parent inheritance (fresh-context fork: the launch
// config, never the conversation), agent and directory defaults, and the
// title. It is idempotent, so handleNewNode runs it once up front and
// createNode runs the very same step instead of a diverging preflight mirror
// (finding 25). Callers hold a.mu. Returns the HTTP status to use on error:
// every resolution failure is a client mistake, 400.
func (a *app) resolveNode(n *Node) (int, error) {
	n.Title = strings.TrimSpace(n.Title)
	n.Prompt = strings.TrimSpace(n.Prompt)
	n.Description = strings.TrimSpace(n.Description)
	n.LaneID = strings.TrimSpace(n.LaneID)
	if n.Title == "" {
		return 400, fmt.Errorf("title must not be empty")
	}
	if n.Prompt == "" {
		if n.Description != "" {
			n.Prompt = n.Description
		} else {
			n.Prompt = n.Title
		}
	}
	if n.Description == "" {
		n.Description = n.Prompt
	}
	if n.Parent != "" {
		p, ok := a.byID[n.Parent]
		if !ok {
			return 400, fmt.Errorf("parent %q not found", n.Parent)
		}
		// Fresh-context fork: inherit the launch config, never the conversation.
		if n.Agent == "" {
			n.Agent = p.Agent
		}
		// Model, effort, and transport are meaningful only for the parent's
		// agent: a fork that switches agents must fall through to that agent's
		// own defaults instead of dragging an incompatible model name (or the
		// parent's tmux transport) across the switch.
		if n.Agent == p.Agent {
			if n.Model == "" {
				n.Model = p.Model
			}
			if n.Effort == "" {
				n.Effort = p.Effort
			}
			// Same mechanism as the parent. Use the migrated value, not the
			// raw field: an old or adopted pi/opencode parent with an empty
			// Transport means tmux, so the child must resolve to tmux too
			// rather than falling through to the agent-derived ACP default
			// below (finding 54). Derivation only fires when neither the
			// request nor a same-agent parent pinned a transport.
			if n.Transport == "" {
				n.Transport = p.transport()
			}
		}
		if n.Dir == "" {
			n.Dir = p.Dir
		}
		if n.LaneID == "" {
			n.LaneID = p.LaneID
		}
		// Classify the fork now, against the destination lane's current
		// membership, so the map never re-derives it from mutable sibling
		// existence. y-stay: same lane as the parent (a parallel same-colour
		// thread). s: the destination lane already holds a station (this fork
		// crosses onto an existing line). y-new: a lane born at this split.
		// Every existing node in the lane is older than this brand-new one, so
		// "any prior member" is exactly the historical "older station existed".
		switch {
		case n.LaneID == p.LaneID:
			n.ForkKind = "y-stay"
		default:
			n.ForkKind = "y-new"
			for _, o := range a.nodes {
				if o.ID != n.ID && o.LaneID == n.LaneID {
					n.ForkKind = "s"
					break
				}
			}
		}
	}
	if n.Agent == "" {
		n.Agent = "claude"
	}
	switch n.Agent {
	case "claude", "codex", "pi", "opencode", "grok", "cursor", "dsh", "muse":
	default:
		return 400, fmt.Errorf("unknown agent %q (want claude, codex, pi, opencode, grok, cursor, dsh, or muse)", n.Agent)
	}
	// A client cannot pin the Muse transport onto a different agent, and a
	// Muse node cannot run on any other transport. Stored records are not
	// rewritten here: Node.transport still treats an absent field as tmux.
	if n.Agent != "muse" && n.Transport == "muse" {
		n.Transport = ""
	}
	// New nodes pick a transport by agent: pi/opencode/grok/cursor/dsh over ACP,
	// codex over its app-server bridge, muse over MSP, claude over tmux. Only
	// set this on creation — stored records with an absent Transport are
	// migrated to tmux by Node.transport, never rewritten here.
	if n.Transport == "" {
		switch n.Agent {
		case "pi", "opencode", "grok", "cursor", "dsh":
			n.Transport = "acp"
		case "codex":
			n.Transport = "codex"
		case "muse":
			n.Transport = "muse"
		default:
			n.Transport = "tmux"
		}
	}
	if n.Agent == "muse" {
		n.Transport = "muse"
	}
	if n.Dir == "" {
		n.Dir = a.home
	}
	abs, err := filepath.Abs(n.Dir)
	if err != nil {
		return 400, err
	}
	n.Dir = abs
	if st, err := os.Stat(n.Dir); err != nil || !st.IsDir() {
		return 400, fmt.Errorf("dir %q is not an existing directory", n.Dir)
	}
	return 0, nil
}

type initialDelivery string

const (
	initialAcknowledged initialDelivery = "acknowledged"
	initialNotSent      initialDelivery = "not_sent"
	initialUnconfirmed  initialDelivery = "unconfirmed"
	initialPending      initialDelivery = "pending"

	sendSubmitting         = "submitting"
	sendUnconfirmed        = "unconfirmed"
	sendInitialUnconfirmed = "initial_unconfirmed"
	// sendDelivering is how sendInitialUnconfirmed is projected to the browser:
	// a first prompt pasted into a healthy launch whose transcript has not
	// caught up. It is deliberately not sendUnconfirmed — see tmuxChatInto.
	sendDelivering = "delivering"
)

// createNode owns new-node publication ordering:
//
//  1. resolveNode + reserve the id under a.mu (invisible to concurrent creates);
//  2. launch with the global app lock released so a long structured negotiation
//     cannot stall /api/state (R18.5);
//  3. re-check for an id collision under the lock before publishing;
//  4. persist store records, then publish in-memory maps — never the reverse;
//  5. deliver the initial prompt (structured Send, or Claude's deferred
//     SessionStart paste path);
//  6. on any failure after reservation, roll back ownership: drop the
//     reservation, archive a pending Claude hook bundle, kill the process,
//     and append compensating store records as needed.
//
// Returns HTTP status for the caller: client mistakes 400, server-side
// failures 500, collision 409. taken carries tmux session names that must
// not be reused as node IDs.
func (a *app) createNode(n *Node, taken map[string]bool) (int, initialDelivery, error) {
	a.mu.Lock()
	if status, err := a.resolveNode(n); err != nil {
		a.mu.Unlock()
		return status, "", err
	}
	n.ID = a.uniqueID(n.Title, taken)
	if a.reserved == nil { // tests build app literals without the map
		a.reserved = map[string]bool{}
	}
	a.reserved[n.ID] = true
	if n.Agent == "claude" {
		sid, err := newUUID()
		if err != nil {
			delete(a.reserved, n.ID)
			a.mu.Unlock()
			return 500, "", fmt.Errorf("allocate session id: %w", err)
		}
		n.SessionID = sid
	}
	n.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	pm := a.proc(n)
	if pm == nil && a.workers != nil && n.Agent == "claude" && !n.Adopted {
		pm = a.workers
	}
	a.mu.Unlock()

	status, err := a.launchNode(n, pm)

	// Publish (or abandon) under the lock; either way the reservation ends.
	// Re-check the id for a collision at publish time: the launch ran with
	// a.mu released, and a double-publish would leave two *Node values under
	// one id with the store replaying both (R20.2). Every id-minting path now
	// consults a.reserved through uniqueID, so a hit here should be
	// unreachable — but the cost of missing one is silent registry and store
	// corruption, so the launch is abandoned and rolled back instead.
	a.mu.Lock()
	delete(a.reserved, n.ID)
	collided := false
	var winner *Node
	if err == nil {
		if w, dup := a.byID[n.ID]; dup {
			collided = true
			winner = w // the node that reached the slug first; preserve its record
		} else {
			a.nodes = append(a.nodes, n)
			a.byID[n.ID] = n
		}
	}
	a.mu.Unlock()
	if err != nil {
		if hookID := a.takePendingClaudeHook(n.ID); hookID != "" {
			a.archiveHookBundle(hookID)
		}
		return status, "", err
	}
	if collided {
		if hookID := a.takePendingClaudeHook(n.ID); hookID != "" {
			a.archiveHookBundle(hookID)
		}
		fmt.Fprintf(os.Stderr, "scimux: node id %s was claimed concurrently during launch; abandoning the new launch\n", n.ID)
		if pm != nil {
			_ = pm.Kill(n.ID)
		} else if s := a.server.Session(n.ID); s.Alive() {
			_ = s.Kill()
		}
		// Negate the just-persisted node record, then re-assert the winner's.
		// Append-only store: the delete record stops this launch's dead config
		// from clobbering the winner at replay (both share the id, and the later
		// node record wins) — but that same delete would also erase the winner's
		// earlier record. Re-appending the winner's node record (its *Node is in
		// hand, captured under the collision lock) makes replay converge on the
		// live node with no manual repair (R21.4). The winner owns
		// sessions/<id>.jsonl, so its log is left intact — never archived here.
		_ = a.appendRecord(storeRecord{Type: "delete", ID: n.ID, Time: time.Now().UTC().Format(time.RFC3339)})
		if winner != nil {
			_ = a.appendRecord(storeRecord{Type: "node", Node: winner})
		}
		return 409, "", fmt.Errorf("node id %q was claimed concurrently; launch abandoned", n.ID)
	}

	hookID := a.takePendingClaudeHook(n.ID)
	hookGeneration := 1
	if hookID == "" && n.Agent == "claude" && a.workers != nil && pm == a.workers {
		// The Claude worker, not the muxer, created this capability. Project its
		// identity into the global append-only registry only after Launch
		// returned successfully; the worker locator remains the crash-window
		// liveness proof used by cleanupOrphanClaudeHooks.
		state := a.workers.State(n.ID)
		hookID = state.HookID
		if state.HookGeneration > 0 {
			hookGeneration = state.HookGeneration
		}
	}
	if hookID != "" {
		if err := a.appendRecord(storeRecord{Type: "claude-hook", ID: n.ID, HookID: hookID, Generation: hookGeneration}); err != nil {
			if pm != nil {
				_ = pm.Kill(n.ID)
			} else if s := a.server.Session(n.ID); s.Alive() {
				_ = s.Kill()
			}
			a.mu.Lock()
			a.removeNodeLocked(n.ID)
			a.mu.Unlock()
			_ = a.appendRecord(storeRecord{Type: "delete", ID: n.ID, Time: time.Now().UTC().Format(time.RFC3339)})
			a.archiveHookBundle(hookID)
			return 500, "", fmt.Errorf("persist claude hook (session rolled back): %v", err)
		}
		a.mu.Lock()
		a.claudeHooks[n.ID] = hookID
		a.claudeGens[n.ID] = hookGeneration
		a.mu.Unlock()
	}

	// Deliver the research question as the first turn of a structured node. A
	// structured Send can fail before anything is recorded — e.g. the subprocess
	// died during launch, or the user-turn append failed. Persist that failure to
	// the node's own history so the chat view shows it instead of a silent, empty
	// successful node (finding 52). Owned Claude is handled separately below.
	if pm != nil && !(n.Agent == "claude" && a.workers != nil && pm == a.workers) {
		if err := pm.Send(n.ID, n.Prompt); err != nil {
			fmt.Fprintf(os.Stderr, "scimux: first prompt to %s node %s failed: %v\n", n.transport(), n.ID, err)
			// If the failure record also cannot be written (the session log is
			// the failing component — commonly the same disk problem that broke
			// Send), there is no durable trace of the lost first prompt. The
			// node is persisted and must stay, but the caller must not see a
			// clean success: report it so the operator retries the prompt
			// (finding 59).
			if rerr := pm.RecordStartFailure(n.ID, err); rerr != nil {
				return 500, "", fmt.Errorf("node created but first prompt %q and its failure record were not durable (retry the prompt): %v", err, rerr)
			}
		}
	}
	if n.Agent == "claude" && a.deliverClaudeInitial != nil {
		if a.workers != nil && pm == a.workers {
			return 0, initialPending, nil
		}
		// Return the node immediately so the browser can select it and paint
		// the pale launch bubble. SessionStart, the single paste, and
		// transcript confirmation run after the HTTP snapshot is marshaled.
		// The send gate stays held so no later prompt can overtake the first.
		a.mu.Lock()
		a.sendState[n.ID] = sendSubmitting
		a.mu.Unlock()
		return 0, initialPending, nil
	}
	return 0, "", nil
}

func (a *app) startClaudeInitialDelivery(id string) {
	if a.deliverClaudeInitial == nil || id == "" {
		return
	}
	a.mu.Lock()
	n := a.byID[id]
	pending := n != nil && n.Agent == "claude" && a.sendState[id] == sendSubmitting
	a.mu.Unlock()
	if pending {
		go a.runClaudeInitialDelivery(n)
	}
}

// runClaudeInitialDelivery waits for SessionStart, pastes once, and confirms
// against the transcript. Failures are inline (draft restore, no retry, no
// terminal). The HTTP create path has already returned.
func (a *app) runClaudeInitialDelivery(n *Node) {
	if n == nil || a.deliverClaudeInitial == nil {
		return
	}
	delivery := a.deliverClaudeInitial(n)
	a.mu.Lock()
	if a.byID[n.ID] == nil {
		delete(a.sendState, n.ID)
		a.mu.Unlock()
		return
	}
	switch delivery {
	case initialUnconfirmed, initialPending:
		a.sendState[n.ID] = sendInitialUnconfirmed
	default:
		delete(a.sendState, n.ID)
	}
	prompt, agent := n.Prompt, n.Agent
	a.mu.Unlock()
	if delivery != initialNotSent && strings.TrimSpace(prompt) != "" {
		a.noteUsagePrompt(agent)
	}
}

// deliverClaudeInitialPrompt waits for a valid SessionStart from this exact
// launched process, then submits the already-durable Node.Prompt through tmux.
// SessionStart is the readiness and hook-health acknowledgement; the
// undocumented transcript bridge_status record is not consulted. Pane changes
// are intentionally ignored: only a matching new transcript user turn proves
// acceptance. Failure is an inline launch/delivery error — the terminal is
// never opened and the prompt is never retried.
func (a *app) deliverClaudeInitialPrompt(n *Node) initialDelivery {
	poll := a.claudeInitialPoll
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	started := time.Now()
	readyDeadline := started.Add(a.claudeReadyTimeout)
	trustDeadline := started.Add(a.claudeDeliveryGiveUp)
	trustExtended := false
	for {
		a.drainClaudeHooks()
		if a.claudeSessionStartReady(n) {
			break
		}
		now := time.Now()
		if a.claudeReadyTimeout <= 0 || now.After(readyDeadline) {
			if !trustExtended && trustDeadline.After(readyDeadline) &&
				now.Before(trustDeadline) && a.claudeWorkspaceTrustVisible(n) {
				// The exact trust prompt is actionable from the web. Keep the
				// launch gate alive while the user decides; unknown startup
				// dialogs retain the ordinary short timeout.
				readyDeadline = trustDeadline
				trustExtended = true
				continue
			}
			a.recordClaudeLaunchError(n.ID, a.diagnoseClaudeStartFailure(n))
			return initialNotSent
		}
		time.Sleep(poll)
	}

	// The transcript may not be bound yet: the CLI creates the file lazily, so
	// an idle launch has none and this very paste is what brings it into being.
	// The link then arrives from the same still-pending inbox event, and the
	// confirmation loop below picks the tailer up when it does. A path already
	// in hand still gets its watermark, so a pre-existing transcript cannot
	// confirm the first prompt with one of its old user turns.
	a.mu.Lock()
	path := n.Transcript
	a.mu.Unlock()
	var tl *transcript.Tailer
	before := 0
	if path != "" {
		tl = &transcript.Tailer{Path: path}
		before = len(tl.Poll())
	}
	pasted := time.Now()
	g := a.autoGateFor(n.ID)
	g.Lock()
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		g.Unlock()
		a.recordClaudeLaunchError(n.ID, claudeTurnFenceExplain)
		return initialNotSent
	}
	if err := a.server.Session(n.ID).Send(n.Prompt); err != nil {
		a.abortClaudeAcceptedTurn(n.ID, turn.Turn)
		g.Unlock()
		fmt.Fprintf(os.Stderr, "scimux: deferred first prompt to Claude node %s failed: %v\n", n.ID, err)
		a.recordClaudeLaunchError(n.ID, claudePasteExplain)
		return initialNotSent
	}
	// A toggle enabled while startup was pending is primed. The accepted first
	// turn arms it only after Enter succeeded, under the same gate as the turn
	// marker publication.
	a.mu.Lock()
	primedLeaseID := ""
	if st := a.autoApprove[n.ID]; st != nil && st.Phase == autoPhasePrimed {
		primedLeaseID = st.LeaseID
	}
	a.mu.Unlock()
	if primedLeaseID != "" {
		a.armAutoApproveOnPromptLocked(n.ID, primedLeaseID, "", 0, false)
		a.syncClaudeLeaseMarker(n)
	}
	g.Unlock()
	// The first prompt reached the pane. Record when confirmation began so the
	// reconciler can eventually surface an unconfirmed delivery. Paste happens
	// exactly once; confirmation failure never retries it.
	a.noteDelivery(n.ID, pasted)
	want := canonicalPrompt(n.Prompt)
	deliveryDeadline := time.Now().Add(a.claudeDeliveryTimeout)
	for {
		if tl == nil {
			// The paste created the transcript, and the SessionStart that named
			// it is still in the inbox waiting for exactly that file. Drain here
			// rather than leaning on the 2 s poll, so confirmation is as prompt
			// as it is when the file already existed. A file first seen after
			// the paste needs no watermark: it holds this launch only.
			a.drainClaudeHooks()
			a.mu.Lock()
			path = n.Transcript
			a.mu.Unlock()
			if path != "" {
				tl = &transcript.Tailer{Path: path}
			}
		}
		if tl != nil {
			turns := tl.Poll()
			for _, turn := range turns[before:] {
				if turn.Role == "user" && canonicalPrompt(turn.Text) == want {
					a.clearClaudeLaunchError(n.ID)
					return initialAcknowledged
				}
			}
		}
		if a.claudeDeliveryTimeout <= 0 || time.Now().After(deliveryDeadline) {
			// Not a failure. SessionStart already proved this launch healthy,
			// and the CLI writes the transcript lazily, so silence here says
			// only that Claude has not logged the prompt yet. Hand the wait to
			// the poller: it releases the gate when the mirror catches up, and
			// raises the inline error at claudeDeliveryGiveUp. Either way the
			// prompt is never pasted a second time.
			return initialUnconfirmed
		}
		time.Sleep(poll)
	}
}

// launchNode starts the tmux session or structured-protocol subprocess (ACP
// for pi/opencode/grok/cursor/dsh, codex app-server for codex, MSP for Muse) and persists
// the node record.
// Runs without a.mu. Persist follows launch: the session/process had to exist
// first, so a store failure rolls it back (kill) — otherwise a session would
// run supervised-in-memory but vanish from the registry on restart. The
// caller publishes the node in memory only after this succeeds.
func (a *app) launchNode(n *Node, pm procManager) (int, error) {
	launch := n
	// Resolve into a launch-only copy: the durable node keeps what the user
	// chose while every launch path receives the concrete id. Claude's choice
	// is a family alias; muse's is often nothing at all, meaning "whatever is
	// current". Writing the resolution back would turn either into a pin on the
	// id that happened to be current once, and a pinned id that later leaves
	// the catalog fails every relaunch and fork of that node -- where the
	// unpinned value would simply resolve again.
	if n != nil && (n.Agent == "muse" || n.transport() == "muse") {
		if !a.settings().MuseApprovalJudgeConsent {
			return 400, errMuseConsentRequired
		}
		id, err := a.resolveMuseLaunchModel(n.Model)
		if err != nil {
			return 400, err
		}
		cp := *n
		cp.Model = id
		launch = &cp
	}
	if n.Agent == "claude" {
		if id := a.resolveClaudeModel(n.Model); id != "" {
			cp := *n
			cp.Model = id
			launch = &cp
		}
	}
	if pm != nil {
		var sid string
		var err error
		if workers, ok := pm.(*workerManager); ok {
			sid, err = workers.LaunchNode(launch, launch.Model)
		} else {
			// launch.Model, not n.Model: this is the branch the comment above
			// calls the legacy path, and it must not be the one place that
			// launches an unresolved id -- for muse that would skip the tier
			// gate resolveMuseLaunchModel just applied.
			sid, err = pm.Launch(n.ID, n.Agent, n.Dir, launch.Model, n.Effort)
		}
		if err != nil {
			// A cursor pair the catalog does not offer is a launch-config
			// error, like the muse gate above: the user can fix it in the
			// dialog, so it must not arrive as a server fault.
			var badModel cursorModelErr
			if errors.As(err, &badModel) {
				return 400, err
			}
			// A configuration the agent refused (an unknown model, a thought
			// level that model does not have) is the user's choice being
			// wrong, not the server failing. It reaches here as text once it
			// has crossed the session-worker boundary, which is why the
			// classifier matches on the sentinel's message too.
			if acp.IsConfigRejection(err) {
				return 400, err
			}
			return 500, err
		}
		n.SessionID = sid
		if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
			if kerr := pm.Kill(n.ID); kerr != nil {
				fmt.Fprintf(os.Stderr, "scimux: rollback of %s session %s failed: %v\n", n.transport(), n.ID, kerr)
			}
			// The launcher already wrote the session log's meta header; without
			// the node record it is dead history that would burn the slug
			// forever (R20.3) — archive it with the rollback.
			a.archiveSessionLog(n.ID)
			return 500, fmt.Errorf("persist node (%s session rolled back): %v", n.transport(), err)
		}
		return 0, nil
	}
	// Extra directories for Claude --add-dir. Only genuinely additional
	// directories outside the working directory — today the node's attachment
	// staging dir. n.Dir is already the process cwd (passing it is a no-op
	// and does not bypass workspace trust). Skip when attachmentsDir is unset
	// so an empty or relative path never reaches the CLI. MkdirAll here
	// because the CLI rejects a non-existent path and storeAttachment
	// otherwise creates the dir lazily on first upload. A persistent
	// permissions.additionalDirectories setting is not required.
	var addDirs []string
	if n.Agent == "claude" && a.attachmentsDir != "" {
		dir := a.attachmentDir(n.ID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return 500, fmt.Errorf("create attachment dir: %w", err)
		}
		addDirs = []string{dir}
	}
	var hookID, settingsPath string
	if n.Agent == "claude" {
		var herr error
		hookID, settingsPath, herr = a.prepareClaudeHookBundle(n.ID)
		if herr != nil {
			return 500, fmt.Errorf("prepare claude hook: %w", herr)
		}
		// Writing the bundle proves its layout, not that Claude loaded or ran it.
		// Capability becomes authoritative only after this live process delivers a
		// valid SessionStart event (processClaudeHookEventAt).
	}
	cmd, err := agentCommandSettings(launch, addDirs, settingsPath)
	if err != nil {
		a.archiveHookBundle(hookID)
		return 400, err
	}
	if _, err := a.server.NewSession(n.ID, n.Dir, wrapLaunch(cmd)); err != nil {
		a.archiveHookBundle(hookID)
		return 500, err
	}
	// Catch a launch that dies before its interface is ready (a rejected --model,
	// a bad flag): surface the agent's own error to the caller rather than
	// persisting a node whose session has already vanished. Nothing is stored
	// yet, so failing here leaves no phantom node — only the held-open session to
	// reap. AXScreenReader is set only after this gate so a failed launch never
	// publishes or persists a misleading AX-marked node.
	if reason, bad := a.awaitLaunch(n.ID); bad {
		if kerr := a.server.Session(n.ID).Kill(); kerr != nil {
			fmt.Fprintf(os.Stderr, "scimux: reaping failed launch %s: %v\n", n.ID, kerr)
		}
		a.archiveHookBundle(hookID)
		return 502, fmt.Errorf("agent exited on launch: %s", reason)
	}
	// Owned Claude was launched with --ax-screen-reader (agentCommand). Record
	// that fact on the same Node that is about to be persisted and published.
	// Never set for historical external records, structured transports, or non-Claude tmux.
	if n.Agent == "claude" {
		n.AXScreenReader = true
	}
	if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		if kerr := a.server.Session(n.ID).Kill(); kerr != nil {
			fmt.Fprintf(os.Stderr, "scimux: rollback of session %s failed: %v\n", n.ID, kerr)
		}
		a.archiveHookBundle(hookID)
		return 500, fmt.Errorf("persist node (session rolled back): %v", err)
	}
	if hookID != "" {
		a.setPendingClaudeHook(n.ID, hookID)
	}
	return 0, nil
}

// firstWords truncates a string to its first n whitespace-separated words.
// Test seam: its production caller was removed, but node_lifecycle_test.go
// still exercises it. It is not dead code — do not delete it with the rest.
func firstWords(s string, n int) string {
	words := strings.Fields(s)
	if len(words) > n {
		words = words[:n]
	}
	return strings.Join(words, " ")
}

// paneSessionID resolves the claude session id from the pane's process tree,
// through the test seam when one is installed.
func (a *app) paneSessionID(pid string) string {
	if a.paneSession != nil {
		return a.paneSession(pid)
	}
	return sessionFromPane(pid, sessionArgFromCmdline)
}

// sessionFromPane inspects the pane's process and its direct children (tmux
// may wrap the command in `sh -c`), applying extract to each command line to
// find an agent session id. Linux /proc only; returns "" anywhere it can't
// look.
func sessionFromPane(panePID string, extract func([]string) string) string {
	if id := extract(procCmdline(panePID)); id != "" {
		return id
	}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid := e.Name()
		if pid[0] < '0' || pid[0] > '9' {
			continue
		}
		if ppidOf(pid) == panePID {
			if id := extract(procCmdline(pid)); id != "" {
				return id
			}
		}
	}
	return ""
}

func procCmdline(pid string) []string {
	b, err := os.ReadFile("/proc/" + pid + "/cmdline")
	if err != nil || len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
}

func ppidOf(pid string) string {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return ""
	}
	// PPid is the 2nd field after the parenthesized comm (which may itself
	// contain spaces), so split after the last ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return ""
	}
	return f[1]
}

var sessionIDArgRe = regexp.MustCompile(`^[0-9a-fA-F][0-9a-fA-F-]{31,35}$`)

func sessionArgFromCmdline(args []string) string {
	for i, a := range args {
		if (a == "--session-id" || a == "--resume" || a == "-r") &&
			i+1 < len(args) && sessionIDArgRe.MatchString(args[i+1]) {
			return args[i+1]
		}
		// `sh -c "claude --resume <id> …"`: the whole command is one arg.
		if strings.Contains(a, "--resume ") || strings.Contains(a, "--session-id ") {
			if id := sessionArgFromCmdline(strings.Fields(a)); id != "" {
				return id
			}
		}
	}
	return ""
}

// closeOwned tears down the process or tmux session scimux owns for n.
// Historical external tmux sessions are deliberately left running.
func (a *app) closeOwned(n *Node) error {
	if n.Adopted {
		if a.workers != nil {
			return a.workers.RetireExternalController(n)
		}
		return nil
	}
	if pm := a.proc(n); pm != nil {
		// The worker is an owned controller even after its underlying harness
		// exits. Stop it based on that ownership, not on the last liveness
		// observation; an unreachable worker must make deletion fail closed.
		if workers, ok := pm.(*workerManager); ok {
			return workers.Kill(n.ID)
		}
		if pm.HasSession(n.ID) {
			return pm.Kill(n.ID)
		}
		return nil
	}
	s := a.server.Session(n.ID)
	if s.Alive() {
		return s.Kill()
	}
	return nil
}
