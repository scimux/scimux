package workerfixture

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func fixture(t *testing.T) (*Parent, string, string, *os.ProcessState) {
	t.Helper()
	base, err := os.MkdirTemp("", "wf-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	p, root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "worker")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "worker.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	t.Cleanup(func() { _ = ln.Close() })
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := p.Register(socket, cmd.ProcessState.Pid()); err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return p, root, socket, cmd.ProcessState
}

func TestCleanupRemovesWaitedOwnedSocket(t *testing.T) {
	p, root, _, state := fixture(t)
	if err := p.Cleanup(state, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("registered parent remains")
	}
}
func TestCleanupPreservesWithoutProofOrAfterCloseFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		closeErr error
		state    bool
	}{{"wait", nil, false}, {"close", errors.New("close"), true}} {
		t.Run(tc.name, func(t *testing.T) {
			p, root, _, state := fixture(t)
			if !tc.state {
				state = nil
			}
			if p.Cleanup(state, tc.closeErr) == nil {
				t.Fatal("missing failure")
			}
			if _, err := os.Lstat(root); err != nil {
				t.Fatal("parent lost")
			}
		})
	}
}
func TestCleanupPreservesUnknownAndReplacedEntries(t *testing.T) {
	for _, kind := range []string{"unknown", "socket", "directory", "parent", "parent-mode", "directory-mode"} {
		t.Run(kind, func(t *testing.T) {
			p, root, socket, state := fixture(t)
			dir := filepath.Dir(socket)
			switch kind {
			case "unknown":
				if err := os.WriteFile(filepath.Join(dir, "unknown"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "socket":
				if err := os.Remove(socket); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(socket, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Rename(dir, dir+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			case "parent":
				if err := os.Rename(root, root+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
			case "parent-mode":
				if err := os.Chmod(root, 0755); err != nil {
					t.Fatal(err)
				}
			case "directory-mode":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if p.Cleanup(state, nil) == nil {
				t.Fatal("unsafe cleanup accepted")
			}
			if _, err := os.Lstat(root); err != nil {
				t.Fatal("parent lost")
			}
		})
	}
}
func TestRejectLongParent(t *testing.T) {
	base := t.TempDir()
	for len(base) < 110 {
		base = filepath.Join(base, strings.Repeat("x", 20))
		if err := os.Mkdir(base, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := New(base); err == nil {
		t.Fatal("long socket parent accepted")
	}
}

func TestRegistrationRejectsInvalidPathsAndObjects(t *testing.T) {
	base, err := os.MkdirTemp("", "wf-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	p, root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	var absent *Parent
	for _, call := range []func() error{
		func() error { return absent.Register("", 1) }, func() error { return p.Register("", 0) }, func() error { return p.Register(filepath.Join(root, "missing", "worker.sock"), 1) },
	} {
		if call() == nil {
			t.Fatal("invalid registration accepted")
		}
	}
	dir := filepath.Join(root, "worker")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "worker.sock")
	if p.Register(socket, 1) == nil {
		t.Fatal("non-private directory accepted")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if p.Register(socket, 1) == nil {
		t.Fatal("missing socket accepted")
	}
	if err := os.WriteFile(socket, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if p.Register(socket, 1) == nil {
		t.Fatal("regular file accepted as socket")
	}
}

func TestWaitMustBelongToRegisteredChild(t *testing.T) {
	p, root, _, _ := fixture(t)
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := p.Cleanup(cmd.ProcessState, nil); err == nil {
		t.Fatal("other child accepted")
	}
	if _, err := os.Lstat(root); err != nil {
		t.Fatal("parent lost")
	}
}

func TestNewRejectsMissingBase(t *testing.T) {
	if _, _, err := New(filepath.Join(os.TempDir(), "missing-synthetic-base", "child")); err == nil {
		t.Fatal("missing base accepted")
	}
	if _, _, err := New("relative"); err == nil {
		t.Fatal("relative base accepted")
	}
}

func TestParentCreationErrorsPreserveAndReport(t *testing.T) {
	base, err := os.MkdirTemp("", "wf-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	failure := errors.New("synthetic filesystem failure")
	mkdir := func(string, string) (string, error) { return base, nil }
	if _, _, err := newParent(base, mkdir, func(string) (os.FileInfo, error) { return nil, failure }); !errors.Is(err, failure) {
		t.Fatal("lost inspection error")
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newParent(base, mkdir, func(string) (os.FileInfo, error) { return os.Lstat(file) }); err == nil {
		t.Fatal("non-directory parent accepted")
	}
}

func TestCleanupFilesystemFailuresAreReported(t *testing.T) {
	failure := errors.New("synthetic filesystem failure")
	for _, point := range []string{"parent-read", "directory-read", "socket-remove", "directory-remove", "parent-remove"} {
		t.Run(point, func(t *testing.T) {
			p, _, _, state := fixture(t)
			reads, removes := 0, 0
			read := func(path string) ([]os.DirEntry, error) {
				reads++
				if (point == "parent-read" && reads == 1) || (point == "directory-read" && reads == 2) {
					return nil, failure
				}
				return os.ReadDir(path)
			}
			remove := func(string) error {
				removes++
				if (point == "socket-remove" && removes == 1) || (point == "directory-remove" && removes == 2) || (point == "parent-remove" && removes == 3) {
					return failure
				}
				return nil
			}
			if err := p.cleanup(state, nil, read, remove); !errors.Is(err, failure) {
				t.Fatal("filesystem failure was not reported")
			}
		})
	}
}

func TestCleanupPreservesUnexpectedSibling(t *testing.T) {
	p, root, _, state := fixture(t)
	if err := os.WriteFile(filepath.Join(root, "neighbor"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if p.Cleanup(state, nil) == nil {
		t.Fatal("unknown sibling accepted")
	}
	if _, err := os.Lstat(filepath.Join(root, "neighbor")); err != nil {
		t.Fatal("neighbor lost")
	}
}

func TestRegistrationAndCleanupRefuseIncompleteOwnership(t *testing.T) {
	p, root, socket, state := fixture(t)
	if p.Register(socket, state.Pid()) == nil {
		t.Fatal("duplicate registration accepted")
	}
	if p.Register(filepath.Join(root, "other", "worker.sock"), state.Pid()) == nil {
		t.Fatal("different worker accepted")
	}
	var missing *Parent
	if missing.Cleanup(state, nil) == nil {
		t.Fatal("missing owner accepted")
	}
	if (&Parent{}).Cleanup(state, nil) == nil {
		t.Fatal("unregistered owner accepted")
	}
	if p.Cleanup(&os.ProcessState{}, nil) == nil {
		t.Fatal("empty wait state accepted")
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	if p.Cleanup(state, nil) == nil {
		t.Fatal("missing registered socket accepted")
	}
	if _, err := os.Lstat(root); err != nil {
		t.Fatal("parent lost")
	}
}

func TestCleanupWaitRequiresActualMatchingWaitAndClose(t *testing.T) {
	base, err := os.MkdirTemp("", "wf-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	p, root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "worker")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "worker.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	t.Cleanup(func() { _ = ln.Close() })
	cmd := exec.Command("sh", "-c", "kill -9 $$")
	waitErr := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) {
		t.Fatal("child did not exit by signal")
	}
	if err := p.Register(socket, exit.ProcessState.Pid()); err != nil {
		t.Fatal(err)
	}
	pending := make(chan struct{})
	done := make(chan struct{})
	close(done)
	result := func() error { return waitErr }
	goodClose := ln.Close
	var missing *Parent
	for _, invoke := range []func() error{
		func() error { return missing.CleanupWait(done, result, goodClose) },
		func() error { return p.CleanupWait(nil, result, goodClose) },
		func() error { return p.CleanupWait(pending, result, goodClose) },
		func() error { return p.CleanupWait(done, nil, goodClose) },
		func() error { return p.CleanupWait(done, result, nil) },
		func() error { return p.CleanupWait(done, func() error { return errors.New("wait failed") }, goodClose) },
		func() error {
			return p.CleanupWait(done, func() error { var exit *exec.ExitError; return exit }, goodClose)
		},
		func() error { return p.CleanupWait(done, result, func() error { return errors.New("close failed") }) },
	} {
		if invoke() == nil {
			t.Fatal("unproven cleanup accepted")
		}
		if _, err := os.Lstat(root); err != nil {
			t.Fatal("unproven cleanup removed parent")
		}
	}
	if err := p.CleanupWait(done, result, goodClose); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("waited parent remains")
	}
}

func TestCleanupReapedUsesTerminalWait4(t *testing.T) {
	base, err := os.MkdirTemp("", "wf-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	p, root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "worker")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "worker.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.StartProcess(shell, []string{shell, "-c", "kill -9 $$"}, &os.ProcAttr{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.BindStarted(process.Pid); err != nil {
		t.Fatal(err)
	}
	if err := p.Register(socket, process.Pid); err != nil {
		t.Fatal(err)
	}
	var status syscall.WaitStatus
	pid, err := syscall.Wait4(process.Pid, &status, 0, nil)
	if err != nil || pid != process.Pid || !status.Signaled() {
		t.Fatal("direct Wait4 missing")
	}
	if err := process.Release(); err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	var missing *Parent
	for _, invoke := range []func() error{
		func() error { return missing.CleanupReaped(pid, status, nil) },
		func() error { return p.CleanupReaped(pid+1, status, nil) },
		func() error { return p.CleanupReaped(pid, syscall.WaitStatus(0x7f), nil) },
		func() error { return p.CleanupReaped(pid, status, errors.New("close")) },
	} {
		if invoke() == nil {
			t.Fatal("bad reaping proof accepted")
		}
	}
	if err := p.CleanupReaped(pid, status, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("registered parent remains")
	}
}

func TestEmptyParentRequiresStartedChildWait(t *testing.T) {
	base, err := os.MkdirTemp("", "wf-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	p, root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "kill -9 $$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := p.BindStarted(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if p.BindStarted(cmd.Process.Pid) == nil {
		t.Fatal("duplicate start accepted")
	}
	waitErr := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) {
		t.Fatal("missing direct wait")
	}
	done := make(chan struct{})
	close(done)
	pending := make(chan struct{})
	result := func() (*os.ProcessState, error) { return cmd.ProcessState, waitErr }
	other := exec.Command("sh", "-c", "exit 0")
	if err := other.Run(); err != nil {
		t.Fatal(err)
	}
	var missing *Parent
	for _, invoke := range []func() error{
		func() error { return missing.BindStarted(cmd.Process.Pid) }, func() error { return (&Parent{}).BindStarted(0) },
		func() error { return missing.CleanupEmptyWait(cmd.Process.Pid, done, result) },
		func() error { return p.CleanupEmptyWait(cmd.Process.Pid+1, done, result) },
		func() error { return p.CleanupEmptyWait(cmd.Process.Pid, nil, result) },
		func() error { return p.CleanupEmptyWait(cmd.Process.Pid, pending, result) },
		func() error { return p.CleanupEmptyWait(cmd.Process.Pid, done, nil) },
		func() error {
			return p.CleanupEmptyWait(cmd.Process.Pid, done, func() (*os.ProcessState, error) { var exit *exec.ExitError; return cmd.ProcessState, exit })
		},
		func() error {
			return p.CleanupEmptyWait(cmd.Process.Pid, done, func() (*os.ProcessState, error) { return nil, nil })
		},
		func() error {
			return p.CleanupEmptyWait(cmd.Process.Pid, done, func() (*os.ProcessState, error) { return other.ProcessState, nil })
		},
		func() error {
			return p.CleanupEmptyWait(cmd.Process.Pid, done, func() (*os.ProcessState, error) { return cmd.ProcessState, nil })
		},

		func() error {
			return p.CleanupEmptyWait(cmd.Process.Pid, done, func() (*os.ProcessState, error) { return cmd.ProcessState, errors.New("wait") })
		},
	} {
		if invoke() == nil {
			t.Fatal("bad start proof accepted")
		}
	}
	failure := errors.New("read")
	if err := p.cleanupEmptyWait(cmd.Process.Pid, done, result, func(string) ([]os.DirEntry, error) { return nil, failure }, os.Remove); !errors.Is(err, failure) {
		t.Fatal("read failure lost")
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(p.CleanupEmptyWait(cmd.Process.Pid, done, result), ErrIdentity) {
		t.Fatal("changed parent accepted")
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "unknown")
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(p.CleanupEmptyWait(cmd.Process.Pid, done, result), ErrContents) {
		t.Fatal("unknown content accepted")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := p.CleanupEmptyWait(cmd.Process.Pid, done, result); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupReapedUsesNormalWait4Exit(t *testing.T) {
	base, err := os.MkdirTemp("", "wf-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	p, root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "worker")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "worker.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.StartProcess(shell, []string{shell, "-c", "exit 0"}, &os.ProcAttr{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.BindStarted(process.Pid); err != nil {
		t.Fatal(err)
	}
	if err := p.Register(socket, process.Pid); err != nil {
		t.Fatal(err)
	}
	var status syscall.WaitStatus
	pid, err := syscall.Wait4(process.Pid, &status, 0, nil)
	if err != nil || pid != process.Pid || !status.Exited() {
		t.Fatal("normal Wait4 missing")
	}
	if err := process.Release(); err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.CleanupReaped(pid, status, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("registered parent remains")
	}
}

func TestEmptyParentAfterCleanDirectWait(t *testing.T) {
	base, err := os.MkdirTemp("", "wf-clean-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	p, root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := p.BindStarted(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if waitErr != nil || cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatal("clean direct wait missing")
	}
	done := make(chan struct{})
	close(done)
	if err := p.CleanupEmptyWait(cmd.Process.Pid, done, func() (*os.ProcessState, error) { return cmd.ProcessState, waitErr }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty parent remains")
	}
}

func TestEmptyParentAfterNonzeroDirectWait(t *testing.T) {
	base, err := os.MkdirTemp("", "wf-nonzero-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	p, root, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := p.BindStarted(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) || exit.ProcessState != cmd.ProcessState {
		t.Fatal("nonzero direct wait missing")
	}
	done := make(chan struct{})
	close(done)
	result := func() (*os.ProcessState, error) { return cmd.ProcessState, waitErr }
	if err := p.CleanupEmptyWait(cmd.Process.Pid, done, result); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty parent remains")
	}
}

func TestStopUnprovedStopsAndWaitsDirectChild(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	path, err := exec.LookPath("cat")
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.StartProcess(path, []string{path}, &os.ProcAttr{Files: []*os.File{reader, nil, nil}})
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := StopUnproved(process); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Wait(); err == nil {
		t.Fatal("direct child was not waited")
	}
	if StopUnproved(nil) == nil {
		t.Fatal("missing child accepted")
	}
}

func TestStopUnprovedReportsStopAndWaitFailures(t *testing.T) {
	failure := errors.New("synthetic failure")
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		kill func() error
		wait func() (*os.ProcessState, error)
		want error
	}{
		{"kill", func() error { return failure }, func() (*os.ProcessState, error) { return cmd.ProcessState, nil }, failure},
		{"wait", func() error { return nil }, func() (*os.ProcessState, error) { return nil, failure }, failure},
		{"missing state", func() error { return nil }, func() (*os.ProcessState, error) { return nil, nil }, errUnsafe},
		{"already done", func() error { return os.ErrProcessDone }, func() (*os.ProcessState, error) { return cmd.ProcessState, nil }, nil},
		{"missing process", func() error { return syscall.ESRCH }, func() (*os.ProcessState, error) { return cmd.ProcessState, nil }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stopUnproved(tc.kill, tc.wait); !errors.Is(got, tc.want) {
				t.Fatalf("failure: %v", got)
			}
		})
	}
}
