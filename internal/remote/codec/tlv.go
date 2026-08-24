package codec

import "encoding/binary"

// Records (tunnel-v2 §4).
//
//	tag    uint8     // field number; 0x00 is permanently reserved
//	length uint24 BE // value bytes that follow
//	value  [length]byte
//
// Deliberately the same four-byte shape as a frame header, so one reader
// serves both layers and one bug can only be found once.
//
// Records are read to exhaustion. There is no count prefix anywhere in
// this protocol: a claimed count is a number a hostile peer picks and a
// decoder is tempted to allocate from, whereas the end of the enclosing
// value cannot be lied about.
//
// The rules that make a MINOR bump safe:
//   - an unknown tag is skipped by its length; the records around it
//     decode normally
//   - a repeated tag is a repeated field; scalars take the last one
//   - a zero-length record is present-and-empty, not absent
//   - values may themselves be records, at any depth
//
// The rules that are not tolerance:
//   - tag 0x00 is malformed (reserved, so refusing it costs no
//     extensibility, and it stops a zero-filled buffer decoding forever)
//   - truncation is malformed; a short tail is never "trailing bytes"
const recordHeaderSize = 4

// Tag numbers (§5). A tag number is permanent: never reused, never
// repurposed, never retyped. The same field keeps the same number
// wherever it appears, which is why id and header are shared.
const (
	tagID     uint8 = 1
	tagHeader uint8 = 5

	// request (§5.2)
	tagMethod uint8 = 2
	tagPath   uint8 = 3
	tagQuery  uint8 = 4

	// response (§5.3)
	tagStatus uint8 = 2

	// body (§5.4)
	tagData uint8 = 2

	// reject (§5.7)
	tagClass uint8 = 2
	tagField uint8 = 3

	// hello (§5.1)
	tagRole         uint8 = 1
	tagMaxRecvFrame uint8 = 2
	tagImpl         uint8 = 3

	// nested header record (§5.8)
	tagHeaderName  uint8 = 1
	tagHeaderValue uint8 = 2
)

type record struct {
	tag uint8
	val []byte
}

func putRecord(b []byte, tag uint8, val []byte) ([]byte, error) {
	if tag == 0 {
		return nil, reject(ClassMalformed, "")
	}
	n := len(val)
	if n > 0xffffff {
		return nil, reject(ClassMalformed, "")
	}
	b = append(b, tag, byte(n>>16), byte(n>>8), byte(n))
	return append(b, val...), nil
}

func putRecordString(b []byte, tag uint8, s string) ([]byte, error) {
	return putRecord(b, tag, []byte(s))
}

func putRecordU8(b []byte, tag uint8, v uint8) ([]byte, error) {
	return putRecord(b, tag, []byte{v})
}

func putRecordU16(b []byte, tag uint8, v uint16) ([]byte, error) {
	return putRecord(b, tag, binary.BigEndian.AppendUint16(nil, v))
}

func putRecordU32(b []byte, tag uint8, v uint32) ([]byte, error) {
	return putRecord(b, tag, binary.BigEndian.AppendUint32(nil, v))
}

// eachRecord walks p to exhaustion, calling fn for every record —
// including tags this build does not know, which is the caller's cue to
// step over them. fn's error stops the walk and is returned.
//
// val aliases p. Callers that keep it past the walk must copy.
func eachRecord(p []byte, fn func(tag uint8, val []byte) error) error {
	for len(p) > 0 {
		if len(p) < recordHeaderSize {
			return reject(ClassMalformed, "")
		}
		tag := p[0]
		if tag == 0 {
			return reject(ClassMalformed, "")
		}
		n := int(p[1])<<16 | int(p[2])<<8 | int(p[3])
		p = p[recordHeaderSize:]
		if n > len(p) {
			return reject(ClassMalformed, "")
		}
		// Non-nil even at length zero: §4 distinguishes present-and-empty
		// from absent, and a nil slice reads as absent to every caller.
		val := p[:n:n]
		if val == nil {
			val = []byte{}
		}
		if err := fn(tag, val); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// A fixed-width record must be exactly its width. Without this a peer
// could shorten a status to one byte and have it read as something else.
func recordU8(val []byte) (uint8, error) {
	if len(val) != 1 {
		return 0, reject(ClassMalformed, "")
	}
	return val[0], nil
}

func recordU16(val []byte) (uint16, error) {
	if len(val) != 2 {
		return 0, reject(ClassMalformed, "")
	}
	return binary.BigEndian.Uint16(val), nil
}

func recordU32(val []byte) (uint32, error) {
	if len(val) != 4 {
		return 0, reject(ClassMalformed, "")
	}
	return binary.BigEndian.Uint32(val), nil
}
