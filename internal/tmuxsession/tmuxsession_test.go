package tmuxsession

// Tier A: pure unit tests against a fake runner. They pin down the exact
// tmux invocations — the contract with tmux is where the real bugs live.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// A session name must be safe as a tmux target, not merely non-empty: tmux
// parses '.' as its window.pane separator and ':' as window, so a name carrying
// either is created but then unaddressable by every pane command. ValidName must
// reject them, while still accepting '_' and non-leading '-'.
func TestValidNameRejectsTmuxSeparators(t *testing.T) {
	for _, ok := range []string{"a", "Review-notes-design-md-2", "v2_0", "chat-3", "A.B-safe_ok"[:1]} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"notes-design.md", "a.b", "v2.0", "a:b", "-lead", "", ".hidden", "a b"} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true, want false", bad)
		}
	}
}

type call struct {
	stdin string
	args  []string
}

type fakeRunner struct {
	calls []call
	out   string
	err   error
}

func (f *fakeRunner) run(ctx context.Context, stdin string, args ...string) (string, error) {
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
	// '.' (window.pane separator) and ':' (window separator) are rejected: a
	// session so named is created but unaddressable by every pane command.
	for _, bad := range []string{"", " ", "has space", "-leadingdash", "a;b", `a"b`, "a'b", "a\nb", "a:b", "rq2.low_speed"} {
		if _, err := sv.NewSession(bad, "/", "bash"); err == nil {
			t.Errorf("name %q accepted, want error", bad)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("invalid names must not reach tmux, got %d calls", len(f.calls))
	}
	for _, good := range []string{"a", "rq2_low_speed-001", "X9"} {
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
	if got := load.args[2:]; len(got) != 4 || got[0] != "load-buffer" || got[1] != "-b" ||
		!strings.HasPrefix(got[2], "scimux-send-") || got[3] != "-" {
		t.Errorf("load-buffer args = %v", got)
	}
	if load.stdin != prompt {
		t.Errorf("prompt not passed byte-identically via stdin")
	}
	buf := load.args[4] // the per-send buffer name
	if got := paste.args[2:]; !reflect.DeepEqual(got, []string{"paste-buffer", "-d", "-b", buf, "-t", "=node1:"}) {
		t.Errorf("paste-buffer args = %v (want same buffer %q as load)", got, buf)
	}
	if got := enter.args[2:]; !reflect.DeepEqual(got, []string{"send-keys", "-t", "=node1:", "Enter"}) {
		t.Errorf("send-keys args = %v", got)
	}
}

// Concurrent sends must never share a paste buffer: a shared name lets one
// node's paste deliver another node's prompt (finding 2 of the 2026-07 review).
func TestSendsUseDistinctBuffers(t *testing.T) {
	f := &fakeRunner{}
	sv := newTestServer(f)
	if err := sv.Session("a").Send("prompt A"); err != nil {
		t.Fatal(err)
	}
	if err := sv.Session("b").Send("prompt B"); err != nil {
		t.Fatal(err)
	}
	bufA, bufB := f.calls[0].args[4], f.calls[3].args[4]
	if bufA == bufB {
		t.Errorf("two sends used the same buffer %q", bufA)
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

func TestSendKeys(t *testing.T) {
	f := &fakeRunner{}
	sv := newTestServer(f)
	s := sv.Session("node1")

	// Compound choice + Enter: one runner call, exact order, pane target =name:.
	if err := s.SendKeys("1", "Enter"); err != nil {
		t.Fatal(err)
	}
	want := []string{"-L", "testsock", "send-keys", "-t", "=node1:", "1", "Enter"}
	if len(f.calls) != 1 {
		t.Fatalf("SendKeys(1, Enter) made %d runner calls, want 1: %v", len(f.calls), f.calls)
	}
	if !reflect.DeepEqual(f.calls[0].args, want) {
		t.Fatalf("args = %v, want %v", f.calls[0].args, want)
	}

	// y + Enter preserves order in one call.
	f.calls = nil
	if err := s.SendKeys("y", "Enter"); err != nil {
		t.Fatal(err)
	}
	wantY := []string{"-L", "testsock", "send-keys", "-t", "=node1:", "y", "Enter"}
	if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0].args, wantY) {
		t.Fatalf("SendKeys(y, Enter) = %v, want one call %v", f.calls, wantY)
	}

	// SendKey remains a one-key wrapper.
	f.calls = nil
	if err := s.SendKey("1"); err != nil {
		t.Fatal(err)
	}
	wantOne := []string{"-L", "testsock", "send-keys", "-t", "=node1:", "1"}
	if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0].args, wantOne) {
		t.Fatalf("SendKey(1) = %v, want %v", f.calls, wantOne)
	}

	// Empty sequence: error, no runner call.
	f.calls = nil
	if err := s.SendKeys(); err == nil {
		t.Fatal("empty SendKeys must error")
	}
	if len(f.calls) != 0 {
		t.Fatalf("empty sequence contacted tmux: %v", f.calls)
	}

	// Any invalid member rejects the entire sequence before tmux contact.
	// Cover invalid before, between, and after valid keys.
	before := len(f.calls)
	for _, seq := range [][]string{
		{"q", "Enter"},        // invalid first
		{"1", "q", "Enter"},   // invalid between
		{"1", "Enter", "C-c"}, // invalid after
		{"0"},                 // invalid alone
		{"rm -rf /"},          // not a whitelist member
		{"Enter Enter"},       // not a single allowed key token
	} {
		f.calls = f.calls[:before]
		if err := s.SendKeys(seq...); err == nil {
			t.Errorf("SendKeys(%v) accepted invalid sequence", seq)
		}
		if len(f.calls) != before {
			t.Errorf("invalid sequence %v contacted tmux: %v", seq, f.calls[before:])
		}
	}

	// Representative allowed special/navigation keys still work as a sequence.
	f.calls = nil
	if err := s.SendKeys("Escape"); err != nil {
		t.Fatal(err)
	}
	if err := s.SendKeys("Tab"); err != nil {
		t.Fatal(err)
	}
	if err := s.SendKeys("Up"); err != nil {
		t.Fatal(err)
	}
	if err := s.SendKeys("Down"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 4 {
		t.Fatalf("special keys: %d calls, want 4", len(f.calls))
	}
	for i, key := range []string{"Escape", "Tab", "Up", "Down"} {
		want := []string{"-L", "testsock", "send-keys", "-t", "=node1:", key}
		if !reflect.DeepEqual(f.calls[i].args, want) {
			t.Errorf("%s args = %v, want %v", key, f.calls[i].args, want)
		}
	}

	// Pane target remains exactly =name: (trailing colon required for pane cmds).
	f.calls = nil
	if err := sv.Session("my-node").SendKeys("n", "Enter"); err != nil {
		t.Fatal(err)
	}
	if got := f.calls[0].args; !reflect.DeepEqual(got, []string{"-L", "testsock", "send-keys", "-t", "=my-node:", "n", "Enter"}) {
		t.Errorf("pane target regression: %v", got)
	}
}

// scriptedRunner answers capture-pane calls from a queue (last entry repeats)
// while recording every invocation — enough to simulate a TUI that does or
// does not react to Enter.
type scriptedRunner struct {
	calls    []call
	captures []string
}

func (s *scriptedRunner) run(ctx context.Context, stdin string, args ...string) (string, error) {
	s.calls = append(s.calls, call{stdin: stdin, args: args})
	for _, a := range args {
		if a == "capture-pane" {
			out := s.captures[0]
			if len(s.captures) > 1 {
				s.captures = s.captures[1:]
			}
			return out, nil
		}
	}
	return "", nil
}

func (s *scriptedRunner) enterCount() int {
	n := 0
	for _, c := range s.calls {
		if len(c.args) > 0 && c.args[len(c.args)-1] == "Enter" {
			n++
		}
	}
	return n
}

func TestSendAckPaneChange(t *testing.T) {
	// The pane changes after Enter: the submission is acknowledged, and Enter
	// was pressed exactly once (a duplicate could answer the next dialog).
	s := &scriptedRunner{captures: []string{"> prompt pasted", "agent is thinking"}}
	sv := NewServerWithRunner("testsock", s.run)
	acked, err := sv.Session("node1").SendAck("hello")
	if err != nil {
		t.Fatal(err)
	}
	if !acked {
		t.Error("pane changed after Enter but send not acknowledged")
	}
	if s.enterCount() != 1 {
		t.Fatalf("Enter pressed %d times, want exactly 1", s.enterCount())
	}
}

func TestSendAckNoReaction(t *testing.T) {
	// A TUI that ignores Enter (e.g. a preloaded editor draft): the pane
	// never changes, the delivery is unconfirmed, and Enter is not retried.
	s := &scriptedRunner{captures: []string{"> /usagehello"}}
	sv := NewServerWithRunner("testsock", s.run)
	acked, err := sv.Session("node1").SendAck("hello")
	if err != nil {
		t.Fatal(err)
	}
	if acked {
		t.Error("static pane must not acknowledge the send")
	}
	if s.enterCount() != 1 {
		t.Fatalf("Enter pressed %d times, want exactly 1 (never retry)", s.enterCount())
	}
}

func TestCaptureVisible(t *testing.T) {
	f := &fakeRunner{out: "screen"}
	sv := newTestServer(f)
	out, err := sv.Session("node1").CaptureVisible()
	if err != nil || out != "screen" {
		t.Fatalf("CaptureVisible = %q, %v", out, err)
	}
	want := []string{"-L", "testsock", "capture-pane", "-p", "-t", "=node1:"}
	if !reflect.DeepEqual(f.calls[0].args, want) {
		t.Fatalf("args = %v, want %v (no -S: visible screen only)", f.calls[0].args, want)
	}
}

// blockingRunner blocks indefinitely until the context is cancelled.
type blockingRunner struct {
	calls []call
}

func (b *blockingRunner) run(ctx context.Context, stdin string, args ...string) (string, error) {
	b.calls = append(b.calls, call{stdin: stdin, args: args})
	<-ctx.Done()
	return "", ctx.Err()
}

func TestTmuxTimeout(t *testing.T) {
	// A tmux call that blocks must timeout and return an error, not freeze
	// the supervisor forever (finding 47 of the 2026-07 review).
	b := &blockingRunner{}
	sv := NewServerWithRunner("testsock", b.run)
	_, err := sv.Session("node1").Capture()
	if err == nil {
		t.Fatal("blocking capture should timeout and return error")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("timeout error should mention context deadline, got: %v", err)
	}
	if len(b.calls) != 1 {
		t.Fatalf("want exactly 1 tmux call attempted, got %d", len(b.calls))
	}
}
