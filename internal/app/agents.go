// agents.go — which agent CLIs (harnesses) are installed on this host, and
// which models each of them can run. Feeds the web UI's new-activity dialog
// via GET /api/agents → {"claude": ["fable", …], "codex": [...], …}.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp/muse"
)

// modelEffort is one model's reasoning-effort menu: the levels its CLI accepts
// and the level it defaults to. Codex obtains this from its live catalog; Grok
// obtains it from its models cache with a static fallback. Other agents leave
// it unset and the UI falls back to a static per-agent effort list.
type modelEffort struct {
	Levels  []string `json:"levels"`
	Default string   `json:"default,omitempty"`
}

// agentInfo is one harness's offering to the new-activity dialog: an ordered
// model list plus, where known, a per-model effort menu keyed by model id.
// Efforts is nil for agents that don't advertise it (claude/pi/opencode) and
// for codex when only the static fallback is available.
type agentInfo struct {
	Models     []string               `json:"models"`
	Efforts    map[string]modelEffort `json:"efforts,omitempty"`
	MuseModels []museModelView        `json:"muse_models,omitempty"`
}

const (
	museTierStandard   = "standard"
	museTierDiscounted = "discounted"
	museTierUnknown    = "unknown"
)

type museModelView struct {
	ID           string `json:"id"`
	Label        string `json:"label,omitempty"`
	Default      bool   `json:"default,omitempty"`
	Tier         string `json:"tier"`
	Launchable   bool   `json:"launchable"`
	ContextLimit *int   `json:"context_limit,omitempty"`
	OutputLimit  *int   `json:"output_limit,omitempty"`
}

var (
	errMuseCatalogUnavailable = errors.New("muse catalog is unavailable")
	errMuseModelNotLaunchable = errors.New("requested muse model is not launchable")
	errMuseNoStandardModel    = errors.New("no launchable muse standard model")
)

// A harness is probed in two steps: LookPath decides whether it appears at
// all, then its own list command (where one exists — the CLI is the only
// party that knows its providers and credentials) supplies the models (and,
// for codex/grok, the per-model effort menus). A missing list command or a failed
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
	// grok exposes ACP as `grok agent stdio`; models come from `grok models`
	// and per-model effort menus from the CLI's models cache when available.
	{bin: "grok", list: grokModelsFromCLI, fallback: grokModelsFallback},
	// Muse has no static model fallback. Catalog rows come from the injectable
	// probe (handleAgents overlay); an installed binary with no probe is an
	// empty, valid model list.
	{bin: "muse"},
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
		agentsCache = probeAgents(harnesses)
	})
	return agentsCache
}

// probeAgents performs one uncached discovery pass. Keeping the cache wrapper
// separate lets tests supply only local script stubs: the full suite must never
// invoke whichever real agent CLIs happen to be installed on the host.
func probeAgents(hs []harness) map[string]agentInfo {
	res := map[string]agentInfo{}
	for _, h := range hs {
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
	return res
}

// claudeProbeWorkdir is the directory every claude probe runs in, created on
// demand. It must never be the cwd scimux inherited: claude writes a
// transcript into ~/.claude/projects/<slug of its cwd>/, so a probe launched
// from a supervised node's directory drops a throwaway session into that
// node's project folder, where the relink machinery can adopt it. A dedicated
// dir under ~/.scimux keeps it somewhere no node can own; if that cannot be
// created the OS temp dir stands in — anything but the inherited cwd.
func claudeProbeWorkdir(dir string) string {
	if dir != "" && os.MkdirAll(dir, 0o700) == nil {
		return dir
	}
	return os.TempDir()
}

// claudeCacheTTL is the backstop under the version key, not the primary
// invalidation. New model ids arrive with a new claude build, so the CLI's own
// version is the evidence that a stored answer still holds; the clock only
// covers the case the version cannot — a scimux process that outlives a
// release without restarting, which is the shape of a long-running supervisor.
//
// It is a day rather than a week because the probe no longer costs anything.
// The old value bought a weekly ceiling on a billed `claude -p` call; the
// zero-token catalog read it replaced has no such price to amortize.
const claudeCacheTTL = 24 * time.Hour

// claudeCache is the on-disk shape of a successful probe (~/.scimux/claude-models.json).
type claudeCache struct {
	Version  string            `json:"claude_version"`
	ProbedAt time.Time         `json:"probed_at"`
	IDs      map[string]string `json:"ids"`
}

// readClaudeCache loads a prior successful probe. A missing or unparseable file
// is not an error the caller must distinguish from staleness — it returns a zero
// cache, which claudeCacheUsable rejects on every count.
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

// claudeCacheUsable decides whether a stored answer may be served for the
// installed CLI. Both keys must hold, with one asymmetry: an *unknown*
// installed version (the `claude --version` call failed, or claude is not on
// PATH) is not a mismatch. It means the question could not be asked, and a
// stored answer inside the backstop still beats offering none — whereas a
// cache carrying no version at all cannot claim to match anything.
func claudeCacheUsable(c claudeCache, version string, now time.Time) bool {
	if len(c.IDs) == 0 {
		return false
	}
	if now.Sub(c.ProbedAt) >= claudeCacheTTL {
		return false
	}
	if version == "" {
		return true
	}
	return c.Version != "" && c.Version == version
}

// writeClaudeCache records a successful probe against the build that produced
// it. A probe with no version is still worth storing: the TTL alone will carry
// it, and the next run with a readable version replaces it.
func writeClaudeCache(path, version string, ids map[string]string) error {
	b, err := json.Marshal(claudeCache{Version: version, ProbedAt: time.Now().UTC(), IDs: ids})
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// claudeCLIVersion reads the installed CLI's version. It is the cheap half of
// the model probe: a local subprocess that spends nothing, gating the
// expensive half (four throwaway sessions) behind "has anything changed?".
// An absent binary or an unreadable answer is "" — see claudeCacheUsable for
// what that means.
func claudeCLIVersion(ctx context.Context) string {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return ""
	}
	// CombinedOutput for the same reason harness_version.go uses it: some CLIs
	// print their version on stderr, and a non-zero exit is not a reason to
	// drop a version we can read.
	b, _ := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	return parseHarnessVersion(string(b))
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

// grokDefaultEfforts is the static effort menu when the CLI does not expose
// per-model levels: low/medium/high with high as the public default for
// grok-4.5 reasoning (https://docs.x.ai/developers/model-capabilities/text/reasoning).
func grokDefaultEfforts() modelEffort {
	return modelEffort{Levels: []string{"low", "medium", "high"}, Default: "high"}
}

// parseGrokModels extracts the default model id and the available-model list
// from `grok models` human text. Defensive: login banners and blank lines are
// ignored; bullet lines `* <id>` or `- <id>` (optional "(default)") contribute
// ids; the "Default model:" line supplies the float-to-front default.
func parseGrokModels(out string) (defaultModel string, models []string) {
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "Default model:"); ok {
			defaultModel = strings.TrimSpace(rest)
			continue
		}
		// "* grok-4.6 (default)" or "- grok-4.5"
		if strings.HasPrefix(line, "*") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "*"))
		} else if strings.HasPrefix(line, "-") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "-"))
		} else {
			continue
		}
		id := line
		if i := strings.IndexByte(id, ' '); i >= 0 {
			id = id[:i]
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
	}
	return defaultModel, models
}

// grokModelsFallback is the static catalog when `grok models` fails: keep the
// harness offerable with a known current id and the public effort menu.
func grokModelsFallback() agentInfo {
	return withGrokStaticEfforts(agentInfo{Models: []string{"grok-4.5"}})
}

// withGrokStaticEfforts attaches the default low/medium/high menu to every
// model that has no effort entry yet.
func withGrokStaticEfforts(info agentInfo) agentInfo {
	if len(info.Models) == 0 {
		return info
	}
	if info.Efforts == nil {
		info.Efforts = map[string]modelEffort{}
	}
	def := grokDefaultEfforts()
	for _, m := range info.Models {
		if _, ok := info.Efforts[m]; !ok {
			info.Efforts[m] = def
		}
	}
	return info
}

// grokModelsFromCLI runs `grok models` and returns its catalog plus per-model
// effort menus. Efforts are enriched from ~/.grok/models_cache.json when that
// file is present (the CLI refreshes it on list); otherwise every model gets
// the static low/medium/high menu. Any failure yields a zero agentInfo so
// detectAgents falls back to grokModelsFallback.
func grokModelsFromCLI(ctx context.Context, bin string) agentInfo {
	out, err := exec.CommandContext(ctx, bin, "models").Output()
	if err != nil {
		return agentInfo{}
	}
	def, models := parseGrokModels(string(out))
	if len(models) == 0 {
		return agentInfo{}
	}
	models = prependModel(def, models)
	info := agentInfo{Models: models, Efforts: grokEffortsFromCache(models)}
	return withGrokStaticEfforts(info)
}

// grokEffortsFromCache reads per-model reasoning_efforts from the Grok CLI's
// models cache. The cache is not a public API — missing/unparseable files and
// models without effort data simply contribute nothing (caller fills static).
func grokEffortsFromCache(models []string) map[string]modelEffort {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(home, ".grok", "models_cache.json"))
	if err != nil {
		return nil
	}
	var cache struct {
		Models map[string]struct {
			Info struct {
				SupportsReasoningEffort bool   `json:"supports_reasoning_effort"`
				ReasoningEffort         string `json:"reasoning_effort"`
				ReasoningEfforts        []struct {
					Value   string `json:"value"`
					Default bool   `json:"default"`
				} `json:"reasoning_efforts"`
			} `json:"info"`
		} `json:"models"`
	}
	if json.Unmarshal(b, &cache) != nil || len(cache.Models) == 0 {
		return nil
	}
	efforts := map[string]modelEffort{}
	for _, id := range models {
		entry, ok := cache.Models[id]
		if !ok || !entry.Info.SupportsReasoningEffort {
			continue
		}
		var levels []string
		def := entry.Info.ReasoningEffort
		for _, e := range entry.Info.ReasoningEfforts {
			if e.Value == "" {
				continue
			}
			levels = append(levels, e.Value)
			if e.Default {
				def = e.Value
			}
		}
		if len(levels) == 0 {
			continue
		}
		if def == "" {
			def = "high"
		}
		efforts[id] = modelEffort{Levels: levels, Default: def}
	}
	if len(efforts) == 0 {
		return nil
	}
	return efforts
}

// handleAgents reports the installed harnesses and their models — the
// new-activity dialog's source of truth (its built-in list is only the
// fallback for when this call fails).
func (a *app) handleAgents(w http.ResponseWriter, r *http.Request) {
	// Opening the new-activity dialog is the moment a stale model list would be
	// seen, so it is the moment to start refreshing one. This never blocks the
	// response: the dialog is served from whatever is known now, and a newly
	// installed claude shows up the next time it opens.
	a.ensureClaudeModels()
	var base map[string]agentInfo
	if a != nil && a.agentCatalog != nil {
		base = a.agentCatalog()
	} else {
		base = detectAgents()
	}
	// detectAgents returns the process-wide cache. Never apply request/app
	// overlays to that shared map: concurrent browsers must not race, and one
	// failed probe must not inherit rows from an earlier request.
	out := cloneAgentCatalog(base)
	a.applyMuseCatalog(out)
	writeJSON(w, out)
}

func cloneAgentCatalog(in map[string]agentInfo) map[string]agentInfo {
	out := make(map[string]agentInfo, len(in))
	for agent, info := range in {
		out[agent] = info
	}
	return out
}

func (a *app) museTierOf(id string) string {
	if a == nil || a.museClassify == nil {
		return museTierUnknown
	}
	switch t := a.museClassify(id); t {
	case museTierStandard, museTierDiscounted:
		return t
	}
	return museTierUnknown
}

// classifyMuseStandard is the maintainer-approved production tier authority:
// every nonblank model ID returned by Muse's live model/list catalog is
// Standard. Catalog membership remains independently authoritative, so this
// does not make forged, stale, blank, or malformed IDs launchable and does not
// infer policy from a model name, label, provider, or update response.
func classifyMuseStandard(modelID string) string {
	if strings.TrimSpace(modelID) == "" {
		return museTierUnknown
	}
	return museTierStandard
}

// museImplicitDefaultEligible keeps content-sharing contributor variants an
// explicit user choice. They remain launchable when selected by exact ID, but
// neither the catalog's default bit nor the server's first-Standard fallback
// may select one on the user's behalf.
func museImplicitDefaultEligible(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	return id != "" && !strings.HasSuffix(id, "-contributor")
}

func museModelLabel(m muse.Model) string {
	if s := strings.TrimSpace(m.Label); s != "" {
		return s
	}
	return strings.TrimSpace(m.Name)
}

func intPtrEq(a, b *int) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func stringPtrEq(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func copyIntPtr(v *int) *int {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}

func museModelConflict(a, b muse.Model) bool {
	if a.Name != b.Name || a.Label != b.Label || a.Source != b.Source || a.ProfileID != b.ProfileID {
		return true
	}
	if a.IsDefault != b.IsDefault {
		return true
	}
	return !intPtrEq(a.ContextLimit, b.ContextLimit) || !intPtrEq(a.OutputLimit, b.OutputLimit) ||
		!stringPtrEq(a.ReleaseDate, b.ReleaseDate)
}

func (a *app) museViews(models []muse.Model) []museModelView {
	type rec struct {
		view     museModelView
		src      muse.Model
		conflict bool
	}
	order := make([]string, 0, len(models))
	byID := map[string]*rec{}
	for _, m := range models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if prev, ok := byID[id]; ok {
			if museModelConflict(prev.src, m) {
				prev.conflict = true
				prev.view.Launchable = false
			}
			continue
		}
		tier := a.museTierOf(id)
		view := museModelView{
			ID:           id,
			Label:        museModelLabel(m),
			Default:      m.IsDefault && tier == museTierStandard && museImplicitDefaultEligible(id),
			Tier:         tier,
			Launchable:   tier == museTierStandard || tier == museTierDiscounted,
			ContextLimit: copyIntPtr(m.ContextLimit),
			OutputLimit:  copyIntPtr(m.OutputLimit),
		}
		byID[id] = &rec{view: view, src: m}
		order = append(order, id)
	}
	out := make([]museModelView, 0, len(order))
	for _, id := range order {
		r := byID[id]
		if r.conflict {
			r.view.Launchable = false
			r.view.Default = false
		}
		out = append(out, r.view)
	}
	return out
}

func (a *app) applyMuseCatalog(out map[string]agentInfo) {
	if out == nil {
		return
	}
	_, present := out["muse"]
	if !present {
		return
	}
	info := out["muse"]
	if info.Models == nil {
		info.Models = []string{}
	}
	if a == nil || a.museCatalog == nil {
		out["muse"] = info
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	models, err := a.museCatalog(ctx)
	if err != nil {
		info.Models = []string{}
		info.MuseModels = nil
		out["muse"] = info
		return
	}
	views := a.museViews(models)
	ids := make([]string, 0, len(views))
	for _, v := range views {
		if v.Launchable {
			ids = append(ids, v.ID)
		}
	}
	info.Models = ids
	info.MuseModels = views
	out["muse"] = info
}

func (a *app) resolveMuseLaunchModel(requested string) (string, error) {
	if a == nil || a.museCatalog == nil {
		return "", errMuseCatalogUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	models, err := a.museCatalog(ctx)
	if err != nil {
		return "", errMuseCatalogUnavailable
	}
	views := a.museViews(models)
	requested = strings.TrimSpace(requested)
	if requested != "" {
		for _, v := range views {
			if v.ID == requested {
				if !v.Launchable {
					return "", errMuseModelNotLaunchable
				}
				return v.ID, nil
			}
		}
		return "", errMuseModelNotLaunchable
	}
	firstStd := ""
	for _, v := range views {
		if !v.Launchable || v.Tier != museTierStandard || !museImplicitDefaultEligible(v.ID) {
			continue
		}
		if v.Default {
			return v.ID, nil
		}
		if firstStd == "" {
			firstStd = v.ID
		}
	}
	if firstStd != "" {
		return firstStd, nil
	}
	return "", errMuseNoStandardModel
}

// probeMuseCatalog is the production catalog probe: `muse serve` through the
// isolated MSP client, initialize only, no session and no turn, always closed.
func probeMuseCatalog(ctx context.Context) ([]muse.Model, error) {
	return probeMuseCatalogWithSpawn(ctx, muse.Spawn)
}

func probeMuseCatalogWithSpawn(ctx context.Context, spawn func(context.Context, string) (muse.Transport, error)) ([]muse.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tr, err := spawn(ctx, "muse")
	if err != nil {
		return nil, err
	}
	c := muse.NewClient(tr, nil, nil)
	defer c.Close()
	if err := c.Initialize(ctx, "scimux", "1"); err != nil {
		return nil, err
	}
	return c.Models(ctx)
}
