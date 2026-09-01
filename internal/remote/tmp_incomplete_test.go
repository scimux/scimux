package remote

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTmpIncomplete documents that tmpIncomplete detects only an unreadable or
// malformed temporary file. Semantic state validation is separate
// (validatePersistedSemantics); a syntactically valid but incomplete enrolled
// JSON object is therefore not incomplete here.
func TestTmpIncomplete(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.json")
	if tmpIncomplete(missing) {
		t.Fatal("missing path must return false")
	}

	malformed := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(malformed, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !tmpIncomplete(malformed) {
		t.Fatal("malformed JSON must return true")
	}

	subdir := filepath.Join(dir, "as-dir")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !tmpIncomplete(subdir) {
		t.Fatal("directory path must return true (unreadable as JSON file)")
	}

	valid := filepath.Join(dir, "ok.json")
	if err := os.WriteFile(valid, []byte(`{"status":"enrolled"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if tmpIncomplete(valid) {
		t.Fatal("syntactically valid JSON must return false even when semantically incomplete")
	}
}
