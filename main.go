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
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp"
	"codeberg.org/chrberger/scimux/internal/acp/codex"
	"codeberg.org/chrberger/scimux/internal/dialoghint"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
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
	ID          string `json:"id"`
	Parent      string `json:"parent,omitempty"`
	Title       string `json:"title"`
	Prompt      string `json:"prompt"`                // first prompt == the node's research question
	Description string `json:"description,omitempty"` // card/map description; defaults to Prompt
	Rationale   string `json:"rationale,omitempty"`   // why this fork exists (decision evidence)
	LaneID      string `json:"lane_id,omitempty"`     // immutable journey lane assignment; empty means unassigned
	// AlsoLanes marks an interchange station: further journeys this activity
	// serves beyond its home LaneID. Mutable, unlike the home lane — a
	// cross-link is discovered knowledge about the journey network, so it must
	// be addable after the fact (corrections are new records).
	AlsoLanes []string `json:"also_lanes,omitempty"`
	// Service is the station's rail-map state, orthogonal to process liveness:
	// "siding" (a question ridden out and set aside — spur + buffer stop) or
	// "terminus" (completed, ceiling reached — cap bar). Empty means through
	// service. Mutable for the same reason as AlsoLanes: a branch's death is
	// often recognized only later.
	Service    string `json:"service,omitempty"`
	Agent      string `json:"agent"` // "claude" | "codex" | "pi" | "opencode"
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"` // codex reasoning effort; ignored for claude
	Dir        string `json:"dir"`
	SessionID  string `json:"session_id,omitempty"` // claude: session uuid (minted by us); codex: thread id from thread/start; ACP: session id
	Transcript string `json:"transcript,omitempty"`
	Adopted    bool   `json:"adopted,omitempty"` // adopted tmux sessions are never killed by scimux
	// Transport selects the supervision mechanism: "tmux" (TUI + pane peek +
	// transcript files, the original path — claude), "acp" (an Agent Client
	// Protocol subprocess, pi/opencode) or "codex" (codex's app-server protocol
	// wrapped as a structured bridge). "acp" and "codex" are both structured
	// subprocess transports (no pane); see procManager. An absent value means
	// tmux — every stored record predates this field, so migration is "" ==
	// "tmux" (see transport).
	Transport string `json:"transport,omitempty"`
	CreatedAt string `json:"created_at"`
}

// transport reports the node's supervision mechanism, defaulting an absent
// value to "tmux" so pre-existing store records replay unchanged.
func (n *Node) transport() string {
	if n.Transport == "" {
		return "tmux"
	}
	return n.Transport
}

// storeRecord is one line of the append-only store file. Node metadata is
// tiny; chat content lives in the agents' own transcript files.
type storeRecord struct {
	Type string `json:"type"` // "node" | "transcript" | "key" | "delete"
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
	attn    map[string]string // node id -> ""|"approval"|"question"|"inspect"
	prevCap map[string]string
	lastChg map[string]time.Time
	// activeSince: when the pane last entered the mechanically active state —
	// the start of the current/most recent working phase. Used to judge at the
	// active→quiet transition whether the linked transcript carried the phase
	// (mtime after the phase start) or has gone dead (session rollover).
	activeSince map[string]time.Time
	tailers     map[string]*transcript.Tailer
	// mirrors: per-node transcript→session-log projection state (mirror.go),
	// so tmux nodes end up with the same on-disk history as structured ones.
	mirrors map[string]*mirror
	// pathClaims: transcript paths reserved by an in-flight discovery store
	// write, so a concurrent adoption cannot publish the same path.
	pathClaims map[string]bool
	// chatMark/staleChat: per-node transcript progress at the last
	// active→quiet pane transition, and whether the file has been growing
	// without recognizable agent-side records since (degrade the UI to peek).
	chatMark  map[string]chatMark
	staleChat map[string]bool
	// sendState: per-node web prompt delivery state, "submitting" while a
	// send is in flight, "unconfirmed" when neither the pane nor the
	// transcript acknowledged the submission. New sends are held (409) until
	// the state clears, so a delayed Enter can never stack a second prompt
	// onto an unsubmitted first one.
	sendState map[string]string
	// reserved: node ids claimed by an in-flight create whose external launch
	// runs outside a.mu; uniqueID must not reissue them, like pathClaims for
	// transcript paths.
	reserved map[string]bool
	// anim: per-node pane-change geometry (noteAnim). Tracks whether
	// successive capture diffs stay confined to the same few lines — the
	// mechanical signature of a static screen with an animation strip (a
	// queued tool's spinner under an approval dialog), as opposed to
	// streaming output. Feeds the active-pane dialog corroboration and the
	// stalled-wait backstop in poll().
	anim map[string]*animState
	// paneSession, when non-nil, replaces the /proc-based session-id lookup
	// for a pane pid (sessionFromPane). Tests set it: the process tree behind
	// a fake tmux pane is not reachable through the Runner seam.
	paneSession func(pid string) string

	server    *tmuxsession.Server
	acp       acpManager
	codex     codexManager
	storePath string
	uiPath    string
	// sessionsDir is the unified session-log store: one JSONL file per node,
	// every transport, one schema (internal/sessionlog). Future readers
	// (search, consolidation, sharing) scan this one directory. It is also
	// the chat read path for every transport: handleChat renders from the
	// log, never from the transcript tailer (phase 3 of the consolidation).
	sessionsDir string
	// segCache memoizes each node's parsed current segment so the 1s chat
	// poll costs a stat, not a reparse, while the log is unchanged.
	segCache map[string]*sessionlog.Cache
	home     string
}

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
	return c.Segment(filepath.Join(a.sessionsDir, n.ID+".jsonl"))
}

// PermOption is one answerable permission/decision choice surfaced to the UI:
// the key a supervisor presses and its human-readable name.
type PermOption struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// procManager is the shared surface of scimux's two structured-protocol
// transports: acp.Manager (pi/opencode over the ACP SDK) and codex.Manager
// (codex over its app-server protocol). Both drive one subprocess per node,
// keep the authoritative history in an append-only session log, and answer
// permission prompts structurally — so the create/poll/chat/send/key/peek paths
// treat them uniformly. tmux (claude) nodes are not driven through it.
type procManager interface {
	Launch(nodeID, agent, dir, model, effort string) (string, error)
	Send(nodeID, text string) error
	// Clear is the structured-transport /clear: a fresh protocol session on
	// the same subprocess, recorded as a source seam in the session log — the
	// chat surface turns the page, the node and its log file stay.
	Clear(nodeID string) error
	Interrupt(nodeID string) error
	PrepareResolve(nodeID, key string) (optID, evidence string, err error)
	Deliver(nodeID, optID string) error
	Pending(nodeID string) (title string, opts []PermOption, ok bool)
	Turns(nodeID string) []transcript.Turn
	Peek(nodeID string) string
	Usage(nodeID string) (used, window int64)
	Live(nodeID string) string
	Attention(nodeID string) string
	LastError(nodeID string) string
	HasSession(nodeID string) bool
	Kill(nodeID string) error
	RecordStartFailure(nodeID string, cause error) error
	Shutdown()
	// Conflict reports whether a Send/Resolve error is a client/state conflict
	// (HTTP 409) rather than a server error (500).
	Conflict(err error) bool
}

// acpManager and codexManager adapt the two concrete managers to procManager:
// each embeds its manager (whose method set already matches) and adds only the
// two pieces the interface needs but the managers express with package-local
// types — a PermOption of the shared shape and error classification.
type acpManager struct{ *acp.Manager }

func (m acpManager) Pending(id string) (string, []PermOption, bool) {
	title, opts, ok := m.Manager.Pending(id)
	out := make([]PermOption, len(opts))
	for i, o := range opts {
		out[i] = PermOption{Key: o.Key, Name: o.Name}
	}
	return title, out, ok
}

func (m acpManager) Conflict(err error) bool {
	return err == acp.ErrNoSession || err == acp.ErrNotAlive ||
		err == acp.ErrTurnActive || err == acp.ErrNoPending || err == acp.ErrNoTurn
}

type codexManager struct{ *codex.Manager }

func (m codexManager) Pending(id string) (string, []PermOption, bool) {
	title, opts, ok := m.Manager.Pending(id)
	out := make([]PermOption, len(opts))
	for i, o := range opts {
		out[i] = PermOption{Key: o.Key, Name: o.Name}
	}
	return title, out, ok
}

func (m codexManager) Conflict(err error) bool {
	return err == codex.ErrNoSession || err == codex.ErrNotAlive ||
		err == codex.ErrTurnActive || err == codex.ErrNoPending || err == codex.ErrNoTurn
}

// proc returns the structured-protocol manager for a node, or nil for a tmux
// (claude) node. It is the single dispatch point that lets the HTTP/poll paths
// treat ACP and codex-app-server nodes identically.
func (a *app) proc(n *Node) procManager {
	switch n.transport() {
	case "acp":
		return a.acp
	case "codex":
		return a.codex
	}
	return nil
}

// ---------- store ----------

// jsonBodyMax bounds every JSON request body (prompts included; 1 MiB is
// generous). -addr may be bound wider than loopback, so unbounded decodes
// would be an easy memory-exhaustion hole (R18.6). /api/ui has its own,
// larger uiStateMax limit.
const jsonBodyMax = 1 << 20

// decodeJSON decodes a bounded JSON request body.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyMax)
	return json.NewDecoder(r.Body).Decode(v)
}

func (a *app) appendRecord(rec storeRecord) error {
	// 0600: the store holds prompts, working dirs, transcript paths, and
	// remote-key pane excerpts. Match the ACP log and ui.json rather than
	// relying on the startup chmod of the parent dir (finding 63).
	f, err := os.OpenFile(a.storePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
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
	// node record. A delete record removes the node from the visible registry;
	// a later node record with the same ID would reintroduce it.
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
			if rec.Node.Description == "" {
				rec.Node.Description = rec.Node.Prompt
			}
			if existing, ok := a.byID[rec.Node.ID]; ok {
				*existing = *rec.Node
			} else {
				a.nodes = append(a.nodes, rec.Node)
				a.byID[rec.Node.ID] = rec.Node
			}
		case rec.Type == "transcript":
			transcripts[rec.ID] = rec.Path
		case rec.Type == "delete":
			a.removeNodeLocked(rec.ID)
			delete(transcripts, rec.ID)
		}
	}
	for id, path := range transcripts {
		if n, ok := a.byID[id]; ok {
			n.Transcript = path
		}
	}
	return nil
}

func (a *app) removeNodeLocked(id string) {
	delete(a.byID, id)
	for i, n := range a.nodes {
		if n.ID == id {
			a.nodes = append(a.nodes[:i], a.nodes[i+1:]...)
			break
		}
	}
	delete(a.live, id)
	delete(a.attn, id)
	delete(a.prevCap, id)
	delete(a.lastChg, id)
	delete(a.activeSince, id)
	delete(a.tailers, id)
	delete(a.mirrors, id)
	delete(a.chatMark, id)
	delete(a.staleChat, id)
	delete(a.sendState, id)
	delete(a.segCache, id)
	delete(a.anim, id)
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
// with the same slug cannot make new-session fail. A session log left on disk
// under the slug also counts as taken: it means a dead node's archive rename
// failed, and reissuing the slug would append new history onto dead history.
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
		if _, used := a.byID[id]; !used && !taken[id] && !a.reserved[id] && !a.sessionLogExists(id) {
			return id
		}
		id = fmt.Sprintf("%s-%d", slug, i)
	}
}

func (a *app) sessionLogExists(id string) bool {
	if a.sessionsDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(a.sessionsDir, id+".jsonl"))
	return err == nil
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
	// pi and opencode reach agentCommand only as a legacy/forced tmux fallback
	// (new pi/opencode nodes resolve to the ACP transport); both take the
	// "provider/model" form their own list commands emit.
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
		if n.Model == "" {
			n.Model = p.Model
		}
		if n.Effort == "" {
			n.Effort = p.Effort
		}
		if n.Dir == "" {
			n.Dir = p.Dir
		}
		if n.LaneID == "" {
			n.LaneID = p.LaneID
		}
		// A fork inherits the parent's transport (fresh context, same
		// mechanism). Use the migrated value, not the raw field: an old or
		// adopted pi/opencode parent with an empty Transport means tmux, so the
		// child must resolve to tmux too rather than falling through to the
		// agent-derived ACP default below (finding 54). Derivation only fires
		// when neither the request nor a parent pinned a transport.
		if n.Transport == "" {
			n.Transport = p.transport()
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
	// New nodes pick a transport by agent: pi/opencode over ACP, codex over its
	// app-server bridge, claude over tmux. Only set this on creation — stored
	// records with an absent Transport are migrated to tmux by Node.transport,
	// never rewritten here.
	if n.Transport == "" {
		switch n.Agent {
		case "pi", "opencode":
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
		n.SessionID = newUUID()
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
	if err == nil {
		if _, dup := a.byID[n.ID]; dup {
			collided = true
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
		// Negate the just-persisted node record. Append-only store: the delete
		// record is the only correction that prevents this launch's dead config
		// from clobbering the collision winner's record at replay (both share
		// the id, and the later node record would win). The winner's in-memory
		// node stays; its supervisor can re-adopt after a restart if needed.
		_ = a.appendRecord(storeRecord{Type: "delete", ID: n.ID, Time: time.Now().UTC().Format(time.RFC3339)})
		a.archiveSessionLog(n.ID)
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
// for pi/opencode, codex app-server for codex) and persists the node record.
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
	cmd, err := agentCommand(n)
	if err != nil {
		return 400, err
	}
	if _, err := a.server.NewSession(n.ID, n.Dir, cmd); err != nil {
		return 500, err
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
		// Structured-protocol nodes (ACP, codex app-server) carry no tmux pane:
		// liveness and needs-input come from the manager's structured state
		// (process alive, turn in flight, pending permission), not pane-change
		// detection. No capture, no transcript discovery.
		if pm := a.proc(n); pm != nil {
			a.mu.Lock()
			a.live[n.ID] = pm.Live(n.ID)
			a.attn[n.ID] = pm.Attention(n.ID)
			if a.live[n.ID] == "active" {
				a.lastChg[n.ID] = time.Now()
			}
			a.mu.Unlock()
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
				}
			}
		}
		a.mu.Lock()
		a.live[n.ID] = state
		a.attn[n.ID] = attn
		a.mu.Unlock()
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
	Attention     string `json:"attention,omitempty"` // "approval" | "question" | "inspect" (quiet, no structured evidence — look at the terminal)
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
		// Structured-protocol nodes (ACP, codex) keep their history in the
		// manager's session log rather than a linked transcript file, so they
		// always have chat to show.
		hasTranscript := n.Transcript != "" || a.proc(n) != nil
		views = append(views, nodeView{Node: &nc, Live: a.live[n.ID], Attention: a.attn[n.ID],
			HasTranscript: hasTranscript, LastActivity: lastMS})
	}
	// Sessions on our socket that no node accounts for: candidates for
	// adoption (manually created, or migrated from another tmux server). A
	// session whose id is reserved by an in-flight create is already spoken
	// for (R20.2) — offering it for adoption would race the publish.
	unadopted := []string{}
	for _, s := range sessions {
		if _, known := a.byID[s]; !known && !a.reserved[s] {
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
		Session     string `json:"session"`
		Title       string `json:"title"`
		Prompt      string `json:"prompt"`
		Description string `json:"description"`
		Agent       string `json:"agent"`
		Model       string `json:"model"`
		Dir         string `json:"dir"`
		SessionID   string `json:"session_id"`
		Transcript  string `json:"transcript"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.Session == "" {
		http.Error(w, "bad request: need session", 400)
		return
	}
	// The tmux session name becomes the node id (and its session-log
	// filename): hold it to the same rules NewSession applies to created
	// nodes. tmux itself already rejects '.' and ':', so this is defense in
	// depth against names that would misbehave as ids (e.g. containing '/').
	if !tmuxsession.ValidName(body.Session) {
		http.Error(w, fmt.Sprintf("session name %q is not adoptable as a node id", body.Session), 400)
		return
	}
	agent := body.Agent
	if agent == "" {
		agent = "claude"
	}
	// Codex now uses the app-server protocol: scimux starts the subprocess
	// itself via POST /api/nodes. Reject before the tmux Alive check so any
	// agent:"codex" request gets the clear explanation regardless of whether
	// the named session exists.
	if agent == "codex" {
		http.Error(w, "codex uses the app-server protocol; use POST /api/nodes with agent:\"codex\" to create a new activity", 400)
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
	// The same taken-checks uniqueID applies to created ids (R20.2, R20.4):
	// an id reserved by an in-flight create must not be adopted out from under
	// the launch, and a leftover session log under this slug is a dead node's
	// history — adopting onto it would bind the mirror to foreign turns under
	// the dead node's identity.
	if a.reserved[body.Session] {
		http.Error(w, fmt.Sprintf("a node %q is being created right now; retry or pick another session name", body.Session), 409)
		return
	}
	if a.sessionLogExists(body.Session) {
		http.Error(w, fmt.Sprintf("session log %s.jsonl already holds a dead node's history; move it out of the sessions directory (or into sessions/archive/) before adopting this name", body.Session), 409)
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
	n := &Node{ID: body.Session, Title: title, Prompt: body.Prompt, Agent: agent,
		Model: body.Model, Dir: dir, SessionID: body.SessionID, Transcript: body.Transcript,
		Description: body.Description, Adopted: true, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if n.Description == "" {
		n.Description = n.Prompt
	}
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
	// Any known claude session id — supplied explicitly (the migration script
	// sends one) or extracted from the pane above — gets its deterministic
	// lookup now (the id is the transcript filename). If the file is not visible
	// yet, discoverTranscript retries the same lookup.
	if n.Agent == "claude" && n.SessionID != "" && n.Transcript == "" {
		if path, ok := transcript.FindClaudeTranscript(a.home, n.SessionID); ok {
			n.Transcript = path
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
	if err := decodeJSON(w, r, &n); err != nil {
		http.Error(w, "bad request: "+err.Error(), 400)
		return
	}
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
	// slow); the ID allocator must avoid unadopted sessions.
	taken := map[string]bool{}
	for _, s := range a.server.Sessions() {
		taken[s] = true
	}
	// createNode manages a.mu itself: the id reservation happens under the
	// lock, the external launch outside it.
	status, err = a.createNode(&n, taken)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, n)
}

func (a *app) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Title       *string   `json:"title"`
		Description *string   `json:"description"`
		LaneID      *string   `json:"lane_id"`
		AlsoLanes   *[]string `json:"also_lanes"`
		Service     *string   `json:"service"`
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
	if body.AlsoLanes != nil {
		// The home lane never repeats in the interchange list, and the list
		// stays deduplicated; the full list replaces the old one (the client
		// sends the complete set on every add/remove).
		seen := map[string]bool{}
		var also []string
		for _, l := range *body.AlsoLanes {
			l = strings.TrimSpace(l)
			if l == "" || l == next.LaneID || seen[l] {
				continue
			}
			seen[l] = true
			also = append(also, l)
		}
		next.AlsoLanes = also
	}
	if body.Service != nil {
		switch svc := strings.TrimSpace(*body.Service); svc {
		case "", "through":
			next.Service = "" // through service is the unmarked default
		case "siding", "terminus":
			next.Service = svc
		default:
			a.mu.Unlock()
			http.Error(w, fmt.Sprintf("unknown service %q (want through, siding or terminus)", svc), 400)
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

func (a *app) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if pm := a.proc(n); pm != nil {
		if pm.HasSession(n.ID) {
			if err := pm.Kill(n.ID); err != nil {
				http.Error(w, "close session: "+err.Error(), 500)
				return
			}
		}
	} else if !n.Adopted {
		s := a.server.Session(n.ID)
		if s.Alive() {
			if err := s.Kill(); err != nil {
				http.Error(w, "close session: "+err.Error(), 500)
				return
			}
		}
	}
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
	a.removeNodeLocked(n.ID)
	a.mu.Unlock()
	a.archiveSessionLog(n.ID)
	writeJSON(w, map[string]string{"ok": "deleted"})
}

// archiveSessionLog moves a deleted node's session log into sessions/archive/
// with a timestamp suffix. Node IDs are friendly slugs that uniqueID can
// reissue once the node is gone; without this move a future node reusing the
// slug would append onto the dead node's history. Best-effort: history
// retention must never block a delete.
func (a *app) archiveSessionLog(id string) {
	src := filepath.Join(a.sessionsDir, id+".jsonl")
	if _, err := os.Stat(src); err != nil {
		return
	}
	dir := filepath.Join(a.sessionsDir, "archive")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: archive session log for %s: %v\n", id, err)
		return
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(src, filepath.Join(dir, id+"."+stamp+".jsonl")); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: archive session log for %s: %v\n", id, err)
	}
}

func (a *app) node(r *http.Request) (*Node, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, ok := a.byID[r.PathValue("id")]
	return n, ok
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
	var body struct{ Text string }
	if err := decodeJSON(w, r, &body); err != nil || strings.TrimSpace(body.Text) == "" {
		http.Error(w, "bad request", 400)
		return
	}
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
		if err := pm.Send(n.ID, body.Text); err != nil {
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
	acked, err := a.server.Session(n.ID).SendAck(body.Text)
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
		logPath := filepath.Join(a.sessionsDir, n.ID+".jsonl")
		if _, err := os.Stat(logPath); err == nil {
			w := &sessionlog.Writer{Path: logPath}
			if err := w.Append(sessionlog.NewSource("", "")); err != nil {
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
	seg := a.segment(n)
	a.mu.Lock()
	var lastMS int64
	if t, ok := a.lastChg[n.ID]; ok {
		lastMS = t.UnixMilli()
	}
	a.mu.Unlock()
	resp := map[string]any{
		"turns": seg.Turns, "last_change": lastMS,
		"chat_started": seg.StartTime, "prior_turns": seg.PriorTurns,
	}
	if pm := a.proc(n); pm != nil {
		a.procChatInto(resp, n, pm, seg)
	} else {
		a.tmuxChatInto(resp, n, seg)
	}
	writeJSON(w, resp)
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
	// the context it measures. Claude transcripts report usage but never the
	// window size; estimate it from the model name (the "[1m]" marker is the
	// long-context variant).
	ctxUsed, ctxWindow := seg.Used, seg.Size
	if ctxUsed > 0 && ctxWindow == 0 {
		if strings.Contains(model, "[1m]") {
			ctxWindow = 1_000_000
		} else {
			ctxWindow = 200_000
		}
	}
	var ctxPct int
	if ctxWindow > 0 {
		ctxPct = int(100 * ctxUsed / ctxWindow)
		if ctxPct > 100 {
			ctxPct = 100
		}
	}
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
	var ctxPct int
	if seg.Size > 0 {
		ctxPct = int(100 * seg.Used / seg.Size)
		if ctxPct > 100 {
			ctxPct = 100
		}
	}
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
// keeps owning clearing. Unlike the poller's tick path this is one-shot and
// human-triggered, so it skips the confined-animation gate.
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
	visible, err := s.CaptureVisible()
	if err != nil || !dialoghint.ClassifyVisible(visible) {
		return
	}
	a.mu.Lock()
	if a.attn[n.ID] == "" {
		a.attn[n.ID] = attentionKind(name)
	}
	a.mu.Unlock()
}

// ---------- UI state ----------

// The web client owns a small blob of cross-device state — map group tabs,
// the archived-card set, private notes — that must survive scimux restarts,
// so it lives next to the node store instead of in localStorage (per-device
// state like drafts stays client-side). The blob is opaque JSON to the
// server: its shape belongs to the client.
//
// Writes are revisioned, not last-writer-wins: every GET carries an ETag
// derived from the stored bytes, every PUT must name the revision it was
// based on (If-Match), and a stale base gets 409 — the client refetches,
// replays its local operations on the fresh document, and retries. That is
// what lets two of the supervisor's devices add notes concurrently without
// silently erasing each other. Clients poll GET with If-None-Match (304) on
// the same cadence as /api/state.
const uiStateMax = 1 << 20

// uiETag derives the revision tag from the canonical stored bytes.
func uiETag(b []byte) string {
	h := fnv.New64a()
	h.Write(b)
	return fmt.Sprintf(`"%x"`, h.Sum64())
}

// readUILocked returns the stored UI document, "{}" when none exists yet.
// Any error other than not-exist is a real storage failure the client must
// see — reporting it as an empty document would invite the next mutation to
// overwrite whatever the unreadable file still holds. Callers hold a.mu.
func (a *app) readUILocked() ([]byte, error) {
	b, err := os.ReadFile(a.uiPath)
	if os.IsNotExist(err) {
		return []byte("{}"), nil
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("stored ui state is not valid JSON")
	}
	return b, nil
}

func (a *app) handleUIGet(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	b, err := a.readUILocked()
	a.mu.Unlock()
	if err != nil {
		http.Error(w, "read ui state: "+err.Error(), 500)
		return
	}
	etag := uiETag(b)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
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
	match := r.Header.Get("If-Match")
	if match == "" {
		http.Error(w, "ui writes require If-Match (use * to bootstrap)", 428)
		return
	}
	// Atomic replace under the lock: a crash mid-write must never leave a
	// truncated file, concurrent PUTs must not interleave tmp files, and the
	// revision check must be atomic with the write it guards.
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, err := a.readUILocked()
	if err != nil {
		http.Error(w, "read ui state: "+err.Error(), 500)
		return
	}
	if match != "*" && match != uiETag(cur) {
		http.Error(w, "ui state changed since this revision was read", 409)
		return
	}
	// Private notes live here: owner-only permissions.
	tmp := a.uiPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := os.Rename(tmp, a.uiPath); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("ETag", uiETag(b))
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
	flag.Parse()

	// The data directory holds private notes and pane-excerpt evidence:
	// owner-only. Tighten pre-existing broader modes where we can.
	if err := os.MkdirAll(*data, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	os.Chmod(*data, 0o700)
	os.Chmod(filepath.Join(*data, "ui.json"), 0o600)
	os.Chmod(filepath.Join(*data, "nodes.jsonl"), 0o600)
	sessionsDir := filepath.Join(*data, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	a := &app{
		byID:        map[string]*Node{},
		live:        map[string]string{},
		attn:        map[string]string{},
		prevCap:     map[string]string{},
		lastChg:     map[string]time.Time{},
		activeSince: map[string]time.Time{},
		tailers:     map[string]*transcript.Tailer{},
		mirrors:     map[string]*mirror{},
		pathClaims:  map[string]bool{},
		chatMark:    map[string]chatMark{},
		staleChat:   map[string]bool{},
		sendState:   map[string]string{},
		reserved:    map[string]bool{},
		anim:        map[string]*animState{},
		server:      tmuxsession.NewServer(*socket),
		acp:         acpManager{acp.NewManager(sessionsDir)},
		codex:       codexManager{codex.NewManager(sessionsDir)},
		storePath:   filepath.Join(*data, "nodes.jsonl"),
		uiPath:      filepath.Join(*data, "ui.json"),
		sessionsDir: sessionsDir,
		home:        home,
	}
	if err := a.loadStore(); err != nil {
		fmt.Fprintln(os.Stderr, "scimux: load store:", err)
		os.Exit(1)
	}

	// Structured-protocol subprocesses (ACP, codex app-server) are ours: unlike
	// tmux sessions (which deliberately survive scimux exit), they must not
	// orphan. Kill every such process group on shutdown. tmux sessions are
	// untouched.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		a.acp.Shutdown()
		a.codex.Shutdown()
		os.Exit(0)
	}()

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
	// The assets tree is compiled in; a Sub failure means the embed layout
	// changed and must fail loudly at startup, not silently drop the route.
	assets, err := fs.Sub(webFS, "web/assets")
	if err != nil {
		panic("embedded web/assets missing: " + err.Error())
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
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
	mux.HandleFunc("PATCH /api/nodes/{id}", a.handleUpdateNode)
	mux.HandleFunc("DELETE /api/nodes/{id}", a.handleDeleteNode)
	mux.HandleFunc("POST /api/adopt", a.handleAdopt)
	mux.HandleFunc("POST /api/nodes/{id}/send", a.handleSend)
	mux.HandleFunc("POST /api/nodes/{id}/send/resolve", a.handleSendResolve)
	mux.HandleFunc("POST /api/nodes/{id}/send/interrupt", a.handleSendInterrupt)
	mux.HandleFunc("POST /api/nodes/{id}/key", a.handleKey)
	mux.HandleFunc("GET /api/nodes/{id}/chat", a.handleChat)
	mux.HandleFunc("GET /api/nodes/{id}/peek", a.handlePeek)
	mux.HandleFunc("GET /api/agents", a.handleAgents)
	mux.HandleFunc("GET /api/ui", a.handleUIGet)
	mux.HandleFunc("PUT /api/ui", a.handleUIPut)
	mux.HandleFunc("GET /api/update/check", handleUpdateCheck)
	mux.HandleFunc("POST /api/update", a.handleUpdateApply)
	mux.HandleFunc("GET /api/licenses", handleLicenses)

	fmt.Printf("scimux: http://%s/  (tmux socket %q, store %s)\n", *addr, *socket, a.storePath)
	fmt.Printf("scimux: attach to a chat by hand: tmux -L %s attach -t <node-id>\n", *socket)
	// -addr may be bound wider than loopback, so give the server real
	// timeouts (slowloris defense). No ReadTimeout/WriteTimeout: legitimate
	// handlers can be slow (structured sends, the self-update download);
	// ReadHeaderTimeout covers the attack that matters.
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
}
