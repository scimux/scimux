package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/backend"
	"codeberg.org/chrberger/scimux/internal/remote"
	"codeberg.org/chrberger/scimux/internal/sessionworker"
)

func TestMuxerExecEnvironmentRoundTripAndValidation(t *testing.T) {
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
		{envMuxerPublicFD: "bad", envMuxerLockFD: "4"},
		{envMuxerPublicFD: "3", envMuxerLockFD: "2"},
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
		envMuxerPublicFD: strconv.Itoa(int(publicR.Fd())),
		envMuxerLockFD:   strconv.Itoa(int(lockR.Fd())),
	}
	files, err := loadMuxerExecFiles(func(key string) string { return values[key] })
	if err != nil || files == nil {
		t.Fatalf("valid inherited descriptors = %#v, %v", files, err)
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv(envMuxerPublicFD, "stale-public")
	t.Setenv(envMuxerLockFD, "stale-lock")
	env := strings.Join(muxerExecEnvironment(7, 8), "\n")
	if strings.Count(env, envMuxerPublicFD+"=") != 1 || strings.Count(env, envMuxerLockFD+"=") != 1 {
		t.Fatalf("replacement environment retained stale descriptors: %s", env)
	}
}

func TestMuxerExecPreparationAndReplacementFailClosed(t *testing.T) {
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
	cmd := &Command{listener: fileErrorListener{Listener: ln}, ownership: owner}
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
	if err := publicR.Close(); err != nil {
		t.Fatal(err)
	}
	if err := replaceMuxerProcess("/new/scimux", &muxerExecFiles{public: publicR, ownership: lockR}); err == nil {
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
	if err := replaceMuxerProcess("/new/scimux", &muxerExecFiles{public: publicR, ownership: lockR}); err == nil {
		t.Fatal("replacement accepted a closed ownership descriptor")
	}
	if !descriptorCloseOnExec(t, int(publicR.Fd())) {
		t.Fatal("public descriptor was not re-protected after ownership descriptor failure")
	}
}

func TestReplaceMuxerProcessMakesOnlyHandoffDescriptorsInheritable(t *testing.T) {
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
	files := &muxerExecFiles{public: publicR, ownership: lockR}
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
	WorkerPIDs map[string]int `json:"worker_pids"`
}

type muxerExecWorkerState struct {
	PID       int    `json:"pid"`
	Build     string `json:"build"`
	Pending   bool   `json:"pending"`
	RequestID string `json:"request_id"`
}

type muxerExecState struct {
	MuxerPID int                             `json:"muxer_pid"`
	Build    string                          `json:"build"`
	Workers  map[string]muxerExecWorkerState `json:"workers"`
}

var muxerExecHarnesses = []struct {
	ID        string
	Agent     string
	Transport string
}{
	{ID: "survivor-claude", Agent: "claude", Transport: "tmux"},
	{ID: "survivor-codex", Agent: "codex", Transport: "codex"},
	{ID: "survivor-grok", Agent: "grok", Transport: "acp"},
	{ID: "survivor-pi", Agent: "pi", Transport: "acp"},
	{ID: "survivor-opencode", Agent: "opencode", Transport: "acp"},
}

const muxerExecReadyFDEnv = "SCIMUX_MUXER_EXEC_READY_FD"

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
	cmd := &Command{listener: ln, ownership: owner}
	workers := syntheticWorkerManager(t, data)
	workers.build = "muxer-v1"
	workerPIDs := make(map[string]int, len(muxerExecHarnesses))
	for _, harness := range muxerExecHarnesses {
		if _, err := workers.Launch(harness.ID, harness.Agent, data, "cheap", "low"); err != nil {
			t.Fatalf("launch %s: %v", harness.Agent, err)
		}
		// Complete one turn before replacement, then leave a second turn
		// awaiting approval. This distinguishes a surviving chat from a
		// process that merely retained its PID without retaining its state.
		if err := workers.Send(harness.ID, "permission-before-update"); err != nil {
			t.Fatalf("send %s: %v", harness.Agent, err)
		}
		pending, ok := workers.Pending(harness.ID)
		if !ok {
			t.Fatalf("%s did not expose its first approval", harness.Agent)
		}
		token, _, err := workers.PrepareResolve(harness.ID, pending.RequestID, "1")
		if err == nil {
			err = workers.Deliver(harness.ID, token)
		}
		if err != nil {
			t.Fatalf("complete %s before update: %v", harness.Agent, err)
		}
		if err := workers.Send(harness.ID, "permission-across-update"); err != nil {
			t.Fatalf("second send %s: %v", harness.Agent, err)
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
	if err := json.NewEncoder(ready).Encode(muxerExecReady{DataDir: data, Addr: ln.Addr().String(), MuxerPID: os.Getpid(), WorkerPIDs: workerPIDs}); err != nil {
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

func TestMuxerExecChildHelperProcess(t *testing.T) {
	if os.Getenv("SCIMUX_MUXER_EXEC_CHILD_TEST") != "1" {
		return
	}
	data := os.Getenv("SCIMUX_MUXER_EXEC_DATA")
	handoff, err := loadMuxerExecFiles(os.Getenv)
	_ = os.Unsetenv(envMuxerPublicFD)
	_ = os.Unsetenv(envMuxerLockFD)
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
	workers := syntheticWorkerManager(t, data)
	workers.build = "muxer-v2"
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
			pending, ok := workers.Pending(node.ID)
			state.Workers[node.ID] = muxerExecWorkerState{
				PID: locator.PID, Build: locator.Identity.Build,
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
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(cmd.Listener()) }()
	<-stop
	_ = srv.Close()
	workers.Shutdown()
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
	cmd.Stderr = os.Stderr
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
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil {
		t.Fatal(err)
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
		resp, err := http.Get("http://" + ready.Addr + "/state")
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
		if worker.PID != ready.WorkerPIDs[harness.ID] || worker.Build != "muxer-v1" || !worker.Pending || worker.RequestID != "synthetic:1" {
			t.Fatalf("%s state after exec = %#v, ready PID=%d", harness.Agent, worker, ready.WorkerPIDs[harness.ID])
		}
	}
	postMuxerExec(t, ready.Addr, "/answer", muxerExecHarnesses)
	postMuxerExec(t, ready.Addr, "/send", muxerExecHarnesses)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state = readMuxerExecState(t, ready.Addr)
		allPending := true
		for _, harness := range muxerExecHarnesses {
			worker := state.Workers[harness.ID]
			allPending = allPending && worker.Pending && worker.PID == ready.WorkerPIDs[harness.ID]
		}
		if allPending {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, harness := range muxerExecHarnesses {
		worker := state.Workers[harness.ID]
		if !worker.Pending || worker.PID != ready.WorkerPIDs[harness.ID] {
			t.Fatalf("%s did not complete a post-update turn on its original worker: %#v", harness.Agent, worker)
		}
	}
	postMuxerExec(t, ready.Addr, "/answer", muxerExecHarnesses)
	resp, err := http.Post("http://"+ready.Addr+"/stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	waited = true
	for _, harness := range muxerExecHarnesses {
		if _, err := sessionworker.Discover(ready.DataDir, harness.ID); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("explicit stop left %s worker: %v", harness.Agent, err)
		}
	}
}

func postMuxerExec(t *testing.T, addr, path string, harnesses []struct {
	ID        string
	Agent     string
	Transport string
}) {
	t.Helper()
	for _, harness := range harnesses {
		resp, err := http.Post("http://"+addr+path+"?id="+harness.ID, "application/json", nil)
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s %s = %#v, %v", harness.Agent, path, resp, err)
		}
		_ = resp.Body.Close()
	}
}

func readMuxerExecState(t *testing.T, addr string) muxerExecState {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/state")
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
