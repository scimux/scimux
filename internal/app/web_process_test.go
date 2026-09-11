package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
	if err := runWebChild(context.Background(), os.Getenv, os.Stdin, os.Stdout, os.Stderr); err != nil {
		t.Fatal(err)
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
	if err := postMutation(ln.Addr().String(), fetchCSRF(t, ln.Addr().String())); err != nil {
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
	}{
		{"local-initial", false, false, false},
		{"remote-initial", true, false, false},
		{"remote-replacement", true, true, false},
		{"remote-replacement-failure", true, true, true},
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
			var active webChildEvent
			if err := dec.Decode(&active); err != nil || active.Phase != "active" {
				t.Fatalf("active = %#v, %v", active, err)
			}
			if tc.remote && tc.replacement {
				select {
				case <-fakeRemote.started:
				case <-time.After(2 * time.Second):
					t.Fatal("replacement remote did not start after activation")
				}
				if tc.startErr {
					deadline := time.Now().Add(2 * time.Second)
					for !strings.Contains(childStderr.String(), "remote restart failed") && time.Now().Before(deadline) {
						time.Sleep(time.Millisecond)
					}
					if !strings.Contains(childStderr.String(), "replacement remote failed") {
						t.Fatalf("replacement error was not reported: %q", childStderr.String())
					}
				}
			}
			resp, err := http.Get("http://" + ln.Addr().String() + "/api/state")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != "state" || resp.Header.Get("X-Scimux-Web-Generation") != "9" {
				t.Fatalf("public response = %q headers %v", body, resp.Header)
			}
			select {
			case status := <-statusReports:
				if status.Generation != 9 || status.Version == "" || (tc.remote && status.Remote == nil) {
					t.Fatalf("published status = %#v", status)
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

type fakeWebRemote struct {
	hostedPairingClient
	status    string
	startErr  error
	started   chan struct{}
	closed    chan struct{}
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
	return &fakeWebRemote{status: status, started: make(chan struct{}), closed: make(chan struct{})}
}

func (f *fakeWebRemote) Start(context.Context) error {
	f.startOnce.Do(func() { close(f.started) })
	return f.startErr
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
		RVOrigin: "https://rv.example",
	}
	env := map[string]string{}
	for _, entry := range encodeWebChildEnv(cfg) {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}
	got, err := loadWebChildConfig(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != cfg.Generation || !got.Replacement || !got.Remote || got.TrustedHosts[0] != "lab.example" {
		t.Fatalf("decoded config = %#v", got)
	}
	for _, key := range []string{envGeneration, envReplacement, envRemote, envInviteStdin, envTrusted, envCoreSocket} {
		t.Run(key, func(t *testing.T) {
			bad := maps.Clone(env)
			bad[key] = ""
			if _, err := loadWebChildConfig(func(k string) string { return bad[k] }); err == nil {
				t.Fatal("accepted malformed environment")
			}
		})
	}
	if _, err := loadWebChildConfig(nil); err == nil {
		t.Fatal("accepted nil environment")
	}
	if _, err := readReadyLine("not-json"); err == nil {
		t.Fatal("accepted malformed readiness line")
	}
	if event, err := readReadyLine(`{"phase":"ready","generation":1,"version":"v1"}`); err != nil || event.Phase != "ready" {
		t.Fatalf("read readiness = %#v, %v", event, err)
	}
}

func TestWebSupervisorConstructionAndPreparedGuards(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	link := backend.Link{Socket: "/tmp/muxer.sock", Token: "cap"}
	cmd := &Command{Stdout: io.Discard, Stderr: io.Discard}
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
	s.readyTimeout, s.drainTimeout = 10*time.Second, 10*time.Second
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

func FuzzWebChildEnvironment(f *testing.F) {
	f.Add("1", "false", "false", "false", `[]`, "/tmp/muxer.sock", "cap", "127.0.0.1:8787", "/tmp/data")
	f.Add("0", "maybe", "x", "x", `{`, "", "", "", "")
	f.Fuzz(func(t *testing.T, generation, replacement, remoteFlag, inviteStdin, trusted, socket, token, addr, data string) {
		env := map[string]string{
			envGeneration: generation, envReplacement: replacement, envRemote: remoteFlag,
			envInviteStdin: inviteStdin, envTrusted: trusted, envCoreSocket: socket,
			envCoreToken: token, envListenAddr: addr, envDataDir: data,
		}
		cfg, err := loadWebChildConfig(func(key string) string { return env[key] })
		if err == nil && (cfg.Generation == 0 || cfg.Link.Socket == "" || cfg.Link.Token == "" || cfg.ListenAddr == "" || cfg.DataDir == "") {
			t.Fatalf("successful parse violated required fields: %#v", cfg)
		}
	})
}

func remoteConfigForWebChildTest(data string) remote.Config {
	return remote.Config{DataDir: data}
}

func assertWebGeneration(t *testing.T, addr, want string) {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/api/state")
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
		resp, err := http.Get("http://" + addr + "/api/state")
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
	resp, err := http.Get("http://" + addr + "/")
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
	resp, err := http.DefaultClient.Do(req)
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
