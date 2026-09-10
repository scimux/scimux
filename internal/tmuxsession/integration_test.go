package tmuxsession

// Tier B: integration tests against a real tmux, on a private random socket
// so they can never touch the user's tmux server. Skipped with -short or
// when tmux is not installed. The wrapped command is plain bash, never an
// actual agent.

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func integrationServer(t *testing.T) *Server {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped with -short")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
	sv := NewServer(fmt.Sprintf("scimux-test-%d", rand.Int63()))
	sv.PasteDelay = 50 * time.Millisecond
	t.Cleanup(func() { killServerAndSocket(t, sv) })
	return sv
}

// killServerAndSocket is the whole cleanup a private server needs. Killing it
// is not enough: tmux never unlinks its socket file when the server exits, so
// every run of the suite used to leave one 0-byte socket in the user's
// /tmp/tmux-<uid> under a random name, for the reboot to clear (observed: 45
// leftovers from one day of test runs). The removal is asserted rather than
// best-effort, because litter nobody is told about is litter nobody clears.
func killServerAndSocket(t *testing.T, sv *Server) {
	t.Helper()
	if err := sv.KillServer(); err != nil {
		t.Errorf("kill server %s: %v", sv.Socket, err)
	}
	if err := os.Remove(sv.SocketPath()); err != nil && !os.IsNotExist(err) {
		t.Errorf("leftover tmux socket %s: %v", sv.SocketPath(), err)
	}
}

// The cleanup above is only as good as the path it deletes, and a stale name
// is indistinguishable from a clean run. This pins SocketPath against real
// tmux: while the server is up, that path is the socket tmux created.
func TestSocketPathNamesTheFileTmuxCreates(t *testing.T) {
	sv := integrationServer(t)
	if _, err := sv.NewSession("sockpath", t.TempDir(), "cat"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(sv.SocketPath())
	if err != nil {
		t.Fatalf("SocketPath() = %q, which tmux did not create: %v", sv.SocketPath(), err)
	}
	if st.Mode()&os.ModeSocket == 0 {
		t.Errorf("SocketPath() = %q is not a socket (mode %v)", sv.SocketPath(), st.Mode())
	}
}

// waitFor polls cond until it returns true or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

func TestRoundTrip(t *testing.T) {
	sv := integrationServer(t)
	dir, _ := os.Getwd()
	s, err := sv.NewSession("roundtrip", dir, "bash --norc")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Alive() {
		t.Fatal("session not alive after start")
	}
	marker := fmt.Sprintf("MARKER-%d", rand.Int63())
	waitFor(t, 5*time.Second, "shell prompt", func() bool {
		out, err := s.Capture()
		return err == nil && out != ""
	})
	if err := s.Send("echo " + marker); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "marker in capture", func() bool {
		out, _ := s.Capture()
		// The echoed command line also contains the marker; require it on
		// its own line to prove the command actually executed.
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) == marker {
				return true
			}
		}
		return false
	})
	if err := s.Kill(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "session death", func() bool { return !s.Alive() })
}

func TestCaptureImmediatelyAfterStart(t *testing.T) {
	sv := integrationServer(t)
	dir, _ := os.Getwd()
	s, err := sv.NewSession("earlybird", dir, "bash --norc")
	if err != nil {
		t.Fatal(err)
	}
	// Must not error even if the shell has not drawn anything yet.
	if _, err := s.Capture(); err != nil {
		t.Fatalf("capture right after start: %v", err)
	}
}

func TestMultiLineSendArrivesAsOnePaste(t *testing.T) {
	sv := integrationServer(t)
	dir, _ := os.Getwd()
	// cat echoes stdin verbatim: if the two lines arrived as separate
	// submissions with the Enter in between, ordering/interleaving with the
	// trailing Enter would differ from a single paste.
	s, err := sv.NewSession("pastetest", dir, "cat")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "cat ready", func() bool {
		_, err := s.Capture()
		return err == nil
	})
	if err := s.Send("alpha beta\ngamma delta"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "both lines echoed", func() bool {
		out, _ := s.Capture()
		return strings.Contains(out, "alpha beta") && strings.Contains(out, "gamma delta")
	})
}

func TestStartOnTakenNameFails(t *testing.T) {
	sv := integrationServer(t)
	dir, _ := os.Getwd()
	if _, err := sv.NewSession("dup", dir, "bash --norc"); err != nil {
		t.Fatal(err)
	}
	if _, err := sv.NewSession("dup", dir, "bash --norc"); err == nil {
		t.Fatal("second NewSession with same name must fail")
	}
}

func TestKillDeadSessionErrorsCleanly(t *testing.T) {
	sv := integrationServer(t)
	dir, _ := os.Getwd()
	s, err := sv.NewSession("shortlived", dir, "bash --norc")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Kill(); err != nil {
		t.Fatal(err)
	}
	// Pinned behavior: killing an already-dead session returns an error
	// (callers use Alive() first if they need idempotency).
	if err := s.Kill(); err == nil {
		t.Fatal("second Kill must return an error")
	}
}

// -p must be inert for a wrapped command that never requests bracketed paste
// mode: tmux inserts the markers only on request, so a plain reader like cat
// must see the text unchanged and never the literal escape sequences.
func TestBracketedPasteIsInertWithoutRequest(t *testing.T) {
	sv := integrationServer(t)
	dir, _ := os.Getwd()
	s, err := sv.NewSession("brackets", dir, "cat")
	if err != nil {
		t.Fatal(err)
	}
	// cat prints nothing until it is fed, so readiness is the live pane itself.
	waitFor(t, 5*time.Second, "pane to start", func() bool { return s.Alive() })
	marker := fmt.Sprintf("ALPHA-%d", rand.Int63())
	if err := s.Send(marker + "\nBETA-" + marker); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "both pasted lines", func() bool {
		out, _ := s.Capture()
		return strings.Contains(out, marker) && strings.Contains(out, "BETA-"+marker)
	})
	out, err := s.Capture()
	if err != nil {
		t.Fatal(err)
	}
	for _, junk := range []string{"200~", "201~"} {
		if strings.Contains(out, junk) {
			t.Errorf("bracketed-paste marker %q leaked into a pane that never requested it:\n%s", junk, out)
		}
	}
}
