package app

import (
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func killUnwaitedWorkerAndAwaitZombie(t *testing.T, pid int) {
	t.Helper()
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	// /proc can report a zombie group leader before the remaining threads
	// release their shared file table and lifetime lock. Wait for the whole
	// process to exit, but leave the child for the manager's reaping assertion.
	const pPID = 1
	deadline := time.Now().Add(3 * time.Second)
	for {
		// Linux siginfo_t is 128 bytes. Only its first int32 (si_signo) is
		// needed; an aligned buffer avoids architecture-specific union offsets.
		var info [16]uint64
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid),
			uintptr(unsafe.Pointer(&info[0])), syscall.WEXITED|syscall.WNOWAIT|syscall.WNOHANG, 0, 0)
		if errno != 0 && errno != syscall.EINTR {
			t.Fatalf("observe worker exit: %v", errno)
		}
		if errno == 0 && *(*int32)(unsafe.Pointer(&info[0])) == int32(syscall.SIGCHLD) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not finish exiting")
		}
		time.Sleep(time.Millisecond)
	}
}
