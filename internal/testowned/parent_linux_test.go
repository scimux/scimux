//go:build linux && !testowned_unsupported

package testowned

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func makeTestParent(t *testing.T) (string, *Parent) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	p, err := BindParent(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, p
}

func TestParentBindingAndEmptyCleanup(t *testing.T) {
	for _, path := range []string{"relative", "/synthetic/."} {
		if _, err := BindParent(path); err == nil {
			t.Fatal("invalid parent path accepted")
		}
	}
	if _, err := BindParent("/missing-synthetic-parent"); err == nil {
		t.Fatal("missing parent accepted")
	}
	path, p := makeTestParent(t)
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := BindParent(path); err == nil {
		t.Fatal("public parent accepted")
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveUnstarted(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty parent remains")
	}
	if err := (*Parent)(nil).RemoveUnstarted(); err == nil {
		t.Fatal("nil parent cleanup accepted")
	}
	if err := (*Parent)(nil).RemoveParent(); err == nil {
		t.Fatal("nil parent removal accepted")
	}
	if err := (*Parent)(nil).RegisterChild("/synthetic"); err == nil {
		t.Fatal("nil parent registration accepted")
	}
}

func TestParentChildRegistrationAndCleanup(t *testing.T) {
	path, p := makeTestParent(t)
	child := filepath.Join(path, "runtime")
	if err := p.RegisterChild(child); err == nil {
		t.Fatal("missing child accepted")
	}
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveParent(); err == nil {
		t.Fatal("nonempty parent removed")
	}
	if err := p.RegisterChild(child); err != nil {
		t.Fatal(err)
	}
	if err := p.RegisterChild(child); err == nil {
		t.Fatal("duplicate child accepted")
	}
	if err := p.RemoveUnstarted(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("registered parent remains")
	}
}

func TestParentReplacementAndUnknownChildRetained(t *testing.T) {
	path, p := makeTestParent(t)
	child := filepath.Join(path, "runtime")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if err := p.RegisterChild(child); err == nil {
		t.Fatal("replacement parent accepted")
	}
	if err := p.RemoveParent(); err == nil {
		t.Fatal("replacement parent removed")
	}
	if err := p.RemoveUnstarted(); err == nil {
		t.Fatal("replacement parent cleanup accepted")
	}
	if _, err := os.Lstat(child); err != nil {
		t.Fatal("replacement child not retained")
	}
}

func TestParentInjectedSetupAndCleanupFailures(t *testing.T) {
	synthetic := errors.New("synthetic")
	path, p := makeTestParent(t)
	child := filepath.Join(path, "runtime")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := bindParent(path, func(string) (os.FileInfo, error) { return nil, synthetic }); !errors.Is(err, synthetic) {
		t.Fatal("parent read failure hidden")
	}
	if err := p.registerChild(child, func(string) (os.FileInfo, error) { return nil, synthetic }); !errors.Is(err, synthetic) {
		t.Fatal("child registration failure hidden")
	}
	if err := p.removeParent(func(string) (os.FileInfo, error) { return nil, synthetic }, os.Remove); !errors.Is(err, synthetic) {
		t.Fatal("parent validation failure hidden")
	}
	if err := p.removeParent(os.Lstat, func(string) error { return synthetic }); !errors.Is(err, synthetic) {
		t.Fatal("parent removal failure hidden")
	}
	if err := p.RegisterChild(child); err != nil {
		t.Fatal(err)
	}
	if err := p.removeUnstarted(os.Lstat, func(string) error { return synthetic }, os.Remove); !errors.Is(err, synthetic) {
		t.Fatal("child removal failure hidden")
	}
	if _, err := os.Lstat(child); err != nil {
		t.Fatal("child not retained on failure")
	}
	if err := p.removeUnstarted(os.Lstat, os.RemoveAll, func(string) error { return synthetic }); !errors.Is(err, synthetic) {
		t.Fatal("parent removal failure hidden")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("parent not retained on failure")
	}
	if err := p.RemoveUnstarted(); err != nil {
		t.Fatalf("retry parent removal: %v", err)
	}
}
