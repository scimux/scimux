package app

import (
	"os"
	"path/filepath"
	"testing"
)

// perm/shown/ was a per-request dialog-visibility marker directory. Nothing
// ever wrote a marker into it and nothing ever read one, so it was removed.
// These assertions keep it removed: a bundle must neither create it nor
// require it, or a future launch would resurrect a directory that only ever
// held the absence of data.
func TestBundleCompleteCurrentIgnoresShown(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedOwnedClaude(t, a, "n1", hookSIDOwn, "")
	hookID, _, err := a.prepareClaudeHookBundle(n.ID)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	bundle := filepath.Join(a.claudeHooksDir(), hookID)
	if !bundleCompleteCurrent(bundle) {
		t.Fatal("freshly prepared current bundle must be complete")
	}

	shown := filepath.Join(bundle, "perm", "shown")
	if _, err := os.Stat(shown); !os.IsNotExist(err) {
		t.Fatalf("prepareClaudeHookBundle must no longer create perm/shown (stat err = %v)", err)
	}

	// Belt and braces: an adopted bundle from an older scimux still has the
	// directory on disk. Its presence must not matter either way.
	if err := os.MkdirAll(shown, 0o700); err != nil {
		t.Fatal(err)
	}
	if !bundleCompleteCurrent(bundle) {
		t.Fatal("a leftover perm/shown from an older bundle must not affect completeness")
	}
	if err := os.RemoveAll(shown); err != nil {
		t.Fatal(err)
	}
	if !bundleCompleteCurrent(bundle) {
		t.Fatal("perm/shown is removed; its absence must not make a bundle incomplete")
	}
}
