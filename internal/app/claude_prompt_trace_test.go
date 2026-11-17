package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/sessionworker"
	"testing"
	"time"
)

// The fake pane writes a real synthetic transcript; the mirror remains the
// authoritative writer. A pane acknowledgement alone is not the assertion.
func TestClaudePromptTraceNativeAnswer(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, capture: "idle", captureAfterEnter: "working"}
	a := newTestApp(t, f)
	n, _ := seedPermClaudeWithTranscript(t, a, "n1", hookSIDOwn, `{"type":"user","timestamp":"2020-01-01T00:00:00Z","message":{"role":"user","content":"previous"}}`, `{"type":"assistant","timestamp":"2020-01-01T00:00:01Z","message":{"role":"assistant","content":"previous answer"}}`)
	boundPath := filepath.Join(filepath.Dir(n.Transcript), hookSIDOwn+".jsonl")
	if err := os.Rename(n.Transcript, boundPath); err != nil {
		t.Fatal(err)
	}
	n.Transcript = boundPath
	a.syncMirror(n)
	stamp := time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)
	f.appendOnEnter(t, n.Transcript, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"edited prompt"}}`, stamp), fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":"synthetic answer"}}`, stamp))
	mirrored := make(chan struct{})
	go func() { <-f.enterSignal(); a.syncMirror(n); close(mirrored) }()
	rec := routeRequest(newTestHandler(t, a), "POST", "/api/nodes/n1/send", `{"text":"edited prompt","trace":true}`, true)
	select {
	case <-mirrored:
	case <-time.After(5 * time.Second):
		t.Fatal("fake pane did not receive Enter")
	}
	var body struct {
		Trace *promptAddress `json:"trace"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Trace == nil {
		t.Fatalf("native send requires exact trace: %d %s", rec.Code, rec.Body.String())
	}
	path := fmt.Sprintf("/api/nodes/n1/chat?trace_uid=%s&trace_segment=%d&trace_record=%d", body.Trace.UID, body.Trace.Segment, body.Trace.Record)
	got := routeRequest(newTestHandler(t, a), "GET", path, "", false)
	var chat struct {
		TraceOutcome string `json:"trace_outcome"`
	}
	if json.Unmarshal(got.Body.Bytes(), &chat) != nil || chat.TraceOutcome != "confirmed" {
		t.Fatalf("native answer = %d %s", got.Code, got.Body.String())
	}
}

func TestClaudeTraceFrontierRequiresCompleteMirroredTurns(t *testing.T) {
	raw := []byte("{\"type\":\"user\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"same\"}}\n")
	meta := sessionlog.Event{T: "meta", Meta: &sessionlog.MetaEvent{UID: "uid"}}
	source := sessionlog.NewSource("/tmp/synthetic.jsonl", "session")
	user := sessionlog.Event{T: "user", Text: "same", Time: "2026-01-01T00:00:00Z"}
	events := []sessionlog.Event{meta, source, user}
	if !verifyClaudeFrontier(raw, events, "/tmp/synthetic.jsonl", "session") {
		t.Fatal("matching complete frontier rejected")
	}
	for _, tc := range []struct {
		name          string
		raw           []byte
		events        []sessionlog.Event
		path, session string
	}{
		{"mirror lag", raw, events[:2], "/tmp/synthetic.jsonl", "session"},
		{"partial", raw[:len(raw)-1], events, "/tmp/synthetic.jsonl", "session"},
		{"bad json", []byte("not-json\n"), events, "/tmp/synthetic.jsonl", "session"},
		{"unknown shape", []byte("{\"type\":\"future\"}\n"), events, "/tmp/synthetic.jsonl", "session"},
		{"role conflict", []byte("{\"type\":\"user\",\"message\":{\"role\":\"assistant\",\"content\":\"same\"}}\n"), events, "/tmp/synthetic.jsonl", "session"},
		{"invalid content", []byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":{}}}\n"), events, "/tmp/synthetic.jsonl", "session"},
		{"null content", []byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":null}}\n"), events, "/tmp/synthetic.jsonl", "session"},
		{"reused uid", raw, []sessionlog.Event{meta, meta, source, user}, "/tmp/synthetic.jsonl", "session"},
		{"changed time", raw, []sessionlog.Event{meta, source, {T: "user", Text: "same", Time: "different"}}, "/tmp/synthetic.jsonl", "session"},
		{"changed provenance", raw, []sessionlog.Event{meta, source, {T: "user", Text: "same", Time: user.Time, Prov: json.RawMessage(`[{"x":1}]`)}}, "/tmp/synthetic.jsonl", "session"},
		{"path", raw, events, "/tmp/other.jsonl", "session"},
		{"session", raw, events, "/tmp/synthetic.jsonl", "other"},
		{"uid", raw, []sessionlog.Event{{T: "meta"}, source, user}, "/tmp/synthetic.jsonl", "session"},
		{"changed text", raw, []sessionlog.Event{meta, source, {T: "user", Text: "other", Time: user.Time}}, "/tmp/synthetic.jsonl", "session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if verifyClaudeFrontier(tc.raw, tc.events, tc.path, tc.session) {
				t.Fatal("ambiguous frontier accepted")
			}
		})
	}
}

func TestClaudeTraceFrontierReadOnlyBinding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	log := filepath.Join(dir, "log.jsonl")
	raw := []byte("{\"type\":\"user\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"source\"}}\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	w := sessionlog.Writer{Path: log}
	for _, ev := range []sessionlog.Event{{T: "meta", Meta: &sessionlog.MetaEvent{UID: "u", Node: "n", Agent: "claude"}}, sessionlog.NewSource(path, "session"), {T: "user", Text: "source", Time: "2026-01-01T00:00:00Z"}} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := captureClaudeTraceFrontier(path, log, "n", "session")
	if !ok || got.UID != "u" || !bytes.Equal(got.Transcript, raw) {
		t.Fatalf("snapshot %#v %v", got, ok)
	}
	for _, tc := range []struct{ path, log, node, session string }{{"", log, "n", "session"}, {path, "missing", "n", "session"}, {"missing", log, "n", "session"}, {path, log, "other", "session"}, {path, log, "n", "other"}} {
		if _, ok := captureClaudeTraceFrontier(tc.path, tc.log, tc.node, tc.session); ok {
			t.Fatalf("bad binding accepted: %+v", tc)
		}
	}
	if err := os.WriteFile(log, []byte("{bad\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := captureClaudeTraceFrontier(path, log, "n", "session"); ok {
		t.Fatal("malformed log accepted")
	}
}

func TestClaudeTraceFrontierRequiresCurrentOwnedNode(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, _ := seedPermClaude(t, a, "n1", hookSIDOwn)
	path := filepath.Join(t.TempDir(), hookSIDOwn+".jsonl")
	appendLines(t, path, `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"source"}}`)
	n.Transcript = path
	a.syncMirror(n)
	if snap, ok := a.captureOwnedClaudeFrontier(context.Background(), n); !ok || snap.UID == "" {
		t.Fatalf("owned snapshot %#v %v", snap, ok)
	}
	n.Adopted = true
	if _, ok := a.captureOwnedClaudeFrontier(context.Background(), n); ok {
		t.Fatal("adopted node accepted")
	}
	n.Adopted = false
	a.mu.Lock()
	a.byID[n.ID] = &Node{ID: n.ID, Agent: "claude"}
	a.mu.Unlock()
	if _, ok := a.captureOwnedClaudeFrontier(context.Background(), n); ok {
		t.Fatal("replaced node accepted")
	}
}

func TestClaudeTraceFrontierWorkerBinding(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, _ := seedPermClaude(t, a, "n1", hookSIDOwn)
	path := filepath.Join(t.TempDir(), hookSIDOwn+".jsonl")
	appendLines(t, path, `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"source"}}`)
	n.Transcript = path
	a.syncMirror(n)
	state := sessionworker.State{HasSession: true, Supervision: string(claudeSupStrict), HookID: "owned", HookGeneration: 1, Transcript: path, SessionID: hookSIDOwn}
	attachSyntheticWorker(t, a, n.ID, &syntheticSessionHarness{launched: true, state: state})
	if snap, ok := a.captureOwnedClaudeFrontier(context.Background(), n); !ok || snap.UID == "" {
		t.Fatalf("worker snapshot %#v %v", snap, ok)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := a.captureOwnedClaudeFrontier(ctx, n); ok {
		t.Fatal("cancelled snapshot accepted")
	}
}

func TestClaudeTraceAddressRequiresUniqueDatedAppend(t *testing.T) {
	admission := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := claudeTraceFrontier{UID: "u", Path: "p", Session: "s", Transcript: []byte("prefix\n"), Events: []sessionlog.Event{{T: "meta", Meta: &sessionlog.MetaEvent{UID: "u"}}, {T: "source"}}}
	later := base
	later.Transcript = []byte("prefix\nnew\n")
	later.Events = append(append([]sessionlog.Event{}, base.Events...), sessionlog.Event{T: "user", Text: "edited", Time: admission.Add(time.Second).Format(time.RFC3339Nano)})
	got, ok := claudePromptAddress(base, later, "edited", admission)
	if !ok || got.Record != 2 || got.Segment != 1 {
		t.Fatalf("address %+v %v", got, ok)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*claudeTraceFrontier)
	}{
		{"uid", func(x *claudeTraceFrontier) { x.UID = "other" }}, {"path", func(x *claudeTraceFrontier) { x.Path = "other" }}, {"session", func(x *claudeTraceFrontier) { x.Session = "other" }},
		{"transcript mutation", func(x *claudeTraceFrontier) { x.Transcript = []byte("different") }},
		{"hook", func(x *claudeTraceFrontier) { x.HookID = "other" }}, {"hook generation", func(x *claudeTraceFrontier) { x.HookGeneration = 1 }}, {"worker", func(x *claudeTraceFrontier) { x.Worker = &workerEntry{} }},
		{"invalid timestamp", func(x *claudeTraceFrontier) { x.Events[2].Time = "bad" }}, {"old delayed identical", func(x *claudeTraceFrontier) { x.Events[2].Time = admission.Add(-time.Second).Format(time.RFC3339Nano) }},
		{"second user", func(x *claudeTraceFrontier) { x.Events = append(x.Events, sessionlog.Event{T: "user", Text: "edited"}) }},
		{"clear seam", func(x *claudeTraceFrontier) { x.Events = append(x.Events, sessionlog.Event{T: "source"}) }},
		{"changed prefix", func(x *claudeTraceFrontier) { x.Events[0].Meta = &sessionlog.MetaEvent{UID: "different"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := later
			x.Events = append([]sessionlog.Event(nil), later.Events...)
			tc.mutate(&x)
			if _, ok := claudePromptAddress(base, x, "edited", admission); ok {
				t.Fatal("ambiguous address accepted")
			}
		})
	}
}

func TestClaudePromptTraceWorkerAnswer(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, capture: "idle", captureAfterEnter: "working"}
	a := newTestApp(t, f)
	n, _ := seedPermClaudeWithTranscript(t, a, "n1", hookSIDOwn, `{"type":"user","timestamp":"2020-01-01T00:00:00Z","message":{"role":"user","content":"previous"}}`)
	path := filepath.Join(filepath.Dir(n.Transcript), hookSIDOwn+".jsonl")
	if err := os.Rename(n.Transcript, path); err != nil {
		t.Fatal(err)
	}
	n.Transcript = path
	a.syncMirror(n)
	attachInteractionWorker(t, a, n)
	stamp := time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)
	f.appendOnEnter(t, path, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"edited"}}`, stamp), fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":"answer"}}`, stamp))
	done := make(chan struct{})
	go func() { <-f.enterSignal(); a.syncMirror(n); close(done) }()
	rec := routeRequest(newTestHandler(t, a), "POST", "/api/nodes/n1/send", `{"text":"edited","trace":true}`, true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no fake Enter")
	}
	var body struct {
		Trace *promptAddress `json:"trace"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Trace == nil {
		t.Fatalf("worker native trace %d %s", rec.Code, rec.Body.String())
	}
	pathQuery := fmt.Sprintf("/api/nodes/n1/chat?trace_uid=%s&trace_segment=%d&trace_record=%d", body.Trace.UID, body.Trace.Segment, body.Trace.Record)
	out := routeRequest(newTestHandler(t, a), "GET", pathQuery, "", false)
	if !strings.Contains(out.Body.String(), `"trace_outcome":"confirmed"`) {
		t.Fatal(out.Body.String())
	}
}

func TestClaudePromptTraceUnconfirmedNeverReturnsAddress(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, capture: "unchanged"}
	a := newTestApp(t, f)
	n, _ := seedPermClaudeWithTranscript(t, a, "n1", hookSIDOwn, `{"type":"user","timestamp":"2020-01-01T00:00:00Z","message":{"role":"user","content":"old"}}`)
	path := filepath.Join(filepath.Dir(n.Transcript), hookSIDOwn+".jsonl")
	if err := os.Rename(n.Transcript, path); err != nil {
		t.Fatal(err)
	}
	n.Transcript = path
	a.syncMirror(n)
	rec := routeRequest(newTestHandler(t, a), "POST", "/api/nodes/n1/send", `{"text":"new","trace":true}`, true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"unconfirmed"`) || strings.Contains(rec.Body.String(), `"trace"`) {
		t.Fatalf("unconfirmed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestClaudeTraceWaitHonorsCancellation(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := a.awaitClaudePromptAddress(ctx, nil, claudeTraceFrontier{}, "x", time.Now()); ok {
		t.Fatal("cancelled wait returned address")
	}
}

func TestClaudeTraceNoAnswerStaysPending(t *testing.T) {
	events := []sessionlog.Event{{T: "meta", Meta: &sessionlog.MetaEvent{UID: "u"}}, {T: "source"}, {T: "user", Text: "one"}}
	if got := promptTraceOutcome(events, promptAddress{UID: "u", Segment: 1, Record: 2}); got != "pending" {
		t.Fatal(got)
	}
}

func TestClaudePromptTraceDoesNotAttributeDelayedIdentical(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, capture: "idle", captureAfterEnter: "working"}
	a := newTestApp(t, f)
	n, _ := seedPermClaudeWithTranscript(t, a, "n1", hookSIDOwn, `{"type":"user","timestamp":"2020-01-01T00:00:00Z","message":{"role":"user","content":"earlier"}}`)
	path := filepath.Join(filepath.Dir(n.Transcript), hookSIDOwn+".jsonl")
	if err := os.Rename(n.Transcript, path); err != nil {
		t.Fatal(err)
	}
	n.Transcript = path
	a.syncMirror(n)
	f.appendOnEnter(t, path, `{"type":"user","timestamp":"2020-01-02T00:00:00Z","message":{"role":"user","content":"same"}}`, `{"type":"assistant","timestamp":"2020-01-02T00:00:01Z","message":{"role":"assistant","content":"old answer"}}`)
	done := make(chan struct{})
	go func() { <-f.enterSignal(); a.syncMirror(n); close(done) }()
	rec := routeRequest(newTestHandler(t, a), "POST", "/api/nodes/n1/send", `{"text":"same","trace":true}`, true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no fake Enter")
	}
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"trace"`) {
		t.Fatalf("old delayed row attributed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestClaudeTraceFrontierRejectsChangingOrFailedReads(t *testing.T) {
	transcript := []byte("{\"type\":\"user\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"same\"}}\n")
	log := []byte("{\"t\":\"meta\",\"meta\":{\"uid\":\"u\",\"node\":\"n\",\"agent\":\"claude\"}}\n{\"t\":\"source\",\"source\":{\"path\":\"p\",\"sessionId\":\"s\"}}\n{\"t\":\"user\",\"text\":\"same\",\"time\":\"2026-01-01T00:00:00Z\"}\n")
	for _, tc := range []struct {
		name         string
		fail, change int
	}{{"transcript error", 1, 0}, {"log error", 2, 0}, {"second transcript error", 3, 0}, {"second log error", 4, 0}, {"transcript changed", 0, 3}, {"log changed", 0, 4}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			read := func(path string) ([]byte, error) {
				calls++
				if calls == tc.fail {
					return nil, errors.New("synthetic read failure")
				}
				data := transcript
				if path == "l" {
					data = log
				}
				if calls == tc.change {
					return append(append([]byte(nil), data...), 'x'), nil
				}
				return data, nil
			}
			if _, ok := captureClaudeTraceFrontierWithRead("p", "l", "n", "s", read); ok {
				t.Fatal("unstable snapshot accepted")
			}
		})
	}
}

func TestClaudeTraceProvenanceSemanticComparison(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"", "", true}, {"", "[]", false},
		{`[{"x":1,"y":[true]}]`, `[ { "y" : [ true ], "x": 1 } ]`, true},
		{`[{"x":1}]`, `[{"x":2}]`, false}, {`invalid`, `[]`, false},
	} {
		if got := sameClaudeProvenance(json.RawMessage(tc.a), json.RawMessage(tc.b)); got != tc.want {
			t.Fatalf("%s / %s: %v", tc.a, tc.b, got)
		}
	}
}

type frontierStateHarness struct {
	*syntheticSessionHarness
	calls       int
	second      func()
	secondState *sessionworker.State
	delivery    *sessionworker.Delivery
	sendErr     error
	sends       int
}

func (h *frontierStateHarness) Send(ctx context.Context, text string) (sessionworker.Delivery, error) {
	h.mu.Lock()
	h.sends++
	h.mu.Unlock()
	if h.sendErr != nil {
		return sessionworker.Delivery{}, h.sendErr
	}
	if h.delivery != nil {
		return *h.delivery, nil
	}
	return h.syntheticSessionHarness.Send(ctx, text)
}

func (h *frontierStateHarness) State(context.Context) sessionworker.State {
	h.calls++
	if h.calls == 2 && h.second != nil {
		h.second()
	}
	if h.calls == 2 && h.secondState != nil {
		return *h.secondState
	}
	return h.state
}

func TestClaudeTraceFrontierWorkerReplacement(t *testing.T) {
	for _, mode := range []string{"invalid initial", "changed session", "replaced node"} {
		t.Run(mode, func(t *testing.T) {
			a := newTestApp(t, &fakeTmux{})
			n, _ := seedPermClaude(t, a, "n1", hookSIDOwn)
			path := filepath.Join(t.TempDir(), hookSIDOwn+".jsonl")
			appendLines(t, path, `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"source"}}`)
			n.Transcript = path
			a.syncMirror(n)
			state := sessionworker.State{HasSession: true, Supervision: string(claudeSupStrict), HookID: "owned", HookGeneration: 1, Transcript: path, SessionID: hookSIDOwn}
			h := &frontierStateHarness{syntheticSessionHarness: &syntheticSessionHarness{launched: true, state: state}}
			if mode == "invalid initial" {
				h.state.HasSession = false
			}
			if mode == "changed session" {
				changed := state
				changed.SessionID = "different"
				h.secondState = &changed
			}
			if mode == "replaced node" {
				h.second = func() { a.mu.Lock(); a.byID[n.ID] = &Node{ID: n.ID, Agent: "claude"}; a.mu.Unlock() }
			}
			identity := sessionworker.Identity{WorkerID: "synthetic", Agent: "claude", Build: "test"}
			server, err := sessionworker.Listen("", identity, h)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			client, err := sessionworker.NewClient(server.Link())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			a.workers = &workerManager{entries: map[string]*workerEntry{n.ID: {client: client, identity: identity}}, dataDir: t.TempDir()}
			if _, ok := a.captureOwnedClaudeFrontier(context.Background(), n); ok {
				t.Fatal("invalid worker identity accepted")
			}
		})
	}
}

func TestClaudePromptTraceWorkerUnconfirmedAndError(t *testing.T) {
	for _, mode := range []string{"unconfirmed", "error"} {
		t.Run(mode, func(t *testing.T) {
			a := newTestApp(t, &fakeTmux{})
			n, _ := seedPermClaude(t, a, "n1", hookSIDOwn)
			path := filepath.Join(t.TempDir(), hookSIDOwn+".jsonl")
			appendLines(t, path, `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"source"}}`)
			n.Transcript = path
			a.syncMirror(n)
			state := sessionworker.State{HasSession: true, Supervision: string(claudeSupStrict), HookID: "owned", Transcript: path, SessionID: hookSIDOwn}
			h := &frontierStateHarness{syntheticSessionHarness: &syntheticSessionHarness{launched: true, state: state}}
			if mode == "unconfirmed" {
				h.delivery = &sessionworker.Delivery{Status: sessionworker.DeliveryUnconfirmed}
			} else {
				h.sendErr = errors.New("synthetic send failure")
			}
			identity := sessionworker.Identity{WorkerID: "synthetic", Agent: "claude", Build: "test"}
			server, err := sessionworker.Listen("", identity, h)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			client, err := sessionworker.NewClient(server.Link())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			a.workers = &workerManager{entries: map[string]*workerEntry{n.ID: {client: client, identity: identity}}, dataDir: t.TempDir()}
			rec := routeRequest(newTestHandler(t, a), "POST", "/api/nodes/n1/send", `{"text":"new","trace":true}`, true)
			h.mu.Lock()
			sends := h.sends
			h.mu.Unlock()
			if strings.Contains(rec.Body.String(), `"trace"`) || sends != 1 {
				t.Fatalf("%s: %d %s sends %d", mode, rec.Code, rec.Body.String(), sends)
			}
			if mode == "unconfirmed" && !strings.Contains(rec.Body.String(), `"unconfirmed"`) {
				t.Fatal(rec.Body.String())
			}
			if mode == "error" && rec.Code < 400 {
				t.Fatal(rec.Body.String())
			}
		})
	}
}

func TestClaudeTraceFrontierCancellationDuringRead(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, _ := seedPermClaude(t, a, "n1", hookSIDOwn)
	path := filepath.Join(t.TempDir(), hookSIDOwn+".jsonl")
	appendLines(t, path, `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"source"}}`)
	n.Transcript = path
	a.syncMirror(n)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	read := func(path string) ([]byte, error) {
		calls++
		b, err := os.ReadFile(path)
		if calls == 4 {
			cancel()
		}
		return b, err
	}
	if _, ok := a.captureOwnedClaudeFrontierWithRead(ctx, n, read); ok {
		t.Fatal("cancelled frontier accepted")
	}
}

func TestClaudePromptTraceNativeUserAloneRemainsPending(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}, capture: "idle", captureAfterEnter: "working"}
	a := newTestApp(t, f)
	n, _ := seedPermClaudeWithTranscript(t, a, "n1", hookSIDOwn, `{"type":"user","timestamp":"2020-01-01T00:00:00Z","message":{"role":"user","content":"previous"}}`)
	path := filepath.Join(filepath.Dir(n.Transcript), hookSIDOwn+".jsonl")
	if err := os.Rename(n.Transcript, path); err != nil {
		t.Fatal(err)
	}
	n.Transcript = path
	a.syncMirror(n)
	stamp := time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)
	f.appendOnEnter(t, path, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"edited"}}`, stamp))
	done := make(chan struct{})
	go func() { <-f.enterSignal(); a.syncMirror(n); close(done) }()
	rec := routeRequest(newTestHandler(t, a), "POST", "/api/nodes/n1/send", `{"text":"edited","trace":true}`, true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no fake Enter")
	}
	var body struct {
		Trace *promptAddress `json:"trace"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Trace == nil {
		t.Fatalf("trace: %d %s", rec.Code, rec.Body.String())
	}
	query := fmt.Sprintf("/api/nodes/n1/chat?trace_uid=%s&trace_segment=%d&trace_record=%d", body.Trace.UID, body.Trace.Segment, body.Trace.Record)
	result := routeRequest(newTestHandler(t, a), "GET", query, "", false)
	if !strings.Contains(result.Body.String(), `"trace_outcome":"pending"`) {
		t.Fatalf("queued user falsely confirmed: %s", result.Body.String())
	}
}

func TestClaudeTraceWaitStopsOnChangedLogIdentity(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n, _ := seedPermClaude(t, a, "n1", hookSIDOwn)
	path := filepath.Join(t.TempDir(), hookSIDOwn+".jsonl")
	appendLines(t, path, `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"source"}}`)
	n.Transcript = path
	a.syncMirror(n)
	before, ok := a.captureOwnedClaudeFrontier(context.Background(), n)
	if !ok {
		t.Fatal("no baseline")
	}
	before.UID = "replaced"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, ok := a.awaitClaudePromptAddress(ctx, n, before, "x", time.Now()); ok {
		t.Fatal("changed identity accepted")
	}
	if ctx.Err() != nil {
		t.Fatal("changed identity waited until the deadline")
	}
}

func TestClaudeTraceWaitStopsDuringPendingPoll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitClaudeTracePoll(ctx, make(chan time.Time)) {
		t.Fatal("cancelled poll continued")
	}
	waiting, cancelWaiting := context.WithCancel(context.Background())
	defer cancelWaiting()
	capture := func() (claudeTraceFrontier, bool) { cancelWaiting(); return claudeTraceFrontier{}, false }
	if _, ok := awaitClaudePromptAddressWithCapture(waiting, claudeTraceFrontier{}, "x", time.Now(), capture); ok {
		t.Fatal("cancelled read returned address")
	}

	tick := make(chan time.Time, 1)
	tick <- time.Now()
	if !waitClaudeTracePoll(context.Background(), tick) {
		t.Fatal("ready poll stopped")
	}
}
