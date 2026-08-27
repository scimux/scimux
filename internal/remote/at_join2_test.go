package remote

// Join 2 — the answered peer must survive the answer.
//
// Join 1 taught the wait loop to consume the sealed offer it had been
// discarding: it opens the envelope, calls CreateSessionAnswer, and seals the
// answer back. That closed the *signalling* path and nothing more, which is
// what its own header comment says.
//
// What it left is a session that exists only on paper. CreateSessionAnswer
// mints a peer connection, produces a description, and closes it on the way
// out (`defer pc.Close()`, transport.go) — its doc comment says so plainly and
// points at InProcessTunnel/EstablishForDevice for "a live session". So the
// device receives a genuine, correctly sealed answer describing a peer that no
// longer exists, and its ICE attempt has nothing to reach. The join is
// half-built by construction, not by accident.
//
// The second gap is FR-16. On the S6 in-process path relayThroughHub calls
// checkRelayedFingerprint before applying a relayed description, and
// transport_fr16_path_test.go pins that. The live path does not: it reaches
// CreateSessionAnswer, whose only admission is checkInner — which asserts the
// fingerprint field is *non-empty*, never that it matches the SDP the peer
// actually offered. So the one check FR-16 exists for is enforced on the test
// harness path and absent from the path that will carry real traffic. That is
// the same "AT proves the primitive, not the path" shape this arc keeps
// finding, and it is worth stating that the tampering modelled here is not
// hypothetical: §12.2 seals the inner, so a rendezvous cannot rewrite it, but
// FR-16 defends the wider case of an inner arriving with a fingerprint that
// does not match its SDP whatever produced it.
//
// These rows drive a real pion peer as the device, because the defect is
// precisely that the primitives are all present and unreachable. A test that
// asserted over client internals would pass against the same broken path.

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"codeberg.org/chrberger/scimux/internal/remote/codec"
)

// devicePeer is the browser half: one peer connection, one "scimux" data
// channel, driven through the same rendezvous a real device would use.
type devicePeer struct {
	pc     *webrtc.PeerConnection
	dc     *webrtc.DataChannel
	stream *dcStream
	offer  SessionInner
}

// newDevicePeer mints the device side and gathers its offer. The channel is
// created before the offer so the SDP carries an m=application section.
func newDevicePeer(t *testing.T, ctx context.Context) *devicePeer {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("device peer connection: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	dc, err := pc.CreateDataChannel(tunnelChannelLabel, nil)
	if err != nil {
		t.Fatalf("device data channel: %v", err)
	}
	stream := newDCStream(dc)
	offer, err := localDescription(ctx, pc, sessionOfferType, func() (webrtc.SessionDescription, error) {
		return pc.CreateOffer(nil)
	})
	if err != nil {
		t.Fatalf("device offer: %v", err)
	}
	return &devicePeer{pc: pc, dc: dc, stream: stream, offer: offer}
}

// applyAnswer completes the device's half of the handshake and waits for the
// data channel to open. A retained computer peer makes this succeed; a closed
// one leaves it to time out, which is exactly the failure being asserted.
func (d *devicePeer) applyAnswer(ctx context.Context, answer SessionInner) error {
	if err := d.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: answer.SDP,
	}); err != nil {
		return err
	}
	return d.stream.waitOpen(ctx)
}

// roundTrip issues one FR-27 request over the open channel.
func (d *devicePeer) roundTrip(ctx context.Context, path string) (*codec.Response, error) {
	// The device end dials, so it is the initiator (tunnel-v2 §5.1).
	conn := codec.NewConn(d.stream, d.stream, codec.RoleInitiator)
	return conn.RoundTrip(ctx, &codec.Request{
		ID:      "join2-0001",
		Method:  http.MethodGet,
		Path:    path,
		Headers: http.Header{},
	})
}

// tunnelEcho is a stand-in for the S3 tunnel boundary: it proves a request
// crossed the wire and reached a handler, without asserting anything about
// what the real boundary decides. Join 3 supplies the real one.
func tunnelEcho(seen chan<- string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case seen <- r.URL.Path:
		default:
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "tunnelled")
	})
}

// liveSessionClient is enrolledClientWithDevice with a tunnel handler set, so
// the answered session has something to serve. The handler travels on Config
// because the client is what receives the offer: by the time the wait loop has
// an envelope in hand there is no caller left to pass one in, which is the
// difference between this path and EstablishForDevice.
func liveSessionClient(t *testing.T, rv *sessionRV, devECDHPub []byte, h http.Handler) (*Client, string) {
	t.Helper()
	cfg := r3ClientCfg(t, rv.URL(), rv.HTTPClient())
	cfg.TunnelHandler = h
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

	rec, err := c.RegisterDevice(ctx, DeviceRecord{
		ID:      "phone",
		PubKey:  append([]byte(nil), testPhonePubKey...),
		ECDHPub: append([]byte(nil), devECDHPub...),
	})
	if err != nil {
		t.Fatalf("register device: %v", err)
	}
	return c, rec.RID
}

// TestLiveSessionCarriesATunnelledRequest is the join. A device posts a sealed
// offer, receives the sealed answer, completes ICE against the computer's
// retained peer, and gets a response from the tunnel handler.
//
// Every earlier row in this package stops at the answer *value*. This is the
// first that requires the answer to describe something reachable.
func TestLiveSessionCarriesATunnelledRequest(t *testing.T) {
	rv := newSessionRV(t)
	devPriv, devPub := deviceKeypair(t)
	seen := make(chan string, 1)
	c, rid := liveSessionClient(t, rv, devPub, tunnelEcho(seen))

	ctx, cancel := ctxTO(t)
	defer cancel()

	dev := newDevicePeer(t, ctx)
	x, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatalf("computer ECDH public: %v", err)
	}
	sealed, err := SealEnvelope(dev.offer, x, DefaultOrigin, rid)
	if err != nil {
		t.Fatalf("seal offer: %v", err)
	}
	rv.QueueEnvelope(rid, sealed)

	answer, err := OpenEnvelope(awaitReply(t, rv, 8*time.Second), devPriv.Bytes(), DefaultOrigin, rid)
	if err != nil {
		t.Fatalf("device could not open the computer's reply: %v", err)
	}
	if err := dev.applyAnswer(ctx, answer); err != nil {
		t.Fatalf("the device could not connect to the answered peer; "+
			"the computer answered and then discarded the connection: %v", err)
	}

	resp, err := dev.roundTrip(ctx, "/api/nodes")
	if err != nil {
		t.Fatalf("tunnelled round trip: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("tunnelled status = %d, want 200", resp.Status)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "tunnelled" {
		t.Fatalf("tunnelled body = %q, want %q", body, "tunnelled")
	}
	select {
	case p := <-seen:
		if p != "/api/nodes" {
			t.Fatalf("handler saw path %q, want /api/nodes", p)
		}
	default:
		t.Fatal("the handler was never reached")
	}
}

// TestLiveSessionRefusesASubstitutedFingerprint is FR-16 on the path that will
// carry traffic. The offer's SDP is untouched and parses; only the sealed
// fingerprint disagrees with it. checkInner accepts that today, so the computer
// answers a description whose DTLS identity it never verified.
//
// The refusal must be total: no reply at all. Answering and then failing later
// would still have handed the far side a usable answer.
func TestLiveSessionRefusesASubstitutedFingerprint(t *testing.T) {
	rv := newSessionRV(t)
	_, devPub := deviceKeypair(t)
	seen := make(chan string, 1)
	c, rid := liveSessionClient(t, rv, devPub, tunnelEcho(seen))

	ctx, cancel := ctxTO(t)
	defer cancel()

	dev := newDevicePeer(t, ctx)
	bad := dev.offer
	bad.Fingerprint = tamperedFingerprint

	x, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatalf("computer ECDH public: %v", err)
	}
	sealed, err := SealEnvelope(bad, x, DefaultOrigin, rid)
	if err != nil {
		t.Fatalf("seal offer: %v", err)
	}
	rv.QueueEnvelope(rid, sealed)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rv.DeliveredCount(rid) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rv.DeliveredCount(rid) == 0 {
		t.Fatal("the rendezvous never served the tampered offer; the loop is not waiting")
	}

	served := rv.WaitCount()
	time.Sleep(500 * time.Millisecond)
	if rs := rv.Replies(); len(rs) != 0 {
		t.Fatalf("the computer answered an offer whose sealed fingerprint does not match its SDP; "+
			"FR-16 is enforced on the in-process path only (%d reply/replies)", len(rs))
	}
	if rv.WaitCount() <= served {
		t.Fatal("the wait loop stopped after a refused offer; a refusal must not wedge it")
	}
}

// TestLiveSessionRefusalIsNotATimeout keeps the FR-16 failure legible. A
// substitution reported as a transport fault sends the operator hunting for a
// network problem instead of a substituting rendezvous (AT-FR-16-b).
func TestLiveSessionRefusalIsNotATimeout(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()

	dev := newDevicePeer(t, ctx)
	bad := dev.offer
	bad.Fingerprint = tamperedFingerprint

	s, _, err := acceptSessionOffer(ctx, bad, tunnelEcho(nil), nil)
	if s != nil {
		_ = s.Close()
		t.Fatal("a substituted fingerprint produced a retained session")
	}
	if err == nil {
		t.Fatal("a substituted fingerprint was accepted")
	}
	requireClass(t, err, ClassFingerprint)
	if g := strings.ToLower(guidanceOf(err)); strings.Contains(g, "timeout") {
		t.Fatalf("substitution must refuse, not time out: %v", err)
	}
}

// TestRevokeClosesTheLiveSession is FR-29 reaching the wire. Revocation
// already closes whatever is registered as the device's Channel; this asserts
// the answered session is registered, so revoking actually severs traffic
// rather than only editing a record.
func TestRevokeClosesTheLiveSession(t *testing.T) {
	rv := newSessionRV(t)
	devPriv, devPub := deviceKeypair(t)
	seen := make(chan string, 1)
	c, rid := liveSessionClient(t, rv, devPub, tunnelEcho(seen))

	ctx, cancel := ctxTO(t)
	defer cancel()

	dev := newDevicePeer(t, ctx)
	x, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatalf("computer ECDH public: %v", err)
	}
	sealed, err := SealEnvelope(dev.offer, x, DefaultOrigin, rid)
	if err != nil {
		t.Fatalf("seal offer: %v", err)
	}
	rv.QueueEnvelope(rid, sealed)

	answer, err := OpenEnvelope(awaitReply(t, rv, 8*time.Second), devPriv.Bytes(), DefaultOrigin, rid)
	if err != nil {
		t.Fatalf("open reply: %v", err)
	}
	if err := dev.applyAnswer(ctx, answer); err != nil {
		t.Fatalf("device could not connect: %v", err)
	}
	if _, err := dev.roundTrip(ctx, "/api/nodes"); err != nil {
		t.Fatalf("round trip before revoke: %v", err)
	}

	if err := c.RevokeDevice(ctx, "phone"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// The device's channel must be gone. Polling rather than asserting once:
	// the teardown crosses a real SCTP association, so the edge is not
	// instantaneous — but it must arrive, and quickly.
	closed := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if dev.dc.ReadyState() != webrtc.DataChannelStateOpen {
			closed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !closed {
		t.Fatal("the device's data channel is still open after revoke; " +
			"the live session is not registered as that device's channel")
	}
}
