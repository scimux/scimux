package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func contLongWaitBundle(t *testing.T) string {
	t.Helper()
	dir := interactionBundle(t)
	caps, ok := readClaudeHookCapabilities(dir)
	if !ok {
		t.Fatal("capabilities")
	}
	caps.InteractionLongWait = 1
	raw, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "capabilities.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func contQuestionEvent() claudePermissionEvent {
	return claudePermissionEvent{
		HookEventName: "PermissionRequest", SessionID: hookSIDOwn,
		ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}
}

func contReadInteraction(t *testing.T, bundle, id string) claudeInteractionRequest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(bundle, "perm", "interact", "req", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec claudeInteractionRequest
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestNewBundleRegistersLongQuestionWait(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	id, settings, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(a.claudeHooksDir(), id)
	raw := mustRead(t, filepath.Join(bundle, "capabilities.json"))
	var caps claudeHookCapabilities
	if json.Unmarshal(raw, &caps) != nil {
		t.Fatalf("capabilities: %s", raw)
	}
	settingsRaw := mustRead(t, settings)
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Timeout int `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(settingsRaw, &doc) != nil {
		t.Fatalf("settings: %s", settingsRaw)
	}
	permTimeout := -1
	for name, groups := range doc.Hooks {
		for _, group := range groups {
			for _, hook := range group.Hooks {
				if name == "PermissionRequest" {
					permTimeout = hook.Timeout
					continue
				}
				if hook.Timeout != 0 {
					t.Fatalf("%s has timeout %d; only PermissionRequest waits", name, hook.Timeout)
				}
			}
		}
	}
	if caps.InteractionLongWait != 1 || !bytes.Contains(raw, []byte(`"interaction_long_wait":1`)) || permTimeout != interactionLongHookTimeoutSec {
		t.Fatalf("long wait = %d, permission timeout = %d; a new bundle must register the four-hour question wait", caps.InteractionLongWait, permTimeout)
	}
	if !bundleCompleteCurrent(bundle) || !bundleSupportsInteraction(bundle) {
		t.Fatal("long wait must keep the bundle complete and interaction-capable")
	}
}

func TestInteractionWaitBudgetFor(t *testing.T) {
	long := contLongWaitBundle(t)
	t.Run("test budget", func(t *testing.T) {
		if got := interactionWaitBudgetFor(long, 40*time.Millisecond); got != 40*time.Millisecond {
			t.Fatalf("test budget = %s, want 40ms", got)
		}
	})
	t.Run("long wait", func(t *testing.T) {
		if got := interactionWaitBudgetFor(long, permHookDeadline); got != interactionLongWait {
			t.Fatalf("long-wait bundle budget = %s, want %s", got, interactionLongWait)
		}
	})
	t.Run("legacy field absent", func(t *testing.T) {
		if got := interactionWaitBudgetFor(interactionBundle(t), permHookDeadline); got != interactionHelperDeadline {
			t.Fatalf("legacy budget = %s, want %s", got, interactionHelperDeadline)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		dir := interactionBundle(t)
		path := filepath.Join(dir, "capabilities.json")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if got := interactionWaitBudgetFor(dir, permHookDeadline); got != interactionHelperDeadline {
			t.Fatalf("unreadable budget = %s, want %s", got, interactionHelperDeadline)
		}
	})
	t.Run("bad json", func(t *testing.T) {
		dir := interactionBundle(t)
		if err := os.WriteFile(filepath.Join(dir, "capabilities.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := interactionWaitBudgetFor(dir, permHookDeadline); got != interactionHelperDeadline {
			t.Fatalf("bad json budget = %s, want %s", got, interactionHelperDeadline)
		}
	})
	t.Run("missing", func(t *testing.T) {
		dir := interactionBundle(t)
		if err := os.Remove(filepath.Join(dir, "capabilities.json")); err != nil {
			t.Fatal(err)
		}
		if got := interactionWaitBudgetFor(dir, permHookDeadline); got != interactionHelperDeadline {
			t.Fatalf("missing budget = %s, want %s", got, interactionHelperDeadline)
		}
	})
}

func TestLongWaitHelperRecordsFourHourDeadlines(t *testing.T) {
	bundle := contLongWaitBundle(t)
	id := "req-long"
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	clock := start
	sleeps := 0
	var stdout bytes.Buffer
	_, err := serveClaudeInteraction(bundle, contQuestionEvent(), id, &stdout, interactionWaitBudgetFor(bundle, permHookDeadline), time.Millisecond,
		func() time.Time { return clock },
		func(time.Duration) {
			sleeps++
			if sleeps == 1 {
				rec := contReadInteraction(t, bundle, id)
				wantHelper := start.Add(interactionLongWait).UTC().Format(time.RFC3339Nano)
				wantAnswer := start.Add(interactionLongWait - interactionRejectMargin).UTC().Format(time.RFC3339Nano)
				if rec.HelperUntil != wantHelper || rec.AnswerUntil != wantAnswer {
					t.Errorf("HelperUntil %s AnswerUntil %s, want %s and %s; a long question wait must record four-hour deadlines", rec.HelperUntil, rec.AnswerUntil, wantHelper, wantAnswer)
				}
				clock = start.Add(3 * time.Hour)
				if phase := claudeInteractionPhase(bundle, id, clock); phase != "pending" {
					t.Errorf("phase at 3h = %q, want pending; a long question wait must still be open", phase)
				}
				return
			}
			if sleeps == 2 {
				path, err := interactionChild(bundle, "expired", id)
				if err != nil {
					t.Error(err)
					return
				}
				if err := exclWrite(path, []byte("expired")); err != nil {
					t.Error(err)
				}
				return
			}
			t.Errorf("helper slept %d times; a long question wait must stop when the card expires", sleeps)
		})
	if err != nil || stdout.Len() != 0 {
		t.Fatalf("helper err=%v stdout=%q, want an empty exit", err, stdout.Bytes())
	}
	if !t.Failed() && sleeps < 2 {
		t.Fatal("helper exited before the long wait could be observed")
	}
}

func TestLegacyBundleKeepsShortQuestionWait(t *testing.T) {
	bundle := interactionBundle(t)
	id := "req-legacy"
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	clock := start
	var stdout bytes.Buffer
	_, err := serveClaudeInteraction(bundle, contQuestionEvent(), id, &stdout, interactionWaitBudgetFor(bundle, permHookDeadline), time.Millisecond,
		func() time.Time { return clock },
		func(time.Duration) {
			path, pathErr := interactionChild(bundle, "expired", id)
			if pathErr != nil {
				t.Error(pathErr)
				return
			}
			if err := exclWrite(path, []byte("expired")); err != nil && !os.IsExist(err) {
				t.Error(err)
			}
		})
	if err != nil || stdout.Len() != 0 {
		t.Fatalf("legacy helper err=%v stdout=%q", err, stdout.Bytes())
	}
	rec := contReadInteraction(t, bundle, id)
	wantHelper := start.Add(interactionHelperDeadline).UTC().Format(time.RFC3339Nano)
	wantAnswer := start.Add(interactionAnswerWindow).UTC().Format(time.RFC3339Nano)
	if rec.HelperUntil != wantHelper || rec.AnswerUntil != wantAnswer {
		t.Fatalf("HelperUntil %s AnswerUntil %s, want %s and %s", rec.HelperUntil, rec.AnswerUntil, wantHelper, wantAnswer)
	}
}

func TestAnsweredQuestionWithdrawsCard(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: contWorking}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	id := "req-card"
	publishSyntheticInteraction(t, bundle, id, "AskUserQuestion", letterQuestionRaw(), n.SessionID)
	markClaudeRequestAnswered(filepath.Join(bundle, "perm"), id)
	if !withdrawAnsweredClaudeInteraction(bundle, time.Now()) {
		t.Fatal("an answered question must withdraw its card")
	}
	marker, err := os.ReadFile(filepath.Join(bundle, "perm", "interact", "expired", id))
	if err != nil || string(marker) != "withdrawn" {
		t.Fatalf("expired marker = %q, %v, want withdrawn", marker, err)
	}
	a.poll()
	if got := a.attn[n.ID]; got == "question" {
		t.Fatal("withdrew card still raised question attention")
	}
	view, ok := a.claudeInteractionView(n)
	if !ok || view.Phase != "expired" {
		t.Fatalf("view phase = %#v, want expired", view)
	}
	chat, _ := chatBody(t, a, n.ID)["claude_interaction"].(map[string]any)
	if chat["phase"] != "expired" {
		t.Fatalf("chat phase = %v, want expired", chat["phase"])
	}

	helperBundle := interactionBundle(t)
	helperID := "req-help"
	publishSyntheticInteraction(t, helperBundle, helperID, "AskUserQuestion", letterQuestionRaw(), hookSIDOwn)
	markClaudeRequestAnswered(filepath.Join(helperBundle, "perm"), helperID)
	var stdout bytes.Buffer
	clock := time.Now()
	_, err = serveClaudeInteraction(helperBundle, contQuestionEvent(), helperID, &stdout, interactionHelperDeadline, time.Millisecond,
		func() time.Time { return clock },
		func(time.Duration) {
			if withdrawAnsweredClaudeInteraction(helperBundle, clock) {
				return
			}
			t.Error("an answered question must withdraw its card")
			path, pathErr := interactionChild(helperBundle, "expired", helperID)
			if pathErr == nil {
				_ = exclWrite(path, []byte("expired"))
			}
		})
	if err != nil || stdout.Len() != 0 {
		t.Fatalf("helper stdout = %q err=%v, want empty", stdout.Bytes(), err)
	}
}

func TestWithdrawalNeedsExactEvidence(t *testing.T) {
	phaseOf := func(t *testing.T, bundle, id string) string {
		t.Helper()
		return claudeInteractionPhase(bundle, id, time.Now())
	}
	t.Run("notice removed", func(t *testing.T) {
		bundle, _ := contCard(t, "req-gone")
		before := phaseOf(t, bundle, "req-gone")
		if withdrawAnsweredClaudeInteraction(bundle, time.Now()) || phaseOf(t, bundle, "req-gone") != before {
			t.Fatal("a missing answered marker must leave the card pending")
		}
	})
	t.Run("other id", func(t *testing.T) {
		bundle, perm := contCard(t, "req-self")
		markClaudeRequestAnswered(perm, "req-other")
		before := phaseOf(t, bundle, "req-self")
		if withdrawAnsweredClaudeInteraction(bundle, time.Now()) || phaseOf(t, bundle, "req-self") != before {
			t.Fatal("another request's marker must not withdraw this card")
		}
	})
	t.Run("no active", func(t *testing.T) {
		bundle, _ := contCard(t, "req-idle")
		if err := os.Remove(filepath.Join(bundle, "perm", "interact", "active.json")); err != nil {
			t.Fatal(err)
		}
		before := phaseOf(t, bundle, "req-idle")
		if withdrawAnsweredClaudeInteraction(bundle, time.Now()) || phaseOf(t, bundle, "req-idle") != before {
			t.Fatal("no active card must stay unchanged")
		}
	})
	t.Run("recorded", func(t *testing.T) {
		bundle, perm := contCard(t, "req-rec")
		markClaudeRequestAnswered(perm, "req-rec")
		if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "recorded", "req-rec"), []byte("recorded"), 0o600); err != nil {
			t.Fatal(err)
		}
		if withdrawAnsweredClaudeInteraction(bundle, time.Now()) || phaseOf(t, bundle, "req-rec") != "recorded" {
			t.Fatal("a recorded card must not be withdrawn")
		}
	})
	t.Run("emitted", func(t *testing.T) {
		bundle, perm := contCard(t, "req-emit")
		markClaudeRequestAnswered(perm, "req-emit")
		if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "ack", "req-emit.json"), claudeInteractionAck{Emitted: true}); err != nil {
			t.Fatal(err)
		}
		if withdrawAnsweredClaudeInteraction(bundle, time.Now()) || phaseOf(t, bundle, "req-emit") != "emitted" {
			t.Fatal("an emitted card must not be withdrawn")
		}
	})
	t.Run("write error", func(t *testing.T) {
		bundle, perm := contCard(t, "req-io")
		markClaudeRequestAnswered(perm, "req-io")
		if err := os.RemoveAll(filepath.Join(bundle, "perm", "interact", "expired")); err != nil {
			t.Fatal(err)
		}
		if withdrawAnsweredClaudeInteraction(bundle, time.Now()) || phaseOf(t, bundle, "req-io") != "pending" {
			t.Fatal("a withdraw that cannot write the marker must leave the card pending")
		}
	})
	t.Run("claimed", func(t *testing.T) {
		bundle, perm := contCard(t, "req-claim")
		markClaudeRequestAnswered(perm, "req-claim")
		if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "claim", "req-claim"), []byte("claimed"), 0o600); err != nil {
			t.Fatal(err)
		}
		if phaseOf(t, bundle, "req-claim") != "claimed" {
			t.Fatalf("phase = %s, want claimed", phaseOf(t, bundle, "req-claim"))
		}
		if !withdrawAnsweredClaudeInteraction(bundle, time.Now()) {
			t.Fatal("claimed card with an answered marker must be withdrawn")
		}
	})
}

func contCard(t *testing.T, id string) (string, string) {
	t.Helper()
	bundle := interactionBundle(t)
	publishSyntheticInteraction(t, bundle, id, "AskUserQuestion", letterQuestionRaw(), hookSIDOwn)
	return bundle, filepath.Join(bundle, "perm")
}

func TestToolEvidenceJoinMarksAskAnswered(t *testing.T) {
	perm := filepath.Join(t.TempDir(), "perm")
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	const id = "ask-tool"
	contAskAt(t, perm, id, "AskUserQuestion", hookSIDOwn, at)
	// The notice digest is "cont-"+id. Match that exact digest.
	ev := toolEvidence{ID: "tu-1", Tool: "AskUserQuestion", Digest: "cont-" + id, At: at.Add(time.Second)}
	if retireAskedByToolEvidence(perm, []toolEvidence{ev}, at.Add(time.Minute)) != 1 {
		t.Fatal("matching tool evidence must retire the notice")
	}
	if _, err := os.Stat(claudeAskedPath(perm, id)); !os.IsNotExist(err) {
		t.Fatal("tool evidence left the notice in place")
	}
	if !claudeRequestAnswered(perm, id) {
		t.Fatal("tool evidence must mark the ask answered")
	}
	const old = "ask-old"
	contAskAt(t, perm, old, "Bash", hookSIDOwn, at)
	if !retireOldestAskedNotice(perm, at.Add(time.Second), at.Add(time.Minute)) {
		t.Fatal("growth retirement did not remove the older notice")
	}
	if claudeRequestAnswered(perm, old) {
		t.Fatal("growth retirement must not mark the ask answered")
	}
}

func TestTurnBoundaryWithdrawsCard(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}, capture: "quiet"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	perm := filepath.Join(bundle, "perm")
	const id = "ask-q"
	contAsk(t, perm, id, "AskUserQuestion", hookSIDOwn)
	publishSyntheticInteraction(t, bundle, id, "AskUserQuestion", letterQuestionRaw(), hookSIDOwn)
	payload, err := json.Marshal(claudeStopEvent{HookEventName: "Stop", SessionID: hookSIDOwn})
	if err != nil {
		t.Fatal(err)
	}
	if err := RunClaudeStopHook(bundle, bytes.NewReader(payload), nil, nil); err != nil {
		t.Fatal(err)
	}
	a.drainClaudeStopInbox(n.ID, filepath.Base(bundle))
	if !claudeRequestAnswered(perm, id) {
		t.Fatal("stop did not mark the ask answered")
	}
	a.poll()
	view, _ := a.claudeInteractionView(n)
	if view.Phase != "expired" || a.attn[n.ID] == "question" {
		t.Fatalf("phase %q attn %q; a turn boundary must withdraw the question card", view.Phase, a.attn[n.ID])
	}
}
