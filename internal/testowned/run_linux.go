//go:build linux && !testowned_unsupported

package testowned

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

type processKernel struct {
	nativeKernel
	args            []string
	cmd             *exec.Cmd
	signals         chan os.Signal
	preflight       func() error
	register        func() error
	seal            func() error
	handles         Handles
	browserTeardown bool
}

func (k *processKernel) prepare() error {
	if err := k.preflight(); err != nil {
		return err
	}
	if k.seal != nil {
		if err := k.seal(); err != nil {
			return err
		}
	}
	return k.register()
}

func sealInherited(arch string, call func() syscall.Errno) error {
	if arch != "amd64" && arch != "arm64" {
		return errors.New("unsupported descriptor isolation")
	}
	if err := call(); err != 0 {
		return err
	}
	return nil
}
func noExistingChildren() error {
	var info [128]byte
	_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, 0, 0, uintptr(unsafe.Pointer(&info[0])), 4|1|0x1000000, 0, 0)
	return checkNoExisting(errno)
}
func registerSubreaper(call func() syscall.Errno) error {
	errno := call()
	if errno != 0 {
		return errno
	}
	return nil
}

func (k *processKernel) start() error {
	defer k.handles.close()
	if len(k.args) == 0 || k.args[0] == "" {
		return errors.New("missing runner")
	}
	k.cmd = exec.Command(k.args[0], k.args[1:]...)
	if k.handles.Stdin != nil {
		k.cmd.Stdin = k.handles.Stdin
	}
	if k.handles.Stdout != nil {
		k.cmd.Stdout = k.handles.Stdout
	}
	if k.handles.Stderr != nil {
		k.cmd.Stderr = k.handles.Stderr
	}
	k.cmd.ExtraFiles = k.handles.ExtraFiles
	return k.cmd.Start()
}
func (k *processKernel) waitRunner() (int, error) {
	done := make(chan runnerWait, 1)
	go func() {
		err := k.cmd.Wait()
		done <- runnerOutcome(err, func() any { return k.cmd.ProcessState.Sys() })
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	grace, limit := runnerBounds(k.browserTeardown)
	return awaitRunner(done, k.signals, ticker.C, func(s os.Signal) error { return k.cmd.Process.Signal(s) }, func() error { return k.cmd.Process.Kill() }, grace, limit)
}
func (k *processKernel) pause(d time.Duration) { time.Sleep(d) }
func Run(args []string) Result {
	return run(args, Handles{}, nil, false)
}

// RunWithHandles is for a fresh, dedicated supervisor process. It takes
// ownership of the supplied handles even when its prerequisite check fails.
func RunWithHandles(args []string, handles Handles) Result {
	return runWithHandles(args, handles, false)
}

// RunBrowserWithHandles allows bounded teardown of the browser test runner.
func RunBrowserWithHandles(args []string, handles Handles) Result {
	return runWithHandles(args, handles, true)
}

func runWithHandles(args []string, handles Handles, browser bool) Result {
	return run(args, handles, func() error {
		return sealInherited(runtime.GOARCH, func() syscall.Errno {
			// close_range(3, UINT_MAX, CLOSE_RANGE_CLOEXEC) marks descriptors
			// without closing or enumerating them; exec maps explicit handles.
			_, _, errno := syscall.Syscall(436, 3, ^uintptr(0)>>32, 4)
			return errno
		})
	}, browser)
}

func run(args []string, handles Handles, seal func() error, browser bool) Result {
	defer handles.close()
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	k := &processKernel{args: args, signals: signals, handles: handles, seal: seal, browserTeardown: browser, preflight: noExistingChildren, register: func() error {
		return registerSubreaper(func() syscall.Errno { _, _, err := syscall.Syscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0); return err })
	}, nativeKernel: nativeKernel{ops: nativeOps{
		list: func() ([]int, error) {
			return listProcChildren("/proc", os.Getpid())
		},
		wait4: func(pid int) (int, error) {
			var status syscall.WaitStatus
			return syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		},
		kill: func(pid, sig int) error { return syscall.Kill(pid, syscall.Signal(sig)) },
	}}}
	return supervise(k, 20, 400)
}
func runnerOutcome(err error, status func() any) runnerWait {
	if err == nil {
		return runnerWait{code: 0}
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return runnerWait{err: err}
	}
	state, ok := status().(syscall.WaitStatus)
	if !ok {
		return runnerWait{err: errors.New("unknown runner status")}
	}
	if state.Signaled() {
		return runnerWait{code: 128 + int(state.Signal())}
	}
	return runnerWait{code: state.ExitStatus()}
}
func checkNoExisting(errno syscall.Errno) error {
	if errno == syscall.ECHILD {
		return nil
	}
	return errors.New("existing child state unknown")
}
