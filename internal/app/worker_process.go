package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/scimux/scimux/internal/sessionworker"
)

const (
	sessionWorkerCmd       = "session-worker"
	workerConfigFDEnv      = "SCIMUX_SESSION_WORKER_CONFIG_FD"
	workerReadyFDEnv       = "SCIMUX_SESSION_WORKER_READY_FD"
	workerReadinessTimeout = 30 * time.Second
)

func runSessionWorkerMain() int {
	config, ready, err := readSessionWorkerStartup()
	if err == nil {
		var harness sessionworker.Harness
		harness, err = newProductionSessionHarness(config)
		if err == nil {
			err = serveSessionWorker(config, ready, harness)
		} else if ready != nil {
			_ = ready.Close()
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		return 1
	}
	return 0
}

type sessionWorkerConfig struct {
	DataDir  string                 `json:"data_dir"`
	Home     string                 `json:"home,omitempty"`
	Socket   string                 `json:"tmux_socket,omitempty"`
	NodeID   string                 `json:"node_id"`
	Identity sessionworker.Identity `json:"identity"`
}

func (c sessionWorkerConfig) validate() error {
	if c.DataDir == "" || c.NodeID == "" || c.Identity.WorkerID == "" || c.Identity.Agent == "" {
		return errors.New("session worker: incomplete startup configuration")
	}
	return nil
}

type sessionWorkerStartOptions struct {
	args    []string
	env     []string
	stdout  io.Writer
	stderr  io.Writer
	timeout time.Duration
}

type sessionWorkerProcess struct {
	pid    int
	client *sessionworker.Client
	wait   chan struct{}
	mu     sync.Mutex
	err    error
}

// startSessionWorker uses inherited pipes for startup configuration and the
// returned capability. Nothing sensitive enters argv, and neither pipe
// remains part of the worker's lifetime after readiness.
func startSessionWorker(ctx context.Context, executable string, config sessionWorkerConfig, opts sessionWorkerStartOptions) (*sessionWorkerProcess, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if executable == "" {
		return nil, errors.New("session worker: empty executable")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	configRead, configWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		_ = configRead.Close()
		_ = configWrite.Close()
		return nil, err
	}
	closePipes := func() {
		_ = configRead.Close()
		_ = configWrite.Close()
		_ = readyRead.Close()
		_ = readyWrite.Close()
	}
	args := opts.args
	if len(args) == 0 {
		args = []string{sessionWorkerCmd}
	}
	cmd := exec.Command(executable, args...)
	cmd.ExtraFiles = []*os.File{configRead, readyWrite}
	cmd.Env = append(os.Environ(), opts.env...)
	cmd.Env = append(cmd.Env, workerConfigFDEnv+"=3", workerReadyFDEnv+"=4")
	cmd.Stdin = nil
	cmd.Stdout = opts.stdout
	cmd.Stderr = opts.stderr
	if err := cmd.Start(); err != nil {
		closePipes()
		return nil, err
	}
	_ = configRead.Close()
	_ = readyWrite.Close()
	p := &sessionWorkerProcess{pid: cmd.Process.Pid, wait: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.wait)
	}()

	if err := json.NewEncoder(configWrite).Encode(config); err != nil {
		_ = configWrite.Close()
		_ = readyRead.Close()
		_ = cmd.Process.Kill()
		return nil, err
	}
	_ = configWrite.Close()

	type readyResult struct {
		locator sessionworker.Locator
		err     error
	}
	readyCh := make(chan readyResult, 1)
	go func() {
		var locator sessionworker.Locator
		err := json.NewDecoder(io.LimitReader(readyRead, 16<<10)).Decode(&locator)
		_ = readyRead.Close()
		readyCh <- readyResult{locator: locator, err: err}
	}()
	timeout := opts.timeout
	if timeout <= 0 {
		timeout = workerReadinessTimeout
	}
	var ready readyResult
	select {
	case ready = <-readyCh:
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return nil, ctx.Err()
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return nil, errors.New("session worker: readiness timeout")
	}
	if ready.err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("session worker: readiness: %w", ready.err)
	}
	if ready.locator.NodeID != config.NodeID || ready.locator.WorkerID != config.Identity.WorkerID || ready.locator.PID != p.pid {
		_ = cmd.Process.Kill()
		return nil, errors.New("session worker: readiness identity mismatch")
	}
	client, err := sessionworker.NewClient(ready.locator.Link)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	hello, err := client.Hello(ctx)
	if err == nil {
		err = sessionworker.CheckCompatibility(hello)
	}
	if err != nil || hello.Identity != config.Identity {
		_ = client.Close()
		_ = cmd.Process.Kill()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("session worker: hello identity mismatch")
	}
	p.client = client
	return p, nil
}

func (p *sessionWorkerProcess) waitForExit(timeout time.Duration) error {
	if p == nil {
		return nil
	}
	select {
	case <-p.wait:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.err
	case <-time.After(timeout):
		return errors.New("session worker: did not exit")
	}
}

func inheritedFile(name string) (*os.File, error) {
	raw := os.Getenv(name)
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("session worker: invalid %s", name)
	}
	return os.NewFile(uintptr(fd), name), nil
}

func readSessionWorkerStartup() (sessionWorkerConfig, *os.File, error) {
	configFile, err := inheritedFile(workerConfigFDEnv)
	if err != nil {
		return sessionWorkerConfig{}, nil, err
	}
	defer configFile.Close()
	ready, err := inheritedFile(workerReadyFDEnv)
	if err != nil {
		return sessionWorkerConfig{}, nil, err
	}
	var config sessionWorkerConfig
	if err := json.NewDecoder(io.LimitReader(configFile, 64<<10)).Decode(&config); err != nil {
		_ = ready.Close()
		return sessionWorkerConfig{}, nil, err
	}
	if err := config.validate(); err != nil {
		_ = ready.Close()
		return sessionWorkerConfig{}, nil, err
	}
	return config, ready, nil
}

// serveSessionWorker owns registration and the private socket for the whole
// process lifetime. Loss of the spawning muxer is deliberately absent from
// the wait set; only an authenticated Stop or a direct process signal ends it.
func serveSessionWorker(config sessionWorkerConfig, ready *os.File, harness sessionworker.Harness) error {
	if err := config.validate(); err != nil {
		return err
	}
	if ready == nil || harness == nil {
		return errors.New("session worker: missing ready pipe or harness")
	}
	defer ready.Close()
	registration, err := sessionworker.Claim(config.DataDir, config.NodeID)
	if err != nil {
		return err
	}
	defer registration.Close()
	server, err := sessionworker.Listen("", config.Identity, harness)
	if err != nil {
		return err
	}
	defer server.Close()
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("session worker: locate executable: %w", err)
	}
	locator := sessionworker.Locator{
		Identity: config.Identity, NodeID: config.NodeID, PID: os.Getpid(),
		Executable: executable, Link: server.Link(),
	}
	if err := registration.Publish(locator); err != nil {
		return err
	}
	if err := json.NewEncoder(ready).Encode(locator); err != nil {
		return err
	}
	if err := ready.Close(); err != nil {
		return err
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	select {
	case <-server.Stopped():
		return nil
	case <-stop:
		return harness.Stop(context.Background(), config.Identity.Agent != "claude")
	}
}
