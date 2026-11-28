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
- **Browser response policy.** Successful and error responses carry a
  deny-by-default Content Security Policy (`script-src 'self'`, no objects,
  bases, forms, or framing), `X-Frame-Options: DENY`, `nosniff`,
  `Referrer-Policy: no-referrer`, and a restrictive Permissions Policy. Inline
  styles remain allowed for dynamic layout and lane colours; inline scripts
  and script attributes do not. The frozen `/assets/` compatibility surface
  retains its historical header shape, while executable CSS/JS handlers set
  `nosniff` themselves.

Binding `-addr` wider than loopback still exposes full controller access
to every client that can reach an allowed Host.

**Compression:** when the client sends `Accept-Encoding: gzip`, eligible
responses are gzip-compressed (`Content-Encoding: gzip`, `Vary:
Accept-Encoding`). Compression applies across `/api/*` and the static tree for
compressible content types (JSON, text, JavaScript, SVG, …). Already-compressed
payloads (images, PDF, zip, …) and empty short-circuit responses (`304`,
`204`) are left alone so poll validators stay intact.

## Reading state

### People reaction records

People reactions use the existing authenticated comments log and unchanged
relay envelope. The relay treats each body as opaque. scimux application
records use this versioned shape:

```json
{"v":1,"t":"reaction","target":42,"emoji":"thumbs_up","active":true}
```

`target` is the earlier comments sequence of a valid People comment, including
a file-only comment. The participant is the authenticated relay envelope
author; a body-supplied actor is invalid. A target comment author cannot react
to their own comment. Supported identifiers, in fixed display order, are
`celebration` (🎉), `thinking` (🤔), `laughing` (😂), `heart` (❤️, including
U+FE0F), `thumbs_down` (👎), and `thumbs_up` (👍). Each event is an explicit
desired state. The latest valid retained event for a target, participant, and
emoji wins; removal appends `active:false` and never edits history.

The local routes are scoped by server, owner, chat, and generation:

* `GET /api/team/subscriptions/{server}/{owner}/{chat}/{generation}/reactions?after=N&copy_instance=HEX`
  reads at most 256 log entries and 4 MiB per page. `after` is an optional
  canonical decimal sequence (default `0`); `copy_instance` is the 32-digit
  local copy identity and is required by the UI to fence stale views. The
  response is `{ "events": [...], "last": N, "next": N, "more": false }`;
  events contain the validated record fields plus `seq`, `key`,
  `target_author`, and viewer-relative `mine`. Invalid events are omitted.
  This read is available from a retained local copy after the share is offline
  or ended.
* `POST` to the same path requires `copy_instance` and the ordinary
  `X-Scimux-CSRF` header. JSON is exactly
  `{ "target": 42, "emoji": "thumbs_up", "active": true, "key": "0123456789abcdef" }`.
  Bodies are limited to 4096 bytes; unknown fields are rejected. The key is a
  caller-generated 16-digit lowercase hexadecimal idempotency key, scoped by
  the route's generation. `200` returns `{ "seq": N }`; that acknowledgment
  alone does not confirm a payload. Clients confirm only when the matching
  validated retained event is read back. A retry after an uncertain response
  must repeat the exact body and key. Pending writes are never replayed
  automatically. `400 bad_request`, `403 read_only`, `404 not_found`,
  `409 not_active|copy_changed|copy_instance_required`, `413` for an oversized
  body, `500` for local storage/identity failure, and `502 refused|offline|changed`
  are possible. Writes require current live write access, an online joined
  server, and an unchanged generation, copy identity, membership and server
  configuration. These are local UI routes; no relay upgrade is needed.

Reaction events are a separate `reactions` property in the ordinary comments
read response and never appear in `comments.entries`. Reaction-only records
are neutral to upgraded unread classification; earlier unread human comments
remain unread, and unknown gaps remain unknown. Classification does not
advance `/seen`. Older clients skip the opaque reaction body and preserve the
conversation, but their existing unknown-unread fallback may trigger when
skipped sequence numbers make the comments projection discontinuous.

### `GET /api/state`

The polling snapshot the UI runs on. Returns:

```json
{
  "nodes":     [ { …node fields…, "live": "quiet", "attention": "approval",
                   "has_transcript": true, "last_activity": 1752849600000,
                   "ctx_pct": 37, "storage_bytes": 1048576,
                   "storage_writable": true,
                   "stops": ["2026-07-24T09:12:00Z"] } ],
  "sys":       { …host load/memory… },
  "storage":   { "used_bytes": 8388608, "free_bytes": 4294967296,
                   "min_free_bytes": 67108864, "writable": true },
  "socket":    "scimux",
  "hostname":  "workstation",
  "version":   "v1.0.0"
}
```

Each node also has a server-minted `instance_id` identifying that actual chat
across reloads and reconnects. A deleted node's reusable `id` can later name
a different instance. Existing live and closed nodes receive a persisted identity
during legacy store migration. The instance label is public and is not a credential.

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
present, is the live segment's context occupancy percentage. Token spend is
the separate trio `fare_fresh_in`, `fare_out`, and `fare_total`, present once
a segment has counted turns. For `vibe` each stored turn is the increase over
the previous cumulative counters in that ACP session. Vibe's input counter
includes cached prompt tokens and does not report a fresh/cache split, so
that increase is `fare_unsplit_in` and is not counted as `fare_fresh_in` or
`fare_cache_read`. `fare_total` for `vibe` is that unsplit input plus
`fare_out`. A simultaneous drop in Vibe's cumulative token counters starts
a new baseline, as can happen after plan acceptance. If an interrupted turn
returns no token breakdown, its reported USD cost is retained on that turn;
the next cumulative token report establishes a new baseline because it cannot
separate the interrupted turn's tokens from the next turn's. `/clear` and a fork
start a new session, so the next increase is measured from zero. `fare_cost`
is paired with `fare_cost_complete`. For `vibe` that cost is the sum of
positive USD increments Vibe reported for those turns: a Vibe-reported cost.
When a counted turn has no trustworthy USD increment, `fare_cost_complete` is
false and the cost is unknown. Vibe's unsplit input does not drive the metro
map's comparative heat width; its ticket labels the figure as reported tokens
and says cached input may be included. A missing occupancy update leaves `ctx_pct`
unset. The gauge reads occupancy only. `stops` are the
node's `/clear` page-turn timestamps; the map prepends `created_at` to draw the
full station chain. `ended_at` (RFC 3339, present only once set) marks a thread
deliberately closed via
`POST …/exit`: the node stays visible with a dead-end cap on the map rather
than being deleted. Live tmux inventory is not exposed; it remains internal
input for owned-session liveness and name-collision checks.

`storage_bytes` is the live node's session log, attachments, and stored asset
blobs; `storage_writable` also accounts for that node's configured limit. The
top-level `storage` snapshot covers those managed stores globally,
including archived history; it reports usage/free space, effective limits,
whether writes are currently possible, and a `reason` when not. It is sampled
at most once per 30 seconds so polling does not rescan history every second.

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
include `turns`, `last_change`, `chat_started`, `prior_turns`, `log_uid`,
`segment_index`, context-gauge
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
`pi`/`opencode`/`grok`/`cursor`/`dsh`/`vibe`/`copilot`,
and Muse MSP), `source` is `acp` for compatibility across these transports;
there is no pane fallback. Pending approval details are returned as
`perm_title`, `perm_options`, `perm_tool_kind`, `perm_reason`, and — only
while a permission is pending — an opaque `perm_request_id` that
`POST …/key` must echo back. When an ACP permission request names a tool-call
id and no title, `perm_title` is the latest title and command scimux recorded
for that id earlier in the same turn, and `perm_tool_kind` is filled from
that record when the request omits a kind. Option ids, names, and order stay
the agent's own, including two `allow_always` options that differ by scope.
A request with no trustworthy title uses `perm_title` `Unknown tool` and
still waits for a human choice. Idle structured
chats and tmux nodes do not invent a request id. For a strict Claude
permission dialog the echo token is `perm_dialog_id`, a **server-minted
visible-dialog epoch**. Claude's `Notification(permission_prompt)` payload
has no PermissionRequest id; scimux does not claim exact request correlation.
Structured-node turn failures may also set `error`.

`GET /api/nodes/{id}/chat?history=1` returns the whole log as ordered read-only
surfaces: `{"segments":[{"start","seam","reason","turns","log_uid","segment_index"}, …], "empty_segments":[...]}`. Empty segment addresses are provided separately so owner discussion can attach to a clear seam without changing the ordinary history surfaces. This is the
on-demand path behind "show earlier history" and earlier metro stops; normal
polling stays current-segment only. The history branch does not participate in
ETag short-circuiting.

On the polled (non-history) path, responses carry an `ETag`; polling clients
may send `If-None-Match` and receive `304 Not Modified` when the snapshot is
unchanged. The tag hashes the marshalled response without `last_change`,
`watermark`, `progress`, `pending_calls`, `waiting_on`, and `ctx_used`; these
fields remain in the 200 body. Attention, needs-input state, `ctx_pct`,
`ctx_window`, delivery, Claude `compacting`, and `elicitation_waiting` remain
hashed, so changes to them still invalidate the cache.

The polled path may request a bounded recent-turn window with `?tail=N`, where
`N` is an integer from 1 through 500, or an anchored window with `?start=I`,
where `I` is a non-negative segment-absolute turn index. They cannot be used
together. Invalid `tail` values return `400 bad tail`; invalid `start` values
return `400 bad start`; both together return `400 tail and start`. A `start`
greater than the segment length is clamped to its length, and the returned
window begins exactly at that index without snapping.

For `tail`, the server includes at most `3*N` turns, starting at the latest
user turn at or before the requested tail when that complete cycle fits under
the cap; otherwise it uses a hard cut at the requested tail. Both window modes
include `omitted_turns`, the count before the returned window. When a window
starts in the middle of a cycle, `window_head` gives the durable `uid`,
`segment`, and `record` address of that cycle's first assistant turn. When
turns are omitted, `decisions` contains records at or after the first returned
turn; with no omitted turns all decisions remain. `lease_base` maps each
retained lease with omitted decisions to the number omitted so clients can
continue its ordinal. The `?history=1` path takes precedence and ignores
`tail` and `start`. Without either window parameter, the polled response
remains the full current segment.

The bounded page path is `?page=before|after|around&at=R&log_uid=UID` with an
optional `limit=N` (default 100, range 1–200). It is a read-only, non-polled
response with no `ETag`. `before` returns up to N readable turns with record
less than R; `after` returns up to N with record greater than R; `around`
returns the addressed readable turn and its neighbours, up to N turns total.
Pages can cross segment seams. The response contains `segments` of sliced
history, `empty_segments`, `before_count`, `after_count`, `current_segment`,
`assets`, and `allow_external_attachments`. Each slice includes its segment
header and turns, plus `segment_offset`, `segment_turns`, and any applicable
`window_head` and `lease_base`. Only assets referenced by returned turns are
included. Missing or invalid parameters return `400`; a changed `log_uid`
returns `409 log_changed`; `around` at a record that is not a readable turn
returns `404 no_turn`. `?history=1` takes precedence over page parameters and
keeps the existing complete-history response unchanged. Page reads use the
cached log index and read only the selected records.

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

### `POST /api/reference-media`

Durably capture supported managed raster images referenced by one exact stored
turn. The body is:

```json
{"source":{"uid":"a1b2c3","segment":0,"record":7}}
```

All three fields are required; zero is a valid explicit ordinal. The server
resolves the stored turn and its time-correct asset bindings. Callers cannot
supply text, a path, URL, MIME authority, bytes, or a blob digest. Success
returns the server-projected `text` and a versioned `media` descriptor whose
items are ordered and whose ids are decimal indexes. Ready items contain only
`id`, namespaced `key`, display `name`, and `state`; unavailable source items
are explicit. Images are limited to PNG, JPEG, GIF, and WebP, at most 32
distinct descriptors, 50 MiB per image, 50 MiB total image bytes, and a 64 KiB
manifest. A limit failure rejects the complete capture with `413`; storage
budget exhaustion is `507`; invalid input is `400`; an absent exact source is
`404`; unexpected storage I/O is `500` without local path disclosure.

New captures are immutable and remain byte-identical after source deletion,
slug reuse, `/clear`, log rotation, restart, and browser reopen. Historical
bookmarks without `media` are not migrated; the browser may perform one
bounded exact-address `/api/preview` lookup while the source remains live.

### `GET /api/reference-media/{captureID}/assets/{itemID}`

Serve one ready capture item. `captureID` is exactly 64 lowercase hexadecimal
characters and `itemID` is canonical non-negative decimal. Malformed ids are
`400`; unknown, unavailable, missing, or corrupt entries are `404`. Successful
responses preserve the original encoded bytes, use the canonical safe raster
Content-Type, and carry `nosniff` plus the common browser security headers.
`HEAD` has the same routing and headers without a response body. There is no
general blob or filesystem download route.

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

Most catalogs are probed once and cached for cheap dialog opens. The explicit
`GET /api/harnesses/latest` action replaces that cache from the installed
CLIs, so newly installed harnesses, provider model releases, and user-edited
pi/OpenCode configuration appear without restarting scimux. dsh has no list
command and no read-only discovery surface: naming its models would mean
opening an ACP session, which dsh flushes to its own durable history and offers
no way to delete, so scimux never opens one for discovery. dsh's `model` and
`effort` are therefore settable only through the API, and the strict
configuration path they exercise is not reachable from the dialog. dsh
reports an empty `models` and no `efforts`; the dialog offers only
"(default)", it launches on the profile's own default model, and the thought
levels that model accepts are read from the live session at launch — the UI's
static effort list for dsh is deliberately empty, because dsh's levels are
`off`/`low`/`high`/`max` for models whose route reasons and absent for the
rest.

`vibe` is offered when `vibe-acp` is on `PATH`. Startup, menu open,
`GET /api/agents`, and an ordinary `GET /api/harnesses/latest` do not execute
`vibe-acp`. They publish an empty `models` list and no `efforts`. The dialog
offers (default) model and (default) thinking. Launching that pair sends no
ACP model or thinking setter; Vibe chooses its own default.

`copilot` is offered when the user-installed `copilot` binary is on `PATH`,
the same presence-only rule. Its default launch is `copilot --acp --stdio
--no-auto-update`, with Copilot's own default model and reasoning level.
Startup, menu open, `GET /api/agents`, and an ordinary `GET /api/harnesses/latest`
do not execute `copilot` beyond its version read, and publish an empty `models`
list and no `efforts`.

`POST /api/harnesses/latest` with JSON `{"inspect_copilot": true}` is a
separate one-shot request that reads the model menu of one temporary ACP
session (`initialize`, `session/new`, `session/close`, never `session/prompt`).
It publishes the advertised models, and efforts only for the model the session
reports as current. An option is offered only when it has no `copilotEnablement`
key or its value is `"enabled"`; any other state is not offered, and an explicit
choice of it is refused. A failed, timed-out, or empty inspection leaves the
default launch. Closing the session does not prove Copilot deletes it from its
own history. The field is case-sensitive and follows the same boolean rules as
`inspect_vibe`; one body may name both keys.

`POST /api/harnesses/latest` with JSON `{"inspect_vibe": true}` is the one
request that runs the catalog probe. The field has to be the boolean `true`.
A missing field, `false`, `null`, an empty body, and every GET — including a
query string or a JSON body carrying the same name — stay ordinary refreshes
and do not start `vibe-acp`. The field name is case-sensitive. Malformed JSON,
or `inspect_vibe` as a string, number, or array, is `400` before any refresh or
probe; a body over 1 MiB is `413`. The probe is one temporary ACP session:
`initialize`, `session/new`, and `session/close`. That probe never calls
`session/prompt`, never approves a tool, and never grants filesystem or
terminal access. Model ids are the advertised option values in the agent's
order; an accidental `currentValue` is not added and is not a scimux default.
The initial thinking menu's value ids are shown for each advertised model;
an explicit selection is validated again against the live session at launch.
The probe's protocol budget is 12 seconds after process startup.
`initialize`, which also waits for the agent to start, may take up to
8 seconds; each later call is limited to 4 seconds and close to 2 seconds
within that budget.
A failed, timed-out, or empty catalog keeps the
empty `models` list: the chat still launches on Vibe's own default, and the
response invents no model, thinking level, or success state. After a
successful inspection, `GET /api/agents` returns that cached catalog until
the next ordinary refresh replaces it with the empty offer. Explicit
`model` then `effort` are applied in that order on launch and on `/clear`,
using the ids the session advertised (`thought_level`, or Vibe's `thinking`
option). With installed Vibe 2.25 those setters persist in Vibe's config.
A choice the session does not offer is `400` before the session is
recorded. When Vibe cannot create a session or answer a prompt because its
configuration or authentication is unusable, that failure is returned on
the existing launch and prompt error path and no chat is recorded for a
failed session create. Context occupancy (`ctx_pct` on the state snapshot, and
`ctx_used` / `ctx_window` / `ctx_pct` on the chat payload), token spend, and
any Vibe-reported cost are separate figures. The state snapshot is where
token spend and the Vibe-reported cost are projected.

Claude's model IDs also have a persistent cache, refreshed on a CLI
version change or after one day;
`GET /api/agents` can trigger that refresh. Its model probes submit no billed
prompt and are independent of consent for the usage gauge.

An empty top-level object is a successful, authoritative snapshot: no
supported harness is currently launchable. Clients replace their catalog with
it; only a failed request preserves the previous catalog.

Muse additionally returns `muse_models`, an array of objects with `id`,
`tier` (`standard`, `discounted`, or `unknown`), and `launchable`, plus
optional `label`, `default`, `context_limit`, and `output_limit`. Its
`models` list includes only launchable IDs; clients should use the richer
rows to display labels and availability. Unknown-tier or conflicting
duplicate catalog entries are not launchable. An empty catalog is a valid
response, not a guessed static model list.
The browser omits unlaunchable rows and displays model names without tier badges.
Production classification uses the reviewed exact Muse Spark 1.1, 1.2, and 1.3
IDs and the 1.2/1.3 Contributor IDs; new IDs remain unknown until their terms
are reviewed. Live catalog membership is still required.

Muse's catalog is read from cache. A stale cache, including the initial empty
cache, starts a background
refresh through `muse serve` initialization/model listing, without a session
or prompt; the request does not wait for it. Refreshes are coalesced, the cache
is fresh for two minutes, and a failed refresh preserves the last good answer.
The explicit harness-update check bypasses that freshness window and waits for
the same coalesced read before returning its catalog snapshot.
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
`dsh`, `muse`, `vibe`, and `copilot`. `copilot` is offered when the user-installed
`copilot` binary is on `PATH` and launches `copilot --acp --stdio --no-auto-update`.
Model and effort travel as session options the CLI advertises, never on argv. `vibe` is offered when the user-installed
`vibe-acp` binary is on `PATH`. It has no static model list: an empty
`models` array means the chat launches Vibe's own default and sends no ACP
model or thinking setter. An explicit model, then an explicit thinking
level, uses the advertised ACP option ids. With installed Vibe 2.25 those
choices persist in Vibe's config. When Vibe has no usable default, session
creation fails with Vibe's own error and the node is not recorded.
A cursor launch whose `model`/`effort` pair is absent from the
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
Contributor creation also requires `muse_contributor_acknowledged: true` in
this request, acknowledging Meta's retention/training terms and the prohibition
on submitting confidential, sensitive, or personal information. Missing or false
acknowledgment is `400`, before any session or prompt. Forks require a fresh
acknowledgment even when the model is inherited. The field is request-only:
it is neither a stored node field nor a computer setting. It does not replace
the separate approval-judge consent. The browser shows the notice and an unchecked
checkbox above Start, resets it when the model changes or a new dialog opens,
and disables Start until acknowledged. Existing chats and `/clear` retain their
existing session policy; this acknowledgment gates new node creation.

A dsh create carries its model over the wire rather than on argv: if the live
agent does not offer the requested model, or refuses it (or the requested thought
level), the launch is refused, the process and its meta-only session log are
discarded, and the response is `400` naming what was rejected — never a chat
quietly running a different model from the one the node records. `400` means
the choice itself was wrong: the option is absent from the set the agent
advertised, or the agent answered the apply with JSON-RPC `-32602`. A failure
of the same call for any other reason — an internal error, a cancellation, a
transport that dies mid-launch — still fails the create, but as `500`, because
the model and thought level the user picked were not the problem. An opencode
create applies its chosen model the same way when the live agent advertises a
model menu, but effort remains tolerant. If the model is absent and its
provider has no entries in that menu, scimux sends the raw model value and
retries `-32602` refusals every 100 ms for up to 5 seconds while the provider
loads. A model from an already listed provider gets one attempt; a remaining
`-32602` refusal is `400`, and other errors are `500` without retry. An opencode
agent with no model menu keeps its own default.

Only launch-config fields are honored. Server-owned fields (`id`, `session_id`,
`transcript`, `created_at`, `ended_at`, `fork_kind`, `adopted`) are ignored if
present in the body — they are minted or derived server-side. There is no
endpoint for attaching or importing an externally started session.

### `PATCH /api/nodes/{id}`

Body: any of `title`, `description`, `lane_id`. Title must stay non-empty.
Lane assignment is write-once: setting it on an unassigned node succeeds,
changing an existing assignment is `409`.

The optional `chat_instance` query or `X-Scimux-Chat-Instance` header binds
the edit to the actual node instance, with the same matching rules as `/send`.
The description editor uses the query so the check also survives remote access.

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

The optional `chat_instance` query or `X-Scimux-Chat-Instance` header binds
the deletion to the actual node instance, using the matching rules of `/send`.
The current browser supplies the query so a delayed request cannot delete a
replacement that reused the same node ID. Legacy callers may omit it.
If the current browser cannot obtain an instance identity, it reports that
removal is unavailable until reconnection instead of submitting an unbound delete.

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

The current browser sends the node's server-minted `instance_id` as the
`chat_instance` query parameter and as `X-Scimux-Chat-Instance`. The query
survives the existing remote transport, which drops unlisted headers. A
mismatched, empty or repeated query precondition is rejected with 409,
including when a deleted node's reusable ID now names another chat. If both
carriers are present, both must match. Legacy callers may omit them; this is
an identity precondition, not an authorization credential.

Body: `{"text": "…", "attachments": [<ref>, …]}`. Delivers a follow-up turn
(tmux: paste + Enter; structured transports: a protocol prompt). `409` while a
turn is still in flight, while a tmux send is unconfirmed, or after `/exit`
(`thread is closed; fork to continue`). On structured transports `"/clear"` is
implemented by scimux itself: a fresh protocol session on the same node,
recorded as a source seam — same page-turn semantics as Claude's `/clear`.
ACP replaces the agent subprocess; Codex opens a fresh thread on its existing
process, and Muse starts a fresh session on its connection. Muse `/clear`
requires the current `muse_approval_judge_consent` setting (`400` when off).
A dsh or opencode `/clear` that the replacement agent refuses because its saved
model (or, for dsh, effort) cannot be reapplied returns `400` before writing a
seam, leaves the old session untouched, and tells the user to fork with a model
the agent still offers; other replacement failures remain `500`.
Claude's page turn is confirmed by its own `SessionStart` clear hook, not
by successful pasting alone; an unconfirmed `/clear` does not retire history.
A Claude send whose leading slash command is `/fork` or `/fork …` is `400`
(Claude's native fork is not supported; use `POST /api/nodes` with `parent`
to create a fresh node). Ordinary prose that merely contains the text
`/fork` is accepted. Claude SessionStart sources accepted by the hook are
`startup`, `clear`, `compact`, and `resume` only.

An optional `"trace": true` on an owner follow-up asks for an exact
prompt address in `trace: {uid, segment, record}`. The address is
returned only when serialized admission and session-log readback identify one
unambiguous new user row in the same log. It is not delivery confirmation;
unsupported or ambiguous sends omit `trace`. Ordinary responses are unchanged.
For owned Claude, scimux verifies the complete transcript and mirror frontier
before the existing send, then waits briefly for one uniquely attributable
mirrored user record with a later transcript timestamp. Mirror lag, changed
bindings, ambiguous rows, missing timestamps and unconfirmed sends can omit
the address; the worker remains the sole mirror writer. `/clear` does not
provide a trace address. To inspect a returned
address, `GET …/chat?trace_uid=<uid>&trace_segment=<n>&trace_record=<n>` adds
`trace_outcome`: `pending`, `confirmed`, `rejected`, or `unknown`. An actual
assistant response before a later user, source seam, or error is positive
evidence; an error first is classified as rejected and an intervening boundary
is unknown. The error classification does not prove the transport never
accepted the prompt; it only reports the first error in that interval.
A queue row, HTTP acknowledgment and matching text alone are not delivery
evidence. Trace polling does not send or retry a prompt. This uses the same
node ownership and security boundary as the existing send and chat routes.

`attachments` is optional; each element is a reference returned by
`POST …/attachments` (below). Every ref must point inside this node's own
upload directory (others are rejected `400`). For each, a plain-text line
`[attached image|file: /abs/path]` is appended to the delivered prompt so the
agent reads the file — one uniform mechanism across all transports (no inline
image bytes). When the chat is shared, the copy mirrored to recipients
carries only the file name (`[attached file: notes.txt]`); the agent and the
local log keep the absolute path. Agent replies are mirrored as written.
A turn may carry text, attachments, or both; an attachments-only
turn is valid, but an entirely empty one is `400`.

### `POST /api/nodes/{id}/attachments`

The optional `chat_instance` query and `X-Scimux-Chat-Instance` preconditions have the same meaning as for
`/send`; it is checked after the multipart body arrives and before it is stored.

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

### `POST /api/nodes/{id}/asset-imports/retry`

Retries one persisted, previously blocked agent file reference without sending
an agent turn. The JSON body is
`{"turn_record": <record index>, "occurrence": <zero-based reference index>}`.
Clients cannot supply a path: the server looks up that exact reference in the
append-only session log, validates that it belongs to the named node and turn,
and rechecks current filesystem and settings eligibility. The usual mutation
security checks apply.

In the chat response, any blocked reference whose current file exceeds the
import limit is labeled `too_large` with no retry action. A reference stored
as `not_found` or `too_large` is labeled `retry_ready` only when it now names
a regular file within the import limit and the node workspace, or within the
limit with external attachments enabled. For those references, a file within
the limit but outside the workspace is labeled `outside_workspace` while that
setting is off. These labels change only the chat view, not the stored import
reason. Retry always rechecks eligibility.

Success is `200` with `status` equal to `imported` (including `asset_id`,
`name`, `turn_record`, `occurrence`, and `current_bytes:true`) or
`already_imported`. Repeated and concurrent requests are idempotent. A disabled
external-file setting or unavailable chat returns `409`; a missing recorded
reference returns `404`; a missing, unreadable, nonregular, oversized, or
storage-refused file returns `422` with a stable `reason` and explanatory
`message`. A successful retry snapshots the file's bytes at retry time and
binds the new asset explicitly to the earlier referenced turn, even after
later turns or a `/clear` seam.

### `POST /api/nodes/{id}/send/resolve`

Resolve an unconfirmed tmux delivery (the pane never acknowledged the
paste): clears the unconfirmed-send state so the composer unblocks. `409`
while a send is actively submitting.

### `POST /api/nodes/{id}/send/interrupt`

Interrupt the node's in-flight turn.
The optional `chat_instance` query and `X-Scimux-Chat-Instance` preconditions have the same identity
meaning as for `/send`. Interrupt admission remains independent of an active
send so a current chat can still be interrupted.

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

### `POST /api/nodes/{id}/claude-interaction`

Body: `{"request_id","action"}` plus the fields that action uses. `action`
is `answer`, `discuss`, `approve`, `request_changes`, `cancel`, `terminal`, or
`native`. `answer` carries `answers` of `{question_id, option_ids,
free_text}`. `discuss` and `request_changes` carry non-empty `feedback`.
`approve` may carry `mode` of exactly `restore`, `default`, or
`acceptEdits`. The server looks up question text, option labels, and plan
text from the hook request it already holds. The body cannot replace that
request, supply hook JSON, a filesystem path, or any `updatedPermissions`
document.

`native` carries the current `dialog_id` and one server-issued
`question_id`/`option_ids` selection. The server resolves its key from the
current visible dialog; clients never submit key numbers. It is available only
after structured answering expires and only for one visible single-select
question whose exact prompt text and labels match, including the native AX
header, quote prefix, and recognized trailing `Other`/`Chat about this` rows.
Multi-select and free-text answers require the terminal. The visible
notification epoch is not an exact Claude request identity. `terminal` can
hand an expired standing question to the existing native controls after
validating the current question, menu, and dialog epoch; it records the
handoff, sends no key, and keeps the question visible and in attention. Those
controls are bound to that epoch. A manual or native key is fenced as
unconfirmed before send; a fresh notification cannot replay it. The question
card reports manual handoff, recorded structured answers, sent keys, and
uncertain delivery as distinct non-retryable states.

`202` means the owning worker durably recorded the decision (`status:
accepted`, `delivery: recorded`, `emitted: false`). It does not mean Claude
consumed the reply. `emitted` means only a complete helper stdout write;
this feature does not register a consumption-confirmation hook. Unconfirmed
delivery is not replayed. A failed post-audit submission appends a correlated
application outcome (`source: application`, correlated by `request_id`, with
only delivery status and fixed context in `outcome`). It repeats no human
content or raw errors: `not_delivered` means known rejection, `unconfirmed`
means I/O uncertainty. Recording intent does not assert approval reached
Claude. A known stale, replaced, canceled,
handed-off, expired, or competing request is `409`. Malformed or incomplete
input is `400`, including a body over 32 KiB. Feedback and each free-text
answer are at most 4096 UTF-8 bytes, checked by both browser and server.
`/send` stays `413` at 1 MiB. An unsupported
node, a worker that does not advertise `claude-interaction-v1`, or a legacy hook
bundle without interaction support is `404`. All new Claude hook bundles
support structured questions and plans without a feature flag.
While the interaction owns the request, `POST …/key` for that node is `409`
and no native key is delivered, including when ownership appears after the
key was prepared. `terminal` revokes that ownership first; the
visible-dialog epoch on `…/key` is then authoritative again. Loss of the helper's
waiter lock rejects answers and releases native controls before the deadline;
recovery removes that dead request's active slot. An emitted
cancellation settles on the first quiet pane tick while its accepted turn is
still current or closing. A `202` proves the cancellation was recorded, not
that it was emitted or that Claude consumed the reply. Settlement does not
read the transcript, and it does not settle on an active tick. Settlement
closes that accepted-turn token. A cancel for a turn never closes the next
one. The cancel fence kept after payload retirement still settles, including
across a worker restart. Quiet is a pane snapshot, not proof that Claude
consumed the reply.
`perm_dialog_id` stays that epoch and is not the hook request id. Questions
and plans are never auto-approved, including while a lease is armed.

The current chat snapshot may include `claude_interaction` (`request_id`,
`phase`, `tool`, questions or plan text, mode choices, `native_keys`).
An outstanding question may use phases `native`, `manual`, `recorded`, `sent`,
or `unconfirmed` after the helper expires. Native choices include
server-resolved question and option ids; `sent` and `unconfirmed` are not
retryable and do not assert Claude consumed a key. `manual` means the current
dialog was handed to the existing controls. The question remains visible and
in attention until transcript evidence or a session lifecycle event settles
it.
`GET …/chat?history=1` does not.

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
links so this navigation index cannot grow without bound. Confirmed prompt
links may contain a bounded submitted-prompt preview for display; navigation
uses the exact destination UID, segment, and record, not that preview. GET sets an `ETag`
and honors `If-None-Match`. PUT requires
`If-Match` (the last ETag, or `*` to bootstrap) — `428` without it, `409` on
mismatch — and replaces the document atomically. Bodies over the size limit
are `413`.

## Settings

### `GET /api/settings` / `PUT /api/settings`

The **computer's** own settings (`~/.scimux/settings.json`). The server reads
and enforces consent and attachment settings and stores the shared harness-logo
preference. `/api/ui` holds the separate shared UI document, whose contents
are opaque to the server.

```json
{
  "claude_usage_checks": false,
  "muse_approval_judge_consent": false,
  "allow_external_attachments": false,
  "use_accent_harness_logos": false,
  "storage_global_limit_bytes": 0,
  "storage_node_limit_bytes": 0,
  "storage_min_free_bytes": 67108864
}
```

`claude_usage_checks` is consent to run the Claude usage probe, which spends a
small amount of the user's own subscription quota (see `GET /api/usage`).

`muse_approval_judge_consent` permits launching Muse with its default
approval judge, which can consume the user's quota. It is required for new
Muse nodes (including forks) and `/clear`. It does not disable the judge,
grant a tool permission, or arm auto-approval. Turning it off prevents those
new launches/session resets; it does not stop an existing Muse session.

`allow_external_attachments` permits regular files referenced by an agent
outside that chat's working directory to be copied into durable chat history
under the scimux process's existing OS permissions. It defaults to false and
missing, unreadable, or malformed settings fail closed. Workers read the
server-owned setting for every import, so changes apply without restarting a
chat. Turning it off blocks later imports and retries but does not remove
already imported snapshots or automatically revisit older blocked references.

`use_accent_harness_logos` selects the accent-color CLI harness badges in all
connected browser views. It defaults to false, which uses the matching
monochrome badges. Changing it affects the current browser immediately;
other browsers pick up the change when they next read settings.

The storage fields are non-negative integer byte counts. A zero global or
per-node limit means unlimited; a zero minimum-free value disables that
reserve. A missing `storage_min_free_bytes` uses the 64 MiB safety default.
The global budget covers `nodes.jsonl`, session logs, attachments, asset blobs,
and their archives. The per-node budget covers that live node's session log,
attachments, and asset blobs. Before a managed write, all scimux processes
serialize the usage check through an owner-only lock. A write that would cross
a limit fails without truncating or deleting existing durable history.

**Both consent fields, external attachments, and accent logos default to off.** A missing,
unreadable, empty, oversized, or invalid settings file is read as no consent
and no permission to import external attachments.

PUT **merges the supplied JSON object** into the stored document. Omitted
fields retain their current values, including unknown fields written by a
newer client. For example, `{"claude_usage_checks":false}` turns off the
Claude probe without changing Muse consent. To revoke both, explicitly send
both fields as `false`; `{}` changes nothing. Send booleans for consent and
non-negative integers for storage limits:
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
    "text": "the cited turn",
    "media": {"version":1,"capture_id":"<64 lowercase hex>","items":[]}
  }
}
```

The optional media field is accepted only when its capture exists and its
manifest source exactly matches `source`; the server persists the canonical
stored descriptor rather than caller-supplied item fields. The reference is
self-contained when it carries captured media and still renders after the
source node is deleted. Older references without media retain read-only,
best-effort live-source compatibility and are not retroactively durable.

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
The browser calls this route both from the version-row refresh control and in
parallel with an explicit harness update check.

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

License texts bundled into the binary (shown in the About sheet). Returns a
JSON object keyed by the About sheet's license identifiers, with each value
holding the corresponding full notice.

### `GET /api/harnesses`

Which supported agent CLIs this computer has, in registry order:
`{harnesses:[{agent, present, launchable, has_source, installed, path}]}`. Local
only — never a network call, because the menu reads it on every open.
`has_source` says whether this harness has a public upstream version channel;
when false, the UI shows the installed version without implying that an
upstream check failed. For every harness except Vibe, `installed` is the
first version-shaped token of `<bin> --version`, empty when the output does
not carry one. For Copilot the read is `copilot --version --no-auto-update`,
which reports the launched executable rather than a newer previously downloaded
build; scimux never runs `copilot update` or `copilot version`. Vibe is detected through `PATH`, then its installed version is
read from adjacent `mistral-vibe` package metadata when one unambiguous copy
is present. Startup inventory, menu open, `GET /api/agents`, and an ordinary
refresh do not execute `vibe-acp`; an unsupported or ambiguous install has an
empty `installed` field and the menu says its version metadata is unavailable.
Vibe has a public upstream source, so a known installed version starts in the
unchecked state and can report a newer version after the explicit check.
`present` and `launchable` are separate facts: pi is installed as `pi` but
launched through `pi-acp`. The probe is cached during ordinary menu use and
replaced by the explicit harness-update check, so an installed or upgraded CLI
appears without restarting scimux.
For Muse, `launchable` is true when the `muse` executable is found on `PATH`.
This inventory flag does not grant permission to create a chat: creation
separately enforces approval-judge consent and model-catalog checks.

### `GET /api/harnesses/latest`

The explicit refresh response is
`{latest:{<agent>:{version, source}}, harnesses:[…], agents:{…}}`. `latest` is
what each harness publishes upstream; `harnesses` is the newly reprobed local
inventory from `GET /api/harnesses`; and `agents` is the newly reprobed model
catalog from `GET /api/agents`. For Vibe that ordinary reprobe is `PATH`
presence plus adjacent package metadata: empty `models`, no `efforts`, a
version only where metadata identifies the installed distribution, and
`vibe-acp` stays unstarted.
The upstream Vibe answer comes from the public `mistral-vibe` package version
endpoint (`https://pypi.org/pypi/mistral-vibe/json`, `info.version`). It is
checked on the same explicit action across supported operating systems. The
menu shows the version result without naming the package registry. A failed
or malformed response omits Vibe from `latest`.
The cache replacements are atomic, so launches after the response use the
same catalog the browser displays.

### `POST /api/harnesses/latest`

This route accepts JSON `{"inspect_copilot": true}` as a one-shot request to
read Copilot's advertised model menu from one temporary ACP session, under the
same rules as `inspect_vibe` below; it is offered only when `copilot` is
installed and launchable, and it is not stored consent.

This route accepts JSON `{"inspect_vibe": true}` as a one-shot
request to run Vibe's bounded catalog probe and return the models and
per-model thinking levels it found. The browser checkbox that sends the field
is consumed when the action starts and is cleared when the menu closes; it is
offered only when Vibe is installed and launchable. It is not stored consent.
Inspection reads only the initial session menus and does not change Vibe's
saved default. A failed or empty inspection leaves the empty default offer.
The `inspect_vibe` field name is case-sensitive; malformed values return
`400`, and a body over 1 MiB returns `413` before refresh or inspection.

The full refresh is reached only on an explicit tap — the server never polls
the registries or repeatedly reruns every CLI list command. Local probes, the
scimux release check made by the browser, and the upstream requests run
concurrently. Overlapping refresh requests (for example, from two tabs) reuse
the completed in-flight local inventory and model probes instead of running
the CLI commands again. An accepted Vibe inspection runs after that shared
refresh, as its own bounded probe. There is no single upstream lane: five sources are npm
packages (codex, pi, opencode, dsh and Copilot's `@github/copilot` `latest` tag), grok is a plain-text channel file,
Vibe is a package version response, and Claude's depends on whether
it was installed natively (compared against the installer's own `stable`, not
the npm dist-tag, which it can never receive). Muse has a separate stable
channel metadata source. Its response format is not yet supported, so Muse
is currently omitted from `latest` even when the channel is reachable.
Sources are read concurrently and independently, and an agent whose source
failed is simply absent from the
map — "unknown" is what happened, and it is not the same claim as up to date.
An upstream failure does not discard successful local inventory or model
refreshes. Claude's model refresh remains zero-token and, on this explicit
path, bypasses its ordinary version/age cache so a same-version server-side
model release can appear; Muse uses initialization/model listing without
creating a session or turn. No model or usage probe requiring paid work is
introduced by this route.

## Remote pairing

This experimental namespace is present only in a run where remote access was
unlocked and explicitly activated; otherwise every `/api/remote/*` request is
404. Loopback pairing for an active remote run. Confirm is a separate explicit
call; mint never completes a pairing. CSRF is required on unsafe methods;
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
needs no CSRF header. It is always reachable during an active remote run —
this is application content, not pairing state — and absent with the rest of
the namespace when the experiment is inactive.

## Team

The muxer always serves these routes; the web child and session workers never
do. Serving them creates no file and contacts no team server until a server is
joined.

An open browser refreshes the Sharing controls after a server is joined and
retries temporary unavailability without a reload or a chat switch.

The token a team server returns is a credential. It is stored in
`team.json` and never leaves the server process: no response, log line, or
error text contains it.

Bodies are JSON except for the People file staging route described below. An
error is `{"error":"<code>"}`. The routes sit
behind the same request boundary as `/api/settings`.

### Agent ownership boundary

Team sharing never grants access to the sharer's agent controls. Recipients
MUST NEVER send directly to that agent: `read` allows viewing, and `write`
allows viewing and posting human comments only. A team token, share id or
subscription id grants no authority to call the owner's node send, key,
approval or other controller routes. This is a request-routing boundary,
independent of which controls the browser displays.

Human comments MUST NOT be forwarded to the agent automatically. In the
unified shared-chat UI, **Use as prompt** and the Agent destination are
owner-only. The owner selects Use as prompt to prepare an editable draft in
their own chat and select Agent, then explicitly sends it to their own
agent. Preparing the draft never sends it. Preserve author attribution and
applicable AI provenance. Auto-approval, retries and reconnects cannot
replace the owner's explicit selection and send.

The unified UI marks the source bubble as soon as Use as prompt prepares a
nonblank draft, using the existing bookmark marker presentation with a
distinct prompt icon and label. Edits keep the source association; completely
erasing the draft cancels the pending association. Preparing a draft must
not be presented as agent delivery. After confirmed delivery, the source
marker persists and links to the resulting prompt, including edited prompts.
An awaiting marker is distinct from both prepared and confirmed. An HTTP
acknowledgment, queue state, busy state or matching text alone does not prove
delivery: confirmation requires assistant evidence at the exact queued address
before a later user turn or error. Claude additionally requires a complete
verified native frontier and a unique later user record in the same binding.
Ambiguous or failed outcomes are never presented as sent. The browser keeps
at most 32 unresolved prompt flights per node and at most 500 confirmed
forward links; the latter navigate by exact address rather than their preview.
Retained comments can be prepared after sharing ends or while offline only
while the owner's local node is still open.

### List

`GET /api/team/servers` answers `200` and
`{"servers":[{"id","name","url","seat_name","joined_at"}]}`.
An empty list is `{"servers":[]}`. The token is not a field. A server file
that cannot be read, including a damaged one, is `500` `store_unavailable`.

### Join

`POST /api/team/servers` takes a body `{"link","name"}`, at most 4 KiB,
with no unknown fields. `name` is
1 to 64 bytes of UTF-8 without control characters and without leading or
trailing space. `link` is `BASE/join#CODE`.

Success is `201` with one server object in the same shape as the list.

Checks run in this order:

| Status | Code | When |
| --- | --- | --- |
| 400 | `bad_request` | The body or the name is not usable, or the team server rejected the registration as `bad_request` |
| 400 | `invalid_link` | The link is not a join link |
| 500 | `identity_unavailable` | The installation ID cannot be read |
| 500 | `store_unavailable` | The server file cannot be read |
| 502 | `unreachable` | The team server could not be contacted |
| 502 | `bad_server` | The team server did not speak protocol 1, or registration named a different server than hello |
| 409 | `already_joined` | This seat already has that server. The join code is not sent |
| 403 | `join_refused` | The team server refused the join code |
| 409 | `already_registered` | The installation is already registered on that server |
| 507 | `server_full` | The team server is at its seat limit |
| 500 | `joined_but_not_saved` | The team server accepted the seat and this computer could not store the result. The join code is spent |

### Status

`GET /api/team/servers/{id}/status` answers `200` and
`{"status":"<state>"}`. The check writes nothing. It is bounded to 8
seconds. The token is sent only after Hello on the stored URL names the
stored server.

| State | When |
| --- | --- |
| `connected` | Hello named the stored server and the token is still accepted |
| `refused` | Hello named the stored server and the token was refused |
| `offline` | Hello or the token check could not reach the server, or the time bound passed |
| `changed` | Hello named another server, or the answer was not protocol 1, or the token check failed for another reason |

An id that is not in the server file is `404` `not_found`. A server file
that cannot be read, including a damaged one, is `500` `store_unavailable`.

### Change this seat's name

`PUT /api/team/servers/{id}/seat` takes `{"name":"..."}`, at most 1 KiB,
with no unknown fields, and requires CSRF. The name follows the join name
rules. The seat confirms the server identity before sending its token and the
new name. Success is `200` with the server object from the list, including the
updated `seat_name`.

Errors are `400 bad_request` for a bad body or name, `404 not_found` when the
server is not joined, `502 unreachable` when it cannot be reached, `502 refused`
when the token is refused, `422 name_refused` when the server rejects the name,
`502 bad_server` for an identity or protocol error, and `500 store_unavailable`
when local state cannot be saved. If the server accepts the change but saving
fails, the names differ until the next successful change.

### Leave

`POST /api/team/servers/{id}/leave` takes no body. Success is `204` when
the seat left the server and the entry was removed. A token the server
refuses counts as left: the seat is already gone there, the entry is
removed, and the answer is `204`. An entry another request removed in the
meantime is still `204`. The token is sent only after Hello on the stored
URL names the stored server.

| Status | Code | When |
| --- | --- | --- |
| 404 | `not_found` | The id is not in the server file |
| 500 | `store_unavailable` | The server file cannot be read |
| 502 | `unreachable` | Hello or the leave call could not reach the server |
| 502 | `bad_server` | Hello named another server, or Hello or the leave call failed for another reason |
| 500 | `left_but_not_removed` | The seat left and the entry could not be removed |

On `unreachable` and `bad_server` the entry stays.

### Forget

`DELETE /api/team/servers/{id}` answers `204` when the server was
forgotten, and `404` `not_found` when it was not in the file. The call
does not contact the team server. The seat stays registered there until the operator revokes it. Use this when leaving is not possible. A damaged server file is
`500` `store_unavailable` and is left untouched.

### Seats

`GET /api/team/servers/{id}/seats` answers `200` and
`{"self":"<installation id>","seats":[{"id","name"}]}` in the order the
team server sent. An empty roster is `"seats":[]`. The token is sent only
after Hello on the stored URL names the stored server. The call does not
write a file.

| Status | Code | When |
| --- | --- | --- |
| 404 | `not_found` | The id is not in the server file |
| 500 | `identity_unavailable` | The installation ID cannot be read |
| 500 | `store_unavailable` | The server file cannot be read |
| 502 | `unreachable` | Hello or the roster call could not reach the server |
| 502 | `bad_server` | Hello named another server, or the answer was not the roster |
| 502 | `refused` | The team server answered 401 |
| 422 | `unknown_member` | The team server rejected a member |
| 507 | `server_full` | The team server is at its quota |

### Shares

`GET /api/team/shares` answers `200` and `{"shares":[<view>]}` ordered by
node, then chat id. An empty list is `{"shares":[]}`. The file is not
created when there are no shares.
`GET /api/team/shares/{node}` answers one view, or `404` `not_found`.
Neither call contacts a team server.

```json
{"uid":"...","node":"...","server":"...","generation":"...","scope":"from_now",
 "state":"active","delivery":"ok","members":[{"seat":"...","access":"read"}],
 "shared_at":"2026-11-14T09:30:00Z"}
```

`members` is `[]` when no seat was named. `delivery` is what the outbox
last saw for that server in this process.

| Delivery | When |
| --- | --- |
| `ok` | The last copy succeeded, or no copy has been attempted |
| `offline` | The team server could not be reached |
| `refused` | The team server answered 401 |
| `changed` | Hello named another server, or the answer was not protocol 1 |

Before the first attempt it is `ok`. A word outside this list is reported
as `ok`.

`PUT /api/team/shares/{node}` takes
`{"server","scope","members":[{"seat","access"}]}`, optionally with
`"membership_only":{"uid","generation","baseline":["seat",...]}`, at most 64 KiB, with
no unknown fields. `scope` is `from_now` or `all`. `access` is `read` or
`write`. `seat` is 32 lowercase hexadecimal characters, not this
installation's id, and not repeated. At most 200 members. `members` may
be `[]` and may not be null or absent.

Here `write` means **Can comment**, never permission to prompt or control
the owner's agent.

The sharing UI chooses members without a permission dropdown. On an existing
share it sends `membership_only` with the viewed chat UID, generation, and
the previously selected visible seats. Under the shared mutation lock the
server applies additions and removals to the current membership: retained
seats keep their current access, seats newly selected since the sheet opened
get `write`, and members added concurrently are retained. A baseline member
removed concurrently stays absent unless explicitly re-invited from a fresh
sheet; an unrelated save cannot restore that member. A changed or missing share identity
returns `409` `share_changed`. The baseline must contain at most 200 distinct,
valid seat identifiers. Requests without `membership_only` retain the explicit
role-changing API behavior. Both access levels remain enforced.

The node must still be in `nodes.jsonl`, and its session log must carry
one chat id. Otherwise `404` `not_found` or `409` `no_history`.

No share for that chat: Hello, create the channel, set the members, then
save state `active`. Success is `201` and the view. `from_now` starts at
the current record count. `all` starts at 0. A channel that already
exists is deleted and created once more. If it still exists, the answer
is `502` `bad_server` and nothing is stored. A failure after the channel
exists deletes it again, best effort, and stores nothing.

A share for that chat: `server` and `scope` must equal the stored ones,
otherwise `409` `share_fixed`. State `active` replaces the members for an
ordinary request or applies the membership-only merge above, then answers
`200` and the view. The cursor, title, generation, and sharing
time stay as they were. Any other state is `409` `not_active`.

| Status | Code | When |
| --- | --- | --- |
| 400 | `bad_request` | The body, the scope, or a member is not usable |
| 404 | `not_found` | The node or the server is not known |
| 409 | `no_history` | The node has no single chat id in its session log |
| 409 | `share_fixed` | The server or the scope differs from the stored share |
| 409 | `not_active` | The share is stopping, lost, or full |
| 409 | `lane_share` | The chat is owned by a lane share. Change it through the lane route |
| 422 | `unknown_member` | The team server rejected a member |
| 500 | `identity_unavailable` | The installation ID cannot be read |
| 500 | `store_unavailable` | A file cannot be read or written |
| 502 | `unreachable` | Hello or a channel call could not reach the server |
| 502 | `bad_server` | Hello named another server, or the answer was not protocol 1 |
| 502 | `refused` | The team server answered 401 |
| 507 | `server_full` | The team server is at its quota |
| 507 | `too_many` | This seat already has 1000 shares |

A chat owned by a lane share answers `409` `lane_share` to `PUT` and
`DELETE`. `GET` still returns the view, with `lane` set to that lane share's
id and `members` taken from the lane.

`DELETE /api/team/shares/{node}` stops sharing. The share is saved as
`stopping` before any call, and from then on the outbox sends no content
for it. Hello and delete follow. Success, a missing channel, and a
refused token all count as stopped: the share is removed and the answer
is `204` with an empty body. When Hello cannot reach the server, or Hello
names another server, the answer is `202` and the view with state
`stopping`. The outbox finishes the job. A server that is no longer in
the server file has no URL to call, so the share is removed locally and
the answer is `204`.

| Status | Code | When |
| --- | --- | --- |
| 404 | `not_found` | This node has no share |
| 500 | `identity_unavailable` | The installation ID cannot be read. The share stays as it was |
| 500 | `store_unavailable` | A file cannot be read or written. A failed save leaves the previous state |
| 502 | `bad_server` | Hello or the delete call failed for a reason other than being unreachable or naming another server. The share stays `stopping` |
| 202 | `stopping` | The server could not be reached, or Hello named another server |

### Lane shares

`GET /api/team/lane-shares` answers `200` and `{"lane_shares":[<view>]}`.
An empty list is `{"lane_shares":[]}`. The file is not created when there
are no lane shares.
`GET /api/team/lane-shares/{lane}` answers one view, or `404` `not_found`.
`{lane}` is the local lane id. Neither call contacts a team server.

```json
{"lane":"north","id":"...","server":"...","scope":"from_now","name":"North",
 "color":"#1a2b3c","members":[{"seat":"...","access":"write"}],"state":"active",
 "delivery":"ok","chats":0,"excluded":1,"shared_at":"2026-11-12T09:30:00Z"}
```

`chats` is how many chat shares this lane owns. `excluded` is how many
chats were archived when the lane was shared. `delivery` is the same word
as on a chat share.

`PUT /api/team/lane-shares/{lane}` takes the chat share body, including
`membership_only` with the same rules, plus `name`, `color`, and
`excluded`. `name` is 1 to 80 bytes of UTF-8 without control characters.
`color` is `#` and six lowercase hex digits. `excluded` is the local ids
of chats archived at that moment. The PUT does not create chat shares.

No lane share yet: Hello, create the index channel titled `Shared lane · `
plus the name, set its members, and save `rev` 1 and `index_rev` 1.
Success is `201` and the view. `from_now` records the record count of
every lane chat that already has a log. `all` records none.

A lane share already: `server` and `scope` must equal the stored ones,
otherwise `409` `share_fixed`. State `active` changes the name, the color,
and the members, and increases `rev` when the members change. `excluded`
stays as stored at create. Any other state is `409` `not_active`.

`DELETE /api/team/lane-shares/{lane}` marks the lane share and each chat
share it owns stopping, kicks the outbox, and answers `202` and the view
with state `stopping`. An unknown lane is `404` `not_found`. The outbox
deletes the index channel and then the lane share. A server that is no
longer in the server file is removed locally, with its chat shares, and
no call is made. The answer is never `204`. DELETE does not call the team
server, so it does not answer 502.

| Status | Code | When |
| --- | --- | --- |
| 400 | `bad_request` | The body, the name, the color, the scope, or a member is not usable |
| 404 | `not_found` | The lane or the server is not known |
| 409 | `share_fixed` | The server or the scope differs from the stored lane share |
| 409 | `share_changed` | `membership_only` names another index channel or generation, or the lane is not shared |
| 409 | `not_active` | The lane share is stopping, lost, or full |
| 422 | `unknown_member` | The team server rejected a member |
| 500 | `identity_unavailable` | The installation ID cannot be read |
| 500 | `store_unavailable` | A file cannot be read or written |
| 502 | `unreachable` | Hello or a channel call could not reach the server |
| 502 | `bad_server` | Hello named another server, or the answer was not protocol 1 |
| 502 | `refused` | The team server answered 401 |
| 507 | `server_full` | The team server is at its quota |
| 507 | `too_many` | This seat already has 64 lane shares |

### Subscriptions

`GET /api/team/subscriptions` lists chats shared with this seat. It answers
`200` with `{"subscriptions":[<view>],"servers":[{"id","delivery"}],"truncated":false}`.
A view contains `server`, `server_name`, `owner`, `owner_name`, `chat`,
`generation`, `title`, `access`, `state`, `mirror_last`, `seen_mirror`,
`comments_last`, `seen_comments`, `unread` and `first_seen`. Views are ordered by server, owner, chat and first
sighting. Access is `read`, `write` or `owner`; state is `live` or `ended`.
Unread means the subscription is live and comments are unseen, or, for a
guest, mirror entries are unseen. The owner's own mirror entries do not count.
`truncated` says the 4096-entry
limit prevented a new subscription from being stored. Delivery is `ok`,
`offline`, `refused` or `changed` as in the share view, and starts at `ok`.
The browser treats this sequence-based `unread` as a candidate: for unseen
comments it reads the retained comment pages and classifies their authenticated
`mine` fields. A proven comment by another seat is unread; only proven local
comments do not create unread. Missing or incomplete pages leave the browser's
status unknown. This inspection does not advance either read position, and a
later local comment cannot mark an earlier incoming comment read. An unseen
recipient mirror remains unread independently of comments.
An owned view may include `local_node` and `local_frontier` when its retained owner frontier still
matches the UID in that local node’s session log. Owned views include `local_order`,
the retained subscription order used when multiple generations share an anchor.
Recipient views omit all three fields.
Channels whose owner, chat or generation is not fixed-length lowercase hex
are skipped.

When the local People file capability is advertised, the response adds
`people_attachments` with `version:1`, `max_file_bytes:4194304`,
`max_files:16`, and `max_total_bytes:16777216`. Server entries report
`people_files` as `supported`, `unsupported`, or `unknown`; supported means
the configured relay's identity and generation-safe blob capability were
verified. A subscription can include a 32-character lowercase hexadecimal
`people_copy_instance`. It identifies this installation's retained copy,
survives reload and share retirement, and changes when that local copy is
removed and recreated. It is a concurrency precondition, not a credential.
If it is absent or unavailable, clients must not perform file operations for
that copy. Legacy text-only requests do not require it.

The list refreshes each joined server at most once per ten seconds, after
Hello confirms its identity. Missing or revoked shares become ended; a subscription becomes live again
if the same generation is listed again, retaining its read position and copy.
A new subscription inherits a known owner name from the same server; otherwise
a new unnamed owner prompts a roster request. Other roster refreshes are at
least ten minutes apart, including after a failed request.
A damaged or inaccessible server or subscription file is `500`
`store_unavailable`.

`GET /api/team/subscriptions/{server}/{owner}/{chat}/{generation}?after=N`
fetches up to eight pages for a live subscription on a joined server, appends
valid entries to its local copy, then answers `200` with
`{"view":<view with delivery>,"entries":[...],"last":N,"next":N,"more":false}`.
`after` defaults to zero. The answer includes at most 256 entries and 4 MiB
of text; `last` is the highest sequence in the copy. `next` is the last
sequence dealt with, including omitted entries; pass it as `after` to continue.
When no entry was dealt with, `next` is `after`, capped at `last`. `more`
is true when `next` is below `last` or the fetch stopped at its page limit. Known start, user,
assistant and source bodies are projected with `seq` and `at`; unknown bodies
remain in the copy and are omitted. On owner subscriptions only, entries with a valid mirror record key include `local_segment` and `local_record`, the local session-log address used to place comments. Recipient reads omit these fields. A user entry may carry boolean `prov` to identify forwarded AI text and string `omitted` when the message is too large to share. Ended subscriptions and unavailable
servers remain readable from the copy.

Errors are `404` `not_found` for an invalid identity or unknown subscription,
`400` `bad_request` for an invalid `after`, `500` `store_unavailable` for
unavailable state, `500` `copy_damaged` for a damaged copy, and `507`
`storage_full` when the storage budget refuses an append and the copy has no
entry after `after`.

`GET /api/team/subscriptions/{server}/{owner}/{chat}/{generation}/comments?after=N`
fetches and reads the comments copy under the same paging, retention and error
rules as the mirror. It answers `{"entries":[{"seq","at","author","mine",
"text","anchor"}],"names":{},"last":N,"next":N,"more":false}`. Names are
the last-known names of retained comment authors on that server, overlaid by
its cached roster and then this seat's own name when joined. The roster is read
again at most once per 60 seconds while comments are read, so a known author's
new name shows within about a minute. Last-known names survive a restart or a
seat leaving; they are removed when the last subscription to that server is
removed. Local browser names still take precedence in the page. A failure to
read or write last-known names does not fail the comments read. A muxer restart
while an author is still unnamed forgets that author until a read from the start
of the copy, for example opening the view, shows the comment again. Unknown
comment shapes remain in the copy but are skipped in the answer.
An owner can receive an anchor-zero comment before the initial native mirror
arrives. If its saved frontier predates the current seam and there are no
earlier agent turns, the browser displays it as earlier discussion in the
normal timeline; it does not add the comment to the agent's session log.

`POST /api/team/subscriptions/{server}/{owner}/{chat}/{generation}/comments`
takes `{"text":"...","anchor":N,"key":"<16 lowercase hex>"}`, at most
64 KiB, without unknown fields, and requires CSRF. Text must be valid UTF-8,
nonblank, and at most 16,384 bytes. The anchor cannot exceed the last copied
mirror number. A successful or duplicate append answers `200 {"seq":N}`;
messages are not queued or copied by the post. Errors are `400 bad_request`,
`404 not_found`, `403 read_only`, `409 not_active`, `500 store_unavailable`,
`500 copy_damaged`, `502 refused`, `502 unreachable`, `502 bad_server`,
`507 server_full`, and `413 too_large`.

A successful comment append has no agent side effects: it cannot send a
prompt, answer a tool request or select the owner's Agent destination.

### Local People inventory

`GET /api/team/subscriptions/{server}/{owner}/{chat}/{generation}/local?copy_instance=ID`
reads local review metadata without contacting the relay or an agent. It requires
People files to be enabled, the exact current persisted copy identity and the
local installation identity. It does not require current relay configuration,
membership, writable access, or a live native owner. It does not authorize a
send, upload, or deletion. An unavailable or damaged local identity or record
fails closed with `409`; malformed or duplicate query parameters return `400`.

The response has `entries` and, when more records remain, `next_cursor`. Each
page examines at most eight records. Supply the returned cursor as `cursor` on
the next request. Cursors consist of `p` or `s` followed by a 16-character
lowercase hexadecimal record ID; ordering is pending then stage by ID. Pages
are not a snapshot: changes before the cursor require restarting enumeration.
An empty page can have a next cursor when terminal records were examined.
Pending entries contain `kind`, `id`, `revision`, optional `text` and
`stage_ids`. Stage entries contain `kind`, `id`, `revision`, `name`, `mime`,
`size` and `hash`. Revisions hash the exact immutable record and are local
concurrency preconditions. No file bytes, paths, credentials, or invented
creation times are returned. Completed and retired pending keys are terminal
and are not returned as active pending. Corrupt or ambiguous records report
unavailability rather than being silently omitted. Inventory never creates a
copy identity or automatically adopts, uploads, or sends anything.

`GET /api/team/subscriptions/{server}/{owner}/{chat}/{generation}/pending/{key}?copy_instance=ID`
reads the state of one local publication key under the same identity and local
authority rules. `key` is 16 lowercase hexadecimal characters. The response
state is `pending`, `retired`, `completed`, `absent_retired`, or `missing`.
Pending, full retirement and completion include the immutable `revision`,
`stage_ids`, and, when nonempty, saved `text`. A completed state includes the
confirmed `seq` only after the retained comment is validated. If its pending
record still exists, `pending_pinned` is true and retained file integrity is
also checked. Otherwise removed local file bytes do not invalidate a matching
receipt and comment. Contradictory or damaged records fail closed. The read
does not remove a pending record or release its pins; an interrupted completion
still needs explicit local cleanup. Missing or typed absent retirement has no
revision or sequence. The response does not include the raw publication body,
file bytes, or configuration identifiers.

### Retire an absent People publication

`POST /api/team/subscriptions/{server}/{owner}/{chat}/{generation}/pending/{key}/retire-absent?copy_instance=ID` has no body. It durably retires a file-publication key only when there is no local pending record or completion receipt. It requires the current persisted copy and installation identities, but no current relay membership or native owner. The operation contacts neither a relay nor an agent. Success returns `{"state":"locally_discarded"}`; repeating the same durable retirement returns the same result. An existing pending, completed, or previously retired publication requires exact review and returns `409 review_required`. Existing revision-checked discard remains separate.

A new retirement record consumes the ordinary local storage budget, including the atomic-write peak; a full budget can return `507 storage_full`. Terminal retirement records remain until the local copy is removed. On any refusal or uncertain response, retain the browser recovery state and retry explicitly. Retirement prevents later local admission of the key; a comment already dispatched remotely may still appear. It does not retract a remote comment or remove staged file bytes.

### People files

When the local version 1 capability and the relay's `supported` state are
available, a writable participant can stage a file with
`POST /api/team/subscriptions/{server}/{owner}/{chat}/{generation}/attachments`.
The multipart body contains one `files` part, and the query includes the
current `copy_instance` and optionally a 16-character lowercase hexadecimal
`stage_id` for an exact retry. Owners also supply their native `chat_instance`
query precondition. A successful response includes an attachment descriptor:
`id`, `sha256`, `size`, `name`, advisory `mime`, and the immutable stage
`revision` used for local removal. Files must contain at
least one byte and be at most 4 MiB; a message can contain at most 16 files
and 16 MiB total. Names are bounded, sanitized UTF-8 leaf names. New uploads
reject bidi controls U+061C, U+200E–U+200F, U+202A–U+202E, and U+2066–U+2069.
A retained name is shown and downloaded with those characters removed, or as
`file` if that would be empty, `.`, or `..`. These limits
do not alter ordinary Agent uploads.

Stage metadata uses GET on `.../attachments/stage/{id}` with the current
copy precondition and, for an owner, native precondition; it also returns the
stage revision. Local removal uses
DELETE on that path with `copy_instance` and the exact `revision` returned by
the original upload or an explicitly reviewed local inventory entry. It requires the current local installation and copy identity,
but not current relay configuration, membership, write access or native owner.
A pinned, changed or damaged stage is refused with `409`. Success is `204`;
a retry also returns `204` when the stage is already absent after repairing its
directory barrier. A replacement with the same ID is preserved if its revision
differs. Removal does not retract an upload or delete independent retained files.
File-bearing comment POSTs include an ordered
`attachments` array of staged IDs and the current `copy_instance` query;
the text may be empty when files are present. The publication body and key
are frozen for retry. An uncertain outcome must be reviewed and retried with
that same body and key; a conflicting existing body is not success. A
successful retained receipt can be reconciled without another publication.
A storage-budget refusal while saving the kept copy confirms the owner's
already appended comment and leaves the file unkept. A different error saving
that copy returns `507` `storage_full` and keeps the pending pin.

The shared log entry remains a version-1 comment. Its ordinary `text` is the
original text followed by a newline and `[Attachments unavailable in this
version of scimux.]`; for a file-only message it is the notice alone. The
extension carries the exact original `content_text`,
`people_attachments_version:1`, and an ordered `people_attachments` manifest.
Older readers therefore retain the original text and a visible notice rather
than silently displaying an empty message. A new reader uses the extension only
after validating the exact fallback and every descriptor; unknown or malformed
extensions retain the fallback and report unsupported or corrupt attachments.
An old relay without the generation capability is not used for People blob
transfers. Relay blobs remain opaque and unreferenced uploads remain stored
within the relay's channel budget until that channel is removed; an interrupted
upload can therefore leave an unreferenced blob. Removing a local copy does not
remove independent captured reference media or automatically republish any
browser draft.

Associated downloads use GET `.../attachments/{seq}/{id}` with the current
copy precondition. `preview=1` requests a verified bounded PNG or JPEG preview;
unsupported previews do not prevent an available file download. Downloads
use an attachment disposition and `nosniff`. When verified bytes are served
without a local kept copy, the response includes `X-Scimux-People-Kept: false`.
A kept response adds no such header. The full subscription identity,
comment sequence, and logical file ID authorize the association; a bare hash
does not. Available verified local bytes remain readable after a share ends
or its relay is unavailable. Missing, corrupt, unsupported, and unavailable
files are distinct from empty files. Removing the local copy removes its
managed files; independent captured reference media has its own retention.

`copy_instance_required`, `copy_changed`, and `copy_identity_unavailable`
report a missing, stale, or unavailable local precondition. These checks do
not replace permission, server, generation, or native owner checks. Relay
quota and size refusals use `relay_quota` and `file_too_large`; an uncertain
upload or publication is not reported as confirmed delivery. File transfer,
preview, download, and comment publication never dispatch an agent.
When a comment response is uncertain, the browser keeps its text, anchor and
idempotency key for an explicit retry. Retrying that same pending comment uses
the same key, so a previously accepted append is not duplicated. People
discussion is text-only when the required file capabilities are unavailable.
Its draft is separate from the owner's Agent draft;
Agent attachments keep the existing staging behavior across destination
changes and in-flight work. For a shared owner chat, Agent is a one-send arm:
the composer returns to People when an Agent attempt is dispatched, while
validation failures keep the selected destination and input for correction.
An uncertain attempt remains unconfirmed; returning to People does not prove
delivery. Retired drafts are discarded; they are never inserted into a
replacement instance.

`PUT /api/team/subscriptions/{server}/{owner}/{chat}/{generation}/seen`
takes `{"mirror":N}`, `{"comments":N}`, or both, with no unknown fields
and at most 1 KiB, and requires the CSRF header. It records the greater of
each old seen position and `N`, capped at that log's highest copied sequence,
without contacting a server. Success
is `204`; errors are `404` `not_found`, `400` `bad_request`, and `500`
`store_unavailable`.
An optional `copy_instance` query binds the read-position update to the
selected local copy; a stale or unavailable identity returns `409` and does
not advance the replacement copy. Older text-only clients can omit it.

`DELETE /api/team/subscriptions/{server}/{owner}/{chat}/{generation}` removes
one subscription and its saved copy. A `copy_instance` query precondition,
when supplied, must match the current local copy before the operation proceeds;
legacy clients may omit it. An already admitted operation is retired when its
copy is removed. For a live subscription on a joined
server, it first confirms that server's identity and leaves the channel;
ended or unjoined subscriptions need no server request. Success is `204`.
Errors are `404` `not_found` for an invalid identity or unknown subscription,
`409` `own_share` for a live owner subscription, `502` `unreachable` for an unreachable server, `502` `bad_server` for another
server failure, and `500` `store_unavailable` for a local storage failure.
A failed local save can leave the entry without its copy, so it can be retried.

The subscription list, read and comment-post routes ask a team server on
behalf of this seat; no background subscription fetch is performed.

Any other method or path under `/api/team` is `404` `not_found`.
