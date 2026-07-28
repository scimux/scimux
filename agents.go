// agents.go — which agent CLIs (harnesses) are installed on this host, and
// which models each of them can run. Feeds the web UI's new-activity dialog
// via GET /api/agents → {"claude": ["fable", …], "codex": [...], …}.
package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A harness is probed in two steps: LookPath decides whether it appears at
// all, then its own list command (where one exists — the CLI is the only
// party that knows its providers and credentials) supplies the models. A
// missing list command or a failed probe falls back to a static set: an
// installed harness must never vanish from the dialog because a list call
// broke, and an empty model list is still valid (the harness launches with
// its own default model).
type harness struct {
	bin string
	// require is the executable whose presence gates whether this harness is
	// offered at all. Empty means bin itself. It differs from bin when creation
	// launches through a different binary than the one that lists models: pi
	// nodes launch via `pi-acp` (the ACP transport) but list models via `pi`,
	// so offering pi when only `pi` — not `pi-acp` — is installed would present
	// a selectable agent that cannot start (finding 56).
	require  string
	list     func(ctx context.Context, bin string) []string
	fallback func() []string
}

var harnesses = []harness{
	// claude has no model-list command; these aliases are what `claude
	// --model` documents (latest model per family).
	{bin: "claude", fallback: func() []string {
		return []string{"fable", "opus", "sonnet", "haiku"}
	}},
	// codex has no list command either; put the locally configured model
	// first, then the known current line-up.
	{bin: "codex", fallback: codexModels},
	// pi is launchable only when its ACP binary `pi-acp` exists; the models
	// come from `pi --list-models` when that is also present.
	{bin: "pi", require: "pi-acp", list: piModels},
	// opencode exposes ACP as a subcommand of the same binary, so bin suffices.
	{bin: "opencode", list: opencodeModels},
}

var agentsOnce sync.Once
var agentsCache map[string][]string

// detectAgents probes once per process (a warm-up goroutine in main runs it
// at startup, so the first dialog open doesn't wait on subprocesses). A
// harness installed while scimux runs appears after a restart — acceptable
// for a tool that is itself restarted far more often than agents are
// installed.
func detectAgents() map[string][]string {
	agentsOnce.Do(func() {
		res := map[string][]string{}
		for _, h := range harnesses {
			require := h.require
			if require == "" {
				require = h.bin
			}
			// Presence is gated on the binary creation actually requires.
			if _, err := exec.LookPath(require); err != nil {
				continue
			}
			var models []string
			// Model probing uses bin, which may differ from require and may be
			// absent on its own — an installed launcher with no lister still
			// yields an offerable agent that runs on its default model.
			if h.list != nil {
				if bin, err := exec.LookPath(h.bin); err == nil {
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					models = h.list(ctx, bin)
					cancel()
				}
			}
			if len(models) == 0 && h.fallback != nil {
				models = h.fallback()
			}
			if models == nil {
				models = []string{}
			}
			res[h.bin] = models
		}
		agentsCache = res
	})
	return agentsCache
}

// claudeModelPrompt asks the running claude harness to enumerate the concrete
// model ids the current account can use. The CLI (observed on 2.1.x) mis-resolves
// the short family aliases — `--model opus` expands to `claude-4-8-opus`, the
// pre-4.x segment order the API no longer accepts — so scimux resolves
// opus/sonnet/haiku/fable to the concrete id itself and passes that to --model.
// The model answers from the ids actually available in its environment.
const claudeModelPrompt = "List every Claude model id I can use in this environment, one per line, " +
	"in the form claude-<family>-<version> (for example claude-opus-4-8). Output only the ids, nothing else."

// claudeModelID matches a concrete model id of a known family. The family name
// sits immediately after "claude-", so the mis-ordered "claude-4-8-opus" form
// never matches — exactly the ids we must not adopt.
var claudeModelID = regexp.MustCompile(`claude-(fable|opus|sonnet|haiku)-\d+(?:-\d+)*`)

// parseClaudeModels extracts a family->concrete-id map from probe output. It is
// deliberately tolerant: it picks model-id-shaped tokens out of arbitrary prose
// or markdown, first id per family wins, and unknown/mis-ordered tokens are
// ignored. No ids found yields an empty map (callers fall back to the alias).
func parseClaudeModels(out string) map[string]string {
	m := map[string]string{}
	for _, match := range claudeModelID.FindAllStringSubmatch(out, -1) {
		if _, seen := m[match[1]]; !seen {
			m[match[1]] = match[0]
		}
	}
	return m
}

// probeClaudeModels runs the claude harness once to learn the concrete ids it
// accepts. Best-effort: a missing binary, an auth failure, or an unparseable
// answer returns nil, and scimux falls back to passing the bare family alias
// (the pre-existing behavior). This shells out to a real agent CLI, so it runs
// only at startup, never in tests.
func probeClaudeModels(ctx context.Context) map[string]string {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, bin, "-p", claudeModelPrompt).Output()
	if err != nil {
		return nil
	}
	return parseClaudeModels(string(out))
}

// claudeCacheTTL bounds how long a successful model probe is trusted before
// scimux re-runs the (API-billed) `claude -p` call. A failed probe writes
// nothing, so a failure is simply retried at the next startup.
const claudeCacheTTL = 7 * 24 * time.Hour

// claudeCache is the on-disk shape of a successful probe (~/.scimux/claude-models.json).
type claudeCache struct {
	ProbedAt time.Time         `json:"probed_at"`
	IDs      map[string]string `json:"ids"`
}

// readClaudeCache loads a prior successful probe. A missing or unparseable file
// is not an error the caller must distinguish from staleness — it returns a zero
// cache, which is neither fresh (old ProbedAt) nor usable (empty IDs).
func readClaudeCache(path string) claudeCache {
	var c claudeCache
	b, err := os.ReadFile(path)
	if err != nil {
		return claudeCache{}
	}
	if json.Unmarshal(b, &c) != nil {
		return claudeCache{}
	}
	return c
}

// writeClaudeCache records a successful probe with the current timestamp.
func writeClaudeCache(path string, ids map[string]string) error {
	b, err := json.Marshal(claudeCache{ProbedAt: time.Now().UTC(), IDs: ids})
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

var codexConfigModel = regexp.MustCompile(`(?m)^\s*model\s*=\s*"([^"]+)"`)

// codexModels: the model the user configured in ~/.codex/config.toml first
// (a top-level `model = "…"` line; the regexp cannot match table entries
// because those keys are quoted or dotted), then a static current line-up.
func codexModels() []string {
	known := []string{"gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-5.3-codex"}
	home, err := os.UserHomeDir()
	if err != nil {
		return known
	}
	b, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		return known
	}
	m := codexConfigModel.FindSubmatch(b)
	if m == nil {
		return known
	}
	out := []string{string(m[1])}
	for _, k := range known {
		if k != out[0] {
			out = append(out, k)
		}
	}
	return out
}

// piModels parses `pi --list-models`: a column table whose first two fields
// are provider and model id; `pi --model` accepts the joined "provider/id"
// form. The header row is skipped by name, not position, so leading blank
// or notice lines don't poison the list.
func piModels(ctx context.Context, bin string) []string {
	out, err := exec.CommandContext(ctx, bin, "--list-models").Output()
	if err != nil {
		return nil
	}
	var models []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] == "provider" {
			continue
		}
		models = append(models, f[0]+"/"+f[1])
	}
	return models
}

// opencodeModels parses `opencode models`: one "provider/model" per line,
// which is exactly the form `opencode --model` takes back.
func opencodeModels(ctx context.Context, bin string) []string {
	out, err := exec.CommandContext(ctx, bin, "models").Output()
	if err != nil {
		return nil
	}
	var models []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			models = append(models, line)
		}
	}
	return models
}
