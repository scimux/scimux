package app

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzClaudeHookStdin(f *testing.F) {
	root := f.TempDir()
	bundle := filepath.Join(root, "hookdir")
	for _, sub := range []string{
		"inbox", "stop", "notify", "compact",
		filepath.Join("elicitation", "active"), "perm",
	} {
		if err := os.MkdirAll(filepath.Join(bundle, sub), 0o700); err != nil {
			f.Fatal(err)
		}
	}

	f.Add([]byte(`{"hook_event_name":"SessionStart","source":"startup","session_id":"s1","transcript_path":"/tmp/x.jsonl"}`))
	f.Add([]byte(`{"hook_event_name":"Stop","session_id":"s1"}`))
	f.Add([]byte(`{"hook_event_name":"Notification","notification_type":"permission_prompt","session_id":"s1","title":"Permission Required"}`))
	f.Add([]byte(`{"hook_event_name":"PreCompact","session_id":"s1","trigger":"auto"}`))
	f.Add([]byte(`{"hook_event_name":"Elicitation","session_id":"s1","mcp_server_name":"srv","message":"confirm"}`))
	f.Add([]byte(`{"hook_event_name":"SessionEnd","source":"startup","session_id":"s1"}`))
	f.Add([]byte(`{"hook_event_name":"Stop"}`))
	f.Add([]byte(`{"hook_event_name":"Stop","session_id":"s1","stop_hook_active":true}`))
	f.Add([]byte(``))
	f.Add([]byte(`this is not json`))
	f.Add([]byte(`["not","an","object"]`))
	over := bytes.Repeat([]byte(" "), claudeHookStdinLimit+1)
	copy(over, `{"hook_event_name":"Stop","session_id":"s1"}`)
	f.Add(over)

	helpers := []struct {
		name string
		fn   func(string, io.Reader, io.Writer, io.Writer) error
		sub  string
	}{
		{"session", RunClaudeSessionHook, "inbox"},
		{"stop", RunClaudeStopHook, "stop"},
		{"notify", RunClaudeNotifyHook, "notify"},
		{"compact", RunClaudeCompactHook, "compact"},
		{"elicitation", RunClaudeElicitationHook, "elicitation"},
	}

	f.Fuzz(func(t *testing.T, stdin []byte) {
		for _, h := range helpers {
			beforeFiles, beforeDirs := listRel(t, root)
			var stdout bytes.Buffer
			err := h.fn(bundle, bytes.NewReader(stdin), &stdout, io.Discard)
			if stdout.Len() != 0 {
				t.Fatalf("%s wrote %d bytes to stdout: %q", h.name, stdout.Len(), stdout.Bytes())
			}
			if err != nil && !errors.Is(err, errClaudeHookRejected) {
				t.Fatalf("%s leaked error %v", h.name, err)
			}
			afterFiles, afterDirs := listRel(t, root)
			if extra := extraPaths(beforeDirs, afterDirs); len(extra) != 0 {
				t.Fatalf("%s created directories %v", h.name, extra)
			}
			prefix := filepath.Join("hookdir", h.sub)
			for _, p := range extraPaths(beforeFiles, afterFiles) {
				if p != prefix && !strings.HasPrefix(p, prefix+string(os.PathSeparator)) {
					t.Fatalf("%s wrote %q outside %s/", h.name, p, prefix)
				}
			}
		}
	})
}

func listRel(t *testing.T, root string) (files, dirs []string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, rel)
		} else {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files, dirs
}

func extraPaths(before, after []string) []string {
	seen := make(map[string]struct{}, len(before))
	for _, p := range before {
		seen[p] = struct{}{}
	}
	var extra []string
	for _, p := range after {
		if _, ok := seen[p]; !ok {
			extra = append(extra, p)
		}
	}
	return extra
}
