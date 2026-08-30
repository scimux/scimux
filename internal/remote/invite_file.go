package remote

import (
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
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
		return "", classErrorf(ClassInviteFile, "invite-file", inviteFileGuidance(path, "cannot be opened"), err)
	}
	if !st.Regular {
		return "", inviteFileModeError(path)
	}
	if st.Mode&0o777 != 0o600 {
		return "", inviteFileModeError(path)
	}
	if st.UID != sys.EffectiveUID() {
		return "", inviteFileModeError(path)
	}
	b, err := sys.ReadFile(path)
	if err != nil {
		return "", classErrorf(ClassInviteFile, "invite-file", inviteFileGuidance(path, "cannot be read"), err)
	}
	if len(b) > inviteInputMax {
		return "", classError(ClassInviteFormat, "invite", "invite input exceeds the allowed size")
	}
	return string(b), nil
}

// inviteFileGuidance names the path. --invite-file is the operator saying
// "this file"; when that read fails, the one thing they need back is which
// path was tried, because the failure is nearly always a typo, a stale copy,
// or a file an earlier run already consumed. The path is theirs and is not
// the credential — the invite's *contents* are what must never be printed,
// and they are not printed here.
func inviteFileGuidance(path, what string) string {
	return "invite file " + path + " " + what
}

func inviteFileModeError(path string) error {
	return classError(ClassInviteFile, "invite-file", inviteFileGuidance(path, "must be a regular file owned by you with mode 0600"))
}

func checkInviteFileStat(path string, st syscall.Stat_t) error {
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return inviteFileModeError(path)
	}
	if st.Mode&0777 != 0600 {
		return inviteFileModeError(path)
	}
	if int(st.Uid) != os.Geteuid() {
		return inviteFileModeError(path)
	}
	return nil
}

func (c *Client) readInviteFileOwned(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		var st syscall.Stat_t
		if e2 := syscall.Lstat(path, &st); e2 == nil {
			if e := checkInviteFileStat(path, st); e != nil {
				return "", e
			}
		}
		return "", classErrorf(ClassInviteFile, "invite-file", inviteFileGuidance(path, "cannot be opened"), err)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return "", classErrorf(ClassInviteFile, "invite-file", inviteFileGuidance(path, "cannot be opened"), err)
	}
	if err := checkInviteFileStat(path, st); err != nil {
		syscall.Close(fd)
		return "", err
	}
	raw := make([]byte, inviteInputMax+1)
	n, err := syscall.Read(fd, raw)
	if err != nil {
		syscall.Close(fd)
		return "", classErrorf(ClassInviteFile, "invite-file", inviteFileGuidance(path, "cannot be read"), err)
	}
	if n > inviteInputMax {
		syscall.Close(fd)
		return "", classError(ClassInviteFormat, "invite", "invite input exceeds the allowed size")
	}
	c.closeInviteFD()
	c.inviteFD = fd
	c.inviteDev = uint64(st.Dev)
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

	root, err := os.OpenRoot(filepath.Dir(src.file))
	if err != nil {
		if shredErr != nil {
			return inviteCleanupErr(shredErr)
		}
		return inviteCleanupErr(err)
	}
	defer root.Close()

	// Directory-anchored operations need a root; the identity re-checks
	// below are path opens that were already portable.
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
	cerr := c.consumeInviteName(root, filepath.Base(src.file), origFD, orig)
	if shredErr != nil {
		return inviteCleanupErr(shredErr)
	}
	return cerr
}

func (c *Client) consumeInviteName(root *os.Root, name string, origFD int, orig syscall.Stat_t) error {
	var qname string
	var claimed bool
	for i := 0; i < inviteQuarantineAttempts; i++ {
		rnd := make([]byte, inviteQuarantineEntropy)
		if _, err := io.ReadFull(c.rand(), rnd); err != nil {
			_ = shredInviteFD(origFD)
			return inviteCleanupErr(err)
		}
		cand := inviteQuarantinePrefix + hex.EncodeToString(rnd)
		err := c.inviteLink(root, name, cand)
		if err == nil {
			// Between Link and Remove the invite is briefly reachable
			// under two names, where a rename moved it atomically. If
			// the original name is replaced inside that window, Remove
			// unlinks the replacement rather than our inode. That is an
			// accepted cost of dropping RENAME_NOREPLACE, not an oversight.
			if err := root.Remove(name); err != nil {
				_ = root.Remove(cand)
				_ = shredInviteFD(origFD)
				return inviteCleanupErr(err)
			}
			qname = cand
			claimed = true
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			_ = shredInviteFD(origFD)
			return inviteCleanupErr(err)
		}
	}
	if !claimed {
		_ = shredInviteFD(origFD)
		return inviteCleanupErr(syscall.EEXIST)
	}
	fi, err := root.Lstat(qname)
	if err != nil {
		_ = shredInviteFD(origFD)
		_ = c.restoreInviteName(root, qname, name)
		return inviteCleanupErr(err)
	}
	if !fi.Mode().IsRegular() {
		_ = shredInviteFD(origFD)
		_ = c.restoreInviteName(root, qname, name)
		return inviteCleanupErr(errInviteQuarantineNotRegular)
	}
	// os.Root does not honour O_NOFOLLOW (measured on Go 1.25: OpenFile
	// followed an in-root symlink that syscall.Open refused). Symlink
	// resolution cannot escape the root. The Lstat check above is the
	// explicit refusal; the dev/ino comparison immediately below is the
	// authority on identity. TestInviteQuarantineRefusesASymlinkedName is
	// what holds that claim up: restoring O_NOFOLLOW and dropping the
	// Lstat check still fails it.
	//
	// Lstat then OpenFile is two operations, so the entry can change in
	// between. The check narrows the window and makes the refusal explicit;
	// it does not close it, and the identity comparison is what actually
	// decides.
	qf, err := root.OpenFile(qname, os.O_RDWR, 0)
	if err != nil {
		_ = shredInviteFD(origFD)
		_ = c.restoreInviteName(root, qname, name)
		return inviteCleanupErr(err)
	}
	qfd := int(qf.Fd())
	var qst syscall.Stat_t
	if err := syscall.Fstat(qfd, &qst); err != nil {
		qf.Close()
		_ = shredInviteFD(origFD)
		if rerr := c.restoreInviteName(root, qname, name); rerr != nil {
			return inviteCleanupErr(err)
		}
		return inviteCleanupErr(err)
	}
	if qst.Dev != orig.Dev || qst.Ino != orig.Ino {
		qf.Close()
		if c.cfg.BeforeInviteRestore != nil {
			c.cfg.BeforeInviteRestore()
		}
		if err := c.restoreInviteName(root, qname, name); err != nil {
			_ = shredInviteFD(origFD)
			return inviteCleanupErr(err)
		}
		return shredInviteFD(origFD)
	}
	if err := shredInviteFD(qfd); err != nil {
		qf.Close()
		return inviteCleanupErr(err)
	}
	qf.Close()
	if err := c.inviteUnlinkFinal(root, qname); err != nil {
		return inviteCleanupErr(err)
	}
	return nil
}

func (c *Client) inviteLink(root *os.Root, oldname, newname string) error {
	if c.cfg.InviteLink != nil {
		return c.cfg.InviteLink(oldname, newname)
	}
	return root.Link(oldname, newname)
}

func (c *Client) inviteUnlinkFinal(root *os.Root, name string) error {
	if c.cfg.InviteUnlink != nil {
		return c.cfg.InviteUnlink(name)
	}
	return root.Remove(name)
}

func (c *Client) restoreInviteName(root *os.Root, qname, name string) error {
	if err := c.inviteLink(root, qname, name); err != nil {
		return err
	}
	return root.Remove(qname)
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
)

var errInviteQuarantineNotRegular = errors.New("quarantine name is not a regular file")
