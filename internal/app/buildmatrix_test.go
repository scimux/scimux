package app

// The CI workflows and AGENTS.md claim two things that are not true:
//
//  1. build.yml and release.yml compile darwin/arm64 and darwin/amd64
//     release binaries. Those targets do not compile: internal/remote
//     uses Linux-only syscalls, and cmd/scimux → internal/app →
//     internal/remote is an unconditional import.
//
//  2. AGENTS.md documents `go test ./...` as "unit + integration". The
//     six real-tmux tests in internal/tmuxsession are skipped under
//     -short AND skipped when tmux is absent. All three workflows run
//     `go test -short ./...` and none installs tmux, so those tests
//     execute in no CI job.
//
// This guard is the mechanical statement of both claims. It is written
// to fail: a guard added alongside the fix it waits for is a guard
// nobody ever saw fail. Vacuous truth is the hazard, so each test
// asserts that its parse actually found something — a path bug that
// scanned zero workflow files would otherwise report "every CI target
// builds" forever.
//
// The workflow files are the single source of truth for the matrix. A
// target added to CI is automatically a target this test defends; the
// pairs are not hard-coded here.

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// goosGoarchRe is the `GOOS=<x> GOARCH=<y>` pair the workflows write on
// every cross-build line. It requires adjacency and that order: a
// looser search would invent a pair from two unrelated assignments.
// Comment removal is yamlCode's job, done by the caller before this
// runs; the regex itself does not skip comments.
var goosGoarchRe = regexp.MustCompile(`GOOS=([A-Za-z0-9_]+)\s+GOARCH=([A-Za-z0-9_]+)`)

// crossBuildWorkflows are the jobs that produce release binaries. The
// offline job is deliberately not in this list: it never cross-builds.
var crossBuildWorkflows = []string{
	filepath.Join(".github", "workflows", "build.yml"),
	filepath.Join(".github", "workflows", "release.yml"),
}

type crossBuildTarget struct {
	goos, goarch string
}

func (t crossBuildTarget) name() string { return t.goos + "/" + t.goarch }

// TestCrossBuildTargets compiles ./cmd/scimux for every GOOS/GOARCH pair
// the release workflows claim to ship. Skipped under -short because
// cross-compiling the standard library is slow; the claim is still
// pinned, just not on every unit-test run.
func TestCrossBuildTargets(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiling the standard library is slow; skipped with -short")
	}
	root := repoRootFromTest(t)
	targets := parseWorkflowBuildTargets(t, root)
	for _, tgt := range targets {
		t.Run(tgt.name(), func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "scimux-"+tgt.goos+"-"+tgt.goarch)
			cmd := exec.Command("go", "build", "-o", out, "./cmd/scimux")
			cmd.Dir = root
			cmd.Env = append(os.Environ(),
				"CGO_ENABLED=0",
				"GOOS="+tgt.goos,
				"GOARCH="+tgt.goarch,
			)
			b, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s does not compile:\n%s", tgt.name(), b)
			}
		})
	}
}

// parseWorkflowBuildTargets reads the two release workflows and returns
// each unique GOOS/GOARCH pair in first-seen order. Both files must
// exist and each must yield at least one pair: a missing file or a
// parse that matches nothing would let every build pass vacuously.
//
// A total-miss is not the only vacuity. goosGoarchRe requires GOOS=
// immediately before GOARCH=, so a line written in the other order
// would drop that target while the file still yielded pairs, and the
// test would report success on a target it never built. The GOOS=
// count must equal the pair count so a form change fails closed.
//
// Comments are stripped first: a target commented out of the matrix
// must leave the guard, or the suite goes on defending a build CI no
// longer ships. The count-equality check cannot catch that on its own
// — a commented pair would increment both counters together.
func parseWorkflowBuildTargets(t *testing.T, root string) []crossBuildTarget {
	t.Helper()
	var out []crossBuildTarget
	seen := map[string]bool{}
	for _, rel := range crossBuildWorkflows {
		path := filepath.Join(root, rel)
		goosCount, pairCount := 0, 0
		scanWorkflowLines(t, path, func(line string) {
			code := yamlCode(line)
			goosCount += strings.Count(code, "GOOS=")
			for _, m := range goosGoarchRe.FindAllStringSubmatch(code, -1) {
				pairCount++
				tgt := crossBuildTarget{goos: m[1], goarch: m[2]}
				key := tgt.name()
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, tgt)
			}
		})
		if pairCount == 0 {
			t.Fatalf("%s contains no GOOS=<os> GOARCH=<arch> pairs; the workflow is the matrix, so a parse that finds none would let every target pass vacuously", rel)
		}
		if goosCount != pairCount {
			t.Fatalf("%s has %d GOOS= occurrences but %d GOOS=/GOARCH= pairs; a pair written GOARCH= first would drop that target from the matrix", rel, goosCount, pairCount)
		}
	}
	if len(out) == 0 {
		t.Fatal("parsed no unique GOOS/GOARCH pairs from build.yml and release.yml; the matrix is empty and every build would pass vacuously")
	}
	return out
}

// TestWorkflowsRunTheIntegrationTier is the other half of the AGENTS.md
// claim. It does not skip: the short suite is exactly the suite CI runs,
// so this assertion has to live there or it never fires.
//
// The claim is existential — at least one workflow file must install
// tmux *and* invoke an un-shortened `go test` that covers
// internal/tmuxsession. It is not required of every workflow:
// offline.yml is NFR-08, the short suite with no tmux, and forcing it
// to change is out of scope.
//
// Both halves are checked per file, not per job. A file with one job
// installing tmux and a different job running the tests would pass.
// All three workflows are single-job today (build-and-test,
// offline-tests, build-and-test-and-release), so file-level and
// job-level coincide, and parsing jobs without a YAML library is not
// worth it.
func TestWorkflowsRunTheIntegrationTier(t *testing.T) {
	root := repoRootFromTest(t)
	files := listWorkflowFiles(t, root)

	for _, path := range files {
		body := readWorkflow(t, path)
		if workflowInstallsTmux(body) && workflowRunsUnshortenedGoTest(body) {
			return
		}
	}

	rels := make([]string, 0, len(files))
	for _, path := range files {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		rels = append(rels, filepath.ToSlash(rel))
	}
	t.Fatalf("no workflow both installs tmux (apk add) and runs an un-shortened go test that covers internal/tmuxsession.\n"+
		"AGENTS.md documents `go test ./...` as unit + integration, but the six real-tmux tests in internal/tmuxsession are skipped under -short and skipped when tmux is absent; they execute in no CI job.\n"+
		"scanned: %s", strings.Join(rels, ", "))
}

func listWorkflowFiles(t *testing.T, root string) []string {
	t.Helper()
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatalf("found no workflow files in %s; the scan is not reaching CI", dir)
	}
	return files
}

func readWorkflow(t *testing.T, path string) []string {
	t.Helper()
	var lines []string
	scanWorkflowLines(t, path, func(line string) {
		lines = append(lines, line)
	})
	return lines
}

func scanWorkflowLines(t *testing.T, path string, fn func(string)) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fn(sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
}

// workflowInstallsTmux reports whether any `apk add` line names the
// tmux package. Comments are stripped first: offline.yml mentions tmux
// in a comment about skipping integration, which is the opposite of
// installing it.
func workflowInstallsTmux(lines []string) bool {
	for _, line := range lines {
		code := yamlCode(line)
		if !strings.Contains(code, "apk add") {
			continue
		}
		for _, f := range strings.Fields(code) {
			if f == "tmux" {
				return true
			}
		}
	}
	return false
}

// workflowRunsUnshortenedGoTest reports whether any line invokes
// `go test` without `-short` *and* with arguments that cover the
// integration package. `go test -short ./...` is the unit-only suite
// every workflow runs today and does not count. Neither does an
// un-shortened `go test ./internal/app`: that is the cross-build
// guard, not the tmuxsession suite, and treating it as coverage would
// let the integration claim go green the day the unrelated step lands.
func workflowRunsUnshortenedGoTest(lines []string) bool {
	for _, line := range lines {
		code := yamlCode(line)
		if !strings.Contains(code, "go test") {
			continue
		}
		if hasShortFlag(code) {
			continue
		}
		if !coversIntegrationPackage(code) {
			continue
		}
		return true
	}
	return false
}

// coversIntegrationPackage is true when a `go test` argument is `./...`
// (the whole module, including internal/tmuxsession) or a path that
// names tmuxsession itself.
func coversIntegrationPackage(line string) bool {
	for _, f := range strings.Fields(line) {
		if f == "./..." || strings.Contains(f, "tmuxsession") {
			return true
		}
	}
	return false
}

func yamlCode(line string) string {
	if i := strings.Index(line, "#"); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSpace(line)
}

func hasShortFlag(line string) bool {
	for _, f := range strings.Fields(line) {
		if f == "-short" || strings.HasPrefix(f, "-short=") {
			return true
		}
	}
	return false
}
