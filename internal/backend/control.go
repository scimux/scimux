package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	locatorName    = "muxer.json"
	maxLocatorSize = 4 << 10
	claimAttempts  = 3
	claimProbeTime = 250 * time.Millisecond
)

// Registration publishes a muxer's already-authenticated private link in its
// owner-only data directory. It is a locator, not a second listener.
type Registration struct {
	path    string
	payload []byte
	once    sync.Once
	err     error
}

// Register atomically makes link discoverable by `scimux stop`. A reachable
// owner rejects a second muxer for the same data directory; an unreachable
// stale locator is replaced. Registration.Close removes only its own value.
func Register(dataDir string, link Link) (*Registration, error) {
	if dataDir == "" {
		return nil, errors.New("backend: empty data directory")
	}
	if link.Socket == "" || link.Token == "" {
		return nil, errors.New("backend: incomplete muxer link")
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		return nil, fmt.Errorf("backend: inspect data directory: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("backend: data path is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("backend: data directory is not owner-only")
	}
	// Link contains only strings, so its JSON encoding cannot fail.
	payload, _ := json.Marshal(link)
	payload = append(payload, '\n')
	path := filepath.Join(dataDir, locatorName)
	for range claimAttempts {
		err := publishLocator(dataDir, path, payload)
		if err == nil {
			return &Registration{path: path, payload: payload}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		current, discoverErr := Discover(dataDir)
		if discoverErr == nil && socketReachable(current.Socket) {
			return nil, errors.New("backend: a muxer is already running for this data directory")
		}
		existing, statErr := os.Lstat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return nil, fmt.Errorf("backend: inspect existing muxer locator: %w", statErr)
		}
		if !existing.Mode().IsRegular() {
			return nil, errors.New("backend: existing muxer locator is not a regular file")
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("backend: remove stale muxer locator: %w", err)
		}
	}
	return nil, errors.New("backend: muxer locator changed while claiming it")
}

func publishLocator(dataDir, path string, payload []byte) error {
	tmp, err := os.CreateTemp(dataDir, ".muxer-*.tmp")
	if err != nil {
		return fmt.Errorf("backend: create muxer locator: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(payload); err != nil {
		return fmt.Errorf("backend: write muxer locator: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("backend: close muxer locator: %w", err)
	}
	if err := os.Link(tmpPath, path); err != nil {
		return fmt.Errorf("backend: publish muxer locator: %w", err)
	}
	return nil
}

func socketReachable(path string) bool {
	conn, err := net.DialTimeout("unix", path, claimProbeTime)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Discover reads the capability published for one data directory. The mode
// check refuses to use a capability that other local users could read.
func Discover(dataDir string) (Link, error) {
	path := filepath.Join(dataDir, locatorName)
	info, err := os.Lstat(path)
	if err != nil {
		return Link{}, fmt.Errorf("backend: read muxer locator: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return Link{}, errors.New("backend: read muxer locator: locator is not an owner-only regular file")
	}
	if info.Size() > maxLocatorSize {
		return Link{}, errors.New("backend: read muxer locator: locator is too large")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return Link{}, fmt.Errorf("backend: read muxer locator: %w", err)
	}
	var link Link
	if err := json.Unmarshal(payload, &link); err != nil {
		return Link{}, fmt.Errorf("backend: read muxer locator: %w", err)
	}
	if link.Socket == "" || link.Token == "" {
		return Link{}, errors.New("backend: read muxer locator: incomplete link")
	}
	return link, nil
}

// Close withdraws this registration. A different payload means another muxer
// has taken ownership, so the older registration deliberately leaves it.
func (r *Registration) Close() error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		payload, err := os.ReadFile(r.path)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			r.err = err
			return
		}
		if !bytes.Equal(payload, r.payload) {
			return
		}
		if err := os.Remove(r.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.err = err
		}
	})
	return r.err
}
