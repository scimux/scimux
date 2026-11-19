package app

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionworker"
)

func TestForcedWorkerFixtureAcceptsTrailingTempSeparator(t *testing.T) {
	root := os.Getenv("SCIMUX_WORKER_FIXTURE_ROOT")
	if root == "" {
		t.Skip("requires a private externally supervised worker fixture root")
	}
	t.Setenv("TMPDIR", root+string(os.PathSeparator))
	data := t.TempDir()
	if err := os.Chmod(data, 0700); err != nil {
		t.Fatal(err)
	}
	m := syntheticWorkerManager(t, data)
	t.Cleanup(m.Detach)
	f := prepareForcedWorker(t, m)
	if _, err := m.Launch("trailing-temp", "pi", data, "", ""); err != nil {
		t.Fatal(err)
	}
	f.register(t, m, "trailing-temp")
	p, err := os.FindProcess(f.entry.process.pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.entry.process.wait:
	case <-time.After(3 * time.Second):
		t.Fatal("worker wait missing")
	}
}

// The outer test owner supplies a fresh short private root inside a supervised
// process namespace; this test never searches or removes historical workers.
func TestKilledSyntheticWorkerFixtureRemovesOwnedSocketParent(t *testing.T) {
	root := os.Getenv("SCIMUX_WORKER_FIXTURE_ROOT")
	if root == "" {
		t.Skip("requires a private externally supervised worker fixture root")
	}
	if st, err := os.Lstat(root); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatal("private worker fixture root is invalid")
	}
	t.Setenv("TMPDIR", root)
	neighbor := filepath.Join(root, "scimux-worker-neighbor")
	if err := os.Mkdir(neighbor, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(neighbor, "neighbor.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	checkNeighbor := func() {
		c, err := net.DialTimeout("unix", filepath.Join(neighbor, "neighbor.sock"), time.Second)
		if err != nil {
			t.Fatal("neighbor is not usable")
		}
		_ = c.Close()
	}
	checkNeighbor()
	var workerDir string
	var original os.FileInfo
	waited := false
	if !t.Run("existing manager fixture", func(t *testing.T) {
		data := t.TempDir()
		if err := os.Chmod(data, 0o700); err != nil {
			t.Fatal("cannot protect synthetic worker data")
		}
		m := syntheticWorkerManager(t, data)
		t.Cleanup(m.Detach)
		fixture := prepareForcedWorker(t, m)
		if _, err := m.Launch("synthetic-cleanup", "pi", data, "", ""); err != nil {
			t.Fatal(err)
		}
		fixture.register(t, m, "synthetic-cleanup")
		entry := m.entry("synthetic-cleanup")
		if entry == nil || entry.process == nil {
			t.Fatal("fixture did not own a worker process")
		}
		t.Cleanup(func() {
			select {
			case <-entry.process.wait:
			default:
				owned, err := os.FindProcess(entry.process.pid)
				if err != nil || owned.Kill() != nil {
					t.Error("could not stop owned worker")
				}
			}
			select {
			case <-entry.process.wait:
			case <-time.After(3 * time.Second):
				t.Error("owned worker was not waited")
			}
		})
		locator, err := sessionworker.Discover(data, "synthetic-cleanup")
		if err != nil {
			t.Fatal(err)
		}
		workerDir = filepath.Dir(locator.Link.Socket)
		if filepath.Dir(workerDir) != fixture.path || workerDir == neighbor {
			t.Fatal("worker parent was not registered under the private root")
		}
		original, err = os.Lstat(workerDir)
		if err != nil || !original.IsDir() {
			t.Fatal("worker parent identity unavailable")
		}
		process, err := os.FindProcess(entry.process.pid)
		if err != nil {
			t.Fatal(err)
		}
		if err := process.Kill(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-entry.process.wait:
			entry.process.mu.Lock()
			waitErr := entry.process.err
			entry.process.mu.Unlock()
			var exit *exec.ExitError
			if !errors.As(waitErr, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatal("worker did not have the expected waited termination")
			}
			waited = true
		case <-time.After(3 * time.Second):
			t.Fatal("worker close was not waited")
		}
	}) {
		t.Fatal("fixture setup or termination proof failed")
	}
	if !waited || workerDir == "" || original == nil {
		t.Fatal("owned worker proof is incomplete")
	}
	checkNeighbor()
	remaining, err := os.Lstat(workerDir)
	if err == nil {
		if !os.SameFile(original, remaining) {
			t.Fatal("owned worker parent was replaced")
		}
		t.Error("owned worker socket parent remains after fixture cleanup")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("worker parent could not be inspected")
	}
}
