package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/backend"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/sessionworker"
)

func syntheticWorkerManager(t *testing.T, data string) *workerManager {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := newWorkerManager(exe, data, "muxer-new")
	m.startOptions = sessionWorkerStartOptions{
		args: []string{"-test.run=^TestSessionWorkerHelperProcess$"},
		env:  []string{"SCIMUX_SESSION_WORKER_TEST_HELPER=1"},
	}
	m.nextID = func() string { return "worker-generation-old" }
	return m
}

func TestWorkerManagerRejectsInvalidAndDuplicateLaunches(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	m := syntheticWorkerManager(t, data)
	if _, err := m.LaunchNode(nil, ""); err == nil {
		t.Fatal("nil node launch accepted")
	}
	if _, err := m.AdoptClaude(nil, "hook", 1); err == nil {
		t.Fatal("nil Claude adoption accepted")
	}
	if _, err := m.AdoptClaude(&Node{ID: "not-claude", Agent: "pi"}, "hook", 1); err == nil {
		t.Fatal("non-Claude adoption accepted")
	}
	if _, err := m.Launch("duplicate", "pi", data, "", ""); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()
	if _, err := m.Launch("duplicate", "pi", data, "", ""); !errors.Is(err, errNoSessionWorker) {
		t.Fatalf("duplicate launch = %v", err)
	}
	if err := m.Kill("missing"); err != nil {
		t.Fatalf("stop missing worker = %v", err)
	}
}

func TestWorkerManagerOwnsOneProcessPerSessionAndReconciles(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	m := syntheticWorkerManager(t, data)
	sid, err := m.Launch("chat-one", "opencode", data, "cheap", "low")
	if err != nil || sid != "synthetic-session" {
		t.Fatalf("Launch = %q, %v", sid, err)
	}
	locator, err := sessionworker.Discover(data, "chat-one")
	if err != nil {
		t.Fatal(err)
	}
	if locator.Executable == "" {
		t.Fatal("worker locator omitted its pinned executable")
	}
	workerPID := locator.PID
	if !m.HasSession("chat-one") || m.Live("chat-one") != "quiet" {
		t.Fatalf("initial state: session=%v live=%q", m.HasSession("chat-one"), m.Live("chat-one"))
	}
	if got := m.SessionID("chat-one"); got != "synthetic-session" {
		t.Fatalf("SessionID = %q", got)
	}
	if err := m.Send("chat-one", "read a file"); err != nil {
		t.Fatal(err)
	}
	pending, ok := m.Pending("chat-one")
	if !ok || pending.RequestID != "synthetic:1" || m.Attention("chat-one") != "approval" || m.Live("chat-one") != "active" {
		t.Fatalf("pending = %#v, %v attention=%q live=%q", pending, ok, m.Attention("chat-one"), m.Live("chat-one"))
	}
	if incarnation, max, ok := m.PermissionBoundary("chat-one"); !ok || incarnation != "synthetic" || max != 1 {
		t.Fatalf("boundary = %q %d %v", incarnation, max, ok)
	}
	if got := m.Peek("chat-one"); got != "synthetic event log" {
		t.Fatalf("Peek = %q", got)
	}

	// A muxer handoff closes only clients. The registered worker and its
	// synthetic in-flight permission remain owned by the same Unix PID.
	m.Detach()
	replacement := syntheticWorkerManager(t, data)
	node := &Node{ID: "chat-one", Agent: "opencode", Transport: "acp"}
	if err := replacement.Reconcile([]*Node{node}); err != nil {
		t.Fatal(err)
	}
	locator, err = sessionworker.Discover(data, "chat-one")
	if err != nil || locator.PID != workerPID {
		t.Fatalf("worker after handoff = %#v, %v; want PID %d", locator, err, workerPID)
	}
	pending, ok = replacement.Pending("chat-one")
	if !ok || pending.RequestID != "synthetic:1" {
		t.Fatalf("reattached pending = %#v, %v", pending, ok)
	}
	token, evidence, err := replacement.PrepareResolve("chat-one", pending.RequestID, "1")
	if err != nil || token != "allow-once" || evidence != "read a file" {
		t.Fatalf("PrepareResolve = %q %q %v", token, evidence, err)
	}
	if err := replacement.Deliver("chat-one", token); err != nil {
		t.Fatal(err)
	}
	if _, ok := replacement.Pending("chat-one"); ok || replacement.Live("chat-one") != "quiet" {
		t.Fatal("permission did not settle after reattached delivery")
	}
	if err := replacement.RecordStartFailure("chat-one", errors.New("visible failure")); err != nil {
		t.Fatal(err)
	}
	if err := replacement.AppendSessionEvent("chat-one", sessionlog.NewAttentionEdge("approval", "end")); err != nil {
		t.Fatal(err)
	}
	if err := replacement.Interrupt("chat-one"); err != nil {
		t.Fatal(err)
	}
	if err := replacement.Clear("chat-one"); err != nil {
		t.Fatal(err)
	}
	if err := replacement.Kill("chat-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionworker.Discover(data, "chat-one"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("locator after Kill = %v", err)
	}
}

func TestWorkerManagerRecoversInterruptedCreateAndFinishesDurableStops(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	data := filepath.Dir(a.storePath)
	first := syntheticWorkerManager(t, data)
	createdAt := time.Now().UTC().Format(time.RFC3339)
	fresh := &Node{
		ID: "unpublished", Parent: "parent", Title: "Recovered", Prompt: "first", Description: "description",
		Rationale: "fork reason", LaneID: "lane", ForkKind: "y-stay", Agent: "opencode", Model: "cheap",
		Effort: "low", Dir: a.home, Transport: "acp", CreatedAt: createdAt,
	}
	deleted := &Node{ID: "deleted", Title: "Deleted", Prompt: "first", Agent: "codex", Dir: a.home, Transport: "codex", CreatedAt: createdAt}
	ended := &Node{ID: "ended", Title: "Ended", Prompt: "first", Agent: "pi", Dir: a.home, Transport: "acp", CreatedAt: createdAt, EndedAt: createdAt}
	if _, err := first.LaunchNode(fresh, fresh.Model); err != nil {
		t.Fatal(err)
	}
	if _, err := first.LaunchNode(deleted, deleted.Model); err != nil {
		t.Fatal(err)
	}
	if _, err := first.LaunchNode(ended, ended.Model); err != nil {
		t.Fatal(err)
	}
	first.Detach() // model a killed muxer after worker launch
	if err := a.appendRecord(storeRecord{Type: "delete", ID: deleted.ID, Time: createdAt}); err != nil {
		t.Fatal(err)
	}
	if err := a.appendRecord(storeRecord{Type: "node", Node: ended}); err != nil {
		t.Fatal(err)
	}
	a = reloadApp(t, a, f)

	replacement := syntheticWorkerManager(t, data)
	if err := replacement.Reconcile(a.nodes); err != nil {
		t.Fatal(err)
	}
	if err := replacement.RecoverUnknown(a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replacement.Shutdown)
	got := a.byID[fresh.ID]
	if got == nil || got.Parent != fresh.Parent || got.Description != fresh.Description || got.Rationale != fresh.Rationale || got.LaneID != fresh.LaneID || got.ForkKind != fresh.ForkKind || got.SessionID != "synthetic-session" {
		t.Fatalf("recovered node = %#v", got)
	}
	if !replacement.manages(fresh.ID) {
		t.Fatal("recovered node was not attached to its original worker")
	}
	if a.byID[deleted.ID] != nil || replacement.manages(deleted.ID) || replacement.manages(ended.ID) {
		t.Fatal("durably stopped worker was resurrected or retained")
	}
	for _, id := range []string{deleted.ID, ended.ID} {
		if _, err := sessionworker.Discover(data, id); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("durably stopped worker locator %s remains: %v", id, err)
		}
	}
	records := keyRecords(t, a.storePath)
	if len(records) != 3 || records[0].Type != "delete" || records[1].Type != "node" || records[1].Node.ID != ended.ID || records[2].Type != "node" || records[2].Node.ID != fresh.ID {
		t.Fatalf("recovery records = %#v", records)
	}
}

func TestWorkerManagerRecoveryClassifiesMixedLocatorSet(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	data := filepath.Dir(a.storePath)
	m := newWorkerManager("unused", data, "new")
	if err := m.RecoverUnknown(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Detach)

	register := func(nodeID, agent string, harness *syntheticSessionHarness) (*sessionworker.Client, sessionworker.Identity) {
		t.Helper()
		identity := sessionworker.Identity{WorkerID: "worker-" + nodeID, Agent: agent, Build: "old"}
		server, err := sessionworker.Listen("", identity, harness)
		if err != nil {
			t.Fatal(err)
		}
		registration, err := sessionworker.Claim(data, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		locator := sessionworker.Locator{Identity: identity, NodeID: nodeID, PID: os.Getpid(), Link: server.Link()}
		if err := registration.Publish(locator); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = registration.Close()
			_ = server.Close()
		})
		client, err := sessionworker.NewClient(server.Link())
		if err != nil {
			t.Fatal(err)
		}
		return client, identity
	}

	known := &Node{ID: "known", Title: "Known", Agent: "pi", Transport: "acp"}
	endedMismatch := &Node{ID: "ended-mismatch", Title: "Ended mismatch", Agent: "pi", Transport: "acp", EndedAt: "2026-09-11T21:00:00Z"}
	endedStopFailure := &Node{ID: "ended-stop-failure", Title: "Ended stop failure", Agent: "pi", Transport: "acp", EndedAt: "2026-09-11T21:00:00Z"}
	a.nodes = append(a.nodes, known, endedMismatch, endedStopFailure)
	a.byID[known.ID] = known
	a.byID[endedMismatch.ID] = endedMismatch
	a.byID[endedStopFailure.ID] = endedStopFailure
	knownClient, _ := register(known.ID, known.Agent, &syntheticSessionHarness{})
	defer knownClient.Close()
	register(endedMismatch.ID, "grok", &syntheticSessionHarness{})
	register(endedStopFailure.ID, endedStopFailure.Agent, &syntheticSessionHarness{stopErr: errors.New("provider refused stop")})
	managedClient, managedIdentity := register("managed", "opencode", &syntheticSessionHarness{})
	m.entries["managed"] = &workerEntry{client: managedClient, identity: managedIdentity}
	register("incomplete", "grok", &syntheticSessionHarness{state: sessionworker.State{HasSession: true, Live: "quiet"}})

	control := filepath.Join(data, "control", "workers")
	if err := os.WriteFile(filepath.Join(control, "malformed.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	unreachable, err := sessionworker.Claim(data, "unreachable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unreachable.Close() })
	if err := unreachable.Publish(sessionworker.Locator{
		Identity: sessionworker.Identity{WorkerID: "worker-unreachable", Agent: "pi"},
		NodeID:   "unreachable", PID: os.Getpid(),
		Link: sessionworker.Link{Socket: filepath.Join(data, "absent.sock"), Token: "missing"},
	}); err != nil {
		t.Fatal(err)
	}

	createdAt := "2026-09-11T21:00:00Z"
	for _, tc := range []struct {
		id, agent, wantTransport string
		hook                     bool
	}{
		{id: "recover-pi", agent: "pi", wantTransport: "acp"},
		{id: "recover-codex", agent: "codex", wantTransport: "codex"},
		{id: "recover-claude", agent: "claude", wantTransport: "tmux", hook: true},
	} {
		launch := sessionworker.LaunchRequest{NodeID: tc.id, Agent: tc.agent, Title: tc.id, Prompt: "first", Dir: a.home, CreatedAt: createdAt}
		state := sessionworker.State{Launch: &launch, HasSession: true, SessionID: "session-" + tc.id, Live: "quiet"}
		if tc.hook {
			state.HookID, state.HookGeneration = "hook-recovered", 3
		}
		register(tc.id, tc.agent, &syntheticSessionHarness{launched: true, state: state})
	}

	if err := m.RecoverUnknown(a); err == nil {
		t.Fatal("mixed recovery hid malformed and unreachable locators")
	}
	if m.manages("incomplete") || m.manages("unreachable") || m.manages(endedMismatch.ID) {
		t.Fatal("invalid worker became routable")
	}
	if !m.manages(endedStopFailure.ID) {
		t.Fatal("failed durable stop forgot the still-live worker")
	}
	if !m.manages("managed") {
		t.Fatal("already managed worker was disturbed")
	}
	for _, tc := range []struct{ id, transport string }{
		{"recover-pi", "acp"}, {"recover-codex", "codex"}, {"recover-claude", "tmux"},
	} {
		if node := a.byID[tc.id]; node == nil || node.Transport != tc.transport || node.Description != "first" {
			t.Fatalf("recovered %s = %#v", tc.id, node)
		}
	}
	if a.claudeHookID("recover-claude") != "hook-recovered" || a.claudeGeneration("recover-claude") != 3 {
		t.Fatal("Claude recovery did not project its hook identity")
	}
}

func TestWorkerManagerShutdownPreservesClaudeSessionOnly(t *testing.T) {
	m := &workerManager{entries: map[string]*workerEntry{}, dataDir: t.TempDir()}
	type observed struct {
		harness   *syntheticSessionHarness
		terminate bool
	}
	var cases []observed
	for _, tc := range []struct {
		id, agent string
		terminate bool
	}{{"claude-chat", "claude", false}, {"acp-chat", "opencode", true}} {
		harness := &syntheticSessionHarness{}
		identity := sessionworker.Identity{WorkerID: "worker-" + tc.id, Agent: tc.agent}
		server, err := sessionworker.Listen("", identity, harness)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		client, err := sessionworker.NewClient(server.Link())
		if err != nil {
			t.Fatal(err)
		}
		m.entries[tc.id] = &workerEntry{client: client, identity: identity}
		cases = append(cases, observed{harness: harness, terminate: tc.terminate})
	}

	m.Shutdown()
	for _, tc := range cases {
		tc.harness.mu.Lock()
		stopped, terminate := tc.harness.stop, tc.harness.terminate
		tc.harness.mu.Unlock()
		if !stopped || terminate != tc.terminate {
			t.Fatalf("stop = %v terminate=%v, want true/%v", stopped, terminate, tc.terminate)
		}
	}
}

func TestAppRoutesStructuredSessionEventsToWorker(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	node := seedStructuredNode(t, a, "worker-writer", "opencode", "acp")
	harness := &syntheticSessionHarness{}
	server, err := sessionworker.Listen("", sessionworker.Identity{WorkerID: "writer", Agent: "opencode"}, harness)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := sessionworker.NewClient(server.Link())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	a.workers = &workerManager{entries: map[string]*workerEntry{node.ID: {client: client}}, dataDir: filepath.Dir(a.storePath)}
	event := sessionlog.NewAttentionEdge("approval", "start")
	if err := a.appendSessionEvent(node.ID, event); err != nil {
		t.Fatal(err)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if len(harness.events) != 1 || !reflect.DeepEqual(harness.events[0].Attention, event.Attention) {
		t.Fatalf("worker events = %#v", harness.events)
	}
}

func TestWorkerBackedPollPersistsStructuredAttentionEdges(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "worker-attention", "opencode", "acp")
	manager := &stubProc{live: "active", hasSession: true, hasPending: true, pending: allowPending("worker:1")}
	harness := newOneSessionHarness(n.ID, manager)
	harness.logw = &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	server, err := sessionworker.Listen("", sessionworker.Identity{WorkerID: "attention", Agent: n.Agent}, harness)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := sessionworker.NewClient(server.Link())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	a.workers = newWorkerManager("", filepath.Dir(a.storePath), "test")
	a.workers.entries[n.ID] = &workerEntry{client: client}

	a.poll()
	manager.hasPending = false
	a.workers.invalidate(n.ID)
	a.poll()

	var edges []sessionlog.AttentionEvent
	for _, event := range sessionlog.ReadEvents(a.sessionLogPath(n.ID)) {
		if event.T == "attention" && event.Attention != nil {
			edges = append(edges, *event.Attention)
		}
	}
	want := []sessionlog.AttentionEvent{{Kind: "approval", Status: "start"}, {Kind: "approval", Status: "end"}}
	if !reflect.DeepEqual(edges, want) {
		t.Fatalf("durable attention edges = %#v, want %#v", edges, want)
	}
}

func TestCreateClaudeWorkerPersistsWorkerOwnedHookIdentity(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	dataDir := filepath.Dir(a.storePath)
	m := syntheticWorkerManager(t, dataDir)
	m.home, m.socket = a.home, "synthetic-claude"
	a.workers = m
	t.Cleanup(m.Shutdown)

	node := &Node{Title: "worker hook", Agent: "claude", Dir: a.home, Prompt: "hello"}
	if status, delivery, err := a.createNode(node, nil); err != nil || status != 0 || delivery != initialPending {
		t.Fatalf("createNode = %d %q %v", status, delivery, err)
	}
	if got := a.claudeHookID(node.ID); got != "hook-synthetic" {
		t.Fatalf("projected hook id = %q", got)
	}
	if got := a.claudeGeneration(node.ID); got != 3 {
		t.Fatalf("projected hook generation = %d", got)
	}
	records := keyRecords(t, a.storePath)
	last := records[len(records)-1]
	if last.Type != "claude-hook" || last.ID != node.ID || last.HookID != "hook-synthetic" || last.Generation != 3 {
		t.Fatalf("last durable record = %#v", last)
	}
}

func TestWorkerManagerConflictAndShutdown(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	m := syntheticWorkerManager(t, data)
	if _, err := m.Launch("one", "pi", data, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Launch("one", "pi", data, "", ""); !m.Conflict(err) {
		t.Fatalf("duplicate Launch = %v, want conflict", err)
	}
	for name, err := range map[string]error{
		"send": m.Send("missing", "x"), "clear": m.Clear("missing"),
		"interrupt": m.Interrupt("missing"), "deliver": m.Deliver("missing", "x"),
	} {
		if !m.Conflict(err) {
			t.Errorf("%s missing error = %v, want conflict", name, err)
		}
	}
	if _, _, err := m.PrepareResolve("missing", "r", "1"); !m.Conflict(err) {
		t.Fatalf("prepare missing = %v, want conflict", err)
	}
	if err := m.RecordStartFailure("missing", errors.New("x")); !m.Conflict(err) {
		t.Fatalf("record missing = %v, want conflict", err)
	}
	if m.HasSession("missing") || m.Live("missing") != "exited" || m.Attention("missing") != "" || m.LastError("missing") != "" {
		t.Fatal("missing worker did not degrade to exited neutral state")
	}
	if m.SessionID("missing") != "" {
		t.Fatal("missing worker returned a session id")
	}
	if _, _, ok := m.PermissionBoundary("missing"); ok {
		t.Fatal("missing worker returned a permission boundary")
	}
	if got := m.Peek("missing"); got != "" {
		t.Fatalf("missing Peek = %q", got)
	}
	m.Shutdown()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := sessionworker.Discover(data, "one")
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Shutdown left worker registered: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWorkerManagerReconcileRejectsUntrustedRegistrations(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	m := newWorkerManager("unused", data, "new")
	if err := m.Reconcile([]*Node{nil, {ID: "ended", Agent: "pi", Transport: "acp", EndedAt: "now"}, {ID: "tmux", Agent: "claude"}}); err != nil {
		t.Fatal(err)
	}

	control := filepath.Join(data, "control", "workers")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile([]*Node{{ID: "broken", Agent: "pi", Transport: "acp"}}); err == nil {
		t.Fatal("Reconcile accepted malformed locator")
	}

	reg, err := sessionworker.Claim(data, "dead")
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	dead := sessionworker.Locator{Identity: sessionworker.Identity{WorkerID: "dead", Agent: "pi"}, NodeID: "dead", PID: os.Getpid(), Link: sessionworker.Link{Socket: filepath.Join(data, "absent.sock"), Token: "token"}}
	if err := reg.Publish(dead); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile([]*Node{{ID: "dead", Agent: "pi", Transport: "acp"}}); err == nil {
		t.Fatal("Reconcile accepted unreachable worker")
	}

	for _, mismatch := range []struct {
		name        string
		serverAgent string
		locatorID   string
		nodeAgent   string
	}{
		{name: "identity", serverAgent: "pi", locatorID: "other-id", nodeAgent: "pi"},
		{name: "agent", serverAgent: "pi", locatorID: "server-id", nodeAgent: "opencode"},
	} {
		t.Run(mismatch.name, func(t *testing.T) {
			harness := &syntheticSessionHarness{}
			identity := sessionworker.Identity{WorkerID: "server-id", Agent: mismatch.serverAgent, Build: "old"}
			server, err := sessionworker.Listen("", identity, harness)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			reg, err := sessionworker.Claim(data, mismatch.name)
			if err != nil {
				t.Fatal(err)
			}
			defer reg.Close()
			locator := sessionworker.Locator{
				Identity: sessionworker.Identity{WorkerID: mismatch.locatorID, Agent: mismatch.serverAgent, Build: "old"},
				NodeID:   mismatch.name, PID: os.Getpid(), Link: server.Link(),
			}
			if err := reg.Publish(locator); err != nil {
				t.Fatal(err)
			}
			node := &Node{ID: mismatch.name, Agent: mismatch.nodeAgent, Transport: "acp"}
			if err := m.Reconcile([]*Node{node}); err == nil {
				t.Fatal("Reconcile accepted mismatched worker")
			}
		})
	}
}

func TestWorkerManagerReportsDisconnectedWorker(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	m := syntheticWorkerManager(t, data)
	if _, err := m.Launch("gone", "pi", data, "", ""); err != nil {
		t.Fatal(err)
	}
	entry := m.entry("gone")
	if err := entry.client.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing idle connections is not enough to make a Unix endpoint fail;
	// stop the worker directly while leaving the muxer's registry entry stale.
	if err := entry.client.Stop(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if entry.process != nil {
		_ = entry.process.waitForExit(3 * time.Second)
	}
	entry.observedAt = time.Time{}
	if got := m.Live("gone"); got != "exited" {
		t.Fatalf("disconnected Live = %q", got)
	}
	if got := m.Peek("gone"); got != "" {
		t.Fatalf("reaped worker manager Peek = %q, want no RPC result", got)
	}
	m.Detach()
}

func TestWorkerManagerReapsOnlyProvablyStaleLocator(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	m := syntheticWorkerManager(t, data)
	if _, err := m.Launch("stale", "pi", data, "", ""); err != nil {
		t.Fatal(err)
	}
	entry := m.entry("stale")
	realClient := entry.client
	t.Cleanup(func() { _ = realClient.Close() })
	unreachable, err := sessionworker.NewClient(sessionworker.Link{
		Socket: filepath.Join(data, "missing.sock"), Token: "missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	entry.client = unreachable

	if got := m.Live("stale"); got != "unavailable" {
		t.Fatalf("unreachable Live = %q, want unavailable", got)
	}
	if _, err := sessionworker.Discover(data, "stale"); err != nil {
		t.Fatalf("live worker locator was reaped: %v", err)
	}

	process, err := os.FindProcess(entry.process.pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = entry.process.waitForExit(3 * time.Second)
	entry.observedAt = time.Time{}
	if got := m.Live("stale"); got != "exited" {
		t.Fatalf("dead worker Live = %q", got)
	}
	if _, err := sessionworker.Discover(data, "stale"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead worker locator remains: %v", err)
	}
	m.Detach()
}

func startUnwaitedSyntheticWorker(t *testing.T, nodeID string) (string, int, *sessionworker.Client) {
	t.Helper()
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	configR, configW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer configW.Close()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	cmd := exec.Command(exe, "-test.run=^TestSessionWorkerHelperProcess$")
	cmd.ExtraFiles = []*os.File{configR, readyW}
	cmd.Env = append(os.Environ(), "SCIMUX_SESSION_WORKER_TEST_HELPER=1", workerConfigFDEnv+"=3", workerReadyFDEnv+"=4")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var client *sessionworker.Client
	workerSocketDir := ""
	t.Cleanup(func() {
		if client != nil {
			_ = client.Close()
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if workerSocketDir != "" {
			if err := os.RemoveAll(workerSocketDir); err != nil {
				t.Errorf("remove crashed worker socket directory %s: %v", workerSocketDir, err)
			}
			if _, err := os.Stat(workerSocketDir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("crashed worker socket directory remains: %s: %v", workerSocketDir, err)
			}
		}
	})
	_ = configR.Close()
	_ = readyW.Close()
	config := sessionWorkerConfig{
		DataDir: data, Home: data, NodeID: nodeID,
		Identity: sessionworker.Identity{WorkerID: nodeID + "-reconnected", Agent: "opencode", Build: "old"},
	}
	if err := json.NewEncoder(configW).Encode(config); err != nil {
		t.Fatal(err)
	}
	_ = configW.Close()
	if err := readyR.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var locator sessionworker.Locator
	if err := json.NewDecoder(readyR).Decode(&locator); err != nil {
		t.Fatal(err)
	}
	workerSocketDir = filepath.Dir(locator.Link.Socket)
	client, err = sessionworker.NewClient(locator.Link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Launch(context.Background(), sessionworker.LaunchRequest{
		NodeID: nodeID, Agent: "opencode", Title: nodeID, Dir: data, Prompt: "ping",
	}); err != nil {
		t.Fatal(err)
	}
	return data, cmd.Process.Pid, client
}

func TestWorkerManagerReapsDeadReattachedWorker(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux waitid to observe completed exit without reaping")
	}
	for _, tc := range []struct {
		name string
		act  func(*workerManager, string) error
	}{
		{"failed Stop RPC", func(m *workerManager, id string) error { return m.Kill(id) }},
		{"periodic observation", func(m *workerManager, id string) error {
			if got := m.Live(id); got != "exited" {
				return fmt.Errorf("Live = %q, want exited", got)
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := strings.ReplaceAll(tc.name, " ", "-")
			data, pid, client := startUnwaitedSyntheticWorker(t, id)
			killUnwaitedWorkerAndAwaitZombie(t, pid)
			m := newWorkerManager("", data, "new")
			m.entries[id] = &workerEntry{client: client, pid: pid}
			if err := tc.act(m, id); err != nil {
				t.Fatal(err)
			}
			if m.manages(id) {
				t.Fatal("dead worker remained in the connection registry")
			}
			var status syscall.WaitStatus
			if waited, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
				t.Fatalf("dead reattached worker was not reaped: pid=%d err=%v", waited, err)
			}
		})
	}
}

func TestWorkerManagerRetriesReapAfterProvenDeadTimeout(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux /proc to observe a zombie without reaping it")
	}
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("cat")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	client, err := sessionworker.NewClient(sessionworker.Link{Socket: filepath.Join(data, "missing.sock"), Token: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	m := newWorkerManager("", data, "new")
	owned := &workerEntry{process: &sessionWorkerProcess{wait: make(chan struct{})}}
	if err := m.reapWorkerEntry("owned-timeout", owned, time.Now()); err == nil {
		t.Fatal("owned worker reap hid a missing exit notification")
	}
	entry := &workerEntry{client: client, pid: pid}
	m.entries["retry-reap"] = entry

	if err := m.Kill("retry-reap"); err == nil || !m.manages("retry-reap") {
		t.Fatalf("initial reap = %v, managed=%v", err, m.manages("retry-reap"))
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(status), ") Z ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retry child did not become a zombie")
		}
		time.Sleep(time.Millisecond)
	}
	if err := m.Kill("retry-reap"); err != nil || m.manages("retry-reap") {
		t.Fatalf("retry reap = %v, managed=%v", err, m.manages("retry-reap"))
	}
	var status syscall.WaitStatus
	if waited, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("retry did not reap child: pid=%d err=%v", waited, err)
	}
}

func TestDeletePreservesUnreachableOwnedWorker(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "unreachable-delete", "opencode", "acp")
	data := filepath.Dir(a.storePath)
	m := syntheticWorkerManager(t, data)
	if _, err := m.Launch(n.ID, n.Agent, a.home, "", ""); err != nil {
		t.Fatal(err)
	}
	entry := m.entry(n.ID)
	realClient := entry.client
	t.Cleanup(func() {
		entry.client = realClient
		if m.manages(n.ID) {
			_ = m.Kill(n.ID)
		}
	})
	unreachable, err := sessionworker.NewClient(sessionworker.Link{
		Socket: filepath.Join(data, "missing.sock"), Token: "missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unreachable.Close()
	entry.client = unreachable
	entry.observedAt = time.Time{}
	a.workers = m

	coreHandler, err := newCoreMux(a)
	if err != nil {
		t.Fatal(err)
	}
	core, err := backend.Listen("", coreHandler)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	handler := newTestWebGeneration(t, a, core.Link(), nil)
	rec := routeRequest(handler, http.MethodDelete, "/api/nodes/"+n.ID, "", true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE status = %d body %q, want 500", rec.Code, rec.Body.String())
	}
	if a.byID[n.ID] == nil || !m.manages(n.ID) {
		t.Fatal("failed delete forgot the node or its worker ownership")
	}
	state, err := realClient.State(context.Background())
	if err != nil || !state.HasSession {
		t.Fatalf("owned worker after failed delete = %#v, %v", state, err)
	}
	if _, err := sessionworker.Discover(data, n.ID); err != nil {
		t.Fatalf("owned worker locator after failed delete: %v", err)
	}
	if _, err := os.Stat(a.sessionLogPath(n.ID)); err != nil {
		t.Fatalf("live session log was archived: %v", err)
	}
	if archived, _ := filepath.Glob(filepath.Join(a.sessionsDir, "archive", n.ID+".*.jsonl")); len(archived) != 0 {
		t.Fatalf("failed delete archived live history: %v", archived)
	}
	records := keyRecords(t, a.storePath)
	if len(records) < 2 || records[len(records)-2].Type != "delete" || records[len(records)-1].Type != "node" {
		t.Fatalf("failed delete was not durably re-asserted: %#v", records)
	}
	entry.client = realClient
	if err := m.Kill(n.ID); err != nil || m.manages(n.ID) {
		t.Fatalf("retried worker stop = %v, managed=%v", err, m.manages(n.ID))
	}
}

func TestDeleteStopsWorkerAfterHarnessExited(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "exited-worker", "claude", "tmux")
	harness := &syntheticSessionHarness{state: sessionworker.State{Live: "exited", HasSession: false}}
	server, err := sessionworker.Listen("", sessionworker.Identity{WorkerID: "exited", Agent: n.Agent}, harness)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := sessionworker.NewClient(server.Link())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	a.workers = newWorkerManager("", filepath.Dir(a.storePath), "test")
	a.workers.entries[n.ID] = &workerEntry{client: client}

	handler, err := NewHandler(a, webFS)
	if err != nil {
		t.Fatal(err)
	}
	if rec := routeRequest(handler, http.MethodDelete, "/api/nodes/"+n.ID, "", true); rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d body %q", rec.Code, rec.Body.String())
	}
	harness.mu.Lock()
	stopped := harness.stop
	harness.mu.Unlock()
	if !stopped || a.workers.manages(n.ID) {
		t.Fatal("deleting an exited harness left its session worker registered")
	}
}

func TestWorkerManagerReconcileReapsDeadLocator(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	first := syntheticWorkerManager(t, data)
	if _, err := first.Launch("dead-at-restart", "opencode", data, "", ""); err != nil {
		t.Fatal(err)
	}
	entry := first.entry("dead-at-restart")
	first.Detach()
	process, err := os.FindProcess(entry.process.pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = entry.process.waitForExit(3 * time.Second)

	replacement := syntheticWorkerManager(t, data)
	node := &Node{ID: "dead-at-restart", Agent: "opencode", Transport: "acp"}
	if err := replacement.Reconcile([]*Node{node}); err != nil {
		t.Fatalf("Reconcile dead locator: %v", err)
	}
	if _, err := sessionworker.Discover(data, node.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead locator after Reconcile: %v", err)
	}

	// The same cleanup applies when the only durable record is a delete and
	// startup therefore encounters the locator through RecoverUnknown.
	second := syntheticWorkerManager(t, data)
	if _, err := second.Launch("dead-after-delete", "pi", data, "", ""); err != nil {
		t.Fatal(err)
	}
	deletedEntry := second.entry("dead-after-delete")
	second.Detach()
	process, err = os.FindProcess(deletedEntry.process.pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = deletedEntry.process.waitForExit(3 * time.Second)
	durable := &app{byID: map[string]*Node{}, deletedNodes: map[string]bool{"dead-after-delete": true}}
	if err := replacement.RecoverUnknown(durable); err != nil {
		t.Fatalf("RecoverUnknown dead locator: %v", err)
	}
	if _, err := sessionworker.Discover(data, "dead-after-delete"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead deleted locator after recovery: %v", err)
	}
}

func TestAppStructuredDispatchPrefersSessionWorkers(t *testing.T) {
	workers := &workerManager{}
	a := &app{workers: workers}
	if got := a.proc(&Node{Transport: "acp"}); got != workers {
		t.Fatalf("ACP proc = %T, want worker manager", got)
	}
	if got := a.proc(&Node{Transport: "codex"}); got != workers {
		t.Fatalf("Codex proc = %T, want worker manager", got)
	}
	if got := a.proc(&Node{Transport: "tmux"}); got != nil {
		t.Fatalf("tmux proc = %T, want nil until Claude worker phase", got)
	}
}

func TestWorkerManagerAdoptsOnlyLiveExistingClaudeChats(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTmux{alive: map[string]bool{"live-claude": true}}
	a := newTestApp(t, f)
	live := seedOwnedClaude(t, a, "live-claude", hookSIDOwn, "")
	dead := seedOwnedClaude(t, a, "dead-claude", hookSIDForeign, "")
	pi := &Node{ID: "pi", Agent: "pi", Transport: "acp"}
	a.nodes = append(a.nodes, pi)
	a.byID[pi.ID] = pi

	m := newWorkerManager(exe, data, "migration")
	m.home, m.socket = a.home, "synthetic-migration"
	m.startOptions = sessionWorkerStartOptions{
		args: []string{"-test.run=^TestSessionWorkerHelperProcess$"},
		env:  []string{"SCIMUX_SESSION_WORKER_TEST_HELPER=1"},
	}
	if err := m.AdoptExistingClaude(a); err != nil {
		t.Fatal(err)
	}
	if !m.manages(live.ID) || m.manages(dead.ID) || m.manages(pi.ID) {
		t.Fatalf("adopted set: live=%v dead=%v pi=%v", m.manages(live.ID), m.manages(dead.ID), m.manages(pi.ID))
	}
	m.Shutdown()
}

// ownedWorkerLockDir returns an owner-only data directory that Claim accepts.
func ownedWorkerLockDir(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	return data
}

// holdWorkerLifetimeLock takes the node's lifetime lock the way a live worker
// does. flock conflicts per open file description, so a second claim is
// refused even though both live in this process.
func holdWorkerLifetimeLock(t *testing.T, data, nodeID string) *sessionworker.Registration {
	t.Helper()
	held, err := sessionworker.Claim(data, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	return held
}

// Linux publishes a SIGKILLed process as a zombie before it runs the deferred
// final __fput that drops the process's flocks, so for about a jiffy after a
// worker is provably dead its lifetime lock is still held. A single refused
// claim therefore proves nothing, and treating it as proof of a live owner
// makes every reaping path misread a dead worker as merely unreachable.
func TestReapStaleWorkerLocatorWaitsOutADeferredLockRelease(t *testing.T) {
	data := ownedWorkerLockDir(t)
	held := holdWorkerLifetimeLock(t, data, "deferred-release")
	release := time.AfterFunc(20*time.Millisecond, func() { _ = held.Close() })
	t.Cleanup(func() { release.Stop() })
	if !reapStaleWorkerLocator(data, "deferred-release") {
		t.Fatal("reapStaleWorkerLocator gave up on a lock the kernel had not released yet")
	}
}

// The grace must not become permission to forget ownership: a lock that stays
// held is still a live owner, and the verdict must stay false and bounded.
func TestReapStaleWorkerLocatorFailsClosedWhileTheLockStaysHeld(t *testing.T) {
	data := ownedWorkerLockDir(t)
	holdWorkerLifetimeLock(t, data, "live-owner")
	start := time.Now()
	if reapStaleWorkerLocator(data, "live-owner") {
		t.Fatal("reapStaleWorkerLocator claimed a node whose owner still holds the lifetime lock")
	}
	if elapsed := time.Since(start); elapsed < staleWorkerLockGrace {
		t.Fatalf("gave up after %s; the grace is %s", elapsed, staleWorkerLockGrace)
	} else if elapsed > 5*time.Second {
		t.Fatalf("fail-closed verdict took %s; the grace budget must stay bounded", elapsed)
	}
}

// An error that is not a refused claim describes the directory or the node id,
// never a lock the kernel is about to release, so it must fail immediately
// rather than burn the grace.
func TestReapStaleWorkerLocatorRejectsAnUnusableDataDirectoryAtOnce(t *testing.T) {
	start := time.Now()
	if reapStaleWorkerLocator(filepath.Join(t.TempDir(), "absent"), "no-such-node") {
		t.Fatal("reapStaleWorkerLocator claimed a node under a data directory it cannot use")
	}
	if elapsed := time.Since(start); elapsed >= staleWorkerLockGrace {
		t.Fatalf("permanent error waited %s; only a refused claim may spend the grace", elapsed)
	}
}

// The budget has to outlast a deferred release on a loaded host without
// stalling deletion of a worker that really is unreachable.
func TestStaleWorkerLockGraceStaysWithinItsBudget(t *testing.T) {
	if staleWorkerLockGrace < 100*time.Millisecond {
		t.Fatalf("grace %s is too short to outlast a deferred __fput", staleWorkerLockGrace)
	}
	if staleWorkerLockGrace > time.Second {
		t.Fatalf("grace %s would stall every path that reaps an unreachable worker", staleWorkerLockGrace)
	}
}
