package app

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// permBundle builds a bare hook bundle directory with the permission
// rendezvous layout, without going through prepareClaudeHookBundle.
func permBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, sub := range []string{"inbox", "processed", "perm", "perm/req", "perm/ans", "perm/processed"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func permRequestJSON(overrides map[string]any) []byte {
	doc := map[string]any{
		"hook_event_name": "PermissionRequest",
		"session_id":      hookSIDOwn,
		"transcript_path": "/tmp/" + hookSIDOwn + ".jsonl",
		"cwd":             "/tmp",
		"prompt_id":       "prompt-1",
		"permission_mode": "default",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": "ls"},
	}
	for k, v := range overrides {
		if v == nil {
			delete(doc, k)
			continue
		}
		doc[k] = v
	}
	b, _ := json.Marshal(doc)
	return b
}

func writeLeaseFile(t *testing.T, dir, lease string, exp time.Time) {
	t.Helper()
	b, err := json.Marshal(claudePermLease{Lease: lease, Expires: exp.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "perm", "lease"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// readOneRequest waits for exactly one request file to appear and returns it.
func readOneRequest(t *testing.T, dir string) claudePermRequest {
	t.Helper()
	reqDir := filepath.Join(dir, "perm", "req")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ents, _ := os.ReadDir(reqDir)
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(reqDir, e.Name()))
			if err != nil {
				continue
			}
			var req claudePermRequest
			if json.Unmarshal(b, &req) != nil {
				continue
			}
			if req.ID == "" {
				continue
			}
			if strings.TrimSuffix(e.Name(), ".json") != req.ID {
				t.Fatalf("request file %q does not match minted id %q", e.Name(), req.ID)
			}
			return req
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no request file appeared")
	return claudePermRequest{}
}

func answerRequest(t *testing.T, dir string, ans claudePermAnswer) {
	t.Helper()
	b, err := json.Marshal(ans)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "perm", "ans", ans.ID+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudePermissionHookIsSilentWithoutLease(t *testing.T) {
	// AT-CP-01: the toggle-off path is the one every user takes today. No
	// lease marker means no stdout, no request file, no error, and no wait.
	dir := permBundle(t)
	var stdout, stderr bytes.Buffer
	start := time.Now()
	if err := runClaudePermissionHook(dir, bytes.NewReader(permRequestJSON(nil)), &stdout, &stderr, time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("no-lease path must not error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("no-lease path waited %v; it must not block", elapsed)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q, want both empty", stdout.Bytes(), stderr.Bytes())
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "perm", "req"))
	if len(ents) != 0 {
		t.Fatalf("request written without a lease: %v", ents)
	}
}

func TestClaudePermissionHookExpiredLeaseIsSilent(t *testing.T) {
	// AT-CP-02: a stale marker file can only ever cost the hook a read.
	dir := permBundle(t)
	writeLeaseFile(t, dir, "lease-1", time.Now().Add(-time.Second))
	var stdout bytes.Buffer
	if err := runClaudePermissionHook(dir, bytes.NewReader(permRequestJSON(nil)), &stdout, io.Discard, time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("expired lease must not error: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.Bytes())
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "perm", "req")); len(ents) != 0 {
		t.Fatalf("request written for an expired lease: %v", ents)
	}
}

func TestClaudePermissionHookRendezvousAllow(t *testing.T) {
	// AT-CP-03: armed lease → request carrying the payload and the echoed
	// lease id → allow answer → byte-exact allow-once decision on stdout.
	dir := permBundle(t)
	writeLeaseFile(t, dir, "lease-1", time.Now().Add(time.Minute))

	var stdout, stderr bytes.Buffer
	var wg sync.WaitGroup
	var runErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		runErr = runClaudePermissionHook(dir, bytes.NewReader(permRequestJSON(map[string]any{
			"agent_id":               "sub-1",
			"agent_type":             "explorer",
			"permission_suggestions": []any{map[string]any{"type": "addRules"}},
		})), &stdout, &stderr, 3*time.Second, 5*time.Millisecond)
	}()

	req := readOneRequest(t, dir)
	if req.Lease != "lease-1" {
		t.Fatalf("request lease = %q, want the marker's lease id", req.Lease)
	}
	if req.Event.ToolName != "Bash" || req.Event.SessionID != hookSIDOwn {
		t.Fatalf("request lost payload fields: %+v", req.Event)
	}
	if req.Event.PromptID != "prompt-1" || req.Event.PermissionMode != "default" {
		t.Fatalf("request lost turn fields: %+v", req.Event)
	}
	if req.Event.AgentID != "sub-1" || req.Event.AgentType != "explorer" {
		t.Fatalf("request lost subagent identity: %+v", req.Event)
	}
	if !req.Event.HasSuggestions {
		t.Fatal("request must record that a persistent-grant suggestion was offered")
	}
	answerRequest(t, dir, claudePermAnswer{Lease: req.Lease, ID: req.ID, Decision: "allow"})
	wg.Wait()

	if runErr != nil {
		t.Fatalf("rendezvous: %v", runErr)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.Bytes())
	}
	if got := stdout.String(); got != claudePermAllowJSON {
		t.Fatalf("stdout = %q, want %q", got, claudePermAllowJSON)
	}
	// The allow-once guarantee is an omission, so assert the omission.
	if strings.Contains(stdout.String(), "updatedPermissions") || strings.Contains(stdout.String(), "updatedInput") {
		t.Fatalf("allow answer must carry no persistent grant and no input rewrite: %s", stdout.String())
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "perm", "req")); len(ents) != 0 {
		t.Fatalf("hook left its request file behind: %v", ents)
	}
}

func TestClaudePermissionHookAllowJSONIsAllowOnce(t *testing.T) {
	// AT-CP-04: pin the exact wire shape Claude 2.1.224 parses
	// (hookSpecificOutput.decision as an object union) and the absence of the
	// two members that would turn an allow-once into an allow-always.
	var doc struct {
		HookSpecificOutput struct {
			HookEventName string                 `json:"hookEventName"`
			Decision      map[string]interface{} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(claudePermAllowJSON), &doc); err != nil {
		t.Fatalf("allow JSON: %v", err)
	}
	if doc.HookSpecificOutput.HookEventName != "PermissionRequest" {
		t.Fatalf("hookEventName = %q", doc.HookSpecificOutput.HookEventName)
	}
	if len(doc.HookSpecificOutput.Decision) != 1 || doc.HookSpecificOutput.Decision["behavior"] != "allow" {
		t.Fatalf("decision = %v, want exactly {behavior:allow}", doc.HookSpecificOutput.Decision)
	}
}

func TestClaudePermissionHookRefusesUnusableAnswers(t *testing.T) {
	// AT-CP-05: only a well-formed answer that echoes this request's lease
	// and nonce may speak. Everything else, including silence, escalates to
	// the normal dialog by printing nothing.
	cases := []struct {
		name string
		mut  func(req claudePermRequest) (claudePermAnswer, bool)
	}{
		{"lease mismatch", func(r claudePermRequest) (claudePermAnswer, bool) {
			return claudePermAnswer{Lease: "other-lease", ID: r.ID, Decision: "allow"}, true
		}},
		{"nonce mismatch", func(r claudePermRequest) (claudePermAnswer, bool) {
			return claudePermAnswer{Lease: r.Lease, ID: r.ID, Decision: "allow"}, false
		}},
		{"unknown decision", func(r claudePermRequest) (claudePermAnswer, bool) {
			return claudePermAnswer{Lease: r.Lease, ID: r.ID, Decision: "deny"}, true
		}},
		{"empty decision", func(r claudePermRequest) (claudePermAnswer, bool) {
			return claudePermAnswer{Lease: r.Lease, ID: r.ID}, true
		}},
		{"no answer at all", func(r claudePermRequest) (claudePermAnswer, bool) {
			return claudePermAnswer{}, false
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := permBundle(t)
			writeLeaseFile(t, dir, "lease-1", time.Now().Add(time.Minute))
			var stdout bytes.Buffer
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = runClaudePermissionHook(dir, bytes.NewReader(permRequestJSON(nil)), &stdout, io.Discard, 250*time.Millisecond, 5*time.Millisecond)
			}()
			req := readOneRequest(t, dir)
			ans, atOwnID := tc.mut(req)
			if ans.Decision != "" || ans.ID != "" || ans.Lease != "" {
				if !atOwnID {
					ans.ID = "some-other-nonce"
				}
				answerRequest(t, dir, ans)
			}
			wg.Wait()
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty (escalate to the dialog)", stdout.Bytes())
			}
		})
	}
}

func TestClaudePermissionHookRejectsBadInput(t *testing.T) {
	// AT-CP-06: the same fail-closed fences the SessionStart helper applies.
	// A rejected invocation never writes stdout and never writes a request.
	valid := permBundle(t)
	writeLeaseFile(t, valid, "lease-1", time.Now().Add(time.Minute))
	noPerm := t.TempDir() // bundle predating this feature: no perm/ directory
	if err := os.MkdirAll(filepath.Join(noPerm, "inbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	var padded map[string]any
	if err := json.Unmarshal(permRequestJSON(nil), &padded); err != nil {
		t.Fatal(err)
	}
	padded["pad"] = strings.Repeat("x", claudeHookStdinLimit)
	oversize, err := json.Marshal(padded)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		dir  string
		body []byte
	}{
		{"malformed", valid, []byte("{")},
		{"oversized", valid, oversize},
		{"pretooluse payload", valid, permRequestJSON(map[string]any{"hook_event_name": "PreToolUse"})},
		{"missing tool name", valid, permRequestJSON(map[string]any{"tool_name": nil})},
		{"missing session id", valid, permRequestJSON(map[string]any{"session_id": nil})},
		{"relative dir", "relative/dir", permRequestJSON(nil)},
		{"unclean dir", valid + "/../" + filepath.Base(valid), permRequestJSON(nil)},
		{"bundle without perm", noPerm, permRequestJSON(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := runClaudePermissionHook(tc.dir, bytes.NewReader(tc.body), &stdout, &stderr, 200*time.Millisecond, 5*time.Millisecond)
			if err == nil {
				t.Fatal("invalid input must be rejected")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.Bytes())
			}
			if ents, _ := os.ReadDir(filepath.Join(tc.dir, "perm", "req")); len(ents) != 0 {
				t.Fatalf("request written on the reject path: %v", ents)
			}
		})
	}
}

func TestClaudePermissionHookMainAlwaysExitsZero(t *testing.T) {
	// AT-CP-07: a non-zero exit from a permission hook is interpreted by
	// Claude as hook failure/blocking. scimux escalates by staying silent,
	// never by an exit code.
	if code := runClaudePermissionHookMain([]string{"--dir", "relative"}); code != 0 {
		t.Fatalf("exit code = %d, want 0 even on the reject path", code)
	}
}

func TestClaudeHookSettingsRegistersPermissionRequestOnly(t *testing.T) {
	// AT-CP-08: PermissionRequest and the turn-end Stop family ride the same
	// private settings file. PreToolUse (every call) and SubagentStop (not
	// the main turn) are never registered.
	raw, err := claudeHookSettingsJSON("/tmp/scimux dir/scimux", "/tmp/scimux hooks/hook-id")
	if err != nil {
		t.Fatalf("settings JSON: %v", err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("settings JSON: %v\n%s", err, raw)
	}
	allowed := map[string]bool{
		"SessionStart": true, "PermissionRequest": true,
		"Notification": true, "Stop": true, "StopFailure": true,
		"PreCompact": true, "PostCompact": true,
		"Elicitation": true, "ElicitationResult": true,
	}
	for name := range doc.Hooks {
		if !allowed[name] {
			t.Fatalf("unexpected hook event %q registered: %s", name, raw)
		}
	}
	for _, name := range []string{"SessionStart", "PermissionRequest", "Notification", "Stop", "StopFailure"} {
		if len(doc.Hooks[name]) != 1 || len(doc.Hooks[name][0].Hooks) != 1 {
			t.Fatalf("want exactly one %s hook: %s", name, raw)
		}
	}
	if cmd := doc.Hooks["PermissionRequest"][0].Hooks[0].Command; !strings.Contains(cmd, claudePermissionHookCmd) {
		t.Fatalf("PermissionRequest command = %q, want the permission helper", cmd)
	}
	if cmd := doc.Hooks["SessionStart"][0].Hooks[0].Command; !strings.Contains(cmd, claudeSessionHookCmd) {
		t.Fatalf("SessionStart command = %q, want the session helper", cmd)
	}
	if cmd := doc.Hooks["Stop"][0].Hooks[0].Command; !strings.Contains(cmd, claudeStopHookCmd) {
		t.Fatalf("Stop command = %q, want the stop helper", cmd)
	}
	if cmd := doc.Hooks["StopFailure"][0].Hooks[0].Command; cmd != doc.Hooks["Stop"][0].Hooks[0].Command {
		t.Fatalf("StopFailure command = %q, want the same helper as Stop", cmd)
	}
	if strings.Contains(string(raw), "PreToolUse") {
		t.Fatalf("PreToolUse must never be registered: %s", raw)
	}
	if strings.Contains(string(raw), "SubagentStop") {
		t.Fatalf("SubagentStop must never be registered: %s", raw)
	}
}

func TestClaudeHookBundleCarriesPermissionCapability(t *testing.T) {
	// AT-CP-09: the bundle advertises its layout on disk. A fresh app does not
	// activate that capability until SessionStart proves Claude loaded it (pinned
	// at the binding seam); restart replay may then recover the proven layout.
	f := &fakeTmux{}
	a := newTestApp(t, f)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	bundle := filepath.Join(a.claudeHooksDir(), hookID)
	for _, sub := range []string{"perm", "perm/req", "perm/ans", "perm/processed"} {
		st, err := os.Stat(filepath.Join(bundle, sub))
		if err != nil {
			t.Fatalf("%s: %v", sub, err)
		}
		if !st.IsDir() || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v, want dir 0700", sub, st.Mode())
		}
	}
	capPath := filepath.Join(bundle, "capabilities.json")
	st, err := os.Stat(capPath)
	if err != nil {
		t.Fatalf("capabilities.json: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("capabilities.json mode = %o, want 0600", st.Mode().Perm())
	}
	if !bundleSupportsPermission(bundle) {
		t.Fatal("freshly prepared bundle must advertise the permission capability")
	}
	if bundleSupportsPermission(t.TempDir()) {
		t.Fatal("a bundle without capabilities.json must not advertise permission support")
	}
}

func basePermCtx(now time.Time) claudePermContext {
	return claudePermContext{
		Armed:     true,
		LeaseID:   "lease-1",
		SessionID: hookSIDOwn,
		Now:       now,
	}
}

func basePermReq(now time.Time) claudePermRequest {
	return claudePermRequest{
		Lease: "lease-1",
		ID:    "nonce-1",
		At:    now.UTC().Format(time.RFC3339Nano),
		Event: claudePermissionEvent{
			HookEventName:  "PermissionRequest",
			SessionID:      hookSIDOwn,
			PromptID:       "prompt-1",
			PermissionMode: "default",
			ToolName:       "Bash",
		},
	}
}

func TestEligibleClaudePermissionMatrix(t *testing.T) {
	// AT-CP-10: every clause flipped one at a time. Eligibility must be
	// proven; when it cannot be, the request stays manual and the human
	// answers the dialog that Claude is about to draw.
	now := time.Now()
	cases := []struct {
		name string
		ctx  func(c *claudePermContext)
		req  func(r *claudePermRequest)
		want bool
	}{
		{"baseline", nil, nil, true},
		{"accept edits mode", nil, func(r *claudePermRequest) { r.Event.PermissionMode = "acceptEdits" }, true},
		{"subagent call", nil, func(r *claudePermRequest) {
			r.Event.AgentID, r.Event.AgentType = "sub-1", "explorer"
		}, true},
		{"persistent grant offered", nil, func(r *claudePermRequest) { r.Event.HasSuggestions = true }, true},

		{"lease off", func(c *claudePermContext) { c.Armed = false }, nil, false},
		{"lease rotated", func(c *claudePermContext) { c.LeaseID = "lease-2" }, nil, false},
		{"no lease bound", func(c *claudePermContext) { c.LeaseID = "" }, nil, false},
		{"request without lease", nil, func(r *claudePermRequest) { r.Lease = "" }, false},
		{"request without nonce", nil, func(r *claudePermRequest) { r.ID = "" }, false},

		{"foreign session", nil, func(r *claudePermRequest) { r.Event.SessionID = "00000000-0000-4000-8000-000000000999" }, false},
		{"node unbound", func(c *claudePermContext) { c.SessionID = "" }, nil, false},
		{"pretooluse payload", nil, func(r *claudePermRequest) { r.Event.HookEventName = "PreToolUse" }, false},

		{"plan mode", nil, func(r *claudePermRequest) { r.Event.PermissionMode = "plan" }, false},
		{"bypass mode", nil, func(r *claudePermRequest) { r.Event.PermissionMode = "bypassPermissions" }, false},
		{"unknown mode", nil, func(r *claudePermRequest) { r.Event.PermissionMode = "someNewMode" }, false},
		{"missing mode", nil, func(r *claudePermRequest) { r.Event.PermissionMode = "" }, false},

		{"exit plan mode tool", nil, func(r *claudePermRequest) { r.Event.ToolName = "ExitPlanMode" }, false},
		{"ask user question tool", nil, func(r *claudePermRequest) { r.Event.ToolName = "AskUserQuestion" }, false},
		{"missing tool", nil, func(r *claudePermRequest) { r.Event.ToolName = "" }, false},

		{"request already escalated", nil, func(r *claudePermRequest) {
			r.At = time.Now().Add(-10 * permHookDeadline).UTC().Format(time.RFC3339Nano)
		}, false},
		{"request without timestamp", nil, func(r *claudePermRequest) { r.At = "" }, false},
		{"request timestamp unparseable", nil, func(r *claudePermRequest) { r.At = "yesterday" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, req := basePermCtx(now), basePermReq(now)
			if tc.ctx != nil {
				tc.ctx(&ctx)
			}
			if tc.req != nil {
				tc.req(&req)
			}
			if got := eligibleClaudePermission(ctx, req); got != tc.want {
				t.Fatalf("eligible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEligibleClaudePermissionHonoursPromptFence(t *testing.T) {
	// AT-CP-11: prompt_id is the protocol's own turn identity. It fences only
	// when the app knows the current turn's id; unknown means unfenced, never
	// falsely eligible on the other clauses.
	now := time.Now()
	ctx := basePermCtx(now)
	ctx.TurnPromptID = "prompt-1"
	if !eligibleClaudePermission(ctx, basePermReq(now)) {
		t.Fatal("matching prompt_id must stay eligible")
	}
	other := basePermReq(now)
	other.Event.PromptID = "prompt-2"
	if eligibleClaudePermission(ctx, other) {
		t.Fatal("a request from a different turn must not be auto-approved")
	}
	blank := basePermReq(now)
	blank.Event.PromptID = ""
	if eligibleClaudePermission(ctx, blank) {
		t.Fatal("a request with no turn identity must not pass a bound fence")
	}
	// Fence not bound: the lease is the only turn boundary, as designed.
	unfenced := basePermCtx(now)
	if !eligibleClaudePermission(unfenced, other) {
		t.Fatal("with no bound prompt fence the lease alone must still allow")
	}
}

// seedPermClaude seeds an owned Claude node whose hook bundle really exists on
// disk with the permission rendezvous, as a launched node's would.
func seedPermClaude(t *testing.T, a *app, id, sid string) (*Node, string) {
	t.Helper()
	n := seedOwnedClaude(t, a, id, sid, "")
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatalf("prepare bundle: %v", err)
	}
	a.mu.Lock()
	a.claudeHooks[n.ID] = hookID
	a.mu.Unlock()
	a.markClaudeHookAck(n.ID)
	a.noteClaudeHookCapabilitiesForNode(n.ID)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: a.sessionLogPath(id)}
	if err := w.Append(sessionlog.NewMeta(id, "claude", "m", "", a.home)); err != nil {
		t.Fatal(err)
	}
	return n, filepath.Join(a.claudeHooksDir(), hookID)
}

// dropRequest writes a request file exactly as the blocked hook helper would.
func dropRequest(t *testing.T, bundle string, req claudePermRequest) {
	t.Helper()
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "req", req.ID+".json"), req); err != nil {
		t.Fatal(err)
	}
}

func answerFileFor(bundle, id string) (claudePermAnswer, bool) {
	return readClaudePermAnswer(filepath.Join(bundle, "perm", "ans", id+".json"))
}

func armedRequest(a *app, n *Node, id string) claudePermRequest {
	a.mu.Lock()
	lease := ""
	if st := a.autoApprove[n.ID]; st != nil {
		lease = st.LeaseID
	}
	sid := n.SessionID
	a.mu.Unlock()
	return claudePermRequest{
		Lease: lease,
		ID:    id,
		At:    time.Now().UTC().Format(time.RFC3339Nano),
		Event: claudePermissionEvent{
			HookEventName:  "PermissionRequest",
			SessionID:      sid,
			PromptID:       "prompt-1",
			PermissionMode: "default",
			ToolName:       "Bash",
			ToolInput:      json.RawMessage(`{"command":"ls -la"}`),
		},
	}
}

func TestClaudeAutoApproveSupportedNeedsBundleCapability(t *testing.T) {
	// AT-CP-12: support is a property of the pane's own bundle. A pane
	// adopted (no bundle) or launched before this feature stays unsupported
	// and says so, rather than offering a toggle that can never arm.
	a := newTestApp(t, &fakeTmux{})
	capable, _ := seedPermClaude(t, a, "cap", hookSIDOwn)
	if !a.autoApproveSupportedFor(capable) {
		t.Fatal("a launched Claude node with a permission-capable bundle must be supported")
	}
	legacy := seedOwnedClaude(t, a, "legacy", "00000000-0000-4000-8000-0000000000aa", "")
	if a.autoApproveSupportedFor(legacy) {
		t.Fatal("a bundle without the permission capability must not be supported")
	}
	adopted := seedOwnedClaude(t, a, "adopted", "00000000-0000-4000-8000-0000000000bb", "")
	a.mu.Lock()
	delete(a.claudeHooks, adopted.ID)
	a.mu.Unlock()
	if a.autoApproveSupportedFor(adopted) {
		t.Fatal("an adopted pane has no hook bundle and must not be supported")
	}
	if a.autoApproveViewOf(capable).Supported != true {
		t.Fatal("the polled view must advertise support for a capable Claude node")
	}
	if a.autoApproveViewOf(adopted).Supported != false {
		t.Fatal("the polled view must not advertise support for an adopted pane")
	}
}

func TestClaudeLeaseMarkerTracksArmedPhaseOnly(t *testing.T) {
	// AT-CP-13: the marker is the hook's fast path, and it exists only while
	// the lease is armed. Enabling while idle primes without writing it, so a
	// tool call from a turn that is already running cannot be caught.
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	marker := filepath.Join(bundle, "perm", "lease")

	a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0)
	if v := a.autoApproveViewOf(n); v.Phase != string(autoPhasePrimed) {
		t.Fatalf("phase = %q, want primed", v.Phase)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a primed lease must not write the marker the hook reads")
	}

	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	if v := a.autoApproveViewOf(n); v.Phase != string(autoPhaseArmed) {
		t.Fatalf("phase = %q, want armed", v.Phase)
	}
	lease, ok := readClaudePermLease(filepath.Join(bundle, "perm"), time.Now())
	if !ok {
		t.Fatal("an armed lease must publish the marker")
	}
	a.mu.Lock()
	want := a.autoApprove[n.ID].LeaseID
	a.mu.Unlock()
	if lease != want {
		t.Fatalf("marker lease = %q, want %q", lease, want)
	}

	a.disableAutoApprove(n.ID)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("disable must remove the marker before clearing state")
	}
}

func TestResolveClaudePermissionAllowsEligibleRequest(t *testing.T) {
	// AT-CP-14: the fast lane's happy path — audit first, then answer, then
	// count. The audit describes the request that was actually granted.
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	req := armedRequest(a, n, "nonce-1")
	dropRequest(t, bundle, req)

	if got := a.resolveClaudePermissionsFor(n); got != 1 {
		t.Fatalf("answers written = %d, want 1", got)
	}
	ans, ok := answerFileFor(bundle, req.ID)
	if !ok {
		t.Fatal("no answer written for an eligible request")
	}
	if !ans.allows(req) {
		t.Fatalf("answer = %+v, want an allow bound to this lease and nonce", ans)
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "req", req.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("an answered request must be claimed out of req/")
	}

	var dec *sessionlog.DecisionEvent
	for _, ev := range logEvents(t, a, n.ID) {
		if ev.T == "decision" && ev.Decision != nil {
			dec = ev.Decision
		}
	}
	if dec == nil {
		t.Fatal("missing decision audit")
	}
	if dec.Source != "auto" || dec.RequestID != req.ID || dec.Agent != "claude" {
		t.Fatalf("decision = %+v", dec)
	}
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	a.mu.Unlock()
	if dec.LeaseID != st.LeaseID {
		t.Fatalf("decision lease = %q, want %q", dec.LeaseID, st.LeaseID)
	}
	if dec.Selected.Kind != "allow" {
		t.Fatalf("selected = %+v, want kind allow", dec.Selected)
	}
	if len(dec.Options) != 0 {
		t.Fatalf("options = %+v; Claude offers scimux no menu, so the audit must not invent one", dec.Options)
	}
	if dec.Title == "" || !strings.Contains(dec.Title, "Bash") {
		t.Fatalf("title = %q, want the tool that was granted", dec.Title)
	}
	if st.Count != 1 {
		t.Fatalf("count = %d, want 1", st.Count)
	}

	// Re-running the lane must not answer or audit the same request twice.
	if got := a.resolveClaudePermissionsFor(n); got != 0 {
		t.Fatalf("second pass answered %d requests, want 0", got)
	}
}

func TestResolveClaudePermissionLeavesIneligibleManual(t *testing.T) {
	// AT-CP-15: everything the fence declines falls through to the dialog —
	// no answer, no audit, and no lease consumption.
	cases := []struct {
		name string
		mut  func(r *claudePermRequest)
	}{
		{"plan mode", func(r *claudePermRequest) { r.Event.PermissionMode = "plan" }},
		{"question tool", func(r *claudePermRequest) { r.Event.ToolName = "AskUserQuestion" }},
		{"foreign session", func(r *claudePermRequest) { r.Event.SessionID = "00000000-0000-4000-8000-0000000000cc" }},
		{"stale lease", func(r *claudePermRequest) { r.Lease = "lease-from-a-previous-turn" }},
		{"already escalated", func(r *claudePermRequest) {
			r.At = time.Now().Add(-10 * permHookDeadline).UTC().Format(time.RFC3339Nano)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t, &fakeTmux{})
			n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
			a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
			req := armedRequest(a, n, "nonce-1")
			tc.mut(&req)
			dropRequest(t, bundle, req)

			if got := a.resolveClaudePermissionsFor(n); got != 0 {
				t.Fatalf("answers written = %d, want 0", got)
			}
			if _, ok := answerFileFor(bundle, req.ID); ok {
				t.Fatal("an ineligible request must not be answered")
			}
			for _, ev := range logEvents(t, a, n.ID) {
				if ev.T == "decision" {
					t.Fatal("an ineligible request must not be audited as a decision")
				}
			}
			a.mu.Lock()
			count := a.autoApprove[n.ID].Count
			a.mu.Unlock()
			if count != 0 {
				t.Fatalf("count = %d, want 0", count)
			}
		})
	}
}

func TestResolveClaudePermissionQuarantinesUnparseableRequests(t *testing.T) {
	// AT-CP-16: undocumented internals get defensive parsing — junk in the
	// rendezvous is moved aside, never answered and never fatal.
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	junk := filepath.Join(bundle, "perm", "req", "broken.json")
	if err := os.WriteFile(junk, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := a.resolveClaudePermissionsFor(n); got != 0 {
		t.Fatalf("answers written = %d, want 0", got)
	}
	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Fatal("unparseable request must be moved out of req/")
	}
	if ents, _ := os.ReadDir(filepath.Join(bundle, "perm", "ans")); len(ents) != 0 {
		t.Fatalf("answers written for junk: %v", ents)
	}
}

func TestResolveClaudePermissionFailsClosedWhenAuditFails(t *testing.T) {
	// AT-CP-17: audit before delivery. If the decision cannot be recorded,
	// nothing is granted — the human answers the dialog and the failure is
	// visible in the chat error, exactly as for the structured transports.
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	req := armedRequest(a, n, "nonce-1")
	dropRequest(t, bundle, req)

	logPath := a.sessionLogPath(n.ID)
	if err := os.Chmod(logPath, 0o400); err != nil {
		t.Skipf("cannot make the session log read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(logPath, 0o600) })
	if err := os.Chmod(a.sessionsDir, 0o500); err != nil {
		t.Skipf("cannot make the session store read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(a.sessionsDir, 0o700) })

	if got := a.resolveClaudePermissionsFor(n); got != 0 {
		t.Fatalf("answers written = %d, want 0 when the audit cannot be appended", got)
	}
	if _, ok := answerFileFor(bundle, req.ID); ok {
		t.Fatal("an unaudited approval must never be delivered")
	}
	a.mu.Lock()
	st := a.autoApprove[n.ID]
	errText, count := st.Error, st.Count
	a.mu.Unlock()
	if !strings.Contains(errText, "audit failed") {
		t.Fatalf("lease error = %q, want an honest audit failure", errText)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}

func TestResolveClaudePermissionIsInertWhenOff(t *testing.T) {
	// AT-CP-18: with the toggle off the lane must not touch the bundle at
	// all — a request left by an earlier armed turn is never answered late.
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	req := armedRequest(a, n, "nonce-1")
	req.Lease = "some-old-lease"
	dropRequest(t, bundle, req)
	if got := a.resolveClaudePermissionsFor(n); got != 0 {
		t.Fatalf("answers written = %d, want 0 with no lease", got)
	}
	if _, ok := answerFileFor(bundle, req.ID); ok {
		t.Fatal("no lease must mean no answer")
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "req", req.ID+".json")); err != nil {
		t.Fatal("with the toggle off the lane must leave the rendezvous untouched")
	}
}

func TestClaudePermToolKindMapsToSharedVocabulary(t *testing.T) {
	// AT-CP-26: tool_kind is the audit's shared *rendering* vocabulary
	// (PERM_CODE_KINDS in the UI), not the raw protocol tool name — the same
	// contract codex's mapApprovalToolKind honours. Claude's own tool name
	// stays visible in the title, so nothing is lost from the corpus.
	cases := map[string]string{
		"Bash":         "execute",
		"BashOutput":   "execute",
		"KillShell":    "execute",
		"Edit":         "edit",
		"Write":        "edit",
		"NotebookEdit": "edit",
		"Read":         "read",
		"Glob":         "search",
		"Grep":         "search",
		"WebFetch":     "",
		"Task":         "",
		"":             "",
		"Frobnicate":   "", // unknown stays unknown, never guessed
	}
	for tool, want := range cases {
		if got := claudePermToolKind(tool); got != want {
			t.Errorf("claudePermToolKind(%q) = %q, want %q", tool, got, want)
		}
	}
}

func TestResolveClaudePermissionAuditsRenderableToolKind(t *testing.T) {
	// AT-CP-27: the delivered audit record carries the mapped kind and a title
	// that still names the tool, so the decision row reads as a command block.
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	req := armedRequest(a, n, "nonce-1")
	dropRequest(t, bundle, req)

	if got := a.resolveClaudePermissionsFor(n); got != 1 {
		t.Fatalf("delivered = %d, want 1", got)
	}
	var dec *sessionlog.DecisionEvent
	for _, ev := range sessionlog.ReadEvents(a.sessionLogPath(n.ID)) {
		if ev.T == "decision" && ev.Decision != nil {
			dec = ev.Decision
		}
	}
	if dec == nil {
		t.Fatal("no decision record was audited")
	}
	if dec.ToolKind != "execute" {
		t.Fatalf("tool_kind = %q, want the renderable kind for Bash", dec.ToolKind)
	}
	if !strings.Contains(dec.Title, "Bash") || !strings.Contains(dec.Title, "ls -la") {
		t.Fatalf("title = %q, want it to name both the tool and its subject", dec.Title)
	}
}

// permBundleWithAsked is permBundle plus the escalation-notice directory: the
// layout every bundle prepared from this version on carries.
func permBundleWithAsked(t *testing.T) string {
	t.Helper()
	root := permBundle(t)
	if err := os.MkdirAll(filepath.Join(root, "perm", "asked"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLeaseLatchesFirstPromptIDAndRefusesAnother(t *testing.T) {
	// AT-CP-20 (gap 2): the lease must not outlive its turn even when the
	// pane-liveness disarm misses the edge. prompt_id is the protocol's own turn
	// identity — proven in P0 to be shared by a parent and its subagent within a
	// turn and to change between turns — so the first request a lease answers
	// latches it, and a request from any other turn is declined. Without the
	// latch, TurnPromptID stays empty forever and the fence never binds: exactly
	// the deviation §4's status block called out.
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)

	first := armedRequest(a, n, "nonce-1")
	dropRequest(t, bundle, first)
	if got := a.resolveClaudePermissionsFor(n); got != 1 {
		t.Fatalf("first request answers = %d, want 1", got)
	}
	a.mu.Lock()
	latched := ""
	if st := a.autoApprove[n.ID]; st != nil {
		latched = st.TurnPromptID
	}
	a.mu.Unlock()
	if latched != first.Event.PromptID {
		t.Fatalf("latched turn prompt = %q, want %q", latched, first.Event.PromptID)
	}

	// Same turn (a parallel call, or a subagent under the same prompt_id).
	same := armedRequest(a, n, "nonce-2")
	dropRequest(t, bundle, same)
	if got := a.resolveClaudePermissionsFor(n); got != 1 {
		t.Fatalf("same-turn request answers = %d, want 1", got)
	}

	// A different turn under the same armed lease: declined, and left for the
	// human exactly as any other policy decline.
	other := armedRequest(a, n, "nonce-3")
	other.Event.PromptID = "prompt-2"
	dropRequest(t, bundle, other)
	if got := a.resolveClaudePermissionsFor(n); got != 0 {
		t.Fatalf("next-turn request answers = %d, want 0", got)
	}
	if _, ok := answerFileFor(bundle, other.ID); ok {
		t.Fatal("a request from another turn must never be answered")
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "processed", "manual", other.ID+".json")); err != nil {
		t.Fatalf("declined request not recorded as manual: %v", err)
	}
}

func TestReArmingClearsTheLatchedTurn(t *testing.T) {
	// AT-CP-21 (gap 2): the latch lives on the lease, not the node, so its
	// lifetime needs no cleanup code that a future path could forget. A human
	// who toggles auto-approve on again is arming for the turn in front of them,
	// so a fence latched by the previous lease must not make the new one inert.
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	first := armedRequest(a, n, "nonce-1")
	dropRequest(t, bundle, first)
	if got := a.resolveClaudePermissionsFor(n); got != 1 {
		t.Fatalf("first request answers = %d, want 1", got)
	}

	a.setAutoApproveEnabled(n.ID, false, "active", "", 0)
	a.mu.Lock()
	stillLeased := a.autoApprove[n.ID] != nil
	a.mu.Unlock()
	if stillLeased {
		t.Fatal("disarm must drop the lease state, and the latch with it")
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	next := armedRequest(a, n, "nonce-9")
	next.Event.PromptID = "prompt-2"
	dropRequest(t, bundle, next)
	if got := a.resolveClaudePermissionsFor(n); got != 1 {
		t.Fatalf("new lease answers = %d, want 1 — a fresh lease latches the new turn", got)
	}
}

func TestAnswerWindowIsStrictlyShorterThanTheHookDeadline(t *testing.T) {
	// AT-CP-22 (gap 3): the two windows must not be the same number. The helper
	// stops waiting at permHookDeadline; if the app were still willing to answer
	// at that same age, a request answered at the last moment would be audited
	// as an approval the blocked process never read — the call escalates to the
	// dialog anyway and the decision row lies about reality. So the app's window
	// closes first, by more than one full lane pass plus one helper poll, which
	// is the longest an answer can take to be noticed.
	if permAnswerWindow >= permHookDeadline {
		t.Fatalf("answer window %v must be strictly shorter than the hook deadline %v",
			permAnswerWindow, permHookDeadline)
	}
	if slack := permHookDeadline - permAnswerWindow; slack < claudePermLaneEvery+permHookPollEvery {
		t.Fatalf("slack %v is too small for one lane pass (%v) plus one helper poll (%v)",
			slack, claudePermLaneEvery, permHookPollEvery)
	}
	// A human waiting on a dialog is the failure this feature exists to avoid,
	// so the deadline is not allowed to shrink back to a value where a busy
	// host loses approvals to a race it cannot see.
	if permHookDeadline < 5*time.Second {
		t.Fatalf("hook deadline %v is too tight for a loaded host", permHookDeadline)
	}

	// And the window is what the policy actually uses.
	now := time.Now()
	late := basePermReq(now)
	late.At = now.Add(-permAnswerWindow - time.Millisecond).UTC().Format(time.RFC3339Nano)
	if eligibleClaudePermission(basePermCtx(now), late) {
		t.Fatal("a request older than the answer window must not be answered")
	}
	fresh := basePermReq(now)
	fresh.At = now.Add(-permAnswerWindow + 50*time.Millisecond).UTC().Format(time.RFC3339Nano)
	if !eligibleClaudePermission(basePermCtx(now), fresh) {
		t.Fatal("a request inside the answer window must stay eligible")
	}
}

func TestMissedDeadlineIsVisibleAndPolicyDeclinesAreSilent(t *testing.T) {
	// AT-CP-23 (gap 3): a degradation must not be silent. When the lease was
	// armed and the request was eligible on every clause *except* the clock, the
	// human is about to see a dialog they told scimux to answer, and the only
	// honest report is to say so — an error event in the log and the concise
	// error the chat header already renders. A policy decline is different: the
	// fence working as designed is not a fault and must stay quiet, or every
	// plan-mode call would cry wolf.
	expired := func(r *claudePermRequest) {
		r.At = time.Now().Add(-10 * permHookDeadline).UTC().Format(time.RFC3339Nano)
	}
	policy := func(r *claudePermRequest) { r.Event.PermissionMode = "plan" }

	for _, tc := range []struct {
		name     string
		mut      func(r *claudePermRequest)
		wantLoud bool
	}{
		{"missed deadline", expired, true},
		{"policy decline", policy, true},
		// A stale clock cannot be told apart from a policy decline, and an
		// unreadable stamp is a parse failure, not a slow host.
		{"unparseable stamp", func(r *claudePermRequest) { r.At = "yesterday" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t, &fakeTmux{})
			n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
			a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
			req := armedRequest(a, n, "nonce-1")
			tc.mut(&req)
			dropRequest(t, bundle, req)

			if got := a.resolveClaudePermissionsFor(n); got != 0 {
				t.Fatalf("answers written = %d, want 0", got)
			}
			if _, ok := answerFileFor(bundle, req.ID); ok {
				t.Fatal("a declined request must never be answered")
			}
			loud := false
			for _, ev := range logEvents(t, a, n.ID) {
				if ev.T == "decision" {
					t.Fatal("a declined request must not be audited as a decision")
				}
				if ev.T == "error" && strings.Contains(ev.Error, req.ID) {
					loud = true
				}
			}
			a.mu.Lock()
			st := a.autoApprove[n.ID]
			errText := ""
			if st != nil {
				errText = st.Error
			}
			a.mu.Unlock()
			if tc.wantLoud {
				if !loud {
					t.Fatal("a missed hook deadline must be reported in the session log")
				}
				if errText == "" {
					t.Fatal("a missed hook deadline must surface in the chat header error")
				}
			} else {
				if loud {
					t.Fatal("a policy decline must not log an error")
				}
				if errText != "" {
					t.Fatalf("a policy decline must leave no error, got %q", errText)
				}
			}
		})
	}
}
