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
here, including answering approval prompts.

## Reading state

### `GET /api/state`

The polling snapshot the UI runs on. Returns:

```json
{
  "nodes":     [ { …node fields…, "live": "quiet", "attention": "approval",
                   "has_transcript": true, "last_activity": 1752849600000 } ],
  "unadopted": [ "tmux-session-name" ],
  "sys":       { …host load/memory… }
}
```

Each entry is the stored node (id, title, prompt, description, rationale,
lane_id, agent, model, effort, dir, transport, created_at, …) plus the
mechanical view: `live` is `active | quiet | exited | unavailable`,
`attention` (when set) is `approval | question | inspect`, and
`last_activity` is the last pane change in Unix milliseconds. `unadopted`
lists tmux sessions on scimux's socket that no node accounts for —
candidates for `POST /api/adopt`.

### `GET /api/nodes/{id}/chat`

The conversation, rendered from the session-log store (the current segment —
everything after the last `/clear` seam), plus turn mechanics: `turns`,
`source` (`transcript` or pane fallback, with `fallback` set), context-gauge
fields (`ctx_used`, `ctx_window`, `ctx_pct`), `live`, `attention`,
`pending`/`pending_calls` (unresolved tool calls), and `delivery` (state of
the last send).

### `GET /api/nodes/{id}/peek[?mode=visible]`

Plain-text pane snapshot for tmux nodes (`mode=visible` captures only the
visible pane instead of history). For structured-transport nodes (ACP,
codex) there is no pane; peek returns a tail of the raw event log.

### `GET /api/agents`

Detected agent CLIs and their models/efforts, as offered in the new-activity
dialog.

## Managing nodes

### `POST /api/nodes`

Create and start a session. Body is the launch configuration (same fields as
a stored node): `prompt` is required, `agent` selects the CLI, plus
optionally `title`, `model`, `effort`, `dir`, `description`, `lane_id`, and
`parent` + `rationale` for a fork. Returns the created node. Validation
failures (empty prompt, unknown parent, bad agent or dir) are 4xx.

### `PATCH /api/nodes/{id}`

Body: any of `title`, `description`, `lane_id`. Title must stay non-empty.
Lane assignment is write-once: setting it on an unassigned node succeeds,
changing an existing assignment is `409`.

### `DELETE /api/nodes/{id}`

Appends a `delete` tombstone to the store and archives the node's session
log to `sessions/archive/`. History is never destroyed.

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
turn is still in flight. On structured transports `"/clear"` is implemented by
scimux itself: a fresh protocol session on the same node, recorded as a source
seam — same page-turn semantics as Claude's `/clear`.

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
cross-node access; anything else is `404`.

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

The shared UI document (`~/.scimux/ui.json`: groups, archived cards, notes,
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
