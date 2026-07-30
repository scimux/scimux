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
                "source": "claude-oauth" }
  }
}
```

Percentages are `0..100`; `*_remaining` is `100 - used` and is the runway a user
wants before starting work. Claude may additionally carry
`extra_usage_enabled | extra_usage_used | extra_usage_limit |
extra_usage_currency`. Numeric fields are omitted when unknown, so `0` stays
distinct from missing.

Two contracts matter. **This read is cache-only:** it never performs provider
I/O, so opening many tabs cannot fan out into many Claude OAuth calls or Codex
transcript scans. Collection is prompt-driven instead — a successful
Claude/Codex prompt refreshes the snapshot at most once every 15 minutes (or
immediately when it is older than 60 minutes), and stops once no prompt has
arrived for 15 minutes, so an idle or closed browser never triggers provider
checks. **It is best-effort and never errors:** an agent with no usable data is
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
snapshot. For structured nodes (`codex`, ACP `pi`/`opencode`), `source` is
`acp`; there is no pane fallback, and pending approval details are returned as
`perm_title` and `perm_options`. Structured-node turn failures may also set
`error`.

`GET /api/nodes/{id}/chat?history=1` returns the whole log as ordered read-only
surfaces: `{"segments":[{"start","seam","reason","turns"}, …]}`. This is the
on-demand path behind "show earlier history" and earlier metro stops; normal
polling stays current-segment only.

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
header-less log) — read via `GET /api/archived`. `forkable` reports whether the
chat can seed a fork (a live node, or an archived one whose recorded dir still
exists). Each hit's `role` is `user | assistant | asset | bookmark`; log hits
carry the stable `(segment, record)` ordinal pair that anchors an archived read,
bookmark hits carry `bookmark_id` and the referenced `turn_time`;
`before`/`match`/`after` are the excerpt around the match.

A query shorter than 2 runes returns empty `groups` (the field is searched live
as it is typed); longer queries are clamped to 128 runes. The scan is bounded
(per-file, per-group, total-hit, and group-count caps); tripping any cap sets
`partial: true`. There is no cache and no `ETag` — each call rescans.

### `GET /api/archived?uid=<uid>&seg=<n>&rec=<n>&at=<time>`

The read-only surface for a **deleted** chat (a live search hit opens the normal
chat instead). `uid` is required — the archived log's identity from a search
group. The hit is anchored by its `(seg, rec)` ordinal (with `at`, a turn
timestamp, as a defensive fallback). Returns a bounded window of turns around
the anchor:

```json
{
  "uid": "a1b2c3", "title": "Fix the poller",
  "agent": "claude", "model": "…", "effort": "…", "dir": "/path",
  "forkable": true,
  "turns": [ { …transcript turn… } ],
  "anchor": 8, "before_truncated": true, "after_truncated": false
}
```

`anchor` is the index into `turns` of the hit turn; `before_truncated` /
`after_truncated` say whether turns were dropped on either side. `forkable` is
true when the recorded agent and dir survive. There is no node, process,
composer, or polling here — just a photo of the turns so the supervisor can read
what was said and, if the dir survives, fork from it. Asset markers render as
inert "unavailable" chips (a deleted node's blobs are archived away). Missing
`uid` is `400`; every other failure (unknown uid, unreadable log, traversal
attempt) degrades to `404`.

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
is added client-side to mean "launch with the harness default". `efforts` is
present only where the CLI advertises per-model reasoning effort (codex, via
`codex debug models`): a map from model id to that model's accepted `levels` and
its `default`. Agents without it (claude/pi/opencode), and codex when only the
static fallback is available, omit `efforts` and the UI uses a static per-agent
effort list. The list is probed once per process at startup and cached for its
lifetime; a newly installed CLI or model appears after a restart.

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

## UI state

### `GET /api/ui` / `PUT /api/ui`

The shared UI document (`~/.scimux/ui.json`: groups, archived cards, bookmarks,
lanes). GET sets an `ETag` and honors `If-None-Match`. PUT requires
`If-Match` (the last ETag, or `*` to bootstrap) — `428` without it, `409` on
mismatch — and replaces the document atomically. Bodies over the size limit
are `413`.

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
