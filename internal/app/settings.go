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
	"path/filepath"
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
//
// Serialized, and through a uniquely named temporary file. Both halves matter
// for the same reason: this document is consent, so a save that reports
// success must be the save that landed. A shared temporary name lets two
// concurrent toggles write the same inode and race its rename — the winner
// reports success having written the loser's value, and the loser fails on a
// file the winner already renamed away. The user sees "off", the file says on,
// and the probe keeps spending their quota.
func (a *app) saveSettings(s settings) error {
	if a == nil || a.settingsPath == "" {
		return errSettingsUnavailable
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	f, err := os.CreateTemp(filepath.Dir(a.settingsPath), ".settings-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Leave nothing behind on any failure after this point: the rename is what
	// publishes the value, so an abandoned temporary is debris, not a setting.
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
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
