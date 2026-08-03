package app

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"codeberg.org/chrberger/scimux/internal/tmuxsession"
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

// slugStrip collapses any run of characters not allowed in a tmux session name
// into a single '-'. '.' is excluded from the allow-set on purpose: it is tmux's
// window.pane target separator, so a slug containing it yields an unaddressable
// session (see tmuxsession.nameRe). dashRun then squeezes the '-' runs this can
// leave (e.g. from " - ") so names stay tidy.
var slugStrip = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
var dashRun = regexp.MustCompile(`-{2,}`)

// uniqueID allocates a slug that collides neither with registered nodes nor
// with any name in taken — the current tmux sessions, so an unadopted session
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

// agentCommand builds the launch command. The first prompt rides on the
// command line so prompt delivery and session start are atomic — no
// "is the TUI drawn yet" race, which only later turns (via paste) tolerate.
func agentCommand(n *Node) (string, error) {
	switch n.Agent {
	case "claude":
		parts := []string{"claude", "--session-id", n.SessionID, "--remote-control"}
		if n.Title != "" {
			parts = append(parts, shellQuote(n.Title))
		}
		if n.Model != "" {
			parts = append(parts, "--model", shellQuote(n.Model))
		}
		// The claude CLI takes --effort <level> (low/medium/high/xhigh/max); pass
		// it only when set so an effort-less node launches exactly as before.
		if n.Effort != "" {
			parts = append(parts, "--effort", shellQuote(n.Effort))
		}
		return strings.Join(append(parts, shellQuote(n.Prompt)), " "), nil
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
	return "", fmt.Errorf("unknown agent %q (want claude, codex, pi, opencode, or grok)", n.Agent)
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
// vanishing into an unexplained dead node (the "unknown error state, could not
// be adopted" failure). The original command runs verbatim first — so the first
// prompt still rides the command line — and the sentinel is emitted only on a
// non-zero exit, so a clean quit falls through untouched (pane closes, session
// dies, exactly as before). tmux runs this whole string via `sh -c`.
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

// refreshClaudeModels populates the family->id map, preferring a cached probe
// no older than claudeCacheTTL over a fresh (API-billed) `claude -p` call. A
// successful probe is cached; a failed one writes nothing and falls back to any
// stale cache, so the call is retried at the next startup. Best-effort: run in a
// background goroutine at startup, never blocking the UI.
func (a *app) refreshClaudeModels(ctx context.Context) {
	cache := readClaudeCache(a.claudeCachePath)
	if len(cache.IDs) > 0 && time.Since(cache.ProbedAt) < claudeCacheTTL {
		a.setClaudeIDs(cache.IDs)
		return
	}
	if ids := probeClaudeModels(ctx); len(ids) > 0 {
		a.setClaudeIDs(ids)
		if err := writeClaudeCache(a.claudeCachePath, ids); err != nil {
			fmt.Fprintf(os.Stderr, "scimux: cache claude models: %v\n", err)
		}
		return
	}
	// Probe failed: keep serving a stale cache if we have one; the retry is the
	// next startup (nothing fresh was written).
	if len(cache.IDs) > 0 {
		a.setClaudeIDs(cache.IDs)
	}
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
	case "claude", "codex", "pi", "opencode", "grok":
	default:
		return 400, fmt.Errorf("unknown agent %q (want claude, codex, pi, opencode, or grok)", n.Agent)
	}
	// New nodes pick a transport by agent: pi/opencode/grok over ACP, codex over
	// its app-server bridge, claude over tmux. Only set this on creation —
	// stored records with an absent Transport are migrated to tmux by
	// Node.transport, never rewritten here.
	if n.Transport == "" {
		switch n.Agent {
		case "pi", "opencode", "grok":
			n.Transport = "acp"
		case "codex":
			n.Transport = "codex"
		default:
			n.Transport = "tmux"
		}
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

// createNode validates and reserves the node id under a.mu, then runs the
// external launch and the store append with the lock released — a structured
// launch can spend up to its negotiation timeout shelling out, and holding
// a.mu for that window would stall every handler, /api/state polling included
// (R18.5). The reservation keeps the id invisible to concurrent creates until
// the node is published or the attempt failed. Returns the HTTP status to use
// on error: client mistakes are 400, server-side failures (tmux, store) are
// 500. taken carries tmux session names that must not be reused as node IDs.
func (a *app) createNode(n *Node, taken map[string]bool) (int, error) {
	a.mu.Lock()
	if status, err := a.resolveNode(n); err != nil {
		a.mu.Unlock()
		return status, err
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
			return 500, fmt.Errorf("allocate session id: %w", err)
		}
		n.SessionID = sid
	}
	n.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	pm := a.proc(n)
	a.mu.Unlock()

	status, err := a.launchNode(n, pm)

	// Publish (or abandon) under the lock; either way the reservation ends.
	// Re-check the id for a collision at publish time: the launch ran with
	// a.mu released, and a double-publish would leave two *Node values under
	// one id with the store replaying both (R20.2). Every id-minting path now
	// consults a.reserved (uniqueID, handleAdopt), so a hit here should be
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
		return status, err
	}
	if collided {
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
		// live node with no manual re-adopt (R21.4). The winner owns
		// sessions/<id>.jsonl, so its log is left intact — never archived here.
		_ = a.appendRecord(storeRecord{Type: "delete", ID: n.ID, Time: time.Now().UTC().Format(time.RFC3339)})
		if winner != nil {
			_ = a.appendRecord(storeRecord{Type: "node", Node: winner})
		}
		return 409, fmt.Errorf("node id %q was claimed concurrently; launch abandoned", n.ID)
	}

	// Deliver the research question as the first turn of a structured node.
	// Unlike the tmux path (where the first prompt rides the launch command
	// line and thus always reaches the agent), a structured Send can fail
	// before anything is recorded — e.g. the subprocess died during launch, or
	// the user-turn append failed. Persist that failure to the node's own
	// history so the chat view shows it instead of a silent, empty successful
	// node (finding 52). The node still exists and the prompt can be retried.
	if pm != nil {
		if err := pm.Send(n.ID, n.Prompt); err != nil {
			fmt.Fprintf(os.Stderr, "scimux: first prompt to %s node %s failed: %v\n", n.transport(), n.ID, err)
			// If the failure record also cannot be written (the session log is
			// the failing component — commonly the same disk problem that broke
			// Send), there is no durable trace of the lost first prompt. The
			// node is persisted and must stay, but the caller must not see a
			// clean success: report it so the operator retries the prompt
			// (finding 59).
			if rerr := pm.RecordStartFailure(n.ID, err); rerr != nil {
				return 500, fmt.Errorf("node created but first prompt %q and its failure record were not durable (retry the prompt): %v", err, rerr)
			}
		}
	}
	return 0, nil
}

// launchNode starts the tmux session or structured-protocol subprocess (ACP
// for pi/opencode/grok, codex app-server for codex) and persists the node record.
// Runs without a.mu. Persist follows launch: the session/process had to exist
// first, so a store failure rolls it back (kill) — otherwise a session would
// run supervised-in-memory but vanish from the registry on restart. The
// caller publishes the node in memory only after this succeeds.
func (a *app) launchNode(n *Node, pm procManager) (int, error) {
	if pm != nil {
		sid, err := pm.Launch(n.ID, n.Agent, n.Dir, n.Model, n.Effort)
		if err != nil {
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
	launch := n
	// Resolve a claude family alias (opus) to the concrete id the CLI accepts
	// (claude-opus-4-8) — the CLI mis-resolves its own aliases. Resolve into a
	// copy so the stored node keeps the durable alias; only the launched command
	// carries the id. No mapping (probe absent/failed, or already a concrete id)
	// leaves the value unchanged.
	if n.Agent == "claude" {
		if id := a.resolveClaudeModel(n.Model); id != "" {
			cp := *n
			cp.Model = id
			launch = &cp
		}
	}
	cmd, err := agentCommand(launch)
	if err != nil {
		return 400, err
	}
	if _, err := a.server.NewSession(n.ID, n.Dir, wrapLaunch(cmd)); err != nil {
		return 500, err
	}
	// Catch a launch that dies before its interface is ready (a rejected --model,
	// a bad flag): surface the agent's own error to the caller rather than
	// persisting a node whose session has already vanished. Nothing is stored
	// yet, so failing here leaves no phantom node — only the held-open session to
	// reap.
	if reason, bad := a.awaitLaunch(n.ID); bad {
		if kerr := a.server.Session(n.ID).Kill(); kerr != nil {
			fmt.Fprintf(os.Stderr, "scimux: reaping failed launch %s: %v\n", n.ID, kerr)
		}
		return 502, fmt.Errorf("agent exited on launch: %s", reason)
	}
	if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		if kerr := a.server.Session(n.ID).Kill(); kerr != nil {
			fmt.Fprintf(os.Stderr, "scimux: rollback of session %s failed: %v\n", n.ID, kerr)
		}
		return 500, fmt.Errorf("persist node (session rolled back): %v", err)
	}
	return 0, nil
}

func firstWords(s string, n int) string {
	words := strings.Fields(s)
	if len(words) > n {
		words = words[:n]
	}
	return strings.Join(words, " ")
}

// sessionFromPane inspects the pane's process and its direct children (tmux
// may wrap the command in `sh -c`), applying extract to each command line to
// find an agent session id. Linux /proc only; returns "" anywhere it can't
// look.
// paneSessionID resolves the claude session id from the pane's process tree,
// through the test seam when one is installed.
func (a *app) paneSessionID(pid string) string {
	if a.paneSession != nil {
		return a.paneSession(pid)
	}
	return sessionFromPane(pid, sessionArgFromCmdline)
}

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

// closeOwned tears down the process or tmux session scimux owns for n. Adopted
// tmux sessions are deliberately left running — scimux did not start them.
func (a *app) closeOwned(n *Node) error {
	if pm := a.proc(n); pm != nil {
		if pm.HasSession(n.ID) {
			return pm.Kill(n.ID)
		}
		return nil
	}
	if n.Adopted {
		return nil
	}
	s := a.server.Session(n.ID)
	if s.Alive() {
		return s.Kill()
	}
	return nil
}
