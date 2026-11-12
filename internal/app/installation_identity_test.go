package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/remote"
)

func TestEnsureInstallationIdentityCreatesAndReuses(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	ensureInstallationIdentity(dir, &out)
	if out.Len() != 0 {
		t.Fatalf("error output = %q", out.String())
	}
	first, err := identity.LoadInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, identity.InstallationFile))
	if err != nil {
		t.Fatal(err)
	}
	ensureInstallationIdentity(dir, &out)
	second, err := identity.LoadInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second ID = %q, first ID = %q", second, first)
	}
	after, err := os.ReadFile(filepath.Join(dir, identity.InstallationFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("second call changed the installation file")
	}
	if out.Len() != 0 {
		t.Fatalf("error output = %q", out.String())
	}
}

func TestEnsureInstallationIdentityReportsDamagedFile(t *testing.T) {
	dir := t.TempDir()
	const planted = "0123456789abcdef0123456789abcdef"
	body := []byte(`{"v":2,"id":"` + planted + `"}` + "\n")
	path := filepath.Join(dir, identity.InstallationFile)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ensureInstallationIdentity(dir, &out)
	text := out.String()
	if strings.Count(text, "\n") != 1 || !strings.HasSuffix(text, "\n") || strings.TrimSpace(text) == "" {
		t.Fatalf("error output = %q", text)
	}
	if strings.Contains(text, planted) {
		t.Fatalf("error output contains the installation id: %q", text)
	}
	want := "scimux: installation identity is unavailable: " + strconv.Quote(path) +
		" is damaged or unsupported and was left unchanged\n"
	if text != want {
		t.Fatalf("error output = %q, want %q", text, want)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, after) {
		t.Fatal("damaged installation file was changed")
	}
}

func TestEnsureInstallationIdentityReportsPathAndCause(t *testing.T) {
	// A newline in the directory name must not split the line.
	dir := filepath.Join(t.TempDir(), "data\ndir")
	path := filepath.Join(dir, identity.InstallationFile)
	// A directory in place of the file fails the read for root as well.
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ensureInstallationIdentity(dir, &out)
	want := "scimux: installation identity is unavailable: " + strconv.Quote(path) +
		": " + syscall.EISDIR.Error() + "\n"
	if out.String() != want {
		t.Fatalf("error output = %q, want %q", out.String(), want)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("installation path was changed: %v", err)
	}
}

func TestEnsureInstallationIdentityNilErrorOutput(t *testing.T) {
	dir := t.TempDir()
	body := []byte("{")
	path := filepath.Join(dir, identity.InstallationFile)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	ensureInstallationIdentity(dir, nil)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, after) {
		t.Fatal("damaged installation file was changed")
	}
}

func TestMuxerStartupCreatesInstallationIdentity(t *testing.T) {
	data := t.TempDir()
	cmd := &Command{
		Args:      []string{"scimux", "-addr", "127.0.0.1:0", "-data", data},
		Home:      t.TempDir(),
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		muxerOnly: true,
	}
	if err := cmd.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cmd.closeListener)
	t.Cleanup(cmd.closeOwnership)
	if _, err := identity.LoadInstallation(data); err != nil {
		t.Fatal(err)
	}
}

func TestStartupOutsideTheMuxerDoesNotCreateInstallationIdentity(t *testing.T) {
	t.Run("monolithic", func(t *testing.T) {
		data := t.TempDir()
		cmd := &Command{
			Args:   []string{"scimux", "-addr", "127.0.0.1:0", "-data", data},
			Home:   t.TempDir(),
			Stdout: io.Discard,
			Stderr: io.Discard,
		}
		if err := cmd.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cmd.closeListener)
		assertNoInstallationFile(t, data)
	})
	for _, arg := range []string{webChildCmd, sessionWorkerCmd} {
		t.Run(arg, func(t *testing.T) {
			data := t.TempDir()
			cmd := &Command{
				Args:      []string{"scimux", arg, "-data", data},
				Home:      t.TempDir(),
				Stdout:    io.Discard,
				Stderr:    io.Discard,
				Config:    remote.Config{DataDir: data},
				muxerOnly: true,
			}
			if err := cmd.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertNoInstallationFile(t, data)
		})
	}
}

func assertNoInstallationFile(t *testing.T, data string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(data, identity.InstallationFile))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("installation file err = %v, want not exist", err)
	}
}
