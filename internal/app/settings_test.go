package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func settingsApp(t *testing.T) *app {
	t.Helper()
	return &app{settingsPath: filepath.Join(t.TempDir(), "settings.json")}
}

// Off is the default and it is not a stored value: a settings file that does
// not exist yet must read as "no consent given", never as consent.
func TestSettingsDefaultIsOff(t *testing.T) {
	a := settingsApp(t)
	if a.settings().ClaudeUsageChecks {
		t.Fatal("a computer with no settings file consented to spending quota")
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	a := settingsApp(t)
	if err := a.saveSettings(settings{ClaudeUsageChecks: true}); err != nil {
		t.Fatal(err)
	}
	if !a.settings().ClaudeUsageChecks {
		t.Fatal("consent did not survive a write")
	}
	// A fresh app reading the same file sees it: the setting belongs to the
	// computer whose quota is spent, not to the browser that toggled it.
	b := &app{settingsPath: a.settingsPath}
	if !b.settings().ClaudeUsageChecks {
		t.Fatal("consent did not survive a restart")
	}
	if err := a.saveSettings(settings{}); err != nil {
		t.Fatal(err)
	}
	if a.settings().ClaudeUsageChecks {
		t.Fatal("consent could not be withdrawn")
	}
}

// A corrupt or unreadable settings file must read as off. Every other store
// here degrades toward doing less; a store that degraded toward spending would
// be the one place that gets it backwards.
func TestSettingsDegradeToOff(t *testing.T) {
	a := settingsApp(t)
	if err := writeFileForSettingsTest(a.settingsPath, "{not json"); err != nil {
		t.Fatal(err)
	}
	if a.settings().ClaudeUsageChecks {
		t.Fatal("unreadable settings read as consent")
	}
}

func TestSettingsHTTP(t *testing.T) {
	a := settingsApp(t)
	get := func() map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleSettingsGet(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d", rec.Code)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("GET body %q: %v", rec.Body, err)
		}
		return out
	}
	if got := get()["claude_usage_checks"]; got != false {
		t.Fatalf("default = %v, want false", got)
	}

	put := func(body string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		a.handleSettingsPut(rec, req)
		return rec.Code
	}
	if code := put(`{"claude_usage_checks":true}`); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	if got := get()["claude_usage_checks"]; got != true {
		t.Fatalf("after PUT = %v, want true", got)
	}
	if code := put(`not json`); code != http.StatusBadRequest {
		t.Fatalf("malformed PUT = %d, want 400", code)
	}
	if got := get()["claude_usage_checks"]; got != true {
		t.Fatal("a rejected write changed the stored setting")
	}
}

// The gate is on the server, before anything is launched: a browser that has
// not read the setting still cannot cause a probe.
func TestClaudeUsageCollectionIsGatedOnConsent(t *testing.T) {
	a := settingsApp(t)
	_, err := a.collectClaudeUsage(t.Context())
	if !errors.Is(err, errClaudeUsageOff) {
		t.Fatalf("collect without consent = %v, want errClaudeUsageOff", err)
	}
}

// "Off" and "unavailable" are different things and the browser must be able to
// tell them apart without reading English: one offers a switch, the other only
// explains itself.
func TestUsageViewMarksOffDistinctly(t *testing.T) {
	off := usageView(agentUsage{Agent: "claude"}, errClaudeUsageOff)
	if off.Available || !off.Off {
		t.Fatalf("off view = %+v", off)
	}
	broken := usageView(agentUsage{Agent: "claude"}, errors.New("usage unavailable: claude not installed"))
	if broken.Available || broken.Off {
		t.Fatalf("an ordinary failure claimed to be a switched-off gauge: %+v", broken)
	}
	fine := usageView(agentUsage{Agent: "codex"}, nil)
	if fine.Off {
		t.Fatalf("a working gauge claimed to be off: %+v", fine)
	}
}

func writeFileForSettingsTest(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

// Consent is the one setting whose success response must be true: a 200 on
// "turn it off" that leaves the probe authorized would spend the user's quota
// against an explicit refusal. Concurrent toggles (a double tap, or the local
// UI and a paired device at once) must therefore either report failure or be
// the value on disk — never report success while another writer's bytes land.
func TestSettingsConcurrentTogglesNeverLieAboutSuccess(t *testing.T) {
	a := settingsApp(t)
	for round := 0; round < 50; round++ {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, want := range []bool{true, false} {
			wg.Add(1)
			go func(i int, want bool) {
				defer wg.Done()
				errs[i] = a.saveSettings(settings{ClaudeUsageChecks: want})
			}(i, want)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: concurrent save %d failed: %v", round, i, err)
			}
		}
		// Both reported success, so the file must be one of them intact —
		// not a half-written document that the next read degrades to "off"
		// while the browser shows the switch on.
		b, err := os.ReadFile(a.settingsPath)
		if err != nil {
			t.Fatalf("round %d: settings unreadable after two successful saves: %v", round, err)
		}
		if s := string(b); s != `{"claude_usage_checks":true}` && s != `{"claude_usage_checks":false}` {
			t.Fatalf("round %d: concurrent saves left %q on disk", round, s)
		}
	}
}
