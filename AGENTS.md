# Agent guide for scimux

scimux supervises agent chats from a local web page: Claude runs in tmux;
structured agents use subprocess connections. Read `README.md` for architecture.
Its **Non-goals** are a hard scope fence requiring explicit maintainer approval.
This guide holds universal rules; read the matching detailed rules before edits.

## Commands

```sh
go build -o scimux ./cmd/scimux
go test ./...                    # unit + integration; integration needs tmux
go test -short ./...             # unit only; offline CI lane
node --test web/test/*.test.js   # browser unit suite; no browser needed
go vet ./...
```

Run `gofmt` on changed Go files. CI, cross-build and fuzz commands and their
contracts are in `docs/testing.md`.

## Required reading by change

These documents are part of the invariant set. Open every matching document
**before editing**; nothing loads them automatically. Read only the documents
whose triggers apply, including when changing their tests or instructions.

| Change touches | Read first |
| --- | --- |
| `internal/app/claude_*.go`; hook commands, bundles or `capabilities.json`; Claude usage/model probes or consent; launching, binding or retiring a Claude pane | `docs/invariants/claude-hooks.md` |
| Liveness, attention, needs-input, `internal/dialoghint`, `handlePeek`, inspect/Dismiss, `SendKey`, polled render regions, tmux targeting or prompt delivery | `docs/invariants/attention.md` |
| Remote access, pairing, bootstrap, browser module imports, codec vectors or `internal/remote/` dependencies | `docs/invariants/remote.md` |
| Rendezvous wire contract or vendoring | `docs/protocol/rendezvous-v1.md`, `docs/rendezvous-protocol-sync.md`, `docs/invariants/remote.md` |
| Transcript parsing/mirroring, node persistence, session logs, chat/history reads, provenance, Send-to/bookmarks, clear/fork, notes or note versioning | `docs/invariants/storage.md` |
| Worker/muxer/web-child ownership, startup, shutdown, recovery, self-update, IPC or discovery | `docs/session-workers.md` |
| Tests, fixtures, fuzzing, test infrastructure, build targets, CI, release or installer workflows | `docs/testing.md` |
| HTTP routes, payloads or browser API behavior | `docs/http-api.md` |

## Layout and dependencies

- `cmd/scimux` stays thin: startup and linker-stamped version only.
  `internal/app` owns application behavior and co-located tests/testdata.
  Private leaf packages in `internal/*` never import `internal/app`; `web` and
  `legal` are imported by the app, never the reverse. Keep application Go files
  and tests out of the repository root; no repository-wide test directory.
- Standard library only: no SQLite, WebSocket library or JS framework. New
  dependencies require discussion. Existing exceptions are
  `github.com/coder/acp-go-sdk` **only in `internal/acp/`**, and
  `github.com/pion/webrtc/v4` **only in `internal/remote/`**, with its indirect
  dependency closure. Updating `allowedModuleRequires` in
  `internal/app/remote_boundary_guard_test.go` is a new exception, not an upgrade.
- Ship one static binary, unconditionally: no remote/noremote build tags or
  alternate artifacts. Pion ships in every build; preserve `CGO_ENABLED=0`
  and FreeBSD compatibility. Dependency approval does not widen import scope.
- `web/js/app.js` is the sole entry and intentionally untested composition
  root. Read its header before assessing coverage; extract pure logic into
  tested modules. `Packet <N>` comments preserve design-phase traceability;
  do not mass-rename them.
- Markdown is deny-by-default in `.gitignore`; deliberately allowlist new
  published documents and check `git ls-files '*.md'`. Superseded plans and
  private tools live in untracked `attic/`, which must never be a build, test
  or CI dependency. Only `scripts/install.sh` is published under `scripts/`;
  never force-add retired tools from `attic/scripts/`.

## Universal invariants

- **One command, layered lifetimes.** Only `scimux` is public. The muxer owns
  the listener and metadata; the web child owns browser security/assets and
  remote access; one worker owns each CLI connection and writes its log.
  Updates detach and reconnect workers without stopping chats. Node deletion
  stops the owned chat; full stop preserves Claude tmux panes. Preserve
  ownership proof and recovery; never take ownership of an unrelated process.
- **Snapshot over stream.** Use tmux pane snapshots and CLI transcript JSONL,
  or structured agent records. Never parse terminal byte streams, use tmux
  control mode (`-CC`), WebSockets or a browser terminal emulator. The UI polls.
  Each tmux chat has one session, one window, one pane and one agent process.
- **Consent precedes paid auxiliary work.** Server-owned settings in
  `~/.scimux/settings.json`, not browser `ui.json`, gate the Claude usage probe
  (`claude_usage_checks`, checked by `collectClaudeUsage`) and Muse approval
  judge (`muse_approval_judge_consent`). Missing/unreadable consent is off.
  New paid auxiliary calls need the same explicit consent. Local model
  discovery and `--version` must remain free.
- **Durable history and provenance.** Node records and session logs are
  append-only; every transport uses the same JSONL session-log schema.
  Unknown transcript shapes degrade silently to peek; preserve provenance.
  Never present known AI output as human-authored. `/clear` appends a page-turn
  seam under the same node; fork creates a fresh node without conversation
  history. Mutable notes are the documented exception to append-only storage.
- **Privacy is a project rule.** User history may feed deterministic
  heuristics, never model training, fine-tuning, distillation or development.
  Do not publish cross-harness benchmarks from it. Committed fixtures must be
  independently synthetic: no real transcript/wire captures, system prompts,
  tool/skill/path inventories, hostnames, session IDs, timezones or secrets.
  Local `real-*` captures remain ignored even after scrubbing; optional replay
  tests skip without them. Public tests never depend on private capture tools.
- **Tests cannot touch a user's agents.** Never run real agent CLIs in tests;
  this includes `claude`, `codex`, `pi`, `opencode`, `grok`, `cursor-agent`,
  `dsh`, and `muse`. Use `bash --norc`, `cat` or fake protocol/helper
  processes. Integration tests use private random tmux sockets and remove both
  servers and socket files.
  Fuzz crashers are bug reports to fix, never committed fixtures.
- **Frozen characterization suites.**
  `internal/app/router_characterization_test.go`,
  `internal/app/static_characterization_test.go`,
  `internal/app/web_js_static_test.go` and `internal/app/css_cascade_test.go`
  are frozen at commit `300c7b0`. Adding assertions is fine; changing or
  deleting one is a review stop because it changes pinned behavior. Preserve
  indeterminate progress animation under Reduce Motion; its deliberate
  pre-freeze correction is why the freeze names that commit.
