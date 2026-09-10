package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/tmuxsession"
)

// probeTmux is a real tmux on a private random socket. The probe launches a
// session, so there is nothing to assert without one; the wrapped command is
// always bash, never a real agent CLI.
func probeTmux(t *testing.T) *tmuxsession.Server {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped with -short")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
	sv := tmuxsession.NewServer(fmt.Sprintf("scimux-usage-%d", rand.Int63()))
	t.Cleanup(func() { sv.KillServer() })
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
