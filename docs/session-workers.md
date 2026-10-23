# Session-worker architecture

The process split exists for user continuity, not source-code modularity. A
release must not tear down a user's active turns merely because the web UI,
muxer coordination, operating-system support, dependency closure, or one CLI
adapter changed.

## Ownership

The single `scimux` artifact runs hidden internal roles:

```text
browser <-> web-child <-> muxer <-> one session-worker per chat <-> agent CLI
                     WebRTC/rendezvous ^             Claude worker <-> tmux pane
```

- The web child owns browser-facing assets, request security, and remote
  WebRTC/rendezvous state. It has no harness handles.
- The muxer owns the public listener, node metadata, notes, and routing. It
  reads session logs but does not own live CLI connections.
- A session worker owns exactly one CLI connection and is the sole writer for
  that chat's `sessions/<node>.jsonl`. Claude uses the same boundary even
  though the process it controls already lives in tmux.
- No information-management daemon and no per-provider manager daemon exist.
  Durable plain files are the narrow Unix interface between workers and muxer.

Updating disconnects workers; it does not stop them. The replacement muxer
inherits the listener and lifetime ownership lock, then reconnects. Existing
workers deliberately keep the adapter implementation from the release that
started them. Newly created chats use the newly installed binary, so a breaking
change in one vendor CLI rolls out without disturbing unrelated or already
running chats.

Only `scimux` is a public command; service managers must not invoke the hidden
roles directly. Self-update activates a ready web child before handing the
listener and muxer lock through exec, and never calls workers' Stop operation.
Full `scimux stop` retires workers and structured subprocesses but preserves
Claude tmux panes. `scimux stop [-data <path>]` addresses only the muxer through
`muxer.json`; `muxer.lock` remains the lifetime single-owner claim for that
data directory.

## Common-denominator protocol

The authenticated HTTP+JSON protocol runs only over a Unix socket in a random
mode-0700 directory. Its v1 operations are not a speculative abstraction:
each is required by at least one current harness path.

| Operation | Current reason it exists |
| --- | --- |
| `Launch` | Claude, ACP, Codex, and Muse create a session. It also recovers a scimux-owned pre-worker Claude pane. |
| `Send` | Every harness accepts a user turn. |
| `Clear` | Every harness implements the existing `/clear` page turn. |
| `ResolveDelivery` | Claude clears an explicitly acknowledged unconfirmed paste. |
| `Interrupt` | Every harness exposes the current Interrupt action; Claude returns physical-key evidence for the canonical audit. |
| `PreparePermission`, `DeliverPermission` | Every structured harness and Claude fence a decision, persist its audit, then deliver exactly once. |
| `State` | The muxer polls liveness, attention, delivery, and bounded permission mechanics for the existing state/chat responses. |
| `Peek` | The existing Inspect surface reads a pane snapshot for Claude or a bounded structured-log tail for other harnesses. It is a snapshot, never a terminal stream. |
| `SetAutoApprove` | Claude's one-turn lease lives beside its hook rendezvous; structured leases remain muxer policy. |
| `RecordStartFailure` | Structured creation makes a failed first-turn delivery durable. |
| `AppendSessionEvent` | Muxer-originated station, attention, decision, and retry-binding asset records still have exactly one physical session-log writer. |
| `Stop` | Node deletion terminates the owned chat; whole-program shutdown retires Claude's wrapper without killing its tmux pane. |

`State` is the bounded description the UI already polls. `Peek` is the existing
fallback/inspect snapshot, not a second history channel. Conversation turns and
usage remain in the shared session log and are intentionally absent from RPC.

## Compatibility and discovery

Protocol major 1 freezes the operation meanings above. A major mismatch fails
readiness. Minor versions and new capabilities are additive: future muxers must
feature-gate an optional operation instead of requiring an old live worker to
gain it. Unknown response fields are ignored by normal Go JSON decoding.

`asset-backing-v1` advertises that `AppendSessionEvent` preserves an asset
record's `backingId` content reference. A replacement muxer reconnecting to an
older worker must append a self-contained asset record instead; an older JSON
decoder legitimately ignores the unknown field before its sole-writer append.

`claude-interaction-v1` is an optional Hello capability, not a new protocol
major and not a mandatory `Harness` method. Only the Claude harness implements
it. A worker that omits the capability, or a harness that does not implement
the optional owner, reports the interaction unsupported. New muxers reconnect
to old workers and do not enable the feature by rewriting an active bundle.
The bundle's minted capability, not the current process environment, decides
an already launched chat. Optional state may carry one current interaction.
The worker remains the only session-log writer. The owning worker retires
stale interaction state on startup and on each poll after the helper deadline,
and a replacement may do the same. Raw payloads are removed. An emitted
cancellation keeps a minimal turn fence so settlement can still run; that
fence is not replayed. The replay fence and the append-only audit remain.
The worker's submitted caller generation comes from the node's binding when
that generation is at least 1, and otherwise from `perm/generation.json`.
That file is the helper process's authority for generation, node, and session.
The owning worker is the only writer. Muxer projection and recovery record the
snapshot they observed and must not publish it over the worker's fence. A
request that was bound to a generation fails closed when the fence is missing
or unreadable. Rebinding the session at the same generation revokes answers
stamped for the previous session. When the binding record is already durable
and only the helper fence failed, the worker republishes the fence. The same
SessionStart then finishes retirement of the previous transcript. Ordinary
sending stays blocked until that fence is published.
Native key delivery checks structured ownership again before the keys are
sent. It must not replay an uncertain decision into a new helper or turn.

Workers do not broadcast. Broadcast discovery would add races, spoofing, and
platform-specific behavior without identifying ownership. Each worker instead
holds a kernel lock for its node lifetime and atomically publishes an
owner-readable locator under `~/.scimux/control/workers/`. The locator contains
only identity, PID, executable path, socket path, and a random capability; the
lock, not the JSON file, decides ownership. `ls` remains useful for diagnosis.
The muxer trusts a locator only after the socket's authenticated Hello agrees
with the durable node identity.

The per-node `.lock` inode deliberately remains after the worker exits. It is
zero bytes and is reused if that node title is launched again. Unlinking after
taking `flock` is not safe: another claimant may already have opened the old
inode and be waiting on it, then acquire that unlinked inode while a third
claimant locks the newly created path. Stable lock inodes trade tiny bounded
directory entries for the absence of split ownership.

The executable used for workers is copied to a content-addressed, owner-only
path before launch. Replacing the user-facing `scimux` pathname cannot change
what an old muxer or long-lived worker will exec halfway through an update.
Startup hashes the running image and reuses an existing pin without rewriting
it. After reconciliation it retains the current pin, every pin named by a
worker locator, and every pin named by the `exec` of a live (unarchived)
Claude hook bundle, and removes other content-addressed generations. The
bundle rule matters after a full stop: the preserved pane's hook settings keep
exec'ing the retired worker's pin even though a newer worker recovered it, so
deleting that pin would silently disable every hook, auto-approve included. An
unreadable bundle capability record fails the sweep closed. A live locator
from an older worker that cannot name its executable disables that sweep; disk
cleanup fails safe until that worker exits.

Web-child startup configuration is a bounded JSON document on an inherited
pipe, so the muxer capability and CSRF token never enter a new child's argv or
environment. A new web child still reads the former environment contract when
the pipe marker is absent: that one-way compatibility is what lets an already
running older muxer prepare and activate the newly downloaded web generation.

Automatic cleanup is fail-safe. A live Claude worker's reported hook identity
protects its hook bundle even during the narrow launch/publication window; an
unreadable or unauthenticated locator disables cleanup rather than risking a
running chat. On startup, an authenticated worker interrupted before its node
record was published supplies that record's launch description and is recovered.
If the latest record is instead a delete tombstone, startup finishes the Stop
and never resurrects it. An `EndedAt` node receives the same authenticated
cleanup on startup, covering a crash between persisting `/exit` and reaching
the worker. A muxer crash otherwise merely drops client connections. Claude
hook rotations (including `/clear`) are projected back to the global append-only
registry by the normal poll, so a later clean stop and owned-pane recovery preserve the
worker's newest capability bundle. Explicit deletion is the only normal path
that tells a worker to terminate its owned session.

Historical workers for records marked `adopted:true` are retired during
reconciliation with `Stop(false)`: the worker exits, no new controller is
started, and the external pane remains running. If an authenticated orphan
worker reports that legacy ownership in its launch description, it is retired
instead of being republished. Failure to authenticate or read that ownership
fails closed rather than risking an ordinary stop against an external pane.

Worker-reported liveness, attention, delivery and permission mechanics are
derived state, not another durable registry. The worker reconstructs them from
its provider connection, hook journal, tmux pane and canonical session log; the
muxer reconstructs its projection by polling `State` and does not write the
worker's `perm/generation.json` from that snapshot. For Claude, the worker's
fixed two-second supervision lane produces that snapshot. Browser reads return
the last snapshot and never trigger pane polling or append audit records.
