//go:build linux && !testowned_unsupported

package testowned

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
	"unsafe"
)

type processKernel struct {
	nativeKernel
	args      []string
	cmd       *exec.Cmd
	signals   chan os.Signal
	preflight func() error
	register  func() error
}

func (k *processKernel) prepare() error {
	if err := k.preflight(); err != nil {
		return err
	}
	return k.register()
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
	if len(k.args) == 0 || k.args[0] == "" {
		return errors.New("missing runner")
	}
	k.cmd = exec.Command(k.args[0], k.args[1:]...)
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
	return awaitRunner(done, k.signals, ticker.C, func(s os.Signal) error { return k.cmd.Process.Signal(s) }, func() error { return k.cmd.Process.Kill() }, 20, 40)
}
func (k *processKernel) pause(d time.Duration) { time.Sleep(d) }
func Run(args []string) Result {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	k := &processKernel{args: args, signals: signals, preflight: noExistingChildren, register: func() error {
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
