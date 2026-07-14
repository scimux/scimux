package codex

import (
	"os/exec"
	"testing"
	"time"
)

// TestNaturalExitReaped verifies that a process which exits on its own (without
// Close being called) is still reaped via Wait — no zombie is left behind. This
// directly tests the go t.reap() call added to Spawn (finding 74).
//
// We build the execTransport directly (same package) with an inert command, so
// no agent CLI is involved.
func TestNaturalExitReaped(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("sh not available: %v", err)
	}
	tr := &execTransport{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		waited: make(chan struct{}),
	}
	go tr.reap() // mirrors what Spawn now does

	select {
	case <-tr.waited:
		// Process was reaped; waited channel closed as expected.
	case <-time.After(5 * time.Second):
		t.Fatal("natural subprocess exit was not reaped: waited channel never closed")
	}
}
