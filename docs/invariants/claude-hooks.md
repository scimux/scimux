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
*scimux-owned* Claude node; Codex, ACP, pi, opencode, grok and cursor keep
the ordinary inspect/fallback behavior.

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
  files. An exact pre-session workspace-trust dialog is instead surfaced as
  an audited `y`/`n` web decision; generic lettered menus remain inert.
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
- Two consequences to know before "fixing" a bug report: delivery timing is
  never transcript-ownership evidence — Claude may hold a paste behind a
  local modal and append it to the same transcript later, so only a validated
  successor SessionStart may replace a binding — and because `settings.json`
  bakes `os.Executable()`, a pane that outlives a *move* of the binary keeps a
  settings file pointing nowhere, so its next `/clear` silently fails to bind
  (rebuilding in place is harmless). A just-created successor may also hold
  only meta records; "no recognized content yet" is never staleness
  (`maybeRelinkTranscript`).
- **`/clear` is refused while a turn is in flight**, with the same
  `errClaudeTurnInFlight` 409 every other Claude send gets. Claude Code
  absorbs a mid-turn paste into the running turn — its own transcript names
  this, `absorbed_mid_turn` — and an absorbed *slash command* evaporates
  leaving no record at all, while the paste still reports delivered. The
  fence reads the accepted-turn nonce and must never *claim* one: a page turn
  is not a turn, and `beginClaudeAcceptedTurn` mints and persists a nonce.
  Pane liveness is deliberately not the predicate either — approval dialogs
  are mechanically quiet and a `/clear` at one is legitimate.
- **A Claude page turn is authored by the hook, never by the send.**
  Delivering `/clear` proves only that a paste was accepted, and retirement
  is irreversible — it tombstones the transcript path and session id for
  good, so a page turn taken on the delivery report alone strands a live
  session's mirror for the rest of its life; a live session was stranded this
  way. So the send does nothing but note the time
  (`noteClaudeClearSent`), and the whole page turn — retire the old link,
  tombstone it transactionally with the successor binding, append the
  path-less seam — belongs to `commitClaudeBinding` under `SessionStart
  source:"clear"`, which already did exactly that for a `/clear` typed
  straight into the pane. Nothing else needs to be durable: the hook event
  lands in the bundle inbox, so a restart mid-wait loses only the notice.
  That notice is the other half — a `/clear` that never turns a page must not
  be silent, so `reconcileClaudeClear` gives up after `claudeDeliveryGiveUp`
  and says so inline. **Never retry `/clear`**: a retry that races a
  late-arriving rollover pages twice and throws away a turn the user has
  already spent.

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
SessionStart. SessionStart timeout and paste failure are inline errors that
restore the prompt as the node's draft and never retry. An exact
workspace-trust dialog is the exception: it raises `attention:"dialog"`
immediately, so the user can answer through the ordinary audited remote-key
route, and extends the SessionStart wait to `claudeDeliveryGiveUp`; expiry
then becomes the inline error. A generic lettered startup dialog is **not**
diagnosed as workspace trust and retains the ordinary short timeout.

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

## The usage status line is not a hook, and never rides a supervised pane

Claude states subscription quota (`rate_limits.five_hour`, `.seven_day`)
nowhere except the JSON it pipes to a configured `statusLine` command. Reading
it replaced an OAuth call that read the CLI's stored credentials file and
spoke to an undocumented endpoint with the user's own bearer token; that path
was **deleted, not kept as a fallback**, because a path that reads a token is
a path that can be asked to read a token. Do not reintroduce it.

- **The status line goes on a throwaway probe session only.** A status line
  replaces Claude's own footer, and with it the `esc to interrupt` anchor
  `internal/dialoghint` and the poller read to suppress false attention.
  A controlled comparison showed that the anchor disappeared with a status
  line installed. Installing one on a supervised pane
  trades supervision — the product — for a gauge. `docs/invariants/attention.md`
  owns the anchor; this rule is why it is still there to own.
- **The probe is not a hook bundle.** It proves no ownership, binds no
  transcript and answers no approval, so it gets its own tiny settings file
  with a `statusLine` key and nothing else: no `hooks`, no
  `capabilities.json`, no bundle subdirectory, no entry in
  `claudeHookBundleSubdirs`. Nothing about it is fail-closed-capability
  gated, because there is no capability to prove.
- **This helper prints, and every other one must not.** Its stdout *is* the
  status line, so `FuzzClaudeHookStdin`'s "writes nothing to stdout" contract
  does not cover it and must not be widened to. `FuzzClaudeUsageStatusLine`
  states the matching contract instead: stdout is exactly one fixed literal
  for every possible input, never a byte derived from the snapshot — which
  carries `cwd`, `transcript_path` and workspace paths.
- **A blank reading never erases a good one.** `rate_limits` is absent until
  the session's first API response, so the status line fires several times
  with nothing in it. Only a snapshot with a usable window touches the marker;
  the marker's *arrival* is the probe's completion signal, which is why the
  probe deletes it before launching and why a stale marker is never served as
  a fresh reading.
- **The argv is a measured cost control, not a style choice.** Cold-cache
  comparisons showed that successively removing tools and setting sources,
  replacing the system prompt, and carrying the prompt in argv reduced the
  input cost to a small fraction of a default TUI launch. Repeated probes did
  not visibly move the displayed quota gauge. Dropping a flag can therefore
  multiply the cost of measuring a budget by spending it. Two things
  that look like options are not: headless `claude -p` never invokes a status
  line (so the probe must be a real TUI), and `--bare` forces
  `ANTHROPIC_API_KEY` auth (so it would measure the wrong account).
- **The gauge is off until the user says otherwise.** `claude_usage_checks`
  in `~/.scimux/settings.json` gates `collectClaudeUsage` *first*, before the
  live-node check and before anything is launched. The gate is on the side
  that spends, not on the browser that renders, and every failure mode reads
  as off — a missing, unreadable or nonsense settings file is "no consent
  given", never consent. This is the one store here whose degrade direction is
  a promise rather than a convenience. `errClaudeUsageOff` is a distinct
  sentinel and surfaces as `"off": true` on `/api/usage` because it is the
  only dark gauge a tap can fix: the browser must tell it from a failure
  without matching on English, the same reason `#m_pair` has `#m_pair_note`.
- **The probe namespace is reserved, so a probe is never a stranger.** Every
  probe session name starts with `probeSessionPrefix`
  (`claudeUsageProbeSessionName`), and `isProbeSession` keeps it off the
  adoption surface: out of `/api/state`'s `unadopted` list and refused by
  `/api/adopt`. That list is subtractive — every session on scimux's socket
  that no node accounts for — so a probe with no reserved name is
  indistinguishable from one the user made by hand, and each reading flashed
  an "unadopted tmux session" card that appeared and vanished by itself. A
  prefix rather than a lifetime entry in `a.reserved` because it also holds
  for a probe leaked by a killed scimux: adopting one would bind a node to a
  pane launched with no tools, no user settings and scimux's own status line.
  A new probe shape gets its name from the same generator or it reintroduces
  the flash.
- **A probe is justified by an open Claude session, nothing else.** The
  refresh policy in `usage.go` is the whole schedule — prompt-driven, at most
  one reading per `usageMinInterval`, never a wall clock. `hasLiveClaudeNode`
  is the extra gate: a Codex or Grok prompt drives the same refresh cycle, and
  without it a user not touching Claude would be billed for a Claude reading.
  When the user is genuinely rate-limited the probe turn is refused, so the
  gauge goes dark exactly when it matters most; that is honest ("usage
  unavailable"), and inventing a number from a failed turn would not be.

## The model catalog is read, not asked

Which concrete model ids this account can launch used to be a `claude -p`
turn: the model was asked to recite a list the CLI already holds (measured
20,146 input tokens, ~$0.02, on whatever model the user's settings defaulted
to, so an Opus default paid several times that). That was the wrong
instrument, and it is deleted — `claudeModelPrompt`, `parseClaudeModels` and
`probeClaudeModels` are gone. Do not reintroduce a billed model probe.

- **A launch with no prompt costs nothing.** `claude --model <candidate>`
  renders its status line before any API request, and the status line states
  the model the CLI *resolved* — `--model sonnet` comes back
  `claude-sonnet-5`. Measured over nine launches: zero API calls, zero tokens.
  The whole property rests on submitting nothing, which is why
  `claudeModelProbeArgv` carries no prompt and no `-p`
  (`TestClaudeModelProbeArgvSubmitsNothing` is the fence), and why headless
  `-p` is not an option anyway: it renders no status line at all.
- **`display_name == id` is the validity oracle, and the only one there is.**
  A known id comes back with a friendly name ("Opus 5"); an id the catalog
  does not know echoes itself. It is not decoration: on 2.1.267 `--model opus`
  still expands to `claude-4-6-opus` — family after version, an id the API
  refuses — which is the whole reason scimux resolves ids rather than passing
  aliases through. `claudeModelIDPattern` is anchored so that form can never
  match.
- **Two rounds, and the second one costs a session.** Aliases first
  (authoritative: the CLI resolved them itself); the `/model` picker only for
  what the aliases could not answer. A family that survives neither round is
  *dropped*, never guessed — the launcher then passes the bare alias, which is
  what scimux did before any of this existed. Display names are lossy
  (`claude-fable-5` and `claude-fable-5-1` both render "Fable 5"), so a
  picker-derived id is a candidate that must be probed and family-checked
  before use: an answer from another family would relabel a model, and
  choosing "opus" in scimux would quietly launch Sonnet.
- **Opening the picker is a read.** The picker's own footer says Enter sets
  the default, so no key is ever pressed on a row and the session is killed
  with the picker still open, which changes nothing persistent. The only thing
  submitted is `/model` itself, exactly once — a second one would land in a
  picker that did open, where it is text in a filter box. Readiness is the
  model marker's arrival, not a sleep and not a guess at the TUI's chrome:
  the status line renders when the session is up, so that file is mechanical
  proof a paste will land.
- **The picker gets a probe directory of its own** (`claudeModelPickerDirName`)
  and that is what makes the previous bullet true. It is the only probe whose
  readiness is a marker's *existence*; a candidate probe reads the marker's
  contents, so a sibling's leftover fails the family check. Measured on
  2.1.267: a candidate probe's status line writes the shared marker once more
  *after* its session is killed, landing after the picker cleared the
  directory and indistinguishable from the picker's own. Readiness fired
  before the picker's TUI existed, the one `/model` paste was discarded, the
  capture spent its whole 20 s on a pane that would never draw a picker — and
  the run dropped `opus`, the single family the picker exists to name. Do not
  merge the directories back: clearing a file you do not own is not
  ownership, and the paste is deliberately not retried.
- **The cache is keyed on the CLI's version, with time as the backstop.** New
  ids arrive with a new claude build, so `claude --version` (a local
  subprocess that spends nothing) is the cheap gate on the expensive half.
  `claudeCacheTTL` is a day rather than the old week because there is no
  longer a bill to amortize — it exists only for a scimux that outlives a
  release without restarting. An *unknown* installed version is not a
  mismatch: it means the question could not be asked, and a stored answer
  inside the backstop beats none.
- **The expensive machinery is installed, not constructed.**
  `installClaudeModelProbe` runs on the serve path only, so `ensureClaudeModels`
  is inert in every test and from every handler that has no resolver. That is
  what lets `handleAgents` and `handleHarnessLatest` ask for a refresh at all
  without the suite ever launching a real `claude`.
