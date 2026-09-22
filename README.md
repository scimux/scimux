# scimux

**Your control room for supervising AI-assisted research.**
Run many investigations without losing the plot: scimux shows you where you're
needed and keeps every discovery traceable.

*Pronounced “sigh-mux” — sci as in sci-fi, mux as in tmux.*

[![Version](https://img.shields.io/github/v/release/scimux/scimux?label=version&color=blue)](https://github.com/scimux/scimux/releases)
[![License](https://img.shields.io/badge/license-MPL--2.0-brightgreen)](LICENSE)

## Install

```sh
curl -fsSL https://scimux.ai/install | bash
scimux
```

Then open <http://127.0.0.1:8787/>, tap **+**, choose an installed agent and
start a conversation. The installer puts one static binary in
`~/.local/bin`; it uses no `sudo`, changes no shell files, and installs no
agent CLI.

You can also download a binary from the
[releases page](https://github.com/scimux/scimux/releases) or build it yourself:

Source builds require Go 1.26 or later. With Go's default
`GOTOOLCHAIN=auto`, this module selects the patched Go 1.26.8 toolchain.

```sh
env GOTOOLCHAIN=auto CGO_ENABLED=0 go build -o scimux ./cmd/scimux
```

## What it gives you

- One view of every investigation: working, quiet, finished, or waiting for
  your input.
- Conversations rendered from each agent's structured records, with a raw
  inspect view when scimux cannot make a reliable claim.
- A metro-map view of journeys, forks, page turns, and token usage (where
  supported).
- Search across live and archived chats, plus immutable bookmarks that jump
  back to their source.
- Mutable notes with sections and embedded chat references, so conclusions
  stay attached to evidence.
- File and image attachments, auditable approval decisions, conversation
  forks, and explicit one-turn auto-approval.

Closing the browser does not stop the agents. Each chat has its own worker,
and its history is written continuously to plain JSONL on your machine.

## Supported agent harnesses

scimux supervises an agent CLI you installed and authenticated yourself. It
does not provide model access, accounts, subscriptions, credentials, or
tokens.

| Agent | scimux integration | What must be installed |
| --- | --- | --- |
| [Claude Code](https://code.claude.com/docs/en/overview) | Owned tmux session, transcript, official hooks, and Remote Control | A current `claude` release, plus `tmux`; tested with v2.1.278 |
| [Codex CLI](https://developers.openai.com/codex/cli/) | Codex app-server protocol | `codex` |
| [pi](https://pi.dev/) | Agent Client Protocol (ACP) | `pi-acp`; `pi` enables model discovery |
| [opencode](https://opencode.ai/docs) | ACP via `opencode acp` | `opencode` |
| [Grok CLI](https://docs.x.ai/build/cli/reference) | ACP via `grok agent stdio` | `grok`, authenticated with `grok login` |
| [Cursor CLI](https://docs.cursor.com/en/cli/installation) | ACP via `cursor-agent acp` | `cursor-agent` |
| [dsh](https://github.com/deepseek-ai/deepseek-harness) | ACP via `dsh --profile acp` | `dsh` and an `acp` profile whose default model is usable |
| [Muse Code](https://developer.meta.com/ai/products/muse-code/) | MSP via `muse serve` | `muse`; launch consent is off until the computer owner enables it |

The new-activity dialog shows only harnesses that are launchable on the
current machine. Model and reasoning choices come from each CLI where it has a
read-only discovery surface; otherwise scimux uses the harness default rather
than opening a disposable session or guessing.

The burger menu's **Check for harness updates** action refreshes that local
inventory and those model catalogs, checks each harness's public release
channel, and checks for a new scimux release. This is the way a long-running
scimux notices a newly installed CLI, a changed pi/OpenCode configuration, or
a newly published model such as a Grok model without being restarted. It does
not install harness updates, and it still cannot enumerate dsh models because
dsh has no read-only discovery surface.

scimux starts Claude Code with [`--remote-control`](https://code.claude.com/docs/en/remote-control),
so the same local session can also be continued from `claude.ai` or the Claude
mobile app when your Anthropic account permits it.

### dsh profile setup

scimux launches `dsh --profile acp`. That profile has its own model route, so
a model selected in `dsh web` is not necessarily the model its ACP profile
uses. For DeepSeek's official service, store `DEEPSEEK_API_KEY` through dsh's
Models page, or export it in the environment that launches scimux.

For a custom or local provider already declared in `~/.dsh/settings.yaml`, set
the ACP route in `~/.dsh/profiles/acp/cordis.patch.yml` to the same provider and
model identifiers; for example:

```yaml
- id: acp
  config:
    provider: mynamefortheprovider
    model: qwen/qwen3.8-27b
```

Check the effective profile before starting scimux:

```sh
dsh --profile acp --dump-config
```

## How it works

`scimux` is the only public command. The one executable runs three layers:

```text
browser <-> web child <-> muxer <-> one session worker per chat <-> agent CLI
```

The muxer owns the listener and durable metadata. The web child serves the
embedded interface. A worker owns each CLI connection and is the sole writer
of that chat's session log. Claude runs in a private tmux session; Codex, Muse,
and ACP harnesses run as structured subprocesses.

The browser polls snapshots; scimux does not stream terminal bytes and does
not put a terminal emulator in the page. Agent transcripts and protocol
records are the conversation source of truth. When evidence is incomplete,
the UI says so instead of inventing state.

Updates replace the web and muxer layers and reconnect existing workers.
Unexpected muxer restarts are recoverable. `scimux stop` deliberately retires
structured subprocesses while preserving scimux-owned Claude tmux sessions
for recovery.

## Data, accounts, and security

By default scimux listens only on `127.0.0.1:8787`. The web UI has no login:
anyone who can reach the listener can read conversations and answer approval
prompts. Do not bind it to an untrusted network. See [SECURITY.md](SECURITY.md)
for the security boundary and vulnerability-reporting address.

scimux never reads a harness credential file and never runs a vendor login
flow. It starts the CLI you already authenticated. Those CLIs make their own
network requests under your existing vendor account and terms; scimux is not
in that path.

scimux's own records stay under `~/.scimux`:

- `nodes.jsonl` — append-only activity metadata.
- `sessions/<node>.jsonl` — one plain-JSONL conversation log per chat.
- `sessions/archive/` — logs of deleted activities.
- `notes/<id>/note.json` — mutable synthesis notes and their references.

User history is never used for model training, fine-tuning, or distillation.
scimux does not publish comparative harness benchmarks.

Two optional operations can spend account quota and therefore default off:
the Claude usage probe and the Muse approval judge. Their switches are
computer-owned settings in the burger menu, not browser-local preferences.

scimux is independent and is not affiliated with or endorsed by Anthropic,
OpenAI, xAI, Meta, Anysphere, DeepSeek, or the pi and opencode projects. Their
names and marks identify compatible tools only. Your use of each harness and
model remains governed by its provider's terms.

## Everyday operations

```sh
scimux                         # start; default http://127.0.0.1:8787/
scimux stop                    # stop this instance cleanly
scimux stop -data /other/path  # stop an instance that uses another data directory
```

Use `scimux -h` for the complete flag reference. The JSON HTTP interface used
by the web page is documented in [docs/http-api.md](docs/http-api.md).

## Non-goals

scimux is not a scheduler, task queue, browser terminal, plugin host, or
cloud store. It manages only chats it started, and never takes over unrelated
agent processes or histories. It is a single-user supervisor for agents
running on your machine.

## Development

```sh
env GOTOOLCHAIN=auto CGO_ENABLED=0 go build -o scimux ./cmd/scimux
env GOTOOLCHAIN=auto go test ./... # integration tests require tmux
node --test web/test/*.test.js   # Node 22+
env GOTOOLCHAIN=auto go vet ./...
```

The shipped application has no Node runtime dependency. See
[AGENTS.md](AGENTS.md) for architecture, privacy, dependency, fixture, and
testing invariants.

## Development disclosure

AI-enabled Software Engineering was used to realise this software, in particular through Claude, Codex, and Grok.

---

*scimux: because supervising ten conversations shouldn't take ten terminals.*
