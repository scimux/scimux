package app

// S0 (remote refactor) — the tracked-source guard.
//
// This exists because of a real, already-paid-for failure. `.gitignore` carried
// unanchored `scimux-*` and `scimux` entries meant to exclude the built binary;
// unanchored, they also matched the *directory* cmd/scimux/, so the build
// entrypoint was silently absent from git. The repository built fine for
// everyone who had it locally and could not build at all from a clean clone.
// The pattern is now anchored (`/scimux`), and this guard is what stops the
// class from recurring.
//
// It is deliberately a class guard, not a check for that one path: any source
// file the build or the test suite needs, silently ignored, produces the same
// unclonable repository. So it asserts that no Go source, no browser module and
// no stylesheet is git-ignored — the fixture files that are ignored ON PURPOSE
// are named as exceptions, which makes the intent auditable instead of implied.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// deliberatelyIgnored are the path prefixes that are ignored by design. Each is
// a documented decision in AGENTS.md's "Fixtures and privacy" section: captured
// fixtures are personal environment snapshots and must never be committed.
var deliberatelyIgnored = []string{
	"internal/transcript/testdata/real-",
	"internal/acp/codex/testdata/real-",
	"internal/acp/testdata/real-",
}

// trackedSourceGlobs are the file classes that must reach a clean clone: the
// product, its tests, and the browser source the binary embeds.
var trackedSourceGlobs = []string{"*.go", "*.js", "*.css", "*.html"}

func gitInRepo(t *testing.T) (string, bool) {
	t.Helper()
	root := repoRootFromTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		return "", false
	}
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = root
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return root, true
}

func isBuildSource(rel string) bool {
	if strings.Contains(rel, "node_modules/") || strings.HasPrefix(rel, "attic/") {
		return false
	}
	for _, p := range deliberatelyIgnored {
		if strings.HasPrefix(rel, p) {
			return false
		}
	}
	for _, g := range trackedSourceGlobs {
		if m, _ := filepath.Match(g, filepath.Base(rel)); m {
			return true
		}
	}
	return false
}

// TestNoBuildSourceIsGitIgnored is the guard proper, and it covers BOTH forms
// the hazard takes.
//
//	untracked + ignored — the file never entered the index, so a clean clone
//	                      simply does not have it. This is what happened to
//	                      cmd/scimux/main.go.
//	  tracked + ignored — the clone still gets the file (gitignore does not
//	                      evict what is already tracked), so nothing breaks
//	                      today; it breaks the first time the file is removed
//	                      and re-added, at which point `git add` silently
//	                      declines. Latent, and worth failing on now.
//
// The second form needs `git check-ignore --no-index`: without it, check-ignore
// deliberately says nothing about tracked paths, and this guard would report a
// clean tree while an ignore pattern sat over the browser source.
func TestNoBuildSourceIsGitIgnored(t *testing.T) {
	root, ok := gitInRepo(t)
	if !ok {
		t.Skip("not a git work tree (or git absent); nothing to check")
	}

	// Form 1: ignored files that never made it into the index.
	untracked := exec.Command("git", "ls-files", "--others", "--ignored", "--exclude-standard")
	untracked.Dir = root
	out, err := untracked.Output()
	if err != nil {
		t.Fatalf("git ls-files --others --ignored: %v", err)
	}
	var bad []string
	for _, line := range strings.Split(string(out), "\n") {
		rel := strings.TrimSpace(line)
		if rel != "" && isBuildSource(rel) {
			bad = append(bad, rel+" (ignored and never tracked — absent from a clean clone)")
		}
	}

	// Form 2: tracked files an ignore pattern nonetheless matches.
	tracked := exec.Command("git", "ls-files")
	tracked.Dir = root
	tout, err := tracked.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var sources []string
	for _, line := range strings.Split(string(tout), "\n") {
		rel := strings.TrimSpace(line)
		if rel != "" && isBuildSource(rel) {
			sources = append(sources, rel)
		}
	}
	if len(sources) < 50 {
		// Anti-vacuity: if the listing came back thin, the check below proves
		// nothing and would keep proving nothing.
		t.Fatalf("only %d tracked build sources found; the git listing is not working, "+
			"so this guard would pass vacuously", len(sources))
	}
	chk := exec.Command("git", "check-ignore", "--no-index", "--stdin")
	chk.Dir = root
	chk.Stdin = strings.NewReader(strings.Join(sources, "\n") + "\n")
	cout, cerr := chk.Output()
	// check-ignore exits 1 when nothing matched, which is the good case.
	if cerr != nil && len(cout) == 0 {
		if ee, isExit := cerr.(*exec.ExitError); !isExit || ee.ExitCode() != 1 {
			t.Fatalf("git check-ignore: %v", cerr)
		}
	}
	for _, line := range strings.Split(string(cout), "\n") {
		rel := strings.TrimSpace(line)
		if rel != "" {
			bad = append(bad, rel+" (tracked, but an ignore pattern matches it — "+
				"survives only because it is already in the index)")
		}
	}

	if len(bad) > 0 {
		t.Fatalf("build sources are git-ignored:\n\t%s\n"+
			"An unanchored .gitignore pattern already did this once to cmd/scimux/ "+
			"(the entrypoint matched the binary's own name). Anchor the pattern, or add "+
			"the path to deliberatelyIgnored with the reason.", strings.Join(bad, "\n\t"))
	}
}

// TestBuildEntrypointIsTracked is the specific assertion for the file that was
// actually lost. The class guard above would catch it too; this one names it so
// the failure message points straight at the history.
func TestBuildEntrypointIsTracked(t *testing.T) {
	root, ok := gitInRepo(t)
	if !ok {
		t.Skip("not a git work tree (or git absent); nothing to check")
	}
	const entry = "cmd/scimux/main.go"

	cmd := exec.Command("git", "ls-files", "--error-unmatch", entry)
	cmd.Dir = root
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s is NOT tracked by git: a clean clone cannot build scimux. "+
			"This is the exact failure an unanchored .gitignore pattern caused; "+
			"check that the binary patterns stay anchored (/scimux, /scimux-*).", entry)
	}

	// And it must not be ignored either — a tracked-but-ignored file stays in
	// the clone but silently drops out the moment it is removed and re-added.
	// --no-index is required: check-ignore says nothing about tracked paths
	// without it, which would make this half of the assertion inert.
	chk := exec.Command("git", "check-ignore", "--no-index", "-q", entry)
	chk.Dir = root
	if err := chk.Run(); err == nil {
		t.Fatalf("%s is matched by a .gitignore pattern; it survives only because it "+
			"is already in the index", entry)
	}
}

// TestGitIgnoreBinaryPatternsAreAnchored pins the fix itself. The unanchored
// forms are what caused the incident, so their absence is the invariant, not an
// implementation detail of the current file.
func TestGitIgnoreBinaryPatternsAreAnchored(t *testing.T) {
	root := repoRootFromTest(t)
	raw, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "scimux" || line == "scimux-*" {
			t.Fatalf("unanchored .gitignore pattern %q: it names the built binary but also "+
				"matches the directory cmd/scimux/, which once removed the build entrypoint "+
				"from git. Anchor it to the repo root (/%s).", line, line)
		}
	}
}
