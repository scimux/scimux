package app

// Stage A — app-level LogCache wiring: one walk per unchanged poll cycle
// across /api/state and chat (sites that previously walked independently).

import (
	"net/http/httptest"
	"testing"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// A3. Zero walks on an unchanged file across a simulated poll cycle:
// two consecutive /api/state renders plus a chat render for an unchanged
// node parse the log at most once total.
func TestLogCache_UnchangedPollCycleOneWalk(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{
		ID: "walk-1", Title: "w", Agent: "claude", Model: "sonnet",
		Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z",
	}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.live[n.ID] = "quiet"

	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "claude", "sonnet", "", a.home),
		{T: "user", Time: "2026-07-15T09:00:00Z", Text: "hello"},
		{T: "assistant", Time: "2026-07-15T09:00:01Z", Text: "hi"},
		{T: "usage", Time: "2026-07-15T09:00:02Z", Usage: &sessionlog.UsageEvent{
			Used: 1000, Size: 200_000,
			InputTokens: 10, OutputTokens: 20, TurnID: "t1", CostAmount: 0.01,
		}},
		{T: "asset", Time: "2026-07-15T09:00:03Z", Asset: &sessionlog.AssetEvent{
			ID: "a1", Name: "x.png", Mime: "image/png", Size: 4,
			Storage: "inline", Bytes: "AAAA", SourceKind: "upload", SourcePath: "/tmp/x.png",
		}},
	})

	// Two state polls: segment + fare (+ rides) all share one walk.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
		if rec.Code != 200 {
			t.Fatalf("state poll %d: code = %d body %q", i, rec.Code, rec.Body.String())
		}
	}

	// Chat poll: segment + anchored assets + assets — must hit the same cache.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/"+n.ID+"/chat", nil)
	r.SetPathValue("id", n.ID)
	a.handleChat(rec, r)
	if rec.Code != 200 {
		t.Fatalf("chat: code = %d body %q", rec.Code, rec.Body.String())
	}

	walks := logCacheWalks(a, n.ID)
	if walks != 1 {
		t.Fatalf("poll cycle Walks() = %d, want 1 (two state + one chat on unchanged log)", walks)
	}
}
