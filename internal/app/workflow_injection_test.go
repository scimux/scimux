package app

import (
	"path/filepath"
	"strings"
	"testing"
)

// Every checked-in CI pipeline. build.yml and offline.yml only clone and
// test; release.yml also publishes to Codeberg with a token.
var allWorkflows = []string{
	".forgejo/workflows/build.yml",
	".forgejo/workflows/offline.yml",
	".forgejo/workflows/release.yml",
}

// A `${{ ... }}` expression is substituted **textually** into the script
// before any shell parses it, so a ref name of
//
//	v0$(printf${IFS}whatever)
//
// is not a string the script reads — it is a command the runner executes,
// with whatever the workflow holds in its environment. Quoting in the YAML
// does not help, because the quotes are in the text the substitution
// happens inside.
//
// The fix is that no run step contains an expression at all: values reach
// the shell through the step's own `env:`, where a substitution can only
// ever become the value of a variable.
func TestWorkflowRunStepsTakeNoExpressionSubstitution(t *testing.T) {
	root := repoRootFromTest(t)
	for _, rel := range allWorkflows {
		src := mustReadFile(t, filepath.Join(root, rel))
		for _, block := range workflowRunBlocks(stripYAMLComments(src)) {
			for _, line := range strings.Split(block, "\n") {
				if strings.Contains(line, "${{") {
					t.Errorf("%s interpolates into a shell script; route the value through the step's env: instead\n\t%s",
						rel, strings.TrimSpace(line))
				}
			}
		}
	}
}

// A secret declared at workflow level is in the environment of every step,
// including `go test ./...` and every tool it shells out to. Scoping it to
// the step that spends it is what keeps the release token out of reach of
// code that has no business holding it.
func TestWorkflowSecretsAreScopedToTheStepThatSpendsThem(t *testing.T) {
	root := repoRootFromTest(t)
	for _, rel := range allWorkflows {
		src := stripYAMLComments(mustReadFile(t, filepath.Join(root, rel)))
		block, ok := workflowTopLevelBlock(src, "env:")
		if !ok {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if strings.Contains(line, "secrets.") {
				t.Errorf("%s puts a secret in every step's environment; move it to the step that needs it\n\t%s",
					rel, strings.TrimSpace(line))
			}
		}
	}
}

// Routing the ref through `env:` stops it being executed; it does not stop
// it being wrong. The release lane names a tag to the Codeberg API and
// stamps it into every binary, so it bounds what a tag may be before it
// builds or publishes anything.
func TestTheReleaseTagIsBoundedBeforeItIsBuiltOrPublished(t *testing.T) {
	rel := ".forgejo/workflows/release.yml"
	src := stripYAMLComments(mustReadFile(t, filepath.Join(repoRootFromTest(t), rel)))

	guard := strings.Index(src, `case "$REF_NAME" in`)
	if guard < 0 {
		t.Fatalf("%s never bounds the release tag it was handed", rel)
	}
	// A case with only a positive arm passes anything, because a shell
	// glob's * matches shell syntax too. The rejecting arm is the guard.
	if !strings.Contains(src, `*[!0-9A-Za-z.-]*)`) {
		t.Errorf("%s admits a tag containing characters outside 0-9 A-Z a-z . -", rel)
	}
	for _, use := range []string{"go build", "codeberg-release.sh"} {
		at := strings.Index(src, use)
		if at < 0 {
			t.Fatalf("%s no longer contains %q; this guard needs rewriting, not deleting", rel, use)
		}
		if at < guard {
			t.Errorf("%s reaches %q before it has bounded the tag", rel, use)
		}
	}
}

// workflowRunBlocks returns the shell scripts a workflow hands to the
// runner: the inline `run: cmd` form and the `run: |` block form, the
// latter being every following line indented past the key.
func workflowRunBlocks(src string) []string {
	lines := strings.Split(src, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimLeft(lines[i], " ")
		if !strings.HasPrefix(trimmed, "run:") {
			continue
		}
		indent := len(lines[i]) - len(trimmed)
		if rest := strings.TrimSpace(trimmed[len("run:"):]); !isYAMLBlockScalar(rest) {
			out = append(out, rest)
			continue
		}
		var b strings.Builder
		for i+1 < len(lines) {
			next := lines[i+1]
			if strings.TrimSpace(next) != "" && len(next)-len(strings.TrimLeft(next, " ")) <= indent {
				break
			}
			b.WriteString(next)
			b.WriteString("\n")
			i++
		}
		out = append(out, b.String())
	}
	return out
}

func isYAMLBlockScalar(s string) bool {
	return s == "" || s == "|" || s == "|-" || s == "|+" || s == ">" || s == ">-" || s == ">+"
}

// workflowTopLevelBlock returns the lines nested under an unindented key.
func workflowTopLevelBlock(src, key string) (string, bool) {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if line != key {
			continue
		}
		var b strings.Builder
		for _, next := range lines[i+1:] {
			if strings.TrimSpace(next) != "" && !strings.HasPrefix(next, " ") {
				break
			}
			b.WriteString(next)
			b.WriteString("\n")
		}
		return b.String(), true
	}
	return "", false
}
