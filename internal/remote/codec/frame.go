package codec

import (
	"io"
	"net/http"
)

// Wire format (one frame), tunnel-v2 §3:
//
//	type   uint8     // see the constants below
//	length uint24 BE // payload bytes that follow
//	payload [length]byte
//
// The payload is a sequence of records (§4, tlv.go), which share this
// header's four-byte shape. Frame types above the highest assigned one
// are frames this build predates: read whole, then dropped by the
// dispatch, which is what makes a MINOR addition additive.
//
// Type 0x00 is the one exception to skip-the-unknown. It is permanently
// reserved, so no future MINOR can assign it and rejecting it costs no
// extensibility; and without the rule a zero-filled or padded buffer
// decodes as "type 0x00, length 0" — a valid empty frame, consumed
// silently, forever.
//
// A body stream is zero or more typeBody frames followed by typeBodyEnd
// (the end marker is a distinct type, not a zero-length chunk).
//
// The 4-byte header is what AT-remote-codec-c pins. Garbage is judged by
// the length prefix, so it reads as truncated rather than malformed:
//
//	empty / 1 byte → truncated (header incomplete)
//	0xff 0xff 0xff → truncated (type 0xff, then 2 of 3 length bytes)
//	raw HTTP       → truncated (type 'G', length "ET " is over the cap)
const (
	typeReserved uint8 = 0x00
	typeHello    uint8 = 0x01
	typeRequest  uint8 = 0x02
	typeResponse uint8 = 0x03
	typeBody     uint8 = 0x04
	typeBodyEnd  uint8 = 0x05
	typeCancel   uint8 = 0x06
	typeReject   uint8 = 0x07
)

const (
	frameHeaderSize = 4
	maxFramePayload = (64 << 10) + 1024
	bodyChunkSize   = 32 << 10
)

type frame struct {
	typ     uint8
	payload []byte
}

// isKnownType reports whether the type byte is one this version has an
// opinion about. Deliberately not a validity test: tunnel-v2 §2.1 lets a
// MINOR bump add frame types, and this binary is the half that goes stale
// (rv is deployed once and reaches every browser; scimux sits on many
// computers at many versions). The dispatch switches in conn.go ignore what
// they do not recognise, which is what makes an addition additive.
//
// typeReserved is excluded: it never becomes known, because decodeFrame
// refuses it outright.
func isKnownType(t uint8) bool {
	return t != typeReserved && t <= typeReject
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
	if typ == typeReserved {
		// §3.1's single exception to skip-the-unknown. 0x00 is permanently
		// reserved, so refusing it forecloses nothing a future MINOR could
		// have used; and without the refusal a zero-filled or padded
		// buffer decodes as "type 0x00, length 0" — a valid empty frame,
		// consumed silently, forever.
		return nil, reject(ClassMalformed, "")
	}
	// No rejection on any other type byte. Rejecting there tore down the
	// connection *and* desynced the stream, because the length prefix
	// below had not been read yet: one frame type added in a MINOR bump
	// would have broken every computer older than it. Every frame is
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

// writeRaw writes bytes that are not a frame. The preamble is the only
// such thing in this protocol, and it exists precisely to precede framing.
func (c *Conn) writeRaw(b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return io.ErrClosedPipe
	}
	_, err := c.w.Write(b)
	return err
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

// encodeHeaders writes one nested header record per name (§5.8): a
// name record and one value record per value. No count anywhere — the
// enclosing record's length ends the list.
func encodeHeaders(b []byte, h http.Header) ([]byte, error) {
	for name, vals := range h {
		inner, err := putRecordString(nil, tagHeaderName, name)
		if err != nil {
			return nil, err
		}
		for _, v := range vals {
			inner, err = putRecordString(inner, tagHeaderValue, v)
			if err != nil {
				return nil, err
			}
		}
		b, err = putRecord(b, tagHeader, inner)
		if err != nil {
			return nil, err
		}
	}
	return b, nil
}

// decodeHeader reads one nested header record into h. A repeated name
// record is last-wins (§4, scalars); repeated value records are the
// multi-valued case.
func decodeHeader(h http.Header, val []byte) error {
	name := ""
	// Do not preallocate from anything the peer said: there is no count to
	// preallocate from, which is the point.
	vals := make([]string, 0)
	err := eachRecord(val, func(tag uint8, v []byte) error {
		switch tag {
		case tagHeaderName:
			name = string(v)
		case tagHeaderValue:
			vals = append(vals, string(v))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if name == "" {
		// A header record with no name is not a header. It is not
		// tolerated as an unknown field either: the record's tag is one
		// this build knows, so its contents are this build's business.
		return reject(ClassMalformed, "")
	}
	h[name] = vals
	return nil
}

func encodeRequestPayload(id, method, path, query string, h http.Header) ([]byte, error) {
	b := make([]byte, 0, 64)
	var err error
	if b, err = putRecordString(b, tagID, id); err != nil {
		return nil, err
	}
	if b, err = putRecordString(b, tagMethod, method); err != nil {
		return nil, err
	}
	if b, err = putRecordString(b, tagPath, path); err != nil {
		return nil, err
	}
	if b, err = putRecordString(b, tagQuery, query); err != nil {
		return nil, err
	}
	return encodeHeaders(b, h)
}

func decodeRequestPayload(p []byte) (id, method, path, query string, h http.Header, err error) {
	h = make(http.Header)
	err = eachRecord(p, func(tag uint8, val []byte) error {
		switch tag {
		case tagID:
			id = string(val)
		case tagMethod:
			method = string(val)
		case tagPath:
			path = string(val)
		case tagQuery:
			query = string(val)
		case tagHeader:
			return decodeHeader(h, val)
		}
		// Every other tag is a field a later MINOR added. Skipping it is
		// what makes that addition additive rather than a MAJOR break.
		return nil
	})
	return
}

func encodeResponsePayload(id string, status int, h http.Header) ([]byte, error) {
	if !validHTTPStatus(status) {
		return nil, reject(ClassMalformed, "")
	}
	b := make([]byte, 0, 64)
	var err error
	if b, err = putRecordString(b, tagID, id); err != nil {
		return nil, err
	}
	if b, err = putRecordU16(b, tagStatus, uint16(status)); err != nil {
		return nil, err
	}
	return encodeHeaders(b, h)
}

func decodeResponsePayload(p []byte) (id string, status int, h http.Header, err error) {
	h = make(http.Header)
	seenStatus := false
	err = eachRecord(p, func(tag uint8, val []byte) error {
		switch tag {
		case tagID:
			id = string(val)
		case tagStatus:
			st, sErr := recordU16(val)
			if sErr != nil {
				return sErr
			}
			status = int(st)
			seenStatus = true
		case tagHeader:
			return decodeHeader(h, val)
		}
		return nil
	})
	if err != nil {
		return
	}
	if !seenStatus || !validHTTPStatus(status) {
		err = reject(ClassMalformed, "")
	}
	return
}

func validHTTPStatus(s int) bool {
	return s >= 100 && s <= 599
}

func encodeID(id string) ([]byte, error) {
	return putRecordString(nil, tagID, id)
}

// encodeBodyPayload is a body chunk: the stream's id and its data (§5.4).
func encodeBodyPayload(id string, data []byte) ([]byte, error) {
	b, err := putRecordString(nil, tagID, id)
	if err != nil {
		return nil, err
	}
	return putRecord(b, tagData, data)
}

// decodeIDPayload returns the id and, for a body frame, its data. Body-end
// and cancel carry no data record, so data is empty for them.
func decodeIDPayload(p []byte) (id string, data []byte, err error) {
	err = eachRecord(p, func(tag uint8, val []byte) error {
		switch tag {
		case tagID:
			id = string(val)
		case tagData:
			data = val
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return id, data, nil
}

func encodeRejectPayload(id string, class Class, field string) ([]byte, error) {
	b, err := putRecordString(nil, tagID, id)
	if err != nil {
		return nil, err
	}
	if b, err = putRecordString(b, tagClass, string(class)); err != nil {
		return nil, err
	}
	return putRecordString(b, tagField, field)
}

func decodeRejectPayload(p []byte) (id string, class Class, field string, err error) {
	err = eachRecord(p, func(tag uint8, val []byte) error {
		switch tag {
		case tagID:
			id = string(val)
		case tagClass:
			class = Class(val)
		case tagField:
			field = string(val)
		}
		return nil
	})
	return
}
