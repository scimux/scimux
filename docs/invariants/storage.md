# Storage, history and provenance invariants

Part of the invariant set in `AGENTS.md`. Read before changing transcript
parsing or mirroring, node persistence, session logs, chat/history reads,
Send-to or bookmarks, clear/fork behavior, or notes and their git versioning.
The root guide carries the project-wide privacy and benchmark restrictions.
Claude lifecycle changes also require `docs/invariants/claude-hooks.md`.

- **Defensive transcript parsing.** The CLI log formats are undocumented
  internals: unknown record types and shapes are silently ignored, never
  errors, and a transcript that is missing or stops making sense degrades the
  UI to the pane snapshot ("peek"). Keep that contract in
  `internal/transcript`. Two non-obvious parts, both held by `FuzzParseLine`:
  - "Unknown shape" includes an unknown **field value**: `ParseLine` yields a
    turn only for roles `user` and `assistant`, in both the Claude and Codex
    branches. Not tidiness — `mirror.go` writes `Turn.Role` straight into
    `sessionlog.Event.T`, a closed set including `source`, the `/clear`
    page-turn marker, in a store that is never rewritten, so a role waved
    through becomes a counterfeit seam.
  - A Claude record states its role twice, so there the test is **agreement**,
    not membership: `message.role` must match the envelope `type`, and a
    self-contradicting record is ignored though both values are individually
    legal. Either direction misattributes the turn, and because the
    scaffolding filter keys on `role == "user"`, a `type:"user"` record
    claiming `role:"assistant"` would render an injected `<user_instructions>`
    block as agent prose.
  - Claude Code 2.1.278 wraps a bracketed terminal paste in a transcript-only
    `<pasted_content id="…">` envelope. That is a real user turn, not injected
    scaffolding: unwrap it only when the scalar envelope is exact, the bounded
    safe id matches its non-XML closing tag, and nothing follows it. Then run
    the ordinary scaffolding guard on the unwrapped body. A loose exception to
    the "user text beginning with `<`" rule can expose injected instructions;
    rejecting the wrapper strands every scimux-delivered Claude prompt.

- **Append-only store.** `~/.scimux/nodes.jsonl` is replayed at startup;
  corrections are new records, never rewrites.
- **Installation identity is a public label.** The data directory holds
  `installation.json`, written once when the muxer first owns that directory.
  The ID is not a credential and must never be accepted as proof of identity.
  A damaged file is never regenerated automatically. Deleting the file yields
  a new ID on the next start.
- **Joined team servers are credentials.** `team.json` holds the token of
  each server this seat has joined. Only the muxer reads and writes it, and
  a write happens only from an explicit join, forget, or changing this seat's own name on a server.
  A damaged file is never overwritten.
  It is untouched until a server is joined.
- **Shared chats name a server and who may read them.** `team-shares.json`
  holds each share's server, scope, members, and delivery cursor.
  It holds no chat content and no token. Only the muxer reads and writes
  it. It is untouched until a share exists.
  The sharing UI selects people without a permission dropdown. Newly invited
  members receive `write` (view and comment). Keep the underlying `read` and
  `write` permissions and their enforcement; saving unrelated membership
  changes must not silently upgrade existing read-only members. Membership-only
  saves are serialized with share mutations and the outbox, retain current
  permissions and unrelated new members, preserve concurrent removals of
  baseline members, and check UID and generation. Queued outbox work refreshes
  its share under the same gate before wire work; a missing, damaged or replaced
  identity cannot upload or alter another generation.
- **Early native discussion stays accessible.** A comment accepted at an
  initial empty frontier can predate the first native seam. When there are no
  earlier agent turns and thus no history control, display that discussion in
  the owner's normal timeline, using the saved log UID and frontier rather
  than writing the comment into the session log.
- **Sharing never delegates agent control.** Recipients MUST NEVER send to
  the sharer's agent, regardless of their sharing permission. `read` means
  view; `write` means view and comment to people only. Received comments stay
  in the human discussion and MUST NOT become agent input automatically.
  Only the chat owner may select **Use as prompt**, review or edit the text
  in their own agent composer, and explicitly send it to their own agent.
  Selecting the action only prepares a draft and selects Agent; it never
  sends. Recipients have neither this action nor an Agent destination for
  the shared chat. Enforce the boundary in request handling and routing,
  independently of hidden browser controls; retries, reconnects and tool
  auto-approval never authorize forwarding. Preserve original authorship
  and applicable AI provenance when the owner forwards text.
- **People reactions are append-only, derived decorations.** The unchanged
  relay authenticates the author and stores/forwards the opaque versioned
  application record `{v:1,t:"reaction",target,emoji,active}` in its existing
  comments log. scimux derives the participant only from the authenticated
  envelope, validates the earlier target against a retained valid People
  comment, and rejects self-reactions. Replaying retained events in log order
  uses the latest valid event for each target, participant and fixed emoji.
  Remove appends an explicit inactive record; it never rewrites history.
  Reactions render beneath the exact source comment and remain separate from
  ordinary `comments.entries`, authored text, bookmarks and prompt forwarding.
  They are polled and paged with existing orchestration; no push channel or
  independent timer is introduced. A response acknowledgment is not evidence
  of state: only a matching validated retained event confirms the desired
  state. Uncertain writes retain their exact body and idempotency key for an
  explicit retry and are never replayed after reload. Reaction-only records
  are neutral for upgraded unread classification, without advancing `/seen`;
  unexplained gaps remain unknown. Retained reactions stay readable offline
  and after share retirement.
- **Use as prompt retains its source.** In the unified shared-chat UI, the
  owner-only action marks its source bubble using the existing bookmark
  marker placement and visual style, with a distinct prompt icon and
  accessible label. Address the source by its durable message identity,
  never by matching edited prompt text. The marker appears when the action
  prepares a nonblank Agent draft and survives partial edits. Completely
  erasing the draft cancels that pending source association; later unrelated
  typing does not restore it. Draft restoration and polling preserve the
  association. Distinguish a prepared draft from confirmed agent delivery:
  selecting Use as prompt is not evidence of a send. A confirmed send keeps
  a persistent source marker linked to the resulting prompt, including when
  the owner edited it. Clearing a later draft cannot erase earlier confirmed
  links. Failed or unconfirmed sends must not be labelled as delivered.
  HTTP acknowledgment, queue state, busy state and text alone do not prove
  delivery. Require assistant evidence for the exact queued address before a
  later user turn or error; keep ambiguous outcomes awaiting. Retained
  comments may be prepared offline or after sharing ends only while their
  owner's local node remains open.
- **Guest subscription state is metadata.** `team-subs.json` holds the identities,
  names, access, state and mirror and comment counters and read positions, with no chat content
  or token. Removing a shared chat deletes its subscription entry. Only the
  muxer writes it; it is untouched until a subscription exists.
  An owner subscription may retain its local node and exact share-start
  segment/record frontier, without message content. The node is exposed only
  after verifying its current session-log UID against the subscription.
  The frontier survives sharing retirement and is removed with the subscription.
- **Comment author names are metadata.** `team-people.json` holds at most 4096
  last-known names per server and seat for retained comment authors, without
  chat content or tokens. Current roster names take precedence. Removing the
  last subscription for a server removes its names. A damaged file is never
  overwritten; it is untouched until a subscription exists.
- **Guest copies are retained received history.** `team-copies/` holds received
  mirror and `comments.jsonl` entries, including for this seat's own shared chats,
  unchanged and append-only, including after
  sharing ends. Removing a shared chat deletes its copy, and a cut-off last
  line is removed during repair. Copies count toward total storage usage and
  belong to no node.
  They are never sent anywhere or logged; they are untouched until a
  subscription exists.
- **Local People inventory is read-only.** Inventory checks the exact persisted
  copy identity under copy and serialized budget locks, plus the current local
  installation for pending authors. It does not establish a missing identity,
  consult current relay authority, or expose stored file bytes. Bounded cursor
  pages report damaged records as unavailable and never treat completed or
  retired publications as active. A cursor is not a stable snapshot; clients
  restart enumeration to see earlier concurrent additions.
  Exact-key review distinguishes pending, full retirement, typed absent
  retirement, completion and absence. Completion exposes its sequence only
  after validation against the retained comment; if the pending record still
  pins stages, retained bytes must also validate. Review never removes that
  pending record or releases its pins. Removed bytes remain compatible with a
  validated unpinned receipt, while corrupt retained records fail closed.
- **People files remain bound to their copy and message.** Managed file stages,
  pending publications, receipts and retained bytes belong under the associated
  `team-copies/` identity and count toward the global budget. A local copy
  instance survives restart and share retirement, but changes when that copy is
  removed and recreated. A visible identity is returned only after its containing
  directory is synced; a failed sync remains unavailable until a later retry
  establishes durability, without replacing the identity. A published file is
  resolved through its copy, log, sequence and logical attachment ID; a hash
  alone grants no access. Share
  retirement preserves previously retained bytes, while explicit copy removal
  removes that copy's managed data. An independent reference-media capture has
  its own retention. File drafts and unconfirmed publications require explicit
  review; recovery never sends them to an agent or automatically republishes.
  Local stages and frozen publications use private records with bounded
  temporary writes and atomic admission. A visible stage is usable for upload
  or retry only after its directory sync succeeds; a later read retries that
  sync without replacing the record. Under the copy lock and then the
  global budget lock, recovery removes recognized abandoned temporary files
  before reserving space; a failed admission leaves existing records intact.
  Explicit local stage removal checks the current copy identity and exact stage
  revision under the copy and serialized budget locks. It refuses pinned or
  changed stages, and repairs the stage directory barrier before acknowledging
  an already absent stage on retry. No current relay or native-owner authority
  is needed for this local maintenance.
  An absent file-publication key can be retired under the same copy and budget
  locks using a durable marker bound to the current copy and installation.
  Admission refuses a retired key. Existing pending or completed values require
  exact review, and a failed or uncertain marker write preserves recovery. The
  marker and its atomic-write peak count against the normal storage limits;
  terminal markers remain until copy removal. Retirement does not remove
  staged bytes or retract a dispatched comment.
  A pending publication pins its stages until an exact retained comment and
  associated bytes are confirmed and a completion receipt is durable. Each
  shared-chat copy has two caps. Kept conversation files under `people-records`
  are limited to 128 MiB and 2048 files. This seat's working files under
  `people-stages`, `people-pending`, `people-completed`, and `people-identity`
  have the same limits, checked only against that group. Kept `people-records`
  bytes are outside global managed usage. The minimum-free reserve still applies
  to every write, including a kept-file write. A budget refusal for a kept copy
  still serves the verified bytes and leaves the file unkept. The same refusal
  confirms the owner's already appended comment and leaves that file unkept.
  A different error saving the kept copy keeps the pending pin.
  No network I/O occurs while these locks are held.
- **Browser drafts are separate from retained copies.** The browser's local
  file store holds raw People and prepared source files for review across
  navigation and reload, with a 16 MiB per-scope limit and a 256 MiB/4096-file
  global limit. Text and pending identifiers are stored separately. Removing
  a local chat copy removes its server-managed data; older browser drafts are
  offered for explicit review when available and are not automatically adopted
  by the replacement. Each newly staged file retains its original server stage
  revision and a browser file identity within its copy and binding scope.
  Cleanup uses that revision and refuses to remove a replacement browser file.
  Older rows without a revision remain saved until their local stage can be
  explicitly reviewed. Browser data removal and individual draft cleanup are
  separate from copy removal and independent reference-media captures.
- **Budgets refuse writes; they never rewrite retention.** Managed growth
  (`nodes.jsonl`, session logs, attachments, and asset blobs) is guarded by a
  default 64 MiB free-space reserve plus optional global/per-node byte budgets
  from server-owned settings. Muxer and worker processes serialize the check
  with the same owner-only storage lock; live-to-archive renames take that lock
  too, so a scan cannot miss bytes while they move. The per-node budget counts
  only that node's live session log, attachments, and asset blobs:
  `nodes.jsonl` remains subject to the global and free-space limits so a full
  node can still record audit evidence, be renamed, or be deleted. Hitting a
  limit is an explicit write error and an observable `/api/state` storage
  condition; scimux never silently truncates or deletes durable history to get
  back under budget.
- **Reference media is immutable global storage.** A new bookmark containing
  managed raster-image markers first publishes content-addressed blobs and a
  canonical capture manifest under `reference-media/`. The manifest freezes
  the exact source `(uid, segment, record)`, projected turn text, and ordered
  media descriptors; note snapshots carry only the public capture descriptor,
  never bytes, paths, or blob digests. Captures are charged to the global
  storage budget (including staging overhead), never a source node's budget.
  Source, bookmark, reference, and note deletion deliberately do not collect
  these immutable files: another bookmark, note version, or copy may still
  refer to them. Content deduplication and global limits bound new growth, but
  interrupted work may retain an unreferenced blob. A complete backup of notes
  with captured images includes `reference-media/`; a copied `note.json` is not
  independently portable media. Historical records without a capture may use
  an exact-address, read-only live preview for display, but that is best-effort
  compatibility rather than retroactive durability.
- **One session-log store, one schema.** Every transport writes its per-node
  history to `~/.scimux/sessions/<node-id>.jsonl` as `internal/sessionlog`
  events — plain JSONL, because the corpus must stay grep/sed/awk-able. The
  filename is the node's reusable title slug; identity lives in the file's
  `meta` header record, and deleting a node archives its log so a reissued
  slug can never append onto dead history. tmux nodes reach the store through
  the transcript mirror (`mirror.go`), with `source` seam records marking
  every transcript (re)bind as the dedupe watermark. New transports write the
  same records to the same directory: no per-transport formats or directories.
  `~/.scimux/sessions/` is user history and a multi-vendor output corpus.
  Deterministic heuristics may read it. It must never be used to train,
  fine-tune, distill, or otherwise develop a model — do not turn it into a
  training dataset.
  - Ordinary disarmed Claude tool calls keep digest-only permission evidence.
    A pending human question or plan may hold its original bounded payload
    transiently until the helper deadline. An unanswered `AskUserQuestion`
    also has a normalized private standing record so its question can remain
    visible after raw payload retirement; the record is settled by matching
    transcript evidence or an explicit session lifecycle boundary. This
    side store has a fixed 256-record, 8 MiB per-bundle cap and removes only
    resolved records when admitting a new question. After the
    helper deadline the owning worker removes the raw request and response
    copies, including answered, canceled, handed-off, and uncertain ones, and
    leaves request-specific replay fences. A native or manually controlled
    key writes an unconfirmed fence before delivery; a failed or ambiguous
    send remains non-retryable across terminal handoff and fresh notification
    epochs. Manual controls are bound to the current dialog epoch captured at
    handoff. Marked helpers that exit early release ownership and their
    payloads through the same recovery path. The durable manual decision is a
    bounded append-only exception on the existing decision record: request
    identity, reviewed content and its digest, the human's selections or
    feedback, any chosen execution mode, and the delivery outcome. The initial
    row records intent, without claiming delivery. A later validation or
    publication failure appends a correlated application-authored outcome,
    never a rewrite of the intent row. Its source is `application`, with only
    delivery status and fixed explanatory context in `outcome`, correlated by
    `request_id`. It repeats no human action, feedback, selections, or reviewed
    content, and stores no raw error text. Known stale/expired rejection is
    `not_delivered`; an I/O error cannot prove non-consumption and stays
    `unconfirmed`.
    Application context is stored apart from human feedback. The record is
    not a new conversation role, a fake user prompt, a seam, or a rewrite of
    an earlier row, and it is not a license to log raw hook input for
    ordinary tool calls. The question and plan text come from the hook
    payload the helper already held. `planFilePath` is not read.
  - Agent file imports record the file's actual basename and extension; link
    labels and image alt text are descriptive only and never authorize inline
    rendering. Chat projection keeps the agent's link label as display text,
    falling back to the recorded file name; the browser shows it inline as plain
    text, and tiles always use the recorded name. Attachment markers inside
    fenced code blocks are shown verbatim in chat, matching ingest and
    projection, which never treat fenced references as attachments.
    Failed explicit attachment references are durable additive
    `asset_import` events keyed by their owning turn record and occurrence.
    A link whose target currently exists and is not a regular file (such as a
    directory) is a reference, not an attachment: it records no `asset_import`
    and renders as written.
    A later retry appends a normal asset event with an explicit earlier-turn
    anchor; it never rewrites conversation text or moves the import to the
    newest turn. Reference identity and asset identity remain first-record
    wins. The retried snapshot contains the file's current bytes, not a claim
    about bytes that existed when the original reference was written.
  - Cursor records two representations of the same launch choice: the node
    record keeps the model row and effort level the user selected, while the
    session-log meta header stores the joined cursor id as `model` and an empty
    `effort`, matching the concrete request sent to the worker.
  - The store is also the **chat read path for every transport**: handleChat
    renders the current segment (everything after the last `source` seam),
    while the tailer serves only mechanics — needs-input, staleness, delivery
    confirmation. Earlier segments are readable on demand, never polled
    (`?history=1`, `sessionlog.ReadHistory`, behind a "show earlier history"
    tap). Keep that split: history in the poll payload would ship the whole
    corpus every second, and it can repeat turns across mechanical seams
    because a rotation re-mirrors from turn zero.
    The browser polls a tail window of the current segment (the last 50 turns,
    snapped to the prompt that starts their cycle and capped at 150); earlier
    segment turns and earlier segments are explicit reads. Sent echoes and
    awaiting-forward latches use turn addresses so their boundaries survive
    the window moving.
- **An agent's Output is never passed off as the user's.** Two places can
  break this, and both are quiet. Send-to carries one agent's Output into
  another agent's Input, and the receiving node records the arrival as an
  ordinary user turn — so scimux, not the user, would be the party asserting
  human authorship. `bookmarks.js` owns the one marker (`AI_DISCLOSURE`,
  applied once inside `openSendTo`); every caller must hand over the role it
  knows, and a caller that forgets disables the disclosure without a symptom,
  which is why bookmarks stamp `role` at capture. Provenance is also the named
  exception to defensive parsing: discarding an unknown record type is right,
  discarding an unknown *provenance* field is alteration rather than
  degradation, and `sessionlog` is never rewritten, so the loss is permanent.
  Two deliberate limits, neither an oversight: the marker fires on evidence
  only — an unknown role passes through untouched, because stamping "an AI
  wrote this" on the user's own words is the same misattribution pointed the
  other way — and the clipboard is outside the rule entirely, because there
  the user copies, pastes and attributes, and scimux is upstream of that.
  Send-to destination drafts keep their source intent in device-local storage;
  `forward_links` enters shared `ui.json` only after the sent text is matched to
  a real destination transcript turn -- which is the sent text itself, or that
  text plus the attachment reference the server appends to what it delivered.
  Editing the draft does not break provenance. Failed or unconfirmed sends
  remain pending, while leaving an empty destination cancels the pending
  intent. The awaiting latch expires after a day, because a latch that never
  matches would otherwise hold both the cancel guard and the overwrite guard
  open forever and silently retire Send-to for that node. The shared navigation index
  retains at most the newest 500 links and omits destination text when a
  transcript UID or timestamp can address the turn; last-resort text fallback
  is prefix-bounded.
  Owner "Use as prompt" links use a distinct `prompt` kind in this same
  bounded index. Their full local or shared source address and author/AI
  provenance are retained; the destination is the exact queued UID, segment
  and record. A link is created only after the trace reports actual response
  evidence for that address. A bounded, whitespace-normalized preview of the
  submitted prompt may be stored on the link solely to label chooser entries;
  it is never part of the destination address or a navigation fallback. Older
  links without previews use numbered labels. For owned Claude, the address requires a verified
  complete transcript and mirror frontier and a unique later dated native user
  row in the unchanged binding; the existing worker remains the sole mirror
  writer. Missing, delayed or ambiguous evidence may leave a send unconfirmed. Preparing and awaiting associations stay
  in the device-local draft state, with at most 32 unresolved flights per node.
  An ambiguous outcome remains awaiting and stops trace polling; an interval
  error restores the captured Agent draft without claiming it was never accepted.
- **/clear = page turn, fork = fresh notebook.** `/clear` starts a fresh chat
  surface under the *same* node: same log file, an appended `source` seam,
  never a new file or truncation; the context gauge is segment-scoped.
  - ACP nodes (pi/opencode/grok/cursor/dsh/vibe/copilot) implement it as **deterministic
    process replacement** — kill the subprocess, negotiate a fresh one under the same
    node, because a second `session/new` on one connection is unproven
    upstream while a fresh PID self-evidently carries no context. The
    replacement is re-configured with the *same* launch configuration the node
    records — dsh, vibe, and copilot carry neither model nor thinking level on argv, so a
    replacement that is not configured again is a page turn that silently
    changes model; a dsh, vibe, or copilot refusal returns HTTP 400 and fails the clear before
    the seam, leaving the old session and its log untouched. codex opens a new
    thread on the same PID. In both, the seam is appended only after the
    protocol call succeeded. **Claude obeys the same rule**, and its proof is
    that node's own `SessionStart source:"clear"`: pasting `/clear` turns no
    page at all, because the CLI absorbs a paste that lands mid-turn and an
    absorbed slash command evaporates, while the paste still reports
    delivered. Retirement is irreversible (it tombstones the path and session
    id for good), so it waits for the hook, which retires the old link,
    tombstones it transactionally with the successor binding, and appends the
    path-less seam — the same work it already did for a `/clear` typed
    straight into the pane. A `/clear` that never lands therefore changes
    nothing and says so.
  - Fork is the only path that may change launch config: a forked node
    inherits agent/model/effort/dir but never conversation history. Claude's
    native `/fork` and `SessionStart source:"fork"` are unsupported — forking
    is scimux's Fork action only (fresh node/process, ordinary
    `source:"startup"`). Never send `/fork` to the Claude CLI.

- **Notes are mutable documents with optional, isolated git plumbing.** Each
  note lives at `~/.scimux/notes/<id>/note.json` (per-note folder so a note
  can later be an independent repo; delete archives the whole folder), and the
  JSON file is always the source of truth. Optional versioning shells out to
  the **`git` binary** — same pattern as tmux, never a Go module, so zero
  build dependencies holds — into a private `notes/<id>/.git` with forced
  identity (`-c user.name=scimux -c user.email=scimux@localhost`); it never
  opens the user's own repos or depends on their global git config. Commit
  boundaries are structural mutations and client-signalled body-edit
  completion (`section.commit`); mid-edit autosaves do not commit. If `git` is
  absent or fails, note operations succeed unchanged — degrade silently. This
  **reverses the earlier "no version control" stance** for notes only: do not
  remove it as an accidental violation, and do not expand it into a history UI
  without an explicit phase. There is no flat-file migration path.
