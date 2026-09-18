package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/sessionworker"
)

type syntheticSessionHarness struct {
	mu        sync.Mutex
	launched  bool
	state     sessionworker.State
	stop      bool
	stopErr   error
	terminate bool
	events    []sessionlog.Event
	peekMode  string
}

var errSyntheticWorkerConflict = errors.New("synthetic worker conflict")

func (h *syntheticSessionHarness) Launch(_ context.Context, req sessionworker.LaunchRequest) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if req.NodeID == "" || req.Agent == "" {
		return "", errors.New("bad launch")
	}
	h.launched = true
	launch := req
	h.state = sessionworker.State{Launch: &launch, HasSession: true, SessionID: "synthetic-session", Live: "quiet", PermissionBoundary: &sessionworker.PermissionBoundary{Incarnation: "synthetic", MaxSequence: 0}}
	if req.Agent == "claude" {
		h.state.HookID = "hook-synthetic"
		h.state.HookGeneration = 3
	}
	return "synthetic-session", nil
}

func (h *syntheticSessionHarness) Send(context.Context, string) (sessionworker.Delivery, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.launched {
		return sessionworker.Delivery{}, errors.New("not launched")
	}
	h.state.Live = "active"
	h.state.Attention = "approval"
	h.state.Permission = &sessionworker.PendingPermission{RequestID: "synthetic:1", Title: "read a file", Options: []sessionworker.PermissionOption{{Key: "1", Name: "Allow once", Kind: "allow"}}}
	h.state.PermissionBoundary.MaxSequence = 1
	return sessionworker.Delivery{Status: sessionworker.DeliveryAcknowledged}, nil
}

func (h *syntheticSessionHarness) Clear(context.Context) (sessionworker.Delivery, error) {
	return sessionworker.Delivery{Status: sessionworker.DeliveryAcknowledged}, nil
}
func (h *syntheticSessionHarness) ResolveDelivery(context.Context) error { return nil }
func (h *syntheticSessionHarness) Interrupt(context.Context) (sessionworker.ActionEvidence, error) {
	return sessionworker.ActionEvidence{Evidence: "interrupt: synthetic pane", Keys: []string{"Escape"}}, nil
}
func (h *syntheticSessionHarness) PreparePermission(_ context.Context, d sessionworker.PermissionDecision) (sessionworker.PreparedPermission, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state.Permission == nil || d.RequestID != h.state.Permission.RequestID || d.Key != "1" {
		return sessionworker.PreparedPermission{}, errSyntheticWorkerConflict
	}
	return sessionworker.PreparedPermission{Token: "allow-once", Evidence: h.state.Permission.Title, Keys: []string{d.Key}}, nil
}
func (h *syntheticSessionHarness) DeliverPermission(_ context.Context, token string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if token != "allow-once" || h.state.Permission == nil {
		return errSyntheticWorkerConflict
	}
	h.state.Live, h.state.Attention, h.state.Permission = "quiet", "", nil
	return nil
}
func (h *syntheticSessionHarness) State(context.Context) sessionworker.State {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}
func (h *syntheticSessionHarness) Peek(_ context.Context, mode string) string {
	h.mu.Lock()
	h.peekMode = mode
	h.mu.Unlock()
	return "synthetic event log"
}
func (h *syntheticSessionHarness) SetAutoApprove(_ context.Context, enabled bool) (sessionworker.AutoApprove, error) {
	return sessionworker.AutoApprove{Supported: true, Enabled: enabled, Phase: "armed"}, nil
}
func (h *syntheticSessionHarness) RecordStartFailure(context.Context, string) error {
	return nil
}

func (h *syntheticSessionHarness) AppendSessionEvent(_ context.Context, event sessionlog.Event) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
	return nil
}
func (h *syntheticSessionHarness) Stop(_ context.Context, terminate bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stop = true
	h.terminate = terminate
	return h.stopErr
}
func (h *syntheticSessionHarness) Conflict(err error) bool {
	return errors.Is(err, errSyntheticWorkerConflict)
}

func TestSessionWorkerHelperProcess(t *testing.T) {
	if os.Getenv("SCIMUX_SESSION_WORKER_TEST_HELPER") != "1" {
		return
	}
	config, ready, err := readSessionWorkerStartup()
	if err != nil {
		t.Fatal(err)
	}
	if raw := os.Getenv("SCIMUX_SESSION_WORKER_TEST_READY_DELAY"); raw != "" {
		delay, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(delay)
	}
	if err := serveSessionWorker(config, ready, &syntheticSessionHarness{}); err != nil {
		t.Fatal(err)
	}
}

// TestProductionSessionWorkerHelperProcess is the process entry point used by
// the public acceptance test. Unlike TestSessionWorkerHelperProcess, it uses
// the production harness factory and therefore crosses worker -> ACP -> agent.
func TestProductionSessionWorkerHelperProcess(t *testing.T) {
	if os.Getenv("SCIMUX_PRODUCTION_SESSION_WORKER_TEST") != "1" {
		return
	}
	config, ready, err := readSessionWorkerStartup()
	if err != nil {
		t.Fatal(err)
	}
	harness, err := newProductionSessionHarness(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := serveSessionWorker(config, ready, harness); err != nil {
		t.Fatal(err)
	}
}

func TestSessionWorkerStartupFailureHelperProcess(t *testing.T) {
	mode := os.Getenv("SCIMUX_SESSION_WORKER_STARTUP_FAILURE")
	if mode == "" {
		return
	}
	config, ready, err := readSessionWorkerStartup()
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	switch mode {
	case "timeout":
		time.Sleep(time.Second)
	case "malformed":
		_, _ = io.WriteString(ready, "{")
	case "identity":
		_ = json.NewEncoder(ready).Encode(sessionworker.Locator{
			Identity: config.Identity, NodeID: config.NodeID, PID: os.Getpid() + 1,
			Link: sessionworker.Link{Socket: "/absent", Token: "token"},
		})
	case "empty-link":
		_ = json.NewEncoder(ready).Encode(sessionworker.Locator{Identity: config.Identity, NodeID: config.NodeID, PID: os.Getpid()})
	case "unreachable":
		_ = json.NewEncoder(ready).Encode(sessionworker.Locator{
			Identity: config.Identity, NodeID: config.NodeID, PID: os.Getpid(),
			Link: sessionworker.Link{Socket: filepath.Join(config.DataDir, "absent.sock"), Token: "token"},
		})
	default:
		t.Fatalf("unknown startup failure mode %q", mode)
	}
}

func TestSessionWorkerStartupFailsClosedBeforeReadiness(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := sessionWorkerConfig{
		DataDir: data, NodeID: "startup-failure",
		Identity: sessionworker.Identity{WorkerID: "worker-startup-failure", Agent: "pi"},
	}
	for _, mode := range []string{"malformed", "identity", "empty-link", "unreachable"} {
		t.Run(mode, func(t *testing.T) {
			if process, err := startSessionWorker(context.Background(), exe, config, sessionWorkerStartOptions{
				args: []string{"-test.run=^TestSessionWorkerStartupFailureHelperProcess$"},
				env:  []string{"SCIMUX_SESSION_WORKER_STARTUP_FAILURE=" + mode},
			}); err == nil || process != nil {
				t.Fatalf("startup %s = %#v, %v", mode, process, err)
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		if process, err := startSessionWorker(context.Background(), exe, config, sessionWorkerStartOptions{
			args: []string{"-test.run=^TestSessionWorkerStartupFailureHelperProcess$"},
			env:  []string{"SCIMUX_SESSION_WORKER_STARTUP_FAILURE=timeout"}, timeout: 10 * time.Millisecond,
		}); err == nil || process != nil {
			t.Fatalf("timeout startup = %#v, %v", process, err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if process, err := startSessionWorker(ctx, exe, config, sessionWorkerStartOptions{
			args: []string{"-test.run=^TestSessionWorkerStartupFailureHelperProcess$"},
			env:  []string{"SCIMUX_SESSION_WORKER_STARTUP_FAILURE=timeout"},
		}); err == nil || process != nil {
			t.Fatalf("cancelled startup = %#v, %v", process, err)
		}
	})
}

func TestSyntheticSessionWorkerSurvivesClientLossAndReattaches(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := sessionWorkerConfig{DataDir: data, Identity: sessionworker.Identity{WorkerID: "worker-old", Agent: "opencode", Build: "v1.06"}, NodeID: "chat-one"}
	process, err := startSessionWorker(context.Background(), exe, config, sessionWorkerStartOptions{
		args: []string{"-test.run=^TestSessionWorkerHelperProcess$"},
		env:  []string{"SCIMUX_SESSION_WORKER_TEST_HELPER=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if process.client != nil {
			_ = process.client.Stop(context.Background(), true)
		}
		_ = process.waitForExit(3 * time.Second)
	})
	workerPID := process.pid
	ctx := context.Background()
	launch := sessionworker.LaunchRequest{NodeID: "chat-one", Agent: "opencode", Dir: data, Model: "cheap"}
	if sid, err := process.client.Launch(ctx, launch); err != nil || sid != "synthetic-session" {
		t.Fatalf("Launch = %q, %v", sid, err)
	}
	if _, err := process.client.Send(ctx, "read one file"); err != nil {
		t.Fatal(err)
	}
	before, err := process.client.State(ctx)
	if err != nil || before.Permission == nil {
		t.Fatalf("pending before disconnect = %#v, %v", before, err)
	}

	// Closing every muxer-owned client connection models a muxer crash or
	// generation handoff. It must not be interpreted as session-worker death.
	_ = process.client.Close()
	process.client = nil
	locator, err := sessionworker.Discover(data, "chat-one")
	if err != nil {
		t.Fatal(err)
	}
	if locator.PID != workerPID {
		t.Fatalf("registered PID = %d, want %d", locator.PID, workerPID)
	}
	reattached, err := sessionworker.NewClient(locator.Link)
	if err != nil {
		t.Fatal(err)
	}
	defer reattached.Close()
	hello, err := reattached.Hello(ctx)
	if err != nil || hello.Build != "v1.06" || hello.WorkerID != "worker-old" {
		t.Fatalf("reattach hello = %#v, %v", hello, err)
	}
	after, err := reattached.State(ctx)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("state across reattach = %#v, %v; want %#v", after, err, before)
	}
	prepared, err := reattached.PreparePermission(ctx, sessionworker.PermissionDecision{RequestID: "synthetic:1", Key: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := reattached.DeliverPermission(ctx, prepared.Token); err != nil {
		t.Fatal(err)
	}
	settled, err := reattached.State(ctx)
	if err != nil || settled.Permission != nil || settled.Live != "quiet" {
		t.Fatalf("settled = %#v, %v", settled, err)
	}
	if err := reattached.Stop(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := process.waitForExit(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionworker.Discover(data, "chat-one"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("locator survived worker exit: %v", err)
	}
}

func TestRunSessionWorkerMainServesUntilAuthenticatedStop(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	configRead, configWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = configWrite.Close()
		_ = readyRead.Close()
	})
	configFD, err := syscall.Dup(int(configRead.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	readyFD, err := syscall.Dup(int(readyWrite.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	syscall.CloseOnExec(configFD)
	syscall.CloseOnExec(readyFD)
	_ = configRead.Close()
	_ = readyWrite.Close()
	t.Setenv(workerConfigFDEnv, strconv.Itoa(configFD))
	t.Setenv(workerReadyFDEnv, strconv.Itoa(readyFD))
	config := sessionWorkerConfig{
		DataDir: data, NodeID: "run-main",
		Identity: sessionworker.Identity{WorkerID: "worker-main", Agent: "opencode", Build: "old"},
	}
	if err := json.NewEncoder(configWrite).Encode(config); err != nil {
		t.Fatal(err)
	}
	if err := configWrite.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- runSessionWorkerMain() }()
	var locator sessionworker.Locator
	if err := json.NewDecoder(readyRead).Decode(&locator); err != nil {
		t.Fatal(err)
	}
	client, err := sessionworker.NewClient(locator.Link)
	if err != nil {
		t.Fatal(err)
	}
	if hello, err := client.Hello(context.Background()); err != nil || hello.Identity != config.Identity {
		t.Fatalf("Hello = %#v, %v", hello, err)
	}
	if err := client.Stop(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("worker main exit = %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker main did not exit after Stop")
	}
	if _, err := sessionworker.Discover(data, config.NodeID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker main left locator: %v", err)
	}
}

func TestSessionWorkerStartupValidationFailures(t *testing.T) {
	// A test launched inside scimux can inherit its worker environment, but
	// descriptor numbers now belong to this test process (including netpoll).
	// Establish the absent-files case rather than closing unrelated descriptors.
	t.Setenv(workerConfigFDEnv, "")
	t.Setenv(workerReadyFDEnv, "")
	if code := runSessionWorkerMain(); code != 1 {
		t.Fatalf("worker main without inherited files = %d", code)
	}
	if _, err := startSessionWorker(context.Background(), "", sessionWorkerConfig{}, sessionWorkerStartOptions{}); err == nil {
		t.Fatal("start accepted empty executable")
	}
	if _, err := startSessionWorker(context.Background(), "/does/not/exist", sessionWorkerConfig{}, sessionWorkerStartOptions{}); err == nil {
		t.Fatal("start accepted invalid config before executable lookup")
	}
	valid := sessionWorkerConfig{DataDir: t.TempDir(), NodeID: "node", Identity: sessionworker.Identity{WorkerID: "worker", Agent: "pi"}}
	if _, err := startSessionWorker(context.Background(), "/does/not/exist", valid, sessionWorkerStartOptions{}); err == nil {
		t.Fatal("start accepted missing executable")
	}
	if err := serveSessionWorker(sessionWorkerConfig{}, nil, &syntheticSessionHarness{}); err == nil {
		t.Fatal("serve accepted invalid config")
	}
	if err := serveSessionWorker(valid, nil, &syntheticSessionHarness{}); err == nil {
		t.Fatal("serve accepted nil ready pipe")
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = read.Close()
	if err := serveSessionWorker(valid, write, nil); err == nil {
		t.Fatal("serve accepted nil harness")
	}
	var nilProcess *sessionWorkerProcess
	if err := nilProcess.waitForExit(time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestSessionWorkerMainRejectsUnsupportedProductionHarness(t *testing.T) {
	configRead, configWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer configRead.Close()
	defer readyRead.Close()
	defer readyWrite.Close()
	config := sessionWorkerConfig{
		DataDir: t.TempDir(), NodeID: "unsupported",
		Identity: sessionworker.Identity{WorkerID: "worker-unsupported", Agent: "unknown"},
	}
	if err := json.NewEncoder(configWrite).Encode(config); err != nil {
		t.Fatal(err)
	}
	_ = configWrite.Close()
	configFD, err := syscall.Dup(int(configRead.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	readyFD, err := syscall.Dup(int(readyWrite.Fd()))
	if err != nil {
		_ = syscall.Close(configFD)
		t.Fatal(err)
	}
	t.Setenv(workerConfigFDEnv, strconv.Itoa(configFD))
	t.Setenv(workerReadyFDEnv, strconv.Itoa(readyFD))
	if code := runSessionWorkerMain(); code != 1 {
		t.Fatalf("worker main with unsupported harness = %d", code)
	}
}

func TestReadSessionWorkerStartupRejectsEachUntrustedInput(t *testing.T) {
	setFiles := func(t *testing.T, payload string, readyFD string) {
		t.Helper()
		configRead, configWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = configRead.Close() })
		if _, err := io.WriteString(configWrite, payload); err != nil {
			t.Fatal(err)
		}
		_ = configWrite.Close()
		configFD, err := syscall.Dup(int(configRead.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(workerConfigFDEnv, strconv.Itoa(configFD))
		t.Setenv(workerReadyFDEnv, readyFD)
	}

	t.Run("invalid-ready-descriptor", func(t *testing.T) {
		setFiles(t, `{}`, "2")
		if _, ready, err := readSessionWorkerStartup(); err == nil || ready != nil {
			t.Fatalf("invalid ready descriptor = %v, %v", ready, err)
		}
	})
	for _, tc := range []struct {
		name, payload string
	}{
		{name: "malformed-config", payload: `{`},
		{name: "incomplete-config", payload: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readyRead, readyWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = readyRead.Close()
				_ = readyWrite.Close()
			})
			readyFD, err := syscall.Dup(int(readyWrite.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			setFiles(t, tc.payload, strconv.Itoa(readyFD))
			if _, ready, err := readSessionWorkerStartup(); err == nil || ready != nil {
				t.Fatalf("startup %s = %v, %v", tc.name, ready, err)
			}
		})
	}
}

func TestServeSessionWorkerRejectsUnclaimableOwnership(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o755); err != nil {
		t.Fatal(err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	config := sessionWorkerConfig{DataDir: data, NodeID: "node", Identity: sessionworker.Identity{WorkerID: "worker", Agent: "pi"}}
	if err := serveSessionWorker(config, write, &syntheticSessionHarness{}); err == nil {
		t.Fatal("worker served without owner-only registration")
	}
}
