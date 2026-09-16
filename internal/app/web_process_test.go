package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/backend"
	"codeberg.org/chrberger/scimux/internal/remote"
)

// TestWebChildHelperProcess turns this package's test executable into the
// actual hidden child. The parent test still exercises os/exec, inherited
// descriptors, the Unix API, the TCP listener, and graceful SIGTERM.
func TestWebChildHelperProcess(t *testing.T) {
	if os.Getenv("SCIMUX_WEB_CHILD_TEST") != "1" {
		return
	}
	if childVersion := os.Getenv("SCIMUX_WEB_CHILD_TEST_VERSION"); childVersion != "" {
		version = childVersion
	}
	if code := runWebChildMain(); code != 0 {
		t.Fatalf("web child exited %d", code)
	}
}

func TestBadWebChildHelperProcess(t *testing.T) {
	if os.Getenv("SCIMUX_WEB_CHILD_TEST") != "1" {
		return
	}
	ready := os.NewFile(webReadyFD, "bad-web-ready")
	if ready == nil {
		t.Fatal("missing ready descriptor")
	}
	defer ready.Close()
	_, _ = io.WriteString(ready, `{"phase":"wrong","generation":99,"version":"bad"}`)
}

func TestInvalidReadyWebChildHelperProcess(t *testing.T) {
	if os.Getenv("SCIMUX_WEB_CHILD_TEST") != "1" {
		return
	}
	ready := os.NewFile(webReadyFD, "invalid-web-ready")
	if ready == nil {
		t.Fatal("missing ready descriptor")
	}
	defer ready.Close()
	_, _ = io.WriteString(ready, `{"phase":"ready","generation":99,"version":"bad"}`)
	// Stay alive until the supervisor rejects the event and closes activation.
	// Exiting here races the readiness decoder and makes the asserted error
	// depend on which event the scheduler observes first.
	activate := os.NewFile(webActivateFD, "invalid-web-activate")
	if activate != nil {
		defer activate.Close()
		_, _ = io.Copy(io.Discard, activate)
	}
}

func TestGatedWebChildHelperProcess(t *testing.T) {
	if os.Getenv("SCIMUX_WEB_CHILD_TEST") != "1" {
		return
	}
	marker, gate := os.Getenv("SCIMUX_WEB_CHILD_MARKER"), os.Getenv("SCIMUX_WEB_CHILD_GATE")
	if marker == "" || gate == "" {
		t.Fatal("missing gated-child paths")
	}
	if err := os.WriteFile(marker, []byte("started\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for gated-child release")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if code := runWebChildMain(); code != 0 {
		t.Fatalf("web child exited %d", code)
	}
}

func TestUnrelatedExecDescriptorProbe(t *testing.T) {
	target := os.Getenv("SCIMUX_LISTENER_PROBE")
	if target == "" {
		return
	}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		link, _ := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if link == target {
			t.Fatalf("unrelated process inherited listener as fd %s (%s)", fd.Name(), link)
		}
	}
}

func TestWebSupervisorStartsAndReplacesRealChild(t *testing.T) {
	statusReports := make(chan backend.Status, 16)
	coreMux := http.NewServeMux()
	coreMux.HandleFunc("POST /_scimux/status", func(w http.ResponseWriter, r *http.Request) {
		var status backend.Status
		if err := json.NewDecoder(r.Body).Decode(&status); err == nil {
			statusReports <- status
		}
		w.WriteHeader(http.StatusNoContent)
	})
	coreMux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"marker":"muxer-alive"}`)
	})
	core, err := backend.Listen(t.TempDir(), coreMux)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cmd := &Command{
		Stdout: io.Discard, Stderr: io.Discard,
		listenAddr: ln.Addr().String(),
		Config:     remoteConfigForWebChildTest(t.TempDir()),
	}
	s, err := newWebSupervisor(ln, core.Link(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	s.readyTimeout = 10 * time.Second
	s.drainTimeout = 10 * time.Second
	s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
	s.extraEnv = []string{"SCIMUX_WEB_CHILD_TEST=1"}
	defer s.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), exe); err != nil {
		t.Fatal(err)
	}
	firstPID := s.current.cmd.Process.Pid
	if runtime.GOOS == "linux" {
		environ, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(firstPID), "environ"))
		if err != nil {
			t.Fatal(err)
		}
		for label, secret := range map[string]string{"muxer capability": core.Link().Token, "CSRF token": cmd.csrfToken} {
			if bytes.Contains(environ, []byte(secret)) {
				t.Fatalf("web-child environment exposed %s", label)
			}
		}
	}
	firstOwner := s.current.owner
	assertWebGeneration(t, ln.Addr().String(), "1")

	prepared, err := s.Prepare(context.Background(), exe)
	if err != nil {
		t.Fatal(err)
	}
	if s.current.cmd.Process.Pid != firstPID {
		t.Fatal("preparation disturbed the active child")
	}
	assertWebGeneration(t, ln.Addr().String(), "1")
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	if s.current.cmd.Process.Pid == firstPID {
		t.Fatal("rotation retained the old child PID")
	}
	if firstOwner == nil {
		t.Fatal("web child has no parent-lifetime pipe")
	}
	assertWebGeneration(t, ln.Addr().String(), "2")

	currentPID := s.current.cmd.Process.Pid
	s.childArgs = []string{"-test.run=^TestBadWebChildHelperProcess$"}
	if err := s.Rotate(context.Background(), exe); err == nil {
		t.Fatal("malformed candidate became active")
	}
	if s.current.cmd.Process.Pid != currentPID || s.generation != 2 {
		t.Fatalf("failed rotation changed current child: pid=%d generation=%d", s.current.cmd.Process.Pid, s.generation)
	}
	assertWebGeneration(t, ln.Addr().String(), "2")
	s.childArgs = []string{"-test.run=^TestInvalidReadyWebChildHelperProcess$"}
	if err := s.Rotate(context.Background(), exe); err == nil || !strings.Contains(err.Error(), "invalid readiness") {
		t.Fatalf("invalid readiness error = %v", err)
	}
	if s.current.cmd.Process.Pid != currentPID || s.generation != 2 {
		t.Fatal("invalid readiness disturbed the active generation")
	}

	// A genuinely crashed current child is different from a planned retire:
	// the muxer replaces it automatically, using the still-bound listener.
	s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
	recoverCtx, cancelRecover := context.WithCancel(context.Background())
	defer cancelRecover()
	recoveryErrors := make(chan error, 4)
	go s.Recover(recoverCtx, exe, func(err error) { recoveryErrors <- err })
	if err := s.current.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	recovered := false
	for !recovered {
		select {
		case status := <-statusReports:
			if status.Generation >= 3 {
				assertWebGenerationEventually(t, ln.Addr().String(), "3")
				recovered = true
			}
		case err := <-recoveryErrors:
			t.Fatalf("first recovery: %v", err)
		case <-deadline:
			t.Fatal("automatic recovery did not activate generation 3")
		}
	}

	// A failed recovery is reported and retried; a dead current pointer must
	// not strand the public listener indefinitely after the first bad binary.
	s.mu.Lock()
	s.childArgs = []string{"-test.run=^TestBadWebChildHelperProcess$"}
	broken := s.current
	s.mu.Unlock()
	if err := broken.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-recoveryErrors:
	case <-time.After(10 * time.Second):
		t.Fatal("failed recovery was not reported")
	}
	s.mu.Lock()
	s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
	s.mu.Unlock()
	deadline = time.After(10 * time.Second)
	for {
		select {
		case status := <-statusReports:
			if status.Generation >= 4 {
				assertWebGenerationEventually(t, ln.Addr().String(), "4")
				return
			}
		case err := <-recoveryErrors:
			t.Logf("recovery retry while swapping test child: %v", err)
		case <-deadline:
			t.Fatal("automatic recovery did not retry generation 4")
		}
	}
}

func TestWebChildExitsWhenMuxerLifetimePipeCloses(t *testing.T) {
	coreMux := http.NewServeMux()
	coreMux.HandleFunc("POST /_scimux/status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	core, err := backend.Listen(t.TempDir(), coreMux)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cmd := &Command{Stdout: io.Discard, Stderr: io.Discard, listenAddr: ln.Addr().String(), Config: remoteConfigForWebChildTest(t.TempDir())}
	s, err := newWebSupervisor(ln, core.Link(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	s.readyTimeout, s.drainTimeout = 10*time.Second, 10*time.Second
	s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
	s.extraEnv = []string{"SCIMUX_WEB_CHILD_TEST=1"}
	defer s.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), exe); err != nil {
		t.Fatal(err)
	}
	child := s.current
	if child.owner == nil {
		t.Fatal("child has no muxer lifetime writer")
	}
	if err := child.owner.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.done:
		if err := child.err(); err != nil {
			t.Fatalf("parent-death exit = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("web child survived loss of its muxer lifetime pipe")
	}
}

func TestRunWebChildFilesOwnsInheritedDescriptors(t *testing.T) {
	if err := runWebChildFiles(context.Background(), webChildConfig{}, nil, nil, nil, nil, nil, nil, nil, webChildDeps{}); err == nil {
		t.Fatal("web child accepted missing inherited descriptors")
	}
	files := make([]*os.File, 4)
	for i := range files {
		file, err := os.CreateTemp(t.TempDir(), "not-a-listener")
		if err != nil {
			t.Fatal(err)
		}
		files[i] = file
	}
	if err := runWebChildFiles(context.Background(), webChildConfig{}, files[0], files[1], files[2], files[3], nil, nil, nil, webChildDeps{}); err == nil {
		t.Fatal("web child accepted a regular file as its public listener")
	}

	coreMux := http.NewServeMux()
	coreMux.HandleFunc("POST /_scimux/status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	core, err := backend.Listen(t.TempDir(), coreMux)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	publicFile, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	activateR, activateW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer activateW.Close()
	ownerR, ownerW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- runWebChildFiles(context.Background(), webChildConfig{
			Link: core.Link(), Generation: 1, ListenAddr: ln.Addr().String(),
			DataDir: data, CSRFToken: strings.Repeat("a", 64),
		}, publicFile, readyW, activateR, ownerR, strings.NewReader(""), io.Discard, io.Discard, webChildDeps{})
	}()
	dec := json.NewDecoder(readyR)
	var event webChildEvent
	if err := dec.Decode(&event); err != nil || event.Phase != "ready" {
		t.Fatalf("ready = %#v, %v", event, err)
	}
	if _, err := activateW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&event); err != nil || event.Phase != "active" {
		t.Fatalf("active = %#v, %v", event, err)
	}
	if err := ownerW.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("child did not stop when owner descriptor closed")
	}
	if _, err := publicFile.Stat(); err == nil {
		t.Fatal("web child retained inherited public descriptor")
	}
}

func TestWebRotationDrainsInFlightMutation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	var calls int
	var callsMu sync.Mutex
	coreMux := http.NewServeMux()
	coreMux.HandleFunc("POST /_scimux/status", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	coreMux.HandleFunc("POST /api/mutate", func(w http.ResponseWriter, _ *http.Request) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		if call == 1 {
			enterOnce.Do(func() { close(entered) })
			<-release
		}
		_, _ = io.WriteString(w, "done")
	})
	core, err := backend.Listen(t.TempDir(), coreMux)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cmd := &Command{
		Stdout: io.Discard, Stderr: io.Discard,
		listenAddr: ln.Addr().String(), Config: remoteConfigForWebChildTest(t.TempDir()),
	}
	s, err := newWebSupervisor(ln, core.Link(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	s.readyTimeout, s.drainTimeout = 10*time.Second, 10*time.Second
	s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
	s.extraEnv = []string{"SCIMUX_WEB_CHILD_TEST=1"}
	defer s.Close()
	exe, _ := os.Executable()
	if err := s.Start(context.Background(), exe); err != nil {
		t.Fatal(err)
	}

	csrf := fetchCSRF(t, ln.Addr().String())
	firstDone := make(chan error, 1)
	go func() { firstDone <- postMutation(ln.Addr().String(), csrf) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("mutation never entered muxer")
	}
	drainStarted := make(chan struct{})
	s.beforeDrain = func() { close(drainStarted) }
	rotateDone := make(chan error, 1)
	go func() { rotateDone <- s.Rotate(context.Background(), exe) }()
	select {
	case <-drainStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("rotation never reached drain")
	}
	select {
	case err := <-rotateDone:
		t.Fatalf("rotation completed before in-flight request: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-rotateDone; err != nil {
		t.Fatal(err)
	}
	if err := postMutation(ln.Addr().String(), csrf); err != nil {
		t.Fatal(err)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls != 2 {
		t.Fatalf("mutation calls = %d, want exactly one before and after rotation", calls)
	}
}

func TestServeWebChildInProcess(t *testing.T) {
	for _, tc := range []struct {
		name        string
		remote      bool
		replacement bool
		startErr    bool
		stayLocal   bool
	}{
		{name: "local-initial"},
		{name: "remote-initial", remote: true},
		{name: "remote-replacement", remote: true, replacement: true},
		{name: "remote-replacement-failure", remote: true, replacement: true, startErr: true},
		{name: "unlinked-replacement", remote: true, replacement: true, stayLocal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statusReports := make(chan backend.Status, 8)
			coreMux := http.NewServeMux()
			coreMux.HandleFunc("POST /_scimux/status", func(w http.ResponseWriter, r *http.Request) {
				var status backend.Status
				_ = json.NewDecoder(r.Body).Decode(&status)
				statusReports <- status
				w.WriteHeader(http.StatusNoContent)
			})
			coreMux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "state")
			})
			core, err := backend.Listen("", coreMux)
			if err != nil {
				t.Fatal(err)
			}
			defer core.Close()
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			readyR, readyW := io.Pipe()
			activateR, activateW := io.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fakeRemote := newFakeWebRemote("enrolled")
			if tc.stayLocal {
				fakeRemote.state = remote.StateAbsent
			}
			if tc.replacement && !tc.startErr && !tc.stayLocal {
				fakeRemote.startGate = make(chan struct{})
			}
			if tc.startErr {
				fakeRemote.startErr = errors.New("replacement remote failed")
			}
			var childStderr lockedBuffer
			var factoryCalls atomic.Int32
			deps := webChildDeps{newRemote: func(cfg remote.Config) webRemoteClient {
				factoryCalls.Add(1)
				if cfg.TunnelHandlerFor == nil || cfg.DataDir == "" {
					t.Error("remote factory received incomplete web-owned config")
				}
				return fakeRemote
			}}
			cfg := webChildConfig{
				Link: core.Link(), Generation: 9, Replacement: tc.replacement,
				ListenAddr: ln.Addr().String(), DataDir: t.TempDir(), Remote: tc.remote,
			}
			done := make(chan error, 1)
			go func() {
				defer readyW.Close()
				done <- serveWebChild(ctx, cfg, ln, readyW, activateR, strings.NewReader(""), io.Discard, &childStderr, deps)
			}()
			dec := json.NewDecoder(readyR)
			var ready webChildEvent
			if err := dec.Decode(&ready); err != nil || ready.Phase != "ready" {
				t.Fatalf("ready = %#v, %v", ready, err)
			}
			if tc.remote && !tc.replacement {
				select {
				case <-fakeRemote.started:
				default:
					t.Fatal("initial remote did not start before readiness")
				}
			}
			if !tc.remote && factoryCalls.Load() != 0 {
				t.Fatal("local child constructed a remote client")
			}
			if _, err := activateW.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			_ = activateW.Close()
			activeResult := make(chan struct {
				event webChildEvent
				err   error
			}, 1)
			go func() {
				var active webChildEvent
				err := dec.Decode(&active)
				activeResult <- struct {
					event webChildEvent
					err   error
				}{active, err}
			}()
			if tc.remote && tc.replacement && !tc.stayLocal {
				select {
				case <-fakeRemote.started:
				case <-time.After(2 * time.Second):
					t.Fatal("replacement remote did not start after activation")
				}
				if tc.startErr {
					result := <-activeResult
					if result.err == nil {
						t.Fatalf("fatal replacement remote became active: %#v", result.event)
					}
					if err := <-done; err == nil || !strings.Contains(err.Error(), "replacement remote failed") {
						t.Fatalf("replacement failure = %v, stderr=%q", err, childStderr.String())
					}
					return
				}
				select {
				case result := <-activeResult:
					t.Fatalf("replacement activated before remote startup completed: %#v, %v", result.event, result.err)
				case <-time.After(50 * time.Millisecond):
				}
				close(fakeRemote.startGate)
			}
			result := <-activeResult
			if result.err != nil || result.event.Phase != "active" {
				t.Fatalf("active = %#v, %v", result.event, result.err)
			}
			resp, err := testHTTPClient.Get("http://" + ln.Addr().String() + "/api/state")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != "state" || resp.Header.Get("X-Scimux-Web-Generation") != "9" {
				t.Fatalf("public response = %q headers %v", body, resp.Header)
			}
			if tc.stayLocal {
				resp, err := testHTTPClient.Get("http://" + ln.Addr().String() + "/api/remote/status")
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusNotFound {
					t.Fatalf("unlinked replacement remote status = %d, want 404", resp.StatusCode)
				}
			}
			select {
			case status := <-statusReports:
				if status.Generation != 9 || status.Version == "" || status.Remote == nil {
					t.Fatalf("published status = %#v", status)
				}
				wantRemote := ""
				if tc.remote && !tc.stayLocal {
					wantRemote = "enrolled"
				}
				if *status.Remote != wantRemote {
					t.Fatalf("published remote status = %q, want %q", *status.Remote, wantRemote)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("web child did not publish status")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("web child did not drain on context cancellation")
			}
			if tc.remote {
				select {
				case <-fakeRemote.closed:
				default:
					t.Fatal("web child did not close its remote client")
				}
			}
		})
	}
}

func TestReplacementWaitsForRealRemoteClientStartup(t *testing.T) {
	data := t.TempDir()
	writeEnrolledState(t, data)
	authEntered := make(chan struct{})
	releaseAuth := make(chan struct{})
	var challengeOnce sync.Once
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v1/challenge":
			challengeOnce.Do(func() { close(authEntered) })
			<-releaseAuth
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"challenge":"` + strings.Repeat("11", 32) + `","v":1}`)),
				Request: req,
			}, nil
		case "/v1/verify":
			return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
		default:
			return nil, fmt.Errorf("unexpected rendezvous request %s", req.URL.Path)
		}
	})}

	coreMux := http.NewServeMux()
	coreMux.HandleFunc("POST /_scimux/status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	coreMux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "state") })
	core, err := backend.Listen(t.TempDir(), coreMux)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	readyR, readyW := io.Pipe()
	activateR, activateW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer readyW.Close()
		done <- serveWebChild(ctx, webChildConfig{
			Link: core.Link(), Generation: 2, Replacement: true,
			ListenAddr: ln.Addr().String(), DataDir: data, Remote: true,
		}, ln, readyW, activateR, strings.NewReader(""), io.Discard, io.Discard, webChildDeps{
			newRemote: func(cfg remote.Config) webRemoteClient {
				// A successful first generation consumed its invite. Recovery must
				// construct the real client without that obsolete file/stdin input.
				cfg.HTTPClient = httpClient
				cfg.NewTerminal = noTestTerminal
				return remote.NewClient(cfg)
			},
		})
	}()
	dec := json.NewDecoder(readyR)
	var ready webChildEvent
	if err := dec.Decode(&ready); err != nil || ready.Phase != "ready" {
		t.Fatalf("ready = %#v, %v", ready, err)
	}
	if _, err := activateW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = activateW.Close()
	select {
	case <-authEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("real remote client did not begin authentication")
	}
	activeResult := make(chan error, 1)
	go func() {
		var active webChildEvent
		err := dec.Decode(&active)
		if err == nil && active.Phase != "active" {
			err = fmt.Errorf("active event = %#v", active)
		}
		activeResult <- err
	}()
	select {
	case err := <-activeResult:
		t.Fatalf("web became active before real remote startup completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseAuth)
	if err := <-activeResult; err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUnlinkSurvivesWebRotationAndRecovery(t *testing.T) {
	var enrollCalls atomic.Int32
	rv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/challenge":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"challenge":"`+strings.Repeat("11", 32)+`","v":1}`)
		case "/v1/verify", "/v1/unenroll":
			w.WriteHeader(http.StatusNoContent)
		case "/v1/enroll":
			enrollCalls.Add(1)
			http.Error(w, "unexpected enrollment", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer rv.Close()
	data := t.TempDir()
	writeEnrolledState(t, data)
	rewriteEnrolledOrigin(t, data, rv.URL)
	s, addr := newProjectedRemoteWebSupervisor(t, data, rv.URL)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), exe); err != nil {
		t.Fatal(err)
	}
	assertRemoteProjection(t, addr, "enrolled")
	csrf := fetchCSRF(t, addr)
	code, body := publicJSON(t, addr, http.MethodPost, "/api/remote/unenroll", []byte(`{}`), csrf)
	if code != http.StatusOK || !strings.Contains(string(body), `"hosted":""`) {
		t.Fatalf("unlink = %d %s", code, body)
	}
	if err := s.Rotate(context.Background(), exe); err != nil {
		t.Fatalf("rotation after unlink: %v", err)
	}
	assertWebGenerationEventually(t, addr, "2")
	assertRemoteProjection(t, addr, "")

	recoverCtx, cancelRecover := context.WithCancel(context.Background())
	defer cancelRecover()
	recoveryErrors := make(chan error, 1)
	go s.Recover(recoverCtx, exe, func(err error) {
		select {
		case recoveryErrors <- err:
		default:
		}
	})
	if err := s.current.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	client := &http.Client{Timeout: 250 * time.Millisecond}
	for time.Now().Before(deadline) {
		select {
		case err := <-recoveryErrors:
			t.Fatalf("recovery after unlink: %v", err)
		default:
		}
		resp, err := client.Get("http://" + addr + "/api/state")
		if err == nil {
			got := resp.Header.Get("X-Scimux-Web-Generation")
			_ = resp.Body.Close()
			if got == "3" {
				if enrollCalls.Load() != 0 {
					t.Fatalf("replacement attempted enrollment %d times", enrollCalls.Load())
				}
				assertRemoteProjection(t, addr, "")
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("local web API did not recover after unlink")
}

func TestUnlinkDuringPreparedHandoff(t *testing.T) {
	unlinkEntered := make(chan struct{})
	releaseUnlink := make(chan struct{})
	var unlinkOnce sync.Once
	var enrollCalls atomic.Int32
	rv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/challenge":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"challenge":"`+strings.Repeat("11", 32)+`","v":1}`)
		case "/v1/verify":
			w.WriteHeader(http.StatusNoContent)
		case "/v1/unenroll":
			unlinkOnce.Do(func() { close(unlinkEntered) })
			<-releaseUnlink
			w.WriteHeader(http.StatusNoContent)
		case "/v1/enroll":
			enrollCalls.Add(1)
			http.Error(w, "unexpected enrollment", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer rv.Close()
	data := t.TempDir()
	writeEnrolledState(t, data)
	rewriteEnrolledOrigin(t, data, rv.URL)
	s, addr := newProjectedRemoteWebSupervisor(t, data, rv.URL)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), exe); err != nil {
		t.Fatal(err)
	}
	assertRemoteProjection(t, addr, "enrolled")
	prepared, err := s.Prepare(context.Background(), exe)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Abort()

	type unlinkResult struct {
		code int
		body []byte
		err  error
	}
	unlinked := make(chan unlinkResult, 1)
	csrf := fetchCSRF(t, addr)
	go func() {
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/remote/unenroll", strings.NewReader(`{}`))
		if err != nil {
			unlinked <- unlinkResult{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scimux-CSRF", csrf)
		resp, err := testHTTPClient.Do(req)
		if err != nil {
			unlinked <- unlinkResult{err: err}
			return
		}
		defer resp.Body.Close()
		body, readErr := io.ReadAll(resp.Body)
		unlinked <- unlinkResult{code: resp.StatusCode, body: body, err: readErr}
	}()
	select {
	case <-unlinkEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("unlink did not reach the rendezvous")
	}
	// Keep the request in flight until graceful drain has begun. Commit must
	// observe its durable result before it activates the already-ready child.
	s.beforeDrain = func() {
		time.AfterFunc(50*time.Millisecond, func() { close(releaseUnlink) })
	}
	if err := prepared.Commit(); err != nil {
		t.Fatalf("commit after in-flight unlink: %v", err)
	}
	result := <-unlinked
	if result.err != nil || result.code != http.StatusOK {
		t.Fatalf("unlink = %d %s, %v", result.code, result.body, result.err)
	}
	assertWebGenerationEventually(t, addr, "2")
	code, body := publicJSON(t, addr, http.MethodGet, "/api/remote/status", nil, "")
	if code != http.StatusNotFound {
		t.Fatalf("replacement retained remote routes: %d %s", code, body)
	}
	assertRemoteProjection(t, addr, "")
	if enrollCalls.Load() != 0 {
		t.Fatalf("prepared replacement attempted enrollment %d times", enrollCalls.Load())
	}
}

func TestConsumedInviteAuthOutageThenReplacement(t *testing.T) {
	var authAvailable atomic.Bool
	var enrollCalls atomic.Int32
	rv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"min":1,"v":1}`)
		case "/v1/enroll":
			enrollCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"handle":"ih_041061050R3GG28A","v":1}`)
		case "/v1/challenge":
			if !authAvailable.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"challenge":"`+strings.Repeat("11", 32)+`","v":1}`)
		case "/v1/verify":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer rv.Close()
	data := t.TempDir()
	invite := filepath.Join(data, "invite")
	if err := os.WriteFile(invite, []byte("0410-6105-0R3G-G28A-1C60-T3GF\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, addr := newRealRemoteWebSupervisor(t, data, rv.URL)
	s.config.InviteFile = invite
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), exe); err != nil {
		t.Fatal(err)
	}
	assertWebGeneration(t, addr, "1")
	if _, err := os.Stat(invite); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invite was not consumed: %v", err)
	}
	authAvailable.Store(true)
	if err := s.Rotate(context.Background(), exe); err != nil {
		t.Fatalf("replacement after authentication recovery: %v", err)
	}
	assertWebGeneration(t, addr, "2")
	if enrollCalls.Load() != 1 {
		t.Fatalf("enrollment requests = %d, want exactly one", enrollCalls.Load())
	}
}

func newRealRemoteWebSupervisor(t *testing.T, data, origin string) (*webSupervisor, string) {
	t.Helper()
	coreMux := http.NewServeMux()
	coreMux.HandleFunc("POST /_scimux/status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	coreMux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"marker":"muxer-alive"}`)
	})
	return newRemoteWebSupervisorWithCore(t, data, origin, coreMux)
}

func newProjectedRemoteWebSupervisor(t *testing.T, data, origin string) (*webSupervisor, string) {
	t.Helper()
	a := newTestApp(t, &fakeTmux{})
	muxer, err := newMuxerBackend(a)
	if err != nil {
		t.Fatal(err)
	}
	muxer.enableRemote(true)
	coreHandler, err := muxer.handler(nil)
	if err != nil {
		t.Fatal(err)
	}
	return newRemoteWebSupervisorWithCore(t, data, origin, coreHandler)
}

func newRemoteWebSupervisorWithCore(t *testing.T, data, origin string, coreHandler http.Handler) (*webSupervisor, string) {
	t.Helper()
	core, err := backend.Listen(t.TempDir(), coreHandler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	cmd := &Command{
		Stdout: io.Discard, Stderr: io.Discard, listenAddr: ln.Addr().String(),
		Config: remote.Config{DataDir: data, Remote: true, Origin: origin},
	}
	s, err := newWebSupervisor(ln, core.Link(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	s.readyTimeout, s.drainTimeout = 5*time.Second, 5*time.Second
	s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
	s.extraEnv = []string{"SCIMUX_WEB_CHILD_TEST=1"}
	t.Cleanup(func() { _ = s.Close() })
	return s, ln.Addr().String()
}

func assertRemoteProjection(t *testing.T, addr, want string) {
	t.Helper()
	code, body := publicJSON(t, addr, http.MethodGet, "/api/state", nil, "")
	var payload struct {
		Remote *struct {
			Status string `json:"status"`
		} `json:"remote"`
	}
	if err := json.Unmarshal(body, &payload); code != http.StatusOK || err != nil {
		t.Fatalf("state = %d %s, %v", code, body, err)
	}
	if payload.Remote == nil || payload.Remote.Status != want {
		t.Fatalf("remote projection = %#v, want status %q", payload.Remote, want)
	}
}

func rewriteEnrolledOrigin(t *testing.T, data, origin string) {
	t.Helper()
	c := remote.NewClient(remote.Config{DataDir: data})
	raw, err := os.ReadFile(c.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	var state remote.PersistedState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	state.Origin = origin
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.StatePath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

type fakeWebRemote struct {
	hostedPairingClient
	status    string
	state     remote.EnrollmentState
	startErr  error
	started   chan struct{}
	closed    chan struct{}
	startGate chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func newFakeWebRemote(status string) *fakeWebRemote {
	return &fakeWebRemote{
		status: status, state: remote.StateEnrolled,
		started: make(chan struct{}), closed: make(chan struct{}),
	}
}

func (f *fakeWebRemote) Start(context.Context) error {
	f.startOnce.Do(func() { close(f.started) })
	if f.startGate != nil {
		<-f.startGate
	}
	return f.startErr
}

func (f *fakeWebRemote) State() (remote.EnrollmentState, error) {
	return f.state, nil
}

func (f *fakeWebRemote) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeWebRemote) HostedStatus() string { return f.status }

func TestWebChildConfigurationValidation(t *testing.T) {
	if client := (webChildDeps{}).remote(remote.Config{DataDir: t.TempDir()}); client == nil {
		t.Fatal("default remote factory returned nil")
	}
	cfg := webChildConfig{
		Link:       backend.Link{Socket: "/tmp/muxer.sock", Token: "cap"},
		Generation: 3, Replacement: true, ListenAddr: "127.0.0.1:8787",
		TrustedHosts: []string{"lab.example"}, DataDir: "/tmp/data",
		Remote: true, InviteFile: "/tmp/invite", InviteStdin: false,
		RVOrigin: "https://rv.example", CSRFToken: strings.Repeat("a", 64),
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readWebChildConfig(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != cfg.Generation || !got.Replacement || !got.Remote || got.TrustedHosts[0] != "lab.example" || got.CSRFToken != cfg.CSRFToken {
		t.Fatalf("decoded config = %#v", got)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*webChildConfig)
	}{
		{"generation", func(c *webChildConfig) { c.Generation = 0 }},
		{"core socket", func(c *webChildConfig) { c.Link.Socket = "" }},
		{"core token", func(c *webChildConfig) { c.Link.Token = "" }},
		{"listen address", func(c *webChildConfig) { c.ListenAddr = "" }},
		{"data directory", func(c *webChildConfig) { c.DataDir = "" }},
		{"CSRF token", func(c *webChildConfig) { c.CSRFToken = "bad" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := cfg
			tc.mutate(&bad)
			payload, err := json.Marshal(bad)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := readWebChildConfig(bytes.NewReader(payload)); err == nil {
				t.Fatal("accepted incomplete pipe configuration")
			}
		})
	}
	if _, err := readWebChildConfig(strings.NewReader("{")); err == nil {
		t.Fatal("accepted malformed pipe configuration")
	}
	if _, err := readWebChildConfig(nil); err == nil {
		t.Fatal("accepted missing pipe configuration")
	}
}

func TestWebChildAcceptsLegacyEnvironmentFromOlderMuxer(t *testing.T) {
	if _, err := loadLegacyWebChildConfig(nil); err == nil {
		t.Fatal("accepted nil legacy environment")
	}
	env := map[string]string{
		envGeneration: "7", envReplacement: "true", envRemote: "false", envInviteStdin: "false",
		envTrusted: `["lab.example"]`, envCoreSocket: "/tmp/legacy.sock", envCoreToken: "legacy-capability",
		envListenAddr: "127.0.0.1:8787", envDataDir: "/tmp/legacy-data", envCSRFToken: strings.Repeat("b", 64),
	}
	legacyGetenv := func(key string) string { return env[key] }
	cfg, err := loadWebChildStartupConfig(legacyGetenv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Generation != 7 || !cfg.Replacement || cfg.Remote || cfg.Link.Token != "legacy-capability" || len(cfg.TrustedHosts) != 1 {
		t.Fatalf("legacy config = %#v", cfg)
	}
	if cfg.ViewerOrigin != remote.DefaultViewerOrigin {
		t.Fatalf("legacy ViewerOrigin = %q, want %q", cfg.ViewerOrigin, remote.DefaultViewerOrigin)
	}
	for _, key := range []string{envGeneration, envReplacement, envRemote, envInviteStdin, envTrusted, envCoreSocket, envCoreToken, envListenAddr, envDataDir, envCSRFToken} {
		t.Run("invalid "+key, func(t *testing.T) {
			bad := make(map[string]string, len(env))
			for name, value := range env {
				bad[name] = value
			}
			bad[key] = ""
			if _, err := loadLegacyWebChildConfig(func(name string) string { return bad[name] }); err == nil {
				t.Fatal("accepted incomplete legacy environment")
			}
		})
	}

	piped := webChildConfig{
		Link: backend.Link{Socket: "/tmp/new.sock", Token: "new-capability"}, Generation: 8,
		ListenAddr: "127.0.0.1:8787", DataDir: "/tmp/new-data", CSRFToken: strings.Repeat("c", 64),
	}
	payload, err := json.Marshal(piped)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := loadWebChildStartupConfig(func(key string) string {
		if key == envWebConfigFD {
			return strconv.Itoa(webConfigFD)
		}
		return ""
	}, bytes.NewReader(payload)); err != nil || got.Link.Token != piped.Link.Token {
		t.Fatalf("pipe config = %#v, %v", got, err)
	}
	if _, err := loadWebChildStartupConfig(func(string) string { return "bad-fd" }, nil); err == nil {
		t.Fatal("accepted invalid configuration descriptor")
	}
	if _, err := loadWebChildStartupConfig(nil, nil); err == nil {
		t.Fatal("accepted nil startup environment")
	}

	// Hidden-role startup must fail closed and scrub inherited secrets even
	// when an older muxer supplies an incomplete legacy environment.
	legacyNames := []string{
		envCoreSocket, envCoreToken, envGeneration, envReplacement, envListenAddr, envTrusted,
		envDataDir, envRemote, envInviteFile, envInviteStdin, envRVOrigin, envCSRFToken,
	}
	for _, name := range legacyNames {
		t.Setenv(name, os.Getenv(name))
	}
	t.Setenv(envWebConfigFD, "")
	t.Setenv(envCoreToken, "must-not-survive-startup")
	t.Setenv(envGeneration, "")
	if code := runWebChildMain(); code != 1 {
		t.Fatalf("invalid legacy startup exit = %d, want 1", code)
	}
	if got := os.Getenv(envCoreToken); got != "" {
		t.Fatalf("legacy core capability survived startup: %q", got)
	}
}

func TestOlderMuxerStartsNewWebChild(t *testing.T) {
	coreMux := http.NewServeMux()
	coreMux.HandleFunc("POST /_scimux/status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	coreMux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"legacy":"ok"}`) })
	core, err := backend.Listen(t.TempDir(), coreMux)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	publicFile, err := listenerFileForExec(ln)
	if err != nil {
		t.Fatal(err)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	activateR, activateW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer activateW.Close()
	ownerR, ownerW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cfg := webChildConfig{
		Link: core.Link(), Generation: 4, ListenAddr: ln.Addr().String(),
		DataDir: t.TempDir(), CSRFToken: strings.Repeat("d", 64),
	}
	trusted, err := json.Marshal(cfg.TrustedHosts)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestWebChildHelperProcess$")
	cmd.ExtraFiles = []*os.File{publicFile, readyW, activateR, ownerR}
	cmd.Env = []string{
		"SCIMUX_WEB_CHILD_TEST=1",
		envCoreSocket + "=" + cfg.Link.Socket, envCoreToken + "=" + cfg.Link.Token,
		envGeneration + "=4", envReplacement + "=false", envListenAddr + "=" + cfg.ListenAddr,
		envTrusted + "=" + string(trusted), envDataDir + "=" + cfg.DataDir, envRemote + "=false",
		envInviteStdin + "=false", envCSRFToken + "=" + cfg.CSRFToken,
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	_ = publicFile.Close()
	_ = readyW.Close()
	_ = activateR.Close()
	_ = ownerR.Close()
	dec := json.NewDecoder(readyR)
	var event webChildEvent
	if err := dec.Decode(&event); err != nil || event.Phase != "ready" || event.Generation != cfg.Generation {
		t.Fatalf("legacy-start readiness = %#v, %v", event, err)
	}
	if _, err := activateW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&event); err != nil || event.Phase != "active" || event.Generation != cfg.Generation {
		t.Fatalf("legacy-start activation = %#v, %v", event, err)
	}
	resp, err := testHTTPClient.Get("http://" + ln.Addr().String() + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"legacy":"ok"}` {
		t.Fatalf("legacy-start response = %d %q", resp.StatusCode, body)
	}
	if err := ownerW.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteLifecycleReconciliation(t *testing.T) {
	t.Run("local mode is unchanged", func(t *testing.T) {
		s := &webSupervisor{config: webChildConfig{DataDir: t.TempDir(), InviteStdin: true}}
		s.reconcileRemoteLifecycle(true)
		if s.config.Remote || !s.config.InviteStdin {
			t.Fatalf("local configuration changed: %#v", s.config)
		}
	})

	t.Run("state read error preserves remote mode", func(t *testing.T) {
		data := t.TempDir()
		statePath := remote.NewClient(remote.Config{DataDir: data}).StatePath()
		if err := os.MkdirAll(statePath, 0o700); err != nil {
			t.Fatal(err)
		}
		s := &webSupervisor{config: webChildConfig{DataDir: data, Remote: true, InviteStdin: true}}
		s.reconcileRemoteLifecycle(true)
		if !s.config.Remote || s.config.InviteStdin {
			t.Fatalf("state read failure changed durable mode or retained stdin: %#v", s.config)
		}
	})

	t.Run("preflight outage keeps unspent file", func(t *testing.T) {
		data := t.TempDir()
		invite := filepath.Join(data, "invite")
		if err := os.WriteFile(invite, []byte("unspent"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := &webSupervisor{config: webChildConfig{DataDir: data, Remote: true, InviteFile: invite}}
		s.reconcileRemoteLifecycle(true)
		if !s.config.Remote || s.config.InviteFile != invite {
			t.Fatalf("reusable file was discarded: %#v", s.config)
		}
	})

	t.Run("committed enrollment consumes inputs", func(t *testing.T) {
		data := t.TempDir()
		writeEnrolledState(t, data)
		s := &webSupervisor{config: webChildConfig{
			DataDir: data, Remote: true, InviteFile: "/already/consumed", InviteStdin: true,
		}}
		s.reconcileRemoteLifecycle(true)
		if !s.config.Remote || s.config.InviteFile != "" || s.config.InviteStdin {
			t.Fatalf("enrolled state retained one-shot inputs: %#v", s.config)
		}
		if err := os.Remove(remote.NewClient(remote.Config{DataDir: data}).StatePath()); err != nil {
			t.Fatal(err)
		}
		s.generation = 1
		s.reconcileRemoteLifecycle(false)
		if s.config.Remote {
			t.Fatalf("unlinked state retained remote startup: %#v", s.config)
		}
	})

	t.Run("stdin cannot be replayed", func(t *testing.T) {
		s := &webSupervisor{config: webChildConfig{DataDir: t.TempDir(), Remote: true, InviteStdin: true}}
		s.reconcileRemoteLifecycle(true)
		if s.config.Remote || s.config.InviteStdin {
			t.Fatalf("completed stdin attempt remained replayable: %#v", s.config)
		}
	})
}

func TestCommitRejectsMismatchedActivation(t *testing.T) {
	activateR, activateW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer activateR.Close()
	go io.Copy(io.Discard, activateR)
	events := make(chan webChildEvent, 1)
	events <- webChildEvent{Phase: "active", Generation: 2, Version: "v1"}
	s := &webSupervisor{readyTimeout: time.Second, drainTimeout: time.Millisecond}
	candidate := &webProcess{activate: activateW, events: events, eventErr: make(chan error), done: make(chan struct{}), version: "v1"}
	err = s.commitLocked(&preparedWeb{supervisor: s, candidate: candidate, generation: 1})
	if err == nil || !strings.Contains(err.Error(), "invalid activation") {
		t.Fatalf("mismatched activation error = %v", err)
	}
}

func TestWebSupervisorConstructionAndPreparedGuards(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	link := backend.Link{Socket: "/tmp/muxer.sock", Token: "cap"}
	const inheritedCSRF = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	cmd := &Command{Stdout: io.Discard, Stderr: io.Discard, csrfToken: inheritedCSRF}
	for _, tc := range []struct {
		name string
		ln   net.Listener
		link backend.Link
		cmd  *Command
	}{
		{"nil-listener", nil, link, cmd},
		{"empty-socket", ln, backend.Link{Token: "cap"}, cmd},
		{"empty-token", ln, backend.Link{Socket: "/tmp/muxer.sock"}, cmd},
		{"nil-command", ln, link, nil},
		{"invalid-csrf", ln, link, &Command{csrfToken: "invalid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newWebSupervisor(tc.ln, tc.link, tc.cmd); err == nil {
				t.Fatal("accepted invalid construction")
			}
		})
	}

	wrapped := struct{ net.Listener }{ln}
	s, err := newWebSupervisor(wrapped, link, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if s.config.CSRFToken != inheritedCSRF {
		t.Fatalf("web child CSRF token = %q, want inherited %q", s.config.CSRFToken, inheritedCSRF)
	}
	if err := s.Start(nil, os.Args[0]); err == nil || !strings.Contains(err.Error(), "cannot be inherited") {
		t.Fatalf("non-inheritable listener error = %v", err)
	}
	s, err = newWebSupervisor(fileErrorListener{Listener: ln}, link, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), os.Args[0]); err == nil || !strings.Contains(err.Error(), "duplicate listener") {
		t.Fatalf("listener duplication error = %v", err)
	}
	s.closed = true
	if err := s.Start(context.Background(), os.Args[0]); err == nil {
		t.Fatal("closed supervisor started")
	}
	s.closed = false
	if err := s.Start(context.Background(), ""); err == nil {
		t.Fatal("empty executable started")
	}

	var nilPrepared *preparedWeb
	if err := nilPrepared.Commit(); err == nil {
		t.Fatal("nil prepared child committed")
	}
	nilPrepared.Abort()
	foreign := &preparedWeb{}
	if err := foreign.Commit(); err == nil {
		t.Fatal("foreign prepared child committed")
	}
}

type fileErrorListener struct{ net.Listener }

func (fileErrorListener) File() (*os.File, error) { return nil, errors.New("duplicate failed") }

func TestPreparedWebCanAbortWithoutDisturbingCurrent(t *testing.T) {
	coreMux := http.NewServeMux()
	coreMux.HandleFunc("POST /_scimux/status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	core, err := backend.Listen(t.TempDir(), coreMux)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cmd := &Command{Stdout: io.Discard, Stderr: io.Discard,
		listenAddr: ln.Addr().String(), Config: remoteConfigForWebChildTest(t.TempDir())}
	s, err := newWebSupervisor(ln, core.Link(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	s.readyTimeout, s.drainTimeout = 10*time.Second, 30*time.Second
	s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
	s.extraEnv = []string{"SCIMUX_WEB_CHILD_TEST=1"}
	exe, _ := os.Executable()
	if err := s.Start(context.Background(), exe); err != nil {
		t.Fatal(err)
	}
	first := s.current
	prepared, err := s.Prepare(nil, exe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(context.Background(), exe); err == nil {
		t.Fatal("prepared two replacements")
	}
	prepared.Abort()
	prepared.Abort()
	if s.current != first || s.generation != 1 {
		t.Fatal("abort disturbed current generation")
	}
	if err := prepared.Commit(); err == nil {
		t.Fatal("aborted child committed")
	}
	if err := s.commitLocked(nil); err == nil {
		t.Fatal("nil prepared child committed")
	}
	pending, err := s.Prepare(context.Background(), exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pending.Commit(); err == nil {
		t.Fatal("standby committed after supervisor close")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if first.err() != nil {
		t.Fatalf("graceful child exit = %v", first.err())
	}
	var nilSupervisor *webSupervisor
	if err := nilSupervisor.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestListenerStaysNonblockingDuringGatedReplacement(t *testing.T) {
	coreMux := http.NewServeMux()
	coreMux.HandleFunc("POST /_scimux/status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	core, err := backend.Listen(t.TempDir(), coreMux)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cmd := &Command{Stdout: io.Discard, Stderr: io.Discard,
		listenAddr: ln.Addr().String(), Config: remoteConfigForWebChildTest(t.TempDir())}
	s, err := newWebSupervisor(ln, core.Link(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	s.readyTimeout, s.drainTimeout = 10*time.Second, 5*time.Second
	s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
	s.extraEnv = []string{"SCIMUX_WEB_CHILD_TEST=1"}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), exe); err != nil {
		t.Fatal(err)
	}
	first := s.current

	barrierDir := t.TempDir()
	marker, gate := filepath.Join(barrierDir, "started"), filepath.Join(barrierDir, "continue")
	s.childArgs = []string{"-test.run=^TestGatedWebChildHelperProcess$"}
	s.extraEnv = []string{
		"SCIMUX_WEB_CHILD_TEST=1",
		"SCIMUX_WEB_CHILD_MARKER=" + marker,
		"SCIMUX_WEB_CHILD_GATE=" + gate,
	}
	type prepareResult struct {
		prepared *preparedWeb
		err      error
	}
	preparedCh := make(chan prepareResult, 1)
	go func() {
		prepared, err := s.Prepare(context.Background(), exe)
		preparedCh <- prepareResult{prepared: prepared, err: err}
	}()
	waitForTestPath(t, marker)
	if !testListenerNonblocking(t, ln.(*net.TCPListener)) {
		t.Fatal("replacement descriptor transfer made the shared listener blocking")
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err := os.WriteFile(gate, []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := <-preparedCh
	if result.err != nil {
		t.Fatal(result.err)
	}
	result.prepared.Abort()

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("active child did not shut down promptly after listener inheritance")
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	}
	if err := first.err(); err != nil {
		t.Fatalf("graceful child exit = %v", err)
	}
}

func TestListenerDescriptorDoesNotLeakIntoConcurrentExec(t *testing.T) {
	if testing.Short() {
		t.Skip("launches subprocesses to stress the fork boundary")
	}
	if runtime.GOOS != "linux" {
		t.Skip("uses procfs to identify inherited descriptors")
	}
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	raw, err := ln.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var target string
	var readErr error
	if err := raw.Control(func(fd uintptr) {
		target, readErr = os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	}); err != nil {
		t.Fatal(err)
	}
	if readErr != nil || target == "" {
		t.Fatalf("identify listener descriptor: %q, %v", target, readErr)
	}

	done := make(chan struct{})
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				f, err := listenerFileForExec(ln)
				if err != nil {
					t.Errorf("duplicate listener: %v", err)
					return
				}
				_ = f.Close()
			}
		}()
	}
	defer func() {
		close(done)
		workers.Wait()
	}()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		cmd := exec.Command(exe, "-test.run=^TestUnrelatedExecDescriptorProbe$")
		cmd.Env = append(os.Environ(), "SCIMUX_LISTENER_PROBE="+target)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("unrelated exec %d inherited listener: %v\n%s", i, err, out)
		}
	}
}

func testListenerNonblocking(t *testing.T, ln *net.TCPListener) bool {
	t.Helper()
	raw, err := ln.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags uintptr
	var errno syscall.Errno
	if err := raw.Control(func(fd uintptr) {
		flags, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if errno != 0 {
		t.Fatalf("read listener flags: %v", errno)
	}
	return flags&syscall.O_NONBLOCK != 0
}

func waitForTestPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestWaitWebEventFailures(t *testing.T) {
	process := func() (*webProcess, chan webChildEvent, chan error) {
		events := make(chan webChildEvent)
		eventErr := make(chan error)
		return &webProcess{events: events, eventErr: eventErr, done: make(chan struct{})}, events, eventErr
	}
	t.Run("wrong-phase", func(t *testing.T) {
		p, events, _ := process()
		go func() { events <- webChildEvent{Phase: "wrong"} }()
		if _, err := waitWebEvent(context.Background(), p, "ready", time.Second); err == nil {
			t.Fatal("accepted wrong phase")
		}
	})
	t.Run("closed-events", func(t *testing.T) {
		p, events, _ := process()
		close(events)
		if _, err := waitWebEvent(context.Background(), p, "ready", time.Second); err == nil {
			t.Fatal("accepted closed readiness stream")
		}
	})
	t.Run("decode-error", func(t *testing.T) {
		p, _, eventErr := process()
		go func() { eventErr <- errors.New("decode") }()
		if _, err := waitWebEvent(context.Background(), p, "ready", time.Second); err == nil {
			t.Fatal("accepted readiness decoder error")
		}
	})
	t.Run("exit", func(t *testing.T) {
		p, _, _ := process()
		p.waitErr = errors.New("exit")
		close(p.done)
		if _, err := waitWebEvent(context.Background(), p, "ready", time.Second); err == nil {
			t.Fatal("accepted early exit")
		}
	})
	t.Run("clean-exit", func(t *testing.T) {
		p, _, _ := process()
		close(p.done)
		if _, err := waitWebEvent(context.Background(), p, "ready", time.Second); err == nil || strings.Contains(err.Error(), "%!w") {
			t.Fatalf("clean early exit error = %v", err)
		}
	})
	t.Run("context", func(t *testing.T) {
		p, _, _ := process()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := waitWebEvent(ctx, p, "ready", time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("context error = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		p, _, _ := process()
		if _, err := waitWebEvent(context.Background(), p, "ready", time.Millisecond); err == nil {
			t.Fatal("accepted readiness timeout")
		}
	})
	stopWebProcess(nil, time.Millisecond)
	stopWebProcess(&webProcess{}, time.Millisecond)

	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan webChildEvent, 1)
	eventErr := make(chan error, 1)
	go decodeWebEvents(readyR, events, eventErr)
	_, _ = io.WriteString(readyW, "not-json")
	_ = readyW.Close()
	if err := <-eventErr; err == nil {
		t.Fatal("malformed readiness stream had no decoder error")
	}
}

func TestRecoverStateGuards(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	(&webSupervisor{prepared: &preparedWeb{}}).Recover(ctx, "unused", nil)
	(&webSupervisor{closed: true}).Recover(context.Background(), "unused", nil)

	done := make(chan struct{})
	s := &webSupervisor{current: &webProcess{done: done}}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	s.Recover(ctx, "unused", nil)
}

func TestCommitRejectsChangedGenerationAndActivationFailures(t *testing.T) {
	newCandidate := func(t *testing.T) (*webProcess, *os.File) {
		t.Helper()
		activateR, activateW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		return &webProcess{
			activate: activateW, events: make(chan webChildEvent, 1),
			eventErr: make(chan error), done: make(chan struct{}),
		}, activateR
	}

	s := &webSupervisor{drainTimeout: time.Millisecond}
	candidate, activateR := newCandidate(t)
	defer activateR.Close()
	changed := &preparedWeb{supervisor: s, candidate: candidate, generation: 2}
	if err := s.commitLocked(changed); err == nil {
		t.Fatal("committed a skipped generation")
	}

	s = &webSupervisor{drainTimeout: time.Millisecond}
	candidate, activateR = newCandidate(t)
	_ = candidate.activate.Close()
	defer activateR.Close()
	failedWrite := &preparedWeb{supervisor: s, candidate: candidate, generation: 1}
	if err := s.commitLocked(failedWrite); err == nil || !strings.Contains(err.Error(), "activate") {
		t.Fatalf("closed activation pipe error = %v", err)
	}

	s = &webSupervisor{drainTimeout: time.Millisecond}
	candidate, activateR = newCandidate(t)
	defer activateR.Close()
	badEvents := make(chan webChildEvent, 1)
	badEvents <- webChildEvent{Phase: "ready"}
	candidate.events = badEvents
	wrongActive := &preparedWeb{supervisor: s, candidate: candidate, generation: 1}
	if err := s.commitLocked(wrongActive); err == nil {
		t.Fatal("committed child without active event")
	}

	foreign := &webSupervisor{}
	if err := s.commitLocked(&preparedWeb{supervisor: foreign}); err == nil {
		t.Fatal("committed child prepared by another supervisor")
	}
}

type failNthWriter struct {
	writes int
	failAt int
}

func (w *failNthWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, errors.New("write failed")
	}
	return len(p), nil
}

func TestServeWebChildSetupAndHandshakeFailures(t *testing.T) {
	base := webChildConfig{
		Link: backend.Link{Socket: "/missing/muxer.sock", Token: "cap"}, Generation: 1,
		ListenAddr: "127.0.0.1:8787", DataDir: t.TempDir(),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := serveWebChild(context.Background(), webChildConfig{}, ln, io.Discard, strings.NewReader(""),
		strings.NewReader(""), io.Discard, io.Discard, webChildDeps{}); err == nil {
		t.Fatal("accepted an incomplete muxer link")
	}
	if err := serveWebChild(context.Background(), base, ln, io.Discard, strings.NewReader(""),
		strings.NewReader(""), io.Discard, io.Discard, webChildDeps{}); err == nil {
		t.Fatal("accepted an unreachable muxer")
	}

	core, err := backend.Listen(t.TempDir(), http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	base.Link = core.Link()
	badPolicy := base
	badPolicy.ListenAddr = "not-a-listen-address"
	if err := serveWebChild(context.Background(), badPolicy, ln, io.Discard, strings.NewReader(""),
		strings.NewReader(""), io.Discard, io.Discard, webChildDeps{}); err == nil {
		t.Fatal("accepted invalid public-listener policy")
	}

	remoteFatal := base
	remoteFatal.Remote = true
	fatalClient := newFakeWebRemote("unavailable")
	fatalClient.startErr = errors.New("fatal remote start")
	if err := serveWebChild(context.Background(), remoteFatal, ln, io.Discard, strings.NewReader(""),
		strings.NewReader(""), io.Discard, io.Discard,
		webChildDeps{newRemote: func(remote.Config) webRemoteClient { return fatalClient }}); err == nil {
		t.Fatal("accepted fatal initial remote failure")
	}

	degradedClient := newFakeWebRemote("unavailable")
	degradedClient.startErr = &remote.Error{Class: remote.ClassUnavailable, Op: "start", Guidance: "offline"}
	var ready, stderr bytes.Buffer
	err = serveWebChild(context.Background(), remoteFatal, ln, &ready, strings.NewReader(""), strings.NewReader(""),
		io.Discard, &stderr, webChildDeps{newRemote: func(remote.Config) webRemoteClient { return degradedClient }})
	if err == nil || !strings.Contains(err.Error(), "activation") || !strings.Contains(stderr.String(), "remote access is off") {
		t.Fatalf("degraded activation = %v, stderr=%q", err, stderr.String())
	}

	writer := &failNthWriter{failAt: 1}
	if err := serveWebChild(context.Background(), base, ln, writer, strings.NewReader(""), strings.NewReader(""),
		io.Discard, io.Discard, webChildDeps{}); err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("readiness write error = %v", err)
	}

	activeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	writer = &failNthWriter{failAt: 2}
	err = serveWebChild(context.Background(), base, activeLn, writer, strings.NewReader("x"), strings.NewReader(""),
		io.Discard, io.Discard, webChildDeps{})
	_ = activeLn.Close()
	if err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("activation acknowledgement error = %v", err)
	}

	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = closedLn.Close()
	ready.Reset()
	err = serveWebChild(context.Background(), base, closedLn, &ready, strings.NewReader("x"), strings.NewReader(""),
		io.Discard, io.Discard, webChildDeps{})
	if err == nil {
		t.Fatal("accepted a failed public listener")
	}
}

func FuzzWebChildConfiguration(f *testing.F) {
	f.Add(
		`{"Link":{"Socket":"/tmp/muxer.sock","Token":"cap"},"Generation":1,"ListenAddr":"127.0.0.1:8787","DataDir":"/tmp/data","CSRFToken":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		"1", "false", "false", "false", `[]`, "/tmp/muxer.sock", "cap", "127.0.0.1:8787", "/tmp/data", strings.Repeat("a", 64),
	)
	f.Add(`{"Generation":0}`, "0", "maybe", "x", "x", `{`, "", "", "", "", "bad-csrf")
	f.Fuzz(func(t *testing.T, payload, generation, replacement, remoteFlag, inviteStdin, trusted, socket, token, addr, data, legacyCSRF string) {
		cfg, err := readWebChildConfig(strings.NewReader(payload))
		if err == nil && (cfg.Generation == 0 || cfg.Link.Socket == "" || cfg.Link.Token == "" || cfg.ListenAddr == "" || cfg.DataDir == "" || !validCSRFToken(cfg.CSRFToken)) {
			t.Fatalf("successful parse violated required fields: %#v", cfg)
		}

		env := map[string]string{
			envGeneration: generation, envReplacement: replacement, envRemote: remoteFlag,
			envInviteStdin: inviteStdin, envTrusted: trusted, envCoreSocket: socket,
			envCoreToken: token, envListenAddr: addr, envDataDir: data, envCSRFToken: legacyCSRF,
		}
		cfg, err = loadLegacyWebChildConfig(func(key string) string { return env[key] })
		if err == nil && (cfg.Generation == 0 || cfg.Link.Socket == "" || cfg.Link.Token == "" || cfg.ListenAddr == "" || cfg.DataDir == "" || !validCSRFToken(cfg.CSRFToken)) {
			t.Fatalf("successful legacy parse violated required fields: %#v", cfg)
		}
	})
}

func remoteConfigForWebChildTest(data string) remote.Config {
	return remote.Config{DataDir: data}
}

func assertWebGeneration(t *testing.T, addr, want string) {
	t.Helper()
	resp, err := testHTTPClient.Get("http://" + addr + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != `{"marker":"muxer-alive"}` {
		t.Fatalf("split response = %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Scimux-Web-Generation"); got != want {
		t.Fatalf("web generation = %q, want %q", got, want)
	}
}

func assertWebGenerationEventually(t *testing.T, addr, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := testPollHTTPClient.Get("http://" + addr + "/api/state")
		if err == nil {
			_ = resp.Body.Close()
			if resp.Header.Get("X-Scimux-Web-Generation") == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("web generation %s did not become reachable", want)
}

func fetchCSRF(t *testing.T, addr string) string {
	t.Helper()
	resp, err := testHTTPClient.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	const marker = `name="scimux-csrf" content="`
	_, rest, ok := strings.Cut(string(b), marker)
	if !ok {
		t.Fatal("index has no CSRF meta tag")
	}
	token, _, ok := strings.Cut(rest, `"`)
	if !ok || token == "" {
		t.Fatal("index has malformed CSRF token")
	}
	return token
}

func postMutation(addr, csrf string) error {
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/mutate", strings.NewReader(`{}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scimux-CSRF", csrf)
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mutation = %d (%s)", resp.StatusCode, body)
	}
	return nil
}
