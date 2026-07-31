package main

import (
	"os"
	"path/filepath"
)

// prepareDataDir creates the scimux data root and the durable child
// directories that must exist before NewApp runs. Modes match the historical
// main() ownership: the data root and its children are owner-only (0700);
// pre-existing broader modes on the root, ui.json, and nodes.jsonl are
// best-effort tightened (Chmod errors are ignored). notes/ is intentionally
// not created — the notes store makes it lazily on first write.
func prepareDataDir(data string) error {
	// The data directory holds private notes and pane-excerpt evidence:
	// owner-only. Tighten pre-existing broader modes where we can.
	if err := os.MkdirAll(data, 0o700); err != nil {
		return err
	}
	os.Chmod(data, 0o700)
	os.Chmod(filepath.Join(data, "ui.json"), 0o600)
	os.Chmod(filepath.Join(data, "nodes.jsonl"), 0o600)
	if err := os.MkdirAll(filepath.Join(data, "sessions"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(data, "attachments"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(data, "assets"), 0o700); err != nil {
		return err
	}
	// The notes store creates its own directory lazily on first write, so no
	// MkdirAll here — an empty install has no notes/ until the user makes one.
	return nil
}
