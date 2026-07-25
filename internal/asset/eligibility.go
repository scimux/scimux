// Package asset holds the path-eligibility core for session assets: the
// containment check that decides whether a local path an agent or user
// referenced may be ingested at all. This is deliberately separate from
// storage-mode selection (StorageMode) — size/type only ever choose how an
// eligible file is stored, never whether it is ingested. See
// upload-design.md, "Path Resolution Rules" and "Guaranteed Outcome".
package asset

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrNotFound means the path does not resolve to any file.
	ErrNotFound = errors.New("asset: path not found")
	// ErrNotRegularFile means the resolved target is a directory, device, or
	// other non-regular file — directories are never eligible, even via a
	// symlink that points at one.
	ErrNotRegularFile = errors.New("asset: not a regular file")
	// ErrOutsideRoot means the resolved path (after following symlinks)
	// falls outside every allowed root. This is the check that keeps a
	// reference like /etc/passwd from being ingested: "resolves to a
	// regular file" alone is not sufficient eligibility.
	ErrOutsideRoot = errors.New("asset: outside allowed roots")
)

// Resolve decides whether ref is eligible for ingestion as a session asset.
// ref may be absolute or relative; relative paths are resolved against dir
// (the node's working directory). Symlinks are followed to their final
// target, and eligibility is checked against the resolved target, not the
// link — a symlink inside an allowed root whose target lives outside every
// root is rejected. On success it returns the resolved absolute path and
// the file's size; StorageMode then decides inline vs blob storage, but
// never revisits eligibility.
func Resolve(ref, dir string, roots []string) (path string, size int64, err error) {
	p := ref
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p = filepath.Clean(p)

	if _, err := os.Lstat(p); err != nil {
		return "", 0, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}

	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	resolved = filepath.Clean(resolved)

	fi, err := os.Stat(resolved)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	if !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%w: %s", ErrNotRegularFile, ref)
	}

	for _, root := range roots {
		rroot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rroot = filepath.Clean(rroot)
		if withinRoot(resolved, rroot) {
			return resolved, fi.Size(), nil
		}
	}
	return "", 0, fmt.Errorf("%w: %s", ErrOutsideRoot, ref)
}

// withinRoot reports whether path is root itself or a descendant of it.
// A plain strings.HasPrefix(path, root) would let a root "/a/b" wrongly
// match a sibling "/a/bc/…"; joining the separator closes that gap.
func withinRoot(path, root string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// StorageMode picks inline vs blob storage for an already-eligible file. It
// never rejects: an oversized file still gets a mode (blob) because size
// governs storage, not whether the file is ingested at all.
func StorageMode(size, inlineCap int64) string {
	if size <= inlineCap {
		return "inline"
	}
	return "blob"
}
