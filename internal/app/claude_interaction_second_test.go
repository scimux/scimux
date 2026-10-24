package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/sessionworker"
	"github.com/scimux/scimux/internal/transcript"
)

// undatedInterruptTailer is the boundary Claude records when a cancellation
// has no timestamp. Settlement must accept it; a previous assistant
// completion is a different record and is covered separately.
func undatedInterruptTailer(t *testing.T) *transcript.Tailer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "interrupt.jsonl")
	line := `{"type":"user","message":{"content":"[Request interrupted by user]"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	tl := &transcript.Tailer{Path: path}
	_ = tl.Poll()
	if !tl.EndTurn() || tl.Delivered() {
		t.Fatal("synthetic interrupt was not an interruption boundary")
	}
	return tl
}

func publishEmittedCancel(t *testing.T, bundle string, rec claudeInteractionRequest) {
	t.Helper()
	publishInteraction(t, bundle, rec)
	env, err := buildClaudeInteractionReply(mustClassify(t, rec.Tool, rec.Original), claudeInteractionReply{Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
	if err := noteClaudeInteractionEmission(bundle, rec.ID, claudeInteractionAck{Emitted: true}); err != nil {
		t.Fatal(err)
	}
}

func TestCancellationSurvivesPayloadRetirement(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rec := sampleRequest(bundle, now.Add(-2*time.Minute))
	rec.ID = "retire-cancel"
	rec.SessionID = n.SessionID
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	rec.HelperUntil = now.Add(-time.Millisecond).UTC().Format(time.RFC3339Nano)
	publishEmittedCancel(t, bundle, rec)

	// The worker poll recovers expired payloads before settlement.
	h := testClaudeSessionHarness(a, n.ID)
	h.recoverInteractions(now)
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "req", rec.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("recovery left the raw request in place")
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("recovery left the raw answer in place")
	}
	a.settleEmittedClaudeInteractionCancels(n)
	if got := a.claudeAcceptedTurnOf(n.ID).Turn; got == turn.Turn {
		t.Fatal("cleanup deleted the request and answer needed to settle an emitted cancellation; accepted turn remains locked")
	}
	if phase := claudeInteractionPhase(bundle, rec.ID, now); phase != "interrupted" {
		t.Fatalf("phase after delayed interrupt = %s", phase)
	}
	fencePath := filepath.Join(bundle, "perm", "interact", "cancel", rec.ID)
	fence, err := os.ReadFile(fencePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(fence), "question") || strings.Contains(string(fence), "hookSpecificOutput") || strings.Contains(string(fence), string(rec.Original)) {
		t.Fatalf("cancel fence kept raw payload: %s", fence)
	}
	var replay bytes.Buffer
	handled, serr := serveClaudeInteraction(bundle, claudePermissionEvent{
		SessionID: n.SessionID, ToolName: rec.Tool, ToolInput: rec.Original,
	}, rec.ID, &replay, time.Minute, time.Millisecond, func() time.Time { return now }, func(time.Duration) {})
	if serr != nil || !handled || replay.Len() != 0 {
		t.Fatalf("retired cancel replayed: handled=%v err=%v stdout=%q", handled, serr, replay.String())
	}

	// A restarted worker sweeps again, then a transcript that appears only
	// after that sweep still settles. The app process keeps the accepted turn.
	restart := newTestApp(t, &fakeTmux{})
	rn, rbundle := seedPermClaude(t, restart, "n2", hookSIDOwn)
	rturn, err := restart.beginClaudeAcceptedTurn(rn)
	if err != nil {
		t.Fatal(err)
	}
	late := sampleRequest(rbundle, now.Add(-2*time.Minute))
	late.ID = "restart-cancel"
	late.SessionID = rn.SessionID
	late.Turn = rturn.Turn
	late.TurnGen = rturn.Gen
	late.HelperUntil = now.Add(-time.Millisecond).UTC().Format(time.RFC3339Nano)
	publishInteraction(t, rbundle, late)
	env, err := buildClaudeInteractionReply(mustClassify(t, late.Tool, late.Original), claudeInteractionReply{Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	if err := exclWrite(filepath.Join(rbundle, "perm", "interact", "ans", late.ID+".json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
	first := testClaudeSessionHarness(restart, rn.ID)
	first.recoverInteractions(now)
	if _, err := os.Stat(filepath.Join(rbundle, "perm", "interact", "ans", late.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("recovery waited for the acknowledgement before deleting the answer")
	}
	if err := noteClaudeInteractionEmission(rbundle, late.ID, claudeInteractionAck{Emitted: true}); err != nil {
		t.Fatal(err)
	}
	second := testClaudeSessionHarness(restart, rn.ID)
	second.recoverInteractions(now)
	restart.settleEmittedClaudeInteractionCancels(rn)
	if got := restart.claudeAcceptedTurnOf(rn.ID).Turn; got == rturn.Turn {
		t.Fatal("late acknowledgement plus a restarted worker left the accepted turn locked")
	}
	next, err := restart.beginClaudeAcceptedTurn(rn)
	if err != nil {
		t.Fatal(err)
	}
	restart.settleEmittedClaudeInteractionCancels(rn)
	if got := restart.claudeAcceptedTurnOf(rn.ID).Turn; got != next.Turn {
		t.Fatal("retired cancellation closed the successor turn")
	}

	// An emitted allow is still retired, and it does not become a cancel fence.
	allowNode, allowBundle := seedPermClaude(t, a, "n3", hookSIDOwn)
	allowTurn, err := a.beginClaudeAcceptedTurn(allowNode)
	if err != nil {
		t.Fatal(err)
	}
	allow := sampleRequest(allowBundle, now.Add(-2*time.Minute))
	allow.ID = "retire-allow"
	allow.SessionID = allowNode.SessionID
	allow.Turn = allowTurn.Turn
	allow.TurnGen = allowTurn.Gen
	allow.HelperUntil = now.Add(-time.Millisecond).UTC().Format(time.RFC3339Nano)
	publishInteraction(t, allowBundle, allow)
	if err := exclWrite(filepath.Join(allowBundle, "perm", "interact", "ans", allow.ID+".json"), []byte(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := noteClaudeInteractionEmission(allowBundle, allow.ID, claudeInteractionAck{Emitted: true}); err != nil {
		t.Fatal(err)
	}
	testClaudeSessionHarness(a, allowNode.ID).recoverInteractions(now)
	if _, err := os.Stat(filepath.Join(allowBundle, "perm", "interact", "req", allow.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("recovery kept an expired allow request")
	}
	if _, err := os.Stat(filepath.Join(allowBundle, "perm", "interact", "cancel", allow.ID)); !os.IsNotExist(err) {
		t.Fatal("recovery retained a cancel fence for an allow")
	}
	if got := a.claudeAcceptedTurnOf(allowNode.ID).Turn; got != allowTurn.Turn {
		t.Fatal("retiring an allow closed the accepted turn")
	}
}

func TestCancellationFenceRetainsNothingWithoutATurn(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rec := sampleRequest(bundle, now.Add(-2*time.Minute))
	rec.ID = "cancel-without-turn"
	rec.SessionID = n.SessionID
	rec.Turn = ""
	rec.HelperUntil = now.Add(-time.Millisecond).UTC().Format(time.RFC3339Nano)
	publishEmittedCancel(t, bundle, rec)
	testClaudeSessionHarness(a, n.ID).recoverInteractions(now)
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "req", rec.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("a cancel with no turn kept its raw request")
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "cancel", rec.ID)); !os.IsNotExist(err) {
		t.Fatal("a cancel with no turn wrote a fence")
	}
	if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
		t.Fatal("a turn-less cancel closed the accepted turn")
	}

	settled := sampleRequest(bundle, now.Add(-2*time.Minute))
	settled.ID = "already-interrupted"
	settled.SessionID = n.SessionID
	settled.Turn = turn.Turn
	settled.TurnGen = turn.Gen
	settled.HelperUntil = now.Add(-time.Millisecond).UTC().Format(time.RFC3339Nano)
	_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
	publishEmittedCancel(t, bundle, settled)
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "interrupted", settled.ID), []byte("interrupted")); err != nil {
		t.Fatal(err)
	}
	testClaudeSessionHarness(a, n.ID).recoverInteractions(now)
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "ans", settled.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("an already interrupted cancel kept its answer")
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "cancel", settled.ID)); !os.IsNotExist(err) {
		t.Fatal("an already interrupted cancel grew a fence")
	}
}

func TestCancellationFenceWriteFailureKeepsPayload(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rec := sampleRequest(bundle, now.Add(-2*time.Minute))
	rec.ID = "fence-blocked"
	rec.SessionID = n.SessionID
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	rec.HelperUntil = now.Add(-time.Millisecond).UTC().Format(time.RFC3339Nano)
	publishEmittedCancel(t, bundle, rec)
	cancelDir := filepath.Join(bundle, "perm", "interact", "cancel")
	if err := os.RemoveAll(cancelDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cancelDir, []byte("not-a-directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	testClaudeSessionHarness(a, n.ID).recoverInteractions(now)
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "req", rec.ID+".json")); err != nil {
		t.Fatal("recovery deleted the cancel after the fence directory could not be created")
	}
	if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
		t.Fatal("a failed fence write closed the turn")
	}

	if err := os.Remove(cancelDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cancelDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cancelDir, rec.ID+".tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	testClaudeSessionHarness(a, n.ID).recoverInteractions(now)
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json")); err != nil {
		t.Fatal("recovery deleted the answer after the fence publish failed")
	}
}

func TestCorruptCancelFenceDoesNotSettle(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rec := sampleRequest(bundle, now)
	rec.ID = "corrupt-fence"
	rec.SessionID = n.SessionID
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	publishEmittedCancel(t, bundle, rec)
	if err := os.Remove(filepath.Join(bundle, "perm", "interact", "req", rec.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "cancel", rec.ID), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.settleEmittedClaudeInteractionCancels(n)
	if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
		t.Fatal("a corrupt cancel fence settled the turn")
	}
	if ok, why := a.settleClaudeInteractionCancel(n, rec.ID); ok || why != "turn mismatch" {
		t.Fatalf("corrupt fence settle = %v %s", ok, why)
	}
}

func seedEmittedCancelOnTurn(t *testing.T, a *app, n *Node, bundle string, id string) claudeAcceptedTurn {
	t.Helper()
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	rec := sampleRequest(bundle, time.Now().UTC())
	rec.ID = id
	rec.SessionID = n.SessionID
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
	publishEmittedCancel(t, bundle, rec)
	return turn
}

func pollLines(t *testing.T, body string) *transcript.Tailer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "boundary.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tl := &transcript.Tailer{Path: path}
	_ = tl.Poll()
	return tl
}

func TestPublishedIdentityUsesBindingGeneration(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.claudeGens[n.ID] = 7
	a.mu.Unlock()
	publishSyntheticInteraction(t, bundle, "real-generation", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	rec, err := readInteractionRequest(bundle, "real-generation")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Generation != 7 || rec.NodeID != n.ID {
		t.Fatalf("published generation=%d node=%q, want 7 and %s", rec.Generation, rec.NodeID, n.ID)
	}
	gen, node, ok := readInteractionGeneration(bundle)
	if !ok || gen != 7 || node != n.ID {
		t.Fatalf("helper fence = %d %q ok=%v", gen, node, ok)
	}
}

func TestWorkerProjectionPublishesBindingFence(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	hookID := a.claudeHookID(n.ID)
	// The worker publishes the helper fence. A muxer projection may record a
	// newer observation in the append-only registry, and it must not write
	// perm/generation.json.
	a.mu.Lock()
	a.claudeGens[n.ID] = 4
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	if a.claudeGeneration(n.ID) != 4 {
		t.Fatalf("worker generation = %d", a.claudeGeneration(n.ID))
	}
	gen, node, ok := readInteractionGeneration(bundle)
	if !ok || gen != 4 || node != n.ID {
		t.Fatalf("worker fence = %d %q ok=%v", gen, node, ok)
	}
	a.projectWorkerClaudeHook(n.ID, sessionworker.State{HookID: hookID, HookGeneration: 4})
	publishSyntheticInteraction(t, bundle, "projected", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	rec, err := readInteractionRequest(bundle, "projected")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Generation != 4 || rec.NodeID != n.ID {
		t.Fatalf("projected request = generation %d node %q", rec.Generation, rec.NodeID)
	}
	a.mu.Lock()
	a.claudeGens[n.ID] = 5
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	a.projectWorkerClaudeHook(n.ID, sessionworker.State{HookID: hookID, HookGeneration: 4})
	if got, _, ok := readInteractionGeneration(bundle); !ok || got != 5 {
		t.Fatalf("stale projection rewrote the worker fence to %d ok=%v", got, ok)
	}
	_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
	publishSyntheticInteraction(t, bundle, "successor-gen", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	next, err := readInteractionRequest(bundle, "successor-gen")
	if err != nil {
		t.Fatal(err)
	}
	if next.Generation != 5 || next.NodeID != n.ID {
		t.Fatalf("successor generation = %d node %q", next.Generation, next.NodeID)
	}
	env, err := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), letterReply())
	if err != nil {
		t.Fatal(err)
	}
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "ans", "projected.json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	emitted, err := emitInteractionAnswer(bundle, "projected", env.Stdout, &out, time.Now, time.Now().Add(time.Minute))
	if err != nil || !emitted || out.Len() != 0 {
		t.Fatalf("generation-4 answer emitted after rebind: emitted=%v err=%v stdout=%q", emitted, err, out.String())
	}
	a.mu.Lock()
	a.claudeGens[n.ID] = 9
	a.mu.Unlock()
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "ans", "successor-gen.json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if _, err := emitInteractionAnswer(bundle, "successor-gen", env.Stdout, &out, time.Now, time.Now().Add(time.Minute)); err != nil || out.Len() != 0 {
		t.Fatalf("answer emitted after memory generation moved ahead of the fence: err=%v stdout=%q", err, out.String())
	}
}

func TestHelperRejectsGenerationRevokedAfterApproval(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now()
	if err := writeClaudeAcceptedTurn(filepath.Join(dir, "perm"), claudeAcceptedTurn{Turn: "review-current", Gen: 1, Session: hookSIDOwn}); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "generation.json"), map[string]any{"generation": 1, "node_id": "node-a"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	steps := 0
	_, err := serveClaudeInteraction(dir, claudePermissionEvent{
		SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "rebind-after-approval", &out, time.Minute, time.Millisecond, func() time.Time { return now }, func(time.Duration) {
		steps++
		if steps > 1 {
			t.Fatal("helper failed to retire a revoked generation")
		}
		env, buildErr := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), letterReply())
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		if writeErr := exclWrite(filepath.Join(dir, "perm", "interact", "ans", "rebind-after-approval.json"), env.Stdout); writeErr != nil {
			t.Fatal(writeErr)
		}
		if writeErr := writeClaudePermFile(filepath.Join(dir, "perm", "generation.json"), map[string]any{"generation": 2, "node_id": "node-a"}); writeErr != nil {
			t.Fatal(writeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Len() > 0 {
		t.Fatal("helper emitted a generation-1 approval after the binding advanced to generation 2")
	}
}

func TestLegacyGenerationFileStaysAuthoritative(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	// A seeded node starts at generation 1. Clearing that binding leaves the
	// helper-visible file as the authority, and publication must not rewrite
	// it back to zero.
	a.mu.Lock()
	a.claudeGens[n.ID] = 0
	a.mu.Unlock()
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "generation.json"), map[string]any{"generation": 3, "node_id": n.ID}); err != nil {
		t.Fatal(err)
	}
	publishSyntheticInteraction(t, bundle, "legacy-file", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	rec, err := readInteractionRequest(bundle, "legacy-file")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Generation != 3 || rec.NodeID != n.ID {
		t.Fatalf("file-backed request = generation %d node %q", rec.Generation, rec.NodeID)
	}
	gen, _, ok := readInteractionGeneration(bundle)
	if !ok || gen != 3 {
		t.Fatalf("fence was replaced while memory generation was unset: %d ok=%v", gen, ok)
	}
}

func TestLegacyBindingDoesNotWriteGenerationFence(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	var caps claudeHookCapabilities
	if err := json.Unmarshal(mustRead(t, filepath.Join(bundle, "capabilities.json")), &caps); err != nil {
		t.Fatal(err)
	}
	caps.Interaction = 0
	if err := writeClaudePermFile(filepath.Join(bundle, "capabilities.json"), caps); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundle, "perm", "generation.json")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	hookID := a.claudeHookID(n.ID)
	a.projectWorkerClaudeHook(n.ID, sessionworker.State{HookID: hookID, HookGeneration: 2})
	if _, err := os.Stat(filepath.Join(bundle, "perm", "generation.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy bundle wrote generation.json: %v", err)
	}
}

func TestCorruptGenerationFenceBlocksEmission(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now()
	if err := writeClaudeAcceptedTurn(filepath.Join(dir, "perm"), claudeAcceptedTurn{Turn: "turn-live", Gen: 1, Session: hookSIDOwn}); err != nil {
		t.Fatal(err)
	}
	rec := sampleRequest(dir, now)
	rec.ID = "corrupt-generation"
	rec.Turn = "turn-live"
	rec.TurnGen = 1
	publishInteraction(t, dir, rec)
	if err := os.WriteFile(filepath.Join(dir, "perm", "generation.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := buildClaudeInteractionReply(mustClassify(t, rec.Tool, rec.Original), letterReply())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := emitInteractionAnswer(dir, rec.ID, env.Stdout, &out, func() time.Time { return now }, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if out.Len() > 0 {
		t.Fatal("helper emitted an answer while the binding fence was unreadable")
	}
}

func armNativeDialog(t *testing.T, n *Node, bundle string) string {
	t.Helper()
	perm := filepath.Join(bundle, "perm")
	note := claudeAskedNotice{At: time.Now().UTC().Format(time.RFC3339Nano), Session: n.SessionID, Tool: "Bash", Digest: "native-fence"}
	if err := writeClaudePermFile(claudeAskedPath(perm, "native-fence"), note); err != nil {
		t.Fatal(err)
	}
	epoch, err := mintClaudeVisibleEpoch(perm, n.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return epoch.Epoch
}

func prepareNativeKey(t *testing.T, h *claudeSessionHarness, epoch string) sessionworker.PreparedPermission {
	t.Helper()
	prepared, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch, Key: "1"})
	if err != nil || prepared.Token == "" {
		t.Fatalf("prepare = %#v %v", prepared, err)
	}
	return prepared
}

func TestNativeDeliveryRefusesStructuredOwnership(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: axPermissionPane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	n.AXScreenReader = true
	h := testClaudeSessionHarness(a, n.ID)
	epoch := armNativeDialog(t, n, bundle)
	prepared := prepareNativeKey(t, h, epoch)
	competing := prepareNativeKey(t, h, epoch)

	prev := claudeNativeDeliveryPause
	t.Cleanup(func() { claudeNativeDeliveryPause = prev })
	started := make(chan struct{})
	release := make(chan struct{})
	claudeNativeDeliveryPause = func(*claudeSessionHarness) {
		close(started)
		<-release
	}
	done := make(chan error, 1)
	go func() {
		done <- h.DeliverPermission(context.Background(), prepared.Token)
	}()
	<-started
	publishSyntheticInteraction(t, bundle, "structured-owner", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	if _, err := h.PreparePermission(context.Background(), sessionworker.PermissionDecision{RequestID: epoch, Key: "1"}); err == nil {
		t.Fatal("a competing native prepare was accepted after structured ownership")
	}
	close(release)
	if err := <-done; err == nil || containsSub(f.subcommands(), "send-keys") {
		t.Fatalf("native keys delivered after structured ownership was published: err=%v", err)
	}
	if err := h.DeliverPermission(context.Background(), competing.Token); err == nil || containsSub(f.subcommands(), "send-keys") {
		t.Fatalf("competing native delivery sent keys: err=%v", err)
	}

	// Terminal handoff returns native authority. Delivery that already passed
	// the initial guard observes the handoff before it sends the key.
	if err := os.Remove(filepath.Join(bundle, "perm", "interact", "active.json")); err != nil {
		t.Fatal(err)
	}
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "closed", "structured-owner"), []byte("closed")); err != nil {
		t.Fatal(err)
	}
	handoff := prepareNativeKey(t, h, epoch)
	started = make(chan struct{})
	release = make(chan struct{})
	claudeNativeDeliveryPause = func(*claudeSessionHarness) {
		close(started)
		<-release
	}
	done = make(chan error, 1)
	go func() {
		done <- h.DeliverPermission(context.Background(), handoff.Token)
	}()
	<-started
	publishSyntheticInteraction(t, bundle, "handoff-owner", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	if err := os.Remove(filepath.Join(bundle, "perm", "interact", "active.json")); err != nil {
		t.Fatal(err)
	}
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "closed", "handoff-owner"), []byte("closed")); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil || !containsSub(f.subcommands(), "send-keys") {
		t.Fatalf("terminal handoff did not send the prepared key: %v", err)
	}
}

func TestNativeDeliveryRefusesWhenFenceCannotBeLocked(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: axPermissionPane}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	n.AXScreenReader = true
	h := testClaudeSessionHarness(a, n.ID)
	epoch := armNativeDialog(t, n, bundle)
	prepared := prepareNativeKey(t, h, epoch)
	interact := filepath.Join(bundle, "perm", "interact")
	if err := os.RemoveAll(interact); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(interact, []byte("not-a-directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.DeliverPermission(context.Background(), prepared.Token); err == nil || containsSub(f.subcommands(), "send-keys") {
		t.Fatalf("native keys were sent without the interaction fence: %v", err)
	}
}

func TestEmissionRejectsBindingNodeMismatch(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now()
	if err := writeClaudeAcceptedTurn(filepath.Join(dir, "perm"), claudeAcceptedTurn{Turn: "turn-live", Gen: 1, Session: hookSIDOwn}); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "generation.json"), map[string]any{"generation": 4, "node_id": "node-a"}); err != nil {
		t.Fatal(err)
	}
	rec := sampleRequest(dir, now)
	rec.ID = "node-mismatch"
	rec.Turn = "turn-live"
	rec.TurnGen = 1
	rec.Generation = 4
	rec.NodeID = "node-a"
	publishInteraction(t, dir, rec)
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "generation.json"), map[string]any{"generation": 4, "node_id": "node-b"}); err != nil {
		t.Fatal(err)
	}
	env, err := buildClaudeInteractionReply(mustClassify(t, rec.Tool, rec.Original), letterReply())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := emitInteractionAnswer(dir, rec.ID, env.Stdout, &out, func() time.Time { return now }, now.Add(time.Minute)); err != nil || out.Len() != 0 {
		t.Fatalf("emitted for a different binding node: err=%v stdout=%q", err, out.String())
	}
}

func TestPublishRefusesBrokenInteractionFence(t *testing.T) {
	dir := interactionBundle(t)
	interact := filepath.Join(dir, "perm", "interact")
	if err := os.RemoveAll(interact); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(interact, []byte("not-a-directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := publishInteractionRequest(dir, claudePermissionEvent{SessionID: hookSIDOwn, ToolName: "AskUserQuestion"}, "req-fence", mustClassify(t, "AskUserQuestion", letterQuestionRaw()), time.Now(), time.Minute)
	if err == nil {
		t.Fatal("published a request without an interaction fence")
	}
}

func TestUnusableBundlePathDoesNotRetainCancelFence(t *testing.T) {
	err := retainInteractionCancelFence("relative-bundle", claudeInteractionRequest{ID: "keep", Turn: "turn-1"})
	if !errors.Is(err, errInteractionBadRequest) {
		t.Fatalf("unusable bundle path = %v, want a bad-request error so the payload stays", err)
	}
}

func TestCancelFenceAlreadyPresentIsNotRewritten(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rec := sampleRequest(bundle, now.Add(-2*time.Minute))
	rec.ID = "fence-kept"
	rec.SessionID = n.SessionID
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	rec.HelperUntil = now.Add(-time.Millisecond).UTC().Format(time.RFC3339Nano)
	publishEmittedCancel(t, bundle, rec)
	fencePath := filepath.Join(bundle, "perm", "interact", "cancel", rec.ID)
	kept := []byte(`{"action":"cancel","turn":"` + turn.Turn + `","turn_gen":` + strconv.Itoa(turn.Gen) + `,"kept":true}`)
	if err := os.MkdirAll(filepath.Dir(fencePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fencePath, kept, 0o600); err != nil {
		t.Fatal(err)
	}
	testClaudeSessionHarness(a, n.ID).recoverInteractions(now)
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "req", rec.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("an existing cancel fence left the raw request in place")
	}
	got, err := os.ReadFile(fencePath)
	if err != nil || string(got) != string(kept) {
		t.Fatalf("existing fence rewritten: %s %v", got, err)
	}
	a.settleEmittedClaudeInteractionCancels(n)
	if a.claudeAcceptedTurnOf(n.ID).Turn == turn.Turn {
		t.Fatal("a pre-existing cancel fence did not settle after the payload was retired")
	}
}

func TestReservedRequestIDKeepsCancelPayload(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rec := sampleRequest(bundle, now.Add(-2*time.Minute))
	rec.ID = "archive"
	rec.SessionID = n.SessionID
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	rec.HelperUntil = now.Add(-time.Millisecond).UTC().Format(time.RFC3339Nano)
	publishInteraction(t, bundle, rec)
	env, err := buildClaudeInteractionReply(mustClassify(t, rec.Tool, rec.Original), claudeInteractionReply{Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "ack", rec.ID+".json"), []byte(`{"emitted":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	testClaudeSessionHarness(a, n.ID).recoverInteractions(now)
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "req", rec.ID+".json")); err != nil {
		t.Fatal("recovery deleted a cancel whose fence path is not a safe component")
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json")); err != nil {
		t.Fatal("recovery deleted the answer when the cancel fence could not be named")
	}
	if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
		t.Fatal("an unnameable cancel fence closed the accepted turn")
	}
}

func TestUnusableCancelFenceDoesNotSettle(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	allow := sampleRequest(bundle, now)
	allow.ID = "fence-allow"
	allow.SessionID = n.SessionID
	allow.Turn = turn.Turn
	allow.TurnGen = turn.Gen
	publishEmittedCancel(t, bundle, allow)
	if err := os.Remove(filepath.Join(bundle, "perm", "interact", "req", allow.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundle, "perm", "interact", "ans", allow.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "cancel", allow.ID), []byte(`{"action":"allow","turn":"`+turn.Turn+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	blank := sampleRequest(bundle, now)
	blank.ID = "fence-blank-turn"
	blank.SessionID = n.SessionID
	blank.Turn = turn.Turn
	blank.TurnGen = turn.Gen
	_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
	publishEmittedCancel(t, bundle, blank)
	if err := os.Remove(filepath.Join(bundle, "perm", "interact", "req", blank.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundle, "perm", "interact", "ans", blank.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "cancel", blank.ID), []byte(`{"action":"cancel","turn":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ack := []byte(`{"emitted":true}`)
	if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "ack", "archive.json"), ack, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range pendingCancelInteractions(bundle) {
		if id == "archive" || id == allow.ID || id == blank.ID {
			t.Fatalf("pending cancellation included %s", id)
		}
	}
	a.settleEmittedClaudeInteractionCancels(n)
	if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
		t.Fatal("an allow fence, a blank turn, or an unnameable ack settled the cancellation")
	}
}

func TestLiveBindingAuthorizesSubmitAfterFileLoss(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.claudeGens[n.ID] = 7
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	publishSyntheticInteraction(t, bundle, "live-gen", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	fence := filepath.Join(bundle, "perm", "generation.json")
	if err := os.Remove(fence); err != nil {
		t.Fatal(err)
	}
	h := testClaudeSessionHarness(a, n.ID)
	h.recoverInteractions(time.Now())
	gen, node, ok := readInteractionGeneration(bundle)
	if !ok || gen != 7 || node != n.ID {
		t.Fatalf("worker restart republished %d %q ok=%v", gen, node, ok)
	}
	if err := os.Remove(fence); err != nil {
		t.Fatal(err)
	}
	result, err := h.SubmitClaudeInteraction(context.Background(), sessionworker.InteractionAction{
		RequestID: "live-gen", Action: "answer",
		Answers: []sessionworker.InteractionSelection{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}},
	})
	if err != nil || result.Status != "accepted" || result.Delivery != "recorded" || result.Emitted {
		t.Fatalf("submit without the helper file = %#v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "ans", "live-gen.json")); err != nil {
		t.Fatal("live binding did not record the answer")
	}
	if _, err := os.Stat(fence); !os.IsNotExist(err) {
		t.Fatal("submit rewrote the helper fence the restart had already published")
	}
}

func TestSubmitUsesFileWhenLiveBindingIsUnset(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.claudeGens[n.ID] = 0
	a.mu.Unlock()
	if err := writeClaudePermFile(filepath.Join(bundle, "perm", "generation.json"), map[string]any{"generation": 3, "node_id": n.ID}); err != nil {
		t.Fatal(err)
	}
	publishSyntheticInteraction(t, bundle, "file-submit", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	h := testClaudeSessionHarness(a, n.ID)
	result, err := h.SubmitClaudeInteraction(context.Background(), sessionworker.InteractionAction{
		RequestID: "file-submit", Action: "answer",
		Answers: []sessionworker.InteractionSelection{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}},
	})
	if err != nil || result.Status != "accepted" || result.Delivery != "recorded" {
		t.Fatalf("file-backed submit = %#v %v", result, err)
	}
	var generation int
	for _, ev := range sessionlog.ReadEvents(a.sessionLogPath(n.ID)) {
		if ev.Decision != nil && ev.Decision.Manual != nil {
			generation = ev.Decision.Manual.Generation
		}
	}
	if generation != 3 {
		t.Fatalf("recorded generation = %d, want the helper fence", generation)
	}
}

func TestDetachedRegistryCannotOverrideBindingFile(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.claudeGens[n.ID] = 4
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	a.mu.Lock()
	a.claudeHooks[n.ID] = "other-hook"
	a.claudeGens[n.ID] = 9
	a.mu.Unlock()
	if _, _, ok := lookupClaudeBinding(bundle); ok {
		t.Fatal("a node that no longer owns the bundle still authorizes it")
	}
	publishSyntheticInteraction(t, bundle, "file-still-wins", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	rec, err := readInteractionRequest(bundle, "file-still-wins")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Generation != 4 || rec.NodeID != n.ID {
		t.Fatalf("detached registry stamped generation %d node %q", rec.Generation, rec.NodeID)
	}
	gen, _, ok := readInteractionGeneration(bundle)
	if !ok || gen != 4 {
		t.Fatalf("detached registry rewrote the helper fence to %d ok=%v", gen, ok)
	}
}

func TestBindingRegistryIgnoresEmptyOwners(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	registerClaudeBindingOwner("", a, n.ID)
	registerClaudeBindingOwner(bundle, nil, n.ID)
	registerClaudeBindingOwner(bundle, a, "")
	if _, _, ok := lookupClaudeBinding(""); ok {
		t.Fatal("an empty bundle became a binding")
	}
	key := filepath.Join(t.TempDir(), "foreign")
	t.Cleanup(func() { claudeBindingOwners.Delete(key) })
	claudeBindingOwners.Store(key, "not-an-owner")
	if _, _, ok := lookupClaudeBinding(key); ok {
		t.Fatal("a foreign registry value authorized a bundle")
	}
	claudeBindingOwners.Store(key, &claudeBindingOwner{})
	if _, _, ok := lookupClaudeBinding(key); ok {
		t.Fatal("an owner with no app authorized a bundle")
	}
}

func TestBindingFenceRejectsBrokenPerm(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	if err := os.RemoveAll(perm); err != nil {
		t.Fatal(err)
	}
	if err := publishClaudeBindingFenceFile(bundle, n.ID, 4, ""); !os.IsNotExist(err) {
		t.Fatalf("missing perm = %v", err)
	}
	if err := os.WriteFile(perm, []byte("not-a-directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishClaudeBindingFenceFile(bundle, n.ID, 4, ""); !errors.Is(err, errInteractionIO) {
		t.Fatalf("perm file = %v", err)
	}
	body, err := os.ReadFile(perm)
	if err != nil || string(body) != "not-a-directory" {
		t.Fatalf("broken perm grew a generation fence: %q %v", body, err)
	}
	a.publishClaudeBindingFence("missing-node")
	a.publishClaudeBindingFence("")
	var missing *app
	missing.publishClaudeBindingFence(n.ID)
}

func TestStampRefusesUnrefreshableGenerationFence(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.claudeGens[n.ID] = 8
	a.mu.Unlock()
	fence := filepath.Join(bundle, "perm", "generation.json")
	if err := os.Remove(fence); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fence, 0o700); err != nil {
		t.Fatal(err)
	}
	err := publishInteractionRequest(bundle, claudePermissionEvent{
		SessionID: n.SessionID, ToolName: "AskUserQuestion",
	}, "stamp-blocked", mustClassify(t, "AskUserQuestion", letterQuestionRaw()), time.Now(), time.Minute)
	if !errors.Is(err, errInteractionIO) {
		t.Fatalf("publish = %v", err)
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "req", "stamp-blocked.json")); !os.IsNotExist(err) {
		t.Fatal("request was published without a refreshable generation fence")
	}
}

func TestEmissionRejectsLiveNodeMismatch(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	t.Cleanup(func() { claudeBindingOwners.Delete(bundle) })
	a.mu.Lock()
	a.claudeGens[n.ID] = 4
	a.claudeHooks["other"] = a.claudeHooks[n.ID]
	a.claudeGens["other"] = 4
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	publishSyntheticInteraction(t, bundle, "live-node", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	registerClaudeBindingOwner(bundle, a, "other")
	env, err := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), letterReply())
	if err != nil {
		t.Fatal(err)
	}
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "ans", "live-node.json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := emitInteractionAnswer(bundle, "live-node", env.Stdout, &out, time.Now, time.Now().Add(time.Minute)); err != nil || out.Len() != 0 {
		t.Fatalf("emitted for a live owner that is not the request node: err=%v stdout=%q", err, out.String())
	}
	if phase := claudeInteractionPhase(bundle, "live-node", time.Now()); phase != "expired" {
		t.Fatalf("mismatched live owner left phase %s", phase)
	}
	registerClaudeBindingOwner(bundle, a, n.ID)
	publishSyntheticInteraction(t, bundle, "live-restored", "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "ans", "live-restored.json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	emitted, err := emitInteractionAnswer(bundle, "live-restored", env.Stdout, &out, time.Now, time.Now().Add(time.Minute))
	if err != nil || !emitted || out.Len() == 0 {
		t.Fatalf("restored owner did not emit: emitted=%v err=%v stdout=%q", emitted, err, out.String())
	}
}

func TestClearGenerationRejectsBindingMovedDuringAppend(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	hookID := a.claudeHookID(n.ID)
	prev := claudeClearGenerationPause
	t.Cleanup(func() { claudeClearGenerationPause = prev })
	claudeClearGenerationPause = func(app *app) {
		app.mu.Lock()
		app.claudeGens[n.ID] = 5
		app.mu.Unlock()
	}
	lock := a.claudeBindLock(n.ID)
	lock.Lock()
	defer lock.Unlock()
	err := a.persistClaudeClearGeneration(n.ID, hookID, 1)
	if !errors.Is(err, errClaudeHookRejected) {
		t.Fatalf("clear after the binding moved = %v", err)
	}
	if got := a.claudeGeneration(n.ID); got != 5 {
		t.Fatalf("memory generation = %d, want the generation observed during the append", got)
	}
	gen, _, ok := readInteractionGeneration(bundle)
	if !ok || gen != 1 {
		t.Fatalf("helper fence advanced despite the rejected clear: %d ok=%v", gen, ok)
	}
	claudeClearGenerationPause = nil
	a.mu.Lock()
	a.claudeGens[n.ID] = 1
	a.mu.Unlock()
	if err := a.persistClaudeClearGeneration(n.ID, hookID, 1); err != nil {
		t.Fatal(err)
	}
	if got := a.claudeGeneration(n.ID); got != 2 {
		t.Fatalf("later clear generation = %d", got)
	}
	nextGen, nodeID, nextOK := readInteractionGeneration(bundle)
	if !nextOK || nextGen != 2 || nodeID != n.ID {
		t.Fatalf("later clear fence = %d %q ok=%v", nextGen, nodeID, nextOK)
	}
}
