package app

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/sessionworker"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

func TestProductionSessionHarnessUsesOnlyCurrentStructuredTransports(t *testing.T) {
	dataDir := t.TempDir()
	for _, agent := range []string{"pi", "opencode", "grok", "cursor", "dsh", "codex", "muse"} {
		t.Run(agent, func(t *testing.T) {
			config := sessionWorkerConfig{
				DataDir:  dataDir,
				NodeID:   "node-" + agent,
				Identity: sessionworker.Identity{WorkerID: "worker-" + agent, Agent: agent, Build: "test"},
			}
			harness, err := newProductionSessionHarness(config)
			if err != nil {
				t.Fatal(err)
			}
			one, ok := harness.(*oneSessionHarness)
			if !ok || one.nodeID != config.NodeID || one.manager == nil {
				t.Fatalf("harness = %#v", harness)
			}
			if agent == "pi" && (one.afterLaunch == nil || one.beforeState == nil) {
				t.Fatal("pi worker does not own native-fare projection")
			}
			if agent != "pi" && (one.afterLaunch != nil || one.beforeState != nil) {
				t.Fatal("non-pi worker acquired pi native-fare projection")
			}
		})
	}

	config := sessionWorkerConfig{
		DataDir: dataDir, Home: dataDir, Socket: "worker-claude-test", NodeID: "node-claude",
		Identity: sessionworker.Identity{WorkerID: "worker-claude", Agent: "claude", Build: "test"},
	}
	harness, err := newProductionSessionHarness(config)
	if err != nil {
		t.Fatal(err)
	}
	claude, ok := harness.(*claudeSessionHarness)
	if !ok || claude.nodeID != config.NodeID || claude.app == nil {
		t.Fatalf("Claude harness = %#v", harness)
	}
	t.Cleanup(func() { _ = claude.Stop(context.Background(), true) })
	if _, err := newProductionSessionHarness(sessionWorkerConfig{}); err == nil {
		t.Fatal("structured worker factory accepted invalid configuration")
	}
	if _, err := newProductionSessionHarness(sessionWorkerConfig{
		DataDir: dataDir, NodeID: "node-unknown",
		Identity: sessionworker.Identity{WorkerID: "worker-unknown", Agent: "unknown"},
	}); err == nil {
		t.Fatal("structured worker factory accepted an unsupported agent")
	}
	if matches, err := filepath.Glob(filepath.Join(dataDir, "sessions", "*.jsonl")); err != nil || len(matches) != 0 {
		t.Fatalf("construction wrote session logs: %v, %v", matches, err)
	}
}

type contractProc struct {
	stubProc
	launchArgs []string
	killed     string
	recorded   error
	peek       string
}

type sessionIDProc struct{ *contractProc }

func (*sessionIDProc) SessionID(string) string { return "provider-session" }

type museMismatchProc struct{ *contractProc }

func (*museMismatchProc) FingerprintMismatch(string) bool { return true }

func (p *contractProc) Launch(nodeID, agent, dir, model, effort string) (string, error) {
	p.launchArgs = []string{nodeID, agent, dir, model, effort}
	p.hasSession = true
	return "session-x", nil
}

func (p *contractProc) Clear(string) error     { return p.sendErr }
func (p *contractProc) Interrupt(string) error { return p.deliverErr }
func (p *contractProc) Peek(string) string     { return p.peek }
func (p *contractProc) Kill(id string) error   { p.killed = id; return nil }
func (p *contractProc) RecordStartFailure(_ string, err error) error {
	p.recorded = err
	return nil
}
func (p *contractProc) Turns(string) []transcript.Turn { return nil }

func TestOneSessionHarnessAdaptsEveryCurrentManagerOperation(t *testing.T) {
	p := &contractProc{peek: "raw events"}
	p.conflictErrors = []error{errStubStale}
	p.live = "active"
	p.lastError = "agent error"
	p.hasPending = true
	p.pending = allowPending("inc:4")
	p.boundarySet, p.boundaryIncarn, p.boundaryMaxSeq = true, "inc", 4
	h := newOneSessionHarness("node-a", p)
	ctx := context.Background()

	sid, err := h.Launch(ctx, sessionworker.LaunchRequest{NodeID: "node-a", Agent: "opencode", Dir: "/work", Model: "m", Effort: "low"})
	if err != nil || sid != "session-x" || !reflect.DeepEqual(p.launchArgs, []string{"node-a", "opencode", "/work", "m", "low"}) {
		t.Fatalf("Launch = %q, %v args=%v", sid, err, p.launchArgs)
	}
	if delivery, err := h.Send(ctx, "hello"); err != nil || delivery.Status != sessionworker.DeliveryAcknowledged || p.sendCalls != 1 {
		t.Fatalf("Send = %#v, %v calls=%d", delivery, err, p.sendCalls)
	}
	if delivery, err := h.Clear(ctx); err != nil || delivery.Status != sessionworker.DeliveryAcknowledged {
		t.Fatalf("Clear = %#v, %v", delivery, err)
	}
	if err := h.ResolveDelivery(ctx); err != nil {
		t.Fatal(err)
	}
	if evidence, err := h.Interrupt(ctx); err != nil || len(evidence.Keys) != 0 {
		t.Fatal(err)
	}
	prepared, err := h.PreparePermission(ctx, sessionworker.PermissionDecision{RequestID: "inc:4", Key: "1"})
	if err != nil || prepared.Token != "tok-1" || prepared.Evidence != "permission: go test ./..." {
		t.Fatalf("PreparePermission = %#v, %v", prepared, err)
	}
	if err := h.DeliverPermission(ctx, prepared.Token); err != nil || p.lastDeliver != "tok-1" {
		t.Fatalf("DeliverPermission = %v token=%q", err, p.lastDeliver)
	}
	state := h.State(ctx)
	if !state.HasSession || state.Live != "active" || state.Attention != "approval" || state.LastError != "agent error" ||
		state.Permission == nil || state.Permission.RequestID != "inc:4" || state.PermissionBoundary == nil || state.PermissionBoundary.MaxSequence != 4 {
		t.Fatalf("State = %#v", state)
	}
	if got := h.Peek(ctx, "visible"); got != "raw events" {
		t.Fatalf("Peek = %q", got)
	}
	if got, err := h.SetAutoApprove(ctx, true); err != nil || got.Supported {
		t.Fatalf("SetAutoApprove = %#v, %v", got, err)
	}
	if err := h.RecordStartFailure(ctx, "not delivered"); err != nil || p.recorded == nil || p.recorded.Error() != "not delivered" {
		t.Fatalf("RecordStartFailure = %v recorded=%v", err, p.recorded)
	}
	if err := h.Stop(ctx, true); err != nil || p.killed != "node-a" {
		t.Fatalf("Stop = %v killed=%q", err, p.killed)
	}
	if !h.Conflict(errStubStale) || h.Conflict(errors.New("other")) {
		t.Fatal("Conflict did not preserve manager classification")
	}
}

func TestOneSessionHarnessEmptyStateHasNoInventedPermission(t *testing.T) {
	h := newOneSessionHarness("node", &contractProc{})
	state := h.State(context.Background())
	if state.Permission != nil || state.PermissionBoundary != nil || state.HasSession {
		t.Fatalf("empty state = %#v", state)
	}
}

func TestOneSessionHarnessUsesManagerSessionIdentityWhenAvailable(t *testing.T) {
	h := newOneSessionHarness("node", &sessionIDProc{contractProc: &contractProc{}})
	if got := h.State(context.Background()).SessionID; got != "provider-session" {
		t.Fatalf("SessionID = %q", got)
	}
}

func TestOneSessionHarnessProjectsMuseSchemaMismatch(t *testing.T) {
	h := newOneSessionHarness("node", &museMismatchProc{contractProc: &contractProc{}})
	if state := h.State(context.Background()); !state.MuseSchemaMismatch {
		t.Fatalf("Muse schema mismatch was lost at worker boundary: %#v", state)
	}
}

func TestOneSessionHarnessOwnsTurnCompletionAcrossMuxerClients(t *testing.T) {
	p := &contractProc{}
	p.live = "quiet"
	h := newOneSessionHarness("node", p)
	ctx := context.Background()
	if _, err := h.Launch(ctx, sessionworker.LaunchRequest{NodeID: "node", Agent: "opencode", Dir: "/work"}); err != nil {
		t.Fatal(err)
	}
	p.sendHook = func(p *stubProc) { p.live = "active" }
	if _, err := h.Send(ctx, "ping"); err != nil {
		t.Fatal(err)
	}
	if state := h.State(ctx); state.Live != "active" || state.TurnDone {
		t.Fatalf("active state = %#v", state)
	}

	p.live = "quiet"
	if state := h.State(ctx); !state.TurnDone {
		t.Fatalf("first quiet state = %#v", state)
	}
	// State is the complete reconnect contract. A replacement muxer has no
	// local active→quiet history, so the worker must retain the bounded latch.
	if state := h.State(ctx); !state.TurnDone {
		t.Fatalf("reattached quiet state = %#v", state)
	}
	if _, err := h.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if state := h.State(ctx); state.TurnDone {
		t.Fatalf("state after clear = %#v", state)
	}
}

func TestOneSessionHarnessOwnsLaunchAndStateSideEffects(t *testing.T) {
	p := &contractProc{}
	h := newOneSessionHarness("node", p)
	var launched sessionworker.LaunchRequest
	var launchedID string
	states := 0
	h.afterLaunch = func(req sessionworker.LaunchRequest, sid string) { launched, launchedID = req, sid }
	h.beforeState = func() { states++ }
	req := sessionworker.LaunchRequest{NodeID: "node", Agent: "pi", Dir: "/work", Model: "cheap", Effort: "low"}
	if _, err := h.Launch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	_ = h.State(context.Background())
	if !reflect.DeepEqual(launched, req) || launchedID != "session-x" || states != 1 {
		t.Fatalf("launch hook = %#v %q, state calls=%d", launched, launchedID, states)
	}
}

func TestOneSessionHarnessIsSoleTypedSessionEventWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	h := newOneSessionHarness("node", &contractProc{})
	event := sessionlog.NewAttentionEdge("approval", "start")
	if err := h.AppendSessionEvent(context.Background(), event); err == nil {
		t.Fatal("append succeeded without worker-owned writer")
	}
	h.logw = &sessionlog.Writer{Path: path}
	if err := h.AppendSessionEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	events := sessionlog.ReadEvents(path)
	if len(events) != 1 || !reflect.DeepEqual(events[0].Attention, event.Attention) {
		t.Fatalf("events = %#v", events)
	}
}

func TestOneSessionHarnessRejectsWrongLaunchIdentity(t *testing.T) {
	p := &contractProc{}
	h := newOneSessionHarness("node-a", p)
	if _, err := h.Launch(context.Background(), sessionworker.LaunchRequest{NodeID: "node-b", Agent: "pi"}); err == nil {
		t.Fatal("Launch accepted another node")
	}
}
