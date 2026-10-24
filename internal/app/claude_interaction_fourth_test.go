package app

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/transcript"
)

// fourthCancel begins an accepted turn and publishes one emitted cancellation
// for it. The transcript path must already be the file whose boundary the
// test will poll.
func fourthCancel(t *testing.T, a *app, n *Node, bundle, id string) claudeAcceptedTurn {
	t.Helper()
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	rec := sampleRequest(bundle, time.Now().UTC())
	rec.ID = id
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	rec.SessionID = n.SessionID
	_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
	publishEmittedCancel(t, bundle, rec)
	return turn
}

func interruptLine(stamp, text string) string {
	if stamp == "" {
		return `{"type":"user","message":{"role":"user","content":"` + text + `"}}` + "\n"
	}
	return `{"type":"user","timestamp":"` + stamp + `","message":{"role":"user","content":"` + text + `"}}` + "\n"
}

func userLine(stamp, text string) string {
	return `{"type":"user","timestamp":"` + stamp + `","message":{"role":"user","content":"` + text + `"}}` + "\n"
}

func writeTranscript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendTranscript(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func touchAfter(t *testing.T, path string, at time.Time) {
	t.Helper()
	when := at.Add(time.Second)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func pollTranscript(t *testing.T, path string) *transcript.Tailer {
	t.Helper()
	tl := &transcript.Tailer{Path: path}
	_ = tl.Poll()
	return tl
}

func mustSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func TestRecoveredFenceFinishesCommittedRebind(t *testing.T) {
	f := &fakeTmux{list: []string{"n1"}, alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	oldPath := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("keep", 0))
	nextPath := writeClaudeProject(t, a.home, "-w-proj", hookSIDOther, claudeUserLine("next", 0))
	n.Transcript = oldPath
	n.Dir = "/w/proj"
	a.mu.Lock()
	a.claudeGens[n.ID] = 1
	a.tailers[n.ID] = &transcript.Tailer{Path: oldPath}
	a.mirrors[n.ID] = &mirror{path: oldPath}
	a.chatMark[n.ID] = chatMark{seen: true}
	a.staleChat[n.ID] = true
	a.mu.Unlock()
	a.publishClaudeBindingFence(n.ID)
	body := elicitationEventJSON("Elicitation", hookSIDOwn, "docs-server", "Need a token", map[string]any{"mode": "form"})
	if err := RunClaudeElicitationHook(bundle, bytes.NewReader(body), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(listElicitationFiles(t, bundle)) != 1 {
		t.Fatal("setup: old-session elicitation was not recorded")
	}
	publishSyntheticInteraction(t, bundle, "old-session-helper", "AskUserQuestion", letterQuestionRaw(), hookSIDOwn)
	env, err := buildClaudeInteractionReply(mustClassify(t, "AskUserQuestion", letterQuestionRaw()), letterReply())
	if err != nil {
		t.Fatal(err)
	}
	if err := exclWrite(filepath.Join(bundle, "perm", "interact", "ans", "old-session-helper.json"), env.Stdout); err != nil {
		t.Fatal(err)
	}

	blocked := filepath.Join(bundle, "perm", "generation.json.tmp")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	ev := claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "resume", Cwd: n.Dir,
		SessionID: hookSIDOther, TranscriptPath: nextPath,
	}
	if err := a.processClaudeHookEventAt(n.ID, ev, 1); err == nil {
		t.Fatal("setup: obstructed publication unexpectedly succeeded")
	}
	if n.SessionID != hookSIDOther || n.Transcript != nextPath {
		t.Fatal("setup: the new binding was not committed")
	}
	if !a.isDeadTranscript(n.ID, oldPath, hookSIDOwn) {
		t.Fatal("setup: the committed rebind did not tombstone the old transcript")
	}
	msg := a.claudeLaunchError(n.ID)
	if msg == "" {
		t.Fatal("fence publication failure left no diagnostic")
	}
	if msg == claudeResumeExplain || bytes.Contains([]byte(msg), []byte("previous transcript is still attached")) {
		t.Fatalf("committed rebind was diagnosed as if the old transcript were still attached: %s", msg)
	}
	var replay bytes.Buffer
	if _, err := emitInteractionAnswer(bundle, "old-session-helper", env.Stdout, &replay, time.Now, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if replay.Len() != 0 {
		t.Fatal("old-session helper emitted after the new binding's fence publication failed")
	}
	if err := a.processClaudeHookEventAt(n.ID, ev, 1); err == nil {
		t.Fatal("retrying the same resume while fence publication is obstructed succeeded")
	}
	if msg := a.claudeLaunchError(n.ID); msg == "" || msg == claudeResumeExplain || bytes.Contains([]byte(msg), []byte("previous transcript is still attached")) {
		t.Fatalf("obstructed retry dropped the fence diagnostic: %q", msg)
	}

	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	testClaudeSessionHarness(a, n.ID).recoverInteractions(time.Now())
	if err := a.processClaudeHookEventAt(n.ID, ev, 1); err != nil {
		t.Fatal(err)
	}
	fence, ok := readClaudeBindingFence(bundle)
	if !ok || fence.SessionID != hookSIDOther || fence.Generation != 1 || fence.NodeID != n.ID {
		t.Fatalf("recovered fence = %+v ok=%v", fence, ok)
	}
	if msg := a.claudeLaunchError(n.ID); msg != "" {
		t.Fatalf("fence recovered and resume retry succeeded, but the launch error remains: %s", msg)
	}
	a.mu.Lock()
	_, tailerLeft := a.tailers[n.ID]
	_, mirrorLeft := a.mirrors[n.ID]
	_, markLeft := a.chatMark[n.ID]
	staleLeft := a.staleChat[n.ID]
	sup := a.claudeSupervisionOf(n)
	a.mu.Unlock()
	if tailerLeft || mirrorLeft || markLeft || staleLeft {
		t.Fatalf("committed rebind left poller state behind: tailer=%v mirror=%v mark=%v stale=%v", tailerLeft, mirrorLeft, markLeft, staleLeft)
	}
	if len(listElicitationFiles(t, bundle)) != 0 {
		t.Fatal("committed rebind left the old session's elicitation in place")
	}
	if sup != claudeSupStrict {
		t.Fatalf("supervision after recovery = %s", sup)
	}
	status, _, sendErr := a.sendTmuxPrompt(n, "synthetic follow-up", false)
	if sendErr != nil || status != 0 {
		t.Fatalf("ordinary send after recovery = %d %v", status, sendErr)
	}
	replay.Reset()
	if _, err := emitInteractionAnswer(bundle, "old-session-helper", env.Stdout, &replay, time.Now, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if replay.Len() != 0 {
		t.Fatal("old-session helper emitted after the recovered fence named the new session")
	}
}

func TestSameIdentityResumeKeepsUnrelatedLaunchError(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	cur := writeClaudeProject(t, a.home, "-w-proj", hookSIDOwn, claudeUserLine("keep", 0))
	n.Transcript = cur
	n.Dir = "/w/proj"
	a.recordClaudeLaunchError(n.ID, claudePasteExplain)
	ev := claudeSessionStartEvent{
		HookEventName: "SessionStart", Source: "resume", Cwd: n.Dir,
		SessionID: hookSIDOwn, TranscriptPath: cur,
	}
	if err := a.processClaudeHookEventAt(n.ID, ev, 1); err != nil {
		t.Fatal(err)
	}
	if got := a.claudeLaunchError(n.ID); got != claudePasteExplain {
		t.Fatalf("same-identity resume cleared an unrelated launch error: %q", got)
	}

	key := claudeResumeRemainderKey{app: a, nodeID: n.ID}
	claudeResumeRemainders.Store(key, "not-a-remainder")
	if err := a.processClaudeHookEventAt(n.ID, ev, 1); err != nil {
		t.Fatal(err)
	}
	if got := a.claudeLaunchError(n.ID); got != claudePasteExplain {
		t.Fatalf("corrupt remainder cleared an unrelated launch error: %q", got)
	}

	claudeResumeRemainders.Store(key, claudeResumeRemainder{
		sessionID: hookSIDOther, path: cur + ".other", bundle: bundle, gen: 1,
	})
	if err := a.processClaudeHookEventAt(n.ID, ev, 1); err != nil {
		t.Fatal(err)
	}
	if got := a.claudeLaunchError(n.ID); got != claudePasteExplain {
		t.Fatalf("a remainder for another session cleared an unrelated launch error: %q", got)
	}
	if _, ok := claudeResumeRemainders.Load(key); !ok {
		t.Fatal("a mismatched remainder was discarded")
	}

	claudeResumeRemainders.Store(key, claudeResumeRemainder{
		sessionID: hookSIDOwn, path: cur, oldPath: cur, oldSID: hookSIDOwn, bundle: bundle, gen: 1,
	})
	a.recordClaudeLaunchError(n.ID, claudeResumeFenceExplain)
	if err := os.Remove(filepath.Join(bundle, "perm", "generation.json")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	blocked := filepath.Join(bundle, "perm", "generation.json.tmp")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.processClaudeHookEventAt(n.ID, ev, 1); err == nil {
		t.Fatal("republishing an obstructed fence succeeded")
	}
	if got := a.claudeLaunchError(n.ID); got != claudeResumeFenceExplain {
		t.Fatalf("obstructed republish changed the fence diagnostic: %q", got)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	a.recordClaudeLaunchError(n.ID, claudePasteExplain)
	if err := a.processClaudeHookEventAt(n.ID, ev, 1); err != nil {
		t.Fatal(err)
	}
	if got := a.claudeLaunchError(n.ID); got != claudePasteExplain {
		t.Fatalf("republishing the fence cleared an unrelated launch error: %q", got)
	}
	if _, ok := claudeResumeRemainders.Load(key); ok {
		t.Fatal("a finished remainder was left behind")
	}
	a.recordClaudeLaunchError(n.ID, claudeResumeFenceExplain)
	if err := a.processClaudeHookEventAt(n.ID, ev, 1); err != nil {
		t.Fatal(err)
	}
	if got := a.claudeLaunchError(n.ID); got != claudeResumeFenceExplain {
		t.Fatalf("same-identity resume without a remainder cleared the fence diagnostic: %q", got)
	}
}

// An empty permission directory is not a path. Clearing it must leave a real
// accepted-turn file untouched.
func TestClearClaudeAcceptedTurnFileEmptyPermLeavesTurn(t *testing.T) {
	perm := filepath.Join(t.TempDir(), "perm")
	if err := os.Mkdir(perm, 0o755); err != nil {
		t.Fatal(err)
	}
	want := claudeAcceptedTurn{Turn: "kept-turn", Gen: 1, Session: hookSIDOwn}
	if err := writeClaudeAcceptedTurn(perm, want); err != nil {
		t.Fatal(err)
	}
	clearClaudeAcceptedTurnFile("")
	got, ok := readClaudeAcceptedTurn(perm)
	if !ok || got.Turn != want.Turn || got.Gen != want.Gen || got.Session != want.Session {
		t.Fatalf("empty perm cleared the accepted turn: ok=%v got=%+v", ok, got)
	}
}
