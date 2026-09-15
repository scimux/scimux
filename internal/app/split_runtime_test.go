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
	"codeberg.org/chrberger/scimux/internal/sessionworker"
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
	restartRequested := make(chan struct{}, 1)
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
		requestStop:    func() { stopRequested <- struct{}{} },
		requestRestart: func() { restartRequested <- struct{}{} },
		report:         func(err error) { t.Errorf("unexpected recovery error: %v", err) },
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
	select {
	case <-restartRequested:
	default:
		t.Fatal("committed web candidate did not request muxer replacement")
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

func TestSplitRuntimeRejectsIncompleteCompositionAndNilLifecycle(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	for _, tc := range []struct {
		a   *app
		cmd *Command
		exe string
	}{
		{a: nil, cmd: &Command{}, exe: "scimux"},
		{a: a, cmd: nil, exe: "scimux"},
		{a: a, cmd: &Command{}, exe: ""},
		{a: a, cmd: &Command{}, exe: "scimux"},
	} {
		if runtime, err := startSplitRuntime(nil, tc.a, tc.cmd, tc.exe, splitRuntimeOptions{}); err == nil || runtime != nil {
			t.Fatalf("incomplete runtime = %#v, %v", runtime, err)
		}
	}
	var nilRuntime *splitRuntime
	if files, err := nilRuntime.PrepareExec(); err == nil || files != nil {
		t.Fatalf("nil PrepareExec = %#v, %v", files, err)
	}
	if err := nilRuntime.QuiesceForExec(); err != nil {
		t.Fatal(err)
	}
	if files, err := (&splitRuntime{}).PrepareExec(); err == nil || files != nil {
		t.Fatalf("empty PrepareExec = %#v, %v", files, err)
	}

	var nilMuxer *muxerBackend
	nilMuxer.releaseHarnessesAfterStartupFailure()
	manager := &workerManager{entries: map[string]*workerEntry{}}
	(&muxerBackend{app: &app{workers: manager}}).releaseHarnessesAfterStartupFailure()
	(&muxerBackend{app: newTestApp(t, &fakeTmux{})}).releaseHarnessesAfterStartupFailure()
}

func TestSplitRuntimeQuiesceForExecPreservesSessionWorkerAndOwnership(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	data := filepath.Dir(a.storePath)
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	workers := syntheticWorkerManager(t, data)
	a.workers = workers
	if _, err := workers.Launch("survivor", "opencode", data, "cheap", "low"); err != nil {
		t.Fatal(err)
	}
	workerBefore, err := sessionworker.Discover(data, "survivor")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cmd := &Command{Stdout: io.Discard, Stderr: io.Discard, listener: ln, listenAddr: ln.Addr().String(), Config: remote.Config{DataDir: data}}
	claimCommandOwnership(t, cmd)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rt, err := startSplitRuntime(context.Background(), a, cmd, exe, splitRuntimeOptions{configureWeb: func(s *webSupervisor) {
		s.readyTimeout, s.drainTimeout = 10*time.Second, 10*time.Second
		s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
		s.extraEnv = []string{"SCIMUX_WEB_CHILD_TEST=1"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	files, err := rt.PrepareExec()
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	if err := rt.QuiesceForExec(); err != nil {
		t.Fatal(err)
	}
	if cmd.Listener() == nil || cmd.ownership == nil {
		t.Fatal("exec quiesce released public listener or lifetime ownership")
	}
	workerAfter, err := sessionworker.Discover(data, "survivor")
	if err != nil || workerAfter.PID != workerBefore.PID {
		t.Fatalf("worker after quiesce = %#v, %v; want PID %d", workerAfter, err, workerBefore.PID)
	}
	if workers.entry("survivor") != nil {
		t.Fatal("old muxer retained a worker client after quiesce")
	}
	if err := workers.Reconcile([]*Node{{ID: "survivor", Agent: "opencode", Transport: "acp"}}); err != nil {
		t.Fatal(err)
	}
	workers.Shutdown()
	cmd.closeListener()
	cmd.closeOwnership()
}

func TestSplitRuntimeQuiesceFailureReleasesListenerAndOwnership(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	data := filepath.Dir(a.storePath)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cmd := &Command{Stdout: io.Discard, Stderr: io.Discard, listener: ln, listenAddr: ln.Addr().String(), Config: remote.Config{DataDir: data}}
	claimCommandOwnership(t, cmd)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rt, err := startSplitRuntime(context.Background(), a, cmd, exe, splitRuntimeOptions{configureWeb: func(s *webSupervisor) {
		s.readyTimeout, s.drainTimeout = 10*time.Second, 10*time.Second
		s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
		s.extraEnv = []string{"SCIMUX_WEB_CHILD_TEST=1"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	coreDir := filepath.Dir(rt.core.Link().Socket)
	blocker := filepath.Join(coreDir, "force-close-error")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(blocker)
		_ = os.Remove(coreDir)
	})
	if err := rt.QuiesceForExec(); err == nil {
		t.Fatal("forced private-backend cleanup failure was hidden")
	}
	if cmd.Listener() != nil || cmd.ownership != nil {
		t.Fatal("failed exec quiesce retained public listener or data-directory ownership")
	}
	replacement, err := backend.Claim(data)
	if err != nil {
		t.Fatalf("replacement could not claim ownership after failed quiesce: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSplitRuntimeRetainsOwnershipWhileWebDrains(t *testing.T) {
	requestEntered := make(chan struct{})
	releaseRequest := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	a := newTestApp(t, &fakeTmux{alive: map[string]bool{"draining-request": true}})
	n := &Node{ID: "draining-request", Title: "draining request", Agent: "claude", Transport: "tmux", Adopted: true, CreatedAt: "2026-01-01T00:00:00Z"}
	a.nodes, a.byID[n.ID] = []*Node{n}, n
	a.deleteGateHook = func(id string) {
		if id != n.ID {
			return
		}
		enteredOnce.Do(func() { close(requestEntered) })
		<-releaseRequest
	}
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
		req, err := http.NewRequest(http.MethodDelete, "http://"+ln.Addr().String()+"/api/nodes/"+n.ID, nil)
		if err == nil {
			req.Header.Set("X-Scimux-CSRF", cmd.csrfToken)
		}
		var resp *http.Response
		if err == nil {
			resp, err = client.Do(req)
		}
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
