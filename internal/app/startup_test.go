package app

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareDataDirCreatesDurableDirectories(t *testing.T) {
	data := filepath.Join(t.TempDir(), "scimux-data")
	if err := prepareDataDir(data); err != nil {
		t.Fatal(err)
	}
	wantDirs := []string{
		data,
		filepath.Join(data, "sessions"),
		filepath.Join(data, "attachments"),
		filepath.Join(data, "assets"),
	}
	for _, path := range wantDirs {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if !st.IsDir() {
			t.Fatalf("%s is not a directory", path)
		}
		if got := st.Mode().Perm(); got != 0o700 {
			t.Fatalf("%s mode = %o, want 0700", path, got)
		}
	}
	notes := filepath.Join(data, "notes")
	if _, err := os.Stat(notes); !os.IsNotExist(err) {
		t.Fatalf("notes/ should remain absent; stat = %v", err)
	}
}

func TestPrepareDataDirTightensPreexistingModes(t *testing.T) {
	data := filepath.Join(t.TempDir(), "scimux-data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-create children with loose modes so the helper must tighten the
	// root and existing store files without recreating the whole tree from
	// scratch as the only path to correct permissions.
	for _, name := range []string{"sessions", "attachments", "assets"} {
		path := filepath.Join(data, name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ui := filepath.Join(data, "ui.json")
	store := filepath.Join(data, "nodes.jsonl")
	for _, path := range []string{ui, store} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareDataDir(data); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Fatalf("data root mode = %o, want 0700", got)
	}
	for _, path := range []string{ui, store} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 0600", path, got)
		}
	}
	// Child directories are created with 0700 when missing; when they already
	// exist, MkdirAll does not chmod them. Confirm the helper still succeeds
	// and does not create notes/.
	if _, err := os.Stat(filepath.Join(data, "notes")); !os.IsNotExist(err) {
		t.Fatalf("notes/ should remain absent; stat = %v", err)
	}
}

func TestPrepareDataDirRootCreationFailure(t *testing.T) {
	// A regular file at the requested data path makes MkdirAll fail with the
	// same underlying error main previously reported as scimux: <err>.
	root := t.TempDir()
	blocked := filepath.Join(root, "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := prepareDataDir(blocked)
	if err == nil {
		t.Fatal("prepareDataDir returned nil error for a file path")
	}
}

func TestPrepareDataDirSessionsCreationFailure(t *testing.T) {
	data := filepath.Join(t.TempDir(), "scimux-data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	// sessions as a regular file blocks the child MkdirAll.
	if err := os.WriteFile(filepath.Join(data, "sessions"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := prepareDataDir(data)
	if err == nil {
		t.Fatal("prepareDataDir returned nil error when sessions is a file")
	}
}

func TestPrepareDataDirAttachmentsCreationFailure(t *testing.T) {
	data := filepath.Join(t.TempDir(), "scimux-data")
	if err := os.MkdirAll(filepath.Join(data, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "attachments"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := prepareDataDir(data)
	if err == nil {
		t.Fatal("prepareDataDir returned nil error when attachments is a file")
	}
}

func TestPrepareDataDirAssetsCreationFailure(t *testing.T) {
	data := filepath.Join(t.TempDir(), "scimux-data")
	for _, name := range []string{"sessions", "attachments"} {
		if err := os.MkdirAll(filepath.Join(data, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(data, "assets"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := prepareDataDir(data)
	if err == nil {
		t.Fatal("prepareDataDir returned nil error when assets is a file")
	}
}

func TestConfigureUsageIncludesSummary(t *testing.T) {
	fs := flag.NewFlagSet("scimux", flag.ContinueOnError)
	var b strings.Builder
	fs.SetOutput(&b)
	fs.String("addr", "127.0.0.1:8787", "listen address")
	configureUsage(fs, "./scimux")

	fs.Usage()

	got := b.String()
	if !strings.HasPrefix(got, appSummary+"\n\nUsage: ./scimux [options]\n\n") {
		t.Fatalf("usage header = %q", got)
	}
	if !strings.Contains(got, "-addr") {
		t.Fatalf("usage flags missing from %q", got)
	}
}

func TestStartStatusPlainOutput(t *testing.T) {
	var b strings.Builder
	status := startStatus(&b, "scimux: preparing chats", false)
	status.Done()
	status.Done()

	got := b.String()
	if !strings.Contains(got, "scimux: preparing chats ...\n") ||
		!strings.Contains(got, "scimux: preparing chats done (") {
		t.Fatalf("status output = %q", got)
	}
	if strings.Count(got, "done") != 1 {
		t.Fatalf("Done should be idempotent, got %q", got)
	}
}

func TestStartStatusAnimatedAndRuntimeHelpers(t *testing.T) {
	var b strings.Builder
	status := startStatus(&b, "scimux: preparing chats", true)
	time.Sleep(130 * time.Millisecond)
	status.Done()
	status.Done()
	got := b.String()
	if !strings.Contains(got, "\rscimux: preparing chats") || !strings.Contains(got, "\x1b[Kscimux: preparing chats done (") {
		t.Fatalf("animated status output = %q", got)
	}
	if strings.Count(got, "done") != 1 {
		t.Fatalf("Done should be idempotent, got %q", got)
	}

	regular, err := os.CreateTemp(t.TempDir(), "terminal-check")
	if err != nil {
		t.Fatal(err)
	}
	defer regular.Close()
	if isTerminal(regular) {
		t.Fatal("regular file identified as terminal")
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	if !isTerminal(devNull) {
		t.Fatal("character device was not identified")
	}

	oldVersion := version
	SetVersion("v-test")
	if version != "v-test" {
		t.Fatalf("SetVersion left %q", version)
	}
	SetVersion(oldVersion)
}
