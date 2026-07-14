package codex

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// eventCollector is a concurrency-safe sink capturing decoded log events.
type eventCollector struct {
	mu sync.Mutex
	ev []Event
}

func (c *eventCollector) sink(e Event) {
	c.mu.Lock()
	c.ev = append(c.ev, e)
	c.mu.Unlock()
}
func (c *eventCollector) snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.ev...)
}
func (c *eventCollector) byType(t string) []Event {
	var out []Event
	for _, e := range c.snapshot() {
		if e.T == t {
			out = append(out, e)
		}
	}
	return out
}

func newClientWithMock(t *testing.T) (*Client, *mockServer, *eventCollector) {
	t.Helper()
	mt, ms := newMockTransport()
	col := &eventCollector{}
	c := NewClient(mt, col.sink, nil)
	t.Cleanup(func() { _ = c.Close() })
	return c, ms, col
}

// happy-path result body for thread/start, with the effective policy differing
// from what a caller might request (exercises the read-back).
const threadStartResult = `{"thread":{"id":"THREAD-1","path":"/scrubbed/rollout.jsonl"},"model":"gpt-5.5","approvalPolicy":"on-request","sandbox":{"type":"workspaceWrite"},"reasoningEffort":"high"}`

func TestFullTurnHappyPath(t *testing.T) {
	c, ms, col := newClientWithMock(t)
	ctx := context.Background()

	var info ThreadInfo
	done := make(chan error, 1)
	go func() {
		if _, err := c.Initialize(ctx, "test", "1"); err != nil {
			done <- err
			return
		}
		ti, err := c.StartThread(ctx, StartThreadParams{Cwd: "/w", ApprovalPolicy: "untrusted"})
		if err != nil {
			done <- err
			return
		}
		info = ti
		done <- c.RunTurn(ctx, ti.ID, "ping")
	}()

	// Server scenario (runs on the test goroutine so t.Fatal is legal).
	if r := ms.nextReq(t); r.Method != "initialize" {
		t.Fatalf("first req = %q", r.Method)
	} else {
		ms.reply(t, r.ID, `{"userAgent":"codex","platformOs":"linux"}`)
	}
	r := ms.nextReq(t)
	if r.Method != "thread/start" {
		t.Fatalf("second req = %q", r.Method)
	}
	ms.reply(t, r.ID, threadStartResult)

	r = ms.nextReq(t)
	if r.Method != "turn/start" {
		t.Fatalf("third req = %q", r.Method)
	}
	// Verify the client sent the prompt in the documented input shape.
	if !strings.Contains(string(r.Params), `"text":"ping"`) {
		t.Fatalf("turn/start params missing prompt: %s", r.Params)
	}
	ms.reply(t, r.ID, `{"turn":{}}`)
	ms.note(t, "item/completed", `{"item":{"type":"agentMessage","text":"pong"}}`)
	ms.note(t, "thread/tokenUsage/updated", `{"tokenUsage":{"last":{"totalTokens":42,"inputTokens":30,"outputTokens":12},"modelContextWindow":1000}}`)
	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turn":{}}`)

	if err := <-done; err != nil {
		t.Fatalf("turn error: %v", err)
	}

	// ThreadInfo carries deterministic identity + effective (downgraded) policy.
	if info.ID != "THREAD-1" || info.Path != "/scrubbed/rollout.jsonl" || info.Model != "gpt-5.5" {
		t.Fatalf("thread info = %+v", info)
	}
	if info.EffectivePolicy != "on-request" {
		t.Fatalf("effective policy not read back: %q", info.EffectivePolicy)
	}
	if info.SandboxType != "workspaceWrite" {
		t.Fatalf("sandbox = %q", info.SandboxType)
	}

	// Events: the user turn is logged by the Manager (not the client), so the
	// client sink sees only the assistant "pong" and one usage with folded tokens.
	if u := col.byType("user"); len(u) != 0 {
		t.Fatalf("client should not emit user events (Manager owns that): %+v", u)
	}
	a := col.byType("assistant")
	if len(a) != 1 || a[0].Text != "pong" {
		t.Fatalf("assistant events = %+v", a)
	}
	usage := col.byType("usage")
	if len(usage) != 1 || usage[0].Usage.Used != 42 || usage[0].Usage.Size != 1000 {
		t.Fatalf("usage events = %+v", usage)
	}
}

func TestDeltaOnlyAssistantOutput(t *testing.T) {
	// Some server versions stream the full assistant answer as
	// item/agentMessage/delta fragments and emit an empty text field in the
	// item/completed notification. The client must accumulate the deltas and
	// surface the concatenated text as the assistant event (finding 76).
	c, ms, col := newClientWithMock(t)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, _ = c.Initialize(ctx, "test", "1")
		ti, _ := c.StartThread(ctx, StartThreadParams{Cwd: "/w"})
		done <- c.RunTurn(ctx, ti.ID, "hello")
	}()

	ms.reply(t, ms.nextReq(t).ID, `{}`)              // initialize
	ms.reply(t, ms.nextReq(t).ID, threadStartResult) // thread/start
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`)     // turn/start

	// Server streams deltas then sends a completed item with empty text.
	ms.note(t, "item/agentMessage/delta", `{"itemId":"msg-1","delta":"Hello"}`)
	ms.note(t, "item/agentMessage/delta", `{"itemId":"msg-1","delta":", world!"}`)
	ms.note(t, "item/completed", `{"item":{"type":"agentMessage","id":"msg-1","text":""}}`)
	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turn":{}}`)

	if err := <-done; err != nil {
		t.Fatalf("turn error: %v", err)
	}
	a := col.byType("assistant")
	if len(a) != 1 {
		t.Fatalf("want 1 assistant event, got %d: %+v", len(a), a)
	}
	if a[0].Text != "Hello, world!" {
		t.Fatalf("assistant text = %q, want %q", a[0].Text, "Hello, world!")
	}
}

func TestDeltaWithInlineTextPreservesInlineText(t *testing.T) {
	// When item/completed carries inline text, it takes precedence over buffered
	// deltas (the delta buffer is still flushed to prevent memory accumulation).
	c, ms, col := newClientWithMock(t)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, _ = c.Initialize(ctx, "test", "1")
		ti, _ := c.StartThread(ctx, StartThreadParams{Cwd: "/w"})
		done <- c.RunTurn(ctx, ti.ID, "hi")
	}()

	ms.reply(t, ms.nextReq(t).ID, `{}`)
	ms.reply(t, ms.nextReq(t).ID, threadStartResult)
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`)

	ms.note(t, "item/agentMessage/delta", `{"itemId":"msg-2","delta":"partial"}`)
	// Completed item has full text — delta is discarded.
	ms.note(t, "item/completed", `{"item":{"type":"agentMessage","id":"msg-2","text":"full response"}}`)
	ms.note(t, "turn/completed", `{}`)

	if err := <-done; err != nil {
		t.Fatalf("turn error: %v", err)
	}
	a := col.byType("assistant")
	if len(a) != 1 || a[0].Text != "full response" {
		t.Fatalf("assistant events = %+v", a)
	}
}

func TestTurnApprovalAccepted(t *testing.T) {
	c, ms, col := newClientWithMock(t)
	var gotApproval Approval
	var once sync.Once
	c.SetApprovalHandler(func(a Approval) (string, json.RawMessage, bool) {
		once.Do(func() { gotApproval = a })
		// Pick the first non-rejecting decision (accept).
		for _, d := range a.AvailableDecisions {
			if !d.IsRejection() {
				return d.Key, d.Payload, true
			}
		}
		return "", nil, false
	})
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		_, _ = c.Initialize(ctx, "test", "1")
		ti, err := c.StartThread(ctx, StartThreadParams{Cwd: "/w", ApprovalPolicy: "on-request"})
		if err != nil {
			done <- err
			return
		}
		done <- c.RunTurn(ctx, ti.ID, "fetch")
	}()

	ms.reply(t, ms.nextReq(t).ID, `{}`)              // initialize
	ms.reply(t, ms.nextReq(t).ID, threadStartResult) // thread/start
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`)     // turn/start
	ms.serverRequest(t, 0, "item/commandExecution/requestApproval", syntheticApproval)

	// Client answers the approval; assert it chose "accept" verbatim.
	dec := ms.nextResp(t)
	if !strings.Contains(string(dec.Result), `"decision":"accept"`) {
		t.Fatalf("decision result = %s", dec.Result)
	}
	ms.note(t, "serverRequest/resolved", `{"requestId":0}`)
	ms.note(t, "turn/completed", `{"turn":{}}`)

	if err := <-done; err != nil {
		t.Fatalf("turn error: %v", err)
	}
	if len(gotApproval.AvailableDecisions) != 3 {
		t.Fatalf("handler saw %d decisions", len(gotApproval.AvailableDecisions))
	}
	// The pending approval is logged as decision evidence.
	if tools := col.byType("tool"); len(tools) == 0 || tools[0].Tool.Kind != "approval" {
		t.Fatalf("approval not logged as tool evidence: %+v", tools)
	}
}

func TestTurnApprovalRejectedFailClosed(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	// No approval handler installed → fail-closed rejection.
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, _ = c.Initialize(ctx, "test", "1")
		ti, _ := c.StartThread(ctx, StartThreadParams{ApprovalPolicy: "on-request"})
		done <- c.RunTurn(ctx, ti.ID, "fetch")
	}()
	ms.reply(t, ms.nextReq(t).ID, `{}`)
	ms.reply(t, ms.nextReq(t).ID, threadStartResult)
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`)
	ms.serverRequest(t, 0, "item/commandExecution/requestApproval", syntheticApproval)

	dec := ms.nextResp(t)
	if dec.Result != nil || !strings.Contains(string(mustField(dec, "error")), "no approval handler") {
		t.Fatalf("expected fail-closed error response, got %+v", dec)
	}
	ms.note(t, "turn/completed", `{"turn":{}}`)
	if err := <-done; err != nil {
		t.Fatalf("turn error: %v", err)
	}
}

func TestUnknownServerRequestRejected(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	ctx := context.Background()
	go func() { _, _ = c.Initialize(ctx, "t", "1") }()
	ms.reply(t, ms.nextReq(t).ID, `{}`)
	// An unmodelled server request must get a clean JSON-RPC error, not a hang.
	ms.serverRequest(t, 5, "some/unknown/method", `{}`)
	dec := ms.nextResp(t)
	if !strings.Contains(string(mustField(dec, "error")), "unhandled server request") {
		t.Fatalf("unknown request not rejected: %+v", dec)
	}
}

func TestCurrentTimeRequestAnswered(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	ctx := context.Background()
	go func() { _, _ = c.Initialize(ctx, "t", "1") }()
	ms.reply(t, ms.nextReq(t).ID, `{}`)
	ms.serverRequest(t, 9, "currentTime/read", `{}`)
	dec := ms.nextResp(t)
	if !strings.Contains(string(dec.Result), "currentTime") {
		t.Fatalf("currentTime not answered: %+v", dec)
	}
}

func TestTurnFailedSignalsError(t *testing.T) {
	c, ms, col := newClientWithMock(t)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, _ = c.Initialize(ctx, "t", "1")
		ti, _ := c.StartThread(ctx, StartThreadParams{})
		done <- c.RunTurn(ctx, ti.ID, "x")
	}()
	ms.reply(t, ms.nextReq(t).ID, `{}`)
	ms.reply(t, ms.nextReq(t).ID, threadStartResult)
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`)
	ms.note(t, "turn/failed", `{"message":"boom"}`)

	err := <-done
	if err == nil || !strings.Contains(err.Error(), "turn failed") {
		t.Fatalf("want turn-failed error, got %v", err)
	}
	if len(col.byType("error")) != 1 {
		t.Fatal("error event not logged")
	}
}

func TestRunTurnRejectsConcurrentTurn(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	ctx := context.Background()
	// Bring up a thread, then start a turn that never completes.
	go func() { _, _ = c.Initialize(ctx, "t", "1") }()
	ms.reply(t, ms.nextReq(t).ID, `{}`)
	go func() { _, _ = c.StartThread(ctx, StartThreadParams{}) }()
	ms.reply(t, ms.nextReq(t).ID, threadStartResult)

	go func() { _ = c.RunTurn(ctx, "THREAD-1", "first") }()
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`) // turn/start accepted, no completion

	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.turnDone != nil
	})
	if err := c.RunTurn(ctx, "THREAD-1", "second"); err != ErrTurnActive {
		t.Fatalf("want ErrTurnActive, got %v", err)
	}
}

func TestRunTurnAfterCloseFails(t *testing.T) {
	c, _, _ := newClientWithMock(t)
	_ = c.Close()
	if err := c.RunTurn(context.Background(), "T", "x"); err != ErrClosed {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

func TestContextCancelUnblocksCall(t *testing.T) {
	c, _, _ := newClientWithMock(t)
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { _, err := c.Initialize(ctx, "t", "1"); res <- err }()
	cancel() // server never replies
	select {
	case err := <-res:
		if err != context.Canceled {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context cancel did not unblock the call")
	}
}

// TestSyntheticReplay drives the client against a committed synthetic fixture
// that pins the schema slice scimux actually consumes. It covers: numeric
// and string response ids, delta-only assistant output (finding 76), silently
// ignored unknown notification, and an approval with object-variant decisions
// (finding 66). The fixture is fully scrubbed — no real session content.
func TestSyntheticReplay(t *testing.T) {
	frames := loadFixture(t, "testdata/synthetic-session.ndjson")
	c, ms, col := newClientWithMock(t)
	// Accept the first non-rejecting decision (mirrors the normal supervisor path).
	c.SetApprovalHandler(func(a Approval) (string, json.RawMessage, bool) {
		for _, d := range a.AvailableDecisions {
			if !d.IsRejection() {
				return d.Key, d.Payload, true
			}
		}
		return "", nil, false
	})
	ctx := context.Background()

	done := make(chan error, 1)
	var info ThreadInfo
	go func() {
		if _, err := c.Initialize(ctx, "scimux", "1"); err != nil {
			done <- err
			return
		}
		ti, err := c.StartThread(ctx, StartThreadParams{Cwd: "/scrubbed"})
		if err != nil {
			done <- err
			return
		}
		info = ti
		done <- c.RunTurn(ctx, ti.ID, "ls /tmp")
	}()

	replay(t, ms, frames)

	if err := <-done; err != nil {
		t.Fatalf("synthetic replay turn error: %v", err)
	}
	if info.ID != "THREAD-SYN" || info.Path == "" || info.Model == "" {
		t.Fatalf("thread info incomplete: %+v", info)
	}
	// Deltas must be assembled: "Delta " + "assembled." → "Delta assembled."
	a := col.byType("assistant")
	if len(a) != 1 || a[0].Text != "Delta assembled." {
		t.Fatalf("assistant events = %+v", a)
	}
	// Token usage must be captured.
	if len(col.byType("usage")) != 1 {
		t.Fatal("no usage events from synthetic replay")
	}
	// Error events must not appear (unknown notification is silently ignored).
	if errs := col.byType("error"); len(errs) != 0 {
		t.Fatalf("unexpected error events: %+v", errs)
	}
}

// TestGoldenReplay drives the client against the exact server frames recorded
// from codex 0.144.1 (testdata/turn_with_approval.ndjson). This is the "mock
// built from monitored I/O": real bytes, deterministic assertions.
func TestGoldenReplay(t *testing.T) {
	frames := loadFixture(t, "testdata/real-turn-with-approval.ndjson")
	c, ms, col := newClientWithMock(t)
	c.SetApprovalHandler(func(a Approval) (string, json.RawMessage, bool) {
		return "cancel", nil, true // matches the recorded run (network denied)
	})
	ctx := context.Background()

	done := make(chan error, 1)
	var info ThreadInfo
	go func() {
		if _, err := c.Initialize(ctx, "scimux", "1"); err != nil {
			done <- err
			return
		}
		ti, err := c.StartThread(ctx, StartThreadParams{Cwd: "/w", ApprovalPolicy: "on-request"})
		if err != nil {
			done <- err
			return
		}
		info = ti
		done <- c.RunTurn(ctx, ti.ID, "Use curl to fetch https://example.com and show the first line.")
	}()

	replay(t, ms, frames)

	if err := <-done; err != nil {
		t.Fatalf("golden turn error: %v", err)
	}
	if info.ID == "" || info.Path == "" || info.Model == "" {
		t.Fatalf("golden thread info incomplete: %+v", info)
	}
	// The real agent produced a non-empty final answer and at least one usage.
	if a := col.byType("assistant"); len(a) == 0 || strings.TrimSpace(a[len(a)-1].Text) == "" {
		t.Fatalf("no assistant text from golden replay: %+v", a)
	}
	if len(col.byType("usage")) == 0 {
		t.Fatal("no usage events from golden replay")
	}
}

// replay feeds recorded server frames to the client, pairing each recorded
// response with the next client request and answering the recorded approval
// request from the client's own decision.
func replay(t *testing.T, ms *mockServer, frames []map[string]json.RawMessage) {
	t.Helper()
	for _, f := range frames {
		_, hasMethod := f["method"]
		_, hasID := f["id"]
		_, hasResult := f["result"]
		switch {
		case hasMethod && hasID:
			// Server request (approval): forward with a fresh id, await decision.
			ms.writeRaw(t, mustMarshal(f))
			_ = ms.nextResp(t)
		case hasMethod:
			// Notification: forward verbatim.
			ms.writeRaw(t, mustMarshal(f))
		case hasResult || hasID:
			// Response: pair with the next client request, echo with its id.
			req := ms.nextReq(t)
			out := map[string]json.RawMessage{"id": rawOf(req.ID), "result": f["result"]}
			ms.writeRaw(t, mustMarshal(out))
		}
	}
}

// --- fixture helpers ---

func loadFixture(t *testing.T, path string) []map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// The real-capture fixture is gitignored (personal env snapshot), so in
		// a fresh checkout it is absent. Skip rather than fail, matching how
		// scimux handles its gitignored real-*.jsonl fixtures.
		t.Skipf("golden fixture %s absent (gitignored real capture); skipping", path)
	}
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var out []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad fixture line: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func mustMarshal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func rawOf(p *json.RawMessage) json.RawMessage {
	if p == nil {
		return json.RawMessage("null")
	}
	return *p
}

func mustField(f clientFrame, key string) json.RawMessage {
	switch key {
	case "error":
		return f.Error
	case "result":
		return f.Result
	}
	return nil
}
