package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/tmuxsession"
)

// probeTmux is a real tmux on a private random socket. The probe launches a
// session, so there is nothing to assert without one; the wrapped command is
// always bash, never a real agent CLI.
func TestRunClaudeProbeSessionFailurePaths(t *testing.T) {
	base := claudeProbeOptions{Dir: t.TempDir(), ExecPath: "test-exe", Server: tmuxsession.NewServerWithRunner("synthetic", func(context.Context, string, ...string) (string, error) {
		return "", fmt.Errorf("synthetic launch failure")
	})}
	ready := func(*tmuxsession.Session) bool { return false }
	for _, name := range []string{"nil-argv", "nil-ready", "unclean-dir", "unsafe-dir", "mkdir", "settings", "launch"} {
		t.Run(name, func(t *testing.T) {
			opts := base
			argv, done := claudeUsageProbeArgv, ready
			switch name {
			case "nil-argv":
				argv = nil
			case "nil-ready":
				done = nil
			case "unclean-dir":
				opts.Dir += "/."
			case "unsafe-dir":
				opts.Dir = "/"
			case "mkdir":
				opts.Dir = filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(opts.Dir, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "settings":
				opts.Dir = t.TempDir()
				if err := os.Mkdir(filepath.Join(opts.Dir, claudeUsageProbeSettingsName), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := runClaudeProbeSession(context.Background(), opts, argv, done); err != errClaudeUsageProbe {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRunClaudeProbeSessionCancellationAndDefaults(t *testing.T) {
	opts := probeOpts(t, probeTmux(t), "bash --norc -c 'sleep 30'")
	opts.Timeout, opts.Poll = 0, 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runClaudeProbeSession(ctx, opts, claudeUsageProbeArgv, func(*tmuxsession.Session) bool { return false }); err != errClaudeUsageProbe {
		t.Fatalf("error = %v", err)
	}
	if sessions := opts.Server.Sessions(); len(sessions) != 0 {
		t.Fatalf("leftover sessions = %v", sessions)
	}
}

func TestCollectClaudeUsageWiringUsesNeutralDir(t *testing.T) {
	a := &app{settingsPath: filepath.Join(t.TempDir(), "settings.json"), claudeProbeDir: t.TempDir()}
	if err := a.saveSettings(settings{ClaudeUsageChecks: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.collectClaudeUsage(context.Background()); err != errClaudeUsageProbe {
		t.Fatalf("missing server error = %v", err)
	}
	a.server = tmuxsession.NewServerWithRunner("synthetic", func(context.Context, string, ...string) (string, error) { return "", nil })
	if _, err := a.collectClaudeUsage(context.Background()); err == nil || !strings.Contains(err.Error(), "no open Claude session") {
		t.Fatalf("no node error = %v", err)
	}
	a.nodes = []*Node{{ID: "synthetic", Agent: "claude"}}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if _, err := a.collectClaudeUsage(context.Background()); err == nil || !strings.Contains(err.Error(), "claude not installed") {
		t.Fatalf("not installed error = %v", err)
	}
	// LookPath can find only this independently synthetic executable. The fake
	// tmux runner below never executes it or any launch command.
	writeScript(t, dir, "claude", "exit 0")
	a.server = tmuxsession.NewServerWithRunner("synthetic", func(_ context.Context, _ string, args ...string) (string, error) {
		for i, arg := range args {
			if arg == "-c" && args[i+1] != a.claudeProbeDir {
				t.Fatalf("usage cwd = %q", args[i+1])
			}
			if arg == "new-session" {
				used, reset := 17.0, time.Now().Add(time.Hour).Unix()
				b, _ := json.Marshal(claudeUsageMarker{FiveHourUsed: &used, FiveHourReset: &reset, At: time.Now().UTC().Format(time.RFC3339Nano)})
				if err := os.WriteFile(claudeUsageMarkerPath(a.claudeProbeDir), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return "", nil
	})
	got, err := a.collectClaudeUsage(context.Background())
	if err != nil || got.FiveHourUsed == nil || *got.FiveHourUsed != 17 {
		t.Fatalf("usage = %+v, error = %v", got, err)
	}
}

func probeTmux(t *testing.T) *tmuxsession.Server {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped with -short")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
	// The prefix is deliberately not probeSessionPrefix: that one names
	// *sessions* on scimux's own socket and is load-bearing (isProbeSession
	// keeps them off the adoption surface), while this names a private tmux
	// *socket* that exists only for this test. Sharing the string made the two
	// look like one namespace when reading /tmp/tmux-<uid>.
	sv := tmuxsession.NewServer(fmt.Sprintf("scimux-test-%d", rand.Int63()))
	// Killing the server does not remove its socket file — tmux leaves it
	// behind — so the test must, or every run adds one to the user's
	// /tmp/tmux-<uid>.
	t.Cleanup(func() {
		if err := sv.KillServer(); err != nil {
			t.Errorf("kill server %s: %v", sv.Socket, err)
		}
		if err := os.Remove(sv.SocketPath()); err != nil && !os.IsNotExist(err) {
			t.Errorf("leftover tmux socket %s: %v", sv.SocketPath(), err)
		}
	})
	return sv
}

func probeOpts(t *testing.T, sv *tmuxsession.Server, command string) claudeProbeOptions {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return claudeProbeOptions{
		Dir:      dir,
		ExecPath: "/usr/bin/scimux",
		Server:   sv,
		Command:  command,
		Timeout:  8 * time.Second,
		Poll:     20 * time.Millisecond,
	}
}

// writeMarkerCmd is a stand-in for a probe session that reached a quota
// reading: it writes what the status-line helper would have written.
func writeMarkerCmd(dir string, used float64, resetIn time.Duration) string {
	m := claudeUsageMarker{At: time.Now().UTC().Format(time.RFC3339Nano)}
	m.FiveHourUsed = &used
	r := time.Now().Add(resetIn).Unix()
	m.FiveHourReset = &r
	b, _ := json.Marshal(m)
	return "bash --norc -c " + shellQuote("printf %s "+shellQuote(string(b))+" > "+shellQuote(claudeUsageMarkerPath(dir))+"; sleep 30")
}

func TestClaudeUsageProbeReadsMarker(t *testing.T) {
	sv := probeTmux(t)
	opts := probeOpts(t, sv, "")
	opts.Command = writeMarkerCmd(opts.Dir, 37, time.Hour)
	u, err := runClaudeUsageProbe(context.Background(), opts)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if u.FiveHourUsed == nil || *u.FiveHourUsed != 37 {
		t.Fatalf("five hour used = %v", u.FiveHourUsed)
	}
	if u.Source != "claude-statusline" {
		t.Fatalf("source = %q", u.Source)
	}
	// The probe session is a throwaway: it must not outlive the reading.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(sv.Sessions()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("probe session survived: %v", sv.Sessions())
}

// A probe that never reaches a quota reading is "unavailable" — and still
// tears its session down.
func TestClaudeUsageProbeTimesOutAndCleansUp(t *testing.T) {
	sv := probeTmux(t)
	opts := probeOpts(t, sv, "bash --norc -c 'sleep 30'")
	opts.Timeout = 700 * time.Millisecond
	if _, err := runClaudeUsageProbe(context.Background(), opts); err == nil {
		t.Fatal("probe with no marker reported usage")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(sv.Sessions()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("probe session survived: %v", sv.Sessions())
}

// A marker left by an earlier probe must never be served as this probe's
// reading: the collector's whole job is to state a current number.
func TestClaudeUsageProbeIgnoresStaleMarker(t *testing.T) {
	sv := probeTmux(t)
	opts := probeOpts(t, sv, "bash --norc -c 'sleep 30'")
	opts.Timeout = 700 * time.Millisecond
	stale := 99.0
	reset := time.Now().Add(time.Hour).Unix()
	if err := writeClaudePermFile(claudeUsageMarkerPath(opts.Dir), claudeUsageMarker{
		FiveHourUsed: &stale, FiveHourReset: &reset, At: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	if u, err := runClaudeUsageProbe(context.Background(), opts); err == nil {
		t.Fatalf("stale marker served as a fresh reading: %v", u.FiveHourUsed)
	}
	if _, ok := readClaudeUsageMarker(opts.Dir); ok {
		t.Fatal("stale marker still on disk after a probe")
	}
}

func TestClaudeUsageProbeRejectsIncompleteOptions(t *testing.T) {
	sv := probeTmux(t)
	base := probeOpts(t, sv, "bash --norc -c 'sleep 1'")
	for name, mutate := range map[string]func(*claudeProbeOptions){
		"no dir":    func(o *claudeProbeOptions) { o.Dir = "" },
		"rel dir":   func(o *claudeProbeOptions) { o.Dir = "probe" },
		"no exec":   func(o *claudeProbeOptions) { o.ExecPath = "" },
		"no server": func(o *claudeProbeOptions) { o.Server = nil },
	} {
		o := base
		mutate(&o)
		if _, err := runClaudeUsageProbe(context.Background(), o); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// The probe argv is the cost control. Each flag is load-bearing and measured:
// dropping any one of them multiplies the tokens a probe spends.
func TestClaudeUsageProbeArgvIsMinimal(t *testing.T) {
	cmd := claudeUsageProbeArgv("/data/probe/settings.json")
	if !strings.HasPrefix(cmd, "CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1 claude ") {
		t.Fatalf("usage probe does not suppress the feedback survey: %q", cmd)
	}
	for _, want := range []string{
		"claude ",
		"--settings '/data/probe/settings.json'",
		"--setting-sources ''",
		"--tools ''",
		"--system-prompt '",
		"--model 'haiku'",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("argv %q missing %q", cmd, want)
		}
	}
	// The prompt rides in argv: a pasted prompt needs the TUI to be ready,
	// and every measured alternative cost more tokens.
	if !strings.HasSuffix(cmd, shellQuote(claudeUsageProbePrompt)) {
		t.Fatalf("argv %q does not end with the probe prompt", cmd)
	}
	// A probe is not a supervised node: it takes none of the flags that make
	// a pane observable, and must never resume or write to a real session.
	for _, forbidden := range []string{"--ax-screen-reader", "--remote-control", "--session-id", "--resume", "--continue", "--add-dir"} {
		if strings.Contains(cmd, forbidden) {
			t.Fatalf("argv %q carries %q", cmd, forbidden)
		}
	}
}

// The probe's settings file installs the status line and nothing else. It is
// not a hook bundle: no hooks key, no capabilities, and it must not be
// world-readable.
func TestClaudeUsageProbeSettingsInstallOnlyTheStatusLine(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, err := writeClaudeUsageProbeSettings(dir, "/usr/bin/scimux")
	if err != nil {
		t.Fatalf("write settings: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode = %v", st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("settings not JSON: %v", err)
	}
	if _, ok := doc["hooks"]; ok {
		t.Fatal("probe settings carry hooks")
	}
	sl, ok := doc["statusLine"].(map[string]any)
	if !ok {
		t.Fatalf("no statusLine in %s", b)
	}
	if sl["type"] != "command" {
		t.Fatalf("statusLine type = %v", sl["type"])
	}
	cmd, _ := sl["command"].(string)
	if !strings.Contains(cmd, claudeUsageStatusLineCmd) || !strings.Contains(cmd, "--dir "+shellQuote(dir)) {
		t.Fatalf("statusLine command = %q", cmd)
	}
}

// writeModelMarkerCmd stands in for a session that rendered its status line.
func writeModelMarkerCmd(dir, id, display string) string {
	b, _ := json.Marshal(claudeModelMarker{ID: id, DisplayName: display, At: time.Now().UTC().Format(time.RFC3339Nano)})
	return "bash --norc -c " + shellQuote("printf %s "+shellQuote(string(b))+" > "+shellQuote(claudeModelMarkerPath(dir))+"; sleep 30")
}

// The whole point of this probe is that it is free, and it is free only
// because nothing is ever submitted to the pane. A prompt in this argv — or a
// -p that would run it headless, where no status line renders at all — turns a
// zero-token catalog read back into a billed turn.
func TestClaudeModelProbeArgvSubmitsNothing(t *testing.T) {
	argv := claudeModelProbeArgv("/probe/settings.json", "claude-opus-5")
	if !strings.HasPrefix(argv, "CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1 claude ") {
		t.Fatalf("model probe does not suppress the feedback survey: %q", argv)
	}
	for _, want := range []string{
		"--settings " + shellQuote("/probe/settings.json"),
		"--setting-sources " + shellQuote(""),
		"--tools " + shellQuote(""),
		"--model " + shellQuote("claude-opus-5"),
	} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv %q missing %q", argv, want)
		}
	}
	for _, forbidden := range []string{" -p ", "--print", claudeUsageProbePrompt} {
		if strings.Contains(argv, forbidden) {
			t.Fatalf("argv %q must not carry %q — that would cost a turn", argv, forbidden)
		}
	}
	if !strings.HasSuffix(argv, shellQuote("claude-opus-5")) {
		t.Fatalf("argv %q must end at the model: a trailing argument is a prompt", argv)
	}
}

func TestClaudeModelProbeReadsMarker(t *testing.T) {
	sv := probeTmux(t)
	opts := probeOpts(t, sv, "")
	opts.Command = writeModelMarkerCmd(opts.Dir, "claude-opus-5", "Opus 5")
	m, err := runClaudeModelProbe(context.Background(), opts, "claude-opus-5")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if m.ID != "claude-opus-5" || m.DisplayName != "Opus 5" {
		t.Fatalf("got %+v", m)
	}
	for _, s := range sv.Sessions() {
		if strings.HasPrefix(s, "scimux-") {
			t.Fatalf("probe session %q outlived the probe", s)
		}
	}
}

// A marker from an earlier candidate must never be read as this one's answer;
// otherwise every family would resolve to whatever was probed first.
func TestClaudeModelProbeIgnoresStaleMarker(t *testing.T) {
	sv := probeTmux(t)
	opts := probeOpts(t, sv, "bash --norc -c "+shellQuote("sleep 30"))
	opts.Timeout = 1500 * time.Millisecond
	stale, _ := json.Marshal(claudeModelMarker{ID: "claude-sonnet-5", DisplayName: "Sonnet 5", At: time.Now().UTC().Format(time.RFC3339Nano)})
	if err := os.WriteFile(claudeModelMarkerPath(opts.Dir), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runClaudeModelProbe(context.Background(), opts, "claude-opus-5"); err == nil {
		t.Fatal("a stale marker must not satisfy a probe that reported nothing")
	}
}

// Every probe session name must fall inside the reserved namespace, because
// that prefix — not a lifetime registration — is what keeps a probe out of
// the adoption surface. Ordinary node ids must stay outside it.
func TestProbeSessionNamesAreReserved(t *testing.T) {
	for i := 0; i < 8; i++ {
		name, err := claudeUsageProbeSessionName()
		if err != nil {
			t.Fatal(err)
		}
		if !isProbeSession(name) {
			t.Errorf("claudeUsageProbeSessionName() = %q, outside the probe namespace", name)
		}
		if !tmuxsession.ValidName(name) {
			t.Errorf("claudeUsageProbeSessionName() = %q, not a valid tmux session name", name)
		}
	}
	for _, name := range []string{"", "ghost", "my-node", "scimux", "scimux-notes", "usage-1"} {
		if isProbeSession(name) {
			t.Errorf("isProbeSession(%q) = true, want false", name)
		}
	}
}

func TestRunClaudeProbeSessionLaunchesInCwd(t *testing.T) {
	sv := probeTmux(t)
	opts := probeOpts(t, sv, "")
	opts.Cwd = t.TempDir()
	assertProbeCwd(t, opts, opts.Cwd)
	entries, err := os.ReadDir(opts.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("launch cwd was written: %v", entries)
	}
}

func assertProbeCwd(t *testing.T, opts claudeProbeOptions, want string) {
	t.Helper()
	path := filepath.Join(opts.Dir, "pwd")
	opts.Command = "bash --norc -c " + shellQuote("pwd > "+shellQuote(path)+"; sleep 30")
	err := runClaudeProbeSession(context.Background(), opts, claudeUsageProbeArgv, func(*tmuxsession.Session) bool {
		_, err := os.Stat(path)
		return err == nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != want {
		t.Fatalf("pwd = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(opts.Dir, claudeUsageProbeSettingsName)); err != nil {
		t.Fatalf("settings not in marker directory: %v", err)
	}
}

func TestRunClaudeProbeSessionEmptyCwdUsesDir(t *testing.T) {
	opts := probeOpts(t, probeTmux(t), "")
	assertProbeCwd(t, opts, opts.Dir)
}

func TestRunClaudeProbeSessionInvalidCwdUsesDir(t *testing.T) {
	sv := probeTmux(t)
	for _, name := range []string{"relative", "missing", "unclean", "file"} {
		t.Run(name, func(t *testing.T) {
			opts := probeOpts(t, sv, "")
			switch name {
			case "relative":
				opts.Cwd = "relative"
			case "missing":
				opts.Cwd = filepath.Join(t.TempDir(), "missing")
			case "unclean":
				opts.Cwd = opts.Dir + "/."
			case "file":
				opts.Cwd = filepath.Join(opts.Dir, "file")
				if err := os.WriteFile(opts.Cwd, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			assertProbeCwd(t, opts, opts.Dir)
		})
	}
}

func TestClaudeUsageProbeOptionsLeaveCwdEmpty(t *testing.T) {
	a := &app{home: t.TempDir(), claudeProbeDir: t.TempDir()}
	writeTrustedProjects(t, a.home, map[string]any{a.home: map[string]bool{"hasTrustDialogAccepted": true}})
	opts := a.claudeUsageProbeOptions("test-exe")
	if opts.Cwd != "" || opts.Dir != a.claudeProbeDir || opts.Server != a.server || opts.ExecPath != "test-exe" {
		t.Fatalf("usage options = %+v", opts)
	}
}

const syntheticProbeTrustPane = "Accessing workspace\nChoose whether to trust this folder\n❯ No, exit\n  Yes, I trust this folder\nEnter to confirm · Esc to cancel"

func TestClaudeProbeTrustPromptDetector(t *testing.T) {
	for _, tt := range []struct {
		name, pane string
		want       bool
	}{
		{"arrow", syntheticProbeTrustPane, true},
		{"lettered", "Is this a project you created?\nA. Yes, I trust this folder\nB. No, exit", true},
		{"ansi-and-case", "Is this \x1b[32mONE YOU TRUST\x1b[0m?\nYes, I TRUST \x1b[1mTHIS\x1b[0m FOLDER", true},
		{"picker", claudeModelPickerPane, false},
		{"empty", "", false},
		{"conversation", "We trust the result of this calculation.", false},
		{"folder-only", "I trust this folder", false},
		{"workspace-only", "Accessing workspace", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeProbeLooksUntrusted(tt.pane); got != tt.want {
				t.Fatalf("trust prompt = %v, want %v", got, tt.want)
			}
		})
	}
}

func trustProbeOpts(t *testing.T) claudeProbeOptions {
	t.Helper()
	opts := probeOpts(t, probeTmux(t), "bash --norc -c "+shellQuote("printf %s "+shellQuote(syntheticProbeTrustPane)+"; sleep 30"))
	opts.Timeout = 20 * time.Second
	return opts
}

func TestRunClaudeProbeSessionAbortsTrustPrompt(t *testing.T) {
	opts := trustProbeOpts(t)
	start := time.Now()
	err := runClaudeProbeSession(context.Background(), opts, claudeUsageProbeArgv, func(*tmuxsession.Session) bool { return false })
	if !errors.Is(err, errClaudeProbeUntrusted) {
		t.Fatalf("error = %v, want untrusted", err)
	}
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Fatalf("trust prompt took %v", elapsed)
	}
	if sessions := opts.Server.Sessions(); len(sessions) != 0 {
		t.Fatalf("leftover sessions = %v", sessions)
	}
}

func TestClaudeUsageProbeTrustPromptStaysUsageError(t *testing.T) {
	opts := trustProbeOpts(t)
	start := time.Now()
	_, err := runClaudeUsageProbe(context.Background(), opts)
	if err != errClaudeUsageProbe {
		t.Fatalf("error = %v, want usage unavailable", err)
	}
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Fatalf("trust prompt took %v", elapsed)
	}
	if sessions := opts.Server.Sessions(); len(sessions) != 0 {
		t.Fatalf("leftover sessions = %v", sessions)
	}
}
