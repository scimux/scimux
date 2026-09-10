// settings.go — the small set of choices the *computer* holds, as opposed to
// the opaque per-browser blob in ui.json.
//
// There is exactly one today and it is the reason the file exists: consent to
// spend the user's Claude quota measuring it. That cannot live in ui.json,
// which is deliberately opaque to the server and revisioned for concurrent
// browsers; a gate the server enforces must be a value the server owns and can
// read without parsing someone else's document.
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
}

// settings reads the stored document. It takes no lock beyond the file's own
// atomicity because the value is one bool read on a poll and written on a tap.
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

// handleSettingsPut replaces the document wholesale rather than patching one
// field: the client always sends the state it means, so a request that loses a
// field cannot silently mean "leave it as it was".
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
	var s settings
	if json.Unmarshal(b, &s) != nil {
		http.Error(w, "settings must be valid JSON", 400)
		return
	}
	if err := a.saveSettings(s); err != nil {
		http.Error(w, "save settings: "+err.Error(), 500)
		return
	}
	writeJSON(w, s)
}
