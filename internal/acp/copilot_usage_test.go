package acp

import (
	"context"
	"errors"
	"testing"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/scimux/scimux/internal/sessionlog"
)

// Copilot streams its occupancy as usage_update shells during a turn and
// reports the turn's token totals once, at turn end. The totals are spend
// across the turn's model calls, so they must never become the gauge. The
// numbers here are synthetic and chosen only to be distinct from each other.

// noteShell sends one occupancy notification, as the agent would mid-turn.
func noteShell(t *testing.T, s *Session, used, size int) {
	t.Helper()
	err := s.SessionUpdate(context.Background(), sdk.SessionNotification{Update: sdk.SessionUpdate{
		UsageUpdate: &sdk.SessionUsageUpdate{Used: used, Size: size},
	}})
	if err != nil {
		t.Fatal(err)
	}
}

// copilotTurnTotals is the turn-end PromptResponse usage: inputs, outputs, and
// the total across the turn's calls.
var copilotTurnTotals = &sdk.Usage{InputTokens: 41000, OutputTokens: 250, TotalTokens: 41250}

// usageRecords returns every usage record the session log holds, in order.
func usageRecords(t *testing.T, s *Session) []UsageEvent {
	t.Helper()
	var out []UsageEvent
	for _, ev := range readEvents(s.logw.Path) {
		if ev.T == "usage" && ev.Usage != nil {
			out = append(out, *ev.Usage)
		}
	}
	return out
}

func tokenRecords(recs []UsageEvent) []UsageEvent {
	var out []UsageEvent
	for _, r := range recs {
		if r.TotalTokens > 0 || r.InputTokens > 0 || r.OutputTokens > 0 {
			out = append(out, r)
		}
	}
	return out
}

func TestCopilotTurnEndKeepsOccupancyAndRecordsTotalsOnce(t *testing.T) {
	cases := []struct {
		name     string
		shell    *[2]int // occupancy shell sent mid-turn, if any
		ctxSize  int
		wantUsed int
		wantSize int
	}{
		{name: "shell occupancy wins over the turn total", shell: &[2]int{3100, 9000},
			wantUsed: 3100, wantSize: 9000},
		{name: "no shell leaves occupancy unknown, never the total", ctxSize: 0,
			wantUsed: 0, wantSize: 0},
		{name: "no shell takes the advertised window as size", ctxSize: 7000,
			wantUsed: 0, wantSize: 7000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := copilotSession(t)
			s.ctxSize = c.ctxSize
			if c.shell != nil {
				noteShell(t, s, c.shell[0], c.shell[1])
			}
			s.endTurn(sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn, Usage: copilotTurnTotals}, nil)

			tokens := tokenRecords(usageRecords(t, s))
			if len(tokens) != 1 {
				t.Fatalf("token records = %d, want the turn total exactly once: %+v", len(tokens), usageRecords(t, s))
			}
			got := tokens[0]
			if got.InputTokens != 41000 || got.OutputTokens != 250 || got.TotalTokens != 41250 {
				t.Fatalf("turn totals = %+v, want the reported 41000/250/41250", got)
			}
			if got.Used != c.wantUsed || got.Size != c.wantSize {
				t.Fatalf("turn record gauge = used %d size %d, want used %d size %d",
					got.Used, got.Size, c.wantUsed, c.wantSize)
			}
			if got.CostAmount != 0 || got.CostCurrency != "" || got.CacheCreationTokens != 0 {
				t.Fatalf("invented cost or cache fields: %+v", got)
			}
			// The gauge the chat reads is the session log's last nonzero occupancy.
			used, size := latestUsage(s.logw.Path)
			if used != int64(c.wantUsed) || size != int64(c.wantSize) {
				t.Fatalf("session-log gauge = used %d size %d, want used %d size %d", used, size, c.wantUsed, c.wantSize)
			}
			// The chat reads its context gauge from the segment projection, which
			// applies the same last-nonzero rule; check that read path too.
			seg := (&sessionlog.LogCache{}).Segment(s.logw.Path)
			if seg.Used != int64(c.wantUsed) || seg.Size != int64(c.wantSize) {
				t.Fatalf("chat segment gauge = used %d size %d, want used %d size %d", seg.Used, seg.Size, c.wantUsed, c.wantSize)
			}
		})
	}
}

// An error turn reports no totals: it keeps the shell's occupancy and writes
// no breakdown record.
func TestCopilotErrorTurnKeepsShellAndWritesNoTotals(t *testing.T) {
	s := copilotSession(t)
	noteShell(t, s, 3100, 9000)
	s.endTurn(sdk.PromptResponse{}, errors.New("agent failed mid-turn"))
	if n := len(tokenRecords(usageRecords(t, s))); n != 0 {
		t.Fatalf("error turn wrote %d token records", n)
	}
	used, size := latestUsage(s.logw.Path)
	if used != 3100 || size != 9000 {
		t.Fatalf("error turn gauge = used %d size %d, want the shell's 3100/9000", used, size)
	}
}

// The shell belongs to the turn that sent it. A later turn with no shell must
// not inherit the earlier occupancy into its own turn-end record.
func TestCopilotShellDoesNotCarryIntoNextTurn(t *testing.T) {
	s := copilotSession(t)
	noteShell(t, s, 3100, 9000)
	s.endTurn(sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn, Usage: copilotTurnTotals}, nil)
	s.mu.Lock()
	s.turnActive = true
	s.mu.Unlock()
	s.endTurn(sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn, Usage: &sdk.Usage{InputTokens: 900, OutputTokens: 20, TotalTokens: 920}}, nil)

	tokens := tokenRecords(usageRecords(t, s))
	if len(tokens) != 2 {
		t.Fatalf("token records = %d, want one per turn", len(tokens))
	}
	if tokens[1].Used != 0 {
		t.Fatalf("second turn recorded %d as its gauge; its own shell was absent, so it must be 0", tokens[1].Used)
	}
}
