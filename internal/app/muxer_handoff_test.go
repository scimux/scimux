package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/backend"
	"github.com/scimux/scimux/internal/remote"
	"github.com/scimux/scimux/internal/sessionworker"
	"github.com/scimux/scimux/internal/tmuxsession"
)

func TestMuxerExecEnvironmentRoundTripAndValidation(t *testing.T) {
	const inheritedCSRF = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := (*muxerExecFiles)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMuxerExecFiles(nil); err == nil {
		t.Fatal("nil environment reader accepted")
	}
	if files, err := prepareMuxerExecFiles(nil); err == nil || files != nil {
		t.Fatalf("nil handoff preparation = %#v, %v", files, err)
	}
	for _, files := range []*muxerExecFiles{nil, {}, {public: os.Stdin}, {ownership: os.Stdin}} {
		if err := replaceMuxerProcess("", files); err == nil {
			t.Fatalf("incomplete replacement accepted: %#v", files)
		}
	}
	if files, err := loadMuxerExecFiles(func(string) string { return "" }); err != nil || files != nil {
		t.Fatalf("absent handoff = %#v, %v", files, err)
	}
	for _, values := range []map[string]string{
		{envMuxerPublicFD: "3"},
		{envMuxerLockFD: "4"},
		{envMuxerCSRFToken: inheritedCSRF},
		{envMuxerPublicFD: "bad", envMuxerLockFD: "4"},
		{envMuxerPublicFD: "3", envMuxerLockFD: "2"},
		{envMuxerPublicFD: "3", envMuxerLockFD: "4", envMuxerCSRFToken: "invalid"},
	} {
		if files, err := loadMuxerExecFiles(func(key string) string { return values[key] }); err == nil || files != nil {
			t.Fatalf("invalid handoff %v = %#v, %v", values, files, err)
		}
	}
	publicR, publicW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	lockR, lockW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer publicW.Close()
	defer lockW.Close()
	values := map[string]string{
		envMuxerPublicFD:  strconv.Itoa(handOverFD(t, publicR)),
		envMuxerLockFD:    strconv.Itoa(handOverFD(t, lockR)),
		envMuxerCSRFToken: inheritedCSRF,
	}
	files, err := loadMuxerExecFiles(func(key string) string { return values[key] })
	if err != nil || files == nil {
		t.Fatalf("valid inherited descriptors = %#v, %v", files, err)
	}
	if files.csrfToken != inheritedCSRF {
		t.Fatalf("inherited CSRF token = %q, want %q", files.csrfToken, inheritedCSRF)
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	legacyPublicR, legacyPublicW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	legacyLockR, legacyLockW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer legacyPublicW.Close()
	defer legacyLockW.Close()
	legacyValues := map[string]string{
		envMuxerPublicFD: strconv.Itoa(handOverFD(t, legacyPublicR)),
		envMuxerLockFD:   strconv.Itoa(handOverFD(t, legacyLockR)),
	}
	legacy, err := loadMuxerExecFiles(func(key string) string { return legacyValues[key] })
	if err != nil || legacy == nil || legacy.csrfToken != "" {
		t.Fatalf("legacy inherited descriptors = %#v, %v", legacy, err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv(envMuxerPublicFD, "stale-public")
	t.Setenv(envMuxerLockFD, "stale-lock")
	t.Setenv(envMuxerCSRFToken, strings.Repeat("f", 64))
	env := strings.Join(muxerExecEnvironment(7, 8, inheritedCSRF), "\n")
	if strings.Count(env, envMuxerPublicFD+"=") != 1 || strings.Count(env, envMuxerLockFD+"=") != 1 ||
		strings.Count(env, envMuxerCSRFToken+"=") != 1 || !strings.Contains(env, envMuxerCSRFToken+"="+inheritedCSRF) {
		t.Fatalf("replacement environment retained stale descriptors: %s", env)
	}
}

func TestMuxerExecPreparationAndReplacementFailClosed(t *testing.T) {
	const validCSRF = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, err := backend.Claim(data)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	invalidTokenCmd := &Command{listener: ln, ownership: owner, csrfToken: "invalid"}
	if files, err := prepareMuxerExecFiles(invalidTokenCmd); err == nil || files != nil {
		t.Fatalf("handoff accepted invalid CSRF token: %#v, %v", files, err)
	}
	cmd := &Command{listener: fileErrorListener{Listener: ln}, ownership: owner, csrfToken: validCSRF}
	if files, err := prepareMuxerExecFiles(cmd); err == nil || files != nil {
		t.Fatalf("handoff accepted listener duplication failure: %#v, %v", files, err)
	}
	cmd.listener = ln
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if files, err := prepareMuxerExecFiles(cmd); err == nil || files != nil {
		t.Fatalf("handoff accepted closed ownership: %#v, %v", files, err)
	}
	_ = ln.Close()

	publicR, publicW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	lockR, lockW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer publicW.Close()
	defer lockW.Close()
	if err := replaceMuxerProcess("/new/scimux", &muxerExecFiles{public: publicR, ownership: lockR, csrfToken: "invalid"}); err == nil {
		t.Fatal("replacement accepted invalid CSRF token")
	}
	if err := publicR.Close(); err != nil {
		t.Fatal(err)
	}
	if err := replaceMuxerProcess("/new/scimux", &muxerExecFiles{public: publicR, ownership: lockR, csrfToken: validCSRF}); err == nil {
		t.Fatal("replacement accepted a closed public descriptor")
	}

	publicR, publicW, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer publicR.Close()
	defer publicW.Close()
	if err := lockR.Close(); err != nil {
		t.Fatal(err)
	}
	if err := replaceMuxerProcess("/new/scimux", &muxerExecFiles{public: publicR, ownership: lockR, csrfToken: validCSRF}); err == nil {
		t.Fatal("replacement accepted a closed ownership descriptor")
	}
	if !descriptorCloseOnExec(t, int(publicR.Fd())) {
		t.Fatal("public descriptor was not re-protected after ownership descriptor failure")
	}
}

func TestReplaceMuxerProcessMakesOnlyHandoffDescriptorsInheritable(t *testing.T) {
	const validCSRF = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	publicR, publicW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	lockR, lockW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer publicW.Close()
	defer lockW.Close()
	files := &muxerExecFiles{public: publicR, ownership: lockR, csrfToken: validCSRF}
	syscall.CloseOnExec(int(publicR.Fd()))
	syscall.CloseOnExec(int(lockR.Fd()))
	wantErr := errors.New("exec refused")
	oldExec := muxerExec
	muxerExec = func(path string, argv, env []string) error {
		if path != "/new/scimux" || len(argv) == 0 {
			t.Fatalf("exec args = %q %v", path, argv)
		}
		joined := strings.Join(env, "\n")
		for key, fd := range map[string]int{envMuxerPublicFD: int(publicR.Fd()), envMuxerLockFD: int(lockR.Fd())} {
			if !strings.Contains(joined, key+"="+strconv.Itoa(fd)) {
				t.Errorf("exec env lacks %s", key)
			}
			if descriptorCloseOnExec(t, fd) {
				t.Errorf("%s remained close-on-exec inside exec critical section", key)
			}
		}
		if !strings.Contains(joined, envMuxerCSRFToken+"="+validCSRF) {
			t.Errorf("exec env lacks inherited CSRF token")
		}
		return wantErr
	}
	t.Cleanup(func() { muxerExec = oldExec })
	if err := replaceMuxerProcess("/new/scimux", files); !errors.Is(err, wantErr) {
		t.Fatalf("replace = %v", err)
	}
	for _, fd := range []int{int(publicR.Fd()), int(lockR.Fd())} {
		if !descriptorCloseOnExec(t, fd) {
			t.Fatalf("descriptor %d was not re-protected after failed exec", fd)
		}
	}
	_ = files.Close()
}

// handOverFD duplicates f's descriptor and closes f, so the number it returns
// has exactly one owner: whoever the test hands it to. worker_process_test.go
// already hands descriptors over this way.
func handOverFD(t *testing.T, f *os.File) int {
	t.Helper()
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return fd
}

// A descriptor number handed to code that will own and close it must not be a
// number the test's own os.File still owns. Two owners means two closes, and
// the second one lands at an unpredictable later moment -- after the kernel
// has handed that number to an unrelated open file. The damage then surfaces
// as "bad file descriptor" somewhere else entirely: another test, a later
// package, whatever happened to open a file next.
func TestHandingOverADescriptorLeavesOneOwner(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	owned := int(r.Fd())
	fd := handOverFD(t, r)
	if fd == owned {
		t.Fatalf("handed over descriptor %d, which the pipe still owns", fd)
	}
	if err := r.Close(); err == nil {
		t.Fatal("the pipe was left open, so its finalizer still closes a descriptor it no longer owns")
	}
	if err := syscall.Close(fd); err != nil {
		t.Fatalf("the descriptor handed over was not usable: %v", err)
	}
}

func descriptorCloseOnExec(t *testing.T, fd int) bool {
	t.Helper()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	return flags&syscall.FD_CLOEXEC != 0
}

func TestCommandConsumesInheritedMuxerOwnershipAndListener(t *testing.T) {
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, err := backend.Claim(data)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	public, err := listenerFileForExec(ln)
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := owner.FileForExec()
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()

	cmd := &Command{
		Args: []string{"scimux", "-data", data, "-addr", "127.0.0.1:0"},
		Home: t.TempDir(), Stdout: io.Discard, Stderr: io.Discard,
		Config: remote.Config{}, muxerOnly: true,
		handoff: &muxerExecFiles{public: public, ownership: ownership},
	}
	if err := cmd.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if cmd.Listener() == nil || cmd.ownership == nil || cmd.handoff != nil {
		t.Fatalf("inherited command state: listener=%v owner=%v handoff=%v", cmd.Listener(), cmd.ownership, cmd.handoff)
	}
	cmd.closeListener()
	cmd.closeOwnership()
}

type muxerExecReady struct {
	DataDir    string         `json:"data_dir"`
	Addr       string         `json:"addr"`
	MuxerPID   int            `json:"muxer_pid"`
	CSRFToken  string         `json:"csrf_token"`
	WorkerPIDs map[string]int `json:"worker_pids"`
}

type muxerExecWorkerState struct {
	PID          int    `json:"pid"`
	Build        string `json:"build"`
	SessionID    string `json:"session_id"`
	Live         string `json:"live"`
	Peek         string `json:"peek"`
	Watermark    int64  `json:"watermark"`
	HasSession   bool   `json:"has_session"`
	TurnInFlight bool   `json:"turn_in_flight"`
	Delivery     string `json:"delivery"`
	Pending      bool   `json:"pending"`
	RequestID    string `json:"request_id"`
}

type muxerExecState struct {
	MuxerPID int                             `json:"muxer_pid"`
	Build    string                          `json:"build"`
	Workers  map[string]muxerExecWorkerState `json:"workers"`
}

type muxerExecHarness struct {
	ID        string
	Agent     string
	Transport string
}

var muxerExecHarnesses = []muxerExecHarness{
	{ID: "survivor-claude", Agent: "claude", Transport: "tmux"},
	{ID: "survivor-codex", Agent: "codex", Transport: "codex"},
	{ID: "survivor-grok", Agent: "grok", Transport: "acp"},
	{ID: "survivor-pi", Agent: "pi", Transport: "acp"},
	{ID: "survivor-opencode", Agent: "opencode", Transport: "acp"},
}

const muxerExecReadyFDEnv = "SCIMUX_MUXER_EXEC_READY_FD"
const muxerExecCSRFToken = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func productionMuxerExecManager(t *testing.T, data, build string) (*workerManager, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home, bin := filepath.Join(data, "home"), filepath.Join(data, "bin")
	for _, dir := range []string{home, bin, filepath.Join(data, "sessions")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for binary, helper := range map[string]string{
		"pi-acp":   "TestSyntheticACPAgentHelperProcess",
		"opencode": "TestSyntheticACPAgentHelperProcess",
		"grok":     "TestSyntheticACPAgentHelperProcess",
		"codex":    "TestSyntheticCodexAgentHelperProcess",
	} {
		launcher := "#!/bin/sh\nexec \"" + strings.ReplaceAll(exe, "\"", "\\\"") + "\" -test.run=^" + helper + "$\n"
		if err := os.WriteFile(filepath.Join(bin, binary), []byte(launcher), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(syntheticClaudeLauncher), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SCIMUX_SYNTHETIC_ACP", "1")
	t.Setenv("SCIMUX_SYNTHETIC_CODEX", "1")
	t.Setenv("SCIMUX_SYNTHETIC_ACP_STATUS", filepath.Join(data, "agent-status"))
	t.Setenv("SCIMUX_FAKE_CLAUDE_HOME", home)
	socket := "scimux-muxer-exec-" + filepath.Base(data)
	workers := newWorkerManager(exe, data, build)
	workers.home, workers.socket = home, socket
	workers.startOptions = sessionWorkerStartOptions{
		args: []string{"-test.run=^TestProductionSessionWorkerHelperProcess$"},
		env:  []string{"SCIMUX_PRODUCTION_SESSION_WORKER_TEST=1"},
	}
	return workers, socket
}

func waitMuxerExecWorker(t *testing.T, workers *workerManager, nodeID string, accept func(sessionworker.State, string) bool) (sessionworker.State, string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var state sessionworker.State
	var peek string
	for time.Now().Before(deadline) {
		state, peek = workers.State(nodeID), workers.Peek(nodeID)
		if accept(state, peek) {
			return state, peek
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("worker %s did not reach expected state: state=%#v peek=%q", nodeID, state, peek)
	return state, peek
}

func answerMuxerExecPermission(t *testing.T, workers *workerManager, nodeID string) {
	t.Helper()
	pending, ok := workers.Pending(nodeID)
	if !ok {
		t.Fatalf("%s has no pending approval", nodeID)
	}
	token, _, err := workers.PrepareResolve(nodeID, pending.RequestID, "1")
	if err == nil {
		err = workers.Deliver(nodeID, token)
	}
	if err != nil {
		t.Fatalf("answer %s: %v", nodeID, err)
	}
}

func TestMuxerExecParentHelperProcess(t *testing.T) {
	if os.Getenv("SCIMUX_MUXER_EXEC_PARENT_TEST") != "1" {
		return
	}
	data, err := os.MkdirTemp("", "scimux-muxer-exec-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(data)
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, err := backend.Claim(data)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cmd := &Command{listener: ln, ownership: owner, csrfToken: muxerExecCSRFToken}
	workers, socket := productionMuxerExecManager(t, data, "muxer-v1")
	defer cleanupMuxerExecTmux(t, socket)
	workerPIDs := make(map[string]int, len(muxerExecHarnesses))
	for _, harness := range muxerExecHarnesses {
		node := &Node{
			ID: harness.ID, Title: harness.Agent + " survivor", Prompt: "before update", Agent: harness.Agent,
			Dir: workers.home, Model: "cheap", Effort: "low", Transport: harness.Transport,
			CreatedAt: "2026-09-12T05:00:00Z",
		}
		if _, err := workers.LaunchNode(node, node.Model); err != nil {
			t.Fatalf("launch %s: %v", harness.Agent, err)
		}
		switch harness.Transport {
		case "acp":
			if err := workers.Send(harness.ID, "permission-before-update"); err != nil {
				t.Fatalf("send %s: %v", harness.Agent, err)
			}
			waitMuxerExecWorker(t, workers, harness.ID, func(state sessionworker.State, _ string) bool { return state.Permission != nil })
			answerMuxerExecPermission(t, workers, harness.ID)
			waitMuxerExecWorker(t, workers, harness.ID, func(state sessionworker.State, peek string) bool {
				return state.Live == "quiet" && strings.Contains(peek, "continued-after-approval")
			})
			if err := workers.Send(harness.ID, "permission-across-update"); err != nil {
				t.Fatalf("second send %s: %v", harness.Agent, err)
			}
			waitMuxerExecWorker(t, workers, harness.ID, func(state sessionworker.State, _ string) bool { return state.Permission != nil })
		case "codex":
			if err := workers.Send(harness.ID, "before-update"); err != nil {
				t.Fatalf("send codex: %v", err)
			}
			waitMuxerExecWorker(t, workers, harness.ID, func(state sessionworker.State, peek string) bool {
				return state.Live == "quiet" && strings.Count(peek, "pong") == 1
			})
		case "tmux":
			waitMuxerExecWorker(t, workers, harness.ID, func(state sessionworker.State, _ string) bool {
				return state.HasSession && state.Watermark > 0 && state.Delivery == "" && !state.TurnInFlight
			})
		}
		locator, err := sessionworker.Discover(data, harness.ID)
		if err != nil {
			t.Fatal(err)
		}
		workerPIDs[harness.ID] = locator.PID
	}
	workers.Detach()
	files, err := prepareMuxerExecFiles(cmd)
	if err != nil {
		t.Fatal(err)
	}
	readyFD, err := strconv.Atoi(os.Getenv(muxerExecReadyFDEnv))
	if err != nil || readyFD < 3 {
		t.Fatal("missing muxer-exec readiness descriptor")
	}
	ready := os.NewFile(uintptr(readyFD), "muxer-exec-ready")
	if err := json.NewEncoder(ready).Encode(muxerExecReady{
		DataDir: data, Addr: ln.Addr().String(), MuxerPID: os.Getpid(),
		CSRFToken: muxerExecCSRFToken, WorkerPIDs: workerPIDs,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ready.Close(); err != nil {
		t.Fatal(err)
	}
	var trigger [1]byte
	if _, err := io.ReadFull(os.Stdin, trigger[:]); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{os.Args[0], "-test.run=^TestMuxerExecChildHelperProcess$"}
	if err := os.Setenv("SCIMUX_MUXER_EXEC_CHILD_TEST", "1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("SCIMUX_MUXER_EXEC_DATA", data); err != nil {
		t.Fatal(err)
	}
	if err := replaceMuxerProcess(os.Args[0], files); err != nil {
		t.Fatal(err)
	}
}

func cleanupMuxerExecTmux(t *testing.T, socket string) {
	t.Helper()
	server := tmuxsession.NewServer(socket)
	_ = server.KillServer()
	if err := os.Remove(server.SocketPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("remove muxer-exec tmux socket: %v", err)
	}
}

func TestMuxerExecChildHelperProcess(t *testing.T) {
	if os.Getenv("SCIMUX_MUXER_EXEC_CHILD_TEST") != "1" {
		return
	}
	data := os.Getenv("SCIMUX_MUXER_EXEC_DATA")
	handoff, err := loadMuxerExecFiles(os.Getenv)
	_ = os.Unsetenv(envMuxerPublicFD)
	_ = os.Unsetenv(envMuxerLockFD)
	_ = os.Unsetenv(envMuxerCSRFToken)
	if err != nil || handoff == nil {
		t.Fatalf("load handoff = %#v, %v", handoff, err)
	}
	cmd := &Command{
		Args: []string{"scimux", "-data", data, "-addr", "127.0.0.1:0"},
		Home: data, Stdout: io.Discard, Stderr: io.Discard,
		Config: remote.Config{}, muxerOnly: true, handoff: handoff,
	}
	if err := cmd.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cmd.csrfToken != muxerExecCSRFToken {
		t.Fatalf("CSRF token after exec = %q, want pre-exec token %q", cmd.csrfToken, muxerExecCSRFToken)
	}
	csrfToken = cmd.csrfToken
	workers, socket := productionMuxerExecManager(t, data, "muxer-v2")
	nodes := make([]*Node, 0, len(muxerExecHarnesses))
	for _, harness := range muxerExecHarnesses {
		nodes = append(nodes, &Node{ID: harness.ID, Agent: harness.Agent, Transport: harness.Transport})
	}
	if err := workers.Reconcile(nodes); err != nil {
		t.Fatal(err)
	}
	workerLocators := make(map[string]sessionworker.Locator, len(nodes))
	for _, node := range nodes {
		locator, err := sessionworker.Discover(data, node.ID)
		if err != nil {
			t.Fatal(err)
		}
		workerLocators[node.ID] = locator
	}
	stop := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, _ *http.Request) {
		state := muxerExecState{MuxerPID: os.Getpid(), Build: workers.build, Workers: make(map[string]muxerExecWorkerState, len(nodes))}
		for _, node := range nodes {
			locator, _ := sessionworker.Discover(data, node.ID)
			observed := workers.State(node.ID)
			pending, ok := workers.Pending(node.ID)
			state.Workers[node.ID] = muxerExecWorkerState{
				PID: locator.PID, Build: locator.Identity.Build, SessionID: observed.SessionID,
				Live: observed.Live, Peek: workers.Peek(node.ID), Watermark: observed.Watermark,
				HasSession: observed.HasSession, TurnInFlight: observed.TurnInFlight, Delivery: observed.Delivery,
				Pending: ok, RequestID: pending.RequestID,
			}
		}
		_ = json.NewEncoder(w).Encode(state)
	})
	mux.HandleFunc("POST /answer", func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.URL.Query().Get("id")
		pending, ok := workers.Pending(nodeID)
		if !ok {
			http.Error(w, "no permission", http.StatusConflict)
			return
		}
		token, _, err := workers.PrepareResolve(nodeID, pending.RequestID, "1")
		if err == nil {
			err = workers.Deliver(nodeID, token)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) {
		if err := workers.Send(r.URL.Query().Get("id"), "permission-after-update"); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(stop)
	})
	srv := &http.Server{Handler: guardMutations(mux)}
	go func() { _ = srv.Serve(cmd.Listener()) }()
	<-stop
	_ = srv.Close()
	for _, node := range nodes {
		if err := workers.Kill(node.ID); err != nil {
			t.Errorf("stop %s: %v", node.Agent, err)
		}
	}
	workers.Shutdown()
	cleanupMuxerExecTmux(t, socket)
	for nodeID, locator := range workerLocators {
		var workerStatus syscall.WaitStatus
		if pid, err := syscall.Wait4(locator.PID, &workerStatus, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
			t.Fatalf("reattached %s worker was not reaped after stop: wait4 pid=%d err=%v status=%v", nodeID, pid, err, workerStatus)
		}
	}
	cmd.closeListener()
	cmd.closeOwnership()
}

func TestMuxerExecPreservesWorkerAndHasNoOwnershipGap(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux and real process exec")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestMuxerExecParentHelperProcess$")
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	defer readyW.Close()
	cmd.ExtraFiles = []*os.File{readyW}
	cmd.Env = append(os.Environ(), "SCIMUX_MUXER_EXEC_PARENT_TEST=1", muxerExecReadyFDEnv+"=3")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var childOutput bytes.Buffer
	cmd.Stdout, cmd.Stderr = &childOutput, &childOutput
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = readyW.Close()
	process := cmd.Process
	waited := false
	defer func() {
		if !waited {
			_ = process.Kill()
			_ = cmd.Wait()
		}
	}()
	var ready muxerExecReady
	readyResult := make(chan error, 1)
	go func() { readyResult <- json.NewDecoder(readyR).Decode(&ready) }()
	select {
	case err := <-readyResult:
		if err != nil {
			_ = process.Kill()
			_ = cmd.Wait()
			waited = true
			t.Fatalf("read muxer-exec readiness: %v\n%s", err, childOutput.String())
		}
	case <-time.After(15 * time.Second):
		_ = process.Kill()
		_ = cmd.Wait()
		waited = true
		t.Fatalf("timed out waiting for muxer-exec readiness\n%s", childOutput.String())
	}
	t.Cleanup(func() { _ = os.RemoveAll(ready.DataDir) })
	if _, err := backend.Claim(ready.DataDir); !errors.Is(err, backend.ErrMuxerOwned) {
		t.Fatalf("ownership before exec = %v", err)
	}
	if _, err := stdin.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()

	deadline := time.Now().Add(5 * time.Second)
	var state muxerExecState
	for time.Now().Before(deadline) {
		if contender, err := backend.Claim(ready.DataDir); err == nil {
			_ = contender.Close()
			t.Fatal("muxer ownership became claimable during exec")
		} else if !errors.Is(err, backend.ErrMuxerOwned) {
			t.Fatalf("ownership during exec = %v", err)
		}
		resp, err := testPollHTTPClient.Get("http://" + ready.Addr + "/state")
		if err == nil {
			err = json.NewDecoder(resp.Body).Decode(&state)
			_ = resp.Body.Close()
			if err == nil && len(state.Workers) == len(muxerExecHarnesses) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if state.MuxerPID != ready.MuxerPID || state.Build != "muxer-v2" {
		t.Fatalf("state after exec = %#v, ready=%#v", state, ready)
	}
	for _, harness := range muxerExecHarnesses {
		worker := state.Workers[harness.ID]
		if worker.PID != ready.WorkerPIDs[harness.ID] || worker.Build != "muxer-v1" || !worker.HasSession || worker.Live == "exited" {
			t.Fatalf("%s state after exec = %#v, ready PID=%d", harness.Agent, worker, ready.WorkerPIDs[harness.ID])
		}
		switch harness.Transport {
		case "acp":
			if !worker.Pending || worker.RequestID == "" {
				t.Fatalf("%s lost its in-flight approval: %#v", harness.Agent, worker)
			}
		case "codex":
			if strings.Count(worker.Peek, "pong") != 1 {
				t.Fatalf("Codex lost its completed pre-update turn: %#v", worker)
			}
		case "tmux":
			if worker.SessionID == "" || worker.Watermark == 0 || worker.Delivery != "" || worker.TurnInFlight {
				t.Fatalf("Claude lost its completed pre-update turn: %#v", worker)
			}
		}
	}
	beforeUpdate := state
	withoutToken, err := testHTTPClient.Post("http://"+ready.Addr+"/send?id="+muxerExecHarnesses[0].ID, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = withoutToken.Body.Close()
	if withoutToken.StatusCode != http.StatusForbidden {
		t.Fatalf("post-exec mutation without browser token = %d, want 403", withoutToken.StatusCode)
	}
	postMuxerExec(t, ready.Addr, "/answer", muxerExecACPHarnesses(), ready.CSRFToken)
	waitMuxerExecHTTPState(t, ready.Addr, func(state muxerExecState) bool {
		for _, harness := range muxerExecACPHarnesses() {
			worker := state.Workers[harness.ID]
			if worker.Live != "quiet" || strings.Count(worker.Peek, "continued-after-approval") < 2 {
				return false
			}
		}
		return true
	})
	postMuxerExec(t, ready.Addr, "/send", muxerExecHarnesses, ready.CSRFToken)
	state = waitMuxerExecHTTPState(t, ready.Addr, func(state muxerExecState) bool {
		for _, harness := range muxerExecHarnesses {
			worker := state.Workers[harness.ID]
			if worker.PID != ready.WorkerPIDs[harness.ID] {
				return false
			}
			switch harness.Transport {
			case "acp":
				if !worker.Pending {
					return false
				}
			case "codex":
				if worker.Live != "quiet" || strings.Count(worker.Peek, "pong") < 2 {
					return false
				}
			case "tmux":
				if worker.Watermark <= beforeUpdate.Workers[harness.ID].Watermark || worker.Delivery != "" || worker.TurnInFlight {
					return false
				}
			}
		}
		return true
	})
	for _, harness := range muxerExecHarnesses {
		worker := state.Workers[harness.ID]
		if worker.PID != ready.WorkerPIDs[harness.ID] {
			t.Fatalf("%s did not complete a post-update turn on its original worker: %#v", harness.Agent, worker)
		}
	}
	postMuxerExec(t, ready.Addr, "/answer", muxerExecACPHarnesses(), ready.CSRFToken)
	waitMuxerExecHTTPState(t, ready.Addr, func(state muxerExecState) bool {
		for _, harness := range muxerExecACPHarnesses() {
			worker := state.Workers[harness.ID]
			if worker.Live != "quiet" || strings.Count(worker.Peek, "continued-after-approval") < 3 {
				return false
			}
		}
		return true
	})
	stopReq, err := http.NewRequest(http.MethodPost, "http://"+ready.Addr+"/stop", nil)
	if err != nil {
		t.Fatal(err)
	}
	stopReq.Header.Set("X-Scimux-CSRF", ready.CSRFToken)
	resp, err := testHTTPClient.Do(stopReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("muxer-exec helper: %v\n%s", err, childOutput.String())
	}
	waited = true
	for _, harness := range muxerExecHarnesses {
		if _, err := sessionworker.Discover(ready.DataDir, harness.ID); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("explicit stop left %s worker: %v", harness.Agent, err)
		}
	}
}

func muxerExecACPHarnesses() []muxerExecHarness {
	var out []muxerExecHarness
	for _, harness := range muxerExecHarnesses {
		if harness.Transport == "acp" {
			out = append(out, harness)
		}
	}
	return out
}

func postMuxerExec(t *testing.T, addr, path string, harnesses []muxerExecHarness, csrf string) {
	t.Helper()
	for _, harness := range harnesses {
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+path+"?id="+harness.ID, nil)
		if err != nil {
			t.Fatalf("%s %s request: %v", harness.Agent, path, err)
		}
		req.Header.Set("X-Scimux-CSRF", csrf)
		resp, err := testHTTPClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s %s = %#v, %v", harness.Agent, path, resp, err)
		}
		_ = resp.Body.Close()
	}
}

func waitMuxerExecHTTPState(t *testing.T, addr string, accept func(muxerExecState) bool) muxerExecState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var state muxerExecState
	for time.Now().Before(deadline) {
		state = readMuxerExecState(t, addr)
		if accept(state) {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("muxer workers did not reach expected state: %#v", state)
	return state
}

func readMuxerExecState(t *testing.T, addr string) muxerExecState {
	t.Helper()
	resp, err := testPollHTTPClient.Get("http://" + addr + "/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var state muxerExecState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
