package remote

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Findings 4 and 5 of the 2026-08-27 real run: two ways the invite path
// answers a question the operator did not ask.

// TestEnrolledStartNamesTheUnreadableInviteFile is finding 4. An operator who
// passes --invite-file has said, in the only way the command line allows,
// "use this file". If the path is wrong — a typo, a file already consumed by
// an earlier run, the wrong directory — they must hear about the path, not
// about their enrollment state.
//
// The enrolled branch refuses a *valid* second invite on purpose (a working
// installation is not a place to spend a credential), and that refusal stays.
// What it must not do is reach that refusal by way of a read that failed:
// "already enrolled" is an answer about a file nobody could open.
func TestEnrolledStartNamesTheUnreadableInviteFile(t *testing.T) {
	fake := newFakeRV(t)
	defer fake.Close()
	cfg := clientCfg(t, fake)

	c := NewClient(cfg)
	writeState(t, c, enrolledFixture(t, "ih_041061050R3GG28A"))

	missing := filepath.Join(t.TempDir(), "invite-that-is-not-there")
	cfg.InviteFile = missing
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := startClient(ctx, cfg)
	if err == nil {
		t.Fatal("a start with an unreadable --invite-file succeeded; the operator's typo is invisible")
	}
	if got := classOf(err); got == ClassInviteConflict {
		t.Fatalf("an unreadable file was reported as an invite conflict: %v", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("the error does not name the path the operator has to fix: %v", err)
	}
}

// TestEnrolledStartStillRefusesASecondInvite pins the behaviour the fix must
// not trade away: a readable, valid invite offered to a working installation
// is still refused, still unspent, and still explains the revoke-first route.
func TestEnrolledStartStillRefusesASecondInvite(t *testing.T) {
	fake := newFakeRV(t)
	defer fake.Close()
	cfg := clientCfg(t, fake)

	c := NewClient(cfg)
	writeState(t, c, enrolledFixture(t, "ih_041061050R3GG28A"))

	invite := otherInvite()
	fake.Issue(invite)
	path := writeInviteFile(t, t.TempDir(), invite, 0600)
	cfg.InviteFile = path

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := startClient(ctx, cfg)
	requireClass(t, err, ClassInviteConflict)
	if fake.Redeemed(invite) {
		t.Fatal("the refused invite was spent at the rendezvous")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the refused invite file was erased: %v", statErr)
	}
	requireNoInviteLeak(t, invite, err.Error())
}

// TestEmptyTerminalAnswerSaysSo is finding 5. A bare `scimux --remote` on a
// fresh installation opens a terminal and prompts. Pressing Enter at that
// prompt — the natural move for someone who does not have an invite yet, or
// who ran the command to see what it does — produced
//
//	scimux: remote: invite: invite-format: the invite is not a valid code
//
// four colons deep and, for an empty answer, not even true: there was no
// code. The empty answer is its own case and gets its own sentence, while
// the class stays ClassInviteFormat so startup still fails.
func TestEmptyTerminalAnswerSaysSo(t *testing.T) {
	fake := newFakeRV(t)
	defer fake.Close()
	cfg := clientCfg(t, fake)
	term := &scriptedTerminal{line: "   "}
	cfg.NewTerminal = func() (Terminal, error) { return term, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := startClient(ctx, cfg)
	requireClass(t, err, ClassInviteFormat)

	msg := err.Error()
	low := strings.ToLower(msg)
	if !strings.Contains(low, "no invite") && !strings.Contains(low, "nothing") && !strings.Contains(low, "empty") {
		t.Fatalf("an empty answer was reported as a malformed code: %q", msg)
	}
	if !strings.Contains(low, "invite") {
		t.Fatalf("the message never says what was being asked for: %q", msg)
	}
	if strings.Count(msg, ": ") > 3 {
		t.Fatalf("the message is still a stack of colon-joined labels: %q", msg)
	}
	if fake.RequestCount() != 0 {
		t.Fatalf("an empty answer reached the rendezvous (%d requests)", fake.RequestCount())
	}
}

// TestTerminalPromptSaysWhereAnInviteComesFrom is the other half of finding 5:
// the prompt itself was the bare word "invite: ", which tells someone who has
// never seen one neither what it looks like nor who has it.
func TestTerminalPromptSaysWhereAnInviteComesFrom(t *testing.T) {
	fake := newFakeRV(t)
	defer fake.Close()
	cfg := clientCfg(t, fake)
	term := &scriptedTerminal{line: ""}
	cfg.NewTerminal = func() (Terminal, error) { return term, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = startClient(ctx, cfg)

	prompt := strings.ToLower(term.prompt())
	if !strings.Contains(prompt, "invite") {
		t.Fatalf("prompt does not name what it wants: %q", term.prompt())
	}
	if !strings.Contains(prompt, "paste") && !strings.Contains(prompt, "enter") {
		t.Fatalf("prompt does not tell the operator what to do: %q", term.prompt())
	}
}

// scriptedTerminal answers the invite prompt with a fixed line and records
// what it was asked. It never touches a real tty.
type scriptedTerminal struct {
	line    string
	prompts []string
	echo    bool
}

func (s *scriptedTerminal) DisableEcho() error { s.echo = false; return nil }
func (s *scriptedTerminal) RestoreEcho() error { s.echo = true; return nil }
func (s *scriptedTerminal) EchoEnabled() bool  { return s.echo }
func (s *scriptedTerminal) WritePrompt(b []byte) error {
	s.prompts = append(s.prompts, string(b))
	return nil
}
func (s *scriptedTerminal) ReadLine() (string, error) { return s.line, nil }

func (s *scriptedTerminal) prompt() string { return strings.Join(s.prompts, "") }
