package codec

import (
	"context"
	"io"
	"strings"
	"testing"
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
