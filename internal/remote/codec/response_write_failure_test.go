package codec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type responseBodyFailureWriter struct {
	accepted bytes.Buffer
	writes   int
	failed   []byte
}

func (w *responseBodyFailureWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 3 {
		w.failed = append([]byte(nil), p...)
		return 0, errors.New("synthetic body-frame write failure")
	}
	return w.accepted.Write(p)
}

type responseBodyReadCounter struct {
	reads  int
	closes int
}

func (b *responseBodyReadCounter) Read(p []byte) (int, error) {
	b.reads++
	if b.reads == 1 {
		return copy(p, "synthetic response body"), nil
	}
	return 0, io.EOF
}

func (b *responseBodyReadCounter) Close() error {
	b.closes++
	return nil
}

func TestWriteOutgoingResponseStopsAfterBodyFrameWriteFailure(t *testing.T) {
	writer := &responseBodyFailureWriter{}
	body := &responseBodyReadCounter{}
	conn := NewConn(strings.NewReader(""), writer, RoleInitiator)
	conn.writeOutgoingResponse(context.Background(), "synthetic", "GET", "/api/state", &Response{Status: 200, Body: body})

	if writer.writes != 3 || len(writer.failed) != frameHeaderSize || writer.failed[0] != typeBody {
		t.Fatalf("writes=%d failed frame header=%x; want response header and payload followed by one body-frame header", writer.writes, writer.failed)
	}
	if body.reads != 1 || body.closes != 1 {
		t.Fatalf("body reads=%d closes=%d; want one read and one close", body.reads, body.closes)
	}
	frame, err := decodeFrame(&writer.accepted)
	if err != nil || frame.typ != typeResponse {
		t.Fatalf("accepted response frame=%v err=%v", frame, err)
	}
	id, status, _, err := decodeResponsePayload(frame.payload)
	if err != nil || id != "synthetic" || status != 200 {
		t.Fatalf("response id=%q status=%d err=%v", id, status, err)
	}
	if writer.accepted.Len() != 0 {
		t.Fatalf("unexpected bytes after response header: %x", writer.accepted.Bytes())
	}
}
