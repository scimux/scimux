package remote

import (
	"bytes"
	"strings"
	"testing"
)

// AT-FR-16-a: The sealed envelope contains the DTLS fingerprint; a hub
// that alters the SDP causes handshake failure.
func TestAT_FR_16_a_SealedEnvelopeContainsFingerprintHubAlteredSDPFailsHandshake(t *testing.T) {
	const at = "AT-FR-16-a"
	ctx, cancel := ctxTO(t)
	defer cancel()

	inner, err := CreateSessionOffer(ctx)
	if err != nil {
		t.Fatalf("%s: CreateSessionOffer: %v", at, err)
	}
	if inner.V != 1 {
		t.Fatalf("%s: inner v = %d, want 1", at, inner.V)
	}
	if inner.Type != "session-offer" {
		t.Fatalf("%s: inner type = %q, want session-offer", at, inner.Type)
	}
	if inner.Fingerprint == "" {
		t.Fatalf("%s: inner JSON has no DTLS fingerprint", at)
	}
	if inner.SDP == "" {
		t.Fatalf("%s: inner JSON has no SDP", at)
	}
	if !s6FingerprintInSDP(inner.Fingerprint, inner.SDP) {
		t.Fatalf("%s: fingerprint %q is not bound to SDP", at, inner.Fingerprint)
	}

	pub, priv := s6Recipient(t)
	sealed, err := SealEnvelope(inner, pub, DefaultOrigin, s6VectorRID)
	if err != nil {
		t.Fatalf("%s: SealEnvelope: %v", at, err)
	}
	if len(sealed) == 0 {
		t.Fatalf("%s: sealed envelope is empty", at)
	}
	if bytes.Contains(sealed, []byte(inner.Fingerprint)) {
		t.Fatalf("%s: DTLS fingerprint is visible in the sealed blob; a hub could substitute it", at)
	}
	if bytes.Contains(sealed, []byte(inner.SDP)) {
		t.Fatalf("%s: SDP is visible in the sealed blob", at)
	}

	opened, err := OpenEnvelope(sealed, priv, DefaultOrigin, s6VectorRID)
	if err != nil {
		t.Fatalf("%s: OpenEnvelope: %v", at, err)
	}
	if opened.Fingerprint != inner.Fingerprint {
		t.Fatalf("%s: opened fingerprint %q, want %q", at, opened.Fingerprint, inner.Fingerprint)
	}

	tampered := opened
	tampered.SDP = opened.SDP + "\na=ice-ufrag:HUBTAMPER"
	err = HandshakeSession(ctx, inner, tampered)
	requireClass(t, err, ClassHandshake)
}

// AT-FR-16-b: A hub-substituted fingerprint is detected and the
// connection refused.
func TestAT_FR_16_b_HubSubstitutedFingerprintRefused(t *testing.T) {
	const at = "AT-FR-16-b"
	ctx, cancel := ctxTO(t)
	defer cancel()

	inner, err := CreateSessionOffer(ctx)
	if err != nil {
		t.Fatalf("%s: CreateSessionOffer: %v", at, err)
	}
	if inner.Fingerprint == "" || inner.SDP == "" {
		t.Fatalf("%s: offer missing fingerprint or SDP", at)
	}

	substituted := inner
	substituted.Fingerprint = "sha-256 00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00"
	if substituted.Fingerprint == inner.Fingerprint {
		t.Fatalf("%s: substituted fingerprint equals the original", at)
	}
	err = HandshakeSession(ctx, inner, substituted)
	requireClass(t, err, ClassFingerprint)
	g := strings.ToLower(guidanceOf(err))
	if strings.Contains(g, "timeout") {
		t.Fatalf("%s: substitution must refuse, not time out: %v", at, err)
	}
}
