//go:build linux && !testowned_unsupported

package testowned

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestNativeChildrenRequireKernelECHILD(t *testing.T) {
	for _, tc := range []struct {
		name               string
		listed             []int
		listErr, errorWait error
		waited             int
		want               []int
		ok                 bool
	}{
		{name: "live children", listed: []int{12, 18}, want: []int{12, 18}, ok: true},
		{name: "ECHILD", errorWait: syscall.ECHILD, ok: true},
		{name: "empty proc but live", waited: 0},
		{name: "empty proc but zombie reaped", waited: 41},
		{name: "proc failure", listErr: errors.New("synthetic")},
		{name: "wait failure", errorWait: syscall.EPERM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			n := nativeKernel{ops: nativeOps{list: func() ([]int, error) { return tc.listed, tc.listErr }, wait4: func(pid int) (int, error) {
				calls++
				if pid != -1 {
					t.Fatalf("wait4 pid %d", pid)
				}
				return tc.waited, tc.errorWait
			}}}
			got, err := n.children()
			if (err == nil) != tc.ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v error %v", got, err)
			}
			if len(tc.listed) > 0 && calls != 0 {
				t.Fatal("reaped a child during enumeration")
			}
			if len(tc.listed) == 0 && tc.listErr == nil && calls != 1 {
				t.Fatal("missing kernel proof")
			}
		})
	}
}
func TestNativeWaitAndSignal(t *testing.T) {
	for _, tc := range []struct {
		name string
		ret  int
		err  error
		dead bool
		ok   bool
	}{{"running", 0, nil, false, true}, {"reaped", 7, nil, true, true}, {"not child", 0, syscall.ECHILD, false, false}, {"wrong pid", 8, nil, false, false}, {"error", 0, syscall.EPERM, false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			n := nativeKernel{ops: nativeOps{wait4: func(pid int) (int, error) {
				if pid != 7 {
					t.Fatalf("wait %d", pid)
				}
				return tc.ret, tc.err
			}}}
			d, err := n.wait(7)
			if d != tc.dead || (err == nil) != tc.ok {
				t.Fatalf("dead %v err %v", d, err)
			}
		})
	}
	var pid, sig int
	n := nativeKernel{ops: nativeOps{kill: func(p, s int) error { pid, sig = p, s; return nil }}}
	if err := n.signal(7, 15); err != nil || pid != 7 || sig != 15 {
		t.Fatalf("signal %d %d %v", pid, sig, err)
	}
	n.ops.kill = func(int, int) error { return syscall.EPERM }
	if n.signal(7, 9) == nil {
		t.Fatal("signal failure hidden")
	}
}

func TestListProcChildren(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "not-a-pid"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "-1"), 0700); err != nil {
		t.Fatal(err)
	}
	for id, stat := range map[string]string{"11": "11 (synthetic) S 9 0", "12": "12 (odd ) name) S 9 0", "13": "13 (other) S 2 0"} {
		if err := os.Mkdir(filepath.Join(root, id), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, id, "stat"), []byte(stat), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := listProcChildren(root, 9)
	if err != nil || !reflect.DeepEqual(got, []int{11, 12}) {
		t.Fatalf("children %v err %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(root, "11", "stat"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := listProcChildren(root, 9); err == nil {
		t.Fatal("malformed proc state accepted")
	}
	if _, err := listProcChildren(filepath.Join(root, "missing"), 9); err == nil {
		t.Fatal("missing proc accepted")
	}
}

func TestPreparationProofBeforeRegister(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pre, register error
		want          []string
		ok            bool
	}{
		{"success", nil, nil, []string{"pre", "register"}, true},
		{"unknown existing child", syscall.ECHILD, nil, []string{"pre"}, false},
		{"prctl failure", nil, syscall.EINVAL, []string{"pre", "register"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			k := processKernel{preflight: func() error { order = append(order, "pre"); return tc.pre }, register: func() error { order = append(order, "register"); return tc.register }}
			err := k.prepare()
			if (err == nil) != tc.ok || !reflect.DeepEqual(order, tc.want) {
				t.Fatalf("err %v order %v", err, order)
			}
		})
	}
}

func TestNoExistingChildrenAndStart(t *testing.T) {
	if err := noExistingChildren(); err != nil {
		t.Fatal("empty supervisor process not accepted")
	}
	child := exec.Command("/bin/bash", "--norc", "-c", "exec sleep 5")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	if err := noExistingChildren(); err == nil {
		t.Fatal("existing child accepted")
	}
	for _, args := range [][]string{nil, {""}, {"/definitely-absent-synthetic-binary"}} {
		k := processKernel{args: args}
		if err := k.start(); err == nil {
			t.Fatal("invalid child launch accepted")
		}
	}
}
func TestProcessWaitRunnerExit(t *testing.T) {
	for _, tc := range []struct {
		script string
		want   int
	}{{"exit 7", 7}, {"kill -TERM $$", 143}, {"exit 0", 0}} {
		k := processKernel{args: []string{"/bin/bash", "--norc", "-c", tc.script}, signals: make(chan os.Signal, 1)}
		if err := k.start(); err != nil {
			t.Fatal(err)
		}
		code, err := k.waitRunner()
		if err != nil || code != tc.want {
			t.Fatalf("code %d error %v", code, err)
		}
	}
}

func TestRegisterSubreaperFailure(t *testing.T) {
	if err := registerSubreaper(func() syscall.Errno { return syscall.EINVAL }); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("registration error = %v", err)
	}
}

func TestListProcFailures(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "44"), 0700); err != nil {
		t.Fatal(err)
	}
	if got, err := listProcChildren(root, 1); err != nil || len(got) != 0 {
		t.Fatalf("vanished process %v %v", got, err)
	}
	if err := os.Mkdir(filepath.Join(root, "44", "stat"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := listProcChildren(root, 1); err == nil {
		t.Fatal("read error hidden")
	}
	if err := os.Remove(filepath.Join(root, "44", "stat")); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"44 (synthetic) S", "44 (synthetic) S nope"} {
		if err := os.WriteFile(filepath.Join(root, "44", "stat"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := listProcChildren(root, 1); err == nil {
			t.Fatalf("invalid stat accepted: %q", raw)
		}
	}
}

func TestListProcChildrenSkipsExitedEntryRead(t *testing.T) {
	child := exec.Command("/bin/bash", "--norc", "-c", "exec sleep 30")
	if err := child.Start(); err != nil {
		t.Fatal("synthetic child start failed")
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	stat, err := os.Open(filepath.Join("/proc", strconv.Itoa(child.Process.Pid), "stat"))
	if err != nil {
		t.Fatal("synthetic child stat open failed")
	}
	defer stat.Close()
	if err := child.Process.Kill(); err != nil {
		t.Fatal("synthetic child termination failed")
	}
	waitErr := child.Wait()
	waited = child.ProcessState != nil
	var exitErr *exec.ExitError
	if !waited || !errors.As(waitErr, &exitErr) {
		t.Fatal("synthetic child wait unconfirmed")
	}

	root := t.TempDir()
	for _, name := range []string{"11", "12"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal("synthetic entry creation failed")
		}
	}
	if err := os.WriteFile(filepath.Join(root, "11", "stat"), []byte("11 (synthetic) S 9 0"), 0600); err != nil {
		t.Fatal("synthetic stat creation failed")
	}
	if err := os.Symlink(filepath.Join("/proc/self/fd", strconv.Itoa(int(stat.Fd()))), filepath.Join(root, "12", "stat")); err != nil {
		t.Fatal("synthetic stat link failed")
	}
	got, err := listProcChildren(root, 9)
	if err != nil || !reflect.DeepEqual(got, []int{11}) {
		t.Fatalf("exited entry rejected: ESRCH=%t; matching entries=%d", errors.Is(err, syscall.ESRCH), len(got))
	}
}

func TestListProcEntryReadErrors(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"11", "12"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal("synthetic entry creation failed")
		}
	}
	for _, tc := range []struct {
		name string
		err  error
		want []int
	}{
		{"success", nil, []int{11, 12}},
		{"gone", syscall.ENOENT, []int{11}},
		{"exited", syscall.ESRCH, []int{11}},
		{"permission", syscall.EPERM, nil},
		{"other", syscall.EIO, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := listProcChildrenRead(root, 9, func(path string) ([]byte, error) {
				if filepath.Base(filepath.Dir(path)) == "12" {
					return []byte("12 (synthetic) S 9 0"), tc.err
				}
				return []byte("11 (synthetic) S 9 0"), nil
			})
			if tc.err == syscall.EPERM || tc.err == syscall.EIO {
				if !errors.Is(err, tc.err) || got != nil {
					t.Fatal("entry error not preserved")
				}
			} else if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatal("matching entries not preserved")
			}
		})
	}
	called := false
	_, err := listProcChildrenRead(filepath.Join(root, "missing"), 9, func(string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if err == nil || called {
		t.Fatal("root enumeration failure hidden")
	}
}

func TestRunnerOutcome(t *testing.T) {
	synthetic := errors.New("synthetic")
	for _, tc := range []struct {
		name    string
		err     error
		status  any
		code    int
		failure bool
	}{{"success", nil, nil, 0, false}, {"wait error", synthetic, nil, 0, true}, {"invalid status", &exec.ExitError{}, "bad", 0, true}, {"exit", &exec.ExitError{}, syscall.WaitStatus(7 << 8), 7, false}, {"signal", &exec.ExitError{}, syscall.WaitStatus(syscall.SIGTERM), 143, false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := runnerOutcome(tc.err, func() any { return tc.status })
			if r.code != tc.code || (r.err != nil) != tc.failure {
				t.Fatalf("result %+v", r)
			}
		})
	}
}

func TestProcessWaitEscalatesRunner(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	k := processKernel{args: []string{"/bin/bash", "--norc", "-c", `trap '' TERM; echo ready > "$1"; exec sleep 6`, "synthetic", ready}, signals: make(chan os.Signal, 2)}
	if err := k.start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k.cmd.Process.Kill() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = k.cmd.Process.Kill()
			_ = k.cmd.Wait()
			t.Fatal("runner did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	k.signals <- syscall.SIGTERM
	k.signals <- syscall.SIGTERM
	code, err := k.waitRunner()
	if err != nil || code != 137 {
		t.Fatalf("code %d error %v", code, err)
	}
}

func TestKernelEmptyProofErrors(t *testing.T) {
	for _, tc := range []struct {
		errno syscall.Errno
		ok    bool
	}{{syscall.ECHILD, true}, {0, false}, {syscall.EPERM, false}, {syscall.EINTR, false}} {
		err := checkNoExisting(tc.errno)
		if (err == nil) != tc.ok {
			t.Fatalf("errno=%v err=%v", tc.errno, err)
		}
	}
}

func TestRunDirectSynthetic(t *testing.T) {
	result := Run([]string{"/bin/bash", "--norc", "-c", "exit 12"})
	_, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, 36, 0, 0, 0, 0, 0)
	if errno != 0 {
		t.Fatalf("restore subreaper: %v", errno)
	}
	if !result.Clean || result.Category != "clean" || result.RunnerExit != 12 {
		t.Fatalf("result %+v", result)
	}
}
