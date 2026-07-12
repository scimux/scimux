// scimux — supervise tmux-wrapped agent chats (Claude Code, Codex) from a
// local web page. Each chat is a node in a research tree; prompts go in via
// tmux, replies come back from the transcript files the agent CLIs write.
package main

import (
	"crypto/rand"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/tmuxsession"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// version is stamped at build time: go build -ldflags "-X main.version=v0.5.0"
var version = "dev"

// hostname is resolved once at startup; shown in the UI statusbar.
var hostname = "scimux"

//go:embed web/*
var webFS embed.FS

// ---------- model ----------

type Node struct {
	ID         string `json:"id"`
	Parent     string `json:"parent,omitempty"`
	Title      string `json:"title"`
	Prompt     string `json:"prompt"`              // first prompt == the node's research question
	Rationale  string `json:"rationale,omitempty"` // why this fork exists (decision evidence)
	Agent      string `json:"agent"`               // "claude" | "codex"
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"` // codex reasoning effort; ignored for claude
	Dir        string `json:"dir"`
	SessionID  string `json:"session_id,omitempty"` // claude session uuid (minted by us) or codex session id (extracted from a resumed pane)
	Transcript string `json:"transcript,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// storeRecord is one line of the append-only store file. Node metadata is
// tiny; chat content lives in the agents' own transcript files.
type storeRecord struct {
	Type string `json:"type"` // "node" | "transcript" | "key"
	Node *Node  `json:"node,omitempty"`
	ID   string `json:"id,omitempty"`
	Path string `json:"path,omitempty"`
	// "key" records are the answered-dialog evidence trail: which key was
	// pressed for a node while what dialog (pane excerpt) was on screen.
	// Replay ignores them — they carry no node state.
	Key     string `json:"key,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
	Time    string `json:"time,omitempty"`
}

type app struct {
	mu      sync.Mutex
	nodes   []*Node
	byID    map[string]*Node
	live    map[string]string // node id -> "active"|"quiet"|"exited"
	attn    map[string]string // node id -> ""|"approval"|"question"
	prevCap map[string]string
	lastChg map[string]time.Time
	tailers map[string]*transcript.Tailer
	// rolloutSnaps: node id -> rollout paths that existed at launch; codex
	// discovery only correlates files that appeared afterwards. A node with
	// no entry has no trustworthy snapshot (adopted, created after an
	// incomplete walk, or scimux restarted) — for such nodes the cwd+time
	// heuristic is unsafe and discovery must not run at all.
	rolloutSnaps map[string]map[string]bool
	// pathClaims: transcript paths reserved by an in-flight discovery store
	// write, so a concurrent adoption cannot publish the same path.
	pathClaims map[string]bool
	// chatMark/staleChat: per-node transcript progress at the last
	// active→quiet pane transition, and whether the file has been growing
	// without recognizable agent-side records since (degrade the UI to peek).
	chatMark  map[string]chatMark
	staleChat map[string]bool

	server    *tmuxsession.Server
	storePath string
	uiPath    string
	codexRoot string
	home      string
}

// ---------- store ----------

func (a *app) appendRecord(rec storeRecord) error {
	f, err := os.OpenFile(a.storePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func (a *app) loadStore() error {
	b, err := os.ReadFile(a.storePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// Replay semantics for the append-only store: corrections are new
	// records. A later node record for an existing ID replaces the value in
	// place (first-seen order preserved, no duplicate UI nodes); the latest
	// transcript record wins regardless of where it appears relative to its
	// node record.
	transcripts := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec storeRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		switch {
		case rec.Type == "node" && rec.Node != nil:
			if existing, ok := a.byID[rec.Node.ID]; ok {
				*existing = *rec.Node
			} else {
				a.nodes = append(a.nodes, rec.Node)
				a.byID[rec.Node.ID] = rec.Node
			}
		case rec.Type == "transcript":
			transcripts[rec.ID] = rec.Path
		}
	}
	for id, path := range transcripts {
		if n, ok := a.byID[id]; ok {
			n.Transcript = path
		}
	}
	return nil
}

// ---------- node lifecycle ----------

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var slugStrip = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// uniqueID allocates a slug that collides neither with registered nodes nor
// with any name in taken — the current tmux sessions, so an unadopted session
// with the same slug cannot make new-session fail.
func (a *app) uniqueID(title string, taken map[string]bool) string {
	slug := strings.Trim(slugStrip.ReplaceAllString(title, "-"), "-.")
	if slug == "" || !tmuxsession.ValidName(slug) {
		slug = "chat"
	}
	if len(slug) > 40 {
		slug = slug[:40]
	}
	id := slug
	for i := 2; ; i++ {
		if _, used := a.byID[id]; !used && !taken[id] {
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
		parts := []string{"claude", "--session-id", n.SessionID}
		if n.Model != "" {
			parts = append(parts, "--model", shellQuote(n.Model))
		}
		return strings.Join(append(parts, shellQuote(n.Prompt)), " "), nil
	case "codex":
		parts := []string{"codex"}
		if n.Model != "" {
			parts = append(parts, "--model", shellQuote(n.Model))
		}
		if n.Effort != "" {
			parts = append(parts, "-c", shellQuote("model_reasoning_effort="+n.Effort))
		}
		return strings.Join(append(parts, shellQuote(n.Prompt)), " "), nil
	// pi and opencode run TUI-supervised only (pane peek + send, no
	// transcript discovery); both take the "provider/model" form their own
	// list commands emit.
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
	}
	return "", fmt.Errorf("unknown agent %q (want claude, codex, pi, or opencode)", n.Agent)
}

// resolveNode validates a new-node request and resolves its launch
// configuration in place: parent inheritance (fresh-context fork: the launch
// config, never the conversation), agent and directory defaults, and the
// title. It is idempotent, so handleNewNode runs it once up front — deciding
// from the resolved result whether the codex rollout snapshot is needed at
// all — and createNode runs the very same step instead of a diverging
// preflight mirror (finding 25). Callers hold a.mu. Returns the HTTP status
// to use on error: every resolution failure is a client mistake, 400.
func (a *app) resolveNode(n *Node) (int, error) {
	if strings.TrimSpace(n.Prompt) == "" {
		return 400, fmt.Errorf("prompt must not be empty")
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
		if n.Model == "" {
			n.Model = p.Model
		}
		if n.Effort == "" {
			n.Effort = p.Effort
		}
		if n.Dir == "" {
			n.Dir = p.Dir
		}
	}
	if n.Agent == "" {
		n.Agent = "claude"
	}
	switch n.Agent {
	case "claude", "codex", "pi", "opencode":
	default:
		return 400, fmt.Errorf("unknown agent %q (want claude, codex, pi, or opencode)", n.Agent)
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
	if n.Title == "" {
		n.Title = firstWords(n.Prompt, 6)
	}
	return 0, nil
}

// createNode validates, starts the tmux session, and persists the node.
// It returns the HTTP status to use on error: client mistakes are 400,
// server-side failures (tmux, store) are 500. Callers hold a.mu; taken
// carries tmux session names that must not be reused as node IDs, and
// rollouts is a pre-launch snapshot of existing codex rollout paths — nil
// means no trustworthy snapshot exists, which disables transcript discovery
// for this node (peek still works) rather than risking a wrong link.
func (a *app) createNode(n *Node, taken map[string]bool, rollouts map[string]bool) (int, error) {
	if status, err := a.resolveNode(n); err != nil {
		return status, err
	}
	n.ID = a.uniqueID(n.Title, taken)
	if n.Agent == "claude" {
		n.SessionID = newUUID()
	}
	n.CreatedAt = time.Now().UTC().Format(time.RFC3339)

	cmd, err := agentCommand(n)
	if err != nil {
		return 400, err
	}
	if _, err := a.server.NewSession(n.ID, n.Dir, cmd); err != nil {
		return 500, err
	}
	// Persist before publishing in memory. The tmux session had to exist
	// first, so a store failure rolls it back — otherwise a session would
	// run supervised-in-memory but vanish from the registry on restart.
	if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		if kerr := a.server.Session(n.ID).Kill(); kerr != nil {
			fmt.Fprintf(os.Stderr, "scimux: rollback of session %s failed: %v\n", n.ID, kerr)
		}
		return 500, fmt.Errorf("persist node (session rolled back): %v", err)
	}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	if n.Agent == "codex" && rollouts != nil {
		// Discovery must only correlate rollouts that appear after launch.
		a.rolloutSnaps[n.ID] = rollouts
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

// codexSessionFromCmdline extracts the session id from a resumed codex
// invocation (`codex resume <id>`). A fresh `codex` launch carries no id —
// codex only exposes one on resume.
func codexSessionFromCmdline(args []string) string {
	for i, a := range args {
		if a == "resume" && i+1 < len(args) && sessionIDArgRe.MatchString(args[i+1]) {
			return args[i+1]
		}
		// `sh -c "codex resume <id> …"`: the whole command is one arg.
		if strings.Contains(a, "resume ") {
			if id := codexSessionFromCmdline(strings.Fields(a)); id != "" {
				return id
			}
		}
	}
	return ""
}

// ---------- background poller ----------

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
		s := a.server.Session(n.ID)
		state := "exited"
		if s.Alive() {
			cap, err := s.Capture()
			if err == nil {
				a.mu.Lock()
				if cap != a.prevCap[n.ID] {
					a.prevCap[n.ID] = cap
					a.lastChg[n.ID] = time.Now()
				}
				if time.Since(a.lastChg[n.ID]) < 8*time.Second {
					state = "active"
				} else {
					state = "quiet"
				}
				a.mu.Unlock()
			} else {
				state = "unavailable"
			}
		}
		attn := ""
		if state == "quiet" {
			a.mu.Lock()
			prev := a.live[n.ID] // not yet overwritten this tick
			a.mu.Unlock()
			if tl := a.tailerFor(n); tl != nil {
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
		}
		a.mu.Lock()
		a.live[n.ID] = state
		a.attn[n.ID] = attn
		a.mu.Unlock()
		a.discoverTranscript(n)
	}
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
	created, err := time.Parse(time.RFC3339, n.CreatedAt)
	if err != nil || time.Since(created) > 15*time.Minute {
		a.mu.Lock()
		delete(a.rolloutSnaps, n.ID) // stale: stop searching, free the snapshot
		a.mu.Unlock()
		return // peek remains available
	}
	// Transcript paths are exclusive among nodes, and codex discovery only
	// considers rollouts that appeared after this node's launch (pre-launch
	// snapshot): an older same-cwd rollout still being appended by some other
	// session can never be linked, no matter what its mtime says.
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
	_, haveSnap := a.rolloutSnaps[n.ID]
	for p := range a.rolloutSnaps[n.ID] {
		excluded[p] = true
	}
	a.mu.Unlock()
	var path string
	var ok bool
	switch n.Agent {
	case "claude":
		if n.SessionID == "" {
			return // adopted without an id: adoption already made its one guess
		}
		path, ok = transcript.FindClaudeTranscript(a.home, n.SessionID)
	case "codex":
		switch {
		case n.SessionID != "":
			// Deterministic: the id (from a resumed pane's command line) is
			// embedded in the rollout filename by codex itself.
			path, ok = transcript.FindCodexRolloutBySession(a.codexRoot, n.SessionID)
		case haveSnap:
			path, ok = transcript.FindCodexRollout(a.codexRoot, n.Dir, created, excluded)
		default:
			// No snapshot and no session id — adopted, restarted, or launched
			// after an incomplete walk. The cwd+time heuristic could link a
			// merely plausible rollout, and a wrong conversation is worse
			// than none: stay on peek.
			return
		}
	}
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
	delete(a.rolloutSnaps, n.ID)
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

// ---------- sysload ----------

// sysInfo carries normalized numbers; the browser only renders values and
// trends (no Linux parsing rules in JavaScript). Sampled at most every 30 s
// so the state ETag stays stable between sys refreshes on an idle system.
type sysInfo struct {
	Load1      float64 `json:"load1"`
	NCPU       int     `json:"ncpu"`
	MemPct     float64 `json:"mem_pct"`
	MemTotalGB float64 `json:"mem_total_gb"`
	SwapPct    float64 `json:"swap_pct"`
}

var (
	sysMu      sync.Mutex
	sysCache   sysInfo
	sysCacheAt time.Time
)

func sysload() sysInfo {
	sysMu.Lock()
	defer sysMu.Unlock()
	if time.Since(sysCacheAt) < 30*time.Second {
		return sysCache
	}
	out := sysInfo{NCPU: runtime.NumCPU()}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 1 {
			fmt.Sscanf(f[0], "%f", &out.Load1)
		}
	}
	var totalKB, availKB, swapTotalKB, swapFreeKB float64
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			var v float64
			if _, err := fmt.Sscanf(line, "MemTotal: %f kB", &v); err == nil {
				totalKB = v
			}
			if _, err := fmt.Sscanf(line, "MemAvailable: %f kB", &v); err == nil {
				availKB = v
			}
			if _, err := fmt.Sscanf(line, "SwapTotal: %f kB", &v); err == nil {
				swapTotalKB = v
			}
			if _, err := fmt.Sscanf(line, "SwapFree: %f kB", &v); err == nil {
				swapFreeKB = v
			}
		}
	}
	if totalKB > 0 {
		out.MemPct = 100 * (totalKB - availKB) / totalKB
		out.MemTotalGB = totalKB / 1024 / 1024
	}
	if swapTotalKB > 0 {
		out.SwapPct = 100 * (swapTotalKB - swapFreeKB) / swapTotalKB
	}
	sysCache, sysCacheAt = out, time.Now()
	return out
}

// ---------- HTTP ----------

type nodeView struct {
	*Node
	Live          string `json:"live"`
	Attention     string `json:"attention,omitempty"` // "approval" | "question"
	HasTranscript bool   `json:"has_transcript"`
	LastActivity  int64  `json:"last_activity,omitempty"` // unix ms of last pane change
}

func (a *app) handleState(w http.ResponseWriter, r *http.Request) {
	sessions := a.server.Sessions()
	a.mu.Lock()
	views := make([]nodeView, 0, len(a.nodes))
	for _, n := range a.nodes {
		var lastMS int64
		if t, ok := a.lastChg[n.ID]; ok {
			lastMS = t.UnixMilli()
		}
		// Copy the node while holding the lock: marshaling a live *Node after
		// unlock races the poller's Transcript writes (a Go data race).
		nc := *n
		views = append(views, nodeView{Node: &nc, Live: a.live[n.ID], Attention: a.attn[n.ID],
			HasTranscript: n.Transcript != "", LastActivity: lastMS})
	}
	// Sessions on our socket that no node accounts for: candidates for
	// adoption (manually created, or migrated from another tmux server).
	unadopted := []string{}
	for _, s := range sessions {
		if _, known := a.byID[s]; !known {
			unadopted = append(unadopted, s)
		}
	}
	a.mu.Unlock()
	body, err := json.Marshal(map[string]any{
		"nodes": views, "unadopted": unadopted, "sys": sysload(),
		"socket": a.server.Socket, "hostname": hostname, "version": version,
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// ETag short-circuit: an unchanged state costs the client a few header
	// bytes instead of a body — polling stays, but nearly free when idle.
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

// handleAdopt registers an already-running tmux session (which scimux did
// not start) as a node. No tmux state is touched — this only writes the
// node record, so adoption is always safe for the running agent.
func (a *app) handleAdopt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session    string `json:"session"`
		Title      string `json:"title"`
		Prompt     string `json:"prompt"`
		Agent      string `json:"agent"`
		Model      string `json:"model"`
		Dir        string `json:"dir"`
		SessionID  string `json:"session_id"`
		Transcript string `json:"transcript"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Session == "" {
		http.Error(w, "bad request: need session", 400)
		return
	}
	s := a.server.Session(body.Session)
	if !s.Alive() {
		http.Error(w, fmt.Sprintf("no session %q on socket %q", body.Session, a.server.Socket), 404)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, taken := a.byID[body.Session]; taken {
		http.Error(w, "node already exists", 409)
		return
	}
	dir := body.Dir
	if dir == "" {
		if cwd, err := s.Cwd(); err == nil {
			dir = cwd
		}
	}
	title := body.Title
	if title == "" {
		title = body.Session
	}
	agent := body.Agent
	if agent == "" {
		agent = "claude"
	}
	n := &Node{ID: body.Session, Title: title, Prompt: body.Prompt, Agent: agent,
		Model: body.Model, Dir: dir, SessionID: body.SessionID, Transcript: body.Transcript,
		CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	// Adopted claude session without a known id: first try the pane's own
	// process arguments (claude --resume <id> / --session-id <id>), which is
	// deterministic even with several sessions in one directory. Fall back
	// to the newest session log in the working directory. A wrong or missing
	// guess still leaves peek + send working.
	if n.Agent == "claude" && n.SessionID == "" && n.Transcript == "" {
		if pid, err := s.PanePID(); err == nil {
			n.SessionID = sessionFromPane(pid, sessionArgFromCmdline)
		}
		// A process-derived id remains authoritative even if Claude has not
		// created (or we cannot yet see) its transcript. discoverTranscript
		// will retry it; do not replace it with the nondeterministic mtime guess.
		if n.SessionID == "" {
			if path, sid, ok := transcript.FindClaudeNewestInDir(a.home, n.Dir); ok {
				n.Transcript, n.SessionID = path, sid
			}
		}
	}
	// Adopted codex session: a resumed pane carries the session id on its
	// command line. Without an id there is no pre-launch snapshot to make
	// the cwd+time heuristic safe (an adopted/resumed session normally
	// appends to an *existing* rollout among possibly several same-cwd
	// candidates), so no guess is attempted: linking the wrong conversation
	// is worse than showing peek. The node stays on pane snapshots unless an
	// explicit session id or transcript is supplied.
	if n.Agent == "codex" && n.SessionID == "" && n.Transcript == "" {
		if pid, err := s.PanePID(); err == nil {
			n.SessionID = sessionFromPane(pid, codexSessionFromCmdline)
		}
	}
	// Any known session id — supplied explicitly (the migration script sends
	// one) or extracted from the pane above — gets its deterministic lookup
	// now: the claude id is the transcript filename, the codex id is embedded
	// in the rollout filename by codex itself. If the file is not visible
	// yet, discoverTranscript retries the same lookup.
	if n.SessionID != "" && n.Transcript == "" {
		switch n.Agent {
		case "claude":
			if path, ok := transcript.FindClaudeTranscript(a.home, n.SessionID); ok {
				n.Transcript = path
			}
		case "codex":
			if path, ok := transcript.FindCodexRolloutBySession(a.codexRoot, n.SessionID); ok {
				n.Transcript = path
			}
		}
	}
	// Transcript exclusivity holds for adoption too: a path another node
	// owns (or a discovery is reserving right now) must not be published twice.
	if n.Transcript != "" && a.pathClaimedLocked(n.Transcript, n.ID) {
		http.Error(w, fmt.Sprintf("transcript %q already belongs to another node", n.Transcript), 409)
		return
	}
	// Persist before publishing: an adopted node that exists only in memory
	// would silently vanish from the registry on restart.
	if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		http.Error(w, "persist node: "+err.Error(), 500)
		return
	}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	writeJSON(w, n)
}

func (a *app) handleNewNode(w http.ResponseWriter, r *http.Request) {
	var n Node
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		http.Error(w, "bad request: "+err.Error(), 400)
		return
	}
	// Resolve and validate the launch configuration first: no request that
	// fails validation (empty prompt, unknown parent, bad agent or dir) ever
	// pays the codex-history walk, and the walk decision reads the *resolved*
	// agent, so an explicit-codex request with a missing parent is rejected
	// here rather than after a full traversal (finding 25).
	a.mu.Lock()
	status, err := a.resolveNode(&n)
	a.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	// Snapshot current session names before the critical section (tmux is
	// slow); the ID allocator must avoid unadopted sessions.
	taken := map[string]bool{}
	for _, s := range a.server.Sessions() {
		taken[s] = true
	}
	// The rollout snapshot walks the entire codex history, so take it only
	// when this request will actually launch codex. An incomplete walk
	// cannot guarantee "everything not in the snapshot appeared after
	// launch"; then the launch proceeds but discovery stays disabled (nil
	// snapshot) and the node relies on peek.
	var rollouts map[string]bool
	if n.Agent == "codex" {
		snap, complete := transcript.ListCodexRollouts(a.codexRoot)
		if complete {
			rollouts = snap
		} else {
			fmt.Fprintf(os.Stderr, "scimux: codex rollout snapshot incomplete; transcript discovery disabled for this node\n")
		}
	}
	a.mu.Lock()
	status, err = a.createNode(&n, taken, rollouts)
	a.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, n)
}

func (a *app) node(r *http.Request) (*Node, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, ok := a.byID[r.PathValue("id")]
	return n, ok
}

func (a *app) handleSend(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	var body struct{ Text string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if err := a.server.Session(n.ID).Send(body.Text); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]string{"ok": "sent"})
}

func (a *app) handleChat(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	tl := a.tailerFor(n) // may reset staleness on a relink; read flags after
	a.mu.Lock()
	pending := n.Transcript == ""
	live := a.live[n.ID]
	stale := a.staleChat[n.ID]
	a.mu.Unlock()
	turns := []transcript.Turn{}
	if tl != nil {
		turns = tl.Poll()
	}
	// fallback signals "the transcript is not (or no longer) making sense" —
	// missing, not yet populated, unreadable, format-incompatible from the
	// start, structurally broken after valid turns (Unparseable), or still
	// growing through whole pane-activity cycles without one recognizable
	// chat record (staleChat: a typed future format). A path string existing
	// does not mean the transcript is usable; the client must degrade to the
	// pane snapshot in every one of these cases.
	writeJSON(w, map[string]any{"turns": turns, "pending": pending, "live": live,
		"fallback": len(turns) == 0 || (tl != nil && tl.Unparseable()) || stale})
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

// handleKey presses one whitelisted key in the node's pane — answering an
// approval prompt or question menu remotely — and appends the pane's bottom
// lines plus the key to the store as decision evidence.
func (a *app) handleKey(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	var body struct{ Key string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if !tmuxsession.AllowedKey(body.Key) {
		http.Error(w, fmt.Sprintf("key %q not allowed", body.Key), 400)
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

// handleCodexRollouts lists the codex rollouts whose session_meta records the
// given cwd, newest first — the migration script's candidate enumeration. The
// cwd match reuses the same JSON parsing as discovery (never raw substring
// matching of undocumented log bytes), and the response is plain text — one
// "mtime<TAB>session-id<TAB>path<TAB>preview" line per rollout — so a POSIX
// shell can consume it without a JSON parser. The preview is the last visible
// turn, rendered by the real transcript parser, so the operator can correlate
// a candidate with the pane they are migrating. Deliberately *not* used for
// automatic linking: with several same-cwd sessions only a human (or an
// explicit session id) can pick the right one.
func (a *app) handleCodexRollouts(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		http.Error(w, "bad request: need dir", 400)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, ri := range transcript.FindCodexRolloutsForDir(a.codexRoot, dir) {
		preview := ""
		if turns := (&transcript.Tailer{Path: ri.Path}).Poll(); len(turns) > 0 {
			preview = turns[len(turns)-1].Text
		}
		preview = strings.Join(strings.Fields(preview), " ") // no tabs/newlines
		if r := []rune(preview); len(r) > 80 {
			preview = string(r[:80]) + "…"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			ri.ModTime.UTC().Format(time.RFC3339), ri.SessionID, ri.Path, preview)
	}
}

func (a *app) handlePeek(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	cap, err := a.server.Session(n.ID).Capture()
	if err != nil {
		cap = "(session exited or unavailable)\n\n" + err.Error()
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, cap)
}

// ---------- UI state ----------

// The web client owns a small blob of cross-device state — map group tabs,
// the archived-card set, private notes — that must survive scimux restarts,
// so it lives next to the node store instead of in localStorage (per-device
// state like drafts stays client-side). The blob is opaque JSON to the
// server: its shape belongs to the client. Last writer wins; the writers are
// one supervisor's own devices.
const uiStateMax = 1 << 20

func (a *app) handleUIGet(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	b, err := os.ReadFile(a.uiPath)
	a.mu.Unlock()
	if err != nil {
		b = []byte("{}")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (a *app) handleUIPut(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, uiStateMax+1))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if len(b) > uiStateMax {
		http.Error(w, "ui state too large", 413)
		return
	}
	if !json.Valid(b) {
		http.Error(w, "ui state must be valid JSON", 400)
		return
	}
	// Atomic replace under the lock: a crash mid-write must never leave a
	// truncated file, and concurrent PUTs must not interleave tmp files.
	a.mu.Lock()
	defer a.mu.Unlock()
	tmp := a.uiPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := os.Rename(tmp, a.uiPath); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]string{"ok": "saved"})
}

// handleAgents reports the installed harnesses and their models — the
// new-activity dialog's source of truth (its built-in list is only the
// fallback for when this call fails).
func (a *app) handleAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, detectAgents())
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ---------- main ----------

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		hostname = h
	}
	addr := flag.String("addr", "127.0.0.1:8787", "listen address (loopback only; use an SSH tunnel for remote access)")
	data := flag.String("data", filepath.Join(home, ".scimux"), "data directory for the node store")
	socket := flag.String("socket", "scimux", "tmux socket name (tmux -L) for the private server")
	codexRoot := flag.String("codex-sessions", filepath.Join(home, ".codex", "sessions"), "where Codex writes rollout logs")
	flag.Parse()

	if err := os.MkdirAll(*data, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	a := &app{
		byID:         map[string]*Node{},
		live:         map[string]string{},
		attn:         map[string]string{},
		prevCap:      map[string]string{},
		lastChg:      map[string]time.Time{},
		tailers:      map[string]*transcript.Tailer{},
		rolloutSnaps: map[string]map[string]bool{},
		pathClaims:   map[string]bool{},
		chatMark:     map[string]chatMark{},
		staleChat:    map[string]bool{},
		server:       tmuxsession.NewServer(*socket),
		storePath:    filepath.Join(*data, "nodes.jsonl"),
		uiPath:       filepath.Join(*data, "ui.json"),
		codexRoot:    *codexRoot,
		home:         home,
	}
	if err := a.loadStore(); err != nil {
		fmt.Fprintln(os.Stderr, "scimux: load store:", err)
		os.Exit(1)
	}

	go func() {
		for {
			a.poll()
			time.Sleep(2 * time.Second)
		}
	}()
	// Warm the harness/model probe (it shells out to the agent CLIs) so the
	// first new-activity dialog doesn't wait on subprocesses.
	go detectAgents()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The UI is embedded in the binary and changes with every build;
		// a cached copy after a scimux upgrade is a recurring dogfooding
		// trap (especially iPad Safari). It's one small local page: always
		// fetch fresh.
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})
	mux.HandleFunc("GET /api/state", a.handleState)
	mux.HandleFunc("POST /api/nodes", a.handleNewNode)
	mux.HandleFunc("POST /api/adopt", a.handleAdopt)
	mux.HandleFunc("POST /api/nodes/{id}/send", a.handleSend)
	mux.HandleFunc("POST /api/nodes/{id}/key", a.handleKey)
	mux.HandleFunc("GET /api/nodes/{id}/chat", a.handleChat)
	mux.HandleFunc("GET /api/nodes/{id}/peek", a.handlePeek)
	mux.HandleFunc("GET /api/codex-rollouts", a.handleCodexRollouts)
	mux.HandleFunc("GET /api/agents", a.handleAgents)
	mux.HandleFunc("GET /api/ui", a.handleUIGet)
	mux.HandleFunc("PUT /api/ui", a.handleUIPut)

	fmt.Printf("scimux: http://%s/  (tmux socket %q, store %s)\n", *addr, *socket, a.storePath)
	fmt.Printf("scimux: attach to a chat by hand: tmux -L %s attach -t <node-id>\n", *socket)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
}
