package acp

import (
	"context"
	"errors"
	"math"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/scimux/scimux/internal/sessionlog"
)

// Synthetic cumulatives. Vibe's prompt usage and cost are session totals.
// The gauge is the occupancy update. None of these numbers come from a live run.

func TestVibeCumulativeUsageIsATurnDeltaAndClearResetsIt(t *testing.T) {
	var step int
	agent := &fakeAgent{
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			step++
			switch step {
			case 1:
				vibeOccupancy(a, ctx, p, 1000, 8000, 1.5)
				vibeOccupancy(a, ctx, p, 1000, 8000, 1.5)
				_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.UpdateAgentMessageText("one")})
				return vibePromptUsage(400, 100, 500), nil
			case 2:
				vibeOccupancy(a, ctx, p, 1500, 8000, 1.5)
				vibeOccupancy(a, ctx, p, 1500, 8000, 2)
				_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.UpdateAgentMessageText("two")})
				return vibePromptUsage(700, 200, 900), nil
			case 3:
				vibeOccupancy(a, ctx, p, 40, 8000, 0.25)
				_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.UpdateAgentMessageText("three")})
				return vibePromptUsage(30, 10, 40), nil
			case 4:
				vibeOccupancy(a, ctx, p, 50, 8000, 0.5)
				_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.UpdateAgentMessageText("four")})
				return vibePromptUsage(45, 15, 60), nil
			default:
				t.Fatalf("unexpected prompt %d", step)
				return sdk.PromptResponse{}, errors.New("unexpected prompt")
			}
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "vibe", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	sendQuiet := func(text string) {
		t.Helper()
		if err := m.Send("n1", text); err != nil {
			t.Fatal(err)
		}
		waitFor(t, text, func() bool { return m.Live("n1") == "quiet" })
	}
	sendQuiet("one")
	sendQuiet("two")
	used, size := m.Usage("n1")
	if used != 1500 || size != 8000 {
		t.Fatalf("gauge = %d/%d, want 1500/8000 from occupancy, not the cumulative total", used, size)
	}
	fare := sessionlog.ReadFare(m.logPath("n1"))
	if fare.Turns != 2 || fare.UnsplitIn != 700 || fare.FreshIn != 0 || fare.Out != 200 || fare.ReportedCostUSD != 2 {
		t.Fatalf("fare after two turns = %+v, want 2 turns, unsplit 700, out 200, cost 2", fare)
	}
	if err := m.Clear("n1"); err != nil {
		t.Fatal(err)
	}
	sendQuiet("three")
	sendQuiet("four")
	fare = sessionlog.ReadFare(m.logPath("n1"))
	if fare.Turns != 4 || fare.UnsplitIn != 745 || fare.FreshIn != 0 || fare.Out != 215 || fare.ReportedCostUSD != 2.5 {
		t.Fatalf("fare after clear = %+v, want 4 turns, unsplit 745, out 215, cost 2.5", fare)
	}
	used, size = m.Usage("n1")
	if used != 50 || size != 8000 {
		t.Fatalf("gauge after clear = %d/%d, want 50/8000", used, size)
	}
	for _, ev := range readEvents(m.logPath("n1")) {
		if ev.T != "usage" || ev.Usage == nil || ev.Usage.InputTokens != 0 || ev.Usage.OutputTokens != 0 || ev.Usage.TotalTokens != 0 {
			continue
		}
		if ev.Usage.CostAmount != 0 || ev.Usage.CostCurrency != "" {
			t.Fatalf("occupancy shell carried cost: %+v", ev.Usage)
		}
	}
}

func TestVibeUsageEdges(t *testing.T) {
	t.Run("no occupancy does not borrow total tokens", func(t *testing.T) {
		s := newVibeUsageSession(t)
		endVibeUsage(s, 10, 5, 5000, nil)
		used, size := latestUsage(s.logw.Path)
		if used != 0 || size != 0 {
			t.Fatalf("gauge = %d/%d, want 0/0 when Vibe sent no occupancy", used, size)
		}
		fare := sessionlog.ReadFare(s.logw.Path)
		if fare.UnsplitIn != 10 || fare.FreshIn != 0 || fare.Out != 5 || fare.Turns != 1 {
			t.Fatalf("fare = %+v", fare)
		}
	})

	t.Run("initial zero used keeps the window", func(t *testing.T) {
		s := newVibeUsageSession(t)
		noteVibeUsage(t, s, 0, 8000, nil)
		used, size := latestUsage(s.logw.Path)
		if used != 0 || size != 8000 {
			t.Fatalf("after used=0, gauge = %d/%d, want 0/8000", used, size)
		}
		noteVibeUsage(t, s, 100, 8000, usd(1))
		endVibeUsage(s, 4, 1, 5, nil)
		used, size = latestUsage(s.logw.Path)
		if used != 100 || size != 8000 {
			t.Fatalf("gauge = %d/%d, want 100/8000", used, size)
		}
	})

	t.Run("missing and decreasing counters keep their baselines", func(t *testing.T) {
		s := newVibeUsageSession(t)
		noteVibeUsage(t, s, 10, 80, usd(1))
		endVibeUsage(s, 100, 50, 150, nil)
		s.turnActive = true
		noteVibeUsage(t, s, 20, 80, nil)
		endVibeUsage(s, 0, 80, 40, nil)
		fare := sessionlog.ReadFare(s.logw.Path)
		if fare.Turns != 2 || fare.UnsplitIn != 100 || fare.FreshIn != 0 || fare.Out != 80 {
			t.Fatalf("partial delta fare = %+v, want unsplit 100 out 80", fare)
		}
		s.turnActive = true
		endVibeUsage(s, 40, 80, 150, nil)
		fare = sessionlog.ReadFare(s.logw.Path)
		if fare.Turns != 2 || fare.UnsplitIn != 100 || fare.FreshIn != 0 {
			t.Fatalf("decreased input was charged: %+v", fare)
		}
	})

	t.Run("repeated totals are not a second fare", func(t *testing.T) {
		s := newVibeUsageSession(t)
		noteVibeUsage(t, s, 10, 80, usd(1))
		endVibeUsage(s, 10, 5, 15, nil)
		s.turnActive = true
		noteVibeUsage(t, s, 12, 80, usd(1))
		endVibeUsage(s, 10, 5, 15, nil)
		fare := sessionlog.ReadFare(s.logw.Path)
		if fare.Turns != 1 || fare.ReportedCostUSD != 1 {
			t.Fatalf("repeated cumulative charged twice: %+v", fare)
		}
		used, _ := latestUsage(s.logw.Path)
		if used != 12 {
			t.Fatalf("gauge used = %d, want the later occupancy 12", used)
		}
	})

	t.Run("two model calls bill the latest cost once", func(t *testing.T) {
		s := newVibeUsageSession(t)
		noteVibeUsage(t, s, 10, 80, usd(1))
		noteVibeUsage(t, s, 11, 80, usd(1.5))
		endVibeUsage(s, 8, 2, 10, nil)
		fare := sessionlog.ReadFare(s.logw.Path)
		if fare.Turns != 1 || fare.ReportedCostUSD != 1.5 {
			t.Fatalf("two calls fare = %+v, want one turn cost 1.5", fare)
		}
	})

	t.Run("absent zero and malformed cost stay unknown", func(t *testing.T) {
		s := newVibeUsageSession(t)
		endVibeUsage(s, 3, 1, 4, nil)
		fare := sessionlog.ReadFare(s.logw.Path)
		if fare.ReportedCostUSD != 0 || fare.ReportedCostComplete {
			t.Fatalf("missing cost = %+v, want unknown", fare)
		}
		s.turnActive = true
		noteVibeUsage(t, s, 2, 80, &sdk.Cost{Amount: 0, Currency: "USD"})
		endVibeUsage(s, 5, 2, 7, nil)
		s.turnActive = true
		noteVibeUsage(t, s, 3, 80, &sdk.Cost{Amount: math.NaN(), Currency: "USD"})
		noteVibeUsage(t, s, 3, 80, &sdk.Cost{Amount: math.Inf(1), Currency: "USD"})
		noteVibeUsage(t, s, 3, 80, &sdk.Cost{Amount: -1, Currency: "USD"})
		endVibeUsage(s, 6, 3, 9, nil)
		s.turnActive = true
		noteVibeUsage(t, s, 4, 80, usd(2))
		endVibeUsage(s, 8, 4, 12, nil)
		fare = sessionlog.ReadFare(s.logw.Path)
		if fare.ReportedCostUSD != 2 {
			t.Fatalf("cost after ignored updates = %v, want 2 (baseline stayed 0)", fare.ReportedCostUSD)
		}
	})

	t.Run("unsupported currency is not billed onto a later usd update", func(t *testing.T) {
		s := newVibeUsageSession(t)
		noteVibeUsage(t, s, 1, 80, &sdk.Cost{Amount: 4, Currency: ""})
		noteVibeUsage(t, s, 1, 80, &sdk.Cost{Amount: 4, Currency: "EUR"})
		endVibeUsage(s, 10, 1, 11, nil)
		s.turnActive = true
		noteVibeUsage(t, s, 2, 80, usd(5))
		noteVibeUsage(t, s, 2, 80, usd(8))
		endVibeUsage(s, 12, 2, 14, nil)
		fare := sessionlog.ReadFare(s.logw.Path)
		if fare.Turns != 2 || fare.ReportedCostUSD != 3 {
			t.Fatalf("currency fare = %+v, want 2 turns and cost 3", fare)
		}
		if fare.ReportedCostComplete {
			t.Fatal("cost complete despite the unpriced first turn")
		}
	})

	t.Run("late cost is baseline and failed turn keeps its own fare", func(t *testing.T) {
		s := newVibeUsageSession(t)
		noteVibeUsage(t, s, 5, 80, usd(1))
		endVibeUsage(s, 4, 1, 5, nil)
		s.turnActive = false
		noteVibeUsage(t, s, 0, 0, usd(1.4))
		s.turnActive = true
		noteVibeUsage(t, s, 6, 80, usd(2))
		s.endTurn(sdk.PromptResponse{}, errors.New("prompt failed"))
		s.turnActive = true
		noteVibeUsage(t, s, 7, 80, usd(2.5))
		endVibeUsage(s, 7, 2, 9, nil)
		fare := sessionlog.ReadFare(s.logw.Path)
		if fare.Turns != 3 || fare.UnsplitIn != 4 || fare.Out != 1 || math.Abs(fare.ReportedCostUSD-2.1) > 1e-9 {
			t.Fatalf("late/failed cost fare = %+v, want three turns, known first tokens, cost 2.1", fare)
		}
		used, size := latestUsage(s.logw.Path)
		if used != 7 || size != 80 {
			t.Fatalf("gauge = %d/%d, want 7/80", used, size)
		}
	})

	t.Run("a nil usage update writes nothing", func(t *testing.T) {
		s := newVibeUsageSession(t)
		before := len(readEvents(s.logw.Path))
		s.mu.Lock()
		s.noteVibeUsageLocked(nil)
		s.mu.Unlock()
		if got := len(readEvents(s.logw.Path)); got != before {
			t.Fatalf("events = %d, want %d after a nil usage update", got, before)
		}
	})

	t.Run("an untrusted currency after a seen usd is not billed", func(t *testing.T) {
		s := newVibeUsageSession(t)
		noteVibeUsage(t, s, 1, 80, usd(2.5))
		noteVibeUsage(t, s, 1, 80, &sdk.Cost{Amount: 9, Currency: "EUR"})
		endVibeUsage(s, 4, 1, 5, nil)
		fare := sessionlog.ReadFare(s.logw.Path)
		if fare.Turns != 1 || fare.ReportedCostUSD != 0 || fare.ReportedCostComplete {
			t.Fatalf("mixed currency fare = %+v, want one turn and an unknown cost", fare)
		}
		s.turnActive = true
		noteVibeUsage(t, s, 2, 80, usd(4))
		endVibeUsage(s, 6, 2, 8, nil)
		fare = sessionlog.ReadFare(s.logw.Path)
		if fare.Turns != 2 || fare.ReportedCostUSD != 0 {
			t.Fatalf("following usd was billed: %+v", fare)
		}
		s.turnActive = true
		noteVibeUsage(t, s, 3, 80, usd(4.25))
		endVibeUsage(s, 7, 3, 10, nil)
		fare = sessionlog.ReadFare(s.logw.Path)
		if fare.Turns != 3 || fare.ReportedCostUSD != 0.25 {
			t.Fatalf("fare after the baseline advanced = %+v, want cost 0.25", fare)
		}
	})

	t.Run("a seen usd that is not higher than the baseline stays unbilled when the currency turns untrusted", func(t *testing.T) {
		s := newVibeUsageSession(t)
		noteVibeUsage(t, s, 1, 80, usd(3))
		endVibeUsage(s, 4, 1, 5, nil)
		s.turnActive = true
		noteVibeUsage(t, s, 2, 80, usd(3))
		noteVibeUsage(t, s, 2, 80, &sdk.Cost{Amount: 1, Currency: "EUR"})
		endVibeUsage(s, 8, 2, 10, nil)
		fare := sessionlog.ReadFare(s.logw.Path)
		if fare.Turns != 2 || fare.ReportedCostUSD != 3 {
			t.Fatalf("fare = %+v, want the first turn's 3 only", fare)
		}
		s.turnActive = true
		noteVibeUsage(t, s, 3, 80, usd(3.5))
		endVibeUsage(s, 9, 3, 12, nil)
		fare = sessionlog.ReadFare(s.logw.Path)
		if fare.Turns != 3 || fare.ReportedCostUSD != 3 {
			t.Fatalf("a later usd was billed across the untrusted turn: %+v", fare)
		}
	})

	t.Run("a second session does not inherit the baseline", func(t *testing.T) {
		s := newVibeUsageSession(t)
		noteVibeUsage(t, s, 9, 80, usd(3))
		endVibeUsage(s, 100, 20, 120, nil)
		other := newVibeUsageSession(t)
		noteVibeUsage(t, other, 1, 80, usd(0.25))
		endVibeUsage(other, 8, 2, 10, nil)
		fare := sessionlog.ReadFare(other.logw.Path)
		if fare.UnsplitIn != 8 || fare.FreshIn != 0 || fare.ReportedCostUSD != 0.25 {
			t.Fatalf("fresh session fare = %+v", fare)
		}
	})
}

func TestVibeTurnCarriesShellOccupancy(t *testing.T) {
	s := newVibeUsageSession(t)
	noteVibeUsage(t, s, 0, 8000, nil)
	noteVibeUsage(t, s, 42, 8000, usd(0.5))
	endVibeUsage(s, 6, 2, 8, nil)
	var fareUsed, fareSize int
	var shells int
	for _, ev := range readEvents(s.logw.Path) {
		if ev.T != "usage" || ev.Usage == nil {
			continue
		}
		u := ev.Usage
		if u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 {
			shells++
			if u.CostAmount != 0 {
				t.Fatalf("shell cost = %v", u.CostAmount)
			}
			continue
		}
		fareUsed, fareSize = u.Used, u.Size
		if u.CostAmount != 0.5 || u.CostCurrency != "USD" {
			t.Fatalf("turn cost = %v %q", u.CostAmount, u.CostCurrency)
		}
	}
	if shells != 2 {
		t.Fatalf("occupancy shells = %d, want 2", shells)
	}
	if fareUsed != 42 || fareSize != 8000 {
		t.Fatalf("turn occupancy = %d/%d, want the shell 42/8000", fareUsed, fareSize)
	}
}

func TestVibeCumulativeCountersRecoverAfterPlanReset(t *testing.T) {
	s := newVibeUsageSession(t)
	noteVibeUsage(t, s, 10, 80, usd(3))
	endVibeUsage(s, 100, 20, 120, nil)
	s.turnActive = true
	noteVibeUsage(t, s, 11, 80, usd(0.7))
	endVibeUsage(s, 20, 5, 25, nil)
	fare := sessionlog.ReadFare(s.logw.Path)
	if fare.UnsplitIn != 120 || fare.Out != 25 || math.Abs(fare.ReportedCostUSD-3.7) > 1e-9 || fare.Turns != 2 {
		t.Fatalf("plan-reset fare = %+v", fare)
	}
}

func TestVibeInterruptedTurnKeepsCostWithoutChargingItsUnknownTokensToNextTurn(t *testing.T) {
	s := newVibeUsageSession(t)
	noteVibeUsage(t, s, 10, 80, usd(0.4))
	s.endTurn(sdk.PromptResponse{}, errors.New("interrupted"))
	s.turnActive = true
	noteVibeUsage(t, s, 11, 80, usd(0.7))
	endVibeUsage(s, 20, 5, 25, nil)
	s.turnActive = true
	noteVibeUsage(t, s, 12, 80, usd(1))
	endVibeUsage(s, 25, 8, 33, nil)
	fare := sessionlog.ReadFare(s.logw.Path)
	if fare.UnsplitIn != 5 || fare.Out != 3 || math.Abs(fare.ReportedCostUSD-1) > 1e-9 || fare.Turns != 3 {
		t.Fatalf("interrupted-turn fare = %+v", fare)
	}
}

func TestVibeSuccessfulTurnWithoutUsageAlsoLeavesAnUnknownTokenGap(t *testing.T) {
	s := newVibeUsageSession(t)
	noteVibeUsage(t, s, 1, 80, usd(0.2))
	s.endTurn(sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil)
	s.turnActive = true
	noteVibeUsage(t, s, 2, 80, usd(0.5))
	endVibeUsage(s, 20, 5, 25, nil)
	fare := sessionlog.ReadFare(s.logw.Path)
	if fare.UnsplitIn != 0 || fare.Out != 0 || fare.Turns != 2 || math.Abs(fare.ReportedCostUSD-0.5) > 1e-9 {
		t.Fatalf("missing-breakdown fare = %+v", fare)
	}
}

func newVibeUsageSession(t *testing.T) *Session {
	t.Helper()
	dir := t.TempDir()
	s := &Session{
		nodeID:     "vibe-usage",
		agent:      "vibe",
		turnActive: true,
		done:       make(chan struct{}),
		logw:       &logWriter{Path: dir + "/n.jsonl"},
	}
	if err := s.logw.Append(sessionlog.NewMeta(s.nodeID, "vibe", "synthetic", "", dir)); err != nil {
		t.Fatal(err)
	}
	return s
}

func noteVibeUsage(t *testing.T, s *Session, used, size int, cost *sdk.Cost) {
	t.Helper()
	err := s.SessionUpdate(context.Background(), sdk.SessionNotification{Update: sdk.SessionUpdate{UsageUpdate: &sdk.SessionUsageUpdate{
		Used: used, Size: size, Cost: cost,
	}}})
	if err != nil {
		t.Fatal(err)
	}
}

func endVibeUsage(s *Session, in, out, total int, err error) {
	s.endTurn(sdk.PromptResponse{
		StopReason: sdk.StopReasonEndTurn,
		Usage:      &sdk.Usage{InputTokens: in, OutputTokens: out, TotalTokens: total},
	}, err)
}

func usd(amount float64) *sdk.Cost {
	return &sdk.Cost{Amount: amount, Currency: "USD"}
}

func vibeOccupancy(a *fakeAgent, ctx context.Context, p sdk.PromptRequest, used, size int, amount float64) {
	_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.SessionUpdate{UsageUpdate: &sdk.SessionUsageUpdate{
		Used: used, Size: size, Cost: usd(amount),
	}}})
}

func vibePromptUsage(in, out, total int) sdk.PromptResponse {
	return sdk.PromptResponse{
		StopReason: sdk.StopReasonEndTurn,
		Usage:      &sdk.Usage{InputTokens: in, OutputTokens: out, TotalTokens: total},
	}
}
