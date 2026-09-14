package acp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// Killing an agent must stop the work it started, not merely the process
// scimux holds a handle on. An ACP agent spawns tool grandchildren into its
// own group, and those are what orphan prevention exists for — so an agent
// that exits promptly while its tools keep running is the ordinary case, not
// the exception. Escalation gated on the parent's exit cancels itself exactly
// then, and the tools run on unsupervised.
func TestKillEscalatesToAChildThatOutlivesItsParent(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process-group signalling is POSIX-specific")
	}
	dir := t.TempDir()
	child := filepath.Join(dir, "child.sh")
	// A tool that does not take the polite hint, and would still be working
	// long past the escalation deadline. The loop matters: a single `sleep`
	// would die of the group's SIGTERM even with the shell ignoring it, and
	// the group would empty on its own — proving nothing about escalation.
	ready := filepath.Join(dir, "ready")
	if err := os.WriteFile(child, []byte("trap '' TERM\ntouch "+ready+"\nwhile :; do sleep 0.2; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(dir, "parent.sh")
	// Exits at once, leaving the child behind in the group.
	if err := os.WriteFile(parent, []byte("bash --norc "+child+" &\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "--norc", parent)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	// Never leave the child running if the assertion fails.
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })

	// Wait for the grandchild to exist. Signalling before the parent shell has
	// forked it would kill an ordinary one-process group and prove nothing.
	if !fileAppearsWithin(ready, 10*time.Second) {
		t.Fatal("the fixture's surviving child never started")
	}

	p := &osProcess{cmd: cmd}
	// Reap the parent promptly, which is what the connection layer does — and
	// what used to cancel the escalation.
	go func() { _ = p.Wait() }()
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	if !groupGoneWithin(pgid, 10*time.Second) {
		t.Fatal("a child that ignored SIGTERM was still running after the escalation deadline")
	}
}

func fileAppearsWithin(path string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// groupGoneWithin polls for the group having no members left. ESRCH on a
// signal-0 to the whole group is the only portable way to ask.
func groupGoneWithin(pgid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err == syscall.ESRCH {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// A group that leaves when asked is never signalled again: once its last
// member is gone the kernel is free to hand that pgid to an unrelated
// process, so a SIGKILL held back for the full grace period would be aimed at
// a stranger.
func TestTerminateGroupStopsWatchingOnceTheGroupHasLeft(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process-group signalling is POSIX-specific")
	}
	cmd := exec.Command("bash", "--norc", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	go func() { _ = cmd.Wait() }()

	start := time.Now()
	TerminateGroup(pgid)
	// Returning is the observable: the SIGKILL only ever happens after the
	// grace period elapses, so a return inside it is a SIGKILL not sent.
	if waited := time.Since(start); waited >= groupTermGrace {
		t.Fatalf("watched an already-empty group for the full grace period: %v", waited)
	}
}
