package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
)

// Synthetic schedule tests for the reviewed interaction defects.
// No agent CLI and no live capture.

func TestReviewHelperIgnoresUnfinishedAnswerFile(t *testing.T) {
	dir := interactionBundle(t)
	start := time.Now()
	clock := start
	var out bytes.Buffer
	var created string
	sleeps := 0
	handled, err := serveClaudeInteraction(dir, claudePermissionEvent{
		SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "review-partial", &out, time.Minute, time.Millisecond, func() time.Time { return clock }, func(time.Duration) {
		sleeps++
		if sleeps == 1 {
			created = filepath.Join(dir, "perm", "interact", "ans", "review-partial.json")
			f, openErr := os.OpenFile(created, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
			if openErr != nil {
				t.Fatal(openErr)
			}
			if closeErr := f.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			return
		}
		clock = start.Add(2 * time.Minute)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("helper declined a structured question")
	}
	ack, ok := readInteractionAck(dir, "review-partial")
	if out.Len() != 0 || (ok && ack.Emitted) {
		t.Fatalf("helper emitted an unfinished answer: stdout=%q ack=%+v ok=%v", out.String(), ack, ok)
	}
	if created == "" {
		t.Fatal("writer did not open the answer path")
	}
}

func TestReviewRevalidateTurnAfterAudit(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now()
	rec := sampleRequest(dir, now)
	rec.Turn = "review-turn-old"
	rec.TurnGen = 1
	old := claudeAcceptedTurn{Turn: rec.Turn, Gen: rec.TurnGen, Session: rec.SessionID}
	if err := writeClaudeAcceptedTurn(filepath.Join(dir, "perm"), old); err != nil {
		t.Fatal(err)
	}
	publishInteraction(t, dir, rec)
	caller := sampleCaller(dir)
	caller.Turn = rec.Turn
	caller.TurnGen = rec.TurnGen
	receipt, err := submitClaudeInteraction(dir, caller, rec.ID, letterReply(), now, func(sessionlog.Event) error {
		return writeClaudeAcceptedTurn(filepath.Join(dir, "perm"), claudeAcceptedTurn{Turn: "review-turn-next", Gen: 2, Session: rec.SessionID})
	})
	if err == nil || interactionFileExists(dir, "ans", rec.ID+".json") {
		t.Fatalf("accepted stale turn after audit: err=%v receipt=%+v", err, receipt)
	}
}

func TestReviewDeletionDuringAuditDoesNotPublish(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now()
	rec := sampleRequest(dir, now)
	publishInteraction(t, dir, rec)
	req := filepath.Join(dir, "perm", "interact", "req", rec.ID+".json")
	_, err := submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, letterReply(), now, func(sessionlog.Event) error {
		return os.Remove(req)
	})
	if err == nil || interactionFileExists(dir, "ans", rec.ID+".json") {
		t.Fatalf("published after the request was deleted: %v", err)
	}
}

func TestReviewRebindDuringAuditDoesNotPublish(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now()
	rec := sampleRequest(dir, now)
	rec.Generation = 4
	rec.NodeID = "node-a"
	publishInteraction(t, dir, rec)
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "generation.json"), map[string]any{"generation": 4, "node_id": "node-a"}); err != nil {
		t.Fatal(err)
	}
	caller := sampleCaller(dir)
	caller.Generation = 4
	_, err := submitClaudeInteraction(dir, caller, rec.ID, letterReply(), now, func(sessionlog.Event) error {
		return writeClaudePermFile(filepath.Join(dir, "perm", "generation.json"), map[string]any{"generation": 5, "node_id": "node-a"})
	})
	if err == nil || interactionFileExists(dir, "ans", rec.ID+".json") {
		t.Fatalf("published after rebind: %v", err)
	}
}

func TestReviewClosedTurnDuringAuditDoesNotPublish(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now()
	rec := sampleRequest(dir, now)
	rec.Turn = "review-turn-live"
	rec.TurnGen = 2
	if err := writeClaudeAcceptedTurn(filepath.Join(dir, "perm"), claudeAcceptedTurn{Turn: rec.Turn, Gen: rec.TurnGen, Session: rec.SessionID}); err != nil {
		t.Fatal(err)
	}
	publishInteraction(t, dir, rec)
	caller := sampleCaller(dir)
	caller.Turn = rec.Turn
	caller.TurnGen = rec.TurnGen
	_, err := submitClaudeInteraction(dir, caller, rec.ID, letterReply(), now, func(sessionlog.Event) error {
		clearClaudeAcceptedTurnFile(filepath.Join(dir, "perm"))
		return nil
	})
	if err == nil || interactionFileExists(dir, "ans", rec.ID+".json") {
		t.Fatalf("published after the turn file was removed: %v", err)
	}
}

func TestReviewHelperRejectsAnswerAfterDeadline(t *testing.T) {
	dir := interactionBundle(t)
	start := time.Now()
	clock := start
	var out bytes.Buffer
	handled, err := serveClaudeInteraction(dir, claudePermissionEvent{
		SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "review-expiry", &out, time.Minute, time.Millisecond, func() time.Time { return clock }, func(time.Duration) {
		clock = start.Add(2 * time.Minute)
		env, buildErr := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), letterReply())
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		if writeErr := exclWrite(filepath.Join(dir, "perm", "interact", "ans", "review-expiry.json"), env.Stdout); writeErr != nil {
			t.Fatal(writeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("helper declined")
	}
	if out.Len() > 0 {
		t.Fatalf("emitted a reply after the helper deadline: %s", out.String())
	}
	ack, ok := readInteractionAck(dir, "review-expiry")
	if ok && ack.Emitted {
		t.Fatalf("ack claims emission after the deadline: %+v", ack)
	}
}

func TestReviewHelperDoesNotEmitAfterTurnRevocation(t *testing.T) {
	dir := interactionBundle(t)
	start := time.Now()
	if err := writeClaudeAcceptedTurn(filepath.Join(dir, "perm"), claudeAcceptedTurn{Turn: "turn-live", Gen: 1, Session: hookSIDOwn}); err != nil {
		t.Fatal(err)
	}
	clock := start
	var out bytes.Buffer
	sleeps := 0
	handled, err := serveClaudeInteraction(dir, claudePermissionEvent{
		SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "review-revoke", &out, time.Minute, time.Millisecond, func() time.Time { return clock }, func(time.Duration) {
		sleeps++
		if sleeps > 2 {
			t.Fatal("helper did not decide after the turn was revoked")
		}
		if sleeps != 1 {
			return
		}
		clearClaudeAcceptedTurnFile(filepath.Join(dir, "perm"))
		env, buildErr := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), letterReply())
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		if writeErr := exclWrite(filepath.Join(dir, "perm", "interact", "ans", "review-revoke.json"), env.Stdout); writeErr != nil {
			t.Fatal(writeErr)
		}
	})
	if err != nil || !handled {
		t.Fatalf("helper err=%v handled=%v", err, handled)
	}
	if out.Len() != 0 {
		t.Fatalf("emitted after the accepted turn was revoked: %s", out.String())
	}
	ack, ok := readInteractionAck(dir, "review-revoke")
	if ok && ack.Emitted {
		t.Fatalf("ack claims emission after revocation: %+v", ack)
	}
}

func TestReviewSubmitRejectsWhenAuditPassesAnswerDeadline(t *testing.T) {
	dir := interactionBundle(t)
	start := time.Now()
	rec := sampleRequest(dir, start)
	rec.AnswerUntil = start.Add(time.Minute).UTC().Format(time.RFC3339Nano)
	rec.HelperUntil = start.Add(time.Hour).UTC().Format(time.RFC3339Nano)
	publishInteraction(t, dir, rec)
	clock := start
	_, err := submitClaudeInteractionClock(dir, sampleCaller(dir), rec.ID, letterReply(), func() time.Time { return clock }, func(sessionlog.Event) error {
		clock = start.Add(2 * time.Minute)
		return nil
	})
	if err == nil || interactionFileExists(dir, "ans", rec.ID+".json") {
		t.Fatalf("published after the answer deadline: %v", err)
	}
}

func TestReviewSubmitRejectsWhenAuditPassesHelperDeadline(t *testing.T) {
	dir := interactionBundle(t)
	start := time.Now()
	rec := sampleRequest(dir, start)
	rec.AnswerUntil = start.Add(time.Hour).UTC().Format(time.RFC3339Nano)
	rec.HelperUntil = start.Add(time.Minute).UTC().Format(time.RFC3339Nano)
	publishInteraction(t, dir, rec)
	clock := start
	_, err := submitClaudeInteractionClock(dir, sampleCaller(dir), rec.ID, letterReply(), func() time.Time { return clock }, func(sessionlog.Event) error {
		clock = start.Add(2 * time.Minute)
		return nil
	})
	if err == nil || interactionFileExists(dir, "ans", rec.ID+".json") {
		t.Fatalf("published after the helper deadline: %v", err)
	}
}

func TestReviewSubmitAcceptsInsideBothDeadlines(t *testing.T) {
	dir := interactionBundle(t)
	start := time.Now()
	rec := sampleRequest(dir, start)
	rec.AnswerUntil = start.Add(time.Hour).UTC().Format(time.RFC3339Nano)
	rec.HelperUntil = start.Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	publishInteraction(t, dir, rec)
	clock := start
	receipt, err := submitClaudeInteractionClock(dir, sampleCaller(dir), rec.ID, letterReply(), func() time.Time { return clock }, func(sessionlog.Event) error {
		clock = start.Add(time.Minute)
		return nil
	})
	if err != nil || !interactionFileExists(dir, "ans", rec.ID+".json") || receipt.Phase != "recorded" {
		t.Fatalf("inside both deadlines: err=%v receipt=%+v", err, receipt)
	}
}

func TestReviewSubmitRejectsWhenAuditPassesBothDeadlines(t *testing.T) {
	dir := interactionBundle(t)
	start := time.Now()
	rec := sampleRequest(dir, start)
	rec.AnswerUntil = start.Add(time.Minute).UTC().Format(time.RFC3339Nano)
	rec.HelperUntil = start.Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)
	publishInteraction(t, dir, rec)
	clock := start
	receipt, err := submitClaudeInteractionClock(dir, sampleCaller(dir), rec.ID, letterReply(), func() time.Time { return clock }, func(sessionlog.Event) error {
		clock = start.Add(3 * time.Minute)
		return nil
	})
	if err == nil || interactionFileExists(dir, "ans", rec.ID+".json") {
		t.Fatalf("published after both deadlines: err=%v receipt=%+v", err, receipt)
	}
}

func TestReviewDeadHelperReleasesRecordedOwnership(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now()
	rec := sampleRequest(dir, now)
	publishInteraction(t, dir, rec)
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, letterReply(), now, func(sessionlog.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	phase := claudeInteractionPhase(dir, rec.ID, now.Add(24*time.Hour))
	if !interactionNativeKeys(phase) {
		t.Fatalf("dead helper owns native controls: phase=%q", phase)
	}
}

func TestReviewWorkerPollRecoversDeadInteraction(t *testing.T) {
	a := newTestApp(t, &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	now := time.Now().UTC()
	rec := sampleRequest(bundle, now.Add(-24*time.Hour))
	rec.AnswerUntil = now.Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	rec.HelperUntil = now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	rec.SessionID = n.SessionID
	publishInteraction(t, bundle, rec)
	recorded := []byte(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`)
	if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "recorded", rec.ID), recorded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json"), recorded, 0o600); err != nil {
		t.Fatal(err)
	}
	h := testClaudeSessionHarness(a, n.ID)
	h.poll()
	if _, err := os.Stat(filepath.Join(bundle, "perm", "interact", "active.json")); !os.IsNotExist(err) {
		t.Fatal("worker poll left the dead helper's active ownership")
	}
	var replay bytes.Buffer
	clock := now
	_, err := serveClaudeInteraction(bundle, claudePermissionEvent{
		SessionID: n.SessionID, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, rec.ID, &replay, time.Minute, time.Millisecond, func() time.Time { return clock }, func(time.Duration) {
		clock = now.Add(time.Minute)
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay.Len() != 0 {
		t.Fatalf("recovered helper replayed an uncertain answer: %s", replay.String())
	}
	nextClock := now
	_, err = serveClaudeInteraction(bundle, claudePermissionEvent{
		SessionID: n.SessionID, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "req-future", &bytes.Buffer{}, time.Minute, time.Millisecond, func() time.Time { return nextClock }, func(time.Duration) {
		nextClock = now.Add(time.Minute)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(bundle, "perm", "interact", "req", "req-future.json")); statErr != nil {
		t.Fatalf("stale ownership still blocks a later request: %v", statErr)
	}
}

func TestReviewOldCancelCannotHideCurrentCancel(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	old, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	env, err := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), claudeInteractionReply{Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"a-review-old-cancel", "m-review-older-cancel"} {
		rec := sampleRequest(bundle, time.Now())
		rec.ID = id
		rec.Turn = old.Turn
		rec.TurnGen = old.Gen
		if i == 0 {
			publishInteraction(t, bundle, rec)
		} else if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "req", rec.ID+".json"), rec); err != nil {
			t.Fatal(err)
		}
		if err := exclWrite(filepath.Join(bundle, "perm", "interact", "ans", rec.ID+".json"), env.Stdout); err != nil {
			t.Fatal(err)
		}
		if err := noteClaudeInteractionEmission(bundle, rec.ID, claudeInteractionAck{Emitted: true}); err != nil {
			t.Fatal(err)
		}
	}
	a.endClaudePermissionTurn(n)
	_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
	next, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	current := sampleRequest(bundle, time.Now())
	current.ID = "z-review-current-cancel"
	current.Turn = next.Turn
	current.TurnGen = next.Gen
	publishInteraction(t, bundle, current)
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "ans", current.ID+".json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
	if err := noteClaudeInteractionEmission(bundle, current.ID, claudeInteractionAck{Emitted: true}); err != nil {
		t.Fatal(err)
	}
	a.settleEmittedClaudeInteractionCancels(n)
	if got := a.claudeAcceptedTurnOf(n.ID).Turn; got == next.Turn {
		t.Fatalf("historical cancels hid %s; turn still %s", current.ID, got)
	}
	if phase := claudeInteractionPhase(bundle, current.ID, time.Now()); phase != "interrupted" {
		t.Fatalf("current cancel phase = %s", phase)
	}
	if phase := claudeInteractionPhase(bundle, "a-review-old-cancel", time.Now()); phase == "interrupted" {
		t.Fatal("historical cancel was settled as the current interruption")
	}
}

func TestReviewPayloadRetirementCoversEveryOutcome(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now().UTC()
	answer := []byte(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`)
	type outcome struct {
		id     string
		ans    []byte
		extra  string
		ack    *claudeInteractionAck
		retire bool
	}
	rows := []outcome{
		{id: "req-answered", ans: answer, ack: &claudeInteractionAck{Emitted: true}, retire: true},
		{id: "req-canceled", ans: []byte(`{"hookSpecificOutput":{"decision":{"behavior":"deny","interrupt":true}}}`), extra: "interrupted", ack: &claudeInteractionAck{Emitted: true}, retire: true},
		{id: "req-expired", extra: "expired", retire: true},
		{id: "req-handoff", ans: []byte(`{"terminal":true}`), extra: "closed", ack: &claudeInteractionAck{Emitted: true, Terminal: true}, retire: true},
		{id: "req-unanswered", retire: true},
		{id: "req-uncertain", ans: answer, ack: &claudeInteractionAck{Emitted: false, Error: "stdout"}, retire: true},
	}
	for _, row := range rows {
		rec := sampleRequest(dir, now.Add(-time.Hour))
		rec.ID = row.id
		rec.HelperUntil = now.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
		rec.AnswerUntil = now.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
		if err := writeClaudePermFile(filepath.Join(dir, "perm", "interact", "req", rec.ID+".json"), rec); err != nil {
			t.Fatal(err)
		}
		if row.ans != nil {
			if err := os.WriteFile(filepath.Join(dir, "perm", "interact", "ans", rec.ID+".json"), row.ans, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "perm", "interact", "recorded", rec.ID), row.ans, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if row.extra != "" {
			if err := os.WriteFile(filepath.Join(dir, "perm", "interact", row.extra, rec.ID), []byte(row.extra), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if row.ack != nil {
			if err := noteClaudeInteractionEmission(dir, rec.ID, *row.ack); err != nil {
				t.Fatal(err)
			}
		}
	}
	fresh := sampleRequest(dir, now)
	fresh.ID = "req-fresh"
	fresh.HelperUntil = now.Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "interact", "req", fresh.ID+".json"), fresh); err != nil {
		t.Fatal(err)
	}
	if err := retireExpiredInteractionPayloads(dir, now); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if _, err := os.Stat(filepath.Join(dir, "perm", "interact", "req", row.id+".json")); !os.IsNotExist(err) {
			t.Fatalf("%s request payload outlived the helper deadline", row.id)
		}
		if row.ans != nil {
			if _, err := os.Stat(filepath.Join(dir, "perm", "interact", "ans", row.id+".json")); !os.IsNotExist(err) {
				t.Fatalf("%s response payload outlived the helper deadline", row.id)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "perm", "interact", "req", fresh.ID+".json")); err != nil {
		t.Fatal("fresh request was retired")
	}
	var replay bytes.Buffer
	clock := now
	_, err := serveClaudeInteraction(dir, claudePermissionEvent{
		SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "req-answered", &replay, time.Minute, time.Millisecond, func() time.Time { return clock }, func(time.Duration) {
		clock = now.Add(time.Minute)
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay.Len() != 0 {
		t.Fatalf("retired answer was replayed: %s", replay.String())
	}
}

type reviewBlockFile struct {
	f       *os.File
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *reviewBlockFile) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.f.Write(p)
}

func (b *reviewBlockFile) Sync() error  { return b.f.Sync() }
func (b *reviewBlockFile) Close() error { return b.f.Close() }

type reviewLieFile struct{ f *os.File }

func (l reviewLieFile) Write(p []byte) (int, error) {
	if _, err := l.f.Write(bytes.Repeat([]byte("y"), len(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (l reviewLieFile) Sync() error  { return l.f.Sync() }
func (l reviewLieFile) Close() error { return l.f.Close() }

type reviewDropFile struct{ f *os.File }

func (d reviewDropFile) Write(p []byte) (int, error) { return d.f.Write(p) }
func (d reviewDropFile) Sync() error                 { return d.f.Sync() }
func (d reviewDropFile) Close() error {
	name := d.f.Name()
	err := d.f.Close()
	_ = os.Remove(name)
	return err
}

type reviewErrFile struct{}

func (reviewErrFile) Write([]byte) (int, error) { return 0, errors.New("write") }
func (reviewErrFile) Sync() error               { return nil }
func (reviewErrFile) Close() error              { return nil }

func TestReviewPublicationIsAtomic(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "active.json")
	payload := []byte(`{"id":"owned"}`)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- publishExclusive(final, payload, func(name string) (interactionSyncFile, error) {
			f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return nil, err
			}
			return &reviewBlockFile{f: f, started: started, release: release}, nil
		})
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("publication did not reach the write")
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatal("ownership marker was visible before the write finished")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(final); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("published %#q err=%v", got, err)
	}

	if err := publishExclusive(filepath.Join(dir, "empty.json"), nil, openExclusivePublish); err == nil {
		t.Fatal("empty payload was published")
	}
	if err := publishExclusive(filepath.Join(dir, "bad.json"), []byte("{"), openExclusivePublish); err == nil {
		t.Fatal("partial json was published")
	}
	if _, err := os.Stat(filepath.Join(dir, "empty.json")); !os.IsNotExist(err) {
		t.Fatal("empty publication created a file")
	}
	if _, err := os.Stat(filepath.Join(dir, "bad.json")); !os.IsNotExist(err) {
		t.Fatal("partial json created a file")
	}
	if err := publishExclusive(filepath.Join(dir, "marker"), []byte("claim"), nil); err == nil {
		t.Fatal("nil opener published")
	}

	origNonce := interactionPublishNonce
	origSync := interactionPublishSync
	t.Cleanup(func() {
		interactionPublishNonce = origNonce
		interactionPublishSync = origSync
	})
	interactionPublishNonce = func() (string, error) { return "", errors.New("rand") }
	if err := publishExclusive(filepath.Join(dir, "nonce.json"), payload, openExclusivePublish); err == nil {
		t.Fatal("nonce failure published")
	}
	interactionPublishNonce = origNonce

	if err := publishExclusive(filepath.Join(dir, "open.json"), payload, func(string) (interactionSyncFile, error) {
		return nil, errors.New("open")
	}); err == nil {
		t.Fatal("open failure published")
	}
	if _, err := os.Stat(filepath.Join(dir, "open.json")); !os.IsNotExist(err) {
		t.Fatal("open failure created the final file")
	}
	if err := publishExclusive(filepath.Join(dir, "write.json"), payload, func(string) (interactionSyncFile, error) {
		return reviewErrFile{}, nil
	}); err == nil {
		t.Fatal("write failure published")
	}
	if _, err := os.Stat(filepath.Join(dir, "write.json")); !os.IsNotExist(err) {
		t.Fatal("write failure created the final file")
	}
	if err := publishExclusive(filepath.Join(dir, "lie.json"), payload, func(name string) (interactionSyncFile, error) {
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		return reviewLieFile{f: f}, nil
	}); err == nil {
		t.Fatal("reread mismatch published")
	}
	if _, err := os.Stat(filepath.Join(dir, "lie.json")); !os.IsNotExist(err) {
		t.Fatal("reread mismatch left the final file")
	}
	if err := publishExclusive(filepath.Join(dir, "drop.json"), payload, func(name string) (interactionSyncFile, error) {
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		return reviewDropFile{f: f}, nil
	}); err == nil {
		t.Fatal("reread failure published")
	}
	exist := filepath.Join(dir, "exist.json")
	if err := os.WriteFile(exist, []byte(`{"id":"kept"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishExclusive(exist, payload, openExclusivePublish); err == nil {
		t.Fatal("link over an existing file succeeded")
	}
	if got, err := os.ReadFile(exist); err != nil || !bytes.Equal(got, []byte(`{"id":"kept"}`)) {
		t.Fatalf("existing file changed: %s %v", got, err)
	}
	interactionPublishSync = func(string) error { return errors.New("sync") }
	if err := publishExclusive(filepath.Join(dir, "sync.json"), payload, openExclusivePublish); err == nil {
		t.Fatal("sync failure published")
	}
	if _, err := os.Stat(filepath.Join(dir, "sync.json")); !os.IsNotExist(err) {
		t.Fatal("sync failure left the final file")
	}
	interactionPublishSync = origSync
	matches, err := filepath.Glob(filepath.Join(dir, ".pub-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary publication files remained: %v %v", matches, err)
	}
}

func TestReviewFenceAndRecoveryPaths(t *testing.T) {
	if !interactionDeadlineOpen(time.Now(), time.Time{}) {
		t.Fatal("a zero deadline expired")
	}
	due := time.Now()
	if interactionDeadlineOpen(due, due) {
		t.Fatal("a deadline at the current instant stayed open")
	}
	if interactionAnswerReady(nil) || interactionAnswerReady([]byte("{")) || interactionAnswerReady([]byte(`{"hookSpecificOutput":{"decision":{"behavior":"ask"}}}`)) {
		t.Fatal("incomplete answer was ready")
	}
	if !interactionAnswerReady([]byte(`{"hookSpecificOutput":{"decision":{"behavior":"deny"}}}`)) || !interactionAnswerReady([]byte(`{"terminal":true}`)) {
		t.Fatal("complete answer was not ready")
	}
	dir := interactionBundle(t)
	origFlock := interactionFlock
	t.Cleanup(func() { interactionFlock = origFlock })
	if _, err := lockInteractionFence("relative/bundle"); err == nil {
		t.Fatal("relative bundle locked")
	}
	unlock, err := lockInteractionFence(filepath.Join(dir, "missing-child"))
	if err != nil || unlock == nil {
		t.Fatalf("missing interact lock = %v", err)
	}
	unlock()
	interact := filepath.Join(dir, "perm", "interact")
	if err := os.RemoveAll(interact); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(interact, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lockInteractionFence(dir); err == nil {
		t.Fatal("file fence locked")
	}
	if err := os.Remove(interact); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(interact, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, sub := range interactionDirs() {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(interact, 0o700); _ = os.Chmod(filepath.Join(dir, "perm"), 0o700) })
	if err := os.Chmod(interact, 0o555); err != nil {
		t.Fatal(err)
	}
	if _, err := lockInteractionFence(dir); err == nil {
		t.Fatal("read-only interact directory locked")
	}
	if err := os.Chmod(interact, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "turn.json"), claudeAcceptedTurn{Turn: "turn-clear", Gen: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(interact, 0o555); err != nil {
		t.Fatal(err)
	}
	clearClaudeAcceptedTurnFile(filepath.Join(dir, "perm"))
	if err := os.Chmod(interact, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "perm", "turn.json")); !os.IsNotExist(err) {
		t.Fatal("turn file survived a lock failure")
	}
	if err := os.Chmod(interact, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeAcceptedTurn(filepath.Join(dir, "perm"), claudeAcceptedTurn{Turn: "turn-block", Gen: 2}); err == nil {
		t.Fatal("turn publication ignored the lock failure")
	}
	if err := os.Chmod(interact, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "perm"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := lockInteractionFence(dir); err == nil {
		t.Fatal("unsearchable perm directory locked")
	}
	if err := os.Chmod(filepath.Join(dir, "perm"), 0o700); err != nil {
		t.Fatal(err)
	}

	interactionFlock = func(int, int) error { return errors.New("flock") }
	now := time.Now()
	rec := sampleRequest(dir, now)
	rec.ID = "req-flock"
	publishInteraction(t, dir, rec)
	env, err := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), letterReply())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(interact, "ans", rec.ID+".json"), env.Stdout, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	handled, err := serveClaudeInteraction(dir, claudePermissionEvent{
		SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, rec.ID, &out, time.Minute, time.Millisecond, func() time.Time { return now }, func(time.Duration) {
		t.Fatal("lock failure waited")
	})
	if !handled || err == nil || out.Len() != 0 {
		t.Fatalf("flock handled=%v err=%v stdout=%q", handled, err, out.String())
	}
	interactionFlock = origFlock

	recoverClaudeInteractionBundle(t.TempDir(), now)
	bare := interactionBundle(t)
	if err := os.RemoveAll(filepath.Join(bare, "perm", "interact")); err != nil {
		t.Fatal(err)
	}
	recoverClaudeInteractionBundle(bare, now)
	var bareHarness *claudeSessionHarness
	bareHarness.recoverInteractions(now)
	(&claudeSessionHarness{}).recoverInteractions(now)
	h := testClaudeSessionHarness(newTestApp(t, &fakeTmux{}), "missing-node")
	h.recoverInteractions(now)

	owned := interactionBundle(t)
	if err := os.WriteFile(filepath.Join(owned, "perm", "interact", "owner"), []byte("legacy-owner"), 0600); err != nil {
		t.Fatal(err)
	}
	stale := sampleRequest(owned, now.Add(-2*time.Hour))
	stale.ID = "req-foreign-stale"
	stale.HelperUntil = now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	publishInteraction(t, owned, stale)
	fresh := sampleRequest(owned, now)
	fresh.ID = "req-still-live"
	if err := writeClaudePermFile(filepath.Join(owned, "perm", "interact", "req", fresh.ID+".json"), fresh); err != nil {
		t.Fatal(err)
	}
	recoverClaudeInteractionBundle(owned, now)
	if _, err := os.Stat(filepath.Join(owned, "perm", "interact", "active.json")); !os.IsNotExist(err) {
		t.Fatal("foreign owner blocked recovery of active ownership")
	}
	if _, err := os.Stat(filepath.Join(owned, "perm", "interact", "req", fresh.ID+".json")); err != nil {
		t.Fatal("recovery removed a live request")
	}

	zero := sampleRequest(dir, now)
	zero.ID = "req-zero-deadline"
	zero.HelperUntil = ""
	if err := writeClaudePermFile(filepath.Join(dir, "perm", "interact", "req", zero.ID+".json"), zero); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "perm", "interact", "recorded", zero.ID), []byte(`{"terminal":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if phase := claudeInteractionPhase(dir, zero.ID, now.Add(24*time.Hour)); phase != "recorded" {
		t.Fatalf("zero helper deadline phase = %s", phase)
	}
}

func TestReviewReplayFencesAndSubmissionFailures(t *testing.T) {
	dir := interactionBundle(t)
	now := time.Now()
	fences := []struct {
		id   string
		kind string
		name string
	}{
		{"req-fence-recorded", "recorded", "req-fence-recorded"},
		{"req-fence-waiter", "waiter", "req-fence-waiter"},
		{"req-fence-ack", "ack", "req-fence-ack.json"},
		{"req-fence-expired", "expired", "req-fence-expired"},
		{"req-fence-interrupted", "interrupted", "req-fence-interrupted"},
		{"req-fence-closed", "closed", "req-fence-closed"},
	}
	for _, fence := range fences {
		body := []byte(fence.id)
		if fence.kind == "ack" {
			body = []byte(`{"emitted":true}`)
		}
		if err := os.WriteFile(filepath.Join(dir, "perm", "interact", fence.kind, fence.name), body, 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		clock := now
		if _, err := serveClaudeInteraction(dir, claudePermissionEvent{
			SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
		}, fence.id, &out, time.Minute, time.Millisecond, func() time.Time { return clock }, func(time.Duration) {
			t.Fatalf("%s replay waited", fence.id)
		}); err != nil || out.Len() != 0 {
			t.Fatalf("%s replay err=%v stdout=%q", fence.id, err, out.String())
		}
	}

	missing := interactionBundle(t)
	if err := os.WriteFile(filepath.Join(missing, "perm", "interact", "active.json"), []byte(`{"id":"req-missing"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var missingOut bytes.Buffer
	if _, err := serveClaudeInteraction(missing, claudePermissionEvent{
		SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "req-missing", &missingOut, time.Minute, time.Millisecond, func() time.Time { return now }, func(time.Duration) {
		t.Fatal("missing request waited")
	}); err != nil || missingOut.Len() != 0 {
		t.Fatalf("missing request err=%v stdout=%q", err, missingOut.String())
	}

	closedOnly := interactionBundle(t)
	clock := now
	var closedOut bytes.Buffer
	if _, err := serveClaudeInteraction(closedOnly, claudePermissionEvent{
		SessionID: hookSIDOwn, ToolName: "AskUserQuestion", ToolInput: letterQuestionRaw(),
	}, "req-closed-only", &closedOut, time.Minute, time.Millisecond, func() time.Time { return clock }, func(time.Duration) {
		if err := os.WriteFile(filepath.Join(closedOnly, "perm", "interact", "closed", "req-closed-only"), []byte("closed"), 0o600); err != nil {
			t.Fatal(err)
		}
	}); err != nil || closedOut.Len() != 0 {
		t.Fatalf("closed-only err=%v stdout=%q", err, closedOut.String())
	}
	if ack, ok := readInteractionAck(closedOnly, "req-closed-only"); !ok || !ack.Terminal {
		t.Fatalf("closed-only ack = %+v ok=%v", ack, ok)
	}

	origNonce := interactionPublishNonce
	origSync := interactionPublishSync
	t.Cleanup(func() {
		interactionPublishNonce = origNonce
		interactionPublishSync = origSync
	})
	rec := sampleRequest(dir, now)
	rec.ID = "req-sync-claim"
	publishInteraction(t, dir, rec)
	interactionPublishSync = func(string) error { return errors.New("sync") }
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), rec.ID, letterReply(), now, func(sessionlog.Event) error { return nil }); err == nil || interactionFileExists(dir, "ans", rec.ID+".json") {
		t.Fatalf("syncing claim published: %v", err)
	}
	interactionPublishSync = origSync

	failAt := map[string]int{"req-record-fail": 2, "req-answer-fail": 3}
	for id, n := range failAt {
		item := sampleRequest(dir, now)
		item.ID = id
		_ = os.Remove(filepath.Join(dir, "perm", "interact", "active.json"))
		publishInteraction(t, dir, item)
		calls := 0
		interactionPublishNonce = func() (string, error) {
			calls++
			if calls == n {
				return "", errors.New("rand")
			}
			return origNonce()
		}
		_, err := submitClaudeInteraction(dir, sampleCaller(dir), item.ID, letterReply(), now, func(sessionlog.Event) error { return nil })
		interactionPublishNonce = origNonce
		if err == nil || interactionFileExists(dir, "ans", item.ID+".json") {
			t.Fatalf("%s published: %v", id, err)
		}
	}
	term := sampleRequest(dir, now)
	term.ID = "req-closed-fail"
	_ = os.Remove(filepath.Join(dir, "perm", "interact", "active.json"))
	publishInteraction(t, dir, term)
	calls := 0
	interactionPublishNonce = func() (string, error) {
		calls++
		if calls == 3 {
			return "", errors.New("rand")
		}
		return origNonce()
	}
	if _, err := submitClaudeInteraction(dir, sampleCaller(dir), term.ID, claudeInteractionReply{Action: "terminal"}, now, func(sessionlog.Event) error { return nil }); err == nil {
		t.Fatal("closed publication failure was accepted")
	}
	interactionPublishNonce = origNonce

	late := sampleRequest(dir, now)
	late.ID = "req-second-check"
	late.AnswerUntil = now.Add(time.Hour).UTC().Format(time.RFC3339Nano)
	late.HelperUntil = now.Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	_ = os.Remove(filepath.Join(dir, "perm", "interact", "active.json"))
	publishInteraction(t, dir, late)
	ticks := 0
	_, err := submitClaudeInteractionClock(dir, sampleCaller(dir), late.ID, letterReply(), func() time.Time {
		ticks++
		if ticks >= 4 {
			return now.Add(3 * time.Hour)
		}
		return now
	}, func(sessionlog.Event) error { return nil })
	if err == nil || interactionFileExists(dir, "ans", late.ID+".json") || interactionFileExists(dir, "recorded", late.ID) {
		t.Fatalf("second deadline check published: err=%v ticks=%d", err, ticks)
	}
	termLate := late
	termLate.ID = "req-second-terminal"
	_ = os.Remove(filepath.Join(dir, "perm", "interact", "active.json"))
	publishInteraction(t, dir, termLate)
	ticks = 0
	_, err = submitClaudeInteractionClock(dir, sampleCaller(dir), termLate.ID, claudeInteractionReply{Action: "terminal"}, func() time.Time {
		ticks++
		if ticks >= 4 {
			return now.Add(3 * time.Hour)
		}
		return now
	}, func(sessionlog.Event) error { return nil })
	if err == nil || interactionFileExists(dir, "ans", termLate.ID+".json") || interactionFileExists(dir, "closed", termLate.ID) {
		t.Fatalf("terminal second check published: err=%v ticks=%d", err, ticks)
	}

	locked := interactionBundle(t)
	lockRec := sampleRequest(locked, now)
	publishInteraction(t, locked, lockRec)
	interact := filepath.Join(locked, "perm", "interact")
	t.Cleanup(func() { _ = os.Chmod(interact, 0o700) })
	_, err = submitClaudeInteraction(locked, sampleCaller(locked), lockRec.ID, letterReply(), now, func(sessionlog.Event) error {
		return os.Chmod(interact, 0o555)
	})
	_ = os.Chmod(interact, 0o700)
	if err == nil || interactionFileExists(locked, "ans", lockRec.ID+".json") {
		t.Fatalf("fence failure published: %v", err)
	}
}

func TestReviewRemainingDecisionPaths(t *testing.T) {
	if interactionAnswerReady([]byte(`{"hookSpecificOutput":[]}`)) || interactionAnswerReady([]byte(`{"hookSpecificOutput":{"decision":"nope"}}`)) {
		t.Fatal("valid JSON of the wrong shape was treated as an answer")
	}
	dir := interactionBundle(t)
	if interactionTurnAuthoritative(dir, "req-missing") {
		t.Fatal("a request that cannot be read stayed authoritative")
	}

	active := filepath.Join(dir, "perm", "interact", "active.json")
	if err := os.WriteFile(active, []byte(`{"id":"other-request"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	removeInteractionActive(dir, "mine")
	if got, err := os.ReadFile(active); err != nil || string(got) != `{"id":"other-request"}` {
		t.Fatalf("different owner was removed: %q %v", got, err)
	}

	origNonce := interactionPublishNonce
	t.Cleanup(func() { interactionPublishNonce = origNonce })
	if err := os.WriteFile(active, []byte(`{"id":"req-expire-fail"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	interactionPublishNonce = func() (string, error) { return "", errors.New("rand") }
	if err := expireInteraction(dir, "req-expire-fail"); err == nil {
		t.Fatal("expiry publication failure was ignored")
	}
	if _, err := os.Stat(filepath.Join(dir, "perm", "interact", "expired", "req-expire-fail")); !os.IsNotExist(err) {
		t.Fatal("failed expiry left a marker")
	}
	if got, err := os.ReadFile(active); err != nil || string(got) != `{"id":"req-expire-fail"}` {
		t.Fatalf("failed expiry dropped active ownership: %q %v", got, err)
	}
	interactionPublishNonce = origNonce
	if err := markInteractionExpired("relative/bundle", "req-mark"); err == nil {
		t.Fatal("relative bundle published an expiry marker")
	}
	interactionPublishNonce = func() (string, error) { return "", errors.New("rand") }
	if err := markInteractionExpired(dir, "req-mark"); err == nil {
		t.Fatal("expiry marker publication failure was ignored")
	}
	if _, err := os.Stat(filepath.Join(dir, "perm", "interact", "expired", "req-mark")); !os.IsNotExist(err) {
		t.Fatal("failed marker publication left a file")
	}
	interactionPublishNonce = origNonce

	if err := exclWrite(filepath.Join(dir, "perm", "interact", "ack", "req-torn.json"), []byte(`{"emitted":false}`)); err != nil {
		t.Fatal(err)
	}
	if phase := claudeInteractionPhase(dir, "req-torn", time.Now()); phase != "unconfirmed" {
		t.Fatalf("torn non-emitted ack phase = %s", phase)
	}

	unclean := dir + "/."
	recoverClaudeInteractionBundle(unclean, time.Now())
	if _, err := os.Stat(filepath.Join(dir, "perm", "interact", "owner")); !os.IsNotExist(err) {
		t.Fatal("non-canonical bundle path mutated interaction state")
	}
	recoverClaudeInteractionBundle(dir, time.Now())
	if _, err := os.Stat(filepath.Join(dir, "perm", "interact", "owner")); !os.IsNotExist(err) {
		t.Fatal("canonical recovery created a redundant PID owner")
	}

	past := sampleRequest(dir, time.Now())
	past.ID = "req-wall-past"
	past.AnswerUntil = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	past.HelperUntil = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	_ = os.Remove(active)
	publishInteraction(t, dir, past)
	if _, err := submitClaudeInteractionClock(dir, sampleCaller(dir), past.ID, letterReply(), nil, func(sessionlog.Event) error { return nil }); err == nil || interactionFileExists(dir, "ans", past.ID+".json") {
		t.Fatalf("nil clock accepted a deadline already behind the wall clock: %v", err)
	}
	live := sampleRequest(dir, time.Now())
	live.ID = "req-wall-live"
	live.AnswerUntil = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	live.HelperUntil = time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	_ = os.Remove(active)
	publishInteraction(t, dir, live)
	receipt, err := submitClaudeInteractionClock(dir, sampleCaller(dir), live.ID, letterReply(), nil, func(sessionlog.Event) error { return nil })
	if err != nil || !interactionFileExists(dir, "ans", live.ID+".json") || receipt.Phase != "recorded" {
		t.Fatalf("nil clock rejected a request inside both wall-clock deadlines: err=%v receipt=%+v", err, receipt)
	}
}

func TestReviewUnreadableCancelDoesNotHideCurrent(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	env, err := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), claudeInteractionReply{Action: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "ans", "a-unreadable.json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
	if err := noteClaudeInteractionEmission(bundle, "a-unreadable", claudeInteractionAck{Emitted: true}); err != nil {
		t.Fatal(err)
	}
	next, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	current := sampleRequest(bundle, time.Now())
	current.ID = "z-review-readable-cancel"
	current.Turn = next.Turn
	current.TurnGen = next.Gen
	_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
	publishInteraction(t, bundle, current)
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "ans", current.ID+".json"), env.Stdout); err != nil {
		t.Fatal(err)
	}
	if err := noteClaudeInteractionEmission(bundle, current.ID, claudeInteractionAck{Emitted: true}); err != nil {
		t.Fatal(err)
	}
	a.settleEmittedClaudeInteractionCancels(n)
	if got := a.claudeAcceptedTurnOf(n.ID).Turn; got == next.Turn {
		t.Fatalf("unreadable cancel hid %s; turn still %s", current.ID, got)
	}
	if phase := claudeInteractionPhase(bundle, current.ID, time.Now()); phase != "interrupted" {
		t.Fatalf("current cancel phase = %s", phase)
	}
	if interactionFileExists(bundle, "interrupted", "a-unreadable") {
		t.Fatal("unreadable cancel was marked interrupted")
	}
}
