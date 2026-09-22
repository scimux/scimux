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

// Open decides whether ref is eligible for ingestion as a session asset and
// returns the already-checked descriptor. The caller must read this descriptor,
// never reopen path: os.Root keeps every symlink traversal beneath the approved
// root and closes the check/use pathname race.
// ref may be absolute or relative; relative paths are resolved against dir
// (the node's working directory). Symlinks are followed to their final
// target, and eligibility is checked against the resolved target, not the
// link — a symlink inside an allowed root whose target lives outside every
// root is rejected. On success it returns the descriptor, resolved absolute
// path, and descriptor size.
func Open(ref, dir string, roots []string) (file *os.File, path string, size int64, err error) {
	p := ref
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p, err = filepath.Abs(filepath.Clean(p))
	if err != nil {
		return nil, "", 0, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}

	if _, err := os.Lstat(p); err != nil {
		return nil, "", 0, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}

	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, "", 0, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	resolved, err = filepath.Abs(filepath.Clean(resolved))
	if err != nil {
		return nil, "", 0, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}

	for _, root := range roots {
		rroot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rroot, err = filepath.Abs(filepath.Clean(rroot))
		if err != nil || !withinRoot(resolved, rroot) {
			continue
		}

		// Open and pin the approved root before opening the candidate. Stat the
		// root on both sides of OpenRoot so replacing the root pathname cannot
		// silently redirect the capability to another tree.
		before, err := os.Stat(rroot)
		if err != nil || !before.IsDir() {
			continue
		}
		cap, err := os.OpenRoot(rroot)
		if err != nil {
			continue
		}
		rootInfo, statErr := cap.Stat(".")
		if statErr != nil || !os.SameFile(before, rootInfo) {
			cap.Close()
			continue
		}
		rel, err := filepath.Rel(rroot, resolved)
		if err != nil {
			cap.Close()
			continue
		}
		f, err := cap.Open(rel)
		cap.Close()
		if err != nil {
			return nil, "", 0, fmt.Errorf("%w: %s", ErrNotFound, ref)
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, "", 0, fmt.Errorf("%w: %s", ErrNotFound, ref)
		}
		if !fi.Mode().IsRegular() {
			f.Close()
			return nil, "", 0, fmt.Errorf("%w: %s", ErrNotRegularFile, ref)
		}
		return f, resolved, fi.Size(), nil
	}
	return nil, "", 0, fmt.Errorf("%w: %s", ErrOutsideRoot, ref)
}

// Resolve is the metadata-only compatibility surface. Security-sensitive
// ingestion must use Open and consume its returned descriptor.
func Resolve(ref, dir string, roots []string) (path string, size int64, err error) {
	f, path, size, err := Open(ref, dir, roots)
	if f != nil {
		_ = f.Close()
	}
	return path, size, err
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
