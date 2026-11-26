package app

import (
	"os"
	"testing"
)

// Copilot queues concurrent permission requests and shows them one at a time
// in admission order. Auto-approve's cutoff is the admission sequence, so a
// request admitted before the feature was enabled must stay manual even after
// it is promoted to the shown request, while one admitted afterwards is
// eligible. The stub is the manager the policy reads; these are the same
// Pending and boundary answers the copilot queue gives.
func TestCopilotPromotedPreEnableRequestStaysManual(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "cop-promoted", "copilot", "acp")
	// Admitted as sequence 2 while sequence 1 was shown; enable happens with
	// the boundary at 2, so sequence 2 is pre-enable whenever it is promoted.
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending:     allowPending("inc:2"),
		boundarySet: true, boundaryIncarn: "inc", boundaryMaxSeq: 2,
	}
	if v := a.enableAutoApprove(n.ID, stub); v.Phase != "armed" {
		t.Fatalf("enable = %+v", v)
	}
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 0 || stub.prepareCalls != 0 {
		t.Fatalf("promoted pre-enable request was touched: prepare=%d deliver=%d", stub.prepareCalls, stub.deliverCalls)
	}
	// A request admitted after enable is eligible when it is shown.
	stub.pending = allowPending("inc:3")
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 1 {
		t.Fatalf("post-enable request deliver calls = %d, want 1", stub.deliverCalls)
	}
}

// An audit write that fails for a shown copilot request must not deliver, and
// must not resolve the request behind it: the shown request stays for manual
// handling and the queue is left as it was.
func TestCopilotAuditFailureOnShownRequestLeavesQueueUntouched(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "cop-audit", "copilot", "acp")
	path := a.sessionLogPath(n.ID)
	_ = os.Remove(path)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	stub := &stubProc{
		live: "active", hasSession: true, hasPending: true,
		pending: allowPending("inc:4"),
	}
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
	a.maybeAutoApprove(n, stub)
	if stub.deliverCalls != 0 {
		t.Fatalf("deliver called %d times after an audit failure", stub.deliverCalls)
	}
	if stub.prepareCalls != 1 {
		t.Fatalf("prepare calls = %d, want the one attempt", stub.prepareCalls)
	}
	if !stub.hasPending || stub.pending.RequestID != "inc:4" {
		t.Fatalf("shown request changed after an audit failure: %+v", stub.pending)
	}
	// The poller must not retry the same request on every tick.
	a.maybeAutoApprove(n, stub)
	if stub.prepareCalls != 1 || stub.deliverCalls != 0 {
		t.Fatalf("retry after audit failure: prepare=%d deliver=%d", stub.prepareCalls, stub.deliverCalls)
	}
}
