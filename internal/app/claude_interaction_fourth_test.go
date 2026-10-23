package app

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
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

func TestMetadataAppendCannotRefreshAnOldInterrupt(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	path := filepath.Join(t.TempDir(), "watched.jsonl")
	writeTranscript(t, path, interruptLine("", "[Request interrupted by user]"))
	n.Transcript = path
	tl := pollTranscript(t, path)
	if !tl.EndTurn() || tl.Delivered() {
		t.Fatal("setup: old undated interrupt was not a boundary")
	}
	turn := fourthCancel(t, a, n, bundle, "fourth-old-interrupt")
	appendTranscript(t, path, `{"type":"file-history-snapshot","snapshot":{}}`+"\n")
	touchAfter(t, path, parseInteractionTime(turn.At))
	_ = tl.Poll()
	a.noteClaudeInteractionInterrupt(n, tl)
	if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
		t.Fatal("an unrelated metadata append refreshed an interruption consumed before this accepted turn")
	}
}

func TestFreshUserTimestampCannotRefreshOldDatedInterrupt(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	path := filepath.Join(t.TempDir(), "dated.jsonl")
	writeTranscript(t, path, "")
	n.Transcript = path
	turn := fourthCancel(t, a, n, bundle, "fourth-old-dated")
	at := parseInteractionTime(turn.At)
	current := at.Add(time.Millisecond).Format(time.RFC3339Nano)
	previous := at.Add(-time.Hour).Format(time.RFC3339Nano)
	// Claude can flush an older stamp late. The stamp that matters is the
	// interruption's, not the maximum stamp on some other record.
	writeTranscript(t, path, userLine(current, "new synthetic prompt")+interruptLine(previous, "[Request interrupted by user]"))
	tl := pollTranscript(t, path)
	if !tl.EndTurn() || tl.Delivered() {
		t.Fatal("setup: old dated interrupt was not a boundary")
	}
	a.noteClaudeInteractionInterrupt(n, tl)
	if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
		t.Fatal("the newest timestamp from another record refreshed an explicitly old interruption")
	}
}

func TestInterruptFreshnessFollowsTheInterruptionRecord(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	notice := "[Request interrupted by user]"
	toolNotice := "[Request interrupted by user for tool use]"

	t.Run("fresh dated notice settles", func(t *testing.T) {
		turn := fourthCancel(t, a, n, bundle, "fresh-dated-notice")
		stamp := time.Now().UTC().Format(time.RFC3339Nano)
		tl := pollLines(t, interruptLine(stamp, notice))
		a.noteClaudeInteractionInterrupt(n, tl)
		if a.claudeAcceptedTurnOf(n.ID).Turn != "" || a.claudeClosingTurnOf(n.ID).Turn != turn.Turn {
			t.Fatal("a dated interruption after the turn did not settle")
		}
	})

	t.Run("fresh tool-use notice settles", func(t *testing.T) {
		_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
		a.closeClaudeAcceptedTurnLocked(n.ID)
		turn := fourthCancel(t, a, n, bundle, "fresh-tool-notice")
		stamp := time.Now().UTC().Format(time.RFC3339Nano)
		tl := pollLines(t, interruptLine(stamp, toolNotice))
		a.noteClaudeInteractionInterrupt(n, tl)
		if a.claudeAcceptedTurnOf(n.ID).Turn != "" || a.claudeClosingTurnOf(n.ID).Turn != turn.Turn {
			t.Fatal("a fresh tool-use interruption did not settle")
		}
	})

	t.Run("old tool-use notice does not borrow a later prompt stamp", func(t *testing.T) {
		_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
		a.closeClaudeAcceptedTurnLocked(n.ID)
		path := filepath.Join(t.TempDir(), "tool-old.jsonl")
		writeTranscript(t, path, "")
		n.Transcript = path
		turn := fourthCancel(t, a, n, bundle, "old-tool-notice")
		at := parseInteractionTime(turn.At)
		body := userLine(at.Add(time.Millisecond).Format(time.RFC3339Nano), "later prompt") +
			interruptLine(at.Add(-time.Hour).Format(time.RFC3339Nano), toolNotice)
		writeTranscript(t, path, body)
		tl := pollTranscript(t, path)
		a.noteClaudeInteractionInterrupt(n, tl)
		if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
			t.Fatal("a later prompt stamp refreshed an older tool-use interruption")
		}
	})

	t.Run("new undated notice after dated history settles", func(t *testing.T) {
		_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
		a.closeClaudeAcceptedTurnLocked(n.ID)
		path := filepath.Join(t.TempDir(), "history.jsonl")
		writeTranscript(t, path, userLine("2001-01-01T00:00:00Z", "older synthetic prompt"))
		n.Transcript = path
		tl := pollTranscript(t, path)
		turn := fourthCancel(t, a, n, bundle, "new-undated-after-history")
		appendTranscript(t, path, interruptLine("", notice))
		touchAfter(t, path, parseInteractionTime(turn.At))
		_ = tl.Poll()
		a.noteClaudeInteractionInterrupt(n, tl)
		if a.claudeAcceptedTurnOf(n.ID).Turn != "" {
			t.Fatal("a new undated interruption was rejected because an older record carried the only timestamp")
		}
	})

	t.Run("metadata after mixed history does not settle", func(t *testing.T) {
		_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
		a.closeClaudeAcceptedTurnLocked(n.ID)
		path := filepath.Join(t.TempDir(), "mixed.jsonl")
		writeTranscript(t, path, userLine("2001-01-01T00:00:00Z", "older synthetic prompt")+interruptLine("", notice))
		n.Transcript = path
		tl := pollTranscript(t, path)
		turn := fourthCancel(t, a, n, bundle, "mixed-metadata")
		appendTranscript(t, path, `{"type":"file-history-snapshot","snapshot":{"trackedFileBackups":{}}}`+"\n")
		touchAfter(t, path, parseInteractionTime(turn.At))
		_ = tl.Poll()
		a.noteClaudeInteractionInterrupt(n, tl)
		if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
			t.Fatal("metadata after dated history refreshed an undated interruption that was already consumed")
		}
	})

	t.Run("appended undated notice still settles", func(t *testing.T) {
		_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
		a.closeClaudeAcceptedTurnLocked(n.ID)
		path := filepath.Join(t.TempDir(), "append-real.jsonl")
		writeTranscript(t, path, interruptLine("", notice))
		n.Transcript = path
		tl := pollTranscript(t, path)
		turn := fourthCancel(t, a, n, bundle, "append-real-notice")
		appendTranscript(t, path, interruptLine("", notice))
		touchAfter(t, path, parseInteractionTime(turn.At))
		_ = tl.Poll()
		a.noteClaudeInteractionInterrupt(n, tl)
		if a.claudeAcceptedTurnOf(n.ID).Turn != "" {
			t.Fatal("a newly appended undated interruption did not settle")
		}
	})

	t.Run("read lag", func(t *testing.T) {
		_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
		a.closeClaudeAcceptedTurnLocked(n.ID)
		path := filepath.Join(t.TempDir(), "lag.jsonl")
		writeTranscript(t, path, "")
		n.Transcript = path
		turn := fourthCancel(t, a, n, bundle, "fourth-lag")
		appendTranscript(t, path, interruptLine("", notice)+`{"type":"user"`)
		touchAfter(t, path, parseInteractionTime(turn.At))
		tl := pollTranscript(t, path)
		a.noteClaudeInteractionInterrupt(n, tl)
		if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
			t.Fatal("an undated interruption with unread trailing bytes settled")
		}
	})

	t.Run("read failure", func(t *testing.T) {
		_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
		a.closeClaudeAcceptedTurnLocked(n.ID)
		turn := fourthCancel(t, a, n, bundle, "fourth-unreadable")
		tl := undatedInterruptTailer(t)
		if err := os.Chmod(tl.Path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(tl.Path, 0o600) })
		touchAfter(t, tl.Path, parseInteractionTime(turn.At))
		a.noteClaudeInteractionInterrupt(n, tl)
		if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
			t.Fatal("an unreadable undated transcript settled")
		}
	})

	t.Run("retired payload does not revive a stale notice", func(t *testing.T) {
		_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
		a.closeClaudeAcceptedTurnLocked(n.ID)
		path := filepath.Join(t.TempDir(), "retired.jsonl")
		writeTranscript(t, path, interruptLine("", notice))
		n.Transcript = path
		tl := pollTranscript(t, path)
		turn := fourthCancel(t, a, n, bundle, "fourth-retired")
		now := time.Now().UTC()
		rec := sampleRequest(bundle, now.Add(-2*time.Minute))
		rec.ID = "fourth-retired"
		rec.SessionID = n.SessionID
		rec.Turn = turn.Turn
		rec.TurnGen = turn.Gen
		rec.HelperUntil = now.Add(-time.Millisecond).UTC().Format(time.RFC3339Nano)
		if err := os.Remove(filepath.Join(bundle, "perm", "interact", "req", rec.ID+".json")); err != nil {
			t.Fatal(err)
		}
		if err := writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "req", rec.ID+".json"), rec); err != nil {
			t.Fatal(err)
		}
		testClaudeSessionHarness(a, n.ID).recoverInteractions(now)
		touchAfter(t, path, parseInteractionTime(turn.At))
		a.noteClaudeInteractionInterrupt(n, tl)
		if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
			t.Fatal("retiring the raw payload let a stale undated interruption settle")
		}
	})

	t.Run("successor", func(t *testing.T) {
		_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
		a.closeClaudeAcceptedTurnLocked(n.ID)
		path := filepath.Join(t.TempDir(), "successor.jsonl")
		writeTranscript(t, path, interruptLine("", notice))
		tl := pollTranscript(t, path)
		n.Transcript = path
		next, err := a.beginClaudeAcceptedTurn(n)
		if err != nil {
			t.Fatal(err)
		}
		rec := sampleRequest(bundle, time.Now().UTC())
		rec.ID = "fourth-successor"
		rec.SessionID = n.SessionID
		rec.Turn = next.Turn
		rec.TurnGen = next.Gen
		_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
		publishEmittedCancel(t, bundle, rec)
		touchAfter(t, path, parseInteractionTime(next.At))
		a.noteClaudeInteractionInterrupt(n, tl)
		if a.claudeAcceptedTurnOf(n.ID).Turn != next.Turn {
			t.Fatal("an undated interruption from before the successor settled it")
		}
	})

	t.Run("fresh prompt does not vouch for a notice that was not parsed", func(t *testing.T) {
		_ = os.Remove(filepath.Join(bundle, "perm", "interact", "active.json"))
		a.closeClaudeAcceptedTurnLocked(n.ID)
		path := filepath.Join(t.TempDir(), "unparsed-notice.jsonl")
		writeTranscript(t, path, "")
		n.Transcript = path
		turn := fourthCancel(t, a, n, bundle, "fourth-unparsed-notice")
		at := parseInteractionTime(turn.At)
		body := userLine(at.Add(time.Millisecond).Format(time.RFC3339Nano), "new synthetic prompt") +
			`{"type":"user","message":{"role":"user","content":""},"interruptedMessageId":"synthetic-1"}` + "\n"
		writeTranscript(t, path, body)
		tl := pollTranscript(t, path)
		if !tl.EndTurn() || tl.Delivered() {
			t.Fatal("setup: structural boundary was not an interruption")
		}
		a.noteClaudeInteractionInterrupt(n, tl)
		if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
			t.Fatal("a fresh prompt stamp settled an interruption that the tailer did not parse as that notice")
		}
	})
}

func TestInterruptPrefixFailuresDoNotInventABoundary(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	notice := "[Request interrupted by user]"
	toolNotice := "[Request interrupted by user for tool use]"
	if _, ok := interruptBoundaryTurn(nil); ok {
		t.Fatal("a missing tailer invented an interruption")
	}
	if _, ok := interruptBoundaryTurn(&transcript.Tailer{}); ok {
		t.Fatal("an empty tailer invented an interruption")
	}
	if interruptBoundaryAlreadyPresent(filepath.Join(t.TempDir(), "missing.jsonl"), 4, nil, nil) != true {
		t.Fatal("an unreadable prefix was treated as a new interruption")
	}
	path := filepath.Join(t.TempDir(), "prefix.jsonl")
	writeTranscript(t, path, interruptLine("", notice))
	live := pollTranscript(t, path)
	if interruptBoundaryAlreadyPresent(path, 0, live, nil) {
		t.Fatal("an empty prefix invented a boundary that was already present")
	}
	dir := t.TempDir()
	if !interruptBoundaryAlreadyPresent(dir, 1, live, nil) {
		t.Fatal("a prefix that cannot be read was treated as a new interruption")
	}
	spaces := filepath.Join(t.TempDir(), "spaces.jsonl")
	writeTranscript(t, spaces, " \n")
	if interruptBoundaryAlreadyPresent(spaces, 2, live, nil) {
		t.Fatal("whitespace before the turn was treated as the interruption")
	}
	prompt := filepath.Join(t.TempDir(), "prompt.jsonl")
	writeTranscript(t, prompt, userLine("2001-01-01T00:00:00Z", "older synthetic prompt"))
	if interruptBoundaryAlreadyPresent(prompt, int64(len(userLine("2001-01-01T00:00:00Z", "older synthetic prompt"))), live, nil) {
		t.Fatal("a prompt that is not an interruption notice was treated as one")
	}
	if !interruptBoundaryAlreadyPresent(path, int64(len(interruptLine("", notice))), &transcript.Tailer{}, nil) {
		t.Fatal("a live tailer that lost the notice was treated as a new interruption")
	}
	same := pollTranscript(t, path)
	if !interruptBoundaryAlreadyPresent(path, int64(len(interruptLine("", notice))), same, nil) {
		t.Fatal("the same undated notice was treated as new")
	}
	retimed := filepath.Join(t.TempDir(), "retimed.jsonl")
	writeTranscript(t, retimed, interruptLine("2001-01-01T00:00:00Z", notice))
	if interruptBoundaryAlreadyPresent(path, int64(len(interruptLine("", notice))), pollTranscript(t, retimed), nil) {
		t.Fatal("a dated copy of an undated notice was treated as the same boundary")
	}
	tool := filepath.Join(t.TempDir(), "tool.jsonl")
	writeTranscript(t, tool, interruptLine("", toolNotice))
	if interruptBoundaryAlreadyPresent(path, int64(len(interruptLine("", notice))), pollTranscript(t, tool), nil) {
		t.Fatal("a different interruption notice was treated as the same boundary")
	}
	delivered := filepath.Join(t.TempDir(), "delivered.jsonl")
	writeTranscript(t, delivered, `{"type":"assistant","timestamp":"2001-01-01T00:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}}`+"\n")
	if interruptBoundaryAlreadyPresent(delivered, int64(mustSize(t, delivered)), live, nil) {
		t.Fatal("a delivered completion was treated as an interruption that was already present")
	}
	garbage := filepath.Join(t.TempDir(), "garbage.jsonl")
	var unrecognized strings.Builder
	for i := 0; i < 10; i++ {
		unrecognized.WriteString("not-a-record\n")
	}
	writeTranscript(t, garbage, unrecognized.String())
	if !interruptBoundaryAlreadyPresent(garbage, int64(len(unrecognized.String())), live, nil) {
		t.Fatal("an unparseable prefix was treated as proof of a new interruption")
	}
	structural := filepath.Join(t.TempDir(), "structural.jsonl")
	structuralBody := userLine("2001-01-01T00:00:00Z", "older synthetic prompt") +
		`{"type":"user","message":{"role":"user","content":""},"interruptedMessageId":"synthetic-1"}` + "\n"
	writeTranscript(t, structural, structuralBody)
	structuralTL := pollTranscript(t, structural)
	if !structuralTL.EndTurn() || structuralTL.Delivered() {
		t.Fatal("setup: structural prefix was not an interruption boundary")
	}
	if interruptBoundaryAlreadyPresent(structural, int64(len(structuralBody)), live, nil) {
		t.Fatal("an interruption the tailer did not parse as a notice was treated as already present")
	}

	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	writeTranscript(t, blocked, "x")
	promptLen := int64(len(userLine("2001-01-01T00:00:00Z", "older synthetic prompt")))
	prevDir := interruptPrefixTempDir
	interruptPrefixTempDir = blocked
	t.Cleanup(func() { interruptPrefixTempDir = prevDir })
	if !interruptBoundaryAlreadyPresent(prompt, promptLen, live, nil) {
		t.Fatal("a prefix that cannot be copied was treated as a new interruption")
	}
	interruptPrefixTempDir = prevDir

	prevWrite := writeInterruptPrefix
	writeInterruptPrefix = func(*os.File, []byte) error { return os.ErrClosed }
	t.Cleanup(func() { writeInterruptPrefix = prevWrite })
	if !interruptBoundaryAlreadyPresent(prompt, promptLen, live, nil) {
		t.Fatal("a prefix copy that fails to write was treated as a new interruption")
	}
	writeInterruptPrefix = prevWrite

	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	watched := filepath.Join(t.TempDir(), "watched.jsonl")
	writeTranscript(t, watched, interruptLine("", notice))
	n.Transcript = watched
	turn := fourthCancel(t, a, n, bundle, "prefix-copy-failed")
	appendTranscript(t, watched, interruptLine("", notice))
	touchAfter(t, watched, parseInteractionTime(turn.At))
	interruptPrefixTempDir = blocked
	tl := pollTranscript(t, watched)
	a.noteClaudeInteractionInterrupt(n, tl)
	if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
		t.Fatal("a prefix that cannot be copied let an undated interruption settle")
	}
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
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
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
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
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
