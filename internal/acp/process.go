package acp

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
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
// tmuxsession.NewServerWithRunner). model and effort are optional launch
// config: pi/opencode ignore them (they negotiate via ACP session options);
// grok applies them as CLI flags because its ACP session/new does not expose
// standard modes/configOptions for those knobs.
type Runner func(nodeID, agent, dir, model, effort string) (Process, error)

// agentArgv maps a scimux agent name to its ACP launch command. pi ships a
// dedicated `pi-acp` binary; opencode exposes ACP as `opencode acp`; grok
// exposes ACP as `grok agent stdio` with optional -m / --reasoning-effort
// on the agent parent (session options do not carry them).
func agentArgv(agent, model, effort string) ([]string, error) {
	switch agent {
	case "pi":
		return []string{"pi-acp"}, nil
	case "opencode":
		return []string{"opencode", "acp"}, nil
	case "grok":
		// --no-auto-update: docs recommend it for ACP/scripted runs so a
		// background update check never races the JSON-RPC stream.
		argv := []string{"grok", "--no-auto-update", "agent"}
		if model != "" {
			argv = append(argv, "-m", model)
		}
		if effort != "" {
			argv = append(argv, "--reasoning-effort", effort)
		}
		argv = append(argv, "stdio")
		return argv, nil
	}
	return nil, fmt.Errorf("agent %q has no ACP transport", agent)
}

// execRunner is the production Runner: it spawns the agent in its own process
// group and wires stdin/stdout as the ACP transport. stderr is forwarded so an
// agent's auth/login guidance is visible to whoever launched scimux.
func execRunner(nodeID, agent, dir, model, effort string) (Process, error) {
	argv, err := agentArgv(agent, model, effort)
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
		stdin.Close() // Start never ran, so nothing else will close it
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		// Start closes both pipe ends itself on failure.
		return nil, fmt.Errorf("start %s ACP: %w", agent, err)
	}
	return &osProcess{cmd: cmd, stdin: stdin, stdout: stdout, waited: make(chan struct{})}, nil
}

type osProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.Reader
	waited   chan struct{} // closed by Wait once the process is reaped
	waitOnce sync.Once
	killOnce sync.Once
}

func (p *osProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *osProcess) Stdout() io.Reader     { return p.stdout }

// Kill signals the whole process group (negative pid), then escalates to
// SIGKILL only if the group is still running — an ACP agent may have spawned
// tool grandchildren that must not orphan. Escalation is gated on the process
// actually still being alive (waited), not a blind timer, so a group that
// exits promptly is never SIGKILLed after its pid/pgid may have been reused by
// an unrelated process (finding 58). Repeated calls are idempotent.
func (p *osProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	p.killOnce.Do(func() {
		pgid := p.cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		go func(pgid int) {
			select {
			case <-p.waited:
				// Exited after SIGTERM; do not signal a possibly-reused pgid.
			case <-time.After(2 * time.Second):
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}(pgid)
	})
	return nil
}

func (p *osProcess) Wait() error {
	err := p.cmd.Wait()
	p.waitOnce.Do(func() { close(p.waited) })
	return err
}
