package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp"
	"codeberg.org/chrberger/scimux/internal/acp/codex"
	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/notestore"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/tmuxsession"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

type Config struct {
	Home        string
	DataDir     string
	Socket      string
	LaunchGrace time.Duration
	LaunchPoll  time.Duration
}

type appDeps struct {
	Server *tmuxsession.Server
}

func NewApp(cfg Config) (*app, error) {
	return newApp(cfg, appDeps{})
}

func newApp(cfg Config, deps appDeps) (*app, error) {
	if cfg.Home == "" {
		return nil, errors.New("home is required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("data dir is required")
	}
	socket := cfg.Socket
	if socket == "" {
		socket = "scimux"
	}
	launchGrace := cfg.LaunchGrace
	if launchGrace == 0 {
		launchGrace = 2500 * time.Millisecond
	}
	launchPoll := cfg.LaunchPoll
	if launchPoll == 0 {
		launchPoll = 250 * time.Millisecond
	}
	server := deps.Server
	if server == nil {
		server = tmuxsession.NewServer(socket)
	}
	sessionsDir := filepath.Join(cfg.DataDir, "sessions")
	notesDir := filepath.Join(cfg.DataDir, "notes")
	a := &app{
		server:          server,
		launchGrace:     launchGrace,
		launchPoll:      launchPoll,
		claudeCachePath: filepath.Join(cfg.DataDir, "claude-models.json"),
		acp:             acpManager{acp.NewManager(sessionsDir)},
		codex:           codexManager{codex.NewManager(sessionsDir)},
		storePath:       filepath.Join(cfg.DataDir, "nodes.jsonl"),
		uiPath:          filepath.Join(cfg.DataDir, "ui.json"),
		sessionsDir:     sessionsDir,
		attachmentsDir:  filepath.Join(cfg.DataDir, "attachments"),
		assetsDir:       filepath.Join(cfg.DataDir, "assets"),
		assetHook:       nil,
		notes:           notestore.New(notesDir),
		home:            cfg.Home,
	}
	// Map fields live here so production and poll-reaching test fixtures share
	// one initializer; initMaps never overwrites a map the caller already set.
	a.initMaps()
	a.assetHook = a.ingestAssetHook
	a.acp.SetAssetHook(a.assetHook)
	a.codex.SetAssetHook(a.assetHook)
	a.usage = newUsageCache(a.collectUsage)
	if err := a.loadStore(); err != nil {
		return nil, fmt.Errorf("load store: %w", err)
	}
	return a, nil
}

// initMaps fills any nil map fields with empty maps. Production newApp calls
// it once; tests that build *app literals and reach poll() call it after
// setting explicit fixture values so those values are kept and only the maps
// the test did not care about are filled. Do not reintroduce nil-map guards
// in poller.go for test convenience.
func (a *app) initMaps() {
	if a.byID == nil {
		a.byID = map[string]*Node{}
	}
	if a.live == nil {
		a.live = map[string]string{}
	}
	if a.attn == nil {
		a.attn = map[string]string{}
	}
	if a.turnDone == nil {
		a.turnDone = map[string]bool{}
	}
	if a.attnAt == nil {
		a.attnAt = map[string]time.Time{}
	}
	if a.prevCap == nil {
		a.prevCap = map[string]string{}
	}
	if a.lastChg == nil {
		a.lastChg = map[string]time.Time{}
	}
	if a.activeSince == nil {
		a.activeSince = map[string]time.Time{}
	}
	if a.tailers == nil {
		a.tailers = map[string]*transcript.Tailer{}
	}
	if a.mirrors == nil {
		a.mirrors = map[string]*mirror{}
	}
	if a.piMirrors == nil {
		a.piMirrors = map[string]*piFareMirror{}
	}
	if a.pathClaims == nil {
		a.pathClaims = map[string]bool{}
	}
	if a.chatMark == nil {
		a.chatMark = map[string]chatMark{}
	}
	if a.staleChat == nil {
		a.staleChat = map[string]bool{}
	}
	if a.sendState == nil {
		a.sendState = map[string]string{}
	}
	if a.reserved == nil {
		a.reserved = map[string]bool{}
	}
	if a.anim == nil {
		a.anim = map[string]*animState{}
	}
	if a.deadTranscripts == nil {
		a.deadTranscripts = map[string]map[string]bool{}
	}
	if a.claudeIDs == nil {
		a.claudeIDs = map[string]string{}
	}
	if a.logCache == nil {
		a.logCache = map[string]*sessionlog.LogCache{}
	}
	if a.autoApprove == nil {
		a.autoApprove = map[string]*autoApproveState{}
	}
}

// ---------- model ----------

type Node struct {
	ID          string `json:"id"`
	Parent      string `json:"parent,omitempty"`
	Title       string `json:"title"`
	Prompt      string `json:"prompt"`                // first prompt == the node's research question
	Description string `json:"description,omitempty"` // card/map description; defaults to Prompt
	Rationale   string `json:"rationale,omitempty"`   // why this fork exists (decision evidence)
	LaneID      string `json:"lane_id,omitempty"`     // immutable topic/lane assignment; empty means unassigned
	// ForkKind records what a fork did relative to its destination lane, fixed
	// at creation ("y-stay" | "y-new" | "s"; empty for a root). It is stored
	// rather than recomputed so a later sibling deletion cannot silently
	// reclassify the fork on the map (a live recomputation flips S→Y-new when
	// the older station that made it a crossover is removed). Empty on records
	// that predate the field; the client falls back to the historical
	// computation for those.
	ForkKind string `json:"fork_kind,omitempty"`
	// EndedAt marks a thread the user deliberately closed via /exit (the "ended"
	// head state): a dead-end ⊣ cap on the map, kept visible and rideable. This
	// is a scimux decision, distinct from a *mechanical* process exit — a crash
	// leaves the node live until /exit is invoked. Empty means not ended.
	EndedAt    string `json:"ended_at,omitempty"`
	Agent      string `json:"agent"` // "claude" | "codex" | "pi" | "opencode" | "grok"
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"` // reasoning effort: claude/grok launch flag or codex thread config
	Dir        string `json:"dir"`
	SessionID  string `json:"session_id,omitempty"` // claude: session uuid (minted by us); codex: thread id from thread/start; ACP: session id
	Transcript string `json:"transcript,omitempty"`
	Adopted    bool   `json:"adopted,omitempty"` // adopted tmux sessions are never killed by scimux
	// AXScreenReader is true when scimux launched this Claude process with
	// --ax-screen-reader. It is server-owned launch metadata: clients must not
	// set it, adoption always leaves it false, and older store records that
	// lack the field replay as false. It controls tmux menu-key translation
	// (choice+Enter for numbered/y/n dialogs) and quiet-branch attention
	// corroboration under the flatter AX renderer (unresolved tools need a
	// visible dialog; Owing() alone never raises inspect). It must not affect
	// liveness or transcript parsing.
	AXScreenReader bool `json:"ax_screen_reader,omitempty"`
	// Transport selects the supervision mechanism: "tmux" (TUI + pane peek +
	// transcript files, the original path — claude), "acp" (an Agent Client
	// Protocol subprocess, pi/opencode/grok) or "codex" (codex's app-server protocol
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

type app struct {
	mu    sync.Mutex
	nodes []*Node
	byID  map[string]*Node
	live  map[string]string // node id -> "active"|"quiet"|"exited"
	attn  map[string]string // node id -> ""|"approval"|"question"|"inspect"
	// turnDone: quiet + newest transcript record is assistant + no pending
	// tool call + no attention classified (P5 item 10). Projected as
	// turn_done on nodeView. Never set for ACP nodes (no Tailer) — absent
	// means UNKNOWN, not "not finished". Never feeds liveness.
	turnDone map[string]bool
	// attnAt: when a.attn[id] was last set by *fresh* evidence (a classification
	// this tick, or the one-shot peek path) rather than carried over. The
	// active-branch preserve path (R20.5) keeps attention while the corroborated
	// check is indeterminate, but only while it is younger than animStallAfter —
	// otherwise a human answer given in the terminal (which touches nothing) would
	// leave the card claiming "approval" for the whole runtime of the approved
	// tool, since the tool-call record stays unresolved until the tool completes
	// (R21.2). A web-key answer clears attn outright in handleKey.
	attnAt  map[string]time.Time
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
	// piMirrors: per-node pi native JSONL → session-log usage projection
	// (pi_fare.go / fare-design Phase 4). Separate from mirrors so tmux and
	// pi native state never collide on the same node id.
	piMirrors map[string]*piFareMirror
	// piMapPath overrides the pi-acp session-map.json path (tests). Empty
	// means <home>/.pi/pi-acp/session-map.json.
	piMapPath string
	// piModelsPath overrides pi's models.json path (tests). Empty means
	// <home>/.pi/agent/models.json. Used for the occupancy tank Size
	// (Phase 4a); never for cost rates (D8).
	piModelsPath string
	// piModels is the mtime-cached id→contextWindow view of models.json.
	// Lazily created by piContextWindow; not shared across apps.
	piModels *transcript.PiModels
	// pathClaims: transcript paths reserved by an in-flight discovery store
	// write, so a concurrent adoption cannot publish the same path.
	pathClaims map[string]bool
	// deadTranscripts: per-node set of retired transcript paths and session
	// ids (P2a). A /clear tombstones the old link so maybeRelinkTranscript /
	// discoverTranscript cannot rebind the pre-clear file from a pane cmdline
	// or a late mtime touch. Per-node, never global — another node may own
	// the same path. Replay of "transcript-retired" records rebuilds this;
	// a delete drops the node's set with the node.
	deadTranscripts map[string]map[string]bool
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

	// storeMu serializes every append to nodes.jsonl, independent of a.mu (some
	// callers hold a.mu, some do not). It gives the append-only store one
	// process-local write point so concurrent audit/transcript/delete records
	// cannot interleave a partial line, and pairs each append with an fsync —
	// these records are the durable truth the supervisor replays and audits.
	storeMu sync.Mutex
	// uiMu serializes the read-modify-write of the UI-state file (ui.json),
	// independent of a.mu. The revision check plus the atomic tmp-write+rename
	// must be one critical section, but they are pure file I/O over a private
	// document — holding the app-wide a.mu across two syscalls would stall the
	// poller and every other handler for a write that touches no shared state.
	uiMu   sync.Mutex
	server *tmuxsession.Server
	acp    acpManager
	codex  codexManager
	// launchGrace bounds how long a freshly-launched tmux agent is watched for
	// an immediate failure (a rejected --model, a bad flag). The launch is
	// wrapped so such a process leaves its error on the pane (wrapLaunch); within
	// this window awaitLaunch reads it and reports it to the create caller instead
	// of persisting an unexplained dead node. Zero disables the check. launchPoll
	// is the capture interval inside that window.
	launchGrace time.Duration
	launchPoll  time.Duration
	// claudeIDs maps the family alias the UI offers (opus/sonnet/haiku/fable) to
	// the concrete model id the installed claude CLI actually accepts, probed once
	// at startup (probeClaudeModels) because the CLI mis-resolves its own aliases.
	// Empty until the probe returns, and empty forever if claude is absent or the
	// probe fails — in which case launches fall back to passing the bare alias.
	// Written once by the startup goroutine, read per launch; guarded by claudeMu.
	claudeIDs       map[string]string
	claudeMu        sync.Mutex
	claudeCachePath string // ~/.scimux/claude-models.json; empty disables caching
	storePath       string
	uiPath          string
	// sessionsDir is the unified session-log store: one JSONL file per node,
	// every transport, one schema (internal/sessionlog). Future readers
	// (search, consolidation, sharing) scan this one directory. It is also
	// the chat read path for every transport: handleChat renders from the
	// log, never from the transcript tailer (phase 3 of the consolidation).
	sessionsDir string
	// attachmentsDir holds uploaded files, one subdirectory per node
	// (~/.scimux/attachments/<node-id>/). Bytes live here; the session log and
	// nodes.jsonl only ever record a text reference (Attachment) — the
	// text-only/append-only store invariants forbid binary in either JSONL.
	attachmentsDir string
	// assetsDir holds blob-storage session assets, one subdirectory per node
	// (~/.scimux/assets/<node-id>/, internal/asset.WriteBlob/ResolveBlobPath).
	// The session log stays the index of record — every blob has an "asset"
	// event carrying id/name/mime/sha256/blobPath — this directory holds only
	// the bytes for assets too large to inline. See upload-design.md.
	assetsDir string
	// assetHook is the Phase 4 turn-append ingestion hook, called by every
	// transport (mirror.go for tmux, a.acp/a.codex for ACP/codex — wired via
	// SetAssetHook at startup) with each turn's scanned local-path candidates.
	// See asset.IngestFunc and ingestAssetHook (agent_asset.go).
	assetHook asset.IngestFunc
	// logCache memoizes each node's poll-path session-log products (segment,
	// fare+rides, anchored assets, asset index) under one (path, size, mtime)
	// key. An unchanged log costs a stat; growth resumes from the last
	// newline watermark and parses only the tail. Never holds history segments.
	logCache map[string]*sessionlog.LogCache
	// autoApprove is the in-memory per-node one-turn auto-approval lease
	// (P3). Never persisted — restart is always off. New/forked node IDs
	// start off because no entry exists. Protected by a.mu; never hold a.mu
	// across manager calls or session-log I/O.
	autoApprove map[string]*autoApproveState
	// autoGate serializes automatic decision commitment against enable,
	// disable, and re-arm per node. Once disable/re-arm returns, no decision
	// belonging to the prior lease may subsequently deliver. Held across
	// prepare/audit/deliver on the decision path (narrow per-node lock —
	// never a.mu across those ops). autoGateMu protects the map only.
	autoGateMu sync.Mutex
	autoGate   map[string]*sync.Mutex
	// testProc, when non-nil, is returned by proc() instead of the real ACP
	// or Codex manager. Tests only — never set in production.
	testProc procManager
	// deleteGateHook, when non-nil, runs inside finalizeDeleteWithAutoBarrier
	// after the auto-gate is held and the lease is cleared, before manager
	// teardown. Tests only — used to race enable/decision against deletion.
	deleteGateHook func(id string)
	// acceptArmHook, when non-nil, runs inside acceptStructuredPrompt after
	// primed→armed while the auto-gate is still held. Tests only.
	acceptArmHook func(id string)
	// notes is the synthesis-document store (~/.scimux/notes/, one mutable
	// JSON file per note — internal/notestore). Deliberately separate from the
	// append-only session log: notes are documents, not an event stream (see
	// notes-design.md "Storage Model"). noteMu serializes the handler-level
	// read-modify-write so a section autosave and a rename cannot clobber each
	// other's untouched fields; the store's own writes are atomic per file.
	notes  *notestore.Store
	noteMu sync.Mutex
	home   string
	// usage caches subscription-budget snapshots (usage.go). Its own mutex is
	// independent of a.mu — collectors do file/HTTP I/O (and a short-lived
	// grok ACP subprocess). Served cache-only via /api/usage; refreshed only
	// by successful Claude/Codex/Grok prompts.
	usage *usageCache
	// Provider source overrides for tests; empty means the real default path
	// (~/.codex/sessions, ~/.claude/.credentials.json, the OAuth endpoint,
	// live `grok agent stdio` for billing).
	codexSessionsDir string
	claudeCredsPath  string
	claudeUsageURL   string
	grokUsageOpts    acp.GrokBillingOptions
}

// PermOption is one answerable permission/decision choice surfaced to the UI:
// the key a supervisor presses, its human-readable name, and its role kind
// when known ("allow" | "allow_always" | "reject" | "reject_always" | "").
// Empty Kind means "unknown" — never an error, never a guess.
type PermOption struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

// PendingPermission is the UI-facing view of one outstanding approval on a
// structured-transport node. Empty ToolKind or Reason means "unknown".
// RequestID is an opaque stable identity for the current pending request.
type PendingPermission struct {
	RequestID string // opaque; stable while this request is pending
	Title     string
	ToolKind  string
	Reason    string // why the agent is asking; empty when unknown
	Options   []PermOption
}

// procManager is the shared surface of scimux's two structured-protocol
// transports: acp.Manager (pi/opencode/grok over the ACP SDK) and codex.Manager
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
	// PrepareResolve maps key against the pending request named by
	// expectedRequestID. A mismatch is a conflict (HTTP 409); the current
	// pending request is left untouched. Delivery still re-checks the
	// prepared token.
	PrepareResolve(nodeID, expectedRequestID, key string) (optID, evidence string, err error)
	Deliver(nodeID, optID string) error
	Pending(nodeID string) (PendingPermission, bool)
	// PermissionBoundary returns the session incarnation and highest issued
	// request sequence for auto-approve enable cutoffs. ok is false when
	// there is no live session.
	PermissionBoundary(nodeID string) (incarn string, maxSeq uint64, ok bool)
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

func (m acpManager) Pending(id string) (PendingPermission, bool) {
	p, ok := m.Manager.Pending(id)
	if !ok {
		return PendingPermission{}, false
	}
	out := make([]PermOption, len(p.Options))
	for i, o := range p.Options {
		out[i] = PermOption{Key: o.Key, Name: o.Name, Kind: o.Kind}
	}
	return PendingPermission{RequestID: p.RequestID, Title: p.Title, ToolKind: p.ToolKind, Reason: p.Reason, Options: out}, true
}

func (m acpManager) Conflict(err error) bool {
	return err == acp.ErrNoSession || err == acp.ErrNotAlive ||
		err == acp.ErrTurnActive || err == acp.ErrNoPending || err == acp.ErrNoTurn ||
		err == acp.ErrStalePermission
}

type codexManager struct{ *codex.Manager }

func (m codexManager) Pending(id string) (PendingPermission, bool) {
	p, ok := m.Manager.Pending(id)
	if !ok {
		return PendingPermission{}, false
	}
	out := make([]PermOption, len(p.Options))
	for i, o := range p.Options {
		out[i] = PermOption{Key: o.Key, Name: o.Name, Kind: o.Kind}
	}
	return PendingPermission{RequestID: p.RequestID, Title: p.Title, ToolKind: p.ToolKind, Reason: p.Reason, Options: out}, true
}

func (m codexManager) Conflict(err error) bool {
	return err == codex.ErrNoSession || err == codex.ErrNotAlive ||
		err == codex.ErrTurnActive || err == codex.ErrNoPending || err == codex.ErrNoTurn ||
		err == codex.ErrStalePermission
}

// proc returns the structured-protocol manager for a node, or nil for a tmux
// (claude) node. It is the single dispatch point that lets the HTTP/poll paths
// treat ACP and codex-app-server nodes identically.
//
// When testProc is non-nil (tests only), it replaces the real manager so HTTP
// handlers can be exercised against a stub without a live agent subprocess.
func (a *app) proc(n *Node) procManager {
	if a.testProc != nil {
		return a.testProc
	}
	switch n.transport() {
	case "acp":
		return a.acp
	case "codex":
		return a.codex
	}
	return nil
}
