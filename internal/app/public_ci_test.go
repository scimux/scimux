package app

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseWorkerRequiresPublishedReleaseAndProtectedMain(t *testing.T) {
	root := repoRootFromTest(t)
	caller := stripYAMLComments(mustReadFile(t, filepath.Join(root, ".github/workflows/release.yml")))
	if !strings.Contains(caller, "uses: scimux/scimux/.github/workflows/release-worker.yml@main") {
		t.Fatal("release must call the worker at protected main, not the release tag")
	}
	src := stripYAMLComments(mustReadFile(t, filepath.Join(root, ".github/workflows/release-worker.yml")))
	build := workflowJobBlock(t, src, "build-and-test:")
	gate := strings.SplitN(build, "steps:", 2)[0]
	for _, required := range []string{
		"github.repository == 'scimux/scimux'",
		"github.event_name == 'release'",
		"github.event.action == 'published'",
		"github.ref_type == 'tag'",
	} {
		if !strings.Contains(gate, required) {
			t.Errorf("worker job lacks event restriction %q", required)
		}
	}
	// Execute the actual shell guard against synthetic Git history. A release
	// may name an older main commit, but never a commit from an unmerged branch.
	marker := `git merge-base --is-ancestor "$COMMIT_SHA" refs/remotes/origin/main`
	start := strings.Index(build, marker)
	end := strings.Index(build, `git checkout --detach "$COMMIT_SHA"`)
	if start < 0 || end < start {
		t.Fatal("main ancestry must be checked before checkout")
	}
	guard := build[start:end]
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Synthetic CI Test")
	git("config", "user.email", "synthetic@example.invalid")
	commit := func(message string) string {
		git("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", message)
		return git("rev-parse", "HEAD")
	}
	base := commit("synthetic base")
	main := commit("synthetic main")
	git("update-ref", "refs/remotes/origin/main", main)
	git("checkout", "-b", "unmerged", base)
	unmerged := commit("synthetic unmerged")
	for _, tc := range []struct {
		name, sha string
		accept    bool
	}{
		{"older main", base, true}, {"main tip", main, true}, {"unmerged", unmerged, false},
		{"unknown commit", strings.Repeat("0", 40), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-eu", "-c", guard)
			cmd.Dir = dir
			cmd.Env = append(cmd.Environ(), "COMMIT_SHA="+tc.sha)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.accept {
				t.Fatalf("accept=%v, error=%v: %s", tc.accept, err, out)
			}
		})
	}
}

func TestPRWorkflowKeepsUntrustedCodeOnHostedRunner(t *testing.T) {
	src := stripYAMLComments(mustReadFile(t, filepath.Join(repoRootFromTest(t), ".github/workflows/pr.yml")))
	for _, forbidden := range []string{"pull_request_target", "self-hosted", "contents: write", "secrets.", "github.event.pull_request.head.repo"} {
		if strings.Contains(src, forbidden) {
			t.Errorf("PR workflow crosses trust boundary with %q", forbidden)
		}
	}
	for _, required := range []string{"pull_request:", "runs-on: ubuntu-24.04", "contents: read", "persist-credentials: false", "name: PR tests", "go test -timeout 20m ./...", "node --test web/test/*.test.js"} {
		if !strings.Contains(src, required) {
			t.Errorf("PR workflow lacks %q", required)
		}
	}
	// Do not override checkout's default PR merge ref with main or a contributor
	// ref: the required check must test the code that would actually be merged.
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ref:") {
			t.Fatal("PR checkout must test the default merge commit")
		}
	}
}

func TestSelfHostedPushWorkflowsRunOnlyOnMain(t *testing.T) {
	for _, name := range []string{"build.yml", "offline.yml"} {
		t.Run(name, func(t *testing.T) {
			src := stripYAMLComments(mustReadFile(t, filepath.Join(repoRootFromTest(t), ".github/workflows", name)))
			trigger := strings.SplitN(src, "permissions:", 2)[0]
			if !strings.Contains(trigger, "on:\n  push:\n    branches: [main]\n") {
				t.Fatalf("%s must schedule self-hosted push jobs only on main", name)
			}
			if !strings.Contains(src, "runs-on: [self-hosted, linux, x64, go-builder]") {
				t.Fatalf("%s lost its dedicated runner requirement", name)
			}
		})
	}
}
