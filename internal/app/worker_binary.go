package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hash), source); err != nil {
		return "", fmt.Errorf("session worker binary: copy executable: %w", err)
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
	destination := filepath.Join(dir, "scimux-"+hex.EncodeToString(hash.Sum(nil)))
	if err := os.Rename(tmpPath, destination); err != nil {
		return "", fmt.Errorf("session worker binary: publish pin: %w", err)
	}
	ok = true
	return destination, nil
}
