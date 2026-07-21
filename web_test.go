package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
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

// Extract the pure markdown helpers (mdInline/md) from the embedded page and
// execute them under node with a DOM-free esc stub. Finding 97: paragraph
// lines must be escaped per line and then joined with a real <br>, never the
// other way around.
func TestWebMarkdownParagraphs(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	start := strings.Index(html, "function mdInline(")
	if start < 0 {
		t.Fatal("could not locate mdInline in web/index.html")
	}
	end := strings.Index(html[start:], "/* ---------- statusbar")
	if end < 0 {
		t.Fatal("could not locate the end of the markdown helpers in web/index.html")
	}
	script := `function esc(s){ return String(s ?? "").replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;"); }` + "\n" +
		html[start:start+end] + `
const assert = require("assert");
assert.strictEqual(md("line one\nline two"), "<p>line one<br>line two</p>");
assert.strictEqual(md("a <b> tag\nnext"), "<p>a &lt;b&gt; tag<br>next</p>");
assert.strictEqual(md("solo"), "<p>solo</p>");
assert.strictEqual(md("p1 l1\np1 l2\n\np2"), "<p>p1 l1<br>p1 l2</p><p>p2</p>");
`
	f := filepath.Join(t.TempDir(), "md.js")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("markdown paragraph rendering broken: %v\n%s", err, out)
	}
}

// Finding 98 contract: a fork must not submit the sheet's stale launch config.
// The payload sends agent/model/effort/dir empty whenever a parent is set so
// that resolveNode remains the single inheritance authority.
func TestForkPayloadDefersLaunchConfigToServer(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	for _, field := range []string{"agent", "model", "effort", "dir"} {
		if !strings.Contains(html, field+`: ncParent ? "" :`) {
			t.Errorf("new-node payload field %q is not emptied in fork mode", field)
		}
	}
}

// R18.1 altitude: a lane color is user-authored and flows into many style=""
// interpolations. safeColor must return only well-formed color syntax verbatim
// and reject anything that could break out of an attribute, so the accessor is
// the single guard rather than every call site's esc() wrap.
func TestWebSafeColorRejectsInjection(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	start := strings.Index(html, "const SAFE_COLOR")
	end := strings.Index(html, "function laneColor(")
	if start < 0 || end < 0 || end < start {
		t.Fatal("could not locate the safeColor helpers in web/index.html")
	}
	script := html[start:end] + `
const assert = require("assert");
for (const ok of ["#abc", "#aabbcc", "#aabbccdd", "var(--work)", "rgb(1,2,3)", "rgba(1,2,3,.5)", "hsl(200, 50%, 40%)", "tomato", "  #fff  "]) {
  assert.ok(safeColor(ok) !== null, "should accept " + ok);
}
for (const bad of ['#fff"><script>', 'red;background:url(x)', 'expression(1)', '</style>', '', null, undefined, "var(--x); }"]) {
  assert.strictEqual(safeColor(bad), null, "should reject " + JSON.stringify(bad));
}
assert.strictEqual(safeColor("  #fff  "), "#fff", "trims");
`
	f := filepath.Join(t.TempDir(), "color.js")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("safeColor sanitization broken: %v\n%s", err, out)
	}
}

func TestEmbeddedAgentAssetsServe(t *testing.T) {
	assets, err := fs.Sub(webFS, "web/assets")
	if err != nil {
		t.Fatalf("sub web/assets: %v", err)
	}
	h := http.StripPrefix("/assets/", http.FileServer(http.FS(assets)))
	req := httptest.NewRequest("GET", "/assets/agents/openai.svg", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("asset code = %d body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Font Awesome") {
		t.Fatal("openai asset does not look like the vendored Font Awesome SVG")
	}
}

func TestStructuredApprovalDoesNotInventYN(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	i := strings.Index(html, `} else if (d.source === "acp") {`)
	if i < 0 {
		t.Fatal("structured approval branch not found")
	}
	j := strings.Index(html[i:], `} else {`)
	if j < 0 {
		t.Fatal("structured approval branch end not found")
	}
	branch := html[i : i+j]
	if strings.Contains(branch, `["y","n"]`) || strings.Contains(branch, `['y','n']`) {
		t.Fatal("structured approval branch must render protocol options only, not y/n fallback")
	}
}
