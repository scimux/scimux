package app

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const (
	envMuxerPublicFD  = "SCIMUX_MUXER_PUBLIC_FD"
	envMuxerLockFD    = "SCIMUX_MUXER_LOCK_FD"
	envMuxerCSRFToken = "SCIMUX_MUXER_CSRF_TOKEN"
)

type muxerExecFiles struct {
	public    *os.File
	ownership *os.File
	csrfToken string
}

func (f *muxerExecFiles) Close() error {
	if f == nil {
		return nil
	}
	var errs []error
	if f.public != nil {
		errs = append(errs, f.public.Close())
		f.public = nil
	}
	if f.ownership != nil {
		errs = append(errs, f.ownership.Close())
		f.ownership = nil
	}
	return errors.Join(errs...)
}

func prepareMuxerExecFiles(cmd *Command) (*muxerExecFiles, error) {
	if cmd == nil || cmd.Listener() == nil || cmd.ownership == nil {
		return nil, errors.New("muxer handoff: runtime does not own listener and data directory")
	}
	if !validCSRFToken(cmd.csrfToken) {
		return nil, errors.New("muxer handoff: runtime has no valid CSRF token")
	}
	public, err := listenerFileForExec(cmd.Listener())
	if err != nil {
		return nil, err
	}
	ownership, err := cmd.ownership.FileForExec()
	if err != nil {
		_ = public.Close()
		return nil, err
	}
	return &muxerExecFiles{public: public, ownership: ownership, csrfToken: cmd.csrfToken}, nil
}

// loadMuxerExecFiles reads descriptor numbers plus the browser capability that
// must survive the exec. The lock file and TCP listener themselves are
// authenticated structurally when Command consumes them; no path or backend
// capability is accepted from the environment. A missing CSRF value is a
// compatible handoff from a pre-token-preservation muxer and is minted anew.
func loadMuxerExecFiles(getenv func(string) string) (*muxerExecFiles, error) {
	if getenv == nil {
		return nil, errors.New("muxer handoff: nil environment")
	}
	publicRaw, lockRaw := getenv(envMuxerPublicFD), getenv(envMuxerLockFD)
	csrf := getenv(envMuxerCSRFToken)
	if publicRaw == "" && lockRaw == "" {
		if csrf != "" {
			return nil, errors.New("muxer handoff: CSRF token without inherited descriptors")
		}
		return nil, nil
	}
	if publicRaw == "" || lockRaw == "" {
		return nil, errors.New("muxer handoff: incomplete inherited descriptors")
	}
	publicFD, publicErr := strconv.Atoi(publicRaw)
	lockFD, lockErr := strconv.Atoi(lockRaw)
	if publicErr != nil || lockErr != nil || publicFD < 3 || lockFD < 3 || publicFD == lockFD {
		return nil, errors.New("muxer handoff: invalid inherited descriptors")
	}
	if csrf != "" && !validCSRFToken(csrf) {
		return nil, errors.New("muxer handoff: invalid inherited CSRF token")
	}
	return &muxerExecFiles{
		public:    os.NewFile(uintptr(publicFD), "scimux-inherited-public-listener"),
		ownership: os.NewFile(uintptr(lockFD), "scimux-inherited-muxer-ownership"),
		csrfToken: csrf,
	}, nil
}

var muxerExec = syscall.Exec

func descriptorSetCloseOnExec(fd int, enabled bool) error {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		return errno
	}
	if enabled {
		flags |= syscall.FD_CLOEXEC
	} else {
		flags &^= syscall.FD_CLOEXEC
	}
	_, _, errno = syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_SETFD), flags)
	if errno != 0 {
		return errno
	}
	return nil
}

func muxerExecEnvironment(publicFD, lockFD int, csrf string) []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, envMuxerPublicFD+"=") || strings.HasPrefix(item, envMuxerLockFD+"=") ||
			strings.HasPrefix(item, envMuxerCSRFToken+"=") {
			continue
		}
		env = append(env, item)
	}
	return append(env,
		envMuxerPublicFD+"="+strconv.Itoa(publicFD),
		envMuxerLockFD+"="+strconv.Itoa(lockFD),
		envMuxerCSRFToken+"="+csrf,
	)
}

// replaceMuxerProcess keeps the listener and lifetime lock inheritable only
// while no Go goroutine can fork. On failure both descriptors become
// close-on-exec again before subprocess creation is unblocked.
func replaceMuxerProcess(path string, files *muxerExecFiles) error {
	if path == "" || files == nil || files.public == nil || files.ownership == nil {
		return errors.New("muxer handoff: incomplete replacement")
	}
	if !validCSRFToken(files.csrfToken) {
		return errors.New("muxer handoff: invalid CSRF token")
	}
	publicFD, lockFD := int(files.public.Fd()), int(files.ownership.Fd())
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()
	if err := descriptorSetCloseOnExec(publicFD, false); err != nil {
		return fmt.Errorf("muxer handoff: expose public listener: %w", err)
	}
	if err := descriptorSetCloseOnExec(lockFD, false); err != nil {
		_ = descriptorSetCloseOnExec(publicFD, true)
		return fmt.Errorf("muxer handoff: expose ownership lock: %w", err)
	}
	err := muxerExec(path, os.Args, muxerExecEnvironment(publicFD, lockFD, files.csrfToken))
	_ = descriptorSetCloseOnExec(publicFD, true)
	_ = descriptorSetCloseOnExec(lockFD, true)
	return err
}
