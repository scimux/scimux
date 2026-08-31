package app

// Phase 4 (F-12) — .gitignore's markdown block is deny-by-default (`*.md`
// plus `!` re-inclusions), so its allow-list is the repository's statement of
// which documents are published. A `!` line naming a file that does not exist
// is not inert: it reads as a promise the repo keeps a document it does not.
// `!docs/fare-v2-p1-grounding.md` sat in that block naming a file that was
// never tracked at all, and nothing could notice, because an unmatched
// re-inclusion changes no behaviour.
//
// This guard covers only that direction — a listed file that is missing —
// because it is the one answerable from the filesystem alone. The opposite
// direction (a tracked .md the list forgets, which is how the two rendezvous
// docs came to be tracked but unlisted) needs the index, and a test that
// shells out to git would fail wherever the suite runs outside a checkout.
// The .gitignore comment names `git ls-files '*.md'` as that check.

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitignoreMarkdownAllowlistNamesRealFiles(t *testing.T) {
	root := moduleRoot(t)
	f, err := os.Open(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("open .gitignore: %v", err)
	}
	defer f.Close()

	var checked int
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		entry := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(entry, "!") {
			continue
		}
		rel := strings.TrimPrefix(entry, "!")
		// Only the literal paths are answerable here; a pattern re-inclusion
		// would need glob semantics this guard deliberately does not model.
		if strings.ContainsAny(rel, "*?[") {
			continue
		}
		checked++
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf(".gitignore:%d re-includes %q, which does not exist.\n"+
				"An allow-line for a missing file is a published-docs promise the "+
				"repository does not keep. Delete the line, or add the document.",
				line, rel)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}

	// Anti-vacuity: the block exists and is the reason this test does. If a
	// refactor drops every `!` line, finding nothing must fail, not pass.
	if checked == 0 {
		t.Fatal(".gitignore has no literal `!` re-inclusions; either the deny-by-default " +
			"markdown block was removed (in which case this guard needs rewriting, not " +
			"deleting) or the file was not found where moduleRoot pointed")
	}
}
