package app

import (
	"encoding/json"
	"github.com/scimux/scimux/internal/sessionlog"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPromptTraceAdmissionAndEvidence(t *testing.T) {
	meta := sessionlog.Event{T: "meta", Meta: &sessionlog.MetaEvent{UID: "uid-one"}}
	user := sessionlog.Event{T: "user", Text: "edited prompt"}
	response := sessionlog.Event{T: "assistant", Text: "response"}
	failure := sessionlog.Event{T: "error", Error: "rejected"}
	baseline := []sessionlog.Event{meta, {T: "error", Error: "stale"}}
	queued := append(append([]sessionlog.Event{}, baseline...), user)
	address, ok := queuedPromptAddress(baseline, queued, "edited prompt")
	if !ok || address.UID != "uid-one" || address.Segment != 0 || address.Record != 2 {
		t.Fatalf("queued address = %+v, %v", address, ok)
	}
	tests := []struct {
		name   string
		events []sessionlog.Event
		want   string
	}{
		{"queued is not delivery", queued, "pending"},
		{"agent answer", append(append([]sessionlog.Event{}, queued...), response), "confirmed"},
		{"rejection", append(append([]sessionlog.Event{}, queued...), failure), "rejected"},
		{"later identical prompt is a boundary", append(append(append([]sessionlog.Event{}, queued...), user), response), "unknown"},
		{"clear is a boundary", append(append(append([]sessionlog.Event{}, queued...), sessionlog.Event{T: "source"}), response), "unknown"},
		{"error precedes later response", append(append(append([]sessionlog.Event{}, queued...), failure), response), "rejected"},
		{"reused slug", []sessionlog.Event{{T: "meta", Meta: &sessionlog.MetaEvent{UID: "other"}}, user, response}, "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := promptTraceOutcome(tt.events, address); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
	for _, tc := range []struct {
		name          string
		before, after []sessionlog.Event
	}{
		{"changed UID", baseline, []sessionlog.Event{{T: "meta", Meta: &sessionlog.MetaEvent{UID: "other"}}, user}},
		{"two identical users", baseline, append(append(append([]sessionlog.Event{}, baseline...), user), user)},
		{"clear", baseline, append(append(append([]sessionlog.Event{}, baseline...), sessionlog.Event{T: "source"}), user)},
		{"no new row", baseline, baseline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := queuedPromptAddress(tc.before, tc.after, "edited prompt"); ok {
				t.Fatal("ambiguous admission accepted")
			}
		})
	}
}

func TestPromptTraceExistingSendAndChatRoutes(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{ID: "trace-node", Agent: "codex", Transport: "codex"}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	if err := os.MkdirAll(a.sessionsDir, 0700); err != nil {
		t.Fatal(err)
	}
	writer := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	appendEvent := func(e sessionlog.Event) {
		t.Helper()
		if err := writer.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(sessionlog.Event{T: "meta", Meta: &sessionlog.MetaEvent{UID: "trace-uid"}})
	proc := &stubProc{hasSession: true, live: "idle"}
	a.testProc = proc
	proc.sendHook = func(_ *stubProc) { appendEvent(sessionlog.Event{T: "user", Text: "edited"}) }
	h := newTestHandler(t, a)
	send := routeRequest(h, "POST", "/api/nodes/trace-node/send", `{"text":"edited","trace":true}`, true)
	if send.Code != 200 {
		t.Fatalf("send %d %s", send.Code, send.Body.String())
	}
	var body struct {
		Status string         `json:"status"`
		Trace  *promptAddress `json:"trace"`
	}
	if err := json.Unmarshal(send.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "acknowledged" || body.Trace == nil || *body.Trace != (promptAddress{UID: "trace-uid", Record: 1}) {
		t.Fatalf("send body: %s", send.Body.String())
	}
	url := "/api/nodes/trace-node/chat?trace_uid=trace-uid&trace_segment=0&trace_record=1"
	check := func(want string) {
		t.Helper()
		rec := routeRequest(h, "GET", url, "", false)
		var data map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		if got := data["trace_outcome"]; got != want {
			t.Fatalf("outcome %v want %s: %s", got, want, rec.Body.String())
		}
	}
	check("pending")
	appendEvent(sessionlog.Event{T: "assistant", Text: "actual response"})
	check("confirmed")
	// Ordinary sends retain the original response, without a trace field.
	proc.sendHook = func(_ *stubProc) { appendEvent(sessionlog.Event{T: "user", Text: "ordinary"}) }
	ordinary := routeRequest(h, "POST", "/api/nodes/trace-node/send", `{"text":"ordinary"}`, true)
	if ordinary.Code != 200 || strings.Contains(ordinary.Body.String(), "trace") {
		t.Fatalf("ordinary: %d %s", ordinary.Code, ordinary.Body.String())
	}
	bad := routeRequest(h, "GET", "/api/nodes/trace-node/chat?trace_uid=trace-uid&trace_segment=bad&trace_record=1", "", false)
	if !strings.Contains(bad.Body.String(), `"trace_outcome":"unknown"`) {
		t.Fatal(bad.Body.String())
	}
}

func TestPromptTraceRejectedRowDoesNotBorrowLaterIdenticalResponse(t *testing.T) {
	events := []sessionlog.Event{{T: "meta", Meta: &sessionlog.MetaEvent{UID: "uid"}}, {T: "user", Text: "same"}, {T: "error", Error: "dispatch rejected"}, {T: "user", Text: "same"}, {T: "assistant", Text: "later response"}}
	if got := promptTraceOutcome(events, promptAddress{UID: "uid", Record: 1}); got != "rejected" {
		t.Fatal(got)
	}
	if got := promptTraceOutcome(events, promptAddress{UID: "uid", Record: 3}); got != "confirmed" {
		t.Fatal(got)
	}
}

func TestPromptTraceConcurrentOwnerSendsSerializeReadback(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{ID: "trace-concurrent", Agent: "codex", Transport: "codex"}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	if err := os.MkdirAll(a.sessionsDir, 0700); err != nil {
		t.Fatal(err)
	}
	writer := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	if err := writer.Append(sessionlog.Event{T: "meta", Meta: &sessionlog.MetaEvent{UID: "uid"}}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	proc := &stubProc{hasSession: true, live: "idle"}
	a.testProc = proc
	proc.sendHook = func(_ *stubProc) {
		if err := writer.Append(sessionlog.Event{T: "user", Text: "same"}); err != nil {
			t.Error(err)
		}
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	h := newTestHandler(t, a)
	results := make(chan *httptest.ResponseRecorder, 2)
	send := func() {
		results <- routeRequest(h, "POST", "/api/nodes/trace-concurrent/send", `{"text":"same","trace":true}`, true)
	}
	go send()
	<-entered
	go send()
	close(release)
	first, second := <-results, <-results
	seen := map[int]bool{}
	for _, r := range []*httptest.ResponseRecorder{first, second} {
		var body struct {
			Trace *promptAddress `json:"trace"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &body) != nil || body.Trace == nil {
			t.Fatalf("response %d %s", r.Code, r.Body.String())
		}
		seen[body.Trace.Record] = true
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("addresses %v", seen)
	}
}

func TestPromptTraceMalformedAndSegmentedLogs(t *testing.T) {
	meta := sessionlog.Event{T: "meta", Meta: &sessionlog.MetaEvent{UID: "uid"}}
	user := sessionlog.Event{T: "user", Text: "text"}
	for _, before := range [][]sessionlog.Event{{{T: "meta"}}, {{T: "meta", Meta: &sessionlog.MetaEvent{}}}, {meta, meta}, {{T: "error"}}} {
		after := append(append([]sessionlog.Event{}, before...), user)
		if _, ok := queuedPromptAddress(before, after, "text"); ok {
			t.Fatalf("invalid baseline %#v", before)
		}
	}
	before := []sessionlog.Event{meta, {T: "source"}}
	events := append(append([]sessionlog.Event{}, before...), user)
	address, ok := queuedPromptAddress(before, events, "text")
	if !ok || address.Segment != 1 || address.Record != 2 {
		t.Fatalf("%+v %v", address, ok)
	}
	for _, tc := range []struct {
		events []sessionlog.Event
		addr   promptAddress
	}{
		{events, promptAddress{}}, {events, promptAddress{UID: "uid", Segment: -1}}, {events, promptAddress{UID: "uid", Record: -1}}, {events, promptAddress{UID: "uid", Record: 10}},
		{[]sessionlog.Event{{T: "meta"}, user}, promptAddress{UID: "uid", Record: 1}},
		{[]sessionlog.Event{meta, meta, user}, promptAddress{UID: "uid", Record: 2}},
		{events, promptAddress{UID: "uid", Record: 2}},
	} {
		if got := promptTraceOutcome(tc.events, tc.addr); got != "unknown" {
			t.Fatalf("invalid trace %+v: %s", tc.addr, got)
		}
	}
	if got := promptTraceOutcome(append(events, sessionlog.Event{T: "assistant", Text: "  "}), address); got != "pending" {
		t.Fatal(got)
	}
	if got := promptTraceOutcome(append(events, sessionlog.Event{T: "assistant", Text: "ok"}), address); got != "confirmed" {
		t.Fatal(got)
	}
}

func TestStructuredTraceTransport(t *testing.T) {
	if structuredTraceTransport(&Node{Agent: "claude", Transport: "codex"}) {
		t.Fatal("Claude is not traceable")
	}
	for _, tc := range []struct {
		transport string
		want      bool
	}{{"codex", true}, {"acp", true}, {"muse", true}, {"tmux", false}, {"", false}} {
		if got := structuredTraceTransport(&Node{Transport: tc.transport}); got != tc.want {
			t.Fatalf("%s: %v", tc.transport, got)
		}
	}
}

func TestPromptTraceCannotFollowReplacedNode(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{ID: "replace", Agent: "codex", Transport: "codex"}
	a.byID[n.ID] = n
	a.nodes = append(a.nodes, n)
	if err := os.MkdirAll(a.sessionsDir, 0700); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	if err := w.Append(sessionlog.Event{T: "meta", Meta: &sessionlog.MetaEvent{UID: "original"}}); err != nil {
		t.Fatal(err)
	}
	proc := &stubProc{hasSession: true, live: "idle"}
	a.testProc = proc
	proc.sendHook = func(_ *stubProc) {
		if err := w.Append(sessionlog.Event{T: "user", Text: "edited"}); err != nil {
			t.Error(err)
		}
		a.mu.Lock()
		a.byID[n.ID] = &Node{ID: n.ID, Agent: "codex", Transport: "codex"}
		a.mu.Unlock()
	}
	rec := routeRequest(newTestHandler(t, a), "POST", "/api/nodes/replace/send", `{"text":"edited","trace":true}`, true)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"trace"`) {
		t.Fatalf("replacement: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPromptTraceRecipientCannotSendToSharedAddress(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	proc := &stubProc{hasSession: true, live: "idle"}
	a.testProc = proc
	r := routeRequest(newTestHandler(t, a), "POST", "/api/nodes/team%2Fserver%2Fowner%2Fchat%2Fgeneration/send", `{"text":"forged","trace":true}`, true)
	if r.Code < 400 || proc.sendCalls != 0 {
		t.Fatalf("forged request: %d %s, sends %d", r.Code, r.Body.String(), proc.sendCalls)
	}
}

func TestPromptTraceRouteOmitsAmbiguousRowsAndUnsupportedEvidence(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{ID: "ambiguous", Agent: "codex", Transport: "codex"}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	if err := os.MkdirAll(a.sessionsDir, 0700); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	if err := w.Append(sessionlog.Event{T: "meta", Meta: &sessionlog.MetaEvent{UID: "uid"}}); err != nil {
		t.Fatal(err)
	}
	proc := &stubProc{hasSession: true, live: "idle"}
	a.testProc = proc
	proc.sendHook = func(_ *stubProc) {
		for range 2 {
			if err := w.Append(sessionlog.Event{T: "user", Text: "same"}); err != nil {
				t.Error(err)
			}
		}
	}
	h := newTestHandler(t, a)
	r := routeRequest(h, "POST", "/api/nodes/ambiguous/send", `{"text":"same","trace":true}`, true)
	if r.Code != 200 || strings.Contains(r.Body.String(), `"trace"`) {
		t.Fatalf("ambiguous: %d %s", r.Code, r.Body.String())
	}
	n.Transport = "tmux"
	r = routeRequest(h, "GET", "/api/nodes/ambiguous/chat?trace_uid=uid&trace_segment=0&trace_record=1", "", false)
	if !strings.Contains(r.Body.String(), `"trace_outcome":"unknown"`) {
		t.Fatal(r.Body.String())
	}
}
