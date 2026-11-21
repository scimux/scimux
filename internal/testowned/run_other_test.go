//go:build !linux || testowned_unsupported

package testowned

import (
	"io"
	"os"
	"testing"
	"time"
)

func TestUnsupportedFailsBeforeLaunch(t *testing.T) {
	result := Run([]string{"synthetic"})
	if result.Clean || result.Category != "prerequisite" {
		t.Fatalf("unsupported result %+v", result)
	}
}

func TestUnsupportedClosesTransferredHandle(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	result := RunWithHandles([]string{"synthetic"}, Handles{ExtraFiles: []*os.File{writer}})
	if result.Clean || result.Category != "prerequisite" {
		t.Fatalf("result %+v", result)
	}
	if err := reader.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := reader.Read(b[:]); err != io.EOF {
		t.Fatalf("transferred handle remains open: %v", err)
	}
}

func TestUnsupportedBrowserClosesTransferredHandle(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	result := RunBrowserWithHandles([]string{"synthetic"}, Handles{ExtraFiles: []*os.File{writer}})
	if result.Clean || result.Category != "prerequisite" {
		t.Fatalf("result %+v", result)
	}
	if err := reader.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := reader.Read(b[:]); err != io.EOF {
		t.Fatalf("transferred handle remains open: %v", err)
	}
}
