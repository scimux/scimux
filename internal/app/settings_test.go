package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/scimux/scimux/internal/storagebudget"
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
	if got := a.settings().StorageMinFreeBytes; got != storagebudget.DefaultMinFreeBytes {
		t.Fatalf("default minimum free bytes = %d, want %d", got, storagebudget.DefaultMinFreeBytes)
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
	if got := get()["allow_external_attachments"]; got != false {
		t.Fatalf("external attachments default = %v, want false", got)
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
	if code := put(`{"allow_external_attachments":true}`); code != http.StatusOK {
		t.Fatalf("external attachments PUT = %d", code)
	}
	if got := get()["allow_external_attachments"]; got != true {
		t.Fatalf("external attachments after PUT = %v, want true", got)
	}
	if code := put(`{"allow_external_attachments":null}`); code != http.StatusOK {
		t.Fatalf("null external attachments PUT = %d", code)
	}
	if got := get()["allow_external_attachments"]; got != true {
		t.Fatalf("null external attachment update changed setting to %v", got)
	}
	if code := put(`{"allow_external_attachments":"yes"}`); code != http.StatusBadRequest {
		t.Fatalf("string external attachment setting = %d, want 400", code)
	}
	if code := put(`not json`); code != http.StatusBadRequest {
		t.Fatalf("malformed PUT = %d, want 400", code)
	}
	if got := get()["claude_usage_checks"]; got != true {
		t.Fatal("a rejected write changed the stored setting")
	}
	if code := put(`{"storage_global_limit_bytes":1048576,"storage_node_limit_bytes":524288,"storage_min_free_bytes":0}`); code != http.StatusOK {
		t.Fatalf("storage PUT = %d", code)
	}
	limits := get()
	if limits["storage_global_limit_bytes"] != float64(1048576) ||
		limits["storage_node_limit_bytes"] != float64(524288) ||
		limits["storage_min_free_bytes"] != float64(0) {
		t.Fatalf("storage settings = %#v", limits)
	}
	if code := put(`{"storage_node_limit_bytes":-1}`); code != http.StatusBadRequest {
		t.Fatalf("negative storage PUT = %d, want 400", code)
	}
}

func TestAccentHarnessLogosSetting(t *testing.T) {
	a := settingsApp(t)
	get := func() map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleSettingsGet(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	put := func(body string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleSettingsPut(rec, httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body)))
		return rec.Code
	}
	if got := get()["use_accent_harness_logos"]; got != false {
		t.Fatalf("default accent preference = %v, want false", got)
	}
	if code := put(`{"use_accent_harness_logos":true}`); code != http.StatusOK {
		t.Fatalf("enable accent preference = %d", code)
	}
	if got := get()["use_accent_harness_logos"]; got != true {
		t.Fatalf("saved accent preference = %v, want true", got)
	}
	if got := (&app{settingsPath: a.settingsPath}).settings().UseAccentHarnessLogos; !got {
		t.Fatal("accent preference did not survive a new app instance")
	}
	if code := put(`{"use_accent_harness_logos":null}`); code != http.StatusOK {
		t.Fatalf("null accent preference = %d", code)
	}
	if get()["use_accent_harness_logos"] != true {
		t.Fatal("null changed the accent preference")
	}
	if code := put(`{"use_accent_harness_logos":"yes"}`); code != http.StatusBadRequest {
		t.Fatalf("non-boolean accent preference = %d, want 400", code)
	}
	if get()["use_accent_harness_logos"] != true {
		t.Fatal("rejected write changed the accent preference")
	}
	if code := put(`{"use_accent_harness_logos":false}`); code != http.StatusOK {
		t.Fatalf("disable accent preference = %d", code)
	}
	if get()["use_accent_harness_logos"] != false {
		t.Fatal("accent preference could not be turned off")
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

// Consent is the one setting whose success response must be true: a 200 on
// "turn it off" that leaves the probe authorized would spend the user's quota
// against an explicit refusal. Concurrent toggles (a double tap, or the local
// UI and a paired device at once) must therefore either report failure or be
// the value on disk — never report success while another writer's bytes land.
func TestSettingsConcurrentTogglesNeverLieAboutSuccess(t *testing.T) {
	a := settingsApp(t)
	// The two documents a winning save may leave, derived rather than spelled
	// out: a later consent field would otherwise turn this into a test of
	// nothing by making both literals unreachable.
	on, err := json.Marshal(settings{ClaudeUsageChecks: true})
	if err != nil {
		t.Fatal(err)
	}
	off, err := json.Marshal(settings{ClaudeUsageChecks: false})
	if err != nil {
		t.Fatal(err)
	}
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
		if s := string(b); s != string(on) && s != string(off) {
			t.Fatalf("round %d: concurrent saves left %q on disk, want %q or %q", round, s, on, off)
		}
	}
}

// The PUT handler holds settingsMu across its whole read-modify-write, which
// is wider than the save it ends with. This is what the extra width buys. The
// two consent fields are independent switches over different costs, and the
// handler publishes the whole document, so a patch that names one field still
// rewrites the other from whatever it read. Lock only the save and both
// requests read the same all-off document; whichever lands second republishes
// its own field and silently withdraws the consent the other just granted —
// a 200 to the browser that turned a switch on, and the switch off on disk.
func TestSettingsConcurrentPutsPreserveEachOthersConsent(t *testing.T) {
	dir := t.TempDir()
	patches := []string{
		`{"claude_usage_checks":true}`,
		`{"muse_approval_judge_consent":true}`,
	}
	for round := 0; round < 50; round++ {
		a := &app{settingsPath: filepath.Join(dir, "settings-"+strconv.Itoa(round)+".json")}
		var wg sync.WaitGroup
		codes := make([]int, len(patches))
		bodies := make([]string, len(patches))
		for i, patch := range patches {
			wg.Add(1)
			go func(i int, patch string) {
				defer wg.Done()
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(patch))
				req.Header.Set("Content-Type", "application/json")
				a.handleSettingsPut(rec, req)
				codes[i], bodies[i] = rec.Code, rec.Body.String()
			}(i, patch)
		}
		wg.Wait()
		for i, code := range codes {
			if code != http.StatusOK {
				t.Fatalf("round %d: PUT %s = %d %s", round, patches[i], code, bodies[i])
			}
		}
		// Both were told yes, so both must be true — on disk and to the next
		// reader, which is the party that decides whether to spend anything.
		got := a.settings()
		if !got.ClaudeUsageChecks || !got.MuseApprovalJudgeConsent {
			t.Fatalf("round %d: concurrent grants left claude=%v muse=%v, want both true; one PUT withdrew the other's consent",
				round, got.ClaudeUsageChecks, got.MuseApprovalJudgeConsent)
		}
	}
}
