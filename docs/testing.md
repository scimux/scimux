# Testing and release rules

Part of the invariant set in `AGENTS.md`. Read before changing tests, fixtures,
fuzzing, test infrastructure, build targets, or CI/release/install workflows.
For application changes, read the relevant subsystem rules as well.

## Commands

```sh
go build -o scimux ./cmd/scimux # single static binary; web/index.html is embedded
go test ./...          # unit + integration (integration needs tmux)
go test -short ./...   # unit only; the offline CI lane (build.yml runs the full suite)
node --test web/test/*.test.js  # browser unit suite; no browser needed
gofmt -w $(find . -name '*.go' -type f) && go vet ./...
go test -run TestCrossBuildTargets ./internal/app  # cross-builds every target CI ships
go test ./internal/transcript -run=XXX -fuzz=FuzzParseLine       -fuzztime=30s
go test ./internal/app        -run=XXX -fuzz=FuzzClaudeHookStdin -fuzztime=30s
go test ./internal/backend    -run=XXX -fuzz=FuzzProtocolHeaders -fuzztime=30s
go test ./internal/app        -run=XXX -fuzz=FuzzWebChildConfiguration -fuzztime=30s
```

- **No `${{ }}` inside a workflow `run:` block.** An expression is substituted
  into the script as *text* before any shell parses it, so a ref name of
  `v0$(id)` is a command the runner executes — and quoting in the YAML cannot
  help, because the quotes are inside the text being substituted. Values reach
  the shell through `env:`, where a substitution can only become a variable's
  value, and a secret is declared on the one step that spends it rather than
  at workflow level where `go test` inherits it. `release.yml` also bounds the
  tag, in a step that runs before anything is built or published: a positive
  `case` arm is not enough on its own, because a shell glob's `*` matches
  shell syntax too. `internal/app/workflow_injection_test.go` pins all three.
- The release matrix is not written down twice: `TestCrossBuildTargets` parses
  the `GOOS=… GOARCH=…` pairs out of `.forgejo/workflows/{build,release}.yml`,
  so a target added to a workflow is defended from that moment. freebsd/amd64
  is built but deliberately not released — it keeps the static-build invariant
  honest.
- Release builds and publication are separate jobs with a same-workflow
  artifact handoff. The build job has no publication secret; the fresh publish
  job accepts only the closed binary inventory, verifies `SHA256SUMS`, and only
  then exposes the release token to the reviewed upload script. Forgejo upload
  and download actions are pinned to commits
  `16871d9e8cfcf27ff31822cac382bbb5450f1e1e` and
  `d8d0a99033603453ad2255e58720b460a0555e1e`. The operator must configure the
  `release-publisher` runner as a fresh environment. Runner labels do not grant
  authorization; repository write access remains maintainer-only.
- Integration tests create private, randomly named tmux sockets and never
  touch a user's tmux server. They clean up after themselves, and that
  includes the socket *file*: tmux does not unlink it when the server exits,
  so a helper that only kills the server leaves one 0-byte name per test in
  the user's `/tmp/tmux-<uid>` until the next reboot. `SocketPath` is what a
  cleanup deletes, and the two helpers assert the removal rather than
  best-effort it — litter nobody is told about is litter nobody clears. Never
  run a real agent CLI (`claude`, `codex`, `pi`, `opencode`, `grok`,
  `cursor-agent`, `dsh`, `muse`) in tests — wrapped test commands are
  `bash --norc` or `cat`.
- The fuzz targets state contracts as properties over all inputs; seed
  corpora run under plain `go test`. `FuzzParseLine` guards defensive parsing in
  `docs/invariants/storage.md`; `FuzzClaudeHookStdin` guards that no Claude hook helper writes to
  stdout whatever it is fed and disturbs nothing outside its own bundle
  subdirectory. A crasher written to `testdata/fuzz/` is a bug report, not a
  fixture: quote it, fix the code, do not commit it.

## Fixtures and privacy

- `internal/transcript/testdata/real-*.jsonl`,
  `internal/acp/codex/testdata/real-*.ndjson`,
  `internal/acp/testdata/real-*.ndjson`, and
  `internal/acp/muse/testdata/real-*.ndjson` are captured from real CLI runs and
  are **gitignored — never commit them**: even scrubbed they are personal
  environment snapshots. Optional replay tests skip when these local files
  are absent. Live capture tools are private maintainer utilities archived
  under `attic/scripts/`; public tests must not depend on them. Keep system
  prompts, tool/skill/path inventories, hostnames, session ids, timezone and
  secret-shaped strings out of committed fixtures.
- `TestRealDshReplay` and `TestRealDshModelMenu` are that lane for dsh, and they
  are the only checks that a real dsh still states occupancy in `usage_update`
  and still encodes a model option value as the JSON `["provider","model"]`
  pair the launch path decodes. A synthetic fixture can only prove we read the
  shape we wrote down, so when dsh's wire changes these are what notice — and
  they skip everywhere the capture is absent, which is everywhere but a
  maintainer's machine.
- Every committed JSONL/NDJSON fixture under `internal/*/testdata` is fully
  synthetic, including the transcript samples, ACP/Codex protocol samples,
  `internal/app/testdata/codex-session-sample.jsonl`, and the fare fixtures in
  `internal/sessionlog/testdata/fare/`. Keep them that way; never paste real
  transcript or wire content into them.
