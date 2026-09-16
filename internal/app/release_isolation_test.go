package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReleaseVerificationBlockExecutesFailClosed(t *testing.T) {
	names := []string{"scimux-darwin-amd64", "scimux-darwin-arm64", "scimux-linux-amd64", "scimux-linux-arm64"}
	cases := map[string]func(string, []string){
		"valid":              nil,
		"duplicate checksum": func(dir string, lines []string) { writeLines(t, dir, append(lines, lines[0])) },
		"bad hash": func(dir string, lines []string) {
			lines[0] = strings.Repeat("0", 64) + "  " + names[0]
			writeLines(t, dir, lines)
		},
		"bad name and path": func(dir string, lines []string) {
			lines[0] = strings.Fields(lines[0])[0] + "  ../outside"
			writeLines(t, dir, lines)
		},
		"wrong count":     func(dir string, lines []string) { writeLines(t, dir, lines[1:]) },
		"unexpected file": func(dir string, lines []string) { os.WriteFile(filepath.Join(dir, "extra"), []byte("x"), 0o600) },
		"symlink": func(dir string, lines []string) {
			os.Remove(filepath.Join(dir, names[0]))
			os.Symlink(names[1], filepath.Join(dir, names[0]))
		},
		"directory": func(dir string, lines []string) {
			os.Remove(filepath.Join(dir, names[0]))
			os.Mkdir(filepath.Join(dir, names[0]), 0o700)
		},
		"fifo": func(dir string, lines []string) {
			os.Remove(filepath.Join(dir, names[0]))
			if err := exec.Command("mkfifo", filepath.Join(dir, names[0])).Run(); err != nil {
				t.Fatal(err)
			}
		},
	}
	script := releaseVerifyScript(t)
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			handoff := filepath.Join(root, "handoff")
			os.Mkdir(handoff, 0o700)
			var lines []string
			for _, file := range names {
				body := []byte("synthetic-" + file)
				os.WriteFile(filepath.Join(handoff, file), body, 0o600)
				sum := sha256.Sum256(body)
				lines = append(lines, fmt.Sprintf("%x  %s", sum, file))
			}
			writeLines(t, handoff, lines)
			if mutate != nil {
				mutate(handoff, append([]string(nil), lines...))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", script)
			cmd.Dir = root
			err := cmd.Run()
			if ctx.Err() != nil {
				t.Fatal("verification blocked on untrusted input")
			}
			if (mutate == nil) != (err == nil) {
				t.Fatalf("verification result error=%v", err)
			}
		})
	}
}

func writeLines(t *testing.T, dir string, lines []string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func releaseVerifyScript(t *testing.T) string {
	t.Helper()
	src := mustReadFile(t, filepath.Join(repoRootFromTest(t), ".forgejo/workflows/release.yml"))
	marker := "      - name: Verify closed inventory and exact digests"
	start := strings.Index(src, marker)
	if start < 0 {
		t.Fatal("verification step absent")
	}
	rest := src[start:]
	run := strings.Index(rest, "        run: |\n")
	if run < 0 {
		t.Fatal("verification run block absent")
	}
	rest = rest[run+len("        run: |\n"):]
	if end := strings.Index(rest, "      - name:"); end >= 0 {
		rest = rest[:end]
	}
	var lines []string
	for _, line := range strings.Split(rest, "\n") {
		lines = append(lines, strings.TrimPrefix(line, "          "))
	}
	return strings.Join(lines, "\n")
}

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
	for _, guard := range []string{"-type l", "non-regular release artifact", "unexpected release artifact", "invalid checksum manifest", "sha256sum -c SHA256SUMS"} {
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
