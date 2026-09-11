package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/backend"
)

func TestStopCommandSuccessAndDataSelection(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, ".scimux")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	requested := make(chan struct{}, 1)
	s, err := backend.Listen(t.TempDir(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/_scimux/stop" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		requested <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	registration, err := backend.Register(data, s.Link())
	if err != nil {
		t.Fatal(err)
	}
	defer registration.Close()
	go func() {
		<-requested
		_ = registration.Close()
	}()

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if code := runStopCommand(ctx, nil, home, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if stdout.String() != "scimux: stopped\n" || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestStopCommandUsageAndFailures(t *testing.T) {
	home := t.TempDir()
	tests := []struct {
		name string
		args []string
		ctx  func() (context.Context, context.CancelFunc)
		want int
		text string
	}{
		{"help", []string{"-h"}, backgroundStopContext, 0, "Usage: scimux stop [options]"},
		{"unknown-flag", []string{"-wat"}, backgroundStopContext, 2, "flag provided but not defined"},
		{"positional", []string{"extra"}, backgroundStopContext, 2, "unexpected argument"},
		{"not-running", nil, backgroundStopContext, 1, "no running muxer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()
			var stdout, stderr bytes.Buffer
			if got := runStopCommand(ctx, tc.args, home, &stdout, &stderr); got != tc.want {
				t.Fatalf("exit = %d, want %d; stderr=%q", got, tc.want, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.text) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.text)
			}
		})
	}

	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "muxer.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if got := runStopCommand(context.Background(), []string{"-data", data}, home, io.Discard, &stderr); got != 1 || !strings.Contains(stderr.String(), "read muxer locator") {
		t.Fatalf("malformed locator: exit=%d stderr=%q", got, stderr.String())
	}
}

func TestRunStopMainHelp(t *testing.T) {
	var stderr bytes.Buffer
	if got := runStopMain([]string{"-h"}, io.Discard, &stderr); got != 0 {
		t.Fatalf("exit = %d, stderr=%q", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Usage: scimux stop [options]") {
		t.Fatalf("help = %q", stderr.String())
	}
}

func TestStopCommandRequestAndWaitFailures(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, ".scimux")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("request", func(t *testing.T) {
		registration, err := backend.Register(data, backend.Link{Socket: filepath.Join(t.TempDir(), "absent.sock"), Token: "cap"})
		if err != nil {
			t.Fatal(err)
		}
		defer registration.Close()
		var stderr bytes.Buffer
		if got := runStopCommand(context.Background(), nil, home, io.Discard, &stderr); got != 1 || !strings.Contains(stderr.String(), "request stop") {
			t.Fatalf("exit=%d stderr=%q", got, stderr.String())
		}
	})

	t.Run("wait", func(t *testing.T) {
		s, err := backend.Listen(t.TempDir(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		}))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		registration, err := backend.Register(data, s.Link())
		if err != nil {
			t.Fatal(err)
		}
		defer registration.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		var stderr bytes.Buffer
		if got := runStopCommand(ctx, nil, home, io.Discard, &stderr); got != 1 || !strings.Contains(stderr.String(), context.DeadlineExceeded.Error()) {
			t.Fatalf("exit=%d stderr=%q", got, stderr.String())
		}
	})
}

func TestWaitForMuxerStop(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	link := backend.Link{Socket: "/tmp/old.sock", Token: "old"}
	registration, err := backend.Register(data, link)
	if err != nil {
		t.Fatal(err)
	}
	defer registration.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForMuxerStop(ctx, data, link); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
	if err := registration.Close(); err != nil {
		t.Fatal(err)
	}
	newRegistration, err := backend.Register(data, backend.Link{Socket: "/tmp/new.sock", Token: "new"})
	if err != nil {
		t.Fatal(err)
	}
	defer newRegistration.Close()
	if err := waitForMuxerStop(context.Background(), data, link); err != nil {
		t.Fatalf("replacement wait = %v", err)
	}
	if err := newRegistration.Close(); err != nil {
		t.Fatal(err)
	}
	if err := waitForMuxerStop(context.Background(), data, link); err != nil {
		t.Fatalf("removed wait = %v", err)
	}
	badData := t.TempDir()
	if err := os.WriteFile(filepath.Join(badData, "muxer.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForMuxerStop(context.Background(), badData, link); err == nil {
		t.Fatal("wait accepted a malformed replacement locator")
	}
}

func backgroundStopContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}
