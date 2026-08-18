package app

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkPermDirs creates the rendezvous layout a prepared bundle carries.
func mkPermDirs(t *testing.T, bundle string) {
	t.Helper()
	for _, d := range []string{"asked", "req", "ans", "processed/allowed", "processed/manual"} {
		if err := os.MkdirAll(filepath.Join(bundle, "perm", d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// Per-call notice retirement (2026-08-18). Growth-based retirement could not
// keep up with a working agent: it is gated on PendingCount()==0, and a busy
// Claude nearly always has a call in flight, so notices piled up. Observed in
// the maintainer's own store — 16 standing notices spanning 23 minutes, every
// one of them from a single prompt_id whose dialogs had long been answered by
// hand. A standing notice raises hard attention on an active pane, so the UI
// screamed for attention at an agent that was quietly working, and the peek it
// unfolded showed exactly that: a spinner and "esc to interrupt".
//
// The join that fixes it is exact rather than statistical. A notice records a
// digest of the tool_input it escalated, and Claude writes the matching
// tool_use record only *after* the human answers — so a record whose tool name
// and input digest match a standing notice is proof that that specific dialog
// closed. Verified against real data before it was built: the hook's bytes and
// the transcript's bytes hash identically, raw.

func digestOf(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:16]
}

// askedNoticeAt drops a notice for a given tool/digest at a given time.
func askedNoticeAt(t *testing.T, bundle, id, tool, digest string, at time.Time) {
	t.Helper()
	note := claudeAskedNotice{
		At:     at.UTC().Format(time.RFC3339Nano),
		Tool:   tool,
		Digest: digest,
	}
	if err := writeClaudePermFile(claudeAskedPath(filepath.Join(bundle, "perm"), id), note); err != nil {
		t.Fatal(err)
	}
}

func TestNoticeRetiredByItsOwnToolCall(t *testing.T) {
	// The crux: a matching record retires the notice even though another call
	// is still unresolved. That is the whole point — the old gate refused to
	// retire anything while any call was pending, which on a working agent is
	// nearly always.
	bundle := t.TempDir()
	mkPermDirs(t, bundle)
	input := `{"command":"date > x.txt"}`
	askedNoticeAt(t, bundle, "n-one", "Bash", digestOf(input), time.Now().Add(-time.Minute))

	seen := []toolEvidence{
		{ID: "tu1", Tool: "Bash", Digest: digestOf(input), At: time.Now()},
		{ID: "tu2", Tool: "Bash", Digest: digestOf(`{"command":"still running"}`), At: time.Now()},
	}
	if n := retireAskedByToolEvidence(filepath.Join(bundle, "perm"), seen, time.Now()); n != 1 {
		t.Fatalf("retired %d notices, want exactly the one whose call appeared", n)
	}
	if got := standingNotices(t, bundle); len(got) != 0 {
		t.Fatalf("notices still standing: %v", got)
	}
}

func TestNoticeSurvivesUnrelatedToolCall(t *testing.T) {
	// A different call is not evidence about this ask. Silence must cost a
	// notice nothing: the gate suppresses attention, so a wrong retirement
	// hides a dialog that is still on screen.
	bundle := t.TempDir()
	mkPermDirs(t, bundle)
	askedNoticeAt(t, bundle, "n-one", "Bash", digestOf(`{"command":"mine"}`), time.Now().Add(-time.Minute))

	seen := []toolEvidence{{ID: "tu9", Tool: "Bash", Digest: digestOf(`{"command":"other"}`), At: time.Now()}}
	if n := retireAskedByToolEvidence(filepath.Join(bundle, "perm"), seen, time.Now()); n != 0 {
		t.Fatalf("retired %d notices on an unrelated call", n)
	}
	if got := standingNotices(t, bundle); len(got) != 1 {
		t.Fatalf("notices = %v, want the one still standing", got)
	}
}

func TestSameToolNameDifferentInputDoesNotRetire(t *testing.T) {
	// Tool name alone is far too coarse: a session runs dozens of Bash calls.
	// The digest is what makes the join exact.
	bundle := t.TempDir()
	mkPermDirs(t, bundle)
	askedNoticeAt(t, bundle, "n-one", "Bash", digestOf(`{"command":"a"}`), time.Now().Add(-time.Minute))

	seen := []toolEvidence{{ID: "tu1", Tool: "Edit", Digest: digestOf(`{"command":"a"}`), At: time.Now()}}
	if n := retireAskedByToolEvidence(filepath.Join(bundle, "perm"), seen, time.Now()); n != 0 {
		t.Fatalf("retired %d notices across differing tool names", n)
	}
}

func TestOneRecordRetiresOneNotice(t *testing.T) {
	// Two identical asks (the same command approved twice) with one record on
	// disk: exactly one notice may go. The second dialog may still be up.
	bundle := t.TempDir()
	mkPermDirs(t, bundle)
	input := `{"command":"date > x.txt"}`
	askedNoticeAt(t, bundle, "n-old", "Bash", digestOf(input), time.Now().Add(-2*time.Minute))
	askedNoticeAt(t, bundle, "n-new", "Bash", digestOf(input), time.Now().Add(-time.Minute))

	seen := []toolEvidence{{ID: "tu1", Tool: "Bash", Digest: digestOf(input), At: time.Now()}}
	if n := retireAskedByToolEvidence(filepath.Join(bundle, "perm"), seen, time.Now()); n != 1 {
		t.Fatalf("retired %d notices from one record, want 1", n)
	}
	left := standingNotices(t, bundle)
	if len(left) != 1 || left[0] != "n-new.json" {
		t.Fatalf("remaining = %v, want the newer notice (oldest is retired first)", left)
	}
}

func TestNoticeNewerThanItsRecordSurvives(t *testing.T) {
	// The ordering clause growth-based retirement already had, kept: an agent
	// that ran a tool and *then* asked for the same command again produces a
	// record older than the notice, and that record proves nothing about it.
	bundle := t.TempDir()
	mkPermDirs(t, bundle)
	input := `{"command":"date > x.txt"}`
	askedNoticeAt(t, bundle, "n-one", "Bash", digestOf(input), time.Now())

	seen := []toolEvidence{{ID: "tu1", Tool: "Bash", Digest: digestOf(input), At: time.Now().Add(-time.Minute)}}
	if n := retireAskedByToolEvidence(filepath.Join(bundle, "perm"), seen, time.Now()); n != 0 {
		t.Fatalf("retired %d notices on a record older than the ask", n)
	}
}

func TestUndatedEvidenceRetiresNothing(t *testing.T) {
	// Same rule every other freshness gate follows: undated reads as unknown
	// and declines rather than guessing.
	bundle := t.TempDir()
	mkPermDirs(t, bundle)
	input := `{"command":"date > x.txt"}`
	askedNoticeAt(t, bundle, "n-one", "Bash", digestOf(input), time.Now().Add(-time.Minute))

	seen := []toolEvidence{{ID: "tu1", Tool: "Bash", Digest: digestOf(input)}}
	if n := retireAskedByToolEvidence(filepath.Join(bundle, "perm"), seen, time.Now()); n != 0 {
		t.Fatalf("retired %d notices on undated evidence", n)
	}
}

// A tool call whose input the hook escalated: the record Claude writes once the
// human has approved the dialog. Its input bytes are what the notice digested.
const approvedCallInput = `{"command":"rm -rf build"}`

func approvedCallLine(id, tool string) string {
	return `{"type":"assistant","timestamp":"2026-08-18T12:00:05Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"` +
		id + `","name":"` + tool + `","input":` + approvedCallInput + `}]}}`
}

func TestPollerRetiresNoticeFromTranscriptCall(t *testing.T) {
	// The wiring, end to end through the app: a notice stands, the transcript
	// grows the very call it announced, and the node stops asking for
	// attention — with a second call still unresolved, the state the old
	// PendingCount()==0 gate refused to retire in.
	f := &fakeTmux{alive: map[string]bool{"t1": true}, list: []string{"t1"}, capture: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaudeWithTranscript(t, a, "t1", hookSIDOwn,
		`{"type":"user","timestamp":"2026-08-18T12:00:00Z","message":{"role":"user","content":"go"}}`,
		approvedCallLine("tu1", "Bash"),
		runningToolCall)
	mkPermDirs(t, bundle)
	a.refreshClaudePermCaps()
	askedNoticeAt(t, bundle, "n1", "Bash", digestOf(approvedCallInput),
		time.Date(2026, 8, 18, 12, 0, 1, 0, time.UTC))

	if attn, _ := a.claudeAskState(n); attn == "" {
		t.Fatal("setup: the notice should be raising attention before retirement")
	}
	tl := a.tailerFor(n)
	if tl == nil {
		t.Fatal("no tailer")
	}
	tl.Poll()
	if got := a.retireClaudeAskedByCalls(n, tl.ToolStamps()); got != 1 {
		t.Fatalf("retired %d notices, want 1", got)
	}
	if attn, _ := a.claudeAskState(n); attn != "" {
		t.Fatalf("attention %q still raised after the call landed", attn)
	}
}

func TestTranscriptCallCreditedOnlyOnce(t *testing.T) {
	// The stamps accumulate for the life of the tailer, so every tick sees the
	// same call again. A watermark keeps one record worth one retirement —
	// otherwise a single approved call would drain notices for dialogs that
	// are still on screen.
	f := &fakeTmux{alive: map[string]bool{"t1": true}, list: []string{"t1"}, capture: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaudeWithTranscript(t, a, "t1", hookSIDOwn,
		approvedCallLine("tu1", "Bash"))
	mkPermDirs(t, bundle)
	a.refreshClaudePermCaps()
	at := time.Date(2026, 8, 18, 12, 0, 1, 0, time.UTC)
	askedNoticeAt(t, bundle, "n1", "Bash", digestOf(approvedCallInput), at)
	askedNoticeAt(t, bundle, "n2", "Bash", digestOf(approvedCallInput), at.Add(time.Second))

	tl := a.tailerFor(n)
	tl.Poll()
	if got := a.retireClaudeAskedByCalls(n, tl.ToolStamps()); got != 1 {
		t.Fatalf("first pass retired %d, want 1", got)
	}
	tl.Poll()
	if got := a.retireClaudeAskedByCalls(n, tl.ToolStamps()); got != 0 {
		t.Fatalf("second pass retired %d from the same record, want 0", got)
	}
	if attn, _ := a.claudeAskState(n); attn == "" {
		t.Fatal("the second, uncredited notice should still raise attention")
	}
}
