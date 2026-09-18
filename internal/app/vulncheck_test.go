package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The push lane and the release lane must both refuse a build whose inputs
// carry known defects. Which lane matters differently: the push lane is how
// an advisory published after a green build gets noticed at all, and the
// release lane is the last point before users download the result.
var vulncheckWorkflows = []string{
	".github/workflows/build.yml",
	".github/workflows/release.yml",
}

// A scanner whose findings nobody acts on is a report, not a gate. This pins
// the two ways that has already gone wrong once: govulncheck's JSON mode
// exits zero while reporting findings, so a step written that way passes CI
// with a list of advisories in its log; and a scan that runs after the
// artifacts are published cannot stop anything.
func TestWorkflowsGateOnAVulnerabilityScan(t *testing.T) {
	root := repoRootFromTest(t)
	for _, rel := range vulncheckWorkflows {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		body := stripYAMLComments(string(b))
		if !strings.Contains(body, "govulncheck") {
			t.Fatalf("%s runs no vulnerability scan, so an advisory against a build input is nobody's failure", rel)
		}
		for _, line := range strings.Split(body, "\n") {
			if !strings.Contains(line, "govulncheck") {
				continue
			}
			// -json makes the exit status describe whether the scan ran, not
			// whether it found anything.
			if strings.Contains(line, "-json") || strings.Contains(line, "-format json") {
				t.Fatalf("%s scans in JSON mode, whose exit status is zero even with findings:\n\t%s", rel, strings.TrimSpace(line))
			}
		}
	}
}

// Publishing is the irreversible step: once an asset is attached, a scan
// result can only be an apology.
func TestReleaseScansBeforeItPublishes(t *testing.T) {
	root := repoRootFromTest(t)
	body := stripYAMLComments(mustReadFile(t, filepath.Join(root, ".github/workflows/release.yml")))
	scan := strings.Index(body, "govulncheck")
	publish := strings.Index(body, "github-release.sh")
	if publish < 0 {
		t.Fatal("release.yml no longer calls github-release.sh; this guard needs rewriting, not deleting")
	}
	if scan < 0 || scan > publish {
		t.Fatal("release.yml publishes artifacts before it scans them, so the scan gates nothing")
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(b)
}

// stripYAMLComments blanks whole-line comments so a step commented out of a
// workflow stops satisfying these guards.
func stripYAMLComments(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}
