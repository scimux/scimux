package app

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parseClaudeHookSettings(t *testing.T, raw []byte) map[string][]struct {
	Matcher string `json:"matcher"`
	Hooks   []struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	} `json:"hooks"`
} {
	t.Helper()
	var doc struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("settings JSON: %v\n%s", err, raw)
	}
	return doc.Hooks
}

func TestClaudeHookSettingsRegistersCompactHooks(t *testing.T) {
	raw, err := claudeHookSettingsJSON("/tmp/scimux dir/scimux", "/tmp/scimux hooks/hook-id")
	if err != nil {
		t.Fatalf("settings JSON: %v", err)
	}
	hooks := parseClaudeHookSettings(t, raw)
	for _, name := range []string{"PreCompact", "PostCompact"} {
		if len(hooks[name]) != 1 || len(hooks[name][0].Hooks) != 1 {
			t.Fatalf("want exactly one %s hook: %s", name, raw)
		}
		cmd := hooks[name][0].Hooks[0].Command
		if !strings.Contains(cmd, "__claude-compact-hook") {
			t.Fatalf("%s command = %q, want the compact helper", name, cmd)
		}
		m := hooks[name][0].Matcher
		if !strings.Contains(m, "manual") || !strings.Contains(m, "auto") {
			t.Fatalf("%s matcher = %q, want both manual and auto triggers", name, m)
		}
	}
	if hooks["PreCompact"][0].Hooks[0].Command != hooks["PostCompact"][0].Hooks[0].Command {
		t.Fatalf("PreCompact and PostCompact must invoke the same helper: %s", raw)
	}
	if strings.Contains(string(raw), "PreToolUse") {
		t.Fatalf("PreToolUse must never be registered: %s", raw)
	}
	if strings.Contains(string(raw), "SubagentStop") {
		t.Fatalf("SubagentStop must never be registered: %s", raw)
	}
}

func TestClaudeHookBundleCarriesCompactCapability(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	bundle := filepath.Join(a.claudeHooksDir(), hookID)
	compactDir := filepath.Join(bundle, "compact")
	st, err := os.Stat(compactDir)
	if err != nil {
		t.Fatalf("compact/: %v", err)
	}
	if !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("compact/ mode = %v, want dir 0700", st.Mode())
	}
	var caps struct {
		Compact int `json:"compact"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(bundle, "capabilities.json")), &caps); err != nil {
		t.Fatal(err)
	}
	if caps.Compact != 1 {
		t.Fatalf("capabilities.json compact = %d, want 1", caps.Compact)
	}
}

func TestBundleCompleteCurrentRequiresCompact(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	bundle := filepath.Join(a.claudeHooksDir(), hookID)
	if !bundleCompleteCurrent(bundle) {
		t.Fatal("freshly prepared current bundle must be complete")
	}

	caps := mustRead(t, filepath.Join(bundle, "capabilities.json"))
	var doc map[string]any
	if err := json.Unmarshal(caps, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "compact")
	stripped, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "capabilities.json"), stripped, 0o600); err != nil {
		t.Fatal(err)
	}
	if bundleCompleteCurrent(bundle) {
		t.Fatal("a bundle lacking the compact capability must not be complete-current")
	}

	if err := os.WriteFile(filepath.Join(bundle, "capabilities.json"), caps, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(bundle, "compact"))
	if bundleCompleteCurrent(bundle) {
		t.Fatal("a bundle lacking compact/ must not be complete-current")
	}
}

func TestClaudeHookBundleLeavesUserAndProjectSettingsUntouched(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	n.Dir = filepath.Join(a.home, "work")
	userSettings := filepath.Join(a.home, ".claude", "settings.json")
	projectSettings := filepath.Join(n.Dir, ".claude", "settings.local.json")
	userBody := []byte(`{"hooks":{"SessionStart":[{"type":"command","command":"echo user"}]}}`)
	projectBody := []byte(`{"hooks":{"SessionStart":[{"type":"command","command":"echo project"}]}}`)
	if err := os.MkdirAll(filepath.Dir(userSettings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(projectSettings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userSettings, userBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectSettings, projectBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.prepareClaudeHookBundle(n.ID); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !bytes.Equal(mustRead(t, userSettings), userBody) {
		t.Fatal("prepare mutated user ~/.claude/settings.json")
	}
	if !bytes.Equal(mustRead(t, projectSettings), projectBody) {
		t.Fatal("prepare mutated project settings.local.json")
	}
}

func compactHookBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "compact"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func compactEventJSON(event, sid, trigger string, extra map[string]any) []byte {
	doc := map[string]any{
		"hook_event_name": event,
		"session_id":      sid,
		"trigger":         trigger,
	}
	for k, v := range extra {
		doc[k] = v
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return b
}

func readCompactMarker(t *testing.T, bundle string) (map[string]any, os.FileMode) {
	t.Helper()
	path := claudeCompactActivePath(bundle)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("active marker: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(mustRead(t, path), &doc); err != nil {
		t.Fatalf("marker JSON: %v", err)
	}
	return doc, st.Mode().Perm()
}

func TestClaudeCompactHookWritesActiveMarker(t *testing.T) {
	for _, trigger := range []string{"auto", "manual"} {
		t.Run(trigger, func(t *testing.T) {
			root := compactHookBundle(t)
			var stdout, stderr bytes.Buffer
			body := compactEventJSON("PreCompact", hookSIDOwn, trigger, map[string]any{
				"custom_instructions": "do not persist this",
			})
			if err := RunClaudeCompactHook(root, bytes.NewReader(body), &stdout, &stderr); err != nil {
				t.Fatalf("helper: %v", err)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("helper wrote output stdout=%q stderr=%q", stdout.Bytes(), stderr.Bytes())
			}
			doc, mode := readCompactMarker(t, root)
			if mode != 0o600 {
				t.Fatalf("marker mode = %o, want 0600", mode)
			}
			if len(doc) != 3 {
				t.Fatalf("marker fields = %v, want only session_id, trigger, timestamp", doc)
			}
			if doc["session_id"] != hookSIDOwn {
				t.Fatalf("session_id = %v", doc["session_id"])
			}
			if doc["trigger"] != trigger {
				t.Fatalf("trigger = %v, want %s", doc["trigger"], trigger)
			}
			at, _ := doc["at"].(string)
			if at == "" {
				t.Fatal("marker must carry a timestamp")
			}
			raw := string(mustRead(t, claudeCompactActivePath(root)))
			if strings.Contains(raw, "custom_instructions") || strings.Contains(raw, "do not persist") {
				t.Fatalf("custom_instructions retained: %s", raw)
			}
		})
	}
}

func TestClaudeCompactHookPostCompactRemovesMatchingMarker(t *testing.T) {
	root := compactHookBundle(t)
	pre := compactEventJSON("PreCompact", hookSIDOwn, "auto", nil)
	if err := RunClaudeCompactHook(root, bytes.NewReader(pre), io.Discard, io.Discard); err != nil {
		t.Fatalf("pre: %v", err)
	}
	if _, err := os.Stat(claudeCompactActivePath(root)); err != nil {
		t.Fatalf("expected marker after PreCompact: %v", err)
	}
	post := compactEventJSON("PostCompact", hookSIDOwn, "auto", map[string]any{
		"compact_summary": "SECRET SUMMARY that must not be retained",
	})
	var stdout bytes.Buffer
	if err := RunClaudeCompactHook(root, bytes.NewReader(post), &stdout, io.Discard); err != nil {
		t.Fatalf("post: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("PostCompact stdout = %q", stdout.Bytes())
	}
	if _, err := os.Stat(claudeCompactActivePath(root)); !os.IsNotExist(err) {
		t.Fatalf("marker still present after PostCompact: %v", err)
	}
	ents, err := os.ReadDir(filepath.Join(root, "compact"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b := mustRead(t, filepath.Join(root, "compact", e.Name()))
		if strings.Contains(string(b), "SECRET SUMMARY") || strings.Contains(string(b), "compact_summary") {
			t.Fatalf("compact_summary retained in %s: %s", e.Name(), b)
		}
	}
	if err := RunClaudeCompactHook(root, bytes.NewReader(post), io.Discard, io.Discard); err != nil {
		t.Fatalf("repeated PostCompact: %v", err)
	}
}

func TestClaudeCompactHookPostCompactOtherSessionLeavesMarker(t *testing.T) {
	root := compactHookBundle(t)
	pre := compactEventJSON("PreCompact", hookSIDOwn, "manual", nil)
	if err := RunClaudeCompactHook(root, bytes.NewReader(pre), io.Discard, io.Discard); err != nil {
		t.Fatalf("pre: %v", err)
	}
	post := compactEventJSON("PostCompact", hookSIDSuccessor, "manual", nil)
	if err := RunClaudeCompactHook(root, bytes.NewReader(post), io.Discard, io.Discard); err == nil {
		// Rejection is allowed; the invariant is that the marker stays.
	}
	if _, err := os.Stat(claudeCompactActivePath(root)); err != nil {
		t.Fatalf("foreign PostCompact removed the marker: %v", err)
	}
}

func TestClaudeCompactHookRejectsBadInput(t *testing.T) {
	valid := compactHookBundle(t)
	pre := compactEventJSON("PreCompact", hookSIDOwn, "auto", nil)
	if err := RunClaudeCompactHook(valid, bytes.NewReader(pre), io.Discard, io.Discard); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	seeded := mustRead(t, claudeCompactActivePath(valid))

	oversize, err := json.Marshal(map[string]any{
		"hook_event_name": "PreCompact",
		"session_id":      hookSIDOwn,
		"trigger":         "auto",
		"pad":             strings.Repeat("x", 5<<20),
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		dir  string
		body []byte
	}{
		{"malformed", valid, []byte("{")},
		{"unknown event", valid, compactEventJSON("SessionStart", hookSIDOwn, "auto", nil)},
		{"unknown trigger", valid, compactEventJSON("PreCompact", hookSIDOwn, "idle", nil)},
		{"relative dir", "relative/dir", pre},
		{"unclean dir", valid + "/../" + filepath.Base(valid), pre},
		{"missing directory", filepath.Join(t.TempDir(), "gone"), pre},
		{"oversized", valid, oversize},
		{"trailing garbage", valid, append(append([]byte(nil), pre...), []byte(" trailing garbage")...)},
		{"second json value", valid, append(append([]byte(nil), pre...), []byte(`{"hook_event_name":"PreCompact"}`)...)},
		{"trailing whitespace exceeds limit", valid, append(append([]byte(nil), pre...), bytes.Repeat([]byte(" "), claudeCompactStdinLimit)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := RunClaudeCompactHook(tc.dir, bytes.NewReader(tc.body), &stdout, &stderr)
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("output stdout=%q stderr=%q", stdout.Bytes(), stderr.Bytes())
			}
			if err == nil {
				t.Fatal("invalid input must be rejected")
			}
			got, readErr := os.ReadFile(claudeCompactActivePath(valid))
			if readErr != nil {
				t.Fatalf("seed marker disappeared: %v", readErr)
			}
			if !bytes.Equal(got, seeded) {
				t.Fatalf("reject path mutated marker:\n%s\nvs\n%s", got, seeded)
			}
		})
	}
}

func TestRemoveClaudeCompactMarkerIsIdempotentAndDurable(t *testing.T) {
	src := mustRead(t, "claude_compact.go")
	if !strings.Contains(string(src), "sessionlog.SyncParentDir") {
		t.Fatal("PostCompact removal must sync the parent directory after a successful unlink")
	}
	root := compactHookBundle(t)
	pre := compactEventJSON("PreCompact", hookSIDOwn, "auto", nil)
	if err := RunClaudeCompactHook(root, bytes.NewReader(pre), io.Discard, io.Discard); err != nil {
		t.Fatalf("pre: %v", err)
	}
	post := compactEventJSON("PostCompact", hookSIDOwn, "auto", nil)
	if err := RunClaudeCompactHook(root, bytes.NewReader(post), io.Discard, io.Discard); err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, err := os.Stat(claudeCompactActivePath(root)); !os.IsNotExist(err) {
		t.Fatalf("marker still present after PostCompact: %v", err)
	}
	if err := RunClaudeCompactHook(root, bytes.NewReader(post), io.Discard, io.Discard); err != nil {
		t.Fatalf("missing marker must be idempotent: %v", err)
	}
}

func TestClaudeCompactHookMainAlwaysExitsZero(t *testing.T) {
	if code := runClaudeCompactHookMain([]string{"--dir", "relative"}); code != 0 {
		t.Fatalf("exit code = %d, want 0 even on the reject path", code)
	}
	root := compactHookBundle(t)
	// The command-main wrapper cannot be fed stdin here; the reject-path
	// zero exit is the load-bearing contract (PreCompact must not block).
	_ = root
}
