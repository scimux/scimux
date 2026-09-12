package app

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinWorkerExecutableIsContentAddressedAndOwnerExecutable(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "scimux")
	content := []byte("one immutable scimux generation")
	if err := os.WriteFile(source, content, 0o755); err != nil {
		t.Fatal(err)
	}
	path, err := pinWorkerExecutable(source, data)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	if want := hex.EncodeToString(digest[:]); !strings.Contains(filepath.Base(path), want) {
		t.Fatalf("pin path = %q, want digest %s", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(content) {
		t.Fatalf("pinned bytes = %q, %v", got, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("pin mode = %v, %v", info, err)
	}
	again, err := pinWorkerExecutable(source, data)
	if err != nil || again != path {
		t.Fatalf("second pin = %q, %v; want %q", again, err, path)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("pin directory = %v, %v", entries, err)
	}
}

func TestPinWorkerExecutableRejectsUnsafeInputs(t *testing.T) {
	data := t.TempDir()
	if _, err := pinWorkerExecutable("", data); err == nil {
		t.Fatal("pin accepted empty executable")
	}
	if _, err := pinWorkerExecutable(filepath.Join(t.TempDir(), "missing"), data); err == nil {
		t.Fatal("pin accepted missing executable")
	}
	directory := t.TempDir()
	if _, err := pinWorkerExecutable(directory, data); err == nil {
		t.Fatal("pin accepted directory as executable")
	}
	source := filepath.Join(t.TempDir(), "scimux")
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	insecure := t.TempDir()
	if err := os.Chmod(insecure, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := pinWorkerExecutable(source, insecure); err == nil {
		t.Fatal("pin accepted insecure data directory")
	}
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pinWorkerExecutable(source, blocked); err == nil {
		t.Fatal("pin accepted a file as data directory")
	}
	if _, err := pinWorkerExecutable(source, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("pin accepted a missing data directory")
	}
	blockedControl := t.TempDir()
	if err := os.Chmod(blockedControl, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blockedControl, "control"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pinWorkerExecutable(source, blockedControl); err == nil {
		t.Fatal("pin accepted a non-directory control path")
	}
}
