package codec

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"
)

type v5CodecFailWriter struct {
	conn       *Conn
	registered bool
}

func (w *v5CodecFailWriter) Write([]byte) (int, error) {
	w.conn.mu.Lock()
	w.registered = len(w.conn.pending) == 1
	w.conn.mu.Unlock()
	return 0, io.ErrClosedPipe
}
func TestRoundTripForgetsCallAfterWriteFailure(t *testing.T) {
	w := &v5CodecFailWriter{}
	c := NewConn(strings.NewReader(""), w, RoleInitiator)
	w.conn = c
	// Isolate the post-negotiation request writer from the independent read loop.
	c.handshakeOnce.Do(func() {})
	c.loopOnce.Do(func() {})
	defer c.Close()
	_, err := c.RoundTrip(context.Background(), &Request{ID: "synthetic", Method: "GET", Path: "/api/state"})
	if err != io.ErrClosedPipe {
		t.Fatalf("write error = %v", err)
	}
	if !w.registered {
		t.Fatal("write failed before registration")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) != 0 {
		t.Fatal("failed write left a pending call")
	}
}

func TestReadLoopDoesNotReadAfterClose(t *testing.T) {
	for _, server := range []bool{false, true} {
		r := strings.NewReader("unread synthetic input")
		c := NewConn(r, io.Discard, RoleInitiator)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if err := c.readLoop(context.Background(), server); err != nil {
			t.Fatalf("closed connection (server=%v): %v", server, err)
		}
		if r.Len() != len("unread synthetic input") {
			t.Fatal("closed connection consumed input")
		}
	}
}

func TestFramePayloadReadPreservesTransportError(t *testing.T) {
	// A complete header followed by a transport error is not a truncated
	// peer frame: callers need the original error to diagnose the channel.
	r := io.MultiReader(bytes.NewReader([]byte{typeRequest, 0, 0, 1}), iotest.ErrReader(io.ErrClosedPipe))
	frame, err := decodeFrame(r)
	if frame != nil || err != io.ErrClosedPipe {
		t.Fatalf("payload transport error: frame=%v err=%v", frame, err)
	}
}

type earlyResponseWriter struct {
	conn *Conn
	fail bool
	once sync.Once
}

func (w *earlyResponseWriter) Write(data []byte) (int, error) {
	w.once.Do(func() {
		if w.fail {
			w.conn.failAll(io.ErrClosedPipe)
			return
		}
		w.conn.mu.Lock()
		defer w.conn.mu.Unlock()
		for _, call := range w.conn.pending {
			call.respCh <- respOrErr{resp: &Response{Status: 204, Body: io.NopCloser(strings.NewReader(""))}}
		}
	})
	return len(data), nil
}

type observedUpload struct {
	io.ReadCloser
	done chan struct{}
}

func (r observedUpload) Close() error {
	err := r.ReadCloser.Close()
	close(r.done)
	return err
}

func TestRoundTripReturnsEarlyResponseWhileUploadIsBlocked(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "error"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				body, input := io.Pipe()
				defer body.Close()
				defer input.Close()
				c := NewConn(strings.NewReader(""), io.Discard, RoleInitiator)
				c.w = &earlyResponseWriter{conn: c, fail: fail}
				c.handshakeOnce.Do(func() {})
				c.loopOnce.Do(func() {})
				defer c.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				uploadDone := make(chan struct{})
				// Keep the upload blocked until RoundTrip has returned. A response must
				// win the outer select, never race with a completed body or the clock.
				response, err := c.RoundTrip(ctx, &Request{
					ID: "synthetic-early-response", Method: "POST", Path: "/api/nodes/synthetic/send", Body: observedUpload{ReadCloser: body, done: uploadDone},
				})
				if fail {
					if response != nil || err != io.ErrClosedPipe {
						t.Fatalf("peer error during upload: response=%v err=%v", response, err)
					}
				} else {
					if err != nil || response == nil || response.Status != 204 {
						t.Fatalf("early response: response=%v err=%v", response, err)
					}
					_ = response.Body.Close()
				}
				_ = input.CloseWithError(io.ErrClosedPipe)
				select {
				case <-uploadDone:
				case <-ctx.Done():
					t.Fatal("returned request did not close its upload")
				}
				// The production drain has no public completion callback. Quiescence
				// proves it has consumed the body result before this test ends.
				synctest.Wait()
				_ = c.Close()
				c.mu.Lock()
				defer c.mu.Unlock()
				if len(c.pending) != 0 {
					t.Fatal("closed connection retained a pending request")
				}
			})
		})
	}
}
