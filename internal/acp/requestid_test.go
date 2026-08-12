package acp

import (
	"path/filepath"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
)

// TestPendingRequestIDStableAndAdvances: each permission request gets a stable
// opaque RequestID while pending; the next request gets a different ID even
// with identical title/options. PrepareResolve tokens pin delivery to that
// request so a replacement cannot be answered with a stale prepare.
func TestPendingRequestIDStableAndAdvances(t *testing.T) {
	s := &Session{nodeID: "n1", logw: &logWriter{Path: filepath.Join(t.TempDir(), "n1.jsonl")}}
	opts := []sdk.PermissionOption{
		{OptionId: "opt_allow", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce},
		{OptionId: "opt_reject", Name: "Reject", Kind: sdk.PermissionOptionKindRejectOnce},
	}
	s.pendingSeq++
	s.pending = &pendingPermission{
		seq: s.pendingSeq, toolTitle: "run bash", toolKind: "execute",
		options: opts, ch: make(chan sdk.PermissionOptionId, 1),
	}
	p1, ok := s.pendingInfo()
	if !ok || p1.RequestID == "" {
		t.Fatalf("pending = %+v ok=%v, want non-empty RequestID", p1, ok)
	}
	// Stable while same request remains.
	p1b, _ := s.pendingInfo()
	if p1b.RequestID != p1.RequestID {
		t.Fatalf("RequestID changed while pending: %q → %q", p1.RequestID, p1b.RequestID)
	}

	// Prepare against request A.
	tokA, _, err := s.prepareResolve("1")
	if err != nil {
		t.Fatal(err)
	}

	// Replace with a new identical-looking request B.
	s.pendingSeq++
	s.pending = &pendingPermission{
		seq: s.pendingSeq, toolTitle: "run bash", toolKind: "execute",
		options: opts, ch: make(chan sdk.PermissionOptionId, 1),
	}
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
	tokB, _, err := s.prepareResolve("1")
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
