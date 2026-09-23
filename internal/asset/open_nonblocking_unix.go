//go:build linux || darwin || freebsd

package asset

import (
	"os"
	"syscall"
)

// openNonblocking makes opening a pathname safe even if a checked regular
// file is replaced by a FIFO before this call. O_NONBLOCK has no effect on an
// ordinary regular-file read after open; the descriptor's type is checked by
// OpenExternal before it is returned.
func openNonblocking(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
