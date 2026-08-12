# HTTP API

scimux's web UI talks to the binary over a small JSON HTTP API on the
loopback listener (default `127.0.0.1:8787`). This page documents it for
scripting and debugging.

**Stability:** this is first and foremost the embedded UI's internal API.
The routes below are stable in spirit, but response shapes may gain fields
in any release and are only guaranteed to match the scimux version serving
them. Script against it read-mostly, and prefer `GET /api/state` and the
per-node reads over anything that mutates.

**Security:** there is no authentication (see "Remote access and security"
in the README). Anything that can reach the port can do everything listed
here, including answering approval prompts. Browser-originated unsafe methods
are still guarded against CSRF: non-`GET`/`HEAD`/`OPTIONS` requests must be
same-origin, must carry the `X-Scimux-CSRF` token embedded in the served page,
and must use an accepted content type (`application/json` or multipart uploads).

**Compression:** when the client sends `Accept-Encoding: gzip`, eligible
responses are gzip-compressed (`Content-Encoding: gzip`, `Vary:
Accept-Encoding`). Compression applies across `/api/*` and the static tree for
compressible content types (JSON, text, JavaScript, SVG, …). Already-compressed
payloads (images, PDF, zip, …) and empty short-circuit responses (`304`,
`204`) are left alone so poll validators stay intact.

## Reading state

### `GET /api/state`

The polling snapshot the UI runs on. Returns:

```json
{
  "nodes":     [ { …node fields…, "live": "quiet", "attention": "approval",
                   "has_transcript": true, "last_activity": 1752849600000,
                   "ctx_pct": 37, "stops": ["2026-07-24T09:12:00Z"] } ],
  "unadopted": [ "tmux-session-name" ],
  "sys":       { …host load/memory… },
  "socket":    "scimux",
  "hostname":  "workstation",
  "version":   "v0.5.0"
}
```

Each entry is the stored node (id, title, prompt, description, rationale,
lane_id, fork_kind, agent, model, effort, dir, transport, created_at,
`ended_at`, …) plus the mechanical view: `live` is
`active | quiet | exited | unavailable`,
`attention` (when set) is `approval | question | inspect`, and
`last_activity` is the last pane change in Unix milliseconds. `ctx_pct`, when
present, is the live segment's context occupancy percentage. `stops` are the
node's `/clear` page-turn timestamps; the map prepends `created_at` to draw the
full station chain. `ended_at` (RFC 3339, present only once set) marks a thread
deliberately closed via
`POST …/exit`: the node stays visible with a dead-end cap on the map rather
than being deleted. `unadopted` lists tmux sessions on scimux's socket that no
node accounts for — candidates for `POST /api/adopt`.

Responses carry an `ETag`; polling clients may send `If-None-Match` and receive
`304 Not Modified` when the snapshot is unchanged.

### `GET /api/usage`

The latest subscription-budget snapshot for the agents that expose one, for the
status-bar gauge. Returns:

```json
{
  "observed_at":      "2026-07-27T10:30:00Z",
  "next_check_after": "2026-07-27T10:45:00Z",
  "agents": {
    "codex": {
      "available": true,
      "plan": "edu",
      "five_hour_used": 10, "five_hour_remaining": 90,
      "five_hour_reset": "2026-07-27T02:10:00Z",
      "weekly_used": 3, "weekly_remaining": 97,
      "weekly_reset": "2026-08-01T07:00:00Z",
      "source": "codex-session-jsonl"
    },
    "claude": { "available": false, "reason": "usage unavailable",
                "source": "claude-oauth" },
    "grok": {
      "available": true,
      "plan": "SuperGrok Lite",
      "weekly_used": 14, "weekly_remaining": 86,
      "weekly_reset": "2026-08-05T00:00:00Z",
      "source": "grok-acp-billing"
    }
  }
}
```

Percentages are `0..100`; `*_remaining` is `100 - used` and is the runway a user
wants before starting work. Claude may additionally carry
`extra_usage_enabled | extra_usage_used | extra_usage_limit |
extra_usage_currency`. Grok reports only the weekly credit window (no five-hour
bucket). Numeric fields are omitted when unknown, so `0` stays distinct from
missing.

Two contracts matter. **This read is cache-only:** it never performs provider
I/O, so opening many tabs cannot fan out into many Claude OAuth calls, Codex
transcript scans, or Grok ACP billing probes. Collection is prompt-driven
instead — a successful Claude/Codex/Grok prompt refreshes the snapshot at most
once every 15 minutes (or immediately when it is older than 60 minutes), and
stops once no prompt has arrived for 15 minutes, so an idle or closed browser
never triggers provider checks. **It is best-effort and never errors:** an
agent with no usable data is
returned as `{"available": false, "reason": "usage unavailable"}` rather than
failing the request. The endpoint always answers `200`; an empty cache reports
every agent unavailable. There is no `ETag` — the body is small and served from
memory.

### `GET /api/nodes/{id}/chat`

The conversation, rendered from the session-log store (the current segment —
everything after the last `/clear` seam), plus turn mechanics. Common fields
include `turns`, `last_change`, `chat_started`, `prior_turns`, context-gauge
fields (`ctx_used`, `ctx_window`, `ctx_pct`), `live`, `attention`, `source`,
`reason`, `fallback`, `pending`, `pending_calls`, `waiting_on`, `watermark`,
`progress`, and `delivery` (state of the last tmux send).

For tmux/Claude nodes, `source` is `transcript`, `peek`, `none`, or
`terminal_only`; `fallback:true` means the UI should degrade to the pane
snapshot. For structured nodes (`codex`, ACP `pi`/`opencode`/`grok`), `source` is
`acp`; there is no pane fallback, and pending approval details are returned as
`perm_title` and `perm_options`. Structured-node turn failures may also set
`error`.

`GET /api/nodes/{id}/chat?history=1` returns the whole log as ordered read-only
surfaces: `{"segments":[{"start","seam","reason","turns"}, …]}`. This is the
on-demand path behind "show earlier history" and earlier metro stops; normal
polling stays current-segment only. The history branch does not participate in
ETag short-circuiting.

On the polled (non-history) path, responses carry an `ETag`; polling clients
may send `If-None-Match` and receive `304 Not Modified` when the snapshot is
unchanged. The tag hashes the fully marshalled body (turns *and* mechanics such
as attention, `last_change`, and delivery), so a needs-input flip without a log
write still invalidates the cache.

### `GET /api/nodes/{id}/peek[?mode=visible]`

Plain-text pane snapshot for tmux nodes (`mode=visible` captures only the
visible pane instead of history). For structured-transport nodes (ACP,
codex) there is no pane; peek returns a tail of the raw event log.

### `GET /api/search?q=<query>`

Full-text search across every conversation in the session-log store — live
nodes, deleted (archived) logs, and bookmarks — grouped by chat. Returns:

```json
{
  "query": "timeout",
  "groups": [
    {
      "kind": "live", "id": "my-node", "uid": "a1b2c3",
      "title": "Fix the poller", "lane_id": "infra", "agent": "claude",
      "forkable": true,
      "hits": [
        { "role": "assistant", "segment": 2, "record": 14,
          "time": "2026-07-24T09:12:00Z",
          "before": "…raised the ", "match": "timeout", "after": " to 10s…" }
      ]
    }
  ],
  "partial": false
}
```

`kind` is `live | archived | bookmarks`. A `live` group carries the node `id`
(open the normal chat, or fork it); an `archived` group carries only `uid` —
the deleted log's on-disk identity (a meta UID, or `legacy:<path>` for a
header-less log) — read via `GET /api/preview`. `forkable` reports whether the
chat can seed a fork (a live node, or an archived one whose recorded dir still
exists). Each hit's `role` is `user | assistant | asset | bookmark`; log hits
carry the stable `(segment, record)` ordinal pair that anchors a preview read,
bookmark hits carry `bookmark_id` and the referenced `turn_time`;
`before`/`match`/`after` are the excerpt around the match.

A query shorter than 2 runes returns empty `groups` (the field is searched live
as it is typed); longer queries are clamped to 128 runes. The scan is bounded
(per-file, per-group, total-hit, and group-count caps); tripping any cap sets
`partial: true`. There is no cache and no `ETag` — each call rescans.

### `GET /api/preview?uid=<uid>&seg=<n>&rec=<n>&at=<time>`

The read-only turn window a search hit (or bookmark) opens onto its surrounding
conversation — for **every** hit, live or deleted. It is deliberately not
`GET /api/nodes/{id}/chat`: no composer, no polling, no history, no mechanics —
just a photo of the turns around the hit. `uid` is required (the log's on-disk
identity from a search group or bookmark address). The hit is anchored by its
`(seg, rec)` ordinal (with `at`, a turn timestamp, as a defensive fallback).
Returns a bounded window of turns around the anchor:

```json
{
  "uid": "a1b2c3", "node": "my-node", "title": "Fix the poller",
  "agent": "claude", "model": "…",
  "turns": [ { …transcript turn… } ],
  "assets": { "a_1": { … } },
  "anchor": 8, "before_truncated": true, "after_truncated": false
}
```

`node` is present only when the chat still lives (the UI can offer "Open chat");
it is omitted for deleted logs. `anchor` is the index into `turns` of the hit
turn; `before_truncated` / `after_truncated` say whether turns were dropped on
either side. Live nodes project assets the same way chat does; a deleted node's
blobs were archived away, so asset markers render as inert "unavailable" chips.
This surface does not describe forkability (`forkable`, `dir`, and `effort` are
not returned). Missing `uid` is `400`; every other failure (unknown uid,
unreadable log, traversal attempt) degrades to `404`.

### `GET /api/agents`

Detected agent CLIs and what they offer the new-activity dialog, keyed by
agent:

```json
{
  "claude": { "models": ["fable", "opus", "sonnet", "haiku"] },
  "codex": {
    "models": ["gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.5"],
    "efforts": {
      "gpt-5.6-sol": { "levels": ["low", "medium", "high", "xhigh", "max", "ultra"], "default": "medium" },
      "gpt-5.5":     { "levels": ["low", "medium", "high", "xhigh"], "default": "xhigh" }
    }
  }
}
```

`models` is ordered (the configured default first when set); an empty `""` model
is added client-side to mean "launch with the harness default". `efforts` is a
map from model id to that model's accepted `levels` and its `default`. Codex
obtains it from `codex debug models`; Grok reads its CLI models cache and fills
missing entries from its static low/medium/high menu. Agents without per-model
data (claude/pi/opencode), and codex when only its static fallback is available,
omit `efforts`; the UI then uses its static per-agent list. The catalog is
probed once per process at startup and cached for its lifetime; a newly
installed CLI or model appears after a restart.

## Managing nodes

### `POST /api/nodes`

Create and start a session. Body is the launch configuration (same fields as
a stored node): `agent` selects the CLI, plus optionally `prompt`, `title`,
`model`, `effort`, `dir`, `description`, `lane_id`, and `parent` + `rationale`
for a fork. A plain new activity needs a non-empty `prompt`; a fork inherits
its parent's agent/model/effort/dir when those fields are omitted and must land
on a lane. Returns the created node. Validation failures (empty prompt where
required, unknown parent, bad agent or dir, bad fork lane) are 4xx.

Only launch-config fields are honored. Server-owned fields (`id`, `session_id`,
`transcript`, `created_at`, `ended_at`, `fork_kind`, `adopted`) are ignored if
present in the body — they are minted or derived server-side. Adopting an
existing tmux session is a separate endpoint (`POST /api/adopt`).

### `PATCH /api/nodes/{id}`

Body: any of `title`, `description`, `lane_id`. Title must stay non-empty.
Lane assignment is write-once: setting it on an unassigned node succeeds,
changing an existing assignment is `409`.

### `POST /api/nodes/{id}/exit`

Close a thread deliberately: stamps `ended_at` durably and keeps the node
visible on the map with a dead-end cap (unlike `DELETE`, which removes it).
Idempotent — a second call keeps the original stamp. The `ended_at` record is
persisted first, then the owned process is stopped best-effort, so a failure to
reach the agent never un-ends the node.

Returns `{"node": <node>, "closed": true, "stopped": <bool>, "reason": <string>}`.
`stopped` reports whether the underlying process was actually stopped: it is
`false` with `reason:"adopted"` for an adopted tmux session (scimux never kills
a session it did not start — the agent keeps running), and `false` with
`reason:"kill_failed"` when teardown was attempted but errored. Owned sessions
and structured-transport links are stopped (`stopped:true`, empty `reason`).
`/exit` is idempotent: a repeat call on an already-closed node does not tear the
process down again and reports its current standing (`reason:"running"` when the
agent is still live), so it never issues a second kill.

### `DELETE /api/nodes/{id}`

Appends a `delete` tombstone to the store and archives the node's session
log to `sessions/archive/`. Uploaded files are archived under
`attachments/archive/`. History is never destroyed.

### `POST /api/adopt`

Adopt a tmux session that scimux didn't start. Body: `session` (required;
must exist on scimux's tmux socket), plus optional `title`, `prompt`,
`description`, `agent` (default `claude`), `model`, `dir`, `session_id`,
`transcript`. Adopted sessions are never killed by scimux. `agent:"codex"`
is rejected — codex runs as a scimux-started subprocess, not in tmux.

## Talking to a node

### `POST /api/nodes/{id}/send`

Body: `{"text": "…", "attachments": [<ref>, …]}`. Delivers a follow-up turn
(tmux: paste + Enter; structured transports: a protocol prompt). `409` while a
turn is still in flight, while a tmux send is unconfirmed, or after `/exit`
(`thread is closed; fork to continue`). On structured transports `"/clear"` is
implemented by scimux itself: a fresh protocol session on the same node,
recorded as a source seam — same page-turn semantics as Claude's `/clear`.

`attachments` is optional; each element is a reference returned by
`POST …/attachments` (below). Every ref must point inside this node's own
upload directory (others are rejected `400`). For each, a plain-text line
`[attached image|file: /abs/path]` is appended to the delivered prompt so the
agent reads the file — one uniform mechanism across all transports (no inline
image bytes). A turn may carry text, attachments, or both; an attachments-only
turn is valid, but an entirely empty one is `400`.

### `POST /api/nodes/{id}/attachments`

`multipart/form-data` with one or more files under the field `files`. Stores
them under `~/.scimux/attachments/{id}/` and returns
`{"attachments": [{"path","mime","name","size"}, …]}` — the refs to pass to
`/send`. Separate, larger size cap than the JSON body; the bytes never enter
any JSONL store (the log records only the text reference). Files are archived
with the node on delete.

### `GET /api/nodes/{id}/attachments/{name}`

Serves one uploaded file's bytes (for the chat view's thumbnails). Only a bare
filename inside this node's own upload directory is served — no traversal, no
cross-node access; anything else is `404`. Safe raster images (`png`, `jpg`,
`jpeg`, `gif`, `webp`) are served inline with `nosniff`; every other type
downloads as `application/octet-stream` so uploaded active content cannot run
same-origin.

### `GET /api/nodes/{id}/assets/{assetID}`

Serves the bytes of one session asset — a file the agent produced (or that was
otherwise captured) during the conversation, indexed in the node's session log
by `assetID`. Bytes come from the log's inline base64 record or from blob
storage under `~/.scimux/assets/{id}/`. As with attachments, safe raster images
(`png`, `jpg`, `jpeg`, `gif`, `webp`) are served inline with `nosniff`; every
other type downloads as `application/octet-stream` so agent-generated active
content (`.html`, `.svg`, …) can never run same-origin. An unknown node or asset
id, or a blob path that resolves outside the node's own asset directory, is
`404`.

### `POST /api/nodes/{id}/send/resolve`

Resolve an unconfirmed tmux delivery (the pane never acknowledged the
paste): clears the unconfirmed-send state so the composer unblocks. `409`
while a send is actively submitting.

### `POST /api/nodes/{id}/send/interrupt`

Interrupt the node's in-flight turn.

### `POST /api/nodes/{id}/key`

Body: `{"key": "1"}`. Answers a dialog. Keys are a whitelist (digits, `y`/`n`,
arrows, Tab, Enter, Escape) — this endpoint answers prompts, it is not a
keystroke injector; anything else is `400`. Every accepted key is recorded
in the store together with decision evidence (the pane's bottom lines, or
the tool title on structured transports); on structured transports the
decision is persisted *before* the agent learns the answer.

### `POST /api/nodes/{id}/auto-approve`

Body: `{"enabled": true}`. Arms or disarms the server-owned, one-turn
auto-approval lease for structured transports only (Codex app-server; Grok,
OpenCode, and Pi through ACP). Claude/tmux returns `400` and creates no
state. State is never persisted across process restart.

Response (authoritative):

```json
{"supported": true, "enabled": true, "phase": "primed", "count": 0}
```

`phase` is `off`, `primed` (enabled while idle), or `armed` (active for the
current turn). `count` advances only after an eligible decision is audited
into the session log and then successfully delivered. Eligibility selects
the sole offered option with semantic kind `allow` (one-time / request-
scoped), regardless of display key or position — never `allow_always`,
`reject`, or an ambiguous multi-allow menu. Requests already pending when
the lease was enabled are never auto-resolved. The same object is included
on `GET /api/nodes/{id}/chat` as `auto_approve` and participates in that
response's full-body ETag. Current-segment decision audit surfaces appear
as `decisions` on the same response (and in `?history=1` segments).

## UI state

### `GET /api/ui` / `PUT /api/ui`

The shared UI document (`~/.scimux/ui.json`: groups, archived cards, bookmarks,
lanes). GET sets an `ETag` and honors `If-None-Match`. PUT requires
`If-Match` (the last ETag, or `*` to bootstrap) — `428` without it, `409` on
mismatch — and replaces the document atomically. Bodies over the size limit
are `413`.

## Notes

Synthesis documents (not bookmarks — those live in `ui.json`). Each note is a
folder at `~/.scimux/notes/<id>/note.json`: the JSON file is always the source
of truth. Optional per-note git versioning shells out to the `git` binary into
`notes/<id>/.git` and degrades silently when git is absent or fails — a failed
commit never fails the HTTP write. Mutating methods below are subject to the
CSRF rules in the Security preamble.

### `GET /api/notes`

Sparse list of notes — enough to render cards, never section bodies:

```json
{
  "notes": [
    {
      "id": "…", "title": "Install Guide",
      "created_at": "…", "edited_at": "…", "order": 0,
      "section_count": 2, "lanes": ["#c0392b"]
    }
  ]
}
```

`lanes` is the deduped set of embedded-reference lane colors across sections
(first-seen order). Listing errors are `500`.

### `POST /api/notes`

Create a note: server-minted id, auto title (`YYYY-MM-DD HH:MM`), a single
starter section titled `"Section 1"`, and an `order` that appends it after
existing notes. Returns the full document. Create failures are `500`.

### `GET /api/notes/{id}`

The full note document (`id`, `title`, `created_at`, `edited_at`, `order`,
`sections[]` with each section's `id`/`title`/`body`/`order`/`references`).
Unknown id is `404`.

### `PATCH /api/notes/{id}`

Partial update: only the fields present in the body are applied. Body fields
(all optional):

- `title` — rename the note
- `order` — reposition among notes
- `add_section` — append a section with this title (`""` becomes `"Section N"`)
- `section` — edit one existing section by `id`:
  - `title` / `body` / `order` — field updates
  - `delete: true` — remove the section
  - `commit: true` — client signal that a non-structural body edit has
    finished (mid-edit debounced autosaves omit it so optional git does not
    snapshot every keystroke). Structural changes (title/order/add/delete)
    commit immediately without it. `commit` is a PATCH field, not a route.

Returns the full document with an advanced `edited_at`. Unknown note is
`404`; unknown section id is `400`; save failures are `500`. Bad JSON is
`400`.

### `DELETE /api/notes/{id}`

Archives the whole note folder to `notes/archive/<id>.<stamp>/` rather than
destroying it (a reissued id can never collide with live data). Returns
`{"ok":"deleted"}`. Unknown id is `404`.

### `POST /api/notes/{id}/sections/{sectionID}/references`

Append an embedded chat reference to a section. Body carries the durable
source address and a display snapshot; the server mints the reference `id`
(any client-supplied id is ignored). Default placement is the bottom of the
section. Returns the full note. Unknown note or section is `404`; bad JSON is
`400`.

```json
{
  "source": {
    "uid": "a1b2c3", "segment": 2, "record": 7,
    "node": "my-node", "turnTime": "2026-07-28T10:00:00Z"
  },
  "snapshot": {
    "lane": "#c0392b", "station": "my-node",
    "speaker": "assistant", "time": "2026-07-28T10:00:00Z",
    "text": "the cited turn"
  }
}
```

The reference is self-contained: it still renders after the source node is
deleted.

### `DELETE /api/notes/{id}/sections/{sectionID}/references/{refID}`

Remove one embedded reference from a section. Does not touch the source chat
bubble or any capture-layer bookmark. Returns the full note. Unknown note is
`404`; unknown section or reference id is `404`.

## Maintenance

### `GET /api/update/check`

Queries the release feed. Returns `{current, latest, url, notes, available}`;
`available` is never true for `dev` builds. `502` if the feed is
unreachable.

### `POST /api/update`

Self-update: downloads the release binary for this OS/arch, verifies it
against the release's `SHA256SUMS`, and atomically replaces the running
executable (restart is manual). `409` if already up to date, a `dev` build,
or an update is already in progress.

### `GET /api/licenses`

License texts bundled into the binary (shown in the About sheet).
