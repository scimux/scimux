package team

import (
	"fmt"
	"io"
	"path/filepath"
)

func publishFile(dir, name, what string, body []byte) error {
	f, err := storeOps.createTemp(dir)
	if err != nil {
		return fmt.Errorf("team: write %s: %w", what, err)
	}
	tmpName := f.Name()
	closed := false
	defer func() {
		if !closed {
			_ = storeOps.close(f)
		}
		if tmpName != "" {
			_ = storeOps.remove(tmpName)
		}
	}()
	if err := storeOps.chmod(f, 0o600); err != nil {
		return fmt.Errorf("team: write %s: %w", what, err)
	}
	n, err := storeOps.write(f, body)
	if err != nil {
		return fmt.Errorf("team: write %s: %w", what, err)
	}
	if n != len(body) {
		return fmt.Errorf("team: write %s: %w", what, io.ErrShortWrite)
	}
	if err := storeOps.syncFile(f); err != nil {
		return fmt.Errorf("team: write %s: %w", what, err)
	}
	if err := storeOps.close(f); err != nil {
		return fmt.Errorf("team: write %s: %w", what, err)
	}
	closed = true
	if err := storeOps.rename(tmpName, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("team: write %s: %w", what, err)
	}
	tmpName = ""
	return nil
}
