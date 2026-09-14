package remote

// Finding 9, at the path rather than the primitive.
//
// transport_session_seal_test.go pins the construction. This file pins the
// wait loop, because the construction was never the thing that decided who
// was allowed a session: answerSessionEnvelope opened an envelope and, if it
// opened, answered it. Under §12.2.1 that meant anyone holding the
// computer's ECDH public point and a live rendezvous id could obtain a
// sealed answer and dial the session — and both of those travel in the
// invite link, which is the one thing the device is asked to handle.
//
// The rows here drive the real client against the real wait loop, so a
// future refactor that reintroduced an anonymous open would fail here even
// if every primitive still round-tripped.

import (
	"crypto/ecdh"
	"crypto/rand"
	"testing"
	"time"
)

// TestAStrangerHoldingTheComputerKeyGetsNoAnswer is the review's own
// reproduction. The attacker is given everything §12.2.1 required: the
// computer's public X and the device's rendezvous id. It holds no pairing
// key, and that is now the whole difference.
func TestAStrangerHoldingTheComputerKeyGetsNoAnswer(t *testing.T) {
	rv := newSessionRV(t)
	_, devPub := deviceKeypair(t)
	c, rid := enrolledClientWithDevice(t, rv, devPub)

	ctx, cancel := ctxTO(t)
	defer cancel()
	dev := newDevicePeer(t, ctx)

	x, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatalf("computer ECDH public: %v", err)
	}
	stranger, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealSessionEnvelope(dev.offer, x, stranger.Bytes(), DefaultOrigin, rid)
	if err != nil {
		t.Fatalf("seal as the stranger: %v", err)
	}
	rv.QueueEnvelope(rid, sealed)

	// Give the wait loop long enough to have answered if it were going to:
	// the positive row below completes well inside this budget.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rs := rv.Replies(); len(rs) > 0 {
			t.Fatalf("the computer answered an envelope sealed by a stranger holding only its public key")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rv.WaitCount() == 0 {
		t.Fatal("the wait loop never ran, so this row proved nothing")
	}
}

// TestTheDeviceStaticIsWhatBuysASession is the positive half, and it is what
// stops the row above from passing because the seal broke for everyone.
func TestTheDeviceStaticIsWhatBuysASession(t *testing.T) {
	rv := newSessionRV(t)
	devPriv, devPub := deviceKeypair(t)
	c, rid := enrolledClientWithDevice(t, rv, devPub)

	ctx, cancel := ctxTO(t)
	defer cancel()
	dev := newDevicePeer(t, ctx)

	x, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatalf("computer ECDH public: %v", err)
	}
	sealed, err := SealSessionEnvelope(dev.offer, x, devPriv.Bytes(), DefaultOrigin, rid)
	if err != nil {
		t.Fatalf("seal as the paired device: %v", err)
	}
	rv.QueueEnvelope(rid, sealed)

	// The device opens the answer naming the computer as the sender. A reply
	// that did not authenticate as the paired computer is refused here, which
	// is the other direction of the same finding: the device must not dial an
	// answer that anyone holding Y could have written.
	answer, err := OpenSessionEnvelope(awaitReply(t, rv, 8*time.Second), devPriv.Bytes(), x, DefaultOrigin, rid)
	if err != nil {
		t.Fatalf("the device could not open the computer's answer: %v", err)
	}
	if answer.Type != sessionAnswerType {
		t.Fatalf("opened inner is a %q, want a session-answer", answer.Type)
	}
}

// TestTheAnswerIsNotOpenableAsAnonymous keeps the computer's reply on the new
// construction. Without it the offer could move to §12.2.2 while the answer
// stayed anonymous, and a hub that swapped the reply would still be
// undetectable by the device.
func TestTheAnswerIsNotOpenableAsAnonymous(t *testing.T) {
	rv := newSessionRV(t)
	devPriv, devPub := deviceKeypair(t)
	c, rid := enrolledClientWithDevice(t, rv, devPub)

	ctx, cancel := ctxTO(t)
	defer cancel()
	dev := newDevicePeer(t, ctx)

	x, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatalf("computer ECDH public: %v", err)
	}
	sealed, err := SealSessionEnvelope(dev.offer, x, devPriv.Bytes(), DefaultOrigin, rid)
	if err != nil {
		t.Fatal(err)
	}
	rv.QueueEnvelope(rid, sealed)

	reply := awaitReply(t, rv, 8*time.Second)
	if _, err := openV1(reply, devPriv.Bytes(), DefaultOrigin, rid); err == nil {
		t.Fatal("the computer's answer opened as an anonymous §12.2.1 envelope")
	}
	stranger, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSessionEnvelope(reply, devPriv.Bytes(), stranger.PublicKey().Bytes(), DefaultOrigin, rid); err == nil {
		t.Fatal("the computer's answer opened as some other sender's")
	}
}

// TestAV1OfferIsNoLongerAnswered is the no-downgrade row at the path. The
// wire shape is identical, so nothing but the derivation distinguishes the
// two; if a fallback ever reappeared, the attack would simply seal §12.2.1.
func TestAV1OfferIsNoLongerAnswered(t *testing.T) {
	rv := newSessionRV(t)
	_, devPub := deviceKeypair(t)
	c, rid := enrolledClientWithDevice(t, rv, devPub)

	ctx, cancel := ctxTO(t)
	defer cancel()
	dev := newDevicePeer(t, ctx)

	x, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealV1(dev.offer, x, DefaultOrigin, rid)
	if err != nil {
		t.Fatal(err)
	}
	rv.QueueEnvelope(rid, sealed)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rs := rv.Replies(); len(rs) > 0 {
			t.Fatal("the computer answered an anonymous §12.2.1 offer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rv.WaitCount() == 0 {
		t.Fatal("the wait loop never ran, so this row proved nothing")
	}
}
