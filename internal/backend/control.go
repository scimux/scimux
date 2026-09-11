package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const (
	locatorName    = "muxer.json"
	lockName       = "muxer.lock"
	maxLocatorSize = 4 << 10
)

// ErrMuxerOwned means another live process holds the data directory's lifetime
// lock. The locator is deliberately not consulted for ownership: it can be
// stale after a crash and cannot make check/remove/publish atomic.
var ErrMuxerOwned = errors.New("backend: a muxer is already running for this data directory")

// Registration owns a data directory for its entire lifetime and optionally
// publishes the muxer's already-authenticated private link. The persistent
// lock file provides ownership; muxer.json is only a locator for `scimux stop`.
type Registration struct {
	lock        *os.File
	locatorPath string
	payload     []byte
	once        sync.Once
	err         error
}

// Claim takes the kernel-held ownership lock before any store state is read.
// muxer.lock is never unlinked: replacing its inode would let two processes
// lock different files with the same name.
func Claim(dataDir string) (*Registration, error) {
	if dataDir == "" {
		return nil, errors.New("backend: empty data directory")
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
	lock, err := os.OpenFile(filepath.Join(dataDir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("backend: open muxer ownership lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrMuxerOwned
		}
		return nil, fmt.Errorf("backend: lock data directory: %w", err)
	}
	locatorPath := filepath.Join(dataDir, locatorName)
	if stale, err := os.Lstat(locatorPath); err == nil {
		if !stale.Mode().IsRegular() {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
			return nil, errors.New("backend: existing muxer locator is not a regular file")
		}
		if err := os.Remove(locatorPath); err != nil {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
			return nil, fmt.Errorf("backend: remove stale muxer locator: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, fmt.Errorf("backend: inspect stale muxer locator: %w", err)
	}
	return &Registration{lock: lock, locatorPath: locatorPath}, nil
}

// Publish atomically makes link discoverable by `scimux stop`. It replaces a
// locator left by a crashed previous owner while the lifetime lock prevents a
// contender from racing that replacement.
func (r *Registration) Publish(link Link) error {
	if r == nil || r.lock == nil {
		return errors.New("backend: publish without ownership")
	}
	if link.Socket == "" || link.Token == "" {
		return errors.New("backend: incomplete muxer link")
	}
	// Link contains only strings, so its JSON encoding cannot fail.
	payload, _ := json.Marshal(link)
	payload = append(payload, '\n')
	if err := publishLocator(filepath.Dir(r.locatorPath), r.locatorPath, payload); err != nil {
		return err
	}
	r.payload = payload
	return nil
}

// Register is the convenience form used when no work is needed between
// claiming ownership and publishing the control locator.
func Register(dataDir string, link Link) (*Registration, error) {
	owner, err := Claim(dataDir)
	if err != nil {
		return nil, err
	}
	if err := owner.Publish(link); err != nil {
		_ = owner.Close()
		return nil, err
	}
	return owner, nil
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
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("backend: publish muxer locator: %w", err)
	}
	return nil
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

// Close withdraws this registration and releases lifetime ownership. Locator
// removal precedes unlock, so a successor cannot publish until cleanup ends.
func (r *Registration) Close() error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		if len(r.payload) != 0 {
			payload, err := os.ReadFile(r.locatorPath)
			if err == nil && bytes.Equal(payload, r.payload) {
				if err := os.Remove(r.locatorPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					r.err = err
				}
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				r.err = err
			}
		}
		if r.lock != nil {
			if err := syscall.Flock(int(r.lock.Fd()), syscall.LOCK_UN); err != nil && r.err == nil {
				r.err = err
			}
			if err := r.lock.Close(); err != nil && r.err == nil {
				r.err = err
			}
		}
	})
	return r.err
}
