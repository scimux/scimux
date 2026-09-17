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
// on the agent parent (session options do not carry them); cursor exposes ACP
// as `cursor-agent acp` and takes its model the same way grok does.
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
	case "cursor":
		// Model is a launch flag, not a session option. Cursor's
		// session/set_config_option rejects anything outside the closed enum
		// its own session/new advertised (-32602 "Invalid model value"), and
		// those values are a different namespace from the ids `cursor-agent
		// --list-models` prints. The flag takes the printed ids, so that is
		// the surface scimux drives.
		//
		// Effort is deliberately absent: cursor has no effort flag, because
		// the level is part of the model id ("claude-opus-5-thinking-high").
		// The catalog resolves (model, effort) to that exact id before launch,
		// so by the time argv is built the choice is already spelled once.
		argv := []string{"cursor-agent"}
		if model != "" {
			argv = append(argv, "--model", model)
		}
		return append(argv, "acp"), nil
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
	return &osProcess{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

// groupTermGrace is how long a process group is given to leave of its own
// accord after SIGTERM, before scimux insists.
const groupTermGrace = 2 * time.Second

// groupPollInterval is how often the group is asked whether it has emptied
// while the grace period runs.
const groupPollInterval = 50 * time.Millisecond

// TerminateGroup stops the process group pgid: SIGTERM, then SIGKILL if the
// group still has members when the grace period ends. It blocks until the
// group is empty or has been insisted upon, so callers that must not wait run
// it in a goroutine.
//
// What it waits on is the *group*, not the process scimux started. An ACP
// agent spawns tool grandchildren into its own group, and those grandchildren
// are the whole reason orphan prevention exists — an agent that exits
// promptly while its tools keep working is the ordinary case, not the
// exception. Gating escalation on the parent's exit therefore cancels it
// exactly when it is needed, leaving the tools running with nothing
// supervising them.
//
// kill(-pgid, 0) returning ESRCH is the oracle for "the group has no members
// left", which also keeps the original reason for not using a blind timer:
// nothing is signalled once the group has emptied, so a pgid the kernel has
// since handed to an unrelated process is never hit. The emptiness check
// immediately precedes the SIGKILL, which is as tight as POSIX allows —
// there is no way to signal a group conditionally on it being the same group.
//
// A group whose last member is an unreaped zombie still counts as populated,
// so the SIGKILL lands on a corpse and does nothing. That is correct and not
// worth avoiding: every production caller reaps.
func TerminateGroup(pgid int) {
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err == syscall.ESRCH {
		return
	}
	deadline := time.Now().Add(groupTermGrace)
	for {
		if syscall.Kill(-pgid, 0) == syscall.ESRCH {
			return
		}
		if !time.Now().Before(deadline) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			return
		}
		time.Sleep(groupPollInterval)
	}
}

type osProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.Reader
	killOnce sync.Once
}

func (p *osProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *osProcess) Stdout() io.Reader     { return p.stdout }

// Kill stops the whole process group. Repeated calls are idempotent.
func (p *osProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	p.killOnce.Do(func() { go TerminateGroup(p.cmd.Process.Pid) })
	return nil
}

func (p *osProcess) Wait() error { return p.cmd.Wait() }
