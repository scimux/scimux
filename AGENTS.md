# Agent guide for scimux

scimux supervises tmux-wrapped agent chats (Claude Code, Codex) from a local
web page. Read the README for architecture; this file lists only the
invariants you must not break and the workflows you need. Mechanics that the
named code already shows are not repeated — what is written down is the
*intent*, which is what a reader cannot recover from the tree and what stops a
rule being "improved" away. Two subsystem blocks (Claude's hooks, and
attention/input) live in `docs/invariants/` and load on demand; see
"Subsystem invariants" below for which one binds your change.

`remote-by-invite-only.md` is the remote-access design and the home of every
FR-xx/NFR-xx cited from `internal/remote`, the protocol specs and `scimux-rv`.
Markdown is deny-by-default: `.gitignore` ignores `*.md` and allowlists the
tracked set, so a new document is tracked on purpose or not at all
(`git ls-files '*.md'` is the check). Superseded plans live in untracked
`attic/`.

## Commands

```sh
go build -o scimux ./cmd/scimux # single static binary; web/index.html is embedded
go test ./...          # unit + integration (integration needs tmux)
go test -short ./...   # unit only; the offline CI lane (build.yml runs the full suite)
node --test web/test/*.test.js  # browser unit suite; no browser needed
gofmt -w $(find . -name '*.go' -type f) && go vet ./...
go test -run TestCrossBuildTargets ./internal/app  # cross-builds every target CI ships
scripts/vendor-rendezvous.sh /path/to/scimux-rv   # re-vendor the rendezvous vectors
go test ./internal/transcript -run=XXX -fuzz=FuzzParseLine       -fuzztime=30s
go test ./internal/app        -run=XXX -fuzz=FuzzClaudeHookStdin -fuzztime=30s
```

- The release matrix is not written down twice: `TestCrossBuildTargets` parses
  the `GOOS=… GOARCH=…` pairs out of `.forgejo/workflows/{build,release}.yml`,
  so a target added to a workflow is defended from that moment. freebsd/amd64
  is built but deliberately not released — it keeps the static-build invariant
  honest.
- Integration tests create private, randomly named tmux sockets and never
  touch a user's tmux server. They clean up after themselves, and that
  includes the socket *file*: tmux does not unlink it when the server exits,
  so a helper that only kills the server leaves one 0-byte name per test in
  the user's `/tmp/tmux-<uid>` until the next reboot. `SocketPath` is what a
  cleanup deletes, and the two helpers assert the removal rather than
  best-effort it — litter nobody is told about is litter nobody clears. Never
  run a real agent CLI (`claude`, `codex`, `pi`, `opencode`, `grok`) in tests
  — wrapped test commands are `bash --norc` or `cat`.
- The fuzz targets state contracts as properties over all inputs; seed
  corpora run under plain `go test`. `FuzzParseLine` guards defensive parsing
  below; `FuzzClaudeHookStdin` guards that no Claude hook helper writes to
  stdout whatever it is fed and disturbs nothing outside its own bundle
  subdirectory. A crasher written to `testdata/fuzz/` is a bug report, not a
  fixture: quote it, fix the code, do not commit it.

## Layout and dependencies

- `cmd/scimux` is deliberately thin: startup and the linker-stamped version
  only. Keep product behavior out of it.
- `internal/app` owns the private application and its co-located tests; its
  `testdata/` belongs to that package — no repository-wide test directory.
- `internal/*` are private leaf subsystems: `internal/app` may depend on them,
  they must never import it. `web` and `legal` are imported by `internal/app`,
  never the reverse.
- `web/js/app.js` is the sole entry and the deliberate untested composition
  root; its header states what the exception covers (wiring) and what it does
  not (pure logic, extracted into a tested module on sight). Read it before
  answering a coverage finding about that file.
- Keep the repository root for module metadata, docs, licenses and top-level
  directories — no application Go files or tests there.
- `Packet <N>` comments are design-doc phase references, kept so a change
  stays traceable to its phase. Do not mass-rename them as jargon.

## Invariants (deliberate design decisions — do not "improve" them away)

- **Zero build dependencies.** Standard library only — no SQLite, no
  WebSocket library, no JS framework. If a feature seems to need a module,
  stop and discuss. Two maintainer-approved exceptions exist, both scoped by
  directory, neither to be relitigated:
  - `github.com/coder/acp-go-sdk` (Agent Client Protocol), **only inside
    `internal/acp/`** — see `docs/acp-integration-plan.md`.
  - `github.com/pion/webrtc/v4`, **only inside `internal/remote/`**: a browser
    speaks only WebRTC for a peer-to-peer data channel, so the computer end
    must speak the same ICE/DTLS/SCTP stack. That single direct require is the
    exception; its siblings are `// indirect` closure. Scope is mechanically
    enforced by `internal/app/remote_boundary_guard_test.go` — adding a line
    to its `allowedModuleRequires` is a new exception, not an upgrade.
  - **Packaging: one binary, unconditionally.** No build tag, no
    `remote`/`noremote` variant, no second artifact; a split to keep a
    pion-free default was considered and rejected. So pion is in every build
    and cannot be dead-stripped (remote is a runtime switch) — accept that.
    It is pure Go, so `CGO_ENABLED=0` and the FreeBSD static build keep
    working. Approving a dependency widened *what* may be imported, never
    *where from*.
- **The tunnel protocol is owned by scimux-rv, not by this repository.** The
  browser↔computer wire format is specified in rv's
  `docs/protocol/tunnel-v2.md`, and its **browser** implementation lives there
  too (`web/js/codec.js`, `web/js/connection.js`). Deployment asymmetry is the
  reason: rv is deployed once and reaches every browser on the next page load,
  while scimux binaries sit on many computers at many versions, so the side
  that must tolerate the spread holds the protocol. Never reintroduce a
  browser codec or channel transport here — it would arrive over the very
  channel it exists to create. Read the spec for the format; what binds here:
  - The computer half is `internal/remote/codec` plus `web/js/bootstrap.js`,
    which is the *loader*, not the transport. It ships with the binary because
    it knows this repository's module graph, which is what lets us restructure
    that graph with no rv deployment; vendoring it into rv was rejected (rv
    would go stale, and would supply the code that checks this computer's own
    integrity values, inverting FR-40).
  - Three constants are protocol, not local names: `GET /api/remote/bootstrap`,
    `GET /js/bootstrap.js`, and the `bootstrap({channel, manifest,
    createObjectURL, installImportMap, importModule})` signature. Renaming or
    moving any of them is a MAJOR bump that breaks every deployed rv.
  - The loader rewrites every specifier to the imported module's absolute
    `blob:` URL and installs **no** import map (a `blob:` URL has an opaque
    path, so nothing relative or root-absolute resolves against it). Hence
    `installImportMap` stays in the signature as protocol but is left
    uncalled, and **the browser module graph must stay acyclic** — a `blob:`
    graph cannot express a cycle, which is the named `bootstrap-cyclic` abort
    (AT-FR-40-f), never a partial boot.
  - Versioning is semantic and binds both halves: MAJOR must surface as a
    named FR-24 state saying which side is behind
    (`tunnel-version-mismatch`); MINOR is additive and must never escalate to
    the user. Keeping MINOR safe is a local obligation: ignore unknown frame
    types, record tags, rejection classes and trailing bytes instead of
    rejecting them, treat type/tag `0x00` as permanently reserved and always
    malformed, never reuse or retype a tag number, and add no count prefix
    anywhere so a claimed count cannot be lied about. The frozen 8-byte
    `SCMX` preamble never gains a field — capabilities grow in the hello, and
    payload fields are tagged records rather than positional.
- **`internal/remote/codec/testdata/vectors.json` is a published contract, not
  a local fixture.** `vectors_test.go` regenerates it from the production
  encoders and fails on drift; scimux-rv byte-copies it and decodes every
  vector with its browser codec. It is the only oracle either side has, so it
  must stay generated — never hand-edited, pretty-printed, or authored by
  reading the spec. Each vendoring lane has a one-directional script in the
  *receiving* repository (`scripts/vendor-rendezvous.sh` here,
  `scripts/vendor-tunnel.sh` there; see `docs/rendezvous-protocol-sync.md`);
  a single bidirectional "sync everything" entry point is deliberately absent,
  because it would eventually regenerate these vectors on the consuming side.
- **`PairingTTL` is a constant shared with a server this repository does not
  deploy.** `internal/remote.PairingTTL` and rv's `PairingWaitTTL` must hold
  the same value — **120 s** as of 2026-09-05 — and nothing enforces it; a
  disagreement is silent in the direction that matters (expire first here and
  the user is told a live code is dead; expire first there and the computer
  waits on a dead slot). The value is an economics decision (10^8 codes
  against rv's 30 offers/s is ~3600 blind tries per window), so change it in
  both repositories, with the arithmetic, or not at all. FR-11 carries it too.
- **Pairing is offered only where it could complete.** Minting is entirely
  local and asks the rendezvous for nothing, so a computer rv would not admit
  can still mint a perfect code and QR for a meeting that can never happen,
  which the user reads as a timing problem rather than "not enrolled".
  `internal/app.pairingAvailable` is therefore an **allowlist** — "enrolled"
  and "unavailable", nothing else — gating the mint, the session projection
  and the `can_pair` the menu reads. "unavailable" is in it because the mint
  starts the wait loop that clears a transient outage; refusing it would be
  self-sustaining. `""` (just unlinked) and the FR-30 broken states are out.
  The browser must not re-derive this from `hosted`: one copy of the policy,
  on the side that enforces it. `#m_pair` ships hidden and is revealed by the
  status read, leaving `#m_pair_note` to say why — a control that vanishes
  without a word reads as a bug.
- **Snapshot over stream.** Output is read via `tmux capture-pane -p`
  snapshots and the transcript JSONL the CLIs write themselves. Never parse
  the terminal byte stream, never use tmux control mode (`-CC`), no
  WebSockets, no in-browser terminal emulator; polling is the intended
  transport. `internal/acp/` is consistent — it consumes the agent's
  *structured* records, never a byte stream, and the UI still polls (an ACP
  "peek" renders a tail of the session log instead of a pane photo).
- **One session = one window = one pane = one agent process.** No muxing.
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
- **Nothing spends the user's quota without consent.** Exactly one reading
  scimux takes costs money — the Claude usage gauge — and it is off by
  default, gated server-side in `collectClaudeUsage` on `claude_usage_checks`
  in `~/.scimux/settings.json`. That file is the *computer's* settings, not
  the opaque per-browser blob in `ui.json`: a gate the server enforces must be
  a value the server owns. Every read of it degrades toward off. A new
  provider call that costs anything gets the same treatment or does not ship;
  everything scimux learns locally (model catalog, `--version`) must stay
  free.
- **Append-only store.** `~/.scimux/nodes.jsonl` is replayed at startup;
  corrections are new records, never rewrites.
- **One session-log store, one schema.** Every transport writes its per-node
  history to `~/.scimux/sessions/<node-id>.jsonl` as `internal/sessionlog`
  events — plain JSONL, because the corpus must stay grep/sed/awk-able. The
  filename is the node's reusable title slug; identity lives in the file's
  `meta` header record, and deleting a node archives its log so a reissued
  slug can never append onto dead history. tmux nodes reach the store through
  the transcript mirror (`mirror.go`), with `source` seam records marking
  every transcript (re)bind as the dedupe watermark. New transports write the
  same records to the same directory: no per-transport formats or directories.
  - The store is also the **chat read path for every transport**: handleChat
    renders the current segment (everything after the last `source` seam),
    while the tailer serves only mechanics — needs-input, staleness, delivery
    confirmation. Earlier segments are readable on demand, never polled
    (`?history=1`, `sessionlog.ReadHistory`, behind a "show earlier history"
    tap). Keep that split: history in the poll payload would ship the whole
    corpus every second, and it can repeat turns across mechanical seams
    because a rotation re-mirrors from turn zero.
- **/clear = page turn, fork = fresh notebook.** `/clear` starts a fresh chat
  surface under the *same* node: same log file, an appended `source` seam,
  never a new file or truncation; the context gauge is segment-scoped.
  - ACP nodes (pi/opencode/grok) implement it as **deterministic process
    replacement** — kill the subprocess, negotiate a fresh one under the same
    node, because a second `session/new` on one connection is unproven
    upstream while a fresh PID self-evidently carries no context. codex opens
    a new thread on the same PID. In both, the seam is appended only after the
    protocol call succeeded. Claude gets a path-less "detached" seam at retire
    time and the successor bind only from its own SessionStart hook.
  - Fork is the only path that may change launch config: a forked node
    inherits agent/model/effort/dir but never conversation history. Claude's
    native `/fork` and `SessionStart source:"fork"` are unsupported — forking
    is scimux's Fork action only (fresh node/process, ordinary
    `source:"startup"`). Never send `/fork` to the Claude CLI.

### Subsystem invariants that load on demand

Two blocks of this guide live in `docs/invariants/` because they bind only
once you are already inside the code they govern. They are part of the
invariant set, not appendices — each is the record of decisions that look like
bugs from the outside, so read the relevant one **before** you edit, not after
a reviewer asks:

- **`docs/invariants/claude-hooks.md`** — required before touching
  `internal/app/claude_*.go`, the hook helper commands, hook-bundle or
  `capabilities.json` handling, the Claude usage/model probes and their
  consent gate, or anything that launches, binds or retires a Claude pane. It
  owns: SessionStart as the sole binding and ownership proof;
  the paste gate and the two delivery clocks; approvals answered through the
  hook (lease, nonce, prompt_id fence, the two deadlines); auto-approve as a
  one-turn lease whose failures keep authority; the strict terminal policy;
  escalation notices as evidence; compaction and MCP elicitation as passive
  chat UX; the usage status line, which is deliberately *not* a hook — it
  prints by design, rides a throwaway probe session, never a supervised pane,
  and is **off until the user consents**; and the model catalog, which is
  *read* from a zero-token launch rather than asked for in a billed turn.
- **`docs/invariants/attention.md`** — required before touching liveness,
  attention or needs-input detection, `internal/dialoghint`, `handlePeek`, the
  inspect/Dismiss surface, `SendKey`, or any polled render region. It owns:
  liveness is mechanical only; pane text may corroborate but never create
  attention; the owing backstop; neutral inspect is acknowledgeable, not
  actionable; the remote-key whitelist; polling must never clobber user input.

Nothing loads those two files for you. Match your change against the trigger
lists above before you start, and open the one that names your files — a rule
you did not read binds exactly as much as one you did. If you are working
anywhere else, the rules above and below are the whole contract.

### Notes, tests, scope

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
- **Frozen characterization suites.**
  `internal/app/router_characterization_test.go`,
  `internal/app/static_characterization_test.go`,
  `internal/app/web_js_static_test.go` and `internal/app/css_cascade_test.go`
  are frozen at commit `4e35aad`. Adding assertions is fine; needing to CHANGE
  or DELETE one is a review stop, not an edit — it means the refactor altered
  behaviour the suites exist to pin. (The freeze names a commit because one
  pre-freeze commit deliberately reversed a `css_cascade_test.go` assertion:
  indeterminate progress must keep animating under Reduce Motion.)
- The README's **Non-goals** section is a hard scope fence: features listed
  there need explicit maintainer approval, not code.

## tmux gotchas (learned the hard way)

- Pane-level commands (`capture-pane`, `paste-buffer`, `send-keys`) need the
  target written `=name:` (trailing colon). Bare `=name` resolves only for
  session-level commands (`has-session`, `kill-session`). Observed on tmux 3.6.
- Prompts are delivered as a single paste (`load-buffer`/`paste-buffer`), then
  a separate Enter — `send-keys` with raw text would re-interpret newlines as
  submissions. Structured transports keep their protocol-owned first-turn
  delivery instead.
- That paste is bracketed (`paste-buffer -d -p`). tmux translates a buffer's
  newlines to carriage returns, indistinguishable from Enter, so without the
  markers a receiving TUI can only guess from arrival speed whether a block
  was pasted or typed — under load that guess fails and one prompt lands as
  several submitted messages. tmux emits the markers only when the application
  requested bracketed-paste mode, so `-p` is inert for a wrapped `cat` or
  `bash` (pinned by `TestBracketedPasteIsInertWithoutRequest`).
- Line endings are normalized to LF before `load-buffer`
  (`normalizeNewlines`). Because tmux rewrites LF to CR on paste, a CRLF pair
  would survive as CR CR — two line breaks where the author wrote one, which
  corrupts the prompt and breaks first-turn delivery confirmation (the pasted
  text stops matching `canonicalPrompt`). Only line endings are touched:
  whitespace, deliberate blank lines and interior spacing are the user's text.

## Fixtures and privacy

- `internal/transcript/testdata/real-*.jsonl`,
  `internal/acp/codex/testdata/real-*.ndjson` and
  `internal/acp/testdata/real-*.ndjson` are captured from real CLI runs and
  are **gitignored — never commit them**: even scrubbed they are personal
  environment snapshots. Regenerate with `scripts/capture-fixtures.sh` and
  `scripts/capture-grok-acp-fixture.sh`, which run one tiny prompt per agent
  and scrub system prompts, tool/skill/path inventories, hostnames, session
  ids, timezone and secret-shaped strings.
- The committed fixtures (`claude-session.jsonl`, `codex-rollout.jsonl`,
  `internal/acp/codex/testdata/synthetic-session.ndjson`,
  `internal/acp/testdata/synthetic-grok-turn.ndjson`) are fully synthetic.
  Keep them that way; never paste real transcript or wire content into them.
