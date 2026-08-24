package acp

// pi's startup banner (Defect 1): pi-acp advertises it in the new-session
// _meta, namespaced under "piAcp" (probed against pi-acp 0.0.27):
//
//	"_meta": {"piAcp": {"startupInfo": "pi v0.84.2\n---\n..."}}
//
// captureBanner must read that namespaced shape so appendAssistantLocked can
// strip the banner instead of storing it as the session's first assistant
// message. Best-effort and pi-specific: junk shapes never panic, never error.

import (
	"context"
	"testing"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	sdk "github.com/coder/acp-go-sdk"
)

func TestCaptureBannerNamespacedPiMeta(t *testing.T) {
	const banner = "pi v0.84.2\n---\n"
	s := &Session{agent: "pi"}
	s.captureBanner(sdk.NewSessionResponse{
		SessionId: sdk.SessionId("sess_pi"),
		Meta: map[string]any{
			"piAcp": map[string]any{"startupInfo": banner},
		},
	})
	if s.banner != banner {
		t.Fatalf("banner = %q, want the namespaced piAcp.startupInfo %q", s.banner, banner)
	}
}

func TestCaptureBannerTopLevelFallbackStillWorks(t *testing.T) {
	const banner = "pi v0.84.2\n---\n"
	s := &Session{agent: "pi"}
	s.captureBanner(sdk.NewSessionResponse{
		SessionId: sdk.SessionId("sess_pi"),
		Meta:      map[string]any{"startupInfo": banner},
	})
	if s.banner != banner {
		t.Fatalf("banner = %q, want the top-level startupInfo %q", s.banner, banner)
	}
}

func TestCaptureBannerIgnoresNonPiAgent(t *testing.T) {
	meta := func() map[string]any {
		return map[string]any{
			"piAcp": map[string]any{"startupInfo": "pi v0.84.2\n---\n"},
		}
	}
	for _, agent := range []string{"opencode", "grok"} {
		s := &Session{agent: agent}
		s.captureBanner(sdk.NewSessionResponse{SessionId: sdk.SessionId("s"), Meta: meta()})
		if s.banner != "" {
			t.Errorf("%s: banner = %q, want \"\" (pi-specific capture)", agent, s.banner)
		}
	}
}

func TestCaptureBannerMalformedShapesAreInert(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]any
	}{
		{"nil meta", nil},
		{"piAcp a string", map[string]any{"piAcp": "pi v0.84.2\n---\n"}},
		{"piAcp map, startupInfo a number",
			map[string]any{"piAcp": map[string]any{"startupInfo": float64(1)}}},
		{"piAcp map without startupInfo", map[string]any{"piAcp": map[string]any{}}},
		{"empty startupInfo",
			map[string]any{"piAcp": map[string]any{"startupInfo": ""}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Session{agent: "pi"}
			s.captureBanner(sdk.NewSessionResponse{SessionId: sdk.SessionId("s"), Meta: c.meta})
			if s.banner != "" {
				t.Fatalf("banner = %q, want \"\" for %s", s.banner, c.name)
			}
		})
	}
}

// TestPiBannerChunkIsStrippedFromLog drives a real session the way
// TestBasicTurn does: the fake agent advertises the namespaced banner on
// session/new and streams a byte-identical banner as its first assistant
// chunk, then real text. The log must hold the real text and not the banner.
func TestPiBannerChunkIsStrippedFromLog(t *testing.T) {
	const banner = "pi v0.84.2\n---\n"
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			return sdk.NewSessionResponse{
				SessionId: sdk.SessionId("sess_pi"),
				Meta:      map[string]any{"piAcp": map[string]any{"startupInfo": banner}},
			}
		},
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.UpdateAgentMessageText(banner)})
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.UpdateAgentMessageText("the real answer")})
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "pi", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn to finish", func() bool { return m.Live("n1") == "quiet" })

	foundReal := false
	for _, ev := range sessionlog.ReadEvents(m.logPath("n1")) {
		if ev.T != "assistant" {
			continue
		}
		if ev.Text == banner {
			t.Fatalf("banner was stored as an assistant record: %q", ev.Text)
		}
		if ev.Text == "the real answer" {
			foundReal = true
		}
	}
	if !foundReal {
		t.Fatal("real text missing from the session log")
	}
}

// TestPiBannerStrippedAgainAfterClear: /clear builds a fresh Session (so
// bannerDone resets and captureBanner runs again on the replacement
// session/new), so the post-clear banner chunk must be stripped too.
func TestPiBannerStrippedAgainAfterClear(t *testing.T) {
	const banner = "pi v0.84.2\n---\n"
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			return sdk.NewSessionResponse{
				SessionId: sdk.SessionId("sess_pi"),
				Meta:      map[string]any{"piAcp": map[string]any{"startupInfo": banner}},
			}
		},
		prompt: func(a *fakeAgent, ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			// pi emits the banner only at session startup; one prompt per
			// session here, so a banner first chunk is faithful per session.
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.UpdateAgentMessageText(banner)})
			_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId,
				Update: sdk.UpdateAgentMessageText("answer")})
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		},
	}
	m := newManager(t, agent)
	if _, err := m.Launch("n1", "pi", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "first"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first turn to finish", func() bool { return m.Live("n1") == "quiet" })
	if err := m.Clear("n1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "second"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second turn to finish", func() bool { return m.Live("n1") == "quiet" })

	var evs []sessionlog.Event
	sawClearSeam := false
	for _, ev := range sessionlog.ReadEvents(m.logPath("n1")) {
		evs = append(evs, ev)
		if ev.T == "source" && ev.Source != nil && ev.Source.Reason == "clear" {
			sawClearSeam = true
		}
		if ev.T == "assistant" && ev.Text == banner {
			t.Fatalf("post-clear banner was stored as an assistant record: %q", ev.Text)
		}
	}
	if !sawClearSeam {
		t.Fatal("clear seam missing — test is not exercising the post-clear path")
	}
	// The live segment is the post-clear chat: it holds the second answer.
	seg := sessionlog.ReadSegment(m.logPath("n1"))
	if len(seg.Turns) != 2 || seg.Turns[1].Role != "assistant" || seg.Turns[1].Text != "answer" {
		t.Fatalf("post-clear segment = %+v, want user 'second' + assistant 'answer'", seg.Turns)
	}
}
