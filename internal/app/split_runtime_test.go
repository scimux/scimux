package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/backend"
	"codeberg.org/chrberger/scimux/internal/remote"
)

func TestSplitRuntimeCompositionAndShutdown(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cmd := &Command{
		Stdout: io.Discard, Stderr: io.Discard,
		listener: ln, listenAddr: ln.Addr().String(), Config: remote.Config{DataDir: data},
	}
	stopRequested := make(chan struct{}, 1)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := startSplitRuntime(nil, a, cmd, exe, splitRuntimeOptions{
		configureWeb: func(s *webSupervisor) {
			s.readyTimeout, s.drainTimeout = 10*time.Second, 10*time.Second
			s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
			s.extraEnv = []string{"SCIMUX_WEB_CHILD_TEST=1"}
		},
		requestStop: func() { stopRequested <- struct{}{} },
		report:      func(err error) { t.Errorf("unexpected recovery error: %v", err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRuntimeWebGeneration(t, ln.Addr().String(), "1")
	if a.prepareWebUpdate == nil {
		t.Fatal("runtime did not install the update handoff")
	}
	link, err := backend.Discover(data)
	if err != nil {
		t.Fatal(err)
	}
	client, err := backend.NewClient(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RequestStop(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	select {
	case <-stopRequested:
	default:
		t.Fatal("runtime stop endpoint did not request shutdown")
	}
	handoff, err := a.prepareWebUpdate(context.Background(), exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := handoff.commit(); err != nil {
		t.Fatal(err)
	}
	assertRuntimeWebGeneration(t, ln.Addr().String(), "2")
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.prepareWebUpdate(context.Background(), exe); err == nil {
		t.Fatal("closed runtime prepared an update")
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if cmd.Listener() != nil {
		t.Fatal("runtime close retained the public listener")
	}
	if _, err := backend.Discover(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime close retained muxer locator: %v", err)
	}
	var nilRuntime *splitRuntime
	if err := nilRuntime.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertRuntimeWebGeneration(t *testing.T, addr, want string) {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Scimux-Web-Generation") != want {
		t.Fatalf("runtime generation = %d %q, want 200 %q", resp.StatusCode, resp.Header.Get("X-Scimux-Web-Generation"), want)
	}
}

func TestSplitRuntimeRejectsInvalidComposition(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	cmd := &Command{Stdout: io.Discard, Stderr: io.Discard}
	if _, err := startSplitRuntime(context.Background(), nil, cmd, "test", splitRuntimeOptions{}); err == nil {
		t.Fatal("accepted nil application")
	}
	if _, err := startSplitRuntime(context.Background(), a, nil, "test", splitRuntimeOptions{}); err == nil {
		t.Fatal("accepted nil command")
	}
	if _, err := startSplitRuntime(context.Background(), a, cmd, "", splitRuntimeOptions{}); err == nil {
		t.Fatal("accepted empty executable")
	}
	if _, err := startSplitRuntime(context.Background(), a, cmd, "/missing/scimux", splitRuntimeOptions{}); err == nil {
		t.Fatal("accepted command without a public listener")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cmd.listener, cmd.listenAddr = ln, ln.Addr().String()
	cmd.Config.DataDir = t.TempDir()
	if _, err := startSplitRuntime(context.Background(), a, cmd, "/missing/scimux", splitRuntimeOptions{}); err == nil {
		t.Fatal("accepted an executable that could not start")
	}
}

func TestSplitRuntimeRejectsUnsafeControlDirectory(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	data := t.TempDir()
	if err := os.Chmod(data, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cmd := &Command{
		Stdout: io.Discard, Stderr: io.Discard,
		listener: ln, listenAddr: ln.Addr().String(), Config: remote.Config{DataDir: data},
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, err = startSplitRuntime(context.Background(), a, cmd, exe, splitRuntimeOptions{
		configureWeb: func(s *webSupervisor) {
			s.readyTimeout, s.drainTimeout = 10*time.Second, 10*time.Second
			s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
			s.extraEnv = []string{"SCIMUX_WEB_CHILD_TEST=1"}
		},
	})
	if err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("unsafe control directory error = %v", err)
	}
	if cmd.Listener() == nil {
		t.Fatal("failed runtime unexpectedly consumed the caller-owned listener")
	}
	_ = ln.Close()
}

func TestMuxerOnlyCommandDoesNotConstructPresentation(t *testing.T) {
	cmd := &Command{
		Args: []string{"scimux", "-addr", "127.0.0.1:0", "-data", t.TempDir()},
		Home: t.TempDir(), Stdout: io.Discard, Stderr: io.Discard, muxerOnly: true,
	}
	if err := cmd.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer cmd.closeListener()
	if cmd.application == nil || cmd.Listener() == nil {
		t.Fatal("muxer-only startup did not construct core state and listener")
	}
	if cmd.Handler() != nil || cmd.tunnelHandlerFor != nil || cmd.client != nil {
		t.Fatal("muxer-only startup constructed web or remote presentation")
	}
}
