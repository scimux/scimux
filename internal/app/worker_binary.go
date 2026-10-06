package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/scimux/scimux/internal/sessionworker"
)

// pinWorkerExecutable snapshots the running muxer's own image into a
// content-addressed private path. Self-update atomically replaces the public
// executable name; without this pin an old muxer could launch a new-major
// worker in the short interval before its own exec handoff.
func pinWorkerExecutable(executable, dataDir string) (string, error) {
	if executable == "" {
		return "", errors.New("session worker binary: empty executable")
	}
	dataInfo, err := os.Stat(dataDir)
	if err != nil {
		return "", fmt.Errorf("session worker binary: inspect data directory: %w", err)
	}
	if !dataInfo.IsDir() || dataInfo.Mode().Perm()&0o077 != 0 {
		return "", errors.New("session worker binary: data directory is not owner-only")
	}
	source, err := os.Open(executable)
	if err != nil {
		return "", fmt.Errorf("session worker binary: open executable: %w", err)
	}
	defer source.Close()
	if info, err := source.Stat(); err != nil {
		return "", fmt.Errorf("session worker binary: inspect executable: %w", err)
	} else if !info.Mode().IsRegular() {
		return "", errors.New("session worker binary: executable is not a regular file")
	}
	dir := filepath.Join(dataDir, "control", "worker-binaries")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("session worker binary: create pin directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("session worker binary: protect pin directory: %w", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, source); err != nil {
		return "", fmt.Errorf("session worker binary: hash executable: %w", err)
	}
	digest := hash.Sum(nil)
	destination := filepath.Join(dir, "scimux-"+hex.EncodeToString(digest))
	if pinned, err := os.Lstat(destination); err == nil {
		if !pinned.Mode().IsRegular() || pinned.Mode().Perm() != 0o700 {
			return "", errors.New("session worker binary: existing pin is not an owner-only executable")
		}
		return destination, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("session worker binary: inspect existing pin: %w", err)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("session worker binary: rewind executable: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".scimux-worker-*")
	if err != nil {
		return "", fmt.Errorf("session worker binary: create pin: %w", err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	copiedHash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, copiedHash), source); err != nil {
		return "", fmt.Errorf("session worker binary: copy executable: %w", err)
	}
	if !bytes.Equal(copiedHash.Sum(nil), digest) {
		return "", errors.New("session worker binary: executable changed while pinning")
	}
	if err := tmp.Chmod(0o700); err != nil {
		return "", fmt.Errorf("session worker binary: chmod pin: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("session worker binary: sync pin: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("session worker binary: close pin: %w", err)
	}
	if err := os.Rename(tmpPath, destination); err != nil {
		return "", fmt.Errorf("session worker binary: publish pin: %w", err)
	}
	ok = true
	return destination, nil
}

// sweepWorkerExecutables removes generations that no current or discoverable
// worker can exec, and that no live Claude hook bundle in hooksDir names. A
// locator written by a pre-Executable worker makes the sweep fail closed:
// keeping a few pins is cheaper than stranding that chat on its next provider
// subprocess replacement.
func sweepWorkerExecutables(dataDir, hooksDir, current string) error {
	dir := filepath.Join(dataDir, "control", "worker-binaries")
	current = filepath.Clean(current)
	if filepath.Dir(current) != filepath.Clean(dir) {
		return errors.New("session worker binary: current pin is outside pin directory")
	}
	locators, err := sessionworker.List(dataDir)
	if err != nil {
		return fmt.Errorf("session worker binary: list live pins: %w", err)
	}
	keep := map[string]bool{filepath.Base(current): true}
	for _, locator := range locators {
		if locator.Executable == "" {
			return nil
		}
		path := filepath.Clean(locator.Executable)
		if filepath.Dir(path) == filepath.Clean(dir) {
			keep[filepath.Base(path)] = true
		}
	}
	if err := keepClaudeBundlePins(hooksDir, filepath.Clean(dir), keep); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("session worker binary: read pin directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if keep[name] || len(name) != len("scimux-")+sha256.Size*2 || !strings.HasPrefix(name, "scimux-") {
			continue
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(name, "scimux-")); err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("session worker binary: inspect stale pin: %w", err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("session worker binary: remove stale pin: %w", err)
		}
	}
	return nil
}

// keepClaudeBundlePins marks every pin a live Claude hook bundle names. A
// bundle's settings.json bakes the pin of the worker that launched it, and the
// pane keeps exec'ing that path for every hook after a full stop retires the
// worker and a newer one recovers the pane, so the bundle is as much a
// reference as a locator. Deleting the pin would leave the recovered chat with
// no runnable hooks: no auto-approve, no Stop, no /clear binding. Archived
// bundles belong to no pane. A bundle without capabilities.json predates pins;
// one that cannot be read or parsed fails the sweep closed.
func keepClaudeBundlePins(hooksDir, pinDir string, keep map[string]bool) error {
	if hooksDir == "" {
		return nil
	}
	bundles, err := os.ReadDir(hooksDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("session worker binary: list Claude hook bundles: %w", err)
	}
	for _, bundle := range bundles {
		if !bundle.IsDir() || bundle.Name() == "archive" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(hooksDir, bundle.Name(), "capabilities.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("session worker binary: read Claude hook bundle %s: %w", bundle.Name(), err)
		}
		var caps claudeHookCapabilities
		if err := json.Unmarshal(b, &caps); err != nil {
			return fmt.Errorf("session worker binary: parse Claude hook bundle %s: %w", bundle.Name(), err)
		}
		if caps.Exec == "" {
			continue
		}
		path := filepath.Clean(caps.Exec)
		if filepath.Dir(path) == pinDir {
			keep[filepath.Base(path)] = true
		}
	}
	return nil
}
