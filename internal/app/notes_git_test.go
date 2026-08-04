package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/notestore"
)

// gitNoteTests shells out to git (like tmux integration tests). Skipped under
// -short and when git is not on PATH.
func requireGit(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("git note versioning test skipped with -short")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
}

func noteGitDir(a *app, id string) string {
	return filepath.Join(a.notes.Dir, id)
}

func gitCommitCount(t *testing.T, noteDir string) int {
	t.Helper()
	if _, err := os.Stat(filepath.Join(noteDir, ".git")); err != nil {
		return 0
	}
	out, err := exec.Command("git", "-C", noteDir, "rev-list", "--count", "HEAD").CombinedOutput()
	if err != nil {
		// Empty repo (init but no commits yet) or other benign state.
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("rev-list count %q: %v", out, err)
	}
	return n
}

// Structural PATCHes and reference add/remove each produce exactly one commit
// in the note's own repo; mid-edit body autosaves do not; a completing body
// PATCH with "commit":true does. Lazy init — no .git until the first commit.
func TestNoteGitCommitBoundaries(t *testing.T) {
	requireGit(t)
	a := newTestApp(t, &fakeTmux{})
	sh := createNote(t, a)
	dir := noteGitDir(a, sh.ID)
	sid := sh.Sections[0].ID

	if gitCommitCount(t, dir) != 0 {
		t.Fatal("create must not commit; lazy init on first boundary")
	}

	// Title change (structural) → exactly one commit.
	if rec := patchNote(a, sh.ID, `{"title":"Versioned Title"}`); rec.Code != 200 {
		t.Fatalf("title patch: %d %s", rec.Code, rec.Body.String())
	}
	if n := gitCommitCount(t, dir); n != 1 {
		t.Fatalf("after title change: want 1 commit, got %d", n)
	}

	// Mid-edit body autosave (no commit marker) → no new commit.
	if rec := patchNote(a, sh.ID, `{"section":{"id":"`+sid+`","body":"draft one"}}`); rec.Code != 200 {
		t.Fatalf("body autosave: %d %s", rec.Code, rec.Body.String())
	}
	if n := gitCommitCount(t, dir); n != 1 {
		t.Fatalf("mid-edit body autosave must not commit: got %d", n)
	}
	// Body is still saved (store is source of truth).
	got := createGet(t, a, sh.ID)
	if got.Sections[0].Body != "draft one" {
		t.Fatalf("autosave body not persisted: %q", got.Sections[0].Body)
	}

	// Completing body PATCH with commit:true → one new commit.
	if rec := patchNote(a, sh.ID, `{"section":{"id":"`+sid+`","body":"final body","commit":true}}`); rec.Code != 200 {
		t.Fatalf("body commit: %d %s", rec.Code, rec.Body.String())
	}
	if n := gitCommitCount(t, dir); n != 2 {
		t.Fatalf("after body commit:true: want 2 commits, got %d", n)
	}

	// add_section (structural) → +1
	if rec := patchNote(a, sh.ID, `{"add_section":"Findings"}`); rec.Code != 200 {
		t.Fatalf("add_section: %d %s", rec.Code, rec.Body.String())
	}
	if n := gitCommitCount(t, dir); n != 3 {
		t.Fatalf("after add_section: want 3 commits, got %d", n)
	}
	afterAdd := createGet(t, a, sh.ID)
	newSec := afterAdd.Sections[len(afterAdd.Sections)-1]

	// section.order (structural) → +1
	if rec := patchNote(a, sh.ID, `{"section":{"id":"`+newSec.ID+`","order":-1}}`); rec.Code != 200 {
		t.Fatalf("section.order: %d %s", rec.Code, rec.Body.String())
	}
	if n := gitCommitCount(t, dir); n != 4 {
		t.Fatalf("after section.order: want 4 commits, got %d", n)
	}

	// section.delete (structural) → +1
	if rec := patchNote(a, sh.ID, `{"section":{"id":"`+newSec.ID+`","delete":true}}`); rec.Code != 200 {
		t.Fatalf("section.delete: %d %s", rec.Code, rec.Body.String())
	}
	if n := gitCommitCount(t, dir); n != 5 {
		t.Fatalf("after section.delete: want 5 commits, got %d", n)
	}

	// Reference add → +1
	refBody := `{"source":{"uid":"u1","segment":0,"record":1},"snapshot":{"text":"evidence","lane":"#c0392b"}}`
	if rec := addReference(a, sh.ID, sid, refBody); rec.Code != 200 {
		t.Fatalf("add ref: %d %s", rec.Code, rec.Body.String())
	}
	if n := gitCommitCount(t, dir); n != 6 {
		t.Fatalf("after reference add: want 6 commits, got %d", n)
	}
	// Find the minted ref id.
	withRef := createGet(t, a, sh.ID)
	if len(withRef.Sections[0].References) != 1 {
		t.Fatalf("want 1 ref, got %d", len(withRef.Sections[0].References))
	}
	refID := withRef.Sections[0].References[0].ID

	// Reference remove → +1
	if rec := trashReference(a, sh.ID, sid, refID); rec.Code != 200 {
		t.Fatalf("trash ref: %d %s", rec.Code, rec.Body.String())
	}
	if n := gitCommitCount(t, dir); n != 7 {
		t.Fatalf("after reference remove: want 7 commits, got %d", n)
	}

	// Repo is inside the note folder only — never the data dir root.
	if _, err := os.Stat(filepath.Join(a.notes.Dir, ".git")); !os.IsNotExist(err) {
		t.Error("git repo must not be at notes/ root; only notes/<id>/.git")
	}
}

// A user global gitconfig with commit.gpgsign=true must not break scimux
// versioning: gitIsolatedEnv points GLOBAL/SYSTEM config at os.DevNull so
// only the forced -c identity applies. Without that isolation, every commit
// would fail (no signing key) and versioning would silently never land.
func TestNoteGitIgnoresUserGlobalConfig(t *testing.T) {
	requireGit(t)

	// Process HOME is what gitIsolatedEnv still forwards; point it at a
	// throwaway tree whose .gitconfig would otherwise force GPG signing.
	userHome := t.TempDir()
	cfg := filepath.Join(userHome, ".gitconfig")
	if err := os.WriteFile(cfg, []byte("[commit]\n\tgpgsign = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", userHome)

	a := newTestApp(t, &fakeTmux{})
	sh := createNote(t, a)
	dir := noteGitDir(a, sh.ID)

	if rec := patchNote(a, sh.ID, `{"title":"Signed-or-not"}`); rec.Code != 200 {
		t.Fatalf("structural title patch: %d %s", rec.Code, rec.Body.String())
	}
	if n := gitCommitCount(t, dir); n != 1 {
		t.Fatalf("commit must land despite user commit.gpgsign=true: want 1, got %d", n)
	}
}

// Without git on PATH, note mutations still succeed and leave no repo.
func TestNoteGitAbsentDegrades(t *testing.T) {
	if testing.Short() {
		t.Skip("mutates findGit; skipped with -short")
	}
	// Force "git not found" regardless of the host PATH.
	restore := notestore.SetFindGitForTest(func() (string, error) {
		return "", exec.ErrNotFound
	})
	defer restore()

	a := newTestApp(t, &fakeTmux{})
	sh := createNote(t, a)
	if rec := patchNote(a, sh.ID, `{"title":"Still Works"}`); rec.Code != 200 {
		t.Fatalf("title patch without git: %d %s", rec.Code, rec.Body.String())
	}
	got := createGet(t, a, sh.ID)
	if got.Title != "Still Works" {
		t.Errorf("title = %q, want Still Works", got.Title)
	}
	if _, err := os.Stat(filepath.Join(noteGitDir(a, sh.ID), ".git")); !os.IsNotExist(err) {
		t.Error("no git binary: must not create a .git directory")
	}
}
