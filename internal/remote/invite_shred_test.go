package remote

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestEraseInviteReportsShredFailure pins that a shred failure is
// returned in preference to every later error, including a nil that
// would report consume as success while the invite is still readable.
//
// eraseInvite checks shredErr at nine return sites. Two of them —
// syscall.Fstat of origFD and syscall.Fstat of pathfd — cannot fail:
// both descriptors are held open by this function. Those arms are
// omitted on purpose; do not add a seam to reach them.

func TestEraseInviteReportsShredFailure(t *testing.T) {
	rows := []struct {
		name  string
		src   func(path string) inviteSrc
		cfg   func(t *testing.T, path string) Config
		setup func(t *testing.T, path string)
	}{
		{
			name: "stdin invite",
			src:  func(string) inviteSrc { return inviteSrc{} },
		},
		{
			name: "Sys seam active",
			cfg:  func(*testing.T, string) Config { return Config{Sys: newFakeSys(0)} },
		},
		{
			name: "parent dir gone",
			setup: func(t *testing.T, path string) {
				dir := filepath.Dir(path)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "invite gone",
			setup: func(t *testing.T, path string) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "inode swapped",
			setup: func(t *testing.T, path string) {
				replaceInviteInode(t, path, []byte("swapped-inode\n"))
			},
		},
		{
			name: "swapped late",
			cfg: func(t *testing.T, path string) Config {
				return Config{BeforeInviteUnlink: func() {
					replaceInviteInode(t, path, []byte("late-swap\n"))
				}}
			},
		},
		{
			name: "removed late",
			cfg: func(t *testing.T, path string) Config {
				return Config{BeforeInviteUnlink: func() {
					if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
						t.Errorf("late remove: %v", err)
					}
				}}
			},
		},
		{
			name: "clean path",
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeInviteFile(t, dir, "invite", 0o600)
			fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			cfg := Config{}
			if row.cfg != nil {
				cfg = row.cfg(t, path)
			}
			c := NewClient(cfg)
			c.inviteFD = fd
			if row.setup != nil {
				row.setup(t, path)
			}
			src := inviteSrc{file: path}
			if row.src != nil {
				src = row.src(path)
			}
			err = c.eraseInvite(src, "")
			requireClass(t, err, ClassInviteFile)
			if !errors.Is(err, syscall.EBADF) {
				t.Fatalf("error does not unwrap to shred failure (EBADF): %v", err)
			}
		})
	}
}

func replaceInviteInode(t *testing.T, path string, body []byte) {
	t.Helper()
	tmp := filepath.Join(filepath.Dir(path), "replacement")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}
