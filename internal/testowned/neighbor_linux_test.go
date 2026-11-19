//go:build linux && !testowned_unsupported

package testowned

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func neighborResponds(_ *exec.Cmd, input, output *os.File) error {
	if err := input.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	if err := output.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	if _, err := input.Write([]byte("ping\n")); err != nil {
		return err
	}
	var response [5]byte
	if _, err := io.ReadFull(output, response[:]); err != nil {
		return err
	}
	if string(response[:]) != "pong\n" {
		return fmt.Errorf("unexpected synthetic fixture response")
	}
	return nil
}

func TestNeighborProbeRejectsUnreapedDeadFixture(t *testing.T) {
	cmd, input, output := startNeighbor(t)
	defer func() { _ = input.Close(); _ = output.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := neighborResponds(cmd, input, output); err != nil {
		t.Fatalf("live fixture did not respond: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/stat")
		if err != nil {
			t.Fatal(err)
		}
		end := strings.LastIndex(string(data), ") ")
		if end >= 0 && strings.HasPrefix(string(data[end+2:]), "Z ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture did not become an unreaped zombie")
		}
		time.Sleep(time.Millisecond)
	}
	if err := neighborResponds(cmd, input, output); err == nil {
		t.Fatal("dead unreaped fixture was accepted as alive")
	}
}

func startNeighbor(t *testing.T) (*exec.Cmd, *os.File, *os.File) {
	t.Helper()
	childIn, parentIn, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	parentOut, childOut, err := os.Pipe()
	if err != nil {
		_ = childIn.Close()
		_ = parentIn.Close()
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", "--norc", "-c", `while IFS= read -r line; do printf 'pong\n'; done`)
	cmd.Stdin = childIn
	cmd.Stdout = childOut
	if err := cmd.Start(); err != nil {
		_ = childIn.Close()
		_ = parentIn.Close()
		_ = parentOut.Close()
		_ = childOut.Close()
		t.Fatal(err)
	}
	_ = childIn.Close()
	_ = childOut.Close()
	return cmd, parentIn, parentOut
}

func cleanupOwnedFixtures(k kernel) Result { return supervise(k, 20, 400) }
func TestFixtureCleanupActuallyRetiresAndReaps(t *testing.T) {
	k := &fakeKernel{childrenLists: [][]int{{41}, {41}, {}}, waits: map[int][]bool{41: {false, true}}}
	result := cleanupOwnedFixtures(k)
	want := []string{"prepare", "start", "runner", "children", "wait", "signal:41:15", "children", "wait", "children"}
	if !result.Clean || result.Reaped != 1 || !reflect.DeepEqual(k.events, want) {
		t.Fatalf("cleanup did not retire and reap fixture: %+v events=%v", result, k.events)
	}
}

// fixtureReaper is confined to this test process. All direct Cmd.Wait calls
// finish before it asks the kernel to retire any adopted descendants.
type fixtureReaper struct {
	nativeKernel
	prior      int32
	blocked    bool
	clean      bool
	diagnostic string
}

func fixtureErrClass(err error) string {
	switch {
	case errors.Is(err, syscall.ESRCH):
		return "esrch"
	case errors.Is(err, syscall.ENOENT):
		return "enoent"
	case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES):
		return "permission"
	case errors.Is(err, syscall.EINTR):
		return "interrupted"
	case errors.Is(err, syscall.ECHILD):
		return "echild"
	case err != nil && (err.Error() == "invalid process status" || err.Error() == "invalid process parent"):
		return "parse"
	default:
		var errno syscall.Errno
		if errors.As(err, &errno) {
			return "errno-other"
		}
		return "other"
	}
}

func fixtureWaitClass(pid int, err error) string {
	if err != nil {
		return "wait-" + fixtureErrClass(err)
	}
	if pid == 0 {
		return "wait-live"
	}
	return "wait-reaped"
}

func TestFixtureDiagnosticClasses(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{syscall.ESRCH, "esrch"}, {syscall.ENOENT, "enoent"},
		{syscall.EPERM, "permission"}, {syscall.EACCES, "permission"},
		{syscall.EINTR, "interrupted"}, {syscall.ECHILD, "echild"},
		{errors.New("invalid process status"), "parse"},
		{errors.New("invalid process parent"), "parse"},
		{syscall.EIO, "errno-other"}, {errors.New("synthetic"), "other"}, {nil, "other"},
	} {
		if got := fixtureErrClass(tc.err); got != tc.want {
			t.Fatalf("error class = %s, want %s", got, tc.want)
		}
	}
	for _, tc := range []struct {
		pid  int
		err  error
		want string
	}{{0, nil, "wait-live"}, {11, nil, "wait-reaped"}, {0, syscall.ECHILD, "wait-echild"}, {0, syscall.EPERM, "wait-permission"}} {
		if got := fixtureWaitClass(tc.pid, tc.err); got != tc.want {
			t.Fatalf("wait class = %s, want %s", got, tc.want)
		}
	}
}

func (r *fixtureReaper) prepare() error           { return nil }
func (r *fixtureReaper) start() error             { return nil }
func (r *fixtureReaper) waitRunner() (int, error) { return 0, nil }
func (r *fixtureReaper) pause(d time.Duration)    { time.Sleep(d) }
func installFixtureReaper(t *testing.T) *fixtureReaper {
	t.Helper()
	if err := noExistingChildren(); err != nil {
		t.Fatalf("fixture process already has a child: %v", err)
	}
	r := &fixtureReaper{}
	r.nativeKernel = nativeKernel{ops: nativeOps{
		list: func() ([]int, error) {
			pids, err := listProcChildren("/proc", os.Getpid())
			if err != nil {
				r.diagnostic = "list-" + fixtureErrClass(err)
			}
			return pids, err
		},
		wait4: func(pid int) (int, error) {
			var status syscall.WaitStatus
			got, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
			if pid == -1 {
				r.diagnostic = fixtureWaitClass(got, err)
			}
			return got, err
		},
		kill: func(pid, sig int) error { return syscall.Kill(pid, syscall.Signal(sig)) },
	}}
	_, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, 37, uintptr(unsafe.Pointer(&r.prior)), 0, 0, 0, 0)
	if errno != 0 {
		t.Fatalf("inspect fixture subreaper: %v", errno)
	}
	if err := registerSubreaper(func() syscall.Errno { _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0); return e }); err != nil {
		t.Fatalf("register fixture subreaper: %v", err)
	}
	t.Cleanup(func() {
		if r.blocked {
			t.Error("direct fixture wait unconfirmed; adopted-child cleanup withheld")
		} else if result := cleanupOwnedFixtures(r); !result.Clean {
			t.Errorf("fixture descendant cleanup unproven: %s detail=%s", result.Category, r.diagnostic)
		} else {
			r.clean = true
		}
		_, _, e := syscall.Syscall6(syscall.SYS_PRCTL, 36, uintptr(r.prior), 0, 0, 0, 0)
		if e != 0 {
			t.Errorf("restore fixture subreaper: %v", e)
		}
	})
	return r
}
func finishNeighbor(t *testing.T, guard *fixtureReaper, cmd *exec.Cmd, input, output *os.File) {
	t.Helper()
	_ = input.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("fixture neighbor exit: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		select {
		case <-done:
			t.Error("fixture neighbor required forced termination")
		case <-time.After(2 * time.Second):
			guard.blocked = true
			t.Error("fixture neighbor wait unconfirmed")
		}
	}
	_ = output.Close()
}

func fixtureRoot(t *testing.T) (string, *fixtureReaper) {
	t.Helper()
	dir, err := os.MkdirTemp("", "testowned-")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("fixture root ownership unproven")
	}
	var guard *fixtureReaper
	t.Cleanup(func() {
		if guard == nil || !guard.clean {
			t.Error("fixture root retained because termination is unproven")
			return
		}
		now, err := os.Lstat(dir)
		if err != nil || !os.SameFile(info, now) || !now.IsDir() || now.Mode().Perm() != 0700 {
			t.Error("fixture root identity changed; retained")
			return
		}
		current, ok := now.Sys().(*syscall.Stat_t)
		if !ok || current.Uid != st.Uid {
			t.Error("fixture root owner changed; retained")
			return
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("fixture root removal failed: %v", err)
		}
	})
	guard = installFixtureReaper(t)
	return dir, guard
}

func TestFixtureCleanupReapsAdoptedAfterRunnerExit(t *testing.T) {
	dir, guard := fixtureRoot(t)
	pidfile := dir + "/pid"
	cmd := exec.Command("/bin/bash", "--norc", "-c", `(sleep 5 & echo $! > "$1") & wait`, "synthetic", pidfile)
	if err := cmd.Run(); err != nil {
		t.Fatalf("synthetic parent: %v", err)
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	result := cleanupOwnedFixtures(guard)
	if !result.Clean || result.Reaped < 1 {
		t.Fatalf("cleanup unproven: clean=%t category=%s reaped=%d detail=%s", result.Clean, result.Category, result.Reaped, guard.diagnostic)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("adopted fixture still present: %v", err)
	}
}

func TestPreexistingChildProbePreservesWaitStatus(t *testing.T) {
	guard := installFixtureReaper(t)
	neighbor, input, output := startNeighbor(t)
	defer finishNeighbor(t, guard, neighbor, input, output)
	if err := noExistingChildren(); err == nil {
		t.Fatal("pre-existing child was accepted")
	}
	if err := neighborResponds(neighbor, input, output); err != nil {
		t.Fatalf("pre-existing child stopped responding: %v", err)
	}
}
