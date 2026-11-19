package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Exercise the actual module configuration with independent synthetic packages.
func TestGoPackageDiscoveryExcludesRootAttic(t *testing.T) {
	root := t.TempDir()
	mod, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":              string(mod),
		"main.go":             "package fixture\n",
		"published/doc.go":    "package published\n",
		"nested/attic/doc.go": "package attic\n",
		"attic/scratch.go":    "package scratch\nvar _ = undefinedSyntheticValue\n",
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	goexe := filepath.Join(runtime.GOROOT(), "bin", "go")
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(goexe, args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOFLAGS=-mod=readonly")
		return cmd.CombinedOutput()
	}
	list, listErr := run("list", "./...")
	_, buildErr := run("build", "./...")
	if listErr != nil || buildErr != nil {
		t.Fatalf("synthetic module discovery/build failed: list=%v build=%v", listErr, buildErr)
	}
	packages := make(map[string]bool)
	for _, line := range strings.Fields(string(list)) {
		packages[line] = true
	}
	for _, name := range []string{"github.com/scimux/scimux", "github.com/scimux/scimux/published", "github.com/scimux/scimux/nested/attic"} {
		if !packages[name] {
			t.Errorf("package %s missing from discovery", name)
		}
	}
	if packages["github.com/scimux/scimux/attic"] {
		t.Error("root attic was discovered")
	}
}
