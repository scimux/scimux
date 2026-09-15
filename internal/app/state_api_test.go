package app

// Packet 3C characterization: pure state-projection helpers and /api/state
// snapshot contracts that existing tests leave as gaps. Complements — does not
// replace — TestHandleStateETag, TestHandleStateOmitsExternalSessionDiscovery,
// TestHandleStateReportsLastInteractionFromCurrentSegment, TestHandleStateEmitsStationLabels,
// TestSysloadOnLinux, router GET /api/state characterization, or chat/context
// coverage that already exercises ctxWindowFor/ctxPctOf through handleChat.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// ---------- 1. Pure timestamp / interaction helpers ----------

func TestUnixMSStamp(t *testing.T) {
	rfc3339 := "2026-07-15T09:05:00Z"
	rfc3339Nano := "2026-07-15T09:05:00.123456789Z"
	wantRFC := time.Date(2026, 7, 15, 9, 5, 0, 0, time.UTC).UnixMilli()
	wantNano := time.Date(2026, 7, 15, 9, 5, 0, 123456789, time.UTC).UnixMilli()

	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{rfc3339, wantRFC},
		{rfc3339Nano, wantNano},
		{"not-a-timestamp", 0},
		{"2026-07-15 09:05:00", 0}, // layout not accepted
	}
	for _, tc := range cases {
		if got := unixMSStamp(tc.in); got != tc.want {
			t.Errorf("unixMSStamp(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestLastInteractionMS(t *testing.T) {
	created := "2026-07-14T00:00:00Z"
	createdMS := unixMSStamp(created)
	segStart := "2026-07-15T09:00:00Z"
	segStartMS := unixMSStamp(segStart)
	userOld := "2026-07-15T09:02:00Z"
	userNew := "2026-07-15T09:05:00Z"
	userNewMS := unixMSStamp(userNew)
	asst := "2026-07-15T09:06:00Z"
	tool := "2026-07-15T09:07:00Z"

	n := &Node{ID: "n1", CreatedAt: created}

	t.Run("latest_valid_user_in_segment", func(t *testing.T) {
		seg := sessionlog.Segment{
			StartTime: segStart,
			Turns: []transcript.Turn{
				{Role: "user", Text: "old", Time: userOld},
				{Role: "assistant", Text: "a", Time: asst},
				{Role: "user", Text: "new", Time: userNew},
			},
		}
		if got := lastInteractionMS(n, seg); got != userNewMS {
			t.Fatalf("got %d, want latest user %d", got, userNewMS)
		}
	})

	t.Run("assistant_and_tool_do_not_replace_user", func(t *testing.T) {
		seg := sessionlog.Segment{
			StartTime: segStart,
			Turns: []transcript.Turn{
				{Role: "user", Text: "q", Time: userNew},
				{Role: "assistant", Text: "a", Time: asst},
				{Role: "tool", Text: "t", Time: tool},
			},
		}
		if got := lastInteractionMS(n, seg); got != userNewMS {
			t.Fatalf("got %d, want user turn %d (assistant/tool ignored)", got, userNewMS)
		}
	})

	t.Run("falls_back_to_segment_start", func(t *testing.T) {
		seg := sessionlog.Segment{
			StartTime: segStart,
			Turns: []transcript.Turn{
				{Role: "assistant", Text: "a", Time: asst},
			},
		}
		if got := lastInteractionMS(n, seg); got != segStartMS {
			t.Fatalf("got %d, want segment start %d", got, segStartMS)
		}
	})

	t.Run("falls_back_to_node_created", func(t *testing.T) {
		seg := sessionlog.Segment{Turns: nil}
		if got := lastInteractionMS(n, seg); got != createdMS {
			t.Fatalf("got %d, want created_at %d", got, createdMS)
		}
	})

	t.Run("invalid_user_timestamp_falls_through", func(t *testing.T) {
		// Latest user has garbage time → skip; earlier valid user wins.
		seg := sessionlog.Segment{
			StartTime: segStart,
			Turns: []transcript.Turn{
				{Role: "user", Text: "earlier", Time: userOld},
				{Role: "user", Text: "broken", Time: "not-a-time"},
			},
		}
		if got := lastInteractionMS(n, seg); got != unixMSStamp(userOld) {
			t.Fatalf("got %d, want earlier valid user %d", got, unixMSStamp(userOld))
		}
	})

	t.Run("all_invalid_user_falls_to_segment_start", func(t *testing.T) {
		seg := sessionlog.Segment{
			StartTime: segStart,
			Turns: []transcript.Turn{
				{Role: "user", Text: "broken", Time: "bogus"},
			},
		}
		if got := lastInteractionMS(n, seg); got != segStartMS {
			t.Fatalf("got %d, want segment start %d", got, segStartMS)
		}
	})
}

// ---------- 2. Context projection helpers ----------

func TestCtxWindowFor(t *testing.T) {
	cases := []struct {
		name   string
		used   int64
		window int64
		model  string
		want   int64
	}{
		{"explicit_window", 50_000, 128_000, "claude-sonnet", 128_000},
		{"explicit_zero_no_usage", 0, 0, "claude-sonnet", 0},
		{"unknown_window_with_usage_default", 10_000, 0, "claude-sonnet", 200_000},
		{"unknown_window_1m_model", 10_000, 0, "claude-opus[1m]", 1_000_000},
		{"1m_marker_anywhere", 1, 0, "foo [1m] bar", 1_000_000},
		{"explicit_overrides_1m_inference", 10_000, 500_000, "claude[1m]", 500_000},
		{"zero_usage_known_window", 0, 200_000, "x", 200_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ctxWindowFor(tc.used, tc.window, tc.model); got != tc.want {
				t.Fatalf("ctxWindowFor(%d,%d,%q) = %d, want %d",
					tc.used, tc.window, tc.model, got, tc.want)
			}
		})
	}
}

func TestCtxPctOf(t *testing.T) {
	cases := []struct {
		name   string
		used   int64
		window int64
		want   int
	}{
		{"unknown_window", 50_000, 0, 0},
		{"negative_window", 50_000, -1, 0},
		{"zero_usage", 0, 200_000, 0},
		{"ordinary", 50_000, 200_000, 25},
		{"full", 200_000, 200_000, 100},
		{"over_100_clamped", 250_000, 200_000, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ctxPctOf(tc.used, tc.window); got != tc.want {
				t.Fatalf("ctxPctOf(%d,%d) = %d, want %d", tc.used, tc.window, got, tc.want)
			}
		})
	}
}

// writeSessionLog appends events to sessions/<id>.jsonl under the test app.
func writeSessionLog(t *testing.T, a *app, id string, evs []sessionlog.Event) {
	t.Helper()
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, id+".jsonl")}
	for _, ev := range evs {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
}

func decodeStateNodes(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatal(err)
	}
	raw, ok := top["nodes"].([]any)
	if !ok {
		t.Fatalf("nodes type %T", top["nodes"])
	}
	out := make([]map[string]any, len(raw))
	for i, v := range raw {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("node[%d] type %T", i, v)
		}
		out[i] = m
	}
	return out
}

func TestHandleStateContextGaugeProjection(t *testing.T) {
	// Live node + known window + zero usage → explicit 0% (not omitted).
	// Live node + no usage/window → gauge omitted.
	// Exited / unavailable → gauge omitted even with usage.
	f := &fakeTmux{}
	a := newTestApp(t, f)

	liveZero := &Node{
		ID: "live-zero", Title: "lz", Agent: "claude", Model: "claude-sonnet",
		Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z",
	}
	liveNone := &Node{
		ID: "live-none", Title: "ln", Agent: "claude", Model: "claude-sonnet",
		Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z",
	}
	exited := &Node{
		ID: "exited", Title: "ex", Agent: "claude", Model: "claude-sonnet",
		Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z",
	}
	unavail := &Node{
		ID: "unavail", Title: "ua", Agent: "claude", Model: "claude-sonnet",
		Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z",
	}
	a.nodes = []*Node{liveZero, liveNone, exited, unavail}
	for _, n := range a.nodes {
		a.byID[n.ID] = n
	}
	a.live["live-zero"] = "quiet"
	a.live["live-none"] = "active"
	a.live["exited"] = "exited"
	a.live["unavail"] = "unavailable"

	// Known window, zero used → win>0 so ctx_pct pointer is set to 0.
	writeSessionLog(t, a, "live-zero", []sessionlog.Event{
		sessionlog.NewMeta("live-zero", "claude", "claude-sonnet", "", a.home),
		{T: "usage", Time: "2026-07-15T09:00:00Z", Usage: &sessionlog.UsageEvent{Used: 0, Size: 200_000}},
	})
	// No usage events → win<=0 → no gauge.
	writeSessionLog(t, a, "live-none", []sessionlog.Event{
		sessionlog.NewMeta("live-none", "claude", "claude-sonnet", "", a.home),
		{T: "user", Text: "hi", Time: "2026-07-15T09:00:00Z"},
	})
	// Usage present but node exited/unavailable → gauge suppressed.
	for _, id := range []string{"exited", "unavail"} {
		writeSessionLog(t, a, id, []sessionlog.Event{
			sessionlog.NewMeta(id, "claude", "claude-sonnet", "", a.home),
			{T: "usage", Time: "2026-07-15T09:00:00Z", Usage: &sessionlog.UsageEvent{Used: 50_000, Size: 200_000}},
		})
	}

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d body %q", rec.Code, rec.Body.String())
	}
	nodes := decodeStateNodes(t, rec.Body.Bytes())
	byID := map[string]map[string]any{}
	for _, n := range nodes {
		byID[n["id"].(string)] = n
	}

	// live-zero: ctx_pct must be present and exactly 0 (JSON number).
	if _, ok := byID["live-zero"]["ctx_pct"]; !ok {
		t.Fatalf("live-zero missing ctx_pct; want explicit 0%%, got keys %v", keysOf(byID["live-zero"]))
	}
	if got := int(byID["live-zero"]["ctx_pct"].(float64)); got != 0 {
		t.Errorf("live-zero ctx_pct = %v, want 0", byID["live-zero"]["ctx_pct"])
	}

	// live-none: omit gauge.
	if _, ok := byID["live-none"]["ctx_pct"]; ok {
		t.Errorf("live-none ctx_pct = %v, want omitted", byID["live-none"]["ctx_pct"])
	}

	// exited / unavailable: omit gauge.
	for _, id := range []string{"exited", "unavail"} {
		if _, ok := byID[id]["ctx_pct"]; ok {
			t.Errorf("%s ctx_pct = %v, want omitted for dead live state", id, byID[id]["ctx_pct"])
		}
	}
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// ---------- 3. Node snapshot fields + non-mutation ----------

func TestHandleStateProjectsNodeScalarsAndLiveness(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	// Every scalar Node field must round-trip through the state projection copy.
	n := &Node{
		ID:          "full",
		Parent:      "root",
		Title:       "Title",
		Prompt:      "the prompt",
		Description: "desc",
		Rationale:   "why",
		LaneID:      "lane-1",
		ForkKind:    "y-new",
		EndedAt:     "2026-07-20T12:00:00Z",
		Agent:       "pi",
		Transport:   "tmux",
		Model:       "claude-sonnet",
		Effort:      "high",
		Dir:         "/work",
		SessionID:   "sess-1",
		Transcript:  "/tmp/t.jsonl",
		Adopted:     true,
		CreatedAt:   "2026-07-14T00:00:00Z",
	}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.live[n.ID] = "active"
	a.attn[n.ID] = "approval"
	attn := time.Date(2026, 7, 15, 9, 59, 0, 0, time.UTC)
	a.attnAt[n.ID] = attn
	chg := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	a.lastChg[n.ID] = chg

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	nodes := decodeStateNodes(t, rec.Body.Bytes())
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d", len(nodes))
	}
	got := nodes[0]

	// Scalar projections.
	wantStr := map[string]string{
		"id": "full", "parent": "root", "title": "Title", "prompt": "the prompt",
		"description": "desc", "rationale": "why", "lane_id": "lane-1",
		"fork_kind": "y-new", "ended_at": "2026-07-20T12:00:00Z",
		"agent": "pi", "model": "claude-sonnet", "effort": "high",
		"dir": "/work", "session_id": "sess-1", "transcript": "/tmp/t.jsonl",
		"created_at": "2026-07-14T00:00:00Z",
	}
	for k, want := range wantStr {
		if got[k] != want {
			t.Errorf("%s = %v, want %q", k, got[k], want)
		}
	}
	if got["adopted"] != true {
		t.Errorf("adopted = %v, want true", got["adopted"])
	}
	if got["live"] != "unavailable" || got["attention"] != nil {
		t.Errorf("retired external mechanics = live:%v attention:%v", got["live"], got["attention"])
	}
	if !strings.Contains(got["launch_error"].(string), "retired") {
		t.Errorf("retirement explanation = %v", got["launch_error"])
	}
	// tmux HasTranscript follows the transcript link.
	if got["has_transcript"] != true {
		t.Errorf("has_transcript = %v, want true (transcript set)", got["has_transcript"])
	}
	if int64(got["last_activity"].(float64)) != chg.UnixMilli() {
		t.Errorf("last_activity = %v, want %d", got["last_activity"], chg.UnixMilli())
	}
	if int64(got["attention_at"].(float64)) != attn.UnixMilli() {
		t.Errorf("attention_at = %v, want %d", got["attention_at"], attn.UnixMilli())
	}
	// No session log → last_interaction falls to created_at.
	if int64(got["last_interaction"].(float64)) != unixMSStamp(n.CreatedAt) {
		t.Errorf("last_interaction = %v, want created_at ms", got["last_interaction"])
	}
}

// P5 item 10 / P2: turn_done serializes when true and is omitted when
// unknown (false / absent). A structured node that has latched Ready
// serializes the same as Claude; a never-ran quiet node still omits.
func TestHandleStateTurnDoneField(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	done := &Node{ID: "done", Title: "D", Agent: "claude", Transcript: "/t.jsonl", CreatedAt: "2026-07-14T00:00:00Z"}
	quiet := &Node{ID: "quiet", Title: "Q", Agent: "claude", Transcript: "/t2.jsonl", CreatedAt: "2026-07-14T00:00:00Z"}
	acp := &Node{ID: "acp1", Title: "A", Agent: "pi", Transport: "acp", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes = []*Node{done, quiet, acp}
	for _, n := range a.nodes {
		a.byID[n.ID] = n
		a.live[n.ID] = "quiet"
	}
	if a.turnDone == nil {
		a.turnDone = map[string]bool{}
	}
	a.turnDone[done.ID] = true
	a.turnDone[acp.ID] = true
	// quiet intentionally leaves turnDone unset / false (UNKNOWN)

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	// Raw JSON: omitempty must drop the key when false.
	raw := rec.Body.String()
	if !strings.Contains(raw, `"turn_done":true`) {
		t.Errorf("finished node must serialize turn_done:true; body=%s", raw)
	}
	nodes := decodeStateNodes(t, rec.Body.Bytes())
	byID := map[string]map[string]any{}
	for _, n := range nodes {
		byID[n["id"].(string)] = n
	}
	if v, ok := byID["done"]["turn_done"]; !ok || v != true {
		t.Errorf("done.turn_done = %v,%v, want true", v, ok)
	}
	if _, ok := byID["quiet"]["turn_done"]; ok {
		t.Errorf("quiet.turn_done present = %v, want omitted", byID["quiet"]["turn_done"])
	}
	if v, ok := byID["acp1"]["turn_done"]; !ok || v != true {
		t.Errorf("acp.turn_done = %v,%v, want true when the structured latch is set", v, ok)
	}
}

func TestHandleStateHasTranscriptTmuxVsStructured(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	tmuxNo := &Node{ID: "tmux-no", Title: "t0", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	tmuxYes := &Node{ID: "tmux-yes", Title: "t1", Agent: "claude", Transcript: "/x.jsonl", CreatedAt: "2026-07-14T00:00:00Z"}
	// Structured nodes report chat availability without a tmux transcript path:
	// a.proc(n) is non-nil for acp/codex transports.
	acpN := &Node{ID: "acp1", Title: "a1", Agent: "pi", Transport: "acp", CreatedAt: "2026-07-14T00:00:00Z"}
	codexN := &Node{ID: "codex1", Title: "c1", Agent: "codex", Transport: "codex", CreatedAt: "2026-07-14T00:00:00Z"}
	museN := &Node{ID: "muse1", Title: "m1", Agent: "muse", Transport: "muse", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes = []*Node{tmuxNo, tmuxYes, acpN, codexN, museN}
	for _, n := range a.nodes {
		a.byID[n.ID] = n
	}

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	nodes := decodeStateNodes(t, rec.Body.Bytes())
	want := map[string]bool{
		"tmux-no": false, "tmux-yes": true, "acp1": true, "codex1": true, "muse1": true,
	}
	for _, n := range nodes {
		id := n["id"].(string)
		got, ok := n["has_transcript"].(bool)
		if !ok {
			t.Fatalf("%s missing has_transcript", id)
		}
		if got != want[id] {
			t.Errorf("%s has_transcript = %v, want %v", id, got, want[id])
		}
	}
}

func TestHandleStateDoesNotMutateNodeOrPollState(t *testing.T) {
	f := &fakeTmux{list: []string{"ghost"}}
	a := newTestApp(t, f)
	n := &Node{
		ID: "m1", Title: "M", Agent: "claude", Model: "claude-sonnet",
		Dir: "/tmp", Transcript: "/t.jsonl", CreatedAt: "2026-07-14T00:00:00Z",
	}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.live[n.ID] = "quiet"
	a.attn[n.ID] = "inspect"
	a.lastChg[n.ID] = time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	a.staleChat[n.ID] = true
	a.prevCap[n.ID] = "pane"
	a.sendState[n.ID] = "ok"
	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "claude", "claude-sonnet", "", a.home),
		{T: "usage", Time: "2026-07-15T09:00:00Z", Usage: &sessionlog.UsageEvent{Used: 10_000, Size: 200_000}},
	})

	// Snapshot everything projection is allowed to read but must not rewrite.
	nodeBefore := *n
	liveBefore := map[string]string{}
	attnBefore := map[string]string{}
	for k, v := range a.live {
		liveBefore[k] = v
	}
	for k, v := range a.attn {
		attnBefore[k] = v
	}
	staleBefore := a.staleChat[n.ID]
	prevBefore := a.prevCap[n.ID]
	sendBefore := a.sendState[n.ID]
	lastChgBefore := a.lastChg[n.ID]

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}

	if !reflect.DeepEqual(*n, nodeBefore) {
		t.Errorf("Node mutated: before=%+v after=%+v", nodeBefore, *n)
	}
	if !reflect.DeepEqual(a.live, liveBefore) {
		t.Errorf("live map mutated: before=%v after=%v", liveBefore, a.live)
	}
	if !reflect.DeepEqual(a.attn, attnBefore) {
		t.Errorf("attn map mutated: before=%v after=%v", attnBefore, a.attn)
	}
	if a.staleChat[n.ID] != staleBefore {
		t.Errorf("staleChat mutated")
	}
	if a.prevCap[n.ID] != prevBefore {
		t.Errorf("prevCap mutated")
	}
	if a.sendState[n.ID] != sendBefore {
		t.Errorf("sendState mutated")
	}
	if !a.lastChg[n.ID].Equal(lastChgBefore) {
		t.Errorf("lastChg mutated")
	}
}

// ---------- 4. Response shape, stops, ETag/content-type ----------

func TestHandleStateStopsSegmentDerived(t *testing.T) {
	// Stops are clear-tagged seams only (segment.ClearTimes), oldest first.
	// Complements TestHandleStateEmitsStationLabels (labels) without duplicating it.
	f := &fakeTmux{}
	a := newTestApp(t, f)
	n := &Node{ID: "s1", Title: "S", Agent: "claude", CreatedAt: "2026-07-01T09:00:00Z"}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	// Mechanical NewSource must not become a stop; only clear-tagged seams do.
	// Stamp clear seam times explicitly (NewClearSource leaves Time empty).
	clear1 := sessionlog.NewClearSource("sid2")
	clear1.Time = "2026-07-02T12:00:00Z"
	clear2 := sessionlog.NewClearSource("sid3")
	clear2.Time = "2026-07-03T12:00:00Z"
	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "claude", "", "", a.home),
		{T: "user", Text: "before", Time: "2026-07-01T09:30:00Z"},
		sessionlog.NewSource("/tmp/x.jsonl", "sid1"),
		{T: "user", Text: "mid", Time: "2026-07-02T10:00:00Z"},
		clear1,
		{T: "user", Text: "after", Time: "2026-07-03T11:00:00Z"},
		clear2,
	})

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	nodes := decodeStateNodes(t, rec.Body.Bytes())
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d", len(nodes))
	}
	rawStops, ok := nodes[0]["stops"].([]any)
	if !ok {
		t.Fatalf("stops missing or wrong type: %T %v", nodes[0]["stops"], nodes[0]["stops"])
	}
	var stops []string
	for _, s := range rawStops {
		stops = append(stops, s.(string))
	}
	want := []string{"2026-07-02T12:00:00Z", "2026-07-03T12:00:00Z"}
	if !reflect.DeepEqual(stops, want) {
		t.Errorf("stops = %v, want %v (clear seams only)", stops, want)
	}
}

func TestHandleStateResponseShapeContentTypeAndStableETag(t *testing.T) {
	// Complements TestHandleStateETag (304 path) with top-level
	// shape, content type, and etag stability across identical snapshots.
	f := &fakeTmux{list: []string{"node-a", "ghost"}}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "node-a", Title: "A", Agent: "claude", Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z"}}
	a.byID["node-a"] = a.nodes[0]
	a.reserved["pending"] = true

	req1 := httptest.NewRequest("GET", "/api/state", nil)
	rec1 := httptest.NewRecorder()
	a.handleState(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("code = %d", rec1.Code)
	}
	ct := rec1.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	etag1 := rec1.Header().Get("ETag")
	if etag1 == "" {
		t.Fatal("missing ETag")
	}

	var top map[string]any
	if err := json.Unmarshal(rec1.Body.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"nodes", "sys", "socket", "hostname", "version"} {
		if _, ok := top[key]; !ok {
			t.Errorf("top-level missing %q", key)
		}
	}
	if top["socket"] != a.server.Socket {
		t.Errorf("socket = %v, want %q", top["socket"], a.server.Socket)
	}
	if top["hostname"] != hostname {
		t.Errorf("hostname = %v, want %q", top["hostname"], hostname)
	}
	if top["version"] != version {
		t.Errorf("version = %v, want %q", top["version"], version)
	}
	if _, ok := top["unadopted"]; ok {
		t.Errorf("removed unadopted field present: %v", top["unadopted"])
	}
	sys, ok := top["sys"].(map[string]any)
	if !ok {
		t.Fatalf("sys type %T", top["sys"])
	}
	for _, k := range []string{"load1", "ncpu", "mem_pct", "mem_total_gb", "swap_pct"} {
		if _, ok := sys[k]; !ok {
			t.Errorf("sys missing %q", k)
		}
	}

	// Stable snapshot → stable ETag; matching If-None-Match → 304 empty body.
	rec2 := httptest.NewRecorder()
	a.handleState(rec2, httptest.NewRequest("GET", "/api/state", nil))
	if rec2.Header().Get("ETag") != etag1 {
		t.Errorf("ETag drifted: %q → %q", etag1, rec2.Header().Get("ETag"))
	}
	req3 := httptest.NewRequest("GET", "/api/state", nil)
	req3.Header.Set("If-None-Match", etag1)
	rec3 := httptest.NewRecorder()
	a.handleState(rec3, req3)
	if rec3.Code != http.StatusNotModified {
		t.Errorf("If-None-Match code = %d, want 304", rec3.Code)
	}
	if rec3.Body.Len() != 0 {
		t.Errorf("304 body = %q, want empty", rec3.Body.String())
	}
}

func TestSysloadOnLinux(t *testing.T) {
	s := sysload()
	if s.NCPU <= 0 {
		t.Errorf("ncpu = %d, want > 0", s.NCPU)
	}
	if s.MemTotalGB <= 0 {
		t.Errorf("mem_total_gb = %f, want > 0", s.MemTotalGB)
	}
	if s.MemPct <= 0 || s.MemPct > 100 {
		t.Errorf("mem_pct = %f, want (0,100]", s.MemPct)
	}
	if s.SwapPct < 0 || s.SwapPct > 100 {
		t.Errorf("swap_pct = %f, want [0,100]", s.SwapPct)
	}
	// cached: a second call within the 30 s window returns the same sample
	if s2 := sysload(); s2 != s {
		t.Error("sysload not cached within 30s window")
	}
}

// --- /api/state ---

func TestHandleStateETag(t *testing.T) {
	f := &fakeTmux{list: []string{"node-a", "ghost"}}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "node-a", Title: "A", Agent: "claude", Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z"}}
	a.byID["node-a"] = a.nodes[0]

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("state code = %d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag header")
	}
	var body struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Nodes) != 1 || body.Nodes[0].ID != "node-a" {
		t.Errorf("nodes = %+v", body.Nodes)
	}

	// A matching If-None-Match short-circuits to 304 with no body.
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	a.handleState(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: code = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 must have empty body, got %q", rec2.Body.String())
	}
}

func TestHandleStateOmitsExternalSessionDiscovery(t *testing.T) {
	a := newTestApp(t, &fakeTmux{list: []string{"foreign-pane"}})
	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("state = %d %q", rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, exists := body["unadopted"]; exists {
		t.Fatalf("state still publishes external-session discovery: %s", rec.Body.String())
	}
}

func TestHandleStateDoesNotDiscoverReservedOrForeignSessions(t *testing.T) {
	f := &fakeTmux{list: []string{"ghost", "pending"}}
	a := newTestApp(t, f)
	a.reserved = map[string]bool{"pending": true}
	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["unadopted"]; ok {
		t.Errorf("external discovery returned: %s", rec.Body.String())
	}
}

func TestHandleStateReportsLastInteractionFromCurrentSegment(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "c1", Title: "c1", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes, a.byID["c1"] = []*Node{n}, n
	a.live["c1"] = "quiet"
	a.lastChg["c1"] = time.Date(2026, 7, 15, 9, 7, 0, 0, time.UTC)
	w := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, "c1.jsonl")}
	for _, ev := range []sessionlog.Event{
		sessionlog.NewMeta("c1", "claude", "", "", a.home),
		{T: "user", Text: "old question", Time: "2026-07-14T01:00:00Z"},
		{T: "source", Time: "2026-07-15T09:00:00Z", Source: &sessionlog.SourceEvent{SessionID: "s2", Reason: "clear"}},
		{T: "assistant", Text: "ready", Time: "2026-07-15T09:01:00Z"},
		{T: "user", Text: "new question", Time: "2026-07-15T09:05:00Z"},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("state code = %d", rec.Code)
	}
	var body struct {
		Nodes []struct {
			ID              string `json:"id"`
			LastActivity    int64  `json:"last_activity"`
			LastInteraction int64  `json:"last_interaction"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Nodes) != 1 {
		t.Fatalf("nodes = %+v", body.Nodes)
	}
	if body.Nodes[0].LastActivity != time.Date(2026, 7, 15, 9, 7, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("last_activity = %d", body.Nodes[0].LastActivity)
	}
	if body.Nodes[0].LastInteraction != time.Date(2026, 7, 15, 9, 5, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("last_interaction = %d, want current-segment user turn", body.Nodes[0].LastInteraction)
	}
}

// handleState projects each node's per-station labels so the map can render a
// stop with its own name/description rather than the node's single title.
func TestHandleStateEmitsStationLabels(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "n1", Title: "Head", Agent: "claude", Dir: "/tmp", CreatedAt: "2026-07-01T09:00:00Z"}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	w := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, "n1.jsonl")}
	for _, ev := range []sessionlog.Event{
		sessionlog.NewMeta("n1", "claude", "", "", a.home),
		sessionlog.NewStation("2026-07-01T09:00:00Z", "First", "first desc"),
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("state code = %d", rec.Code)
	}
	var body struct {
		Nodes []struct {
			StationLabels map[string]struct {
				Title string `json:"title"`
				Desc  string `json:"desc"`
			} `json:"station_labels"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Nodes) != 1 {
		t.Fatalf("nodes = %d", len(body.Nodes))
	}
	got := body.Nodes[0].StationLabels["2026-07-01T09:00:00Z"]
	if got.Title != "First" || got.Desc != "first desc" {
		t.Fatalf("station_labels = %+v, want First/first desc", body.Nodes[0].StationLabels)
	}
}

// scimux's own throwaway probe sessions (the Claude usage/model status-line
// probes) live on the supervised socket for a few seconds and account for no
// node, so the subtractive unadopted list used to offer them for adoption:
// an "unadopted tmux session" card that appeared and vanished by itself. The
// probe namespace is reserved, never a stranger.
func TestHandleStateDoesNotDiscoverProbeSessions(t *testing.T) {
	probe, err := claudeUsageProbeSessionName()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTmux{list: []string{"ghost", probe}}
	a := newTestApp(t, f)
	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["unadopted"]; ok {
		t.Errorf("external discovery returned for probe %s: %s", probe, rec.Body.String())
	}
}

type fpProc struct {
	countingProc
	mismatch bool
	calls    int
}

func (p *fpProc) FingerprintMismatch(string) bool {
	p.calls++
	return p.mismatch
}

func TestHandleStateSkipsMuseFingerprintProbeForOtherAgents(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	seedStructuredNode(t, a, "codex-fp", "codex", "codex")
	proc := &fpProc{}
	a.testProc = proc

	a.handleState(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/state", nil))
	if proc.calls != 0 {
		t.Fatalf("Muse fingerprint probes for codex = %d, want 0", proc.calls)
	}
}

func TestHandleStateMuseFingerprintWarningIsAdditive(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "muse-fp", "muse", "muse")
	proc := &fpProc{mismatch: true}
	proc.live = "quiet"
	a.testProc = proc
	a.live[n.ID] = "quiet"
	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "muse", "std-1", "", a.home),
		{T: "user", Text: "hi"},
		{T: "assistant", Text: "hello", Prov: []byte(`[{"loc":"envelope","key":"_meta","v":{"k":1}}]`)},
	})

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	nodes := decodeStateNodes(t, rec.Body.Bytes())
	var found map[string]any
	for _, row := range nodes {
		if row["id"] == n.ID {
			found = row
			break
		}
	}
	if found == nil {
		t.Fatal("muse node missing from state")
	}
	if found["muse_schema_warning"] != museSchemaWarning {
		t.Fatalf("warning = %v, want %q", found["muse_schema_warning"], museSchemaWarning)
	}
	if found["live"] != "quiet" {
		t.Fatalf("mismatch changed liveness: %v", found["live"])
	}
	if found["has_transcript"] != true {
		t.Fatal("mismatch suppressed chat availability")
	}

	proc.mismatch = false
	rec = httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	nodes = decodeStateNodes(t, rec.Body.Bytes())
	for _, row := range nodes {
		if row["id"] == n.ID {
			if _, ok := row["muse_schema_warning"]; ok {
				t.Fatalf("matching fingerprint still warned: %v", row["muse_schema_warning"])
			}
		}
	}
}
