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
