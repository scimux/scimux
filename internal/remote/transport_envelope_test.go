package remote

// S6 R2 — the product envelope against the S1 vectors.
//
// protocol_vectors_test.go (S1, Gate B) executes the specification with an
// independent implementation of the same construction. That proves the
// *spec* is executable; it says nothing about the code the product ships.
// This file closes that gap: the §12.2.1 seal is pinned to the checked-in
// envelope-seal-p256 vector, so a construction drift in transport.go fails
// here rather than in the field. Since finding 9 the only product caller of
// that construction is pairing, so these rows exercise sealEnvelopeBytes and
// openEnvelopeBytes — the functions pairing actually calls — rather than a
// session wrapper kept alive for a test.

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// sealV1 and openV1 lift the pairing-path bytes into §12.1 form so these
// rows can keep asserting over a SessionInner.
func sealV1(inner SessionInner, recipientPub []byte, origin, rid string) ([]byte, error) {
	plain, err := canonicalInner(inner)
	if err != nil {
		return nil, err
	}
	return sealEnvelopeBytes(plain, recipientPub, origin, rid)
}

func openV1(sealed, recipientPriv []byte, origin, rid string) (SessionInner, error) {
	plain, err := openEnvelopeBytes(sealed, recipientPriv, origin, rid)
	if err != nil {
		return SessionInner{}, err
	}
	return decodeSessionInner(plain)
}

func envelopeSealVector(t *testing.T) vector {
	t.Helper()
	for _, v := range loadVectors(t, "S6-envelope") {
		if v.ID == "envelope-seal-p256" {
			return v
		}
	}
	t.Fatal("S6-envelope: the envelope-seal-p256 vector is absent")
	return vector{}
}

// TestS6ProductOpensTheEnvelopeVector is the conformance direction that
// matters: the bytes in the vector were produced by the specification, not
// by this code, so opening them is evidence and not a round trip.
func TestS6ProductOpensTheEnvelopeVector(t *testing.T) {
	v := envelopeSealVector(t)
	sealed, err := hex.DecodeString(v.SealedHex)
	if err != nil {
		t.Fatalf("sealed_hex: %v", err)
	}
	priv, err := hex.DecodeString(v.RecipientPrivHex)
	if err != nil {
		t.Fatalf("recipient_priv_hex: %v", err)
	}
	ad, err := hex.DecodeString(v.ADHex)
	if err != nil {
		t.Fatalf("ad_hex: %v", err)
	}
	if !bytes.Equal(ad, envelopeAD(DefaultOrigin, s6VectorRID)) {
		t.Fatalf("the product AD is not the vector's: %q vs %q", envelopeAD(DefaultOrigin, s6VectorRID), ad)
	}

	inner, err := openV1(sealed, priv, DefaultOrigin, s6VectorRID)
	if err != nil {
		t.Fatalf("openV1 on the spec's own bytes: %v", err)
	}
	if inner.V != ProtocolVersion || inner.Type != sessionOfferType {
		t.Fatalf("opened inner = %+v, want a v%d session-offer", inner, ProtocolVersion)
	}

	// The canonical encoding is part of the construction: the plaintext the
	// vector sealed is sorted-key JSON, and a struct-ordered encoder would
	// produce a different envelope for the same session.
	plain, err := canonicalInner(inner)
	if err != nil {
		t.Fatal(err)
	}
	want, err := hex.DecodeString(v.PlaintextHex)
	if err != nil {
		t.Fatalf("plaintext_hex: %v", err)
	}
	if !bytes.Equal(plain, want) {
		t.Fatalf("canonical inner is not the vector plaintext:\n got %s\nwant %s", plain, want)
	}
}

// TestS6EnvelopeIsBoundToOriginAndRID pins the AD half of §12.2: an
// envelope replayed onto another rendezvous id, or resealed for another
// origin, must not open. Without this the AD could be dropped entirely and
// every other envelope assertion would still pass.
func TestS6EnvelopeIsBoundToOriginAndRID(t *testing.T) {
	v := envelopeSealVector(t)
	pub, priv := s6Recipient(t)
	if got := hex.EncodeToString(pub); got != v.RecipientPubHex {
		t.Fatalf("helper recipient key drifted from the vector")
	}

	inner := SessionInner{V: ProtocolVersion, Type: sessionOfferType, SDP: "v=0", Fingerprint: "sha-256 AA:BB"}
	sealed, err := sealV1(inner, pub, DefaultOrigin, s6VectorRID)
	if err != nil {
		t.Fatalf("sealV1: %v", err)
	}
	if got, err := openV1(sealed, priv, DefaultOrigin, s6VectorRID); err != nil || got != inner {
		t.Fatalf("round trip: %+v, %v", got, err)
	}

	otherRID := "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	if _, err := openV1(sealed, priv, DefaultOrigin, otherRID); err == nil {
		t.Fatal("an envelope replayed onto another rendezvous id opened")
	}
	if _, err := openV1(sealed, priv, "https://elsewhere.example", s6VectorRID); err == nil {
		t.Fatal("an envelope opened under another origin")
	}

	// A flipped ciphertext byte must fail the tag, not decode to something.
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := openV1(tampered, priv, DefaultOrigin, s6VectorRID); err == nil {
		t.Fatal("a tampered envelope opened")
	}
}

// TestS6SealDrawsAFreshEphemeral guards the one line of the vector that is
// explicitly not a minting rule: "ephemeral_priv_hex is a fixture.
// Production MUST draw a fresh ephemeral per seal."
func TestS6SealDrawsAFreshEphemeral(t *testing.T) {
	pub, _ := s6Recipient(t)
	inner := SessionInner{V: ProtocolVersion, Type: sessionOfferType, SDP: "v=0", Fingerprint: "sha-256 AA:BB"}
	first, err := sealV1(inner, pub, DefaultOrigin, s6VectorRID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sealV1(inner, pub, DefaultOrigin, s6VectorRID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first[:p256UncompressedLen], second[:p256UncompressedLen]) {
		t.Fatal("two seals reused the same ephemeral public key")
	}
	if bytes.Equal(first[p256UncompressedLen:p256UncompressedLen+gcmNonceLen],
		second[p256UncompressedLen:p256UncompressedLen+gcmNonceLen]) {
		t.Fatal("two seals reused the same nonce")
	}
	if bytes.Equal(first, second) {
		t.Fatal("two seals of the same inner produced the same envelope")
	}
}
