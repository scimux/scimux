//go:build linux && !testowned_unsupported

package testowned

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

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
