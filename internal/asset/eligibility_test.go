package asset

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolve_AbsoluteUnderRoot(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "sketch.png")
	writeFile(t, f, []byte("hello"))

	got, size, err := Resolve(f, root, []string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != f {
		t.Fatalf("got %q, want %q", got, f)
	}
	if size != 5 {
		t.Fatalf("size = %d, want 5", size)
	}
}

func TestResolve_OutsideAllRoots(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	f := filepath.Join(outside, "passwd")
	writeFile(t, f, []byte("secret"))

	_, _, err := Resolve(f, root, []string{root})
	if !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("err = %v, want ErrOutsideRoot", err)
	}
}

func TestResolve_RelativeUnderDir(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "results", "report.md")
	writeFile(t, f, []byte("report"))

	got, _, err := Resolve("results/report.md", root, []string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != f {
		t.Fatalf("got %q, want %q", got, f)
	}
}

func TestResolve_RelativeEscapesRoot(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "work")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), []byte("x"))

	rel, err := filepath.Rel(sub, filepath.Join(outside, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Resolve(rel, sub, []string{root})
	if !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("err = %v, want ErrOutsideRoot", err)
	}
}

func TestResolve_DirectoryNeverEligible(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "subdir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err := Resolve(dir, root, []string{root})
	if !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("err = %v, want ErrNotRegularFile", err)
	}
}

func TestResolve_MissingFile(t *testing.T) {
	root := t.TempDir()
	_, _, err := Resolve(filepath.Join(root, "nope.png"), root, []string{root})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestOpenUnreadableParentDoesNotClaimFileIsMissing(t *testing.T) {
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	path := filepath.Join(locked, "report.md")
	writeFile(t, path, []byte("present"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if _, err := os.Lstat(path); err == nil {
		t.Skip("current user can stat through a mode-000 directory")
	} else if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inaccessible file appeared absent: %v", err)
	}
	_, _, _, err := Open(path, root, []string{root})
	if !errors.Is(err, ErrUnreadable) || errors.Is(err, ErrNotFound) {
		t.Fatalf("Open error = %v, want unreadable rather than missing", err)
	}
	link := filepath.Join(root, "alias.md")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = Open(link, root, []string{root})
	if !errors.Is(err, ErrUnreadable) || errors.Is(err, ErrNotFound) {
		t.Fatalf("Open symlink error = %v, want unreadable rather than missing", err)
	}
}

func TestResolve_SymlinkWithinRootToRegularFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real.png")
	writeFile(t, target, []byte("bytes"))
	link := filepath.Join(root, "link.png")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	got, size, err := Resolve(link, root, []string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != target {
		t.Fatalf("got %q, want resolved target %q", got, target)
	}
	if size != 5 {
		t.Fatalf("size = %d, want 5", size)
	}
}

func TestResolve_SymlinkEscapingRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.png")
	writeFile(t, target, []byte("bytes"))
	link := filepath.Join(root, "link.png")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	_, _, err := Resolve(link, root, []string{root})
	if !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("err = %v, want ErrOutsideRoot (symlink inside root, target outside)", err)
	}
}

func TestResolve_SymlinkToDirectory(t *testing.T) {
	root := t.TempDir()
	targetDir := filepath.Join(root, "realdir")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linkdir")
	if err := os.Symlink(targetDir, link); err != nil {
		t.Fatal(err)
	}

	_, _, err := Resolve(link, root, []string{root})
	if !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("err = %v, want ErrNotRegularFile", err)
	}
}

// TestResolve_RootPrefixNotSubstring guards against a naive strings.HasPrefix
// containment check: a root "<tmp>/b" must not swallow a sibling "<tmp>/bc/…".
func TestResolve_RootPrefixNotSubstring(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "b")
	sibling := filepath.Join(base, "bc")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sibling, "file.png")
	writeFile(t, f, []byte("x"))

	_, _, err := Resolve(f, sibling, []string{root})
	if !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("err = %v, want ErrOutsideRoot (sibling dir must not match root prefix)", err)
	}
}

func TestResolve_RootItselfEligible(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "at-root.png")
	writeFile(t, f, []byte("x"))
	// root passed without trailing separator; file directly inside it.
	got, _, err := Resolve(f, root, []string{root})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != f {
		t.Fatalf("got %q, want %q", got, f)
	}
}

func TestResolve_MultipleRootsSecondMatches(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	f := filepath.Join(rootB, "gen.png")
	writeFile(t, f, []byte("x"))

	got, _, err := Resolve(f, rootA, []string{rootA, rootB})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != f {
		t.Fatalf("got %q, want %q", got, f)
	}
}

func TestOpenPinsEligibleDescriptorAcrossPathReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "result.txt")
	writeFile(t, path, []byte("approved"))
	f, _, _, err := Open(path, root, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, []byte("replacement"))
	got, err := os.ReadFile(path + ".old")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(got))
	if _, err := f.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "approved" {
		t.Fatalf("descriptor read %q, want original approved bytes", buf)
	}
}

func TestOpenExternalPinsRegularFileWithoutTreatingRootAsTrusted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outside.txt")
	writeFile(t, path, []byte("original"))
	f, resolved, size, err := OpenExternal(path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if resolved != path || size != 8 {
		t.Fatalf("OpenExternal = %q, %d", resolved, size)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, []byte("replacement"))
	b, err := os.ReadFile(path + ".old")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(b))
	if _, err := f.Read(buf); err != nil || string(buf) != "original" {
		t.Fatalf("pinned read = %q, %v", buf, err)
	}
}

func TestOpenExternalRejectsNonregularAndMissing(t *testing.T) {
	dir := t.TempDir()
	if f, _, _, err := OpenExternal(dir, dir); f != nil || !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("directory = %v, %v", f, err)
	}
	if f, _, _, err := OpenExternal(filepath.Join(dir, "missing"), dir); f != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v, %v", f, err)
	}
}

func TestOpenExternalRejectsFIFOWithoutWaitingForAWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if f, _, _, err := OpenExternal(path, dir); f != nil || !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("fifo = %v, %v", f, err)
	}
}

func TestOpenExternalRejectsFIFOReplacementWithoutWaitingForAWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "replace-me")
	writeFile(t, path, []byte("regular before open"))
	done := make(chan error, 1)
	go func() {
		var replaceErr error
		f, _, _, err := openExternal(path, dir, func() {
			if removeErr := os.Remove(path); removeErr != nil {
				replaceErr = removeErr
				return
			}
			if fifoErr := syscall.Mkfifo(path, 0o600); fifoErr != nil {
				replaceErr = fifoErr
			}
		})
		if f != nil {
			f.Close()
		}
		if replaceErr != nil {
			done <- replaceErr
			return
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegularFile) {
			t.Fatalf("replacement error = %v, want ErrNotRegularFile", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenExternal blocked opening a FIFO replacement")
	}
}

func TestOpenExternalRelativeTraversalAndDanglingSymlink(t *testing.T) {
	work := t.TempDir()
	outside := t.TempDir()
	path := filepath.Join(outside, "relative.txt")
	writeFile(t, path, []byte("relative"))
	ref, err := filepath.Rel(work, path)
	if err != nil {
		t.Fatal(err)
	}
	f, resolved, _, err := OpenExternal(ref, work)
	if err != nil || resolved != path {
		t.Fatalf("relative traversal = %q, %v", resolved, err)
	}
	f.Close()
	dangling := filepath.Join(outside, "dangling")
	if err := os.Symlink(filepath.Join(outside, "absent"), dangling); err != nil {
		t.Fatal(err)
	}
	if f, _, _, err := OpenExternal(dangling, work); f != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("dangling symlink = %v, %v", f, err)
	}
}

func TestOpenExternalReportsUnreadablePath(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	path := filepath.Join(locked, "file.txt")
	writeFile(t, path, []byte("private"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o700)
	f, _, _, err := OpenExternal(path, dir)
	if f != nil {
		f.Close()
		t.Skip("current user can traverse mode-000 directories")
	}
	if !errors.Is(err, ErrUnreadable) {
		t.Fatalf("error = %v, want ErrUnreadable", err)
	}
}

func TestStorageMode(t *testing.T) {
	cases := []struct {
		size, cap int64
		want      string
	}{
		{size: 10, cap: 100, want: "inline"},
		{size: 100, cap: 100, want: "inline"}, // exactly at cap counts as inline
		{size: 101, cap: 100, want: "blob"},
		{size: 10_000_000, cap: 100, want: "blob"},
	}
	for _, c := range cases {
		got := StorageMode(c.size, c.cap)
		if got != c.want {
			t.Errorf("StorageMode(%d, %d) = %q, want %q", c.size, c.cap, got, c.want)
		}
	}
}

// TestStorageMode_NeverGatesEligibility documents the design decision: an
// oversized-but-eligible file must still be ingestible (as a blob), so
// StorageMode has no "reject" outcome — it only ever returns inline or blob.
func TestStorageMode_NeverGatesEligibility(t *testing.T) {
	got := StorageMode(1<<40, 100) // absurdly large, still must resolve to a mode, not an error
	if got != "blob" {
		t.Fatalf("StorageMode for oversized file = %q, want %q (never a rejection)", got, "blob")
	}
}
