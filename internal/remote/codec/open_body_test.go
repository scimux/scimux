package codec

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestHandlerReturnClosesRequestBodyLeftOpen drives the handler-exit path
// that closes a request body the peer never ended. The body timeout and the
// connection teardown must not win that race: the handler observes the body
// still open, returns, and only then is the request retired.
func TestHandlerReturnClosesRequestBodyLeftOpen(t *testing.T) {
	serverRead, peerWrite := io.Pipe()
	peerRead, serverWrite := io.Pipe()
	c := NewConn(serverRead, serverWrite, RoleResponder)
	c.bodyTimeout = time.Hour

	entered := make(chan struct{})
	release := make(chan struct{})
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- c.Serve(context.Background(), HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
			c.mu.Lock()
			inf := c.incoming[req.ID]
			open := inf != nil && inf.bodyOpen
			c.mu.Unlock()
			if !open {
				t.Errorf("request %s was not open inside the handler", req.ID)
				return nil, io.ErrClosedPipe
			}
			close(entered)
			<-release
			// A nil response returns before any frame write. Writing a
			// response here would block on the unbuffered pipe and the
			// handler would still be inside ServeRemote.
			return nil, nil
		}))
	}()

	if err := peerHandshake(peerRead, peerWrite, RoleInitiator); err != nil {
		t.Fatal(err)
	}
	payload, err := encodeRequestPayload("open-body", http.MethodPost, "/api/nodes/n1/send", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRawFrame(peerWrite, typeRequest, payload); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not observe the open request body")
	}
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		_, live := c.incoming["open-body"]
		c.mu.Unlock()
		if !live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("handler exit left the open request registered")
		}
		time.Sleep(time.Millisecond)
	}

	// The request is already retired. Closing the peer ends the read loop on
	// a partial frame, which this codec reports as truncation.
	_ = peerWrite.Close()
	select {
	case err := <-serveErr:
		var rej *RejectError
		if err != nil && err != io.EOF && err != io.ErrClosedPipe && (!errors.As(err, &rej) || rej.Class != ClassTruncated) {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after the peer closed")
	}
}
