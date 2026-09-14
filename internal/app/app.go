package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp"
	"codeberg.org/chrberger/scimux/internal/acp/codex"
	"codeberg.org/chrberger/scimux/internal/acp/muse"
	"codeberg.org/chrberger/scimux/internal/agentperm"
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
	Server               *tmuxsession.Server
	DeliverClaudeInitial func(*Node) initialDelivery
	// StorePath and ClaudeHooksDir isolate a Claude worker's private harness
	// journal while retaining the established shared hook-bundle location.
	// Production muxers leave both empty.
	StorePath       string
	ClaudeHooksDir  string
	SkipHookCleanup bool
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
		claudeProbeDir:  filepath.Join(cfg.DataDir, "probe"),
		acp:             acpManager{acp.NewManager(sessionsDir)},
		codex:           codexManager{codex.NewManager(sessionsDir)},
		muse:            museManager{muse.NewManager(sessionsDir)},
		storePath:       filepath.Join(cfg.DataDir, "nodes.jsonl"),
		uiPath:          filepath.Join(cfg.DataDir, "ui.json"),
		settingsPath:    filepath.Join(cfg.DataDir, "settings.json"),
		sessionsDir:     sessionsDir,
		attachmentsDir:  filepath.Join(cfg.DataDir, "attachments"),
		assetsDir:       filepath.Join(cfg.DataDir, "assets"),
		assetHook:       nil,
		notes:           notestore.New(notesDir),
		home:            cfg.Home,
	}
	if deps.StorePath != "" {
		a.storePath = deps.StorePath
	}
	a.claudeHooksRoot = deps.ClaudeHooksDir
	a.skipHookCleanup = deps.SkipHookCleanup
	// Owned Claude is the sole tmux transport whose first prompt is deferred:
	// Remote Control must finish bootstrapping before its editor is safe. Tests
	// inject an acknowledged result so unrelated lifecycle coverage need not
	// manufacture Claude's private transcript records.
	//
	// These three are deliberately fixed rather than Config fields, and the
	// asymmetry with LaunchGrace/LaunchPoll above is smaller than it looks:
	// those two are exported on Config but have no production caller either —
	// both pairs are test seams, differing only in which struct the seam sits
	// on. Promoting these would create an operator-facing knob for a deadline
	// whose correct value is a property of the Claude CLI's startup, not of
	// the user's preference, and whose only two failure modes are already
	// non-silent: too short surfaces an inline startup error with the prompt
	// preserved as a draft, too long delays that same error. Tests set the
	// fields directly (claude_strict_test.go), which is all the seam anyone
	// has needed.
	a.claudeReadyTimeout = 15 * time.Second
	a.claudeDeliveryTimeout = 15 * time.Second
	a.claudeDeliveryGiveUp = 180 * time.Second
	a.claudeInitialPoll = 100 * time.Millisecond
	if deps.DeliverClaudeInitial != nil {
		a.deliverClaudeInitial = deps.DeliverClaudeInitial
	} else {
		a.deliverClaudeInitial = a.deliverClaudeInitialPrompt
	}
	// Map fields live here so production and poll-reaching test fixtures share
	// one initializer; initMaps never overwrites a map the caller already set.
	a.initMaps()
	a.assetHook = a.ingestAssetHook
	a.acp.SetAssetHook(a.assetHook)
	a.codex.SetAssetHook(a.assetHook)
	a.muse.SetAssetHook(a.assetHook)
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
	if a.deletedNodes == nil {
		a.deletedNodes = map[string]bool{}
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
	if a.claudeHooks == nil {
		a.claudeHooks = map[string]string{}
	}
	if a.claudeGens == nil {
		a.claudeGens = map[string]int{}
	}
	if a.pendingClaudeHooks == nil {
		a.pendingClaudeHooks = map[string]string{}
	}
	if a.claudeBoundAt == nil {
		a.claudeBoundAt = map[string]time.Time{}
	}
	if a.claudePermCap == nil {
		a.claudePermCap = map[string]bool{}
	}
	if a.claudeAskedCap == nil {
		a.claudeAskedCap = map[string]bool{}
	}
	if a.claudeAskedOff == nil {
		a.claudeAskedOff = map[string]int64{}
	}
	if a.claudeAskedTools == nil {
		a.claudeAskedTools = map[string]int{}
	}
	if a.claudeAck == nil {
		a.claudeAck = map[string]bool{}
	}
	if a.claudeStartPending == nil {
		a.claudeStartPending = map[string]string{}
	}
	if a.claudeStrictCap == nil {
		a.claudeStrictCap = map[string]bool{}
	}
	if a.claudeLaunchErr == nil {
		a.claudeLaunchErr = map[string]string{}
	}
	if a.claudeTurns == nil {
		a.claudeTurns = map[string]claudeAcceptedTurn{}
	}
	if a.claudeClosing == nil {
		a.claudeClosing = map[string]claudeAcceptedTurn{}
	}
	if a.claudeDialogNote == nil {
		a.claudeDialogNote = map[string]string{}
	}
	if a.lastDeliver == nil {
		a.lastDeliver = map[string]time.Time{}
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
	Agent      string `json:"agent"` // "claude" | "codex" | "pi" | "opencode" | "grok" | "muse"
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
	// Protocol subprocess, pi/opencode/grok), "codex" (codex's app-server protocol
	// wrapped as a structured bridge), or "muse" (Muse subprocess over MSP).
	// "acp", "codex" and "muse" are structured subprocess transports (no pane);
	// see procManager. An absent value means tmux — every stored record that
	// predates this field migrates as "" == "tmux" (see transport). Empty is
	// never reinterpreted as muse.
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
	// tool call + no attention classified (P5 item 10). Structured agents
	// latch the same flag from the active→quiet edge (P2). Projected as
	// turn_done on nodeView. Absent means UNKNOWN, not "not finished".
	// Never feeds liveness.
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
	// ids (P2a). A /clear tombstones the old link so discoverTranscript and
	// a late hook event cannot rebind the pre-clear file. Per-node, never
	// global — another node may own the same path. Replay of
	// "transcript-retired" records rebuilds this; a delete drops the node's
	// set with the node.
	deadTranscripts map[string]map[string]bool
	// chatMark/staleChat: per-node transcript progress at the last
	// active→quiet pane transition, and whether the file has been growing
	// without recognizable agent-side records since (degrade the UI to peek).
	chatMark  map[string]chatMark
	staleChat map[string]bool
	// sendState: per-node web prompt delivery state, "submitting" while a send
	// is in flight, "unconfirmed" for an ambiguous ordinary send, and
	// "initial_unconfirmed" for the deferred Claude first prompt. Only that
	// initial state self-heals when its matching mirrored user turn arrives;
	// ordinary ambiguity stays manual. New sends are held (409) until the state
	// clears, so a delayed Enter can never stack a second prompt onto the first.
	sendState map[string]string
	// reserved: node ids claimed by an in-flight create whose external launch
	// runs outside a.mu; uniqueID must not reissue them, like pathClaims for
	// transcript paths.
	reserved map[string]bool
	// deletedNodes retains only the latest replayed lifecycle state. Startup
	// uses it to finish a deletion instead of resurrecting a worker whose muxer
	// died between the durable tombstone and the worker Stop.
	deletedNodes map[string]bool
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
	// claudeHooks maps a node id to its private hook capability id. It is not
	// a Node JSON field and must never appear on /api/state.
	claudeHooks map[string]string
	// claudeGens is the per-node Claude binding generation (claude-fixes.md D7).
	claudeGens map[string]int
	// pendingClaudeHooks holds a prepared hook id after launch and before the
	// hook record is persisted at publish time.
	pendingClaudeHooks map[string]string
	// claudePermCap maps a Claude hook bundle id to whether it carries the
	// PermissionRequest rendezvous. Rebuilt from disk at startup so support
	// survives a restart as a property of the pane's bundle, not of memory.
	claudePermCap map[string]bool
	// claudeAskedCap maps a Claude hook bundle id to whether it carries the
	// escalation-notice layout (claude_asked.go). Separate from
	// claudePermCap on purpose: answering and discriminating shipped in
	// different versions, and a bundle that proves only the former must keep
	// today's attention backstop rather than lose attention silently.
	claudeAskedCap map[string]bool
	// claudeAskedOff is the transcript watermark the notice retirement compares
	// against, per node: growth means the agent reached its own control flow
	// again, which for a waiting call cannot happen before a human answered.
	claudeAskedOff map[string]int64
	// claudeAskedTools is the per-node count of tool stamps already credited
	// against notices. The tailer accumulates stamps for the life of a
	// transcript, so without a watermark one approved call would go on
	// retiring a notice every tick.
	claudeAskedTools map[string]int
	// claudeBoundAt is when each node's current transcript link was
	// established. A link established after the delivery being judged cannot
	// have missed it (maybeRelinkTranscript).
	claudeBoundAt map[string]time.Time
	// claudeAck is set only after a valid SessionStart for that node has
	// been processed. A newly launched node is not hook-capable until then.
	claudeAck map[string]bool
	// claudeStartPending maps a node id to the session id of a SessionStart
	// that proved itself in every respect except one: the CLI has not created
	// the transcript file yet. It releases the first-prompt paste gate (the
	// paste is what creates the file) and nothing else — the binding, the
	// acknowledgement, and hook capability still wait for the real file.
	claudeStartPending map[string]string
	// claudeStrictCap maps a hook bundle id to whether it is the complete
	// current contract (SessionStart, PermissionRequest, Notification, Stop,
	// StopFailure, asked layout, runnable exec).
	claudeStrictCap map[string]bool
	// claudeLaunchErr is a durable-for-the-process inline launch/delivery
	// error for a Claude node. It is never a reason to open the terminal.
	claudeLaunchErr map[string]string
	// claudeClearSent is when a /clear was pasted, kept only until the
	// SessionStart source:"clear" that proves the page actually turned. It
	// exists to time the wait out, never to act on: the page turn itself is
	// bindClaudeClear's, on evidence. Losing it to a restart costs the
	// notice and nothing else, because the hook event is durable in the
	// bundle inbox and the binding it commits is the whole page turn.
	claudeClearSent map[string]time.Time
	// claudeTurns is the live accepted-turn nonce per Claude node. Stop and
	// Notification helpers capture it; drain matches it exactly.
	claudeTurns map[string]claudeAcceptedTurn
	// claudeClosing is the last closed accepted turn, kept so a second Stop
	// for that turn is idempotent and cannot match the next begin.
	claudeClosing map[string]claudeAcceptedTurn
	// claudeDialogNote is an inline manual diagnostic when a notification
	// cannot be ordered against the current turn. It never carries buttons.
	claudeDialogNote map[string]string
	// claudeBindMu / claudeBindLocks serialize per-node Claude binding
	// mutations (startup, clear, resume) so a failed commit cannot race a
	// generation change onto an uncommitted store record.
	claudeBindMu    sync.Mutex
	claudeBindLocks map[string]*sync.Mutex
	// claudeAfterCandidate, when non-nil, runs after an uncommitted
	// claude-binding-candidate is appended and before the committed record
	// is validated. Tests only.
	claudeAfterCandidate func(nodeID string)
	// lastDeliver is when scimux last pasted a prompt into each node's pane.
	// It is the only thing that makes a stale link provable: the agent owes
	// output for that prompt, so a transcript with no content since then is
	// not the file the pane is writing to. Pane phases cannot stand in for it
	// — the poller's first capture after a restart manufactures one (prevCap
	// starts empty), and so does TUI chrome redrawing after a finished turn.
	// In memory only, and deliberately so: a fresh process has delivered
	// nothing, so it has nothing to judge and leaves every link alone.
	lastDeliver map[string]time.Time

	// storeMu serializes every append to nodes.jsonl, independent of a.mu (some
	// callers hold a.mu, some do not). It gives the append-only store one
	// process-local write point so concurrent audit/transcript/delete records
	// cannot interleave a partial line, and pairs each append with an fsync —
	// these records are the durable truth the supervisor replays and audits.
	storeMu sync.Mutex
	// settingsMu serializes saveSettings. The document is consent (settings.go),
	// so its write-then-rename must be one critical section: a save that
	// reports success has to be the save that reached disk. The settings
	// endpoint holds it across its whole read-modify-write instead, so two
	// browsers cannot clobber independently updated consent fields, and calls
	// writeSettingsLocked because this mutex does not nest.
	settingsMu sync.Mutex
	// uiMu serializes the read-modify-write of the UI-state file (ui.json),
	// independent of a.mu. The revision check plus the atomic tmp-write+rename
	// must be one critical section, but they are pure file I/O over a private
	// document — holding the app-wide a.mu across two syscalls would stall the
	// poller and every other handler for a write that touches no shared state.
	uiMu   sync.Mutex
	server *tmuxsession.Server
	acp    acpManager
	codex  codexManager
	muse   museManager
	// workers is installed only by the production muxer. Tests and the
	// session-worker process itself retain the in-process managers above so
	// their existing state-machine tests stay narrow and deterministic.
	workers *workerManager
	// launchGrace bounds how long a freshly-launched tmux agent is watched for
	// an immediate failure (a rejected --model, a bad flag). The launch is
	// wrapped so such a process leaves its error on the pane (wrapLaunch); within
	// this window awaitLaunch reads it and reports it to the create caller instead
	// of persisting an unexplained dead node. Zero disables the check. launchPoll
	// is the capture interval inside that window.
	launchGrace           time.Duration
	launchPoll            time.Duration
	claudeReadyTimeout    time.Duration
	claudeDeliveryTimeout time.Duration
	// claudeDeliveryGiveUp is the outer bound on an unconfirmed first prompt.
	// claudeDeliveryTimeout only ends the synchronous wait; because the CLI
	// writes the transcript lazily, expiry there is neutral and the poller
	// keeps watching. This is where the wait finally becomes an inline error,
	// which it must, or a paste swallowed by a startup dialog would hang in
	// silence. A non-positive value means no bound: a zero budget must not
	// turn the backstop itself into an instant failure.
	claudeDeliveryGiveUp time.Duration
	claudeInitialPoll    time.Duration
	deliverClaudeInitial func(*Node) initialDelivery
	// claudeIDs maps the family alias the UI offers (opus/sonnet/haiku/fable) to
	// the concrete model id the installed claude CLI actually accepts, probed once
	// by refreshClaudeModels because the CLI mis-resolves its own aliases.
	// Empty until the probe returns, and empty forever if claude is absent or the
	// probe fails — in which case launches fall back to passing the bare alias.
	// Written by the refresh triggers, read per launch; guarded by claudeMu.
	claudeIDs map[string]string
	claudeMu  sync.Mutex
	// claudeResolveModels performs the probe. It is a field, not a call, so the
	// expensive machinery is installed by the serve path rather than reachable
	// from any app value: the suite must never launch a real claude, and a
	// handler that asks for a refresh must stay inert in a test.
	claudeResolveModels func(context.Context) map[string]string
	// claudeVersion reads the installed CLI's version, the cheap key the cache
	// is validated against. Injected for the same reason: a test must not shell
	// out to whichever claude happens to be on the host.
	claudeVersion func(context.Context) string
	// claudeRefreshing collapses overlapping refresh triggers into one run.
	claudeRefreshing atomic.Bool
	claudeCachePath  string // ~/.scimux/claude-models.json; empty disables caching
	// claudeProbeDir is the neutral cwd every claude probe runs in, so a
	// throwaway session's transcript can never land in a node's
	// ~/.claude/projects folder (claudeProbeWorkdir).
	claudeProbeDir string
	storePath      string
	// claudeHooksRoot decouples a worker's private harness journal from the
	// shared hook bundles invoked by the CLI. Empty preserves the ordinary
	// <data>/claude-hooks derivation.
	claudeHooksRoot string
	skipHookCleanup bool
	uiPath          string
	// settingsPath is the computer's own settings (~/.scimux/settings.json),
	// distinct from the opaque per-browser blob at uiPath. Empty disables them,
	// which reads as every default — see settings.go.
	settingsPath string
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
	// museCatalog, when set, is the injectable Muse model-list probe. Nil
	// (the test default) is unavailable authority and fail-closed. Production
	// installs probeMuseCatalog on the serve path only.
	museCatalog func(context.Context) ([]muse.Model, error)
	// museModels is the last catalog a probe returned, and the only thing
	// GET /api/agents is allowed to read: the probe spawns `muse serve` and
	// MSP-initializes it, which is not work a browser request may wait on.
	// museModelsAt dates it; a zero time means no probe has ever succeeded,
	// which reads as an empty catalog rather than as an error.
	museModelsMu sync.Mutex
	museModels   []muse.Model
	museModelsAt time.Time
	// museRefreshing collapses overlapping refresh triggers into one run, the
	// same guard claudeRefreshing provides for the claude probe.
	museRefreshing atomic.Bool
	// agentCatalog overrides installed-harness discovery in focused tests. The
	// returned map is always cloned before a request-specific Muse overlay, so
	// the process-wide discovery cache remains immutable and race-safe.
	agentCatalog func() map[string]agentInfo
	// museClassify maps an exact model ID to "standard" or "discounted".
	// Nil or any other result is "unknown". Tests leave it nil by default;
	// the production serve path installs the maintainer-approved policy.
	museClassify func(modelID string) string
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
	// (~/.codex/sessions, live `grok agent stdio` for billing). Claude has no
	// entry here: its reading comes from a probe session, not a file or a URL.
	codexSessionsDir string
	grokUsageOpts    acp.GrokBillingOptions
	// requestPolicy is the Host / Fetch Metadata / anti-framing boundary
	// configured from -addr and -trusted-host. Nil means the default
	// loopback policy (127.0.0.1), matching the shipped -addr default.
	requestPolicy *requestPolicy
	// prepareWebUpdate validates a replacement web child from the verified
	// temporary executable while the current generation is still serving.
	// The returned transaction is committed only after atomic installation,
	// and aborted if installation fails. Nil only in monolithic unit fixtures.
	prepareWebUpdate func(context.Context, string) (webUpdateHandoff, error)
	// runtimeStatus is the active web generation's small projection. Harness
	// state never moves into it; it exists so an old long-lived muxer can name
	// the newly activated web binary and its web-owned remote client.
	runtimeStatus *muxerRuntimeStatus
	// hostedRemote, when set, is the computer remote-access client whose
	// status is projected on GET /api/state. Nil without --remote.
	hostedRemote interface{ HostedStatus() string }
	// hostedPairing is the pairing API the local HTTP routes call.
	// It is a sibling of hostedRemote, not a widening of it.
	hostedPairing hostedPairingClient
}

// PermOption is one answerable permission choice, identical to agentperm.Option.
type PermOption = agentperm.Option

// PendingPermission is the UI-facing permission view, identical to
// agentperm.Pending.
type PendingPermission = agentperm.Pending

// procManager is the shared surface of scimux's structured-protocol
// transports: acp.Manager (pi/opencode/grok over the ACP SDK), codex.Manager
// (codex over its app-server protocol), and muse.Manager (Muse over MSP). Each drives one subprocess per node,
// keep the authoritative history in an append-only session log, and answer
// permission prompts structurally — so the create/poll/chat/send/key/peek paths
// treat them uniformly. The in-process tmux path does not use this interface;
// workerManager does, including when it routes a Claude session worker.
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
	Peek(nodeID string) string
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
// each embeds its manager (whose method set already matches) and adds only
// error classification.
type acpManager struct{ *acp.Manager }

func (m acpManager) Conflict(err error) bool {
	return err == acp.ErrNoSession || err == acp.ErrNotAlive ||
		err == acp.ErrTurnActive || err == acp.ErrNoPending || err == acp.ErrNoTurn ||
		err == acp.ErrStalePermission
}

type codexManager struct{ *codex.Manager }

func (m codexManager) Conflict(err error) bool {
	return err == codex.ErrNoSession || err == codex.ErrNotAlive ||
		err == codex.ErrTurnActive || err == codex.ErrNoPending || err == codex.ErrNoTurn ||
		err == codex.ErrStalePermission
}

// museManager adapts *muse.Manager to procManager. Conflict uses errors.Is
// so wrapped Muse sentinels still classify as HTTP 409.
type museManager struct{ *muse.Manager }

func (m museManager) Conflict(err error) bool {
	return errors.Is(err, muse.ErrNoSession) || errors.Is(err, muse.ErrNotAlive) ||
		errors.Is(err, muse.ErrTurnActive) || errors.Is(err, muse.ErrNoPending) ||
		errors.Is(err, muse.ErrNoTurn) || errors.Is(err, muse.ErrStalePermission) ||
		errors.Is(err, muse.ErrLaunchConflict)
}

func (m museManager) Shutdown() {
	if m.Manager == nil {
		return
	}
	m.Manager.Shutdown()
}

func (m museManager) SetAssetHook(f asset.IngestFunc) {
	if m.Manager == nil {
		return
	}
	m.Manager.SetAssetHook(f)
}

// museFingerprintReporter is an optional narrow surface implemented by the
// Muse adapter. It is not on procManager: ACP and Codex must not grow a
// method solely for this warning.
type museFingerprintReporter interface {
	FingerprintMismatch(nodeID string) bool
}

func (m museManager) FingerprintMismatch(nodeID string) bool {
	if m.Manager == nil {
		return false
	}
	return m.Manager.FingerprintMismatch(nodeID)
}

const museSchemaWarning = "muse_schema_fingerprint_mismatch"

// proc returns the process manager for a node. An in-process tmux (Claude)
// node has none; a worker-backed Claude node uses the same boundary as ACP and
// structured subprocess nodes so the HTTP/poll paths do not depend on their
// transport.
//
// When testProc is non-nil (tests only), it replaces the real manager so HTTP
// handlers can be exercised against a stub without a live agent subprocess.
func (a *app) proc(n *Node) procManager {
	if a.testProc != nil {
		return a.testProc
	}
	if a.workers != nil {
		if n.transport() == "acp" || n.transport() == "codex" || n.transport() == "muse" {
			return a.workers
		}
		if n.Agent == "claude" && a.workers.manages(n.ID) {
			return a.workers
		}
	}
	switch n.transport() {
	case "acp":
		return a.acp
	case "codex":
		return a.codex
	case "muse":
		return a.muse
	}
	return nil
}

// shutdownStructured tears down every in-process structured-protocol manager.
// Production shutdown goes through the session-worker owner; this remains the
// monolithic test/fallback path. tmux sessions are left running on purpose and
// each manager's Shutdown is idempotent.
func (a *app) shutdownStructured() {
	if a == nil {
		return
	}
	a.acp.Shutdown()
	a.codex.Shutdown()
	a.muse.Shutdown()
}
