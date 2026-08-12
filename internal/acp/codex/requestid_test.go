package codex

import (
	"testing"
)

// TestPendingRequestIDStableAndAdvances: Codex projects pendingSeq as RequestID;
// it is stable while the head request is pending and advances for the next.
func TestPendingRequestIDStableAndAdvances(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	if err := m.Send("n1", "approve me"); err != nil {
		t.Fatal(err)
	}
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`)
	ms.serverRequest(t, 1, "item/commandExecution/requestApproval", syntheticApproval)
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })

	p1, ok := m.Pending("n1")
	if !ok || p1.RequestID == "" {
		t.Fatalf("pending = %+v ok=%v", p1, ok)
	}
	p1b, _ := m.Pending("n1")
	if p1b.RequestID != p1.RequestID {
		t.Fatalf("RequestID unstable: %q → %q", p1.RequestID, p1b.RequestID)
	}

	tokA, _, err := m.PrepareResolve("n1", "1")
	if err != nil {
		t.Fatal(err)
	}
	// Queue a second approval; after answering the first, head becomes the second.
	ms.serverRequest(t, 2, "item/fileChange/requestApproval",
		`{"threadId":"THREAD-1","turnId":"turn-1","itemId":"patch-1","startedAtMs":1}`)

	if err := m.Deliver("n1", tokA); err != nil {
		t.Fatalf("deliver first: %v", err)
	}
	_ = ms.nextResp(t)

	waitFor(t, func() bool {
		p, ok := m.Pending("n1")
		return ok && p.RequestID != "" && p.RequestID != p1.RequestID
	})
	p2, _ := m.Pending("n1")
	if p2.RequestID == p1.RequestID {
		t.Fatalf("second RequestID = %q, want different from first", p2.RequestID)
	}

	// Stale token for request A must not answer request B.
	if err := m.Deliver("n1", tokA); err == nil {
		t.Fatal("stale token must not deliver into the next request")
	}
	if _, ok := m.Pending("n1"); !ok {
		t.Fatal("request B must remain pending after stale deliver")
	}
}
