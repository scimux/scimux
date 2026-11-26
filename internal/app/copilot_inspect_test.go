package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/scimux/scimux/internal/acp"
)

// The Copilot model menu is read only when the user explicitly asks for it:
// a POST to the harness check carrying {"inspect_copilot": true}. Ordinary
// startup, GET, and every other body leave the launchable default in place.

func TestCopilotInspectChoiceIsExplicit(t *testing.T) {
	cases := []struct {
		name   string
		method string
		body   string
		want   bool
		status int
	}{
		{name: "explicit true", method: http.MethodPost, body: `{"inspect_copilot":true}`, want: true},
		{name: "false is an ordinary check", method: http.MethodPost, body: `{"inspect_copilot":false}`},
		{name: "empty body is an ordinary check", method: http.MethodPost, body: ``},
		{name: "absent key is an ordinary check", method: http.MethodPost, body: `{"inspect_vibe":true}`},
		{name: "null is an ordinary check", method: http.MethodPost, body: `{"inspect_copilot":null}`},
		{name: "GET never inspects", method: http.MethodGet, body: `{"inspect_copilot":true}`},
		{name: "non-boolean is malformed", method: http.MethodPost, body: `{"inspect_copilot":"yes"}`, status: http.StatusBadRequest},
		{name: "invalid JSON is malformed", method: http.MethodPost, body: `{`, status: http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "/api/harnesses/latest", strings.NewReader(c.body))
			_, got, status := harnessInspectChoices(r)
			if got != c.want || status != c.status {
				t.Fatalf("choice = %v status %d, want %v status %d", got, status, c.want, c.status)
			}
		})
	}
}

// withCopilotProbe swaps the catalog probe and a copilot binary onto PATH, and
// restores both. The probe is the only thing that could start a session.
func withCopilotProbe(t *testing.T, probe func(context.Context, string) (acp.CopilotCatalog, error)) *atomic.Int32 {
	t.Helper()
	binDir := t.TempDir()
	writeScript(t, binDir, "copilot", `exit 0`)
	t.Setenv("PATH", binDir)
	resetAgentsCache(t)
	var calls atomic.Int32
	prev := probeCopilotCatalog
	probeCopilotCatalog = func(ctx context.Context, bin string) (acp.CopilotCatalog, error) {
		calls.Add(1)
		return probe(ctx, bin)
	}
	t.Cleanup(func() { probeCopilotCatalog = prev })
	return &calls
}

func checkHarness(t *testing.T, body string) map[string]agentInfo {
	t.Helper()
	a := newTestApp(t, &fakeTmux{})
	a.harnessInventory = func(bool) []harnessRow { return nil }
	a.harnessLatestSources = func() map[string]harnessSource { return map[string]harnessSource{} }
	req := httptest.NewRequest(http.MethodPost, "/api/harnesses/latest", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.handleHarnessLatest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Agents map[string]agentInfo `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Agents
}

// An explicit inspection publishes the advertised model menu. Efforts appear
// only for the model the session reported as current.
func TestCopilotInspectionPublishesAdvertisedMenus(t *testing.T) {
	calls := withCopilotProbe(t, func(context.Context, string) (acp.CopilotCatalog, error) {
		return acp.CopilotCatalog{
			Models:  []string{"auto", "alpha"},
			Efforts: map[string][]string{"auto": {"low", "high"}},
		}, nil
	})
	agents := checkHarness(t, `{"inspect_copilot":true}`)
	if calls.Load() != 1 {
		t.Fatalf("probe calls = %d, want exactly one explicit inspection", calls.Load())
	}
	got := agents["copilot"]
	if len(got.Models) != 2 || got.Models[0] != "auto" || got.Models[1] != "alpha" {
		t.Fatalf("copilot models = %v", got.Models)
	}
	if e := got.Efforts["auto"]; len(e.Levels) != 2 || e.Levels[0] != "low" || e.Required || e.Default != "" {
		t.Fatalf("auto efforts = %+v, want the advertised levels with no invented default", e)
	}
	if _, ok := got.Efforts["alpha"]; ok {
		t.Fatal("effort levels were attributed to a model the session did not describe")
	}
}

// Missing, failed, or empty inspection keeps the launchable default and never
// invents a model.
func TestCopilotInspectionFailureKeepsDefaults(t *testing.T) {
	cases := map[string]func(context.Context, string) (acp.CopilotCatalog, error){
		"probe error": func(context.Context, string) (acp.CopilotCatalog, error) {
			return acp.CopilotCatalog{}, errors.New("refused")
		},
		"empty menu": func(context.Context, string) (acp.CopilotCatalog, error) { return acp.CopilotCatalog{}, nil },
	}
	for name, probe := range cases {
		t.Run(name, func(t *testing.T) {
			withCopilotProbe(t, probe)
			agents := checkHarness(t, `{"inspect_copilot":true}`)
			got, ok := agents["copilot"]
			if !ok {
				t.Fatal("a failed inspection removed the copilot harness from the dialog")
			}
			if got.Models == nil || len(got.Models) != 0 || got.Efforts != nil {
				t.Fatalf("copilot = %+v, want the launchable default with no invented menu", got)
			}
		})
	}
}

// Ordinary checks never start a Copilot session.
func TestCopilotProbeRunsOnlyForExplicitInspection(t *testing.T) {
	for _, body := range []string{``, `{"inspect_copilot":false}`, `{"inspect_vibe":true}`} {
		calls := withCopilotProbe(t, func(context.Context, string) (acp.CopilotCatalog, error) {
			return acp.CopilotCatalog{Models: []string{"alpha"}}, nil
		})
		checkHarness(t, body)
		if n := calls.Load(); n != 0 {
			t.Fatalf("body %q started %d copilot probes", body, n)
		}
	}
}

// An explicit inspection starts a Copilot session only when there is an
// installed copilot whose menu is still empty and whose binary still resolves.
// In every other case the probe is never started and the snapshot is returned
// unchanged.
func TestInspectCopilotCatalogStartsOnlyForAnEmptyInstalledHarness(t *testing.T) {
	cases := map[string]struct {
		cache     map[string]agentInfo
		emptyPath bool
		wantCount int
	}{
		"copilot not installed": {
			cache:     map[string]agentInfo{"claude": {Models: []string{"fable"}}},
			wantCount: 0,
		},
		"menu already published": {
			cache:     map[string]agentInfo{"copilot": {Models: []string{"alpha"}}},
			wantCount: 0,
		},
		"binary gone since the snapshot": {
			cache:     map[string]agentInfo{"copilot": {Models: []string{}}},
			emptyPath: true,
			wantCount: 0,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			calls := withCopilotProbe(t, func(context.Context, string) (acp.CopilotCatalog, error) {
				return acp.CopilotCatalog{Models: []string{"beta"}}, nil
			})
			if c.emptyPath {
				t.Setenv("PATH", t.TempDir())
			}
			resetAgentsCache(t)
			agentsCacheMu.Lock()
			agentsCache = c.cache
			agentsLoaded = true
			agentsCacheMu.Unlock()
			got := inspectCopilotCatalog()
			if n := calls.Load(); int(n) != c.wantCount {
				t.Fatalf("probe calls = %d, want %d", n, c.wantCount)
			}
			if len(got) != len(c.cache) {
				t.Fatalf("snapshot changed: %v", got)
			}
			for k, v := range c.cache {
				if !reflect.DeepEqual(got[k].Models, v.Models) {
					t.Fatalf("agent %s models = %v, want unchanged %v", k, got[k].Models, v.Models)
				}
			}
		})
	}
}
