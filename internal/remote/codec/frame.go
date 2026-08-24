package codec

import (
	"encoding/binary"
	"io"
	"net/http"
)

// Wire format (one frame):
//
//	type   uint8     // 0x00 request, 0x01 response, 0x02 body, 0x03 body-end,
//	                 // 0x04 cancel, 0x05 reject. Anything else is a frame
//	                 // this build predates: read whole, then dropped by the
//	                 // dispatch (tunnel-v1 §2.1, see isKnownType).
//	length uint24 BE // payload bytes that follow
//	payload [length]byte
//
// A body stream is zero or more typeBody frames followed by typeBodyEnd
// (the end marker is a distinct type, not a zero-length chunk).
//
// The 4-byte header is what AT-remote-codec-c pins. Since the type byte
// stopped being a verdict, garbage is judged by the length prefix, so it
// reads as truncated rather than malformed:
//
//	empty / 1 byte → truncated (header incomplete)
//	0xff 0xff 0xff → truncated (type 0xff, then 2 of 3 length bytes)
//	raw HTTP       → truncated (type 'G', length "ET " is over the cap)
//	00 00 10 00 01 → truncated (type 0x00, length 0x001000, 1 payload byte)
const (
	typeRequest  uint8 = 0x00
	typeResponse uint8 = 0x01
	typeBody     uint8 = 0x02
	typeBodyEnd  uint8 = 0x03
	typeCancel   uint8 = 0x04
	typeReject   uint8 = 0x05
)

const (
	frameHeaderSize = 4
	maxFramePayload = (64 << 10) + 1024
	bodyChunkSize   = 32 << 10
	maxString       = 1 << 16
)

type frame struct {
	typ     uint8
	payload []byte
}

// isKnownType reports whether the type byte is one this version has an
// opinion about. Deliberately not a validity test: tunnel-v1 §2.1 lets a
// MINOR bump add frame types, and this binary is the half that goes stale
// (rv is deployed once and reaches every browser; scimux sits on many
// laptops at many versions). The dispatch switches in conn.go ignore what
// they do not recognise, which is what makes an addition additive.
func isKnownType(t uint8) bool {
	return t <= typeReject
}

func decodeFrame(r io.Reader) (*frame, error) {
	var hdr [frameHeaderSize]byte
	n, err := io.ReadFull(r, hdr[:1])
	if n == 0 && (err == io.EOF || err == io.ErrUnexpectedEOF) {
		return nil, reject(ClassTruncated, "")
	}
	if err != nil && n == 0 {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, reject(ClassTruncated, "")
		}
		return nil, err
	}
	typ := hdr[0]
	// No rejection on the type byte. Rejecting here tore down the
	// connection *and* desynced the stream, because the length prefix
	// below had not been read yet: one frame type added in a MINOR bump
	// would have broken every laptop older than it. Every frame is
	// length-prefixed, so an unknown one is skipped whole instead —
	// decodeFrame reads it, the dispatch drops it.
	if _, err := io.ReadFull(r, hdr[1:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, reject(ClassTruncated, "")
		}
		return nil, err
	}
	length := uint32(hdr[1])<<16 | uint32(hdr[2])<<8 | uint32(hdr[3])
	if length == 0 {
		return &frame{typ: typ}, nil
	}
	if length > maxFramePayload {
		copied, copyErr := io.CopyN(io.Discard, r, int64(length))
		if copied < int64(length) || copyErr == io.EOF || copyErr == io.ErrUnexpectedEOF {
			return nil, reject(ClassTruncated, "")
		}
		if copyErr != nil {
			return nil, copyErr
		}
		return nil, reject(ClassMalformed, "")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, reject(ClassTruncated, "")
		}
		return nil, err
	}
	return &frame{typ: typ, payload: payload}, nil
}

func (c *Conn) writeFrame(typ uint8, payload []byte) error {
	if len(payload) > 0xffffff {
		return reject(ClassMalformed, "")
	}
	var hdr [frameHeaderSize]byte
	hdr[0] = typ
	n := len(payload)
	hdr[1] = byte(n >> 16)
	hdr[2] = byte(n >> 8)
	hdr[3] = byte(n)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return io.ErrClosedPipe
	}
	if _, err := c.w.Write(hdr[:]); err != nil {
		return err
	}
	if n > 0 {
		if _, err := c.w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func putU16(b []byte, v int) []byte {
	var tmp [2]byte
	binary.BigEndian.PutUint16(tmp[:], uint16(v))
	return append(b, tmp[:]...)
}

func putBytes(b []byte, s []byte) ([]byte, error) {
	if len(s) >= maxString {
		return nil, reject(ClassMalformed, "")
	}
	b = putU16(b, len(s))
	return append(b, s...), nil
}

func putString(b []byte, s string) ([]byte, error) {
	return putBytes(b, []byte(s))
}

func takeU16(p []byte) (uint16, []byte, error) {
	if len(p) < 2 {
		return 0, nil, reject(ClassMalformed, "")
	}
	return binary.BigEndian.Uint16(p[:2]), p[2:], nil
}

func takeBytes(p []byte) ([]byte, []byte, error) {
	n, rest, err := takeU16(p)
	if err != nil {
		return nil, nil, err
	}
	if int(n) > len(rest) {
		return nil, nil, reject(ClassMalformed, "")
	}
	return rest[:n], rest[n:], nil
}

func takeString(p []byte) (string, []byte, error) {
	b, rest, err := takeBytes(p)
	if err != nil {
		return "", nil, err
	}
	return string(b), rest, nil
}

func encodeHeaders(b []byte, h http.Header) ([]byte, error) {
	if h == nil {
		return putU16(b, 0), nil
	}
	n := 0
	for range h {
		n++
	}
	b = putU16(b, n)
	for name, vals := range h {
		var err error
		b, err = putString(b, name)
		if err != nil {
			return nil, err
		}
		b = putU16(b, len(vals))
		for _, v := range vals {
			b, err = putString(b, v)
			if err != nil {
				return nil, err
			}
		}
	}
	return b, nil
}

func decodeHeaders(p []byte) (http.Header, []byte, error) {
	n, rest, err := takeU16(p)
	if err != nil {
		return nil, nil, err
	}
	// Do not size the map or value slices from a claimed count — a hostile
	// peer can claim 65535 headers and send two bytes.
	h := make(http.Header)
	for i := 0; i < int(n); i++ {
		name, r, err := takeString(rest)
		if err != nil {
			return nil, nil, err
		}
		nv, r, err := takeU16(r)
		if err != nil {
			return nil, nil, err
		}
		vals := make([]string, 0)
		for j := 0; j < int(nv); j++ {
			v, rr, err := takeString(r)
			if err != nil {
				return nil, nil, err
			}
			vals = append(vals, v)
			r = rr
		}
		h[name] = vals
		rest = r
	}
	return h, rest, nil
}

func encodeRequestPayload(id, method, path, query string, h http.Header) ([]byte, error) {
	b := make([]byte, 0, 64)
	var err error
	b, err = putString(b, id)
	if err != nil {
		return nil, err
	}
	b, err = putString(b, method)
	if err != nil {
		return nil, err
	}
	b, err = putString(b, path)
	if err != nil {
		return nil, err
	}
	b, err = putString(b, query)
	if err != nil {
		return nil, err
	}
	return encodeHeaders(b, h)
}

func decodeRequestPayload(p []byte) (id, method, path, query string, h http.Header, err error) {
	id, p, err = takeString(p)
	if err != nil {
		return
	}
	method, p, err = takeString(p)
	if err != nil {
		return
	}
	path, p, err = takeString(p)
	if err != nil {
		return
	}
	query, p, err = takeString(p)
	if err != nil {
		return
	}
	h, p, err = decodeHeaders(p)
	return
}

func encodeResponsePayload(id string, status int, h http.Header) ([]byte, error) {
	b := make([]byte, 0, 64)
	var err error
	b, err = putString(b, id)
	if err != nil {
		return nil, err
	}
	if !validHTTPStatus(status) {
		return nil, reject(ClassMalformed, "")
	}
	b = putU16(b, status)
	return encodeHeaders(b, h)
}

func decodeResponsePayload(p []byte) (id string, status int, h http.Header, err error) {
	id, p, err = takeString(p)
	if err != nil {
		return
	}
	st, p, err := takeU16(p)
	if err != nil {
		return
	}
	status = int(st)
	if !validHTTPStatus(status) {
		err = reject(ClassMalformed, "")
		return
	}
	h, _, err = decodeHeaders(p)
	return
}

func validHTTPStatus(s int) bool {
	return s >= 100 && s <= 599
}

func encodeID(id string) ([]byte, error) {
	return putString(nil, id)
}

func decodeIDPayload(p []byte) (string, []byte, error) {
	return takeString(p)
}

func encodeRejectPayload(id string, class Class, field string) ([]byte, error) {
	b, err := putString(nil, id)
	if err != nil {
		return nil, err
	}
	b, err = putString(b, string(class))
	if err != nil {
		return nil, err
	}
	return putString(b, field)
}

func decodeRejectPayload(p []byte) (id string, class Class, field string, err error) {
	id, p, err = takeString(p)
	if err != nil {
		return
	}
	var cs string
	cs, p, err = takeString(p)
	if err != nil {
		return
	}
	field, _, err = takeString(p)
	class = Class(cs)
	return
}
