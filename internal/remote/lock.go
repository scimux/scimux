package remote

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/scimux/scimux/internal/privatefs"
)

const lockFileName = "state.lock"

func (c *Client) acquireLock(ctxDone <-chan struct{}) error {
	if err := c.holdLock("start"); err != nil {
		return err
	}

	if c.cfg.LockHeldFile != "" {
		if err := os.WriteFile(c.cfg.LockHeldFile, []byte("held\n"), 0o600); err != nil {
			c.releaseLock()
			return err
		}
	}
	if c.cfg.LockReleaseFile != "" {
		if err := waitForFile(c.cfg.LockReleaseFile, ctxDone); err != nil {
			c.releaseLock()
			return err
		}
	}
	return nil
}

func (c *Client) holdLock(op string) error {
	c.lockMu.Lock()
	defer c.lockMu.Unlock()
	if c.lockFile != nil {
		c.lockRefs++
		if c.cfg.OnLockHeld != nil {
			c.cfg.OnLockHeld()
		}
		return nil
	}
	if err := c.tryFlockLocked(op); err != nil {
		return err
	}
	c.lockRefs = 1
	if c.cfg.OnLockHeld != nil {
		c.cfg.OnLockHeld()
	}
	return nil
}

func (c *Client) unholdLock() {
	c.lockMu.Lock()
	defer c.lockMu.Unlock()
	if c.lockRefs > 0 {
		c.lockRefs--
	}
	if c.lockRefs == 0 {
		c.releaseLockLocked()
	}
}

func (c *Client) tryFlock(op string) error {
	c.lockMu.Lock()
	defer c.lockMu.Unlock()
	return c.tryFlockLocked(op)
}

func (c *Client) tryFlockLocked(op string) error {
	if c.lockFile != nil {
		return nil
	}
	if err := privatefs.EnsureDir(c.PrivateDir(), 0o700); err != nil {
		return err
	}
	path := filepath.Join(c.PrivateDir(), lockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := privatefs.SecureOpenedFile(path, f, 0o600); err != nil {
		_ = f.Close()
		return err
	}
	fd := f.Fd()
	if err := syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return classErrorf(ClassStateLock, op, "another process holds the remote state lock", err)
	}
	syscall.CloseOnExec(int(fd))
	runtime.KeepAlive(f)
	c.lockFile = f
	return nil
}

func (c *Client) releaseLock() {
	c.lockMu.Lock()
	defer c.lockMu.Unlock()
	c.lockRefs = 0
	c.releaseLockLocked()
}

func (c *Client) releaseLockLocked() {
	if c.lockFile == nil {
		return
	}
	_ = syscall.Flock(int(c.lockFile.Fd()), syscall.LOCK_UN)
	_ = c.lockFile.Close()
	c.lockFile = nil
}

func waitForFile(path string, ctxDone <-chan struct{}) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctxDone:
			return os.ErrDeadlineExceeded
		case <-time.After(5 * time.Millisecond):
		}
	}
}
