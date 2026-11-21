//go:build linux && !testowned_unsupported

package testowned

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// requireDescriptorSeal skips an explicit-handle test where this system
// refuses the adapter's close_range(CLOSE_RANGE_CLOEXEC) seal, for example
// under a container seccomp profile. The adapter then reports a prerequisite
// failure before launch, which native_linux_test.go covers.
func requireDescriptorSeal(t *testing.T) {
	t.Helper()
	if err := DescriptorSealAvailable(); err != nil {
		t.Skipf("explicit-handle supervisor prerequisite unavailable: close_range: %v", err)
	}
}

// waitHandleFixture waits each direct supervisor before the fixture reaper
// examines adopted children. Timeout never treats a killed supervisor as proof.
func waitHandleFixture(t *testing.T, guard *fixtureReaper, cmd *exec.Cmd) func() error {
	t.Helper()
	cmd.WaitDelay = 2 * time.Second
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	var result error
	wait := func() error {
		once.Do(func() {
			select {
			case result = <-done:
			case <-time.After(2 * time.Second):
				result = errors.New("fixture supervisor timed out")
				_ = cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					_ = cmd.Process.Kill()
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						guard.blocked = true
					}
				}
			}
		})
		return result
	}
	t.Cleanup(func() {
		if err := wait(); err != nil {
			t.Errorf("fixture supervisor: %v", err)
		}
	})
	return wait
}

func TestExplicitHandlesHelper(t *testing.T) {
	if os.Getenv("TESTOWNED_HANDLES_HELPER") != "1" {
		return
	}
	result := RunWithHandles([]string{"/bin/bash", "--norc", "-c", `printf ready >&3; if printf leak 2>/dev/null >&4; then exit 19; fi; IFS= read -r line; printf 'out:%s' "$line"; printf err >&2`}, Handles{Stdin: os.Stdin, Stdout: os.NewFile(4, "out"), Stderr: os.NewFile(5, "err"), ExtraFiles: []*os.File{os.NewFile(3, "ready")}})
	_ = json.NewEncoder(os.Stdout).Encode(result)
	os.Exit(0)
}

func TestExplicitHandlesStartFailureHelper(t *testing.T) {
	if os.Getenv("TESTOWNED_HANDLES_FAILURE_HELPER") != "1" {
		return
	}
	result := RunWithHandles([]string{"/definitely-absent-synthetic-binary"}, Handles{ExtraFiles: []*os.File{os.NewFile(3, "ready")}})
	if os.Getenv("TESTOWNED_BLOCK_PROOF_HELPER") == "1" {
		time.Sleep(30 * time.Second)
	}
	_, _ = os.Stdout.WriteString(result.Category + "\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestHandleProofReadTimeoutIsBoundedAndReaped(t *testing.T) {
	requireDescriptorSeal(t)
	_, guard := fixtureRoot(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestSupervisorHandlesClosedOnStartFailure$")
	cmd.Env = append(os.Environ(), "TESTOWNED_BLOCK_PROOF_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("blocked proof should fail: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			guard.blocked = true
		}
		t.Fatal("blocked proof read did not return within its deadline")
	}
	if result := cleanupOwnedFixtures(guard); !result.Clean {
		t.Fatalf("probe descendants unproven: %s", result.Category)
	}
}

func TestHandleOrphanHelper(t *testing.T) {
	if os.Getenv("TESTOWNED_HANDLES_ORPHAN_HELPER") != "1" {
		return
	}
	result := RunWithHandles([]string{"/bin/bash", "--norc", "-c", `sleep 10 & printf '%s\n' "$!" >&3; printf runner; exit 17`}, Handles{Stdout: os.NewFile(4, "runner-output"), ExtraFiles: []*os.File{os.NewFile(3, "ready")}})
	_ = json.NewEncoder(os.Stdout).Encode(result)
	os.Exit(0)
}

func TestUnregisteredHandleHelper(t *testing.T) {
	if os.Getenv("TESTOWNED_UNREGISTERED_HELPER") != "1" {
		return
	}
	if os.Getenv("TESTOWNED_BLOCK_HANDLE_HELPER") == "1" {
		time.Sleep(30 * time.Second)
	}
	result := RunWithHandles([]string{"/bin/bash", "--norc", "-c", `if printf leaked 2>/dev/null >&5; then exit 19; fi; exit 0`}, Handles{ExtraFiles: []*os.File{os.NewFile(3, "ready")}, Stdout: os.NewFile(4, "output")})
	_ = json.NewEncoder(os.Stdout).Encode(result)
	os.Exit(0)
}

func TestHandleProbeTimeoutIsBoundedAndReaped(t *testing.T) {
	requireDescriptorSeal(t)
	_, guard := fixtureRoot(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestUnregisteredHandleNotInherited$")
	cmd.Env = append(os.Environ(), "TESTOWNED_BLOCK_HANDLE_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("blocked probe should fail: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			guard.blocked = true
		}
		t.Fatal("blocked probe did not return within its deadline")
	}
	if result := cleanupOwnedFixtures(guard); !result.Clean {
		t.Fatalf("probe descendants unproven: %s", result.Category)
	}
}

func TestUnregisteredHandleNotInherited(t *testing.T) {
	requireDescriptorSeal(t)
	guard := installFixtureReaper(t)
	var readers, writers []*os.File
	for range 3 {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		readers, writers = append(readers, r), append(writers, w)
		defer r.Close()
		defer w.Close()
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUnregisteredHandleHelper$")
	cmd.Env = append(os.Environ(), "TESTOWNED_UNREGISTERED_HELPER=1")
	cmd.ExtraFiles = writers
	var proof bytes.Buffer
	cmd.Stdout = &proof
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wait := waitHandleFixture(t, guard, cmd)
	for _, f := range writers {
		_ = f.Close()
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	var result Result
	if err := json.Unmarshal(proof.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Clean || result.RunnerExit != 0 {
		t.Fatalf("unexpected inherited handle: %+v", result)
	}
}

func TestHandleOrphanEarlyExitPreservesNeighbor(t *testing.T) {
	requireDescriptorSeal(t)
	guard := installFixtureReaper(t)
	neighbor, input, output := startNeighbor(t)
	defer finishNeighbor(t, guard, neighbor, input, output)
	pidRead, pidWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pidRead.Close()
	defer pidWrite.Close()
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outRead.Close()
	defer outWrite.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHandleOrphanHelper$")
	cmd.Env = append(os.Environ(), "TESTOWNED_HANDLES_ORPHAN_HELPER=1")
	cmd.ExtraFiles = []*os.File{pidWrite, outWrite}
	var proof bytes.Buffer
	cmd.Stdout = &proof
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wait := waitHandleFixture(t, guard, cmd)
	_ = pidWrite.Close()
	_ = outWrite.Close()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	var result Result
	if err := json.Unmarshal(proof.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Clean || result.RunnerExit != 17 || result.Reaped < 1 {
		t.Fatalf("unproven early exit: %+v", result)
	}
	if err := pidRead.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(pidRead)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("adopted child remains: %v", err)
	}
	if err := outRead.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	raw, err = io.ReadAll(outRead)
	if err != nil || string(raw) != "runner" {
		t.Fatalf("runner output %q: %v", raw, err)
	}
	if err := neighborResponds(neighbor, input, output); err != nil {
		t.Fatalf("neighbor failed: %v", err)
	}
}

func TestSupervisorHandlesClosedOnStartFailure(t *testing.T) {
	requireDescriptorSeal(t)
	guard := installFixtureReaper(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestExplicitHandlesStartFailureHelper$")
	cmd.Env = append(os.Environ(), "TESTOWNED_HANDLES_FAILURE_HELPER=1")
	cmd.ExtraFiles = []*os.File{writer}
	gate, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	proof, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wait := waitHandleFixture(t, guard, cmd)
	_ = writer.Close()
	defer gate.Close()
	var status [6]byte
	if err := proof.(*os.File).SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(proof, status[:]); err != nil || string(status[:]) != "start\n" {
		t.Fatalf("proof %q: %v", status, err)
	}
	if err := reader.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := reader.Read(b[:]); err != io.EOF {
		t.Fatalf("handle remained open: %v", err)
	}
	_ = gate.Close()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorExplicitHandlesAndEOF(t *testing.T) {
	requireDescriptorSeal(t)
	guard := installFixtureReaper(t)
	readers := make([]*os.File, 3)
	writers := make([]*os.File, 3)
	for i := range readers {
		var err error
		readers[i], writers[i], err = os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer readers[i].Close()
		defer writers[i].Close()
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestExplicitHandlesHelper$")
	cmd.Env = append(os.Environ(), "TESTOWNED_HANDLES_HELPER=1")
	cmd.Stdin = strings.NewReader("synthetic input\n")
	cmd.ExtraFiles = writers
	var proof bytes.Buffer
	cmd.Stdout = &proof
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wait := waitHandleFixture(t, guard, cmd)
	for _, writer := range writers {
		_ = writer.Close()
	}
	if err := wait(); err != nil {
		t.Fatalf("supervisor wait: %v", err)
	}
	var result Result
	if err := json.Unmarshal(proof.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Clean || result.RunnerExit != 0 {
		t.Fatalf("runner result: %+v", result)
	}
	for i, want := range []string{"ready", "out:synthetic input", "err"} {
		if err := readers[i].SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(readers[i])
		if err != nil || string(got) != want {
			t.Fatalf("handle %d: got %q error %v", i, got, err)
		}
	}
}

func TestSupervisorRealHelper(t *testing.T) {
	if os.Getenv("TESTOWNED_HELPER") != "1" {
		return
	}
	result := Run([]string{"/bin/bash", "--norc", "-c", `(sleep 8 & echo $! > "$1") & wait; exit 7`, "synthetic", os.Getenv("TESTOWNED_PIDFILE")})
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}
func TestSupervisorRealOrphan(t *testing.T) {
	dir, _ := fixtureRoot(t)
	pidfile := dir + "/pid"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSupervisorRealHelper$")
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), "TESTOWNED_HELPER=1", "TESTOWNED_PIDFILE="+pidfile)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("supervisor helper failed: %v", err)
	}
	var result Result
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("invalid status: %v", err)
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("missing synthetic child pid: %v", err)
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Clean || result.RunnerExit != 7 || result.Reaped < 1 {
		t.Fatalf("unproven cleanup: %+v", result)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("synthetic child still present: %v", err)
	}
}

func TestSupervisorSignalHelper(t *testing.T) {
	if os.Getenv("TESTOWNED_SIGNAL_HELPER") != "1" {
		return
	}
	script := `trap 'exit 0' TERM INT; (trap 'sleep 5 & echo $! > "$4"; echo term > "$3"; sleep 1; exit 0' TERM; trap '' HUP; echo $BASHPID > "$1"; while :; do sleep .1; done) & while [ ! -f "$1" ]; do sleep .01; done; echo ready > "$2"; while :; do sleep .1; done`
	result := Run([]string{"/bin/bash", "--norc", "-c", script, "synthetic", os.Getenv("TESTOWNED_PIDFILE"), os.Getenv("TESTOWNED_READY"), os.Getenv("TESTOWNED_TERM"), os.Getenv("TESTOWNED_NEWCHILD")})
	_ = json.NewEncoder(os.Stdout).Encode(result)
	os.Exit(0)
}
func TestSupervisorRepeatedSignalAndNeighbor(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) { testSignalScenario(t, sig) })
	}
}
func testSignalScenario(t *testing.T, sig syscall.Signal) {
	dir, guard := fixtureRoot(t)
	pidfile := dir + "/pid"
	ready := dir + "/ready"
	termfile := dir + "/term"
	newchildfile := dir + "/newchild"
	neighbor, neighborInput, neighborOutput := startNeighbor(t)
	defer finishNeighbor(t, guard, neighbor, neighborInput, neighborOutput)
	cmd := exec.Command(os.Args[0], "-test.run=^TestSupervisorSignalHelper$")
	cmd.Env = append(os.Environ(), "TESTOWNED_SIGNAL_HELPER=1", "TESTOWNED_PIDFILE="+pidfile, "TESTOWNED_READY="+ready, "TESTOWNED_TERM="+termfile, "TESTOWNED_NEWCHILD="+newchildfile)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finished := false
	defer func() {
		if finished {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
			finished = true
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			select {
			case <-done:
				finished = true
			case <-time.After(2 * time.Second):
				guard.blocked = true
				t.Error("supervisor fixture wait unconfirmed")
			}
		}
	}()
	deadline := time.Now().Add(4 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("synthetic runner did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(4 * time.Second)
	for {
		if _, err := os.Stat(termfile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owned child did not enter teardown")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		finished = true
		t.Fatalf("supervisor exited before repeated signal: %v", err)
	default:
	}
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatalf("supervisor exited: %v", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("supervisor timeout")
	}
	var result Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Clean || result.Reaped < 1 {
		t.Fatalf("unproven cleanup: %+v", result)
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("orphan still alive: %v", err)
	}
	raw, err = os.ReadFile(newchildfile)
	if err != nil {
		t.Fatal(err)
	}
	newpid, err := strconv.Atoi(string(bytes.TrimSpace(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(newpid, 0); err != syscall.ESRCH {
		t.Fatalf("child spawned during teardown still alive: %v", err)
	}
	if err := neighborResponds(neighbor, neighborInput, neighborOutput); err != nil {
		t.Fatalf("unrelated neighbor did not survive: %v", err)
	}
}

func TestNonChildProbeHelper(t *testing.T) {
	if os.Getenv("TESTOWNED_NONCHILD") != "1" {
		return
	}
	pid, err := strconv.Atoi(os.Getenv("TESTOWNED_NEIGHBOR"))
	if err != nil {
		os.Exit(2)
	}
	k := nativeKernel{ops: nativeOps{wait4: func(pid int) (int, error) {
		var status syscall.WaitStatus
		return syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	}}}
	_, err = k.wait(pid)
	if err != syscall.ECHILD {
		os.Exit(3)
	}
	os.Exit(0)
}
func TestActualNonChildRefused(t *testing.T) {
	guard := installFixtureReaper(t)
	neighbor, neighborInput, neighborOutput := startNeighbor(t)
	defer finishNeighbor(t, guard, neighbor, neighborInput, neighborOutput)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNonChildProbeHelper$")
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), "TESTOWNED_NONCHILD=1", "TESTOWNED_NEIGHBOR="+strconv.Itoa(neighbor.Process.Pid))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe failed: %v %s", err, out)
	}
	if err := neighborResponds(neighbor, neighborInput, neighborOutput); err != nil {
		t.Fatalf("neighbor did not survive: %v", err)
	}
}

func TestBrowserHandlesHelper(t *testing.T) {
	if os.Getenv("TESTOWNED_PREFLIGHT_HELPER") != "1" {
		return
	}
	result := RunBrowserWithHandles([]string{"/bin/bash", "--norc", "-c", "exit 0"}, Handles{})
	_ = json.NewEncoder(os.Stdout).Encode(result)
	os.Exit(0)
}

func TestBrowserHandlesLaunchAndProof(t *testing.T) {
	requireDescriptorSeal(t)
	guard := installFixtureReaper(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestBrowserHandlesHelper$")
	cmd.Env = append(os.Environ(), "TESTOWNED_PREFLIGHT_HELPER=1")
	var output bytes.Buffer
	cmd.Stdout = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wait := waitHandleFixture(t, guard, cmd)
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	var result Result
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Clean || result.Category != "clean" || result.RunnerExit != 0 {
		t.Fatalf("browser teardown proof: %+v", result)
	}
}
