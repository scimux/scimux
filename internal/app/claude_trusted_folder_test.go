package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTrustedProjects(t *testing.T, home string, projects map[string]any) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"projects": projects})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeTrustedFolderPicksLexicallyFirst(t *testing.T) {
	home := t.TempDir()
	projects := map[string]any{}
	for _, name := range []string{"zeta", "alpha", "middle"} {
		dir := filepath.Join(home, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		projects[dir] = map[string]bool{"hasTrustDialogAccepted": true}
	}
	writeTrustedProjects(t, home, projects)
	if got, want := claudeTrustedFolder(home), filepath.Join(home, "alpha"); got != want {
		t.Fatalf("folder = %q, want %q", got, want)
	}
}

func TestClaudeTrustedFolderSkipsInvalidEntries(t *testing.T) {
	home := t.TempDir()
	untrusted := filepath.Join(home, "untrusted")
	missingFlag := filepath.Join(home, "missing-flag")
	valid := filepath.Join(home, "valid")
	for _, dir := range []string{untrusted, missingFlag, valid} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(home, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	writeTrustedProjects(t, home, map[string]any{
		untrusted:                          map[string]bool{"hasTrustDialogAccepted": false},
		missingFlag:                        map[string]bool{},
		filepath.Join(home, "nonexistent"): map[string]bool{"hasTrustDialogAccepted": true},
		file:                               map[string]bool{"hasTrustDialogAccepted": true},
		"relative":                         map[string]bool{"hasTrustDialogAccepted": true},
		valid + "/../valid":                map[string]bool{"hasTrustDialogAccepted": true},
		valid:                              map[string]bool{"hasTrustDialogAccepted": true},
	})
	if got := claudeTrustedFolder(home); got != valid {
		t.Fatalf("folder = %q, want %q", got, valid)
	}
	// None of the rejected entries may stand on its own.
	for _, path := range []string{untrusted, missingFlag, filepath.Join(home, "nonexistent"), file, "relative", valid + "/../valid"} {
		flag := path != untrusted && path != missingFlag
		writeTrustedProjects(t, home, map[string]any{path: map[string]bool{"hasTrustDialogAccepted": flag}})
		if got := claudeTrustedFolder(home); got != "" {
			t.Fatalf("accepted %q: %q", path, got)
		}
	}
}

func TestClaudeTrustedFolderDegradesOnErrors(t *testing.T) {
	for _, name := range []string{"missing", "malformed", "projects-array", "oversized", "directory", "wrong-flag"} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".claude.json")
			var err error
			switch name {
			case "malformed":
				err = os.WriteFile(path, []byte("{"), 0600)
			case "projects-array":
				err = os.WriteFile(path, []byte(`{"projects":[]}`), 0600)
			case "wrong-flag":
				err = os.WriteFile(path, []byte(`{"projects":{"relative":{"hasTrustDialogAccepted":"yes"}}}`), 0600)
			case "oversized":
				var f *os.File
				f, err = os.Create(path)
				if err == nil {
					err = f.Truncate((32 << 20) + 1)
					f.Close()
				}
			case "directory":
				err = os.Mkdir(path, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := claudeTrustedFolder(home); got != "" {
				t.Fatalf("folder = %q, want empty", got)
			}
		})
	}
}

func TestClaudeTrustedFolderLeavesTrustStateUnchanged(t *testing.T) {
	home := t.TempDir()
	writeTrustedProjects(t, home, map[string]any{home: map[string]bool{"hasTrustDialogAccepted": true}})
	path := filepath.Join(home, ".claude.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := claudeTrustedFolder(home); got != home {
		t.Fatalf("folder = %q", got)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !st.ModTime().Equal(updated.ModTime()) {
		t.Fatal("lookup changed trust state bytes or mtime")
	}
}
