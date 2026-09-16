package app

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseUsesFreshCredentialedPublishJob(t *testing.T) {
	src := stripYAMLComments(mustReadFile(t, filepath.Join(repoRootFromTest(t), ".forgejo/workflows/release.yml")))
	build := workflowJobBlock(t, src, "build-and-test:")
	publish := workflowJobBlock(t, src, "publish:")

	if !strings.Contains(publish, "needs: build-and-test") {
		t.Error("publish job must depend on the completed build-and-test job")
	}
	for _, pin := range []string{
		"https://code.forgejo.org/forgejo/upload-artifact@16871d9e8cfcf27ff31822cac382bbb5450f1e1e",
		"https://code.forgejo.org/forgejo/download-artifact@d8d0a99033603453ad2255e58720b460a0555e1e",
	} {
		if !strings.Contains(src, pin) {
			t.Errorf("release workflow does not use verified Forgejo action pin %q", pin)
		}
	}
	if strings.Contains(build, "secrets.") {
		t.Error("build-and-test job has a secret in reach")
	}
	if !strings.Contains(publish, "secrets.CODEBERG_TOKEN") {
		t.Error("only the fresh publish job should receive the release token")
	}
	for _, forbidden := range []string{"go test", "go build", "govulncheck", "web/test/"} {
		if strings.Contains(publish, forbidden) {
			t.Errorf("publish job executes untrusted build/test code containing %q", forbidden)
		}
	}
	verify := strings.Index(publish, "sha256sum -c SHA256SUMS")
	secret := strings.Index(publish, "secrets.CODEBERG_TOKEN")
	if verify < 0 || secret < 0 || verify > secret {
		t.Error("downloaded artifact must pass exact digest validation before the release token is exposed")
	}
}

func TestReleaseBindsSafeTagToExactCommit(t *testing.T) {
	src := stripYAMLComments(mustReadFile(t, filepath.Join(repoRootFromTest(t), ".forgejo/workflows/release.yml")))
	build := workflowJobBlock(t, src, "build-and-test:")
	for _, required := range []string{
		`grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'`,
		`refs/tags/$REF_NAME`,
		`git rev-parse "$REF_NAME^{commit}"`,
		`"$TAG_COMMIT" = "$COMMIT_SHA"`,
	} {
		if !strings.Contains(build, required) {
			t.Errorf("release build lacks tag/commit guard %q", required)
		}
	}
	if strings.Contains(src, "oauth2:") {
		t.Error("public source must be cloned anonymously, without a token in the URL")
	}
}

func TestReleaseArtifactInventoryIsClosed(t *testing.T) {
	src := stripYAMLComments(mustReadFile(t, filepath.Join(repoRootFromTest(t), ".forgejo/workflows/release.yml")))
	publish := workflowJobBlock(t, src, "publish:")
	for _, name := range []string{
		"SHA256SUMS",
		"scimux-linux-amd64",
		"scimux-linux-arm64",
		"scimux-darwin-amd64",
		"scimux-darwin-arm64",
	} {
		if !strings.Contains(publish, name) {
			t.Errorf("publish verification does not close over %s", name)
		}
	}
	for _, guard := range []string{"-type l", "unexpected release artifact", "invalid checksum manifest", "sha256sum -c SHA256SUMS"} {
		if !strings.Contains(publish, guard) {
			t.Errorf("publish verification lacks %q", guard)
		}
	}
}

func workflowJobBlock(t *testing.T, src, key string) string {
	t.Helper()
	needle := "  " + key
	start := strings.Index(src, needle)
	if start < 0 {
		t.Fatalf("workflow has no %s job", strings.TrimSuffix(key, ":"))
	}
	rest := src[start+len(needle):]
	lines := strings.Split(rest, "\n")
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") && strings.HasSuffix(strings.TrimSpace(line), ":") {
			return strings.Join(lines[:i], "\n")
		}
	}
	return rest
}
