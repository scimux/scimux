package app

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// probeDir is a marker directory shaped the way the probe builds one.
func probeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	return real
}

func runStatusLine(t *testing.T, dir, payload string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := RunClaudeUsageStatusLine(dir, strings.NewReader(payload), &out, io.Discard)
	return out.String(), err
}

func readMarker(t *testing.T, dir string) (claudeUsageMarker, bool) {
	t.Helper()
	return readClaudeUsageMarker(dir)
}

// A full snapshot yields both windows.
func TestClaudeStatusLineWritesBothWindows(t *testing.T) {
	dir := probeDir(t)
	reset := time.Now().Add(2 * time.Hour).Unix()
	weekly := time.Now().Add(72 * time.Hour).Unix()
	payload := `{"session_id":"s","rate_limits":{"five_hour":{"used_percentage":22,"resets_at":` +
		itoa(reset) + `},"seven_day":{"used_percentage":8,"resets_at":` + itoa(weekly) + `}}}`
	if _, err := runStatusLine(t, dir, payload); err != nil {
		t.Fatalf("status line: %v", err)
	}
	m, ok := readMarker(t, dir)
	if !ok {
		t.Fatal("no marker written")
	}
	if m.FiveHourUsed == nil || *m.FiveHourUsed != 22 {
		t.Fatalf("five hour used = %v", m.FiveHourUsed)
	}
	if m.WeeklyUsed == nil || *m.WeeklyUsed != 8 {
		t.Fatalf("weekly used = %v", m.WeeklyUsed)
	}
	u, err := claudeUsageFromMarker(m, time.Now())
	if err != nil {
		t.Fatalf("from marker: %v", err)
	}
	if u.Source != "claude-statusline" {
		t.Fatalf("source = %q", u.Source)
	}
	if u.FiveHourRemaining == nil || *u.FiveHourRemaining != 78 {
		t.Fatalf("five hour remaining = %v", u.FiveHourRemaining)
	}
	if u.FiveHourReset == nil || u.FiveHourReset.Unix() != reset {
		t.Fatalf("five hour reset = %v", u.FiveHourReset)
	}
}

// A Team/Pro account with no weekly window is still a usable snapshot.
func TestClaudeStatusLineFiveHourOnlyIsUsable(t *testing.T) {
	dir := probeDir(t)
	payload := `{"rate_limits":{"five_hour":{"used_percentage":12,"resets_at":` +
		itoa(time.Now().Add(time.Hour).Unix()) + `}}}`
	if _, err := runStatusLine(t, dir, payload); err != nil {
		t.Fatalf("status line: %v", err)
	}
	m, ok := readMarker(t, dir)
	if !ok {
		t.Fatal("no marker written")
	}
	if m.WeeklyUsed != nil {
		t.Fatalf("weekly invented: %v", m.WeeklyUsed)
	}
	if _, err := claudeUsageFromMarker(m, time.Now()); err != nil {
		t.Fatalf("five-hour-only must be usable: %v", err)
	}
}

// The status line fires several times before the first API response, with no
// rate_limits at all. Those calls must not create a marker, and must never
// clobber a good one written by an earlier call in the same session.
func TestClaudeStatusLineWithoutRateLimitsKeepsPriorMarker(t *testing.T) {
	dir := probeDir(t)
	good := `{"rate_limits":{"five_hour":{"used_percentage":30,"resets_at":` +
		itoa(time.Now().Add(time.Hour).Unix()) + `}}}`
	if _, err := runStatusLine(t, dir, good); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := runStatusLine(t, dir, `{"session_id":"s","model":{"id":"haiku"}}`); err == nil {
		t.Fatal("a payload with no quota window must report an error")
	}
	m, ok := readMarker(t, dir)
	if !ok || m.FiveHourUsed == nil || *m.FiveHourUsed != 30 {
		t.Fatalf("prior marker clobbered: ok=%v m=%+v", ok, m)
	}
}

func TestClaudeStatusLineNoMarkerBeforeFirstQuotaReading(t *testing.T) {
	dir := probeDir(t)
	if _, err := runStatusLine(t, dir, `{"session_id":"s"}`); err == nil {
		t.Fatal("expected error for a payload with no rate_limits")
	}
	if _, ok := readMarker(t, dir); ok {
		t.Fatal("marker written for a payload with no quota window")
	}
}

// A null/absent percentage is an unknown gauge, never 0% used.
func TestClaudeStatusLineNullPercentageIsNotZero(t *testing.T) {
	dir := probeDir(t)
	payload := `{"rate_limits":{"five_hour":{"used_percentage":null,"resets_at":123},"seven_day":{"resets_at":456}}}`
	if _, err := runStatusLine(t, dir, payload); err == nil {
		t.Fatal("a window with no percentage is not a usable window")
	}
	if _, ok := readMarker(t, dir); ok {
		t.Fatal("marker written from a null percentage")
	}
}

// The runway may be unknown while the budget is known.
func TestClaudeStatusLineMissingResetKeepsTheGauge(t *testing.T) {
	dir := probeDir(t)
	if _, err := runStatusLine(t, dir, `{"rate_limits":{"five_hour":{"used_percentage":40}}}`); err != nil {
		t.Fatalf("status line: %v", err)
	}
	m, ok := readMarker(t, dir)
	if !ok || m.FiveHourUsed == nil || *m.FiveHourUsed != 40 {
		t.Fatalf("gauge lost: ok=%v m=%+v", ok, m)
	}
	u, err := claudeUsageFromMarker(m, time.Now())
	if err != nil {
		t.Fatalf("from marker: %v", err)
	}
	if u.FiveHourReset != nil {
		t.Fatalf("reset invented: %v", u.FiveHourReset)
	}
}

// A reset that cannot be read is not a reason to drop the budget.
func TestClaudeStatusLineMalformedResetKeepsTheGauge(t *testing.T) {
	dir := probeDir(t)
	if _, err := runStatusLine(t, dir, `{"rate_limits":{"five_hour":{"used_percentage":40,"resets_at":"soon"}}}`); err != nil {
		t.Fatalf("status line: %v", err)
	}
	m, ok := readMarker(t, dir)
	if !ok || m.FiveHourUsed == nil {
		t.Fatalf("gauge lost: ok=%v m=%+v", ok, m)
	}
}

// Fields we do not know about must never make a readable window unreadable.
func TestClaudeStatusLineUnknownFieldsAreIgnored(t *testing.T) {
	dir := probeDir(t)
	payload := `{"rate_limits":{"five_hour":{"used_percentage":5,"resets_at":` +
		itoa(time.Now().Add(time.Hour).Unix()) + `,"tier":"pro"},"opus_weekly":{"used_percentage":3}},"future":{"x":[1,2]}}`
	if _, err := runStatusLine(t, dir, payload); err != nil {
		t.Fatalf("status line: %v", err)
	}
	if _, ok := readMarker(t, dir); !ok {
		t.Fatal("unknown fields dropped a usable window")
	}
}

// Unlike every hook helper, this one must print — the status line is its
// stdout. What it prints is a fixed literal, never anything from stdin.
func TestClaudeStatusLineStdoutIsAFixedLiteral(t *testing.T) {
	dir := probeDir(t)
	secret := "SHOULD-NOT-APPEAR"
	payload := `{"cwd":"/` + secret + `","transcript_path":"/` + secret +
		`","rate_limits":{"five_hour":{"used_percentage":9}}}`
	out, err := runStatusLine(t, dir, payload)
	if err != nil {
		t.Fatalf("status line: %v", err)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("stdout echoed stdin: %q", out)
	}
	if strings.TrimSpace(out) != claudeUsageStatusLineText {
		t.Fatalf("stdout = %q, want %q", out, claudeUsageStatusLineText)
	}
	// Even a rejected payload prints the same literal: an empty status line
	// would blank the probe pane's footer and tell the user nothing.
	out2, _ := runStatusLine(t, dir, `{`)
	if strings.TrimSpace(out2) != claudeUsageStatusLineText {
		t.Fatalf("rejected payload stdout = %q", out2)
	}
}

func TestClaudeStatusLineRejectsOversizeStdin(t *testing.T) {
	dir := probeDir(t)
	big := `{"pad":"` + strings.Repeat("x", claudeUsageStdinLimit+16) +
		`","rate_limits":{"five_hour":{"used_percentage":50}}}`
	if _, err := runStatusLine(t, dir, big); err == nil {
		t.Fatal("oversize stdin accepted")
	}
	if _, ok := readMarker(t, dir); ok {
		t.Fatal("marker written from oversize stdin")
	}
}

func TestClaudeStatusLineRejectsBadDir(t *testing.T) {
	payload := `{"rate_limits":{"five_hour":{"used_percentage":1}}}`
	for _, dir := range []string{"", "relative/dir", "/no/such/scimux/probe/dir"} {
		if _, err := runStatusLine(t, dir, payload); err == nil {
			t.Fatalf("dir %q accepted", dir)
		}
	}
}

// A window whose reset has already passed is stale: the CLI drops it, and a
// gauge for a window that has rolled over would misreport the budget.
func TestClaudeUsageMarkerExpiredWindowIsDropped(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Minute).Unix()
	used := 44.0
	m := claudeUsageMarker{FiveHourUsed: &used, FiveHourReset: &past, At: now.Format(time.RFC3339Nano)}
	if _, err := claudeUsageFromMarker(m, now); err == nil {
		t.Fatal("expired window reported as usable")
	}
}

func TestClaudeUsageMarkerEmptyIsUnavailable(t *testing.T) {
	if _, err := claudeUsageFromMarker(claudeUsageMarker{}, time.Now()); err == nil {
		t.Fatal("empty marker reported as usable")
	}
}

// A corrupt or oversized marker file is unreadable, never a partial gauge.
func TestReadClaudeUsageMarkerRejectsJunk(t *testing.T) {
	dir := probeDir(t)
	path := claudeUsageMarkerPath(dir)
	for _, body := range []string{"", "{", "not json", strings.Repeat("x", claudeUsageMarkerMax+1)} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := readClaudeUsageMarker(dir); ok {
			t.Fatalf("junk marker accepted: %.20q", body)
		}
	}
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// FuzzClaudeUsageStatusLine states the two contracts that hold for every
// possible input. The status line is the one scimux helper that prints, so
// what it prints is pinned exactly: a fixed literal, one line, never a byte
// derived from the session snapshot (which carries cwd, transcript paths and
// workspace paths). And a marker only ever exists with a usable window, so no
// input can blank a good reading into a false 0%.
func FuzzClaudeUsageStatusLine(f *testing.F) {
	f.Add(`{"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":4102444800}}}`)
	f.Add(`{"rate_limits":{"five_hour":{"used_percentage":null}}}`)
	f.Add(`{"rate_limits":{}}`)
	f.Add(`{"session_id":"s","cwd":"/home/u/secret"}`)
	f.Add(`{`)
	f.Add(``)
	f.Add(`{"rate_limits":{"seven_day":{"used_percentage":1e9}}}`)
	f.Fuzz(func(t *testing.T, payload string) {
		dir := probeDir(t)
		var out bytes.Buffer
		_ = RunClaudeUsageStatusLine(dir, strings.NewReader(payload), &out, io.Discard)
		if out.String() != claudeUsageStatusLineText+"\n" {
			t.Fatalf("stdout = %q", out.String())
		}
		m, ok := readClaudeUsageMarker(dir)
		if !ok {
			return
		}
		if m.FiveHourUsed == nil && m.WeeklyUsed == nil {
			t.Fatalf("marker with no window: %+v", m)
		}
		if _, err := claudeUsageFromMarker(m, time.Unix(0, 0)); err != nil {
			t.Fatalf("marker written that no collector can read: %+v", m)
		}
	})
}
