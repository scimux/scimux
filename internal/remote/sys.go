package remote

import (
	"os"
	"syscall"
)

type osSys struct{}

func (osSys) EffectiveUID() int { return os.Geteuid() }

func (osSys) Lstat(path string) (FileStat, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return FileStat{}, err
	}
	st := FileStat{
		Mode:    info.Mode().Perm(),
		Regular: info.Mode().IsRegular(),
	}
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		st.UID = int(sys.Uid)
	}
	return st, nil
}

func (osSys) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (c *Client) sys() Sys {
	if c != nil && c.cfg.Sys != nil {
		return c.cfg.Sys
	}
	return osSys{}
}
