package remote

// terminal.go is the concrete Terminal behind the invite prompt, and it is the
// one file here a unit test cannot reach in full: OpenOwnerTerminal wants
// /dev/tty, and DisableEcho, EchoEnabled and the tail of RestoreEcho want a
// descriptor an ioctl will accept. CI has no controlling terminal, and
// manufacturing a pty means the platform-specific ioctls the Phase-1 split
// exists to keep out of this logic.
//
// What is reachable without a tty is everything that decides an outcome. The
// two tests below take it:
//
//   - RestoreEcho is a four-rung guard ladder, and the rungs are the contract.
//     readInviteTTY (client.go:747) calls it on every exit including the error
//     ones, so a rung that stopped short would either double-restore a terminal
//     or swallow the failure that left echo off — a user typing an invite into
//     a visible line.
//   - ReadLine is the untrusted-input boundary for the invite. The size check
//     is the only thing between a pipe and an unbounded append.
//
// The ioctl on a pipe fails with ENOTTY, which is what makes the live rung
// testable at all: it is the failure path, and the failure path is the one
// that has to cache.

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestRestoreEchoIdempotence(t *testing.T) {
	cached := errors.New("cached restore failure")

	t.Run("nil receiver", func(t *testing.T) {
		// readInviteTTY restores through the Terminal interface, so a nil
		// *ownerTerminal can arrive here as a non-nil interface value.
		var term *ownerTerminal
		if err := term.RestoreEcho(); err != nil {
			t.Fatalf("RestoreEcho() on a nil receiver = %v, want nil", err)
		}
	})

	t.Run("already restored beats the live path", func(t *testing.T) {
		// f is deliberately a live descriptor an ioctl would reject. If the
		// restored guard did not come first, this row would report ENOTTY.
		f := pipeReadEnd(t)
		term := &ownerTerminal{f: f, restored: true}
		if err := term.RestoreEcho(); err != nil {
			t.Fatalf("RestoreEcho() after restoration = %v, want nil", err)
		}
		if term.f != f {
			t.Fatal("RestoreEcho() touched the file after restoration; the guard ran too late")
		}
	})

	t.Run("nil file returns the cached error", func(t *testing.T) {
		term := &ownerTerminal{f: nil, restoreErr: cached}
		if err := term.RestoreEcho(); !errors.Is(err, cached) {
			t.Fatalf("RestoreEcho() = %v, want the cached %v", err, cached)
		}
	})

	t.Run("nil file with no cached error", func(t *testing.T) {
		term := &ownerTerminal{f: nil}
		if err := term.RestoreEcho(); err != nil {
			t.Fatalf("RestoreEcho() = %v, want nil", err)
		}
	})

	t.Run("a live failure is cached for later callers", func(t *testing.T) {
		// The rung that matters most. readInviteTTY joins RestoreEcho's error
		// into the one it reports, and a later caller must see the same
		// failure rather than a clean nil that says echo came back.
		term := &ownerTerminal{f: pipeReadEnd(t)}
		first := term.RestoreEcho()
		if first == nil {
			t.Fatal("RestoreEcho() on a pipe = nil; the ioctl was expected to fail")
		}
		if term.restored {
			t.Fatal("RestoreEcho() marked the terminal restored after a failed ioctl")
		}
		if term.f == nil {
			t.Fatal("RestoreEcho() dropped the file after a failed ioctl; the retry can no longer reach it")
		}
		second := term.RestoreEcho()
		if !errors.Is(second, first) {
			t.Fatalf("second RestoreEcho() = %v, want the first failure %v", second, first)
		}
		// The repeat above cannot tell a cached error from a freshly failing
		// ioctl — both are the same ENOTTY value — so the cache is asserted
		// where it is actually observable. This is the error the nil-file rung
		// hands to every later caller once the descriptor is gone.
		if !errors.Is(term.restoreErr, first) {
			t.Fatalf("restoreErr = %v after a failed restore, want the failure %v", term.restoreErr, first)
		}
	})
}

func TestReadLineRejectsOversizeInput(t *testing.T) {
	rows := []struct {
		name  string
		input string
		want  string
		class Class // "" when no error is expected
		eof   bool
	}{
		{
			name:  "ordinary line",
			input: "ABCDE-FGHIJ\n",
			want:  "ABCDE-FGHIJ",
		},
		{
			// inviteInputMax is a limit, not a threshold: the longest legal
			// input must still be accepted, or the check is off by one in the
			// direction that rejects a valid invite.
			name:  "exactly the limit",
			input: strings.Repeat("A", inviteInputMax) + "\n",
			want:  strings.Repeat("A", inviteInputMax),
		},
		{
			name:  "one byte over the limit",
			input: strings.Repeat("A", inviteInputMax+1) + "\n",
			class: ClassInviteFormat,
		},
		{
			// The reader is sized to inviteInputMax+1, so a caller sending
			// far more must still be refused by the counter rather than by
			// a full buffer.
			name:  "far over the limit",
			input: strings.Repeat("A", 8*inviteInputMax) + "\n",
			class: ClassInviteFormat,
		},
		{
			name:  "closed before any newline",
			input: "ABCDE",
			eof:   true,
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			term := &ownerTerminal{f: pipeWith(t, row.input)}
			got, err := term.ReadLine()
			switch {
			case row.eof:
				if !errors.Is(err, io.EOF) {
					t.Fatalf("ReadLine() error = %v, want io.EOF", err)
				}
			case row.class != "":
				requireClass(t, err, row.class)
				if got != "" {
					t.Fatalf("ReadLine() = %q on a rejected input, want the empty string", got)
				}
			default:
				if err != nil {
					t.Fatalf("ReadLine() error = %v, want nil", err)
				}
				if got != row.want {
					t.Fatalf("ReadLine() = %q (%d bytes), want %d bytes", got, len(got), len(row.want))
				}
			}
		})
	}
}

// TestEchoEnabledOnANonTerminal pins the one answer EchoEnabled can give
// without a tty. It has no production caller — the invite flow disables echo
// and restores it, and the acceptance tests assert restoration through their
// own fakes — but it is part of the Terminal interface, so a reader deserves
// to know what the concrete implementation does when the ioctl fails: it
// reports echo off, which is the safe direction. Reporting it *on* would tell
// a caller the invite is visible when nothing is known.
func TestEchoEnabledOnANonTerminal(t *testing.T) {
	term := &ownerTerminal{f: pipeReadEnd(t)}
	if term.EchoEnabled() {
		t.Fatal("EchoEnabled() = true on a pipe; a failed ioctl must not report echo on")
	}
}

// TestDisableEchoOnANonTerminal pins the failure readInviteTTY is written
// around: DisableEcho is the first call in that flow, and it must report a
// descriptor it could not silence rather than returning nil and letting the
// prompt be typed in the clear. The caller restores and reports
// TerminalInviteError on this path (client.go:749).
func TestDisableEchoOnANonTerminal(t *testing.T) {
	term := &ownerTerminal{f: pipeReadEnd(t)}
	if err := term.DisableEcho(); err == nil {
		t.Fatal("DisableEcho() = nil on a pipe; a descriptor that cannot be silenced must say so")
	}
}

func TestWritePromptReachesTheTerminal(t *testing.T) {
	r, w := osPipe(t)
	term := &ownerTerminal{f: w}
	if err := term.WritePrompt([]byte(invitePrompt)); err != nil {
		t.Fatalf("WritePrompt() error = %v, want nil", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close write end: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	if !bytes.Equal(got, []byte(invitePrompt)) {
		t.Fatalf("prompt on the wire = %q, want %q", got, invitePrompt)
	}
}

func osPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	return r, w
}

// pipeReadEnd is a live *os.File that is not a terminal: every ioctl on it
// fails with ENOTTY.
func pipeReadEnd(t *testing.T) *os.File {
	t.Helper()
	r, _ := osPipe(t)
	return r
}

// pipeWith fills a pipe with s and closes the write end, so ReadLine sees the
// bytes followed by EOF rather than blocking.
func pipeWith(t *testing.T, s string) *os.File {
	t.Helper()
	r, w := osPipe(t)
	go func() {
		_, _ = io.WriteString(w, s)
		_ = w.Close()
	}()
	return r
}
