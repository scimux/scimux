package app

import (
	"errors"
	"os"
	"testing"
)

var errInjectedHookOp = errors.New("injected hook bundle failure")

func TestClaudeHookBundleRollsBackOnFailure(t *testing.T) {
	rows := []struct {
		name string
		ops  func() hookOps
	}{
		{"root MkdirAll", func() hookOps { return failNthMkdirAll(1) }},
		{"root Chmod", func() hookOps { return failNthChmod(1) }},
	}
	for i, sub := range claudeHookBundleSubdirs {
		sub, i := sub, i
		rows = append(rows,
			struct {
				name string
				ops  func() hookOps
			}{"subdir MkdirAll " + sub, func() hookOps { return failNthMkdirAll(i + 2) }},
			struct {
				name string
				ops  func() hookOps
			}{"subdir Chmod " + sub, func() hookOps { return failNthChmod(i + 2) }},
		)
	}
	rows = append(rows,
		struct {
			name string
			ops  func() hookOps
		}{"os.Executable", func() hookOps {
			ops := defaultHookOps()
			ops.Executable = func() (string, error) { return "", errInjectedHookOp }
			return ops
		}},
		// Empty exec path makes claudeHookSettingsJSON reject; this is not
		// a marshal failure. The json.Marshal(caps) rollback is unreachable
		// — a struct of ints and a string does not fail to marshal — and no
		// seam was added for it deliberately.
		struct {
			name string
			ops  func() hookOps
		}{"empty exec path", func() hookOps {
			ops := defaultHookOps()
			ops.Executable = func() (string, error) { return "", nil }
			return ops
		}},
		struct {
			name string
			ops  func() hookOps
		}{"capabilities.json write", func() hookOps {
			ops := defaultHookOps()
			ops.WriteFile = func(string, []byte, os.FileMode) error { return errInjectedHookOp }
			return ops
		}},
		struct {
			name string
			ops  func() hookOps
		}{"settings.json O_EXCL open", func() hookOps {
			ops := defaultHookOps()
			ops.OpenFile = func(string, int, os.FileMode) (hookFile, error) {
				return nil, errInjectedHookOp
			}
			return ops
		}},
		struct {
			name string
			ops  func() hookOps
		}{"settings.json write", func() hookOps { return failSettingsFile(errInjectedHookOp, nil, nil) }},
		struct {
			name string
			ops  func() hookOps
		}{"settings.json chmod", func() hookOps { return failSettingsFile(nil, errInjectedHookOp, nil) }},
		struct {
			name string
			ops  func() hookOps
		}{"settings.json close", func() hookOps { return failSettingsFile(nil, nil, errInjectedHookOp) }},
	)

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			a := newTestApp(t, &fakeTmux{})
			notes := 0
			ops := row.ops()
			ops.Note = func(string) { notes++ }
			_, _, err := a.prepareClaudeHookBundleWith(ops, "n1")
			if err == nil {
				t.Fatal("prepare succeeded; injected failure was ignored")
			}
			root := a.claudeHooksDir()
			ents, rerr := os.ReadDir(root)
			if rerr != nil && !os.IsNotExist(rerr) {
				t.Fatalf("read hook root: %v", rerr)
			}
			if len(ents) > 0 {
				t.Fatalf("bundle directory remains after failure: %s", ents[0].Name())
			}
			if notes != 0 {
				t.Fatalf("noteClaudeStrictCapability called %d times after a failed prepare", notes)
			}
			if claudeCapabilityRegistered(a) {
				t.Fatal("noteClaudeStrictCapability registered capability after a failed prepare")
			}
		})
	}
}

func failNthMkdirAll(n int) hookOps {
	ops := defaultHookOps()
	i := 0
	orig := ops.MkdirAll
	ops.MkdirAll = func(path string, perm os.FileMode) error {
		i++
		if i == n {
			return errInjectedHookOp
		}
		return orig(path, perm)
	}
	return ops
}

func failNthChmod(n int) hookOps {
	ops := defaultHookOps()
	i := 0
	orig := ops.Chmod
	ops.Chmod = func(name string, mode os.FileMode) error {
		i++
		if i == n {
			return errInjectedHookOp
		}
		return orig(name, mode)
	}
	return ops
}

func failSettingsFile(writeErr, chmodErr, closeErr error) hookOps {
	ops := defaultHookOps()
	orig := ops.OpenFile
	ops.OpenFile = func(name string, flag int, perm os.FileMode) (hookFile, error) {
		f, err := orig(name, flag, perm)
		if err != nil {
			return nil, err
		}
		return &hookFileFaults{hookFile: f, writeErr: writeErr, chmodErr: chmodErr, closeErr: closeErr}, nil
	}
	return ops
}

type hookFileFaults struct {
	hookFile
	writeErr error
	chmodErr error
	closeErr error
}

func (f *hookFileFaults) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.hookFile.Write(p)
}

func (f *hookFileFaults) Chmod(mode os.FileMode) error {
	if f.chmodErr != nil {
		return f.chmodErr
	}
	return f.hookFile.Chmod(mode)
}

func (f *hookFileFaults) Close() error {
	err := f.hookFile.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

func claudeCapabilityRegistered(a *app) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, v := range a.claudeStrictCap {
		if v {
			return true
		}
	}
	return false
}
