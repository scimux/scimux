package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// lifecycleBarrier proves that every notification written before it has run
// through the client's single peer read loop. It avoids timing-based negative
// assertions when checking that a foreign event did not finish a parent turn.
func lifecycleBarrier(t *testing.T, ms *mockServer, id int) {
	t.Helper()
	ms.serverRequest(t, id, "currentTime/read", `{}`)
	resp := ms.nextResp(t)
	if resp.ID == nil {
		t.Fatal("barrier response has no id")
	}
	want, _ := json.Marshal(id)
	if string(*resp.ID) != string(want) {
		t.Fatalf("barrier response id = %s, want %s", *resp.ID, want)
	}
}

// startInitializedTurn drives the public initialization/thread/turn path and
// leaves one parent turn active with a server-assigned id.
func startInitializedTurn(t *testing.T, c *Client, ms *mockServer, ctx context.Context) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		if _, err := c.Initialize(ctx, "test", "1"); err != nil {
			done <- err
			return
		}
		info, err := c.StartThread(ctx, StartThreadParams{Cwd: "/workspace"})
		if err != nil {
			done <- err
			return
		}
		done <- c.RunTurn(ctx, info.ID, "parent work")
	}()

	ms.reply(t, ms.nextReq(t).ID, `{}`)
	ms.reply(t, ms.nextReq(t).ID, threadStartResult)
	start := ms.nextReq(t)
	if start.Method != "turn/start" {
		t.Fatalf("request = %q, want turn/start", start.Method)
	}
	ms.reply(t, start.ID, `{"turn":{"id":"parent-turn"}}`)
	return done
}

func awaitTurnResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for turn result")
		return nil
	}
}

func assertTurnRunning(t *testing.T, ms *mockServer, done <-chan error, barrierID int) {
	t.Helper()
	lifecycleBarrier(t, ms, barrierID)
	select {
	case err := <-done:
		t.Fatalf("turn finished unexpectedly: %v", err)
	default:
	}
}

func TestChildTurnCompletedDoesNotFinishParent(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	done := startInitializedTurn(t, c, ms, context.Background())

	ms.note(t, "turn/completed", `{"threadId":"CHILD-THREAD","turn":{"id":"child-turn"}}`)
	lifecycleBarrier(t, ms, 901)
	select {
	case err := <-done:
		t.Fatalf("child completion finished parent: %v", err)
	default:
	}

	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turn":{"id":"parent-turn"}}`)
	if err := awaitTurnResult(t, done); err != nil {
		t.Fatalf("parent completion: %v", err)
	}
}

func TestChildTurnStartedDoesNotRetargetParentInterrupt(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := startInitializedTurn(t, c, ms, ctx)

	ms.note(t, "turn/started", `{"threadId":"CHILD-THREAD","turn":{"id":"child-turn"}}`)
	lifecycleBarrier(t, ms, 902)
	cancel()

	interrupt := ms.nextReq(t)
	if interrupt.Method != "turn/interrupt" {
		t.Fatalf("request = %q, want turn/interrupt", interrupt.Method)
	}
	if !strings.Contains(string(interrupt.Params), `"threadId":"THREAD-1"`) ||
		!strings.Contains(string(interrupt.Params), `"turnId":"parent-turn"`) {
		t.Fatalf("interrupt retargeted by child start: %s", interrupt.Params)
	}
	ms.reply(t, interrupt.ID, `{}`)
	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turn":{"id":"parent-turn","status":"interrupted"}}`)
	if err := awaitTurnResult(t, done); err != context.Canceled {
		t.Fatalf("RunTurn = %v, want context canceled", err)
	}
}

func TestLifecycleIdentityDecodeBoundaries(t *testing.T) {
	tests := []struct {
		name                      string
		raw                       string
		valid, hasThread, hasTurn bool
		threadID, turnID          string
	}{
		{name: "absent", valid: true},
		{name: "scalar compatibility", raw: `"connection lost"`, valid: true},
		{name: "empty object", raw: `{}`, valid: true},
		{name: "thread only", raw: `{"threadId":"parent"}`, valid: true, hasThread: true, threadID: "parent"},
		{name: "top turn", raw: `{"turnId":"turn-1"}`, valid: true, hasTurn: true, turnID: "turn-1"},
		{name: "nested turn", raw: `{"turn":{"id":"turn-1"}}`, valid: true, hasTurn: true, turnID: "turn-1"},
		{name: "matching duplicate turn", raw: `{"turnId":"turn-1","turn":{"id":"turn-1"}}`, valid: true, hasTurn: true, turnID: "turn-1"},
		{name: "conflicting turn", raw: `{"turnId":"turn-1","turn":{"id":"turn-2"}}`},
		{name: "empty thread", raw: `{"threadId":""}`},
		{name: "non-string thread", raw: `{"threadId":7}`},
		{name: "empty turn", raw: `{"turnId":""}`},
		{name: "non-object turn", raw: `{"turn":"turn-1"}`},
		{name: "empty nested id", raw: `{"turn":{"id":""}}`},
		{name: "malformed json", raw: `{`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeLifecycleIdentity(json.RawMessage(tc.raw))
			if got.valid != tc.valid || (tc.valid && (got.hasThread != tc.hasThread || got.hasTurn != tc.hasTurn ||
				got.threadID != tc.threadID || got.turnID != tc.turnID)) {
				t.Fatalf("decode = %+v", got)
			}
		})
	}
}

func TestRunTurnRejectsExplicitNonParentThreadBeforeSend(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	initialized := make(chan error, 1)
	go func() {
		_, _ = c.Initialize(context.Background(), "test", "1")
		_, err := c.StartThread(context.Background(), StartThreadParams{Cwd: "/workspace"})
		initialized <- err
	}()
	ms.reply(t, ms.nextReq(t).ID, `{}`)
	ms.reply(t, ms.nextReq(t).ID, threadStartResult)
	if err := <-initialized; err != nil {
		t.Fatal(err)
	}
	if err := c.RunTurn(context.Background(), "CHILD-THREAD", "wrong owner"); err == nil || !strings.Contains(err.Error(), "current parent thread") {
		t.Fatalf("RunTurn foreign thread = %v", err)
	}
}

func TestForeignAndStaleLifecycleLeaveParentBuffersAndState(t *testing.T) {
	c, ms, col := newClientWithMock(t)
	done := startInitializedTurn(t, c, ms, context.Background())
	ms.note(t, "item/agentMessage/delta", `{"itemId":"parent-msg","delta":"parent text"}`)

	notes := []struct {
		method string
		params string
	}{
		{"turn/started", `{"threadId":"CHILD-THREAD","turn":{"id":"child-turn"}}`},
		{"turn/completed", `{"threadId":"CHILD-THREAD","turn":{"id":"child-turn"}}`},
		{"turn/failed", `{"threadId":"CHILD-THREAD","turn":{"id":"child-turn"},"message":"child failed"}`},
		{"error", `{"threadId":"CHILD-THREAD","turnId":"child-turn","message":"child error"}`},
		{"turn/started", `{"threadId":"THREAD-1","turn":{"id":"older-turn"}}`},
		{"turn/completed", `{"threadId":"THREAD-1","turn":{"id":"older-turn"}}`},
		{"turn/failed", `{"threadId":"THREAD-1","turnId":"older-turn"}`},
		{"error", `{"threadId":"THREAD-1","turnId":"older-turn","message":"stale"}`},
		{"turn/completed", `{"threadId":"CHILD-THREAD","turnId":"parent-turn"}`},
		{"turn/completed", `{"threadId":7,"turnId":"parent-turn"}`},
		{"turn/completed", `{"threadId":"THREAD-1","turnId":"parent-turn","turn":{"id":"other-turn"}}`},
	}
	for i, note := range notes {
		ms.note(t, note.method, note.params)
		assertTurnRunning(t, ms, done, 1000+i)
	}
	if errs := col.byType("error"); len(errs) != 0 {
		t.Fatalf("foreign or stale scoped errors reached parent sink: %+v", errs)
	}

	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turn":{"id":"parent-turn"}}`)
	if err := awaitTurnResult(t, done); err != nil {
		t.Fatalf("parent completion: %v", err)
	}
	assistant := col.byType("assistant")
	if len(assistant) != 1 || assistant[0].Text != "parent text" {
		t.Fatalf("parent buffers were cleared by foreign lifecycle: %+v", assistant)
	}
}

func TestMatchingParentFailureAndScopedErrorFailTurn(t *testing.T) {
	for _, tc := range []struct {
		name, method, params string
	}{
		{"failure", "turn/failed", `{"threadId":"THREAD-1","turn":{"id":"parent-turn"},"message":"failed"}`},
		{"scoped error", "error", `{"threadId":"THREAD-1","turnId":"parent-turn","message":"failed"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ms, col := newClientWithMock(t)
			done := startInitializedTurn(t, c, ms, context.Background())
			ms.note(t, tc.method, tc.params)
			if err := awaitTurnResult(t, done); err == nil || !strings.Contains(err.Error(), "turn failed") {
				t.Fatalf("RunTurn = %v", err)
			}
			if errs := col.byType("error"); len(errs) != 1 {
				t.Fatalf("error events = %+v", errs)
			}
		})
	}
}

func TestIdentitylessLifecycleCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, method, params string
		wantErr              bool
	}{
		{"completion empty object", "turn/completed", `{}`, false},
		{"completion empty turn", "turn/completed", `{"turn":{}}`, false},
		{"failure scalar", "turn/failed", `"rate limit"`, true},
		{"connection wide error", "error", `{"message":"connection lost"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ms, _ := newClientWithMock(t)
			done := startInitializedTurn(t, c, ms, context.Background())
			ms.note(t, tc.method, tc.params)
			err := awaitTurnResult(t, done)
			if (err != nil) != tc.wantErr {
				t.Fatalf("RunTurn error = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestParentCompletionBeforeTurnStartResponseRetiresInvocation(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	ctx := context.Background()
	initialized := make(chan ThreadInfo, 1)
	go func() {
		_, _ = c.Initialize(ctx, "test", "1")
		info, _ := c.StartThread(ctx, StartThreadParams{Cwd: "/workspace"})
		initialized <- info
	}()
	ms.reply(t, ms.nextReq(t).ID, `{}`)
	ms.reply(t, ms.nextReq(t).ID, threadStartResult)
	info := <-initialized

	done := make(chan error, 1)
	go func() { done <- c.RunTurn(ctx, info.ID, "parent work") }()
	start := ms.nextReq(t)
	ms.note(t, "turn/completed", `{"threadId":"CHILD-THREAD","turn":{"id":"child-turn"}}`)
	assertTurnRunning(t, ms, done, 1201)
	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turn":{"id":"parent-turn"}}`)
	if err := awaitTurnResult(t, done); err != nil {
		t.Fatalf("completion before response: %v", err)
	}

	// The response belongs to the ended invocation and cannot repopulate it.
	ms.reply(t, start.ID, `{"turn":{"id":"late-parent-turn"}}`)
	lifecycleBarrier(t, ms, 1202)
	c.mu.Lock()
	turnID, turnDone := c.curTurnID, c.turnDone
	c.mu.Unlock()
	if turnID != "" || turnDone != nil {
		t.Fatalf("late response repopulated ended invocation: id=%q done=%v", turnID, turnDone != nil)
	}
}

func TestOldCompletionBeforeNewStartResponseDoesNotRetireInvocation(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	first := startInitializedTurn(t, c, ms, context.Background())
	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turnId":"parent-turn"}`)
	if err := awaitTurnResult(t, first); err != nil {
		t.Fatal(err)
	}

	next := make(chan error, 1)
	go func() { next <- c.RunTurn(context.Background(), "THREAD-1", "next turn") }()
	start := ms.nextReq(t)
	if start.Method != "turn/start" {
		t.Fatalf("request = %q, want turn/start", start.Method)
	}

	// A delayed duplicate from the previous turn arrives while the next turn's
	// response is pending. Its explicit id is owned by the prior invocation.
	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turnId":"parent-turn"}`)
	assertTurnRunning(t, ms, next, 1251)

	ms.reply(t, start.ID, `{"turn":{"id":"next-turn"}}`)
	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turnId":"next-turn"}`)
	if err := awaitTurnResult(t, next); err != nil {
		t.Fatal(err)
	}
}

func TestUnseenRetiredTurnCannotClaimNextInvocation(t *testing.T) {
	oldSettle := interruptSettle
	interruptSettle = 10 * time.Millisecond
	defer func() { interruptSettle = oldSettle }()

	c, ms, _ := newClientWithMock(t)
	initialized := make(chan ThreadInfo, 1)
	go func() {
		info, _ := c.StartThread(context.Background(), StartThreadParams{Cwd: "/workspace"})
		initialized <- info
	}()
	ms.reply(t, ms.nextReq(t).ID, threadStartResult)
	info := <-initialized

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- c.RunTurn(ctx, info.ID, "first turn") }()
	startA := ms.nextReq(t)

	// The cancellation settles without either a response or lifecycle event, so
	// invocation A's server turn id is still unknown when invocation B begins.
	cancel()
	if err := awaitTurnResult(t, first); !errors.Is(err, context.Canceled) {
		t.Fatalf("first RunTurn = %v, want canceled", err)
	}

	next := make(chan error, 1)
	go func() { next <- c.RunTurn(context.Background(), info.ID, "next turn") }()
	startB := ms.nextReq(t)

	// A's first explicit lifecycle event cannot claim B while A's turn/start
	// response remains unresolved.
	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turnId":"previous-unseen-turn"}`)
	assertTurnRunning(t, ms, next, 1253)

	ms.reply(t, startA.ID, `{"turn":{"id":"previous-unseen-turn"}}`)
	ms.reply(t, startB.ID, `{"turn":{"id":"next-turn"}}`)
	assertTurnRunning(t, ms, next, 1254)
	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turnId":"next-turn"}`)
	if err := awaitTurnResult(t, next); err != nil {
		t.Fatal(err)
	}
}

func TestAmbiguousTerminalReplayedAfterStartResponses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		method  string
		params  string
		wantErr bool
	}{
		{
			name:   "completion",
			method: "turn/completed",
			params: `{"threadId":"THREAD-1","turnId":"next-turn"}`,
		},
		{
			name:    "failure",
			method:  "turn/failed",
			params:  `{"threadId":"THREAD-1","turnId":"next-turn","message":"synthetic failure"}`,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldSettle := interruptSettle
			interruptSettle = 10 * time.Millisecond
			defer func() { interruptSettle = oldSettle }()

			c, ms, col := newClientWithMock(t)
			initialized := make(chan ThreadInfo, 1)
			go func() {
				info, _ := c.StartThread(context.Background(), StartThreadParams{Cwd: "/workspace"})
				initialized <- info
			}()
			ms.reply(t, ms.nextReq(t).ID, threadStartResult)
			info := <-initialized

			ctx, cancel := context.WithCancel(context.Background())
			first := make(chan error, 1)
			go func() { first <- c.RunTurn(ctx, info.ID, "first turn") }()
			startA := ms.nextReq(t)
			cancel()
			if err := awaitTurnResult(t, first); !errors.Is(err, context.Canceled) {
				t.Fatalf("first RunTurn = %v, want canceled", err)
			}

			next := make(chan error, 1)
			go func() { next <- c.RunTurn(context.Background(), info.ID, "next turn") }()
			startB := ms.nextReq(t)

			// B's terminal edge arrives while A's response still makes its turn id
			// ambiguous. The responses must resolve and replay it without requiring
			// a duplicate notification from the server.
			ms.note(t, tc.method, tc.params)
			assertTurnRunning(t, ms, next, 1260)
			ms.reply(t, startA.ID, `{"turn":{"id":"previous-unseen-turn"}}`)
			ms.reply(t, startB.ID, `{"turn":{"id":"next-turn"}}`)

			err := awaitTurnResult(t, next)
			if (err != nil) != tc.wantErr {
				t.Fatalf("second RunTurn = %v, wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				if !strings.Contains(err.Error(), "synthetic failure") {
					t.Fatalf("second RunTurn error = %v", err)
				}
				if events := col.byType("error"); len(events) != 1 {
					t.Fatalf("error events = %+v, want one", events)
				}
			} else if events := col.byType("error"); len(events) != 0 {
				t.Fatalf("completion emitted errors: %+v", events)
			}
		})
	}
}

func TestDeferredTerminalOwnershipResolution(t *testing.T) {
	for _, method := range []string{"turn/completed", "turn/failed", "error"} {
		for _, evidence := range []string{"older-response", "current-response", "started", "terminal"} {
			t.Run(method+"/"+evidence, func(t *testing.T) {
				oldSettle := interruptSettle
				interruptSettle = 10 * time.Millisecond
				defer func() { interruptSettle = oldSettle }()
				c, ms, col := newClientWithMock(t)
				initialized := make(chan error, 1)
				go func() {
					_, err := c.StartThread(context.Background(), StartThreadParams{Cwd: "/workspace"})
					initialized <- err
				}()
				ms.reply(t, ms.nextReq(t).ID, threadStartResult)
				if err := <-initialized; err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				first := make(chan error, 1)
				go func() { first <- c.RunTurn(ctx, "THREAD-1", "first") }()
				startA := ms.nextReq(t)
				cancel()
				if err := awaitTurnResult(t, first); !errors.Is(err, context.Canceled) {
					t.Fatalf("first turn: %v", err)
				}
				next := make(chan error, 1)
				go func() { next <- c.RunTurn(context.Background(), "THREAD-1", "next") }()
				startB := ms.nextReq(t)
				ms.note(t, "item/agentMessage/delta", `{"itemId":"answer","delta":"synthetic answer"}`)
				ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turnId":"old-turn"}`)
				ms.note(t, method, `{"threadId":"THREAD-1","turnId":"next-turn","message":"first terminal"}`)
				// Contradictory duplicates must never replace the first terminal.
				ms.note(t, "turn/failed", `{"threadId":"THREAD-1","turnId":"next-turn","message":"duplicate"}`)
				if evidence == "started" || evidence == "terminal" {
					// Multiple unknown candidates cannot be resolved by map iteration.
					ms.note(t, "turn/failed", `{"threadId":"THREAD-1","turnId":"other-turn"}`)
				}
				lifecycleBarrier(t, ms, 1601)
				c.mu.Lock()
				premature := c.turnTerminal
				c.mu.Unlock()
				if premature {
					t.Fatal("ambiguous notification retired the active turn")
				}
				switch evidence {
				case "older-response", "started", "terminal":
					ms.reply(t, startA.ID, `{"turn":{"id":"old-turn"}}`)
					if evidence == "started" || evidence == "terminal" {
						lifecycleBarrier(t, ms, 1602)
						c.mu.Lock()
						premature = c.turnTerminal
						c.mu.Unlock()
						if premature {
							t.Fatal("picked an arbitrary terminal from multiple candidates")
						}
						if evidence == "started" {
							ms.note(t, "turn/started", `{"threadId":"THREAD-1","turnId":"next-turn"}`)
						} else {
							ms.note(t, "turn/failed", `{"threadId":"THREAD-1","turnId":"next-turn","message":"later terminal"}`)
						}
					}
				case "current-response":
					ms.reply(t, startB.ID, `{"turn":{"id":"next-turn"}}`)
				}
				lifecycleBarrier(t, ms, 1603)
				c.mu.Lock()
				retired := c.turnTerminal
				c.mu.Unlock()
				// Settle outstanding calls even on a regression, without hiding it.
				if evidence == "current-response" {
					ms.reply(t, startA.ID, `{"turn":{"id":"old-turn"}}`)
				} else {
					ms.reply(t, startB.ID, `{"turn":{"id":"next-turn"}}`)
				}
				err := awaitTurnResult(t, next)
				if !retired {
					t.Error("ownership became unambiguous but retained terminal was not delivered")
				}
				wantErr := method != "turn/completed"
				if (err != nil) != wantErr || (wantErr && !strings.Contains(err.Error(), "first terminal")) {
					t.Fatalf("terminal result = %v, method=%s", err, method)
				}
				wantAssistant, wantErrors := 1, 0
				if wantErr {
					wantAssistant, wantErrors = 0, 1
				}
				if got := len(col.byType("assistant")); got != wantAssistant {
					t.Errorf("assistant flush count = %d, failure=%v", got, wantErr)
				}
				if got := len(col.byType("error")); got != wantErrors {
					t.Errorf("error count = %d, failure=%v", got, wantErr)
				}
			})
		}
	}
}

func TestDeferredTerminalWaitsForEveryOlderStart(t *testing.T) {
	c := &Client{
		turnSeq:          3,
		turnThreadID:     "THREAD-1",
		turnDone:         make(chan turnResult, 1),
		unresolvedStarts: map[uint64]string{1: "THREAD-1", 2: "THREAD-1", 3: "THREAD-1"},
	}
	c.onNotify("turn/completed", json.RawMessage(`{"threadId":"THREAD-1","turnId":"current"}`))
	c.setTurnID(1, "old-one")
	if c.turnTerminal {
		t.Fatal("resolved ownership while a second older start was still pending")
	}
	// An older rejected start (no server id) removes its ambiguity too.
	c.setTurnID(2, "")
	if !c.turnTerminal {
		t.Fatal("did not replay after the last older start resolved")
	}
	if len(c.pendingTerminals) != 0 {
		t.Fatal("terminal left retained notifications behind")
	}
}

func TestDeferredTerminalClearedWhenInvocationAbandoned(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%t", shutdown), func(t *testing.T) {
			oldSettle := interruptSettle
			interruptSettle = 10 * time.Millisecond
			defer func() { interruptSettle = oldSettle }()
			c, ms, _ := newClientWithMock(t)
			// An earlier completed invocation still has an outstanding response.
			first := make(chan error, 1)
			go func() { first <- c.RunTurn(context.Background(), "THREAD-1", "first") }()
			startA := ms.nextReq(t)
			ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turnId":"old-turn"}`)
			if err := awaitTurnResult(t, first); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			next := make(chan error, 1)
			go func() { next <- c.RunTurn(ctx, "THREAD-1", "next") }()
			startB := ms.nextReq(t)
			ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turnId":"next-turn"}`)
			lifecycleBarrier(t, ms, 1701)
			c.mu.Lock()
			retained := len(c.pendingTerminals)
			c.mu.Unlock()
			if retained != 1 {
				t.Fatalf("retained count = %d, want 1", retained)
			}
			if shutdown {
				_ = c.t.Close()
			} else {
				cancel()
			}
			// On transport closure, a retained completion may win the race
			// with failure of the pending RPC. Either outcome must clean up.
			if err := awaitTurnResult(t, next); !shutdown && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled turn returned %v", err)
			}
			c.mu.Lock()
			retained = len(c.pendingTerminals)
			c.mu.Unlock()
			if retained != 0 {
				t.Fatal("abandoned invocation leaked retained terminals")
			}
			if !shutdown {
				ms.reply(t, startA.ID, `{"turn":{"id":"old-turn"}}`)
				ms.reply(t, startB.ID, `{"turn":{"id":"next-turn"}}`)
				lifecycleBarrier(t, ms, 1702)
			}
		})
	}
}

func TestOldStartedBeforeNewStartResponseDoesNotRetargetInterrupt(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	first := startInitializedTurn(t, c, ms, context.Background())
	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turnId":"parent-turn"}`)
	if err := awaitTurnResult(t, first); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	next := make(chan error, 1)
	go func() { next <- c.RunTurn(ctx, "THREAD-1", "next turn") }()
	start := ms.nextReq(t)
	if start.Method != "turn/start" {
		t.Fatalf("request = %q, want turn/start", start.Method)
	}

	ms.note(t, "turn/started", `{"threadId":"THREAD-1","turnId":"parent-turn"}`)
	lifecycleBarrier(t, ms, 1252)
	ms.reply(t, start.ID, `{"turn":{"id":"next-turn"}}`)
	cancel()

	interrupt := ms.nextReq(t)
	if interrupt.Method != "turn/interrupt" ||
		!strings.Contains(string(interrupt.Params), `"turnId":"next-turn"`) {
		t.Fatalf("interrupt = %q %s, want next-turn", interrupt.Method, interrupt.Params)
	}
	ms.reply(t, interrupt.ID, `{}`)
	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turnId":"next-turn"}`)
	if err := awaitTurnResult(t, next); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTurn = %v, want canceled", err)
	}
}

func TestDelayedResponseCannotOverwriteNewInvocation(t *testing.T) {
	c, _, _ := newClientWithMock(t)
	c.mu.Lock()
	c.turnSeq = 2
	c.turnDone = make(chan turnResult, 1)
	c.turnThreadID = "THREAD-1"
	c.curTurnID = "new-turn"
	c.turnIDReady = make(chan struct{})
	c.mu.Unlock()

	c.setTurnID(1, "late-old-turn")
	c.mu.Lock()
	got := c.curTurnID
	c.mu.Unlock()
	if got != "new-turn" {
		t.Fatalf("stale invocation response changed turn id to %q", got)
	}

	id := decodeLifecycleIdentity(json.RawMessage(`{"threadId":"THREAD-1","turnId":"new-turn"}`))
	if _, _, ok := c.finishNotifiedTurn(id); !ok {
		t.Fatal("new invocation completion was not accepted")
	}
	c.setTurnID(2, "late-after-completion")
	c.mu.Lock()
	got = c.curTurnID
	c.mu.Unlock()
	if got != "" {
		t.Fatalf("ended invocation response repopulated turn id %q", got)
	}
}

func TestInterruptTurnIgnoresRetiredAndUnaddressableInvocation(t *testing.T) {
	c, _, _ := newClientWithMock(t)
	c.mu.Lock()
	c.turnSeq = 2
	c.turnDone = make(chan turnResult, 1)
	c.turnThreadID = "THREAD-1"
	c.turnTerminal = true
	c.mu.Unlock()
	c.interruptTurn(2)

	ready := make(chan struct{})
	close(ready)
	c.mu.Lock()
	c.turnSeq = 3
	c.turnTerminal = false
	c.curTurnID = ""
	c.turnIDReady = ready
	c.mu.Unlock()
	c.interruptTurn(3)

	c.mu.Lock()
	turnID := c.curTurnID
	c.mu.Unlock()
	if turnID != "" {
		t.Fatalf("unaddressable interrupt invented turn id %q", turnID)
	}
}

func TestDuplicateTerminalNotificationAndTransportClosure(t *testing.T) {
	t.Run("duplicate", func(t *testing.T) {
		c, ms, col := newClientWithMock(t)
		done := startInitializedTurn(t, c, ms, context.Background())
		ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turnId":"parent-turn"}`)
		if err := awaitTurnResult(t, done); err != nil {
			t.Fatal(err)
		}
		ms.note(t, "turn/failed", `{"threadId":"THREAD-1","turnId":"parent-turn"}`)
		lifecycleBarrier(t, ms, 1301)
		if errs := col.byType("error"); len(errs) != 0 {
			t.Fatalf("duplicate terminal emitted an error: %+v", errs)
		}
	})

	t.Run("transport closure", func(t *testing.T) {
		mt, ms := newMockTransport()
		c := NewClient(mt, nil, nil)
		done := startInitializedTurn(t, c, ms, context.Background())
		// The turn/start response has to be in the client before the peer
		// dies, or the closure is reported from the start wait instead of
		// the running turn.
		lifecycleBarrier(t, ms, 1302)
		if err := mt.Close(); err != nil {
			t.Fatal(err)
		}
		err := awaitTurnResult(t, done)
		if err == nil || !strings.Contains(err.Error(), "peer closed") {
			t.Fatalf("RunTurn = %v, want peer closure", err)
		}
		select {
		case <-c.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("client Done did not close")
		}
		c.mu.Lock()
		active := c.turnDone != nil || c.curTurnID != ""
		c.mu.Unlock()
		if active {
			t.Fatal("transport closure left active turn state")
		}
	})
}

func TestConnectionWideErrorWhileIdleIsStillReported(t *testing.T) {
	_, ms, col := newClientWithMock(t)
	ms.note(t, "error", `{"message":"connection lost"}`)
	lifecycleBarrier(t, ms, 1401)
	if errs := col.byType("error"); len(errs) != 1 {
		t.Fatalf("connection-wide errors = %+v", errs)
	}
}

func TestCancelBeforeTurnStartResponseIgnoresLateResponse(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	ctx, cancel := context.WithCancel(context.Background())
	initialized := make(chan ThreadInfo, 1)
	go func() {
		_, _ = c.Initialize(context.Background(), "test", "1")
		info, _ := c.StartThread(context.Background(), StartThreadParams{Cwd: "/workspace"})
		initialized <- info
	}()
	ms.reply(t, ms.nextReq(t).ID, `{}`)
	ms.reply(t, ms.nextReq(t).ID, threadStartResult)
	info := <-initialized
	done := make(chan error, 1)
	go func() { done <- c.RunTurn(ctx, info.ID, "cancel me") }()
	start := ms.nextReq(t)

	// A matching started notification gives interrupt an address while the
	// turn/start response itself remains delayed.
	ms.note(t, "turn/started", `{"threadId":"THREAD-1","turnId":"parent-turn"}`)
	lifecycleBarrier(t, ms, 1501)
	cancel()
	interrupt := ms.nextReq(t)
	if interrupt.Method != "turn/interrupt" || !strings.Contains(string(interrupt.Params), `"turnId":"parent-turn"`) {
		t.Fatalf("interrupt = %q %s", interrupt.Method, interrupt.Params)
	}
	ms.reply(t, interrupt.ID, `{}`)
	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turnId":"parent-turn"}`)
	if err := awaitTurnResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTurn = %v, want canceled", err)
	}
	ms.reply(t, start.ID, `{"turn":{"id":"late-turn"}}`)
	lifecycleBarrier(t, ms, 1502)
	c.mu.Lock()
	got := c.curTurnID
	c.mu.Unlock()
	if got != "" {
		t.Fatalf("late canceled response repopulated turn id %q", got)
	}
}
