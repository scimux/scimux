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
go test -cover ./internal/testowned/testdata/supervisor # non-shipped supervisor command entry
go test -cover -tags testowned_unsupported ./internal/testowned # unsupported-platform refusal
gofmt -w $(find . -name '*.go' -type f) && go vet ./...
go test -run TestCrossBuildTargets ./internal/app  # cross-builds every target CI ships
go test ./internal/transcript -run=XXX -fuzz=FuzzParseLine       -fuzztime=30s
go test ./internal/app        -run=XXX -fuzz=FuzzClaudeHookStdin -fuzztime=30s
go test ./internal/backend    -run=XXX -fuzz=FuzzProtocolHeaders -fuzztime=30s
go test ./internal/app        -run=XXX -fuzz=FuzzWebChildConfiguration -fuzztime=30s
```

The non-shipped Linux supervisor command reports cleanup status separately from
`runner_exit`. `status clean` and command exit 0 mean the supervisor proved its
owned children were reaped; the runner may still have exited nonzero or by a
signal. A caller must check both results before accepting a run. Invalid command
arguments and unavailable supervisor prerequisites exit 2; cleanup failures
exit 1. The current interrupt grace is for this focused test tool; integration
must measure an adequate bounded teardown grace before using it for browser E2E.

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
  `cursor-agent`, `dsh`, `muse`, `vibe-acp`) in tests — wrapped test commands are
  `bash --norc` or `cat`. The Claude question and plan interaction tests are
  offline and run in the `-short` lane. Their fixtures are synthetic protocol
  values, not live CLI captures.
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
  are absent. `TestRealTeamE2EReplay` uses the ignored
  `internal/app/testdata/real-team-e2e-*` recording files. Live capture tools
  are private maintainer utilities archived under `attic/scripts/`; public
  tests must not depend on them. Keep system prompts, tool/skill/path
  inventories, hostnames, session ids, timezone and secret-shaped strings out
  of committed fixtures.
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

## Team server interoperability

`TestTeamServerWorkflow` exercises the seat handlers and outbox against a
real, independently built team server. Its conversations are synthetic and
it launches no agent CLI. Build `scimux-team` in its own repository, then run:

```sh
SCIMUX_TEAM_E2E_BINARY=/absolute/path/to/scimux-team \
  go test ./internal/app -run '^TestTeamServerWorkflow$' -count=1
```

The test owns its temporary server state and loopback listener. It covers
both sharing scopes, comment permissions and retries, author-name refresh,
permission changes, offline copies, relay restart and outbox recovery, title
changes and clear seams, removal, sharing again, node deletion, leave/rejoin,
operator revoke, and local forgetting.
It skips without that explicit binary path and in the `-short` lane. It
complements the browser unit suite and live browser acceptance; it does not
exercise DOM rendering or run automatically in the single-repository CI.

### Shared-chat ownership acceptance

The unified shared-chat UI must satisfy these cases with synthetic content
and fake agent transports before its canary release:

- A recipient with `read` or `write` access has no Agent destination or
  **Use as prompt** action in the received chat. Forged actions and direct
  requests cannot reach the sharer's agent or its controller routes.
- A writable recipient's comment becomes visible in the owner's original
  chat, with the recipient's authorship. Receipt, polling, retries and
  reconnects cause zero agent sends, including with auto-approval enabled.
- During a selected received chat, hold an actual mirror or comments response,
  disable Team in settings until the subscription list returns 404, then
  re-enable it. Release the stale response after a replacement read starts,
  post a new human comment, and verify recovery without switching chats,
  duplicate entries, lost drafts, false read marks, or agent sends.
- Only the owner can select **Use as prompt**. It copies the selected text
  into an editable Agent draft, preserves attribution and applicable AI
  provenance, and selects Agent without sending. Only the owner's later
  explicit Send dispatches that draft to their own agent.
- Use as prompt immediately marks the source bubble for a nonblank draft.
  The marker uses the bookmark marker placement with a distinct prompt icon
  and accessible label. Partial edits, draft restoration and polls preserve
  its durable source identity. Completely erasing the draft cancels the
  pending marker; unrelated later typing does not restore it. Confirmed
  delivery retains a source marker linked to the resulting prompt even after
  edits. A new empty draft does not remove previously confirmed links.
  Failed and unconfirmed sends never show a delivered marker. Two confirmed
  uses of one source in the same chat show distinct chooser labels; each
  choice navigates to its exact saved address, including through keyboard
  selection and Escape focus restoration.
- The sharing sheet has a people picker and no permission dropdown. Newly
  invited members are submitted with `access: "write"`; deselected people
  are omitted. Saving another membership change retains existing `read`
  grants. Background enforcement still rejects comments from read-only
  recipients and agent requests from all recipients.
- Returning to People, changing chats, losing access, or receiving a stale
  asynchronous result cannot dispatch a draft or transfer agent authority.

Cover both browser behaviour and the server/request boundary. Hiding a
button alone does not establish the ownership invariant documented in
[storage invariants](invariants/storage.md) and the [Team API](http-api.md#agent-ownership-boundary).

## Public repository CI and release setup

`pr.yml` tests the PR merge commit on a fresh GitHub-hosted Ubuntu runner,
using the same Go toolchain version as main CI. It runs the full Go suite
(including private-socket tmux integration and cross-builds) and Node tests.
The checkout action is pinned, does not persist credentials, and receives only
read access. PRs use `pull_request`, never `pull_request_target`; no repository
secrets are passed. Outside-contributor approvals remain controlled by GitHub.
After a successful PR run, require **PR tests** in the main branch ruleset.
Do not require the push-only build/offline jobs as PR checks.
`build.yml` and `offline.yml` run on the self-hosted `go-builder` only for
pushes to `main`; feature branches use the GitHub-hosted PR check. This keeps
jobs for untrusted branch pushes out of the main-only runner group.

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

## Opt-in browser team acceptance

`web/test/team-e2e.cjs` runs synthetic scimux seats against a separately built
real scimux-team relay. It uses real browser interactions for the sharing,
chat and navigation journeys; API calls provide fixture setup, forged requests
and evidence. The native edge-whitespace check uses the API because the
browser composer trims outer whitespace. All agent executables are synthetic;
no installed agent CLI is used. The phone runs use iPhone Chromium emulation,
not physical Safari. Width checks also use 320, 390 and 768 pixel viewports.

Python 3, tmux, Node 22+, Playwright and its Chromium browser are required.
Playwright is test-only and is not a shipped dependency. Install it outside
this repository, for example with `npm install --prefix /some/test-tools
playwright` followed by `/some/test-tools/node_modules/.bin/playwright install
chromium`; install the browser's system libraries as needed. Build scimux and
an independent relay. The complete suite also needs an unreleased
pre-attachment compatibility client, its matching source tree, and a
pre-attachment compatibility relay. These run as isolated synthetic seats to
verify mixed-version behavior. Build each fixture from an immutable revision
and retain the revisions and build hashes with the private test evidence; the
client source must match its binary. The relay capability advertised by the
`Scimux-Blob-Generation` header is unpublished. A relay with this support
must ship before the attachment-capable client, following separate approval.
Supply absolute paths:

```sh
CGO_ENABLED=0 go build -o /some/output/scimux ./cmd/scimux
(cd /some/scimux-team && CGO_ENABLED=0 go build -o /some/output/scimux-team ./cmd/scimux-team)
SCIMUX_E2E_BINARY=/some/output/scimux \
SCIMUX_TEAM_E2E_BINARY=/some/output/scimux-team \
SCIMUX_E2E_LEGACY_BINARY=/some/output/scimux-compatibility \
SCIMUX_E2E_LEGACY_SOURCE=/some/compatibility-client-source \
SCIMUX_TEAM_LEGACY_E2E_BINARY=/some/output/scimux-team-legacy \
SCIMUX_E2E_PLAYWRIGHT=/some/test-tools/node_modules/playwright \
SCIMUX_E2E_GO=/absolute/path/to/go \
node web/test/team-e2e.cjs
```

The codec over net.Pipe and HTTP bridge fixture requires Go 1.26 or newer.
`SCIMUX_E2E_GO` selects an absolute executable; if omitted, the runner resolves `go` from its incoming
preflight PATH. It verifies the selected version with local toolchain
selection, then invokes that exact executable with an offline module setting
and the restricted fake-agent PATH. Set `GOCACHE` and `GOMODCACHE` or `GOPATH`
to populated test caches for offline use. The manifest records its version and
executable digest; it does not record the path.

Set `PLAYWRIGHT_BROWSERS_PATH` for a custom browser location. The runner fails
on missing prerequisites. Its default executes all implemented scenarios and
records only assertions that passed. The printed private artifact directory
contains a manifest, screenshots, logs, and raw browser JavaScript coverage
with each page checked against its matching current or compatibility client
source. It can contain synthetic credentials: keep it private.
To check the retained manifest and its declared screenshot files, select the
source-defined profile independently of the evidence:

```sh
node web/test/testdata/team-e2e/audit-evidence.cjs --profile people-attachments /path/to/private/run
```

Use `owner-agent-attachments` for the pre-attachment scenario source and
`people-attachments` for the People attachment scenario source. The auditor
checks assertion results, scoped counter changes and the PNG signature and
minimum header structure; it does not decode images. It emits counts and
PASS or FAIL without evidence content. Its structural checks do not attest
that mutable evidence is authentic or that recorded events occurred.
The focused common file-chip contrast check also takes the source tree and a
private artifact directory: `node web/test/testdata/team-e2e/file-contrast.cjs
/absolute/scimux/source /absolute/private/artifacts` with
`SCIMUX_E2E_PLAYWRIGHT` set. On Linux, run the opt-in real-browser tunnel
interruption and timeout checks with `SCIMUX_REMOTE_CLEANUP_REAL=1 node --test
web/test/testdata/team-e2e/test-remote-real-cleanup.mjs`; they inspect owned
processes via `ps` and `/proc` and require the same Playwright environment.
The standalone synthetic attachment checks `missing-file-retry.cjs`,
`invalid-preview.cjs` and `remote-attachment-retry.cjs` accept the same source
and private artifact directory arguments. The complete suite additionally
tests retry and download against isolated application seats and the real relay.
Each run owns its temporary homes, relay, fake PATH, workers and private tmux
sockets. Teardown deletes its fixture nodes and stops owned processes on
success, failure and catchable signals; it retains an owned socket and fails
if the server cannot be confirmed dead. The native fixture can also be checked
with `python3 web/test/testdata/team-e2e/test_native.py`. Cleanup behavior is
checked with `node --test web/test/testdata/team-e2e/test-cleanup.mjs
web/test/testdata/team-e2e/test-tmux-cleanup.mjs`. `SCIMUX_E2E_FAIL_AFTER_NATIVE=1`
injects an intentional failure; `SCIMUX_E2E_CLEANUP_PROBE=failure` or `signal`
selects a cleanup probe, not an acceptance run.
`SCIMUX_E2E_CLEANUP_PROBE=overlap` opens a real synthetic browser and native
fixture, starts a held teardown, and waits for a signal. With the usual E2E
environment, `node --test web/test/testdata/team-e2e/test-cleanup-overlap.mjs`
checks both SIGINT and SIGTERM, the final manifest, coverage and private cleanup.
The same test also holds an in-flight page coverage capture across each signal
and verifies that its scripts are collected exactly once.

Use `SCIMUX_E2E_SCENARIO` for a bounded run. The available selections are
`ui-join`, `stale-membership`, `stale-removed-membership`, `early-native`,
`conversation-core`, `conversation-native`, `conversation-refusal`,
`conversation-trace`, `native-start-failure`, `recipient-boundary`,
`received-offline`, `permission-lifecycle`, `recipient-leave`, `leave-stale`,
`leave-empty`, `held-evidence`, `dropped-send`, `reshare-stale`, `ai-source`,
`viewport-keyboard`, `media-contrast`, `unshared`, and `held-chat-loading`.
The selections `stage1-scanner`, `stage1-unread`, and
`stage1-owner-files` exercise active and stale chat selection, unread
authorship and per-card indicators, and the owner's attachment composer.
They use the same fake agent transports, private seats, and real relay as the
default run. `stage2-shared-actions` checks shared bubble actions, exact
bookmark jumps with duplicate text, local annotations and destinations, and
an explicitly sent owner prompt. `stage2-bookmark-pages` checks an exact jump
across the shared comment page boundary with duplicate text. These scenarios
also run as part of the complete default suite. The bounded
`recreated-chat-isolation`, `bubble-selection`, and `draft-recovery` scenarios
exercise reused local identities, pointer selection, and explicit review of
older saved drafts on desktop and phone. `header-focus` checks Mine and sharer
disclosure focus during unread updates. `description-save-isolation`,
`description-recreated-choice`, and `description-choice-order` exercise held
save responses, reused destination IDs and competing held destination choices.
`no-selection` opens an empty synthetic seat and removes an active selected
chat while an old response is held. `delete-recreation-isolation` checks a
held deletion against a replacement with the same node ID.
`legacy-closed-recreation` seeds an old closed record and holds its deletion
across creation of a replacement with the same ID. The scanner journey also
holds an old active response across a rapid local–shared–local switch and
an idle reconnect. The default suite also runs each of these scenarios. Its share-selection
fixture matches the server, owner, chat and generation, so accumulated shares
from the same owner cannot redirect an assertion to a different card. It also
uses a fresh shared chat for each focus check: earlier browser contexts may
still be viewing an older shared chat and legitimately mark its messages read.
The held-evidence fixture holds the actual send POST response to observe the
pending marker, and separately holds GET trace responses to distinguish
accepted from confirmed. It explicitly releases the synthetic agent; delaying
the fake turn alone does not hold the browser's POST acknowledgement.
`browser-lifecycle` verifies that completed desktop and phone contexts retain
their captured coverage through retirement, including a synthetic failure.
The bounded `people-midbody-permission` and `people-local-budget` selections
exercise the body-read and managed-budget cases described below. Both run in
the no-selector default suite. The manifest checks can also be run on a saved
private artifact with `node web/test/testdata/team-e2e/verify-midbody-witness.cjs
<artifact>/manifest.json` and `node web/test/testdata/team-e2e/verify-local-budget.cjs
<artifact>/manifest.json` respectively.
The default run retires completed cases after their screenshots and coverage
are saved, while keeping each active case's actor contexts together.
The test-only native fixture uses a marker in its private synthetic HOME to
stay alive without SessionStart for the startup-failure scenario. Normal
fixture launches run the usual hook.

The manifest's assertion IDs map the principal requirements to real journeys:

| Requirement | Scenario and assertion examples |
| --- | --- |
| Selected chat sharing and membership | `ui-join`: `selected-ui-team-join`; `stale-membership`: `stale-keeps-current-read`; `stale-removed-membership`: `stale-removed-stays-absent` |
| Activities, folds and pins | Default: `mine-first-distinct-identities`, `folded-unread-poll`, `mixed-pinned`; `viewport-keyboard`: `viewport-keyboard-*` |
| Unified discussion and source links | `conversation-core`: `trace-original-once`, `trace-human-not-agent-log`, `trace-confirmed-exact-address`, `trace-editor-focused-during-poll`, `trace-jump-exact-onscreen`; `early-native`: `early-native-author-source-once` |
| Owner-only agent control | `recipient-boundary`: `boundary-real-clear-and-fork-rejected`, `boundary-forgeries-rejected`, `boundary-armed-owner-comments-no-dispatch`; `ai-source`: `ai-source-explicit-exact-mirrored` |
| Offline, permissions and leave | `received-offline`: `received-offline-saved-copy-and-draft`, `received-explicit-retry-idempotent`; `permission-lifecycle`: `permission-read-draft-and-denial`; `recipient-leave`, `leave-stale`, `leave-empty`: their `leave-*` assertions |
| Delivery and drafts | `held-evidence`: `held-evidence-awaiting-not-sent`; `dropped-send`: `dropped-response-explicit-retry-once`; `reshare-stale`: `reshare-stale-response-keeps-new-generation`; `conversation-core`: `trace-held-upload-*` and `trace-failed-upload-*` |
| Rendering and native recovery | `media-contrast`: `media-caption-contrast`; `viewport-keyboard`: `viewport-control-target`; `held-chat-loading`: `held-chat-loading-reduced-motion`; `native-start-failure`: `native-start-failure-restores-draft` |
| Selection and owner discussion | `stage1-scanner`: `stage1-stale-active-response-isolated`, `stage1-shared-owner-active`; `stage1-unread`: `stage1-incoming-before-own-stays-unread`, `stage1-own-comment-no-unread` |
| Owner files and one-send arm | `stage1-owner-files`: `stage1-owner-file-delivered`, `stage1-people-file-feedback`; `conversation-core`: `stage1-owner-send-disarms` |
| People files and copy replacement | `people-files`: its file, preview and explicit-owner-send assertions; `people-copy-replacement`: its distinct identity, stale-upload, recovery and actionable-control assertions |
| People file recovery and unread | `people-attachment-recovery`: its retry and download assertions; `people-retired-drafts` and `people-file-unread`: their saved-draft and unread assertions |
| Uncertain People publication | `people-uncertain-retry`: its accepted-server, identical retry, newer-draft and no-agent-dispatch assertions |
| People upload permission change | `people-upload-permission`: its empty-file feedback, held-before-admission upload denial, partial draft retention and no-agent-dispatch assertions |
| People upload permission change during body | `people-midbody-permission`: the test-owned forwarder uses the isolated seat's private muxer socket and waits for its direct HTTP 100 Continue, which the muxer's Go server emits on the attachment handler's first body read after admission. It sends only part of the multipart body before changing access. A read-only direct request is refused without muxer Continue; a separate frontend request demonstrates that frontend Continue and partial bytes do not establish muxer admission. The test checks the same live copy, `409 not_active`, preserved draft and earlier stage, no publication, and explicit recovery. The private socket capability is never recorded in artifacts |
| Local managed storage budget | `people-local-budget`: an exclusive sparse synthetic file in the isolated seat's managed assets and a temporary server-owned global limit produce an actual local `507 storage_full`, with no corresponding relay blob. The test removes only that filler, retries the same stage ID explicitly, verifies exact retained bytes and one comment, and restores the settings; screenshots, source/draft isolation and zero agent counters are checked in both viewports |
| Relay file integrity | `people-relay-blob-recovery`: a fresh synthetic relay channel has its actual stored blob removed or replaced with same-length bytes of a different SHA-256 before the reader opens it; the reader reports unavailable, then explicitly retries and verifies restored and subsequently retained bytes without agent dispatch |
| File association isolation | `people-cross-channel-forgery`: a reader verifies two published attachments and attempts to use one channel's stage, manifest and published attachment address in the other, while both synthetic owners' Autoapproval leases remain armed and agent counters remain unchanged |
| People image bookmarks | `people-image-bookmark`: its exact source jump, local annotation and independent capture after copy removal and reload |
| People source lifetime | `people-source-cancel`: removal of source-derived text and file, same-bytes unrelated file, explicit send and absence of a false source link |

To exercise the held-multipart forwarder's cleanup paths, run the selected
`people-midbody-permission` scenario with `SCIMUX_E2E_MIDBODY_PROBE` set to
`failure`, `forward-error`, or `signal`. The first two runs intentionally
fail; the signal run reports interruption. Check the resulting manifest and
owned process and socket cleanup. Leave this probe unset for the default run.

The mapping identifies exercised behavior, not every combination of transport,
error and lifecycle state. Tests never substitute callback invocation for a
browser interaction. Structured refusal reports its actual error and does not
invent a `restore_draft` field; the separate Claude startup scenario exercises
that native recovery path. A complete acceptance run uses the current default
and the Go, Node and build checks described above; a bounded scenario alone
does not stand in for that run.
