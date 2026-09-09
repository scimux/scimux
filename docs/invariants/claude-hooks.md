# Claude Code: the hook-owned agent

Part of the scimux invariant set (`AGENTS.md`), split out because it binds
only once you are inside the code it governs. **Read this before editing
`internal/app/claude_*.go`, the hook helper commands, `capabilities.json`
handling, or anything that launches, binds or retires a Claude pane.** Nearly
every rule here was written after a live failure, so a rule that looks like an
accident is the one most likely to be load-bearing: check here before
"fixing" it.

Claude is the one agent scimux drives through official hooks instead of
supervision heuristics, so it has its own rules. All of this applies to a
*scimux-owned* Claude node; Codex, ACP, pi, opencode and grok keep the
ordinary inspect/fallback behavior.

## A transcript is bound only by that node's own SessionStart hook

Every
launch mints a fresh hook bundle under `~/.scimux/claude-hooks/<hex-id>/`
(`settings.json` 0600, subdirectories 0700) and passes it as `--settings`;
that file registers scimux itself as the `SessionStart` command, so the hook
firing *is* both the process→transcript ownership proof and the
startup/hook-health acknowledgement. The undocumented transcript
`bridge_status` record is not a readiness condition.
- Flags: `--session-id` seeds the first transcript, `--continue` is
  forbidden, and `--settings` must precede `--remote-control` (an
  optional-value flag that would otherwise swallow it). Owned launches also
  pass `--ax-screen-reader` and an `--add-dir` per genuinely additional
  directory (including attachment staging). `--add-dir` does **not** bypass
  Claude workspace trust, and scimux never edits undocumented trust-state
  files.
- Only Claude's default `~/.claude` state root is supported;
  `CLAUDE_CONFIG_DIR` is deliberately not evaluated, and transcript-path
  validation stays anchored under `~/.claude/projects` unless that scope
  decision is explicitly revisited.
- Bindings are per-node and generation-numbered (`claude-binding` records,
  replayed *after* ordinary node records so a later title edit cannot wipe
  one); retired paths and session ids become `deadTranscripts` tombstones
  that can never be rebound for that node. Sources `startup`, `clear`, `compact` and `resume`
  are accepted and never become inspect attention; `fork` is rejected. A
  valid changed session/path is rebound under the full
  path-claim/tombstone/persistence/seam rules — never left silently on the
  old transcript — and the commit is transactional: a failed rebind keeps
  the previous binding in memory and in replay, and surfaces an inline error
  rather than inspect.
- Capability is **fail-closed**: no complete current bundle means
  `claude_unsupported` with an inline explanation — never inspect/fallback
  supervision, never an owner guessed from the pane cmdline. Adopted,
  incomplete, legacy and moved-binary bundles are unsupported.
  `capabilities.json` proves the layout from disk and records the `exec`
  path `settings.json` baked; a fresh bundle reports `claude_starting` and
  becomes authoritative only after that process's valid SessionStart.
- Two consequences to know before "fixing" a bug report: pane noise that
  trips the staleness backstop drops a node to peek until the next `/clear`
  or relaunch hook; and because `settings.json` bakes `os.Executable()`, a
  pane that outlives a *move* of the binary keeps a settings file pointing
  nowhere, so its next `/clear` silently fails to bind (rebuilding in place
  is harmless). Never retire a transcript on absent evidence — a just-created
  successor holds only meta records, so "no recognized content yet" is not
  staleness (`maybeRelinkTranscript`).

## The first prompt is gated on the hook, not on the transcript file

It is
pasted exactly once, only after a valid SessionStart, and its pale bubble
goes solid only when the matching transcript user turn confirms delivery.
The acknowledgement is deliberately the **hook arriving**, not the file
existing: the CLI creates that file lazily, so an idle launch has none for
SessionStart to name and the first prompt is what brings it into being —
gating the paste on the file is a deadlock. A SessionStart failing *only* on
the missing file therefore releases the paste gate (`claudeStartPending`,
fenced by session id) and nothing else; do not re-tighten it. The send gate stays held so no later prompt overtakes the
first.

## Delivery confirmation has two clocks

, because SessionStart attests the
*launch* while only the transcript attests that the prompt *arrived*.
`claudeDeliveryTimeout` (15 s) ends only the synchronous wait and expiry
there is neutral — no error, no draft restore, bubble stands, send gate
held, poller releases it when the mirrored turn appears. It becomes an
inline error, draft restored, only at `claudeDeliveryGiveUp` (180 s); that
outer bound must stay reachable, because a paste swallowed by a startup,
trust, login or rate-limit dialog never reaches Claude's prompt and patience
without a floor is a silent hang. The neutral window is projected as its own
`delivery: "delivering"`, never the ordinary `unconfirmed` — which offers a
resume button and claims the send is unconfirmed, a lie while SessionStart
has arrived; for the same reason the pending note names delivery rather than
SessionStart. SessionStart timeout, a diagnosed
workspace-trust dialog and paste failure are inline errors that restore the
prompt as the node's draft, never retry and never open the terminal; a
generic lettered startup dialog is **not** diagnosed as workspace trust.

## An approval is answered through the hook, never through the pane

The
bundle registers `PermissionRequest` (`__claude-permission-hook`) and
`Stop`/`StopFailure` (`__claude-stop-hook`) beside SessionStart.
`PreToolUse` is deliberately never registered (it fires for every tool call,
decision needed or not); `SubagentStop` likewise never.
- The hook is registered on every owned launch but *authorizes* nothing
  until a lease is armed: with no `perm/lease` marker it prints nothing and
  exits 0, so Claude falls through to its own dialog as if no hook existed.
  It is not silent, though — see the escalation notice.
- Claude offers the hook **no menu and no `tool_use_id`**, so the rendezvous
  is keyed by a hook-minted nonce (one blocked helper = one request = one
  answer), and "allow once" is an *omission*: emit `{"behavior":"allow"}`
  and never `updatedPermissions`, the only member that would persist a rule.
- Ordering that must not be inverted: arm flips in-memory state *then*
  writes the marker; disarm removes the marker *then* clears state, so a
  stale marker can only cost a tool call the helper's deadline, never
  authorize one; and a request is claimed by rename before it is audited,
  audited before it is answered. The marker is published only in the `armed`
  phase — Claude's substitute for the structured transports' enable cutoff,
  since a call that asked before the marker existed left no request to
  answer.
- A lease latches the first `prompt_id` it answers and declines requests
  from another turn (prompt_id is the CLI's own turn identity, shared by a
  parent and its subagent, so the fence holds even when the mechanical turn
  edge is missed). It lives on the lease, not the node, so a fresh lease is
  a fresh fence with no clearing step to forget.
- The two deadlines are deliberately different numbers: the helper waits
  `permHookDeadline` (5 s, generous — a loaded host runs the lane late), the
  app stops answering at the strictly shorter `permAnswerWindow`, because an
  answer written at the helper's last instant would be audited as an
  approval nobody read. A decline that failed *only* on that clock is
  reported; a policy decline stays silent, or every plan-mode call would cry
  wolf.

## Auto-approve is one turn, for every agent

Same label everywhere
("Auto-approve this turn"); it disarms at turn completion, stop/interrupt,
un-toggling, `/clear`, `/exit`, and the hard resets nobody chooses (process
or session loss, node delete, restart — the lease is never persisted).
Enabling *during* a Claude approval dialog still arms the current turn:
those dialogs are mechanically quiet, so `live=="active"` is the wrong
predicate — owing, an unresolved call, or a standing escalation notice means
the turn is still open.
- Claude's turn-completion edge is the official `Stop` hook, not pane
  quietness and not the next accepted prompt. A Stop notice settles a lease
  only if its lease id is still armed, it is inside its TTL, `session_id`
  matches, and its turn nonce matches the current or closing accepted turn —
  a late Stop from turn N must not revoke, clear or tombstone turn N+1.
  Codex/ACP keep their protocol settle. `capabilities.json` carries
  `"stop"`, so a pre-Stop launch is visible on disk and still auto-approves
  through the transcript fallback. `stop_hook_active` writes nothing — that
  turn is still running.
- Failure keeps authority: quarantine on session mismatch or TTL leaves the
  lease **armed**. The turn nonce is published durably before Enter and
  publication failure refuses the send; a second turn is refused until the
  prior matching Stop drained, so a delayed helper cannot relabel turn N as
  N+1. The poller republishes the arm marker only below half of
  `claudeLeaseTTL` (30 min), so Stop still sees it on a long turn without an
  fsync every tick.
- An auto-approve failure (deadline, policy, competing hook, upstream
  ask/deny) is an inline error only: while the lease is armed scimux
  publishes no map/card attention and does not open the terminal. An unread
  Error on the lease is dropped at the boundary, as for every agent — the
  session-log audit still has it. Disabling auto-approve reevaluates a
  still-current dialog epoch and only then publishes attention.
- AskUserQuestion, ExitPlanMode, plan choices, deny rules and unknown modes
  are **never** auto-answered.

## Strict terminal policy

Maintainer-approved. For a SessionStart-acknowledged node with the complete current bundle
(SessionStart, PermissionRequest, Notification(`permission_prompt`), Stop,
StopFailure, PreCompact, PostCompact, Elicitation, ElicitationResult), the
tmux terminal may be visible only when the user opened it with the terminal
button, or when auto-approve is unarmed and a current permission dialog is
structurally proven.
- Notification(`permission_prompt`) proves only that *a* dialog is visible —
  its payload carries no PermissionRequest id and no tool_use id, so scimux
  claims **no** exact request correlation and never parses pane text to
  invent one. The UI action is bound to a server-minted visible-dialog epoch
  (`perm_dialog_id`); a `/key` body carrying `dialog_id` is fail-closed
  (missing, retired or mismatched → 409, never typed into Claude's prompt).
  Notifications are fenced by the accepted-turn nonce so a closed turn's
  notice cannot mint an epoch for the next one.
- Keys and labels come only from a structurally validated current AX menu or
  another exact source; invented Yes / "don't ask again" / No rows are never
  shown. A standing ask's tool hint classifies `AskUserQuestion` and
  `ExitPlanMode` over a generic Notification title. If exact options cannot
  be extracted, the chat shows an inline manual-response state plus Open
  Terminal — no guessed buttons. While auto-approve is armed the map stays
  un-yellowed and the terminal does not auto-open, but the chat still shows
  epoch-bound controls, never generic unbound keys.
- Pane quietness, AX static rendering, unresolved transcript calls,
  missing/stale/unparseable transcripts, fallback chat and owing timeouts
  must never open the terminal automatically and must never emit
  "quiet · inspect terminal"; transcript and startup faults become inline
  status or error states. Compaction is not a permission dialog. A moved or
  missing scimux executable invalidates hook capability; Stop clears
  obsolete attention and terminal-forcing state.

## An escalation notice is evidence, never an answer

The hook writes
`perm/asked/<nonce>.json` for *every* decision it escalates, armed or not
(`internal/app/claude_asked.go`), because it knows the one thing no
transcript or pane reasoning can recover: whether a dialog is about to be
drawn. An unresolved `tool_use` over a confined animation is byte-identical
between a long-running tool and a dialog with parallel calls animating
behind it.
- Where the bundle proves the layout (`capabilities.json` gains `"asked"`, a
  gate independent of `"permission"` so older bundles degrade to *no gate*,
  never to *no attention*), the notice is the sole authority on the
  active-pane paths: a notice raises immediately, classified by tool name;
  no notice suppresses both the `animStallAfter` degradation and the
  attention-preservation window. Both gates also require the baked `exec`
  path to still be a runnable file, because a bundle whose binary moved
  writes no notices and its silence must never read as "Claude asked
  nothing".
- It also raises where nothing else can: Claude flushes the `tool_use`
  record only *after* approval, so for a first blocked call the waiting
  helper is the only proof the dialog exists. The quiet-pane fallback is
  deliberately **not** gated — rate-limit menus, trust-folder and login
  prompts fire no hook and reach a quiet pane.
- Retirement: by the helper when it auto-approves; by the **specific tool
  call** the notice announced; by recognized transcript **growth** as a
  fallback; and by a generous TTL as a leak backstop. Pane geometry is
  deliberately not a retirement signal.
- The per-call rule is primary and is an exact join: the notice stores a
  digest of the `tool_input` — never the input itself, because a disarmed
  session writes one per decision without the human having consented to any
  audit — and Claude writes the matching `tool_use` record only after the
  human answers, so that record proves the dialog closed. It retires
  regardless of what else is in flight, and each record is credited once
  (per-node watermark) so one approved call cannot drain notices for dialogs
  still on screen.
- Growth covers what no digest can join — a dismissed dialog, or an ask that
  never becomes a `tool_use` record (plan choice, question) — under strict
  guards: exactly one notice, oldest first, only a notice **older** than the
  newest dated turn (an agent that printed text and *then* asked produces
  the reverse ordering), and only with no unresolved call at all. Those
  guards also mean growth cannot carry the load alone — a working agent
  nearly always has a call pending — which is why the per-call join is
  primary. The watermark still advances on every tick, including ticks the
  pending set blocks: a frozen mark could not report the growth the *next*
  resolution writes.

## Compaction and MCP elicitation are passive chat UX

PreCompact/
PostCompact and Elicitation/ElicitationResult ride the same private bundle.
Their helpers never print to stdout/stderr and always exit 0, so scimux can
neither block compaction nor deny or rewrite an MCP exchange, and they
retain none of the sensitive payload (`custom_instructions`,
`compact_summary`, `requested_schema`, result `content`). Both states appear
on `GET /api/nodes/{id}/chat` only, never `/api/state`, and both
capabilities are part of the complete current bundle (older bundles are
`claude_unsupported` until relaunch).
- Compaction (`compacting: true`) shows a transient status with the working
  pulse on, composer busy, `reply_ready` false. It never feeds attention,
  mechanical liveness, map/card yellow, terminal forcing, a session-log
  event or a source seam, and does not change `SessionStart
  source:"compact"` binding.
- A current elicitation (`elicitation_waiting` plus bounded metadata) raises
  yellow `question` attention even while auto-approve is armed, is never
  auto-answered and never auto-opens the terminal; the chat explains the
  request and offers Open Terminal without inventing Yes/No or keypad
  controls. Same-identity SessionStart `compact`/`resume` leave it standing;
  `/clear`, Stop, StopFailure, interrupt, `/exit`, a changed-session rebind,
  process loss and node deletion clear it and tombstone unresolved
  asked/epoch records so a later turn cannot pair with them.
