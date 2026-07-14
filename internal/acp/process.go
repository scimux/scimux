package acp

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Process is the subprocess seam. Real ACP agents run as local children;
// tests supply an in-process fake agent over pipes. peerIn is the writer the
// SDK connection sends to (the agent's stdin); peerOut is the reader it
// consumes (the agent's stdout).
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	// Kill terminates the process (and, for the real runner, its whole process
	// group, so tool grandchildren die too — orphan prevention, plan §4.9).
	Kill() error
	// Wait blocks until the process exits.
	Wait() error
}

// Runner starts the ACP agent subprocess for a node. Injectable so unit tests
// point at an in-process fake with no agent CLI, no tokens (cf.
// tmuxsession.NewServerWithRunner).
type Runner func(nodeID, agent, dir string) (Process, error)

// agentArgv maps a scimux agent name to its ACP launch command. pi ships a
// dedicated `pi-acp` binary; opencode exposes ACP as `opencode acp`.
func agentArgv(agent string) ([]string, error) {
	switch agent {
	case "pi":
		return []string{"pi-acp"}, nil
	case "opencode":
		return []string{"opencode", "acp"}, nil
	}
	return nil, fmt.Errorf("agent %q has no ACP transport", agent)
}

// execRunner is the production Runner: it spawns the agent in its own process
// group and wires stdin/stdout as the ACP transport. stderr is forwarded so an
// agent's auth/login guidance is visible to whoever launched scimux.
func execRunner(nodeID, agent, dir string) (Process, error) {
	argv, err := agentArgv(agent)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s ACP: %w", agent, err)
	}
	return &osProcess{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

type osProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
}

func (p *osProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *osProcess) Stdout() io.Reader     { return p.stdout }

// Kill signals the whole process group (negative pid), then escalates: an ACP
// agent may have spawned tool grandchildren that must not orphan.
func (p *osProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	pgid := p.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	// Give the group a brief grace period, then SIGKILL.
	go func(pgid int) {
		time.Sleep(2 * time.Second)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}(pgid)
	return nil
}

func (p *osProcess) Wait() error { return p.cmd.Wait() }
