package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestMuseConsentDefaultIsFalse(t *testing.T) {
	a := settingsApp(t)
	if a.settings().MuseApprovalJudgeConsent {
		t.Fatal("missing settings file consented to Muse approval-judge")
	}
}

func TestMuseConsentUnreadableFileIsFalse(t *testing.T) {
	a := settingsApp(t)
	if err := a.saveSettings(settings{MuseApprovalJudgeConsent: true}); err != nil {
		t.Fatal(err)
	}
	if !a.settings().MuseApprovalJudgeConsent {
		t.Fatal("setup")
	}
	if err := os.Remove(a.settingsPath); err != nil {
		t.Fatal(err)
	}
	// A directory at the settings path gives every user, including root, a
	// deterministic read error without depending on permission semantics.
	if err := os.Mkdir(a.settingsPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if a.settings().MuseApprovalJudgeConsent {
		t.Fatal("unreadable settings file consented to Muse approval-judge")
	}
	rec := httptest.NewRecorder()
	a.handleSettingsGet(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["muse_approval_judge_consent"] != false {
		t.Fatalf("HTTP GET of unreadable settings consented: %v", out["muse_approval_judge_consent"])
	}
}

func TestSettingsOversizedFileFailsClosed(t *testing.T) {
	a := settingsApp(t)
	body := `{"muse_approval_judge_consent":true,"claude_usage_checks":true,"pad":"` + strings.Repeat("x", settingsMax) + `"}`
	if err := writeFileForSettingsTest(a.settingsPath, body); err != nil {
		t.Fatal(err)
	}
	got := a.settings()
	if got.MuseApprovalJudgeConsent || got.ClaudeUsageChecks {
		t.Fatalf("oversized settings consented: %+v", got)
	}
}

func TestMuseConsentEmptyAndCorruptAreFalse(t *testing.T) {
	a := settingsApp(t)
	if err := writeFileForSettingsTest(a.settingsPath, ""); err != nil {
		t.Fatal(err)
	}
	if a.settings().MuseApprovalJudgeConsent {
		t.Fatal("empty settings file consented")
	}
	if err := writeFileForSettingsTest(a.settingsPath, "{not json"); err != nil {
		t.Fatal(err)
	}
	if a.settings().MuseApprovalJudgeConsent {
		t.Fatal("corrupt settings file consented")
	}
}

func TestMuseConsentPersistsAcrossRestart(t *testing.T) {
	a := settingsApp(t)
	if err := a.saveSettings(settings{MuseApprovalJudgeConsent: true}); err != nil {
		t.Fatal(err)
	}
	b := &app{settingsPath: a.settingsPath}
	if !b.settings().MuseApprovalJudgeConsent {
		t.Fatal("Muse consent did not survive a restart")
	}
}

func TestSettingsPreserveClaudeWhenWritingMuseConsent(t *testing.T) {
	a := settingsApp(t)
	if err := a.saveSettings(settings{ClaudeUsageChecks: true}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"muse_approval_judge_consent":true}`))
	req.Header.Set("Content-Type", "application/json")
	a.handleSettingsPut(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT muse consent = %d %s", rec.Code, rec.Body)
	}
	got := a.settings()
	if !got.MuseApprovalJudgeConsent {
		t.Fatal("muse consent was not stored")
	}
	if !got.ClaudeUsageChecks {
		t.Fatal("writing muse consent clobbered claude_usage_checks")
	}
}

func TestSettingsPreserveMuseWhenWritingClaudeUsage(t *testing.T) {
	a := settingsApp(t)
	if err := a.saveSettings(settings{MuseApprovalJudgeConsent: true}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"claude_usage_checks":true}`))
	req.Header.Set("Content-Type", "application/json")
	a.handleSettingsPut(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT claude usage = %d %s", rec.Code, rec.Body)
	}
	got := a.settings()
	if !got.ClaudeUsageChecks {
		t.Fatal("claude usage was not stored")
	}
	if !got.MuseApprovalJudgeConsent {
		t.Fatal("writing claude_usage_checks clobbered muse consent")
	}
}

func TestSettingsPutPreservesUnknownFutureFields(t *testing.T) {
	a := settingsApp(t)
	original := `{"claude_usage_checks":true,"future_policy":{"mode":"strict"},"future_flag":null}`
	if err := writeFileForSettingsTest(a.settingsPath, original); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(`{"muse_approval_judge_consent":true}`))
	a.handleSettingsPut(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	b, err := os.ReadFile(a.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["future_policy"]) != `{"mode":"strict"}` {
		t.Fatalf("future_policy was not preserved: %s", raw["future_policy"])
	}
	if string(raw["future_flag"]) != "null" {
		t.Fatalf("future_flag was not preserved: %s", raw["future_flag"])
	}
}
