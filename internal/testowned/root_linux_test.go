//go:build linux && !testowned_unsupported

package testowned

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type rootFakeInfo struct {
	mode  os.FileMode
	value any
}

func (f rootFakeInfo) Name() string       { return "synthetic" }
func (f rootFakeInfo) Size() int64        { return 0 }
func (f rootFakeInfo) Mode() os.FileMode  { return f.mode }
func (f rootFakeInfo) ModTime() time.Time { return time.Time{} }
func (f rootFakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f rootFakeInfo) Sys() any           { return f.value }

func TestRootInfoDecisions(t *testing.T) {
	if !RootSupported() {
		t.Fatal("Linux test root support unavailable")
	}
	good := &syscall.Stat_t{Uid: uint32(os.Getuid()), Dev: 3}
	wrong := &syscall.Stat_t{Uid: uint32(os.Getuid() + 1), Dev: 3}
	for _, tc := range []struct {
		info os.FileInfo
		ok   bool
	}{
		{nil, false}, {rootFakeInfo{mode: 0600, value: good}, false},
		{rootFakeInfo{mode: os.ModeDir | 0755, value: good}, false},
		{rootFakeInfo{mode: os.ModeDir | 0700, value: nil}, false},
		{rootFakeInfo{mode: os.ModeDir | 0700, value: wrong}, false},
		{rootFakeInfo{mode: os.ModeDir | 0700, value: good}, true},
	} {
		dev, err := rootInfo(tc.info)
		if (err == nil) != tc.ok || (tc.ok && dev != 3) {
			t.Fatalf("root info device=%d error=%v", dev, err)
		}
	}
}

func TestBindRootAndHandle(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "runtime")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := BindRoot(parent, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][2]string{{"relative", root}, {parent, "relative"}, {parent + "/.", root}, {parent, root + "/."}, {parent, parent}, {parent, filepath.Join(parent, "other", "child")}} {
		if _, err := BindRoot(paths[0], paths[1]); err == nil {
			t.Fatalf("invalid path accepted: %v", paths)
		}
	}
	if _, err := BindRoot(filepath.Join(parent, "missing"), filepath.Join(parent, "missing", "root")); err == nil {
		t.Fatal("missing parent accepted")
	}
	if _, err := BindRoot(parent, filepath.Join(parent, "missing")); err == nil {
		t.Fatal("missing root accepted")
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := BindRoot(parent, link); err == nil {
		t.Fatal("symlink root accepted")
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := BindRoot(parent, root); err == nil {
		t.Fatal("public root accepted")
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := BindRoot(parent, root); err == nil {
		t.Fatal("public parent accepted")
	}
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	handle, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := r.VerifyHandle(handle); err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyHandle(nil); err == nil {
		t.Fatal("nil handle accepted")
	}
	other, err := os.Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyHandle(other); err == nil {
		t.Fatal("wrong handle accepted")
	}
	_ = other.Close()
	if err := r.VerifyHandle(other); err == nil {
		t.Fatal("closed handle accepted")
	}
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyHandle(handle); err == nil {
		t.Fatal("replacement root accepted")
	}
	if err := r.RemoveAfter(nil, Result{Clean: true, Category: "clean"}); err == nil {
		t.Fatal("replacement root removed")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("replacement root not retained")
	}
}

func TestRootDeviceAndRemovalFailures(t *testing.T) {
	good := rootFakeInfo{mode: os.ModeDir | 0700, value: &syscall.Stat_t{Uid: uint32(os.Getuid()), Dev: 1}}
	other := rootFakeInfo{mode: os.ModeDir | 0700, value: &syscall.Stat_t{Uid: uint32(os.Getuid()), Dev: 2}}
	if _, err := bindRoot("/synthetic", "/synthetic/root", func(p string) (os.FileInfo, error) {
		if p == "/synthetic" {
			return good, nil
		}
		return other, nil
	}); err == nil {
		t.Fatal("different device accepted")
	}
	if err := (*Root)(nil).verify(os.Lstat); err == nil {
		t.Fatal("nil registration accepted")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "runtime")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := BindRoot(parent, root)
	if err != nil {
		t.Fatal(err)
	}
	synthetic := errors.New("synthetic")
	if err := r.remove(os.Lstat, func(string) error { return synthetic }); !errors.Is(err, synthetic) {
		t.Fatalf("remove error: %v", err)
	}
	for _, tc := range []struct {
		wait   error
		result Result
	}{
		{synthetic, Result{Clean: true, Category: "clean"}},
		{nil, Result{Category: "clean"}},
		{nil, Result{Clean: true, Category: "wait"}},
	} {
		if err := r.RemoveAfter(tc.wait, tc.result); err == nil {
			t.Fatal("invalid proof accepted")
		}
		if _, err := os.Lstat(root); err != nil {
			t.Fatal("unproven root removed")
		}
	}
	if err := r.RemoveAfter(nil, Result{Clean: true, Category: "clean"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("root remains")
	}
	called := false
	if err := r.remove(os.Lstat, func(string) error { called = true; return nil }); err == nil || called {
		t.Fatal("unverified root removed")
	}
}

func TestCleanupProofIndependentOfRunnerAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name             string
		actual, expected int
		accepted         bool
	}{
		{"normal", 0, 0, true}, {"nonzero failure", 1, 0, false},
		{"expected failure", 1, 1, true}, {"late outer abort", 0, 143, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			if err := os.Chmod(parent, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "runtime")
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			r, err := BindRoot(parent, path)
			if err != nil {
				t.Fatal(err)
			}
			result := Result{Clean: true, Category: "clean", RunnerExit: tc.actual}
			if (CheckRunner(result, tc.expected) == nil) != tc.accepted {
				t.Fatal("runner acceptance mismatch")
			}
			if err := r.RemoveAfter(nil, result); err != nil {
				t.Fatalf("clean quiescent root retained: %v", err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("clean root remains")
			}
		})
	}
}

func TestRemoveUnstarted(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "runtime")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := BindRoot(parent, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveUnstarted(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unstarted root remains")
	}
}

func TestRootParentReplacementRetained(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	path := filepath.Join(parent, "runtime")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := BindRoot(parent, path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := os.Rename(parent, parent+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyHandle(handle); err == nil {
		t.Fatal("replacement parent accepted")
	}
	if err := r.RemoveAfter(nil, Result{Clean: true, Category: "clean"}); err == nil {
		t.Fatal("replacement parent removed")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("replacement root not retained")
	}
}
