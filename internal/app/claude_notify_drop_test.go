package app

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDropClaudeNotifyInbox covers dropClaudeNotifyInbox cleanup: safe-name
// quarantine into processed/rejected, confined to the selected hook bundle.
// Contents are intentionally not parsed — cleanup keys on ready-file names.
func TestDropClaudeNotifyInbox(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{ID: "n1", Title: "n1", Agent: "claude"}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	hookID := filepath.Base(installPreparedClaudeHook(t, a, n))
	bundle := filepath.Join(a.claudeHooksDir(), hookID)
	notifyDir := filepath.Join(bundle, "notify")

	writeNotify := func(name string, body []byte) string {
		t.Helper()
		p := filepath.Join(notifyDir, name)
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mustExist := func(path string) {
		t.Helper()
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s to exist: %v", path, err)
		}
	}
	mustGone := func(path string) {
		t.Helper()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s gone, err=%v", path, err)
		}
	}

	t.Run("unsafe hook id touches nothing", func(t *testing.T) {
		sentinel := writeNotify("keep-unsafe-hook.json", []byte(`{}`))
		a.dropClaudeNotifyInbox("../" + hookID)
		a.dropClaudeNotifyInbox(hookID + "/../" + hookID)
		a.dropClaudeNotifyInbox("")
		mustExist(sentinel)
		_ = os.Remove(sentinel)
	})

	t.Run("missing notify directory is a no-op", func(t *testing.T) {
		other := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		missing := filepath.Join(a.claudeHooksDir(), other)
		if err := os.MkdirAll(missing, 0o700); err != nil {
			t.Fatal(err)
		}
		// No notify/ under other — must not create one or panic.
		a.dropClaudeNotifyInbox(other)
		if _, err := os.Stat(filepath.Join(missing, "notify")); !os.IsNotExist(err) {
			t.Fatalf("drop invented notify dir: %v", err)
		}
	})

	t.Run("directories and non-json and unsafe names are ignored", func(t *testing.T) {
		subdir := filepath.Join(notifyDir, "subdir")
		if err := os.MkdirAll(subdir, 0o700); err != nil {
			t.Fatal(err)
		}
		// A directory whose name looks like a ready file must still be skipped
		// (IsDir), otherwise cleanup would rename the directory itself.
		jsonDir := filepath.Join(notifyDir, "trap.json")
		if err := os.MkdirAll(jsonDir, 0o700); err != nil {
			t.Fatal(err)
		}
		plain := writeNotify("notes.txt", []byte("not json"))
		unsafe := writeNotify("..bad.json", []byte(`{}`))
		// Also plant a name with slash-shaped rejection via safePathComponent
		// (".." alone is rejected); keep a valid one to prove selective drop.
		valid := writeNotify("ready-a.json", []byte(`not-parsed`))
		a.dropClaudeNotifyInbox(hookID)
		mustExist(subdir)
		mustExist(jsonDir)
		mustExist(plain)
		mustExist(unsafe)
		mustGone(valid)
		rejected := filepath.Join(bundle, "processed", "rejected", "ready-a.json")
		mustExist(rejected)
		if _, err := os.Stat(filepath.Join(bundle, "processed", "rejected", "trap.json")); !os.IsNotExist(err) {
			t.Fatal("directory trap.json was quarantined; IsDir must skip it")
		}
		_ = os.RemoveAll(subdir)
		_ = os.RemoveAll(jsonDir)
		_ = os.Remove(plain)
		_ = os.Remove(unsafe)
		_ = os.Remove(rejected)
	})

	t.Run("multiple valid entries are all quarantined", func(t *testing.T) {
		a1 := writeNotify("one.json", []byte(`1`))
		a2 := writeNotify("two.json", []byte(`2`))
		a.dropClaudeNotifyInbox(hookID)
		mustGone(a1)
		mustGone(a2)
		mustExist(filepath.Join(bundle, "processed", "rejected", "one.json"))
		mustExist(filepath.Join(bundle, "processed", "rejected", "two.json"))
	})

	t.Run("quarantine destination failure leaves source in place", func(t *testing.T) {
		src := writeNotify("stuck.json", []byte(`{}`))
		// Make processed a regular file so MkdirAll(processed/rejected) fails.
		processed := filepath.Join(bundle, "processed")
		_ = os.RemoveAll(processed)
		if err := os.WriteFile(processed, []byte("not-a-dir"), 0o600); err != nil {
			t.Fatal(err)
		}
		a.dropClaudeNotifyInbox(hookID)
		mustExist(src)
		_ = os.Remove(processed)
		_ = os.MkdirAll(processed, 0o700)
		_ = os.Remove(src)
	})

	t.Run("sentinel outside selected bundle unchanged", func(t *testing.T) {
		otherID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		otherBundle := filepath.Join(a.claudeHooksDir(), otherID)
		otherNotify := filepath.Join(otherBundle, "notify")
		if err := os.MkdirAll(otherNotify, 0o700); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(otherNotify, "foreign.json")
		if err := os.WriteFile(sentinel, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		mine := writeNotify("mine.json", []byte(`{}`))
		a.dropClaudeNotifyInbox(hookID)
		mustGone(mine)
		mustExist(sentinel)
		mustExist(filepath.Join(bundle, "processed", "rejected", "mine.json"))
	})
}
