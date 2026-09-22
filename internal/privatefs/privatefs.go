// Package privatefs establishes and verifies owner-only filesystem objects.
//
// The checks are intentionally descriptor-backed. A successful Chmod alone is
// not enough for private state: the pathname could name a symlink or be
// replaced between inspection and use. Callers get an error unless the opened
// object is the same owner-controlled regular file or directory that was
// inspected, and its final permissions match the requested mode.
package privatefs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// EnsureDir creates path when necessary, then makes and verifies it as an
// owner-owned directory with exactly perm permissions.
func EnsureDir(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	return securePath(path, perm, true)
}

// EnsureFileIfExists makes and verifies an existing file as an owner-owned
// regular file with exactly perm permissions. A missing file is not an error.
func EnsureFileIfExists(path string, perm os.FileMode) error {
	err := securePath(path, perm, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// EnsureFile makes and verifies an existing owner-owned regular file.
func EnsureFile(path string, perm os.FileMode) error {
	return securePath(path, perm, false)
}

// SecureOpenedFile verifies that f is the regular file currently named by
// path, applies perm through the descriptor, and re-checks the result. It is
// used when a caller must create/open a file with particular flags before
// taking a lock or publishing data.
func SecureOpenedFile(path string, f *os.File, perm os.FileMode) error {
	if f == nil {
		return fmt.Errorf("privatefs: %s: nil file", path)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("privatefs: inspect %s: %w", path, err)
	}
	if err := validateInfo(path, before, false); err != nil {
		return err
	}
	opened, err := f.Stat()
	if err != nil {
		return fmt.Errorf("privatefs: stat open %s: %w", path, err)
	}
	if !os.SameFile(before, opened) {
		return fmt.Errorf("privatefs: %s changed while it was opened", path)
	}
	if err := validateInfo(path, opened, false); err != nil {
		return err
	}
	if err := f.Chmod(perm.Perm()); err != nil {
		return fmt.Errorf("privatefs: chmod %s: %w", path, err)
	}
	final, err := f.Stat()
	if err != nil {
		return fmt.Errorf("privatefs: recheck %s: %w", path, err)
	}
	return verifyMode(path, final, perm)
}

func securePath(path string, perm os.FileMode, directory bool) error {
	before, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("privatefs: inspect %s: %w", path, err)
	}
	if err := validateInfo(path, before, directory); err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		// Never chmod an unopened pathname. In an attacker-writable parent it
		// could be swapped for a symlink between Lstat and Chmod, changing the
		// target before an identity check can report the race.
		return fmt.Errorf("privatefs: open %s: %w", path, err)
	}
	defer f.Close()

	opened, err := f.Stat()
	if err != nil {
		return fmt.Errorf("privatefs: stat open %s: %w", path, err)
	}
	if !os.SameFile(before, opened) {
		return fmt.Errorf("privatefs: %s changed while it was opened", path)
	}
	if err := validateInfo(path, opened, directory); err != nil {
		return err
	}
	if err := f.Chmod(perm.Perm()); err != nil {
		return fmt.Errorf("privatefs: chmod %s: %w", path, err)
	}
	final, err := f.Stat()
	if err != nil {
		return fmt.Errorf("privatefs: recheck %s: %w", path, err)
	}
	return verifyMode(path, final, perm)
}

func validateInfo(path string, info os.FileInfo, directory bool) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("privatefs: %s is a symlink", path)
	}
	if directory {
		if !info.IsDir() {
			return fmt.Errorf("privatefs: %s is not a directory", path)
		}
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("privatefs: %s is not a regular file", path)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("privatefs: %s ownership is unavailable", path)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("privatefs: %s is owned by uid %d, want %d", path, st.Uid, os.Geteuid())
	}
	return nil
}

func verifyMode(path string, info os.FileInfo, perm os.FileMode) error {
	if info.Mode().Perm() != perm.Perm() {
		return fmt.Errorf("privatefs: %s permissions are %04o, want %04o", path, info.Mode().Perm(), perm.Perm())
	}
	return nil
}
