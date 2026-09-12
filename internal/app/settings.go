// settings.go — the small set of choices the *computer* holds, as opposed to
// the opaque per-browser blob in ui.json.
//
// The values here are server-enforced consent gates: Claude usage probing and
// Muse's token-spending approval judge. They cannot live in ui.json, which is
// deliberately opaque to the server and revisioned for concurrent browsers;
// a gate the server enforces must be a value the server owns.
//
// Every read degrades toward off. A missing, unreadable or nonsense file is
// "no consent given", never consent — the one store here whose failure mode
// must cost the user nothing.
package app

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
)

const settingsMax = 1 << 16

// settings is the whole document. Fields are added, never repurposed: an older
// scimux reading a newer file must see its own fields and ignore the rest,
// which plain JSON gives us for free.
type settings struct {
	// ClaudeUsageChecks is consent to run the usage probe. Off by default:
	// it is the user's quota, so scimux does not spend it unless asked.
	ClaudeUsageChecks bool `json:"claude_usage_checks"`
	// MuseApprovalJudgeConsent is computer-owned consent to launch Muse with
	// its default approval judge. Missing, empty, corrupt, or unreadable
	// settings read as false. True never disables the judge and never emits a
	// Muse CLI flag.
	MuseApprovalJudgeConsent bool `json:"muse_approval_judge_consent"`
	// Extra preserves settings written by a newer scimux. A Phase 5 write must
	// not erase a field merely because this binary does not understand it yet.
	Extra map[string]json.RawMessage `json:"-"`
}

func (s *settings) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	var known struct {
		ClaudeUsageChecks        bool `json:"claude_usage_checks"`
		MuseApprovalJudgeConsent bool `json:"muse_approval_judge_consent"`
	}
	if err := json.Unmarshal(b, &known); err != nil {
		return err
	}
	s.ClaudeUsageChecks = known.ClaudeUsageChecks
	s.MuseApprovalJudgeConsent = known.MuseApprovalJudgeConsent
	delete(raw, "claude_usage_checks")
	delete(raw, "muse_approval_judge_consent")
	s.Extra = raw
	return nil
}

func (s settings) MarshalJSON() ([]byte, error) {
	raw := make(map[string]json.RawMessage, len(s.Extra)+2)
	for key, value := range s.Extra {
		raw[key] = append(json.RawMessage(nil), value...)
	}
	claude, _ := json.Marshal(s.ClaudeUsageChecks)
	museConsent, _ := json.Marshal(s.MuseApprovalJudgeConsent)
	raw["claude_usage_checks"] = claude
	raw["muse_approval_judge_consent"] = museConsent
	return json.Marshal(raw)
}

const errMuseConsentRequired = errSettings("muse approval-judge consent is required")

// settings reads one atomically replaced snapshot. The PUT handler separately
// serializes its read-modify-write cycle; ordinary readers need no lock.
func (a *app) settings() settings {
	var s settings
	if a == nil || a.settingsPath == "" {
		return s
	}
	b, err := os.ReadFile(a.settingsPath)
	if err != nil || len(b) == 0 || len(b) > settingsMax {
		return settings{}
	}
	if json.Unmarshal(b, &s) != nil {
		return settings{}
	}
	return s
}

// saveSettings replaces the document atomically. Owner-only, like every other
// private file under ~/.scimux.
func (a *app) saveSettings(s settings) error {
	if a == nil || a.settingsPath == "" {
		return errSettingsUnavailable
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := a.settingsPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.settingsPath)
}

type errSettings string

func (e errSettings) Error() string { return string(e) }

const errSettingsUnavailable = errSettings("settings are unavailable")

func (a *app) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.settings())
}

// handleSettingsPut merges the bounded patch into the computer-owned document.
// Known consent fields and unknown future fields all survive unrelated writes.
func (a *app) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, settingsMax+1))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if len(b) > settingsMax {
		http.Error(w, "settings too large", 413)
		return
	}
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	var patch map[string]json.RawMessage
	if json.Unmarshal(b, &patch) != nil {
		http.Error(w, "settings must be valid JSON", 400)
		return
	}
	s := a.settings()
	if s.Extra == nil {
		s.Extra = map[string]json.RawMessage{}
	}
	for key, raw := range patch {
		if key == "claude_usage_checks" || key == "muse_approval_judge_consent" {
			continue
		}
		s.Extra[key] = append(json.RawMessage(nil), raw...)
	}
	if raw, ok := patch["claude_usage_checks"]; ok {
		if json.Unmarshal(raw, &s.ClaudeUsageChecks) != nil {
			http.Error(w, "settings must be valid JSON", 400)
			return
		}
	}
	if raw, ok := patch["muse_approval_judge_consent"]; ok {
		if json.Unmarshal(raw, &s.MuseApprovalJudgeConsent) != nil {
			http.Error(w, "settings must be valid JSON", 400)
			return
		}
	}
	if err := a.saveSettings(s); err != nil {
		http.Error(w, "save settings: "+err.Error(), 500)
		return
	}
	writeJSON(w, s)
}
