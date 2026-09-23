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
	"path/filepath"
	"time"

	"github.com/scimux/scimux/internal/storagebudget"
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
	// AllowExternalAttachments permits agent-authored references outside the
	// chat working directory to be copied into durable node history. It does
	// not expose a filesystem-serving endpoint and defaults off on every read
	// failure.
	AllowExternalAttachments bool `json:"allow_external_attachments"`
	// Storage limits are byte counts. Zero disables that particular limit;
	// MinFreeBytes defaults to a conservative reserve on a missing setting.
	StorageGlobalLimitBytes int64 `json:"storage_global_limit_bytes"`
	StorageNodeLimitBytes   int64 `json:"storage_node_limit_bytes"`
	StorageMinFreeBytes     int64 `json:"storage_min_free_bytes"`
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
		ClaudeUsageChecks        bool  `json:"claude_usage_checks"`
		MuseApprovalJudgeConsent bool  `json:"muse_approval_judge_consent"`
		AllowExternalAttachments bool  `json:"allow_external_attachments"`
		StorageGlobalLimitBytes  int64 `json:"storage_global_limit_bytes"`
		StorageNodeLimitBytes    int64 `json:"storage_node_limit_bytes"`
		StorageMinFreeBytes      int64 `json:"storage_min_free_bytes"`
	}
	known.StorageMinFreeBytes = storagebudget.DefaultMinFreeBytes
	if err := json.Unmarshal(b, &known); err != nil {
		return err
	}
	if known.StorageGlobalLimitBytes < 0 || known.StorageNodeLimitBytes < 0 || known.StorageMinFreeBytes < 0 {
		return errSettings("storage limits must be non-negative integer byte counts")
	}
	s.ClaudeUsageChecks = known.ClaudeUsageChecks
	s.MuseApprovalJudgeConsent = known.MuseApprovalJudgeConsent
	s.AllowExternalAttachments = known.AllowExternalAttachments
	s.StorageGlobalLimitBytes = known.StorageGlobalLimitBytes
	s.StorageNodeLimitBytes = known.StorageNodeLimitBytes
	s.StorageMinFreeBytes = known.StorageMinFreeBytes
	delete(raw, "claude_usage_checks")
	delete(raw, "muse_approval_judge_consent")
	delete(raw, "allow_external_attachments")
	delete(raw, "storage_global_limit_bytes")
	delete(raw, "storage_node_limit_bytes")
	delete(raw, "storage_min_free_bytes")
	s.Extra = raw
	return nil
}

func (s settings) MarshalJSON() ([]byte, error) {
	raw := make(map[string]json.RawMessage, len(s.Extra)+6)
	for key, value := range s.Extra {
		raw[key] = append(json.RawMessage(nil), value...)
	}
	claude, _ := json.Marshal(s.ClaudeUsageChecks)
	museConsent, _ := json.Marshal(s.MuseApprovalJudgeConsent)
	external, _ := json.Marshal(s.AllowExternalAttachments)
	raw["claude_usage_checks"] = claude
	raw["muse_approval_judge_consent"] = museConsent
	raw["allow_external_attachments"] = external
	for key, value := range map[string]int64{
		"storage_global_limit_bytes": s.StorageGlobalLimitBytes,
		"storage_node_limit_bytes":   s.StorageNodeLimitBytes,
		"storage_min_free_bytes":     s.StorageMinFreeBytes,
	} {
		encoded, _ := json.Marshal(value)
		raw[key] = encoded
	}
	return json.Marshal(raw)
}

const errMuseConsentRequired = errSettings("muse approval-judge consent is required")

// settings reads one atomically replaced snapshot. The PUT handler separately
// serializes its read-modify-write cycle; ordinary readers need no lock.
func (a *app) settings() settings {
	s := defaultSettings()
	if a == nil || a.settingsPath == "" {
		return s
	}
	b, err := os.ReadFile(a.settingsPath)
	if err != nil || len(b) == 0 || len(b) > settingsMax {
		return defaultSettings()
	}
	if json.Unmarshal(b, &s) != nil {
		return defaultSettings()
	}
	return s
}

func defaultSettings() settings {
	return settings{StorageMinFreeBytes: storagebudget.DefaultMinFreeBytes}
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
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	return a.writeSettingsLocked(s)
}

// writeSettingsLocked publishes the document; the caller already holds
// settingsMu. handleSettingsPut needs the lock to span its read-modify-write,
// which is wider than a single save, and sync.Mutex does not nest.
func (a *app) writeSettingsLocked(s settings) error {
	if a == nil || a.settingsPath == "" {
		return errSettingsUnavailable
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
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
		if key == "claude_usage_checks" || key == "muse_approval_judge_consent" || key == "allow_external_attachments" ||
			key == "storage_global_limit_bytes" || key == "storage_node_limit_bytes" || key == "storage_min_free_bytes" {
			continue
		}
		s.Extra[key] = append(json.RawMessage(nil), raw...)
	}
	if raw, ok := patch["claude_usage_checks"]; ok {
		if string(raw) != "null" && json.Unmarshal(raw, &s.ClaudeUsageChecks) != nil {
			http.Error(w, "settings must be valid JSON", 400)
			return
		}
	}
	if raw, ok := patch["muse_approval_judge_consent"]; ok {
		if string(raw) != "null" && json.Unmarshal(raw, &s.MuseApprovalJudgeConsent) != nil {
			http.Error(w, "settings must be valid JSON", 400)
			return
		}
	}
	if raw, ok := patch["allow_external_attachments"]; ok {
		if string(raw) != "null" && json.Unmarshal(raw, &s.AllowExternalAttachments) != nil {
			http.Error(w, "settings must be valid JSON", 400)
			return
		}
	}
	for key, dst := range map[string]*int64{
		"storage_global_limit_bytes": &s.StorageGlobalLimitBytes,
		"storage_node_limit_bytes":   &s.StorageNodeLimitBytes,
		"storage_min_free_bytes":     &s.StorageMinFreeBytes,
	} {
		raw, ok := patch[key]
		if !ok || string(raw) == "null" {
			continue
		}
		var value int64
		if json.Unmarshal(raw, &value) != nil || value < 0 {
			http.Error(w, "storage limits must be non-negative integer byte counts", 400)
			return
		}
		*dst = value
	}
	if err := a.writeSettingsLocked(s); err != nil {
		http.Error(w, "save settings: "+err.Error(), 500)
		return
	}
	a.storageMu.Lock()
	a.storageAt = time.Time{}
	a.storageMu.Unlock()
	writeJSON(w, s)
}
