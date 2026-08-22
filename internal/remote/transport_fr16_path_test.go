package remote

// S6 R2 addition — FR-16 on the path that actually carries traffic.
//
// AT-FR-16-a and -b both call HandshakeSession directly. That function is
// an admission gate: it judges a relayed description and returns. It is not
// what the tunnel runs. If the fingerprint check were dropped from
// relayThroughHub — the code the real negotiation goes through — both R1
// rows would stay green and a substituted fingerprint would be accepted by
// every live session.
//
// These rows close that gap by asserting the refusal on the negotiation
// path itself: a mismatched inner must abort the handshake, and no data
// channel may come up.

import (
	"net/http"
	"strings"
	"testing"
)

// tamperedFingerprint is a well-formed sha-256 fingerprint that belongs to
// nothing. Malformed input would be refused by parsing rather than by the
// FR-16 comparison, which would test the wrong thing.
const tamperedFingerprint = "sha-256 " +
	"AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:" +
	"AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"

func TestS6NegotiationRefusesASubstitutedFingerprint(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()

	hub := newSignallingHub()
	hub.tamper = func(in SessionInner) SessionInner {
		in.Fingerprint = tamperedFingerprint
		return in
	}

	s, err := inProcessTunnelVia(ctx, hub, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if s != nil {
		_ = s.Close()
		t.Fatal("a substituted fingerprint produced a live tunnel; FR-16 is not enforced on the negotiation path")
	}
	requireClass(t, err, ClassFingerprint)

	// FR-16 asks for a refusal, so the failure must be legible as one. A
	// session that merely timed out would send the user hunting for a
	// network fault instead of a substituting hub.
	if g := strings.ToLower(guidanceOf(err)); strings.Contains(g, "timeout") {
		t.Fatalf("substitution must refuse, not time out: %v", err)
	}
}

// TestS6NegotiationRefusesAnAlteredSDP is the -a half on the same path: the
// fingerprint still matches what the peer sent, but the SDP it is bound to
// no longer parses. The stack must reject it, and the class must say the
// handshake failed rather than blaming the fingerprint.
func TestS6NegotiationRefusesAnAlteredSDP(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()

	hub := newSignallingHub()
	hub.tamper = func(in SessionInner) SessionInner {
		in.SDP = in.SDP + "\na=ice-ufrag:HUBTAMPER"
		return in
	}

	s, err := inProcessTunnelVia(ctx, hub, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if s != nil {
		_ = s.Close()
		t.Fatal("an altered SDP produced a live tunnel")
	}
	requireClass(t, err, ClassHandshake)
}

// TestS6HonestHubStillNegotiates is the control. Without it the two rows
// above would pass just as well against a negotiation that never succeeds
// at all.
func TestS6HonestHubStillNegotiates(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()

	s, err := inProcessTunnelVia(ctx, newSignallingHub(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if err != nil {
		t.Fatalf("untampered negotiation failed: %v", err)
	}
	defer func() { _ = s.Close() }()
	if !s.ChannelLive() {
		t.Fatal("untampered negotiation produced no live channel")
	}
}
