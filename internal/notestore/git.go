package notestore

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// findGit locates the git binary. Tests may override via SetFindGitForTest so
// "no git on PATH" is exercisable without mutating the process environment.
var findGit = func() (string, error) { return exec.LookPath("git") }

// SetFindGitForTest replaces the git locator; the returned func restores it.
// For tests only.
func SetFindGitForTest(fn func() (string, error)) (restore func()) {
	prev := findGit
	findGit = fn
	return func() { findGit = prev }
}

// TryCommit best-effort versions the note folder with git. The note document
// (note.json) is always the source of truth: a git failure is logged and never
// returned as an error. If git is absent, this is a silent no-op. The repo
// lives inside notes/<id>/.git only — never the user's own repositories and
// never notes/ as a whole. Identity is forced via -c, and global/system
// gitconfig are redirected to os.DevNull, so the user's commit.gpgsign,
// hooksPath, includeIf, etc. never apply.
func (s *Store) TryCommit(id, message string) {
	if id == "" {
		return
	}
	bin, err := findGit()
	if err != nil || bin == "" {
		return
	}
	dir := s.noteDir(id)
	if _, err := os.Stat(s.path(id)); err != nil {
		return // no document to version
	}
	if message == "" {
		message = "update"
	}
	// Lazy init: only create .git on the first commit attempt for this note.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if out, err := runGit(bin, dir, "init"); err != nil {
			logGit("init", id, err, out)
			return
		}
	}
	if out, err := runGit(bin, dir, "add", "-A"); err != nil {
		logGit("add", id, err, out)
		return
	}
	// commit may exit 1 with "nothing to commit" when the working tree is
	// clean (e.g. a no-op PATCH) — that is not a failure worth logging loudly.
	out, err := runGit(bin, dir, "commit", "-m", message)
	if err != nil {
		if strings.Contains(string(out), "nothing to commit") {
			return
		}
		logGit("commit", id, err, out)
	}
}

// runGit executes git -C <dir> with a fixed scimux identity so commits never
// depend on (or write through) the user's global or system git config.
func runGit(bin, dir string, args ...string) ([]byte, error) {
	full := append([]string{
		"-c", "user.name=scimux",
		"-c", "user.email=scimux@localhost",
		"-C", dir,
	}, args...)
	cmd := exec.Command(bin, full...)
	// Isolate repo location and config: no user GIT_* redirects, no global
	// or system gitconfig (gpgsign, hooks, includes).
	cmd.Env = gitIsolatedEnv()
	return cmd.CombinedOutput()
}

// gitIsolatedEnv is the process environment with git repo-location and config
// isolated from the user's setup. Drops GIT_* (so -C dir is the only repo)
// and forces GLOBAL/SYSTEM config to os.DevNull so ~/.gitconfig and
// /etc/gitconfig never apply. Identity comes solely from -c user.* flags.
func gitIsolatedEnv() []string {
	var out []string
	for _, e := range os.Environ() {
		// Drop GIT_DIR / GIT_WORK_TREE / GIT_DIR-like overrides and template
		// config so -C dir is the only repo we ever open.
		if strings.HasPrefix(e, "GIT_") {
			continue
		}
		out = append(out, e)
	}
	// Hermetic config: ignore user global and system gitconfig
	// (commit.gpgsign, core.hooksPath, includeIf, …).
	out = append(out,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
	)
	return out
}

func logGit(op, id string, err error, out []byte) {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		fmt.Fprintf(os.Stderr, "scimux/notestore: git %s %s: %v\n", op, id, err)
		return
	}
	fmt.Fprintf(os.Stderr, "scimux/notestore: git %s %s: %v: %s\n", op, id, err, msg)
}
