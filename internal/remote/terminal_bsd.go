//go:build darwin || freebsd

package remote

import "syscall"

// Linux and the BSDs spell the same two termios ioctls differently.
// syscall.Termios, SYS_IOCTL and the ECHO flag are identical across
// them, so this split is exactly two names wide.
const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlSetTermios = syscall.TIOCSETA
)
