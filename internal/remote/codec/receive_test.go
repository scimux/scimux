package codec

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func writeRawFrame(w io.Writer, typ uint8, payload []byte) error {
	var hdr [frameHeaderSize]byte
	hdr[0] = typ
	n := len(payload)
	hdr[1] = byte(n >> 16)
	hdr[2] = byte(n >> 8)
	hdr[3] = byte(n)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if n > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func consumeClientRequest(r io.Reader) error {
	sawReq := false
	for {
		fr, err := decodeFrame(r)
		if err != nil {
			return err
		}
		if fr.typ == typeRequest {
			sawReq = true
		}
		if fr.typ == typeBodyEnd {
			if !sawReq {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
	}
}

// hostilePair is a client Conn whose peer is the test, writing raw frames
// into sw after consuming the client's request. No product hook.
func hostilePair(t *testing.T, respond func(sw *io.PipeWriter, id string)) *Conn {
	t.Helper()
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	client := NewConn(cr, cw)
	t.Cleanup(func() {
		_ = client.Close()
		_ = cr.Close()
		_ = cw.Close()
		_ = sr.Close()
		_ = sw.Close()
	})
	go func() {
		if err := consumeClientRequest(sr); err != nil {
			return
		}
		respond(sw, "req-hostile")
	}()
	return client
}

func TestReceiveDropsUnlistedResponseHeaders(t *testing.T) {
	client := hostilePair(t, func(sw *io.PipeWriter, id string) {
		payload, err := encodeResponsePayload(id, http.StatusOK, http.Header{
			"Content-Type": {"application/json"},
			"X-Evil":       {"must-not-reach"},
		})
		if err != nil {
			return
		}
		_ = writeRawFrame(sw, typeResponse, payload)
		_ = writeRawFrame(sw, typeBodyEnd, mustID(id))
	})
	resp := mustRoundTrip(t, client, validGET("req-hostile", "/api/state"))
	if resp.Headers.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", resp.Headers.Get("Content-Type"))
	}
	if v := resp.Headers.Values("X-Evil"); len(v) > 0 {
		t.Errorf("unlisted response header reached the client: %q", v)
	}
}

func TestReceiveRejectsCRLFResponseHeader(t *testing.T) {
	client := hostilePair(t, func(sw *io.PipeWriter, id string) {
		payload, err := encodeResponsePayload(id, http.StatusOK, http.Header{
			"Content-Disposition": {"attachment; filename=\"x\r\nX-Injected: 1\""},
		})
		if err != nil {
			return
		}
		_ = writeRawFrame(sw, typeResponse, payload)
		_ = writeRawFrame(sw, typeBodyEnd, mustID(id))
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resp, err := client.RoundTrip(ctx, validGET("req-hostile", "/api/nodes/n1/assets/a1"))
	if err == nil && resp != nil && resp.Body != nil {
		_, err = io.Copy(io.Discard, resp.Body)
	}
	wantReject(t, err, ClassCRLF)
}

func TestReceiveUnknownRejectClassIsMalformed(t *testing.T) {
	client := hostilePair(t, func(sw *io.PipeWriter, id string) {
		payload, err := encodeRejectPayload(id, Class("not-a-real-class"), "x")
		if err != nil {
			return
		}
		_ = writeRawFrame(sw, typeReject, payload)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := client.RoundTrip(ctx, validGET("req-hostile", "/api/state"))
	wantReject(t, err, ClassMalformed)
}

func TestReceiveRejectsOutOfRangeStatus(t *testing.T) {
	client := hostilePair(t, func(sw *io.PipeWriter, id string) {
		b, err := putString(nil, id)
		if err != nil {
			return
		}
		b = putU16(b, 700)
		b = putU16(b, 0)
		_ = writeRawFrame(sw, typeResponse, b)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := client.RoundTrip(ctx, validGET("req-hostile", "/api/state"))
	wantReject(t, err, ClassMalformed)
}

// Cancel overlapping a response-header frame is the failCall/acceptResponse
// bodyW race. Repeating it gives -race a chance to see a missing lock.
func TestCancelDuringResponseHeaderNoRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		started := make(chan struct{})
		release := make(chan struct{})
		client := startPair(t, HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return &Response{
				ID:      req.ID,
				Status:  http.StatusOK,
				Headers: http.Header{"Content-Type": {"application/json"}},
				Body:    bodyOf(`{}`),
			}, nil
		}))
		ctx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		go func() {
			_, err := client.RoundTrip(ctx, validGET("req-race", "/api/state"))
			errc <- err
		}()
		select {
		case <-started:
		case err := <-errc:
			t.Fatalf("RoundTrip returned before handler started: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("handler did not start")
		}
		cancel()
		close(release)
		select {
		case <-errc:
		case <-time.After(2 * time.Second):
			t.Fatal("RoundTrip did not return after cancel")
		}
	}
}
