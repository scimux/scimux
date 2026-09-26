package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute each real checkout block against a synthetic local repository. Git's
// ownership-test switch models a host-owned workspace mounted in a container;
// the isolated global config must trust that workspace without trusting others.
func TestWorkflowCheckoutTrustsOnlyItsWorkspace(t *testing.T) {
	// The hosted PR job uses actions/checkout; the release caller has no steps.
	for _, rel := range []string{
		".github/workflows/build.yml",
		".github/workflows/offline.yml",
		".github/workflows/release-worker.yml",
	} {
		t.Run(rel, func(t *testing.T) {
			src := stripYAMLComments(mustReadFile(t, filepath.Join(repoRootFromTest(t), rel)))
			var script string
			for _, block := range workflowRunBlocks(src) {
				if strings.Contains(block, "clone https://github.com/scimux/scimux.git .") {
					if script != "" {
						t.Fatal("multiple checkout blocks")
					}
					script = block
				}
			}
			if script == "" {
				t.Fatal("checkout block missing")
			}
			root := t.TempDir()
			seed := filepath.Join(root, "source")
			workspace := filepath.Join(root, "workspace with spaces")
			other := filepath.Join(root, "unrelated")
			for _, dir := range []string{seed, workspace, other} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			env := append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(root, "gitconfig"),
				"GIT_CONFIG_NOSYSTEM=1", "GIT_TEST_ASSUME_DIFFERENT_OWNER=0")
			git := func(dir string, args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir, cmd.Env = dir, env
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git(seed, "init", "--quiet", "-b", "main")
			git(seed, "-c", "user.name=Synthetic", "-c", "user.email=synthetic@example.invalid",
				"-c", "commit.gpgsign=false", "commit", "--quiet", "--allow-empty", "-m", "Synthetic checkout fixture")
			sha := git(seed, "rev-parse", "HEAD")
			git(seed, "-c", "tag.gpgsign=false", "tag", "v1.2.3")
			git(other, "init", "--quiet")
			// Only the local source transport needs this test-only trust entry.
			git(seed, "config", "--global", "--add", "safe.directory", filepath.Join(seed, ".git"))
			env = append(env, "GIT_TEST_ASSUME_DIFFERENT_OWNER=1", "GITHUB_WORKSPACE="+workspace,
				"GITHUB_CLONE_TOKEN=synthetic.header.payload", "COMMIT_SHA="+sha,
				"REF_NAME=v1.2.3", "SYNTHETIC_SOURCE="+seed)
			script = strings.ReplaceAll(script, "https://github.com/scimux/scimux.git", `"$SYNTHETIC_SOURCE"`)
			cmd := exec.Command("sh", "-eu", "-c", script)
			cmd.Dir, cmd.Env = workspace, env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("checkout under ownership mismatch: %v\n%s", err, out)
			}
			if got := git(workspace, "rev-parse", "HEAD"); got != sha {
				t.Fatalf("checked out %s, want %s", got, sha)
			}
			if got := git(workspace, "config", "--global", "--get-all", "safe.directory"); got != filepath.Join(seed, ".git")+"\n"+workspace {
				t.Fatalf("unexpected trust entries: %q", got)
			}
			cmd = exec.Command("git", "status", "--porcelain")
			cmd.Dir, cmd.Env = other, env
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "dubious ownership") {
				t.Fatalf("unrelated repository was not rejected: %v\n%s", err, out)
			}
		})
	}
}
