package remote

// Join 1 — the rendezvous wait loop must consume what it waits for.
//
// S5 built the loop: rvLoop fans out one waitLoopRID per live device, which
// long-polls /v1/wait, rotates the challenge, and applies the backoff. S6
// built the transport: CreateSessionOffer/CreateSessionAnswer, the §12.2
// sealed envelope, and the FR-16 fingerprint check. Nothing joins them.
//
// Concretely, postWaitRID today reads the 200 body into `raw`, validates that
// its content type is application/octet-stream, and then returns `error`
// alone — the body is dropped on the floor. So a device can post a sealed
// session offer, the rendezvous can hand it to the laptop, and the laptop
// will discard it and go back to waiting. The loop is a long poll with no
// consumer.
//
// These rows assert the join from the outside, at the protocol boundary: a
// rendezvous that serves a sealed offer must see a sealed answer come back on
// a later wait, and that answer must open under the device's own static key.
// They deliberately do not reach into client internals, because the defect
// being closed is precisely that an internal primitive existed and no path
// reached it.

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"
)

// deviceKeypair mints a device-side static P-256 key. This is Y in the §11
// pairing transcript, and the key the laptop's reply envelope is sealed to.
func deviceKeypair(t *testing.T) (*ecdh.PrivateKey, []byte) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate device P-256 key: %v", err)
	}
	return k, k.PublicKey().Bytes()
}

// enrolledClientWithDevice starts a client against rv with one registered
// device, and returns the client and the device's rendezvous id.
func enrolledClientWithDevice(t *testing.T, rv *sessionRV, devECDHPub []byte) (*Client, string) {
	t.Helper()
	cfg := r3ClientCfg(t, rv.URL(), rv.HTTPClient())
	cfg.Backoff = BackoffConfig{
		Initial:    20 * time.Millisecond,
		Max:        200 * time.Millisecond,
		Factor:     2,
		SuccessFor: 5 * time.Second,
	}
	pub, priv := newEd25519(t)
	writeState(t, NewClient(cfg), PersistedState{
		V:          1,
		Status:     StateEnrolled,
		Handle:     "ih_041061050R3GG28A",
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		Origin:     DefaultOrigin,
	})

	ctx, cancel := ctxTO(t)
	t.Cleanup(cancel)
	c := NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	dev, err := c.RegisterDevice(ctx, DeviceRecord{
		ID:      "phone",
		PubKey:  append([]byte(nil), testPhonePubKey...),
		ECDHPub: append([]byte(nil), devECDHPub...),
	})
	if err != nil {
		t.Fatalf("register device: %v", err)
	}
	return c, dev.RID
}

// awaitReply polls rv for the laptop's first reply blob.
func awaitReply(t *testing.T, rv *sessionRV, within time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if rs := rv.Replies(); len(rs) > 0 {
			return rs[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no reply posted within %v (waits served: %d)", within, rv.WaitCount())
	return nil
}

// TestWaitOpensTheEnvelopeAndPostsASealedAnswer is the join. A device posts a
// sealed session offer; the laptop must open it, negotiate an answer, and
// seal that answer back to the device.
func TestWaitOpensTheEnvelopeAndPostsASealedAnswer(t *testing.T) {
	rv := newSessionRV(t)
	devPriv, devPub := deviceKeypair(t)
	c, rid := enrolledClientWithDevice(t, rv, devPub)

	ctx, cancel := ctxTO(t)
	defer cancel()

	// The device seals its offer to the laptop's static X, exactly as a real
	// browser peer would. Reading X through the public accessor keeps this on
	// the product path rather than on a fixture.
	x, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatalf("laptop ECDH public: %v", err)
	}
	offer, err := CreateSessionOffer(ctx)
	if err != nil {
		t.Fatalf("device session offer: %v", err)
	}
	sealed, err := SealEnvelope(offer, x, DefaultOrigin, rid)
	if err != nil {
		t.Fatalf("seal offer: %v", err)
	}
	rv.QueueEnvelope(rid, sealed)

	reply := awaitReply(t, rv, 8*time.Second)

	// The reply must be a real §12.2 envelope sealed in the opposite
	// direction: openable by the device's static key, under the same origin
	// and rid. Anything else means the laptop echoed or improvised.
	answer, err := OpenEnvelope(reply, devPriv.Bytes(), DefaultOrigin, rid)
	if err != nil {
		t.Fatalf("device could not open the laptop's reply: %v", err)
	}
	if answer.Type != sessionAnswerType {
		t.Fatalf("reply inner type = %q, want %q", answer.Type, sessionAnswerType)
	}
	if answer.SDP == "" {
		t.Fatal("reply carries no SDP")
	}
	if answer.Fingerprint == "" {
		t.Fatal("reply carries no DTLS fingerprint; FR-16 has nothing to bind")
	}
}

// TestWaitIgnoresAnUnopenableEnvelope keeps the join fail-closed. A blob that
// does not authenticate must produce no reply at all — improvising an answer
// for an unopenable envelope would hand an unauthenticated peer a session —
// and must not wedge the loop.
func TestWaitIgnoresAnUnopenableEnvelope(t *testing.T) {
	rv := newSessionRV(t)
	_, devPub := deviceKeypair(t)
	_, rid := enrolledClientWithDevice(t, rv, devPub)

	junk := make([]byte, 160)
	for i := range junk {
		junk[i] = byte(i)
	}
	rv.QueueEnvelope(rid, junk)

	// Give the loop room to serve the junk and keep polling afterwards.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rv.DeliveredCount(rid) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rv.DeliveredCount(rid) == 0 {
		t.Fatal("the rendezvous never served the junk envelope; the loop is not waiting")
	}

	served := rv.WaitCount()
	time.Sleep(500 * time.Millisecond)
	if rs := rv.Replies(); len(rs) != 0 {
		t.Fatalf("laptop answered an unopenable envelope: %d reply/replies", len(rs))
	}
	if rv.WaitCount() <= served {
		t.Fatal("the wait loop stopped after an unopenable envelope; it must keep polling")
	}
}

// TestWaitWithNoEnvelopePostsNoReply is the control. Without it the two rows
// above would pass against a client that never posts a reply under any
// circumstances.
func TestWaitWithNoEnvelopePostsNoReply(t *testing.T) {
	rv := newSessionRV(t)
	_, devPub := deviceKeypair(t)
	enrolledClientWithDevice(t, rv, devPub)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rv.WaitCount() >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rv.WaitCount() < 3 {
		t.Fatalf("wait loop served only %d waits; it is not polling", rv.WaitCount())
	}
	if rs := rv.Replies(); len(rs) != 0 {
		t.Fatalf("laptop posted %d reply/replies with no envelope outstanding", len(rs))
	}
}
