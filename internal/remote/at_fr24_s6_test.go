package remote

import (
	"strings"
	"testing"
)

// AT-FR-24-a (S6 transport causes): ICE failure and connected-then-lost
// are induced as distinct FR-24 states. A generic "unreachable" for either
// fails — conflation is the defect being guarded. Web rendering of all six
// states is AT-FR-24-a at the web layer (S8); this row is the transport
// producer S6 owns.
func TestAT_FR_24_a_ICEAndConnectedThenLostAreDistinct(t *testing.T) {
	const at = "AT-FR-24-a"
	ctx, cancel := ctxTO(t)
	defer cancel()

	iceErr := SimulateICEFailure(ctx)
	requireClass(t, iceErr, ClassICEFailed)
	lostErr := SimulateChannelLoss(ctx)
	requireClass(t, lostErr, ClassLost)

	if classOf(iceErr) == classOf(lostErr) {
		t.Fatalf("%s: ICE failure and connected-then-lost collapsed to %q", at, classOf(iceErr))
	}
	for _, cl := range []Class{ClassUnavailable, ClassPeerAbsent, ClassRevoked, ClassUnauthorized} {
		if classOf(iceErr) == cl {
			t.Fatalf("%s: ICE failure conflated with %s", at, cl)
		}
		if classOf(lostErr) == cl {
			t.Fatalf("%s: connected-then-lost conflated with %s", at, cl)
		}
	}
	if CauseICEFailed == CauseConnectedThenLost {
		t.Fatalf("%s: transport cause constants are not distinct", at)
	}
	if string(CauseICEFailed) != string(ClassICEFailed) {
		t.Fatalf("%s: ICE cause %q does not match class %q", at, CauseICEFailed, ClassICEFailed)
	}
	if string(CauseConnectedThenLost) != string(ClassLost) {
		t.Fatalf("%s: lost cause %q does not match class %q", at, CauseConnectedThenLost, ClassLost)
	}
}

// AT-FR-24-c: The ICE-failure state names the supported fallback
// (SSH/WireGuard/Tailscale); the other five do not, since suggesting a
// fallback for a revoked credential would be wrong advice.
func TestAT_FR_24_c_ICEFailureNamesSupportedFallback(t *testing.T) {
	const at = "AT-FR-24-c"
	ctx, cancel := ctxTO(t)
	defer cancel()

	iceErr := SimulateICEFailure(ctx)
	requireClass(t, iceErr, ClassICEFailed)
	if !s6GuidanceHasFallback(guidanceOf(iceErr) + iceErr.Error()) {
		t.Fatalf("%s: ICE failure guidance does not name SSH/WireGuard/Tailscale: %v", at, iceErr)
	}

	lostErr := SimulateChannelLoss(ctx)
	requireClass(t, lostErr, ClassLost)
	if s6GuidanceHasFallback(guidanceOf(lostErr) + lostErr.Error()) {
		t.Fatalf("%s: connected-then-lost named the ICE fallback: %v", at, lostErr)
	}

	others := []Class{ClassUnavailable, ClassPeerAbsent, ClassRevoked, ClassUnauthorized}
	for _, cl := range others {
		sample := classError(cl, "transport", "sample "+string(cl))
		if s6GuidanceHasFallback(guidanceOf(sample) + sample.Error()) {
			t.Fatalf("%s: class %s named the ICE fallback: %v", at, cl, sample)
		}
	}
	if strings.Contains(strings.ToLower(string(ClassLost)), "ice") {
		t.Fatalf("%s: connected-then-lost class name contains ice", at)
	}
}
