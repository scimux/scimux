package main

// Packet 3C characterization: pure state-projection helpers and /api/state
// snapshot contracts that existing tests leave as gaps. Complements — does not
// replace — TestHandleStateETagAndUnadopted, TestHandleStateUnadoptedExcludesReserved,
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
		Agent:       "claude",
		Model:       "claude-sonnet",
		Effort:      "high",
		Dir:         "/work",
		SessionID:   "sess-1",
		Transcript:  "/tmp/t.jsonl",
		Adopted:     true,
		Transport:   "", // tmux default
		CreatedAt:   "2026-07-14T00:00:00Z",
	}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.live[n.ID] = "active"
	a.attn[n.ID] = "approval"
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
		"agent": "claude", "model": "claude-sonnet", "effort": "high",
		"dir": "/work", "session_id": "sess-1", "transcript": "/tmp/t.jsonl",
		"created_at": "2026-07-14T00:00:00Z",
		"live":       "active", "attention": "approval",
	}
	for k, want := range wantStr {
		if got[k] != want {
			t.Errorf("%s = %v, want %q", k, got[k], want)
		}
	}
	if got["adopted"] != true {
		t.Errorf("adopted = %v, want true", got["adopted"])
	}
	// tmux HasTranscript follows the transcript link.
	if got["has_transcript"] != true {
		t.Errorf("has_transcript = %v, want true (transcript set)", got["has_transcript"])
	}
	if int64(got["last_activity"].(float64)) != chg.UnixMilli() {
		t.Errorf("last_activity = %v, want %d", got["last_activity"], chg.UnixMilli())
	}
	// No session log → last_interaction falls to created_at.
	if int64(got["last_interaction"].(float64)) != unixMSStamp(n.CreatedAt) {
		t.Errorf("last_interaction = %v, want created_at ms", got["last_interaction"])
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
	a.nodes = []*Node{tmuxNo, tmuxYes, acpN, codexN}
	for _, n := range a.nodes {
		a.byID[n.ID] = n
	}

	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
	nodes := decodeStateNodes(t, rec.Body.Bytes())
	want := map[string]bool{
		"tmux-no": false, "tmux-yes": true, "acp1": true, "codex1": true,
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

// ---------- 4. Response shape, stops, unadopted, ETag/content-type ----------

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
	// Complements TestHandleStateETagAndUnadopted (304 path) with top-level
	// shape, content type, and etag stability across identical snapshots.
	f := &fakeTmux{list: []string{"node-a", "ghost"}}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "node-a", Title: "A", Agent: "claude", Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z"}}
	a.byID["node-a"] = a.nodes[0]
	a.reserved["pending"] = true
	// reserved session is not in list; only ghost is unadopted.

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
	for _, key := range []string{"nodes", "unadopted", "sys", "socket", "hostname", "version"} {
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
	// unadopted: known sessions excluded; reserved not in list here; ghost adoptable.
	unad, ok := top["unadopted"].([]any)
	if !ok || len(unad) != 1 || unad[0] != "ghost" {
		t.Errorf("unadopted = %v, want [ghost]", top["unadopted"])
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
