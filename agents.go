// agents.go — which agent CLIs (harnesses) are installed on this host, and
// which models each of them can run. Feeds the web UI's new-activity dialog
// via GET /api/agents → {"claude": ["fable", …], "codex": [...], …}.
package main

import (
	"context"
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
