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

// modelEffort is one model's reasoning-effort menu: the levels its CLI accepts
// and the level it defaults to. Only agents whose CLI advertises per-model
// effort (codex) populate this; the rest leave it unset and the UI falls back
// to a static per-agent effort list.
type modelEffort struct {
	Levels  []string `json:"levels"`
	Default string   `json:"default,omitempty"`
}

// agentInfo is one harness's offering to the new-activity dialog: an ordered
// model list plus, where the CLI reports it, a per-model effort menu keyed by
// model id. Efforts is nil for agents that don't advertise it (claude/pi/
// opencode) and for codex when only the static fallback is available.
type agentInfo struct {
	Models  []string               `json:"models"`
	Efforts map[string]modelEffort `json:"efforts,omitempty"`
}

// A harness is probed in two steps: LookPath decides whether it appears at
// all, then its own list command (where one exists — the CLI is the only
// party that knows its providers and credentials) supplies the models (and,
// for codex, the per-model effort menus). A missing list command or a failed
// probe falls back to a static set: an installed harness must never vanish
// from the dialog because a list call broke, and an empty model list is still
// valid (the harness launches with its own default model).
type harness struct {
	bin string
	// require is the executable whose presence gates whether this harness is
	// offered at all. Empty means bin itself. It differs from bin when creation
	// launches through a different binary than the one that lists models: pi
	// nodes launch via `pi-acp` (the ACP transport) but list models via `pi`,
	// so offering pi when only `pi` — not `pi-acp` — is installed would present
	// a selectable agent that cannot start (finding 56).
	require  string
	list     func(ctx context.Context, bin string) agentInfo
	fallback func() agentInfo
}

// justModels adapts a model-only lister (pi/opencode, which have no per-model
// effort data) into the agentInfo contract.
func justModels(f func(ctx context.Context, bin string) []string) func(ctx context.Context, bin string) agentInfo {
	return func(ctx context.Context, bin string) agentInfo {
		return agentInfo{Models: f(ctx, bin)}
	}
}

var harnesses = []harness{
	// claude has no model-list command; these aliases are what `claude
	// --model` documents (latest model per family).
	{bin: "claude", fallback: func() agentInfo {
		return agentInfo{Models: []string{"fable", "opus", "sonnet", "haiku"}}
	}},
	// codex advertises its catalog (models + per-model effort menus) via
	// `codex debug models`; the static fallback covers a broken/old CLI.
	{bin: "codex", list: codexModelsFromCLI, fallback: codexModels},
	// pi is launchable only when its ACP binary `pi-acp` exists; the models
	// come from `pi --list-models` when that is also present.
	{bin: "pi", require: "pi-acp", list: justModels(piModels)},
	// opencode exposes ACP as a subcommand of the same binary, so bin suffices.
	{bin: "opencode", list: justModels(opencodeModels)},
}

var agentsOnce sync.Once
var agentsCache map[string]agentInfo

// detectAgents probes once per process (a warm-up goroutine in main runs it
// at startup, so the first dialog open doesn't wait on subprocesses). A
// harness installed while scimux runs appears after a restart — acceptable
// for a tool that is itself restarted far more often than agents are
// installed.
func detectAgents() map[string]agentInfo {
	agentsOnce.Do(func() {
		res := map[string]agentInfo{}
		for _, h := range harnesses {
			require := h.require
			if require == "" {
				require = h.bin
			}
			// Presence is gated on the binary creation actually requires.
			if _, err := exec.LookPath(require); err != nil {
				continue
			}
			var info agentInfo
			// Model probing uses bin, which may differ from require and may be
			// absent on its own — an installed launcher with no lister still
			// yields an offerable agent that runs on its default model.
			if h.list != nil {
				if bin, err := exec.LookPath(h.bin); err == nil {
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					info = h.list(ctx, bin)
					cancel()
				}
			}
			// An empty model list means the probe found nothing usable (or ran
			// no probe): fall back to the static set, dropping any partial
			// effort data with it.
			if len(info.Models) == 0 && h.fallback != nil {
				info = h.fallback()
			}
			if info.Models == nil {
				info.Models = []string{}
			}
			res[h.bin] = info
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
// The prompt is deliberately terse and format-pinned: the parser takes the
// first id it sees per family, so prose that mentions an older id before the
// current one would mislead it. Demanding one id per family, latest only, exact
// dash-form, no other text keeps the answer a clean list and removes that risk.
const claudeModelPrompt = "For each Claude model family available to me here — opus, sonnet, haiku, fable — " +
	"output the single current model id, latest version only, one per line, in exact dash form " +
	"claude-<family>-<version> (for example claude-opus-4-8). " +
	"Output only those ids, one per line — no other text, no markdown, no duplicates, no older versions."

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

// parseCodexModels reads a `codex debug models` catalog into an agentInfo:
// list-visible slugs in catalog order, plus each model's effort menu (levels +
// default) when the catalog advertises one. Defensive like the transcript
// parsers — only slug/visibility/effort fields are read, unknown shapes are
// ignored, and any unmarshal failure or empty result yields a zero agentInfo so
// detectAgents' fallback takes over. Hidden models (visibility != "list") and
// empty slugs contribute nothing.
func parseCodexModels(out []byte) agentInfo {
	var r struct {
		Models []struct {
			Slug                     string `json:"slug"`
			Visibility               string `json:"visibility"`
			DefaultReasoningLevel    string `json:"default_reasoning_level"`
			SupportedReasoningLevels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if json.Unmarshal(out, &r) != nil {
		return agentInfo{}
	}
	var models []string
	efforts := map[string]modelEffort{}
	for _, m := range r.Models {
		if m.Slug == "" || m.Visibility != "list" {
			continue
		}
		models = append(models, m.Slug)
		var levels []string
		for _, l := range m.SupportedReasoningLevels {
			if l.Effort != "" {
				levels = append(levels, l.Effort)
			}
		}
		if len(levels) > 0 {
			efforts[m.Slug] = modelEffort{Levels: levels, Default: m.DefaultReasoningLevel}
		}
	}
	if len(models) == 0 {
		return agentInfo{}
	}
	if len(efforts) == 0 {
		efforts = nil
	}
	return agentInfo{Models: models, Efforts: efforts}
}

// codexModelsFromCLI runs `codex debug models` and returns its catalog. It is a
// debug command that also prints a warning to stderr, so Output() (stdout only)
// is deliberate. Any failure — missing binary, timeout, non-zero exit, or
// unparseable/empty catalog — returns a zero agentInfo so detectAgents falls
// back to codexModels(). The configured default model is floated to the front,
// matching the fallback's ordering.
func codexModelsFromCLI(ctx context.Context, bin string) agentInfo {
	out, err := exec.CommandContext(ctx, bin, "debug", "models").Output()
	if err != nil {
		return agentInfo{}
	}
	info := parseCodexModels(out)
	if len(info.Models) == 0 {
		return agentInfo{}
	}
	info.Models = prependModel(codexConfiguredModel(), info.Models)
	return info
}

// codexConfiguredModel reads the user's top-level `model = "…"` line from
// ~/.codex/config.toml (the regexp cannot match table entries because those
// keys are quoted or dotted). Empty when unset or unreadable.
func codexConfiguredModel() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		return ""
	}
	if m := codexConfigModel.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// prependModel floats configured to the front of models, deduped, order of the
// rest preserved. An empty configured (or one already leading) is a no-op.
func prependModel(configured string, models []string) []string {
	if configured == "" {
		return models
	}
	out := []string{configured}
	for _, m := range models {
		if m != configured {
			out = append(out, m)
		}
	}
	return out
}

// codexModels is the static fallback for a broken or old codex CLI: the
// configured model first, then the known current line-up. It carries no
// per-model effort data, so the UI falls back to its static per-agent effort
// list; keep the model list current enough to be useful but don't rely on it
// once the live probe works.
func codexModels() agentInfo {
	known := []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4", "gpt-5.4-mini"}
	return agentInfo{Models: prependModel(codexConfiguredModel(), known)}
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
