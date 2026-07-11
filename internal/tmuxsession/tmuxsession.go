// Package tmuxsession wraps a single tmux session running one command in one
// window/pane on a private tmux server (tmux -L socket). It deliberately
// exposes only five operations — start, alive, send, capture, kill — plus
// killing the whole private server. Output is obtained by photographing the
// rendered pane (capture-pane -p), never by parsing the byte stream: tmux is
// the terminal emulator, this package just takes snapshots.
package tmuxsession

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Runner executes the tmux binary with the given arguments, feeding stdin if
// non-empty, and returns trimmed combined output. It exists as a seam so unit
// tests can assert the exact tmux invocations without a tmux binary.
type Runner func(stdin string, args ...string) (string, error)

func execTmux(stdin string, args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
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
	run        Runner
}

func NewServer(socket string) *Server {
	return &Server{Socket: socket, PasteDelay: 150 * time.Millisecond, run: execTmux}
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
	return sv.run(stdin, append([]string{"-L", sv.Socket}, args...)...)
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

// Send delivers text as a single paste (safe for long, multi-line prompts —
// send-keys would re-interpret newlines as submissions) and then submits it.
func (s *Session) Send(text string) error {
	if out, err := s.sv.tmux(text, "load-buffer", "-b", "scimux-send", "-"); err != nil {
		return fmt.Errorf("tmux load-buffer: %v: %s", err, out)
	}
	if out, err := s.sv.tmux("", "paste-buffer", "-d", "-b", "scimux-send", "-t", s.paneTarget()); err != nil {
		return fmt.Errorf("tmux paste-buffer: %v: %s", err, out)
	}
	time.Sleep(s.sv.PasteDelay)
	if out, err := s.sv.tmux("", "send-keys", "-t", s.paneTarget(), "Enter"); err != nil {
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
