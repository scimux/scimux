package app

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func elicitationHookBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "elicitation", "active"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func elicitationEventJSON(event, sid, server, message string, extra map[string]any) []byte {
	doc := map[string]any{
		"hook_event_name": event,
		"session_id":      sid,
		"mcp_server_name": server,
		"message":         message,
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

func listElicitationFiles(t *testing.T, bundle string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(bundle, "elicitation", "active"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	return names
}

func readElicitationRecords(t *testing.T, bundle string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, name := range listElicitationFiles(t, bundle) {
		var doc map[string]any
		if err := json.Unmarshal(mustRead(t, filepath.Join(bundle, "elicitation", "active", name)), &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out = append(out, doc)
	}
	return out
}

func TestClaudeHookSettingsRegistersElicitationHooks(t *testing.T) {
	raw, err := claudeHookSettingsJSON("/tmp/scimux dir/scimux", "/tmp/scimux hooks/hook-id")
	if err != nil {
		t.Fatalf("settings JSON: %v", err)
	}
	hooks := parseClaudeHookSettings(t, raw)
	for _, name := range []string{"Elicitation", "ElicitationResult"} {
		if len(hooks[name]) != 1 || len(hooks[name][0].Hooks) != 1 {
			t.Fatalf("want exactly one %s hook: %s", name, raw)
		}
		cmd := hooks[name][0].Hooks[0].Command
		if !strings.Contains(cmd, "__claude-elicitation-hook") {
			t.Fatalf("%s command = %q, want the elicitation helper", name, cmd)
		}
		if hooks[name][0].Matcher != "" {
			t.Fatalf("%s matcher = %q, want omitted so every MCP server is covered", name, hooks[name][0].Matcher)
		}
	}
	if hooks["Elicitation"][0].Hooks[0].Command != hooks["ElicitationResult"][0].Hooks[0].Command {
		t.Fatalf("Elicitation and ElicitationResult must invoke the same helper: %s", raw)
	}
	if strings.Contains(string(raw), "elicitation_dialog") || strings.Contains(string(raw), "elicitation_url_dialog") {
		t.Fatalf("Notification must not gain elicitation matchers: %s", raw)
	}
	if strings.Contains(string(raw), "PreToolUse") {
		t.Fatalf("PreToolUse must never be registered: %s", raw)
	}
}

func TestClaudeHookBundleCarriesElicitationCapability(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	bundle := filepath.Join(a.claudeHooksDir(), hookID)
	for _, sub := range []string{"elicitation", filepath.Join("elicitation", "active")} {
		st, err := os.Stat(filepath.Join(bundle, sub))
		if err != nil {
			t.Fatalf("%s: %v", sub, err)
		}
		if !st.IsDir() || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v, want dir 0700", sub, st.Mode())
		}
	}
	var caps struct {
		Elicitation int `json:"elicitation"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(bundle, "capabilities.json")), &caps); err != nil {
		t.Fatal(err)
	}
	if caps.Elicitation != 1 {
		t.Fatalf("capabilities.json elicitation = %d, want 1", caps.Elicitation)
	}
}

func TestBundleCompleteCurrentRequiresElicitation(t *testing.T) {
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
	delete(doc, "elicitation")
	stripped, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "capabilities.json"), stripped, 0o600); err != nil {
		t.Fatal(err)
	}
	if bundleCompleteCurrent(bundle) {
		t.Fatal("a bundle lacking the elicitation capability must not be complete-current")
	}

	if err := os.WriteFile(filepath.Join(bundle, "capabilities.json"), caps, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(bundle, "elicitation"))
	if bundleCompleteCurrent(bundle) {
		t.Fatal("a bundle lacking elicitation/ must not be complete-current")
	}

	if err := os.MkdirAll(filepath.Join(bundle, "elicitation"), 0o700); err != nil {
		t.Fatal(err)
	}
	if bundleCompleteCurrent(bundle) {
		t.Fatal("a bundle lacking elicitation/active must not be complete-current")
	}
}

func TestClaudeElicitationHookWritesActiveRecord(t *testing.T) {
	root := elicitationHookBundle(t)
	var stdout, stderr bytes.Buffer
	body := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "Please provide your credentials", map[string]any{
		"mode":              "form",
		"elicitation_id":    "elicit-123",
		"requested_schema":  map[string]any{"type": "object", "properties": map[string]any{"token": map[string]any{"type": "string"}}},
		"secret_should_not": "do not persist schema",
	})
	if err := RunClaudeElicitationHook(root, bytes.NewReader(body), &stdout, &stderr); err != nil {
		t.Fatalf("helper: %v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("helper wrote output stdout=%q stderr=%q", stdout.Bytes(), stderr.Bytes())
	}
	files := listElicitationFiles(t, root)
	if len(files) != 1 {
		t.Fatalf("active files = %v, want one nonce-named record", files)
	}
	if files[0] == "docs-server.json" || strings.Contains(files[0], "elicit-123") {
		t.Fatalf("filename %q must be a hook-minted nonce, not server or elicitation id", files[0])
	}
	st, err := os.Stat(filepath.Join(root, "elicitation", "active", files[0]))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("record mode = %o, want 0600", st.Mode().Perm())
	}
	raw := string(mustRead(t, filepath.Join(root, "elicitation", "active", files[0])))
	if strings.Contains(raw, "requested_schema") || strings.Contains(raw, "do not persist") || strings.Contains(raw, "token") {
		t.Fatalf("requested_schema retained: %s", raw)
	}
	recs := readElicitationRecords(t, root)
	doc := recs[0]
	if doc["session_id"] != hookSIDOwn || doc["mcp_server_name"] != "docs-server" {
		t.Fatalf("record = %v", doc)
	}
	if doc["message"] != "Please provide your credentials" || doc["mode"] != "form" {
		t.Fatalf("record = %v", doc)
	}
	if _, ok := doc["elicitation_id"]; ok {
		t.Fatalf("raw elicitation id must not be retained: %v", doc)
	}
	if hash, _ := doc["elicitation_id_hash"].(string); hash == "" {
		t.Fatalf("elicitation id digest missing: %v", doc)
	}
	if _, ok := doc["at"].(string); !ok || doc["at"] == "" {
		t.Fatal("record must carry a timestamp")
	}
	nonce, _ := doc["nonce"].(string)
	if nonce == "" || nonce+".json" != files[0] {
		t.Fatalf("nonce %q does not match filename %s", nonce, files[0])
	}
}

func TestClaudeElicitationHookURLModePersistsValidatedURLOnly(t *testing.T) {
	root := elicitationHookBundle(t)
	body := elicitationEventJSON("Elicitation", hookSIDOwn, "auth", "Please authenticate", map[string]any{
		"mode": "url",
		"url":  "https://auth.example.com/login",
	})
	if err := RunClaudeElicitationHook(root, bytes.NewReader(body), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	doc := readElicitationRecords(t, root)[0]
	if doc["url"] != "https://auth.example.com/login" {
		t.Fatalf("url = %v", doc["url"])
	}

	root2 := elicitationHookBundle(t)
	bad := elicitationEventJSON("Elicitation", hookSIDOwn, "auth", "Please authenticate", map[string]any{
		"mode": "url",
		"url":  "javascript:alert(1)",
	})
	if err := RunClaudeElicitationHook(root2, bytes.NewReader(bad), io.Discard, io.Discard); err != nil {
		t.Fatalf("invalid URL must not fail the whole event: %v", err)
	}
	doc2 := readElicitationRecords(t, root2)[0]
	if _, ok := doc2["url"]; ok {
		t.Fatalf("invalid URL must be omitted, got %v", doc2["url"])
	}

	root3 := elicitationHookBundle(t)
	formWithURL := elicitationEventJSON("Elicitation", hookSIDOwn, "auth", "Form input", map[string]any{
		"mode": "form",
		"url":  "https://auth.example.com/must-not-be-retained",
	})
	if err := RunClaudeElicitationHook(root3, bytes.NewReader(formWithURL), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, ok := readElicitationRecords(t, root3)[0]["url"]; ok {
		t.Fatal("a URL supplied outside url mode must not be retained")
	}
}

func TestClaudeElicitationResultRemovesByID(t *testing.T) {
	root := elicitationHookBundle(t)
	first := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "one", map[string]any{
		"mode": "form", "elicitation_id": "elicit-a",
	})
	second := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "two", map[string]any{
		"mode": "form", "elicitation_id": "elicit-b",
	})
	if err := RunClaudeElicitationHook(root, bytes.NewReader(first), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := RunClaudeElicitationHook(root, bytes.NewReader(second), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if n := len(listElicitationFiles(t, root)); n != 2 {
		t.Fatalf("want 2 active, got %d", n)
	}
	result := elicitationEventJSON("ElicitationResult", hookSIDOwn, "docs-server", "", map[string]any{
		"action": "accept", "mode": "form", "elicitation_id": "elicit-a",
		"content": map[string]any{"username": "SECRET"},
	})
	var stdout bytes.Buffer
	if err := RunClaudeElicitationHook(root, bytes.NewReader(result), &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("result stdout = %q", stdout.Bytes())
	}
	left := readElicitationRecords(t, root)
	if len(left) != 1 || left[0]["message"] != "two" {
		t.Fatalf("older result must not clear the newer request: %v", left)
	}
	raw := string(mustRead(t, filepath.Join(root, "elicitation", "active", listElicitationFiles(t, root)[0])))
	if strings.Contains(raw, "SECRET") || strings.Contains(raw, "content") {
		t.Fatalf("result content retained: %s", raw)
	}
	if err := RunClaudeElicitationHook(root, bytes.NewReader(result), io.Discard, io.Discard); err != nil {
		t.Fatalf("repeated result: %v", err)
	}
}

func TestClaudeElicitationResultIDIsScopedToMCPServer(t *testing.T) {
	root := elicitationHookBundle(t)
	for _, server := range []string{"server-a", "server-b"} {
		body := elicitationEventJSON("Elicitation", hookSIDOwn, server, "input for "+server, map[string]any{
			"mode": "form", "elicitation_id": "shared-id",
		})
		if err := RunClaudeElicitationHook(root, bytes.NewReader(body), io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	result := elicitationEventJSON("ElicitationResult", hookSIDOwn, "server-b", "", map[string]any{
		"action": "accept", "mode": "form", "elicitation_id": "shared-id",
	})
	if err := RunClaudeElicitationHook(root, bytes.NewReader(result), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	left := readElicitationRecords(t, root)
	if len(left) != 1 || left[0]["mcp_server_name"] != "server-a" {
		t.Fatalf("result from server-b must leave server-a's same-id request: %v", left)
	}
}

func TestClaudeElicitationLongIDStillResolvesExactly(t *testing.T) {
	root := elicitationHookBundle(t)
	id := strings.Repeat("long-id-", 100)
	pre := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "input", map[string]any{
		"mode": "form", "elicitation_id": id,
	})
	if err := RunClaudeElicitationHook(root, bytes.NewReader(pre), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw := string(mustRead(t, filepath.Join(root, "elicitation", "active", listElicitationFiles(t, root)[0])))
	if strings.Contains(raw, id) || !strings.Contains(raw, "elicitation_id_hash") {
		t.Fatalf("long correlation id must be retained losslessly as a digest, not raw: %s", raw)
	}
	post := elicitationEventJSON("ElicitationResult", hookSIDOwn, "docs-server", "", map[string]any{
		"action": "accept", "mode": "form", "elicitation_id": id,
	})
	if err := RunClaudeElicitationHook(root, bytes.NewReader(post), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := len(listElicitationFiles(t, root)); got != 0 {
		t.Fatalf("long elicitation id did not resolve exactly: %d records remain", got)
	}
}

func TestClaudeElicitationLongServerNameStillResolves(t *testing.T) {
	root := elicitationHookBundle(t)
	server := strings.Repeat("server-name-", 40)
	pre := elicitationEventJSON("Elicitation", hookSIDOwn, server, "input", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(root, bytes.NewReader(pre), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	post := elicitationEventJSON("ElicitationResult", hookSIDOwn, server, "", map[string]any{
		"action": "cancel", "mode": "form",
	})
	if err := RunClaudeElicitationHook(root, bytes.NewReader(post), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := len(listElicitationFiles(t, root)); got != 0 {
		t.Fatalf("long MCP server name did not resolve: %d records remain", got)
	}
}

func TestClaudeElicitationResultFallbackRequiresUniqueMatch(t *testing.T) {
	root := elicitationHookBundle(t)
	a := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "one", map[string]any{"mode": "form"})
	b := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "two", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(root, bytes.NewReader(a), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := RunClaudeElicitationHook(root, bytes.NewReader(b), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	result := elicitationEventJSON("ElicitationResult", hookSIDOwn, "docs-server", "", map[string]any{
		"action": "cancel", "mode": "form",
	})
	if err := RunClaudeElicitationHook(root, bytes.NewReader(result), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if n := len(listElicitationFiles(t, root)); n != 2 {
		t.Fatalf("ambiguous fallback must remove nothing, got %d", n)
	}

	root2 := elicitationHookBundle(t)
	if err := RunClaudeElicitationHook(root2, bytes.NewReader(a), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := RunClaudeElicitationHook(root2, bytes.NewReader(result), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if n := len(listElicitationFiles(t, root2)); n != 0 {
		t.Fatalf("unique fallback must remove the one match, got %d", n)
	}
}

func TestClaudeElicitationResultForeignSessionLeavesRecords(t *testing.T) {
	root := elicitationHookBundle(t)
	pre := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "one", map[string]any{
		"mode": "form", "elicitation_id": "elicit-a",
	})
	if err := RunClaudeElicitationHook(root, bytes.NewReader(pre), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	post := elicitationEventJSON("ElicitationResult", hookSIDSuccessor, "docs-server", "", map[string]any{
		"action": "accept", "elicitation_id": "elicit-a",
	})
	_ = RunClaudeElicitationHook(root, bytes.NewReader(post), io.Discard, io.Discard)
	if n := len(listElicitationFiles(t, root)); n != 1 {
		t.Fatalf("foreign result cleared current records: %d", n)
	}
}

func TestClaudeElicitationHookRejectsBadInput(t *testing.T) {
	valid := elicitationHookBundle(t)
	pre := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "hello", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(valid, bytes.NewReader(pre), io.Discard, io.Discard); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seededName := listElicitationFiles(t, valid)[0]
	seeded := mustRead(t, filepath.Join(valid, "elicitation", "active", seededName))

	oversize, err := json.Marshal(map[string]any{
		"hook_event_name": "Elicitation",
		"session_id":      hookSIDOwn,
		"mcp_server_name": "docs-server",
		"message":         "hello",
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
		{"unknown event", valid, elicitationEventJSON("SessionStart", hookSIDOwn, "docs-server", "hello", nil)},
		{"unknown mode", valid, elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "hello", map[string]any{"mode": "dialog"})},
		{"missing server", valid, elicitationEventJSON("Elicitation", hookSIDOwn, "", "hello", nil)},
		{"missing message", valid, elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "", nil)},
		{"unknown action", valid, elicitationEventJSON("ElicitationResult", hookSIDOwn, "docs-server", "", map[string]any{"action": "allow"})},
		{"relative dir", "relative/dir", pre},
		{"unclean dir", valid + "/../" + filepath.Base(valid), pre},
		{"missing directory", filepath.Join(t.TempDir(), "gone"), pre},
		{"oversized", valid, oversize},
		{"trailing garbage", valid, append(append([]byte(nil), pre...), []byte(" trailing garbage")...)},
		{"second json value", valid, append(append([]byte(nil), pre...), []byte(`{"hook_event_name":"Elicitation"}`)...)},
		{"trailing whitespace exceeds limit", valid, append(append([]byte(nil), pre...), bytes.Repeat([]byte(" "), claudeElicitationStdinLimit)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := RunClaudeElicitationHook(tc.dir, bytes.NewReader(tc.body), &stdout, &stderr)
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("output stdout=%q stderr=%q", stdout.Bytes(), stderr.Bytes())
			}
			if err == nil {
				t.Fatal("invalid input must be rejected")
			}
			got, readErr := os.ReadFile(filepath.Join(valid, "elicitation", "active", seededName))
			if readErr != nil {
				t.Fatalf("seed record disappeared: %v", readErr)
			}
			if !bytes.Equal(got, seeded) {
				t.Fatalf("reject path mutated record:\n%s\nvs\n%s", got, seeded)
			}
		})
	}
}

func TestRemoveClaudeElicitationIsIdempotentAndDurable(t *testing.T) {
	src := mustRead(t, "claude_elicitation.go")
	if !strings.Contains(string(src), "sessionlog.SyncParentDir") {
		t.Fatal("successful removal must sync the parent directory")
	}
	root := elicitationHookBundle(t)
	pre := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "hello", map[string]any{
		"mode": "form", "elicitation_id": "elicit-a",
	})
	if err := RunClaudeElicitationHook(root, bytes.NewReader(pre), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	post := elicitationEventJSON("ElicitationResult", hookSIDOwn, "docs-server", "", map[string]any{
		"action": "decline", "elicitation_id": "elicit-a",
	})
	if err := RunClaudeElicitationHook(root, bytes.NewReader(post), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if n := len(listElicitationFiles(t, root)); n != 0 {
		t.Fatalf("record still present: %d", n)
	}
	if err := RunClaudeElicitationHook(root, bytes.NewReader(post), io.Discard, io.Discard); err != nil {
		t.Fatalf("missing record must be idempotent: %v", err)
	}
}

func TestClaudeElicitationHookMainAlwaysExitsZero(t *testing.T) {
	if code := runClaudeElicitationHookMain([]string{"--dir", "relative"}); code != 0 {
		t.Fatalf("exit code = %d, want 0 even on the reject path", code)
	}
}

func TestClaudeElicitationHookNeverReturnsHookSpecificOutput(t *testing.T) {
	src := string(mustRead(t, "claude_elicitation.go"))
	for _, bad := range []string{`"hookSpecificOutput"`, `"updatedPermissions"`} {
		if strings.Contains(src, bad) {
			t.Fatalf("elicitation helper must not return %s", bad)
		}
	}
}

func seedElicitationNode(t *testing.T, capture string) (*app, *Node, string) {
	t.Helper()
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: capture}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.live[n.ID] = "quiet"
	return a, n, bundle
}

func TestClaudeElicitationRaisesQuestionAttentionIncludingWhenArmed(t *testing.T) {
	a, n, bundle := seedElicitationNode(t, "quiet pane")
	body := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "Need a token", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(body), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	a.poll()
	if got := a.attn[n.ID]; got != "question" {
		t.Fatalf("attention = %q, want question", got)
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	a.poll()
	if got := a.attn[n.ID]; got != "question" {
		t.Fatalf("armed elicitation attention = %q, want question", got)
	}
	src := string(mustRead(t, "claude_elicitation.go"))
	if strings.Contains(src, `"behavior":"allow"`) {
		t.Fatal("elicitation must never auto-answer")
	}
}

func TestClaudeElicitationResultClearsAttentionOnNextPoll(t *testing.T) {
	a, n, bundle := seedElicitationNode(t, "quiet pane")
	pre := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "Need a token", map[string]any{
		"mode": "form", "elicitation_id": "elicit-a",
	})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(pre), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	a.poll()
	if got := a.attn[n.ID]; got != "question" {
		t.Fatalf("attention = %q, want question", got)
	}
	post := elicitationEventJSON("ElicitationResult", hookSIDOwn, "docs-server", "", map[string]any{
		"action": "accept", "elicitation_id": "elicit-a",
	})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(post), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	a.poll()
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("attention after result = %q, want none", got)
	}
}

func TestClaudeElicitationKeepsAttentionUntilAllResolve(t *testing.T) {
	a, n, bundle := seedElicitationNode(t, "quiet pane")
	first := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "one", map[string]any{
		"mode": "form", "elicitation_id": "elicit-a",
	})
	second := elicitationEventJSON("Elicitation", hookSIDOwn, "other-server", "two", map[string]any{
		"mode": "form", "elicitation_id": "elicit-b",
	})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(first), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(second), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	a.poll()
	if got := a.attn[n.ID]; got != "question" {
		t.Fatalf("attention = %q", got)
	}
	post := elicitationEventJSON("ElicitationResult", hookSIDOwn, "docs-server", "", map[string]any{
		"action": "cancel", "elicitation_id": "elicit-a",
	})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(post), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	a.poll()
	if got := a.attn[n.ID]; got != "question" {
		t.Fatalf("remaining request must keep attention, got %q", got)
	}
}

func TestClaudeElicitationCoexistsWithPermissionDialog(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "1. Yes"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "d1"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "ask-a"), note); err != nil {
		t.Fatal(err)
	}
	ep, err := mintClaudeVisibleEpoch(perm, hookSIDOwn)
	if err != nil {
		t.Fatal(err)
	}
	body := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "Need a token", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(body), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	a.poll()
	if got := a.attn[n.ID]; got != "approval" {
		t.Fatalf("unarmed permission dialog should keep approval attention, got %q", got)
	}
	chat := chatBody(t, a, n.ID)
	if chat["perm_dialog_id"] != ep.Epoch {
		t.Fatalf("perm_dialog_id = %v, elicitation must not overwrite permission identity", chat["perm_dialog_id"])
	}
	if chat["elicitation_waiting"] != true {
		t.Fatal("elicitation must still be reported alongside a permission dialog")
	}
	if _, ok := chat["perm_request_id"]; ok {
		t.Fatal("elicitation must not invent perm_request_id")
	}
}

func TestClaudeElicitationDoesNotWriteSessionLogOrSeam(t *testing.T) {
	a, n, bundle := seedElicitationNode(t, "quiet pane")
	before := mustRead(t, a.sessionLogPath(n.ID))
	body := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "Need a token", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(body), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	a.poll()
	after := mustRead(t, a.sessionLogPath(n.ID))
	if bytes.Contains(after, []byte(`"t":"user"`)) || bytes.Contains(after, []byte(`"t":"assistant"`)) ||
		bytes.Contains(after, []byte(`"t":"source"`)) || bytes.Contains(after, []byte(`"t":"key"`)) {
		t.Fatalf("elicitation must not append a transcript turn, source seam, or key audit:\n%s", after)
	}
	if !bytes.Contains(after, []byte(`"t":"attention"`)) {
		t.Fatalf("question attention should still be a mechanical attention edge:\n%s", after)
	}
	_ = before
}

func TestClaudeElicitationTTLExpiresAndRemoves(t *testing.T) {
	root := elicitationHookBundle(t)
	rec := claudeElicitationRecord{
		Nonce:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SessionID:  hookSIDOwn,
		Server:     "docs-server",
		ServerHash: digestElicitationValue("docs-server"),
		Message:    "stale",
		Mode:       "form",
		At:         time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339Nano),
	}
	if err := writeClaudePermFile(filepath.Join(root, "elicitation", "active", rec.Nonce+".json"), rec); err != nil {
		t.Fatal(err)
	}
	if n := len(liveClaudeElicitations(root, hookSIDOwn, time.Now())); n != 0 {
		t.Fatalf("expired records must be ignored, got %d", n)
	}
	if _, err := os.Stat(filepath.Join(root, "elicitation", "active", rec.Nonce+".json")); !os.IsNotExist(err) {
		t.Fatal("expired records must eventually be removed")
	}
}

func TestClaudeElicitationClearsOnPermissionTurnReset(t *testing.T) {
	a, n, bundle := seedElicitationNode(t, "quiet pane")
	body := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "Need a token", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(body), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	a.resetClaudePermissionTurn(n)
	if n := len(listElicitationFiles(t, bundle)); n != 0 {
		t.Fatalf("turn reset must clear elicitation records, got %d", n)
	}
}

func TestClaudeElicitationStartedBeforeTurnCloseCannotReappear(t *testing.T) {
	a, n, bundle := seedElicitationNode(t, "quiet pane")
	turn := claudeAcceptedTurn{
		Turn: "1111222233334444", Gen: 1, Session: hookSIDOwn,
		At: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
	}
	if err := writeClaudeAcceptedTurn(filepath.Join(bundle, "perm"), turn); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.claudeTurns[n.ID] = turn
	a.mu.Unlock()
	started := time.Now().UTC()
	a.resetClaudePermissionTurn(n)

	// Model a helper that received Elicitation before the boundary but won
	// the final atomic publish only after boundary cleanup scanned active/.
	late := claudeElicitationEvent{
		HookEventName: "Elicitation", SessionID: hookSIDOwn,
		Server: "docs-server", Message: "stale", Mode: "form",
		ObservedAt: started,
	}
	if err := publishClaudeElicitation(bundle, late); err != nil {
		t.Fatal(err)
	}
	chat := chatBody(t, a, n.ID)
	if chat["elicitation_waiting"] == true {
		t.Fatal("an elicitation observed before the closed-turn watermark reappeared")
	}
	if got := len(listElicitationFiles(t, bundle)); got != 0 {
		t.Fatalf("closed-turn elicitation leak was not retired: %d", got)
	}

	manual := late
	manual.Message = "new terminal turn"
	manual.ObservedAt = time.Now().Add(time.Millisecond).UTC()
	if err := publishClaudeElicitation(bundle, manual); err != nil {
		t.Fatal(err)
	}
	chat = chatBody(t, a, n.ID)
	if chat["elicitation_waiting"] != true {
		t.Fatal("an untagged elicitation observed after the boundary must remain visible")
	}
}

func TestClaudeSessionStartCompactResumeDoNotClearElicitation(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("keep", 0))
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	n.Transcript = cur
	n.Dir = "/w/proj"
	writeSeedLog(t, a, n.ID, "history")
	body := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "Need a token", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(body), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "compact",
		SessionID: hookSIDOwn, TranscriptPath: cur, Cwd: "/w/proj",
	}); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if n := len(listElicitationFiles(t, bundle)); n != 1 {
		t.Fatalf("same-identity compact cleared elicitation: %d", n)
	}
	if err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "resume",
		SessionID: hookSIDOwn, TranscriptPath: cur, Cwd: "/w/proj",
	}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := len(listElicitationFiles(t, bundle)); n != 1 {
		t.Fatalf("same-identity resume cleared elicitation: %d", n)
	}
}

func TestClaudeChangedSessionResumeClearsElicitation(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("keep", 0))
	other := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor, claudeUserLine("next", 0))
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	n.Transcript = cur
	n.Dir = "/w/proj"
	writeSeedLog(t, a, n.ID, "history")
	body := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "Need a token", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(body), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "resume",
		SessionID: hookSIDSuccessor, TranscriptPath: other, Cwd: "/w/proj",
	}); err != nil {
		t.Fatalf("changed resume: %v", err)
	}
	if n := len(listElicitationFiles(t, bundle)); n != 0 {
		t.Fatalf("changed-session rebind must clear elicitation, got %d", n)
	}
}

func TestClaudeElicitationDoesNotChangeCompaction(t *testing.T) {
	a, n, bundle := seedElicitationNode(t, "quiet pane")
	if err := RunClaudeCompactHook(bundle, bytes.NewReader(compactEventJSON("PreCompact", hookSIDOwn, "auto", nil)), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	body := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "Need a token", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(body), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	chat := chatBody(t, a, n.ID)
	if chat["compacting"] != true {
		t.Fatal("elicitation must not hide compaction status")
	}
	if chat["attention"] != nil && chat["attention"] != "" {
		t.Fatalf("chat attention is poller-owned; compacting itself must not set it, got %v", chat["attention"])
	}
	if _, err := os.Stat(claudeCompactActivePath(bundle)); err != nil {
		t.Fatal("compact marker must survive elicitation")
	}
}
