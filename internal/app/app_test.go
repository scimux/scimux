package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp"
	"codeberg.org/chrberger/scimux/internal/acp/codex"
	"codeberg.org/chrberger/scimux/internal/acp/muse"
	"codeberg.org/chrberger/scimux/internal/tmuxsession"
)

func TestNewAppRejectsEmptyHome(t *testing.T) {
	_, err := NewApp(Config{DataDir: t.TempDir()})
	if err == nil {
		t.Fatal("NewApp accepted an empty Home")
	}
}

func TestNewAppRejectsEmptyDataDir(t *testing.T) {
	_, err := NewApp(Config{Home: t.TempDir()})
	if err == nil {
		t.Fatal("NewApp accepted an empty DataDir")
	}
}

func TestNewAppUsesSuppliedHome(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "process-home"))
	cfgHome := filepath.Join(t.TempDir(), "cfg-home")
	a, err := NewApp(Config{Home: cfgHome, DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	if a.home != cfgHome {
		t.Fatalf("home = %q, want supplied %q", a.home, cfgHome)
	}
}

func TestNewAppInitializesMutableMaps(t *testing.T) {
	a, err := NewApp(Config{Home: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	maps := map[string]bool{
		"byID":        a.byID != nil,
		"live":        a.live != nil,
		"attn":        a.attn != nil,
		"turnDone":    a.turnDone != nil,
		"attnAt":      a.attnAt != nil,
		"prevCap":     a.prevCap != nil,
		"lastChg":     a.lastChg != nil,
		"activeSince": a.activeSince != nil,
		"tailers":     a.tailers != nil,
		"mirrors":     a.mirrors != nil,
		"pathClaims":  a.pathClaims != nil,
		"chatMark":    a.chatMark != nil,
		"staleChat":   a.staleChat != nil,
		"sendState":   a.sendState != nil,
		"reserved":    a.reserved != nil,
		"anim":        a.anim != nil,
		"claudeIDs":   a.claudeIDs != nil,
		"logCache":    a.logCache != nil,
	}
	for name, ok := range maps {
		if !ok {
			t.Errorf("%s map is nil", name)
		}
	}
}

func TestNewAppInstallsInjectedServer(t *testing.T) {
	fake := tmuxsession.NewServerWithRunner("fake-socket", func(ctx context.Context, stdin string, args ...string) (string, error) {
		return "", nil
	})
	a, err := newApp(Config{
		Home:        t.TempDir(),
		DataDir:     filepath.Join(t.TempDir(), "data"),
		LaunchGrace: 40 * time.Millisecond,
		LaunchPoll:  5 * time.Millisecond,
	}, appDeps{Server: fake})
	if err != nil {
		t.Fatal(err)
	}
	if a.server != fake {
		t.Fatal("newApp did not install the supplied tmux server")
	}
	if a.launchGrace != 40*time.Millisecond || a.launchPoll != 5*time.Millisecond {
		t.Fatalf("launch timings = %s/%s", a.launchGrace, a.launchPoll)
	}
}

func TestNewAppDerivesDataPaths(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	a, err := NewApp(Config{Home: t.TempDir(), DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"claudeCachePath": filepath.Join(data, "claude-models.json"),
		"claudeProbeDir":  filepath.Join(data, "probe"),
		"storePath":       filepath.Join(data, "nodes.jsonl"),
		"uiPath":          filepath.Join(data, "ui.json"),
		"sessionsDir":     filepath.Join(data, "sessions"),
		"attachmentsDir":  filepath.Join(data, "attachments"),
		"assetsDir":       filepath.Join(data, "assets"),
		"notesDir":        filepath.Join(data, "notes"),
	}
	got := map[string]string{
		"claudeCachePath": a.claudeCachePath,
		"claudeProbeDir":  a.claudeProbeDir,
		"storePath":       a.storePath,
		"uiPath":          a.uiPath,
		"sessionsDir":     a.sessionsDir,
		"attachmentsDir":  a.attachmentsDir,
		"assetsDir":       a.assetsDir,
		"notesDir":        a.notes.Dir,
	}
	for name, wantPath := range want {
		if got[name] != wantPath {
			t.Errorf("%s = %q, want %q", name, got[name], wantPath)
		}
	}
	distinct := []string{a.sessionsDir, a.attachmentsDir, a.assetsDir, a.notes.Dir}
	for i := range distinct {
		for j := i + 1; j < len(distinct); j++ {
			if distinct[i] == distinct[j] {
				t.Fatalf("data directories are not distinct: %q", distinct[i])
			}
		}
	}
}

func TestNewAppAssetHookAndSourceWiring(t *testing.T) {
	a, err := NewApp(Config{Home: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	if a.assetHook == nil {
		t.Fatal("assetHook is nil")
	}
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"a.acp.SetAssetHook(a.assetHook)", "a.codex.SetAssetHook(a.assetHook)"} {
		if !strings.Contains(string(src), call) {
			t.Fatalf("constructor source is missing %s", call)
		}
	}
}

func TestNewAppDoesNotCreateDurableDirectories(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	a, err := NewApp(Config{Home: filepath.Join(root, "home"), DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	if a == nil {
		t.Fatal("NewApp returned nil app")
	}
	for _, path := range []string{
		data,
		filepath.Join(data, "sessions"),
		filepath.Join(data, "attachments"),
		filepath.Join(data, "assets"),
		filepath.Join(data, "notes"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("constructor touched %s: stat err = %v", path, err)
		}
	}
}

func TestNewAppDoesNotChmodDurableDirectories(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	paths := []string{
		data,
		filepath.Join(data, "sessions"),
		filepath.Join(data, "attachments"),
		filepath.Join(data, "assets"),
		filepath.Join(data, "notes"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	before := map[string]os.FileMode{}
	for _, path := range paths {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = st.Mode().Perm()
	}
	if _, err := NewApp(Config{Home: t.TempDir(), DataDir: data}); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != before[path] {
			t.Fatalf("%s mode = %o, want unchanged %o", path, got, before[path])
		}
	}
}

func TestNewAppReplaysExistingStore(t *testing.T) {
	data := t.TempDir()
	store := filepath.Join(data, "nodes.jsonl")
	line := `{"type":"node","node":{"id":"n1","title":"Node 1","prompt":"Prompt 1","agent":"claude","dir":"/tmp","created_at":"2026-07-14T00:00:00Z"}}` + "\n"
	if err := os.WriteFile(store, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := NewApp(Config{Home: t.TempDir(), DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	n := a.byID["n1"]
	if n == nil {
		t.Fatal("constructor did not replay existing node")
	}
	if n.Description != "Prompt 1" {
		t.Fatalf("description = %q, want replay default from prompt", n.Description)
	}
}

func TestNewAppReturnsStoreReadFailure(t *testing.T) {
	data := t.TempDir()
	if err := os.Mkdir(filepath.Join(data, "nodes.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := NewApp(Config{Home: t.TempDir(), DataDir: data})
	if err == nil {
		t.Fatal("NewApp returned nil error for unreadable nodes.jsonl")
	}
}

func TestNewAppConstructsMuseManagerAndAssetHook(t *testing.T) {
	a, err := NewApp(Config{Home: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	if a.muse.Manager == nil {
		t.Fatal("NewApp did not construct a Muse manager")
	}
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{
		"a.acp.SetAssetHook(a.assetHook)",
		"a.codex.SetAssetHook(a.assetHook)",
		"a.muse.SetAssetHook(a.assetHook)",
	} {
		if !strings.Contains(string(src), call) {
			t.Fatalf("constructor source is missing %s", call)
		}
	}
}

func TestProcSelectsMuseAndLeavesOtherTransportsAlone(t *testing.T) {
	a, err := NewApp(Config{Home: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	got := a.proc(&Node{Transport: "muse"})
	if got == nil {
		t.Fatal(`transport "muse" did not select a structured manager`)
	}
	if _, ok := got.(museManager); !ok {
		t.Fatalf(`transport "muse" selected %T, want museManager`, got)
	}
	got = a.proc(&Node{Agent: "muse", Transport: "muse"})
	if _, ok := got.(museManager); !ok {
		t.Fatalf("muse agent selected %T, want museManager", got)
	}
	got = a.proc(&Node{Transport: "acp"})
	if _, ok := got.(acpManager); !ok {
		t.Fatalf(`transport "acp" selected %T, want acpManager`, got)
	}
	got = a.proc(&Node{Transport: "codex"})
	if _, ok := got.(codexManager); !ok {
		t.Fatalf(`transport "codex" selected %T, want codexManager`, got)
	}
	if got := a.proc(&Node{Agent: "claude", Transport: ""}); got != nil {
		t.Fatalf("legacy empty transport selected %T; want tmux (nil proc)", got)
	}
	if got := a.proc(&Node{Agent: "muse", Transport: ""}); got != nil {
		t.Fatalf("legacy empty transport on a muse-named agent selected %T; empty still means tmux", got)
	}
	if got := a.proc(&Node{Transport: "tmux"}); got != nil {
		t.Fatalf("tmux transport selected %T, want nil", got)
	}
}

func TestProcTestProcSeamStillWins(t *testing.T) {
	a, err := NewApp(Config{Home: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubProc{hasSession: true}
	a.testProc = stub
	if got := a.proc(&Node{Transport: "muse"}); got != stub {
		t.Fatalf("testProc did not replace the Muse manager: got %T", got)
	}
	if got := a.proc(&Node{Transport: "acp"}); got != stub {
		t.Fatalf("testProc did not replace the ACP manager: got %T", got)
	}
}

func TestMuseManagerNilSafeMethods(t *testing.T) {
	var m museManager
	m.Shutdown()
	m.SetAssetHook(nil)
	if m.FingerprintMismatch("n") {
		t.Fatal("nil manager reported a fingerprint mismatch")
	}
	var a *app
	a.shutdownStructured()
}

func TestMuseManagerConflictUsesErrorsIs(t *testing.T) {
	var _ procManager = museManager{}
	m := museManager{Manager: muse.NewManager(t.TempDir())}
	t.Cleanup(m.Shutdown)
	sentinels := []error{
		muse.ErrNoSession,
		muse.ErrNotAlive,
		muse.ErrTurnActive,
		muse.ErrNoPending,
		muse.ErrNoTurn,
		muse.ErrStalePermission,
		muse.ErrLaunchConflict,
	}
	for _, err := range sentinels {
		if !m.Conflict(err) {
			t.Errorf("Conflict(%v) = false, want true", err)
		}
		wrapped := fmt.Errorf("muse op: %w", err)
		if !m.Conflict(wrapped) {
			t.Errorf("Conflict(wrapped %v) = false; adapter must use errors.Is", err)
		}
	}
	if m.Conflict(errors.New("random")) {
		t.Error("Conflict(random) = true, want false")
	}
	if m.Conflict(acp.ErrNoSession) || m.Conflict(codex.ErrNoSession) {
		t.Error("Muse Conflict matched ACP or Codex sentinels by string or identity")
	}
	if p, ok := m.Pending("ghost"); ok {
		t.Errorf("Pending(ghost) = (%+v, true), want ok=false", p)
	}
}

func TestShutdownStructuredClosesMuseExactlyOnce(t *testing.T) {
	a, err := NewApp(Config{Home: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatal(err)
	}
	if c := strings.Count(string(src), "a.muse.Shutdown()"); c != 1 {
		t.Fatalf("shutdownStructured must close Muse exactly once, found %d a.muse.Shutdown() calls", c)
	}
	a.shutdownStructured()
	a.shutdownStructured() // manager Shutdown is idempotent; a second call must not panic
	if a.muse.HasSession("ghost") {
		t.Fatal("Muse manager still claims a session after shutdown")
	}
}

func TestShutdownStructuredWiredInMainAndUpdate(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	updateSrc, err := os.ReadFile("update.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mainSrc), "a.shutdownStructured()") {
		t.Fatal("main.go signal path does not shut structured managers through shutdownStructured")
	}
	if !strings.Contains(string(updateSrc), "a.shutdownStructured()") {
		t.Fatal("update.go re-exec path does not shut structured managers through shutdownStructured")
	}
	if strings.Contains(string(mainSrc), "a.muse") && !strings.Contains(string(mainSrc), "shutdownStructured") {
		t.Fatal("main.go talks to Muse outside shutdownStructured")
	}
}
