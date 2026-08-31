package codec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
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

// peerHandshake plays the other end of tunnel-v2 §2.2: read the Conn's
// preamble and hello, then send our own. Every raw-frame peer in this
// file has to do it, because a Conn sends nothing at all until it has.
func peerHandshake(r io.Reader, w io.Writer, role uint8) error {
	if _, _, err := decodePreamble(r); err != nil {
		return err
	}
	fr, err := decodeFrame(r)
	if err != nil {
		return err
	}
	if fr.typ != typeHello {
		return io.ErrUnexpectedEOF
	}
	payload, err := encodeHelloPayload(Hello{
		Role:         role,
		MaxRecvFrame: maxFramePayload,
		Impl:         "hostile-peer",
	})
	if err != nil {
		return err
	}
	if _, err := w.Write(encodePreamble()); err != nil {
		return err
	}
	return writeRawFrame(w, typeHello, payload)
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
	client := NewConn(cr, cw, RoleInitiator)
	t.Cleanup(func() {
		_ = client.Close()
		_ = cr.Close()
		_ = cw.Close()
		_ = sr.Close()
		_ = sw.Close()
	})
	go func() {
		if err := peerHandshake(sr, sw, RoleResponder); err != nil {
			return
		}
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

// tunnel-v2 §2.1 and §5. This test used to assert the opposite — that an
// unknown class became ClassMalformed. That reading made every new
// rejection class a MAJOR bump, and told the operator the frame was
// corrupt when the peer had merely refused for a newer reason.
func TestReceiveUnknownRejectClassIsSurfacedVerbatim(t *testing.T) {
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
	wantReject(t, err, Class("not-a-real-class"))
	// The request still fails. Tolerating the class is not sanitising
	// the refusal into acceptance.
	if err == nil {
		t.Fatal("an unknown rejection class must still fail the request")
	}
}

// The class is peer-supplied text on its way into an error string, so
// tolerance stops at the token grammar.
func TestReceiveMisshapenRejectClassIsMalformed(t *testing.T) {
	for _, bad := range []string{
		"",
		"Not-Lowercase",
		"has space",
		"crlf\r\ninjected",
		"-leading",
		"trailing-",
		"under_score",
		strings.Repeat("a", maxRejectClassLen+1),
	} {
		t.Run(strconv.Quote(bad), func(t *testing.T) {
			client := hostilePair(t, func(sw *io.PipeWriter, id string) {
				payload, err := encodeRejectPayload(id, Class(bad), "x")
				if err != nil {
					// The encoder refuses some of these outright,
					// which is a stricter answer to the same question.
					return
				}
				_ = writeRawFrame(sw, typeReject, payload)
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			_, err := client.RoundTrip(ctx, validGET("req-hostile", "/api/state"))
			if err == nil {
				t.Fatal("a misshapen rejection class must not resolve the request")
			}
			var rej *RejectError
			if errors.As(err, &rej) && rej.Class != ClassMalformed {
				t.Fatalf("class %q surfaced as %q, want %q", bad, rej.Class, ClassMalformed)
			}
		})
	}
}

// tunnel-v2 §2.1: a frame type this build predates must be skipped
// whole, not rejected. Rejecting on the type byte also desynced the
// stream, because the length prefix had not been read yet — so the
// frames after it decoded from the wrong offset.
func TestReceiveUnknownFrameTypeIsSkippedWhole(t *testing.T) {
	client := hostilePair(t, func(sw *io.PipeWriter, id string) {
		// A type no version defines, carrying a payload that would
		// itself decode as plausible frames if the stream desynced.
		_ = writeRawFrame(sw, 0x42, bytes.Repeat([]byte{typeBodyEnd, 0, 0, 0}, 8))
		payload, err := encodeResponsePayload(id, http.StatusOK, http.Header{
			"Content-Type": {"application/json"},
		})
		if err != nil {
			return
		}
		_ = writeRawFrame(sw, typeResponse, payload)
		_ = writeRawFrame(sw, typeBodyEnd, mustID(id))
	})
	resp := mustRoundTrip(t, client, validGET("req-hostile", "/api/state"))
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if got := resp.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q; the stream desynced past the unknown frame", got)
	}
}

// The same, with the unknown frame arriving between the head and the
// body end: skipping must not disturb an in-flight response either.
func TestReceiveUnknownFrameTypeMidResponse(t *testing.T) {
	client := hostilePair(t, func(sw *io.PipeWriter, id string) {
		payload, err := encodeResponsePayload(id, http.StatusOK, http.Header{
			"Content-Type": {"text/plain"},
		})
		if err != nil {
			return
		}
		_ = writeRawFrame(sw, typeResponse, payload)
		body, err := encodeBodyPayload(id, []byte("hello"))
		if err != nil {
			return
		}
		_ = writeRawFrame(sw, typeBody, body)
		_ = writeRawFrame(sw, 0x7f, []byte("a frame from a newer peer"))
		_ = writeRawFrame(sw, typeBodyEnd, mustID(id))
	})
	resp := mustRoundTrip(t, client, validGET("req-hostile", "/api/state"))
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("body = %q, want %q", got, "hello")
	}
}

func TestReceiveRejectsOutOfRangeStatus(t *testing.T) {
	client := hostilePair(t, func(sw *io.PipeWriter, id string) {
		b, err := putRecordString(nil, tagID, id)
		if err != nil {
			return
		}
		b, err = putRecordU16(b, tagStatus, 700)
		if err != nil {
			return
		}
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
