// Package workerfixture provides cleanup for directly waited synthetic workers.
// It is used by tests only and must not be imported by the application.
package workerfixture

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

var errUnsafe = errors.New("worker fixture: ownership proof unavailable")
var ErrIdentity = errors.New("worker fixture: identity changed")
var ErrContents = errors.New("worker fixture: unexpected contents")

// StopUnproved stops and waits for the fixture's direct child when the
// manager did not provide a reaping receipt. It does not authorize removal.
func StopUnproved(process *os.Process) error {
	if process == nil {
		return errUnsafe
	}
	return stopUnproved(process.Kill, process.Wait)
}
func stopUnproved(kill func() error, wait func() (*os.ProcessState, error)) error {
	killErr := kill()
	state, waitErr := wait()
	if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) && !errors.Is(killErr, syscall.ESRCH) {
		return killErr
	}
	if waitErr != nil {
		return waitErr
	}
	if state == nil {
		return errUnsafe
	}
	return nil
}

type Parent struct {
	path, dir, socket             string
	rootInfo, dirInfo, socketInfo os.FileInfo
	pid                           int
	startedPID                    int
}

// BindStarted records the actual child as soon as Start succeeds.
func (p *Parent) BindStarted(pid int) error {
	if p == nil || pid <= 0 || p.startedPID != 0 || p.pid != 0 {
		return errUnsafe
	}
	p.startedPID = pid
	return nil
}

// CleanupWait accepts only a completed direct wait for the registered child.
// These forced-kill fixtures retain the ExitError returned by cmd.Wait.
func (p *Parent) CleanupWait(wait <-chan struct{}, result func() error, closeClient func() error) error {
	if p == nil || p.pid == 0 || wait == nil || result == nil || closeClient == nil {
		return errUnsafe
	}
	select {
	case <-wait:
	default:
		return errUnsafe
	}
	var exit *exec.ExitError
	if !errors.As(result(), &exit) || exit == nil || exit.ProcessState == nil || exit.ProcessState.Pid() != p.pid {
		return errUnsafe
	}
	return p.Cleanup(exit.ProcessState, closeClient())
}

// CleanupEmptyWait removes an empty registered parent after the directly
// started child was waited, before it created any socket.
func (p *Parent) CleanupEmptyWait(pid int, wait <-chan struct{}, result func() (*os.ProcessState, error)) error {
	return p.cleanupEmptyWait(pid, wait, result, os.ReadDir, os.Remove)
}
func (p *Parent) cleanupEmptyWait(pid int, wait <-chan struct{}, result func() (*os.ProcessState, error), readDir func(string) ([]os.DirEntry, error), remove func(string) error) error {
	if p == nil || p.pid != 0 || pid <= 0 || p.startedPID != pid || wait == nil || result == nil {
		return errUnsafe
	}
	select {
	case <-wait:
	default:
		return errUnsafe
	}
	state, waitErr := result()
	if state == nil || state.Pid() != pid {
		return errUnsafe
	}
	if waitErr == nil {
		if !state.Success() {
			return errUnsafe
		}
	} else {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) || exit == nil || exit.ProcessState != state {
			return errUnsafe
		}
	}
	if !samePrivateDir(p.path, p.rootInfo) {
		return ErrIdentity
	}
	entries, err := readDir(p.path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return ErrContents
	}
	return remove(p.path)
}

// New creates and records the private TMPDIR before the child is started.
func New(base string) (*Parent, string, error) {
	return newParent(base, os.MkdirTemp, os.Lstat)
}
func newParent(base string, mkdir func(string, string) (string, error), lstat func(string) (os.FileInfo, error)) (*Parent, string, error) {
	if !filepath.IsAbs(base) || filepath.Clean(base) != base || len(filepath.Join(base, "wf-1234567890", "scimux-worker-1234567890", "worker.sock")) > 103 {
		return nil, "", errUnsafe
	}
	path, err := mkdir(base, "wf-")
	if err != nil {
		return nil, "", err
	}
	info, err := lstat(path)
	if err != nil {
		return nil, "", err
	}
	if !privateDir(info) {
		return nil, "", errUnsafe
	}
	return &Parent{path: path, rootInfo: info}, path, nil
}
func privateDir(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
func same(path string, original os.FileInfo) bool {
	info, err := os.Lstat(path)
	return err == nil && original != nil && os.SameFile(info, original)
}
func samePrivateDir(path string, original os.FileInfo) bool {
	info, err := os.Lstat(path)
	return err == nil && original != nil && privateDir(info) && os.SameFile(info, original)
}

// Register binds the authenticated worker socket and actual direct child PID.
func (p *Parent) Register(socket string, pid int) error {
	if p == nil || p.pid != 0 || pid <= 0 || (p.startedPID != 0 && p.startedPID != pid) || filepath.Base(socket) != "worker.sock" || filepath.Dir(filepath.Dir(socket)) != p.path || !samePrivateDir(p.path, p.rootInfo) {
		return errUnsafe
	}
	dir := filepath.Dir(socket)
	d, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !privateDir(d) {
		return errUnsafe
	}
	s, err := os.Lstat(socket)
	if err != nil {
		return err
	}
	if s.Mode()&os.ModeSocket == 0 {
		return errUnsafe
	}
	p.dir, p.socket, p.pid, p.dirInfo, p.socketInfo = dir, socket, pid, d, s
	return nil
}

// Cleanup requires the actual direct Wait's ProcessState and successful Close.
// It removes only the recorded socket and empty directories, never unknown files.
func (p *Parent) Cleanup(state *os.ProcessState, closeErr error) error {
	return p.cleanup(state, closeErr, os.ReadDir, os.Remove)
}
func (p *Parent) cleanup(state *os.ProcessState, closeErr error, readDir func(string) ([]os.DirEntry, error), remove func(string) error) error {
	if p == nil || p.pid == 0 || closeErr != nil || state == nil || state.Pid() != p.pid {
		return errUnsafe
	}
	return p.removeOwned(readDir, remove)
}

// CleanupReaped consumes the positive Wait4 result captured from the owning
// manager. ECHILD and non-terminal statuses cannot establish this receipt.
func (p *Parent) CleanupReaped(pid int, status syscall.WaitStatus, closeErr error) error {
	if p == nil || p.pid == 0 || closeErr != nil || pid != p.pid || (!status.Exited() && !status.Signaled()) {
		return errUnsafe
	}
	return p.removeOwned(os.ReadDir, os.Remove)
}

func (p *Parent) removeOwned(readDir func(string) ([]os.DirEntry, error), remove func(string) error) error {
	if !samePrivateDir(p.path, p.rootInfo) || !samePrivateDir(p.dir, p.dirInfo) || !same(p.socket, p.socketInfo) {
		return ErrIdentity
	}
	rootEntries, err := readDir(p.path)
	if err != nil {
		return err
	}
	if len(rootEntries) != 1 || rootEntries[0].Name() != filepath.Base(p.dir) {
		return ErrContents
	}
	entries, err := readDir(p.dir)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(p.socket) {
		return ErrContents
	}
	if err := remove(p.socket); err != nil {
		return err
	}
	if err := remove(p.dir); err != nil {
		return err
	}
	return remove(p.path)
}
