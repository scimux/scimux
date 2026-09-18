# HTTP API

scimux's web UI talks to the binary over a small JSON HTTP API on the
loopback listener (default `127.0.0.1:8787`). This page documents it for
scripting and debugging.

**Stability:** this is first and foremost the embedded UI's internal API.
The routes below are stable in spirit, but response shapes may gain fields
in any release and are only guaranteed to match the scimux version serving
them. Script against it read-mostly, and prefer `GET /api/state` and the
per-node reads over anything that mutates.

**Security:** there is no authentication (see [SECURITY.md](../SECURITY.md)).
Anything that can reach the port on an allowed Host can do everything listed
here, including answering approval prompts. These controls protect the
loopback/browser boundary; they are not a login.

- **Trusted Host.** Every request — the token-bearing index, static files,
  APIs, and 404/405 — is checked against a Host policy derived from `-addr`
  plus optional `-trusted-host` values. Ports are not host identity, so an
  SSH-forwarded client port still works. A loopback listener trusts
  loopback IP literals and `localhost`; a concrete bind trusts that
  identity (and explicit extras); a wildcard bind may be reached by
  IP literal but does not silently trust arbitrary DNS names. A matching
  hostile `Host`/`Origin` (DNS rebinding) is rejected before any handler.
- **CSRF.** Every unsafe method, including requests from scripts, must
  carry the `X-Scimux-CSRF` token from the page's `scimux-csrf` meta tag.
  Origin or Referer, when supplied, must match the request Host. If a
  Content-Type is supplied, it must be JSON or multipart on the attachment
  upload route; bodyless requests may omit it. Use `application/json` for
  JSON bodies. CORS is not enabled.
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
`attention` (when set) is `approval | question | dialog | inspect`; `dialog`
includes an exact pre-SessionStart Claude workspace-trust prompt, answerable
through the audited key route. Claude never uses `inspect` for automatic
terminal fallback, and never publishes tool-permission attention while
auto-approve is armed. `supervision` is the
Claude-only contract (`claude_starting` for a current bundle awaiting
SessionStart, `claude_strict` after acknowledgement, `claude_unsupported`
for legacy/moved-binary bundles, `claude_failed` for startup or
delivery errors), and
`last_activity` is the last pane change in Unix milliseconds. `ctx_pct`, when
present, is the live segment's context occupancy percentage. `stops` are the
node's `/clear` page-turn timestamps; the map prepends `created_at` to draw the
full station chain. `ended_at` (RFC 3339, present only once set) marks a thread
deliberately closed via
`POST …/exit`: the node stays visible with a dead-end cap on the map rather
than being deleted. Live tmux inventory is not exposed; it remains internal
input for owned-session liveness and name-collision checks.

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
    "claude": { "available": false, "off": true,
                "reason": "usage checks are off",
                "source": "claude-statusline" },
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
wants before starting work. Grok reports only the weekly credit window (no
five-hour bucket), and Claude reports no weekly window on plans that have none.
Numeric fields are omitted when unknown, so `0` stays distinct from missing.

Claude's reading comes from the status line of a short throwaway probe session
(`source: "claude-statusline"`), which is the only sanctioned surface that
states subscription quota. It costs one small API turn, so it is **off by
default** and runs only with the user's consent
(`claude_usage_checks` in `PUT /api/settings`) — then only while a Claude
session is actually open, at most once per refresh interval, and never at all
if the CLI is not installed. It reports `"usage unavailable"` while the user is
rate-limited, because the probe turn that would measure the limit is itself
refused.

`"off": true` is that consent being absent, and it is deliberately distinct
from every other unavailable state: it is the only one the user can change by
tapping, so the browser must tell it apart without matching on English. It
never appears alongside `"available": true`.

Two contracts matter. **This read is cache-only:** it never performs provider
I/O, so opening many tabs cannot fan out into many Claude probe sessions, Codex
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

For structured nodes (Codex app-server, ACP
`pi`/`opencode`/`grok`/`cursor`/`dsh`,
and Muse MSP), `source` is `acp` for compatibility across these transports;
there is no pane fallback. Pending approval details are returned as
`perm_title`, `perm_options`, `perm_tool_kind`, `perm_reason`, and — only
while a permission is pending — an opaque `perm_request_id` that
`POST …/key` must echo back. Idle structured
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
Codex, Muse) there is no pane; peek returns a tail of the raw event log.
Historical external nodes with `adopted:true` return `409`; their panes are
no longer accessed. Saved conversation history remains available through
`GET /api/nodes/{id}/chat` and its `?history=1` view.

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
missing entries from its static low/medium/high menu. Cursor derives both from
`cursor-agent --list-models`, whose ids already contain the level: each id is
split into a model row and a level, so `models` lists a few dozen rows instead
of the CLI's flat two-hundred-odd combinations, and `efforts` holds the levels
each row actually published. A row whose ids all name a level carries
`required: true` and a `default`: it has no id meaning "no level", so a blank
`effort` would be one the server silently replaced, and the dialog offers no
blank choice for it and preselects the default instead. Elsewhere — codex,
grok, and cursor rows that do have a level-free id — a blank `effort` stays on
offer and keeps meaning "use the harness default". The mapping from (model,
effort) back to the exact id is server-side only and is never part of this
payload. Agents without per-model data (claude/pi/opencode/dsh), and codex when
only its static fallback is available, omit `efforts`; the UI then uses its
static per-agent list — for cursor that list is deliberately empty, because a
row with no published level has none.

Most catalogs are probed once per process and cached until restart. dsh has no
list command and no read-only discovery surface: naming its models would mean
opening an ACP session, which dsh flushes to its own durable history and offers
no way to delete, so scimux never opens one for discovery. dsh's `model` and
`effort` are therefore settable only through the API, and the strict
configuration path they exercise is not reachable from the dialog. dsh
reports an empty `models` and no `efforts`; the dialog offers only
"(default)", it launches on the profile's own default model, and the thought
levels that model accepts are read from the live session at launch — the UI's
static effort list for dsh is deliberately empty, because dsh's levels are
`off`/`low`/`high`/`max` for models whose route reasons and absent for the
rest. Claude's model IDs also have a persistent cache, refreshed on a CLI
version change or after one day;
`GET /api/agents` can trigger that refresh. Its model probes submit no billed
prompt and are independent of consent for the usage gauge.

Muse additionally returns `muse_models`, an array of objects with `id`,
`tier` (`standard`, `discounted`, or `unknown`), and `launchable`, plus
optional `label`, `default`, `context_limit`, and `output_limit`. Its
`models` list includes only launchable IDs; clients should use the richer
rows to display labels and availability. Unknown-tier or conflicting
duplicate catalog entries are not launchable. An empty catalog is a valid
response, not a guessed static model list.

Muse's catalog is read from cache. A stale cache, including the initial empty
cache, starts a background
refresh through `muse serve` initialization/model listing, without a session
or prompt; the request does not wait for it. Refreshes are coalesced, the cache
is fresh for two minutes, and a failed refresh preserves the last good answer.
Creating a Muse node rechecks the catalog before launch. Model availability
does not grant the separate launch consent documented under `/api/settings`.

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

Supported agents are `claude`, `codex`, `pi`, `opencode`, `grok`, `cursor`,
`dsh`, and `muse`. A cursor launch whose `model`/`effort` pair is absent from the
catalog is refused with `400` rather than passed to the CLI, and so is any
non-empty `effort` the catalog cannot resolve — cursor has no effort flag, so
an unresolved level would be dropped and the chat would run at a level nobody
chose.
Muse creation requires `muse_approval_judge_consent: true` in the computer's
settings; missing consent is `400`. Its requested model must be launchable
in the current catalog, or creation returns `400`. With no explicit model,
the server chooses an eligible Standard model; IDs ending in `-contributor`
require an explicit choice and are never an implicit default. The stored
node retains the user's model choice (including an empty default choice),
while the launch receives the resolved concrete ID.

A dsh create carries its model over the wire rather than on argv, and dsh is
the one agent for which that application is authoritative: if the live agent
does not offer the requested model, or refuses it (or the requested thought
level), the launch is refused, the process and its meta-only session log are
discarded, and the response is `400` naming what was rejected — never a chat
quietly running a different model from the one the node records. `400` means
the choice itself was wrong: the option is absent from the set the agent
advertised, or the agent answered the apply with JSON-RPC `-32602`. A failure
of the same call for any other reason — an internal error, a cancellation, a
transport that dies mid-launch — still fails the create, but as `500`, because
the model and thought level the user picked were not the problem.

Only launch-config fields are honored. Server-owned fields (`id`, `session_id`,
`transcript`, `created_at`, `ended_at`, `fork_kind`, `adopted`) are ignored if
present in the body — they are minted or derived server-side. There is no
endpoint for attaching or importing an externally started session.

### `PATCH /api/nodes/{id}`

Body: any of `title`, `description`, `lane_id`. Title must stay non-empty.
Lane assignment is write-once: setting it on an unassigned node succeeds,
changing an existing assignment is `409`.

To relabel an earlier map station, include `station` with that station's
start-time key and a non-empty `title`, plus optional `description`. This
appends a label to the session log for that station, leaving the current
node fields and lane assignment unchanged. An empty station key or title
is `400`; a missing session store is `409`. Omit `station` when editing the
current node.

### `POST /api/nodes/{id}/exit`

Close a thread deliberately: stamps `ended_at` durably and keeps the node
visible on the map with a dead-end cap (unlike `DELETE`, which removes it).
Idempotent — a second call keeps the original stamp. The `ended_at` record is
persisted first, then the owned process is stopped best-effort, so a failure to
reach the agent never un-ends the node.

Returns `{"node": <node>, "closed": true, "stopped": <bool>, "reason": <string>}`.
`stopped` reports whether the underlying process was actually stopped: it is
`false` with `reason:"adopted"` for a retained historical external record
(scimux never kills a session it did not start — the agent keeps running), and `false` with
`reason:"kill_failed"` when teardown was attempted but errored. Owned sessions
and structured-transport links are stopped (`stopped:true`, empty `reason`).
`/exit` is idempotent: a repeat call on an already-closed node does not tear the
process down again and reports its current standing (`reason:"running"` when the
agent is still live), so it never issues a second kill.

### `DELETE /api/nodes/{id}`

Appends a `delete` tombstone to the store and archives the node's session
log to `sessions/archive/`. Uploaded files are archived under
`attachments/archive/`; session assets are archived too. The node's owned
worker/agent is stopped, while a historical external tmux session is left running.
History is never destroyed. A teardown failure returns `500` and attempts
to re-assert the node in the store so it remains visible for retry.

Historical records with `adopted:true` retain readable saved history and can
be removed, but their external integration is retired: peek, send, resolve,
interrupt, key, and auto-approval requests return `409`. No worker is started
or resumed, and the external pane remains running. Forking such a record
creates a fresh scimux-owned chat without importing its history.

## Talking to a node

### `POST /api/nodes/{id}/send`

Body: `{"text": "…", "attachments": [<ref>, …]}`. Delivers a follow-up turn
(tmux: paste + Enter; structured transports: a protocol prompt). `409` while a
turn is still in flight, while a tmux send is unconfirmed, or after `/exit`
(`thread is closed; fork to continue`). On structured transports `"/clear"` is
implemented by scimux itself: a fresh protocol session on the same node,
recorded as a source seam — same page-turn semantics as Claude's `/clear`.
ACP replaces the agent subprocess; Codex opens a fresh thread on its existing
process, and Muse starts a fresh session on its connection. Muse `/clear`
requires the current `muse_approval_judge_consent` setting (`400` when off).
A dsh `/clear` that the replacement agent refuses because its saved model or
effort is no longer offered returns `400` and tells the user to fork with a
model the agent still offers; other replacement failures remain `500`.
Claude's page turn is confirmed by its own `SessionStart` clear hook, not
by successful pasting alone; an unconfirmed `/clear` does not retire history.
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
"<opaque>"}` for structured transports (Codex, ACP, Muse), or `{"key": "1",
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
epoch, not Claude's PermissionRequest identity. A missing required token is
`400`; an explicit token whose dialog is absent, retired, or mismatched is
`409`. Neither sends keys. A successful key retires that epoch only;
standing asks are not deleted as a guessed identity. Worker-managed Claude
nodes require the token for every `/key` call. The legacy tmux path accepts
`{key}` alone when no strict permission dialog requires an epoch.

Every accepted key is recorded in the store together with decision evidence
(the pane's bottom lines, or the tool title on structured transports); on
structured transports the decision is persisted *before* the agent learns
the answer.

### `POST /api/nodes/{id}/auto-approve`

Body: `{"enabled": true}`. Arms or disarms the server-owned, one-turn
auto-approval lease. Supported on the structured transports (Codex
app-server; pi, opencode, Grok, Cursor, and dsh through ACP; Muse through MSP)
and on Claude/tmux chats that scimux launched itself — those carry a hook
bundle registering a `PermissionRequest` hook, which is how a Claude tool call
can be answered at all. Two cases still return `400` and create no state: a
Claude pane scimux did not launch before the feature existed, so its
bundle has no permission rendezvous), and any other unsupported transport.
The lease is transient. Structured leases are owned by the muxer; Claude's
lease is owned by its session worker. Restarting that owner clears its lease;
a web-child replacement alone does not.

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
Send-to `forward_links`, lanes). The browser retains the newest 500 Send-to
links so this navigation index cannot grow without bound. GET sets an `ETag`
and honors `If-None-Match`. PUT requires
`If-Match` (the last ETag, or `*` to bootstrap) — `428` without it, `409` on
mismatch — and replaces the document atomically. Bodies over the size limit
are `413`.

## Settings

### `GET /api/settings` / `PUT /api/settings`

The **computer's** own settings (`~/.scimux/settings.json`). The server reads
and enforces these values itself; `/api/ui` holds the separate shared UI
document, whose contents are opaque to the server.

```json
{
  "claude_usage_checks": false,
  "muse_approval_judge_consent": false
}
```

`claude_usage_checks` is consent to run the Claude usage probe, which spends a
small amount of the user's own subscription quota (see `GET /api/usage`).

`muse_approval_judge_consent` permits launching Muse with its default
approval judge, which can consume the user's quota. It is required for new
Muse nodes (including forks) and `/clear`. It does not disable the judge,
grant a tool permission, or arm auto-approval. Turning it off prevents those
new launches/session resets; it does not stop an existing Muse session.

**Both consent fields default to off.** A missing, unreadable, empty,
oversized, or invalid settings file is read as no consent.

PUT **merges the supplied JSON object** into the stored document. Omitted
fields retain their current values, including unknown fields written by a
newer client. For example, `{"claude_usage_checks":false}` turns off the
Claude probe without changing Muse consent. To revoke both, explicitly send
both fields as `false`; `{}` changes nothing. Send booleans for consent:
`null` leaves the current value unchanged, while other non-boolean values
are `400`.

The response is the full stored document. Invalid JSON or incompatible field
types are `400`, an oversized body is `413`, and a save failure is `500`.
There is no ETag/revision precondition; writes are serialized, unrelated
fields survive concurrent updates, and the last write to a field wins.

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

With `?usages=1`, the same route returns a sparse reverse index for chat
markers instead: `{"usages":[{"note_id","note_title","section_id",
"section_title","reference_id","source"}]}`. Repeated references to the same
source in one section collapse to one row. Section bodies and frozen reference
snapshots are never included; the browser fetches the chosen full note only
after the user opens a destination.

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

Body: `{"expected_tag":"vX.Y.Z"}`, using the `latest` tag returned by the
update check. Missing or malformed input is `400`; if the latest release has
changed since the check, the apply is `409` and the client must check again.

Self-update: downloads the release binary for this OS/arch from GitHub's exact
HTTPS release hosts (`github.com` and `release-assets.githubusercontent.com`;
no userinfo, port empty or 443), verifies it against the release's
`SHA256SUMS` (the same allowlist applies to the initial URL and every
redirect), starts it as a standby
web-server child, proves that its private Unix-socket protocol is compatible,
and atomically replaces the installed executable only after size, close,
checksum, and compatibility checks succeed. The response then commits a
graceful web-child handoff followed by an in-place muxer exec. The replacement
muxer inherits the public listener and ownership lock, then reconnects to the
existing session workers. Agent processes and Claude tmux panes remain alive;
the session stores are preserved.
Binary downloads are capped at 256 MiB; a response that exceeds the cap is
rejected without accepting a truncated payload. Failed verification or
download, incompatible standby startup, or install leaves the installed
executable byte-for-byte unchanged, keeps the existing web generation serving,
and removes any `.scimux-update-*` temp file. `409` if
already up to date, a `dev` build, or an update is already in progress.
`502` on download/verify failure; `500` on install (chmod/rename) failure.

### `GET /api/licenses`

License texts bundled into the binary (shown in the About sheet).

### `GET /api/harnesses`

Which supported agent CLIs this computer has, in registry order:
`{harnesses:[{agent, present, launchable, has_source, installed, path}]}`. Local
only — never a network call, because the menu reads it on every open.
`has_source` says whether this harness has a public upstream version channel;
when false, the UI shows the installed version without implying that an
upstream check failed. `installed` is the first version-shaped token of
`<bin> --version`, empty when the output does not carry one. `present` and
`launchable` are separate facts: pi is installed as `pi` but launched through
`pi-acp`. The probe runs once per process, so a harness installed while scimux
runs appears after a restart.
For Muse, `launchable` is true when the `muse` executable is found on `PATH`.
This inventory flag does not grant permission to create a chat: creation
separately enforces approval-judge consent and model-catalog checks.

### `GET /api/harnesses/latest`

What each harness publishes upstream: `{latest:{<agent>:{version, source}}}`.
Reached only on an explicit tap, like the scimux update check — the server
never polls the registries. There is no single lane: four sources are npm
packages (codex, pi, opencode and dsh), grok is a plain-text channel file, and
Claude's depends on whether
it was installed natively (compared against the installer's own `stable`, not
the npm dist-tag, which it can never receive). Muse has a separate stable
channel metadata source. Its response format is not yet supported, so Muse
is currently omitted from `latest` even when the channel is reachable.
Sources are read concurrently and independently, and an agent whose source
failed is simply absent from the
map — "unknown" is what happened, and it is not the same claim as up to date.

## Remote pairing

Loopback pairing for `-remote`. Confirm is a separate explicit call;
mint never completes a pairing. CSRF is required on unsafe methods;
GET needs no CSRF header. Missing Origin and Referer are accepted for scripts,
but do not bypass the token requirement.

### `POST /api/remote/pairing`

Mint an 8-character pairing code with a 120-second TTL. Returns
`{code, rid, expires_at, state:"pending", link}`; `expires_at` is UTC
RFC 3339 with optional fractional seconds. Open the returned `link` on the
device to begin pairing. A v2 link opens `/p` on the configured trusted viewer;
its fragment `o` names the separate cryptographic rendezvous service. Pairing
material stays in the fragment rather than the path or query. Confirm flags in
the body are ignored. This split does not add CORS to the local HTTP API.

### `GET /api/remote/pairing/{code}`

State for a code (`pending`, `authority-warning`, `expired`, `cancelled`,
`failed`, or `succeeded`), plus SAS once a device offer has arrived.

### `POST /api/remote/pairing/{code}/confirm`

Complete pairing only when both `computer_confirm` and `device_confirm`
are present and `true`. Omitted flags do not default to true.
The operator asserts the device's half of the confirmation: the server cannot
tell whether a script actually showed the SAS on the device. Compare the SAS
on both devices before sending these flags; the booleans are not proof of that
comparison.

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
afterwards takes a new invite. Origin migration uses this operation while the
saved old rendezvous is still configured; it never rewrites the stored origin
or replaces the whole scimux data directory.

### `GET /api/remote/status`

The installation's hosted enrollment as `hosted` (`enrolled`,
`disabled`, `revoked`, `unavailable`, `""` after an unlink, or one of
the FR-30 broken states) plus `can_pair`, and a `devices` array. Each
device is `{"id":"...","connected":true}` when its tunnel is live.
When it is not, `connected` is false; `cause` is one of the seven FR-24
states (`rendezvous-unavailable`, `computer-offline`,
`signalling-rejected`, `ice-failed`, `auth-failed`,
`connected-then-lost`, `tunnel-version-mismatch`) only when the transport
has produced one. `tunnel-version-mismatch` means a MAJOR tunnel protocol
incompatibility; a MINOR difference does not produce this cause. A
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
