package app

// V2-P2: durable needs-input start→end edges from the existing mechanical
// attention signal only (no new regex, no new attention source).

import (
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
)

func TestPersistAttentionTransition_StartAndEnd(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{ID: "attn1", Agent: "claude", Title: "a", Dir: "/tmp",
		CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	// Seed a log so edges have a place to land.
	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "claude", "c", "", a.home),
	})

	a.persistAttentionTransition(n, "", "approval")
	a.persistAttentionTransition(n, "approval", "")

	evs := sessionlog.ReadEvents(a.sessionLogPath(n.ID))
	var edges []sessionlog.Event
	for _, ev := range evs {
		if ev.T == "attention" {
			edges = append(edges, ev)
		}
	}
	if len(edges) != 2 {
		t.Fatalf("attention edges = %d, want 2: %+v", len(edges), edges)
	}
	if edges[0].Attention == nil || edges[0].Attention.Status != "start" || edges[0].Attention.Kind != "approval" {
		t.Errorf("start = %+v", edges[0].Attention)
	}
	if edges[1].Attention == nil || edges[1].Attention.Status != "end" || edges[1].Attention.Kind != "approval" {
		t.Errorf("end = %+v", edges[1].Attention)
	}
	if edges[0].Time == "" || edges[1].Time == "" {
		t.Error("attention edges must carry timestamps")
	}
}

func TestPersistAttentionTransition_NoopWhenUnchanged(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{ID: "attn2", Agent: "claude", Title: "a", Dir: "/tmp",
		CreatedAt: "2026-07-14T00:00:00Z"}
	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "claude", "c", "", a.home),
	})
	a.persistAttentionTransition(n, "approval", "approval")
	a.persistAttentionTransition(n, "", "")
	for _, ev := range sessionlog.ReadEvents(a.sessionLogPath(n.ID)) {
		if ev.T == "attention" {
			t.Fatalf("unexpected attention edge on unchanged signal: %+v", ev)
		}
	}
}

func TestPersistAttentionTransition_KindChangeClosesAndOpens(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{ID: "attn3", Agent: "claude", Title: "a", Dir: "/tmp",
		CreatedAt: "2026-07-14T00:00:00Z"}
	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "claude", "c", "", a.home),
	})
	a.persistAttentionTransition(n, "inspect", "approval")
	var statuses []string
	for _, ev := range sessionlog.ReadEvents(a.sessionLogPath(n.ID)) {
		if ev.T == "attention" && ev.Attention != nil {
			statuses = append(statuses, ev.Attention.Kind+":"+ev.Attention.Status)
		}
	}
	want := []string{"inspect:end", "approval:start"}
	if len(statuses) != 2 || statuses[0] != want[0] || statuses[1] != want[1] {
		t.Errorf("kind-change edges = %v, want %v", statuses, want)
	}
}

func TestHandleKey_PersistsAttentionEnd(t *testing.T) {
	// Successful web key clears attn and appends an end edge.
	ft := &fakeTmux{
		alive:   map[string]bool{"k1": true},
		capture: "Allow tool? (y/n)",
	}
	a := newTestApp(t, ft)
	n := &Node{ID: "k1", Agent: "claude", Title: "k", Dir: "/tmp",
		CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.live[n.ID] = "quiet"
	a.attn[n.ID] = "approval"
	a.attnAt[n.ID] = time.Now()
	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "claude", "c", "", a.home),
		{T: "attention", Time: "2026-08-01T10:00:00Z", Attention: &sessionlog.AttentionEvent{Kind: "approval", Status: "start"}},
	})

	rec := keyReq(a, "k1", `{"key":"y"}`)
	if rec.Code != 200 {
		t.Fatalf("handleKey code = %d body %q", rec.Code, rec.Body.String())
	}
	if a.attn[n.ID] != "" {
		t.Error("attn not cleared after key")
	}
	var ends int
	for _, ev := range sessionlog.ReadEvents(a.sessionLogPath(n.ID)) {
		if ev.T == "attention" && ev.Attention != nil && ev.Attention.Status == "end" {
			ends++
		}
	}
	if ends != 1 {
		t.Errorf("attention end edges = %d, want 1", ends)
	}
}
