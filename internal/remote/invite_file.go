package remote

import (
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

func (c *Client) readInviteFile(path string) (string, error) {
	if c.cfg.Sys != nil {
		return c.readInviteFileSys(path)
	}
	return c.readInviteFileOwned(path)
}

func (c *Client) readInviteFileSys(path string) (string, error) {
	sys := c.sys()
	st, err := sys.Lstat(path)
	if err != nil {
		return "", classErrorf(ClassInviteFile, "invite-file", "invite file is not usable", err)
	}
	if !st.Regular {
		return "", classError(ClassInviteFile, "invite-file", "invite file must be a regular file owned by you with mode 0600")
	}
	if st.Mode&0o777 != 0o600 {
		return "", classError(ClassInviteFile, "invite-file", "invite file must be a regular file owned by you with mode 0600")
	}
	if st.UID != sys.EffectiveUID() {
		return "", classError(ClassInviteFile, "invite-file", "invite file must be a regular file owned by you with mode 0600")
	}
	b, err := sys.ReadFile(path)
	if err != nil {
		return "", classErrorf(ClassInviteFile, "invite-file", "invite file is not readable", err)
	}
	if len(b) > inviteInputMax {
		return "", classError(ClassInviteFormat, "invite", "invite input exceeds the allowed size")
	}
	return string(b), nil
}

func inviteFileModeError() error {
	return classError(ClassInviteFile, "invite-file", "invite file must be a regular file owned by you with mode 0600")
}

func checkInviteFileStat(st syscall.Stat_t) error {
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return inviteFileModeError()
	}
	if st.Mode&0777 != 0600 {
		return inviteFileModeError()
	}
	if int(st.Uid) != os.Geteuid() {
		return inviteFileModeError()
	}
	return nil
}

func (c *Client) readInviteFileOwned(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		var st syscall.Stat_t
		if e2 := syscall.Lstat(path, &st); e2 == nil {
			if e := checkInviteFileStat(st); e != nil {
				return "", e
			}
		}
		return "", classErrorf(ClassInviteFile, "invite-file", "invite file is not usable", err)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return "", classErrorf(ClassInviteFile, "invite-file", "invite file is not usable", err)
	}
	if err := checkInviteFileStat(st); err != nil {
		syscall.Close(fd)
		return "", err
	}
	raw := make([]byte, inviteInputMax+1)
	n, err := syscall.Read(fd, raw)
	if err != nil {
		syscall.Close(fd)
		return "", classErrorf(ClassInviteFile, "invite-file", "invite file is not readable", err)
	}
	if n > inviteInputMax {
		syscall.Close(fd)
		return "", classError(ClassInviteFormat, "invite", "invite input exceeds the allowed size")
	}
	c.closeInviteFD()
	c.inviteFD = fd
	c.inviteDev = st.Dev
	c.inviteIno = st.Ino
	c.invitePath = path
	return string(raw[:n]), nil
}

func (c *Client) closeInviteFD() {
	if c == nil || c.inviteFD < 0 {
		return
	}
	_ = syscall.Close(c.inviteFD)
	c.inviteFD = -1
}

func (c *Client) takeInviteFD() int {
	if c == nil {
		return -1
	}
	fd := c.inviteFD
	c.inviteFD = -1
	return fd
}

// releaseInvite drops this client's hold on the invite without destroying
// it: the descriptor is closed and any in-memory copy cleared, but the file
// is left intact. It is the counterpart to eraseInvite for the one outcome
// that proves the invite was never consumed.
func (c *Client) releaseInvite() {
	if c == nil {
		return
	}
	c.cfg.InviteString = ""
	if fd := c.takeInviteFD(); fd >= 0 {
		_ = syscall.Close(fd)
	}
	c.invitePath = ""
	c.inviteDev = 0
	c.inviteIno = 0
}

func (c *Client) eraseInvite(src inviteSrc, invite string) error {
	_ = invite
	c.cfg.InviteString = ""
	origFD := c.takeInviteFD()
	if origFD < 0 {
		return nil
	}
	defer syscall.Close(origFD)

	shredErr := shredInviteFD(origFD)
	if src.file == "" || c.cfg.Sys != nil {
		if shredErr != nil {
			return inviteCleanupErr(shredErr)
		}
		return nil
	}

	var orig syscall.Stat_t
	if err := syscall.Fstat(origFD, &orig); err != nil {
		if shredErr != nil {
			return inviteCleanupErr(shredErr)
		}
		return inviteCleanupErr(err)
	}

	dirfd, err := syscall.Open(filepath.Dir(src.file), syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		if shredErr != nil {
			return inviteCleanupErr(shredErr)
		}
		return inviteCleanupErr(err)
	}
	defer syscall.Close(dirfd)

	pathfd, err := syscall.Open(src.file, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if shredErr != nil {
			return inviteCleanupErr(shredErr)
		}
		if err == syscall.ENOENT {
			return nil
		}
		return inviteCleanupErr(err)
	}
	var pst syscall.Stat_t
	if err := syscall.Fstat(pathfd, &pst); err != nil {
		syscall.Close(pathfd)
		if shredErr != nil {
			return inviteCleanupErr(shredErr)
		}
		return inviteCleanupErr(err)
	}
	if pst.Dev != orig.Dev || pst.Ino != orig.Ino {
		syscall.Close(pathfd)
		if shredErr != nil {
			return inviteCleanupErr(shredErr)
		}
		return nil
	}
	syscall.Close(pathfd)

	if c.cfg.BeforeInviteUnlink != nil {
		c.cfg.BeforeInviteUnlink()
	}
	fd3, err := syscall.Open(src.file, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if shredErr != nil {
			return inviteCleanupErr(shredErr)
		}
		if err == syscall.ENOENT {
			return nil
		}
		return inviteCleanupErr(err)
	}
	var st3 syscall.Stat_t
	if err := syscall.Fstat(fd3, &st3); err != nil || st3.Dev != orig.Dev || st3.Ino != orig.Ino {
		syscall.Close(fd3)
		if shredErr != nil {
			return inviteCleanupErr(shredErr)
		}
		return nil
	}
	syscall.Close(fd3)
	if c.cfg.BeforeInviteUnlinkFinal != nil {
		c.cfg.BeforeInviteUnlinkFinal()
	}
	cerr := c.consumeInviteName(dirfd, filepath.Base(src.file), origFD, orig)
	if shredErr != nil {
		return inviteCleanupErr(shredErr)
	}
	return cerr
}

func (c *Client) consumeInviteName(dirfd int, name string, origFD int, orig syscall.Stat_t) error {
	var qname string
	var renamed bool
	for i := 0; i < inviteQuarantineAttempts; i++ {
		rnd := make([]byte, inviteQuarantineEntropy)
		if _, err := io.ReadFull(c.rand(), rnd); err != nil {
			_ = shredInviteFD(origFD)
			return inviteCleanupErr(err)
		}
		cand := inviteQuarantinePrefix + hex.EncodeToString(rnd)
		err := c.renameatNoreplace(dirfd, name, dirfd, cand)
		if err == nil {
			qname = cand
			renamed = true
			break
		}
		if err != syscall.EEXIST {
			_ = shredInviteFD(origFD)
			return inviteCleanupErr(err)
		}
	}
	if !renamed {
		_ = shredInviteFD(origFD)
		return inviteCleanupErr(syscall.EEXIST)
	}
	qfd, err := syscall.Openat(dirfd, qname, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		_ = shredInviteFD(origFD)
		if rerr := c.renameatNoreplace(dirfd, qname, dirfd, name); rerr != nil {
			return inviteCleanupErr(err)
		}
		return inviteCleanupErr(err)
	}
	var qst syscall.Stat_t
	if err := syscall.Fstat(qfd, &qst); err != nil {
		syscall.Close(qfd)
		_ = shredInviteFD(origFD)
		if rerr := c.renameatNoreplace(dirfd, qname, dirfd, name); rerr != nil {
			return inviteCleanupErr(err)
		}
		return inviteCleanupErr(err)
	}
	if qst.Dev != orig.Dev || qst.Ino != orig.Ino {
		syscall.Close(qfd)
		if c.cfg.BeforeInviteRestore != nil {
			c.cfg.BeforeInviteRestore()
		}
		if err := c.renameatNoreplace(dirfd, qname, dirfd, name); err != nil {
			_ = shredInviteFD(origFD)
			return inviteCleanupErr(err)
		}
		return shredInviteFD(origFD)
	}
	if err := shredInviteFD(qfd); err != nil {
		syscall.Close(qfd)
		return inviteCleanupErr(err)
	}
	syscall.Close(qfd)
	if err := c.unlinkat(dirfd, qname); err != nil {
		return inviteCleanupErr(err)
	}
	return nil
}

func inviteCleanupErr(err error) error {
	return classErrorf(ClassInviteFile, "invite-file", "enrollment succeeded but the invite file could not be removed; delete leftover invite files in that directory yourself. Do not retry enrollment; restart without the invite reuses this identity", err)
}

func shredInviteFD(fd int) error {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return err
	}
	zeros := make([]byte, 4096)
	var off int64
	remaining := st.Size
	for remaining > 0 {
		n := len(zeros)
		if int64(n) > remaining {
			n = int(remaining)
		}
		wn, werr := syscall.Pwrite(fd, zeros[:n], off)
		if werr != nil {
			return werr
		}
		if wn == 0 {
			break
		}
		off += int64(wn)
		remaining -= int64(wn)
	}
	if err := syscall.Ftruncate(fd, 0); err != nil {
		return err
	}
	return syscall.Fsync(fd)
}

const (
	inviteQuarantinePrefix   = ".scimux-consumed-"
	inviteQuarantineAttempts = 8
	inviteQuarantineEntropy  = 16
	renameNoreplace          = 1 // linux RENAME_NOREPLACE
)

func renameat2Trap() uintptr {
	switch runtime.GOARCH {
	case "amd64":
		return 316
	case "arm64":
		return 276
	default:
		return 0
	}
}

func (c *Client) renameatNoreplace(olddirfd int, oldpath string, newdirfd int, newpath string) error {
	nr := renameat2Trap()
	if nr == 0 {
		return syscall.ENOSYS
	}
	oldp, err := syscall.BytePtrFromString(oldpath)
	if err != nil {
		return err
	}
	newp, err := syscall.BytePtrFromString(newpath)
	if err != nil {
		return err
	}
	_, _, e := syscall.Syscall6(nr, uintptr(olddirfd), uintptr(unsafe.Pointer(oldp)), uintptr(newdirfd), uintptr(unsafe.Pointer(newp)), uintptr(renameNoreplace), 0)
	runtime.KeepAlive(oldpath)
	runtime.KeepAlive(newpath)
	if e != 0 {
		return e
	}
	return nil
}

func (c *Client) unlinkat(dirfd int, name string) error {
	if c.cfg.Unlinkat != nil {
		return c.cfg.Unlinkat(dirfd, name)
	}
	return syscall.Unlinkat(dirfd, name)
}

func (c *Client) renameat(olddirfd int, oldpath string, newdirfd int, newpath string) error {
	if c.cfg.Renameat != nil {
		return c.cfg.Renameat(olddirfd, oldpath, newdirfd, newpath)
	}
	return syscall.Renameat(olddirfd, oldpath, newdirfd, newpath)
}
