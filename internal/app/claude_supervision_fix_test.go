package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeNativeForkCommandShape(t *testing.T) {
	yes := []string{"/fork", "  /fork  ", "/fork extra", "/fork\targs", "/fork\nmore"}
	no := []string{"please use /fork", "see /fork docs", "/forked", "I ran /fork yesterday", "/clear", "fork"}
	for _, s := range yes {
		if !claudeNativeForkCommand(s) {
			t.Errorf("claudeNativeForkCommand(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if claudeNativeForkCommand(s) {
			t.Errorf("claudeNativeForkCommand(%q) = true, want false", s)
		}
	}
}

func TestClaudeSendRejectsNativeForkCommand(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, capture: "idle"}
	a := newTestApp(t, f)
	n, _ := seedPermClaude(t, a, "n1", hookSIDOwn)

	rec := routeRequest(newTestHandler(t, a), http.MethodPost, "/api/nodes/"+n.ID+"/send",
		`{"text":"/fork"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("/fork status = %d body %q, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Fork action") {
		t.Fatalf("/fork error = %q, want a pointer to scimux Fork", rec.Body.String())
	}
	if containsSub(f.subcommands(), "paste-buffer") || containsSub(f.subcommands(), "send-keys") {
		t.Fatal("rejected /fork must not type into the Claude pane")
	}

	rec = routeRequest(newTestHandler(t, a), http.MethodPost, "/api/nodes/"+n.ID+"/send",
		`{"text":"please use /fork to branch later"}`, true)
	if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "Fork action") {
		t.Fatal("prose mentioning /fork must not be rejected as a native fork")
	}
}

func TestClaudeSendForkDoesNotAffectOtherAgents(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"p1": true}, capture: "idle", captureAfterEnter: "ok"}
	a := newTestApp(t, f)
	n := tmuxFallbackNode("p1", "")
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	a.server.PasteDelay, a.server.AckPoll = time.Millisecond, time.Millisecond
	rec := routeRequest(newTestHandler(t, a), http.MethodPost, "/api/nodes/"+n.ID+"/send",
		`{"text":"/fork"}`, true)
	if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "Fork action") {
		t.Fatalf("non-Claude /fork was rejected as a Claude command: %s", rec.Body.String())
	}
}

func TestClaudeScimuxForkCreatesFreshOwnedNode(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	parentRec := newNode(a, `{"title":"Root","prompt":"first","agent":"claude","model":"opus","effort":"medium","dir":`+strconvQuote(a.home)+`,"lane_id":"lane-a"}`)
	if parentRec.Code != 200 {
		t.Fatalf("parent: %d %s", parentRec.Code, parentRec.Body.String())
	}
	var parent Node
	if err := json.Unmarshal(parentRec.Body.Bytes(), &parent); err != nil {
		t.Fatal(err)
	}
	childRec := newNode(a, `{"title":"Child","prompt":"fresh","parent":`+strconvQuote(parent.ID)+`}`)
	if childRec.Code != 200 {
		t.Fatalf("fork: %d %s", childRec.Code, childRec.Body.String())
	}
	var child Node
	if err := json.Unmarshal(childRec.Body.Bytes(), &child); err != nil {
		t.Fatal(err)
	}
	if child.Agent != "claude" || child.Model != "opus" || child.Effort != "medium" || child.Dir != parent.Dir {
		t.Fatalf("fork did not inherit launch config: agent=%q model=%q effort=%q dir=%q",
			child.Agent, child.Model, child.Effort, child.Dir)
	}
	if child.ID == parent.ID || child.SessionID == "" || child.SessionID == parent.SessionID {
		t.Fatalf("fork must mint a fresh process/session: parent=%q/%q child=%q/%q",
			parent.ID, parent.SessionID, child.ID, child.SessionID)
	}
	if child.Transcript != "" {
		t.Fatalf("fork inherited transcript %q", child.Transcript)
	}
	if a.claudeHookID(child.ID) == "" || a.claudeHookID(child.ID) == a.claudeHookID(parent.ID) {
		t.Fatalf("fork must prepare a fresh hook bundle (parent=%q child=%q)",
			a.claudeHookID(parent.ID), a.claudeHookID(child.ID))
	}
	if !child.AXScreenReader {
		t.Fatal("owned Claude fork must launch with AX")
	}
	path := writeClaudeProject(t, a.home, "-w-proj", child.SessionID)
	if err := a.processClaudeHookEvent(child.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "startup",
		SessionID: child.SessionID, TranscriptPath: path, Cwd: child.Dir,
	}); err != nil {
		t.Fatalf("scimux fork binds via source:startup: %v", err)
	}
	if err := a.processClaudeHookEvent(child.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "fork",
		SessionID: hookSIDSuccessor, TranscriptPath: path, Cwd: child.Dir,
	}); err == nil {
		t.Fatal("source:fork must stay rejected on a scimux-forked node")
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestClaudeQuestionOptionsFromAXMenu(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: axAskUserQuestionPane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "AskUserQuestion", Digest: "q1"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "qreq"), note); err != nil {
		t.Fatal(err)
	}
	ep, err := mintClaudeVisibleEpochLabeled(perm, hookSIDOwn, "Permission Required", "")
	if err != nil {
		t.Fatal(err)
	}
	if ep.HintTool != "AskUserQuestion" {
		t.Fatalf("hint tool = %q, want AskUserQuestion from the standing ask", ep.HintTool)
	}
	a.poll()
	if got := a.attn[n.ID]; got != "question" {
		t.Fatalf("attention = %q, want question (Notification title must not override the ask tool)", got)
	}
	body := chatBody(t, a, n.ID)
	if body["perm_dialog_id"] != ep.Epoch {
		t.Fatalf("perm_dialog_id = %v, want %s", body["perm_dialog_id"], ep.Epoch)
	}
	rawOpts, _ := body["perm_options"].([]any)
	if len(rawOpts) != 4 {
		t.Fatalf("perm_options = %#v, want 4 AX choices including option 4", body["perm_options"])
	}
	var labels []string
	for i, raw := range rawOpts {
		m, _ := raw.(map[string]any)
		labels = append(labels, fmtString(m["name"]))
		if fmtString(m["key"]) != []string{"1", "2", "3", "4"}[i] {
			t.Errorf("option key %v, want %d", m["key"], i+1)
		}
		if fmtString(m["kind"]) != "" {
			t.Errorf("question option %v invented kind %q", m["name"], m["kind"])
		}
		if strings.Contains(strings.ToLower(fmtString(m["name"])), "don't ask") ||
			strings.Contains(strings.ToLower(fmtString(m["name"])), "allow always") {
			t.Fatalf("invented allow-always label %q", m["name"])
		}
	}
	want := []string{"Keep the poller mechanical", "Parse the TUI", "Other", "Chat about this"}
	for i, w := range want {
		if labels[i] != w {
			t.Errorf("label %d = %q, want %q", i+1, labels[i], w)
		}
	}

	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	a.poll()
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("armed question attention = %q, want none (no auto-open)", got)
	}
	armed := chatBody(t, a, n.ID)
	if armed["perm_dialog_id"] != ep.Epoch {
		t.Fatal("armed question must keep the epoch-bound surface")
	}
}

func fmtString(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func TestClaudeQuestionWithoutAXMenuIsManualOnly(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "spinner"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "AskUserQuestion", Digest: "q1"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "qreq"), note); err != nil {
		t.Fatal(err)
	}
	ep, err := mintClaudeVisibleEpoch(perm, hookSIDOwn)
	if err != nil {
		t.Fatal(err)
	}
	body := chatBody(t, a, n.ID)
	if body["perm_dialog_id"] != ep.Epoch {
		t.Fatalf("perm_dialog_id = %v", body["perm_dialog_id"])
	}
	if body["perm_options"] != nil {
		t.Fatalf("guessed perm_options = %#v", body["perm_options"])
	}
	if body["perm_manual"] != true {
		t.Fatal("want perm_manual when options cannot be extracted")
	}
	rec := httptestPostKey(t, a, n.ID, "1", ep.Epoch)
	if rec.Code != http.StatusConflict {
		t.Fatalf("key without proven options = %d, want 409", rec.Code)
	}
	if containsSub(f.subcommands(), "send-keys") {
		t.Fatal("must not type a guessed answer")
	}
}

func TestClaudeStopFromTurnNDoesNotMutateTurnN1(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")

	turnN, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	if turnN.Turn == "" {
		t.Fatal("turn N nonce missing")
	}
	noteN := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "dn"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "ask-n"), noteN); err != nil {
		t.Fatal(err)
	}
	if _, err := mintClaudeVisibleEpoch(perm, hookSIDOwn); err != nil {
		t.Fatal(err)
	}
	stopN := claudeStopNotice{
		Lease: "lease-n", Turn: turnN.Turn, TurnGen: turnN.Gen,
		HookEventName: "Stop", SessionID: hookSIDOwn,
		At: time.Now().UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(stopN)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeHookInboxFile(filepath.Join(bundle, "stop"), raw); err != nil {
		t.Fatal(err)
	}

	if _, err := a.beginClaudeAcceptedTurn(n); !errors.Is(err, errClaudeTurnInFlight) {
		t.Fatalf("begin N+1 before Stop drain = %v, want turn-in-flight", err)
	}
	a.drainClaudeStopInbox(n.ID, filepath.Base(bundle))
	turnN1, err := a.beginClaudeAcceptedTurn(n)
	if err != nil || turnN1.Turn == "" || turnN1.Turn == turnN.Turn {
		t.Fatalf("turn N+1 after Stop = %+v, %v; want a new nonce", turnN1, err)
	}
	noteN1 := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Edit", Digest: "dn1"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "ask-n1"), noteN1); err != nil {
		t.Fatal(err)
	}
	epN1, err := mintClaudeVisibleEpoch(perm, hookSIDOwn)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.live[n.ID] = "active"
	a.attn[n.ID] = "approval"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	leaseN1 := leaseIDOf(a, n.ID)
	if leaseN1 == "" {
		t.Fatal("turn N+1 lease missing")
	}

	if _, err := os.Stat(claudeAskedPath(perm, "ask-n1")); err != nil {
		t.Fatal("late Stop from turn N tombstoned turn N+1's ask")
	}
	if ep, ok := readClaudeVisibleEpoch(perm); !ok || ep.Epoch != epN1.Epoch {
		t.Fatalf("turn N+1 epoch = %+v ok=%v, want %s", ep, ok, epN1.Epoch)
	}
	if got := leaseIDOf(a, n.ID); got != leaseN1 {
		t.Fatalf("lease after late Stop = %q, want N+1 %q", got, leaseN1)
	}
	if got := phaseOf(a, n.ID); got != autoPhaseArmed {
		t.Fatalf("phase after late Stop = %q, want armed", got)
	}
	if got := a.attn[n.ID]; got != "approval" {
		t.Fatalf("attention after late Stop = %q, want N+1 approval", got)
	}
	if got := a.claudeAcceptedTurnOf(n.ID); got.Turn != turnN1.Turn {
		t.Fatalf("current turn = %+v, want N+1 %s", got, turnN1.Turn)
	}
}

func TestClaudeTurnFencePublishedBeforeEnterAndBlocksReplacement(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")

	acked, err := a.acceptTmuxPrompt(n, false, func() (bool, error) {
		turn, ok := readClaudeAcceptedTurn(perm)
		if !ok || turn.Turn == "" {
			t.Fatal("turn fence was not durable before Enter")
		}
		if err := RunClaudeStopHook(bundle, strings.NewReader(string(stopEventJSON("Stop", false))), nil, nil); err != nil {
			t.Fatalf("Stop during send: %v", err)
		}
		return true, nil
	})
	if err != nil || !acked {
		t.Fatalf("accept = %v, %v", acked, err)
	}
	notices := readyStopNotices(t, bundle)
	if len(notices) != 1 || notices[0].Turn == "" {
		t.Fatalf("fast Stop notice = %+v, want the pre-published turn", notices)
	}
	if _, err := a.beginClaudeAcceptedTurn(n); !errors.Is(err, errClaudeTurnInFlight) {
		t.Fatalf("replacement before Stop drain = %v, want turn-in-flight", err)
	}
	a.drainClaudeStopInbox(n.ID, filepath.Base(bundle))
	if got := a.claudeAcceptedTurnOf(n.ID); got.Turn != "" {
		t.Fatalf("matching Stop left turn live: %+v", got)
	}
}

func TestClaudeTurnFenceWriteFailureSendsNothing(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	turnPath := claudeTurnPath(filepath.Join(bundle, "perm"))
	if err := os.Mkdir(turnPath, 0o700); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := a.acceptTmuxPrompt(n, false, func() (bool, error) {
		called = true
		return true, nil
	})
	if !errors.Is(err, errClaudeTurnMarker) {
		t.Fatalf("accept error = %v, want turn-marker failure", err)
	}
	if called {
		t.Fatal("tmux send ran without a durable turn fence")
	}
	if got := a.claudeAcceptedTurnOf(n.ID); got.Turn != "" {
		t.Fatalf("failed marker publication installed live turn %+v", got)
	}
}

func TestClaudeLateNotifyAfterNewTurnAskDoesNotMintEpoch(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: axPermissionPane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")

	turnN, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	a.resetClaudePermissionTurn(n) // closes turn N
	turnN1, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	if turnN1.Turn == turnN.Turn {
		t.Fatal("N+1 reused N")
	}
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "n1"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "ask-n1"), note); err != nil {
		t.Fatal(err)
	}
	oldNotify := claudeNotifyNotice{
		HookEventName: "Notification", NotificationType: "permission_prompt",
		SessionID: hookSIDOwn, Title: "Permission Required",
		Turn: turnN.Turn, TurnGen: turnN.Gen,
		At: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(oldNotify)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeHookInboxFile(filepath.Join(bundle, "notify"), raw); err != nil {
		t.Fatal(err)
	}
	a.drainClaudeNotifyInbox(n.ID, filepath.Base(bundle))
	if _, ok := readClaudeVisibleEpoch(perm); ok {
		t.Fatal("old notification must not mint an epoch after turn N+1's ask exists")
	}
	if a.claudeDialogNoteOf(n.ID) == "" {
		t.Fatal("ambiguous/late notify should leave an inline manual diagnostic")
	}
	body := chatBody(t, a, n.ID)
	if body["perm_dialog_id"] != nil && body["perm_dialog_id"] != "" {
		t.Fatalf("chat exposed perm_dialog_id %v from a late notify", body["perm_dialog_id"])
	}
	if body["perm_options"] != nil {
		t.Fatalf("late notify invented options: %#v", body["perm_options"])
	}
}

func persistCommittedBinding(t *testing.T, a *app, n *Node, hookID string, gen int, path, sid, cause string) {
	t.Helper()
	if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		t.Fatal(err)
	}
	if err := a.appendRecord(storeRecord{Type: "claude-hook", ID: n.ID, HookID: hookID, Generation: gen}); err != nil {
		t.Fatal(err)
	}
	if err := a.appendRecord(storeRecord{
		Type: "claude-binding", ID: n.ID, HookID: hookID, Generation: gen,
		Path: path, SessionID: sid, Cause: cause,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeResumeCandidateThenGenerationChange(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("keep", 0))
	next := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, cur)
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.claudeHooks[n.ID] = hookID
	a.claudeGens[n.ID] = 1
	a.mu.Unlock()
	persistCommittedBinding(t, a, n, hookID, 1, cur, hookSIDOwn, "startup")

	a.claudeAfterCandidate = func(id string) {
		a.mu.Lock()
		a.claudeGens[id] = 2
		a.mu.Unlock()
	}
	err = a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "resume",
		SessionID: hookSIDSuccessor, TranscriptPath: next, Cwd: "/w/proj",
	})
	if err == nil {
		t.Fatal("resume must fail when generation changes after the candidate")
	}
	if n.Transcript != cur || n.SessionID != hookSIDOwn {
		t.Fatalf("live memory rebound after failed resume: %q / %q", n.Transcript, n.SessionID)
	}
	if a.attn[n.ID] == "inspect" {
		t.Fatal("failed resume must not raise inspect")
	}
	if a.claudeLaunchError(n.ID) == "" {
		t.Fatal("failed changed-resume must publish an inline error")
	}

	a2 := reloadApp(t, a, f)
	n2 := a2.byID[n.ID]
	if n2 == nil || n2.Transcript != cur || n2.SessionID != hookSIDOwn {
		t.Fatalf("replay applied an uncommitted candidate: %+v", n2)
	}
	if a2.isDeadTranscript(n.ID, cur, hookSIDOwn) {
		t.Fatal("failed resume must not tombstone the previous transcript")
	}
}

func TestClaudeResumeSerializesConcurrentClearGeneration(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("keep", 0))
	next := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, cur)
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.claudeHooks[n.ID] = hookID
	a.claudeGens[n.ID] = 1
	a.mu.Unlock()
	persistCommittedBinding(t, a, n, hookID, 1, cur, hookSIDOwn, "startup")

	attempted := make(chan struct{})
	clearDone := make(chan error, 1)
	a.claudeAfterCandidate = func(string) {
		go func() {
			close(attempted)
			clearDone <- a.advanceClaudeClearAfterWeb(n)
		}()
		<-attempted
		select {
		case err := <-clearDone:
			t.Fatalf("concurrent clear escaped the bind lock: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "resume",
		SessionID: hookSIDSuccessor, TranscriptPath: next, Cwd: "/w/proj",
	}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := <-clearDone; err != nil {
		t.Fatalf("serialized clear generation: %v", err)
	}
	a.mu.Lock()
	gen := a.claudeGens[n.ID]
	a.mu.Unlock()
	if gen != 2 {
		t.Fatalf("generation = %d, want resume gen 1 followed by clear gen 2", gen)
	}
}

func TestReplaySkipsBindingOlderThanDetachedGeneration(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	path := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn)
	n := &Node{ID: "n1", Title: "n1", Agent: "claude", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		t.Fatal(err)
	}
	if err := a.appendRecord(storeRecord{Type: "claude-binding", ID: n.ID, HookID: "hook-n1", Generation: 1, Path: path, SessionID: hookSIDOwn}); err != nil {
		t.Fatal(err)
	}
	if err := a.appendRecord(storeRecord{Type: "claude-hook", ID: n.ID, HookID: "hook-n1", Generation: 2}); err != nil {
		t.Fatal(err)
	}
	a2 := reloadApp(t, a, f)
	got := a2.byID[n.ID]
	if got == nil || got.Transcript != "" || got.SessionID != "" {
		t.Fatalf("stale generation-1 binding crossed detached generation 2: %+v", got)
	}
	if a2.claudeGens[n.ID] != 2 {
		t.Fatalf("replayed generation = %d, want 2", a2.claudeGens[n.ID])
	}
}

func TestClaudeResumeUncommittedCandidateIgnoredOnReplay(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("keep", 0))
	next := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, cur)
	persistCommittedBinding(t, a, n, "hook-n1", 1, cur, hookSIDOwn, "startup")
	if err := a.appendRecord(storeRecord{
		Type: "claude-binding-candidate", ID: n.ID, HookID: "hook-n1", Generation: 1,
		Path: next, SessionID: hookSIDSuccessor, Cause: "resume",
	}); err != nil {
		t.Fatal(err)
	}
	a2 := reloadApp(t, a, f)
	n2 := a2.byID[n.ID]
	if n2 == nil || n2.Transcript != cur || n2.SessionID != hookSIDOwn {
		t.Fatalf("replay applied candidate: %+v", n2)
	}
}

func TestClaudeResumeTombstoneFailureIsAtomicOnReplay(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("keep", 0))
	next := writeClaudeProject(t, a.home, "-w-proj", hookSIDSuccessor)
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, cur)
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.claudeHooks[n.ID] = hookID
	a.claudeGens[n.ID] = 1
	a.mu.Unlock()
	persistCommittedBinding(t, a, n, hookID, 1, cur, hookSIDOwn, "startup")

	if err := a.processClaudeHookEvent(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "resume",
		SessionID: hookSIDSuccessor, TranscriptPath: next, Cwd: "/w/proj",
	}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n.Transcript != next || n.SessionID != hookSIDSuccessor {
		t.Fatalf("live rebind missing: %q / %q", n.Transcript, n.SessionID)
	}
	if !a.isDeadTranscript(n.ID, cur, hookSIDOwn) {
		t.Fatal("successful resume must tombstone the old path/session")
	}
	a2 := reloadApp(t, a, f)
	n2 := a2.byID[n.ID]
	if n2 == nil || n2.Transcript != next || n2.SessionID != hookSIDSuccessor {
		t.Fatalf("replay lost the new binding: %+v", n2)
	}
	if !a2.isDeadTranscript(n.ID, cur, hookSIDOwn) {
		t.Fatal("replay lost the tombstone")
	}
}

func TestHandleSendClearUnconfirmedStillCleansClaudePermission(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, capture: "same"}
	a := newTestApp(t, f)
	a.server.PasteDelay, a.server.AckPoll = time.Millisecond, time.Millisecond
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	tx := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(tx, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n.Transcript, n.SessionID = tx, hookSIDOwn
	perm := filepath.Join(bundle, "perm")
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: hookSIDOwn, Tool: "Bash", Digest: "d1"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "ask-a"), note); err != nil {
		t.Fatal(err)
	}
	if _, err := mintClaudeVisibleEpoch(perm, hookSIDOwn); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.live[n.ID] = "active"
	a.attn[n.ID] = "approval"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/nodes/"+n.ID+"/send", strings.NewReader(`{"text":"/clear"}`))
	req.SetPathValue("id", n.ID)
	a.handleSend(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/clear: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unconfirmed") {
		t.Fatalf("want unconfirmed /clear (no pane change), got %s", rec.Body.String())
	}
	if _, err := os.Stat(claudeAskedPath(perm, "ask-a")); !os.IsNotExist(err) {
		t.Fatal("unconfirmed /clear must still tombstone standing asks")
	}
	if _, ok := readClaudeVisibleEpoch(perm); ok {
		t.Fatal("unconfirmed /clear must clear the visible epoch")
	}
	if got := phaseOf(a, n.ID); got != autoPhaseOff {
		t.Fatalf("phase after unconfirmed /clear = %q, want off", got)
	}
	if n.Transcript != "" || n.SessionID != "" {
		t.Fatalf("unconfirmed /clear must retire the old transcript: %q / %q", n.Transcript, n.SessionID)
	}
	if got := a.attn[n.ID]; got != "" {
		t.Fatalf("attention after unconfirmed /clear = %q", got)
	}
}
