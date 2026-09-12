package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp/codex"
	"codeberg.org/chrberger/scimux/internal/acp/muse"
)

func TestAgentCommandPiOpencode(t *testing.T) {
	pi := &Node{Agent: "pi", Model: "mistral/devstral-latest", Prompt: "hello"}
	if got, _ := agentCommand(pi, nil); got != `pi --model 'mistral/devstral-latest' 'hello'` {
		t.Errorf("pi cmd = %s", got)
	}
	// opencode takes the first prompt as a flag, not a positional argument.
	oc := &Node{Agent: "opencode", Model: "openai/gpt-5.5", Prompt: "hello"}
	if got, _ := agentCommand(oc, nil); got != `opencode --model 'openai/gpt-5.5' --prompt 'hello'` {
		t.Errorf("opencode cmd = %s", got)
	}
	bare := &Node{Agent: "pi", Prompt: "p"}
	if got, _ := agentCommand(bare, nil); got != `pi 'p'` {
		t.Errorf("bare pi cmd = %s", got)
	}
	// grok is ACP in production; agentCommand is only a legacy tmux fallback.
	g := &Node{Agent: "grok", Model: "grok-4.5", Effort: "low", Prompt: "hello"}
	if got, _ := agentCommand(g, nil); got != `grok -m 'grok-4.5' --reasoning-effort 'low' 'hello'` {
		t.Errorf("grok cmd = %s", got)
	}
}

func TestParseGrokModels(t *testing.T) {
	// `grok models` marks the default with * and every other id with -;
	// older CLIs used * for the whole list. Both markers are catalog lines.
	cases := []struct {
		name    string
		out     string
		wantDef string
		want    []string
	}{
		{
			name:    "current mixed bullets",
			out:     "You are logged in with grok.com.\n\nDefault model: grok-4.6\n\nAvailable models:\n  * grok-4.6 (default)\n  - grok-4.5\n",
			wantDef: "grok-4.6",
			want:    []string{"grok-4.6", "grok-4.5"},
		},
		{
			name:    "legacy all asterisks",
			out:     "You are logged in with grok.com.\n\nDefault model: grok-4.5\n\nAvailable models:\n  * grok-4.5 (default)\n  * grok-code-fast-1\n",
			wantDef: "grok-4.5",
			want:    []string{"grok-4.5", "grok-code-fast-1"},
		},
		{
			name:    "dash only",
			out:     "Default model: grok-4.5\n- grok-4.5\n- grok-code-fast-1\n",
			wantDef: "grok-4.5",
			want:    []string{"grok-4.5", "grok-code-fast-1"},
		},
		{
			name: "duplicate star and dash",
			out:  "* grok-4.6 (default)\n- grok-4.6\n- grok-4.5\n",
			want: []string{"grok-4.6", "grok-4.5"},
		},
		{
			name:    "banner and headings are not models",
			out:     "You are logged in with grok.com.\nAvailable models:\nDefault model: grok-4.6\n",
			wantDef: "grok-4.6",
		},
		{
			name: "empty bullet skipped",
			out:  "*   \n- \n* grok-4.6\n",
			want: []string{"grok-4.6"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def, models := parseGrokModels(tc.out)
			if def != tc.wantDef {
				t.Errorf("default = %q, want %q", def, tc.wantDef)
			}
			if len(tc.want) == 0 {
				if len(models) != 0 {
					t.Errorf("models = %v, want nil/empty", models)
				}
				return
			}
			if !reflect.DeepEqual(models, tc.want) {
				t.Errorf("models = %v, want %v", models, tc.want)
			}
		})
	}
}

func TestGrokModelsFallback(t *testing.T) {
	info := grokModelsFallback()
	if !reflect.DeepEqual(info.Models, []string{"grok-4.5"}) {
		t.Fatalf("Models = %v, want [grok-4.5]", info.Models)
	}
	e, ok := info.Efforts["grok-4.5"]
	if !ok {
		t.Fatal("missing effort menu for grok-4.5")
	}
	want := grokDefaultEfforts()
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("effort = %+v, want %+v", e, want)
	}

	// Callers may mutate returned maps/slices; a later call must stay independent.
	info.Models[0] = "mutated"
	levels := info.Efforts["grok-4.5"].Levels
	levels[0] = "mutated"
	info.Efforts["extra"] = modelEffort{}
	again := grokModelsFallback()
	if !reflect.DeepEqual(again.Models, []string{"grok-4.5"}) {
		t.Fatalf("second Models = %v after mutation, want [grok-4.5]", again.Models)
	}
	if !reflect.DeepEqual(again.Efforts["grok-4.5"], want) {
		t.Fatalf("second effort = %+v after mutation, want %+v", again.Efforts["grok-4.5"], want)
	}
	if _, ok := again.Efforts["extra"]; ok {
		t.Fatal("second call shared Efforts map with first")
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

// The model cache is keyed on the CLI's own version, not on time alone. New
// models arrive with a new claude build, so a matching version is the real
// evidence that a stored answer still describes reality; the TTL underneath it
// is only a backstop for a long-lived scimux process.
func TestClaudeCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-models.json")

	// Missing file: zero cache — no ids, no version.
	if c := readClaudeCache(path); len(c.IDs) != 0 || c.Version != "" {
		t.Errorf("missing cache = %+v, want zero", c)
	}

	ids := map[string]string{"opus": "claude-opus-5", "sonnet": "claude-sonnet-5"}
	if err := writeClaudeCache(path, "2.1.267", ids); err != nil {
		t.Fatal(err)
	}
	got := readClaudeCache(path)
	if got.IDs["opus"] != "claude-opus-5" || got.IDs["sonnet"] != "claude-sonnet-5" {
		t.Errorf("round-trip ids = %v", got.IDs)
	}
	if got.Version != "2.1.267" {
		t.Errorf("round-trip version = %q", got.Version)
	}
	if !claudeCacheUsable(got, "2.1.267", time.Now()) {
		t.Error("a just-written cache from this very CLI build is not usable")
	}
}

// The cases that must each independently reject a stored answer. A model list
// is cheap to rebuild (the probe spends no tokens) and expensive to get wrong:
// a wrong id fails at the API in the user's face.
func TestClaudeCacheUsability(t *testing.T) {
	ids := map[string]string{"opus": "claude-opus-5"}
	now := time.Now()
	fresh := claudeCache{Version: "2.1.267", ProbedAt: now.Add(-time.Minute), IDs: ids}

	if claudeCacheUsable(fresh, "2.1.268", now) {
		t.Error("an upgraded CLI must invalidate the cache: a new build is where new models arrive")
	}
	old := fresh
	old.ProbedAt = now.Add(-claudeCacheTTL - time.Minute)
	if claudeCacheUsable(old, "2.1.267", now) {
		t.Error("the TTL backstop must still expire a same-version cache: scimux can outlive a release")
	}
	empty := fresh
	empty.IDs = nil
	if claudeCacheUsable(empty, "2.1.267", now) {
		t.Error("a cache with no ids is not an answer")
	}
	// An unreadable CLI version is not a mismatch. It means the question could
	// not be asked, and a stored answer within the backstop still beats none.
	if !claudeCacheUsable(fresh, "", now) {
		t.Error("an unknown CLI version must fall back to the TTL, not discard the cache")
	}
	unversioned := claudeCache{ProbedAt: now.Add(-time.Minute), IDs: ids}
	if claudeCacheUsable(unversioned, "2.1.267", now) {
		t.Error("a cache written before versioning cannot claim to match this build")
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
printf '%s\n' 'Default model: grok-4.5' '- grok-code-fast-1' '* grok-4.5 (default)'
`)
	writeScript(t, binDir, "muse", `exit 0`)
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
	if got := agents["muse"].Models; got == nil || len(got) != 0 {
		t.Errorf("muse models = %v, want empty with no static fallback", got)
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

// The model probe shells out to `claude -p`, and claude writes a transcript
// into ~/.claude/projects/<slug of its cwd>/. Inheriting scimux's own cwd
// therefore drops a throwaway probe session into whatever project scimux was
// started from; when that is a supervised node's directory the relink
// machinery can adopt the probe file and the enumeration prompt surfaces in a
// live chat (observed during E2E testing). The probe must always run somewhere
// no node can own.
func TestClaudeProbeWorkdirIsNeverTheInheritedCwd(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("creates and returns the dedicated dir", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "probe")
		got := claudeProbeWorkdir(dir)
		if got != dir {
			t.Fatalf("workdir = %q, want %q", got, dir)
		}
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("probe dir not created: %v", err)
		}
		if !fi.IsDir() {
			t.Fatal("probe path is not a directory")
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Fatalf("probe dir perm = %o, want 700", perm)
		}
	})

	t.Run("falls back to a temp dir, never the cwd", func(t *testing.T) {
		// An unusable path: a regular file stands where the dir should go.
		blocked := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, dir := range []string{"", filepath.Join(blocked, "probe")} {
			got := claudeProbeWorkdir(dir)
			if got == "" || got == cwd {
				t.Fatalf("workdir(%q) = %q, must not be empty or the inherited cwd", dir, got)
			}
			if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
				t.Fatalf("workdir(%q) = %q is not a usable directory (%v)", dir, got, err)
			}
		}
	})
}

func TestProbeMuseCatalogFailsClosedWithoutBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := probeMuseCatalog(context.Background())
	if err == nil {
		t.Fatal("production catalog probe must fail closed when muse is not on PATH")
	}
	_, err = probeMuseCatalog(nil)
	if err == nil {
		t.Fatal("nil context must still fail closed without spawning a turn")
	}
}

type museCatalogPipe struct {
	stdin     *io.PipeWriter
	stdout    *io.PipeReader
	serverIn  *io.PipeReader
	serverOut *io.PipeWriter
	once      sync.Once
	closes    atomic.Int32
}

func newMuseCatalogPipe() *museCatalogPipe {
	serverIn, stdin := io.Pipe()
	stdout, serverOut := io.Pipe()
	return &museCatalogPipe{stdin: stdin, stdout: stdout, serverIn: serverIn, serverOut: serverOut}
}

func (p *museCatalogPipe) Stdin() io.WriteCloser { return p.stdin }
func (p *museCatalogPipe) Stdout() io.Reader     { return p.stdout }
func (p *museCatalogPipe) Close() error {
	p.once.Do(func() {
		p.closes.Add(1)
		_ = p.stdin.Close()
		_ = p.stdout.Close()
		_ = p.serverIn.Close()
		_ = p.serverOut.Close()
	})
	return nil
}

func TestProbeMuseCatalogWithFakeTransportInitializesListsAndCloses(t *testing.T) {
	tr := newMuseCatalogPipe()
	methods := make(chan string, 4)
	serverDone := make(chan error, 1)
	go func() {
		scan := bufio.NewScanner(tr.serverIn)
		for scan.Scan() {
			var frame struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(scan.Bytes(), &frame); err != nil {
				serverDone <- err
				return
			}
			methods <- frame.Method
			if len(frame.ID) == 0 {
				continue
			}
			var result string
			switch frame.Method {
			case "initialize":
				result = fmt.Sprintf(`{"serverInfo":{"name":"muse","version":%q},"schema":{"version":%d,"fingerprint":%q},"capabilities":{}}`, muse.ObservedMuseVersion, muse.ObservedMSPSchema, muse.PinnedFingerprint)
			case "model/list":
				result = `{"source":"synthetic","models":[{"id":"std-synthetic","label":"Synthetic","isDefault":true}]}`
			default:
				serverDone <- fmt.Errorf("unexpected request method %q", frame.Method)
				return
			}
			if _, err := fmt.Fprintf(tr.serverOut, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", frame.ID, result); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- scan.Err()
	}()
	spawn := func(context.Context, string) (muse.Transport, error) { return tr, nil }
	models, err := probeMuseCatalogWithSpawn(context.Background(), spawn)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(models) != 1 || models[0].ID != "std-synthetic" {
		t.Fatalf("models = %+v", models)
	}
	if tr.closes.Load() != 1 {
		t.Fatalf("transport closes = %d, want 1", tr.closes.Load())
	}
	close(methods)
	var got []string
	for method := range methods {
		got = append(got, method)
	}
	if !reflect.DeepEqual(got, []string{"initialize", "initialized", "model/list"}) {
		t.Fatalf("wire methods = %v; probe must initialize and list without session or turn", got)
	}
	if err := <-serverDone; err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("fake server: %v", err)
	}
}

func TestHandleAgentsMuseCatalogIsAdditive(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	base := map[string]agentInfo{"muse": {Models: []string{}}}
	a.agentCatalog = func() map[string]agentInfo { return base }
	ctxLim, outLim := 100000, 8192
	a.museCatalog = func(context.Context) ([]muse.Model, error) {
		return []muse.Model{
			{ID: "std-1", Label: "Standard One", IsDefault: true, ContextLimit: &ctxLim, OutputLimit: &outLim},
			{ID: "disc-1", Label: "Discounted One"},
			{ID: "unk-1", Label: "Unknown One"},
			{ID: "", Label: "dropped"},
			{ID: "std-1", Label: "Standard One", IsDefault: true, ContextLimit: &ctxLim, OutputLimit: &outLim},
		}, nil
	}
	a.museClassify = func(id string) string {
		switch id {
		case "std-1":
			return museTierStandard
		case "disc-1":
			return museTierDiscounted
		}
		return museTierUnknown
	}

	rec := httptest.NewRecorder()
	a.handleAgents(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var out map[string]agentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	info := out["muse"]
	if !reflect.DeepEqual(info.Models, []string{"std-1", "disc-1"}) {
		t.Fatalf("models = %v, want only selectable known-tier ids", info.Models)
	}
	if len(info.MuseModels) != 3 {
		t.Fatalf("muse_models = %+v, want 3 rows", info.MuseModels)
	}
	std := info.MuseModels[0]
	if std.ID != "std-1" || !std.Default || std.Tier != museTierStandard || !std.Launchable {
		t.Fatalf("standard row = %+v", std)
	}
	if std.ContextLimit == nil || *std.ContextLimit != 100000 || std.OutputLimit == nil || *std.OutputLimit != 8192 {
		t.Fatalf("limits fabricated or dropped: %+v", std)
	}
	disc := info.MuseModels[1]
	if disc.Tier != museTierDiscounted || !disc.Launchable || disc.Default {
		t.Fatalf("discounted row = %+v", disc)
	}
	unk := info.MuseModels[2]
	if unk.Tier != museTierUnknown || unk.Launchable {
		t.Fatalf("unknown row must be reported and not launchable: %+v", unk)
	}
	if !reflect.DeepEqual(base["muse"].Models, []string{}) || len(base["muse"].MuseModels) != 0 {
		t.Fatalf("handler mutated shared discovery data: %+v", base["muse"])
	}
}

func TestHandleAgentsDoesNotAdvertiseAbsentMuse(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.agentCatalog = func() map[string]agentInfo {
		return map[string]agentInfo{"codex": {Models: []string{"gpt-test"}}}
	}
	a.museCatalog = func(context.Context) ([]muse.Model, error) {
		return []muse.Model{{ID: "std-1", IsDefault: true}}, nil
	}
	a.museClassify = func(string) string { return museTierStandard }
	rec := httptest.NewRecorder()
	a.handleAgents(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	var out map[string]agentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["muse"]; ok {
		t.Fatalf("absent Muse binary was advertised: %+v", out["muse"])
	}
}

func TestHandleAgentsMuseConcurrentRequestsKeepDiscoveryCachePrivate(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	base := map[string]agentInfo{"muse": {Models: []string{}}}
	a.agentCatalog = func() map[string]agentInfo { return base }
	a.museCatalog = func(context.Context) ([]muse.Model, error) {
		return []muse.Model{{ID: "std-1", IsDefault: true}}, nil
	}
	a.museClassify = func(string) string { return museTierStandard }
	const callers = 24
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			a.handleAgents(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
			if rec.Code != http.StatusOK {
				errs <- errors.New("non-200 response")
				return
			}
			var out map[string]agentInfo
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				errs <- err
				return
			}
			if !reflect.DeepEqual(out["muse"].Models, []string{"std-1"}) {
				errs <- errors.New("request lost launchable Muse model")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(base["muse"].Models) != 0 || len(base["muse"].MuseModels) != 0 {
		t.Fatalf("shared discovery cache mutated: %+v", base["muse"])
	}
}

func installMuseAgentBase(a *app) {
	a.agentCatalog = func() map[string]agentInfo {
		return map[string]agentInfo{"muse": {Models: []string{}}}
	}
}

func TestMuseModelConflictLimitPointers(t *testing.T) {
	n := 1
	a := muse.Model{ID: "x", Label: "L", ContextLimit: &n}
	b := muse.Model{ID: "x", Label: "L"}
	if !museModelConflict(a, b) {
		t.Fatal("nil vs set limit must conflict")
	}
	if museModelConflict(a, a) {
		t.Fatal("identical pointers/values must not conflict")
	}
	m := 1
	c := muse.Model{ID: "x", Label: "L", ContextLimit: &m}
	if museModelConflict(a, c) {
		t.Fatal("equal limit values must not conflict")
	}
	releaseA, releaseB := "2026-01-01", "2026-01-02"
	base := muse.Model{ID: "x", Name: "n", Label: "L", Source: "s", ProfileID: "p", ReleaseDate: &releaseA}
	for label, changed := range map[string]muse.Model{
		"name":         {ID: "x", Name: "other", Label: "L", Source: "s", ProfileID: "p", ReleaseDate: &releaseA},
		"source":       {ID: "x", Name: "n", Label: "L", Source: "other", ProfileID: "p", ReleaseDate: &releaseA},
		"profile":      {ID: "x", Name: "n", Label: "L", Source: "s", ProfileID: "other", ReleaseDate: &releaseA},
		"release date": {ID: "x", Name: "n", Label: "L", Source: "s", ProfileID: "p", ReleaseDate: &releaseB},
	} {
		if !museModelConflict(base, changed) {
			t.Errorf("duplicate with conflicting %s remained launchable", label)
		}
	}
}

func TestMuseDiscountedAndConflictingRowsAreNeverDefaultsOrLegacySelectable(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.agentCatalog = func() map[string]agentInfo {
		return map[string]agentInfo{"muse": {Models: []string{}}}
	}
	a.museCatalog = func(context.Context) ([]muse.Model, error) {
		return []muse.Model{
			{ID: "disc-1", Label: "Discounted", IsDefault: true},
			{ID: "std-bad", Label: "A", IsDefault: true},
			{ID: "std-bad", Label: "B", IsDefault: true},
		}, nil
	}
	a.museClassify = func(id string) string {
		if id == "disc-1" {
			return museTierDiscounted
		}
		return museTierStandard
	}
	rec := httptest.NewRecorder()
	a.handleAgents(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	var out map[string]agentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	info := out["muse"]
	if !reflect.DeepEqual(info.Models, []string{"disc-1"}) {
		t.Fatalf("legacy selectable models = %v, want only the launchable discounted id", info.Models)
	}
	if len(info.MuseModels) != 2 {
		t.Fatalf("rows = %+v", info.MuseModels)
	}
	for _, row := range info.MuseModels {
		if row.Default {
			t.Errorf("non-Standard-default row claimed default: %+v", row)
		}
	}
}

func TestHandleAgentsMuseConflictingDuplicateNotLaunchable(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	installMuseAgentBase(a)
	a.museCatalog = func(context.Context) ([]muse.Model, error) {
		return []muse.Model{
			{ID: "std-1", Label: "A", IsDefault: true},
			{ID: "std-1", Label: "B", IsDefault: true},
		}, nil
	}
	a.museClassify = func(string) string { return museTierStandard }
	rec := httptest.NewRecorder()
	a.handleAgents(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	var out map[string]agentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	rows := out["muse"].MuseModels
	if len(rows) != 1 || rows[0].Launchable {
		t.Fatalf("conflicting duplicate became launchable: %+v", rows)
	}
}

func TestHandleAgentsMuseEmptyCatalogAndProbeFailure(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	installMuseAgentBase(a)
	a.museCatalog = func(context.Context) ([]muse.Model, error) { return nil, nil }
	a.museClassify = func(string) string { return museTierStandard }
	rec := httptest.NewRecorder()
	a.handleAgents(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	var out map[string]agentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if got := out["muse"].Models; got == nil || len(got) != 0 {
		t.Fatalf("empty catalog models = %v, want []", got)
	}
	if len(out["muse"].MuseModels) != 0 {
		t.Fatalf("empty catalog rows = %+v", out["muse"].MuseModels)
	}

	a.museCatalog = func(context.Context) ([]muse.Model, error) { return nil, errors.New("probe down") }
	rec = httptest.NewRecorder()
	a.handleAgents(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if rec.Code != 200 {
		t.Fatalf("probe failure must not 5xx the agents list: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out["muse"].MuseModels) != 0 {
		t.Fatalf("probe failure leaked rows: %+v", out["muse"].MuseModels)
	}
}

func TestMuseTierOfRejectsNonExactClassifierValues(t *testing.T) {
	a := &app{museClassify: func(string) string { return "Standard" }}
	if got := a.museTierOf("std-1"); got != museTierUnknown {
		t.Fatalf("title-case classifier = %q, want unknown", got)
	}
	a.museClassify = func(string) string { return "STANDARD" }
	if got := a.museTierOf("std-1"); got != museTierUnknown {
		t.Fatalf("uppercase classifier = %q, want unknown", got)
	}
	a.museClassify = func(id string) string { return id }
	if got := a.museTierOf("spark-code"); got != museTierUnknown {
		t.Fatalf("model-id echo = %q, want unknown", got)
	}
	a.museClassify = func(string) string { return museTierDiscounted }
	if got := a.museTierOf("x"); got != museTierDiscounted {
		t.Fatalf("exact discounted = %q", got)
	}
}

func TestMuseViewsCopiesLimitPointers(t *testing.T) {
	n, outn := 100, 8
	models := []muse.Model{{ID: "std-1", Label: "Std", IsDefault: true, ContextLimit: &n, OutputLimit: &outn}}
	a := &app{museClassify: func(string) string { return museTierStandard }}
	views := a.museViews(models)
	if len(views) != 1 || views[0].ContextLimit == nil || views[0].OutputLimit == nil {
		t.Fatalf("%+v", views)
	}
	*views[0].ContextLimit = 1
	*views[0].OutputLimit = 2
	if n != 100 || outn != 8 {
		t.Fatalf("museViews aliased catalog pointers: ctx=%d out=%d", n, outn)
	}
}

func TestHandleAgentsNonExactClassifierIsUnlaunchable(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	installMuseAgentBase(a)
	a.museCatalog = func(context.Context) ([]muse.Model, error) {
		return []muse.Model{{ID: "std-1", Label: "Looks standard", IsDefault: true}}, nil
	}
	a.museClassify = func(string) string { return "Standard" }
	rec := httptest.NewRecorder()
	a.handleAgents(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	var out map[string]agentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	rows := out["muse"].MuseModels
	if len(rows) != 1 || rows[0].Tier != museTierUnknown || rows[0].Launchable || rows[0].Default {
		t.Fatalf("non-exact classifier must fail closed: %+v", rows)
	}
	if len(out["muse"].Models) != 0 {
		t.Fatalf("legacy selectable = %v, want empty", out["muse"].Models)
	}
}

func TestHandleAgentsMuseProductionAuthorityIsEmpty(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	installMuseAgentBase(a)
	a.museCatalog = func(context.Context) ([]muse.Model, error) {
		return []muse.Model{{ID: "std-1", Label: "Looks standard", IsDefault: true}}, nil
	}
	// Production classifier is unset/empty.
	rec := httptest.NewRecorder()
	a.handleAgents(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	var out map[string]agentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	rows := out["muse"].MuseModels
	if len(rows) != 1 || rows[0].Tier != museTierUnknown || rows[0].Launchable {
		t.Fatalf("production empty authority must report unknown/unlaunchable: %+v", rows)
	}
}

func TestHandleAgentsOtherAgentsKeepShape(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.agentCatalog = func() map[string]agentInfo {
		return map[string]agentInfo{
			"muse":   {Models: []string{}},
			"claude": {Models: []string{"sonnet"}},
			"codex":  {Models: []string{"gpt-test"}},
		}
	}
	a.museCatalog = func(context.Context) ([]muse.Model, error) {
		return []muse.Model{{ID: "std-1", IsDefault: true}}, nil
	}
	a.museClassify = func(string) string { return museTierStandard }
	rec := httptest.NewRecorder()
	a.handleAgents(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	var raw map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["muse"]["muse_models"]; !ok {
		t.Fatal("muse row missing muse_models")
	}
	for agent, info := range raw {
		if agent == "muse" {
			continue
		}
		if _, ok := info["muse_models"]; ok {
			t.Fatalf("agent %q unexpectedly carries muse_models", agent)
		}
	}
}
