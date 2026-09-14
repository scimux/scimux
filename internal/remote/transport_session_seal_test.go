package remote

// Finding 9 — the session seal must authenticate its sender.
//
// §12.2.1's seal is ephemeral-static: the key is ECDH(e, R) and the
// sender's own key is not an input. Anyone holding the recipient's public
// point — which is public, it travels in the invite link — can produce a
// blob that opens. A successful open therefore proved integrity and never
// identity, and the wait loop treated it as both.
//
// §12.2.2 adds the static-static agreement, so the key exists only for a
// party that holds the sender's private key. These rows are pinned to the
// checked-in envelope-seal-session-p256 vector, which was produced by the
// specification rather than by this code, so opening it is evidence and not
// a round trip.

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func sessionSealVector(t *testing.T) vector {
	t.Helper()
	for _, v := range loadVectors(t, "S6-envelope") {
		if v.ID == "envelope-seal-session-p256" {
			return v
		}
	}
	t.Fatal("S6-envelope: the envelope-seal-session-p256 vector is absent")
	return vector{}
}

func mustHex(t *testing.T, field, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("%s: %v", field, err)
	}
	return b
}

// TestSessionSealOpensTheSpecVectorOnlyUnderItsSender is the conformance
// direction and the defect in one row: the spec's own bytes open under the
// static that sealed them, and refuse every other static — including a
// freshly minted one, which is what an attacker holding only the public
// recipient key has.
func TestSessionSealOpensTheSpecVectorOnlyUnderItsSender(t *testing.T) {
	v := sessionSealVector(t)
	sealed := mustHex(t, "sealed_hex", v.SealedHex)
	recipientPriv := mustHex(t, "recipient_priv_hex", v.RecipientPrivHex)
	senderPub := mustHex(t, "sender_pub_hex", v.SenderPubHex)

	inner, err := OpenSessionEnvelope(sealed, recipientPriv, senderPub, DefaultOrigin, s6VectorRID)
	if err != nil {
		t.Fatalf("OpenSessionEnvelope on the spec's own bytes: %v", err)
	}
	if inner.V != ProtocolVersion || inner.Type != sessionOfferType {
		t.Fatalf("opened inner = %+v, want a v%d session-offer", inner, ProtocolVersion)
	}
	plain, err := canonicalInner(inner)
	if err != nil {
		t.Fatal(err)
	}
	if want := mustHex(t, "plaintext_hex", v.PlaintextHex); !bytes.Equal(plain, want) {
		t.Fatalf("canonical inner is not the vector plaintext:\n got %s\nwant %s", plain, want)
	}

	stranger, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSessionEnvelope(sealed, recipientPriv, stranger.PublicKey().Bytes(), DefaultOrigin, s6VectorRID); err == nil {
		t.Fatal("the vector opened under a static that did not seal it")
	}
	// The recipient's own point is the one a confused implementation would
	// reach for, so name it explicitly rather than trusting "some other key".
	recipientPub := mustHex(t, "recipient_pub_hex", v.RecipientPubHex)
	if _, err := OpenSessionEnvelope(sealed, recipientPriv, recipientPub, DefaultOrigin, s6VectorRID); err == nil {
		t.Fatal("the vector opened with the recipient named as its own sender")
	}
}

// TestSessionSealBindsTheSenderItDerives pins that S in the HKDF info is the
// sealer's own point and not something the caller may assert independently:
// SealSessionEnvelope takes a private scalar and derives the public half, so
// there is no pair to mismatch.
func TestSessionSealBindsTheSenderItDerives(t *testing.T) {
	v := sessionSealVector(t)
	senderPriv := mustHex(t, "sender_priv_hex", v.SenderPrivHex)
	k, err := ecdh.P256().NewPrivateKey(senderPriv)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(k.PublicKey().Bytes()); got != v.SenderPubHex {
		t.Fatalf("sender_pub_hex is not sender_priv_hex's point:\n got %s\nwant %s", got, v.SenderPubHex)
	}

	recipientPub := mustHex(t, "recipient_pub_hex", v.RecipientPubHex)
	recipientPriv := mustHex(t, "recipient_priv_hex", v.RecipientPrivHex)
	inner := SessionInner{V: ProtocolVersion, Type: sessionOfferType, SDP: "v=0", Fingerprint: "sha-256 AA:BB"}
	sealed, err := SealSessionEnvelope(inner, recipientPub, senderPriv, DefaultOrigin, s6VectorRID)
	if err != nil {
		t.Fatalf("SealSessionEnvelope: %v", err)
	}
	got, err := OpenSessionEnvelope(sealed, recipientPriv, k.PublicKey().Bytes(), DefaultOrigin, s6VectorRID)
	if err != nil || got != inner {
		t.Fatalf("round trip under the derived static: %+v, %v", got, err)
	}
}

// TestSessionSealIsNotInterchangeableWithPairing is the no-downgrade property.
// The two constructions share a wire shape by design, so nothing but the
// derivation stops a pairing blob from being accepted where a session one is
// required. If that ever became possible, an attacker would simply seal the
// pairing construction.
func TestSessionSealIsNotInterchangeableWithPairing(t *testing.T) {
	v := sessionSealVector(t)
	recipientPub := mustHex(t, "recipient_pub_hex", v.RecipientPubHex)
	recipientPriv := mustHex(t, "recipient_priv_hex", v.RecipientPrivHex)
	senderPriv := mustHex(t, "sender_priv_hex", v.SenderPrivHex)
	senderPub := mustHex(t, "sender_pub_hex", v.SenderPubHex)
	inner := SessionInner{V: ProtocolVersion, Type: sessionOfferType, SDP: "v=0", Fingerprint: "sha-256 AA:BB"}

	pairing, err := sealPairing(inner, recipientPub, DefaultOrigin, s6VectorRID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSessionEnvelope(pairing, recipientPriv, senderPub, DefaultOrigin, s6VectorRID); err == nil {
		t.Fatal("a pairing envelope opened as an authenticated session envelope")
	}

	session, err := SealSessionEnvelope(inner, recipientPub, senderPriv, DefaultOrigin, s6VectorRID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openPairing(session, recipientPriv, DefaultOrigin, s6VectorRID); err == nil {
		t.Fatal("a session envelope opened as an anonymous pairing envelope")
	}
}

// TestSessionSealKeepsTheWireShapeAndItsBindings is the compatibility half:
// rv relays opaque bytes and must not need a redeploy for a seal change, so
// the blob stays E || nonce || ct. The AD bindings and the fresh ephemeral
// carry over from §12.2.1 and are asserted here because a new derivation is
// exactly where they get dropped.
func TestSessionSealKeepsTheWireShapeAndItsBindings(t *testing.T) {
	v := sessionSealVector(t)
	recipientPub := mustHex(t, "recipient_pub_hex", v.RecipientPubHex)
	recipientPriv := mustHex(t, "recipient_priv_hex", v.RecipientPrivHex)
	senderPriv := mustHex(t, "sender_priv_hex", v.SenderPrivHex)
	senderPub := mustHex(t, "sender_pub_hex", v.SenderPubHex)
	inner := SessionInner{V: ProtocolVersion, Type: sessionOfferType, SDP: "v=0", Fingerprint: "sha-256 AA:BB"}

	plain, err := canonicalInner(inner)
	if err != nil {
		t.Fatal(err)
	}
	first, err := SealSessionEnvelope(inner, recipientPub, senderPriv, DefaultOrigin, s6VectorRID)
	if err != nil {
		t.Fatal(err)
	}
	if want := p256UncompressedLen + gcmNonceLen + len(plain) + gcmTagLen; len(first) != want {
		t.Fatalf("sealed length %d, want %d — the wire shape changed", len(first), want)
	}
	if first[0] != 0x04 {
		t.Fatalf("sealed does not start with an uncompressed point: %#x", first[0])
	}

	otherRID := "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	if _, err := OpenSessionEnvelope(first, recipientPriv, senderPub, DefaultOrigin, otherRID); err == nil {
		t.Fatal("a session envelope replayed onto another rendezvous id opened")
	}
	if _, err := OpenSessionEnvelope(first, recipientPriv, senderPub, "https://elsewhere.example", s6VectorRID); err == nil {
		t.Fatal("a session envelope opened under another origin")
	}
	tampered := append([]byte(nil), first...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := OpenSessionEnvelope(tampered, recipientPriv, senderPub, DefaultOrigin, s6VectorRID); err == nil {
		t.Fatal("a tampered session envelope opened")
	}

	second, err := SealSessionEnvelope(inner, recipientPub, senderPriv, DefaultOrigin, s6VectorRID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first[:p256UncompressedLen], second[:p256UncompressedLen]) {
		t.Fatal("two session seals reused the same ephemeral public key")
	}
	if bytes.Equal(first[p256UncompressedLen:p256UncompressedLen+gcmNonceLen],
		second[p256UncompressedLen:p256UncompressedLen+gcmNonceLen]) {
		t.Fatal("two session seals reused the same nonce")
	}
}
