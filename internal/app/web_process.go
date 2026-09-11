package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"codeberg.org/chrberger/scimux/internal/backend"
	"codeberg.org/chrberger/scimux/internal/remote"
)

const webChildCmd = "web-child"

const (
	webPublicFD   = 3
	webReadyFD    = 4
	webActivateFD = 5
	webOwnerFD    = 6
)

const (
	envCoreSocket  = "SCIMUX_CORE_SOCKET"
	envCoreToken   = "SCIMUX_CORE_TOKEN"
	envGeneration  = "SCIMUX_WEB_GENERATION"
	envReplacement = "SCIMUX_WEB_REPLACEMENT"
	envListenAddr  = "SCIMUX_LISTEN_ADDR"
	envTrusted     = "SCIMUX_TRUSTED_HOSTS"
	envDataDir     = "SCIMUX_DATA_DIR"
	envRemote      = "SCIMUX_REMOTE"
	envInviteFile  = "SCIMUX_INVITE_FILE"
	envInviteStdin = "SCIMUX_INVITE_STDIN"
	envRVOrigin    = "SCIMUX_RENDEZVOUS_ORIGIN"
	envCSRFToken   = "SCIMUX_CSRF_TOKEN"
)

type webChildConfig struct {
	Link         backend.Link
	Generation   uint64
	Replacement  bool
	ListenAddr   string
	TrustedHosts []string
	DataDir      string
	Remote       bool
	InviteFile   string
	InviteStdin  bool
	RVOrigin     string
	CSRFToken    string
}

type webChildEvent struct {
	Phase      string `json:"phase"`
	Generation uint64 `json:"generation"`
	Version    string `json:"version"`
}

type webRemoteClient interface {
	hostedPairingClient
	Start(context.Context) error
	Close() error
}

type webChildDeps struct {
	newRemote func(remote.Config) webRemoteClient
	ownerDone <-chan struct{}
	ownerLost func()
}

func (d webChildDeps) remote(cfg remote.Config) webRemoteClient {
	if d.newRemote != nil {
		return d.newRemote(cfg)
	}
	return remote.NewClient(cfg)
}

type webProcess struct {
	cmd      *exec.Cmd
	activate *os.File
	owner    *os.File
	events   <-chan webChildEvent
	eventErr <-chan error
	done     chan struct{}
	waitMu   sync.Mutex
	waitErr  error
	version  string
}

func (p *webProcess) setWaitErr(err error) {
	if p.owner != nil {
		_ = p.owner.Close()
	}
	p.waitMu.Lock()
	p.waitErr = err
	p.waitMu.Unlock()
	close(p.done)
}

func (p *webProcess) err() error {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	return p.waitErr
}

// webSupervisor is muxer-owned. It is the only code that knows the hidden
// role, so users and service managers always launch plain `scimux`.
type webSupervisor struct {
	mu sync.Mutex

	listener net.Listener
	link     backend.Link
	config   webChildConfig
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer

	readyTimeout time.Duration
	drainTimeout time.Duration
	// childArgs/extraEnv are test seams for re-executing the Go test binary as
	// a real child. Production leaves them empty and runs only `web-child`.
	childArgs   []string
	extraEnv    []string
	beforeDrain func()
	generation  uint64
	current     *webProcess
	prepared    *preparedWeb
	closed      bool
}

type preparedWeb struct {
	supervisor *webSupervisor
	candidate  *webProcess
	expected   *webProcess
	generation uint64
}

func newWebSupervisor(listener net.Listener, link backend.Link, cmd *Command) (*webSupervisor, error) {
	if listener == nil {
		return nil, errors.New("web supervisor: nil public listener")
	}
	if link.Socket == "" || link.Token == "" {
		return nil, errors.New("web supervisor: incomplete muxer link")
	}
	if cmd == nil {
		return nil, errors.New("web supervisor: nil command")
	}
	return &webSupervisor{
		listener: listener,
		link:     link,
		config: webChildConfig{
			Link: link, ListenAddr: cmd.listenAddr,
			TrustedHosts: append([]string(nil), cmd.trustedHosts...),
			DataDir:      cmd.Config.DataDir,
			Remote:       cmd.Config.Remote,
			InviteFile:   cmd.Config.InviteFile,
			InviteStdin:  cmd.Config.InviteStdin,
			RVOrigin:     cmd.Config.Origin,
			CSRFToken:    mustToken(),
		},
		stdin: cmd.Stdin, stdout: cmd.Stdout, stderr: cmd.Stderr,
		readyTimeout: 5 * time.Minute,
		drainTimeout: 30 * time.Second,
	}, nil
}

func (s *webSupervisor) Start(ctx context.Context, executable string) error {
	return s.Rotate(ctx, executable)
}

// Rotate prepares a child without letting it accept, drains the old child,
// then activates the candidate. A failure before activation leaves current
// untouched and serving.
func (s *webSupervisor) Rotate(ctx context.Context, executable string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rotateLocked(ctx, executable)
}

func (s *webSupervisor) rotateLocked(ctx context.Context, executable string) error {
	prepared, err := s.prepareLocked(ctx, executable)
	if err != nil {
		return err
	}
	return s.commitLocked(prepared)
}

// Prepare starts and validates a standby generation without disturbing the
// active one. Update uses this before writing its success response; Commit is
// separate because the old child cannot drain while carrying that response.
func (s *webSupervisor) Prepare(ctx context.Context, executable string) (*preparedWeb, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prepared != nil {
		return nil, errors.New("web supervisor: replacement already prepared")
	}
	prepared, err := s.prepareLocked(ctx, executable)
	if err != nil {
		return nil, err
	}
	s.prepared = prepared
	return prepared, nil
}

func (s *webSupervisor) prepareLocked(ctx context.Context, executable string) (*preparedWeb, error) {
	if s.closed {
		return nil, errors.New("web supervisor: closed")
	}
	if executable == "" {
		return nil, errors.New("web supervisor: empty executable")
	}
	if s.generation > 0 || s.current != nil {
		s.reconcileRemoteLifecycle(false)
	}
	next := s.generation + 1
	cfg := s.config
	cfg.Generation = next
	cfg.Replacement = s.current != nil
	candidate, err := s.launch(ctx, executable, cfg)
	if err != nil {
		return nil, err
	}
	abort := func() {
		_ = candidate.activate.Close()
		stopWebProcess(candidate, s.drainTimeout)
	}
	ready, err := waitWebEvent(ctx, candidate, "ready", s.readyTimeout)
	if err != nil {
		abort()
		return nil, err
	}
	if ready.Generation != next || ready.Version == "" {
		abort()
		return nil, fmt.Errorf("web supervisor: invalid readiness event %#v", ready)
	}
	if !cfg.Replacement {
		s.reconcileRemoteLifecycle(true)
	}
	candidate.version = ready.Version
	return &preparedWeb{supervisor: s, candidate: candidate, expected: s.current, generation: next}, nil
}

func (s *webSupervisor) commitLocked(prepared *preparedWeb) error {
	if prepared == nil || prepared.supervisor != s {
		return errors.New("web supervisor: invalid prepared child")
	}
	if s.closed || s.current != prepared.expected || s.generation+1 != prepared.generation {
		_ = prepared.candidate.activate.Close()
		stopWebProcess(prepared.candidate, s.drainTimeout)
		return errors.New("web supervisor: active generation changed before commit")
	}
	abort := func() {
		_ = prepared.candidate.activate.Close()
		stopWebProcess(prepared.candidate, s.drainTimeout)
	}
	old := s.current
	if old != nil {
		if s.beforeDrain != nil {
			s.beforeDrain()
		}
		stopWebProcess(old, s.drainTimeout)
	}
	if _, err := prepared.candidate.activate.Write([]byte{1}); err != nil {
		abort()
		return fmt.Errorf("web supervisor: activate: %w", err)
	}
	_ = prepared.candidate.activate.Close()
	active, err := waitWebEvent(context.Background(), prepared.candidate, "active", s.readyTimeout)
	if err != nil {
		abort()
		return err
	}
	if active.Generation != prepared.generation || active.Version != prepared.candidate.version {
		abort()
		return fmt.Errorf("web supervisor: invalid activation event %#v", active)
	}
	s.reconcileRemoteLifecycle(true)
	s.current = prepared.candidate
	s.generation = prepared.generation
	return nil
}

// reconcileRemoteLifecycle separates three facts that Start's return value
// cannot: whether a credential is still reusable, whether an identity now
// exists, and whether the operator intentionally removed that identity. It is
// called only while the supervisor lock is held.
func (s *webSupervisor) reconcileRemoteLifecycle(startupCompleted bool) {
	if !s.config.Remote {
		return
	}
	// stdin is a stream, so a completed startup attempt can never replay it.
	if startupCompleted {
		s.config.InviteStdin = false
	}
	if s.config.InviteFile != "" {
		if _, err := os.Stat(s.config.InviteFile); errors.Is(err, os.ErrNotExist) {
			s.config.InviteFile = ""
		}
	}
	probe := remote.NewClient(remote.Config{DataDir: s.config.DataDir, Origin: s.config.RVOrigin})
	state, err := probe.State()
	if err != nil {
		return
	}
	if state == remote.StateAbsent && (startupCompleted || s.generation > 0) &&
		s.config.InviteFile == "" && !s.config.InviteStdin {
		// An active installation that becomes absent was explicitly unlinked.
		// Replacement stays local until a new top-level invocation supplies a
		// fresh invite; it must not reopen a terminal from a background child.
		s.config.Remote = false
	}
}

// Commit drains the expected old child and activates this prepared one.
func (p *preparedWeb) Commit() error {
	if p == nil || p.supervisor == nil {
		return errors.New("web supervisor: nil prepared child")
	}
	s := p.supervisor
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prepared != p {
		return errors.New("web supervisor: prepared child is no longer pending")
	}
	s.prepared = nil
	return s.commitLocked(p)
}

// Abort discards a standby generation without touching the active child.
func (p *preparedWeb) Abort() {
	if p == nil || p.supervisor == nil {
		return
	}
	s := p.supervisor
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prepared != p {
		return
	}
	s.prepared = nil
	_ = p.candidate.activate.Close()
	stopWebProcess(p.candidate, s.drainTimeout)
}

func (s *webSupervisor) launch(ctx context.Context, executable string, cfg webChildConfig) (*webProcess, error) {
	publicFile, err := listenerFileForExec(s.listener)
	if err != nil {
		return nil, err
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		publicFile.Close()
		return nil, fmt.Errorf("web supervisor: readiness pipe: %w", err)
	}
	activateR, activateW, err := os.Pipe()
	if err != nil {
		publicFile.Close()
		readyR.Close()
		readyW.Close()
		return nil, fmt.Errorf("web supervisor: activation pipe: %w", err)
	}
	ownerR, ownerW, err := os.Pipe()
	if err != nil {
		publicFile.Close()
		readyR.Close()
		readyW.Close()
		activateR.Close()
		activateW.Close()
		return nil, fmt.Errorf("web supervisor: parent-lifetime pipe: %w", err)
	}

	args := s.childArgs
	if len(args) == 0 {
		args = []string{webChildCmd}
	}
	// The caller context bounds preparation; it must not own the lifetime of a
	// successfully activated child (an update request ends immediately after
	// activation).
	cmd := exec.Command(executable, args...)
	cmd.Env = append(os.Environ(), encodeWebChildEnv(cfg)...)
	cmd.Env = append(cmd.Env, s.extraEnv...)
	cmd.ExtraFiles = []*os.File{publicFile, readyW, activateR, ownerR}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.stdin, s.stdout, s.stderr
	if err := cmd.Start(); err != nil {
		publicFile.Close()
		readyR.Close()
		readyW.Close()
		activateR.Close()
		activateW.Close()
		ownerR.Close()
		ownerW.Close()
		return nil, fmt.Errorf("web supervisor: start: %w", err)
	}
	publicFile.Close()
	readyW.Close()
	activateR.Close()
	ownerR.Close()

	events := make(chan webChildEvent, 2)
	eventErr := make(chan error, 1)
	go decodeWebEvents(readyR, events, eventErr)
	done := make(chan struct{})
	p := &webProcess{cmd: cmd, activate: activateW, owner: ownerW, events: events, eventErr: eventErr, done: done}
	go func() { p.setWaitErr(cmd.Wait()) }()
	return p, nil
}

// listenerFileForExec duplicates the listener without calling os.File.Fd.
// exec.ExtraFiles calls Fd itself; doing that on the network file returned by
// Listener.File can temporarily clear O_NONBLOCK on the shared socket and
// strand another child's Accept in the kernel. A raw dup wrapped in a fresh
// os.File has no poller state for Fd to dismantle.
func listenerFileForExec(listener net.Listener) (*os.File, error) {
	fl, ok := listener.(interface{ File() (*os.File, error) })
	if !ok {
		return nil, fmt.Errorf("web supervisor: listener %T cannot be inherited", listener)
	}
	networkFile, err := fl.File()
	if err != nil {
		return nil, fmt.Errorf("web supervisor: duplicate listener: %w", err)
	}
	raw, err := networkFile.SyscallConn()
	if err != nil {
		_ = networkFile.Close()
		return nil, fmt.Errorf("web supervisor: access duplicated listener: %w", err)
	}
	inheritedFD := -1
	var dupErr error
	controlErr := raw.Control(func(fd uintptr) {
		inheritedFD, dupErr = syscall.Dup(int(fd))
	})
	_ = networkFile.Close()
	if controlErr != nil {
		return nil, fmt.Errorf("web supervisor: access duplicated listener: %w", controlErr)
	}
	if dupErr != nil {
		return nil, fmt.Errorf("web supervisor: duplicate listener descriptor: %w", dupErr)
	}
	syscall.CloseOnExec(inheritedFD)
	return os.NewFile(uintptr(inheritedFD), "scimux-public-listener"), nil
}

func decodeWebEvents(r *os.File, events chan<- webChildEvent, errs chan<- error) {
	defer r.Close()
	defer close(events)
	dec := json.NewDecoder(io.LimitReader(r, 64<<10))
	for {
		var event webChildEvent
		if err := dec.Decode(&event); err != nil {
			if !errors.Is(err, io.EOF) {
				errs <- err
			}
			return
		}
		events <- event
	}
}

func waitWebEvent(ctx context.Context, p *webProcess, phase string, timeout time.Duration) (webChildEvent, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-p.events:
			if !ok {
				return webChildEvent{}, fmt.Errorf("web supervisor: child closed readiness pipe before %s", phase)
			}
			if event.Phase != phase {
				return webChildEvent{}, fmt.Errorf("web supervisor: child sent phase %q, want %q", event.Phase, phase)
			}
			return event, nil
		case err := <-p.eventErr:
			return webChildEvent{}, fmt.Errorf("web supervisor: readiness: %w", err)
		case <-p.done:
			if err := p.err(); err != nil {
				return webChildEvent{}, fmt.Errorf("web supervisor: child exited before %s: %w", phase, err)
			}
			return webChildEvent{}, fmt.Errorf("web supervisor: child exited before %s", phase)
		case <-ctx.Done():
			return webChildEvent{}, ctx.Err()
		case <-timer.C:
			return webChildEvent{}, fmt.Errorf("web supervisor: timed out waiting for %s", phase)
		}
	}
}

func stopWebProcess(p *webProcess, timeout time.Duration) {
	if p == nil {
		return
	}
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

// Recover keeps presentation available after an unexpected web-child exit.
// Planned rotation is distinguished by pointer identity: Rotate holds s.mu
// until current names the candidate, so the retiring child's observer cannot
// accidentally rotate a second time.
func (s *webSupervisor) Recover(ctx context.Context, executable string, report func(error)) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		observed := s.current
		if observed == nil && s.prepared == nil {
			err := s.rotateLocked(ctx, executable)
			s.mu.Unlock()
			if err == nil {
				continue
			}
			if report != nil {
				report(err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		s.mu.Unlock()
		if observed == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-observed.done:
		}

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		if s.current != observed {
			s.mu.Unlock()
			continue
		}
		if s.prepared != nil {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}
		// The dead process has released the remote lock, so recovery is an
		// initial-style start even though its generation increases.
		s.current = nil
		err := s.rotateLocked(ctx, executable)
		s.mu.Unlock()
		if err == nil {
			continue
		}
		if report != nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *webSupervisor) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.prepared != nil {
		_ = s.prepared.candidate.activate.Close()
		stopWebProcess(s.prepared.candidate, s.drainTimeout)
		s.prepared = nil
	}
	stopWebProcess(s.current, s.drainTimeout)
	s.current = nil
	return nil
}

func encodeWebChildEnv(cfg webChildConfig) []string {
	trusted, _ := json.Marshal(cfg.TrustedHosts)
	return []string{
		envCoreSocket + "=" + cfg.Link.Socket,
		envCoreToken + "=" + cfg.Link.Token,
		envGeneration + "=" + strconv.FormatUint(cfg.Generation, 10),
		envReplacement + "=" + strconv.FormatBool(cfg.Replacement),
		envListenAddr + "=" + cfg.ListenAddr,
		envTrusted + "=" + string(trusted),
		envDataDir + "=" + cfg.DataDir,
		envRemote + "=" + strconv.FormatBool(cfg.Remote),
		envInviteFile + "=" + cfg.InviteFile,
		envInviteStdin + "=" + strconv.FormatBool(cfg.InviteStdin),
		envRVOrigin + "=" + cfg.RVOrigin,
		envCSRFToken + "=" + cfg.CSRFToken,
	}
}

func loadWebChildConfig(getenv func(string) string) (webChildConfig, error) {
	if getenv == nil {
		return webChildConfig{}, errors.New("web child: nil environment")
	}
	gen, err := strconv.ParseUint(getenv(envGeneration), 10, 64)
	if err != nil || gen == 0 {
		return webChildConfig{}, errors.New("web child: invalid generation")
	}
	replacement, err := strconv.ParseBool(getenv(envReplacement))
	if err != nil {
		return webChildConfig{}, errors.New("web child: invalid replacement flag")
	}
	remoteEnabled, err := strconv.ParseBool(getenv(envRemote))
	if err != nil {
		return webChildConfig{}, errors.New("web child: invalid remote flag")
	}
	inviteStdin, err := strconv.ParseBool(getenv(envInviteStdin))
	if err != nil {
		return webChildConfig{}, errors.New("web child: invalid invite-stdin flag")
	}
	var trusted []string
	if err := json.Unmarshal([]byte(getenv(envTrusted)), &trusted); err != nil {
		return webChildConfig{}, errors.New("web child: invalid trusted hosts")
	}
	cfg := webChildConfig{
		Link:       backend.Link{Socket: getenv(envCoreSocket), Token: getenv(envCoreToken)},
		Generation: gen, Replacement: replacement,
		ListenAddr: getenv(envListenAddr), TrustedHosts: trusted,
		DataDir: getenv(envDataDir), Remote: remoteEnabled,
		InviteFile: getenv(envInviteFile), InviteStdin: inviteStdin,
		RVOrigin: getenv(envRVOrigin), CSRFToken: getenv(envCSRFToken),
	}
	if cfg.Link.Socket == "" || cfg.Link.Token == "" || cfg.ListenAddr == "" || cfg.DataDir == "" || !validCSRFToken(cfg.CSRFToken) {
		return webChildConfig{}, errors.New("web child: incomplete environment")
	}
	return cfg, nil
}

func runWebChildMain() int {
	if err := runWebChild(context.Background(), os.Getenv, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "scimux web child:", err)
		return 1
	}
	return 0
}

func runWebChild(ctx context.Context, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) error {
	cfg, err := loadWebChildConfig(getenv)
	if err != nil {
		return err
	}
	// Each web child is a fresh process, so assigning before constructing any
	// handler is race-free. The value is minted once by the muxer supervisor
	// and inherited by every generation, preserving writes from open tabs.
	csrfToken = cfg.CSRFToken
	publicFile := os.NewFile(webPublicFD, "scimux-public-listener")
	ready := os.NewFile(webReadyFD, "scimux-web-ready")
	activate := os.NewFile(webActivateFD, "scimux-web-activate")
	owner := os.NewFile(webOwnerFD, "scimux-web-owner")
	return runWebChildFiles(ctx, cfg, publicFile, ready, activate, owner, stdin, stdout, stderr, webChildDeps{
		// Parent loss is the one shutdown state that cannot rely on cooperative
		// cancellation: enrollment may be blocked in a terminal or stdin read.
		// The muxer is already gone, so the kernel is the only cleanup owner left.
		ownerLost: func() { os.Exit(0) },
	})
}

// runWebChildFiles is the descriptor-independent half of hidden-role startup.
// Keeping inherited FD lookup above this seam lets tests exercise listener
// conversion and the parent-death monitor without altering the test runner's
// own descriptors 3-6.
func runWebChildFiles(ctx context.Context, cfg webChildConfig, publicFile, ready, activate, owner *os.File, stdin io.Reader, stdout, stderr io.Writer, deps webChildDeps) error {
	if publicFile == nil || ready == nil || activate == nil || owner == nil {
		return errors.New("missing inherited descriptors")
	}
	defer ready.Close()
	defer activate.Close()
	defer owner.Close()
	ownerDone := make(chan struct{})
	go func() {
		_, err := io.Copy(io.Discard, owner)
		if err == nil && deps.ownerLost != nil {
			deps.ownerLost()
		}
		close(ownerDone)
	}()
	ln, err := net.FileListener(publicFile)
	publicFile.Close()
	if err != nil {
		return fmt.Errorf("inherit listener: %w", err)
	}
	defer ln.Close()
	deps.ownerDone = ownerDone
	return serveWebChild(ctx, cfg, ln, ready, activate, stdin, stdout, stderr, deps)
}

func serveWebChild(ctx context.Context, cfg webChildConfig, ln net.Listener, ready io.Writer, activate io.Reader, stdin io.Reader, stdout, stderr io.Writer, deps webChildDeps) error {
	core, err := backend.NewClient(cfg.Link)
	if err != nil {
		return err
	}
	defer core.Close()
	helloCtx, cancelHello := context.WithTimeout(ctx, 10*time.Second)
	_, err = core.Hello(helloCtx)
	cancelHello()
	if err != nil {
		return err
	}
	policy, err := newRequestPolicy(cfg.ListenAddr, cfg.TrustedHosts)
	if err != nil {
		return err
	}

	childCtx, childCancel := context.WithCancel(ctx)
	defer childCancel()
	if deps.ownerDone != nil {
		go func() {
			select {
			case <-deps.ownerDone:
				childCancel()
			case <-childCtx.Done():
			}
		}()
	}
	var remoteClient webRemoteClient
	var web *webBackend
	if cfg.Remote {
		rc := remote.Config{
			DataDir: cfg.DataDir, Remote: true, InviteFile: cfg.InviteFile,
			InviteStdin: cfg.InviteStdin, Origin: cfg.RVOrigin,
			Stdin: stdin, Stdout: stdout, Stderr: stderr,
			NewTerminal: remote.OpenOwnerTerminal,
		}
		rc.TunnelHandlerFor = func(p remote.TunnelPeer) http.Handler {
			return web.tunnelFor(tunnelPeer{DeviceID: p.DeviceID, RID: p.RID})
		}
		remoteClient = deps.remote(rc)
	}
	web, err = newWebBackend(webBackendConfig{
		Web: webFS, Core: core.Proxy(), RequestPolicy: policy,
		Pairing: remoteClient, RemoteClose: remoteClient,
	})
	if err != nil {
		return err
	}
	defer web.Close()

	startRemote := func() error {
		if remoteClient == nil {
			return nil
		}
		if err := remoteClient.Start(childCtx); err != nil {
			if childCtx.Err() != nil {
				return childCtx.Err()
			}
			switch remoteClass(err) {
			case remote.ClassRevoked, remote.ClassDisabled, remote.ClassUnavailable:
				fmt.Fprintln(stderr, "scimux: remote access is off:", err)
				return nil
			default:
				return err
			}
		}
		return nil
	}
	if !cfg.Replacement {
		if err := startRemote(); err != nil {
			if childCtx.Err() != nil {
				return nil
			}
			return err
		}
	}

	enc := json.NewEncoder(ready)
	if err := enc.Encode(webChildEvent{Phase: "ready", Generation: cfg.Generation, Version: version}); err != nil {
		return err
	}
	activated := make(chan error, 1)
	go func() {
		var one [1]byte
		_, err := io.ReadFull(activate, one[:])
		activated <- err
	}()
	sigCtx, stopSignals := signal.NotifyContext(childCtx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	select {
	case err := <-activated:
		if err != nil {
			return fmt.Errorf("activation: %w", err)
		}
	case <-sigCtx.Done():
		return nil
	}

	if cfg.Replacement {
		if err := startRemote(); err != nil {
			if childCtx.Err() != nil {
				return nil
			}
			return err
		}
	}
	publish := func() {
		status := backend.Status{Generation: cfg.Generation, Version: version}
		if remoteClient != nil {
			remoteStatus := remoteClient.HostedStatus()
			status.Remote = &remoteStatus
		}
		statusCtx, cancel := context.WithTimeout(childCtx, 2*time.Second)
		defer cancel()
		_ = core.PublishStatus(statusCtx, status)
	}
	publish()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				publish()
			case <-childCtx.Done():
				return
			}
		}
	}()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Scimux-Web-Generation", strconv.FormatUint(cfg.Generation, 10))
		web.local.ServeHTTP(w, r)
	})
	srv := &http.Server{
		Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	if err := enc.Encode(webChildEvent{Phase: "active", Generation: cfg.Generation, Version: version}); err != nil {
		return err
	}
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-sigCtx.Done():
		childCancel()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		if err != nil {
			return err
		}
		return nil
	}
}
