package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The whole UI is one embedded HTML file with inline scripts — a stray
// backtick or brace ships silently in the binary and only surfaces as a
// blank page on the iPad. Parse the scripts with node when it is available.
func TestWebIndexScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS syntax check")
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	blocks := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(string(b), -1)
	if len(blocks) == 0 {
		t.Fatal("no inline <script> blocks found in web/index.html")
	}
	var js strings.Builder
	for _, m := range blocks {
		js.WriteString(m[1])
		js.WriteString("\n;\n")
	}
	f := filepath.Join(t.TempDir(), "index.js")
	if err := os.WriteFile(f, []byte(js.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
		t.Fatalf("inline JS does not parse: %v\n%s", err, out)
	}
}
