package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/sessionworker"
	"github.com/scimux/scimux/internal/transcript"
)

func TestSixthHelperProcess(t *testing.T) {
	if bundle := os.Getenv("SCIMUX_SYNTHETIC_INTERACTION_HELPER"); bundle != "" {
		ev := claudePermissionEvent{SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw()}
		_, err := serveClaudeInteraction(bundle, ev, "helper-process", io.Discard, time.Minute, time.Millisecond, time.Now, time.Sleep)
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func TestSixthKilledHelperImmediatelyReleasesOwnership(t *testing.T) {
	bundle := interactionBundle(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestSixthHelperProcess$")
	cmd.Env = append(os.Environ(), "SCIMUX_SYNTHETIC_INTERACTION_HELPER="+bundle)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	})
	until := time.Now().Add(5 * time.Second)
	for {
		if id, ok := readInteractionActive(bundle); ok && id == "helper-process" {
			break
		}
		if time.Now().After(until) {
			t.Fatal("fake helper did not publish request")
		}
		time.Sleep(time.Millisecond)
	}
	if phase := claudeInteractionPhase(bundle, "helper-process", time.Now()); phase != "pending" {
		t.Fatalf("live helper phase: %s", phase)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	// Process reaping can precede the kernel's final descriptor cleanup by a
	// scheduling tick. Wait for that cleanup, never for the request deadline.
	until = time.Now().Add(time.Second)
	for claudeInteractionPhase(bundle, "helper-process", time.Now()) == "pending" && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if phase := claudeInteractionPhase(bundle, "helper-process", time.Now()); phase != "expired" {
		t.Fatalf("killed helper still owns request: %s", phase)
	}
	if _, err := submitClaudeInteraction(bundle, sampleCaller(bundle), "helper-process", letterReply(), time.Now(), func(sessionlog.Event) error { t.Error("dead helper decision audited"); return nil }); !errors.Is(err, errInteractionConflict) {
		t.Fatalf("dead helper accepted: %v", err)
	}
	recoverClaudeInteractionBundle(bundle, time.Now())
	if _, ok := readInteractionActive(bundle); ok {
		t.Fatal("dead helper kept active slot")
	}
	// A successor helper must be able to acquire the slot immediately.
	if err := publishInteractionRequest(bundle, claudePermissionEvent{SessionID: hookSIDOwn, ToolName: "AskUserQuestion"}, "successor-helper", mustClassify(t, "AskUserQuestion", letterQuestionRaw()), time.Now(), time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestSixthWatermarkAndSubmitSnapshotMutableNodeFields(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	p1, p2 := filepath.Join(t.TempDir(), "one.jsonl"), filepath.Join(t.TempDir(), "two.jsonl")
	writeTranscript(t, p1, userLine("", "one"))
	writeTranscript(t, p2, userLine("", "two"))
	n.Transcript = p1
	rec := sampleRequest(bundle, time.Now())
	publishInteraction(t, bundle, rec)
	h := &claudeSessionHarness{app: a, nodeID: n.ID}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			a.mu.Lock()
			if i%2 == 0 {
				n.Transcript, n.SessionID = p1, hookSIDOwn
			} else {
				n.Transcript, n.SessionID = p2, hookSIDOther
			}
			a.mu.Unlock()
		}
	}()
	close(start)
	for i := 0; i < 1000; i++ {
		rememberClaudeInterruptWatermark(a, n)
		_, _ = h.SubmitClaudeInteraction(t.Context(), sessionworker.InteractionAction{RequestID: rec.ID, Action: "cancel"})
	}
	wg.Wait()
}

func TestSixthClearCompletesCommittedBindingAfterFenceFailure(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	old := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("old", 0))
	next := writeClaudeProject(t, a.home, "-w-proj", hookSIDOther, claudeUserLine("new", 0))
	n.Transcript, n.Dir = old, "/w/proj"
	a.claudeGens[n.ID] = 1
	a.tailers[n.ID] = &transcript.Tailer{Path: old}
	a.publishClaudeBindingFence(n.ID)
	publishSyntheticInteraction(t, bundle, "clear-old", "AskUserQuestion", letterQuestionRaw(), hookSIDOwn)
	blocked := filepath.Join(bundle, "perm", "generation.json.tmp")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	ev := claudeSessionStartEvent{HookEventName: "SessionStart", Source: "clear", Cwd: n.Dir, SessionID: hookSIDOther, TranscriptPath: next}
	if err := a.processClaudeHookEventAt(n.ID, ev, 1); err == nil {
		t.Fatal("obstructed fence succeeded")
	}
	if n.SessionID != hookSIDOther || n.Transcript != next {
		t.Fatal("binding was not durable")
	}
	if got := a.claudeLaunchError(n.ID); got != claudeResumeFenceExplain {
		t.Fatalf("committed clear diagnostic: %q", got)
	}
	if a.tailers[n.ID] != nil {
		t.Fatal("old transcript tailer remained attached")
	}
	if a.claudeInteractionBlocksNativeKey(n) {
		t.Fatal("old interaction retained ownership")
	}
	if err := a.processClaudeHookEventAt(n.ID, ev, 2); err == nil {
		t.Fatal("obstructed retry succeeded")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := a.processClaudeHookEventAt(n.ID, ev, 2); err != nil {
		t.Fatal(err)
	}
	if a.claudeLaunchError(n.ID) != "" {
		t.Fatal("recovered clear kept its fence error")
	}
	if a.claudeGeneration(n.ID) != 2 {
		t.Fatal("retried clear turned the page twice")
	}
}

func TestSixthPostAuditRejectionHasDurableOutcome(t *testing.T) {
	for _, failure := range []string{"expired", "turn", "publication"} {
		t.Run(failure, func(t *testing.T) {
			bundle := interactionBundle(t)
			now := time.Now().UTC()
			rec := sampleRequest(bundle, now)
			publishInteraction(t, bundle, rec)
			logPath := filepath.Join(t.TempDir(), "audit.jsonl")
			writer := &sessionlog.Writer{Path: logPath}
			var events []sessionlog.Event
			_, err := submitClaudeInteractionClock(bundle, sampleCaller(bundle), rec.ID, letterReply(), func() time.Time { return now }, func(ev sessionlog.Event) error {
				if err := writer.Append(ev); err != nil {
					return err
				}
				events = append(events, ev)
				if len(events) == 1 {
					switch failure {
					case "expired":
						now = now.Add(2 * time.Minute)
					case "turn":
						if err := writeClaudeAcceptedTurn(filepath.Join(bundle, "perm"), claudeAcceptedTurn{Turn: "successor", Gen: 2}); err != nil {
							t.Fatal(err)
						}
					case "publication":
						if err := os.Mkdir(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json"), 0700); err != nil {
							t.Fatal(err)
						}
					}
				}
				return nil
			})
			if err == nil {
				t.Fatal("submission unexpectedly succeeded")
			}
			if len(events) != 2 {
				t.Fatalf("rejected audited submission left %d records; want intent and outcome", len(events))
			}
			first, last := events[0].Decision, events[1].Decision
			wantDelivery := "not_delivered"
			if failure == "publication" {
				// An I/O failure cannot prove withdrawal or non-consumption.
				wantDelivery = "unconfirmed"
			}
			if durable := sessionlog.ReadEvents(logPath); len(durable) != 2 || durable[1].Decision.Outcome == nil || durable[1].Decision.Outcome.Delivery != wantDelivery {
				t.Fatalf("failure outcome was not durable: %+v", durable)
			}
			if first.Source != "manual" || first.Manual.Delivery != "recorded" || last.Source != "application" || last.Manual != nil || last.Outcome.Delivery != wantDelivery || last.RequestID != first.RequestID {
				t.Fatalf("audit outcomes: %+v %+v", first, last)
			}
			if !strings.Contains(last.Outcome.AppContext, "Application") {
				t.Fatal("failure provenance is not application-authored")
			}
			if strings.Contains(first.Selected.Name, "Answered") {
				t.Fatal("intent is presented as a delivered answer")
			}
		})
	}
}

func TestSixthTimeoutIsPresentInSettingsBuilder(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	raw, err := claudeHookSettingsJSON("/synthetic/scimux", "/synthetic/bundle")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Timeout int `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Hooks["PermissionRequest"][0].Hooks[0].Timeout != 180 {
		t.Fatal("settings builder omitted interaction timeout")
	}
	for name, entries := range doc.Hooks {
		if name != "PermissionRequest" && entries[0].Hooks[0].Timeout != 0 {
			t.Fatalf("timeout leaked to %s", name)
		}
	}
}

func TestSixthJSONEscapesSurviveQuestionAndPlanReplies(t *testing.T) {
	for _, raw := range []string{
		`{"questions":[{"question":"Path \/ or \ud83d\ude00?","options":[{"label":"A\/B"},{"label":"\ud83d\ude00"}]}]}`,
		`{"questions":[{"question":"Pick?","options":[{"label":"A","description":"\/\ud83d\ude00"},{"label":"B"}]}]}`,
	} {
		model := mustClassify(t, "AskUserQuestion", []byte(raw))
		env, err := buildClaudeInteractionReply(model, letterReply())
		if err != nil || !json.Valid(env.Stdout) {
			t.Fatalf("escaped reply: %s %v", env.Stdout, err)
		}
	}
	plan := mustClassify(t, "ExitPlanMode", []byte(`{"plan":"Review \/ \ud83d\ude00"}`))
	if plan.Plan != "Review / 😀" {
		t.Fatalf("plan decoded as %q", plan.Plan)
	}
	// Decoded keys, including escaped aliases, must still be unique.
	if _, err := walkJSONObject([]byte(`{"a\/b":1,"a/b":2}`)); err == nil {
		t.Fatal("decoded duplicate key accepted")
	}
	if value, ok := decodeJSONStringValue([]byte(`"\/\ud83d\ude00"`)); !ok || value != "/😀" {
		t.Fatalf("valid JSON string: %q %v", value, ok)
	}
	if _, ok := decodeJSONStringValue([]byte(`"\x41"`)); ok {
		t.Fatal("Go-only escape accepted as JSON")
	}
}

func TestSixthAuditFailureStillPreventsAnswer(t *testing.T) {
	bundle := interactionBundle(t)
	now := time.Now()
	rec := sampleRequest(bundle, now)
	publishInteraction(t, bundle, rec)
	_, err := submitClaudeInteraction(bundle, sampleCaller(bundle), rec.ID, letterReply(), now, func(sessionlog.Event) error { return errors.New("unavailable") })
	if !errors.Is(err, errInteractionAudit) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("audit failure published answer")
	}
}

func TestSixthUnchangedCancelDoesNotReparseTranscript(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	path := filepath.Join(t.TempDir(), "cached.jsonl")
	writeTranscript(t, path, interruptLine("2099-01-01T00:00:00Z", "[Request interrupted by user]"))
	n.Transcript = path
	rememberClaudeInterruptWatermark(a, n)
	mark := claudeInterruptWatermarkOf(a, n.ID)
	tl := pollTranscript(t, path)
	previous := writeInterruptPrefix
	copies := 0
	writeInterruptPrefix = func(f *os.File, b []byte) error { copies++; return previous(f, b) }
	t.Cleanup(func() { writeInterruptPrefix = previous })
	turn := claudeAcceptedTurn{At: "2001-01-01T00:00:00Z"}
	if !interactionInterruptIsFresh(tl, turn, mark) {
		t.Fatal("fresh dated interruption rejected")
	}
	first := copies
	for i := 0; i < 10; i++ {
		if !interactionInterruptIsFresh(tl, turn, mark) {
			t.Fatal("cached interruption changed")
		}
	}
	if copies != first {
		t.Fatalf("unchanged prefix reparsed %d extra times", copies-first)
	}
	appendTranscript(t, path, userLine("2099-01-01T00:00:01Z", "new prompt"))
	tl.Poll()
	if interactionInterruptIsFresh(tl, turn, mark) {
		t.Fatal("cache revived the old interruption after a new prompt")
	}
	if copies <= first {
		t.Fatal("changed prefix was not reparsed")
	}
	if _, err := interruptPrefixTailer(path, mustSize(t, path)+1); err == nil {
		t.Fatal("short prefix read was accepted")
	}
}

func TestSixthRecoveryDoesNotInventConfirmationOrOwner(t *testing.T) {
	bundle := interactionBundle(t)
	now := time.Now()
	rec := sampleRequest(bundle, now)
	publishInteraction(t, bundle, rec)
	if err := os.MkdirAll(filepath.Join(bundle, "perm", "interact", "confirmed"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "confirmed", rec.ID), []byte("synthetic test-only claim"), 0600); err != nil {
		t.Fatal(err)
	}
	if phase := claudeInteractionPhase(bundle, rec.ID, now); phase == "confirmed" {
		t.Fatal("unregistered observation path can claim confirmation")
	}
	recoverClaudeInteractionBundle(bundle, now)
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "owner")); !os.IsNotExist(err) {
		t.Fatal("recovery created an unused PID owner claim")
	}
}

func TestSixthCapabilityReadIsCachedButMovedBinaryStillFails(t *testing.T) {
	bundle := interactionBundle(t)
	previous := interactionReadCapabilities
	reads := 0
	interactionReadCapabilities = func(path string) (claudeHookCapabilities, bool) { reads++; return previous(path) }
	t.Cleanup(func() { interactionReadCapabilities = previous })
	for i := 0; i < 5; i++ {
		if !bundleSupportsInteraction(bundle) {
			t.Fatal("capable bundle rejected")
		}
	}
	if reads != 1 {
		t.Fatalf("immutable capabilities read %d times", reads)
	}
	if err := os.Chmod(filepath.Join(bundle, "capabilities.json"), 0400); err != nil {
		t.Fatal(err)
	}
	if !bundleSupportsInteraction(bundle) || reads != 2 {
		t.Fatal("capability permission change did not invalidate the cached read")
	}
	if err := os.Chmod(filepath.Join(bundle, "capabilities.json"), 0600); err != nil {
		t.Fatal(err)
	}
	// A changed manifest must not reuse the old capability decision, and moving
	// the executable must still disable an enabled bundle after a cache hit.
	executable := filepath.Join(t.TempDir(), "synthetic-exec")
	if err := os.WriteFile(executable, []byte("synthetic"), 0700); err != nil {
		t.Fatal(err)
	}
	caps := claudeHookCapabilities{Interaction: 1, Exec: executable}
	raw, _ := json.Marshal(caps)
	if err := os.WriteFile(filepath.Join(bundle, "capabilities.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if !bundleSupportsInteraction(bundle) {
		t.Fatal("changed capable manifest rejected")
	}
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	if bundleSupportsInteraction(bundle) {
		t.Fatal("moved executable retained capability")
	}
}

func TestSixthCancelIndexDoesNotRescanOrphanAcknowledgements(t *testing.T) {
	bundle := interactionBundle(t)
	now := time.Now()
	answer, err := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), claudeInteractionReply{Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	// Historical acknowledgements without a request or cancellation fence
	// cannot settle any turn and must not become pending candidates.
	for _, id := range []string{"old-a", "old-b"} {
		if err := noteClaudeInteractionEmission(bundle, id, claudeInteractionAck{Emitted: true}); err != nil {
			t.Fatal(err)
		}
		if err := exclWrite(filepath.Join(bundle, "perm", "interact", "ans", id+".json"), answer.Stdout); err != nil {
			t.Fatal(err)
		}
	}
	rec := sampleRequest(bundle, now)
	rec.ID = "current-cancel"
	rec.Turn = "accepted-turn"
	publishEmittedCancel(t, bundle, rec)
	ids := pendingCancelInteractions(bundle)
	if len(ids) != 1 || ids[0] != rec.ID {
		t.Fatalf("orphan acknowledgements remained in pending index: %v", ids)
	}
	if err := retainInteractionCancelFence(bundle, rec); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundle, "perm", "interact", "req", rec.ID+".json")); err != nil {
		t.Fatal(err)
	}
	ids = pendingCancelInteractions(bundle)
	if len(ids) != 1 || ids[0] != rec.ID {
		t.Fatal("retired cancellation lost its pending index")
	}
}

func TestSixthAuditOutcomeAppendFailureIsReported(t *testing.T) {
	bundle := interactionBundle(t)
	now := time.Now()
	rec := sampleRequest(bundle, now)
	publishInteraction(t, bundle, rec)
	audits := 0
	_, err := submitClaudeInteractionClock(bundle, sampleCaller(bundle), rec.ID, letterReply(), func() time.Time { return now }, func(sessionlog.Event) error {
		audits++
		if audits == 1 {
			now = now.Add(2 * time.Minute)
			return nil
		}
		return os.ErrPermission
	})
	if audits != 2 || !errors.Is(err, errInteractionAudit) {
		t.Fatalf("outcome append failure: audits=%d err=%v", audits, err)
	}
	if _, err := readInteractionAns(bundle, rec.ID); !os.IsNotExist(err) {
		t.Fatal("failed audit outcome published an answer")
	}
}

func TestSixthCachesRemainBoundedAndFailClosed(t *testing.T) {
	bundle := interactionBundle(t)
	if err := os.WriteFile(filepath.Join(bundle, "capabilities.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if bundleSupportsInteraction(bundle) {
		t.Fatal("malformed capability manifest accepted")
	}
	path := filepath.Join(t.TempDir(), "prefix.jsonl")
	line := userLine("", "synthetic prompt")
	writeTranscript(t, path, strings.Repeat(line, 3))
	cache := &claudeInterruptPrefixCache{}
	for n := 1; n <= 3; n++ {
		snapshot, err := cachedInterruptPrefix(path, int64(n*len(line)), cache)
		if err != nil {
			t.Fatal(err)
		}
		if offset, _ := snapshot.Progress(); offset != int64(n*len(line)) {
			t.Fatal("prefix was not read completely")
		}
		if len(cache.entries) > 2 {
			t.Fatal("accepted-turn cache grew without a bound")
		}
	}
	if _, err := interruptPrefixTailer(path, 0); err == nil {
		t.Fatal("empty committed prefix accepted")
	}
	for _, raw := range []string{`"ok" trailing`, `"\ud800\u0041"`, `"\ud800\uZZZZ"`, `"\udc00\ud800"`} {
		if _, ok := decodeJSONStringValue([]byte(raw)); ok {
			t.Fatalf("invalid Unicode or trailing data accepted: %s", raw)
		}
	}
	if _, ok := decodeJSONStringValue([]byte{'"', 0xff, '"'}); ok {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestSixthHelperWaiterFailuresNeverPublish(t *testing.T) {
	for _, failure := range []string{"open", "lock"} {
		t.Run(failure, func(t *testing.T) {
			bundle := interactionBundle(t)
			id := "waiter-failure"
			oldSync, oldFlock := interactionPublishSync, interactionFlock
			t.Cleanup(func() { interactionPublishSync = oldSync; interactionFlock = oldFlock })
			if failure == "open" {
				interactionPublishSync = func(path string) error {
					if filepath.Base(path) == id {
						return os.Remove(path)
					}
					return oldSync(path)
				}
			} else {
				interactionFlock = func(int, int) error { return syscall.EIO }
			}
			var out bytes.Buffer
			_, err := serveClaudeInteraction(bundle, claudePermissionEvent{SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw()}, id, &out, time.Minute, time.Millisecond, time.Now, time.Sleep)
			if err == nil || out.Len() != 0 {
				t.Fatalf("helper failure emitted: %v %q", err, out.String())
			}
			if _, ok := readInteractionActive(bundle); ok {
				t.Fatal("unlocked waiter published ownership")
			}
		})
	}
	bundle := interactionBundle(t)
	rec := sampleRequest(bundle, time.Now())
	rec.HelperWaiter = true
	if !interactionHelperExited(bundle, rec) {
		t.Fatal("missing waiter was treated as alive")
	}
}

func TestSixthLifecycleGuardsStillProtectMalformedAndLegacyRequests(t *testing.T) {
	bundle := interactionBundle(t)
	now := time.Now()
	rec := sampleRequest(bundle, now)
	publishInteraction(t, bundle, rec)
	if err := noteClaudeInteractionEmission(bundle, rec.ID, claudeInteractionAck{Emitted: false, Error: "stdout"}); err != nil {
		t.Fatal(err)
	}
	if phase := claudeInteractionPhase(bundle, rec.ID, now); phase != "unconfirmed" {
		t.Fatalf("legacy failed emission phase: %s", phase)
	}
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "expired", rec.ID), []byte("expired")); err != nil {
		t.Fatal(err)
	}
	if !interactionSubmissionExpired(bundle, rec.ID, rec, now) {
		t.Fatal("explicit expiry fence ignored")
	}
	if _, err := readInteractionRequest("relative", rec.ID); !errors.Is(err, errInteractionBadRequest) {
		t.Fatal("unsafe request path accepted")
	}
	if _, ok := readInteractionCancelFence("relative", rec.ID); ok {
		t.Fatal("unsafe cancel path accepted")
	}
	if mark := claudeInterruptWatermarkOf(nil, "absent"); mark.path != "" || mark.offset != 0 {
		t.Fatal("missing watermark invented freshness")
	}
	if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "req", "not-json"), []byte("irrelevant"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(bundle, "perm", "interact", "req", "directory.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if ids := pendingCancelInteractions(bundle); len(ids) != 0 {
		t.Fatalf("non-emitted or malformed records entered cancel index: %v", ids)
	}
	// A corrupt request with a real emitted cancel is a candidate but cannot
	// retire an accepted turn without a readable request or minimal fence.
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	rec = sampleRequest(bundle, now)
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	publishEmittedCancel(t, bundle, rec)
	if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "req", rec.ID+".json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	a.noteClaudeInteractionInterrupt(n, pollLines(t, interruptLine("2099-01-01T00:00:00Z", "[Request interrupted by user]")))
	if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
		t.Fatal("corrupt request settled a turn")
	}
}

func TestSixthFenceFailuresRefuseEmissionAndRetirement(t *testing.T) {
	bundle := interactionBundle(t)
	now := time.Now()
	rec := sampleRequest(bundle, now)
	publishInteraction(t, bundle, rec)
	old := interactionFlock
	interactionFlock = func(int, int) error { return syscall.EIO }
	t.Cleanup(func() { interactionFlock = old })
	var out bytes.Buffer
	if _, err := emitInteractionAnswer(bundle, rec.ID, []byte(`{"synthetic":true}`), &out, time.Now, now.Add(time.Minute)); err == nil || out.Len() != 0 {
		t.Fatal("broken fence emitted stdout")
	}
	expireClaudeInteractionSession(bundle, rec.SessionID)
	if id, ok := readInteractionActive(bundle); !ok || id != rec.ID {
		t.Fatal("broken fence retired active ownership")
	}
}

func TestSixthFailedPublicationCannotEmitPreviouslyReadAnswer(t *testing.T) {
	bundle := interactionBundle(t)
	now := time.Now()
	rec := sampleRequest(bundle, now)
	publishInteraction(t, bundle, rec)
	previous := interactionPublishSync
	var readBeforeFailure []byte
	interactionPublishSync = func(path string) error {
		if filepath.Dir(path) == filepath.Join(bundle, "perm", "interact", "ans") {
			var err error
			readBeforeFailure, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			return os.ErrPermission
		}
		return previous(path)
	}
	t.Cleanup(func() { interactionPublishSync = previous })
	_, err := submitClaudeInteraction(bundle, sampleCaller(bundle), rec.ID, letterReply(), now, func(sessionlog.Event) error { return nil })
	if err == nil || len(readBeforeFailure) == 0 {
		t.Fatal("failed publication schedule was not exercised")
	}
	interactionPublishSync = previous
	var out bytes.Buffer
	_, err = emitInteractionAnswer(bundle, rec.ID, readBeforeFailure, &out, func() time.Time { return now }, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatal("helper emitted bytes from an answer whose publication failed")
	}
}

func TestSixthCancelAuditDescribesTheRequest(t *testing.T) {
	env, err := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), claudeInteractionReply{Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.Audit.AppContext, "requested interruption") {
		t.Fatal("intent audit asserted an interruption already happened")
	}
}

func TestSixthClearFailureBeforeCommitPreservesPreviousBinding(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	a := newTestApp(t, &fakeTmux{})
	n, _ := seedPermClaude(t, a, "n1", hookSIDOwn)
	old := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("old", 0))
	next := writeClaudeProject(t, a.home, "-w-proj", hookSIDOther, claudeUserLine("new", 0))
	n.Transcript, n.Dir = old, "/w/proj"
	a.claudeGens[n.ID] = 1
	tailer := &transcript.Tailer{Path: old}
	a.tailers[n.ID] = tailer
	blocked := filepath.Join(t.TempDir(), "store-directory")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	originalStore := a.storePath
	a.claudeAfterCandidate = func(string) { a.storePath = blocked }
	t.Cleanup(func() { a.storePath = originalStore })
	ev := claudeSessionStartEvent{HookEventName: "SessionStart", Source: "clear", SessionID: hookSIDOther, TranscriptPath: next, Cwd: n.Dir}
	if err := a.processClaudeHookEventAt(n.ID, ev, 1); err == nil {
		t.Fatal("failed binding append succeeded")
	}
	if n.Transcript != old || n.SessionID != hookSIDOwn || a.tailers[n.ID] != tailer {
		t.Fatal("pre-commit failure retired the previous binding")
	}
	if a.claudeLaunchError(n.ID) != claudeResumeExplain {
		t.Fatal("pre-commit failure reported a committed binding")
	}
	if a.pathClaims[next] || a.isDeadTranscript(n.ID, old, hookSIDOwn) {
		t.Fatal("pre-commit failure retained a claim or tombstone")
	}
}
