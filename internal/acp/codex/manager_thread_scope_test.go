package codex

import (
	"strings"
	"testing"
	"time"
)

func awaitManagerPending(t *testing.T, m *Manager, nodeID string) PendingPermission {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if pending, ok := m.Pending(nodeID); ok {
			return pending
		}
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for manager approval queue")
		case <-tick.C:
		}
	}
}

func TestManagerChildLifecycleKeepsParentTurnAndApproval(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	t.Cleanup(m.Shutdown)
	launch(t, m, ms, "n1", "", "")
	if err := m.Send("n1", "parent work"); err != nil {
		t.Fatal(err)
	}
	start := ms.nextReq(t)
	if start.Method != "turn/start" {
		t.Fatalf("request = %q, want turn/start", start.Method)
	}
	ms.reply(t, start.ID, `{"turn":{"id":"parent-turn"}}`)

	ms.serverRequest(t, 71, "item/commandExecution/requestApproval", syntheticApproval)
	pending := awaitManagerPending(t, m, "n1")
	if pending.RequestID == "" {
		t.Fatal("pending approval has no request identity")
	}

	for i, note := range []struct{ method, params string }{
		{"turn/started", `{"threadId":"CHILD-THREAD","turn":{"id":"child-turn"}}`},
		{"turn/completed", `{"threadId":"CHILD-THREAD","turn":{"id":"child-turn"}}`},
		{"turn/failed", `{"threadId":"CHILD-THREAD","turnId":"child-turn","message":"failed"}`},
		{"error", `{"threadId":"CHILD-THREAD","turnId":"child-turn","message":"failed"}`},
	} {
		ms.note(t, note.method, note.params)
		lifecycleBarrier(t, ms, 1700+i)
		if got := m.Live("n1"); got != "active" {
			t.Fatalf("live after child %s = %q, want active", note.method, got)
		}
		got, ok := m.Pending("n1")
		if !ok || got.RequestID != pending.RequestID {
			t.Fatalf("pending after child %s = %+v, %v; want %q", note.method, got, ok, pending.RequestID)
		}
	}

	if err := m.Interrupt("n1"); err != nil {
		t.Fatal(err)
	}
	interrupt := ms.nextReq(t)
	if interrupt.Method != "turn/interrupt" ||
		!strings.Contains(string(interrupt.Params), `"threadId":"THREAD-1"`) ||
		!strings.Contains(string(interrupt.Params), `"turnId":"parent-turn"`) {
		t.Fatalf("interrupt after child events = %q %s", interrupt.Method, interrupt.Params)
	}
	ms.reply(t, interrupt.ID, `{}`)
	ms.note(t, "turn/completed", `{"threadId":"THREAD-1","turnId":"parent-turn"}`)

	// The actual parent terminal edge ends the manager turn and cancels its
	// still-pending approval, preserving the existing cleanup behavior.
	decision := ms.nextResp(t)
	if len(decision.Error) == 0 {
		t.Fatalf("parent completion should reject pending approval, got %s", decision.Result)
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for m.Live("n1") != "quiet" || m.Attention("n1") != "" {
		select {
		case <-deadline.C:
			t.Fatalf("parent did not settle: live=%q attention=%q", m.Live("n1"), m.Attention("n1"))
		case <-tick.C:
		}
	}
}

func TestManagerMatchingParentFailureStillEndsTurn(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	t.Cleanup(m.Shutdown)
	launch(t, m, ms, "n1", "", "")
	if err := m.Send("n1", "parent work"); err != nil {
		t.Fatal(err)
	}
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{"id":"parent-turn"}}`)
	ms.note(t, "turn/failed", `{"threadId":"THREAD-1","turnId":"parent-turn","message":"failed"}`)

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for m.Live("n1") != "quiet" || !strings.Contains(m.LastError("n1"), "turn failed") {
		select {
		case <-deadline.C:
			t.Fatalf("parent failure did not settle: live=%q error=%q", m.Live("n1"), m.LastError("n1"))
		case <-tick.C:
		}
	}
}
