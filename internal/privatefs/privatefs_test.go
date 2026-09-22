package privatefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureDirTightensAndVerifiesMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("mode = %04o, want 0700", got)
	}
}

func TestEnsureFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "state")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFile(link, 0o600); err == nil {
		t.Fatal("symlink was accepted as a private file")
	}
}

func TestEnsureFileIfExistsAllowsMissing(t *testing.T) {
	if err := EnsureFileIfExists(filepath.Join(t.TempDir(), "absent"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureFileFailsClosedWhenPathCannotBeOpened(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can open mode-000 files")
	}
	path := filepath.Join(t.TempDir(), "closed")
	if err := os.WriteFile(path, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFile(path, 0o600); err == nil {
		t.Fatal("unopenable path was changed by name instead of failing closed")
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0 {
		t.Fatalf("failed open changed permissions to %04o, want 0000", got)
	}
}

func TestSecureOpenedFileDetectsPathReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SecureOpenedFile(path, f, 0o600); err == nil {
		t.Fatal("replaced pathname was accepted as the opened private file")
	}
}
