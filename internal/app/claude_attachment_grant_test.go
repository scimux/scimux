package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The node's attachment staging dir is granted to Claude through the
// per-launch --settings bundle (permissions.additionalDirectories), not
// --add-dir: Claude 2.1.328+ asks a per-session trust question before the
// session starts for every untrusted --add-dir folder, while command-line
// settings grants are applied without that prompt.

func decodeClaudeSettingsDoc(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("settings JSON: %v\n%s", err, raw)
	}
	return doc
}

// readClaudeSettingsFile decodes a bundle settings.json and pins its private
// file mode.
func readClaudeSettingsFile(t *testing.T, path string) map[string]any {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat settings: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode = %v, want 0600", st.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	return decodeClaudeSettingsDoc(t, raw)
}

// grantedClaudeDirs returns permissions.additionalDirectories, or nil when
// the document carries no permissions key. Any other permissions shape fails.
func grantedClaudeDirs(t *testing.T, doc map[string]any) []string {
	t.Helper()
	rawPerm, ok := doc["permissions"]
	if !ok {
		return nil
	}
	perm, ok := rawPerm.(map[string]any)
	if !ok || len(perm) != 1 {
		t.Fatalf("permissions must hold only additionalDirectories, got %#v", rawPerm)
	}
	list, ok := perm["additionalDirectories"].([]any)
	if !ok {
		t.Fatalf("additionalDirectories must be a list, got %#v", perm["additionalDirectories"])
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("additionalDirectories entry is not a string: %#v", v)
		}
		out = append(out, s)
	}
	return out
}

func TestClaudeHookSettingsGrantsAddDirs(t *testing.T) {
	const execPath = "/fixture/bin/scimux"
	const hookDir = "/fixture/hooks/h1"
	const dir = "/fixture/attachments/n1"

	bareRaw, err := claudeHookSettingsJSON(execPath, hookDir)
	if err != nil {
		t.Fatalf("settings without dirs: %v", err)
	}
	bare := decodeClaudeSettingsDoc(t, bareRaw)
	if _, ok := bare["permissions"]; ok {
		t.Fatalf("settings without dirs must not carry permissions: %s", bareRaw)
	}

	grantRaw, err := claudeHookSettingsJSON(execPath, hookDir, dir)
	if err != nil {
		t.Fatalf("settings with dirs: %v", err)
	}
	grant := decodeClaudeSettingsDoc(t, grantRaw)
	keys := make([]string, 0, len(grant))
	for k := range grant {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"hooks", "permissions"}) {
		t.Fatalf("top-level keys = %v, want [hooks permissions]", keys)
	}
	if got := grantedClaudeDirs(t, grant); !reflect.DeepEqual(got, []string{dir}) {
		t.Fatalf("additionalDirectories = %v, want [%s]", got, dir)
	}
	if !reflect.DeepEqual(grant["hooks"], bare["hooks"]) {
		t.Fatalf("granting a dir changed the hooks document:\nwith:    %s\nwithout: %s", grantRaw, bareRaw)
	}
}

func TestPrepareClaudeHookBundleGrantsAttachmentDir(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if a.attachmentsDir == "" {
		t.Fatal("test app must configure an attachments dir")
	}
	_, settingsPath, err := a.prepareClaudeHookBundle("n1")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	got := grantedClaudeDirs(t, readClaudeSettingsFile(t, settingsPath))
	if want := []string{a.attachmentDir("n1")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("granted dirs = %v, want %v", got, want)
	}

	t.Run("no attachments dir configured", func(t *testing.T) {
		a.attachmentsDir = ""
		_, settingsPath, err := a.prepareClaudeHookBundle("n2")
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		doc := readClaudeSettingsFile(t, settingsPath)
		if _, ok := doc["permissions"]; ok {
			t.Fatalf("no attachments dir must mean no permissions grant, got %#v", doc["permissions"])
		}
	})
}

// The staging dir must exist before Claude starts, so a launch whose dir
// cannot be created fails closed: no hook bundle, no tmux session.
func TestClaudeLaunchFailsClosedWhenAttachmentDirCannotBeCreated(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.attachmentsDir = filepath.Join(blocker, "attachments")
	n := &Node{
		ID: "att-fail", Title: "Att", Prompt: "hi", Agent: "claude",
		Dir: a.home, SessionID: "sess-att-fail", CreatedAt: "2026-08-10T00:00:00Z",
	}
	status, err := a.launchNode(n, nil)
	if status != 500 || err == nil || !strings.Contains(err.Error(), "create attachment dir") {
		t.Fatalf("launchNode = %d, %v; want 500 create attachment dir", status, err)
	}
	if argv := fakeNewSessionArgv(f); argv != "" {
		t.Fatalf("failed launch started a tmux session: %q", argv)
	}
	if bundles, _ := filepath.Glob(filepath.Join(a.claudeHooksDir(), "*", "settings.json")); len(bundles) != 0 {
		t.Fatalf("failed launch wrote hook settings: %v", bundles)
	}
}

// Separate scimux data dirs (homes) stay separate: each instance grants the
// staging dir inside its own data dir and writes its own settings file, even
// for the same node id.
func TestClaudeAttachmentGrantFollowsEachDataDir(t *testing.T) {
	a1 := newTestApp(t, &fakeTmux{})
	a2 := newTestApp(t, &fakeTmux{})
	sep := string(filepath.Separator)
	var grants, settings []string
	for i, a := range []*app{a1, a2} {
		_, settingsPath, err := a.prepareClaudeHookBundle("n1")
		if err != nil {
			t.Fatalf("instance %d prepare: %v", i+1, err)
		}
		got := grantedClaudeDirs(t, readClaudeSettingsFile(t, settingsPath))
		if len(got) != 1 || got[0] != a.attachmentDir("n1") {
			t.Fatalf("instance %d granted %v, want [%s]", i+1, got, a.attachmentDir("n1"))
		}
		dataDir := filepath.Dir(a.attachmentsDir)
		if !strings.HasPrefix(got[0], filepath.Join(dataDir, "attachments")+sep) {
			t.Fatalf("instance %d grant %q is outside its data dir %q", i+1, got[0], dataDir)
		}
		if !strings.HasPrefix(settingsPath, a.claudeHooksDir()+sep) {
			t.Fatalf("instance %d settings %q outside its hooks root %q", i+1, settingsPath, a.claudeHooksDir())
		}
		grants = append(grants, got[0])
		settings = append(settings, settingsPath)
	}
	if grants[0] == grants[1] {
		t.Fatalf("two data dirs granted the same folder %q", grants[0])
	}
	if filepath.Dir(settings[0]) == filepath.Dir(settings[1]) {
		t.Fatalf("two instances share a settings bundle %q", filepath.Dir(settings[0]))
	}
}
