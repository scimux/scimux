package codex

import (
	"errors"
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

	tokA, _, err := m.PrepareResolve("n1", p1.RequestID, "1")
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

// TestPrepareResolveBindsExpectedRequestID: snapshot A, replace head with B
// before prepare, prepare with A's ID → ErrStalePermission and B stays pending.
// Current ID + valid key succeeds; current ID + invalid key is a client error
// that does not consume the request.
func TestPrepareResolveBindsExpectedRequestID(t *testing.T) {
	s := &Session{nodeID: "n1", incarn: "inc1", logw: &logWriter{Path: t.TempDir() + "/n1.jsonl"}}
	// Request A.
	s.pendingSeq++
	a := Approval{
		Method:  "item/commandExecution/requestApproval",
		Command: "go test",
		AvailableDecisions: []Decision{
			{Key: "accept", Payload: []byte(`"accept"`)},
			{Key: "decline", Payload: []byte(`"decline"`)},
		},
	}
	s.pending = append(s.pending, &pendingPermission{
		seq: s.pendingSeq, approval: a, ch: make(chan chosen, 1),
	})
	pA, ok := s.pendingInfo()
	if !ok || pA.RequestID == "" {
		t.Fatalf("pending A = %+v ok=%v", pA, ok)
	}

	// Replace A with identical-looking B (same decisions/title, new seq).
	s.pendingSeq++
	s.pending = []*pendingPermission{{
		seq: s.pendingSeq, approval: a, ch: make(chan chosen, 1),
	}}
	pB, ok := s.pendingInfo()
	if !ok || pB.RequestID == "" || pB.RequestID == pA.RequestID {
		t.Fatalf("pending B = %+v, want different from A %q", pB, pA.RequestID)
	}

	tok, _, err := s.prepareResolve(pA.RequestID, "1")
	if !errors.Is(err, ErrStalePermission) {
		t.Fatalf("prepare with A's id against B = tok %q err %v, want ErrStalePermission", tok, err)
	}
	if p, still := s.pendingInfo(); !still || p.RequestID != pB.RequestID {
		t.Fatalf("B must remain pending after stale prepare; got %+v ok=%v", p, still)
	}

	// Current ID + valid key succeeds.
	tokB, _, err := s.prepareResolve(pB.RequestID, "1")
	if err != nil {
		t.Fatalf("prepare current: %v", err)
	}
	// Current ID + invalid key fails without consuming.
	if _, _, err := s.prepareResolve(pB.RequestID, "9"); err == nil {
		t.Fatal("invalid key must fail")
	} else if errors.Is(err, ErrStalePermission) || errors.Is(err, ErrNoPending) {
		t.Fatalf("invalid key err = %v, want non-conflict client error", err)
	}
	if p, still := s.pendingInfo(); !still || p.RequestID != pB.RequestID {
		t.Fatalf("invalid key must leave B pending; got %+v ok=%v", p, still)
	}
	if err := s.deliver(tokB); err != nil {
		t.Fatalf("deliver B: %v", err)
	}
}

// TestPrepareDeliverRejectsForeignIncarnation: prepare under session A, replace
// with session B reusing seq/options, deliver A's token → refused; B pending.
func TestPrepareDeliverRejectsForeignIncarnation(t *testing.T) {
	path := t.TempDir() + "/n1.jsonl"
	sA := &Session{nodeID: "n1", incarn: "old-inc", logw: &logWriter{Path: path}}
	appr := Approval{
		Method:  "item/commandExecution/requestApproval",
		Command: "go test",
		AvailableDecisions: []Decision{
			{Key: "accept", Payload: []byte(`"accept"`)},
			{Key: "decline", Payload: []byte(`"decline"`)},
		},
	}
	sA.pendingSeq = 1
	sA.pending = []*pendingPermission{{seq: 1, approval: appr, ch: make(chan chosen, 1)}}
	pA, _ := sA.pendingInfo()
	tokA, _, err := sA.prepareResolve(pA.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}

	sB := &Session{nodeID: "n1", incarn: "new-inc", logw: &logWriter{Path: path}}
	sB.pendingSeq = 1
	sB.pending = []*pendingPermission{{seq: 1, approval: appr, ch: make(chan chosen, 1)}}
	if err := sB.deliver(tokA); err == nil {
		t.Fatal("token from prior incarnation must not deliver into a new session")
	}
	if _, ok := sB.pendingInfo(); !ok {
		t.Fatal("B must remain pending")
	}
	if _, _, err := sB.prepareResolve(pA.RequestID, "1"); !errors.Is(err, ErrStalePermission) {
		t.Fatalf("prepare with prior incarnation = %v, want ErrStalePermission", err)
	}
}

func TestPendingInfoWithNothingWaitingIsEmpty(t *testing.T) {
	s := &Session{nodeID: "n1"}
	p, ok := s.pendingInfo()
	if ok || p.RequestID != "" || p.Title != "" || len(p.Options) != 0 {
		t.Fatalf("empty pending = %+v ok=%v", p, ok)
	}
}
