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
in the README). Anything that can reach the port on an allowed Host can do
everything listed here, including answering approval prompts. These
controls protect the loopback/browser boundary; they are not a login.

- **Trusted Host.** Every request — the token-bearing index, static files,
  APIs, and 404/405 — is checked against a Host policy derived from `-addr`
  plus optional `-trusted-host` values. Ports are not host identity, so an
  SSH-forwarded client port still works. A loopback listener trusts
  loopback IP literals and `localhost`; a concrete bind trusts that
  identity (and explicit extras); a wildcard bind may be reached by
  IP literal but does not silently trust arbitrary DNS names. A matching
  hostile `Host`/`Origin` (DNS rebinding) is rejected before any handler.
- **CSRF.** Browser-originated unsafe methods remain same-origin, must
  carry the `X-Scimux-CSRF` token embedded in the served page, and must use
  an accepted content type (`application/json` or multipart uploads).
  CORS is not enabled.
- **Fetch Metadata.** Browser requests to any `/api/*` path that arrive
  with `Sec-Fetch-Site: cross-site` or `same-site` are rejected, including
  safe GETs such as search and update-check. `same-origin`, `none`, and
  clients that omit the header follow the documented route behavior.
- **Anti-framing.** Successful and error responses carry
  `Content-Security-Policy: frame-ancestors 'none'` and
  `X-Frame-Options: DENY`.

Binding `-addr` wider than loopback still exposes full controller access
to every client that can reach an allowed Host.

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
`attention` (when set) is `approval | question | inspect` (Claude never
uses `inspect` for automatic terminal fallback, and never publishes
attention while auto-approve is armed), `supervision` is the
Claude-only contract (`claude_starting` for a current bundle awaiting
SessionStart, `claude_strict` after acknowledgement, `claude_unsupported`
for adopted/legacy/moved-binary bundles, `claude_failed` for startup or
delivery errors), and
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
`terminal_only`; Claude never uses `fallback` to force the terminal. While
the first Claude prompt is awaiting SessionStart or transcript confirmation,
`pending_prompt` carries that text (pale bubble) and `delivery` is
`submitting` or `unconfirmed`. A startup or delivery failure sets `error` and
`restore_draft` with the original prompt; it does not open the terminal.

For structured nodes (`codex`, ACP `pi`/`opencode`/`grok`), `source` is
`acp`; there is no pane fallback, and pending approval details are returned as
`perm_title`, `perm_options`, and — only while a permission is pending — an
opaque `perm_request_id` that `POST …/key` must echo back. Idle structured
chats and tmux nodes do not invent a request id. For a strict Claude
permission dialog the echo token is `perm_dialog_id`, a **server-minted
visible-dialog epoch**. Claude's `Notification(permission_prompt)` payload
has no PermissionRequest id; scimux does not claim exact request correlation.
Structured-node turn failures may also set `error`.

`GET /api/nodes/{id}/chat?history=1` returns the whole log as ordered read-only
surfaces: `{"segments":[{"start","seam","reason","turns"}, …]}`. This is the
on-demand path behind "show earlier history" and earlier metro stops; normal
polling stays current-segment only. The history branch does not participate in
ETag short-circuiting.

On the polled (non-history) path, responses carry an `ETag`; polling clients
may send `If-None-Match` and receive `304 Not Modified` when the snapshot is
unchanged. The tag hashes the fully marshalled body (turns *and* mechanics such
as attention, `last_change`, delivery, and Claude `compacting` /
`elicitation_waiting`), so a
needs-input or compaction flip without a log write still invalidates the cache.

For a strict scimux-owned Claude node, `compacting: true` (and optional
`compact_trigger` of `manual` or `auto`) is set while a current
`compact/active.json` marker is valid. The field is omitted when idle. It
does not appear on `/api/state`. Compaction forces `reply_ready` false and
does not change `live`, `attention`, `fallback`, or `supervision`.

While an MCP elicitation is outstanding, the same chat payload sets
`elicitation_waiting: true`, `elicitation_count`, and a bounded
`elicitations` array of `{server, message, mode, url}` metadata for the
current session. Those fields are omitted when idle and never appear on
`/api/state`. Elicitation forces `reply_ready` false, raises `question`
attention, and does not reuse `perm_*` identity fields. Schema and result
content are never included.

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

Cross-site and same-site browser GETs are rejected at the common request
boundary (`Sec-Fetch-Site`); this endpoint is otherwise unchanged.

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
A Claude create returns immediately with `initial_delivery: "pending"`; the
first prompt is pasted after SessionStart on a background path. Failures after
that are surfaced on `GET …/chat` (`error`, `restore_draft`), not by blocking
this response.

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
A Claude send whose leading slash command is `/fork` or `/fork …` is `400`
(Claude's native fork is not supported; use `POST /api/nodes` with `parent`
to create a fresh node). Ordinary prose that merely contains the text
`/fork` is accepted. Claude SessionStart sources accepted by the hook are
`startup`, `clear`, `compact`, and `resume` only.

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
content (`.html`, `.svg`, …) can never run same-origin. Recorded MIME never
decides inline-vs-download — only the filename extension does. An unknown node
or asset id, or a blob path that resolves outside the node's own asset
directory, is `404`.

This URL is the sole rendering address for both inline and blob storage.
Chat and preview responses list each referenced asset with the canonical
path `/api/nodes/{id}/assets/{assetID}` and never embed inline `data:`
URI content. The browser builds `href`/`src` from encoded node and asset
ids only; recorded MIME is not interpolated into HTML.

### `POST /api/nodes/{id}/send/resolve`

Resolve an unconfirmed tmux delivery (the pane never acknowledged the
paste): clears the unconfirmed-send state so the composer unblocks. `409`
while a send is actively submitting.

### `POST /api/nodes/{id}/send/interrupt`

Interrupt the node's in-flight turn.

### `POST /api/nodes/{id}/key`

Body: `{"key": "1"}` for ordinary tmux keys, `{"key": "1", "request_id":
"<opaque>"}` for structured transports (Codex, ACP), or `{"key": "1",
"dialog_id": "<epoch>"}` for a strict Claude permission dialog. Answers a
dialog. Keys are a whitelist (digits, `y`/`n`, arrows, Tab, Enter, Escape)
— this endpoint answers prompts, it is not a keystroke injector; anything
else is `400`.

On structured nodes `request_id` is **required** and must match the
`perm_request_id` from the pending permission on `GET …/chat`. Missing
`request_id` is `400`; a stale id (the pending request was replaced) is
`409`. Neither case writes a key audit record or delivers a decision.
On a strict Claude node with a visible permission dialog, `dialog_id` (or
`request_id` carrying the same epoch) is required and must match the
current `perm_dialog_id`. That token is a server-minted visible-dialog
epoch, not Claude's PermissionRequest identity. Any `dialog_id` that is
missing from the live epoch, already retired, or mismatched is `409` and
sends no keys. A successful key retires that epoch only; standing asks
are not deleted as a guessed identity. Ordinary tmux keys (user-opened
terminal, no visible dialog, no `dialog_id`) still use the `{key}`-only
body.

Every accepted key is recorded in the store together with decision evidence
(the pane's bottom lines, or the tool title on structured transports); on
structured transports the decision is persisted *before* the agent learns
the answer.

### `POST /api/nodes/{id}/auto-approve`

Body: `{"enabled": true}`. Arms or disarms the server-owned, one-turn
auto-approval lease. Supported on the structured transports (Codex
app-server; Grok, OpenCode, and Pi through ACP) and on Claude/tmux chats
that scimux launched itself — those carry a hook bundle registering a
`PermissionRequest` hook, which is how a Claude tool call can be answered at
all. Two cases still return `400` and create no state: a Claude pane scimux
did not launch (adopted, or launched before the feature existed, so its
bundle has no permission rendezvous), and any other unsupported transport.
State is never persisted across process restart.

Response (authoritative):

```json
{"supported": true, "enabled": true, "phase": "primed", "count": 0}
```

`phase` is `off`, `primed` (enabled while idle), or `armed` (active for the
current turn). `count` advances only after an eligible decision is audited
into the session log and then successfully delivered.

On the structured transports, eligibility selects the sole offered option
with semantic kind `allow` (one-time / request-scoped), regardless of
display key or position — never `allow_always`, `reject`, or an ambiguous
multi-allow menu. Requests already pending when the lease was enabled are
never auto-resolved.

Claude offers scimux no menu: its `PermissionRequest` hook describes the
tool call, and "allow once" versus "allow always" lives on the answer, where
scimux emits `{"behavior":"allow"}` and never the `updatedPermissions`
member that would persist a rule. Eligibility is therefore rebuilt from what
the hook proves: an armed lease the request echoes, the node's own bound
session id, an ordinary permission mode (`default` or `acceptEdits`), and a
tool that is not itself a question (`ExitPlanMode`, `AskUserQuestion`).
Anything else — and any request whose hook has already escalated — stays
manual and meets the human at the dialog. Because the hook writes a request
only while the arm marker exists, a call that asked before the lease was
armed is structurally out of reach; that is Claude's equivalent of the
structured transports' enable cutoff. Audited Claude decisions carry an
empty `options` list for the same reason. The same object is included
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

**Note, section, and reference ids are opaque server-minted identities** (a
single filename-safe path component). Clients must treat them as opaque strings
and must not construct or encode path segments into them. The store enforces
the mint grammar on every entry point; HTTP handlers validate route ids before
mutation or git work.

**Status contract for route identities**

| Condition | Status |
|---|---|
| Malformed / non-minted note, section, or reference id that reaches a handler (including URL-decoded traversal such as `..%2F…`) | `400` |
| Well-formed id that is unknown or archived | `404` |
| Path pattern not owned by ServeMux (e.g. extra `/` segments the route does not bind) | `404` |
| Literal path cleaning by the Go ServeMux (e.g. bare `..` segments) | may `301` before the handler |

Double-encoded input is decoded only once by the router; application code does
not recursively decode path values. A single decode that still yields a
non-minted id is `400`. Traversal requests are never redirected into a valid
mutation route.

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
(first-seen order). Listing errors are `500`. Invalid directory names and
documents whose embedded `id` differs from the folder name are skipped
defensively (they do not fail the whole list).

### `POST /api/notes`

Create a note: server-minted id, auto title (`YYYY-MM-DD HH:MM`), a single
starter section titled `"Section 1"`, and an `order` that appends it after
existing notes. Returns the full document. Create failures are `500`.

### `GET /api/notes/{id}`

The full note document (`id`, `title`, `created_at`, `edited_at`, `order`,
`sections[]` with each section's `id`/`title`/`body`/`order`/`references`).
Malformed id is `400`; unknown well-formed id is `404`. A on-disk document
whose embedded `id` does not match the requested folder id is rejected as
invalid (`400`) — identity cannot redirect a subsequent save into another
directory.

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

Returns the full document with an advanced `edited_at`. Malformed note or
section id is `400`; unknown note is `404`; unknown section id is `400`; save
failures are `500`. Bad JSON is `400`.

### `DELETE /api/notes/{id}`

Archives the whole note folder to `notes/archive/<id>.<stamp>/` rather than
destroying it (a reissued id can never collide with live data). Returns
`{"ok":"deleted"}`. Malformed id is `400`; unknown id is `404`.

### `POST /api/notes/{id}/sections/{sectionID}/references`

Append an embedded chat reference to a section. Body carries the durable
source address and a display snapshot; the server mints the reference `id`
(any client-supplied id is ignored). Default placement is the bottom of the
section. Returns the full note. Malformed note or section id is `400`;
unknown well-formed note or section is `404`; bad JSON is `400`.

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
bubble or any capture-layer bookmark. Returns the full note. Malformed note,
section, or reference id is `400`; unknown well-formed note is `404`; unknown
section or reference id is `404`.

## Maintenance

### `GET /api/update/check`

Queries the release feed. Returns `{current, latest, url, notes, available}`;
`available` is never true for `dev` builds. `502` if the feed is
unreachable. Cross-site and same-site browser GETs are rejected at the
common request boundary (`Sec-Fetch-Site`).

### `POST /api/update`

Self-update: downloads the release binary for this OS/arch from the trusted
HTTPS release origin (`codeberg.org` only — exact host, no userinfo, port
empty or 443), verifies it against the release's `SHA256SUMS` (same origin
policy on the initial URL and every redirect), and atomically replaces the
running executable only after size, close, and checksum checks succeed.
Binary downloads are capped at 256 MiB; a response that exceeds the cap is
rejected without accepting a truncated payload. Failed verification or
download leaves the installed executable byte-for-byte unchanged, does not
restart the process, and removes any `.scimux-update-*` temp file. `409` if
already up to date, a `dev` build, or an update is already in progress.
`502` on download/verify failure; `500` on install (chmod/rename) failure.

### `GET /api/licenses`

License texts bundled into the binary (shown in the About sheet).

### `GET /api/harnesses`

Which supported agent CLIs this computer has, in registry order:
`{harnesses:[{agent, present, launchable, installed, path}]}`. Local only —
never a network call, because the menu reads it on every open. `installed` is
the first version-shaped token of `<bin> --version`, empty when the output
does not carry one. `present` and `launchable` are separate facts: pi is
installed as `pi` but launched through `pi-acp`. The probe runs once per
process, so a harness installed while scimux runs appears after a restart.

### `GET /api/harnesses/latest`

What each harness publishes upstream: `{latest:{<agent>:{version, source}}}`.
Reached only on an explicit tap, like the scimux update check — the server
never polls the registries. There is no single lane: three sources are npm
packages, grok is a plain-text channel file, and Claude's depends on whether
it was installed natively (compared against the installer's own `stable`, not
the npm dist-tag, which it can never receive). Sources are read concurrently
and independently, and an agent whose source failed is simply absent from the
map — "unknown" is what happened, and it is not the same claim as up to date.

## Remote pairing

Loopback pairing for `--remote`. Confirm is a separate explicit call;
mint never completes a pairing. The curl recipe and FR-38 state table
live in `docs/remote-pairing-curl.md`. CSRF is required on unsafe methods;
GET needs no header. `sameOrigin` is true when both Origin and Referer
are absent.

### `POST /api/remote/pairing`

Mint an 8-character pairing code with a 60-second TTL. Confirm flags in
the body are ignored.

### `GET /api/remote/pairing/{code}`

FR-38 state for a code, plus SAS once a device offer has arrived.

### `POST /api/remote/pairing/{code}/confirm`

Complete pairing only when both `computer_confirm` and `device_confirm`
are present and `true`. Omitted flags do not default to true.

### `POST /api/remote/pairing/{code}/cancel`

Tear down the waiter without consuming the code.

### `GET /api/remote/devices`

Paired devices with stable labels and paired-at times. A device's key is
`ecdh_public_key`: the static P-256 key a reply envelope is sealed to,
matching the name the on-disk record uses for the same material. It is
not the device's ed25519 signing identity, which this API does not
expose — `public_key` means ed25519 elsewhere, so it is not reused here.

### `DELETE /api/remote/devices/{id}`

Revoke one paired device.

### `PATCH /api/remote/devices/{id}`

Rename one paired device: `{"label":"my iPhone"}`, answering the updated
device. The label a row carries otherwise came from the device itself, in
its pair-offer, so it is a claim rather than a fact — a phone is free to
name itself after the laptop beside it. This is how the operator replaces
that claim with a name they asserted, and it is safe to accept free-form
text only because the list keeps showing the device identity underneath.

An empty label clears the name rather than failing; the row then falls
back to the device identity. Labels are bounded and stripped of control
characters where they are stored, whoever supplied them, so a rename
never returns exactly what it was given.

### `POST /api/remote/unenroll`

Unlink this computer: release the installation at the rendezvous
(rendezvous-v1 §4.4) and forget the identity locally. Answers
`{"released":bool,"hosted":"..."}`, with `hosted` read after the unlink.

The two halves are deliberately unequal. The local half is
unconditional — a computer that cannot reach the rendezvous is exactly the
one whose owner wants it to stop trying — so an unreachable rendezvous
still answers 200 with `released:false`. That is the honest report: the
computer is unlinked, and an installation the rendezvous still holds can
only be struck off by whoever issued the invite. A failure of the *local*
half is a 500, because an identity still on disk is still an enrollment.

§4.4 releases the installation and never the code, so re-enrolling
afterwards takes a new invite.

### `GET /api/remote/status`

The installation's hosted enrollment as `hosted` (`enrolled`,
`disabled`, `revoked`, `unavailable`, `""` after an unlink, or one of
the FR-30 broken states) plus `can_pair`, and a `devices` array. Each
device is `{"id":"...","connected":true}` when its tunnel is live.
When it is not, `connected` is false; `cause` is one of the six FR-24
states (`rendezvous-unavailable`, `computer-offline`,
`signalling-rejected`, `ice-failed`, `auth-failed`,
`connected-then-lost`) only when the transport has produced one. A
paired device that has never attached a channel omits `cause`
entirely — never connected is not a cause. `guidance` is present only
for `ice-failed`, and is the only state that names
SSH/WireGuard/Tailscale. `hosted` is independent of `cause`: a
revoked installation is still `hosted:"revoked"` and is not an
FR-24 cause.

`can_pair` is whether a pairing started now could complete, and
`pair_refusal` is the sentence to show in place of the button when it
cannot. It is answered here so the menu need not re-derive the policy
from `hosted` and drift from what the mint enforces.

`404` when remote pairing is not enabled. GET needs no CSRF header.
The body never mints a code and never returns a rid, SAS, or public
key.

`POST /api/remote/pairing` returns `409` with `hosted` and a readable
`error` unless `hosted` is `enrolled` or `unavailable`. Minting is
entirely local — a code, a locally minted rid, a link off the configured
origin — so an installation the rendezvous would not admit would
otherwise hand out a code and a QR for a meeting that can never happen,
and the eventual expiry reads as a timing problem rather than as "this
computer is not enrolled". `revoked` and `disabled` are durable facts
about authorization; `""` is a computer that has just unlinked; the
FR-30 broken states describe an identity rv would refuse.

`unavailable` is the one refusal-shaped state that still mints, because
it is a transient rendezvous condition and the mint is what starts the
pairing wait loop that clears it; a code minted then is genuinely
pairable once the outage lifts. For the same reason
`GET /api/remote/pairing/{code}` reports FR-38 `failed` (plus `reason`,
which is `hosted` or `not-enrolled` when `hosted` is empty) under every
refusing state, and leaves a live session `pending` through a transient
outage.

### `GET /api/remote/bootstrap`

Computer-supplied FR-40 bootstrap manifest. JSON with `source` (`computer`),
`entry` (`/js/app.js`), and `entries` naming every served asset with
`url`, `kind`, `size`, and `integrity` (`sha256-` + standard-base64
SHA-256 of the bytes that GET on that URL returns). Derived from the
embedded filesystem the handlers serve, computed once per process. GET
needs no CSRF header. Always reachable — this is application content,
not pairing state.
