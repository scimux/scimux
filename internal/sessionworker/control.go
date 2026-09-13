package sessionworker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
)

const maxLocatorSize = 8 << 10

var (
	validNodeID    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
	ErrWorkerOwned = errors.New("session worker: node already has a live worker")
)

type Locator struct {
	Identity
	NodeID     string `json:"node_id"`
	PID        int    `json:"pid"`
	Executable string `json:"executable,omitempty"`
	Link       Link   `json:"link"`
}

func (l Locator) valid() bool {
	return validNodeID.MatchString(l.NodeID) && l.WorkerID != "" && l.Agent != "" && l.PID > 0 && l.Link.Socket != "" && l.Link.Token != ""
}

type Registration struct {
	nodeID      string
	lock        *os.File
	locatorPath string
	payload     []byte
	once        sync.Once
	err         error
}

func workerControlDir(dataDir string) string { return filepath.Join(dataDir, "control", "workers") }

func workerLocatorPath(dataDir, nodeID string) string {
	return filepath.Join(workerControlDir(dataDir), nodeID+".json")
}

func workerLockPath(dataDir, nodeID string) string {
	return filepath.Join(workerControlDir(dataDir), nodeID+".lock")
}

// Claim holds a per-node kernel lock for the worker's complete lifetime. The
// JSON file is only an inspectable locator; it never decides ownership.
func Claim(dataDir, nodeID string) (*Registration, error) {
	if dataDir == "" {
		return nil, errors.New("session worker: empty data directory")
	}
	if !validNodeID.MatchString(nodeID) {
		return nil, errors.New("session worker: unsafe node id")
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		return nil, fmt.Errorf("session worker: inspect data directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("session worker: data directory is not owner-only")
	}
	dir := workerControlDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session worker: create control directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session worker: protect control directory: %w", err)
	}
	lock, err := os.OpenFile(workerLockPath(dataDir, nodeID), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("session worker: open lifetime lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrWorkerOwned
		}
		return nil, fmt.Errorf("session worker: lock node: %w", err)
	}
	path := workerLocatorPath(dataDir, nodeID)
	if stale, err := os.Lstat(path); err == nil {
		if !stale.Mode().IsRegular() {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
			return nil, errors.New("session worker: existing locator is not a regular file")
		}
		if err := os.Remove(path); err != nil {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
			return nil, fmt.Errorf("session worker: remove stale locator: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, fmt.Errorf("session worker: inspect stale locator: %w", err)
	}
	return &Registration{nodeID: nodeID, lock: lock, locatorPath: path}, nil
}

func (r *Registration) Publish(locator Locator) error {
	if r == nil || r.lock == nil {
		return errors.New("session worker: publish without ownership")
	}
	if !locator.valid() || locator.NodeID != r.nodeID {
		return errors.New("session worker: incomplete or mismatched locator")
	}
	payload, err := json.Marshal(locator)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(r.locatorPath), ".worker-*.tmp")
	if err != nil {
		return fmt.Errorf("session worker: create locator: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(payload); err != nil {
		return fmt.Errorf("session worker: write locator: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session worker: close locator: %w", err)
	}
	if err := os.Rename(tmpPath, r.locatorPath); err != nil {
		return fmt.Errorf("session worker: publish locator: %w", err)
	}
	r.payload = payload
	return nil
}

func Discover(dataDir, nodeID string) (Locator, error) {
	if !validNodeID.MatchString(nodeID) {
		return Locator{}, errors.New("session worker: unsafe node id")
	}
	path := workerLocatorPath(dataDir, nodeID)
	info, err := os.Lstat(path)
	if err != nil {
		return Locator{}, fmt.Errorf("session worker: read locator: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return Locator{}, errors.New("session worker: locator is not an owner-only regular file")
	}
	if info.Size() > maxLocatorSize {
		return Locator{}, errors.New("session worker: locator is too large")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return Locator{}, fmt.Errorf("session worker: read locator: %w", err)
	}
	var locator Locator
	if err := json.Unmarshal(payload, &locator); err != nil {
		return Locator{}, fmt.Errorf("session worker: decode locator: %w", err)
	}
	if !locator.valid() || locator.NodeID != nodeID {
		return Locator{}, errors.New("session worker: incomplete or mismatched locator")
	}
	return locator, nil
}

// List returns every valid locator and joins errors for malformed entries. A
// damaged locator must not prevent `scimux stop` from reaching the others.
func List(dataDir string) ([]Locator, error) {
	dir := workerControlDir(dataDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var locators []Locator
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		nodeID := strings.TrimSuffix(entry.Name(), ".json")
		locator, err := Discover(dataDir, nodeID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", entry.Name(), err))
			continue
		}
		locators = append(locators, locator)
	}
	sort.Slice(locators, func(i, j int) bool { return locators[i].NodeID < locators[j].NodeID })
	return locators, errors.Join(errs...)
}

func (r *Registration) Close() error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		if len(r.payload) > 0 {
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
