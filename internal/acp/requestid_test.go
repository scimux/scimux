package acp

import (
	"errors"
	"path/filepath"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
)

func acpPermOpts() []sdk.PermissionOption {
	return []sdk.PermissionOption{
		{OptionId: "opt_allow", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce},
		{OptionId: "opt_reject", Name: "Reject", Kind: sdk.PermissionOptionKindRejectOnce},
	}
}

func setACPPending(s *Session, title string) {
	if s.incarn == "" {
		s.incarn = newSessionIncarn()
	}
	s.pendingSeq++
	s.pending = &pendingPermission{
		seq: s.pendingSeq, toolTitle: title, toolKind: "execute",
		options: acpPermOpts(), ch: make(chan sdk.PermissionOptionId, 1),
	}
}

// TestPendingRequestIDStableAndAdvances: each permission request gets a stable
// opaque RequestID while pending; the next request gets a different ID even
// with identical title/options. PrepareResolve tokens pin delivery to that
// request so a replacement cannot be answered with a stale prepare.
func TestPendingRequestIDStableAndAdvances(t *testing.T) {
	s := &Session{nodeID: "n1", logw: &logWriter{Path: filepath.Join(t.TempDir(), "n1.jsonl")}}
	setACPPending(s, "run bash")
	p1, ok := s.pendingInfo()
	if !ok || p1.RequestID == "" {
		t.Fatalf("pending = %+v ok=%v, want non-empty RequestID", p1, ok)
	}
	// Stable while same request remains.
	p1b, _ := s.pendingInfo()
	if p1b.RequestID != p1.RequestID {
		t.Fatalf("RequestID changed while pending: %q → %q", p1.RequestID, p1b.RequestID)
	}

	// Prepare against request A with its expected ID.
	tokA, _, err := s.prepareResolve(p1.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}

	// Replace with a new identical-looking request B.
	setACPPending(s, "run bash")
	p2, ok := s.pendingInfo()
	if !ok || p2.RequestID == "" || p2.RequestID == p1.RequestID {
		t.Fatalf("second RequestID = %q, want different from %q", p2.RequestID, p1.RequestID)
	}

	// Stale token for A must not deliver into B.
	if err := s.deliver(tokA); err == nil {
		t.Fatal("stale PrepareResolve token must not deliver into a replacement request")
	}
	// B still pending.
	if _, ok := s.pendingInfo(); !ok {
		t.Fatal("pending B should remain after refused stale deliver")
	}
	// Fresh prepare for B succeeds.
	tokB, _, err := s.prepareResolve(p2.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.deliver(tokB); err != nil {
		t.Fatalf("deliver B: %v", err)
	}
	if _, ok := s.pendingInfo(); ok {
		t.Fatal("pending should clear after successful deliver")
	}
}

// TestPrepareResolveBindsExpectedRequestID: request A is replaced by an
// identical-looking B before prepare. Preparing with A's ID must refuse and
// leave B pending; preparing with B's ID and a valid key succeeds; a valid
// request ID with an invalid key is a client error and does not consume B.
func TestPrepareResolveBindsExpectedRequestID(t *testing.T) {
	s := &Session{nodeID: "n1", logw: &logWriter{Path: filepath.Join(t.TempDir(), "n1.jsonl")}}
	setACPPending(s, "run bash")
	pA, ok := s.pendingInfo()
	if !ok || pA.RequestID == "" {
		t.Fatalf("pending A = %+v ok=%v", pA, ok)
	}

	// Replace A with identical-looking B before any prepare.
	setACPPending(s, "run bash")
	pB, ok := s.pendingInfo()
	if !ok || pB.RequestID == "" || pB.RequestID == pA.RequestID {
		t.Fatalf("pending B = %+v, want different from A %q", pB, pA.RequestID)
	}

	tok, _, err := s.prepareResolve(pA.RequestID, "1")
	if !errors.Is(err, ErrStalePermission) {
		t.Fatalf("prepare with A's id against B = tok %q err %v, want ErrStalePermission", tok, err)
	}
	if _, still := s.pendingInfo(); !still {
		t.Fatal("B must remain pending after refused stale prepare")
	}
	// B's ID still present and unchanged.
	pB2, _ := s.pendingInfo()
	if pB2.RequestID != pB.RequestID {
		t.Fatalf("B RequestID changed after refused prepare: %q → %q", pB.RequestID, pB2.RequestID)
	}

	// Current request ID + valid key succeeds.
	tokB, _, err := s.prepareResolve(pB.RequestID, "1")
	if err != nil {
		t.Fatalf("prepare current id: %v", err)
	}
	if tokB == "" {
		t.Fatal("empty prepare token")
	}
	// Current request ID + invalid key is a client error and does not consume.
	if _, _, err := s.prepareResolve(pB.RequestID, "9"); err == nil {
		t.Fatal("invalid key must fail")
	} else if errors.Is(err, ErrStalePermission) || errors.Is(err, ErrNoPending) {
		t.Fatalf("invalid key err = %v, want non-conflict client error", err)
	}
	if p, still := s.pendingInfo(); !still || p.RequestID != pB.RequestID {
		t.Fatalf("invalid key must leave B pending; got %+v ok=%v", p, still)
	}
	// Delivery of the good token still works.
	if err := s.deliver(tokB); err != nil {
		t.Fatalf("deliver B: %v", err)
	}
}

// TestPrepareDeliverRejectsForeignIncarnation: a prepared token from session
// A must not deliver into a replacement session B that reused the same
// sequence and options (post-/clear or kill/relaunch under the same node id).
func TestPrepareDeliverRejectsForeignIncarnation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "n1.jsonl")
	sA := &Session{nodeID: "n1", incarn: "inc-old", logw: &logWriter{Path: logPath}}
	setACPPending(sA, "run bash")
	pA, _ := sA.pendingInfo()
	tokA, _, err := sA.prepareResolve(pA.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}

	// Replace with a new Session value that reuses seq=1 under a new incarnation
	// (models /clear constructing a fresh Session under the same node id).
	sB := &Session{nodeID: "n1", incarn: "inc-new", logw: &logWriter{Path: logPath}}
	sB.pendingSeq = 1
	sB.pending = &pendingPermission{
		seq: 1, toolTitle: "run bash", toolKind: "execute",
		options: acpPermOpts(), ch: make(chan sdk.PermissionOptionId, 1),
	}
	if err := sB.deliver(tokA); err == nil {
		t.Fatal("token from prior incarnation must not deliver into a new session")
	}
	if _, ok := sB.pendingInfo(); !ok {
		t.Fatal("B must remain pending after refused foreign-incarnation deliver")
	}
	// Stale expected-ID prepare also fails.
	if _, _, err := sB.prepareResolve(pA.RequestID, "1"); !errors.Is(err, ErrStalePermission) {
		t.Fatalf("prepare with prior incarnation id = %v, want ErrStalePermission", err)
	}
	pB, _ := sB.pendingInfo()
	if pB.RequestID == pA.RequestID {
		t.Fatal("RequestID must differ across incarnations even with seq=1")
	}
}
