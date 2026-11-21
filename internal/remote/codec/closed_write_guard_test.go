package codec

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestWriteFrameAfterCloseDoesNotWrite(t *testing.T) {
	var output bytes.Buffer
	conn := NewConn(strings.NewReader(""), &output, RoleInitiator)
	if err := conn.writeFrame(typeBody, []byte("synthetic")); err != nil {
		t.Fatalf("write before close: %v", err)
	}
	before := append([]byte(nil), output.Bytes()...)
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := conn.writeFrame(typeBody, []byte("later")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write after close: %v, want closed pipe", err)
	}
	if !bytes.Equal(output.Bytes(), before) {
		t.Fatal("write after close changed transport bytes")
	}
}
