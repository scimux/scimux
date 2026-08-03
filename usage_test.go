package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp"
)

// ---------- collectors ----------

func TestQueryCodexUsageNewestValidRecord(t *testing.T) {
	dir := t.TempDir()
	day := filepath.Join(dir, "2026", "07", "27")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFixture(t, "testdata/codex-session-sample.jsonl", filepath.Join(day, "rollout.jsonl"))
	u, err := queryCodexUsage(dir)
	if err != nil {
		t.Fatalf("queryCodexUsage: %v", err)
	}
	if u.Agent != "codex" || u.Plan != "edu" || u.FiveHourUsed == nil || *u.FiveHourUsed != 25 {
		t.Fatalf("usage = %+v, want newest codex record (used 25)", u)
	}
	if u.FiveHourRemaining == nil || *u.FiveHourRemaining != 75 {
		t.Fatalf("five hour remaining = %v, want 75", u.FiveHourRemaining)
	}
	if u.WeeklyRemaining == nil || *u.WeeklyRemaining != 96 {
		t.Fatalf("weekly remaining = %v, want 96", u.WeeklyRemaining)
	}
}

func TestQueryCodexUsageUnavailable(t *testing.T) {
	dir := t.TempDir()
	// malformed JSON, unknown type, and a wrong-window record are all ignored.
	writeJSONL(t, filepath.Join(dir, "x.jsonl"),
		"not-json",
		`{"type":"other"}`,
		`{"timestamp":"2026-07-27T08:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":9,"window_minutes":60,"resets_at":1},"secondary":{"used_percent":9,"window_minutes":10080,"resets_at":1}}}}`)
	u, err := queryCodexUsage(dir)
	if err == nil {
		t.Fatal("expected unavailable error")
	}
	if u.Agent != "codex" || u.Source != "codex-session-jsonl" {
		t.Fatalf("usage = %+v, want codex/unavailable shell", u)
	}
}

func TestQueryCodexUsageMissingDir(t *testing.T) {
	u, err := queryCodexUsage(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected unavailable for missing dir")
	}
	if u.Agent != "codex" {
		t.Fatalf("agent = %q, want codex", u.Agent)
	}
}

func TestParseCodexLineMissingResetsAt(t *testing.T) {
	// A token_count record with used_percent present but resets_at absent must
	// leave the reset unknown (nil), not epoch 0 / 1970-01-01.
	line := []byte(`{"timestamp":"2026-07-27T08:00:00Z","type":"event_msg",` +
		`"payload":{"type":"token_count","rate_limits":{` +
		`"primary":{"used_percent":25,"window_minutes":300},` +
		`"secondary":{"used_percent":4,"window_minutes":10080}}}}`)
	u, _, ok := parseCodexLine(line)
	if !ok {
		t.Fatal("parseCodexLine rejected a valid record missing resets_at")
	}
	if u.FiveHourReset != nil {
		t.Fatalf("FiveHourReset = %v, want nil for absent resets_at", *u.FiveHourReset)
	}
	if u.WeeklyReset != nil {
		t.Fatalf("WeeklyReset = %v, want nil for absent resets_at", *u.WeeklyReset)
	}
	// A present resets_at is still surfaced.
	line2 := []byte(`{"timestamp":"2026-07-27T08:00:00Z","type":"event_msg",` +
		`"payload":{"type":"token_count","rate_limits":{` +
		`"primary":{"used_percent":25,"window_minutes":300,"resets_at":1800000000},` +
		`"secondary":{"used_percent":4,"window_minutes":10080,"resets_at":1800000000}}}}`)
	u2, _, ok := parseCodexLine(line2)
	if !ok || u2.FiveHourReset == nil || u2.WeeklyReset == nil {
		t.Fatalf("parseCodexLine dropped a present resets_at: ok=%v u=%+v", ok, u2)
	}
	if !u2.FiveHourReset.Equal(time.Unix(1800000000, 0)) {
		t.Fatalf("FiveHourReset = %v, want %v", u2.FiveHourReset, time.Unix(1800000000, 0))
	}
}

func TestQueryClaudeUsageNormalizes(t *testing.T) {
	dir := t.TempDir()
	creds := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(creds, []byte(`{"claudeAiOauth":{"accessToken":"test-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var sawAuth, sawBeta bool
	body := readFixture(t, "testdata/claude-oauth-usage-success.json")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sawAuth = r.Header.Get("Authorization") == "Bearer test-token"
		sawBeta = r.Header.Get("anthropic-beta") == "oauth-2025-04-20"
		return jsonResponse(200, string(body)), nil
	})}

	u, err := queryClaudeUsage(context.Background(), claudeUsageOptions{
		CredentialsPath: creds,
		UsageURL:        "https://example.invalid/usage",
		Client:          client,
	})
	if err != nil {
		t.Fatalf("queryClaudeUsage: %v", err)
	}
	if !sawAuth || !sawBeta {
		t.Fatalf("headers: auth=%v beta=%v, want both set", sawAuth, sawBeta)
	}
	// fixture five_hour utilization 12 -> remaining 88; seven_day 34 -> 66.
	if u.Agent != "claude" || u.FiveHourRemaining == nil || *u.FiveHourRemaining != 88 {
		t.Fatalf("usage = %+v, want five-hour remaining 88", u)
	}
	if u.WeeklyRemaining == nil || *u.WeeklyRemaining != 66 {
		t.Fatalf("weekly remaining = %v, want 66", u.WeeklyRemaining)
	}
	if u.ExtraUsageEnabled == nil || !*u.ExtraUsageEnabled || u.ExtraUsageCurrency != "USD" {
		t.Fatalf("extra usage = %+v, want enabled USD", u)
	}
	if u.FiveHourReset == nil || u.FiveHourReset.IsZero() {
		t.Fatal("five-hour reset not parsed")
	}
}

func TestQueryClaudeUsageAuthFailureIsGeneric(t *testing.T) {
	dir := t.TempDir()
	creds := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(creds, []byte(`{"claudeAiOauth":{"accessToken":"test-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"error":"secret account detail"}`), nil
	})}
	u, err := queryClaudeUsage(context.Background(), claudeUsageOptions{
		CredentialsPath: creds, UsageURL: "https://example.invalid/usage", Client: client,
	})
	if err == nil || !strings.Contains(err.Error(), "auth status 401") {
		t.Fatalf("err = %v, want generic auth status", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaked response body: %v", err)
	}
	if u.Agent != "claude" {
		t.Fatalf("agent = %q, want claude", u.Agent)
	}
}

func TestQueryClaudeUsageMissingCredsIsGeneric(t *testing.T) {
	_, err := queryClaudeUsage(context.Background(), claudeUsageOptions{
		CredentialsPath: filepath.Join(t.TempDir(), "absent.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "credentials missing") {
		t.Fatalf("err = %v, want credentials missing", err)
	}
}

func TestClaudeCredentialErrorsDoNotLeakContent(t *testing.T) {
	dir := t.TempDir()
	creds := filepath.Join(dir, "credentials.json")
	// Truncated JSON containing a token-looking prefix; the error must not echo it.
	if err := os.WriteFile(creds, []byte(`{"claudeAiOauth":{"accessToken":"sk-secret`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := queryClaudeUsage(context.Background(), claudeUsageOptions{CredentialsPath: creds})
	if err == nil {
		t.Fatal("expected invalid credentials error")
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("error leaked credential content: %v", err)
	}
}

// ---------- endpoint ----------

func TestHandleUsageServesCacheOnly(t *testing.T) {
	var calls int32
	c := newUsageCache(func(ctx context.Context, agent string) (agentUsage, error) {
		atomic.AddInt32(&calls, 1)
		u := 10.0
		rem := 90.0
		return agentUsage{Agent: agent, FiveHourUsed: &u, FiveHourRemaining: &rem, Source: "test"}, nil
	})
	// Seed a snapshot directly, then hit the endpoint repeatedly.
	c.snap = usageSnapshot{ObservedAt: time.Now(), Agents: map[string]agentUsageView{
		"codex": {Available: true, Source: "codex-session-jsonl"},
	}}
	a := &app{usage: c}
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		a.handleUsage(rec, httptest.NewRequest("GET", "/api/usage", nil))
		if rec.Code != 200 {
			t.Fatalf("code = %d", rec.Code)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("collector called %d times; endpoint must be cache-only", got)
	}
	var snap usageSnapshot
	rec := httptest.NewRecorder()
	a.handleUsage(rec, httptest.NewRequest("GET", "/api/usage", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !snap.Agents["codex"].Available {
		t.Fatalf("snapshot = %+v, want seeded codex available", snap)
	}
}

func TestHandleUsageEmptyCacheUnavailable(t *testing.T) {
	a := &app{usage: newUsageCache(func(context.Context, string) (agentUsage, error) {
		t.Fatal("collector must not run for empty-cache read")
		return agentUsage{}, nil
	})}
	rec := httptest.NewRecorder()
	a.handleUsage(rec, httptest.NewRequest("GET", "/api/usage", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d, want 200 even with empty cache", rec.Code)
	}
	var snap usageSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, agent := range []string{"codex", "claude", "grok"} {
		if v, ok := snap.Agents[agent]; !ok || v.Available {
			t.Fatalf("%s = %+v, want present and unavailable", agent, v)
		}
	}
}

func TestHandleUsageNilCache(t *testing.T) {
	a := &app{} // bare app, no usage cache
	rec := httptest.NewRecorder()
	a.handleUsage(rec, httptest.NewRequest("GET", "/api/usage", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d, want 200 for nil cache", rec.Code)
	}
}

// ---------- refresh policy ----------

func newTestUsageCache(now *time.Time, calls *int32) *usageCache {
	c := newUsageCache(func(ctx context.Context, agent string) (agentUsage, error) {
		atomic.AddInt32(calls, 1)
		u := 10.0
		rem := 90.0
		return agentUsage{Agent: agent, FiveHourUsed: &u, FiveHourRemaining: &rem, Source: "test"}, nil
	})
	c.now = func() time.Time { return *now }
	return c
}

func TestMaybeRefreshImmediateWhenStale(t *testing.T) {
	now := time.Now()
	var calls int32
	c := newTestUsageCache(&now, &calls)
	c.notePrompt(now)
	// No snapshot yet -> stale -> collect all budget agents.
	c.maybeRefresh(context.Background())
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("calls = %d, want 3 (codex+claude+grok) on stale refresh", got)
	}
	if c.snap.ObservedAt.IsZero() {
		t.Fatal("snapshot not stored")
	}
}

func TestMaybeRefreshOncePerInterval(t *testing.T) {
	now := time.Now()
	var calls int32
	c := newTestUsageCache(&now, &calls)
	c.notePrompt(now)
	c.maybeRefresh(context.Background()) // stale (empty) -> refresh
	first := atomic.LoadInt32(&calls)
	if first != 3 {
		t.Fatalf("first refresh calls = %d, want 3", first)
	}
	// Prompts keep coming, but within the 15-min interval: no new collection.
	now = now.Add(5 * time.Minute)
	c.notePrompt(now)
	c.maybeRefresh(context.Background())
	if got := atomic.LoadInt32(&calls); got != first {
		t.Fatalf("calls = %d, want no new refresh inside 15-min interval", got)
	}
	// Past the interval, still active: refresh again.
	now = now.Add(11 * time.Minute)
	c.notePrompt(now)
	c.maybeRefresh(context.Background())
	if got := atomic.LoadInt32(&calls); got != first+3 {
		t.Fatalf("calls = %d, want a second refresh after the interval", got)
	}
}

func TestMaybeRefreshStopsWhenIdle(t *testing.T) {
	now := time.Now()
	var calls int32
	c := newTestUsageCache(&now, &calls)
	c.notePrompt(now)
	// 16 minutes later with no new prompt: outside the active window.
	now = now.Add(16 * time.Minute)
	c.maybeRefresh(context.Background())
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("calls = %d, want 0 once idle past the active window", got)
	}
}

func TestMaybeRefreshNoPromptEverIsNoop(t *testing.T) {
	now := time.Now()
	var calls int32
	c := newTestUsageCache(&now, &calls)
	c.maybeRefresh(context.Background()) // never had a prompt
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("calls = %d, want 0 with no prompt ever", got)
	}
}

func TestMaybeRefreshConcurrentCollapse(t *testing.T) {
	now := time.Now()
	var calls int32
	release := make(chan struct{})
	var once sync.Once
	c := newUsageCache(func(ctx context.Context, agent string) (agentUsage, error) {
		atomic.AddInt32(&calls, 1)
		// Block the first collection so concurrent callers pile up behind the
		// in-flight flag rather than starting their own refresh.
		once.Do(func() { <-release })
		return agentUsage{Agent: agent, Source: "test"}, nil
	})
	c.now = func() time.Time { return now }
	c.notePrompt(now)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.maybeRefresh(context.Background()) }()
	}
	// Give the goroutines time to contend, then let the blocked one finish.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	// Exactly one refresh ran: 3 agent collections, not 15.
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("calls = %d, want 3 (one collapsed refresh of all agents)", got)
	}
}

func TestRefreshBacksOffOnAuthError(t *testing.T) {
	now := time.Now()
	var claudeCalls int32
	c := newUsageCache(func(ctx context.Context, agent string) (agentUsage, error) {
		if agent == "claude" {
			atomic.AddInt32(&claudeCalls, 1)
			return agentUsage{Agent: "claude", Source: "claude-oauth"}, errAuth()
		}
		return agentUsage{Agent: agent, Source: "codex-session-jsonl"}, nil
	})
	c.now = func() time.Time { return now }

	// First collection: claude auth-errors -> stored unavailable, 5-min backoff.
	c.refreshNow(context.Background())
	if v := c.snap.Agents["claude"]; v.Available {
		t.Fatalf("claude = %+v, want unavailable after auth error", v)
	}
	if atomic.LoadInt32(&claudeCalls) != 1 {
		t.Fatalf("claude calls = %d, want 1", claudeCalls)
	}
	// A second collection two minutes later must skip claude (still backed off)
	// so a broken auth state does not spam the endpoint.
	now = now.Add(2 * time.Minute)
	c.refreshNow(context.Background())
	if atomic.LoadInt32(&claudeCalls) != 1 {
		t.Fatalf("claude calls = %d, want still 1 within 5-min auth backoff", claudeCalls)
	}
	// Past the backoff, claude is retried.
	now = now.Add(4 * time.Minute)
	c.refreshNow(context.Background())
	if atomic.LoadInt32(&claudeCalls) != 2 {
		t.Fatalf("claude calls = %d, want 2 after backoff expiry", claudeCalls)
	}
}

// ---------- prompt hooks ----------

func TestNoteUsagePromptFilters(t *testing.T) {
	// nil cache: no panic, no-op.
	(&app{}).noteUsagePrompt("claude")

	now := time.Now()
	var calls int32
	c := newTestUsageCache(&now, &calls)
	a := &app{usage: c}
	for _, agent := range []string{"pi", "opencode", "bash", ""} {
		a.noteUsagePrompt(agent)
	}
	c.mu.Lock()
	got := c.lastPromptAt
	c.mu.Unlock()
	if !got.IsZero() {
		t.Fatalf("lastPromptAt = %v, want zero for unsupported agents", got)
	}
	a.noteUsagePrompt("claude")
	c.mu.Lock()
	got = c.lastPromptAt
	c.mu.Unlock()
	if got.IsZero() {
		t.Fatal("lastPromptAt not set for claude")
	}
	// Grok is a first-class budget agent (weekly credits via _x.ai/billing).
	a2 := &app{usage: newTestUsageCache(&now, &calls)}
	a2.noteUsagePrompt("grok")
	a2.usage.mu.Lock()
	got = a2.usage.lastPromptAt
	a2.usage.mu.Unlock()
	if got.IsZero() {
		t.Fatal("lastPromptAt not set for grok")
	}
}

func TestQueryGrokUsageMapsBilling(t *testing.T) {
	raw := json.RawMessage(`{"config":{"creditUsagePercent":25.5,"currentPeriod":{"end":"2026-08-01T12:00:00Z"}},"subscription_tier":"SuperGrok"}`)
	u, err := queryGrokUsage(context.Background(), acp.GrokBillingOptions{
		Query: func(ctx context.Context) (json.RawMessage, error) { return raw, nil },
	})
	if err != nil {
		t.Fatalf("queryGrokUsage: %v", err)
	}
	if u.Agent != "grok" || u.Plan != "SuperGrok" || u.Source != "grok-acp-billing" {
		t.Fatalf("identity = %+v", u)
	}
	if u.WeeklyUsed == nil || *u.WeeklyUsed != 25.5 {
		t.Fatalf("used = %v, want 25.5", u.WeeklyUsed)
	}
	if u.WeeklyRemaining == nil || *u.WeeklyRemaining != 74.5 {
		t.Fatalf("remaining = %v, want 74.5", u.WeeklyRemaining)
	}
	if u.FiveHourUsed != nil || u.FiveHourRemaining != nil {
		t.Fatalf("five-hour fields must be unset for Grok: %+v", u)
	}
	// Injected error stays generic-facing at the collector boundary.
	_, err = queryGrokUsage(context.Background(), acp.GrokBillingOptions{
		Query: func(ctx context.Context) (json.RawMessage, error) {
			return nil, errors.New("secret account detail")
		},
	})
	if err == nil || err.Error() != "usage unavailable" {
		t.Fatalf("error = %v, want generic usage unavailable", err)
	}
}

func TestHandleSendTmuxNotesUsagePrompt(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{}, capture: "idle", captureAfterEnter: "working…"}
	a := newTestApp(t, f)
	installTestUsage(a)
	if rec := newNode(a, `{"title":"T","prompt":"hi","agent":"claude","dir":"`+a.home+`"}`); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := a.nodes[0].ID
	// createNode carried the first prompt -> already noted.
	if promptNoted(a) {
		// good: reset so we isolate the send below
	}
	resetPromptNote(a)

	send(t, a, id, `{"text":"do the thing"}`, 200)
	if !promptNoted(a) {
		t.Fatal("successful tmux send did not note a usage prompt")
	}
}

func TestHandleSendClearDoesNotNoteUsage(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{}, capture: "idle", captureAfterEnter: "cleared"}
	a := newTestApp(t, f)
	installTestUsage(a)
	if rec := newNode(a, `{"title":"T","prompt":"hi","agent":"claude","dir":"`+a.home+`"}`); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := a.nodes[0].ID
	resetPromptNote(a)

	send(t, a, id, `{"text":"/clear"}`, 200)
	if promptNoted(a) {
		t.Fatal("/clear must not note a usage prompt")
	}
}

func TestHandleNewNodeNotesUsageForClaude(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{}, capture: "idle"}
	a := newTestApp(t, f)
	installTestUsage(a)
	if rec := newNode(a, `{"title":"T","prompt":"first prompt","agent":"claude","dir":"`+a.home+`"}`); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if !promptNoted(a) {
		t.Fatal("node creation with a first prompt did not note a usage prompt")
	}
}

// installTestUsage swaps in a cache whose collector never touches the network
// or filesystem, with a fixed clock, so hook tests stay deterministic.
func installTestUsage(a *app) {
	a.usage = newUsageCache(func(context.Context, string) (agentUsage, error) {
		return agentUsage{Source: "test"}, nil
	})
}

func promptNoted(a *app) bool {
	a.usage.mu.Lock()
	defer a.usage.mu.Unlock()
	return !a.usage.lastPromptAt.IsZero()
}

func resetPromptNote(a *app) {
	a.usage.mu.Lock()
	a.usage.lastPromptAt = time.Time{}
	a.usage.mu.Unlock()
}

func send(t *testing.T, a *app, id, body string, wantCode int) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/nodes/"+id+"/send", strings.NewReader(body))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	a.handleSend(w, req)
	if w.Code != wantCode {
		t.Fatalf("send %s: code = %d, want %d (%s)", body, w.Code, wantCode, w.Body.String())
	}
}

func errAuth() error { return &staticErr{"usage unavailable: auth status 401"} }

type staticErr struct{ s string }

func (e *staticErr) Error() string { return e.s }

// roundTripFunc, jsonResponse, writeJSONL, copyFixture, readFixture live here so
// the usage tests are self-contained even if the prototype's helpers are gone.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func writeJSONL(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyFixture(t *testing.T, src, dst string) {
	t.Helper()
	b := readFixture(t, src)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
