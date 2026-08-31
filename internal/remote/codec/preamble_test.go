package codec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// tunnel-v2 §2.2.1. The preamble is the first eight bytes each side
// writes to the channel, and it is frozen: it will never gain a field,
// because a field in it would need a version to say how to parse the
// version. These tests are the freeze.

func wantVersionError(t *testing.T, err error, reason string) *VersionError {
	t.Helper()
	var ve *VersionError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want *VersionError", err, err)
	}
	if ve.Reason != reason {
		t.Fatalf("reason = %q, want %q", ve.Reason, reason)
	}
	return ve
}

func rawPreamble(magic string, major, minor uint16) []byte {
	b := []byte(magic)
	return append(b,
		byte(major>>8), byte(major),
		byte(minor>>8), byte(minor),
	)
}

// The exact bytes, spelled out rather than derived from the constants
// they are meant to pin. scimux-rv decodes these; a test that recomputed
// them from the same constants the encoder uses would agree with any
// change, including a wrong one.
func TestPreambleIsTheFrozenEightBytes(t *testing.T) {
	got := encodePreamble()
	want := []byte{'S', 'C', 'M', 'X', 0x00, 0x02, 0x00, 0x00}
	if !bytes.Equal(got, want) {
		t.Fatalf("preamble = % x, want % x", got, want)
	}
	if len(got) != preambleSize {
		t.Fatalf("preambleSize = %d, encoded %d bytes", preambleSize, len(got))
	}
}

// §2.2.1: 'S' is 0x53, which is not a frame type in any major version.
// A peer that opens with a frame rather than a preamble is therefore
// identifiable rather than merely wrong.
func TestPreambleMagicCannotBeMistakenForAFrameType(t *testing.T) {
	first := encodePreamble()[0]
	if first <= typeReject {
		t.Fatalf("magic starts 0x%02x, which collides with the frame types (highest 0x%02x)",
			first, typeReject)
	}
}

func TestPreambleRoundTrips(t *testing.T) {
	major, minor, err := decodePreamble(bytes.NewReader(encodePreamble()))
	if err != nil {
		t.Fatal(err)
	}
	if major != ProtocolMajor || minor != ProtocolMinor {
		t.Fatalf("decoded %d.%d, want %d.%d", major, minor, ProtocolMajor, ProtocolMinor)
	}
}

func TestPreambleWithoutMagicIsAVersionFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"wrong magic", rawPreamble("XMCS", 2, 0)},
		{"a v1 request frame", []byte{0x00, 0x00, 0x00, 0x0a, 1, 2, 3, 4}},
		{"raw HTTP", []byte("GET /api")},
		{"all zeroes", make([]byte, preambleSize)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := decodePreamble(bytes.NewReader(tc.in))
			ve := wantVersionError(t, err, VersionNoPreamble)
			if ve.HavePeer {
				t.Error("HavePeer is true for a peer whose preamble never parsed")
			}
		})
	}
}

func TestPreambleTruncatedIsAVersionFailure(t *testing.T) {
	full := encodePreamble()
	for n := 0; n < preambleSize; n++ {
		_, _, err := decodePreamble(bytes.NewReader(full[:n]))
		wantVersionError(t, err, VersionNoPreamble)
	}
}

// §2.5: both ends hold the other's preamble, so both can say which side
// is behind. That is the whole reason the version is on the wire.
func TestPreambleNamesWhichSideIsBehind(t *testing.T) {
	t.Run("peer is older", func(t *testing.T) {
		_, _, err := decodePreamble(bytes.NewReader(rawPreamble("SCMX", ProtocolMajor-1, 0)))
		ve := wantVersionError(t, err, VersionUnsupportedMajor)
		if !ve.HavePeer {
			t.Fatal("HavePeer is false although the preamble parsed")
		}
		if !ve.PeerIsBehind() {
			t.Error("PeerIsBehind() is false for a lower major")
		}
		if ve.PeerMajor != ProtocolMajor-1 {
			t.Errorf("PeerMajor = %d", ve.PeerMajor)
		}
	})

	t.Run("we are older", func(t *testing.T) {
		_, _, err := decodePreamble(bytes.NewReader(rawPreamble("SCMX", ProtocolMajor+1, 0)))
		ve := wantVersionError(t, err, VersionUnsupportedMajor)
		if !ve.HavePeer {
			t.Fatal("HavePeer is false although the preamble parsed")
		}
		if ve.PeerIsBehind() {
			t.Error("PeerIsBehind() is true for a higher major")
		}
	})
}

// Major 1 was specified and replaced before any user ran it (§2.3). It
// is not a compatibility branch; it is a version that gets named and
// refused like any other.
func TestPreambleMajorOneIsRefused(t *testing.T) {
	_, _, err := decodePreamble(bytes.NewReader(rawPreamble("SCMX", 1, 0)))
	ve := wantVersionError(t, err, VersionUnsupportedMajor)
	if !ve.PeerIsBehind() {
		t.Error("a major-1 peer is behind us")
	}
}

// §2.2.1: this is a sanity gate, not endianness negotiation. A
// byte-swapped major reads as 512 and dies at byte 4 instead of
// allocating from a misread uint24 further down the stream.
func TestPreambleByteSwappedMajorIsRefused(t *testing.T) {
	swapped := []byte{'S', 'C', 'M', 'X', 0x02, 0x00, 0x00, 0x00}
	_, _, err := decodePreamble(bytes.NewReader(swapped))
	ve := wantVersionError(t, err, VersionUnsupportedMajor)
	if ve.PeerMajor != 512 {
		t.Fatalf("PeerMajor = %d, want 512 — the whole point is that a swap is out of range", ve.PeerMajor)
	}
}

// §2.3: minor is informational, never gating. A peer MUST NOT refuse,
// warn, or change behaviour because the other side's minor differs —
// that is the "MINOR must not escalate to the user" half of the semver
// contract, expressed at the only place it could leak.
func TestPreambleMinorDifferenceIsNeverAFailure(t *testing.T) {
	for _, minor := range []uint16{0, 1, 7, 0xffff} {
		major, got, err := decodePreamble(bytes.NewReader(rawPreamble("SCMX", ProtocolMajor, minor)))
		if err != nil {
			t.Fatalf("minor %d refused: %v", minor, err)
		}
		if major != ProtocolMajor || got != minor {
			t.Fatalf("decoded %d.%d, want %d.%d", major, got, ProtocolMajor, minor)
		}
	}
}

func TestPreambleReadsExactlyEightBytesAndNoMore(t *testing.T) {
	trailer := []byte("this is the first frame's bytes")
	r := bytes.NewReader(append(encodePreamble(), trailer...))
	if _, _, err := decodePreamble(r); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rest, trailer) {
		t.Fatalf("decodePreamble consumed past the preamble: %q left", rest)
	}
}

// --- handshake on a live Conn --------------------------------------------

// §2.2: preamble, then exactly one hello, before anything else. This is
// what a conforming peer sees arriving.
func TestHandshakeWritesPreambleThenHello(t *testing.T) {
	var out bytes.Buffer
	c := NewConn(bytes.NewReader(handshakeBytes(t, RoleResponder)), &out, RoleInitiator)
	if err := c.Handshake(t.Context()); err != nil {
		t.Fatal(err)
	}

	raw := out.Bytes()
	if len(raw) < preambleSize {
		t.Fatalf("wrote %d bytes, less than a preamble", len(raw))
	}
	if !bytes.Equal(raw[:preambleSize], encodePreamble()) {
		t.Fatalf("first %d bytes are not the preamble: % x", preambleSize, raw[:preambleSize])
	}
	fr, err := decodeFrame(bytes.NewReader(raw[preambleSize:]))
	if err != nil {
		t.Fatalf("frame after the preamble: %v", err)
	}
	if fr.typ != typeHello {
		t.Fatalf("first frame is type 0x%02x, want hello 0x%02x", fr.typ, typeHello)
	}
}

func TestHandshakeIsIdempotent(t *testing.T) {
	var out bytes.Buffer
	c := NewConn(bytes.NewReader(handshakeBytes(t, RoleResponder)), &out, RoleInitiator)
	if err := c.Handshake(t.Context()); err != nil {
		t.Fatal(err)
	}
	n := out.Len()
	for i := 0; i < 3; i++ {
		if err := c.Handshake(t.Context()); err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
	}
	if out.Len() != n {
		t.Fatalf("a repeated Handshake wrote %d more bytes", out.Len()-n)
	}
}

// §7 step 0: no request frame may be sent before the handshake. A
// RoundTrip on an unhandshaken Conn must complete the handshake first,
// not race it.
func TestRoundTripRefusesWhenTheHandshakeFails(t *testing.T) {
	var out bytes.Buffer
	// A peer that opens with a v1 frame instead of a preamble.
	peer := []byte{0x00, 0x00, 0x00, 0x00}
	c := NewConn(bytes.NewReader(peer), &out, RoleInitiator)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := c.RoundTrip(ctx, validGET("r1", "/api/state"))
	wantVersionError(t, err, VersionNoPreamble)

	// And nothing beyond our own handshake reached the wire: a request
	// frame sent before negotiation is exactly what §2.2 forbids.
	if got := out.Len(); got > preambleSize+frameHeaderSize+64 {
		t.Fatalf("wrote %d bytes on a failed handshake; a request escaped", got)
	}
	if bytes.Contains(out.Bytes()[preambleSize:], []byte("/api/state")) {
		t.Fatal("the request path reached the wire before negotiation succeeded")
	}
}

func TestServeRefusesWhenTheHandshakeFails(t *testing.T) {
	var out bytes.Buffer
	c := NewConn(bytes.NewReader([]byte("GET / HTTP/1.1\r\n")), &out, RoleResponder)
	err := c.Serve(t.Context(), HandlerFunc(func(context.Context, *Request) (*Response, error) {
		t.Error("the handler ran on a channel that never negotiated")
		return nil, nil
	}))
	wantVersionError(t, err, VersionNoPreamble)
}

// A peer that says nothing is a version failure, not a hang. It is the
// one place in this protocol where a deadline applies (§7), because
// there is nothing yet to cancel and silence is indistinguishable from
// a peer that will never speak.
func TestHandshakeDeadlineIsAVersionFailure(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	var out bytes.Buffer
	c := NewConn(pr, &out, RoleInitiator)

	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	err := c.Handshake(ctx)
	if err == nil {
		t.Fatal("a silent peer completed the handshake")
	}
	wantVersionError(t, err, VersionNoPreamble)
}
