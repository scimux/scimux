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
  - Agent file imports record the file's actual basename and extension; link
    labels and image alt text are descriptive only and never authorize inline
    rendering. Failed explicit attachment references are durable additive
    `asset_import` events keyed by their owning turn record and occurrence.
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
- **/clear = page turn, fork = fresh notebook.** `/clear` starts a fresh chat
  surface under the *same* node: same log file, an appended `source` seam,
  never a new file or truncation; the context gauge is segment-scoped.
  - ACP nodes (pi/opencode/grok/cursor/dsh/vibe) implement it as **deterministic
    process replacement** — kill the subprocess, negotiate a fresh one under the same
    node, because a second `session/new` on one connection is unproven
    upstream while a fresh PID self-evidently carries no context. The
    replacement is re-configured with the *same* launch configuration the node
    records — dsh and vibe carry neither model nor thinking level on argv, so a
    replacement that is not configured again is a page turn that silently
    changes model; a dsh or vibe refusal returns HTTP 400 and fails the clear before
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
