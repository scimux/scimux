// claude_statusline.go — Claude subscription-usage transport.
//
// Claude Code renders a user-supplied status line by running a command and
// piping it a JSON snapshot of the session. That snapshot carries
// rate_limits.{five_hour,seven_day}, which is the only sanctioned surface that
// states subscription quota: it replaces the OAuth call this file's ancestor
// made, which read the Claude CLI's stored credentials file and spoke to an
// undocumented endpoint with the user's own bearer token.
//
// The status line is installed on a throwaway probe session, never on a
// supervised pane (see claude_usage_probe.go for why: a status line replaces
// Claude's "esc to interrupt" footer, which the attention poller reads).
//
// Unlike every hook helper in this package, this helper MUST write to stdout —
// its stdout *is* the status line. What it prints is a fixed literal, never a
// byte derived from stdin, so the "hook helpers are silent" rule is not
// weakened so much as inapplicable: this is not a hook.
//
// Parsing is defensive in the same way transcripts are. rate_limits is absent
// until the session's first API response, each window is independently
// optional (a Team account reports no seven_day), and a window the CLI has
// rolled over is dropped. Any usable window is a usable snapshot; nothing
// usable is "unavailable", and — critically — never a marker overwritten with
// blanks, because the status line fires several times before quota data
// exists.
package app

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// claudeUsageStatusLineCmd is the hidden helper argv token. cmd/scimux only
// dispatches; the probe writes it into the settings file it hands to claude.
const claudeUsageStatusLineCmd = "__claude-usage-statusline"

// claudeUsageStdinLimit bounds the session snapshot. It is a leak backstop:
// the snapshot carries cwd, transcript_path and workspace paths, none of which
// this helper reads or retains.
const claudeUsageStdinLimit = 1 << 20

const claudeUsageMarkerMax = 4096

// claudeUsageStatusLineText is the whole status line. A probe pane is not a
// place to render anything, and printing nothing would leave the pane looking
// broken to anyone who attaches to it.
const claudeUsageStatusLineText = "scimux usage probe"

const claudeUsageMarkerName = "usage.json"

var errClaudeUsageNoWindow = errors.New("usage unavailable: no quota window reported")

// claudeUsageMarker is what one probe leaves on disk for the collector. Resets
// are stored as the unix seconds the snapshot reported, so the file says what
// was observed rather than a re-rendered timestamp.
type claudeUsageMarker struct {
	FiveHourUsed  *float64 `json:"five_hour_used,omitempty"`
	FiveHourReset *int64   `json:"five_hour_reset,omitempty"`
	WeeklyUsed    *float64 `json:"weekly_used,omitempty"`
	WeeklyReset   *int64   `json:"weekly_reset,omitempty"`
	At            string   `json:"at"`
}

// claudeRateLimitWindow holds both members raw on purpose. If either field
// ever changes type upstream, the other must still be readable: a percentage
// that survives a reworked resets_at is the difference between a working gauge
// and a dark one.
type claudeRateLimitWindow struct {
	UsedPercentage json.RawMessage `json:"used_percentage"`
	ResetsAt       json.RawMessage `json:"resets_at"`
}

type claudeStatusLineSnapshot struct {
	RateLimits struct {
		FiveHour *claudeRateLimitWindow `json:"five_hour"`
		SevenDay *claudeRateLimitWindow `json:"seven_day"`
	} `json:"rate_limits"`
}

func claudeUsageMarkerPath(dir string) string {
	return filepath.Join(dir, claudeUsageMarkerName)
}

// claudeUsagePercent reads a utilization. A null, absent, non-numeric or
// out-of-range value is an unknown gauge — never 0% used, which would claim a
// full quota the account may not have.
func claudeUsagePercent(raw json.RawMessage) *float64 {
	if isAbsentJSON(raw) {
		return nil
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		return nil
	}
	if f < 0 || f > 100 {
		return nil
	}
	return &f
}

// claudeUsageReset reads a window reset, accepting both shapes we have seen or
// might see: unix seconds (what the status line sends today) and RFC3339 (what
// the OAuth endpoint sent). Anything else leaves the runway unknown while
// keeping the budget.
func claudeUsageReset(raw json.RawMessage) *int64 {
	if isAbsentJSON(raw) {
		return nil
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		if n <= 0 {
			return nil
		}
		return &n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			u := t.Unix()
			return &u
		}
	}
	return nil
}

// isAbsentJSON treats a literal null as absent. Unmarshalling "null" into a
// float succeeds and leaves zero, which would render an unknown gauge as 0%
// used — the one misreading this whole file exists to prevent.
func isAbsentJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func claudeUsageWindowFields(w *claudeRateLimitWindow) (*float64, *int64) {
	if w == nil {
		return nil, nil
	}
	used := claudeUsagePercent(w.UsedPercentage)
	if used == nil {
		// No gauge means no window. A reset on its own describes a budget we
		// cannot state, which is not something to persist.
		return nil, nil
	}
	return used, claudeUsageReset(w.ResetsAt)
}

// parseClaudeStatusLine projects one status-line snapshot onto a marker.
func parseClaudeStatusLine(body []byte, now time.Time) (claudeUsageMarker, error) {
	var snap claudeStatusLineSnapshot
	if json.Unmarshal(body, &snap) != nil {
		return claudeUsageMarker{}, errors.New("usage unavailable: unexpected status line payload")
	}
	m := claudeUsageMarker{At: now.UTC().Format(time.RFC3339Nano)}
	m.FiveHourUsed, m.FiveHourReset = claudeUsageWindowFields(snap.RateLimits.FiveHour)
	m.WeeklyUsed, m.WeeklyReset = claudeUsageWindowFields(snap.RateLimits.SevenDay)
	if m.FiveHourUsed == nil && m.WeeklyUsed == nil {
		return claudeUsageMarker{}, errClaudeUsageNoWindow
	}
	return m, nil
}

// RunClaudeUsageStatusLine is the hidden helper body. It always prints the
// status line first: a payload it cannot use is the normal case at session
// start, and the pane must still render. Only a snapshot with a usable window
// touches the marker, so an early blank call can never erase a good reading.
func RunClaudeUsageStatusLine(dir string, r io.Reader, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if _, err := io.WriteString(stdout, claudeUsageStatusLineText+"\n"); err != nil {
		return errClaudeHookRejected
	}
	if dir == "" || !filepath.IsAbs(dir) || dir != filepath.Clean(dir) || !safePathComponent(filepath.Base(dir)) {
		return errClaudeHookRejected
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return errClaudeHookRejected
	}
	limited := &io.LimitedReader{R: r, N: claudeUsageStdinLimit + 1}
	body, err := io.ReadAll(limited)
	if err != nil || len(body) > claudeUsageStdinLimit {
		return errClaudeHookRejected
	}
	// The model reading is independent of the quota reading and is attempted
	// first: a payload carries a model on every render, where rate_limits
	// appears only after the session's first API response. Its failure is
	// discarded on purpose — the quota path must not inherit it.
	if mm, err := parseClaudeStatusLineModel(body, time.Now()); err == nil {
		_ = writeClaudePermFile(claudeModelMarkerPath(dir), mm)
	}
	m, err := parseClaudeStatusLine(body, time.Now())
	if err != nil {
		return err
	}
	return writeClaudePermFile(claudeUsageMarkerPath(dir), m)
}

// readClaudeUsageMarker reads the freshest probe result. A marker with no
// window is not a marker: it could only have come from a writer that is not
// this helper.
func readClaudeUsageMarker(dir string) (claudeUsageMarker, bool) {
	var empty claudeUsageMarker
	if dir == "" {
		return empty, false
	}
	path := claudeUsageMarkerPath(dir)
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > claudeUsageMarkerMax {
		return empty, false
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 || len(b) > claudeUsageMarkerMax {
		return empty, false
	}
	var m claudeUsageMarker
	if json.Unmarshal(b, &m) != nil {
		return empty, false
	}
	if m.FiveHourUsed == nil && m.WeeklyUsed == nil {
		return empty, false
	}
	return m, true
}

// claudeLiveReset places a window's reset. A window with no reset keeps its
// gauge (the runway is what is unknown); a window whose reset has passed is
// dropped, because the CLI has rolled it over and the percentage now describes
// a window that no longer exists.
func claudeLiveReset(unix *int64, now time.Time) (*time.Time, bool) {
	if unix == nil {
		return nil, true
	}
	t := time.Unix(*unix, 0).UTC()
	if !t.After(now) {
		return nil, false
	}
	return &t, true
}

// claudeUsageFromMarker is the adapter the collector calls.
func claudeUsageFromMarker(m claudeUsageMarker, now time.Time) (agentUsage, error) {
	shell := agentUsage{Agent: "claude", Source: "claude-statusline"}
	u := shell
	u.ObservedAt = now
	if m.At != "" {
		if t, err := time.Parse(time.RFC3339Nano, m.At); err == nil {
			u.ObservedAt = t
		}
	}
	have := false
	if m.FiveHourUsed != nil {
		if reset, live := claudeLiveReset(m.FiveHourReset, now); live {
			used := *m.FiveHourUsed
			rem := remainingPercent(used)
			u.FiveHourUsed, u.FiveHourRemaining, u.FiveHourReset = &used, &rem, reset
			have = true
		}
	}
	if m.WeeklyUsed != nil {
		if reset, live := claudeLiveReset(m.WeeklyReset, now); live {
			used := *m.WeeklyUsed
			rem := remainingPercent(used)
			u.WeeklyUsed, u.WeeklyRemaining, u.WeeklyReset = &used, &rem, reset
			have = true
		}
	}
	if !have {
		return shell, errClaudeUsageNoWindow
	}
	return u, nil
}

func runClaudeUsageStatusLineMain(args []string) int {
	dir := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			dir = args[i+1]
			i++
		}
	}
	// stdout is real here, unlike every hook helper: claude reads it as the
	// status line. The exit code is always 0 — a failed read is a dark gauge,
	// never a disturbed session.
	_ = RunClaudeUsageStatusLine(dir, os.Stdin, os.Stdout, io.Discard)
	return 0
}
