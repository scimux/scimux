package asset

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteBlobThenResolveAndRead(t *testing.T) {
	root := t.TempDir()
	relPath, err := WriteBlob(root, "n1", "a_1", "sketch.png", []byte("bytes-here"))
	if err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if relPath != filepath.Join("n1", "a_1-sketch.png") {
		t.Fatalf("relPath = %q, want %q", relPath, filepath.Join("n1", "a_1-sketch.png"))
	}
	got, err := ReadBlob(root, "n1", relPath)
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if string(got) != "bytes-here" {
		t.Fatalf("got %q, want %q", got, "bytes-here")
	}
}

func TestWriteBlobPermissions(t *testing.T) {
	root := t.TempDir()
	relPath, err := WriteBlob(root, "n1", "a_1", "sketch.png", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	dirFi, err := os.Stat(filepath.Join(root, "n1"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirFi.Mode().Perm(); perm != 0o700 {
		t.Errorf("node dir perm = %o, want 0700", perm)
	}
	fileFi, err := os.Stat(filepath.Join(root, relPath))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileFi.Mode().Perm(); perm != 0o600 {
		t.Errorf("blob file perm = %o, want 0600", perm)
	}
}

// Asset IDs are collision-resistant, so the blob filename (id-derived) should
// never legitimately repeat; O_EXCL makes a repeat write fail loudly instead
// of silently overwriting an existing asset's bytes.
func TestWriteBlobRejectsCollision(t *testing.T) {
	root := t.TempDir()
	if _, err := WriteBlob(root, "n1", "a_1", "sketch.png", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteBlob(root, "n1", "a_1", "sketch.png", []byte("second")); err == nil {
		t.Fatal("want error on repeat write with the same id+name, got nil")
	}
	got, err := ReadBlob(root, "n1", filepath.Join("n1", "a_1-sketch.png"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Fatalf("collision must not overwrite: got %q, want %q", got, "first")
	}
}

func TestWriteBlobSanitizesName(t *testing.T) {
	root := t.TempDir()
	relPath, err := WriteBlob(root, "n1", "a_1", "../../../etc/evil", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if relPath != filepath.Join("n1", "a_1-evil") {
		t.Fatalf("relPath = %q, want sanitized name under n1/", relPath)
	}
}

func TestResolveBlobPathRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	if _, err := WriteBlob(root, "n1", "a_1", "sketch.png", []byte("x")); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveBlobPath(root, "n1", "../n1/a_1-sketch.png")
	if !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("err = %v, want ErrOutsideRoot", err)
	}
	_, err = ResolveBlobPath(root, "n1", "n1/../../etc/passwd")
	if !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("err = %v, want ErrOutsideRoot", err)
	}
}

// A node must never be able to reach another node's blob even by name, even
// though both live under the same assets root.
func TestResolveBlobPathRejectsCrossNode(t *testing.T) {
	root := t.TempDir()
	relPath, err := WriteBlob(root, "n2", "a_1", "sneak.png", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveBlobPath(root, "n1", relPath) // relPath is "n2/a_1-sneak.png"
	if !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("err = %v, want ErrOutsideRoot (cross-node reach)", err)
	}
}

func TestResolveBlobPathMissingFile(t *testing.T) {
	root := t.TempDir()
	_, err := ResolveBlobPath(root, "n1", filepath.Join("n1", "ghost.png"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveBlobPathRejectsDirectory(t *testing.T) {
	root := t.TempDir()
	if _, err := WriteBlob(root, "n1", "a_1", "sketch.png", []byte("x")); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveBlobPath(root, "n1", "n1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (directories are never servable)", err)
	}
}

func TestReadBlobMissingFile(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadBlob(root, "n1", filepath.Join("n1", "ghost.png")); err == nil {
		t.Fatal("want error for missing blob")
	}
}
