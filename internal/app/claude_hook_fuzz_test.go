package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
	// Plant a file in every location a helper must not touch, so the perimeter
	// has something to protect. Without these, "does not disturb existing
	// out-of-lane files" would hold vacuously: a fresh bundle has none, and a
	// hook that overwrote perm/lease (the auto-approve arm marker) or
	// capabilities.json would pass a create-only check.
	for rel, body := range plantedFiles() {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0o600); err != nil {
			f.Fatal(err)
		}
	}
	pristine := snapshotTree(f, root)

	f.Add([]byte(`{"hook_event_name":"SessionStart","source":"startup","session_id":"s1","transcript_path":"/tmp/x.jsonl"}`))
	f.Add([]byte(`{"hook_event_name":"Stop","session_id":"s1"}`))
	f.Add([]byte(`{"hook_event_name":"Notification","notification_type":"permission_prompt","session_id":"s1","title":"Permission Required"}`))
	f.Add([]byte(`{"hook_event_name":"PreCompact","session_id":"s1","trigger":"auto"}`))
	f.Add([]byte(`{"hook_event_name":"PostCompact","session_id":"s1"}`))
	f.Add([]byte(`{"hook_event_name":"Elicitation","session_id":"s1","mcp_server_name":"srv","message":"confirm"}`))
	f.Add([]byte(`{"hook_event_name":"ElicitationResult","session_id":"s1"}`))
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
			var stdout bytes.Buffer
			err := h.fn(bundle, bytes.NewReader(stdin), &stdout, io.Discard)
			if stdout.Len() != 0 {
				t.Fatalf("%s wrote %d bytes to stdout: %q", h.name, stdout.Len(), stdout.Bytes())
			}
			if err != nil && !errors.Is(err, errClaudeHookRejected) {
				t.Fatalf("%s leaked error %v", h.name, err)
			}
			// The property is that nothing outside the helper's own
			// subdirectory changed — not merely that nothing new appeared
			// there. Comparing presence, contents, and mode against the
			// pristine tree catches an overwrite or a deletion too.
			lane := filepath.Join("hookdir", h.sub)
			after := snapshotTree(t, root)
			for _, rel := range diffOutside(pristine, after, lane) {
				t.Fatalf("%s disturbed %q outside %s/ (was %v, now %v)",
					h.name, rel, lane, pristine[rel], after[rel])
			}
			// Restore the bundle so every iteration starts from the same tree.
			// Without this the lane accumulates one file per accepted input, so
			// a long fuzz session would both slow down quadratically and turn
			// each helper's own past writes into "existing" paths.
			restoreTree(t, root, pristine, after)
		}
	})
}

// treeEntry is the part of a directory entry the perimeter check compares.
type treeEntry struct {
	dir  bool
	mode fs.FileMode
	sum  string // sha256 of contents; empty for a directory
}

func snapshotTree(t testing.TB, root string) map[string]treeEntry {
	t.Helper()
	out := map[string]treeEntry{}
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
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := treeEntry{dir: d.IsDir(), mode: info.Mode()}
		if !e.dir {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			h := sha256.Sum256(b)
			e.sum = hex.EncodeToString(h[:8])
		}
		out[rel] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// diffOutside returns every path that differs between the two trees and does
// not lie inside lane, sorted so a failure names the same path every run.
func diffOutside(before, after map[string]treeEntry, lane string) []string {
	inLane := func(rel string) bool {
		return rel == lane || strings.HasPrefix(rel, lane+string(os.PathSeparator))
	}
	var bad []string
	for rel, b := range before {
		if a, ok := after[rel]; !inLane(rel) && (!ok || a != b) {
			bad = append(bad, rel)
		}
	}
	for rel := range after {
		if _, ok := before[rel]; !ok && !inLane(rel) {
			bad = append(bad, rel)
		}
	}
	sort.Strings(bad)
	return bad
}

// restoreTree puts root back into its pristine shape: extra files are removed,
// and pristine files the helper deleted or rewrote are written again.
func restoreTree(t testing.TB, root string, pristine, after map[string]treeEntry) {
	t.Helper()
	for rel, a := range after {
		if _, ok := pristine[rel]; !ok && !a.dir {
			if err := os.Remove(filepath.Join(root, rel)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for rel, p := range pristine {
		if p.dir {
			continue
		}
		if a, ok := after[rel]; ok && a == p {
			continue
		}
		body, ok := plantedFiles()[filepath.ToSlash(rel)]
		if !ok {
			t.Fatalf("pristine file %q is not in the planted corpus", rel)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// plantedFiles is the one definition of the protected corpus: setup writes it,
// restoreTree puts it back. Paths are slash-separated and joined on use.
func plantedFiles() map[string]string {
	return map[string]string{
		"outside.txt":                             "not part of any bundle",
		"hookdir/settings.json":                   `{"hooks":{}}`,
		"hookdir/capabilities.json":               `{"exec":"/nonexistent"}`,
		"hookdir/inbox/planted.json":              `{"planted":"inbox"}`,
		"hookdir/stop/planted.json":               `{"planted":"stop"}`,
		"hookdir/notify/planted.json":             `{"planted":"notify"}`,
		"hookdir/compact/active.json":             `{"planted":"compact"}`,
		"hookdir/elicitation/active/planted.json": `{"planted":"elicitation"}`,
		"hookdir/perm/lease":                      "planted-lease",
	}
}
