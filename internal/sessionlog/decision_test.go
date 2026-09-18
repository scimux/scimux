package sessionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/scimux/scimux/internal/fare"
)

func sampleDecision() DecisionEvent {
	return DecisionEvent{
		Source:    "auto",
		LeaseID:   "lease-abc",
		RequestID: "req-42",
		Agent:     "grok",
		ToolKind:  "execute",
		Title:     "go test ./...",
		Reason:    "Run the test suite",
		Options: []DecOption{
			{Key: "1", Name: "Allow once", Kind: "allow"},
			{Key: "2", Name: "Reject", Kind: "reject"},
		},
		Selected: DecOption{Key: "1", Name: "Allow once", Kind: "allow"},
	}
}

// TestDecisionEventRoundTrip: every decision field survives append/read.
func TestDecisionEventRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	dec := sampleDecision()
	if err := w.Append(NewDecision(dec)); err != nil {
		t.Fatal(err)
	}
	evs := ReadEvents(path)
	if len(evs) != 1 || evs[0].T != "decision" || evs[0].Decision == nil {
		t.Fatalf("want one decision event, got %+v", evs)
	}
	got := *evs[0].Decision
	if !reflect.DeepEqual(got, dec) {
		t.Fatalf("decision round-trip mismatch:\n got %+v\nwant %+v", got, dec)
	}
	if evs[0].Time == "" {
		t.Fatal("decision event must carry a timestamp")
	}
}

// TestDecisionIgnoredAmongOldEvents: mixed into a legacy log shape, unknown
// readers still see turns/seams/usage correctly and decision is parseable.
func TestDecisionIgnoredAmongOldEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	for _, ev := range []Event{
		NewMeta("n", "codex", "m", "", "/tmp"),
		{T: "user", Text: "q1", Time: "2026-08-01T10:00:00Z"},
		{T: "assistant", Text: "a1", Time: "2026-08-01T10:00:01Z"},
		{T: "usage", Time: "2026-08-01T10:00:02Z", Usage: &UsageEvent{Used: 50, Size: 1000, InputTokens: 10, OutputTokens: 5, TurnID: "t1"}},
		NewDecision(sampleDecision()),
		{T: "tool", Time: "2026-08-01T10:00:03Z", Tool: &ToolEvent{ID: "tool1", Title: "ls", Status: "completed"}},
		{T: "user", Text: "q2", Time: "2026-08-01T10:00:04Z"},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	// Not a user/assistant turn.
	turns := ReadTurns(path)
	if len(turns) != 3 {
		t.Fatalf("turns = %d, want 3 (decision must not become a turn)", len(turns))
	}
	if turns[0].Role != "user" || turns[1].Role != "assistant" || turns[2].Role != "user" {
		t.Fatalf("turn roles = %+v", turns)
	}

	// Not a source seam: still one segment, no prior turns.
	seg := ReadSegment(path)
	if seg.PriorTurns != 0 {
		t.Fatalf("PriorTurns = %d, want 0 (decision is not a seam)", seg.PriorTurns)
	}
	if len(seg.Turns) != 3 {
		t.Fatalf("segment turns = %d, want 3", len(seg.Turns))
	}

	// Fare/usage unchanged by the decision record.
	if used, size := LatestUsage(path); used != 50 || size != 1000 {
		t.Fatalf("usage = %d/%d, want 50/1000", used, size)
	}
	ft := ReadFare(path)
	if ft.FreshIn != 10 || ft.Out != 5 {
		t.Fatalf("fare tokens = %+v, want FreshIn=10 Out=5", ft)
	}

	// Decision is still readable.
	evs := ReadEvents(path)
	var found bool
	for _, ev := range evs {
		if ev.T == "decision" && ev.Decision != nil && ev.Decision.LeaseID == "lease-abc" {
			found = true
		}
	}
	if !found {
		t.Fatal("decision event not found among mixed log")
	}
}

// TestDecisionDoesNotAffectRidesOrAssets: decision is inert for ride timing,
// attention wait, tool duration, and asset anchoring.
func TestDecisionDoesNotAffectRidesOrAssets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	base := []Event{
		NewMeta("n", "grok", "m", "", "/tmp"),
		{T: "user", Text: "q", Time: "2026-08-01T10:00:00Z"},
		{T: "tool", Time: "2026-08-01T10:00:01Z", Tool: &ToolEvent{ID: "t1", Title: "Read", Status: "pending"}},
		{T: "attention", Time: "2026-08-01T10:00:02Z", Attention: &AttentionEvent{Kind: "approval", Status: "start"}},
		{T: "attention", Time: "2026-08-01T10:00:05Z", Attention: &AttentionEvent{Kind: "approval", Status: "end"}},
		{T: "tool", Time: "2026-08-01T10:00:06Z", Tool: &ToolEvent{ID: "t1", Title: "Read", Status: "completed"}},
		{T: "assistant", Text: "done", Time: "2026-08-01T10:00:07Z"},
		{T: "asset", Time: "2026-08-01T10:00:08Z", Asset: &AssetEvent{
			ID: "a1", Name: "f.png", Mime: "image/png", Size: 1, Storage: "inline",
			Bytes: "QQ==", SourcePath: "/tmp/f.png",
		}},
	}
	// Baseline without decision.
	for _, ev := range base {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	rideBase := ReadRide(path)
	anchBase := ReadAnchoredAssets(path)

	// Append a decision mid-stream into a fresh copy with decision injected.
	path2 := filepath.Join(t.TempDir(), "n2.jsonl")
	w2 := &Writer{Path: path2}
	// Insert decision after attention start — must not change wait/tool/asset
	// duration; absolute record indices shift (durable positions) but the asset
	// still anchors to the same nearest preceding turn (assistant "done").
	mixed := []Event{
		base[0], base[1], base[2], base[3],
		NewDecision(sampleDecision()),
		base[4], base[5], base[6], base[7],
	}
	for _, ev := range mixed {
		if err := w2.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	ride2 := ReadRide(path2)
	if rideBase.Real != ride2.Real || rideBase.Tools != ride2.Tools || rideBase.Wait != ride2.Wait {
		t.Fatalf("ride changed with decision: base real/tools/wait=%v/%v/%v got %v/%v/%v",
			rideBase.Real, rideBase.Tools, rideBase.Wait, ride2.Real, ride2.Tools, ride2.Wait)
	}
	anch2 := ReadAnchoredAssets(path2)
	if len(anchBase) != len(anch2) || len(anch2) != 1 {
		t.Fatalf("anchored assets count base=%d mixed=%d", len(anchBase), len(anch2))
	}
	// Resolve anchor to role: decision must not re-home the asset onto itself.
	evs2 := ReadEvents(path2)
	if anch2[0].Anchor < 0 || anch2[0].Anchor >= len(evs2) {
		t.Fatalf("bad anchor %d", anch2[0].Anchor)
	}
	if evs2[anch2[0].Anchor].T != "assistant" || evs2[anch2[0].Anchor].Text != "done" {
		t.Fatalf("asset should still anchor to assistant turn, got record %d type=%s text=%q",
			anch2[0].Anchor, evs2[anch2[0].Anchor].T, evs2[anch2[0].Anchor].Text)
	}
	// Decision itself is never an anchor target for path assets.
	for i, ev := range evs2 {
		if ev.T == "decision" && anch2[0].Anchor == i {
			t.Fatal("asset must not anchor on a decision record")
		}
	}
}

// TestDecisionProjectionOrder: current-segment and history surfaces retain
// durable record order; decisions are not turns and do not shift turn indices.
func TestDecisionProjectionOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	d1 := sampleDecision()
	d1.RequestID = "r1"
	d2 := sampleDecision()
	d2.RequestID = "r2"
	d2.Title = "second"
	for _, ev := range []Event{
		NewMeta("n", "pi", "m", "", "/tmp"),
		{T: "user", Text: "old", Time: "2026-08-01T09:00:00Z"},
		NewDecision(d1), // record 2
		{T: "assistant", Text: "old a", Time: "2026-08-01T09:00:01Z"},
		NewClearSource("s2"),
		{T: "user", Text: "new", Time: "2026-08-01T10:00:00Z"},
		NewDecision(d2), // later record
		{T: "assistant", Text: "new a", Time: "2026-08-01T10:00:01Z"},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	seg := ReadSegment(path)
	if len(seg.Turns) != 2 {
		t.Fatalf("current segment turns = %d, want 2", len(seg.Turns))
	}
	// Turn record indices must still point at user/assistant events only.
	if seg.Turns[0].Role != "user" || seg.Turns[0].Text != "new" {
		t.Fatalf("first live turn = %+v", seg.Turns[0])
	}
	// Decision surfaces for current segment: only d2.
	if len(seg.Decisions) != 1 || seg.Decisions[0].Decision.RequestID != "r2" {
		t.Fatalf("segment decisions = %+v, want only r2", seg.Decisions)
	}
	// Record index is durable event index (not turn index).
	if seg.Decisions[0].Record <= seg.Turns[0].Record {
		t.Fatalf("decision record %d should be after user turn record %d",
			seg.Decisions[0].Record, seg.Turns[0].Record)
	}

	hist := ReadHistory(path)
	if len(hist) != 2 {
		t.Fatalf("history segments = %d, want 2", len(hist))
	}
	// Older surface has d1; newer has d2. Turn counts unchanged by decisions.
	if len(hist[0].Turns) != 2 || len(hist[0].Decisions) != 1 || hist[0].Decisions[0].Decision.RequestID != "r1" {
		t.Fatalf("history[0] turns=%d decisions=%+v", len(hist[0].Turns), hist[0].Decisions)
	}
	if len(hist[1].Turns) != 2 || len(hist[1].Decisions) != 1 || hist[1].Decisions[0].Decision.RequestID != "r2" {
		t.Fatalf("history[1] turns=%d decisions=%+v", len(hist[1].Turns), hist[1].Decisions)
	}

	// LogCache must agree with standalone readers.
	c := &LogCache{}
	cseg := c.Segment(path)
	if !reflect.DeepEqual(cseg.Decisions, seg.Decisions) {
		t.Fatalf("cache decisions = %+v, want %+v", cseg.Decisions, seg.Decisions)
	}
}

// TestDecisionJSONShape matches the audit schema field names.
func TestDecisionJSONShape(t *testing.T) {
	ev := NewDecision(sampleDecision())
	ev.Time = "2026-08-11T14:20:31Z"
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["t"] != "decision" {
		t.Fatalf("t = %v", raw["t"])
	}
	d, ok := raw["decision"].(map[string]any)
	if !ok {
		t.Fatalf("decision payload missing: %s", b)
	}
	for _, k := range []string{"source", "lease_id", "request_id", "agent", "tool_kind", "title", "reason", "options", "selected"} {
		if _, ok := d[k]; !ok {
			t.Errorf("missing decision field %q in %s", k, b)
		}
	}
}

// TestDecisionDoesNotCountAsFareHit is a sanity check that a decision-only
// log contributes zero fare.
func TestDecisionDoesNotCountAsFareHit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	_ = w.Append(NewMeta("n", "codex", "", "", ""))
	_ = w.Append(NewDecision(sampleDecision()))
	ft := ReadFare(path)
	if ft.FreshIn != 0 || ft.Out != 0 || ft.Total() != 0 {
		t.Fatalf("fare with only decision = %+v, want zero tokens", ft)
	}
	_ = fare.FareTotals{} // package import kept for future assertions
}

// Ensure missing files still return empty decisions (defensive).
func TestDecisionReadersDefensive(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone.jsonl")
	seg := ReadSegment(missing)
	if len(seg.Decisions) != 0 {
		t.Fatalf("missing log decisions = %+v", seg.Decisions)
	}
	// Unparseable line with a valid decision after must still work.
	path := filepath.Join(t.TempDir(), "torn.jsonl")
	content := "not-json\n"
	b, _ := json.Marshal(NewDecision(sampleDecision()))
	content += string(b) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	evs := ReadEvents(path)
	if len(evs) != 1 || evs[0].T != "decision" {
		t.Fatalf("defensive parse = %+v", evs)
	}
}
