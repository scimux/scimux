package remote

// The computer's data-channel reader must be attached inside pion's
// OnDataChannel callback. pion's accept loop waits for that callback to
// return and only then starts the read loop (sctptransport.go:339-340), so
// a reader registered there cannot miss a byte. A reader attached later —
// after the channel has been handed off to a serve goroutine — can: pion
// discards any message that arrives while OnMessage is still nil
// (datachannel.go:320-333). The device's 8-byte preamble is the first
// thing on the wire; if it is dropped, the responder reads the hello frame
// header as a preamble, Serve returns no-preamble, and the device hangs
// until its own deadline.
//
// beforeChannelPickup is the stall that makes that late-reader schedule
// observable. It must stay.

import (
	"io"
	"net/http"
	"testing"
	"time"
)

func TestLateChannelPickupDoesNotDropThePreamble(t *testing.T) {
	orig := beforeChannelPickup
	beforeChannelPickup = func() { time.Sleep(250 * time.Millisecond) }
	t.Cleanup(func() { beforeChannelPickup = orig })

	ctx, cancel := ctxTO(t)
	defer cancel()

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

	resp, err := dev.roundTrip(ctx, "/api/nodes")
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "tunnelled" {
		t.Fatalf("body = %q, want %q", body, "tunnelled")
	}
}
