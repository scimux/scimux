package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/scimux/scimux/internal/acp"
)

// Subscription-usage observability. Best-effort only: missing fields, failed
// auth, network errors, and format drift produce "usage unavailable", never a
// fatal app error. This is a status gauge, not a critical path.
//
// Three provider surfaces:
//   - Codex mirrors budget percentages into its local session JSONL; we scan
//     the newest usable token_count record.
//   - Claude states budget percentages on the status line of a session; its
//     local JSONL carries token history, not quota percentages. Reading one
//     costs a throwaway probe session (claude_usage_probe.go).
//   - Grok exposes weekly credit usage via the ACP extension `_x.ai/billing`
//     on a short-lived `grok agent stdio` process (CLI must already be logged
//     in; scimux never authenticates). The wire/process lives in
//     internal/acp; this file only maps the narrow result into agentUsage.
//
// Collection is expensive (a recursive file walk, a probe session, a subprocess),
// so it is kept off the /api/state and /api/usage read paths entirely: /api/usage
// serves only a cached snapshot, and collection is driven by successful user
// prompts (noteUsagePrompt) — never by UI polling or a wall clock. This keeps
// browser traffic decoupled from provider I/O and avoids overnight polling.

// agentUsage is the normalized internal shape a provider adapter returns.
// Pointer numeric fields keep 0 distinct from missing/unavailable.
type agentUsage struct {
	Agent             string
	Plan              string
	FiveHourUsed      *float64
	FiveHourRemaining *float64
	FiveHourReset     *time.Time
	WeeklyUsed        *float64
	WeeklyRemaining   *float64
	WeeklyReset       *time.Time
	ObservedAt        time.Time
	Source            string
}

func remainingPercent(used float64) float64 {
	if used < 0 {
		return 100
	}
	if used > 100 {
		return 0
	}
	return 100 - used
}

// ---------- Codex adapter ----------

func queryCodexUsage(sessionsDir string) (agentUsage, error) {
	if sessionsDir == "" {
		sessionsDir = filepath.Join(os.Getenv("HOME"), ".codex", "sessions")
		if h := os.Getenv("CODEX_HOME"); h != "" {
			sessionsDir = filepath.Join(h, "sessions")
		}
	}
	var best agentUsage
	var bestTS time.Time
	err := filepath.WalkDir(sessionsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable dir/file is normal — degrade, don't fail
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			u, ts, ok := parseCodexLine(sc.Bytes())
			if !ok {
				continue
			}
			if ts.After(bestTS) {
				best, bestTS = u, ts
			}
		}
		return nil
	})
	if err != nil {
		return agentUsage{Agent: "codex", Source: "codex-session-jsonl"}, err
	}
	if bestTS.IsZero() {
		return agentUsage{Agent: "codex", Source: "codex-session-jsonl"}, errors.New("usage unavailable")
	}
	return best, nil
}

type codexRecord struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type       string `json:"type"`
		RateLimits struct {
			PlanType  string     `json:"plan_type"`
			Primary   codexLimit `json:"primary"`
			Secondary codexLimit `json:"secondary"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

type codexLimit struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowMinutes int      `json:"window_minutes"`
	// ResetsAt is a pointer so an absent field is distinguishable from a genuine
	// epoch 0: a token_count record can carry used_percent without resets_at, and
	// time.Unix(0,0) would serialize as 1970-01-01 — a reset time that already
	// passed 50+ years ago — rather than an omitted/unknown value.
	ResetsAt *int64 `json:"resets_at"`
}

// parseCodexLine decodes one JSONL line defensively. Unknown record types,
// malformed JSON, missing budget fields, and unexpected window sizes are all
// silently skipped (ok=false) — the CLI log format is an undocumented
// internal. Windows are matched by minutes, not position, so a future Codex
// reordering of primary/secondary cannot mislabel the budgets.
func parseCodexLine(line []byte) (agentUsage, time.Time, bool) {
	var rec codexRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return agentUsage{}, time.Time{}, false
	}
	if rec.Type != "event_msg" || rec.Payload.Type != "token_count" {
		return agentUsage{}, time.Time{}, false
	}
	p, s := rec.Payload.RateLimits.Primary, rec.Payload.RateLimits.Secondary
	if p.UsedPercent == nil || s.UsedPercent == nil {
		return agentUsage{}, time.Time{}, false
	}
	if p.WindowMinutes != 300 || s.WindowMinutes != 10080 {
		return agentUsage{}, time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return agentUsage{}, time.Time{}, false
	}
	// A missing resets_at leaves the reset unknown (nil), not epoch 0 / 1970.
	var pr, sr *time.Time
	if p.ResetsAt != nil {
		t := time.Unix(*p.ResetsAt, 0)
		pr = &t
	}
	if s.ResetsAt != nil {
		t := time.Unix(*s.ResetsAt, 0)
		sr = &t
	}
	prem, srem := remainingPercent(*p.UsedPercent), remainingPercent(*s.UsedPercent)
	return agentUsage{
		Agent:             "codex",
		Plan:              rec.Payload.RateLimits.PlanType,
		FiveHourUsed:      p.UsedPercent,
		FiveHourRemaining: &prem,
		FiveHourReset:     pr,
		WeeklyUsed:        s.UsedPercent,
		WeeklyRemaining:   &srem,
		WeeklyReset:       sr,
		ObservedAt:        ts,
		Source:            "codex-session-jsonl",
	}, ts, true
}

// ---------- Claude adapter ----------
//
// Claude's quota comes from the status line of a throwaway probe session; the
// transport is claude_statusline.go and the session is claude_usage_probe.go.
// Its predecessor read the Claude CLI's stored OAuth credentials file and
// called an undocumented usage endpoint with the user's own bearer token.
// That was the one place scimux touched an agent's credentials, and it is
// deliberately gone rather than kept as a fallback: a path that reads a token
// is a path that can be asked to read a token.

// ---------- Grok adapter (maps internal/acp billing → agentUsage) ----------

// queryGrokUsage asks the Grok CLI for subscription credit usage via the
// focused ACP billing seam. Weekly creditUsagePercent maps onto
// WeeklyUsed/WeeklyRemaining; there is no five-hour window on this surface.
func queryGrokUsage(ctx context.Context, opts acp.GrokBillingOptions) (agentUsage, error) {
	shell := agentUsage{Agent: "grok", Source: "grok-acp-billing"}
	b, err := acp.QueryGrokBilling(ctx, opts)
	if err != nil {
		return shell, err
	}
	used := b.CreditUsagePercent
	rem := remainingPercent(used)
	return agentUsage{
		Agent:           "grok",
		Plan:            b.Plan,
		WeeklyUsed:      &used,
		WeeklyRemaining: &rem,
		WeeklyReset:     b.WeeklyReset,
		ObservedAt:      time.Now().UTC(),
		Source:          b.Source,
	}, nil
}

// ---------- wire shape (/api/usage) ----------

type usageSnapshot struct {
	ObservedAt     time.Time                 `json:"observed_at"`
	NextCheckAfter time.Time                 `json:"next_check_after,omitempty"`
	Agents         map[string]agentUsageView `json:"agents"`
}

type agentUsageView struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	// Off says the gauge is dark because the user has not switched it on, not
	// because anything failed. The browser needs the two apart without reading
	// English: one state offers a switch, the other only explains itself.
	Off               bool       `json:"off,omitempty"`
	Plan              string     `json:"plan,omitempty"`
	FiveHourUsed      *float64   `json:"five_hour_used,omitempty"`
	FiveHourRemaining *float64   `json:"five_hour_remaining,omitempty"`
	FiveHourReset     *time.Time `json:"five_hour_reset,omitempty"`
	WeeklyUsed        *float64   `json:"weekly_used,omitempty"`
	WeeklyRemaining   *float64   `json:"weekly_remaining,omitempty"`
	WeeklyReset       *time.Time `json:"weekly_reset,omitempty"`
	Source            string     `json:"source,omitempty"`
}

func usageView(u agentUsage, err error) agentUsageView {
	if err != nil {
		reason := err.Error()
		if reason == "" {
			reason = "usage unavailable"
		}
		return agentUsageView{Available: false, Reason: reason, Off: errors.Is(err, errClaudeUsageOff), Source: u.Source}
	}
	return agentUsageView{
		Available:         true,
		Plan:              u.Plan,
		FiveHourUsed:      u.FiveHourUsed,
		FiveHourRemaining: u.FiveHourRemaining,
		FiveHourReset:     u.FiveHourReset,
		WeeklyUsed:        u.WeeklyUsed,
		WeeklyRemaining:   u.WeeklyRemaining,
		WeeklyReset:       u.WeeklyReset,
		Source:            u.Source,
	}
}

// ---------- cache + refresh policy ----------

const (
	usageStaleThreshold = 60 * time.Minute // refresh at once if the snapshot is older than this
	usageActiveWindow   = 15 * time.Minute // prompts within this window keep refreshes scheduled
	usageMinInterval    = 15 * time.Minute // at most one refresh per this while prompts continue
	usageBackoffAuth    = 5 * time.Minute  // missing creds / 401 / 403
	usageBackoffNet     = 60 * time.Second // transient network / parse errors
)

// usageCache holds the last computed snapshot and the refresh bookkeeping. Its
// own mutex is independent of app.mu: the collectors do file/HTTP I/O, and the
// cache must never be held (nor a.mu) across that I/O.
type usageCache struct {
	mu            sync.Mutex
	snap          usageSnapshot
	lastPromptAt  time.Time
	lastRefreshAt time.Time
	refreshing    bool
	backoff       map[string]time.Time // agent -> retry-after

	now     func() time.Time
	collect func(ctx context.Context, agent string) (agentUsage, error)
}

func newUsageCache(collect func(ctx context.Context, agent string) (agentUsage, error)) *usageCache {
	return &usageCache{
		backoff: map[string]time.Time{},
		now:     time.Now,
		collect: collect,
	}
}

// snapshot returns the cached view. An empty cache reports every budget agent as
// unavailable rather than an error, so the endpoint always answers 200.
func (c *usageCache) snapshot() usageSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap.Agents == nil {
		return usageSnapshot{
			Agents: map[string]agentUsageView{
				"codex":  {Available: false, Reason: "usage unavailable"},
				"claude": {Available: false, Reason: "usage unavailable"},
				"grok":   {Available: false, Reason: "usage unavailable"},
			},
		}
	}
	return c.snap
}

func (c *usageCache) notePrompt(t time.Time) {
	c.mu.Lock()
	c.lastPromptAt = t
	c.mu.Unlock()
}

// maybeRefresh decides whether to collect now and, if so, runs the collection
// synchronously (callers spawn it in a goroutine). It collapses concurrent
// callers to a single in-flight refresh and honours the active window, the
// minimum interval, and per-agent error backoff.
func (c *usageCache) maybeRefresh(ctx context.Context) {
	c.mu.Lock()
	now := c.now()
	// Outside the active window nothing is scheduled: no prompt in the last
	// usageActiveWindow means the user is idle — stop probing providers.
	if c.lastPromptAt.IsZero() || now.Sub(c.lastPromptAt) > usageActiveWindow {
		c.mu.Unlock()
		return
	}
	if c.refreshing {
		c.mu.Unlock()
		return
	}
	stale := c.snap.ObservedAt.IsZero() || now.Sub(c.snap.ObservedAt) > usageStaleThreshold
	recent := !c.lastRefreshAt.IsZero() && now.Sub(c.lastRefreshAt) < usageMinInterval
	// A stale-enough snapshot always refreshes; otherwise no more than one
	// refresh per usageMinInterval while prompts keep arriving.
	if !stale && recent {
		c.mu.Unlock()
		return
	}
	c.refreshing = true
	c.lastRefreshAt = now
	c.mu.Unlock()

	c.refreshNow(ctx)

	c.mu.Lock()
	c.refreshing = false
	c.mu.Unlock()
}

// refreshNow collects each known agent (respecting per-agent backoff, keeping
// the prior good value when an agent is backed off) and stores the new snapshot.
func (c *usageCache) refreshNow(ctx context.Context) {
	now := c.now()
	views := map[string]agentUsageView{}
	for _, agent := range []string{"codex", "claude", "grok"} {
		c.mu.Lock()
		until, backed := c.backoff[agent]
		prev, hadPrev := c.snap.Agents[agent]
		c.mu.Unlock()
		if backed && now.Before(until) {
			if hadPrev {
				views[agent] = prev // keep last known while backed off
			} else {
				views[agent] = agentUsageView{Available: false, Reason: "usage unavailable"}
			}
			continue
		}
		u, err := c.collect(ctx, agent)
		v := usageView(u, err)
		views[agent] = v
		c.mu.Lock()
		if err != nil {
			c.backoff[agent] = now.Add(usageBackoffFor(err))
		} else {
			delete(c.backoff, agent)
		}
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.snap = usageSnapshot{
		ObservedAt:     now,
		NextCheckAfter: now.Add(usageMinInterval),
		Agents:         views,
	}
	c.mu.Unlock()
}

func usageBackoffFor(err error) time.Duration {
	if err == nil {
		return 0
	}
	s := err.Error()
	if strings.Contains(s, "auth status") || strings.Contains(s, "credentials") || strings.Contains(s, "token missing") {
		return usageBackoffAuth
	}
	return usageBackoffNet
}

// ---------- app wiring ----------

// noteUsagePrompt records that a budget-consuming prompt was accepted for a
// Claude, Codex, or Grok node and kicks a background refresh if the policy
// allows. It is a no-op for any other agent (and for apps with no usage cache,
// e.g. bare test apps). /clear is excluded by callers, not here.
func (a *app) noteUsagePrompt(agent string) {
	if a.usage == nil {
		return
	}
	if agent != "claude" && agent != "codex" && agent != "grok" {
		return
	}
	a.usage.notePrompt(time.Now())
	a.maybeRefreshUsageAsync()
}

// maybeRefreshUsageAsync runs the refresh decision off the request/poll path.
// The cache collapses concurrent calls to one in-flight refresh, so calling it
// from both the prompt hooks and the poll loop is safe.
func (a *app) maybeRefreshUsageAsync() {
	if a.usage == nil {
		return
	}
	go func() {
		// Wide enough for a Claude probe session: starting a TUI and waiting
		// for its first API response is tens of seconds, where the file walk
		// and the grok subprocess are near-instant. The refresh runs off every
		// request path, so a long ceiling costs nothing but a goroutine.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		a.usage.maybeRefresh(ctx)
	}()
}

// collectUsage is the real provider adapter the cache calls. It never holds
// a.mu (it does file/HTTP I/O) and reads only from local paths configurable
// for tests.
func (a *app) collectUsage(ctx context.Context, agent string) (agentUsage, error) {
	switch agent {
	case "codex":
		return queryCodexUsage(a.codexSessionsDir)
	case "claude":
		return a.collectClaudeUsage(ctx)
	case "grok":
		return queryGrokUsage(ctx, a.grokUsageOpts)
	default:
		return agentUsage{Agent: agent}, errors.New("usage unavailable")
	}
}

func (a *app) handleUsage(w http.ResponseWriter, r *http.Request) {
	var snap usageSnapshot
	if a.usage != nil {
		snap = a.usage.snapshot()
	} else {
		snap = usageSnapshot{Agents: map[string]agentUsageView{
			"codex":  {Available: false, Reason: "usage unavailable"},
			"claude": {Available: false, Reason: "usage unavailable"},
			"grok":   {Available: false, Reason: "usage unavailable"},
		}}
	}
	writeJSON(w, snap)
}
