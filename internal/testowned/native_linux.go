//go:build linux

package testowned

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type nativeOps struct {
	list  func() ([]int, error)
	wait4 func(int) (int, error)
	kill  func(int, int) error
}
type nativeKernel struct{ ops nativeOps }

func (n *nativeKernel) children() ([]int, error) {
	pids, err := n.ops.list()
	if err != nil {
		return nil, err
	}
	if len(pids) > 0 {
		return pids, nil
	}
	_, err = n.ops.wait4(-1)
	if errors.Is(err, syscall.ECHILD) {
		return nil, nil
	}
	return nil, errors.New("child state unknown")
}
func (n *nativeKernel) wait(pid int) (bool, error) {
	got, err := n.ops.wait4(pid)
	if err != nil {
		return false, err
	}
	if got == 0 {
		return false, nil
	}
	if got != pid {
		return false, errors.New("unexpected child")
	}
	return true, nil
}
func (n *nativeKernel) signal(pid, sig int) error { return n.ops.kill(pid, sig) }

func listProcChildren(root string, parent int) ([]int, error) {
	return listProcChildrenRead(root, parent, os.ReadFile)
}

func listProcChildrenRead(root string, parent int, read func(string) ([]byte, error)) ([]int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var result []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := read(filepath.Join(root, entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		end := strings.LastIndex(string(raw), ") ")
		if end < 0 {
			return nil, errors.New("invalid process status")
		}
		fields := strings.Fields(string(raw[end+2:]))
		if len(fields) < 2 {
			return nil, errors.New("invalid process status")
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, errors.New("invalid process parent")
		}
		if ppid == parent {
			result = append(result, pid)
		}
	}
	return result, nil
}
