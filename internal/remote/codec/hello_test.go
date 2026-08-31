package codec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// tunnel-v2 §2.2.2 and §5.1. The hello carries what the frozen preamble
// deliberately cannot: capabilities, which grow by MINOR bump.

// handshakeBytes is what a conforming peer in the given role writes
// before its first real frame: preamble, then exactly one hello.
func handshakeBytes(t *testing.T, role uint8) []byte {
	t.Helper()
	payload, err := encodeHelloPayload(Hello{
		Role:         role,
		MaxRecvFrame: maxFramePayload,
		Impl:         "test-peer/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	out := encodePreamble()
	var buf bytes.Buffer
	if err := writeRawFrame(&buf, typeHello, payload); err != nil {
		t.Fatal(err)
	}
	return append(out, buf.Bytes()...)
}

func TestHelloRoundTrips(t *testing.T) {
	want := Hello{Role: RoleResponder, MaxRecvFrame: 12345, Impl: "scimux/9.9.9"}
	p, err := encodeHelloPayload(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeHelloPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("hello = %#v, want %#v", got, want)
	}
}

// §5.1: an unknown tag in a hello is exactly the MINOR-bump case the
// record layer exists for. A future capability must not break a computer
// that predates it.
func TestHelloIgnoresATagItDoesNotKnow(t *testing.T) {
	p, err := encodeHelloPayload(Hello{Role: RoleInitiator, MaxRecvFrame: 4096, Impl: "x"})
	if err != nil {
		t.Fatal(err)
	}
	p = append(p, rawRecord(99, []byte("a capability from the future"))...)

	got, err := decodeHelloPayload(p)
	if err != nil {
		t.Fatalf("an unknown hello tag was fatal: %v", err)
	}
	if got.Role != RoleInitiator || got.MaxRecvFrame != 4096 || got.Impl != "x" {
		t.Fatalf("known fields disturbed by an unknown tag: %#v", got)
	}
}

// §5.1: role makes a mis-wired pair fail immediately and by name. Two
// initiators on one channel is a bug that otherwise presents as silence.
func TestHelloRoleMismatchIsRefused(t *testing.T) {
	for _, role := range []uint8{RoleInitiator, RoleResponder} {
		var out bytes.Buffer
		// A peer claiming the same role we hold.
		c := NewConn(bytes.NewReader(handshakeBytes(t, role)), &out, role)
		err := c.Handshake(t.Context())
		if err == nil {
			t.Fatalf("role %d: two peers in the same role negotiated successfully", role)
		}
		var rej *RejectError
		if !errors.As(err, &rej) || rej.Class != ClassMalformed {
			t.Fatalf("role %d: err = %v, want %s", role, err, ClassMalformed)
		}
	}
}

func TestHelloRoleIsRejectedWhenItIsNeitherRole(t *testing.T) {
	p, err := encodeHelloPayload(Hello{Role: RoleInitiator, MaxRecvFrame: maxFramePayload})
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite the role record's value with a third role.
	p = append([]byte(nil), p...)
	p[recordHeaderSize] = 7
	if _, err := decodeHelloPayload(p); err == nil {
		t.Fatal("role 7 accepted; §5.1 defines only 0 and 1")
	}
}

// §5.1: max_recv_frame constrains the *peer's* encoder. Advertising a
// small number must actually make the other side send smaller frames —
// otherwise the field is decoration.
func TestHelloMaxRecvFrameClampsOurEncoder(t *testing.T) {
	const peerCap = 4096
	payload, err := encodeHelloPayload(Hello{Role: RoleResponder, MaxRecvFrame: peerCap, Impl: "small"})
	if err != nil {
		t.Fatal(err)
	}
	var peer bytes.Buffer
	if err := writeRawFrame(&peer, typeHello, payload); err != nil {
		t.Fatal(err)
	}
	in := append(encodePreamble(), peer.Bytes()...)

	var out bytes.Buffer
	c := NewConn(bytes.NewReader(in), &out, RoleInitiator)
	if err := c.Handshake(t.Context()); err != nil {
		t.Fatal(err)
	}

	body := bytes.Repeat([]byte{'z'}, 5*peerCap)
	if err := c.writeOutgoingBody(context.Background(), "b", bytes.NewReader(body), 1<<30); err != nil {
		t.Fatal(err)
	}

	raw := out.Bytes()[preambleSize:]
	r := bytes.NewReader(raw)
	sawBody := false
	for r.Len() > 0 {
		fr, err := decodeFrame(r)
		if err != nil {
			t.Fatal(err)
		}
		if fr.typ != typeBody {
			continue
		}
		sawBody = true
		if len(fr.payload) > peerCap {
			t.Fatalf("body frame payload is %d bytes; the peer advertised %d", len(fr.payload), peerCap)
		}
	}
	if !sawBody {
		t.Fatal("no body frames were written")
	}
}

// §5.1: a peer that advertises more than maxFramePayload does not raise
// this side's limit. Tolerance is not obedience.
func TestHelloCannotRaiseOurOwnLimit(t *testing.T) {
	payload, err := encodeHelloPayload(Hello{Role: RoleResponder, MaxRecvFrame: 1 << 24, Impl: "greedy"})
	if err != nil {
		t.Fatal(err)
	}
	var peer bytes.Buffer
	if err := writeRawFrame(&peer, typeHello, payload); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := NewConn(bytes.NewReader(append(encodePreamble(), peer.Bytes()...)), &out, RoleInitiator)
	if err := c.Handshake(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := c.maxSendFrame(); got != maxFramePayload {
		t.Fatalf("maxSendFrame = %d, want %d — a peer cannot raise our cap", got, maxFramePayload)
	}
}

// §5.1: a peer that omits the field is treated as advertising
// maxFramePayload. An absent scalar takes its zero value (§4), and a
// zero cap would otherwise mean "send nothing".
func TestHelloAbsentMaxRecvFrameMeansOurMaximum(t *testing.T) {
	p, err := putRecordU8(nil, tagRole, RoleResponder)
	if err != nil {
		t.Fatal(err)
	}
	var peer bytes.Buffer
	if err := writeRawFrame(&peer, typeHello, p); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := NewConn(bytes.NewReader(append(encodePreamble(), peer.Bytes()...)), &out, RoleInitiator)
	if err := c.Handshake(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := c.maxSendFrame(); got != maxFramePayload {
		t.Fatalf("maxSendFrame = %d, want %d", got, maxFramePayload)
	}
}

// §5.1: impl is free text and MUST NOT be parsed for behaviour. It is
// carried so a support conversation can establish what was talking to
// what, over an already SAS-paired channel.
func TestHelloImplIsCarriedButNeverParsed(t *testing.T) {
	for _, impl := range []string{"", "scimux/2.0.0", "🙂 unusual", "not/a/version at all"} {
		p, err := encodeHelloPayload(Hello{Role: RoleResponder, MaxRecvFrame: maxFramePayload, Impl: impl})
		if err != nil {
			t.Fatalf("impl %q: %v", impl, err)
		}
		got, err := decodeHelloPayload(p)
		if err != nil {
			t.Fatalf("impl %q: %v", impl, err)
		}
		if got.Impl != impl {
			t.Fatalf("impl = %q, want %q", got.Impl, impl)
		}
	}
}

// §2.2.2: exactly one hello per side per channel. A second one is
// malformed — it would otherwise be a way to renegotiate mid-stream,
// which §2.2 says requires a new channel.
func TestSecondHelloIsMalformed(t *testing.T) {
	in := handshakeBytes(t, RoleResponder)
	// Append a second, identical hello after the handshake.
	in = append(in, in[preambleSize:]...)

	var out bytes.Buffer
	c := NewConn(bytes.NewReader(in), &out, RoleInitiator)
	if err := c.Handshake(t.Context()); err != nil {
		t.Fatal(err)
	}
	err := c.Serve(t.Context(), HandlerFunc(func(context.Context, *Request) (*Response, error) {
		return nil, nil
	}))
	var rej *RejectError
	if !errors.As(err, &rej) || rej.Class != ClassMalformed {
		t.Fatalf("a second hello gave %v, want %s", err, ClassMalformed)
	}
}

// §2.2.2: the hello must be the *first* frame. A peer that leads with a
// request has not negotiated, and accepting it would defeat the point of
// negotiating before the first request.
func TestFirstFrameMustBeHello(t *testing.T) {
	payload, err := encodeRequestPayload("r1", "GET", "/api/state", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var peer bytes.Buffer
	if err := writeRawFrame(&peer, typeRequest, payload); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := NewConn(bytes.NewReader(append(encodePreamble(), peer.Bytes()...)), &out, RoleResponder)
	err = c.Handshake(t.Context())
	if err == nil {
		t.Fatal("a request frame was accepted as the hello")
	}
	var rej *RejectError
	if !errors.As(err, &rej) || rej.Class != ClassMalformed {
		t.Fatalf("err = %v, want %s", err, ClassMalformed)
	}
}

// A truncated hello is truncation, not tolerance (§4).
func TestTruncatedHelloIsNotSilentlyAccepted(t *testing.T) {
	full := handshakeBytes(t, RoleResponder)
	for n := preambleSize + 1; n < len(full); n++ {
		var out bytes.Buffer
		c := NewConn(bytes.NewReader(full[:n]), &out, RoleInitiator)
		if err := c.Handshake(t.Context()); err == nil {
			t.Fatalf("a hello cut at %d/%d bytes negotiated successfully", n, len(full))
		}
	}
}

func TestHelloPeerIsReadableAfterHandshake(t *testing.T) {
	var out bytes.Buffer
	c := NewConn(bytes.NewReader(handshakeBytes(t, RoleResponder)), &out, RoleInitiator)
	if _, ok := c.PeerHello(); ok {
		t.Fatal("PeerHello is available before the handshake")
	}
	if err := c.Handshake(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, ok := c.PeerHello()
	if !ok {
		t.Fatal("PeerHello is unavailable after a successful handshake")
	}
	if got.Role != RoleResponder || got.Impl != "test-peer/1" {
		t.Fatalf("peer hello = %#v", got)
	}
}

var _ io.Reader = (*bytes.Reader)(nil)
