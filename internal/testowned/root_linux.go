//go:build linux && !testowned_unsupported

package testowned

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Root binds an isolated test runtime directory and its private parent.
type Root struct {
	parent, path     string
	parentInfo, info os.FileInfo
}

// RootSupported reports whether this build includes the Linux test proof.
func RootSupported() bool { return true }

func rootInfo(info os.FileInfo) (uint64, error) {
	if info == nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return 0, errors.New("invalid test root")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return 0, errors.New("unknown test root owner")
	}
	return uint64(stat.Dev), nil
}

func bindRoot(parent, path string, lstat func(string) (os.FileInfo, error)) (*Root, error) {
	if !filepath.IsAbs(parent) || !filepath.IsAbs(path) || filepath.Clean(parent) != parent || filepath.Clean(path) != path || filepath.Dir(path) != parent {
		return nil, errors.New("invalid test root path")
	}
	p, err := lstat(parent)
	if err != nil {
		return nil, err
	}
	pdev, err := rootInfo(p)
	if err != nil {
		return nil, err
	}
	r, err := lstat(path)
	if err != nil {
		return nil, err
	}
	rdev, err := rootInfo(r)
	if err != nil {
		return nil, err
	}
	if pdev != rdev {
		return nil, errors.New("test root device changed")
	}
	return &Root{parent: parent, path: path, parentInfo: p, info: r}, nil
}

// BindRoot registers an existing 0700 root immediately after creation.
func BindRoot(parent, path string) (*Root, error) { return bindRoot(parent, path, os.Lstat) }

func (r *Root) verify(lstat func(string) (os.FileInfo, error)) error {
	if r == nil {
		return errors.New("missing test root")
	}
	current, err := bindRoot(r.parent, r.path, lstat)
	if err != nil {
		return err
	}
	if !os.SameFile(r.parentInfo, current.parentInfo) || !os.SameFile(r.info, current.info) {
		return errors.New("test root identity changed")
	}
	return nil
}

// VerifyHandle checks a caller-supplied directory handle against registration.
func (r *Root) VerifyHandle(handle *os.File) error {
	if err := r.verify(os.Lstat); err != nil {
		return err
	}
	if handle == nil {
		return errors.New("missing test root handle")
	}
	info, err := handle.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(r.info, info) {
		return errors.New("test root handle mismatch")
	}
	return nil
}

func (r *Root) remove(lstat func(string) (os.FileInfo, error), removeAll func(string) error) error {
	if err := r.verify(lstat); err != nil {
		return err
	}
	return removeAll(r.path)
}

// RemoveAfter requires the direct supervisor wait and its fixed proof before
// removing the exact registered root.
func (r *Root) RemoveAfter(waitErr error, result Result) error {
	if waitErr != nil || !result.Clean || result.Category != "clean" {
		return errors.New("test supervisor proof unavailable")
	}
	return r.remove(os.Lstat, os.RemoveAll)
}

// RemoveUnstarted is only for a launch that never started a child.
func (r *Root) RemoveUnstarted() error { return r.remove(os.Lstat, os.RemoveAll) }

// Parent records an isolated runtime parent before any child is allocated.
type Parent struct {
	path  string
	info  os.FileInfo
	child *Root
}

func bindParent(path string, lstat func(string) (os.FileInfo, error)) (*Parent, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("invalid test parent path")
	}
	info, err := lstat(path)
	if err != nil {
		return nil, err
	}
	if _, err := rootInfo(info); err != nil {
		return nil, err
	}
	return &Parent{path: path, info: info}, nil
}

// BindParent registers an existing private parent immediately after creation.
func BindParent(path string) (*Parent, error) { return bindParent(path, os.Lstat) }

func (p *Parent) registerChild(path string, lstat func(string) (os.FileInfo, error)) error {
	if p == nil || p.child != nil {
		return errors.New("test parent unavailable")
	}
	child, err := bindRoot(p.path, path, lstat)
	if err != nil {
		return err
	}
	if !os.SameFile(p.info, child.parentInfo) {
		return errors.New("test parent identity changed")
	}
	p.child = child
	return nil
}

// RegisterChild records the just-created child before further initialization.
func (p *Parent) RegisterChild(path string) error { return p.registerChild(path, os.Lstat) }

func (p *Parent) removeParent(lstat func(string) (os.FileInfo, error), remove func(string) error) error {
	if p == nil {
		return errors.New("missing test parent")
	}
	current, err := bindParent(p.path, lstat)
	if err != nil {
		return err
	}
	if !os.SameFile(p.info, current.info) {
		return errors.New("test parent identity changed")
	}
	return remove(p.path)
}

// RemoveParent removes only the original empty private parent.
func (p *Parent) RemoveParent() error { return p.removeParent(os.Lstat, os.Remove) }

func (p *Parent) removeUnstarted(lstat func(string) (os.FileInfo, error), removeAll, remove func(string) error) error {
	if p == nil {
		return errors.New("missing test parent")
	}
	if p.child != nil {
		if err := p.child.remove(lstat, removeAll); err != nil {
			return err
		}
		p.child = nil
	}
	return p.removeParent(lstat, remove)
}

// RemoveUnstarted removes the registered runtime and parent only before launch.
func (p *Parent) RemoveUnstarted() error { return p.removeUnstarted(os.Lstat, os.RemoveAll, os.Remove) }
