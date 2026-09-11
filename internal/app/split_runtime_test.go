package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/backend"
	"codeberg.org/chrberger/scimux/internal/remote"
	"codeberg.org/chrberger/scimux/internal/tmuxsession"
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
	claimCommandOwnership(t, cmd)
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

func TestSplitRuntimeRetainsOwnershipWhileWebDrains(t *testing.T) {
	requestEntered := make(chan struct{})
	releaseRequest := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	a := newTestApp(t, &fakeTmux{})
	a.server = tmuxsession.NewServerWithRunner("testsock", func(ctx context.Context, _ string, args ...string) (string, error) {
		if len(args) >= 3 && args[2] == "list-sessions" {
			enteredOnce.Do(func() { close(requestEntered) })
			select {
			case <-releaseRequest:
				return "", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "", nil
	})
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cmd := &Command{
		Stdout: io.Discard, Stderr: io.Discard, listener: ln,
		listenAddr: ln.Addr().String(), Config: remote.Config{DataDir: data},
	}
	claimCommandOwnership(t, cmd)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rt, err := startSplitRuntime(context.Background(), a, cmd, exe, splitRuntimeOptions{
		configureWeb: func(s *webSupervisor) {
			s.readyTimeout, s.drainTimeout = 10*time.Second, 10*time.Second
			s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
			s.extraEnv = []string{"SCIMUX_WEB_CHILD_TEST=1"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseRequest) })
		_ = rt.Close()
	})

	requestDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("http://" + ln.Addr().String() + "/api/state")
		if err == nil {
			_ = resp.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-requestEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not enter the muxer")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- rt.Close() }()

	deadline := time.NewTimer(150 * time.Millisecond)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
retainLoop:
	for {
		select {
		case err := <-closeDone:
			t.Fatalf("runtime closed before its web request drained: %v", err)
		case <-ticker.C:
			contender, err := backend.Claim(data)
			if err == nil {
				_ = contender.Close()
				t.Fatal("successor claimed ownership while the old web request was still draining")
			}
			if !errors.Is(err, backend.ErrMuxerOwned) {
				t.Fatalf("contender claim while draining = %v", err)
			}
		case <-deadline.C:
			break retainLoop
		}
	}

	releaseOnce.Do(func() { close(releaseRequest) })
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	successor, err := backend.Claim(data)
	if err != nil {
		t.Fatalf("successor could not claim after shutdown: %v", err)
	}
	if err := successor.Close(); err != nil {
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
	cmd := &Command{Stdout: io.Discard, Stderr: io.Discard, Config: remote.Config{DataDir: t.TempDir()}}
	if _, err := startSplitRuntime(context.Background(), nil, cmd, "test", splitRuntimeOptions{}); err == nil {
		t.Fatal("accepted nil application")
	}
	if _, err := startSplitRuntime(context.Background(), a, nil, "test", splitRuntimeOptions{}); err == nil {
		t.Fatal("accepted nil command")
	}
	if _, err := startSplitRuntime(context.Background(), a, cmd, "", splitRuntimeOptions{}); err == nil {
		t.Fatal("accepted empty executable")
	}
	claimCommandOwnership(t, cmd)
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

func TestSplitRuntimeRequiresEarlyOwnership(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	data := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cmd := &Command{
		Stdout: io.Discard, Stderr: io.Discard,
		listener: ln, listenAddr: ln.Addr().String(), Config: remote.Config{DataDir: data},
	}
	_, err = startSplitRuntime(context.Background(), a, cmd, "unused", splitRuntimeOptions{})
	if err == nil || !strings.Contains(err.Error(), "does not own") {
		t.Fatalf("missing ownership error = %v", err)
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
	defer cmd.closeOwnership()
	if cmd.application == nil || cmd.Listener() == nil {
		t.Fatal("muxer-only startup did not construct core state and listener")
	}
	if cmd.Handler() != nil || cmd.tunnelHandlerFor != nil || cmd.client != nil {
		t.Fatal("muxer-only startup constructed web or remote presentation")
	}
}

func TestMuxerOnlyCommandClaimsBeforeStoreReplay(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, err := backend.Claim(data)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// If startup touched the store before testing ownership, this invalid path
	// would hide the duplicate-owner error.
	if err := os.Mkdir(filepath.Join(data, "nodes.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := &Command{
		Args: []string{"scimux", "-addr", "127.0.0.1:0", "-data", data},
		Home: t.TempDir(), Stdout: io.Discard, Stderr: io.Discard, muxerOnly: true,
	}
	if err := cmd.Run(context.Background()); !errors.Is(err, backend.ErrMuxerOwned) {
		t.Fatalf("duplicate startup error = %v, want ErrMuxerOwned", err)
	}
	if cmd.application != nil || cmd.Listener() != nil {
		t.Fatal("duplicate startup touched application state or public listener")
	}
}

func TestMuxerOnlyCommandReleasesOwnershipAfterStoreReplayFailure(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(data, "nodes.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := &Command{
		Args: []string{"scimux", "-addr", "127.0.0.1:0", "-data", data},
		Home: t.TempDir(), Stdout: io.Discard, Stderr: io.Discard, muxerOnly: true,
	}
	if err := cmd.Run(context.Background()); err == nil {
		t.Fatal("startup accepted an unreadable store")
	}
	if cmd.ownership != nil {
		t.Fatal("failed startup retained data-directory ownership")
	}
	owner, err := backend.Claim(data)
	if err != nil {
		t.Fatalf("successor could not claim after failed startup: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

func claimCommandOwnership(t *testing.T, cmd *Command) {
	t.Helper()
	if err := os.Chmod(cmd.Config.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, err := backend.Claim(cmd.Config.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	cmd.ownership = owner
	t.Cleanup(cmd.closeOwnership)
}
