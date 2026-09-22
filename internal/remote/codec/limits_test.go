package codec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func rawRequests(t *testing.T, ids ...string) []byte {
	t.Helper()
	in := append([]byte(nil), handshakeBytes(t, RoleInitiator)...)
	var frames bytes.Buffer
	for _, id := range ids {
		payload, err := encodeRequestPayload(id, "POST", "/api/nodes/n1/send", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeRawFrame(&frames, typeRequest, payload); err != nil {
			t.Fatal(err)
		}
	}
	return append(in, frames.Bytes()...)
}

func blockingBodyHandler(_ context.Context, req *Request) (*Response, error) {
	_, err := io.ReadAll(req.Body)
	return nil, err
}

func TestServeRejectsDuplicateLiveRequestID(t *testing.T) {
	var out bytes.Buffer
	c := NewConn(bytes.NewReader(rawRequests(t, "same", "same")), &out, RoleResponder)
	err := c.Serve(t.Context(), HandlerFunc(blockingBodyHandler))
	var rej *RejectError
	if !errors.As(err, &rej) || rej.Class != ClassMalformed {
		t.Fatalf("Serve duplicate ID error = %v, want %s", err, ClassMalformed)
	}
}

func TestServeCapsConcurrentRequestsPerChannel(t *testing.T) {
	ids := make([]string, maxInFlightRequests+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("request-%d", i)
	}
	var out bytes.Buffer
	c := NewConn(bytes.NewReader(rawRequests(t, ids...)), &out, RoleResponder)
	err := c.Serve(t.Context(), HandlerFunc(blockingBodyHandler))
	var rej *RejectError
	if !errors.As(err, &rej) || rej.Class != ClassMalformed {
		t.Fatalf("Serve over in-flight limit error = %v, want %s", err, ClassMalformed)
	}
}

func TestIncompleteRequestBodyExpires(t *testing.T) {
	serverRead, peerWrite := io.Pipe()
	peerRead, serverWrite := io.Pipe()
	c := NewConn(serverRead, serverWrite, RoleResponder)
	c.bodyTimeout = 20 * time.Millisecond
	serveErr := make(chan error, 1)
	bodyErr := make(chan error, 1)
	go func() {
		serveErr <- c.Serve(context.Background(), HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
			_, err := io.ReadAll(req.Body)
			bodyErr <- err
			return nil, err
		}))
	}()
	if err := peerHandshake(peerRead, peerWrite, RoleInitiator); err != nil {
		t.Fatal(err)
	}
	payload, err := encodeRequestPayload("slow", "POST", "/api/nodes/n1/send", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRawFrame(peerWrite, typeRequest, payload); err != nil {
		t.Fatal(err)
	}
	fr, err := decodeFrame(peerRead)
	if err != nil {
		t.Fatal(err)
	}
	if fr.typ != typeReject {
		t.Fatalf("expiry frame type = %d, want reject", fr.typ)
	}
	_, class, _, err := decodeRejectPayload(fr.payload)
	if err != nil || class != ClassTruncated {
		t.Fatalf("expiry rejection = %q, %v; want %q", class, err, ClassTruncated)
	}
	select {
	case err := <-bodyErr:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("handler body error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handler remained blocked on expired body")
	}
	_ = peerWrite.Close()
	select {
	case <-serveErr:
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop after peer close")
	}
	_ = c.Close()
}

func TestServeEOFClosesIncompleteBodiesBeforeWaiting(t *testing.T) {
	var out bytes.Buffer
	seen := make(chan error, 1)
	c := NewConn(bytes.NewReader(rawRequests(t, "unfinished")), &out, RoleResponder)
	done := make(chan error, 1)
	go func() {
		done <- c.Serve(context.Background(), HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
			_, err := io.ReadAll(req.Body)
			seen <- err
			return nil, err
		}))
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Serve waited forever for a handler blocked on an incomplete body")
	}
	select {
	case err := <-seen:
		if err == nil {
			t.Fatal("handler body unexpectedly ended cleanly")
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not observe channel cleanup")
	}
}

func TestServeEOFUnblocksResponseWriteBeforeWaiting(t *testing.T) {
	serverRead, peerWrite := io.Pipe()
	peerRead, serverWrite := io.Pipe()
	c := NewConn(serverRead, serverWrite, RoleResponder)
	done := make(chan error, 1)
	go func() {
		done <- c.Serve(context.Background(), HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
			_, _ = io.ReadAll(req.Body)
			return &Response{Status: 200, Body: io.NopCloser(bytes.NewReader([]byte("response")))}, nil
		}))
	}()
	if err := peerHandshake(peerRead, peerWrite, RoleInitiator); err != nil {
		t.Fatal(err)
	}
	payload, err := encodeRequestPayload("response", "GET", "/api/state", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRawFrame(peerWrite, typeRequest, payload); err != nil {
		t.Fatal(err)
	}
	if err := writeRawFrame(peerWrite, typeBodyEnd, mustID("response")); err != nil {
		t.Fatal(err)
	}
	// Stop the request side and deliberately never consume the response. Serve
	// must close its response writer before waiting for the handler goroutine.
	_ = peerWrite.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Serve waited forever for a handler blocked writing a response")
	}
	_ = peerRead.Close()
}

func TestCloseDuringRequestAdmissionDoesNotPublishAfterClose(t *testing.T) {
	c := NewConn(bytes.NewReader(nil), io.Discard, RoleResponder)
	var handlerCalls atomic.Int32
	c.handler = HandlerFunc(func(context.Context, *Request) (*Response, error) {
		handlerCalls.Add(1)
		return nil, nil
	})
	payload, err := encodeRequestPayload("closing", "GET", "/api/state", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the admission mutex until Close has marked the connection closed.
	// acceptRequest must then refuse to publish request state, a timer, or a
	// handler after failAll has performed its cleanup.
	c.mu.Lock()
	admitStarted := make(chan struct{})
	admitDone := make(chan error, 1)
	go func() {
		close(admitStarted)
		admitDone <- c.acceptRequest(context.Background(), payload)
	}()
	<-admitStarted
	closeDone := make(chan struct{})
	go func() {
		_ = c.Close()
		close(closeDone)
	}()
	deadline := time.Now().Add(time.Second)
	for !c.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !c.closed.Load() {
		c.mu.Unlock()
		t.Fatal("Close did not mark the connection closed")
	}
	c.mu.Unlock()

	select {
	case err := <-admitDone:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("acceptRequest error = %v, want closed pipe", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request admission remained blocked")
	}
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close remained blocked")
	}
	c.mu.Lock()
	incoming := len(c.incoming)
	c.mu.Unlock()
	if incoming != 0 {
		t.Fatalf("incoming requests after close = %d, want 0", incoming)
	}
	if got := handlerCalls.Load(); got != 0 {
		t.Fatalf("handler calls after close = %d, want 0", got)
	}
}
