package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp/codex"
)

func TestAgentCommandPiOpencode(t *testing.T) {
	pi := &Node{Agent: "pi", Model: "mistral/devstral-latest", Prompt: "hello"}
	if got, _ := agentCommand(pi); got != `pi --model 'mistral/devstral-latest' 'hello'` {
		t.Errorf("pi cmd = %s", got)
	}
	// opencode takes the first prompt as a flag, not a positional argument.
	oc := &Node{Agent: "opencode", Model: "openai/gpt-5.5", Prompt: "hello"}
	if got, _ := agentCommand(oc); got != `opencode --model 'openai/gpt-5.5' --prompt 'hello'` {
		t.Errorf("opencode cmd = %s", got)
	}
	bare := &Node{Agent: "pi", Prompt: "p"}
	if got, _ := agentCommand(bare); got != `pi 'p'` {
		t.Errorf("bare pi cmd = %s", got)
	}
	// grok is ACP in production; agentCommand is only a legacy tmux fallback.
	g := &Node{Agent: "grok", Model: "grok-4.5", Effort: "low", Prompt: "hello"}
	if got, _ := agentCommand(g); got != `grok -m 'grok-4.5' --reasoning-effort 'low' 'hello'` {
		t.Errorf("grok cmd = %s", got)
	}
}

func TestParseGrokModels(t *testing.T) {
	out := "You are logged in with grok.com.\n\nDefault model: grok-4.5\n\nAvailable models:\n  * grok-4.5 (default)\n  * grok-code-fast-1\n"
	def, models := parseGrokModels(out)
	if def != "grok-4.5" {
		t.Errorf("default = %q, want grok-4.5", def)
	}
	want := []string{"grok-4.5", "grok-code-fast-1"}
	if !reflect.DeepEqual(models, want) {
		t.Errorf("models = %v, want %v", models, want)
	}
	// Banner-only / empty → no models so detectAgents falls back.
	if _, m := parseGrokModels("You are logged in with grok.com.\n"); len(m) != 0 {
		t.Errorf("empty catalog = %v, want nil/empty", m)
	}
}

func TestWithGrokStaticEfforts(t *testing.T) {
	info := withGrokStaticEfforts(agentInfo{Models: []string{"grok-4.5", "other"}})
	for _, id := range info.Models {
		e, ok := info.Efforts[id]
		if !ok || !reflect.DeepEqual(e.Levels, []string{"low", "medium", "high"}) || e.Default != "high" {
			t.Errorf("effort for %s = %+v", id, e)
		}
	}
	// Existing per-model menu is preserved.
	info = withGrokStaticEfforts(agentInfo{
		Models:  []string{"m"},
		Efforts: map[string]modelEffort{"m": {Levels: []string{"low"}, Default: "low"}},
	})
	if got := info.Efforts["m"]; !reflect.DeepEqual(got.Levels, []string{"low"}) || got.Default != "low" {
		t.Errorf("preserved effort = %+v", got)
	}
}

func TestPiModelsParse(t *testing.T) {
	// Header is skipped by name; provider and id join with "/" — the form
	// `pi --model` accepts back.
	out := "provider                model     context\nmistral   devstral-latest   262K\n\nanthropic  claude-fable-5  1M\n"
	var models []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] == "provider" {
			continue
		}
		models = append(models, f[0]+"/"+f[1])
	}
	want := []string{"mistral/devstral-latest", "anthropic/claude-fable-5"}
	if len(models) != len(want) || models[0] != want[0] || models[1] != want[1] {
		t.Errorf("parsed %v, want %v", models, want)
	}
}

func TestHandleUIRoundTrip(t *testing.T) {
	a := &app{uiPath: filepath.Join(t.TempDir(), "ui.json")}
	put := func(body, ifMatch string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/api/ui", strings.NewReader(body))
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		a.handleUIPut(rec, req)
		return rec
	}

	// A fresh install has no file: GET must serve an empty object, not 404,
	// and carry the revision tag every write must name.
	rec := httptest.NewRecorder()
	a.handleUIGet(rec, httptest.NewRequest("GET", "/api/ui", nil))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Errorf("empty GET = %d %q", rec.Code, rec.Body.String())
	}
	rev := rec.Header().Get("ETag")
	if rev == "" {
		t.Fatal("GET carries no ETag")
	}

	// Writes without a base revision are refused outright.
	if rec := put(`{}`, ""); rec.Code != http.StatusPreconditionRequired {
		t.Errorf("PUT without If-Match = %d, want 428", rec.Code)
	}

	body := `{"groups":[{"id":"g1","name":"automotive","lanes":["a"]}],"archived":["x"],"notes":[]}`
	if rec := put(body, rev); rec.Code != 200 {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	a.handleUIGet(rec, httptest.NewRequest("GET", "/api/ui", nil))
	if rec.Body.String() != body {
		t.Errorf("GET after PUT = %q, want %q", rec.Body.String(), body)
	}
	rev2 := rec.Header().Get("ETag")

	// An unchanged revision polls for free.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/ui", nil)
	req.Header.Set("If-None-Match", rev2)
	a.handleUIGet(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", rec.Code)
	}

	// A write based on a superseded revision must conflict, not overwrite.
	if rec := put(`{"notes":["stale"]}`, rev); rec.Code != http.StatusConflict {
		t.Errorf("stale PUT = %d, want 409", rec.Code)
	}
	// The wildcard bootstrap always wins (single supervisor, explicit intent).
	if rec := put(body, "*"); rec.Code != 200 {
		t.Errorf("wildcard PUT = %d", rec.Code)
	}

	// Invalid JSON must be rejected, and must not clobber the stored state.
	if rec := put("{broken", rev2); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid PUT = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.handleUIGet(rec, httptest.NewRequest("GET", "/api/ui", nil))
	if rec.Body.String() != body {
		t.Errorf("state clobbered by rejected PUT: %q", rec.Body.String())
	}

	// Oversized blobs are refused before touching the file.
	huge := `{"notes":["` + strings.Repeat("x", uiStateMax) + `"]}`
	if rec := put(huge, rev2); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized PUT = %d, want 413", rec.Code)
	}
}

// The claude CLI mis-resolves its own family aliases (opus -> claude-4-8-opus,
// wrong segment order), so scimux probes the concrete ids and maps them itself.
// Parsing must be defensive: pick model-id-shaped tokens out of arbitrary prose
// or markdown, first per family wins, and the mis-ordered form must be ignored.
func TestParseClaudeModels(t *testing.T) {
	out := "Based on the model IDs available in this environment:\n\n```\n" +
		"claude-fable-5\nclaude-opus-4-8\nclaude-sonnet-5\nclaude-haiku-4-5\n```\n" +
		"You could also try claude-4-8-opus (a bogus, mis-ordered id) — ignore it.\n"
	got := parseClaudeModels(out)
	want := map[string]string{
		"fable":  "claude-fable-5",
		"opus":   "claude-opus-4-8",
		"sonnet": "claude-sonnet-5",
		"haiku":  "claude-haiku-4-5",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for fam, id := range want {
		if got[fam] != id {
			t.Errorf("family %q = %q, want %q", fam, got[fam], id)
		}
	}
	// A first-seen id wins; a later dated duplicate for the same family is ignored.
	dup := parseClaudeModels("claude-opus-4-8\nclaude-opus-4-8-20260101\n")
	if dup["opus"] != "claude-opus-4-8" {
		t.Errorf("first-seen id must win, got %q", dup["opus"])
	}
	// No model-id tokens at all -> empty map (caller falls back to the alias).
	if m := parseClaudeModels("I'm not sure which models you have."); len(m) != 0 {
		t.Errorf("no ids should yield empty map, got %v", m)
	}

	// A numbered list with backticks and trailing prose is still parsed cleanly.
	numbered := parseClaudeModels("1. `claude-opus-4-8` — your top model\n" +
		"2. `claude-sonnet-5` (fast)\n3. `claude-haiku-4-5`\n")
	if numbered["opus"] != "claude-opus-4-8" || numbered["sonnet"] != "claude-sonnet-5" || numbered["haiku"] != "claude-haiku-4-5" {
		t.Errorf("numbered/backticked list = %v", numbered)
	}

	// A date-stamped id is a valid concrete id and must be captured whole.
	if d := parseClaudeModels("claude-sonnet-5-20260101"); d["sonnet"] != "claude-sonnet-5-20260101" {
		t.Errorf("dated id truncated: %v", d)
	}
}

// A successful probe is cached for claudeCacheTTL so the billed `claude -p` call
// runs at most weekly; a missing or stale cache is not "fresh" (the caller
// re-probes), and a stale cache still carries usable ids as a fallback.
func TestClaudeCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-models.json")

	// Missing file: zero cache — no ids, not fresh.
	if c := readClaudeCache(path); len(c.IDs) != 0 || !c.ProbedAt.IsZero() {
		t.Errorf("missing cache = %+v, want zero", c)
	}

	ids := map[string]string{"opus": "claude-opus-4-8", "sonnet": "claude-sonnet-5"}
	if err := writeClaudeCache(path, ids); err != nil {
		t.Fatal(err)
	}
	got := readClaudeCache(path)
	if got.IDs["opus"] != "claude-opus-4-8" || got.IDs["sonnet"] != "claude-sonnet-5" {
		t.Errorf("round-trip ids = %v", got.IDs)
	}
	// A just-written cache is fresh.
	if time.Since(got.ProbedAt) >= claudeCacheTTL {
		t.Errorf("fresh cache read as stale: probed_at %v", got.ProbedAt)
	}

	// Hand-write a stale cache (8 days old): ids still present, but past the TTL.
	stale := claudeCache{ProbedAt: time.Now().Add(-8 * 24 * time.Hour).UTC(), IDs: ids}
	b, _ := json.Marshal(stale)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	sc := readClaudeCache(path)
	if len(sc.IDs) == 0 {
		t.Error("stale cache dropped its ids")
	}
	if time.Since(sc.ProbedAt) < claudeCacheTTL {
		t.Error("8-day-old cache should be past the 7-day TTL")
	}
}

// writeScript drops an executable /bin/sh stub named `name` in dir and returns
// its path. Agent discovery tests use only these local stubs: the suite must
// never execute whichever real agent CLIs happen to be installed on the host.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseCodexModels(t *testing.T) {
	// A realistic catalog: list-visible models each carry an effort menu, and
	// those menus genuinely differ between models (sol advertises ultra, 5.5
	// tops out at xhigh). The hidden model must contribute nothing.
	out := []byte(`{"models":[
		{"slug":"gpt-5.6-sol","display_name":"GPT-5.6-Sol","visibility":"list",
		 "default_reasoning_level":"medium",
		 "supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}]},
		{"slug":"gpt-5.5","visibility":"list",
		 "default_reasoning_level":"xhigh",
		 "supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"}]},
		{"slug":"codex-auto-review","visibility":"hide",
		 "supported_reasoning_levels":[{"effort":"low"}]}
	]}`)
	info := parseCodexModels(out)
	if want := []string{"gpt-5.6-sol", "gpt-5.5"}; !reflect.DeepEqual(info.Models, want) {
		t.Fatalf("models = %v, want %v (hidden must be dropped, order preserved)", info.Models, want)
	}
	sol, ok := info.Efforts["gpt-5.6-sol"]
	if !ok {
		t.Fatal("gpt-5.6-sol carries no effort menu")
	}
	if sol.Default != "medium" {
		t.Errorf("sol default effort = %q, want medium", sol.Default)
	}
	if want := []string{"low", "medium", "high", "xhigh", "max", "ultra"}; !reflect.DeepEqual(sol.Levels, want) {
		t.Errorf("sol levels = %v, want %v", sol.Levels, want)
	}
	if info.Efforts["gpt-5.5"].Default != "xhigh" {
		t.Errorf("5.5 default effort = %q, want xhigh", info.Efforts["gpt-5.5"].Default)
	}
	if _, ok := info.Efforts["codex-auto-review"]; ok {
		t.Error("hidden model leaked into the effort map")
	}
}

func TestParseCodexModelsMalformed(t *testing.T) {
	// Malformed, empty-object, and empty-catalog inputs all collapse to a zero
	// agentInfo so detectAgents' fallback takes over — the call site never
	// distinguishes broken JSON from an unusable catalog.
	for _, in := range []string{`not json`, `{}`, `{"models":[]}`} {
		info := parseCodexModels([]byte(in))
		if len(info.Models) != 0 || info.Efforts != nil {
			t.Errorf("input %q: want zero agentInfo, got %+v", in, info)
		}
	}
}

func TestParseCodexModelsDefensive(t *testing.T) {
	// Unknown fields ignored; empty/missing slug and missing visibility dropped;
	// a model with no reasoning levels yields no effort entry (the UI then falls
	// back to the static per-agent effort list for it).
	out := []byte(`{"models":[
		{"slug":"gpt-5.6-sol","visibility":"list","future":{"x":1},"supported_reasoning_levels":[{"effort":"low"}]},
		{"slug":"","visibility":"list"},
		{"visibility":"list"},
		{"slug":"hidden-no-visibility"},
		{"slug":"gpt-5.6-luna","visibility":"list"}
	]}`)
	info := parseCodexModels(out)
	if want := []string{"gpt-5.6-sol", "gpt-5.6-luna"}; !reflect.DeepEqual(info.Models, want) {
		t.Fatalf("models = %v, want %v", info.Models, want)
	}
	if _, ok := info.Efforts["gpt-5.6-luna"]; ok {
		t.Error("a model with no supported_reasoning_levels must have no effort entry")
	}
	if e := info.Efforts["gpt-5.6-sol"]; len(e.Levels) != 1 || e.Levels[0] != "low" {
		t.Errorf("sol effort menu = %+v, want levels [low]", e)
	}
}

func TestPrependModel(t *testing.T) {
	// Configured model floats to the front, deduped, nothing else dropped.
	got := prependModel("gpt-5.6-terra", []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"})
	if want := []string{"gpt-5.6-terra", "gpt-5.6-sol", "gpt-5.6-luna"}; !reflect.DeepEqual(got, want) {
		t.Errorf("prependModel = %v, want %v", got, want)
	}
	in := []string{"a", "b"}
	if got := prependModel("", in); !reflect.DeepEqual(got, in) {
		t.Errorf("empty configured must pass through, got %v", got)
	}
	if got := prependModel("x", []string{"a"}); !reflect.DeepEqual(got, []string{"x", "a"}) {
		t.Errorf("absent configured must be prepended, got %v", got)
	}
}

func TestCodexModelsFromCLI(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.codex/config.toml -> no configured-model prepend
	dir := t.TempDir()
	fixture := filepath.Join(dir, "models.json")
	if err := os.WriteFile(fixture, []byte(`{"models":[
		{"slug":"gpt-5.6-sol","visibility":"list","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"}]},
		{"slug":"gpt-5.6-luna","visibility":"list"},
		{"slug":"codex-auto-review","visibility":"hide"}
	]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// The stub asserts the exact subcommand and, like the real CLI, prints a
	// warning to stderr that Output() must ignore.
	bin := writeScript(t, dir, "codex",
		`if [ "$1" != "debug" ] || [ "$2" != "models" ]; then echo "bad args: $*" >&2; exit 2; fi
echo "warning: debug command" >&2
cat `+fixture)
	info := codexModelsFromCLI(context.Background(), bin)
	if want := []string{"gpt-5.6-sol", "gpt-5.6-luna"}; !reflect.DeepEqual(info.Models, want) {
		t.Fatalf("models = %v, want %v", info.Models, want)
	}
	if info.Efforts["gpt-5.6-sol"].Default != "medium" {
		t.Errorf("per-model efforts not carried through the probe: %+v", info.Efforts)
	}
}

func TestCodexModelsFromCLIFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := map[string]string{
		"nonzero":   `exit 3`,
		"malformed": `echo "not json"`,
		"timeout":   `sleep 5`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			bin := writeScript(t, t.TempDir(), "codex", body)
			ctx := context.Background()
			if name == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			info := codexModelsFromCLI(ctx, bin)
			if len(info.Models) != 0 || info.Efforts != nil {
				t.Errorf("want zero agentInfo on failure, got %+v", info)
			}
		})
	}
}

func TestDetectAgents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := t.TempDir()
	writeScript(t, binDir, "claude", `exit 0`)
	writeScript(t, binDir, "codex", `exit 1`)
	writeScript(t, binDir, "pi-acp", `exit 0`)
	writeScript(t, binDir, "pi", `
if [ "$1" != "--list-models" ]; then exit 2; fi
printf '%s\n' 'provider model' 'anthropic claude-sonnet-4-5'
`)
	writeScript(t, binDir, "opencode", `
if [ "$1" != "models" ]; then exit 2; fi
printf '%s\n' 'openai/gpt-5.5' 'anthropic/claude-sonnet-4-5'
`)
	writeScript(t, binDir, "grok", `
if [ "$1" != "models" ]; then exit 2; fi
printf '%s\n' 'Default model: grok-4.5' '* grok-code-fast-1' '* grok-4.5 (default)'
`)
	t.Setenv("PATH", binDir)

	cacheDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := `{"models":{"grok-4.5":{"info":{"supports_reasoning_effort":true,"reasoning_effort":"medium","reasoning_efforts":[{"value":"low"},{"value":"medium","default":true},{"value":"high"}]}}}}`
	if err := os.WriteFile(filepath.Join(cacheDir, "models_cache.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}

	agents := probeAgents(harnesses)
	if got := len(agents); got != len(harnesses) {
		t.Fatalf("agents = %v, want all %d stubbed harnesses", agents, len(harnesses))
	}
	if got := agents["pi"].Models; !reflect.DeepEqual(got, []string{"anthropic/claude-sonnet-4-5"}) {
		t.Errorf("pi models = %v", got)
	}
	if got := agents["opencode"].Models; !reflect.DeepEqual(got, []string{"openai/gpt-5.5", "anthropic/claude-sonnet-4-5"}) {
		t.Errorf("opencode models = %v", got)
	}
	grok := agents["grok"]
	if want := []string{"grok-4.5", "grok-code-fast-1"}; !reflect.DeepEqual(grok.Models, want) {
		t.Errorf("grok models = %v, want %v", grok.Models, want)
	}
	if got := grok.Efforts["grok-4.5"]; got.Default != "medium" || !reflect.DeepEqual(got.Levels, []string{"low", "medium", "high"}) {
		t.Errorf("grok-4.5 efforts = %+v", got)
	}
	if got := grok.Efforts["grok-code-fast-1"]; got.Default != "high" {
		t.Errorf("grok-code-fast-1 fallback efforts = %+v", got)
	}
	for name, info := range agents {
		if info.Models == nil {
			t.Errorf("agent %q has nil model list", name)
		}
	}
}

func TestCodexManagerConflictAndPending(t *testing.T) {
	// Verify the codexManager adapter methods (Conflict, Pending) are correct
	// (finding 78 — the adapter methods were 0% covered).
	cm := codexManager{codex.NewManager(t.TempDir())}

	// Conflict must return true for the sentinel errors.
	if !cm.Conflict(codex.ErrNoSession) {
		t.Error("Conflict(ErrNoSession) = false, want true")
	}
	if !cm.Conflict(codex.ErrNotAlive) {
		t.Error("Conflict(ErrNotAlive) = false, want true")
	}
	if !cm.Conflict(codex.ErrTurnActive) {
		t.Error("Conflict(ErrTurnActive) = false, want true")
	}
	if !cm.Conflict(codex.ErrNoPending) {
		t.Error("Conflict(ErrNoPending) = false, want true")
	}
	if cm.Conflict(errors.New("random")) {
		t.Error("Conflict(random) = true, want false")
	}

	// Pending on a node with no session must return ok=false.
	if p, ok := cm.Pending("ghost"); ok {
		t.Errorf("Pending(ghost) = (%+v, true), want ok=false", p)
	}
}
