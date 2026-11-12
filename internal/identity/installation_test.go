package identity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	validInstallationID = "0123456789abcdef0123456789abcdef"
	otherInstallationID = "fedcba9876543210fedcba9876543210"
)

func TestParseInstallationID(t *testing.T) {
	id, err := ParseInstallationID(validInstallationID)
	if err != nil {
		t.Fatal(err)
	}
	if id != validInstallationID {
		t.Fatalf("id = %q", id)
	}
	if _, err := ParseInstallationID(strings.Repeat("a", 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseInstallationID(strings.Repeat("0", 32)); err != nil {
		t.Fatal(err)
	}

	rejects := []string{
		"",
		"0123456789abcdef0123456789abcde",    // 31
		"0123456789abcdef0123456789abcdef0",  // 33
		"0123456789ABCDEF0123456789ABCDEF",   // uppercase
		"0123456789abcdef0123456789abcdeg",   // non-hex
		" 0123456789abcdef0123456789abcdef",  // leading space
		"0123456789abcdef0123456789abcdef ",  // trailing space
		"0123456789abcdef0123456789abcdef\n", // trailing newline
		"\t0123456789abcdef0123456789abcdef", // leading tab
	}
	for _, s := range rejects {
		if _, err := ParseInstallationID(s); err == nil {
			t.Errorf("ParseInstallationID(%q) succeeded", s)
		}
	}
}

func TestLoadOrCreateInstallationCreatesExactFile(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseInstallationID(string(id)); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, InstallationFile)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %04o, want 0600", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeInstallationFile(t, raw)
	if doc.V != 1 || doc.ID != string(id) {
		t.Fatalf("document = %+v, id %s", doc, id)
	}
	created, err := time.Parse(time.RFC3339, doc.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(doc.CreatedAt, "Z") {
		t.Fatalf("created_at %q is not UTC", doc.CreatedAt)
	}
	delta := time.Since(created)
	if delta < -time.Minute || delta > time.Minute {
		t.Fatalf("created_at %s is %s from now", doc.CreatedAt, delta)
	}
	want := fmt.Sprintf("{\"v\":1,\"id\":%q,\"created_at\":%q}\n", doc.ID, doc.CreatedAt)
	if string(raw) != want {
		t.Fatalf("file = %q, want %q", raw, want)
	}
	assertOnlyInstallationFile(t, dir)

	again, err := LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Fatalf("second id = %s, want %s", again, id)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, after) {
		t.Fatalf("second call changed the file\nbefore %q\nafter  %q", raw, after)
	}
}

func TestLoadInstallationAbsent(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadInstallation(dir)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	if errors.Is(err, ErrInvalidInstallation) {
		t.Fatalf("missing file reported as damaged: %v", err)
	}
}

func TestDamagedInstallationIsNeverReplaced(t *testing.T) {
	valid := `{"v":1,"id":"` + validInstallationID + `","created_at":"2026-01-02T03:04:05Z"}`
	padded := valid + strings.Repeat(" ", 4097-len(valid))
	if len(padded) != 4097 {
		t.Fatalf("padded length = %d", len(padded))
	}
	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"bad json", "{"},
		{"version 2", `{"v":2,"id":"` + validInstallationID + `","created_at":"2026-01-02T03:04:05Z"}`},
		{"missing version", `{"id":"` + validInstallationID + `","created_at":"2026-01-02T03:04:05Z"}`},
		{"missing id", `{"v":1,"created_at":"2026-01-02T03:04:05Z"}`},
		{"malformed id", `{"v":1,"id":"not-an-installation-id","created_at":"2026-01-02T03:04:05Z"}`},
		{"larger than 4096", padded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, InstallationFile)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LoadInstallation(dir); !errors.Is(err, ErrInvalidInstallation) {
				t.Fatalf("Load err = %v, want ErrInvalidInstallation", err)
			}
			if _, err := LoadOrCreateInstallation(dir); !errors.Is(err, ErrInvalidInstallation) {
				t.Fatalf("LoadOrCreate err = %v, want ErrInvalidInstallation", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("damaged file was modified")
			}
			assertOnlyInstallationFile(t, dir)
		})
	}
}

func TestInstallationFileOfExactly4096BytesIsReadable(t *testing.T) {
	core := `{"v":1,"id":"` + validInstallationID + `","created_at":"2026-01-02T03:04:05Z"}`
	body := core + strings.Repeat(" ", 4096-len(core))
	if len(body) != 4096 {
		t.Fatalf("length = %d", len(body))
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, InstallationFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := LoadInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id != validInstallationID {
		t.Fatalf("id = %s", id)
	}
}

func TestInstallationUnknownFieldsAreTolerated(t *testing.T) {
	body := []byte(`{"v":1,"id":"` + validInstallationID + `","created_at":"2026-01-02T03:04:05Z","note":"synthetic"}` + "\n")
	dir := t.TempDir()
	path := filepath.Join(dir, InstallationFile)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := LoadInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id != validInstallationID {
		t.Fatalf("Load id = %s", id)
	}
	again, err := LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Fatalf("LoadOrCreate id = %s", again)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, after) {
		t.Fatal("file with an unknown field was rewritten")
	}
}

func TestInstallationIDsDifferAcrossDirectories(t *testing.T) {
	first, err := LoadOrCreateInstallation(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateInstallation(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two directories minted %s", first)
	}
}

func TestFailingInstallationEntropyWritesNothing(t *testing.T) {
	snapshotInstallation(t)
	installationEntropy = failReader{err: errors.New("entropy unavailable")}
	dir := t.TempDir()
	if _, err := LoadOrCreateInstallation(dir); err == nil {
		t.Fatal("expected an error from the failing entropy source")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entropy failure left files: %v", namesOf(entries))
	}
}

func TestLostInstallationRaceReturnsExistingID(t *testing.T) {
	snapshotInstallation(t)
	winner := []byte(`{"v":1,"id":"` + otherInstallationID + `","created_at":"2026-01-02T03:04:05Z"}` + "\n")
	beforeInstallationLink = func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, InstallationFile), winner, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	id, err := LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id != otherInstallationID {
		t.Fatalf("id = %s, want the id planted by the other process", id)
	}
	raw, err := os.ReadFile(filepath.Join(dir, InstallationFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, winner) {
		t.Fatalf("winner file changed: %q", raw)
	}
	assertOnlyInstallationFile(t, dir)
}

func TestConcurrentLoadOrCreateInstallation(t *testing.T) {
	dir := t.TempDir()
	const n = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make([]InstallationID, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			ids[i], errs[i] = LoadOrCreateInstallation(dir)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("goroutine %d id = %s, want %s", i, ids[i], ids[0])
		}
	}
	if _, err := ParseInstallationID(string(ids[0])); err != nil {
		t.Fatal(err)
	}
	assertOnlyInstallationFile(t, dir)
}

func TestInstallationFilesystemFailures(t *testing.T) {
	t.Run("data directory is a file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "not-a-directory")
		if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, loadErr := LoadInstallation(path)
		if loadErr == nil {
			t.Fatal("Load succeeded on a file")
		}
		if errors.Is(loadErr, fs.ErrNotExist) {
			t.Fatalf("a file used as a directory reported ErrNotExist: %v", loadErr)
		}
		_, createErr := LoadOrCreateInstallation(path)
		if createErr == nil {
			t.Fatal("LoadOrCreate succeeded on a file")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != "synthetic" {
			t.Fatalf("file changed: %q", raw)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "not-a-directory" {
			t.Fatalf("files = %v", namesOf(entries))
		}
	})

	t.Run("data directory does not exist", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing")
		_, loadErr := LoadInstallation(missing)
		if !errors.Is(loadErr, fs.ErrNotExist) {
			t.Fatalf("Load err = %v, want fs.ErrNotExist", loadErr)
		}
		if _, err := LoadOrCreateInstallation(missing); err == nil {
			t.Fatal("LoadOrCreate succeeded without a directory")
		}
		if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing directory stat = %v", err)
		}
	})
}

func TestInstallationReadErrorDoesNotReplaceFile(t *testing.T) {
	snapshotInstallation(t)
	readInstallationBytes = func(io.Reader) ([]byte, error) {
		return nil, errors.New("read failed")
	}
	dir := t.TempDir()
	body := []byte(`{"v":1,"id":"` + validInstallationID + `","created_at":"2026-01-02T03:04:05Z"}` + "\n")
	path := filepath.Join(dir, InstallationFile)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadInstallation(dir)
	if err == nil || errors.Is(err, ErrInvalidInstallation) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load err = %v, want a read error", err)
	}
	_, err = LoadOrCreateInstallation(dir)
	if err == nil || errors.Is(err, ErrInvalidInstallation) {
		t.Fatalf("LoadOrCreate err = %v, want the read error", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, after) {
		t.Fatal("read failure rewrote the file")
	}
	assertOnlyInstallationFile(t, dir)
}

func TestInstallationPublishFailuresLeaveNoTemp(t *testing.T) {
	cases := []struct {
		name string
		set  func(real installationIO)
	}{
		{"chmod", func(real installationIO) {
			installationOps.chmod = func(*os.File, os.FileMode) error {
				return errors.New("chmod failed")
			}
		}},
		{"write", func(real installationIO) {
			installationOps.write = func(*os.File, []byte) (int, error) {
				return 0, errors.New("write failed")
			}
		}},
		{"short write", func(real installationIO) {
			installationOps.write = func(*os.File, []byte) (int, error) {
				return 1, nil
			}
		}},
		{"sync file", func(real installationIO) {
			installationOps.syncFile = func(*os.File) error {
				return errors.New("sync failed")
			}
		}},
		{"close", func(real installationIO) {
			installationOps.close = func(*os.File) error {
				return errors.New("close failed")
			}
		}},
		{"link", func(real installationIO) {
			installationOps.link = func(string, string) error {
				return errors.New("link failed")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshotInstallation(t)
			tc.set(installationOps)
			dir := t.TempDir()
			if _, err := LoadOrCreateInstallation(dir); err == nil {
				t.Fatal("expected an error")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("left files: %v", namesOf(entries))
			}
		})
	}
}

func TestInstallationTempAlreadyGoneStillPublishes(t *testing.T) {
	snapshotInstallation(t)
	realRemove := installationOps.remove
	installationOps.remove = func(name string) error {
		if err := realRemove(name); err != nil {
			return err
		}
		return fs.ErrNotExist
	}
	dir := t.TempDir()
	id, err := LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseInstallationID(string(id)); err != nil {
		t.Fatal(err)
	}
	assertOnlyInstallationFile(t, dir)
}

func TestInstallationRemoveFailureAfterPublish(t *testing.T) {
	snapshotInstallation(t)
	installationOps.remove = func(string) error {
		return errors.New("remove failed")
	}
	dir := t.TempDir()
	if _, err := LoadOrCreateInstallation(dir); err == nil {
		t.Fatal("expected an error when the temp file could not be removed")
	}
	id, err := LoadInstallation(dir)
	if err != nil {
		t.Fatalf("published file is not readable: %v", err)
	}
	if _, err := ParseInstallationID(string(id)); err != nil {
		t.Fatal(err)
	}
}

func TestInstallationDirectorySyncFailureAfterPublish(t *testing.T) {
	snapshotInstallation(t)
	syncDirectory = func(string) error {
		return errors.New("directory sync failed")
	}
	dir := t.TempDir()
	if _, err := LoadOrCreateInstallation(dir); err == nil {
		t.Fatal("expected a directory sync error")
	}
	id, err := LoadInstallation(dir)
	if err != nil {
		t.Fatalf("published file is not readable: %v", err)
	}
	if _, err := ParseInstallationID(string(id)); err != nil {
		t.Fatal(err)
	}
	assertOnlyInstallationFile(t, dir)
}

func TestSyncDirectoryMissing(t *testing.T) {
	if err := syncDirectory(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected an error syncing a missing directory")
	}
}

type failReader struct{ err error }

func (f failReader) Read([]byte) (int, error) { return 0, f.err }

type installationDocument struct {
	V         int    `json:"v"`
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
}

func decodeInstallationFile(t *testing.T, raw []byte) installationDocument {
	t.Helper()
	var doc installationDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func snapshotInstallation(t *testing.T) {
	t.Helper()
	entropy := installationEntropy
	hook := beforeInstallationLink
	ops := installationOps
	sync := syncDirectory
	read := readInstallationBytes
	t.Cleanup(func() {
		installationEntropy = entropy
		beforeInstallationLink = hook
		installationOps = ops
		syncDirectory = sync
		readInstallationBytes = read
	})
}

func assertOnlyInstallationFile(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != InstallationFile {
		t.Fatalf("directory entries = %v, want only %s", namesOf(entries), InstallationFile)
	}
}

func namesOf(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}
