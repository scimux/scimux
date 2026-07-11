# Agent guide for scimux

scimux supervises tmux-wrapped agent chats (Claude Code, Codex) from a local
web page. Read the README for architecture; this file lists only the
invariants you must not break and the workflows you need.

## Commands

```sh
go build -o scimux .   # single static binary; web/index.html is embedded
go test ./...          # unit + integration (integration needs tmux)
go test -short ./...   # unit only; this is what CI runs
gofmt -w . && go vet ./...
```

Integration tests create private, randomly named tmux sockets and clean up
after themselves; they never touch a user's tmux server. Never run a real
agent CLI (`claude`, `codex`) in tests — wrapped test commands are `bash
--norc` or `cat`.

## Invariants (deliberate design decisions — do not "improve" them away)

- **Zero build dependencies.** Standard library only. Do not add modules
  (no SQLite, no WebSocket lib, no JS framework). If a feature seems to need
  one, stop and discuss instead.
- **Snapshot over stream.** Output is read via `tmux capture-pane -p`
  snapshots and via the transcript JSONL files the agent CLIs write
  themselves. Never parse the terminal byte stream, never use tmux control
  mode (`-CC`), no WebSockets, no in-browser terminal emulator. Polling is
  the intended transport.
- **One session = one window = one pane = one agent process.** No muxing.
- **Defensive transcript parsing.** The CLIs' log formats are undocumented
  internals. Unknown record types/shapes are silently ignored, never errors;
  when a transcript is missing or stops making sense, the UI degrades to the
  pane snapshot ("peek"). Keep this contract when touching
  `internal/transcript`.
- **Append-only store.** `~/.scimux/nodes.jsonl` is replayed at startup;
  corrections are new records, never rewrites.
- **Fork = fresh context.** A forked node inherits launch config (agent,
  model, effort, dir) but never conversation history.
- **Liveness is mechanical only** (active/quiet/exited/unavailable, from
  pane-change detection). Do not add regexes matching agent TUI strings.
- **Polling must never clobber user input.** The web UI re-renders only
  when the state signature changes, and any re-render preserves input
  values, focus, and cursor (`withInputsPreserved` in web/index.html).
  Any new polled UI element must respect this.
- The README's **Non-goals** section is a hard scope fence; features listed
  there need explicit maintainer approval, not code.

## tmux gotchas (learned the hard way)

- Pane-level commands (`capture-pane`, `paste-buffer`, `send-keys`) need the
  target written `=name:` (trailing colon). Bare `=name` resolves only for
  session-level commands (`has-session`, `kill-session`). Observed on tmux 3.6.
- Prompts are delivered as a single paste (`load-buffer`/`paste-buffer`),
  then a separate Enter. `send-keys` with raw text would re-interpret
  newlines as submissions.
- The first prompt of a chat rides on the agent's launch command line
  (atomic, avoids TUI-readiness races); only follow-up turns use paste.

## Fixtures and privacy

- `internal/transcript/testdata/real-*.jsonl` are captured from real CLI
  runs, are **gitignored, and must never be committed**: even scrubbed they
  are personal environment snapshots. Regenerate with
  `scripts/capture-fixtures.sh` (runs one tiny prompt per agent, then scrubs
  system prompts, local tool inventories, and timezone).
- The committed `claude-session.jsonl` / `codex-rollout.jsonl` fixtures are
  fully synthetic. Keep them that way; never paste real transcript content
  into them.
