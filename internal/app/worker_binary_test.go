package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/sessionworker"
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
	firstInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := pinWorkerExecutable(source, data)
	if err != nil || again != path {
		t.Fatalf("second pin = %q, %v; want %q", again, err, path)
	}
	secondInfo, err := os.Stat(again)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstInfo, secondInfo) {
		t.Fatal("second pin rewrote the existing content-addressed executable")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("pin directory = %v, %v", entries, err)
	}
}

func TestPinWorkerExecutableRejectsUnsafeInputs(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
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

func TestPinWorkerExecutableRejectsCompromisedExistingPin(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "scimux")
	content := []byte("immutable generation")
	if err := os.WriteFile(source, content, 0o755); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	dir := filepath.Join(data, "control", "worker-binaries")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "scimux-"+hex.EncodeToString(digest[:]))
	if err := os.WriteFile(path, []byte("not trusted despite the name"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pinWorkerExecutable(source, data); err == nil {
		t.Fatal("pin reused a content-addressed path without owner-executable mode")
	}
}

func TestSweepWorkerExecutablesKeepsCurrentLiveAndLegacyPins(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	pin := func(name string) string {
		t.Helper()
		source := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(source, []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
		path, err := pinWorkerExecutable(source, data)
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	current, live, stale := pin("current"), pin("live"), pin("stale")
	if err := sweepWorkerExecutables(data, "", filepath.Join(data, "outside-pin")); err == nil {
		t.Fatal("sweep accepted a current executable outside the pin directory")
	}
	dir := filepath.Dir(current)
	unrelated := []string{
		"notes",
		"scimux-short",
		"scimux-" + strings.Repeat("g", sha256.Size*2),
	}
	for _, name := range unrelated {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not a pin"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nonRegular := filepath.Join(dir, "scimux-"+strings.Repeat("0", sha256.Size*2))
	if err := os.Mkdir(nonRegular, 0o700); err != nil {
		t.Fatal(err)
	}
	registration, err := sessionworker.Claim(data, "live")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registration.Close() })
	if err := registration.Publish(sessionworker.Locator{
		Identity: sessionworker.Identity{WorkerID: "live-worker", Agent: "pi", Build: "old"},
		NodeID:   "live", PID: os.Getpid(), Executable: live,
		Link: sessionworker.Link{Socket: filepath.Join(data, "live.sock"), Token: "secret"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sweepWorkerExecutables(data, "", current); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{current, live} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained pin %s: %v", path, err)
		}
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale pin remains: %v", err)
	}
	for _, name := range unrelated {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("unrelated file %q was removed: %v", name, err)
		}
	}
	if info, err := os.Stat(nonRegular); err != nil || !info.IsDir() {
		t.Fatalf("non-regular digest-shaped entry was removed: %v, %v", info, err)
	}

	// An old live worker cannot name its executable. Fail closed and keep all
	// pins until it exits rather than guessing which image is safe to remove.
	legacyPin := pin("legacy-unknown")
	legacy, err := sessionworker.Claim(data, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	if err := legacy.Publish(sessionworker.Locator{
		Identity: sessionworker.Identity{WorkerID: "legacy-worker", Agent: "codex", Build: "old"},
		NodeID:   "legacy", PID: os.Getpid(),
		Link: sessionworker.Link{Socket: filepath.Join(data, "legacy.sock"), Token: "secret"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sweepWorkerExecutables(data, "", current); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPin); err != nil {
		t.Fatalf("legacy-ambiguous pin was removed: %v", err)
	}

	// A malformed locator leaves its worker executable unknowable too. List
	// returns the sound locators plus an error; the sweep must fail closed
	// before deleting anything.
	malformedProtected := pin("malformed-protected")
	if err := os.WriteFile(filepath.Join(data, "control", "workers", "malformed.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sweepWorkerExecutables(data, "", current); err == nil {
		t.Fatal("sweep hid a malformed worker locator")
	}
	if _, err := os.Stat(malformedProtected); err != nil {
		t.Fatalf("malformed-locator pin was removed: %v", err)
	}

	missingPins := t.TempDir()
	if err := os.Chmod(missingPins, 0o700); err != nil {
		t.Fatal(err)
	}
	missingCurrent := filepath.Join(missingPins, "control", "worker-binaries", "scimux-"+strings.Repeat("1", sha256.Size*2))
	if err := sweepWorkerExecutables(missingPins, "", missingCurrent); err == nil {
		t.Fatal("sweep hid a missing pin directory")
	}
}

// A full stop retires a Claude worker but preserves its pane, whose hook
// settings keep exec'ing the retired worker's pin. Startup recovers the pane
// with a newer worker and then sweeps; the old pin must survive, or every hook
// of the recovered chat (auto-approve included) silently stops running.
func TestSweepWorkerExecutablesKeepsClaudeBundlePins(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	pin := func(name string) string {
		t.Helper()
		source := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(source, []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
		path, err := pinWorkerExecutable(source, data)
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	hooks := filepath.Join(data, "claude-hooks")
	bundle := func(dir, capabilities string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if capabilities == "" {
			return
		}
		if err := os.WriteFile(filepath.Join(dir, "capabilities.json"), []byte(capabilities), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	current, recovered, archived := pin("current"), pin("recovered"), pin("archived")
	bundle(filepath.Join(hooks, "aaaa"), `{"permission":1,"stop":1,"exec":"`+recovered+`"}`)
	bundle(filepath.Join(hooks, "archive", "bbbb.20260101T000000.000000000Z"), `{"permission":1,"exec":"`+archived+`"}`)
	bundle(filepath.Join(hooks, "cccc"), "")
	bundle(filepath.Join(hooks, "dddd"), `{"permission":1,"exec":"/usr/local/bin/scimux"}`)
	bundle(filepath.Join(hooks, "ffff"), `{"permission":1}`)
	if err := sweepWorkerExecutables(data, hooks, current); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{current, recovered} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained pin %s: %v", path, err)
		}
	}
	if _, err := os.Stat(archived); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pin named only by an archived bundle remains: %v", err)
	}

	// A bundle whose proof cannot be read leaves its pin unknowable: fail
	// closed before deleting anything.
	protected := pin("unparseable-protected")
	bundle(filepath.Join(hooks, "eeee"), "{")
	if err := sweepWorkerExecutables(data, hooks, current); err == nil {
		t.Fatal("sweep hid an unparseable hook bundle")
	}
	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("pin was removed despite an unparseable bundle: %v", err)
	}

	// Read failures other than absence fail closed too. Shapes, not
	// permissions, provoke them, so the test holds when run as root.
	if err := os.Remove(filepath.Join(hooks, "eeee", "capabilities.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(hooks, "eeee", "capabilities.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := sweepWorkerExecutables(data, hooks, current); err == nil {
		t.Fatal("sweep hid an unreadable hook bundle")
	}
	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("pin was removed despite an unreadable bundle: %v", err)
	}
	notDir := filepath.Join(data, "hooks-file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sweepWorkerExecutables(data, notDir, current); err == nil {
		t.Fatal("sweep hid an unlistable hook directory")
	}
	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("pin was removed despite an unlistable hook directory: %v", err)
	}

	// A data directory that has never launched Claude has no hook directory;
	// that is no reference, not a failure.
	if err := sweepWorkerExecutables(data, filepath.Join(data, "no-hooks"), current); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(protected); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced pin remains without a hook directory: %v", err)
	}
}
