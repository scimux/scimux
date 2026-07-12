// Package tmuxsession wraps a single tmux session running one command in one
// window/pane on a private tmux server (tmux -L socket). It deliberately
// exposes only five operations — start, alive, send, capture, kill — plus
// killing the whole private server. Output is obtained by photographing the
// rendered pane (capture-pane -p), never by parsing the byte stream: tmux is
// the terminal emulator, this package just takes snapshots.
package tmuxsession

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// Runner executes the tmux binary with the given arguments, feeding stdin if
// non-empty, and returns trimmed combined output. It exists as a seam so unit
// tests can assert the exact tmux invocations without a tmux binary.
type Runner func(ctx context.Context, stdin string, args ...string) (string, error)

func execTmux(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), "\n"), err
}

// Server addresses a private tmux server via its socket name (tmux -L), so
// scimux sessions can never collide with the user's personal tmux server.
type Server struct {
	Socket string
	// PasteDelay is the pause between pasting a prompt and submitting it,
	// giving the agent TUI time to ingest the paste. Tests set it to 0.
	PasteDelay time.Duration
	// AckPoll is the pause between the pane captures SendAck takes while
	// waiting for the TUI to visibly react to Enter. Tests set it to 0.
	AckPoll time.Duration
	run     Runner
}

func NewServer(socket string) *Server {
	return &Server{Socket: socket, PasteDelay: 150 * time.Millisecond,
		AckPoll: 250 * time.Millisecond, run: execTmux}
}

// NewServerWithRunner is the test constructor.
func NewServerWithRunner(socket string, run Runner) *Server {
	return &Server{Socket: socket, run: run}
}

// Session names must be shell- and tmux-safe: no separators tmux interprets,
// no leading '-' that would parse as a flag.
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func ValidName(name string) bool { return nameRe.MatchString(name) }

func (sv *Server) tmux(stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return sv.run(ctx, stdin, append([]string{"-L", sv.Socket}, args...)...)
}

// NewSession starts command detached in a new session with a fixed-size pane
// (fixed so captures are stable regardless of attached clients).
func (sv *Server) NewSession(name, dir, command string) (*Session, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid session name %q", name)
	}
	out, err := sv.tmux("", "new-session", "-d", "-s", name, "-x", "200", "-y", "50", "-c", dir, command)
	if err != nil {
		return nil, fmt.Errorf("tmux new-session: %v: %s", err, out)
	}
	return &Session{sv: sv, Name: name}, nil
}

// Session returns a handle without checking existence; Alive answers that.
func (sv *Server) Session(name string) *Session { return &Session{sv: sv, Name: name} }

// Sessions lists the names of all sessions on this private server. A server
// that is not running is not an error — it simply has no sessions.
func (sv *Server) Sessions() []string {
	out, err := sv.tmux("", "list-sessions", "-F", "#{session_name}")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// KillServer stops the private tmux server and with it every scimux session.
// A server that is not running is not an error.
func (sv *Server) KillServer() error {
	out, err := sv.tmux("", "kill-server")
	if err != nil && !strings.Contains(out, "no server") {
		return fmt.Errorf("tmux kill-server: %v: %s", err, out)
	}
	return nil
}

// Session is one tmux session running one command in one window/pane.
type Session struct {
	sv   *Server
	Name string
}

// Targets use the '=' prefix for exact-match addressing, so "rq2" can never
// accidentally address "rq2-followup". Pane-level commands additionally need
// a trailing ':' ("session's current window") — a bare "=name" resolves as a
// session but not as a pane target (observed with tmux 3.6).
func (s *Session) target() string     { return "=" + s.Name }
func (s *Session) paneTarget() string { return "=" + s.Name + ":" }

func (s *Session) Alive() bool {
	_, err := s.sv.tmux("", "has-session", "-t", s.target())
	return err == nil
}

// sendSeq makes every Send use its own tmux buffer. HTTP handlers run
// concurrently; a shared buffer name lets one node's paste deliver another
// node's prompt (load A, load B, paste A gets B). A unique name per send
// removes the interleaving entirely without serializing sends.
var sendSeq atomic.Uint64

// Send delivers text as a single paste (safe for long, multi-line prompts —
// send-keys would re-interpret newlines as submissions) and then submits it.
func (s *Session) Send(text string) error {
	buf := fmt.Sprintf("scimux-send-%d", sendSeq.Add(1))
	if out, err := s.sv.tmux(text, "load-buffer", "-b", buf, "-"); err != nil {
		return fmt.Errorf("tmux load-buffer: %v: %s", err, out)
	}
	if out, err := s.sv.tmux("", "paste-buffer", "-d", "-b", buf, "-t", s.paneTarget()); err != nil {
		return fmt.Errorf("tmux paste-buffer: %v: %s", err, out)
	}
	time.Sleep(s.sv.PasteDelay)
	if out, err := s.sv.tmux("", "send-keys", "-t", s.paneTarget(), "Enter"); err != nil {
		return fmt.Errorf("tmux send-keys: %v: %s", err, out)
	}
	return nil
}

// sendAckPolls bounds how long SendAck watches for a pane reaction to Enter:
// sendAckPolls × AckPoll ≈ 2 s with the default 250 ms interval.
const sendAckPolls = 8

// SendAck delivers text like Send but additionally reports whether the pane
// visibly changed after Enter — mechanical evidence that the TUI consumed the
// submission. It never inspects pane content and never presses Enter twice:
// acked == false means "no observable reaction within the window", which the
// caller must treat as an unconfirmed delivery, not as a licence to retry.
func (s *Session) SendAck(text string) (acked bool, err error) {
	buf := fmt.Sprintf("scimux-send-%d", sendSeq.Add(1))
	if out, err := s.sv.tmux(text, "load-buffer", "-b", buf, "-"); err != nil {
		return false, fmt.Errorf("tmux load-buffer: %v: %s", err, out)
	}
	if out, err := s.sv.tmux("", "paste-buffer", "-d", "-b", buf, "-t", s.paneTarget()); err != nil {
		return false, fmt.Errorf("tmux paste-buffer: %v: %s", err, out)
	}
	time.Sleep(s.sv.PasteDelay)
	// Snapshot after the paste and before Enter: the paste itself changes the
	// pane, so only a post-Enter delta counts as a reaction to the submission.
	before, capErr := s.Capture()
	if out, err := s.sv.tmux("", "send-keys", "-t", s.paneTarget(), "Enter"); err != nil {
		return false, fmt.Errorf("tmux send-keys: %v: %s", err, out)
	}
	if capErr != nil {
		return false, nil // delivered, but unverifiable without a baseline
	}
	for i := 0; i < sendAckPolls; i++ {
		time.Sleep(s.sv.AckPoll)
		after, err := s.Capture()
		if err == nil && after != before {
			return true, nil
		}
	}
	return false, nil
}

// allowedKeys are the single keys a supervisor may press into an agent's TUI
// dialog (approval prompts, question menus). A closed whitelist: anything
// else must be a full prompt and goes through Send.
var allowedKeys = map[string]bool{
	"Enter": true, "Escape": true, "Tab": true,
	"Up": true, "Down": true, "Left": true, "Right": true,
	"1": true, "2": true, "3": true, "4": true, "5": true,
	"6": true, "7": true, "8": true, "9": true,
	"y": true, "n": true,
}

func AllowedKey(key string) bool { return allowedKeys[key] }

// SendKey presses one whitelisted key in the pane — how a supervisor answers
// an agent's approval prompt or question menu without attaching.
func (s *Session) SendKey(key string) error {
	if !allowedKeys[key] {
		return fmt.Errorf("key %q not allowed", key)
	}
	if out, err := s.sv.tmux("", "send-keys", "-t", s.paneTarget(), key); err != nil {
		return fmt.Errorf("tmux send-keys: %v: %s", err, out)
	}
	return nil
}

// Capture returns the pane's rendered plain text (last 200 scrollback lines).
func (s *Session) Capture() (string, error) {
	out, err := s.sv.tmux("", "capture-pane", "-p", "-t", s.paneTarget(), "-S", "-200")
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %v: %s", err, out)
	}
	return out, nil
}

// CaptureVisible returns only the pane's currently rendered screen (no
// scrollback) — the decision view: what a supervisor must read to answer an
// approval dialog, without history mixed in.
func (s *Session) CaptureVisible() (string, error) {
	out, err := s.sv.tmux("", "capture-pane", "-p", "-t", s.paneTarget())
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %v: %s", err, out)
	}
	return out, nil
}

// PanePID returns the pid of the process running in the session's pane.
func (s *Session) PanePID() (string, error) {
	out, err := s.sv.tmux("", "display-message", "-p", "-t", s.paneTarget(), "#{pane_pid}")
	if err != nil {
		return "", fmt.Errorf("tmux display-message: %v: %s", err, out)
	}
	return strings.TrimSpace(out), nil
}

// Cwd returns the current working directory of the session's pane. Used
// when adopting a manually created session whose dir scimux never knew.
func (s *Session) Cwd() (string, error) {
	out, err := s.sv.tmux("", "display-message", "-p", "-t", s.paneTarget(), "#{pane_current_path}")
	if err != nil {
		return "", fmt.Errorf("tmux display-message: %v: %s", err, out)
	}
	return strings.TrimSpace(out), nil
}

func (s *Session) Kill() error {
	if out, err := s.sv.tmux("", "kill-session", "-t", s.target()); err != nil {
		return fmt.Errorf("tmux kill-session: %v: %s", err, out)
	}
	return nil
}
