// Package storagebudget enforces aggregate durable-storage limits shared by
// the muxer and long-lived session-worker processes.
//
// Budgets never delete or truncate data. A write that would cross a configured
// limit fails before opening its destination; the caller can surface that
// failure while all existing history remains intact.
package storagebudget

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/scimux/scimux/internal/privatefs"
)

const (
	// DefaultMinFreeBytes protects a fresh installation without imposing an
	// opinionated history-retention limit. Users can configure aggregate and
	// per-node limits independently in settings.json.
	DefaultMinFreeBytes int64 = 64 << 20
	settingsMax               = 1 << 16
	lockName                  = ".storage.lock"
)

var ErrBudget = errors.New("storage budget exceeded")

type Policy struct {
	GlobalLimitBytes int64 `json:"storage_global_limit_bytes"`
	NodeLimitBytes   int64 `json:"storage_node_limit_bytes"`
	MinFreeBytes     int64 `json:"storage_min_free_bytes"`
}

type Status struct {
	UsedBytes        int64            `json:"used_bytes"`
	FreeBytes        int64            `json:"free_bytes"`
	GlobalLimitBytes int64            `json:"global_limit_bytes,omitempty"`
	NodeLimitBytes   int64            `json:"node_limit_bytes,omitempty"`
	MinFreeBytes     int64            `json:"min_free_bytes"`
	Writable         bool             `json:"writable"`
	Reason           string           `json:"reason,omitempty"`
	Nodes            map[string]int64 `json:"-"`
}

type limitError struct{ reason string }

func (e *limitError) Error() string { return "storage budget: " + e.reason }
func (e *limitError) Unwrap() error { return ErrBudget }

func DefaultPolicy() Policy { return Policy{MinFreeBytes: DefaultMinFreeBytes} }

// LoadPolicy reads only storage fields from the server-owned settings file.
// Missing or malformed settings retain the safe free-space default and leave
// optional byte budgets unlimited.
func LoadPolicy(dataDir string) Policy {
	p := DefaultPolicy()
	b, err := os.ReadFile(filepath.Join(dataDir, "settings.json"))
	if err != nil || len(b) == 0 || len(b) > settingsMax {
		return p
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return p
	}
	decodeNonNegative(raw, "storage_global_limit_bytes", &p.GlobalLimitBytes)
	decodeNonNegative(raw, "storage_node_limit_bytes", &p.NodeLimitBytes)
	decodeNonNegative(raw, "storage_min_free_bytes", &p.MinFreeBytes)
	return p
}

func decodeNonNegative(raw map[string]json.RawMessage, key string, dst *int64) {
	b, ok := raw[key]
	if !ok || string(b) == "null" {
		return
	}
	var n int64
	if json.Unmarshal(b, &n) == nil && n >= 0 {
		*dst = n
	}
}

// Reserve serializes the usage check and the caller's subsequent write across
// every scimux process using dataDir. The returned release must be called.
func Reserve(dataDir, nodeID string, incoming int64) (func(), error) {
	if incoming < 0 {
		return nil, fmt.Errorf("storage budget: negative reservation")
	}
	if dataDir == "" {
		return func() {}, nil
	}
	p := LoadPolicy(dataDir)
	// With no configured constraint there is nothing to reserve. Every enabled
	// constraint, including the default free-space floor, must otherwise share
	// the same inter-process lock so the successful check remains valid until
	// the caller completes its write.
	if p.GlobalLimitBytes == 0 && p.NodeLimitBytes == 0 && p.MinFreeBytes == 0 {
		return func() {}, nil
	}
	release, err := Lock(dataDir)
	if err != nil {
		return nil, err
	}
	if err := check(dataDir, nodeID, incoming, p); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// Lock joins a non-writing mutation to the same inter-process critical
// section as Reserve. Archive renames use it because a usage scan must see a
// managed object either at its live path or at its archive path, never miss it
// while it moves between the two. Lock deliberately performs no budget check:
// moving existing bytes must remain possible when a limit is already full.
func Lock(dataDir string) (func(), error) {
	if dataDir == "" {
		return func() {}, nil
	}
	if err := privatefs.EnsureDir(dataDir, 0o700); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dataDir, lockName)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := privatefs.SecureOpenedFile(lockPath, f, 0o600); err != nil {
		f.Close()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}
	return release, nil
}

// ReserveSession applies budgets only to the production sessions/<node>.jsonl
// layout. Standalone sessionlog users and package tests keep working without
// an invented data-root convention.
func ReserveSession(path string, incoming int64) (func(), error) {
	dir := filepath.Dir(path)
	if filepath.Base(dir) != "sessions" {
		return func() {}, nil
	}
	nodeID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return Reserve(filepath.Dir(dir), nodeID, incoming)
}

func check(dataDir, nodeID string, incoming int64, p Policy) error {
	if err := checkFree(dataDir, incoming, p.MinFreeBytes); err != nil {
		return err
	}
	if p.GlobalLimitBytes > 0 {
		used, _, err := managedUsage(dataDir)
		if err != nil {
			return err
		}
		if used > p.GlobalLimitBytes || incoming > p.GlobalLimitBytes-used {
			return &limitError{reason: fmt.Sprintf("global usage %d plus %d bytes exceeds limit %d", used, incoming, p.GlobalLimitBytes)}
		}
	}
	if p.NodeLimitBytes > 0 && nodeID != "" {
		used, err := nodeUsage(dataDir, nodeID)
		if err != nil {
			return err
		}
		if used > p.NodeLimitBytes || incoming > p.NodeLimitBytes-used {
			return &limitError{reason: fmt.Sprintf("node %q usage %d plus %d bytes exceeds limit %d", nodeID, used, incoming, p.NodeLimitBytes)}
		}
	}
	return nil
}

func checkFree(dataDir string, incoming, minimum int64) error {
	free, err := freeBytes(dataDir)
	if err != nil {
		return err
	}
	if minimum > 0 && (free < incoming || free-incoming < minimum) {
		return &limitError{reason: fmt.Sprintf("write needs %d bytes but only %d bytes are free (minimum reserve %d)", incoming, free, minimum)}
	}
	return nil
}

// Inspect returns an observational snapshot. It never mutates or deletes
// durable data and intentionally does not hold the write lock while scanning.
func Inspect(dataDir string) (Status, error) {
	p := LoadPolicy(dataDir)
	free, err := freeBytes(dataDir)
	if err != nil {
		return Status{}, err
	}
	used, nodes, err := managedUsage(dataDir)
	if err != nil {
		return Status{}, err
	}
	s := Status{
		UsedBytes: used, FreeBytes: free, GlobalLimitBytes: p.GlobalLimitBytes,
		NodeLimitBytes: p.NodeLimitBytes, MinFreeBytes: p.MinFreeBytes,
		Writable: true, Nodes: nodes,
	}
	if p.MinFreeBytes > 0 && free < p.MinFreeBytes {
		s.Writable = false
		s.Reason = "minimum free-space reserve reached"
	} else if p.GlobalLimitBytes > 0 && used >= p.GlobalLimitBytes {
		s.Writable = false
		s.Reason = "global storage limit reached"
	}
	return s, nil
}

func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return saturatingMul(int64(st.Bavail), int64(st.Bsize)), nil
}

func managedUsage(dataDir string) (int64, map[string]int64, error) {
	nodes := map[string]int64{}
	var total int64
	if info, err := os.Stat(filepath.Join(dataDir, "nodes.jsonl")); err == nil && info.Mode().IsRegular() {
		total = saturatingAdd(total, info.Size())
	} else if err != nil && !os.IsNotExist(err) {
		return 0, nil, err
	}
	for _, top := range []string{"sessions", "attachments", "assets"} {
		root := filepath.Join(dataDir, top)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) {
					return nil
				}
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total = saturatingAdd(total, info.Size())
			if node := liveNodeForPath(top, root, path); node != "" {
				nodes[node] = saturatingAdd(nodes[node], info.Size())
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return 0, nil, err
		}
	}
	return total, nodes, nil
}

func liveNodeForPath(top, root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 0 || parts[0] == "archive" {
		return ""
	}
	if top == "sessions" {
		if len(parts) != 1 || filepath.Ext(parts[0]) != ".jsonl" {
			return ""
		}
		return strings.TrimSuffix(parts[0], ".jsonl")
	}
	return parts[0]
}

func nodeUsage(dataDir, nodeID string) (int64, error) {
	var total int64
	paths := []string{
		filepath.Join(dataDir, "sessions", nodeID+".jsonl"),
		filepath.Join(dataDir, "attachments", nodeID),
		filepath.Join(dataDir, "assets", nodeID),
	}
	for _, root := range paths {
		err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) {
					return nil
				}
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Type().IsRegular() {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				total = saturatingAdd(total, info.Size())
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return 0, err
		}
	}
	return total, nil
}

func saturatingAdd(a, b int64) int64 {
	if b > 0 && a > int64(^uint64(0)>>1)-b {
		return int64(^uint64(0) >> 1)
	}
	return a + b
}

func saturatingMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	max := int64(^uint64(0) >> 1)
	if a > max/b {
		return max
	}
	return a * b
}
