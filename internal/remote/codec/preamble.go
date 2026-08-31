package codec

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// The channel preamble (tunnel-v2 §2.2.1).
//
//	'S' 'C' 'M' 'X'   // 4 bytes, magic
//	major  uint16 BE  // 2 bytes
//	minor  uint16 BE  // 2 bytes
//
// Eight bytes, written unprompted by both sides the instant the data
// channel opens, before any frame. There is no round trip and no state
// machine: each side reads the other's preamble and decides alone.
//
// The preamble is frozen. It will never gain a field, because a field in
// it would need a version to say how to parse the version, and that
// recursion has no base case. Everything that grows lives in the hello
// frame (§5.1), which is versioned by this preamble and extensible by
// records.
//
// 'S' is 0x53, above every frame type this or any major version assigns,
// so a peer that opens with a frame is identifiable rather than merely
// wrong. A byte-swapped major reads as 512 and fails the range check at
// byte 4 — that is a sanity gate, not endianness negotiation. The wire is
// big-endian, always, and there is no byte-order negotiation.
const (
	// ProtocolMajor breaks compatibility when it changes. A peer at a
	// different major is refused by name (§2.5), never silently.
	ProtocolMajor uint16 = 2
	// ProtocolMinor is additive and informational. A differing minor MUST
	// NOT be refused, warned about, or escalated to the user (§2.3).
	ProtocolMinor uint16 = 0

	preambleSize = 8
)

var preambleMagic = [4]byte{'S', 'C', 'M', 'X'}

// VersionError reasons.
const (
	// VersionNoPreamble means the first bytes were not a preamble at all:
	// a foreign protocol, a major-1 frame, or silence.
	VersionNoPreamble = "no-preamble"
	// VersionUnsupportedMajor means the preamble parsed and named a major
	// this build cannot speak.
	VersionUnsupportedMajor = "unsupported-major"
)

// VersionError is a failed negotiation. It is deliberately not a
// RejectError: FR-24 renders it as tunnel-version-mismatch, a named state
// that says which side is behind, never a decode error or a blank page.
type VersionError struct {
	Reason string
	// PeerMajor and PeerMinor are meaningful only when HavePeer is true.
	PeerMajor uint16
	PeerMinor uint16
	HavePeer  bool
}

func (e *VersionError) Error() string {
	if e == nil {
		return "codec: version mismatch"
	}
	if !e.HavePeer {
		return fmt.Sprintf("codec: %s (peer did not send a tunnel preamble)", e.Reason)
	}
	return fmt.Sprintf("codec: %s (peer speaks tunnel %d.%d, this build speaks %d.%d)",
		e.Reason, e.PeerMajor, e.PeerMinor, ProtocolMajor, ProtocolMinor)
}

// PeerIsBehind reports whether the peer is the older side. Both ends hold
// the other's preamble, so both can say which one needs updating.
func (e *VersionError) PeerIsBehind() bool {
	return e != nil && e.HavePeer && e.PeerMajor < ProtocolMajor
}

func encodePreamble() []byte {
	b := make([]byte, 0, preambleSize)
	b = append(b, preambleMagic[:]...)
	b = binary.BigEndian.AppendUint16(b, ProtocolMajor)
	return binary.BigEndian.AppendUint16(b, ProtocolMinor)
}

// decodePreamble reads exactly preambleSize bytes and no more. The bytes
// after it are the peer's first frame, so over-reading here would eat it.
func decodePreamble(r io.Reader) (major, minor uint16, err error) {
	var buf [preambleSize]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, 0, &VersionError{Reason: VersionNoPreamble}
	}
	if !bytes.Equal(buf[:4], preambleMagic[:]) {
		return 0, 0, &VersionError{Reason: VersionNoPreamble}
	}
	major = binary.BigEndian.Uint16(buf[4:6])
	minor = binary.BigEndian.Uint16(buf[6:8])
	if major != ProtocolMajor {
		// Minor is never gating (§2.3); major always is (§2.5). The peer's
		// numbers ride along so the caller can say which side is behind
		// rather than only that something is wrong.
		return major, minor, &VersionError{
			Reason:    VersionUnsupportedMajor,
			PeerMajor: major,
			PeerMinor: minor,
			HavePeer:  true,
		}
	}
	return major, minor, nil
}
