package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/sessionworker"
)

// The helper child is a separate process. It cannot see this process's
// binding registry, which is the real hook helper's situation.
func TestThirdFenceHelperProcess(t *testing.T) {
	if os.Getenv("SCIMUX_FENCE_HELPER") == "1" {
		runThirdFenceHelperChild()
	}
	t.Run("missing fence", func(t *testing.T) {
		assertHelperFenceRejects(t, "review-missing-generation", func(dir, genPath string) {
			if err := os.Remove(genPath); err != nil {
				t.Fatal(err)
			}
		})
	})
	t.Run("unreadable fence", func(t *testing.T) {
		assertHelperFenceRejects(t, "review-unreadable-generation", func(dir, genPath string) {
			if err := os.Chmod(genPath, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(genPath, 0o600) })
		})
	})
	t.Run("corrupt fence", func(t *testing.T) {
		assertHelperFenceRejects(t, "review-corrupt-generation", func(dir, genPath string) {
			if err := os.WriteFile(genPath, []byte("not-json"), 0o600); err != nil {
				t.Fatal(err)
			}
		})
	})
	t.Run("replaced fence", func(t *testing.T) {
		assertHelperFenceRejects(t, "review-replaced-generation", func(dir, genPath string) {
			if err := writeClaudePermFile(genPath, map[string]any{"generation": 2, "node_id": "node-a", "session_id": hookSIDOwn}); err != nil {
				t.Fatal(err)
			}
		})
	})
	t.Run("revoked session", func(t *testing.T) {
		assertHelperFenceRejects(t, "review-revoked-session", func(dir, genPath string) {
			if err := writeClaudePermFile(genPath, map[string]any{"generation": 1, "node_id": "node-a", "session_id": hookSIDOther}); err != nil {
				t.Fatal(err)
			}
		})
	})
	t.Run("legacy without fence", func(t *testing.T) {
		dir := interactionBundle(t)
		if err := writeClaudeAcceptedTurn(filepath.Join(dir, "perm"), claudeAcceptedTurn{Turn: "review-current", Gen: 1, Session: hookSIDOwn}); err != nil {
			t.Fatal(err)
		}
		out := runThirdFenceHelper(t, dir, "review-legacy-generation", claudePermissionEvent{
			HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
		}, func(id string) {
			writeThirdFenceAnswer(t, dir, id)
		})
		if len(out) == 0 {
			t.Fatal("a legacy request with no binding fence was rejected")
		}
	})
}

func assertHelperFenceRejects(t *testing.T, id string, mutate func(dir, genPath string)) {
	t.Helper()
	dir := interactionBundle(t)
	if err := writeClaudeAcceptedTurn(filepath.Join(dir, "perm"), claudeAcceptedTurn{Turn: "review-current", Gen: 1, Session: hookSIDOwn}); err != nil {
		t.Fatal(err)
	}
	genPath := filepath.Join(dir, "perm", "generation.json")
	if err := writeClaudePermFile(genPath, map[string]any{"generation": 1, "node_id": "node-a", "session_id": hookSIDOwn}); err != nil {
		t.Fatal(err)
	}
	out := runThirdFenceHelper(t, dir, id, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, func(requestID string) {
		mutate(dir, genPath)
		writeThirdFenceAnswer(t, dir, requestID)
	})
	if len(out) > 0 {
		t.Fatalf("helper emitted without its authoritative fence: %s", out)
	}
}

func writeThirdFenceAnswer(t *testing.T, dir, id string) {
	t.Helper()
	env, err := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), letterReply())
	if err != nil {
		t.Fatal(err)
	}
	if err := exclWrite(filepath.Join(dir, "perm", "interact", "ans", id+".json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
}

func runThirdFenceHelperChild() {
	dir := os.Getenv("SCIMUX_FENCE_DIR")
	id := os.Getenv("SCIMUX_FENCE_ID")
	var ev claudePermissionEvent
	if err := json.NewDecoder(os.Stdin).Decode(&ev); err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(2)
	}
	var out bytes.Buffer
	if _, err := serveClaudeInteraction(dir, ev, id, &out, 2*time.Second, 5*time.Millisecond, time.Now, time.Sleep); err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(2)
	}
	_, _ = os.Stdout.Write(out.Bytes())
	os.Exit(0)
}

func runThirdFenceHelper(t *testing.T, dir, id string, ev claudePermissionEvent, mutate func(id string)) []byte {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestThirdFenceHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SCIMUX_FENCE_HELPER=1", "SCIMUX_FENCE_DIR="+dir, "SCIMUX_FENCE_ID="+id)
	cmd.Stdin = bytes.NewReader(raw)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	req := filepath.Join(dir, "perm", "interact", "req", id+".json")
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(req); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(req); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper did not publish %s: %s", req, stderr.String())
	}
	mutate(id)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper process: %v stderr=%s", err, stderr.String())
	}
	return stdout.Bytes()
}

func TestThirdReviewMuxerProjectionMustNotRollBackWorkerFence(t *testing.T) {
	worker := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, worker, "n1", hookSIDOwn)
	worker.mu.Lock()
	worker.claudeGens[n.ID] = 1
	worker.mu.Unlock()
	worker.publishClaudeBindingFence(n.ID)
	hookID := filepath.Base(bundle)
	bind := worker.claudeBindLock(n.ID)
	bind.Lock()
	if err := worker.persistClaudeClearGeneration(n.ID, hookID, 1); err != nil {
		bind.Unlock()
		t.Fatal(err)
	}
	if err := worker.persistClaudeClearGeneration(n.ID, hookID, 2); err != nil {
		bind.Unlock()
		t.Fatal(err)
	}
	bind.Unlock()
	if gen, _, _ := readInteractionGeneration(bundle); gen != 3 {
		t.Fatalf("setup generation=%d", gen)
	}
	muxer := newTestApp(t, &fakeTmux{})
	seedOwnedClaude(t, muxer, n.ID, hookSIDOwn, "")
	muxer.claudeHooksRoot = worker.claudeHooksDir()
	muxer.mu.Lock()
	muxer.claudeHooks[n.ID] = hookID
	muxer.claudeGens[n.ID] = 1
	muxer.mu.Unlock()
	muxer.projectWorkerClaudeHook(n.ID, sessionworker.State{HookID: hookID, HookGeneration: 2})
	if gen, _, _ := readInteractionGeneration(bundle); gen != 3 {
		t.Fatalf("stale muxer snapshot rolled worker binding fence backwards: got=%d want=3", gen)
	}
}

func TestThirdReviewMissingGenerationFenceMustRejectEmission(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now()
	if err := writeClaudeAcceptedTurn(filepath.Join(dir, "perm"), claudeAcceptedTurn{Turn: "review-current", Gen: 1, Session: hookSIDOwn}); err != nil {
		t.Fatal(err)
	}
	genPath := filepath.Join(dir, "perm", "generation.json")
	if err := writeClaudePermFile(genPath, map[string]any{"generation": 1, "node_id": "node-a"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, err := serveClaudeInteraction(dir, claudePermissionEvent{
		SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "review-missing-generation", &out, time.Minute, time.Millisecond, func() time.Time { return now }, func(time.Duration) {
		env, e := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), letterReply())
		if e != nil {
			t.Fatal(e)
		}
		if e := exclWrite(filepath.Join(dir, "perm", "interact", "ans", "review-missing-generation.json"), env.Stdout); e != nil {
			t.Fatal(e)
		}
		if e := os.Remove(genPath); e != nil {
			t.Fatal(e)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Len() > 0 {
		t.Fatal("a generation-1 request emitted after its only helper-visible generation fence disappeared")
	}
}

func TestThirdReviewResumeSessionChangeMustRevokeOldAnswer(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, dir := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.claudeGens[n.ID] = 1
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	if _, err := a.beginClaudeAcceptedTurn(n); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var out bytes.Buffer
	_, err := serveClaudeInteraction(dir, claudePermissionEvent{
		SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "review-resume-binding", &out, time.Minute, time.Millisecond, func() time.Time { return now }, func(time.Duration) {
		env, e := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), letterReply())
		if e != nil {
			t.Fatal(e)
		}
		if e := exclWrite(filepath.Join(dir, "perm", "interact", "ans", "review-resume-binding.json"), env.Stdout); e != nil {
			t.Fatal(e)
		}
		resumePath := filepath.Join(a.home, ".claude", "projects", "synthetic", hookSIDOther+".jsonl")
		if e := os.MkdirAll(filepath.Dir(resumePath), 0o700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(resumePath, []byte("\n"), 0o600); e != nil {
			t.Fatal(e)
		}
		if e := a.applyClaudeResume(n.ID, claudeSessionStartEvent{
			HookEventName: "SessionStart", Source: "resume", Cwd: n.Dir, SessionID: hookSIDOther, TranscriptPath: resumePath,
		}, 1); e != nil {
			t.Fatal(e)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Len() > 0 {
		t.Fatal("an old-session answer emitted after the production binding commit switched the node to another session")
	}
}

func TestThirdReviewPublicationLockFailureMustRevokeOldAnswer(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, dir := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.claudeGens[n.ID] = 1
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	rec := sampleRequest(dir, time.Now())
	rec.ID = "review-lock-failure"
	rec.SessionID = hookSIDOwn
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	rec.Generation = 1
	rec.NodeID = n.ID
	publishInteraction(t, dir, rec)
	env, err := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), letterReply())
	if err != nil {
		t.Fatal(err)
	}
	if err := exclWrite(filepath.Join(dir, "perm", "interact", "ans", rec.ID+".json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
	interact := filepath.Join(dir, "perm", "interact")
	t.Cleanup(func() { _ = os.Chmod(interact, 0o700) })
	if err := os.Remove(filepath.Join(interact, "fence.lock")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Chmod(interact, 0o555); err != nil {
		t.Fatal(err)
	}
	resumePath := filepath.Join(a.home, ".claude", "projects", "synthetic", hookSIDOther+".jsonl")
	if err := os.MkdirAll(filepath.Dir(resumePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resumePath, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resumeErr := a.applyClaudeResume(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "resume", Cwd: n.Dir, SessionID: hookSIDOther, TranscriptPath: resumePath,
	}, 1)
	if err := os.Chmod(interact, 0o700); err != nil {
		t.Fatal(err)
	}
	if resumeErr == nil {
		t.Fatal("resume reported success without publishing the new session fence")
	}
	var out bytes.Buffer
	if _, err := emitInteractionAnswer(dir, rec.ID, env.Stdout, &out, time.Now, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if out.Len() > 0 {
		t.Fatal("an old-session answer emitted after the binding publish could not replace the fence")
	}
}

func TestThirdReviewWatermarkAndFenceBranches(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.claudeGens[n.ID] = 1
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	before, err := os.ReadFile(filepath.Join(bundle, "perm", "generation.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := publishClaudeBindingFenceFile(bundle, n.ID, 1, ""); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(bundle, "perm", "generation.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("empty session replaced the published session: %s", after)
	}
	if err := publishClaudeBindingFenceFile(bundle, n.ID, 0, hookSIDOwn); err != nil {
		t.Fatal(err)
	}
	if err := publishClaudeBindingFenceFile(bundle, "", 1, hookSIDOwn); err != nil {
		t.Fatal(err)
	}
	if err := publishClaudeBindingFenceFile("", n.ID, 1, hookSIDOwn); err != nil {
		t.Fatal(err)
	}
	if gen, node, ok := readInteractionGeneration(t.TempDir()); ok || gen != 0 || node != "" {
		t.Fatalf("missing fence read = %d %q %v", gen, node, ok)
	}
	if err := publishClaudeBindingFenceExclusive("", n.ID, hookSIDOwn, 1); err != nil {
		t.Fatal(err)
	}
	if err := publishClaudeBindingFenceExclusive(bundle, "", hookSIDOwn, 1); err != nil {
		t.Fatal(err)
	}
	if err := publishClaudeBindingFenceExclusive(bundle, n.ID, hookSIDOwn, 0); err != nil {
		t.Fatal(err)
	}
	if err := publishClaudeBindingFenceExclusive(t.TempDir(), n.ID, hookSIDOwn, 1); err != nil {
		t.Fatal(err)
	}
	kept, err := os.ReadFile(filepath.Join(bundle, "perm", "generation.json"))
	if err != nil || string(kept) != string(before) {
		t.Fatalf("early fence returns rewrote authority: %s %v", kept, err)
	}
	if bindingFenceAlreadyAuthoritative(bundle, n.ID, "", 1) != true {
		t.Fatal("empty session did not match the published generation")
	}
	if bindingFenceAlreadyAuthoritative(bundle, "other", hookSIDOwn, 1) ||
		bindingFenceAlreadyAuthoritative(bundle, n.ID, hookSIDOwn, 9) ||
		bindingFenceAlreadyAuthoritative(t.TempDir(), n.ID, hookSIDOwn, 1) {
		t.Fatal("a different node, generation, or missing file matched the fence")
	}

	interact := filepath.Join(bundle, "perm", "interact")
	t.Cleanup(func() { _ = os.Chmod(interact, 0o700) })
	if err := os.Remove(filepath.Join(interact, "fence.lock")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Chmod(interact, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := publishClaudeBindingFenceExclusive(bundle, n.ID, "", 1); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(interact, 0o700); err != nil {
		t.Fatal(err)
	}
	same, err := os.ReadFile(filepath.Join(bundle, "perm", "generation.json"))
	if err != nil || string(same) != string(before) {
		t.Fatalf("a same-authority lock failure revoked the fence: %s %v", same, err)
	}
}

func TestThirdReviewBindingCommitFailureKeepsFence(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, dir := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.claudeGens[n.ID] = 1
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	before, err := os.ReadFile(filepath.Join(dir, "perm", "generation.json"))
	if err != nil {
		t.Fatal(err)
	}
	resumePath := filepath.Join(a.home, ".claude", "projects", "synthetic", hookSIDOther+".jsonl")
	if err := os.MkdirAll(filepath.Dir(resumePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resumePath, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ev := claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "resume", Cwd: n.Dir, SessionID: hookSIDOther, TranscriptPath: resumePath,
	}
	a.claudeAfterCandidate = func(string) {
		blocked := filepath.Join(t.TempDir(), "store-blocked")
		if err := os.Mkdir(blocked, 0o700); err != nil {
			t.Fatal(err)
		}
		a.storePath = blocked
	}
	if err := a.applyClaudeResume(n.ID, ev, 1); err == nil {
		t.Fatal("resume committed a binding when the store append failed")
	}
	got, err := os.ReadFile(filepath.Join(dir, "perm", "generation.json"))
	if err != nil || string(got) != string(before) {
		t.Fatalf("failed binding append changed the fence: %s %v", got, err)
	}
	if n.SessionID != hookSIDOwn {
		t.Fatalf("failed binding append changed the session to %s", n.SessionID)
	}

	a.claudeAfterCandidate = nil
	prev := claudeBindingMemoryPause
	t.Cleanup(func() { claudeBindingMemoryPause = prev })
	claudeBindingMemoryPause = func(app *app) {
		app.mu.Lock()
		delete(app.byID, n.ID)
		app.mu.Unlock()
	}
	store := filepath.Join(t.TempDir(), "nodes.jsonl")
	if err := os.WriteFile(store, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a.storePath = store
	err = a.applyClaudeResume(n.ID, ev, 1)
	if !errors.Is(err, errClaudeHookRejected) {
		t.Fatalf("commit after the node disappeared = %v", err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "perm", "generation.json"))
	if err != nil || string(got) != string(before) {
		t.Fatalf("rejected memory update changed the fence: %s %v", got, err)
	}
}

func TestThirdReviewPublicationWriteFailureRemovesFence(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, dir := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.claudeGens[n.ID] = 1
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	rec := sampleRequest(dir, time.Now())
	rec.ID = "review-write-failure"
	rec.SessionID = hookSIDOwn
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	rec.Generation = 1
	rec.NodeID = n.ID
	publishInteraction(t, dir, rec)
	env, err := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), letterReply())
	if err != nil {
		t.Fatal(err)
	}
	if err := exclWrite(filepath.Join(dir, "perm", "interact", "ans", rec.ID+".json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
	resumePath := filepath.Join(a.home, ".claude", "projects", "synthetic", hookSIDOther+".jsonl")
	if err := os.MkdirAll(filepath.Dir(resumePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resumePath, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "perm", "generation.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	resumeErr := a.applyClaudeResume(n.ID, claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "resume", Cwd: n.Dir, SessionID: hookSIDOther, TranscriptPath: resumePath,
	}, 1)
	if resumeErr == nil {
		t.Fatal("resume reported success when the fence could not be replaced")
	}
	if _, err := os.Stat(filepath.Join(dir, "perm", "generation.json")); !os.IsNotExist(err) {
		t.Fatalf("failed fence publication left the previous authority: %v", err)
	}
	var out bytes.Buffer
	if _, err := emitInteractionAnswer(dir, rec.ID, env.Stdout, &out, time.Now, time.Now().Add(time.Minute)); err != nil || out.Len() != 0 {
		t.Fatalf("old answer emitted after publication failed: err=%v stdout=%q", err, out.String())
	}
}

func TestThirdReviewSameSessionResumeKeepsAnswer(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, dir := seedPermClaude(t, a, "n1", hookSIDOwn)
	path := filepath.Join(a.home, ".claude", "projects", "synthetic", hookSIDOwn+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n.Transcript = path
	a.mu.Lock()
	a.claudeGens[n.ID] = 1
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	if _, err := a.beginClaudeAcceptedTurn(n); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var out bytes.Buffer
	_, err := serveClaudeInteraction(dir, claudePermissionEvent{
		SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "review-same-session", &out, time.Minute, time.Millisecond, func() time.Time { return now }, func(time.Duration) {
		writeThirdFenceAnswer(t, dir, "review-same-session")
		if e := a.applyClaudeResume(n.ID, claudeSessionStartEvent{
			HookEventName: "SessionStart", Source: "resume", Cwd: n.Dir, SessionID: hookSIDOwn, TranscriptPath: path,
		}, 1); e != nil {
			t.Fatal(e)
		}
	})
	if err != nil || out.Len() == 0 {
		t.Fatalf("same-session resume revoked the current answer: err=%v stdout=%q", err, out.String())
	}
}

func TestThirdReviewInvalidResumeKeepsAnswer(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, dir := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.claudeGens[n.ID] = 1
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	if _, err := a.beginClaudeAcceptedTurn(n); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var out bytes.Buffer
	_, err := serveClaudeInteraction(dir, claudePermissionEvent{
		SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "review-invalid-resume", &out, time.Minute, time.Millisecond, func() time.Time { return now }, func(time.Duration) {
		writeThirdFenceAnswer(t, dir, "review-invalid-resume")
		if e := a.applyClaudeResume(n.ID, claudeSessionStartEvent{HookEventName: "SessionStart", Source: "nope"}, 1); e == nil {
			t.Fatal("invalid resume was accepted")
		}
	})
	if err != nil || out.Len() == 0 {
		t.Fatalf("invalid resume revoked the current answer: err=%v stdout=%q", err, out.String())
	}
}

func TestThirdReviewClearGenerationRevokesOldAnswer(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, dir := seedPermClaude(t, a, "n1", hookSIDOwn)
	hookID := a.claudeHookID(n.ID)
	a.mu.Lock()
	a.claudeGens[n.ID] = 1
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	rec := sampleRequest(dir, time.Now())
	rec.ID = "review-clear-generation"
	rec.SessionID = n.SessionID
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	rec.Generation = 1
	rec.NodeID = n.ID
	publishInteraction(t, dir, rec)
	env, err := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), letterReply())
	if err != nil {
		t.Fatal(err)
	}
	if err := exclWrite(filepath.Join(dir, "perm", "interact", "ans", rec.ID+".json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
	bind := a.claudeBindLock(n.ID)
	bind.Lock()
	err = a.persistClaudeClearGeneration(n.ID, hookID, 1)
	bind.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := emitInteractionAnswer(dir, rec.ID, env.Stdout, &out, time.Now, time.Now().Add(time.Minute)); err != nil || out.Len() != 0 {
		t.Fatalf("generation-1 answer emitted after clear advanced the fence: err=%v stdout=%q", err, out.String())
	}
}

func TestThirdReviewSubmitRejectsFenceSession(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, dir := seedPermClaude(t, a, "n1", hookSIDOwn)
	a.mu.Lock()
	a.claudeGens[n.ID] = 1
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	publishSyntheticInteraction(t, dir, "review-submit-session", "AskUserQuestion", letterQuestionRaw(), hookSIDOwn)
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "generation.json"), map[string]any{
		"generation": 1, "node_id": n.ID, "session_id": hookSIDOther,
	}); err != nil {
		t.Fatal(err)
	}
	caller := claudeInteractionCaller{
		NodeID: n.ID, SessionID: hookSIDOwn, BundleID: filepath.Base(dir),
		Generation: 1, Turn: turn.Turn, TurnGen: turn.Gen,
	}
	_, err = submitClaudeInteraction(dir, caller, "review-submit-session", letterReply(), time.Now(), func(sessionlog.Event) error { return nil })
	if err == nil {
		t.Fatal("submit accepted an answer after the fence session changed")
	}
}

func TestThirdReviewRecoveryMustNotRollBackWorkerFence(t *testing.T) {
	worker := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, worker, "n1", hookSIDOwn)
	worker.mu.Lock()
	worker.claudeGens[n.ID] = 1
	worker.mu.Unlock()
	worker.publishClaudeBindingFence(n.ID)
	hookID := filepath.Base(bundle)
	bind := worker.claudeBindLock(n.ID)
	bind.Lock()
	if err := worker.persistClaudeClearGeneration(n.ID, hookID, 1); err != nil {
		bind.Unlock()
		t.Fatal(err)
	}
	if err := worker.persistClaudeClearGeneration(n.ID, hookID, 2); err != nil {
		bind.Unlock()
		t.Fatal(err)
	}
	bind.Unlock()
	turn, err := worker.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	data := ownedWorkerLockDir(t)
	state := sessionworker.State{
		HasSession: true, SessionID: hookSIDOwn, HookID: hookID, HookGeneration: 2,
		Launch: &sessionworker.LaunchRequest{
			NodeID: n.ID, Title: n.Title, Agent: n.Agent, Transport: n.Transport,
			Dir: n.Dir, CreatedAt: n.CreatedAt,
		},
	}
	_, closeWorker := publishRetirementWorker(t, data, n.ID, state)
	t.Cleanup(closeWorker)
	muxer := newTestApp(t, &fakeTmux{})
	muxer.claudeHooksRoot = worker.claudeHooksDir()
	m := newWorkerManager("", data, "new")
	if err := m.RecoverUnknown(muxer); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.RetireController(n.ID) })
	if gen, _, _ := readInteractionGeneration(bundle); gen != 3 {
		t.Fatalf("recovery snapshot rolled the worker fence to %d", gen)
	}
	if got := muxer.claudeGeneration(n.ID); got != 2 {
		t.Fatalf("recovery did not record the snapshot generation: %d", got)
	}
	current := runThirdFenceHelper(t, bundle, "review-recovery-current", claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, func(id string) {
		writeThirdFenceAnswer(t, bundle, id)
	})
	if len(current) == 0 {
		t.Fatal("helper rejected the worker's current generation after a stale recovery snapshot")
	}
	_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
	rec := sampleRequest(bundle, time.Now())
	rec.ID = "review-recovery-old"
	rec.SessionID = hookSIDOwn
	rec.Generation = 2
	rec.NodeID = n.ID
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	publishInteraction(t, bundle, rec)
	old := runThirdFenceHelper(t, bundle, rec.ID, claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, func(id string) {
		writeThirdFenceAnswer(t, bundle, id)
	})
	if len(old) > 0 {
		t.Fatalf("helper emitted a generation-2 answer against the worker fence: %s", old)
	}
}
