package codec

import (
	"bytes"
	"context"
	"errors"
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

func TestClientIgnoresBodyForUnknownRequest(t *testing.T) {
	var output bytes.Buffer
	c := NewConn(strings.NewReader(""), &output, RoleInitiator)
	payload, err := encodeBodyPayload("synthetic-retired-request", []byte("late body"))
	if err != nil {
		t.Fatal(err)
	}
	c.acceptClientBody(payload)
	if len(c.pending) != 0 || output.Len() != 0 {
		t.Fatal("unsolicited body admitted a request or wrote to the peer")
	}
	// Close also proves the ignored frame released the connection lock.
	if err := c.Close(); err != nil {
		t.Fatal(err)
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

func TestClientReadFailureFailsPendingCallsAndStream(t *testing.T) {
	clientRead, peerWrite := io.Pipe()
	peerRead, clientWrite := io.Pipe()
	client := NewConn(clientRead, clientWrite, RoleInitiator)
	ctx, cancel := context.WithCancel(context.Background())
	peerDone := make(chan error, 1)
	inject := make(chan struct{})
	var release sync.Once
	var workers sync.WaitGroup
	readFailure := errors.New("synthetic transport read failure")
	t.Cleanup(func() {
		release.Do(func() { close(inject) })
		cancel()
		_ = client.Close()
		_ = peerRead.Close()
		_ = peerWrite.Close()
		select {
		case <-peerDone:
		case <-time.After(2 * time.Second):
			t.Error("synthetic peer did not stop")
		}
		stopped := make(chan struct{})
		go func() { workers.Wait(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Error("client workers did not stop")
		}
	})
	go func() {
		peerDone <- func() error {
			if err := peerHandshake(peerRead, peerWrite, RoleResponder); err != nil {
				return err
			}
			// Read all three completed requests before opening one response stream.
			for ends := 0; ends < 3; {
				fr, err := decodeFrame(peerRead)
				if err != nil {
					return err
				}
				if fr.typ == typeBodyEnd {
					ends++
				}
			}
			payload, err := encodeResponsePayload("stream", 200, nil)
			if err != nil {
				return err
			}
			if err := writeRawFrame(peerWrite, typeResponse, payload); err != nil {
				return err
			}
			<-inject
			return peerWrite.CloseWithError(readFailure)
		}()
	}()
	type result struct {
		response *Response
		err      error
	}
	results := make(map[string]chan result)
	for _, id := range []string{"first", "second", "stream"} {
		ch := make(chan result, 1)
		results[id] = ch
		workers.Add(1)
		go func(id string) {
			defer workers.Done()
			response, err := client.RoundTrip(ctx, &Request{ID: id, Method: "GET", Path: "/api/state"})
			ch <- result{response, err}
		}(id)
	}
	var stream *Response
	select {
	case got := <-results["stream"]:
		if got.err != nil || got.response == nil || got.response.Status != 200 {
			t.Fatalf("stream response: %+v", got)
		}
		stream = got.response
	case <-time.After(2 * time.Second):
		t.Fatal("stream response did not arrive")
	}
	defer stream.Body.Close()
	streamDone := make(chan error, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		_, err := io.ReadAll(stream.Body)
		streamDone <- err
	}()
	release.Do(func() { close(inject) })
	for _, id := range []string{"first", "second"} {
		select {
		case got := <-results[id]:
			if got.response != nil || !errors.Is(got.err, readFailure) {
				t.Errorf("%s call: response=%v err=%v", id, got.response, got.err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s call did not fail after transport read error", id)
		}
	}
	select {
	case err := <-streamDone:
		if !errors.Is(err, readFailure) {
			t.Errorf("stream read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("stream did not fail after transport read error")
	}
}
