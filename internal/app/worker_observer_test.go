package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionworker"
	"github.com/scimux/scimux/internal/testowned/workerfixture"
)

func TestReapObserverOnlyReceivesActualTerminalResult(t *testing.T) {
	got := 0
	observe := func(pid int, status syscall.WaitStatus) {
		got++
		if pid != 7 || !status.Signaled() {
			t.Error("wrong receipt")
		}
	}
	reportReaped(nil, 7, 7, syscall.WaitStatus(9), nil)
	for _, c := range []struct {
		pid, waited int
		status      syscall.WaitStatus
		err         error
	}{
		{0, 0, 9, nil}, {7, 0, 9, nil}, {7, 8, 9, nil}, {7, 7, 9, syscall.ECHILD}, {7, 7, 0x7f, nil},
	} {
		reportReaped(observe, c.pid, c.waited, c.status, c.err)
	}
	if got != 0 {
		t.Fatal("observer received invalid receipt")
	}
	reportReaped(observe, 7, 7, syscall.WaitStatus(9), nil)
	if got != 1 {
		t.Fatal("terminal receipt omitted")
	}
	exited := false
	reportReaped(func(pid int, status syscall.WaitStatus) { exited = pid == 7 && status.Exited() }, 7, 7, 0, nil)
	if !exited {
		t.Fatal("normal exit receipt omitted")
	}
}

func TestReattachedFixtureReleasesCommandBeforeGC(t *testing.T) {
	if !t.Run("reattached", func(t *testing.T) {
		data, pid, client, fixture := startUnwaitedSyntheticWorker(t, "finalizer-synthetic")
		killUnwaitedWorkerAndAwaitZombie(t, pid)
		m := newWorkerManager("", data, "test")
		m.reapObserver = fixture.observe
		m.entries["finalizer-synthetic"] = &workerEntry{client: client, pid: pid}
		if err := m.Kill("finalizer-synthetic"); err != nil {
			t.Fatal(err)
		}
		if !fixture.reaped {
			t.Fatal("manager did not report direct reap")
		}
	}) {
		t.Fatal("reattached fixture failed")
	}
	runtime.GC()
	debug.FreeOSMemory()
	time.Sleep(20 * time.Millisecond)
}

func TestRetryReapReleasesProcessBeforeGC(t *testing.T) {
	if !t.Run("retry", TestWorkerManagerRetriesReapAfterProvenDeadTimeout) {
		t.Fatal("retry fixture failed")
	}
	runtime.GC()
	debug.FreeOSMemory()
	time.Sleep(20 * time.Millisecond)
}

func TestReapObserverDoesNotTreatECHILDAsReceipt(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := reapReattachedWorker(cmd.Process.Pid, time.Now().Add(time.Second), func(int, syscall.WaitStatus) { called = true }); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("ECHILD produced a receipt")
	}
}

func TestStartedObserverCapturesPreSocketDirectWait(t *testing.T) {
	parent, tmp, err := workerfixture.New(filepath.Clean(os.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	if err := os.Chmod(data, 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := sessionWorkerConfig{DataDir: data, NodeID: "pre-socket", Identity: sessionworker.Identity{WorkerID: "pre-socket", Agent: "pi"}}
	var owned *sessionWorkerProcess
	var bindErr error
	p, err := startSessionWorker(context.Background(), exe, config, sessionWorkerStartOptions{args: []string{"-test.run=^TestSessionWorkerStartupFailureHelperProcess$"}, env: []string{"SCIMUX_SESSION_WORKER_STARTUP_FAILURE=timeout", "TMPDIR=" + tmp}, timeout: 10 * time.Millisecond, started: func(p *sessionWorkerProcess) { owned = p; bindErr = parent.BindStarted(p.pid) }})
	if err == nil || p != nil || owned == nil || bindErr != nil {
		t.Fatal("started child was not captured")
	}
	select {
	case <-owned.wait:
	case <-time.After(3 * time.Second):
		t.Fatal("direct wait missing")
	}
	waitResult := func() (*os.ProcessState, error) {
		owned.mu.Lock()
		defer owned.mu.Unlock()
		return owned.state, owned.err
	}
	if err := parent.CleanupEmptyWait(owned.pid, owned.wait, waitResult); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty private parent remains")
	}
}

func TestStartedObserverNotCalledOnStartFailure(t *testing.T) {
	called := false
	cfg := sessionWorkerConfig{DataDir: t.TempDir(), NodeID: "synthetic", Identity: sessionworker.Identity{WorkerID: "synthetic", Agent: "pi"}}
	p, err := startSessionWorker(context.Background(), filepath.Join(t.TempDir(), "absent"), cfg, sessionWorkerStartOptions{started: func(*sessionWorkerProcess) { called = true }})
	if err == nil || p != nil || called {
		t.Fatal("failed start reported a child")
	}
}
