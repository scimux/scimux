// claude_usage_probe.go — the throwaway session that reads Claude's quota.
//
// Claude only renders a status line in a real TUI (headless `claude -p` never
// invokes it, measured), so reading rate_limits costs a pane. Two shapes were
// possible and the choice is deliberate:
//
//   - Install the status line on every supervised Claude pane. Rejected: a
//     status line replaces Claude's own footer, and with it the "esc to
//     interrupt" anchor that internal/dialoghint and the poller read to
//     suppress false attention. A controlled comparison showed that the
//     anchor disappeared with a status line installed.
//     Supervision is the product; a usage gauge does not get to degrade it.
//   - Spend one throwaway session per reading. Chosen: supervised panes keep
//     their mechanics untouched, and the probe absorbs the footer loss in a
//     pane nobody supervises.
//
// A probe costs an API turn, which is the awkward part of measuring a budget
// by spending it, so the argv is tuned rather than convenient. Cold-cache
// comparisons showed that successively removing tools and setting sources,
// replacing the system prompt, and carrying the prompt in argv reduced the
// input cost to a small fraction of a default TUI launch. Repeated probes did
// not visibly move the displayed quota gauge.
//
// The prompt rides in argv because a pasted prompt would need the TUI to be
// ready first — a readiness poll, a bracketed paste and an Enter, all of which
// this avoids. Nothing is ever sent to a probe pane.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"codeberg.org/chrberger/scimux/internal/tmuxsession"
)

const (
	// claudeUsageProbeModel is the cheapest model that still answers. The
	// reading is the same whichever model produces it.
	claudeUsageProbeModel = "haiku"
	// claudeUsageProbePrompt only has to provoke one API response; rate_limits
	// does not exist in the snapshot until the session has had one.
	claudeUsageProbePrompt = "ping"
	// claudeUsageProbeSystemPrompt replaces Claude Code's own, which is most
	// of what a probe would otherwise pay for.
	claudeUsageProbeSystemPrompt = "Reply with exactly: pong"
	// claudeUsageProbeTimeout bounds session start plus one round trip. Past
	// it the gauge is simply dark until the next refresh.
	claudeUsageProbeTimeout      = 90 * time.Second
	claudeUsageProbePoll         = 250 * time.Millisecond
	claudeUsageProbeSettingsName = "settings.json"
)

var errClaudeUsageProbe = errors.New("usage unavailable: probe did not report")

type claudeUsageProbeOptions struct {
	// Dir is the probe's cwd and the marker's home. It must be a directory no
	// node can own: claude writes a transcript into
	// ~/.claude/projects/<slug of cwd>/, and a probe launched from a
	// supervised node's directory drops a throwaway session where the relink
	// machinery can adopt it.
	Dir      string
	ExecPath string
	Server   *tmuxsession.Server
	Model    string
	// Command overrides the launched argv. Tests only — the suite must never
	// run a real agent CLI.
	Command string
	Timeout time.Duration
	Poll    time.Duration
}

// claudeUsageProbeArgv builds the launch command. Every flag here is a
// measured cost control; see the file header for the numbers.
func claudeUsageProbeArgv(settingsPath, model string) string {
	if model == "" {
		model = claudeUsageProbeModel
	}
	parts := []string{
		"claude",
		"--settings", shellQuote(settingsPath),
		// Load none of the user's own settings: their status line would
		// replace ours, their MCP servers and CLAUDE.md would be billed to
		// this probe, and their hooks have no business firing in it.
		"--setting-sources", shellQuote(""),
		// Tool definitions are the single largest block of a default launch,
		// and a probe has nothing to do.
		"--tools", shellQuote(""),
		"--system-prompt", shellQuote(claudeUsageProbeSystemPrompt),
		"--model", shellQuote(model),
		shellQuote(claudeUsageProbePrompt),
	}
	return strings.Join(parts, " ")
}

// writeClaudeUsageProbeSettings installs the status line and nothing else.
// This is deliberately not a hook bundle: a probe proves no ownership, binds
// no transcript and answers no approval, so it gets no capabilities file and
// no hooks key.
func writeClaudeUsageProbeSettings(dir, execPath string) (string, error) {
	if dir == "" || execPath == "" {
		return "", errClaudeHookRejected
	}
	doc := map[string]any{
		"statusLine": map[string]any{
			"type":    "command",
			"command": shellQuote(execPath) + " " + claudeUsageStatusLineCmd + " --dir " + shellQuote(dir),
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, claudeUsageProbeSettingsName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// runClaudeUsageProbe spends one throwaway session and returns what it read.
//
// The marker is removed first and the probe waits for it to reappear: the
// status line fires several times before the session's first API response,
// carrying no rate_limits and writing nothing, so the file's arrival is
// exactly the signal that a quota reading exists. A marker left by an earlier
// probe must never be mistaken for this one's answer.
func runClaudeUsageProbe(ctx context.Context, opts claudeUsageProbeOptions) (agentUsage, error) {
	shell := agentUsage{Agent: "claude", Source: "claude-statusline"}
	if opts.Server == nil || opts.ExecPath == "" {
		return shell, errClaudeUsageProbe
	}
	dir := opts.Dir
	if dir == "" || !filepath.IsAbs(dir) || dir != filepath.Clean(dir) || !safePathComponent(filepath.Base(dir)) {
		return shell, errClaudeUsageProbe
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return shell, errClaudeUsageProbe
	}
	if err := os.Remove(claudeUsageMarkerPath(dir)); err != nil && !os.IsNotExist(err) {
		return shell, errClaudeUsageProbe
	}
	settings, err := writeClaudeUsageProbeSettings(dir, opts.ExecPath)
	if err != nil {
		return shell, errClaudeUsageProbe
	}
	command := opts.Command
	if command == "" {
		command = claudeUsageProbeArgv(settings, opts.Model)
	}
	name, err := claudeUsageProbeSessionName()
	if err != nil {
		return shell, errClaudeUsageProbe
	}
	sess, err := opts.Server.NewSession(name, dir, command)
	if err != nil {
		return shell, errClaudeUsageProbe
	}
	// The session is a throwaway in every exit path, including a cancelled
	// context: an orphaned probe pane would keep a claude process alive.
	defer sess.Kill()

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = claudeUsageProbeTimeout
	}
	poll := opts.Poll
	if poll <= 0 {
		poll = claudeUsageProbePoll
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		if m, ok := readClaudeUsageMarker(dir); ok {
			return claudeUsageFromMarker(m, time.Now())
		}
		select {
		case <-ctx.Done():
			return shell, errClaudeUsageProbe
		case <-deadline.C:
			return shell, errClaudeUsageProbe
		case <-tick.C:
		}
	}
}

func claudeUsageProbeSessionName() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "scimux-usage-" + hex.EncodeToString(b), nil
}

// ---------- app wiring ----------

// collectClaudeUsage is the collector usage.go calls for "claude".
//
// It probes only while the user actually has a Claude session open. The
// refresh policy in usage.go already limits this to at most one reading per
// usageMinInterval while prompts keep arriving, which is the whole schedule:
// no wall clock, no background timer, and nothing at all overnight. A codex or
// grok prompt drives the same refresh cycle, so without this gate a user who
// is not touching Claude would still be billed for a Claude reading.
func (a *app) collectClaudeUsage(ctx context.Context) (agentUsage, error) {
	shell := agentUsage{Agent: "claude", Source: "claude-statusline"}
	if a == nil || a.server == nil {
		return shell, errClaudeUsageProbe
	}
	if !a.hasLiveClaudeNode() {
		return shell, errors.New("usage unavailable: no open Claude session")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		return shell, errors.New("usage unavailable: claude not installed")
	}
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return shell, errClaudeUsageProbe
	}
	return runClaudeUsageProbe(ctx, claudeUsageProbeOptions{
		Dir:      claudeProbeWorkdir(a.claudeProbeDir),
		ExecPath: exe,
		Server:   a.server,
	})
}

// hasLiveClaudeNode reports whether any Claude node is currently open. Ended
// and exited nodes do not count: a probe is only justified by a session the
// user could still be spending quota in.
func (a *app) hasLiveClaudeNode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, n := range a.nodes {
		if n == nil || n.Agent != "claude" || n.EndedAt != "" {
			continue
		}
		if a.live[n.ID] == "exited" {
			continue
		}
		return true
	}
	return false
}
