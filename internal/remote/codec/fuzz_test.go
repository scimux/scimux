package codec

import (
	"bytes"
	"testing"
)

// FuzzDecodeFrame is the S4 exit-criterion fuzz target on frame decoding.
// It must not panic on any input. A short run is `go test -fuzz=FuzzDecodeFrame
// -fuzztime=5s ./internal/remote/codec`.
func FuzzDecodeFrame(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte("GET /api/state HTTP/1.1\r\n\r\n"))
	f.Add([]byte{0x00, 0x00, 0x10, 0x00, 'x'})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = decodeFrame(bytes.NewReader(data))
	})
}

// FuzzDecodePayloads feeds the payload parsers that decodeFrame leaves
// opaque. Claimed lengths that exceed the remaining slice must not allocate.
func FuzzDecodePayloads(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00, 0x00})
	f.Add([]byte{0xff, 0xff})
	f.Add([]byte{0x00, 0x01, 'a'})
	f.Add([]byte{0xff, 0xff, 'x'})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _, _, _, _ = decodeRequestPayload(data)
		_, _, _, _ = decodeResponsePayload(data)
		_, _, _ = decodeHeaders(data)
		_, _, _, _ = decodeRejectPayload(data)
		_, _, _ = decodeIDPayload(data)
	})
}
