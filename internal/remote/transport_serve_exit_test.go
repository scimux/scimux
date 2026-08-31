package remote

// When Serve returns, the session is over. Leaving it open is the FR-24
// lie: ChannelLive is still true, TransportCause short-circuits to
// "connected, nothing wrong", and a peer that sends a request waits out
// its own deadline. Closing on that exit is not enough on its own —
// Close drives the peer connection to Closed, and OnConnectionStateChange
// would otherwise overwrite a tunnel-version verdict with
// connected-then-lost. These rows pin the cause that survives.

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"codeberg.org/chrberger/scimux/internal/remote/codec"
)

func TestServeExitWrongMajorClosesTheSession(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()

	waitClosed := watchConnectionClosed(t)
	s, dev := serveExitLive(t, ctx)
	writeDevicePreamble(t, dev, serveExitPreamble([4]byte{'S', 'C', 'M', 'X'}, codec.ProtocolMajor+1, 0))
	waitServeDone(t, ctx, s)
	waitClosed(ctx)

	if s.ChannelLive() {
		t.Errorf("ChannelLive() is true after Serve returned; the session still looks connected")
	}
	got, err := s.TransportCause()
	if err != nil {
		t.Fatalf("TransportCause() error = %v, want nil", err)
	}
	if got != CauseTunnelVersionMismatch {
		t.Fatalf("TransportCause() = %q, want %q (not %q, not empty)",
			got, CauseTunnelVersionMismatch, CauseConnectedThenLost)
	}
}

func TestServeExitNonTunnelPeerClosesTheSession(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()

	s, dev := serveExitLive(t, ctx)
	writeDevicePreamble(t, dev, []byte("NOTSCMUX"))
	waitServeDone(t, ctx, s)

	if s.ChannelLive() {
		t.Errorf("ChannelLive() is true after Serve returned; the session still looks connected")
	}

	// A hang waits out ctxTO (10s). This deadline is a fraction of that,
	// so a session that stays open fails this row rather than passing slowly.
	short, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	_, err := dev.roundTrip(short, "/api/nodes")
	if err == nil {
		t.Fatal("device round trip succeeded after Serve returned")
	}
	if short.Err() != nil {
		t.Fatalf("device round trip hung until the short deadline; the session was still accepting: %v", err)
	}
}

func TestServeExitInProcessClosesTheSession(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()

	s, err := InProcessTunnel(ctx, tunnelEcho(nil))
	if err != nil {
		t.Fatalf("InProcessTunnel: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// The in-process client stream is the client end of the channel.
	// A refused handshake makes Serve return without itself closing the
	// data channels, so ChannelLive can go false only if the session
	// Close on that goroutine ran.
	if _, err := s.clientStream.Write([]byte("NOTSCMUX")); err != nil {
		t.Fatalf("client handshake write: %v", err)
	}
	waitServeDone(t, ctx, s)

	if s.ChannelLive() {
		t.Errorf("ChannelLive() is true after Serve returned; the session still looks connected")
	}
	got, causeErr := s.TransportCause()
	if got == "" && causeErr == nil {
		t.Fatal(`TransportCause() = "", nil; the session still reports itself connected`)
	}
}

// serveExitLive answers a real pion device and waits until the data
// channel is open, then returns both halves so the test can write a
// handshake the protocol will refuse.
func serveExitLive(t *testing.T, ctx context.Context) (*Session, *devicePeer) {
	t.Helper()
	dev := newDevicePeer(t, ctx)
	s, answer, err := acceptSessionOffer(ctx, dev.offer, tunnelEcho(nil), nil)
	if s != nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	if err != nil {
		t.Fatalf("acceptSessionOffer: %v", err)
	}
	if err := dev.applyAnswer(ctx, answer); err != nil {
		t.Fatalf("device could not connect: %v", err)
	}
	return s, dev
}

// serveExitPreamble is an 8-byte §2.2.1 preamble built from a literal
// magic and explicit big-endian version words, not from the codec's
// encoder. A test that used encodePreamble could not lie about the major.
func serveExitPreamble(magic [4]byte, major, minor uint16) []byte {
	p := make([]byte, 8)
	copy(p, magic[:])
	binary.BigEndian.PutUint16(p[4:6], major)
	binary.BigEndian.PutUint16(p[6:8], minor)
	return p
}

func writeDevicePreamble(t *testing.T, dev *devicePeer, p []byte) {
	t.Helper()
	if _, err := dev.stream.Write(p); err != nil {
		t.Fatalf("device handshake write: %v", err)
	}
}

func waitServeDone(t *testing.T, ctx context.Context, s *Session) {
	t.Helper()
	select {
	case <-s.serveDone:
	case <-ctx.Done():
		t.Fatal("serve goroutine did not return")
	}
}

// watchConnectionClosed installs afterConnectionStateChange so a test
// can wait for pion's Closed callback. Close() returns, and serveDone
// closes, before that callback runs; waiting on it is what makes the
// cause-preservation guard observable.
func watchConnectionClosed(t *testing.T) func(context.Context) {
	t.Helper()
	seen := make(chan struct{})
	var once sync.Once
	afterConnectionStateMu.Lock()
	orig := afterConnectionStateChange
	afterConnectionStateChange = func(st webrtc.PeerConnectionState) {
		orig(st)
		if st == webrtc.PeerConnectionStateClosed {
			once.Do(func() { close(seen) })
		}
	}
	afterConnectionStateMu.Unlock()
	t.Cleanup(func() {
		afterConnectionStateMu.Lock()
		afterConnectionStateChange = orig
		afterConnectionStateMu.Unlock()
	})
	return func(ctx context.Context) {
		t.Helper()
		select {
		case <-seen:
		case <-ctx.Done():
			t.Fatal("OnConnectionStateChange never saw Closed")
		}
	}
}
