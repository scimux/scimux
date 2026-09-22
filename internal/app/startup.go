package app

import (
	"path/filepath"

	"github.com/scimux/scimux/internal/privatefs"
)

// prepareDataDir creates the scimux data root and the durable child
// directories that must exist before NewApp runs. Modes match the historical
// main() ownership: the data root and its children are owner-only (0700);
// pre-existing broader modes are tightened and re-checked. A symlink, wrong
// type/owner, or a chmod failure is fatal: continuing would expose private
// history while giving the operator no indication that the privacy boundary
// failed. notes/ is intentionally not created — the notes store makes it
// lazily on first write.
func prepareDataDir(data string) error {
	if err := prepareDataDirectories(data); err != nil {
		return err
	}
	return secureDataFiles(data)
}

func prepareDataDirectories(data string) error {
	if err := privatefs.EnsureDir(data, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"sessions", "attachments", "assets"} {
		if err := privatefs.EnsureDir(filepath.Join(data, name), 0o700); err != nil {
			return err
		}
	}
	// The notes store creates its own directory lazily on first write, so no
	// MkdirAll here — an empty install has no notes/ until the user makes one.
	return nil
}

func secureDataFiles(data string) error {
	for _, name := range []string{"ui.json", "nodes.jsonl", "settings.json"} {
		if err := privatefs.EnsureFileIfExists(filepath.Join(data, name), 0o600); err != nil {
			return err
		}
	}
	return nil
}
