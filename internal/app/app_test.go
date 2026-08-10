package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		"segCache":    a.segCache != nil,
		"fareCache":   a.fareCache != nil,
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
		"storePath":       filepath.Join(data, "nodes.jsonl"),
		"uiPath":          filepath.Join(data, "ui.json"),
		"sessionsDir":     filepath.Join(data, "sessions"),
		"attachmentsDir":  filepath.Join(data, "attachments"),
		"assetsDir":       filepath.Join(data, "assets"),
		"notesDir":        filepath.Join(data, "notes"),
	}
	got := map[string]string{
		"claudeCachePath": a.claudeCachePath,
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
