package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
)

func interactionDirs() []string {
	return []string{
		"perm/interact",
		"perm/interact/req",
		"perm/interact/ans",
		"perm/interact/claim",
		"perm/interact/recorded",
		"perm/interact/ack",
		"perm/interact/closed",
		"perm/interact/expired",
		"perm/interact/interrupted",
		"perm/interact/waiter",
		"perm/interact/cancel",
	}
}

func interactionBundle(t *testing.T) string {
	t.Helper()
	dir := permBundle(t)
	for _, sub := range interactionDirs() {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	exec, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(claudeHookCapabilities{
		Permission: 1, Asked: 1, Stop: 1, Notify: 1, Compact: 1, Elicitation: 1,
		Exec: exec, Interaction: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "capabilities.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func publishInteraction(t *testing.T, bundle string, rec claudeInteractionRequest) {
	t.Helper()
	if rec.Digest == "" {
		model := mustClassify(t, rec.Tool, rec.Original)
		rec.Digest = model.Digest
	}
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "req", rec.ID+".json"), rec); err != nil {
		t.Fatal(err)
	}
	if err := exclWriteTest(filepath.Join(bundle, "perm", "interact", "active.json"), []byte(`{"id":"`+rec.ID+`"}`)); err != nil {
		t.Fatal(err)
	}
}

func exclWriteTest(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

func sampleRequest(bundle string, now time.Time) claudeInteractionRequest {
	return claudeInteractionRequest{
		ID:             "req-letter",
		SessionID:      hookSIDOwn,
		Tool:           "AskUserQuestion",
		PromptID:       "prompt-1",
		PermissionMode: "default",
		BundleID:       filepath.Base(bundle),
		At:             now.UTC().Format(time.RFC3339Nano),
		AnswerUntil:    now.Add(time.Minute).UTC().Format(time.RFC3339Nano),
		HelperUntil:    now.Add(90 * time.Second).UTC().Format(time.RFC3339Nano),
		Original:       append([]byte(nil), letterQuestionRaw()...),
	}
}

func sampleCaller(bundle string) claudeInteractionCaller {
	return claudeInteractionCaller{NodeID: "node-a", SessionID: hookSIDOwn, BundleID: filepath.Base(bundle)}
}

func letterReply() claudeInteractionReply {
	return claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}
}

func recordingAudit(t *testing.T, events *[]sessionlog.Event) func(sessionlog.Event) error {
	t.Helper()
	return func(ev sessionlog.Event) error {
		if ev.T != "decision" || ev.Decision == nil {
			t.Errorf("audit event = %+v", ev.Decision)
		} else if ev.Decision.Source == "application" {
			if ev.Decision.Outcome == nil || ev.Decision.Manual != nil {
				t.Errorf("application outcome = %+v", ev.Decision)
			}
		} else if ev.Decision.Source != "manual" || ev.Decision.Manual == nil || ev.Decision.Outcome != nil {
			t.Errorf("manual intent = %+v", ev.Decision)
		}
		*events = append(*events, ev)
		return nil
	}
}

func TestClaudeInteractionHookDeadlineMath(t *testing.T) {
	if interactionWaitBudget(0) != interactionHelperDeadline || interactionWaitBudget(permHookDeadline) != interactionHelperDeadline {
		t.Fatalf("production budget = %s", interactionWaitBudget(permHookDeadline))
	}
	if interactionWaitBudget(40*time.Millisecond) != 40*time.Millisecond {
		t.Fatalf("test budget = %s", interactionWaitBudget(40*time.Millisecond))
	}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := interactionAnswerDeadline(start, interactionHelperDeadline); !got.Equal(start.Add(interactionAnswerWindow)) {
		t.Fatalf("production answer deadline = %s", got)
	}
	short := 30 * time.Millisecond
	got := interactionAnswerDeadline(start, short)
	if !got.After(start) || !got.Before(start.Add(short)) {
		t.Fatalf("short answer deadline = %s, want inside %s", got, short)
	}
	if interactionAnswerDeadline(start, 0) != start || interactionAnswerDeadline(start, time.Nanosecond) != start {
		t.Fatal("zero budget must already be expired")
	}
	if got := interactionSelected("other", ""); got.Name != "other" || got.Kind != "other" {
		t.Fatalf("unknown action label = %+v", got)
	}
}

func TestClaudeInteractionHookGateOffBundle(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "0")
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID, settingsPath, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(a.claudeHooksDir(), hookID)
	caps := mustRead(t, filepath.Join(bundle, "capabilities.json"))
	if bytes.Contains(caps, []byte("interaction")) {
		t.Fatalf("gate-off capabilities changed: %s", caps)
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact")); !os.IsNotExist(err) {
		t.Fatalf("gate-off created interact: %v", err)
	}
	exec := claudeHookCapabilities{}
	if json.Unmarshal(caps, &exec) != nil || exec.Exec == "" {
		t.Fatal("missing exec")
	}
	want, err := claudeHookSettingsJSON(exec.Exec, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, settingsPath), want) {
		t.Fatal("gate-off settings bytes changed")
	}
	if !bundleCompleteCurrent(bundle) {
		t.Fatal("interaction must not be required for a current bundle")
	}
	if bundleSupportsInteraction(bundle) {
		t.Fatal("gate-off bundle advertised interaction")
	}
	naked := t.TempDir()
	raw, err := json.Marshal(claudeHookCapabilities{Interaction: 1, Exec: filepath.Join(naked, "missing-bin")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(naked, "capabilities.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if bundleSupportsInteraction(naked) {
		t.Fatal("unusable exec advertised interaction")
	}
	if strings.Contains(string(want), "PreToolUse") {
		t.Fatal("PreToolUse registered")
	}
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "true")
	hookID, _, err = a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	loose := mustRead(t, filepath.Join(a.claudeHooksDir(), hookID, "capabilities.json"))
	if bytes.Contains(loose, []byte("interaction")) {
		t.Fatalf("non-1 gate minted interaction: %s", loose)
	}
}

func TestClaudeInteractionHookCapableBundle(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID, settingsPath, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(a.claudeHooksDir(), hookID)
	if !bundleSupportsInteraction(bundle) {
		t.Fatal("capable bundle must advertise interaction")
	}
	var caps claudeHookCapabilities
	if json.Unmarshal(mustRead(t, filepath.Join(bundle, "capabilities.json")), &caps) != nil || caps.Interaction != 1 {
		t.Fatal("capabilities missing interaction 1")
	}
	for _, sub := range interactionDirs() {
		st, err := os.Stat(filepath.Join(bundle, sub))
		if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v err=%v", sub, st, err)
		}
	}
	settings := mustRead(t, settingsPath)
	if bytes.Contains(settings, []byte("PreToolUse")) {
		t.Fatalf("PreToolUse registered: %s", settings)
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(settings, &doc) != nil {
		t.Fatalf("settings: %s", settings)
	}
	perm := doc.Hooks["PermissionRequest"]
	if len(perm) != 1 || len(perm[0].Hooks) != 1 || perm[0].Hooks[0].Timeout != interactionHookTimeoutSec {
		t.Fatalf("permission timeout = %+v", perm)
	}
	if doc.Hooks["Stop"][0].Hooks[0].Timeout != 0 {
		t.Fatal("timeout leaked onto Stop")
	}
	if !bundleCompleteCurrent(bundle) {
		t.Fatal("extra interaction dirs must not make the bundle incomplete")
	}

}

func TestClaudeInteractionHookOrdinaryAllowUnchanged(t *testing.T) {
	dir := interactionBundle(t)
	writeLeaseFile(t, dir, "lease-1", time.Now().Add(time.Minute))
	var stdout bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	var runErr error
	go func() {
		defer wg.Done()
		runErr = runClaudePermissionHook(dir, bytes.NewReader(permRequestJSON(nil)), &stdout, io.Discard, time.Second, 5*time.Millisecond)
	}()
	req := readOneRequest(t, dir)
	answerRequest(t, dir, claudePermAnswer{Lease: req.Lease, ID: req.ID, Decision: "allow"})
	wg.Wait()
	if runErr != nil {
		t.Fatal(runErr)
	}
	if stdout.String() != claudePermAllowJSON {
		t.Fatalf("stdout = %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "perm", "interact", "active.json")); !os.IsNotExist(err) {
		t.Fatal("bash approval created an interaction")
	}
	handled, err := serveClaudeInteraction(dir, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "Bash", ToolInput: []byte(`{"command":"ls"}`),
	}, "req-bash", io.Discard, time.Millisecond, time.Millisecond, time.Now, func(time.Duration) { t.Error("bash serve waited") })
	if handled || err != nil {
		t.Fatalf("bash serve handled=%v err=%v", handled, err)
	}
	if eligibleClaudePermission(basePermCtx(time.Now()), func() claudePermRequest {
		r := basePermReq(time.Now())
		r.Event.ToolName = "AskUserQuestion"
		return r
	}()) {
		t.Fatal("question became lease-eligible")
	}
}

func TestClaudeInteractionHookQuestionWithoutCapabilityStaysNative(t *testing.T) {
	dir := permBundle(t)
	var stdout bytes.Buffer
	start := time.Now()
	err := runClaudePermissionHook(dir, bytes.NewReader(permRequestJSON(map[string]any{
		"tool_name":  "AskUserQuestion",
		"tool_input": json.RawMessage(letterQuestionRaw()),
	})), &stdout, io.Discard, 40*time.Millisecond, 5*time.Millisecond)
	if err != nil || stdout.Len() != 0 || time.Since(start) > 300*time.Millisecond {
		t.Fatalf("err=%v stdout=%q elapsed=%s", err, stdout.Bytes(), time.Since(start))
	}
	if _, statErr := os.Stat(filepath.Join(dir, "perm", "interact")); !os.IsNotExist(statErr) {
		t.Fatal("incapable bundle entered the interaction path")
	}
	handled, serr := serveClaudeInteraction(dir, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "req-letter", io.Discard, time.Millisecond, time.Millisecond, time.Now, func(time.Duration) {})
	if handled || serr != nil {
		t.Fatalf("incapable serve handled=%v err=%v", handled, serr)
	}
}

func TestClaudeInteractionHookServesHumanAnswer(t *testing.T) {
	dir := interactionBundle(t)
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "turn.json"), claudeAcceptedTurn{Turn: "turn-live", Gen: 4, Session: hookSIDOwn}); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "generation.json"), map[string]any{"generation": 6, "node_id": "node-a"}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	var runErr error
	start := time.Now()
	go func() {
		defer wg.Done()
		runErr = runClaudePermissionHook(dir, bytes.NewReader(permRequestJSON(map[string]any{
			"tool_name":       "AskUserQuestion",
			"tool_input":      json.RawMessage(letterQuestionRaw()),
			"permission_mode": "plan",
		})), &stdout, io.Discard, permHookDeadline, 5*time.Millisecond)
	}()
	active := filepath.Join(dir, "perm", "interact", "active.json")
	var id string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && id == "" {
		b, err := os.ReadFile(active)
		if err == nil {
			var doc struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(b, &doc) == nil {
				id = doc.ID
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("interaction request was not published")
	}
	var published claudeInteractionRequest
	if json.Unmarshal(mustRead(t, filepath.Join(dir, "perm", "interact", "req", id+".json")), &published) != nil || published.Turn != "turn-live" || published.TurnGen != 4 || published.Generation != 6 || published.NodeID != "node-a" || published.SessionID != hookSIDOwn {
		t.Fatalf("published request = %+v", published)
	}
	model := mustClassify(t, "AskUserQuestion", letterQuestionRaw())
	env, err := buildClaudeInteractionReply(model, letterReply())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "perm", "interact", "ans", id+".json"), env.Stdout, 0o600); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if runErr != nil {
		t.Fatal(runErr)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("human answer waited %s", time.Since(start))
	}
	if stdout.String() != string(env.Stdout) || strings.Contains(stdout.String(), "updatedPermissions") {
		t.Fatalf("stdout = %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "perm", "lease")); !os.IsNotExist(err) {
		t.Fatal("question path required or wrote a lease")
	}
}

func TestClaudeInteractionHookUnsupportedFallsThrough(t *testing.T) {
	dir := interactionBundle(t)
	// Duplicate option labels are not structured-safe.
	raw := []byte(`{"questions":[{"question":"Pick?","header":"H","options":[{"label":"Same","description":"a"},{"label":"Same","description":"b"}],"multiSelect":false}]}`)
	var stdout bytes.Buffer
	start := time.Now()
	err := runClaudePermissionHook(dir, bytes.NewReader(permRequestJSON(map[string]any{
		"tool_name":  "AskUserQuestion",
		"tool_input": json.RawMessage(raw),
	})), &stdout, io.Discard, 40*time.Millisecond, 5*time.Millisecond)
	if err != nil || stdout.Len() != 0 || time.Since(start) > 300*time.Millisecond {
		t.Fatalf("err=%v stdout=%q elapsed=%s", err, stdout.Bytes(), time.Since(start))
	}
	if _, statErr := os.Stat(filepath.Join(dir, "perm", "interact", "active.json")); !os.IsNotExist(statErr) {
		t.Fatal("unsupported shape replaced the native path")
	}
}

func TestClaudeInteractionHookParallelAndNoReplay(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	rec := sampleRequest(dir, now)
	publishInteraction(t, dir, rec)
	var stdout bytes.Buffer
	var sleeps int
	handled, err := serveClaudeInteraction(dir, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion",
		ToolInput: letterQuestionRaw(), PromptID: "prompt-2",
	}, "req-other", &stdout, 30*time.Millisecond, 10*time.Millisecond, func() time.Time { return now }, func(time.Duration) { sleeps++ })
	if err != nil || !handled || stdout.Len() != 0 || sleeps != 0 {
		t.Fatalf("parallel handled=%v err=%v stdout=%q sleeps=%d", handled, err, stdout.Bytes(), sleeps)
	}
	active, _ := os.ReadFile(filepath.Join(dir, "perm", "interact", "active.json"))
	if !bytes.Contains(active, []byte("req-letter")) {
		t.Fatalf("active replaced: %s", active)
	}
	// A second waiter must not emit a prepared answer.
	ans := []byte(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny"}}}`)
	if err := os.WriteFile(filepath.Join(dir, "perm", "interact", "ans", "req-letter.json"), ans, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "perm", "interact", "waiter", "req-letter"), []byte("waiter"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	handled, err = serveClaudeInteraction(dir, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "req-letter", &stdout, time.Second, time.Millisecond, func() time.Time { return now }, func(time.Duration) { sleeps++ })
	if err != nil || !handled || stdout.Len() != 0 {
		t.Fatalf("replay handled=%v err=%v stdout=%q", handled, err, stdout.Bytes())
	}
}

func TestClaudeInteractionHookSubmitAuditAndPhases(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	rec := sampleRequest(dir, now)
	publishInteraction(t, dir, rec)
	ansPath := filepath.Join(dir, "perm", "interact", "ans", rec.ID+".json")
	var events []sessionlog.Event
	audit := func(ev sessionlog.Event) error {
		if _, err := os.Stat(ansPath); err == nil {
			t.Errorf("answer file existed before audit")
		}
		if ev.Decision == nil || ev.Decision.Source != "manual" || ev.Decision.Manual == nil {
			t.Errorf("event %+v", ev.Decision)
		}
		if ev.Decision.Manual.Feedback != "" || ev.Decision.Manual.Selections[0].Kind != "option" {
			t.Errorf("selection %+v", ev.Decision.Manual)
		}
		if ev.Decision.Manual.Delivery != "recorded" || ev.Decision.LeaseID != "" {
			t.Errorf("delivery %+v", ev.Decision)
		}
		events = append(events, ev)
		return nil
	}
	got, err := submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, letterReply(), now, audit)
	if err != nil {
		t.Fatal(err)
	}
	if got.Emitted || got.Status != "accepted" || got.Delivery != "recorded" || got.Phase != "recorded" {
		t.Fatalf("receipt = %+v", got)
	}
	if len(events) != 1 {
		t.Fatalf("audits = %d", len(events))
	}
	env, err := buildClaudeInteractionReply(mustClassify(t, rec.Tool, rec.Original), letterReply())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustRead(t, ansPath), env.Stdout) {
		t.Fatalf("ans = %s", mustRead(t, ansPath))
	}
	if claudeInteractionPhase(dir, rec.ID, now) != "recorded" {
		t.Fatalf("phase = %s", claudeInteractionPhase(dir, rec.ID, now))
	}
	again, err := submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, letterReply(), now, audit)
	if !errors.Is(err, errInteractionConflict) || again.Emitted {
		t.Fatalf("duplicate = %+v err=%v", again, err)
	}
	if len(events) != 1 {
		t.Fatal("duplicate audited again")
	}
	if err := noteClaudeInteractionEmission(dir, rec.ID, claudeInteractionAck{Emitted: true}); err != nil {
		t.Fatal(err)
	}
	if claudeInteractionPhase(dir, rec.ID, now) != "emitted" {
		t.Fatalf("phase after ack = %s", claudeInteractionPhase(dir, rec.ID, now))
	}
	if err := noteClaudeInteractionEmission(dir, rec.ID, claudeInteractionAck{Emitted: false}); !errors.Is(err, errInteractionConflict) {
		t.Fatalf("second ack err = %v", err)
	}

}

func TestClaudeInteractionHookAuditFailureEmitsNothing(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now().UTC()
	rec := sampleRequest(dir, now)
	publishInteraction(t, dir, rec)
	_, err := submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, letterReply(), now, func(sessionlog.Event) error {
		return errors.New("disk full")
	})
	if !errors.Is(err, errInteractionAudit) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "perm", "interact", "ans", rec.ID+".json")); !os.IsNotExist(statErr) {
		t.Fatal("audit failure wrote an answer")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "perm", "interact", "claim", rec.ID)); !os.IsNotExist(statErr) {
		t.Fatal("audit failure left the claim")
	}
	var events []sessionlog.Event
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, letterReply(), now, recordingAudit(t, &events)); err != nil || len(events) != 1 {
		t.Fatalf("retry after audit failure = %v events=%d", err, len(events))
	}
}

func TestClaudeInteractionHookLateAndIdentity(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	rec := sampleRequest(dir, now)
	rec.AnswerUntil = now.Add(time.Second).UTC().Format(time.RFC3339Nano)
	rec.Turn = "turn-n"
	rec.TurnGen = 3
	rec.Generation = 4
	rec.NodeID = "node-a"
	publishInteraction(t, dir, rec)
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "turn.json"), claudeAcceptedTurn{Turn: "turn-n", Gen: 3, Session: hookSIDOwn}); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "generation.json"), map[string]any{"generation": 4, "node_id": "node-a"}); err != nil {
		t.Fatal(err)
	}
	caller := sampleCaller(dir)
	caller.Turn, caller.TurnGen, caller.Generation = "turn-n", 3, 4
	var events []sessionlog.Event
	if _, err := submitClaudeInteraction(dir, caller, rec.ID, letterReply(), now.Add(time.Second), recordingAudit(t, &events)); !errors.Is(err, errInteractionConflict) {
		t.Fatalf("expired err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "perm", "interact", "ans", rec.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("expired submit wrote an answer")
	}
	bad := []claudeInteractionCaller{
		{NodeID: "node-b", SessionID: hookSIDOwn, BundleID: caller.BundleID, Generation: 4, Turn: "turn-n", TurnGen: 3},
		{NodeID: "node-a", SessionID: "other", BundleID: caller.BundleID, Generation: 4, Turn: "turn-n", TurnGen: 3},
		{NodeID: "node-a", SessionID: hookSIDOwn, BundleID: "other-bundle", Generation: 4, Turn: "turn-n", TurnGen: 3},
		{NodeID: "node-a", SessionID: hookSIDOwn, BundleID: caller.BundleID, Generation: 5, Turn: "turn-n", TurnGen: 3},
		{NodeID: "node-a", SessionID: hookSIDOwn, BundleID: caller.BundleID, Generation: 4, Turn: "turn-n1", TurnGen: 4},
	}
	for _, c := range bad {
		if _, err := submitClaudeInteraction(dir, c, rec.ID, letterReply(), now, func(sessionlog.Event) error { return nil }); !errors.Is(err, errInteractionConflict) {
			t.Fatalf("caller %+v err=%v", c, err)
		}
	}
	if _, err := submitClaudeInteraction(dir, caller, "../escape", letterReply(), now, func(sessionlog.Event) error { return nil }); !errors.Is(err, errInteractionBadRequest) {
		t.Fatalf("escape err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "perm", "generation.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := submitClaudeInteraction(dir, caller, rec.ID, letterReply(), now, func(sessionlog.Event) error { return nil }); !errors.Is(err, errInteractionConflict) {
		t.Fatalf("bad generation err = %v", err)
	}
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "generation.json"), map[string]any{"generation": 4, "node_id": "node-a"}); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "turn.json"), claudeAcceptedTurn{Turn: "turn-other", Gen: 9, Session: hookSIDOwn}); err != nil {
		t.Fatal(err)
	}
	if _, err := submitClaudeInteraction(dir, caller, rec.ID, letterReply(), now, func(sessionlog.Event) error { return nil }); !errors.Is(err, errInteractionConflict) {
		t.Fatalf("live successor err = %v", err)
	}
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "turn.json"), claudeAcceptedTurn{Turn: "turn-n", Gen: 3, Session: hookSIDOwn}); err != nil {
		t.Fatal(err)
	}
	if _, err := submitClaudeInteraction(dir, caller, rec.ID, letterReply(), now, recordingAudit(t, &events)); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeInteractionHookDiscussApproveCancelTerminal(t *testing.T) {
	now := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	discuss := func(dir, id string, reply claudeInteractionReply) sessionlog.Event {
		t.Helper()
		var events []sessionlog.Event
		if _, err := submitClaudeInteraction(dir, sampleCaller(dir), id, reply, now, recordingAudit(t, &events)); err != nil {
			t.Fatal(err)
		}
		return events[0]
	}
	dir := interactionBundle(t)
	rec := sampleRequest(dir, now)
	publishInteraction(t, dir, rec)
	ev := discuss(dir, rec.ID, claudeInteractionReply{Action: "discuss", Feedback: "Ask it another way."})
	if ev.Decision.Manual.Feedback != "Ask it another way." || ev.Decision.Manual.AppContext != interactionContextDiscuss {
		t.Fatalf("discuss attribution = %+v", ev.Decision.Manual)
	}
	ans := string(mustRead(t, filepath.Join(dir, "perm", "interact", "ans", rec.ID+".json")))
	if !strings.Contains(ans, `"behavior":"deny"`) || !strings.Contains(ans, `"interrupt":false`) {
		t.Fatalf("discuss ans = %s", ans)
	}

	dir = interactionBundle(t)
	plan := sampleRequest(dir, now)
	plan.ID = "req-plan"
	plan.Tool = "ExitPlanMode"
	plan.PermissionMode = "plan"
	plan.Original = append([]byte(nil), tinyPlanRaw()...)
	publishInteraction(t, dir, plan)
	ev = discuss(dir, plan.ID, claudeInteractionReply{Action: "request_changes", Feedback: "Keep the plan smaller."})
	if ev.Decision.Manual.AppContext != interactionContextChanges || ev.Decision.Manual.Feedback == ev.Decision.Manual.AppContext {
		t.Fatalf("changes = %+v", ev.Decision.Manual)
	}

	dir = interactionBundle(t)
	plan = sampleRequest(dir, now)
	plan.ID = "req-plan"
	plan.Tool = "ExitPlanMode"
	plan.PermissionMode = "plan"
	plan.Original = append([]byte(nil), tinyPlanRaw()...)
	publishInteraction(t, dir, plan)
	ev = discuss(dir, plan.ID, claudeInteractionReply{Action: "approve", Mode: "acceptEdits"})
	if ev.Decision.Manual.Mode != "acceptEdits" || ev.Decision.Manual.ModeLabel != interactionModeEditsLabel || ev.Decision.Manual.EstablishedMode != "" {
		t.Fatalf("approve = %+v", ev.Decision.Manual)
	}
	ans = string(mustRead(t, filepath.Join(dir, "perm", "interact", "ans", plan.ID+".json")))
	if strings.Contains(ans, "addRules") || strings.Contains(ans, "bypassPermissions") || !strings.Contains(ans, `"destination":"session"`) {
		t.Fatalf("approve ans = %s", ans)
	}
	if claudeInteractionPhase(dir, plan.ID, now) != "recorded" {
		t.Fatal("approval receipt invented confirmation")
	}

	dir = interactionBundle(t)
	plan = sampleRequest(dir, now)
	plan.ID = "req-plan"
	plan.Tool = "ExitPlanMode"
	plan.Original = append([]byte(nil), tinyPlanRaw()...)
	publishInteraction(t, dir, plan)
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), plan.ID, claudeInteractionReply{Action: "approve", Mode: "restore"}, now, func(sessionlog.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if claudeInteractionPhase(dir, plan.ID, now) != "recorded" {
		t.Fatal("restored-mode approval invented confirmation")
	}

	dir = interactionBundle(t)
	rec = sampleRequest(dir, now)
	rec.ID = "req-cancel"
	publishInteraction(t, dir, rec)
	ev = discuss(dir, rec.ID, claudeInteractionReply{Action: "cancel"})
	if ev.Decision.Manual.Feedback != "" || ev.Decision.Manual.AppContext != interactionContextCancel {
		t.Fatalf("cancel = %+v", ev.Decision.Manual)
	}
	ans = string(mustRead(t, filepath.Join(dir, "perm", "interact", "ans", rec.ID+".json")))
	if !strings.Contains(ans, `"interrupt":true`) || strings.Contains(ans, "updatedInput") {
		t.Fatalf("cancel ans = %s", ans)
	}

	dir = interactionBundle(t)
	rec = sampleRequest(dir, now)
	rec.ID = "req-term"
	publishInteraction(t, dir, rec)
	var stdout bytes.Buffer
	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	go func() {
		_, _ = serveClaudeInteraction(dir, claudePermissionEvent{
			HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
		}, rec.ID, &stdout, time.Minute, time.Millisecond, func() time.Time { return now }, func(time.Duration) {
			once.Do(func() { close(blocked) })
			<-release
		})
	}()
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("helper did not wait")
	}
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, claudeInteractionReply{Action: "terminal"}, now, func(ev sessionlog.Event) error {
		if _, statErr := os.Stat(filepath.Join(dir, "perm", "interact", "closed", rec.ID)); !os.IsNotExist(statErr) {
			t.Errorf("closed before audit")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "perm", "interact", "closed", rec.ID)); err != nil {
		t.Fatal("terminal did not revoke before release")
	}
	close(release)
	time.Sleep(50 * time.Millisecond)
	if stdout.Len() != 0 {
		t.Fatalf("terminal stdout = %q", stdout.Bytes())
	}
	if claudeInteractionPhase(dir, rec.ID, now) != "handoff" {
		t.Fatalf("phase = %s", claudeInteractionPhase(dir, rec.ID, now))
	}
}

func TestClaudeInteractionHookStdoutAndExpiry(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Date(2026, 7, 8, 9, 10, 11, 0, time.UTC)
	var stdout shortInteractionWriter
	clock := now
	sleeps := 0
	handled, err := serveClaudeInteraction(dir, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion",
		ToolInput: letterQuestionRaw(), PromptID: "prompt-1", PermissionMode: "default",
	}, "req-letter", &stdout, 40*time.Millisecond, 10*time.Millisecond, func() time.Time { return clock }, func(d time.Duration) {
		sleeps++
		if sleeps > 20 {
			t.Fatal("expiry poll spun")
		}
		clock = clock.Add(d)
	})
	if !handled || err != nil || stdout.Len() != 0 {
		t.Fatalf("expiry handled=%v err=%v stdout=%q", handled, err, stdout.buf.Bytes())
	}
	if claudeInteractionPhase(dir, "req-letter", clock) != "expired" {
		t.Fatalf("phase = %s", claudeInteractionPhase(dir, "req-letter", clock))
	}
	later := now.Add(time.Minute)
	var events []sessionlog.Event
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), "req-letter", letterReply(), later, recordingAudit(t, &events)); !errors.Is(err, errInteractionConflict) || len(events) != 0 {
		t.Fatalf("late err=%v events=%d", err, len(events))
	}

	dir = interactionBundle(t)
	env, err := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), letterReply())
	if err != nil {
		t.Fatal(err)
	}
	// Publish through serve, with the answer already waiting, and a short writer.
	rec := sampleRequest(dir, now)
	// Serve mints its own request; plant the answer under the id it will use.
	if err := os.WriteFile(filepath.Join(dir, "perm", "interact", "ans", "req-short.json"), env.Stdout, 0o600); err != nil {
		t.Fatal(err)
	}
	sw := &shortInteractionWriter{limit: 3}
	handled, err = serveClaudeInteraction(dir, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "req-short", sw, time.Second, time.Millisecond, func() time.Time { return now }, func(time.Duration) { t.Error("slept despite a waiting answer") })
	if !handled || !errors.Is(err, io.ErrShortWrite) || sw.buf.Len() >= len(env.Stdout) {
		t.Fatalf("short handled=%v err=%v n=%d", handled, err, sw.buf.Len())
	}
	if claudeInteractionPhase(dir, "req-short", now) != "expired" {
		t.Fatalf("phase = %s", claudeInteractionPhase(dir, "req-short", now))
	}
	_ = rec
}

type shortInteractionWriter struct {
	buf      bytes.Buffer
	limit    int
	nilShort bool
}

func (w *shortInteractionWriter) Write(p []byte) (int, error) {
	if w.nilShort {
		if len(p) == 0 {
			return 0, nil
		}
		_, _ = w.buf.Write(p[:1])
		return 1, nil
	}
	if w.limit <= 0 {
		return 0, io.ErrShortWrite
	}
	n := w.limit
	if n > len(p) {
		n = len(p)
	}
	w.limit = 0
	_, _ = w.buf.Write(p[:n])
	if n < len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func (w *shortInteractionWriter) Len() int { return w.buf.Len() }

func TestClaudeInteractionHookRacesAndIsolation(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now().UTC()
	rec := sampleRequest(dir, now)
	publishInteraction(t, dir, rec)
	var mu sync.Mutex
	var okN, conflictN, audits int
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, letterReply(), now, func(sessionlog.Event) error {
				mu.Lock()
				audits++
				mu.Unlock()
				return nil
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				okN++
			} else if errors.Is(err, errInteractionConflict) {
				conflictN++
			}
		}()
	}
	wg.Wait()
	if okN != 1 || conflictN != 1 || audits != 1 {
		t.Fatalf("ok=%d conflict=%d audits=%d", okN, conflictN, audits)
	}

	other := interactionBundle(t)
	otherRec := sampleRequest(other, now)
	otherRec.ID = "req-other"
	publishInteraction(t, other, otherRec)
	if _, err := os.Stat(filepath.Join(other, "perm", "interact", "ans", otherRec.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("node A answer landed on node B")
	}

	var held sync.Mutex
	held.Lock()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		_, _ = serveClaudeInteraction(dir, claudePermissionEvent{
			HookEventName: "PermissionRequest", SessionID: "other-session", ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
		}, "req-wait", io.Discard, time.Second, time.Millisecond, func() time.Time { return now }, func(time.Duration) {
			held.Lock()
			held.Unlock()
		})
	}()
	// The active request belongs to req-letter, so the extra call must return
	// without waiting. A lock held by the test must not be required for that.
	time.Sleep(20 * time.Millisecond)
	if _, err := os.ReadFile(filepath.Join(dir, "perm", "interact", "active.json")); err != nil {
		t.Fatal(err)
	}
	held.Unlock()
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("parallel serve blocked")
	}
}

func TestClaudeInteractionHookExpiredScratch(t *testing.T) {
	now := time.Now().UTC()
	empty := interactionBundle(t)
	stale := sampleRequest(empty, now.Add(-time.Hour))
	stale.HelperUntil = now.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if err := writeClaudePermFile(filepath.Join(empty, "perm", "interact", "req", stale.ID+".json"), stale); err != nil {
		t.Fatal(err)
	}
	fresh := stale
	fresh.ID = "req-fresh"
	fresh.HelperUntil = now.Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if err := writeClaudePermFile(filepath.Join(empty, "perm", "interact", "req", fresh.ID+".json"), fresh); err != nil {
		t.Fatal(err)
	}
	badTime := fresh
	badTime.ID = "req-bad-time"
	badTime.HelperUntil = "not-a-time"
	if err := writeClaudePermFile(filepath.Join(empty, "perm", "interact", "req", badTime.ID+".json"), badTime); err != nil {
		t.Fatal(err)
	}
	answered := stale
	answered.ID = "req-answered"
	if err := writeClaudePermFile(filepath.Join(empty, "perm", "interact", "req", answered.ID+".json"), answered); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(empty, "perm", "interact", "ans", answered.ID+".json"), []byte(`{"terminal":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(empty, "perm", "interact", "req", "skip.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(empty, "perm", "interact", "req", "req-bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := interactionBundle(t)
	if err := writeClaudePermFile(filepath.Join(other, "perm", "interact", "req", "req-foreign.json"), stale); err != nil {
		t.Fatal(err)
	}
	if err := retireExpiredInteractionPayloads(empty, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(empty, "perm", "interact", "req", stale.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("expired scratch remained")
	}
	if _, err := os.Stat(filepath.Join(empty, "perm", "interact", "req", fresh.ID+".json")); err != nil {
		t.Fatal("fresh scratch was removed")
	}
	if _, err := os.Stat(filepath.Join(empty, "perm", "interact", "req", "req-bad-time.json")); err != nil {
		t.Fatal("unparseable scratch was removed")
	}
	if _, err := os.Stat(filepath.Join(empty, "perm", "interact", "req", "req-answered.json")); !os.IsNotExist(err) {
		t.Fatal("answered request payload remained after the helper deadline")
	}
	if _, err := os.Stat(filepath.Join(empty, "perm", "interact", "ans", "req-answered.json")); !os.IsNotExist(err) {
		t.Fatal("answered response payload remained after the helper deadline")
	}
	if _, err := os.Stat(filepath.Join(empty, "perm", "interact", "expired", "req-answered")); err != nil {
		t.Fatal("answered request lost its replay fence")
	}
	if st, err := os.Stat(filepath.Join(empty, "perm", "interact", "req", "skip.json")); err != nil || !st.IsDir() {
		t.Fatal("non-file scratch was removed")
	}
	if _, err := os.Stat(filepath.Join(empty, "perm", "interact", "req", "req-bad.json")); err != nil {
		t.Fatal("unreadable scratch was removed")
	}
	if _, err := os.Stat(filepath.Join(other, "perm", "interact", "req", "req-foreign.json")); err != nil {
		t.Fatal("other bundle was cleaned")
	}
	if err := os.RemoveAll(filepath.Join(empty, "perm", "interact", "req")); err != nil {
		t.Fatal(err)
	}
	if err := retireExpiredInteractionPayloads(empty, now); !errors.Is(err, errInteractionIO) {
		t.Fatalf("missing scratch dir err = %v", err)
	}

}

func TestClaudeInteractionHookCancelSettlement(t *testing.T) {
	cur := claudeAcceptedTurn{Turn: "turn-n", Gen: 2}
	closing := claudeAcceptedTurn{Turn: "turn-prev", Gen: 1}
	cases := []struct {
		name    string
		emitted bool
		turn    string
		current claudeAcceptedTurn
		closing claudeAcceptedTurn
		wantOK  bool
		wantWhy string
	}{
		{"settled", true, "turn-n", cur, closing, true, "settled"},
		{"not emitted", false, "turn-n", cur, closing, false, "not emitted"},
		{"successor", true, "turn-n", claudeAcceptedTurn{Turn: "turn-n1", Gen: 3}, cur, false, "successor turn"},
		{"closing match", true, "turn-prev", claudeAcceptedTurn{}, closing, true, "settled"},
		{"empty", true, "", claudeAcceptedTurn{}, claudeAcceptedTurn{}, false, "turn mismatch"},
		{"closing mismatch", true, "turn-n", claudeAcceptedTurn{}, claudeAcceptedTurn{Turn: "other", Gen: 1}, false, "turn mismatch"},
	}
	for _, tc := range cases {
		ok, why := claudeInteractionCancelMaySettle(tc.emitted, tc.turn, tc.current, tc.closing)
		if ok != tc.wantOK || why != tc.wantWhy {
			t.Fatalf("%s = %v %q", tc.name, ok, why)
		}
	}

	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, filepath.Join(t.TempDir(), "missing.jsonl"))
	bundle := filepath.Join(a.claudeHooksDir(), "hook-n1")
	if err := os.MkdirAll(filepath.Join(bundle, "perm"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, sub := range interactionDirs() {
		if err := os.MkdirAll(filepath.Join(bundle, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rec := sampleRequest(bundle, now)
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	rec.BundleID = "hook-n1"
	publishInteraction(t, bundle, rec)
	env, err := buildClaudeInteractionReply(mustClassify(t, rec.Tool, rec.Original), claudeInteractionReply{Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json"), env.Stdout, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "ack", rec.ID+".json"), claudeInteractionAck{Emitted: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "ans", "req-decoy.json"), []byte(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","interrupt":false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "ack", "req-decoy.json"), claudeInteractionAck{Emitted: true}); err != nil {
		t.Fatal(err)
	}
	ackDir := filepath.Join(bundle, "perm", "interact", "ack")
	if err := os.Mkdir(filepath.Join(ackDir, "skip"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ackDir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ackDir, "req-bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(filepath.Join(ackDir, "req-quiet.json"), claudeInteractionAck{Emitted: false}); err != nil {
		t.Fatal(err)
	}
	a.settleEmittedClaudeInteractionCancels(n)
	if a.claudeAcceptedTurnOf(n.ID).Turn != "" {
		t.Fatal("cancel did not settle the matching turn")
	}
	if a.claudeClosingTurnOf(n.ID).Turn != turn.Turn {
		t.Fatalf("closing = %+v", a.claudeClosingTurnOf(n.ID))
	}
	if claudeInteractionPhase(bundle, rec.ID, now) != "interrupted" {
		t.Fatalf("phase = %s", claudeInteractionPhase(bundle, rec.ID, now))
	}
	next, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	a.settleEmittedClaudeInteractionCancels(n)
	if a.claudeAcceptedTurnOf(n.ID).Turn != next.Turn {
		t.Fatal("stale cancel closed the successor")
	}
	var dead *app
	if ok, why := dead.settleClaudeInteractionCancel(n, rec.ID); ok || why != "turn mismatch" {
		t.Fatalf("nil app = %v %s", ok, why)
	}
	if ok, why := a.settleClaudeInteractionCancel(nil, rec.ID); ok || why != "turn mismatch" {
		t.Fatalf("nil node = %v %s", ok, why)
	}
	if ok, why := a.settleClaudeInteractionCancel(n, "../req"); ok || why != "turn mismatch" {
		t.Fatalf("bad id = %v %s", ok, why)
	}
	if ids := pendingCancelInteractions(t.TempDir()); len(ids) != 0 {
		t.Fatalf("missing ack directory returned a cancel: %v", ids)
	}
	if ok, why := a.settleClaudeInteractionCancel(n, "req-absent"); ok || why != "turn mismatch" {
		t.Fatalf("missing request = %v %s", ok, why)
	}
	a.mu.Lock()
	delete(a.claudeHooks, n.ID)
	a.mu.Unlock()
	a.settleEmittedClaudeInteractionCancels(n)
	if ok, why := a.settleClaudeInteractionCancel(n, rec.ID); ok || why != "turn mismatch" {
		t.Fatalf("missing bundle = %v %s", ok, why)
	}
}

func TestClaudeInteractionHookProtections(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now().UTC()
	rec := sampleRequest(dir, now)
	publishInteraction(t, dir, rec)
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, letterReply(), now, nil); !errors.Is(err, errInteractionAudit) {
		t.Fatalf("nil audit err = %v", err)
	}
	over := bytes.Repeat([]byte("x"), interactionFileMax+1)
	if err := os.WriteFile(filepath.Join(dir, "perm", "interact", "req", "req-big.json"), over, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), "req-big", letterReply(), now, func(sessionlog.Event) error { return nil }); !errors.Is(err, errInteractionIO) && !errors.Is(err, errInteractionBadRequest) {
		t.Fatalf("oversize err = %v", err)
	}
	if _, err := submitClaudeInteraction("relative", sampleCaller(dir), rec.ID, letterReply(), now, func(sessionlog.Event) error { return nil }); !errors.Is(err, errInteractionBadRequest) {
		t.Fatalf("relative bundle err = %v", err)
	}

	if _, err := serveClaudeInteraction(dir, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "../req", io.Discard, time.Millisecond, time.Millisecond, func() time.Time { return now }, func(time.Duration) {}); !errors.Is(err, errInteractionBadRequest) {
		t.Fatalf("serve escape err = %v", err)
	}
	bare := permBundle(t)
	exec, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	caps, err := json.Marshal(claudeHookCapabilities{Permission: 1, Exec: exec, Interaction: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bare, "capabilities.json"), caps, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := serveClaudeInteraction(bare, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "req-letter", io.Discard, time.Millisecond, time.Millisecond, func() time.Time { return now }, func(time.Duration) {}); !errors.Is(err, errInteractionIO) {
		t.Fatalf("missing interact err = %v", err)
	}
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, claudeInteractionReply{Action: "nope"}, now, func(sessionlog.Event) error {
		t.Error("invalid action was audited")
		return nil
	}); !errors.Is(err, errInteractionBadRequest) {
		t.Fatalf("bad action err = %v", err)
	}
	st, err := os.Stat(filepath.Join(dir, "perm", "interact", "req", rec.ID+".json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("request mode = %v err=%v", st.Mode(), err)
	}
	// Answer file I/O: parent is a file, so the envelope cannot be written.
	// Audit must already have succeeded and a retry must not emit another one.
	dir = interactionBundle(t)
	rec = sampleRequest(dir, now)
	rec.ID = "req-io"
	publishInteraction(t, dir, rec)
	if err := os.Remove(filepath.Join(dir, "perm", "interact", "ans")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "perm", "interact", "ans"), []byte("not-a-dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	audits := 0
	_, err = submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, letterReply(), now, func(sessionlog.Event) error {
		audits++
		return nil
	})
	if !errors.Is(err, errInteractionIO) || audits != 2 {
		t.Fatalf("io err=%v audits=%d", err, audits)
	}
	_, err = submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, letterReply(), now, func(sessionlog.Event) error {
		audits++
		return nil
	})
	if !errors.Is(err, errInteractionConflict) || audits != 2 {
		t.Fatalf("retry err=%v audits=%d", err, audits)
	}
}

func TestClaudeInteractionHookRecordedWithoutAnswerIsNotReplayed(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now().UTC()
	rec := sampleRequest(dir, now)
	publishInteraction(t, dir, rec)
	if claudeInteractionPhase(dir, rec.ID, now) != "pending" {
		t.Fatalf("phase = %s", claudeInteractionPhase(dir, rec.ID, now))
	}
	if err := exclWriteTest(filepath.Join(dir, "perm", "interact", "claim", rec.ID), []byte("claim")); err != nil {
		t.Fatal(err)
	}
	if claudeInteractionPhase(dir, rec.ID, now) != "claimed" {
		t.Fatalf("phase = %s", claudeInteractionPhase(dir, rec.ID, now))
	}
	if err := exclWriteTest(filepath.Join(dir, "perm", "interact", "recorded", rec.ID), []byte(`{"hookSpecificOutput":{}}`)); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	handled, err := serveClaudeInteraction(dir, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, rec.ID, &stdout, time.Second, time.Millisecond, func() time.Time { return now }, func(time.Duration) {})
	if err != nil || !handled || stdout.Len() != 0 {
		t.Fatalf("replay handled=%v err=%v stdout=%q", handled, err, stdout.Bytes())
	}
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, letterReply(), now, func(sessionlog.Event) error {
		t.Error("replay audited")
		return nil
	}); !errors.Is(err, errInteractionConflict) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "perm", "interact", "ans", rec.ID+".json")); !os.IsNotExist(statErr) {
		t.Fatal("replay wrote an answer")
	}
}

func TestClaudeInteractionHookBundleRollback(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	first := 1 + len(claudeHookBundleSubdirs) + 1
	for _, row := range []struct {
		name string
		ops  hookOps
	}{
		{"interaction MkdirAll", failNthMkdirAll(first)},
		{"interaction Chmod", failNthChmod(first)},
	} {
		t.Run(row.name, func(t *testing.T) {
			a := newTestApp(t, &fakeTmux{})
			_, _, err := a.prepareClaudeHookBundleWith(row.ops, "n1")
			if err == nil {
				t.Fatal("prepare succeeded")
			}
			ents, rerr := os.ReadDir(a.claudeHooksDir())
			if rerr != nil && !os.IsNotExist(rerr) {
				t.Fatal(rerr)
			}
			if len(ents) != 0 {
				t.Fatalf("bundle remained: %s", ents[0].Name())
			}
		})
	}
}

type interactionFileFault struct {
	n                 int
	writeErr, syncErr error
	closeErr          error
	short             bool
}

func (f *interactionFileFault) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.short {
		if len(p) == 0 {
			return 0, nil
		}
		return len(p) - 1, nil
	}
	return len(p), nil
}

func (f *interactionFileFault) Sync() error  { return f.syncErr }
func (f *interactionFileFault) Close() error { return f.closeErr }

func TestClaudeInteractionHookExclFaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leaf")
	if err := finishExcl(&interactionFileFault{writeErr: errors.New("write")}, path, []byte("ab")); err == nil {
		t.Fatal("write error was ignored")
	}
	if err := finishExcl(&interactionFileFault{short: true}, path, []byte("ab")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short err = %v", err)
	}
	if err := finishExcl(&interactionFileFault{syncErr: errors.New("sync")}, path, []byte("ab")); err == nil || !strings.Contains(err.Error(), "sync") {
		t.Fatalf("sync err = %v", err)
	}
	if err := finishExcl(&interactionFileFault{closeErr: errors.New("close")}, path, []byte("ab")); err == nil || !strings.Contains(err.Error(), "close") {
		t.Fatalf("close err = %v", err)
	}
	if err := finishExcl(&interactionFileFault{}, filepath.Join(dir, "missing", "leaf"), []byte("ab")); err == nil {
		t.Fatal("missing parent sync succeeded")
	}
}

func TestClaudeInteractionHookFaultPaths(t *testing.T) {
	now := time.Date(2026, 8, 9, 10, 11, 12, 0, time.UTC)
	dir := interactionBundle(t)
	if claudeInteractionPhase(dir, "req-missing", now) != "" || claudeInteractionPhase(dir, "../x", now) != "" {
		t.Fatal("missing or unsafe id looked live")
	}
	if interactionFileExists(dir, "ack", "../x") {
		t.Fatal("unsafe ack path existed")
	}
	if _, ok := readInteractionAck(dir, "../x"); ok {
		t.Fatal("unsafe ack was readable")
	}
	if _, err := readBounded(dir, 8); err == nil {
		t.Fatal("directory read succeeded")
	}
	if _, err := readInteractionAns(dir, "../x"); !errors.Is(err, errInteractionBadRequest) {
		t.Fatalf("ans escape err = %v", err)
	}
	if interactionAnsCancels([]byte(`{"hookSpecificOutput":{}}`)) || interactionAnsCancels([]byte("{")) {
		t.Fatal("answer without an interrupt flag canceled")
	}
	if err := noteClaudeInteractionEmission("relative", "req-letter", claudeInteractionAck{}); !errors.Is(err, errInteractionBadRequest) {
		t.Fatalf("relative ack err = %v", err)
	}
	if err := noteClaudeInteractionEmission(dir, "../x", claudeInteractionAck{}); !errors.Is(err, errInteractionBadRequest) {
		t.Fatalf("ack escape err = %v", err)
	}

	rec := sampleRequest(dir, now)
	publishInteraction(t, dir, rec)
	if err := os.Remove(filepath.Join(dir, "perm", "interact", "active.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, letterReply(), now, func(sessionlog.Event) error {
		t.Error("stale request was audited")
		return nil
	}); !errors.Is(err, errInteractionConflict) {
		t.Fatalf("stale err = %v", err)
	}
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), "req-absent", letterReply(), now, func(sessionlog.Event) error { return nil }); !errors.Is(err, errInteractionConflict) {
		t.Fatalf("missing request err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "perm", "interact", "req", "req-bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), "req-bad", letterReply(), now, func(sessionlog.Event) error { return nil }); !errors.Is(err, errInteractionBadRequest) {
		t.Fatalf("bad request err = %v", err)
	}

	native := sampleRequest(dir, now)
	native.ID = "req-native"
	native.Original = []byte(`{"questions":[{"question":"Pick?","header":"H","options":[{"label":"Same","description":"a"},{"label":"Same","description":"b"}],"multiSelect":false}]}`)
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "interact", "req", native.ID+".json"), native); err != nil {
		t.Fatal(err)
	}
	if err := exclWriteTest(filepath.Join(dir, "perm", "interact", "active.json"), []byte(`{"id":"req-native"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), native.ID, letterReply(), now, func(sessionlog.Event) error {
		t.Error("native original was audited")
		return nil
	}); !errors.Is(err, errInteractionBadRequest) {
		t.Fatalf("native err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "perm", "interact", "claim", native.ID)); !os.IsNotExist(err) {
		t.Fatal("native submit left the claim")
	}

	claimDir := interactionBundle(t)
	claimRec := sampleRequest(claimDir, now)
	claimRec.ID = "req-claim"
	publishInteraction(t, claimDir, claimRec)
	if err := os.Remove(filepath.Join(claimDir, "perm", "interact", "claim")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claimDir, "perm", "interact", "claim"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := submitClaudeInteraction(claimDir, sampleCaller(claimDir), claimRec.ID, letterReply(), now, func(sessionlog.Event) error {
		t.Error("claim failure was audited")
		return nil
	}); !errors.Is(err, errInteractionIO) {
		t.Fatalf("claim err = %v", err)
	}

	recordedDir := interactionBundle(t)
	recordedRec := sampleRequest(recordedDir, now)
	recordedRec.ID = "req-recorded"
	publishInteraction(t, recordedDir, recordedRec)
	if err := os.Remove(filepath.Join(recordedDir, "perm", "interact", "recorded")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recordedDir, "perm", "interact", "recorded"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	audits := 0
	if _, err := submitClaudeInteraction(recordedDir, sampleCaller(recordedDir), recordedRec.ID, letterReply(), now, func(sessionlog.Event) error {
		audits++
		return nil
	}); !errors.Is(err, errInteractionIO) || audits != 2 {
		t.Fatalf("recorded err=%v audits=%d", err, audits)
	}

	closedDir := interactionBundle(t)
	closedRec := sampleRequest(closedDir, now)
	closedRec.ID = "req-closed"
	publishInteraction(t, closedDir, closedRec)
	if err := os.Remove(filepath.Join(closedDir, "perm", "interact", "closed")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(closedDir, "perm", "interact", "closed"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := submitClaudeInteraction(closedDir, sampleCaller(closedDir), closedRec.ID, claudeInteractionReply{Action: "terminal"}, now, func(sessionlog.Event) error {
		return nil
	}); !errors.Is(err, errInteractionIO) {
		t.Fatalf("closed err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(closedDir, "perm", "interact", "ans", closedRec.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("closed failure released an answer")
	}

	ackDir := interactionBundle(t)
	if err := os.RemoveAll(filepath.Join(ackDir, "perm", "interact", "ack")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ackDir, "perm", "interact", "ack"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := noteClaudeInteractionEmission(ackDir, "req-letter", claudeInteractionAck{Emitted: true}); !errors.Is(err, errInteractionIO) {
		t.Fatalf("ack io err = %v", err)
	}

	past := sampleRequest(dir, now)
	past.ID = "req-past"
	past.AnswerUntil = now.Add(-time.Second).UTC().Format(time.RFC3339Nano)
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "interact", "req", past.ID+".json"), past); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "perm", "interact", "active.json")); err != nil {
		t.Fatal(err)
	}
	if err := exclWriteTest(filepath.Join(dir, "perm", "interact", "active.json"), []byte(`{"id":"req-past"}`)); err != nil {
		t.Fatal(err)
	}
	if claudeInteractionPhase(dir, past.ID, now) != "expired" {
		t.Fatalf("clock phase = %s", claudeInteractionPhase(dir, past.ID, now))
	}
	hand := interactionBundle(t)
	handRec := sampleRequest(hand, now)
	handRec.ID = "req-hand"
	if err := writeClaudePermFile(filepath.Join(hand, "perm", "interact", "req", handRec.ID+".json"), handRec); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(filepath.Join(hand, "perm", "interact", "ack", handRec.ID+".json"), claudeInteractionAck{Emitted: true, Terminal: true}); err != nil {
		t.Fatal(err)
	}
	if claudeInteractionPhase(hand, handRec.ID, now) != "handoff" {
		t.Fatalf("ack handoff phase = %s", claudeInteractionPhase(hand, handRec.ID, now))
	}

	pub := interactionBundle(t)
	if err := os.Remove(filepath.Join(pub, "perm", "interact", "req")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pub, "perm", "interact", "req"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := serveClaudeInteraction(pub, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "req-pub", io.Discard, time.Second, time.Millisecond, func() time.Time { return now }, func(time.Duration) { t.Error("publish failure waited") }); !errors.Is(err, errInteractionIO) {
		t.Fatalf("publish err = %v", err)
	}
	active := interactionBundle(t)
	if err := os.WriteFile(filepath.Join(active, "perm", "interact", "active.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := serveClaudeInteraction(active, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "req-active", io.Discard, time.Second, time.Millisecond, func() time.Time { return now }, func(time.Duration) {}); !errors.Is(err, errInteractionIO) {
		t.Fatalf("active err = %v", err)
	}

	waitDir := interactionBundle(t)
	if err := os.Remove(filepath.Join(waitDir, "perm", "interact", "waiter")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(waitDir, "perm", "interact", "waiter"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	handled, err := serveClaudeInteraction(waitDir, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "req-wait", io.Discard, time.Second, time.Millisecond, func() time.Time { return now }, func(time.Duration) {
		t.Error("waiter failure slept")
	})
	if !handled || err != nil {
		t.Fatalf("waiter failure handled=%v err=%v", handled, err)
	}

	var quiet shortInteractionWriter
	quiet.nilShort = true
	quietDir := interactionBundle(t)
	if err := os.WriteFile(filepath.Join(quietDir, "perm", "interact", "ans", "req-quiet-short.json"), []byte(`{"hookSpecificOutput":{"decision":{"behavior":"allow"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	handled, err = serveClaudeInteraction(quietDir, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "req-quiet-short", &quiet, time.Second, time.Millisecond, func() time.Time { return now }, func(time.Duration) {
		t.Error("short write slept")
	})
	if !handled || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("nil short handled=%v err=%v", handled, err)
	}

	term := interactionBundle(t)
	if err := os.WriteFile(filepath.Join(term, "perm", "interact", "ans", "req-term.json"), []byte(`{"terminal":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	handled, err = serveClaudeInteraction(term, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "req-term", &stdout, time.Second, time.Millisecond, func() time.Time { return now }, func(time.Duration) { t.Error("terminal answer waited") })
	if !handled || err != nil || stdout.Len() != 0 {
		t.Fatalf("terminal ans handled=%v err=%v stdout=%q", handled, err, stdout.Bytes())
	}
	if ack, ok := readInteractionAck(term, "req-term"); !ok || !ack.Terminal {
		t.Fatalf("terminal ack = %+v ok=%v", ack, ok)
	}

	idDir := interactionBundle(t)
	idRec := sampleRequest(idDir, now)
	idRec.NodeID = ""
	idRec.Generation = 4
	publishInteraction(t, idDir, idRec)
	if err := os.WriteFile(filepath.Join(idDir, "perm", "generation.json"), []byte(`{"generation":4,"node_id":"node-a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	caller := sampleCaller(idDir)
	caller.NodeID = "node-b"
	caller.Generation = 4
	if _, err := submitClaudeInteraction(idDir, caller, idRec.ID, letterReply(), now, func(sessionlog.Event) error { return nil }); !errors.Is(err, errInteractionConflict) {
		t.Fatalf("generation node err = %v", err)
	}
	plain := interactionBundle(t)
	plainRec := sampleRequest(plain, now)
	plainRec.Generation = 2
	publishInteraction(t, plain, plainRec)
	plainCaller := sampleCaller(plain)
	plainCaller.Generation = 9
	if _, err := submitClaudeInteraction(plain, plainCaller, plainRec.ID, letterReply(), now, func(sessionlog.Event) error { return nil }); !errors.Is(err, errInteractionConflict) {
		t.Fatalf("generation mismatch err = %v", err)
	}
}
