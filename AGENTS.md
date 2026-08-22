# Agent guide for scimux

scimux supervises tmux-wrapped agent chats (Claude Code, Codex) from a local
web page. Read the README for architecture; this file lists only the
invariants you must not break and the workflows you need.

## Commands

```sh
go build -o scimux ./cmd/scimux # single static binary; web/index.html is embedded
go test ./...          # unit + integration (integration needs tmux)
go test -short ./...   # unit only; this is what CI runs
node --test web/test/*.test.js  # browser unit suite (1069 tests, including web/test/audit.test.js; no browser needed)
gofmt -w $(find . -name '*.go' -type f) && go vet ./...
```

Integration tests create private, randomly named tmux sockets and clean up
after themselves; they never touch a user's tmux server. Never run a real
agent CLI (`claude`, `codex`, `pi`, `opencode`, `grok`) in tests — wrapped
test commands are `bash --norc` or `cat`.

## Layout and dependencies

- `cmd/scimux` is the deliberately thin executable: command-line startup and
  the linker-stamped version only. Keep product behavior out of it.
- `internal/app` owns the private application: HTTP routes, node lifecycle,
  polling, persistence, and their co-located Go tests. Its `testdata/` belongs
  to that package; do not recreate a repository-wide test directory.
- `internal/*` packages are private leaf subsystems. `internal/app` may depend
  on them, but they must not import the application package.
- `web` contains the checked-in browser source and the small embedding package;
  `legal` contains the notices embedded in the About sheet. Both are imported
  by `internal/app`, never the reverse.
- Keep the repository root for module metadata, documentation, licenses, and
  top-level directories. Do not add application Go files or tests there.

Comments that say `Packet <N>` are design-doc phase references used
consistently across the codebase; they are kept deliberately so a change stays
traceable to the phase that introduced it. Do not mass-rename them as jargon.

## Invariants (deliberate design decisions — do not "improve" them away)

- **Zero build dependencies.** Standard library only. Do not add modules
  (no SQLite, no WebSocket lib, no JS framework). If a feature seems to need
  one, stop and discuss instead.
  - **One approved exception:** `github.com/coder/acp-go-sdk`, the Agent
    Client Protocol peer, is a deliberate, maintainer-approved dependency
    **scoped to `internal/acp/` only** (the ACP transport for pi/opencode/grok; see
    `acp-integration-plan.md`). A hand-rolled bidirectional JSON-RPC peer with
    typed unions was evaluated and rejected as ~600 lines of ongoing schema
    churn. The tmux/transcript core stays stdlib-only; do not let the SDK (or
    any other module) leak beyond `internal/acp/`.
  - **Approved exception #2 (taken 2026-08-22):** `github.com/pion/...` for
    the WebRTC transport is a deliberate, maintainer-approved dependency
    **scoped to `internal/remote/` only**. The decision is made and is not to
    be relitigated: a browser will only speak WebRTC for a peer-to-peer data
    channel, so the laptop end must speak the same ICE/DTLS/SCTP stack, and
    hand-rolling that in stdlib was rejected as neither realistic nor safe to
    maintain. The module **landed in S6** (2026-08-22) as a single direct
    require, `github.com/pion/webrtc/v4`; its twenty-odd siblings are
    `// indirect` and are transitive closure, not further decisions. The one
    direct path is listed in `allowedModuleRequires`
    (`internal/app/remote_boundary_guard_test.go`), which is the mechanical
    half of the approval — adding another line there is a new exception, not
    an upgrade.
    Nothing outside `internal/remote/` may import it, ever. The scope is
    mechanically enforced: `internal/app/remote_boundary_guard_test.go`
    asserts over the module import graph that no package outside
    `internal/remote/` imports `github.com/pion/...`. That guard was written
    while it is vacuously true, precisely so it can never be "added along
    with the dependency".
  - **Packaging, decided 2026-08-22 — one binary, unconditionally.** pion
    ships in the single default binary. There is **no build tag, no
    `remote`/`noremote` variant, and no second artifact**; a two-binary split
    was considered specifically to keep a pion-free default and was
    **rejected** — one static binary for everything is the product.
    Consequences to accept rather than relitigate: pion is compiled into every
    build, including for users who never pair a device; because remote is a
    *runtime* switch and not a build-time one, the linker cannot dead-strip
    it; and the module count rises from 2 to the whole pion family. What does
    not change: pion is pure Go, so `CGO_ENABLED=0` and the FreeBSD static
    build keep working, and `go build -o scimux ./cmd/scimux` stays the only
    build command. Do not reintroduce build tags to "offer a slim build" —
    that is the rejected option, not an optimisation. The import guard above
    stays in force regardless: approving the dependency widened *what* may be
    imported, never *where it may be imported from*.
- **Snapshot over stream.** Output is read via `tmux capture-pane -p`
  snapshots and via the transcript JSONL files the agent CLIs write
  themselves. Never parse the terminal byte stream, never use tmux control
  mode (`-CC`), no WebSockets, no in-browser terminal emulator. Polling is
  the intended transport. (The ACP transport in `internal/acp/` is consistent
  with this: it consumes the agent's *structured* typed protocol records, never
  a terminal byte stream, and the UI still polls. For an ACP node "peek" renders
  a tail of the append-only session log instead of a pane photo.)
- **One session = one window = one pane = one agent process.** No muxing.
- **Defensive transcript parsing.** The CLIs' log formats are undocumented
  internals. Unknown record types/shapes are silently ignored, never errors;
  when a transcript is missing or stops making sense, the UI degrades to the
  pane snapshot ("peek"). Keep this contract when touching
  `internal/transcript`.
- **Append-only store.** `~/.scimux/nodes.jsonl` is replayed at startup;
  corrections are new records, never rewrites.
- **One session-log store, one schema.** Every structured transport writes its
  per-node history to `~/.scimux/sessions/<node-id>.jsonl` as
  `internal/sessionlog` events (plain JSONL — the corpus must stay
  grep/sed/awk-able; nothing binary). The filename is the node's reusable
  title slug; identity lives in the file's `meta` header record, and deleting
  a node archives its log to `sessions/archive/` so a reissued slug can never
  append onto dead history. tmux nodes reach the store through the transcript
  mirror (`mirror.go`): the poller projects tailer output into the same
  records, with `source` seam records marking every transcript (re)bind
  (/clear rollover, hook bind, rotation) as the dedupe watermark. New transports
  write the same records to the same directory — do not introduce
  per-transport log formats or directories. The store is also the **chat
  read path for every transport** (phase 3): handleChat renders the log's
  current segment — everything after the last `source` seam — while the
  tailer serves only mechanics (needs-input, staleness, delivery
  confirmation). Earlier segments are readable on demand, never polled:
  `?history=1` returns the whole log as ordered surfaces
  (`sessionlog.ReadHistory`), which the UI renders above the live segment
  behind a "show earlier history" tap (and auto-expanded when an earlier
  stop is tapped on the metro map). Keep that split — history in the poll
  payload would ship the whole corpus every second and can repeat turns
  across mechanical seams (a rotation re-mirrors from turn zero).
- **/clear = page turn, fork = fresh notebook.** `/clear` starts a fresh chat
  surface under the *same* node: same log file, an appended `source` seam —
  never a new file, never truncation; the context gauge is segment-scoped.
  ACP nodes (pi/opencode/grok) implement it as **deterministic process
  replacement** — kill the subprocess, negotiate a fresh one under the same
  node (a second `session/new` on one connection is unproven upstream; a
  fresh PID self-evidently carries no context); codex opens a new thread on
  the same PID (multi-thread per app-server process is first-class there).
  In both, the seam is appended only after the protocol call succeeded;
  Claude gets a path-less "detached" seam at retire time and the successor
  bind only from that node's validated SessionStart hook. Fork stays the only
  path that can change launch config: a forked node inherits
  agent/model/effort/dir but never conversation history. Claude's native
  `/fork` command and `SessionStart source:"fork"` are not supported —
  forking is scimux's existing Fork action only (a fresh node/process,
  ordinary `SessionStart source:"startup"`). Never send `/fork` to the
  Claude CLI.
- **A Claude transcript is bound only by that node's own SessionStart hook.**
  Every Claude launch mints a fresh hook bundle under
  `~/.scimux/claude-hooks/<hex-id>/` (`settings.json` 0600, `inbox/` and
  `processed/` 0700) and passes it as `--settings`; the file registers scimux
  itself as the `SessionStart` command, so the hook firing *is* the
  process→transcript ownership proof **and** the startup/hook-health
  acknowledgement. The undocumented transcript `bridge_status` record is not
  a readiness condition. `--session-id` seeds the first transcript,
  `--continue` is forbidden, and `--settings` must precede
  `--remote-control` (an optional-value flag that would otherwise swallow it).
  Every owned launch also passes `--ax-screen-reader` and `--add-dir` for
  every genuinely additional directory outside the working directory
  (including the attachment staging directory). `--add-dir` does **not**
  bypass Claude workspace trust; scimux never edits undocumented trust-state
  files. Create returns the new node immediately so the browser can select it
  and show the first prompt as a pale pending bubble while SessionStart and
  delivery confirmation are outstanding. The prompt is pasted exactly once,
  only after a valid SessionStart acknowledgement; the bubble becomes solid
  only when the matching transcript user turn confirms delivery. The send
  gate stays held across that window so no later prompt can overtake the
  first. SessionStart timeout, a diagnosed workspace-trust dialog, paste
  failure, or final delivery-confirmation failure is an inline error: the
  prompt is restored as the node's draft, never retried automatically, and
  the terminal is not opened. A generic lettered startup dialog is **not**
  diagnosed as workspace trust — the inline error tells the user to inspect
  Claude outside scimux. scimux deliberately supports Claude's default `~/.claude` state root
  only; it does **not** evaluate or support `CLAUDE_CONFIG_DIR`. Keep
  transcript-path validation anchored under `~/.claude/projects` unless that
  scope decision is explicitly revisited.
  Bindings are per-node and generation-numbered (`claude-binding` records,
  replayed *after* ordinary node records so a later title edit cannot wipe
  one); retired paths and session ids become `deadTranscripts` tombstones that
  can never be rebound for that node. SessionStart sources `startup`,
  `clear`, `compact`, and `resume` are accepted and never become
  inspect attention. `source:"fork"` is rejected. Same-identity `resume` is a
  no-op; a valid changed session/path is rebound with the current generation,
  path-claim, tombstone, persistence, and source-seam rules — never left
  silently on the old transcript. The commit is transactional: an uncommitted
  candidate cannot be replayed, and tombstone persistence failure fails the
  rebind. If that rebind cannot complete, the node surfaces an inline
  error rather than inspect. Both live memory and store replay keep the
  previous binding. A newly prepared current hook bundle reports
  `claude_starting` while SessionStart is outstanding; permission and
  auto-approve capability become authoritative only after the valid
  SessionStart acknowledgement. Adopted, incomplete, legacy, or
  moved-binary bundles are `claude_unsupported`. This is deliberately
  **fail-closed**: no complete current hook bundle means the Claude node is
  unsupported (inline explanation), never inspect/fallback supervision, and
  never a guessed owner from the pane cmdline. Consequences worth knowing before "fixing" a
  bug report: spurious pane noise that trips the staleness backstop drops a
  node to peek until the next `/clear` or relaunch hook; and because
  `settings.json` bakes `os.Executable()` at launch, a pane that outlives a
  *move* of the scimux binary keeps a settings file pointing nowhere, so its
  next `/clear` silently fails to bind. Rebuilding in place is harmless (Go
  captures the path at process start — no `(deleted)` suffix — and the path
  resolves to the new binary), and every fresh launch bakes the current path,
  so this needs a moved-or-deleted binary *plus* a surviving pane *plus* a
  `/clear`. Never retire a transcript on absent evidence: a just-created
  successor holds only meta records, so "no recognized content yet" is not
  staleness (see `maybeRelinkTranscript`).
- **Liveness is mechanical only** (active/quiet/exited/unavailable, from
  pane-change detection). Do not add regexes matching agent TUI strings.
  The inspect/owing/quiet-fallback attention path below is for non-Claude
  agents. A scimux-owned Claude node never raises inspect from quietness,
  AX, owing, or transcript faults (see the Claude-only strict terminal
  policy). Needs-input detection follows the same rule: it combines an unresolved
  tool call in the transcript (structured data — both CLIs log the call
  record when the agent asks and the result record only after the human
  answers) with a mechanically quiet pane (running tools animate a timer;
  approval dialogs are static). Pane text never creates attention on its
  own; the fenced exceptions are the `internal/dialoghint` matchers, which
  only ever *corroborate or classify*: on a quiet pane as the regex fallback,
  and on an active pane **only** when (a) the transcript already shows an
  unresolved call and (b) successive capture diffs stay confined to a few
  stable lines (`noteAnim` — diff geometry, still mechanical). That second
  path exists because an approval dialog with parallel tool calls queued
  behind it animates the queued call's spinner indefinitely, so quietness
  never arrives (observed live: a 6m42s approval wait, unnoticed). If the
  matcher goes dark (TUI rewording), the same confined-animation state with
  a stalled transcript degrades to the neutral `inspect` after
  `animStallAfter`, never a classified dialog. (For an owned Claude launch this
  whole active-pane chain is superseded by the escalation notice — see the
  "evidence, never an answer" invariant below — because the notice settles the
  ambiguity these heuristics can only guess at.) A further mechanical quiet-
  branch backstop covers Claude Code's late tool_use flush (the call record
  is not on disk until approval, so `WaitingOn` stays false for the whole
  wait): when the newest recognized transcript record is a **user** turn —
  a human prompt or a tool result, i.e. the agent owes the next output —
  and the pane has been static past `owedStallAfter`, raise neutral
  `inspect`. That signal is turn role plus pane quietness only — no pane
  text — and never feeds liveness. One narrow use of pane text rides on top
  of it: `dialoghint.HasCancelAnchor` (the bare "esc to cancel" chrome
  phrase) shortens that wait to `owedStallCorroborated`. It only ever
  *sharpens the timing of a verdict the mechanical evidence already
  reached* — it cannot raise attention alone and cannot change the kind,
  because the phrase also occurs in ordinary agent prose (any session
  discussing dialoghint prints it). The ordinary `esc to interrupt` working
  footer is the inverse, suppression-only hint: it may suppress neutral
  `inspect`, but it never feeds liveness, creates attention, retires a hook
  notice, or overrides a classified dialog. Claude's explicit interrupted-
  message record is a completed turn boundary even though Claude writes it
  with role `user`; it clears Owing/pending and releases the next-prompt gate.
  `handlePeek` runs the same quiet-branch
  predicate (matcher + owing stall) one-shot when a human opens the terminal
  view, so a late-flush dialog is visible without needing an unresolved call
  in the transcript. `handlePeek` itself is *not* quiet-gated (that is the
  point — it catches the dialog whose queued calls keep the pane animating),
  so the shared predicate enforces the static-pane precondition itself
  (`paneQuietAfter`): on an active pane only the corroborated path may raise.
  Liveness itself stays regex-free — the matchers and the owing backstop must
  never feed active/quiet.
- **A Claude approval is answered through the hook, never through the pane.**
  The same per-node bundle that binds the transcript also registers a
  `PermissionRequest` hook (`__claude-permission-hook --dir <bundle>`), so
  auto-approval has a structured channel instead of a simulated keystroke,
  and a `Stop` / `StopFailure` hook (`__claude-stop-hook --dir <bundle>`)
  so the current-turn lease ends when Claude finishes responding.
  The hook is registered on **every** owned launch and never *authorizes*
  anything until a lease is armed: with no `perm/lease` marker it prints
  nothing and exits 0, so Claude falls through to its own dialog exactly as if
  no hook existed. It is not silent, though — see the escalation-notice
  invariant below. `PreToolUse` is deliberately never registered (it fires for
  every tool call, decision needed or not).
  Claude offers the hook **no menu and no `tool_use_id`**, so the rendezvous is
  keyed by a hook-minted nonce (one blocked helper process = one request = one
  answer), and "allow once" is an *omission*: scimux emits
  `{"behavior":"allow"}` and never `updatedPermissions`, the only member that
  would persist a rule. Ordering rules that must not be inverted: arm flips
  in-memory state *then* writes the marker, disarm removes the marker *then*
  clears state (a stale marker can only cost a tool call the helper's deadline,
  never authorize one); and a request is claimed by rename before it is
  audited, audited before it is answered. The marker is published only in the
  `armed` phase — that is Claude's substitute for the structured transports'
  enable cutoff, because a call that asked before the marker existed left no
  request to answer. A lease also **latches the first `prompt_id` it answers**
  (`autoApproveState.TurnPromptID`) and declines any request from another turn:
  prompt_id is the CLI's own turn identity — shared by a parent and its
  subagent within a turn — so the fence holds even when the mechanical turn
  edge is missed. It lives on the lease, not on the node, so a
  fresh lease is a fresh fence with no clearing step to forget. The two
  deadlines are deliberately different numbers: the helper waits
  `permHookDeadline` (5s, generous — a loaded host runs the lane late), the app
  stops answering at the strictly shorter `permAnswerWindow`, because an answer
  written at the helper's last instant would be audited as an approval nobody
  read. A decline that failed *only* on that clock is reported (session-log
  error plus the chat header's concise error); a policy decline stays silent, or
  every plan-mode call would cry wolf. Bundle layout is proven from disk
  (`capabilities.json`, which also records the `exec` path settings.json baked),
  but a fresh launch becomes capable only after that Claude process successfully
  delivers its SessionStart hook. An adopted pane — or one whose scimux binary
  has been *moved* — degrades to a disabled toggle rather than a lease that can
  never arm.
- **Auto-approve scope is one turn for every agent.** The toggle is
  **current-turn** on Claude, Codex, grok, opencode, and pi: it disarms at
  turn completion, stop/interrupt, un-toggling, `/clear`, `/exit`, and the
  hard resets nobody chooses (process/session loss, node delete, and process
  restart — the lease is never persisted). Every agent shares the same help
  and label ("Auto-approve this turn"). Enabling during a Claude approval
  dialog still arms the *current* turn: those dialogs are mechanically
  quiet, so `live=="active"` is the wrong predicate — owing, an unresolved
  call, or a standing escalation notice means the turn is still open.
  Claude's **turn-completion edge** is its official `Stop` hook
  (`__claude-stop-hook --dir <bundle>`), registered on every owned launch
  beside SessionStart and PermissionRequest; `StopFailure` uses the same
  helper. `capabilities.json` carries `"stop"` so a pre-Stop launch is
  visible on disk (it still auto-approves via the transcript fallback).
  The helper reads the current arm marker and the app-owned accepted-turn
  nonce and writes a lease-and-turn-tagged notice (`processed/stop/`); drain
  *settles* (`settleAutoApproveAfterTurn`) only if that `LeaseID` is still
  armed, the notice is inside a 30-minute TTL, and `session_id` matches.
  Drain also requires the notice's turn nonce to match the current or
  closing accepted turn. A late Stop from turn N cannot revoke, clear,
  tombstone, or otherwise mutate turn N+1. Cleanup for a matching turn is
  unconditional even when no lease was armed. The lease-ID fence remains
  an additional guard.
  Quarantine on session mismatch or TTL leaves the lease **armed** — failure
  keeps authority. The app-owned turn nonce is published durably before
  Enter; marker publication failure refuses the send. A second Claude turn
  is refused until the prior matching Stop has been drained, so a delayed
  helper can never relabel turn N as N+1 by reading a replaced marker.
  The poller republishes the arm marker only when remaining life is below
  half of `claudeLeaseTTL` (30 min), so Stop still sees it on a long turn
  without an fsync every poll tick. `stop_hook_active` writes nothing —
  that turn is still running. `SubagentStop` is never registered.
  An unread auto-approve Error on the lease is dropped at the boundary
  (same as every other agent); the session-log audit still has the failure.
  **For a SessionStart-acknowledged Claude node the lease stays armed until
  Stop / StopFailure, interrupt, disable, `/clear`, `/exit`, or process
  loss.** Pane quietness and the next accepted prompt are not Claude turn
  boundaries. Codex/ACP keep the existing protocol settle. A deadline,
  policy, competing-hook, or upstream ask/deny failure is an inline
  auto-approve error only: while the lease remains armed, scimux does not
  publish map/card attention and does not open the terminal. Disabling
  auto-approve reevaluates any still-current visible-dialog epoch and then
  publishes attention if that dialog is still waiting. Stop, StopFailure,
  interrupt, `/clear`, `/exit`, process loss, and node deletion disarm the
  matching lease, clear Claude attention, and tombstone unresolved
  asked/shown/epoch records for the ending session so a later turn cannot
  pair with them. AskUserQuestion,
  ExitPlanMode, plan choices, deny rules, and unknown modes are never
  auto-answered. Every per-request guard stays: sole one-time
  `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`
  (never `updatedPermissions`), lease IDs, prompt_id fencing, audit-before-
  delivery, and no automatic retry of a request already attempted.
- **Claude-only strict terminal policy** (maintainer-approved, 2026-08-18).
  For a newly scimux-owned, SessionStart-acknowledged Claude node with the
  complete current hook bundle (SessionStart, PermissionRequest,
  Notification(`permission_prompt`), Stop, StopFailure), the tmux terminal
  may be visible only when the user opened it with the terminal button, or
  when auto-approve is not armed and a current permission dialog is
  structurally proven. Notification(`permission_prompt`) proves only that
  *a* permission dialog is visible — Claude's payload has no PermissionRequest
  id and no tool_use id, so scimux does **not** claim exact request
  correlation. The UI action is bound to a server-minted visible-dialog
  epoch (`perm_dialog_id`). Action keys and labels come only from a
  structurally validated current AX menu (or another exact source) after
  that Notification; invented Yes / "don't ask again" / No rows are never
  shown for questions or plan choices. A standing ask's tool hint classifies
  `AskUserQuestion` and `ExitPlanMode` — a generic Notification title does
  not override that. If exact options cannot be extracted, the chat shows
  an inline manual-response state and Open Terminal, with no guessed
  buttons. Notifications are fenced by the accepted-turn nonce; a notice
  belonging to a closed turn must not mint an epoch for the next turn.
  A successful pane key retires that epoch only.
  Standing asked notices are not a guessed Notification identity and retire
  only through matching transcript/tool evidence or an explicit turn
  boundary. A `/key` body that includes `dialog_id` is fail-closed: a
  missing, retired, or mismatched epoch returns 409 and never types into
  Claude's prompt. Questions, plan choices, deny rules, and unknown modes
  are never auto-answered; they stay answerable through the same
  epoch-bound surface. While auto-approve is armed the map does not go
  yellow and the terminal does not auto-open, but the chat still shows
  epoch-bound controls (not generic unbound keys). Out-of-order or
  concurrent asks cannot present an older request as the current dialog
  identity, because no such identity is claimed. Pane text is never parsed
  to invent one. Pane quietness,
  AX static rendering, unresolved transcript calls, missing/stale/
  unparseable transcripts, fallback chat, and owing timeouts must never
  automatically open the terminal and must never emit "quiet · inspect
  terminal". Transcript and startup faults become inline status or error
  states. Adopted panes and incomplete/old hook bundles are
  **unsupported**: they get an inline explanation and never enter this
  hooked path or the inspect/fallback supervision used by other agents.
  A moved or missing scimux executable invalidates hook capability. Stop
  clears obsolete Claude attention and terminal-forcing state. Codex, ACP,
  pi, opencode, and grok keep their existing inspect/fallback behavior.
- **An escalation notice is evidence, never an answer.** The same hook writes
  `perm/asked/<nonce>.json` for *every* decision it escalates, armed or not
  (`internal/app/claude_asked.go`), because the hook knows the one thing no
  amount of transcript or pane reasoning can recover: whether a dialog is about
  to be drawn. An unresolved `tool_use` over a confined animation is
  byte-identical between "a long tool is running" and "a dialog is up with
  parallel calls animating behind it" — which is why the animation-stall
  backstop occasionally fired hard attention at a busy agent. So for a Claude
  node whose bundle proves the layout (`capabilities.json` gains `"asked"`, a
  gate independent of `"permission"` so older bundles degrade to *no gate*,
  never to *no attention* — and both gates additionally require the bundle's
  baked `exec` path to still be a runnable file, because a bundle whose binary
  moved writes no notices and its silence must never read as "Claude asked
  nothing"), the notice becomes the sole authority on the
  active-pane paths: a notice raises immediately, classified by its tool name;
  no notice suppresses both the `animStallAfter` degradation and the
  attention-preservation window. It also raises where nothing else can — Claude
  flushes the `tool_use` record only *after* approval, so for a first blocked
  call the waiting helper is the only proof the dialog exists. The quiet-pane
  fallback is deliberately **not** gated (rate-limit menus, trust-folder and
  login prompts fire no hook and reach a quiet pane). Notices are retired by
  the helper when it auto-approves (an approved call never reaches a dialog), by
  the **specific tool call** each one announced, by recognized transcript
  **growth** as a fallback, and by a generous TTL as a leak backstop. Pane
  geometry is deliberately *not* a retirement signal.
  The per-call rule is the primary one and it is an exact join, not a heuristic:
  the notice stores a digest of the `tool_input` it escalated (never the input
  itself — a disarmed session writes one per decision without the human having
  consented to any audit), Claude writes the matching `tool_use` record only
  *after* the human answers, so that record proves that dialog closed. It
  therefore retires **regardless of what else is in flight**, and each record is
  credited once (a per-node stamp watermark) so one approved call cannot drain
  notices for dialogs still on screen. Growth remains for what no digest can
  join — a dismissed dialog, or an ask that never becomes a `tool_use` record
  (plan choice, question) — and keeps its old guards: exactly one notice, oldest
  first, only a notice **older** than the newest dated turn (an agent that
  printed text and *then* asked produces the reverse ordering), and only with
  **no unresolved call at all** (`PendingCount() == 0`). That last guard is why
  growth cannot carry the load alone: a working agent nearly always has a call
  pending, so notices stood until their 4h TTL and held hard attention over an
  agent that was merely busy — 16 standing notices across a single 23-minute
  turn, observed 2026-08-18, each unfolding a peek that showed only a spinner
  and `esc to interrupt`. The watermark still advances on every tick, including
  the ticks the pending set blocks: a frozen mark could not report the growth
  the *next* resolution writes.
- **Neutral inspect is acknowledgeable, not actionable.** A fresh `inspect`
  may unfold the terminal once. Non-AX inspect shows the default terminal
  action bar (digits, y/n, arrows, Enter, Escape) plus a distinct **Dismiss**
  that never sends a key. Escape stays a remote key so a misclassified dialog
  can still be backed out of; Dismiss is the local “I looked — all good”
  acknowledgement (title/aria say it does not send Escape). **AX inspect is
  Dismiss-only:** `tmuxKeySequence` expands `"1"` to `["1","Enter"]`, so a
  keypad on a spurious inspect would submit a prompt. The acknowledgement
  hides the terminal and action row for that exact evidence epoch. It remains
  dismissed across polls and animation frames, and becomes visible again only
  when fresh evidence changes `attention_at`, the attention kind changes, or
  attention clears and is later raised again. Ordinary approval/question/dialog
  buttons still send their whitelisted remote keys and keep only the short
  stale-poll suppression.
- **Remote keys are a whitelist.** `SendKey` accepts only the dialog keys
  (digits, y/n, arrows, Tab, Enter, Escape) — it answers prompts, it is not
  a keystroke injector. Every key pressed via the API is recorded in the
  store with the pane's bottom lines as decision evidence.
- **Polling must never clobber user input.** The web UI re-renders a region
  only when its state signature changes; the composer is a singleton outside
  all render regions (drafts persist per node); and any editable element that
  lives *inside* a polled render region must survive the rebuild with value,
  focus, and cursor intact (`withCardEditsPreserved` for the card editors,
  build-once + `dataset.node` guards for the chat-head editors — see
  web/index.html). Any new polled UI element must respect this.
- **Notes are mutable documents with optional, isolated git plumbing.**
  Each note lives at `~/.scimux/notes/<id>/note.json` (per-note folder so a
  note can later be an independent repo; delete archives the whole folder to
  `notes/archive/<id>.<stamp>/`). The JSON file is always the source of
  truth. Optional versioning shells out to the **`git` binary** (same pattern
  as tmux — **never a Go module**, respects zero build dependencies) into a
  private `notes/<id>/.git` with forced identity
  (`-c user.name=scimux -c user.email=scimux@localhost`); it never opens the
  user's own repos or depends on their global git config. Commit boundaries
  are structural mutations and client-signalled body-edit completion
  (`section.commit`); mid-edit debounced autosaves do not commit. If `git`
  is absent or fails, note operations succeed unchanged — degrade silently.
  This **reverses the earlier deliberate "no version control" stance** for
  notes only; do not remove it as an accidental invariant violation, and do
  not expand it into a history UI without an explicit phase. There is no
  legacy flat-file migration path.
- **Frozen characterization suites.** These four are frozen at commit
  `4e35aad`: `internal/app/router_characterization_test.go`,
  `internal/app/static_characterization_test.go`,
  `internal/app/web_js_static_test.go`, and
  `internal/app/css_cascade_test.go`. Adding assertions is fine. Needing
  to CHANGE or DELETE one is a review stop, not an edit — it means the
  refactor has altered behaviour the suites exist to pin. The freeze names
  a commit rather than "as written" because commit `0848836` deleted a
  `css_cascade_test.go` assertion (`.stagechip .spin i { animation: none; }`)
  and replaced it with its opposite, deliberately: indeterminate progress
  must keep animating under Reduce Motion. That reversal is pre-freeze and
  arrived via merge from `2026-08-11_scimux-testing`, so it is not a
  protocol violation, but it is why the freeze needs a commit to point at.
- The README's **Non-goals** section is a hard scope fence; features listed
  there need explicit maintainer approval, not code.

## tmux gotchas (learned the hard way)

- Pane-level commands (`capture-pane`, `paste-buffer`, `send-keys`) need the
  target written `=name:` (trailing colon). Bare `=name` resolves only for
  session-level commands (`has-session`, `kill-session`). Observed on tmux 3.6.
- Prompts are delivered as a single paste (`load-buffer`/`paste-buffer`),
  then a separate Enter. `send-keys` with raw text would re-interpret
  newlines as submissions.
- That paste is bracketed (`paste-buffer -d -p`). tmux translates a buffer's
  newlines to carriage returns, which are indistinguishable from Enter, so
  without the markers a receiving TUI can only guess from arrival speed
  whether a block was pasted or typed — under load that guess fails and one
  prompt lands as several submitted messages (observed against claude
  2.1.224). tmux emits the markers only when the application has requested
  bracketed-paste mode, so `-p` is inert for a wrapped `cat` or `bash`
  (pinned by `TestBracketedPasteIsInertWithoutRequest`).
- Line endings are normalized to LF before `load-buffer`
  (`normalizeNewlines`). Because tmux rewrites LF to CR on paste, a CRLF pair
  would survive as CR CR — two line breaks where the author wrote one, which
  both corrupts the prompt and breaks first-turn delivery confirmation
  (the pasted text stops matching `canonicalPrompt`). Only line endings are
  touched: leading/trailing whitespace, deliberate blank lines, and interior
  spacing are the user's text.
- Structured transports keep their protocol-owned first-turn delivery. Owned
  Claude launches with `--remote-control` but without a positional prompt;
  scimux waits for a valid SessionStart from that exact launched process,
  pastes the first prompt once, and accepts only the matching transcript user
  turn as confirmation. The pale launch bubble stays pending until that
  confirmation. A SessionStart, trust, or delivery failure is an inline error
  that preserves the prompt as a draft and never opens the terminal or retries.
  Later tmux prompts use the same single-paste-then-Enter mechanics.

## Fixtures and privacy

- `internal/transcript/testdata/real-*.jsonl` are captured from real CLI
  runs, are **gitignored, and must never be committed**: even scrubbed they
  are personal environment snapshots. Regenerate with
  `scripts/capture-fixtures.sh` (runs one tiny prompt per agent, then scrubs
  system prompts, local tool inventories, and timezone).
- `internal/acp/codex/testdata/real-*.ndjson` and
  `internal/acp/testdata/real-*.ndjson` (Grok ACP wire captures) are likewise
  **gitignored**. Regenerate Grok with `scripts/capture-grok-acp-fixture.sh`
  (one tiny prompt over `grok agent stdio`, then drops vendor notifications,
  skill/path inventories, hostnames, session ids, and secret-shaped strings).
- The committed fixtures (`claude-session.jsonl`, `codex-rollout.jsonl`,
  `internal/acp/codex/testdata/synthetic-session.ndjson`,
  `internal/acp/testdata/synthetic-grok-turn.ndjson`) are fully synthetic.
  Keep them that way; never paste real transcript or wire content into them.
