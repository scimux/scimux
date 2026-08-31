package remote

// Join 3, computer end. Join 2 proved a live data channel serves whatever
// Config names; internal/app's at_join3_test.go proves the product startup
// path names the real S3 boundary. What neither covers is the seam between
// them: the boundary is built *per device*, so the identity the answering
// path already proved has to survive as far as the factory. A bare
// http.Handler destroys it, which is why withTunnelBoundary's peer argument
// sat unused (`_ = peer`) for the whole of S3.

import (
	"crypto/ecdh"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// liveSessionClientFor is liveSessionClient with the per-peer factory instead
// of the bare handler. It is a separate helper rather than a parameter on that
// one because the rows already accepted around it must keep exercising the
// deprecated field: both paths have to work until it is removed.
func liveSessionClientFor(t *testing.T, rv *sessionRV, devECDHPub []byte, f func(TunnelPeer) http.Handler) (*Client, string) {
	t.Helper()
	cfg := r3ClientCfg(t, rv.URL(), rv.HTTPClient())
	cfg.TunnelHandlerFor = f
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

// sessionRoundTrip drives one device through offer, answer, ICE and a single
// tunnelled request, asserting only that the handler was reached. It is the
// body of TestLiveSessionCarriesATunnelledRequest with the identity assertions
// left to the caller.
func sessionRoundTrip(t *testing.T, rv *sessionRV, c *Client, rid string, devPriv *ecdh.PrivateKey, seen <-chan string) {
	t.Helper()
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
		t.Fatalf("the device could not connect to the answered peer: %v", err)
	}

	resp, err := dev.roundTrip(ctx, "/api/nodes")
	if err != nil {
		t.Fatalf("tunnelled round trip: %v", err)
	}
	_ = resp.Body.Close()
	if resp.Status != http.StatusOK {
		t.Fatalf("tunnelled status = %d, want 200", resp.Status)
	}
	select {
	case <-seen:
	default:
		t.Fatal("the handler was never reached")
	}
}

// handlerBody serves h once and returns what it wrote, so the precedence rows
// can tell two handlers apart by result rather than by pointer identity.
func handlerBody(t *testing.T, h http.Handler) string {
	t.Helper()
	if h == nil {
		t.Fatal("no handler was resolved")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec.Body.String()
}

// TestLiveSessionBuildsItsBoundaryForTheProvenPeer is the join-3 seam over a
// real data channel: the factory must be called with the device this session
// belongs to, and the handler it returns must be the one served.
func TestLiveSessionBuildsItsBoundaryForTheProvenPeer(t *testing.T) {
	rv := newSessionRV(t)
	devPriv, devPub := deviceKeypair(t)

	built := make(chan TunnelPeer, 4)
	seen := make(chan string, 1)
	factory := func(p TunnelPeer) http.Handler {
		select {
		case built <- p:
		default:
		}
		return tunnelEcho(seen)
	}

	c, rid := liveSessionClientFor(t, rv, devPub, factory)
	sessionRoundTrip(t, rv, c, rid, devPriv, seen)

	select {
	case p := <-built:
		if p.DeviceID != "phone" {
			t.Errorf("factory built a boundary for DeviceID %q, want %q", p.DeviceID, "phone")
		}
		if p.RID != rid {
			t.Errorf("factory built a boundary for RID %q, want the session's %q", p.RID, rid)
		}
	default:
		t.Fatal("a live session was served without the per-peer factory being consulted")
	}
}

// TestTunnelFactoryTakesPrecedenceOverTheBareHandler pins the migration. Both
// fields set must resolve to the factory, or an installation part-way through
// the change would silently serve a peer-less boundary.
func TestTunnelFactoryTakesPrecedenceOverTheBareHandler(t *testing.T) {
	bare := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "bare")
	})
	viaFactory := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "factory")
	})

	c := &Client{cfg: Config{
		TunnelHandler:    bare,
		TunnelHandlerFor: func(TunnelPeer) http.Handler { return viaFactory },
	}}
	if got := c.tunnelHandler(TunnelPeer{DeviceID: "phone", RID: "r"}); handlerBody(t, got) != "factory" {
		t.Fatal("the bare TunnelHandler won over TunnelHandlerFor")
	}

	// And the deprecated field still works alone, so join 2's rows keep their
	// meaning until it goes.
	only := &Client{cfg: Config{TunnelHandler: bare}}
	if got := only.tunnelHandler(TunnelPeer{}); handlerBody(t, got) != "bare" {
		t.Fatal("the deprecated TunnelHandler stopped being served")
	}

	// A factory that refuses this peer refuses the session rather than
	// falling back to a handler that never saw the peer at all.
	refuses := &Client{cfg: Config{
		TunnelHandler:    bare,
		TunnelHandlerFor: func(TunnelPeer) http.Handler { return nil },
	}}
	if got := refuses.tunnelHandler(TunnelPeer{}); got != nil {
		t.Fatal("a factory that refused a peer fell back to the bare handler")
	}
}
