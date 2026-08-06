package acp

// Phase 5 — opencode ACP capture hardening (fare-design.md §2.1, D5).
//
// O2 (verified 2026-08-06 from corpus through 2026-08-03 Test-OC session):
// opencode still emits two record shapes on the ACP path:
//   1. streamed occupancy shell via sessionUpdate usage_update:
//      {used, size, costCurrency}  (cost amount absent / 0; no breakdown)
//   2. turn-end PromptResponse.usage breakdown:
//      {inputTokens, outputTokens, cachedReadTokens?, totalTokens}
//      (Aug 2026 captures sometimes also echo used≈totalTokens on the
//      breakdown; field names unchanged from the Jul 15 sample).
// Empty {} usage records still occur and must be dropped. No cache-write and
// no cost amount on the ACP path — Phase 5b (SQLite) stays paused.

import (
	"context"
	"testing"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	sdk "github.com/coder/acp-go-sdk"
)

// TestOpencode_PairsOccupancyAndBreakdown: occupancy shell + breakdown for one
// turn → one fare turn; Used/Size from the shell (tank), canonical quantities
// from the breakdown (meter). Never double-count.
func TestOpencode_PairsOccupancyAndBreakdown(t *testing.T) {
	cache := 512
	agent := &fakeAgent{
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			// Shape 1 — streamed occupancy shell (usage_update).
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.SessionUpdate{UsageUpdate: &sdk.SessionUsageUpdate{
					Used: 16042, Size: 200000,
					Cost: &sdk.Cost{Amount: 0, Currency: "USD"},
				}}})
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.UpdateAgentMessageText("pong")})
			// Shape 2 — turn-end breakdown (PromptResponse.usage).
			return sdk.PromptResponse{
				StopReason: sdk.StopReasonEndTurn,
				Usage: &sdk.Usage{
					InputTokens:      15530,
					OutputTokens:     109,
					CachedReadTokens: &cache,
					TotalTokens:      16184,
				},
			}, nil
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn quiet", func() bool { return m.Live("n1") == "quiet" })

	// Gauge: Used/Size from the occupancy shell, not totalTokens (16184).
	used, size := m.Usage("n1")
	if used != 16042 || size != 200000 {
		t.Fatalf("occupancy = %d/%d, want 16042/200000 (shell, not totalTokens 16184)", used, size)
	}

	path := m.logPath("n1")
	f := sessionlog.ReadFare(path)
	if f.Turns != 1 {
		t.Fatalf("Turns = %d, want 1 (pair shell+breakdown; never double-count)", f.Turns)
	}
	// opencode input is fresh-only (direct): FreshIn = inputTokens.
	if f.FreshIn != 15530 {
		t.Errorf("FreshIn = %d, want 15530 (breakdown input, direct)", f.FreshIn)
	}
	if f.CacheRead != 512 {
		t.Errorf("CacheRead = %d, want 512", f.CacheRead)
	}
	if f.Out != 109 {
		t.Errorf("Out = %d, want 109", f.Out)
	}
}

// TestOpencode_TotalTokenFallback: a breakdown carrying only {totalTokens:N}
// is attributed via rawFromUsage → out, not dropped.
func TestOpencode_TotalTokenFallback(t *testing.T) {
	agent := &fakeAgent{
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.SessionUpdate{UsageUpdate: &sdk.SessionUsageUpdate{
					Used: 4000, Size: 200000,
				}}})
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.UpdateAgentMessageText("ok")})
			return sdk.PromptResponse{
				StopReason: sdk.StopReasonEndTurn,
				Usage:      &sdk.Usage{TotalTokens: 5000},
			}, nil
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn quiet", func() bool { return m.Live("n1") == "quiet" })

	f := sessionlog.ReadFare(m.logPath("n1"))
	if f.Turns != 1 {
		t.Fatalf("Turns = %d, want 1 (totalTokens-only must reach ReadFare)", f.Turns)
	}
	if f.Out != 5000 {
		t.Errorf("Out = %d, want 5000 (total-token fallback → out)", f.Out)
	}
	if f.FreshIn != 0 || f.CacheRead != 0 || f.CacheWrite != 0 {
		t.Errorf("non-out fields = %d/%d/%d, want 0/0/0", f.FreshIn, f.CacheRead, f.CacheWrite)
	}
}

// TestOpencode_DropsEmptyUsage: empty {} usage records contribute nothing —
// not written as fare hits, and not counted by ReadFare.
func TestOpencode_DropsEmptyUsage(t *testing.T) {
	agent := &fakeAgent{
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			// Empty occupancy shell.
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.SessionUpdate{UsageUpdate: &sdk.SessionUsageUpdate{}}})
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.UpdateAgentMessageText("hi")})
			// Empty breakdown.
			return sdk.PromptResponse{
				StopReason: sdk.StopReasonEndTurn,
				Usage:      &sdk.Usage{},
			}, nil
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn quiet", func() bool { return m.Live("n1") == "quiet" })

	path := m.logPath("n1")
	var usageN int
	for _, ev := range sessionlog.ReadEvents(path) {
		if ev.T == "usage" {
			usageN++
		}
	}
	if usageN != 0 {
		t.Fatalf("wrote %d usage record(s), want 0 (empty {} must be dropped)", usageN)
	}
	f := sessionlog.ReadFare(path)
	if f.Turns != 0 {
		t.Fatalf("Turns = %d, want 0", f.Turns)
	}
}

// TestOpencode_FreshInputDirect: the negative-fresh trap end-to-end through
// ReadFare. Synthetic counters have input(30) < cacheRead(80); must yield
// FreshIn:30, never −50 or 0 (opencode input is fresh-only / direct).
func TestOpencode_FreshInputDirect(t *testing.T) {
	cache := 80
	agent := &fakeAgent{
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.SessionUpdate{UsageUpdate: &sdk.SessionUsageUpdate{
					Used: 110, Size: 1000,
				}}})
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.UpdateAgentMessageText("ok")})
			return sdk.PromptResponse{
				StopReason: sdk.StopReasonEndTurn,
				Usage: &sdk.Usage{
					InputTokens:      30,
					OutputTokens:     10,
					CachedReadTokens: &cache,
					TotalTokens:      120,
				},
			}, nil
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn quiet", func() bool { return m.Live("n1") == "quiet" })

	f := sessionlog.ReadFare(m.logPath("n1"))
	if f.FreshIn != 30 {
		t.Errorf("FreshIn = %d, want 30 (direct; subtract would be −50 → 0)", f.FreshIn)
	}
	if f.CacheRead != 80 {
		t.Errorf("CacheRead = %d, want 80", f.CacheRead)
	}
	if f.Out != 10 {
		t.Errorf("Out = %d, want 10", f.Out)
	}
}

// TestOpencode_NoCacheWriteNoCostSilent: opencode ACP never emits cache-write
// or cost amount; ReadFare must show CacheWrite:0 and incomplete/absent cost
// — never a fabricated value.
func TestOpencode_NoCacheWriteNoCostSilent(t *testing.T) {
	cache := 100
	agent := &fakeAgent{
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.SessionUpdate{UsageUpdate: &sdk.SessionUsageUpdate{
					Used: 1000, Size: 8000,
					Cost: &sdk.Cost{Amount: 0, Currency: "USD"}, // currency only; no amount
				}}})
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.UpdateAgentMessageText("ok")})
			return sdk.PromptResponse{
				StopReason: sdk.StopReasonEndTurn,
				Usage: &sdk.Usage{
					InputTokens:      200,
					OutputTokens:     30,
					CachedReadTokens: &cache,
					TotalTokens:      330,
				},
			}, nil
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn quiet", func() bool { return m.Live("n1") == "quiet" })

	f := sessionlog.ReadFare(m.logPath("n1"))
	if f.CacheWrite != 0 {
		t.Errorf("CacheWrite = %d, want 0 (no cache-write on opencode ACP)", f.CacheWrite)
	}
	if f.ReportedCostUSD != 0 {
		t.Errorf("ReportedCostUSD = %v, want 0 (never fabricate cost)", f.ReportedCostUSD)
	}
	if f.ReportedCostComplete {
		t.Error("ReportedCostComplete = true, want false (cost absent, not zero-as-real)")
	}
	// Sanity: the three-quantity fare is still present.
	if f.FreshIn != 200 || f.CacheRead != 100 || f.Out != 30 {
		t.Errorf("fare = fresh=%d cache=%d out=%d, want 200/100/30", f.FreshIn, f.CacheRead, f.Out)
	}
}

// TestSyntheticOpencodeReplay drives the committed two-shape fixture through
// the fixture process (same harness as synthetic-grok-turn).
func TestSyntheticOpencodeReplay(t *testing.T) {
	frames := loadACPFixture(t, "synthetic-opencode-turn.ndjson")
	assertFixturePrivacy(t, frames)

	m := NewManagerWithRunner(t.TempDir(), fixtureRunner(t, frames))
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := m.Send("n1", "Reply with exactly: pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, "turn quiet", func() bool { return m.Live("n1") == "quiet" })

	used, window := m.Usage("n1")
	if used != 16042 || window != 200000 {
		t.Fatalf("usage = %d/%d, want 16042/200000 (occupancy shell)", used, window)
	}
	f := sessionlog.ReadFare(m.logPath("n1"))
	if f.Turns != 1 {
		t.Fatalf("Turns = %d, want 1", f.Turns)
	}
	if f.FreshIn != 15530 || f.CacheRead != 512 || f.Out != 109 {
		t.Errorf("fare = fresh=%d cache=%d out=%d, want 15530/512/109",
			f.FreshIn, f.CacheRead, f.Out)
	}
}
