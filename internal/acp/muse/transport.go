package muse

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Transport is the stdio pair a Client speaks over. Tests inject pipes or a
// helper subprocess; production speaks to `muse serve`.
type Transport interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Close() error
}

// commandFn is the process-construction seam. Production uses exec.Command
// (not CommandContext) so cancellation can kill the whole process group
// rather than only the leader. Tests replace it with the current test binary.
var commandFn = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

// processKillGrace is how long Close waits after SIGTERM before SIGKILL to
// the remaining process group, including descendants that ignored SIGTERM.
var processKillGrace = 2 * time.Second

// Spawn starts `<bin> serve` in its own process group and returns its stdio.
// Production spawn is exactly that argv: no provider, base-URL, approval-judge,
// credential, schema, or launcher flags.
func Spawn(ctx context.Context, bin string) (Transport, error) {
	if bin == "" {
		return nil, fmt.Errorf("muse binary is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := commandFn(ctx, bin, "serve")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start muse serve: %w", err)
	}
	t := &execTransport{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		stdoutC:   stdout,
		pgid:      cmd.Process.Pid,
		killGrace: processKillGrace,
		waited:    make(chan struct{}),
	}
	go t.reap()
	go t.watchCtx(ctx)
	return t, nil
}

type execTransport struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.Reader
	stdoutC   io.Closer
	pgid      int
	killGrace time.Duration

	waited    chan struct{}
	waitOnce  sync.Once
	closeOnce sync.Once
}

func (t *execTransport) Stdin() io.WriteCloser { return t.stdin }
func (t *execTransport) Stdout() io.Reader     { return t.stdout }

func (t *execTransport) watchCtx(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = t.Close()
	case <-t.waited:
	}
}

func (t *execTransport) Close() error {
	t.closeOnce.Do(func() {
		if t.stdin != nil {
			_ = t.stdin.Close()
		}
		if t.stdoutC != nil {
			_ = t.stdoutC.Close()
		}
		go t.reap()
		t.signalGroup()
	})
	<-t.waited
	return nil
}

func (t *execTransport) signalGroup() {
	pgid := t.pgid
	if pgid <= 1 {
		return
	}
	grace := t.killGrace
	if grace <= 0 {
		grace = 2 * time.Second
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for {
		if syscall.Kill(-pgid, 0) != nil {
			t.pgid = 0
			return
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if syscall.Kill(-pgid, 0) != nil {
		t.pgid = 0
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	goneDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(goneDeadline) {
		if syscall.Kill(-pgid, 0) != nil {
			t.pgid = 0
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (t *execTransport) reap() {
	t.waitOnce.Do(func() {
		if t.cmd != nil {
			_ = t.cmd.Wait()
		}
		close(t.waited)
	})
}
