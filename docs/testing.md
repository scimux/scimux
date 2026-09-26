# Testing and release rules

Part of the invariant set in `AGENTS.md`. Read before changing tests, fixtures,
fuzzing, test infrastructure, build targets, or CI/release/install workflows.
For application changes, read the relevant subsystem rules as well.

## Commands

```sh
env GOTOOLCHAIN=auto CGO_ENABLED=0 go build -o scimux ./cmd/scimux # static; web is embedded
env GOTOOLCHAIN=auto go test ./...        # unit + integration (integration needs tmux)
env GOTOOLCHAIN=auto go test -short ./... # unit only; CI also runs the full suite
node --test web/test/*.test.js  # browser unit suite; no browser needed
gofmt -w $(find . -name '*.go' -type f) && go vet ./...
go test -run TestCrossBuildTargets ./internal/app  # cross-builds every target CI ships
go test ./internal/transcript -run=XXX -fuzz=FuzzParseLine       -fuzztime=30s
go test ./internal/app        -run=XXX -fuzz=FuzzClaudeHookStdin -fuzztime=30s
go test ./internal/backend    -run=XXX -fuzz=FuzzProtocolHeaders -fuzztime=30s
go test ./internal/app        -run=XXX -fuzz=FuzzWebChildConfiguration -fuzztime=30s
```

Keep `TMPDIR` short (prefer `/tmp`) when running the suite. Integration tests
create Unix-domain sockets below temporary directories, and long sandbox paths
can exceed the platform socket-path limit and produce unrelated failures.

- CI containers use `--init` so orphaned process-group test descendants are
  reaped. Fake long-lived Go helpers sleep on a timer rather than deadlocking
  in `select {}`; SIGTERM-resistant children signal readiness before testing
  escalation. Forced filesystem failures must also work when CI runs as root.
- The offline namespace has loopback plus a local dummy interface at
  `192.0.2.2/32`: Pion excludes loopback ICE candidates by default. The dummy
  has no external connection or default route; the job still verifies that
  off-host traffic is unreachable before running tests.
- **No `${{ }}` inside a workflow `run:` block.** An expression is substituted
  into the script as *text* before any shell parses it, so a ref name of
  `v0$(id)` is a command the runner executes — and quoting in the YAML cannot
  help, because the quotes are inside the text being substituted. Values reach
  the shell through `env:`, where a substitution can only become a variable's
  value, and a secret is declared on the one step that spends it rather than
  at workflow level where `go test` inherits it. `release-worker.yml` also bounds the
  tag, in a step that runs before anything is built or published: a positive
  `case` arm is not enough on its own, because a shell glob's `*` matches
  shell syntax too. `internal/app/workflow_injection_test.go` covers every workflow.
- The release matrix is not written down twice: `TestCrossBuildTargets` parses
  the `GOOS=… GOARCH=…` pairs out of `.github/workflows/{build,release-worker}.yml`,
  so a target added to a workflow is defended from that moment. freebsd/amd64
  is built but deliberately not released — it keeps the static-build invariant
  honest.
- Release builds and publication are separate jobs with a same-workflow
  artifact handoff. The build job has read-only repository permission; the fresh publish
  job accepts only the closed binary inventory, verifies `SHA256SUMS`, and only
  then exposes the release token to the reviewed upload script. GitHub upload
  and download actions are pinned to commits
  `ea165f8d65b6e75b540449e92b4886f43607fa02` and
  `d3f86a106a0bac45b974a628896c90dbdf5c8093`. The operator must configure the
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

## Public repository CI and release setup

`pr.yml` tests the PR merge commit on a fresh GitHub-hosted Ubuntu runner,
using the same Go toolchain version as main CI. It runs the full Go suite
(including private-socket tmux integration and cross-builds) and Node tests.
The checkout action is pinned, does not persist credentials, and receives only
read access. PRs use `pull_request`, never `pull_request_target`; no repository
secrets are passed. Outside-contributor approvals remain controlled by GitHub.
After a successful PR run, require **PR tests** in the main branch ruleset.
Do not require the push-only build/offline jobs as PR checks.

`release.yml` remains triggered by a published release. It calls
`scimux/scimux/.github/workflows/release-worker.yml@main` explicitly, so the
self-hosted jobs are defined by protected main rather than by the tag. The
worker accepts only this repository's published-release tag events, validates
the tag/commit binding, and requires the commit to be an ancestor of main
before executing source code. Its build and publisher retain separate tokens
and the verified artifact handoff. No manual approval is introduced.

After merging these files to main and before publishing the next release,
replace this entry in the organization's **scimux-runners** selected workflows:

```text
scimux/scimux/.github/workflows/release.yml@refs/heads/main
```

with:

```text
scimux/scimux/.github/workflows/release-worker.yml@refs/heads/main
```

Keep the build/offline and other repositories' entries. Do not add PR workflows
to this self-hosted group. Release the new main commit (or a later descendant):
old tags still contain the old caller workflow. Keep immutable releases disabled
until a separate change uploads and verifies assets in a draft before publishing.
