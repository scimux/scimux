package app

// Phase 1 acceptance suite for claude-fixes.md: hook transport and
// binding/rollover contracts. Tests must fail for missing behavior. Synthetic
// payloads only — no real agent CLI.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

const (
	hookSIDOwn       = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1"
	hookSIDSuccessor = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb2"
	hookSIDForeign   = "cccccccc-cccc-4ccc-8ccc-ccccccccccc3"
	hookSIDOther     = "dddddddd-dddd-4ddd-8ddd-ddddddddddd4"
	hookSIDMismatch  = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeee5"
	hookSIDUnsafe    = "ffffffff-ffff-4fff-8fff-fffffffffff6"
	hookSIDSymlink   = "12121212-1212-4121-8121-121212121212"
	hookSIDDir       = "34343434-3434-4343-8343-343434343434"
	hookSIDParent    = "56565656-5656-4565-8565-565656565656"
)

func hookSessionJSON(source, sid, path string) []byte {
	ev := claudeSessionStartEvent{
		HookEventName:  "SessionStart",
		SessionID:      sid,
		TranscriptPath: path,
		Cwd:            "/w/proj",
		Source:         source,
	}
	b, _ := json.Marshal(ev)
	return b
}

func writeClaudeProject(t *testing.T, home, projectEsc, sid string, lines ...string) string {
	t.Helper()
	proj := filepath.Join(home, ".claude", "projects", projectEsc)
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, sid+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(lines) > 0 {
		appendLines(t, path, lines...)
	}
	return path
}

func seedOwnedClaude(t *testing.T, a *app, id, sid, path string) *Node {
	t.Helper()
	n := &Node{
		ID: id, Title: id, Agent: "claude", Dir: "/w/proj",
		SessionID: sid, Transcript: path,
		Prompt: "first", CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	a.nodes = append(a.nodes, n)
	a.byID[id] = n
	a.claudeHooks[id] = "hook-" + id
	a.claudeGens[id] = 1
	return n
}

func hookInboxReady(t *testing.T, a *app, hookID, name string, payload []byte) string {
	t.Helper()
	inbox := filepath.Join(a.claudeHooksDir(), hookID, "inbox")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(inbox, name+".json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func liveHookDirs(t *testing.T, a *app) []string {
	t.Helper()
	ents, err := os.ReadDir(a.claudeHooksDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var live []string
	for _, e := range ents {
		if e.IsDir() && e.Name() != "archive" {
			live = append(live, e.Name())
		}
	}
	return live
}

func archivedHookDirs(t *testing.T, a *app) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(a.claudeHooksDir(), "archive"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func bindingRecs(t *testing.T, a *app, nodeID string) []storeRecord {
	t.Helper()
	var out []storeRecord
	for _, r := range keyRecords(t, a.storePath) {
		if r.Type == "claude-binding" && r.ID == nodeID {
			out = append(out, r)
		}
	}
	return out
}

func clearSeamCount(t *testing.T, a *app, id string) int {
	t.Helper()
	n := 0
	for _, ev := range sessionlog.ReadEvents(a.sessionLogPath(id)) {
		if ev.T == "source" && ev.Source != nil && ev.Source.Reason == "clear" {
			n++
		}
	}
	return n
}

func userTurnCount(t *testing.T, a *app, id string) int {
	t.Helper()
	n := 0
	for _, ev := range sessionlog.ReadEvents(a.sessionLogPath(id)) {
		if ev.T == "user" {
			n++
		}
	}
	return n
}

func reloadApp(t *testing.T, a *app, f *fakeTmux) *app {
	t.Helper()
	a2, err := newApp(Config{
		Home:        a.home,
		DataDir:     filepath.Dir(a.storePath),
		LaunchGrace: 40 * time.Millisecond,
		LaunchPoll:  5 * time.Millisecond,
	}, appDeps{
		Server:               a.server,
		DeliverClaudeInitial: func(*Node) initialDelivery { return initialAcknowledged },
	})
	if err != nil {
		t.Fatal(err)
	}
	if f != nil && a2.server == nil {
		t.Fatal("reloaded app lost tmux server")
	}
	return a2
}

func launchArgvHasSettings(cmd string) bool {
	iSet := strings.Index(cmd, "--settings")
	iRC := strings.Index(cmd, "--remote-control")
	return iSet >= 0 && iRC >= 0 && iSet < iRC && strings.Count(cmd, "--settings") == 1
}

func TestCreateReturnsAcknowledgedForCRTranscript(t *testing.T) {
	// Phase 2 gate: a synthetic multiline create is acknowledged when the
	// transcript user turn uses CR line endings.
	f := &fakeTmux{captureAfterEnter: "pane moved but that is not delivery proof"}
	a := newTestApp(t, f)
	a.deliverClaudeInitial = a.deliverClaudeInitialPrompt
	a.claudeReadyTimeout = 400 * time.Millisecond
	a.claudeDeliveryTimeout = 400 * time.Millisecond
	a.claudeInitialPoll = 5 * time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(350 * time.Millisecond)
		var n *Node
		for time.Now().Before(deadline) {
			a.mu.Lock()
			if len(a.nodes) > 0 {
				n = a.nodes[0]
			}
			a.mu.Unlock()
			if n != nil && n.SessionID != "" {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if n == nil || n.SessionID == "" {
			return
		}
		path := writeClaudeTranscript(t, a.home, n.SessionID)
		if err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
			HookEventName: "SessionStart", Source: "startup",
			SessionID: n.SessionID, TranscriptPath: path, Cwd: a.home,
		}); err != nil {
			return
		}
		wait := time.Now().Add(200 * time.Millisecond)
		for !f.didSendEnter() && time.Now().Before(wait) {
			time.Sleep(time.Millisecond)
		}
		appendLines(t, path, fmt.Sprintf(
			`{"type":"user","timestamp":"2026-08-13T15:00:00Z","message":{"role":"user","content":%q}}`,
			"line1\rline2"))
	}()
	rec := newNode(a, `{"title":"CRCreate","prompt":"line1\nline2","agent":"claude","dir":`+strconv.Quote(a.home)+`}`)
	<-done
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		InitialDelivery initialDelivery `json:"initial_delivery"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.InitialDelivery != initialPending {
		t.Fatalf("initial_delivery = %q, want pending (create is asynchronous)", body.InitialDelivery)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		st := a.sendState[a.nodes[0].ID]
		a.mu.Unlock()
		if st == "" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background delivery never confirmed the CR transcript")
}

func TestOwnedClaudeLaunchInstallsPrivateSettings(t *testing.T) {
	// AT-HK-01
	f := &fakeTmux{}
	a := newTestApp(t, f)
	rec := newNode(a, `{"title":"HookLaunch","prompt":"p","agent":"claude","dir":`+strconv.Quote(a.home)+`}`)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	cmd := fakeNewSessionArgv(f)
	if !launchArgvHasSettings(cmd) {
		t.Fatalf("owned Claude argv missing private --settings before --remote-control: %s", cmd)
	}

	pi := &Node{Agent: "pi", Prompt: "p"}
	if got, _ := agentCommand(pi, nil); strings.Contains(got, "--settings") {
		t.Fatalf("pi command must not carry --settings: %s", got)
	}
	oc := &Node{Agent: "opencode", Prompt: "p"}
	if got, _ := agentCommand(oc, nil); strings.Contains(got, "--settings") {
		t.Fatalf("opencode command must not carry --settings: %s", got)
	}
	gk := &Node{Agent: "grok", Prompt: "p"}
	if got, _ := agentCommand(gk, nil); strings.Contains(got, "--settings") {
		t.Fatalf("grok command must not carry --settings: %s", got)
	}

	f2 := &fakeTmux{alive: map[string]bool{"adopted": true}}
	a2 := newTestApp(t, f2)
	if rec := adopt(a2, `{"session":"adopted","agent":"claude","dir":`+strconv.Quote(a2.home)+`}`); rec.Code != 200 {
		t.Fatalf("adopt: %d %s", rec.Code, rec.Body.String())
	}
	if cmd := fakeNewSessionArgv(f2); strings.Contains(cmd, "--settings") {
		t.Fatalf("adopt must not launch with --settings: %s", cmd)
	}
}

func TestClaudeHookSettingsJSONIsPrivateSessionStartOnly(t *testing.T) {
	// AT-HK-02: generated JSON is the scimux SessionStart hook only. Paths
	// contain spaces, so the command must use the existing shellQuote form.
	execPath := "/tmp/scimux dir/scimux"
	hookDir := "/tmp/scimux hooks/hook-id"
	raw, err := claudeHookSettingsJSON(execPath, hookDir)
	if err != nil {
		t.Fatalf("settings JSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("settings JSON: %v\n%s", err, raw)
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil || len(doc) != 1 {
		t.Fatalf("settings must contain only hooks, got %s", raw)
	}
	starts, _ := hooks["SessionStart"].([]any)
	if len(starts) != 1 {
		t.Fatalf("want exactly one SessionStart hook, got %s", raw)
	}
	// Which other events may appear is pinned by
	// TestClaudeHookSettingsRegistersPermissionRequestOnly.
	if !strings.Contains(string(raw), claudeSessionHookCmd) {
		t.Fatalf("hook command missing %s: %s", claudeSessionHookCmd, raw)
	}
	if !strings.Contains(string(raw), shellQuote(execPath)) {
		t.Fatalf("executable path must be shell-quoted (space in path): %s", raw)
	}
	if !strings.Contains(string(raw), shellQuote(hookDir)) {
		t.Fatalf("hook dir must be shell-quoted (space in path): %s", raw)
	}
}

func TestClaudeHookBundleLeavesUserSettingsUntouched(t *testing.T) {
	// AT-HK-02 / D3: prepare writes only the private bundle. User and project
	// Claude settings sit where the generator could see them.
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

	hookID, settingsPath, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !bytes.Equal(mustRead(t, userSettings), userBody) {
		t.Fatal("prepare mutated user ~/.claude/settings.json")
	}
	if !bytes.Equal(mustRead(t, projectSettings), projectBody) {
		t.Fatal("prepare mutated project settings.local.json")
	}
	st, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatalf("settings.json: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("settings.json mode = %o, want 0600", st.Mode().Perm())
	}
	for _, dir := range []string{"inbox", "processed", "stop"} {
		p := filepath.Join(a.claudeHooksDir(), hookID, dir)
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if !st.IsDir() || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v, want dir 0700", dir, st.Mode())
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestClaudeSessionHookWritesOneInboxEvent(t *testing.T) {
	// AT-HK-03
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "processed"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := hookSessionJSON("startup", hookSIDOwn, "/tmp/"+hookSIDOwn+".jsonl")
	var stdout, stderr bytes.Buffer
	if err := RunClaudeSessionHook(root, bytes.NewReader(payload), &stdout, &stderr); err != nil {
		t.Fatalf("helper: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("helper wrote stdout %q", stdout.Bytes())
	}
	ents, err := os.ReadDir(inbox)
	if err != nil {
		t.Fatal(err)
	}
	ready := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			ready++
		}
	}
	if ready != 1 {
		t.Fatalf("ready inbox events = %d, want 1 (%v)", ready, ents)
	}
}

func TestClaudeSessionHookRejectsBadInput(t *testing.T) {
	// AT-HK-04
	validDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(validDir, "inbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	archived := filepath.Join(t.TempDir(), "gone")
	var padded map[string]any
	if err := json.Unmarshal(hookSessionJSON("startup", hookSIDOwn, "/tmp/"+hookSIDOwn+".jsonl"), &padded); err != nil {
		t.Fatal(err)
	}
	padded["pad"] = strings.Repeat("x", 64*1024)
	oversize, err := json.Marshal(padded)
	if err != nil {
		t.Fatal(err)
	}
	if len(oversize) <= 64*1024 {
		t.Fatalf("oversize fixture is %d bytes, want > 64KiB", len(oversize))
	}
	cases := []struct {
		name string
		dir  string
		body []byte
	}{
		{"malformed", validDir, []byte("{")},
		{"oversized", validDir, oversize},
		{"wrong event", validDir, []byte(`{"hook_event_name":"SessionEnd","source":"startup","session_id":"` + hookSIDOwn + `","transcript_path":"/tmp/x.jsonl"}`)},
		{"missing directory", archived, hookSessionJSON("startup", hookSIDOwn, "/tmp/x.jsonl")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := RunClaudeSessionHook(tc.dir, bytes.NewReader(tc.body), &stdout, io.Discard)
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q", stdout.Bytes())
			}
			if errors.Is(err, errClaudeHookNotImplemented) {
				t.Fatal("helper must classify invalid input, not return not implemented")
			}
			if err == nil {
				t.Fatal("invalid input must fail")
			}
			if ents, _ := os.ReadDir(filepath.Join(tc.dir, "inbox")); len(ents) != 0 {
				t.Fatalf("ready events after reject: %v", ents)
			}
			if tc.name == "missing directory" {
				if _, statErr := os.Stat(tc.dir); !os.IsNotExist(statErr) {
					t.Fatal("helper must not recreate an archived hook directory")
				}
			}
		})
	}
}

func TestClaudeHookRegistrationReplayDeleteAndSlugReuse(t *testing.T) {
	// AT-HK-05
	f := &fakeTmux{}
	a := newTestApp(t, f)
	rec := newNode(a, `{"title":"HookReplay","prompt":"p","agent":"claude","dir":`+strconv.Quote(a.home)+`}`)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created Node
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Body.String(), "hook_id") {
		t.Fatal("hook id must not appear on the create response")
	}
	hookID := a.claudeHookID(created.ID)
	if hookID == "" {
		t.Fatal("create did not persist a hook registration")
	}

	a2 := reloadApp(t, a, f)
	if got := a2.claudeHookID(created.ID); got != hookID {
		t.Fatalf("replay hook id = %q, want %q", got, hookID)
	}

	del := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/nodes/"+created.ID, nil)
	req.SetPathValue("id", created.ID)
	a.handleDeleteNode(del, req)
	if del.Code != 200 {
		t.Fatalf("delete: %d %s", del.Code, del.Body.String())
	}
	if got := a.claudeHookID(created.ID); got != "" {
		t.Fatalf("delete left hook registration %q", got)
	}

	rec = newNode(a, `{"title":"HookReplay","prompt":"p","agent":"claude","dir":`+strconv.Quote(a.home)+`}`)
	if rec.Code != 200 {
		t.Fatalf("recreate: %d %s", rec.Code, rec.Body.String())
	}
	var again Node
	if err := json.Unmarshal(rec.Body.Bytes(), &again); err != nil {
		t.Fatal(err)
	}
	if again.ID != created.ID {
		t.Fatalf("reused slug = %q, want %q", again.ID, created.ID)
	}
	if got := a.claudeHookID(again.ID); got == "" || got == hookID {
		t.Fatalf("reused slug hook id = %q, want a new id (old %q)", got, hookID)
	}
}

func TestClaudeHookLaunchAndStoreFailureRollback(t *testing.T) {
	// AT-HK-06
	t.Run("launch failure", func(t *testing.T) {
		f := &fakeTmux{capture: "API Error: bad flag\n" + launchFailSentinel + " (status 1)\n"}
		a := newTestApp(t, f)
		rec := newNode(a, `{"title":"HookBoom","prompt":"p","agent":"claude","dir":`+strconv.Quote(a.home)+`}`)
		if rec.Code == 200 {
			t.Fatal("launch failure must not create the node")
		}
		if len(a.claudeHooks) != 0 {
			t.Fatalf("active hook registration after launch failure: %v", a.claudeHooks)
		}
		if live := liveHookDirs(t, a); len(live) != 0 {
			t.Fatalf("live hook bundle after launch failure: %v", live)
		}
	})
	t.Run("store failure", func(t *testing.T) {
		f := &fakeTmux{}
		a := newTestApp(t, f)
		breakStore(t, a)
		rec := newNode(a, `{"title":"HookStore","prompt":"p","agent":"claude","dir":`+strconv.Quote(a.home)+`}`)
		if rec.Code != 500 {
			t.Fatalf("store failure create = %d, want 500", rec.Code)
		}
		if killCount(f) == 0 {
			t.Fatal("store failure must kill the owned Claude tmux session")
		}
		if len(a.claudeHooks) != 0 {
			t.Fatalf("active hook registration after store failure: %v", a.claudeHooks)
		}
		if live := liveHookDirs(t, a); len(live) != 0 {
			t.Fatalf("live hook bundle after store failure: %v", live)
		}
		if arch := archivedHookDirs(t, a); len(arch) == 0 {
			t.Fatal("store failure must archive the unused hook bundle")
		}
	})
}

func TestClaudeStartupHookBindsReportedPath(t *testing.T) {
	// AT-BIND-01
	f := &fakeTmux{}
	a := newTestApp(t, f)
	own := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn,
		`{"type":"system","subtype":"bridge_status","sessionId":"`+hookSIDOwn+`","content":"ready"}`)
	foreign := writeClaudeProject(t, a.home, "-w-proj", hookSIDForeign,
		fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"newer"}}`, time.Now().UTC().Format(time.RFC3339Nano)))
	if err := os.Chtimes(own, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatalf("prepare bundle: %v", err)
	}
	a.mu.Lock()
	a.claudeHooks[n.ID] = hookID
	if a.claudePermCap[hookID] || a.claudeAskedCap[hookID] {
		t.Fatal("writing a hook bundle must not claim that Claude loaded it")
	}
	a.mu.Unlock()
	ev := claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "startup",
		SessionID: hookSIDOwn, TranscriptPath: own, Cwd: "/w/proj",
	}
	if err := a.processClaudeHookEvent(n.ID, ev); err != nil {
		t.Fatalf("startup event: %v", err)
	}
	if n.Transcript != own || n.SessionID != hookSIDOwn {
		t.Fatalf("bound %q / %q, want %q / %q", n.Transcript, n.SessionID, own, hookSIDOwn)
	}
	a.mu.Lock()
	permCap, askedCap := a.claudePermCap[hookID], a.claudeAskedCap[hookID]
	a.mu.Unlock()
	if !permCap || !askedCap {
		t.Fatalf("valid live SessionStart did not activate hook capabilities: permission=%v asked=%v", permCap, askedCap)
	}
	if n.Transcript == foreign {
		t.Fatal("newest foreign JSONL must not win")
	}
	if recs := bindingRecs(t, a, n.ID); len(recs) != 1 || recs[0].Path != own || recs[0].Cause != "startup" {
		t.Fatalf("binding records = %+v", recs)
	}
}

func TestClaudeStartupHookRejectsInvalidEvents(t *testing.T) {
	// AT-BIND-02 + D6 path checks. "uuid mismatch" uses an unclaimed fifth
	// UUID so a pathClaims-only implementation cannot satisfy it.
	f := &fakeTmux{}
	a := newTestApp(t, f)
	own := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn)
	outside := filepath.Join(t.TempDir(), hookSIDOwn+".jsonl")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dup := writeClaudeProject(t, a.home, "-w-other", hookSIDOwn)
	retired := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor)
	claimed := writeClaudeProject(t, a.home, "-w-proj", hookSIDForeign)
	mismatch := writeClaudeProject(t, a.home, "-w-proj", hookSIDMismatch)
	_ = seedOwnedClaude(t, a, "other", hookSIDOther, claimed)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	a.markDeadTranscriptLocked(n.ID, retired, hookSIDSuccessor)
	_ = dup

	escapeDir := filepath.Join(a.home, "escape")
	if err := os.MkdirAll(escapeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	escapeFile := filepath.Join(escapeDir, hookSIDOwn+".jsonl")
	if err := os.WriteFile(escapeFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	traversal := filepath.Join(a.home, ".claude", "projects", "-w-proj", "..", "..", "escape", hookSIDOwn+".jsonl")

	linkDir := filepath.Join(a.home, ".claude", "projects", "-w-link")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideSym := filepath.Join(t.TempDir(), hookSIDSymlink+".jsonl")
	if err := os.WriteFile(outsideSym, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, hookSIDSymlink+".jsonl")
	if err := os.Symlink(outsideSym, link); err != nil {
		t.Fatal(err)
	}
	nonRegDir := filepath.Join(a.home, ".claude", "projects", "-w-dir")
	if err := os.MkdirAll(nonRegDir, 0o755); err != nil {
		t.Fatal(err)
	}
	nonRegular := filepath.Join(nonRegDir, hookSIDDir+".jsonl")
	if err := os.Mkdir(nonRegular, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideProj := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideProj, hookSIDParent+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkProj := filepath.Join(a.home, ".claude", "projects", "-w-parentlink")
	if err := os.Symlink(outsideProj, linkProj); err != nil {
		t.Fatal(err)
	}
	parentLinked := filepath.Join(linkProj, hookSIDParent+".jsonl")

	cases := []struct {
		name   string
		minted string
		ev     claudeSessionStartEvent
	}{
		{"uuid mismatch", hookSIDOwn, claudeSessionStartEvent{HookEventName: "SessionStart", Source: "startup", SessionID: hookSIDMismatch, TranscriptPath: mismatch, Cwd: "/w/proj"}},
		{"basename mismatch", hookSIDOwn, claudeSessionStartEvent{HookEventName: "SessionStart", Source: "startup", SessionID: hookSIDOwn, TranscriptPath: claimed, Cwd: "/w/proj"}},
		{"outside root", hookSIDOwn, claudeSessionStartEvent{HookEventName: "SessionStart", Source: "startup", SessionID: hookSIDOwn, TranscriptPath: outside, Cwd: "/w/proj"}},
		{"relative path", hookSIDOwn, claudeSessionStartEvent{HookEventName: "SessionStart", Source: "startup", SessionID: hookSIDOwn, TranscriptPath: filepath.Join(".claude", "projects", "-w-proj", hookSIDOwn+".jsonl"), Cwd: "/w/proj"}},
		{"dotdot escape", hookSIDOwn, claudeSessionStartEvent{HookEventName: "SessionStart", Source: "startup", SessionID: hookSIDOwn, TranscriptPath: traversal, Cwd: "/w/proj"}},
		{"symlink escape", hookSIDSymlink, claudeSessionStartEvent{HookEventName: "SessionStart", Source: "startup", SessionID: hookSIDSymlink, TranscriptPath: link, Cwd: "/w/proj"}},
		{"parent dir symlink escape", hookSIDParent, claudeSessionStartEvent{HookEventName: "SessionStart", Source: "startup", SessionID: hookSIDParent, TranscriptPath: parentLinked, Cwd: "/w/proj"}},
		{"non-regular file", hookSIDDir, claudeSessionStartEvent{HookEventName: "SessionStart", Source: "startup", SessionID: hookSIDDir, TranscriptPath: nonRegular, Cwd: "/w/proj"}},
		{"duplicate uuid files", hookSIDOwn, claudeSessionStartEvent{HookEventName: "SessionStart", Source: "startup", SessionID: hookSIDOwn, TranscriptPath: own, Cwd: "/w/proj"}},
		{"retired path", hookSIDOwn, claudeSessionStartEvent{HookEventName: "SessionStart", Source: "startup", SessionID: hookSIDSuccessor, TranscriptPath: retired, Cwd: "/w/proj"}},
		{"claimed path", hookSIDOwn, claudeSessionStartEvent{HookEventName: "SessionStart", Source: "startup", SessionID: hookSIDForeign, TranscriptPath: claimed, Cwd: "/w/proj"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n.Transcript, n.SessionID = "", tc.minted
			err := a.processClaudeHookEvent(n.ID, tc.ev)
			if errors.Is(err, errClaudeHookNotImplemented) {
				t.Fatal("invalid event must be classified, not left unimplemented")
			}
			if n.Transcript != "" {
				t.Fatalf("bound %q", n.Transcript)
			}
			if recs := bindingRecs(t, a, n.ID); len(recs) != 0 {
				t.Fatalf("binding records = %+v", recs)
			}
		})
	}
}

func TestClaudeClearHookBindsOnlyOwnSuccessor(t *testing.T) {
	// AT-BIND-03, AT-BIND-04
	f := &fakeTmux{alive: map[string]bool{"n1": true, "n2": true}, captureAfterEnter: "cleared"}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldA := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("a0", -2*time.Hour))
	succA := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor, claudeUserLine("a1", -time.Minute))
	keepB := writeClaudeProject(t, a.home, "-w-proj", hookSIDForeign, claudeUserLine("b-newer", 0))
	n1 := seedOwnedClaude(t, a, "n1", hookSIDOwn, oldA)
	n2 := seedOwnedClaude(t, a, "n2", hookSIDForeign, keepB)
	n2.SessionID = hookSIDForeign
	writeSeedLog(t, a, n1.ID, "hello-a")
	writeSeedLog(t, a, n2.ID, "hello-b")
	bLogBefore := mustRead(t, a.sessionLogPath(n2.ID))
	bSID, bPath, bGen := n2.SessionID, n2.Transcript, a.claudeGeneration(n2.ID)
	bTailer := a.tailerFor(n2)
	a.mirrors[n2.ID] = &mirror{path: keepB}

	sendClear(t, a, n1.ID)
	ev := claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "clear",
		SessionID: hookSIDSuccessor, TranscriptPath: succA, Cwd: "/w/proj",
	}
	if err := a.processClaudeHookEvent(n1.ID, ev); err != nil {
		t.Fatalf("clear event: %v", err)
	}
	if n1.Transcript != succA || n1.SessionID != hookSIDSuccessor {
		t.Fatalf("n1 bound %q / %q, want successor", n1.Transcript, n1.SessionID)
	}
	if n2.Transcript != bPath || n2.SessionID != bSID || a.claudeGeneration(n2.ID) != bGen {
		t.Fatalf("n2 mutated: %+v gen=%d", n2, a.claudeGeneration(n2.ID))
	}
	if a.tailers[n2.ID] != bTailer {
		t.Fatal("n2 tailer replaced")
	}
	if a.mirrors[n2.ID] == nil || a.mirrors[n2.ID].path != keepB {
		t.Fatal("n2 mirror mutated")
	}
	if !bytes.Equal(mustRead(t, a.sessionLogPath(n2.ID)), bLogBefore) {
		t.Fatal("n2 session log mutated")
	}
	if recs := bindingRecs(t, a, n2.ID); len(recs) != 0 {
		t.Fatalf("n2 gained binding records: %+v", recs)
	}
}

func claudeUserLine(text string, age time.Duration) string {
	ts := time.Now().UTC().Add(age).Format(time.RFC3339Nano)
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":%q}}`, ts, text)
}

func writeSeedLog(t *testing.T, a *app, id, text string) {
	t.Helper()
	w := &sessionlog.Writer{Path: a.sessionLogPath(id)}
	if err := w.Append(sessionlog.NewMeta(id, "claude", "", "", "/w/proj")); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(sessionlog.Event{T: "user", Text: text}); err != nil {
		t.Fatal(err)
	}
}

func sendClear(t *testing.T, a *app, id string) {
	t.Helper()
	a.mu.Lock()
	n := a.byID[id]
	strict := a.claudeSupervisionOf(n) == claudeSupStrict
	a.mu.Unlock()
	if !strict {
		installPreparedClaudeHook(t, a, n)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/nodes/"+id+"/send", strings.NewReader(`{"text":"/clear"}`))
	req.SetPathValue("id", id)
	a.handleSend(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/clear: %d %s", rec.Code, rec.Body.String())
	}
}

func TestClaudeClearWithoutHookStaysDetached(t *testing.T) {
	// AT-BIND-05
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	old := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("old", -2*time.Hour))
	newest := writeClaudeProject(t, a.home, "-w-proj", hookSIDForeign, claudeUserLine("newest", 0))
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, old)
	a.activeSince[n.ID] = time.Now().Add(-30 * time.Second)
	a.retireTranscript(n)
	a.maybeRelinkTranscript(n)
	a.drainClaudeHooks()
	if n.Transcript != "" || n.SessionID != "" {
		t.Fatalf("missing hook after clear bound %q / %q (newest %q)", n.Transcript, n.SessionID, newest)
	}
}

func TestHookOwnedStalePhaseDetaches(t *testing.T) {
	// A hook-owned node whose linked file did not carry the phase must detach
	// and show the pane. A later valid clear hook still binds the successor.
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("old", -2*time.Hour))
	newest := writeClaudeProject(t, a.home, "-w-proj", hookSIDForeign, claudeUserLine("newest", 0))
	succ := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor, claudeUserLine("after", 0))
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, old)
	writeSeedLog(t, a, n.ID, "before")
	a.activeSince[n.ID] = time.Now().Add(-30 * time.Second)
	// The phase is only judgeable because a prompt was pasted and this file
	// never recorded it; a bare pane phase proves nothing (D1/D2).
	a.noteDelivery(n.ID, time.Now().Add(-time.Minute))

	a.maybeRelinkTranscript(n)
	if n.Transcript == newest || n.SessionID == hookSIDForeign {
		t.Fatalf("hook-owned stale phase guessed newest file: %q / %q", n.Transcript, n.SessionID)
	}
	if n.Transcript != "" || n.SessionID != "" {
		t.Fatalf("hook-owned stale phase must detach, got %q / %q", n.Transcript, n.SessionID)
	}

	if err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "clear",
		SessionID: hookSIDSuccessor, TranscriptPath: succ, Cwd: "/w/proj",
	}); err != nil {
		t.Fatalf("successor after detach: %v", err)
	}
	if n.Transcript != succ || n.SessionID != hookSIDSuccessor {
		t.Fatalf("hook after detach bound %q / %q, want successor", n.Transcript, n.SessionID)
	}
}

func TestClaudePaneTypedClearMatchesWebClear(t *testing.T) {
	// AT-BIND-06
	setup := func(t *testing.T) (*app, *Node, string) {
		t.Helper()
		f := &fakeTmux{alive: map[string]bool{"n1": true}, captureAfterEnter: "cleared"}
		a := newTestApp(t, f)
		if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		old := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("old", -time.Hour))
		succ := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor, claudeUserLine("new", 0))
		n := seedOwnedClaude(t, a, "n1", hookSIDOwn, old)
		writeSeedLog(t, a, n.ID, "before")
		return a, n, succ
	}
	webA, webN, webSucc := setup(t)
	sendClear(t, webA, webN.ID)
	if err := webA.processClaudeHookEvent(webN.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "clear",
		SessionID: hookSIDSuccessor, TranscriptPath: webSucc, Cwd: "/w/proj",
	}); err != nil {
		t.Fatalf("web hook: %v", err)
	}

	termA, termN, termSucc := setup(t)
	if err := termA.processClaudeHookEvent(termN.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "clear",
		SessionID: hookSIDSuccessor, TranscriptPath: termSucc, Cwd: "/w/proj",
	}); err != nil {
		t.Fatalf("pane hook: %v", err)
	}

	if webN.SessionID != hookSIDSuccessor || webN.Transcript == "" {
		t.Fatalf("web clear bound %q / %q", webN.Transcript, webN.SessionID)
	}
	if termN.SessionID != webN.SessionID || filepath.Base(termN.Transcript) != filepath.Base(webN.Transcript) {
		t.Fatalf("pane clear bound %q / %q, web %q / %q", termN.Transcript, termN.SessionID, webN.Transcript, webN.SessionID)
	}
	if clearSeamCount(t, webA, webN.ID) != 1 || clearSeamCount(t, termA, termN.ID) != 1 {
		t.Fatalf("clear seams web=%d pane=%d, want 1/1", clearSeamCount(t, webA, webN.ID), clearSeamCount(t, termA, termN.ID))
	}
}

func TestClaudeDuplicateHookEventsAreIdempotent(t *testing.T) {
	// AT-BIND-07
	f := &fakeTmux{alive: map[string]bool{"n1": true}, captureAfterEnter: "cleared"}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("old", -time.Hour))
	succ := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor, claudeUserLine("new", 0))
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, old)
	writeSeedLog(t, a, n.ID, "before")
	sendClear(t, a, n.ID)
	ev := claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "clear",
		SessionID: hookSIDSuccessor, TranscriptPath: succ, Cwd: "/w/proj",
	}
	if err := a.processClaudeHookEvent(n.ID, ev); err != nil {
		t.Fatalf("first: %v", err)
	}
	turns := userTurnCount(t, a, n.ID)
	seams := clearSeamCount(t, a, n.ID)
	binds := len(bindingRecs(t, a, n.ID))
	if err := a.processClaudeHookEvent(n.ID, ev); err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	if n.Transcript != succ || n.SessionID != hookSIDSuccessor {
		t.Fatalf("bound %q / %q", n.Transcript, n.SessionID)
	}
	if got := len(bindingRecs(t, a, n.ID)); got != binds || binds != 1 {
		t.Fatalf("binding records = %d then %d, want 1", binds, got)
	}
	if clearSeamCount(t, a, n.ID) != seams || seams != 1 {
		t.Fatalf("clear seams = %d then %d, want 1", seams, clearSeamCount(t, a, n.ID))
	}
	if userTurnCount(t, a, n.ID) != turns {
		t.Fatalf("duplicate mirrored turns: before %d after %d", turns, userTurnCount(t, a, n.ID))
	}
}

func TestClaudeStaleGenerationEventIsRejected(t *testing.T) {
	// AT-BIND-08
	f := &fakeTmux{}
	a := newTestApp(t, f)
	old := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn)
	first := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor)
	second := writeClaudeProject(t, a.home, "-w-proj", hookSIDForeign)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, old)
	captured := a.claudeGeneration(n.ID)
	a.claudeGens[n.ID] = captured + 1
	late := claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "clear",
		SessionID: hookSIDSuccessor, TranscriptPath: first, Cwd: "/w/proj",
	}
	if err := a.processClaudeHookEventAt(n.ID, late, captured); err == nil || errors.Is(err, errClaudeHookNotImplemented) {
		t.Fatalf("stale generation must be a classified reject, err=%v", err)
	}
	if n.Transcript != old || len(bindingRecs(t, a, n.ID)) != 0 {
		t.Fatalf("stale event published %q bindings=%+v", n.Transcript, bindingRecs(t, a, n.ID))
	}
	current := claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "clear",
		SessionID: hookSIDForeign, TranscriptPath: second, Cwd: "/w/proj",
	}
	if err := a.processClaudeHookEvent(n.ID, current); err != nil {
		t.Fatalf("current generation: %v", err)
	}
	if n.Transcript != second {
		t.Fatalf("current generation bound %q, want %q", n.Transcript, second)
	}
}

func TestClaudeBindingAppendFailureLeavesInboxRetryable(t *testing.T) {
	// AT-BIND-09
	f := &fakeTmux{}
	a := newTestApp(t, f)
	own := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		t.Fatal(err)
	}
	if err := a.appendRecord(storeRecord{Type: "claude-hook", ID: n.ID, HookID: a.claudeHookID(n.ID)}); err != nil {
		t.Fatal(err)
	}
	inbox := hookInboxReady(t, a, a.claudeHookID(n.ID), "startup", hookSessionJSON("startup", hookSIDOwn, own))
	breakStore(t, a)
	a.drainClaudeHooks()
	if n.Transcript != "" {
		t.Fatalf("memory published on append failure: %q", n.Transcript)
	}
	if _, err := os.Stat(inbox); err != nil {
		t.Fatalf("inbox event must remain retryable: %v", err)
	}
	fixStore(t, a)
	if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		t.Fatal(err)
	}
	if err := a.appendRecord(storeRecord{Type: "claude-hook", ID: n.ID, HookID: a.claudeHookID(n.ID)}); err != nil {
		t.Fatal(err)
	}
	a2 := reloadApp(t, a, f)
	n2 := a2.byID[n.ID]
	if n2 == nil {
		t.Fatal("node missing after restart")
	}
	a2.drainClaudeHooks()
	if n2.Transcript != own {
		t.Fatalf("restart did not converge on %q, got %q", own, n2.Transcript)
	}
}

func TestClaudeCompactAndResumeSources(t *testing.T) {
	// AT-BIND-10, AT-BIND-11
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("keep", 0))
	other := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, cur)
	writeSeedLog(t, a, n.ID, "history")
	gen := a.claudeGeneration(n.ID)

	t.Run("AT-BIND-10 unchanged compact", func(t *testing.T) {
		err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
			HookEventName: "SessionStart", Source: "compact",
			SessionID: hookSIDOwn, TranscriptPath: cur, Cwd: "/w/proj",
		})
		if errors.Is(err, errClaudeHookNotImplemented) {
			t.Fatal("compact with unchanged identity must be handled")
		}
		if err != nil {
			t.Fatalf("compact: %v", err)
		}
		if n.Transcript != cur || n.SessionID != hookSIDOwn || a.claudeGeneration(n.ID) != gen {
			t.Fatalf("compact mutated node %+v gen=%d", n, a.claudeGeneration(n.ID))
		}
		if clearSeamCount(t, a, n.ID) != 0 {
			t.Fatal("compact must not create a clear seam")
		}
	})
	t.Run("AT-BIND-10 changed compact fails closed", func(t *testing.T) {
		err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
			HookEventName: "SessionStart", Source: "compact",
			SessionID: hookSIDSuccessor, TranscriptPath: other, Cwd: "/w/proj",
		})
		if err == nil || errors.Is(err, errClaudeHookNotImplemented) {
			t.Fatalf("changed compact must fail closed, err=%v", err)
		}
		if n.Transcript != cur || n.SessionID != hookSIDOwn {
			t.Fatalf("changed compact rebound %q / %q", n.Transcript, n.SessionID)
		}
	})
	t.Run("AT-BIND-11 resume same identity", func(t *testing.T) {
		err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
			HookEventName: "SessionStart", Source: "resume",
			SessionID: hookSIDOwn, TranscriptPath: cur, Cwd: "/w/proj",
		})
		if err != nil {
			t.Fatalf("resume same identity: %v", err)
		}
		if n.Transcript != cur || n.SessionID != hookSIDOwn {
			t.Fatalf("resume mutated %q / %q", n.Transcript, n.SessionID)
		}
		if a.attn[n.ID] == "inspect" {
			t.Fatal("resume must not raise inspect")
		}
		if userTurnCount(t, a, n.ID) == 0 {
			t.Fatal("resume must keep prior history")
		}
	})
	t.Run("AT-BIND-11 resume changed identity rebinds", func(t *testing.T) {
		err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
			HookEventName: "SessionStart", Source: "resume",
			SessionID: hookSIDSuccessor, TranscriptPath: other, Cwd: "/w/proj",
		})
		if err != nil {
			t.Fatalf("changed resume must rebind, err=%v", err)
		}
		if n.Transcript != other || n.SessionID != hookSIDSuccessor {
			t.Fatalf("resume did not rebind: %q / %q", n.Transcript, n.SessionID)
		}
		if a.attn[n.ID] == "inspect" {
			t.Fatal("resume must not raise inspect")
		}
		if a.claudeLaunchError(n.ID) != "" {
			t.Fatalf("successful rebind recorded launch error %q", a.claudeLaunchError(n.ID))
		}
		if a.claudeGeneration(n.ID) != gen {
			t.Fatalf("resume must keep generation %d, got %d", gen, a.claudeGeneration(n.ID))
		}
		if a.isDeadTranscript(n.ID, cur, hookSIDOwn) != true {
			t.Fatal("resume must tombstone the previous transcript")
		}
	})
}

func TestResumeEventIsParkedNotReplayed(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("keep", 0))
	other := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, cur)
	hookID := a.claudeHookID(n.ID)
	inbox := hookInboxReady(t, a, hookID, "resume", hookSessionJSON("resume", hookSIDSuccessor, other))

	a.drainClaudeHooks()
	if n.Transcript != other || n.SessionID != hookSIDSuccessor {
		t.Fatalf("changed resume drain did not rebind: %q / %q", n.Transcript, n.SessionID)
	}
	if a.attn[n.ID] == "inspect" {
		t.Fatal("resume must not raise inspect")
	}
	if _, err := os.Stat(inbox); !os.IsNotExist(err) {
		t.Fatal("resume event must leave the inbox after one apply")
	}
}

func TestAdoptedClaudeWithoutUUIDStaysTranscriptless(t *testing.T) {
	// AT-BIND-12
	f := &fakeTmux{alive: map[string]bool{"orphan": true}}
	a := newTestApp(t, f)
	newest := writeClaudeProject(t, a.home, "-w-proj", hookSIDForeign, claudeUserLine("newest", 0))
	rec := adopt(a, `{"session":"orphan","agent":"claude","dir":"/w/proj"}`)
	if rec.Code != 200 {
		t.Fatalf("adopt: %d %s", rec.Code, rec.Body.String())
	}
	var n Node
	if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
		t.Fatal(err)
	}
	if n.Transcript != "" || n.SessionID != "" {
		t.Fatalf("adopted without UUID bound %q / %q (dir had %q)", n.Transcript, n.SessionID, newest)
	}
}

func TestLegacyStoreReplayAndNoNewestFileRelink(t *testing.T) {
	// AT-BIND-14
	f := &fakeTmux{alive: map[string]bool{"legacy": true}}
	a := newTestApp(t, f)
	old := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("old", -2*time.Hour))
	n := &Node{
		ID: "legacy", Title: "legacy", Agent: "claude", Dir: "/w/proj",
		SessionID: hookSIDOwn, Transcript: old,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		t.Fatal(err)
	}
	if err := a.appendRecord(storeRecord{Type: "transcript", ID: n.ID, Path: old}); err != nil {
		t.Fatal(err)
	}
	a2 := reloadApp(t, a, f)
	got := a2.byID["legacy"]
	if got == nil || got.Transcript != old || got.SessionID != hookSIDOwn {
		t.Fatalf("legacy replay = %+v", got)
	}
	if a2.claudeHookID("legacy") != "" {
		t.Fatal("legacy replay must not mint a hook id")
	}

	newest := writeClaudeProject(t, a.home, "-w-proj", hookSIDForeign, claudeUserLine("newest", 0))
	a2.activeSince["legacy"] = time.Now().Add(-30 * time.Second)
	a2.maybeRelinkTranscript(got)
	if got.Transcript == newest || got.SessionID == hookSIDForeign {
		t.Fatalf("legacy node entered newest-file relink: %q / %q", got.Transcript, got.SessionID)
	}
	// The reloaded process has delivered nothing, so it has no evidence the
	// link is stale and must leave it alone (D1: this is what retired every
	// idle Claude node on every restart).
	if got.Transcript != old {
		t.Fatalf("reload retired a legacy link it never delivered to: %q", got.Transcript)
	}

	// Once a prompt is pasted and this file does not record it, it detaches —
	// still without guessing the newest file.
	a2.noteDelivery("legacy", time.Now().Add(-time.Minute))
	a2.maybeRelinkTranscript(got)
	if got.Transcript == newest || got.SessionID == hookSIDForeign {
		t.Fatalf("detach guessed newest file: %q / %q", got.Transcript, got.SessionID)
	}
	if got.Transcript != "" {
		t.Fatalf("legacy stale transcript must detach, got %q", got.Transcript)
	}
}

func TestClaudeDrainRejectsHostileInboxNames(t *testing.T) {
	// D6 / §8: event filenames and hook IDs are path components; drain must
	// not follow them out of the private hook root. A valid sibling event
	// still binds.
	f := &fakeTmux{}
	a := newTestApp(t, f)
	own := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID := a.claudeHookID(n.ID)
	hookInboxReady(t, a, hookID, "startup", hookSessionJSON("startup", hookSIDOwn, own))

	inbox := filepath.Join(a.claudeHooksDir(), hookID, "inbox")
	planted := filepath.Join(inbox, "..", "..", "..", "escaped.json")
	plantedBody := hookSessionJSON("startup", hookSIDOwn, own)
	if err := os.WriteFile(planted, plantedBody, 0o600); err != nil {
		t.Fatal(err)
	}
	plantedClean := filepath.Clean(planted)
	outsideHook := filepath.Join(filepath.Dir(a.claudeHooksDir()), "escaped-hook")

	a.drainClaudeHooks()
	if n.Transcript != own {
		t.Fatalf("valid inbox event did not bind, transcript=%q", n.Transcript)
	}
	if got, err := os.ReadFile(plantedClean); err != nil || !bytes.Equal(got, plantedBody) {
		t.Fatalf("drain must not consume a path that escapes the inbox: %v", err)
	}
	if recs := bindingRecs(t, a, n.ID); len(recs) != 1 {
		t.Fatalf("want one binding from the ready inbox name, got %+v", recs)
	}

	n.Transcript = ""
	a.claudeHooks[n.ID] = filepath.Join("..", "escaped-hook")
	a.drainClaudeHooks()
	if _, err := os.Stat(outsideHook); err == nil {
		t.Fatal("hostile hook id created a directory outside claude-hooks")
	}
	if n.Transcript != "" {
		t.Fatalf("hostile hook id bound %q", n.Transcript)
	}
}

func TestClaudeBindingSurvivesLaterNodeRecordOnReplay(t *testing.T) {
	// D7: a later title/description Node record must not overwrite a newer
	// authoritative hook binding during restart.
	f := &fakeTmux{}
	a := newTestApp(t, f)
	own := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn)
	n := &Node{
		ID: "n1", Title: "First", Agent: "claude", Dir: "/w/proj",
		Prompt: "first", Description: "first",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		t.Fatal(err)
	}
	if err := a.appendRecord(storeRecord{Type: "claude-hook", ID: n.ID, HookID: "hook-n1"}); err != nil {
		t.Fatal(err)
	}
	if err := a.appendRecord(storeRecord{
		Type: "claude-binding", ID: n.ID, HookID: "hook-n1",
		Generation: 2, SessionID: hookSIDOwn, Path: own, Cause: "startup",
	}); err != nil {
		t.Fatal(err)
	}
	renamed := *n
	renamed.Title = "Renamed"
	renamed.Description = "why"
	if err := a.appendRecord(storeRecord{Type: "node", Node: &renamed}); err != nil {
		t.Fatal(err)
	}

	a2 := reloadApp(t, a, f)
	got := a2.byID[n.ID]
	if got == nil {
		t.Fatal("node missing after replay")
	}
	if got.Title != "Renamed" || got.Description != "why" {
		t.Fatalf("title update lost: %+v", got)
	}
	if got.Transcript != own || got.SessionID != hookSIDOwn {
		t.Fatalf("later Node record overwrote hook binding: transcript=%q session=%q", got.Transcript, got.SessionID)
	}
	if a2.claudeGeneration(n.ID) != 2 {
		t.Fatalf("generation = %d, want 2", a2.claudeGeneration(n.ID))
	}
	if a2.claudeHookID(n.ID) != "hook-n1" {
		t.Fatalf("hook id = %q, want hook-n1", a2.claudeHookID(n.ID))
	}
}

func TestOrphanClaudeHookRecordDoesNotResurrectNode(t *testing.T) {
	// D5: a claude-hook record whose node is gone must not mint a phantom node.
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := a.appendRecord(storeRecord{Type: "claude-hook", ID: "ghost", HookID: "hook-ghost"}); err != nil {
		t.Fatal(err)
	}
	a2 := reloadApp(t, a, f)
	if _, ok := a2.byID["ghost"]; ok {
		t.Fatal("orphan claude-hook resurrected a phantom node")
	}
	if a2.claudeHookID("ghost") != "" {
		t.Fatal("orphan claude-hook must not install a hook registration")
	}
}

func TestUnsafeHookIDIsNotRestoredOnReplay(t *testing.T) {
	// §8: hook IDs are validated path components.
	f := &fakeTmux{}
	a := newTestApp(t, f)
	n := &Node{
		ID: "n1", Title: "n1", Agent: "claude", Dir: "/w/proj",
		Prompt: "p", CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		t.Fatal(err)
	}
	if err := a.appendRecord(storeRecord{Type: "claude-hook", ID: n.ID, HookID: filepath.Join("..", hookSIDUnsafe)}); err != nil {
		t.Fatal(err)
	}
	a2 := reloadApp(t, a, f)
	if got := a2.claudeHookID(n.ID); got != "" {
		t.Fatalf("unsafe hook id restored as %q", got)
	}
	a2.drainClaudeHooks()
	escaped := filepath.Join(filepath.Dir(a2.claudeHooksDir()), hookSIDUnsafe)
	if _, err := os.Stat(escaped); err == nil {
		t.Fatal("unsafe hook id escaped the private hook root")
	}
}

func TestHandleNewNodeDoesNotExposeHookCapability(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	rec := newNode(a, `{"title":"Priv","prompt":"p","agent":"claude","dir":`+strconv.Quote(a.home)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "hook_id") || strings.Contains(rec.Body.String(), "claude-hook") {
		t.Fatalf("create leaked hook capability: %s", rec.Body.String())
	}
}

// claudeFreshClearLines reproduces the preamble Claude Code writes into a
// /clear successor's transcript before the next human turn: a mode record, the
// remote-control bridge record, a file-history snapshot, and the isMeta
// local-command caveat. ParseLine recognizes none of them, so the file has no
// content time at all for as long as the human stays away.
func claudeFreshClearLines(sid string) []string {
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	return []string{
		`{"type":"mode","mode":"normal","sessionId":"` + sid + `"}`,
		`{"type":"bridge-session","sessionId":"` + sid + `","bridgeSessionId":"cse_test","lastSequenceNum":0}`,
		`{"type":"file-history-snapshot","messageId":"m1","snapshot":{"messageId":"m1","trackedFileBackups":{},"timestamp":"` + stamp + `"},"isSnapshotUpdate":false}`,
		`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"<local-command-caveat>caveat</local-command-caveat>"},"isMeta":true,"uuid":"u1","timestamp":"` + stamp + `"}`,
	}
}

// A transcript with no recognized turns yet is absence of evidence, not proof
// of staleness. Observed live against claude 2.1.224: the first active→quiet
// transition after a /clear retired and tombstoned the freshly bound successor,
// leaving the node permanently in peek.
func TestContentlessTranscriptIsNotRetiredAsStale(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeFreshClearLines(hookSIDOwn)...)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, cur)
	writeSeedLog(t, a, n.ID, "before")
	a.activeSince[n.ID] = time.Now().Add(-30 * time.Second)

	a.maybeRelinkTranscript(n)

	if n.Transcript != cur || n.SessionID != hookSIDOwn {
		t.Fatalf("contentless transcript was retired: %q / %q", n.Transcript, n.SessionID)
	}
	if a.deadTranscripts[n.ID][cur] {
		t.Fatal("contentless transcript was tombstoned; a later bind can never recover it")
	}
	for _, rec := range keyRecords(t, a.storePath) {
		if rec.Type == "transcript-retired" && rec.ID == n.ID {
			t.Fatal("contentless transcript wrote a transcript-retired record")
		}
	}
}

// A link established after the phase began cannot be stale for that phase, even
// if the file's newest content predates it.
func TestRecentlyBoundTranscriptIsNotRetiredAsStale(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("old", -2*time.Hour))
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, cur)
	writeSeedLog(t, a, n.ID, "before")
	a.activeSince[n.ID] = time.Now().Add(-30 * time.Second)
	a.claudeBoundAt[n.ID] = time.Now()

	a.maybeRelinkTranscript(n)

	if n.Transcript != cur || n.SessionID != hookSIDOwn {
		t.Fatalf("link bound inside this phase was retired: %q / %q", n.Transcript, n.SessionID)
	}
}

// The whole /clear rollover, in the order the poller runs it: the successor is
// bound by the hook, then the next active→quiet transition must leave it alone.
func TestClearHookSuccessorSurvivesNextPollTransition(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("before clear", -time.Minute))
	succ := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor, claudeFreshClearLines(hookSIDSuccessor)...)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, old)
	writeSeedLog(t, a, n.ID, "before")

	a.retireTranscript(n) // what the web /clear and the pane-typed /clear both reach
	if err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "clear",
		SessionID: hookSIDSuccessor, TranscriptPath: succ, Cwd: "/w/proj",
	}); err != nil {
		t.Fatalf("clear hook: %v", err)
	}
	if n.Transcript != succ {
		t.Fatalf("clear hook did not bind successor: %q", n.Transcript)
	}

	a.activeSince[n.ID] = time.Now().Add(-30 * time.Second)
	a.maybeRelinkTranscript(n)

	if n.Transcript != succ || n.SessionID != hookSIDSuccessor {
		t.Fatalf("poll transition after /clear dropped the successor: %q / %q", n.Transcript, n.SessionID)
	}
	if a.deadTranscripts[n.ID][succ] {
		t.Fatal("successor tombstoned; the node can never rebind it")
	}
}

// D1/D2. A pane phase is not evidence that the agent worked: the poller's
// first capture after a restart manufactures one (prevCap is empty, so any
// pane looks changed), and so does late TUI chrome redrawing seconds after a
// turn finished. Both were observed retiring healthy transcripts live. The
// only mechanical proof a link is stale is an *unanswered delivery* — scimux
// pasted a prompt and the transcript never recorded anything since.
func TestRelinkRequiresAnUnansweredDelivery(t *testing.T) {
	// setup returns an app with a hook-owned node whose transcript's newest
	// content is contentAge old, and a completed active→quiet phase.
	setup := func(t *testing.T, contentAge time.Duration) (*app, *Node, string) {
		t.Helper()
		f := &fakeTmux{alive: map[string]bool{"n1": true}}
		a := newTestApp(t, f)
		if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn,
			claudeUserLine("turn", -contentAge))
		n := seedOwnedClaude(t, a, "n1", hookSIDOwn, cur)
		writeSeedLog(t, a, n.ID, "before")
		a.activeSince[n.ID] = time.Now().Add(-30 * time.Second)
		return a, n, cur
	}

	// D1: nothing was ever delivered through scimux — the phase is the
	// poller's own first-capture artifact after a restart. Every idle Claude
	// node was retired and tombstoned this way on every restart.
	t.Run("no delivery recorded survives", func(t *testing.T) {
		a, n, cur := setup(t, 2*time.Hour)
		a.maybeRelinkTranscript(n)
		if n.Transcript != cur {
			t.Fatalf("restart phase retired an idle node: %q", n.Transcript)
		}
		if a.deadTranscripts[n.ID][cur] {
			t.Fatal("idle node's transcript tombstoned; it can never rebind")
		}
	})

	// D2: the delivery was answered — content exists after it — and only then
	// did the pane redraw (remote-control chrome, spinner teardown).
	t.Run("delivery answered survives a later redraw", func(t *testing.T) {
		a, n, cur := setup(t, 30*time.Second)
		a.noteDelivery(n.ID, time.Now().Add(-60*time.Second))
		a.maybeRelinkTranscript(n)
		if n.Transcript != cur {
			t.Fatalf("late redraw retired an answered transcript: %q", n.Transcript)
		}
	})

	// The real stale case: a prompt was pasted and this file never carried it.
	t.Run("unanswered delivery retires", func(t *testing.T) {
		a, n, cur := setup(t, 2*time.Hour)
		a.noteDelivery(n.ID, time.Now().Add(-60*time.Second))
		a.maybeRelinkTranscript(n)
		if n.Transcript != "" || n.SessionID != "" {
			t.Fatalf("unanswered delivery must detach, got %q / %q", n.Transcript, n.SessionID)
		}
		if !a.deadTranscripts[n.ID][cur] {
			t.Fatal("retired path not tombstoned")
		}
	})

	// A transcript write lags the paste; judging inside that window would
	// retire a link that is about to be answered.
	t.Run("delivery inside the grace window survives", func(t *testing.T) {
		a, n, cur := setup(t, 2*time.Hour)
		a.noteDelivery(n.ID, time.Now())
		a.maybeRelinkTranscript(n)
		if n.Transcript != cur {
			t.Fatalf("retired inside the delivery grace window: %q", n.Transcript)
		}
	})

	// A link established after the prompt was pasted cannot have missed it.
	t.Run("link bound after the delivery survives", func(t *testing.T) {
		a, n, cur := setup(t, 2*time.Hour)
		a.noteDelivery(n.ID, time.Now().Add(-60*time.Second))
		a.claudeBoundAt[n.ID] = time.Now()
		a.maybeRelinkTranscript(n)
		if n.Transcript != cur {
			t.Fatalf("link bound after the delivery was retired: %q", n.Transcript)
		}
	})
}

// D1 end to end through the poller: a fresh process adopting a live pane must
// survive its own first capture. prevCap is empty at startup, so tick one sees
// a "change" and opens an active phase that no existing transcript can have
// carried; tick two closes it as quiet and runs the backstop.
func TestFirstPollAfterRestartKeepsIdleTranscripts(t *testing.T) {
	proj := t.TempDir()
	n := &Node{ID: "n1", Agent: "claude", Dir: "/w/proj", SessionID: "old-session"}
	a := newPollApp(t, n, pollRunner([]string{n.ID}, "a live pane", false, nil))
	cur := filepath.Join(proj, "old-session.jsonl")
	appendLines(t, cur, claudeUserLine("hours ago", -2*time.Hour))
	n.Transcript = cur

	a.poll() // first capture: prevCap empty, so the pane looks changed
	if a.live[n.ID] != "active" {
		t.Fatalf("first tick live = %q, want active (precondition)", a.live[n.ID])
	}
	a.lastChg[n.ID] = time.Now().Add(-2 * paneQuietAfter) // let the phase end
	a.poll()
	if a.live[n.ID] != "quiet" {
		t.Fatalf("second tick live = %q, want quiet (precondition)", a.live[n.ID])
	}

	if n.Transcript != cur {
		t.Fatalf("restart retired an idle node's transcript: %q", n.Transcript)
	}
	if a.deadTranscripts[n.ID][cur] {
		t.Fatal("restart tombstoned the transcript; the node is stranded in peek")
	}
}
