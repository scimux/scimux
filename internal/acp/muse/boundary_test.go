package muse

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

var productionFiles = []string{
	"uuid.go",
	"peer.go",
	"transport.go",
	"events.go",
	"approval.go",
	"client.go",
	"log.go",
	"manager.go",
}

func productionDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(thisFile)
}

func TestBoundaryEnumeratesEveryProductionFile(t *testing.T) {
	dir := productionDir(t)
	seen := map[string]bool{}
	for _, name := range productionFiles {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected production file %s: %v", name, err)
		}
		seen[name] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var extra []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		if !seen[e.Name()] {
			extra = append(extra, e.Name())
		}
	}
	if len(extra) > 0 {
		t.Fatalf("production files not in the explicit scan list (the walk would omit them): %v", extra)
	}
	if len(seen) != 8 {
		t.Fatalf("want 8 production files, listed %d", len(seen))
	}
}

func TestBoundaryAntiVacuityScannedTheRealPackage(t *testing.T) {
	files := readProduction(t)
	if len(files) != 8 {
		t.Fatalf("scanned %d production files, want 8", len(files))
	}
	joined := ""
	for _, b := range files {
		joined += b
	}
	if !strings.Contains(joined, "PinnedFingerprint") || !strings.Contains(joined, "NewCommandID") {
		t.Fatal("scan did not reach the real muse production package")
	}
	if strings.Contains(joined, "this planted token must not appear in production") {
		t.Fatal("scan included test files")
	}
}

func TestBoundaryImportsAreStdlibOrScimuxInternal(t *testing.T) {
	dir := productionDir(t)
	fset := token.NewFileSet()
	for _, name := range productionFiles {
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			if !allowedImport(p) {
				t.Errorf("%s imports %s (stdlib or scimux internal only)", name, p)
			}
			if strings.Contains(p, "github.com/coder/acp-go-sdk") || strings.Contains(p, "acp-go-sdk") {
				t.Errorf("%s imports ACP SDK %s", name, p)
			}
		}
	}
}

func TestBoundaryForbiddenLiteralsAbsentFromProduction(t *testing.T) {
	files := readProduction(t)
	for name, src := range files {
		for _, hit := range forbiddenLiteralHits(src) {
			t.Errorf("%s: %s", name, hit)
		}
	}
}

func TestBoundaryRejectsDotlessThirdPartyImport(t *testing.T) {
	if allowedImport("corp/localpkg") {
		t.Fatal("dotless third-party path must not be classified as standard library")
	}
	if allowedImport("github.com/coder/acp-go-sdk") {
		t.Fatal("ACP SDK must be rejected")
	}
	if !allowedImport("fmt") || !allowedImport("codeberg.org/chrberger/scimux/internal/sessionlog") {
		t.Fatal("allowlist rejected a genuine production import")
	}
}

func TestBoundaryMatchersCatchPlantedViolations(t *testing.T) {
	cred := "credentials" + ".json"
	auth := "auth" + ".json"
	plants := map[string]string{
		"meta endpoint":   `u := "https://api.meta.ai/muse-code/channels/muse-stable"`,
		"muse config dir": `path := "/home/user/.muse/tokens"`,
		"base-url flag":   `args := []string{"serve", "--base-url", "http://127.0.0.1"}`,
		"provider flag":   `args := []string{"serve", "--provider", "other"}`,
		"approval-judge":  `args := []string{"serve", "--approval-judge", "off"}`,
		"credential file": `os.ReadFile("muse-token.json")`,
		"cred filename":   "os.ReadFile(\"" + cred + "\")",
		"auth filename":   "os.ReadFile(\"" + auth + "\")",
		"launcher curl":   `exec.Command("curl", "https://example.invalid/install-muse.sh")`,
		"schema artifact": `ioutil.ReadFile("schema/msp.schema.json")`,
		"golden testdata": `data, _ := os.ReadFile("testdata/golden-approval.ndjson")`,
		"acp sdk import":  `import "github.com/coder/acp-go-sdk"`,
	}
	for label, plant := range plants {
		hits := forbiddenLiteralHits(plant)
		if strings.Contains(plant, "acp-go-sdk") {
			if allowedImport("github.com/coder/acp-go-sdk") {
				t.Fatalf("%s: SDK import was allowed", label)
			}
			continue
		}
		if len(hits) == 0 {
			t.Fatalf("matcher missed planted violation %s: %s", label, plant)
		}
	}
	// Harmless protocol fields must not trip the flag matchers.
	safe := []string{
		`ProviderName string ` + "`json:\"providerName\"`",
		`params["providerId"] = p.ProviderID`,
		`Title: "approval-judge unavailable"`,
	}
	for _, s := range safe {
		if hits := forbiddenLiteralHits(s); len(hits) > 0 {
			t.Fatalf("false positive on %q: %v", s, hits)
		}
	}
}

var allowedStdlibImports = map[string]bool{
	"bufio": true, "bytes": true, "context": true, "crypto/rand": true,
	"encoding/binary": true, "encoding/hex": true, "encoding/json": true,
	"errors": true, "fmt": true, "io": true, "os": true, "os/exec": true,
	"path/filepath": true, "regexp": true, "strconv": true, "strings": true,
	"sync": true, "syscall": true, "time": true,
}

var allowedInternalImports = map[string]bool{
	"codeberg.org/chrberger/scimux/internal/agentperm":  true,
	"codeberg.org/chrberger/scimux/internal/asset":      true,
	"codeberg.org/chrberger/scimux/internal/sessionlog": true,
	"codeberg.org/chrberger/scimux/internal/transcript": true,
}

func allowedImport(p string) bool {
	if allowedStdlibImports[p] {
		return true
	}
	if allowedInternalImports[p] {
		return true
	}
	return false
}

func readProduction(t *testing.T) map[string]string {
	t.Helper()
	dir := productionDir(t)
	out := map[string]string{}
	for _, name := range productionFiles {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = string(b)
	}
	return out
}

func forbiddenLiteralHits(src string) []string {
	var hits []string
	checks := []struct {
		name string
		ok   func(string) bool
	}{
		{"meta api endpoint", func(s string) bool { return strings.Contains(s, "api.meta.ai") }},
		{"muse config dir", func(s string) bool {
			return strings.Contains(s, "/.muse") || strings.Contains(s, "~/.muse")
		}},
		{"base-url flag", func(s string) bool { return strings.Contains(s, "--base-url") }},
		{"provider flag", func(s string) bool { return strings.Contains(s, "--provider") }},
		{"approval-judge flag", func(s string) bool { return strings.Contains(s, "--approval-judge") }},
		{"credential filename", func(s string) bool {
			return strings.Contains(s, "muse-token") ||
				strings.Contains(s, "credentials"+".json") ||
				strings.Contains(s, "auth"+".json")
		}},
		{"launcher fetch", func(s string) bool {
			return strings.Contains(s, "install-muse") || strings.Contains(s, "muse-stable")
		}},
		{"schema artifact", func(s string) bool {
			return strings.Contains(s, "msp.schema.json") || strings.Contains(s, "/schema/")
		}},
		{"vendored testdata", func(s string) bool {
			return strings.Contains(s, "testdata/golden") || strings.Contains(s, "testdata/real-") ||
				strings.Contains(s, ".ndjson")
		}},
	}
	for _, c := range checks {
		if c.ok(src) {
			hits = append(hits, c.name)
		}
	}
	return hits
}
