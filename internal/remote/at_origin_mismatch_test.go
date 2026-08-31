package remote

import (
	"strings"
	"testing"
)

// An enrolled identity belongs to one rendezvous. The handle was issued there,
// the invite was redeemed there, and the origin string is bound into every
// pairing transcript (rendezvous-v1 §11). Pointing an enrolled installation at
// a different rendezvous is therefore not a reconfiguration — it is presenting
// one server's handle to another.
//
// Nothing checked this before --rendezvous-url existed, because nothing could
// change the origin. Now that an operator can, the failure has to be legible
// here: a rendezvous that does not know a handle rejects it with a deliberately
// opaque constant (the protocol's anti-enumeration rule), so the server can
// never explain this. Only the client can.
func TestEnrolledOriginMustMatchTheConfiguredRendezvous(t *testing.T) {
	cfg := clientCfg(t, nil)
	cfg.Origin = "https://rv.example"
	cfg.RendezvousURL = ""
	writeState(t, NewClient(cfg), enrolledFixture(t, "ih_041061050R3GG28A"))

	ctx, cancel := ctxTO(t)
	defer cancel()

	err := NewClient(cfg).Start(ctx)
	if got := classOf(err); got != ClassOriginMismatch {
		t.Fatalf("start against a different rendezvous: class %q, want %q (err=%v)",
			got, ClassOriginMismatch, err)
	}
	g := guidanceOf(err)
	for _, want := range []string{DefaultOrigin, "https://rv.example"} {
		if !strings.Contains(g, want) {
			t.Errorf("guidance %q does not name %q; the operator cannot tell which "+
				"of the two is the stale one", g, want)
		}
	}
}

// The same origin is not a mismatch, and neither is an unenrolled state: a
// fresh installation has no identity bound to anything yet, so the operator is
// free to name whichever rendezvous they intend to enroll against.
func TestMatchingAndUnenrolledOriginsStart(t *testing.T) {
	t.Run("matching", func(t *testing.T) {
		cfg := clientCfg(t, nil)
		cfg.Origin = DefaultOrigin
		cfg.RendezvousURL = ""
		writeState(t, NewClient(cfg), enrolledFixture(t, "ih_041061050R3GG28A"))
		ctx, cancel := ctxTO(t)
		defer cancel()
		if err := NewClient(cfg).Start(ctx); classOf(err) == ClassOriginMismatch {
			t.Fatalf("an unchanged origin was reported as a mismatch: %v", err)
		}
	})

	t.Run("unenrolled", func(t *testing.T) {
		cfg := clientCfg(t, nil)
		cfg.Origin = "https://rv.example"
		cfg.RendezvousURL = ""
		ctx, cancel := ctxTO(t)
		defer cancel()
		if err := NewClient(cfg).Start(ctx); classOf(err) == ClassOriginMismatch {
			t.Fatalf("a fresh installation was refused a rendezvous choice: %v", err)
		}
	})
}
