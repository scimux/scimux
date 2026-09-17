package app

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// cursorListModelsFixture is a hand-written stand-in for `cursor-agent
// --list-models`, not a capture. It is deliberately small but reproduces every
// shape the real grammar has: a bare `auto` row, a family with a suffix-less
// (CLI-default) member, the `-fast` variant that forces a second row, a
// `-thinking` base, a two-word `extra-high` level that must win over `high`,
// a family whose labels never omit their own level, and the trailing tip line.
//
// The one id that matters most is `example-opus-thinking-high-fast`: joining
// its row (`example-opus-thinking-fast`) to its level (`high`) yields
// `example-opus-thinking-fast-high`, which cursor does not know. Offered pairs
// must therefore be looked up, never composed.
const cursorListModelsFixture = `Available models

auto - Auto (current, default)
example-codex-low - Example Codex Low
example-codex - Example Codex
example-codex-high - Example Codex High
example-codex-xhigh - Example Codex Extra High
example-codex-fast - Example Codex Fast
example-codex-high-fast - Example Codex High Fast
example-opus-thinking-high - Example Opus Thinking
example-opus-thinking-xhigh - Example Opus Extra High Thinking
example-opus-thinking-high-fast - Example Opus Thinking Fast
example-opus-thinking-xhigh-fast - Example Opus Extra High Thinking Fast
example-flash-low - Example Flash Low
example-flash-medium - Example Flash Medium
example-flash-high - Example Flash High
example-mini-extra-high - Example Mini Extra High
example-plain - Example Plain

Tip: use --model <id> (or /model <id> in interactive mode) to switch.
`

// fixtureIDs is every model id the fixture prints, which is exactly the set a
// resolved launch flag is allowed to draw from.
func fixtureIDs(t *testing.T) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, line := range strings.Split(cursorListModelsFixture, "\n") {
		id, _, ok := strings.Cut(strings.TrimSpace(line), " - ")
		if !ok || id == "" || strings.ContainsAny(id, " :") {
			continue
		}
		ids[id] = true
	}
	if len(ids) != 16 {
		t.Fatalf("fixture id count = %d, want 16", len(ids))
	}
	return ids
}

// The dialog gets model rows, not the CLI's flat id list: a row is the id with
// its effort level lifted out, so the three fields the user already knows
// (agent, model, effort) keep their meaning.
func TestParseCursorModelsLiftsEffortOutOfTheModelID(t *testing.T) {
	info := parseCursorModels(cursorListModelsFixture)
	want := []string{
		"auto", "example-codex", "example-codex-fast", "example-opus-thinking",
		"example-opus-thinking-fast", "example-flash", "example-mini", "example-plain",
	}
	if !reflect.DeepEqual(info.Models, want) {
		t.Fatalf("models = %v, want %v", info.Models, want)
	}
	for _, c := range []struct {
		model  string
		levels []string
		dflt   string
		req    bool
	}{
		// A suffix-less member is the CLI's own default level, and it is
		// reachable by leaving effort blank — so no level is marked default.
		{"example-codex", []string{"low", "high", "xhigh"}, "", false},
		{"example-codex-fast", []string{"high"}, "", false},
		// No suffix-less member: the row's default is the level whose label
		// omits its own level word, which is how the CLI renders it.
		{"example-opus-thinking", []string{"high", "xhigh"}, "high", true},
		{"example-opus-thinking-fast", []string{"high", "xhigh"}, "high", true},
		// No label singles out a default, but blank effort still has to land
		// on a real id, so the level it lands on is published as the default
		// rather than being applied silently behind an empty select.
		{"example-flash", []string{"low", "medium", "high"}, "low", true},
		// The two-word level must beat the one-word suffix it ends with, and a
		// row with exactly one level has nothing else blank effort could mean.
		{"example-mini", []string{"extra-high"}, "extra-high", true},
	} {
		got := info.Efforts[c.model]
		if !reflect.DeepEqual(got.Levels, c.levels) || got.Default != c.dflt || got.Required != c.req {
			t.Errorf("efforts[%s] = %+v, want levels %v default %q required %v", c.model, got, c.levels, c.dflt, c.req)
		}
	}
	// Rows with a single, level-free id offer no menu at all; the browser must
	// not fall back to a generic low/medium/high that cursor would reject.
	for _, model := range []string{"auto", "example-plain"} {
		if _, ok := info.Efforts[model]; ok {
			t.Errorf("efforts[%s] = %+v, want no menu", model, info.Efforts[model])
		}
	}
}

// Every pair the dialog can offer must name an id the CLI actually printed,
// including the blank effort the model select starts on.
func TestCursorModelIndexResolvesEveryOfferedPair(t *testing.T) {
	info := parseCursorModels(cursorListModelsFixture)
	ids := fixtureIDs(t)
	for _, model := range info.Models {
		levels := append([]string{""}, info.Efforts[model].Levels...)
		for _, effort := range levels {
			id, err := resolveCursorModel(info, model, effort)
			if err != nil {
				t.Errorf("resolve(%q, %q): %v", model, effort, err)
				continue
			}
			if !ids[id] {
				t.Errorf("resolve(%q, %q) = %q, which cursor never printed", model, effort, id)
			}
		}
	}
}

// The index exists because the grammar is not a join: -fast trails the effort
// level in the id but leads it in the row.
func TestCursorModelIndexNeverComposesIDs(t *testing.T) {
	info := parseCursorModels(cursorListModelsFixture)
	for _, c := range []struct{ model, effort, want string }{
		{"example-opus-thinking-fast", "high", "example-opus-thinking-high-fast"},
		{"example-opus-thinking-fast", "xhigh", "example-opus-thinking-xhigh-fast"},
		{"example-codex-fast", "high", "example-codex-high-fast"},
		{"example-mini", "extra-high", "example-mini-extra-high"},
		// Blank effort takes the suffix-less id where one exists, and the row's
		// default level otherwise — never a synthesized id.
		{"example-codex", "", "example-codex"},
		{"example-opus-thinking", "", "example-opus-thinking-high"},
		{"example-flash", "", "example-flash-low"},
		{"auto", "", "auto"},
	} {
		got, err := resolveCursorModel(info, c.model, c.effort)
		if err != nil || got != c.want {
			t.Errorf("resolve(%q, %q) = %q, %v; want %q", c.model, c.effort, got, err, c.want)
		}
	}
}

// What the dialog shows and what the process gets must be the same level.
// Blank effort always resolves to some id, so a row whose published default is
// empty has to have a level-free member; otherwise the empty select is hiding
// a level the launch will silently apply.
func TestCursorBlankEffortMatchesThePublishedDefault(t *testing.T) {
	info := parseCursorModels(cursorListModelsFixture)
	for _, model := range info.Models {
		eff, ok := info.Efforts[model]
		if !ok {
			continue
		}
		blank, err := resolveCursorModel(info, model, "")
		if err != nil {
			t.Errorf("resolve(%q, \"\"): %v", model, err)
			continue
		}
		if eff.Required != (eff.Default != "") {
			t.Errorf("efforts[%s] = %+v: required must mark exactly the rows whose blank effort is a level in disguise", model, eff)
		}
		if eff.Default == "" {
			// No default published, so blank effort must be a real level-free
			// id of its own -- not one of the levels in disguise.
			for _, level := range eff.Levels {
				if id, _ := resolveCursorModel(info, model, level); id == blank {
					t.Errorf("efforts[%s] publishes no default, but blank effort launches %q, which is level %q",
						model, blank, level)
				}
			}
			continue
		}
		want, err := resolveCursorModel(info, model, eff.Default)
		if err != nil || want != blank {
			t.Errorf("efforts[%s] default %q resolves to %q, but blank effort launches %q",
				model, eff.Default, want, blank)
		}
	}
}

// A pair outside the catalog must fail here, with a name in it. Cursor's own
// rejection of a bogus --model surfaces as a bare session/new failure.
func TestResolveCursorModelRefusesPairsTheCatalogDoesNotHold(t *testing.T) {
	info := parseCursorModels(cursorListModelsFixture)
	for _, c := range [][2]string{
		{"example-flash", "max"},
		{"example-plain", "high"},
		{"auto", "low"},
		{"no-such-model", ""},
	} {
		if got, err := resolveCursorModel(info, c[0], c[1]); err == nil {
			t.Errorf("resolve(%q, %q) = %q, want refusal", c[0], c[1], got)
		}
	}
	// An exact id typed straight into the API still launches: it is a value the
	// catalog holds, just not a row name.
	if got, err := resolveCursorModel(info, "example-opus-thinking-xhigh-fast", ""); err != nil || got != "example-opus-thinking-xhigh-fast" {
		t.Errorf("exact id passthrough = %q, %v", got, err)
	}
	// No catalog is no evidence about a bare model: cursor validates the flag
	// itself at launch. An effort is different — cursor has no effort flag, so
	// an unresolved one would be dropped on the floor and the node would launch
	// at a level nobody picked. Refuse instead of guessing.
	if got, err := resolveCursorModel(agentInfo{}, "whatever", ""); err != nil || got != "whatever" {
		t.Errorf("catalog-less resolve = %q, %v", got, err)
	}
	if got, err := resolveCursorModel(agentInfo{}, "example-codex", "high"); err == nil {
		t.Errorf("catalog-less resolve with an effort = %q, want refusal", got)
	}
	// Same reasoning with a catalog present but no model named.
	if got, err := resolveCursorModel(info, "", "high"); err == nil {
		t.Errorf("effort without a model = %q, want refusal", got)
	}
	// An empty model stays empty: cursor then picks its own default.
	if got, err := resolveCursorModel(info, "", ""); err != nil || got != "" {
		t.Errorf("blank model resolve = %q, %v", got, err)
	}
}

// The index is a launch-side lookup table. Publishing it would ship the very
// 200-plus flat id list the row decomposition exists to avoid.
func TestCursorIndexNeverReachesTheBrowser(t *testing.T) {
	out, err := json.Marshal(map[string]agentInfo{"cursor": parseCursorModels(cursorListModelsFixture)})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"example-opus-thinking-high-fast", "example-codex-high-fast", "example-mini-extra-high"} {
		if strings.Contains(string(out), id) {
			t.Fatalf("/api/agents payload leaked the exact id %q: %s", id, out)
		}
	}
}

// A pair cursor does not offer is the dialog's mistake, not the server's. It
// has to come back as a 400 like the muse gate's, or the browser files it as a
// failure of the machine and the user has nothing to correct.
func TestLaunchNodeRefusesAnUnofferedCursorPairWith400(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	m := syntheticWorkerManager(t, data)
	m.catalog = func() map[string]agentInfo {
		return map[string]agentInfo{"cursor": parseCursorModels(cursorListModelsFixture)}
	}
	defer m.Shutdown()

	a := newTestApp(t, &fakeTmux{})
	n := &Node{ID: "c1", Agent: "cursor", Transport: "acp", Dir: data, Model: "example-flash", Effort: "max"}
	status, err := a.launchNode(n, m)
	if err == nil {
		t.Fatal("launch accepted a pair cursor does not offer")
	}
	if status != 400 {
		t.Errorf("status = %d, want 400 (a launch-config error, not a server fault)", status)
	}
	if !strings.Contains(err.Error(), "model") {
		t.Errorf("error %q names neither field; the browser routes it by the words in it", err)
	}
	// The failure must not leave a half-made node behind.
	if n.SessionID != "" {
		t.Errorf("refused launch still set a session id %q", n.SessionID)
	}
}

func TestLaunchNodeReportsAnUnreadableCursorCatalogWith500(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	m := syntheticWorkerManager(t, data)
	m.catalog = func() map[string]agentInfo {
		return map[string]agentInfo{"cursor": {}}
	}
	defer m.Shutdown()

	a := newTestApp(t, &fakeTmux{})
	n := &Node{ID: "c1", Agent: "cursor", Transport: "acp", Dir: data, Model: "example-flash", Effort: "high"}
	status, err := a.launchNode(n, m)
	if err == nil {
		t.Fatal("launch accepted a model and effort without a readable cursor catalog")
	}
	if status != 500 {
		t.Errorf("status = %d, want 500 (catalog failure, not a bad user choice)", status)
	}
	if !strings.Contains(err.Error(), "model list could not be read") {
		t.Errorf("error %q does not identify the failed catalog read", err)
	}
	if strings.Contains(err.Error(), "cursor has no model") {
		t.Errorf("error %q mislabels a machine failure as a missing user choice", err)
	}
	if n.SessionID != "" {
		t.Errorf("failed launch still set a session id %q", n.SessionID)
	}
}

// The launch request is where a row plus a level becomes the one flag cursor
// accepts; the effort must not travel on as a second spelling of the choice.
func TestWorkerManagerResolvesCursorModelBeforeLaunch(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	m := syntheticWorkerManager(t, data)
	info := parseCursorModels(cursorListModelsFixture)
	m.catalog = func() map[string]agentInfo { return map[string]agentInfo{"cursor": info} }
	defer m.Shutdown()

	if _, err := m.Launch("chat-cursor", "cursor", data, "example-opus-thinking-fast", "high"); err != nil {
		t.Fatal(err)
	}
	launch := m.State("chat-cursor").Launch
	if launch == nil || launch.Model != "example-opus-thinking-high-fast" || launch.Effort != "" {
		t.Fatalf("launch = %+v, want model example-opus-thinking-high-fast with no effort", launch)
	}
	if _, err := m.Launch("chat-bogus", "cursor", data, "example-flash", "max"); err == nil {
		t.Fatal("launch accepted a pair cursor does not have")
	}
	// Other agents keep both fields exactly as chosen.
	if _, err := m.Launch("chat-grok", "grok", data, "example-flash", "max"); err != nil {
		t.Fatal(err)
	}
	if launch := m.State("chat-grok").Launch; launch == nil || launch.Model != "example-flash" || launch.Effort != "max" {
		t.Fatalf("grok launch = %+v, want the chosen model and effort untouched", launch)
	}
}
