package tmuxsession

// Tier A: pure unit tests against a fake runner. They pin down the exact
// tmux invocations — the contract with tmux is where the real bugs live.

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type call struct {
	stdin string
	args  []string
}

type fakeRunner struct {
	calls []call
	out   string
	err   error
}

func (f *fakeRunner) run(stdin string, args ...string) (string, error) {
	f.calls = append(f.calls, call{stdin: stdin, args: args})
	return f.out, f.err
}

func newTestServer(f *fakeRunner) *Server {
	return NewServerWithRunner("testsock", f.run)
}

func TestNewSessionBuildsExactArgs(t *testing.T) {
	f := &fakeRunner{}
	sv := newTestServer(f)
	if _, err := sv.NewSession("rq2-low-speed", "/data/exp1", `claude "hello"`); err != nil {
		t.Fatal(err)
	}
	want := []string{"-L", "testsock", "new-session", "-d", "-s", "rq2-low-speed",
		"-x", "200", "-y", "50", "-c", "/data/exp1", `claude "hello"`}
	if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0].args, want) {
		t.Fatalf("args = %v, want %v", f.calls, want)
	}
}

func TestNameValidation(t *testing.T) {
	f := &fakeRunner{}
	sv := newTestServer(f)
	for _, bad := range []string{"", " ", "has space", "-leadingdash", "a;b", `a"b`, "a'b", "a\nb", "a:b"} {
		if _, err := sv.NewSession(bad, "/", "bash"); err == nil {
			t.Errorf("name %q accepted, want error", bad)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("invalid names must not reach tmux, got %d calls", len(f.calls))
	}
	for _, good := range []string{"a", "rq2.low_speed-001", "X9"} {
		if _, err := sv.NewSession(good, "/", "bash"); err != nil {
			t.Errorf("name %q rejected: %v", good, err)
		}
	}
}

func TestSendOrderingAndPayload(t *testing.T) {
	f := &fakeRunner{}
	sv := newTestServer(f)
	// Multi-line, unicode, >4KB: the paste path must carry it byte-identically.
	prompt := "first line\nsecond line — ünïcode 🚗\n" + strings.Repeat("x", 5000)
	if err := sv.Session("node1").Send(prompt); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("want 3 tmux calls (load, paste, enter), got %d", len(f.calls))
	}
	load, paste, enter := f.calls[0], f.calls[1], f.calls[2]
	if got := load.args[2:]; !reflect.DeepEqual(got, []string{"load-buffer", "-b", "scimux-send", "-"}) {
		t.Errorf("load-buffer args = %v", got)
	}
	if load.stdin != prompt {
		t.Errorf("prompt not passed byte-identically via stdin")
	}
	if got := paste.args[2:]; !reflect.DeepEqual(got, []string{"paste-buffer", "-d", "-b", "scimux-send", "-t", "=node1:"}) {
		t.Errorf("paste-buffer args = %v", got)
	}
	if got := enter.args[2:]; !reflect.DeepEqual(got, []string{"send-keys", "-t", "=node1:", "Enter"}) {
		t.Errorf("send-keys args = %v", got)
	}
}

func TestExactTargeting(t *testing.T) {
	f := &fakeRunner{}
	sv := newTestServer(f)
	s := sv.Session("rq2")
	s.Alive()
	s.Capture()
	s.Kill()
	for _, c := range f.calls {
		found := false
		for i, a := range c.args {
			if a == "-t" {
				found = true
				if got := c.args[i+1]; got != "=rq2" && got != "=rq2:" {
					t.Errorf("%v: target %q, want exact-match \"=rq2\" or \"=rq2:\"", c.args, got)
				}
			}
		}
		if !found {
			t.Errorf("call %v has no -t target", c.args)
		}
	}
}

func TestErrorsPropagateVerbatim(t *testing.T) {
	f := &fakeRunner{out: "no server running on /tmp/x", err: errors.New("exit status 1")}
	sv := newTestServer(f)
	_, err := sv.Session("n").Capture()
	if err == nil || !strings.Contains(err.Error(), "no server running on /tmp/x") {
		t.Fatalf("tmux stderr not propagated verbatim: %v", err)
	}
}

func TestAliveMapsExitCode(t *testing.T) {
	sv := newTestServer(&fakeRunner{})
	if !sv.Session("n").Alive() {
		t.Error("exit 0 must mean alive")
	}
	sv = newTestServer(&fakeRunner{err: errors.New("exit status 1")})
	if sv.Session("n").Alive() {
		t.Error("non-zero exit must mean not alive")
	}
}

func TestSessionsListsNames(t *testing.T) {
	f := &fakeRunner{out: "ra-study\nlc-study\nskab-e2"}
	sv := newTestServer(f)
	got := sv.Sessions()
	want := []string{"ra-study", "lc-study", "skab-e2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Sessions() = %v, want %v", got, want)
	}
	if got := f.calls[0].args[2:]; !reflect.DeepEqual(got, []string{"list-sessions", "-F", "#{session_name}"}) {
		t.Errorf("list-sessions args = %v", got)
	}
}

func TestSessionsEmptyWhenServerDown(t *testing.T) {
	f := &fakeRunner{out: "no server running on /tmp/x", err: errors.New("exit status 1")}
	if got := newTestServer(f).Sessions(); got != nil {
		t.Fatalf("dead server must yield no sessions, got %v", got)
	}
}

func TestCwdUsesPaneTarget(t *testing.T) {
	f := &fakeRunner{out: "/data/exp1"}
	sv := newTestServer(f)
	cwd, err := sv.Session("n1").Cwd()
	if err != nil || cwd != "/data/exp1" {
		t.Fatalf("Cwd() = %q, %v", cwd, err)
	}
	want := []string{"display-message", "-p", "-t", "=n1:", "#{pane_current_path}"}
	if got := f.calls[0].args[2:]; !reflect.DeepEqual(got, want) {
		t.Errorf("display-message args = %v", got)
	}
}

func TestKillServerToleratesNoServer(t *testing.T) {
	f := &fakeRunner{out: "no server running on /tmp/tmux-1000/testsock", err: errors.New("exit status 1")}
	if err := newTestServer(f).KillServer(); err != nil {
		t.Fatalf("kill-server on dead server must be a no-op, got %v", err)
	}
}

func TestSendKey(t *testing.T) {
	f := &fakeRunner{}
	sv := newTestServer(f)
	if err := sv.Session("node1").SendKey("1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"-L", "testsock", "send-keys", "-t", "=node1:", "1"}
	if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0].args, want) {
		t.Fatalf("args = %v, want %v", f.calls, want)
	}
	// Non-whitelisted input must never reach tmux — SendKey answers dialogs,
	// it is not a general keystroke injector.
	for _, bad := range []string{"", "q", "C-c", "rm -rf /", "Enter Enter", "0"} {
		if err := sv.Session("node1").SendKey(bad); err == nil {
			t.Errorf("key %q accepted, want error", bad)
		}
	}
	if len(f.calls) != 1 {
		t.Fatalf("rejected keys reached tmux: %d calls", len(f.calls))
	}
	if !AllowedKey("Escape") || AllowedKey("C-c") {
		t.Error("AllowedKey whitelist inconsistent")
	}
}
