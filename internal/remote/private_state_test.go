package remote

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPersistRejectsTempSymlinkWithoutTruncatingTarget(t *testing.T) {
	c := NewClient(Config{DataDir: t.TempDir()})
	if err := os.MkdirAll(c.PrivateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "unrelated")
	const original = "must survive"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, c.tempPath()); err != nil {
		t.Fatal(err)
	}
	if err := c.persist(PersistedState{}); err == nil {
		t.Fatal("persist accepted a symlink in place of its private temp file")
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != original {
		t.Fatalf("symlink target changed to %q", b)
	}
}
