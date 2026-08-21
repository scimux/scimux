package remote

import (
	"bufio"
	"os"
	"syscall"
	"unsafe"
)

type ownerTerminal struct {
	f          *os.File
	orig       syscall.Termios
	restored   bool
	restoreErr error
}

// OpenOwnerTerminal opens the process controlling terminal with echo disabled.
func OpenOwnerTerminal() (Terminal, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	t := &ownerTerminal{f: f}
	if err := t.ioctl(syscall.TCGETS, &t.orig); err != nil {
		f.Close()
		return nil, err
	}
	return t, nil
}

func (t *ownerTerminal) ioctl(req uintptr, arg *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, t.f.Fd(), req, uintptr(unsafe.Pointer(arg)))
	if errno != 0 {
		return errno
	}
	return nil
}

func (t *ownerTerminal) DisableEcho() error {
	var cur syscall.Termios
	if err := t.ioctl(syscall.TCGETS, &cur); err != nil {
		return err
	}
	cur.Lflag &^= syscall.ECHO
	return t.ioctl(syscall.TCSETS, &cur)
}

func (t *ownerTerminal) RestoreEcho() error {
	if t == nil {
		return nil
	}
	if t.restored {
		return nil
	}
	if t.f == nil {
		if t.restoreErr != nil {
			return t.restoreErr
		}
		return nil
	}
	err := t.ioctl(syscall.TCSETS, &t.orig)
	if err != nil {
		t.restoreErr = err
		return err
	}
	cerr := t.f.Close()
	t.f = nil
	t.restored = true
	t.restoreErr = nil
	return cerr
}

func (t *ownerTerminal) EchoEnabled() bool {
	var cur syscall.Termios
	if err := t.ioctl(syscall.TCGETS, &cur); err != nil {
		return false
	}
	return cur.Lflag&syscall.ECHO != 0
}

func (t *ownerTerminal) ReadLine() (string, error) {
	r := bufio.NewReaderSize(t.f, inviteInputMax+1)
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			break
		}
		if len(buf) >= inviteInputMax {
			return "", classError(ClassInviteFormat, "invite", "invite input exceeds the allowed size")
		}
		buf = append(buf, b)
	}
	return string(buf), nil
}

func (t *ownerTerminal) WritePrompt(p []byte) error {
	_, err := t.f.Write(p)
	return err
}
