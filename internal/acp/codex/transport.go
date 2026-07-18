package codex

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Transport is the subprocess seam, mirroring internal/acp's Process. The real
// transport runs `codex app-server --stdio`; unit tests inject an in-process
// scripted server over pipes so nothing shells out and no tokens are needed.
//
// Stdin is the writer the client sends framed requests to (the server's
// stdin); Stdout is the reader the client consumes (the server's stdout).
type Transport interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	// Close terminates the transport. For the exec transport this signals the
	// process group so tool grandchildren die too; it is idempotent.
	Close() error
}

// Spawn launches `codex app-server --stdio` in its own process group and wires
// stdin/stdout as the JSON-RPC transport. stderr is forwarded so the server's
// auth/login guidance stays visible. This is the only place that shells out;
// everything above it is transport-agnostic and unit-testable.
func Spawn(bin string, extraArgs ...string) (Transport, error) {
	if bin == "" {
		bin = "codex"
	}
	args := append([]string{"app-server", "--stdio"}, extraArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close() // Start never ran, so nothing else will close it
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		// Start closes both pipe ends itself on failure.
		return nil, fmt.Errorf("start codex app-server: %w", err)
	}
	t := &execTransport{cmd: cmd, stdin: stdin, stdout: stdout, waited: make(chan struct{})}
	go t.reap() // always reap so natural exits don't produce zombies
	return t, nil
}

type execTransport struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.Reader
	waited   chan struct{}
	waitOnce sync.Once
	killOnce sync.Once
}

func (t *execTransport) Stdin() io.WriteCloser { return t.stdin }
func (t *execTransport) Stdout() io.Reader     { return t.stdout }

// Close signals the whole process group (negative pid), escalating to SIGKILL
// only if the group is still alive after a grace period — matching the
// orphan-prevention discipline in internal/acp/process.go.
func (t *execTransport) Close() error {
	if t.cmd.Process == nil {
		return nil
	}
	go t.reap()
	t.killOnce.Do(func() {
		pgid := t.cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		go func(pgid int) {
			select {
			case <-t.waited:
			case <-time.After(2 * time.Second):
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}(pgid)
	})
	return nil
}

func (t *execTransport) reap() {
	t.waitOnce.Do(func() {
		_ = t.cmd.Wait()
		close(t.waited)
	})
}
