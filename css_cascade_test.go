package main

import (
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"testing"
	"testing/fstest"
)

// productionCSSFinalOrder is the exact final Phase 5 stylesheet cascade.
// Linked production stylesheets must be an exact prefix of this list while
// inline CSS remains, and must equal the full list once no inline style remains.
// accessibility.css, whenever present, is always the final linked file.
var productionCSSFinalOrder = []string{
	"/css/tokens.css",
	"/css/base.css",
	"/css/layout.css",
	"/css/cards.css",
	"/css/map.css",
	"/css/chat.css",
	"/css/sheets.css",
	"/css/notes.css",
	"/css/accessibility.css",
}

// cssCascadeSeparator joins successive CSS sources. It is not significant CSS
// and exists only so independent sources do not glue declarations together.
const cssCascadeSeparator = "\n"

// assembleCSSCascade concatenates every CSS source referenced by html in
// document order: the text of each <style> block and each same-origin
// rel=stylesheet link, read from fsys. Non-stylesheet <link> tags (for example
// the data-URI favicon) are ignored. External URLs, traversal paths, unexpected
// stylesheet roots, and missing or unreadable linked files return an error.
// Network resources are never fetched.
func assembleCSSCascade(html string, fsys fs.FS) (string, error) {
	sources, err := collectCSSSources(html)
	if err != nil {
		return "", err
	}
	if len(sources) == 0 {
		return "", nil
	}
	parts := make([]string, 0, len(sources))
	for _, src := range sources {
		switch src.kind {
		case cssSourceInline:
			parts = append(parts, src.text)
		case cssSourceLink:
			embedPath, err := stylesheetEmbedPath(src.href)
			if err != nil {
				return "", err
			}
			b, err := fs.ReadFile(fsys, embedPath)
			if err != nil {
				return "", fmt.Errorf("stylesheet %q unreadable at %s: %w", src.href, embedPath, err)
			}
			parts = append(parts, string(b))
		default:
			return "", fmt.Errorf("unknown CSS source kind %d", src.kind)
		}
	}
	return strings.Join(parts, cssCascadeSeparator), nil
}

// mustCSSCascade is the thin testing wrapper around assembleCSSCascade.
func mustCSSCascade(t *testing.T, html string, fsys fs.FS) string {
	t.Helper()
	css, err := assembleCSSCascade(html, fsys)
	if err != nil {
		t.Fatalf("assemble CSS cascade: %v", err)
	}
	return css
}

// mustProductionCSSCascade assembles the production cascade from the embedded
// index and webFS. Prefer this for CSS declaration/media-query assertions.
func mustProductionCSSCascade(t *testing.T) string {
	t.Helper()
	return mustCSSCascade(t, mustReadIndex(t), webFS)
}

type cssSourceKind int

const (
	cssSourceInline cssSourceKind = iota + 1
	cssSourceLink
)

type cssSource struct {
	kind cssSourceKind
	text string // inline only
	href string // link only
}

// collectCSSSources walks html in document order and records inline <style>
// blocks and stylesheet <link> tags. Attribute parsing is intentionally small
// and case-insensitive; it never fetches anything.
func collectCSSSources(html string) ([]cssSource, error) {
	var out []cssSource
	lower := strings.ToLower(html)
	i := 0
	for i < len(html) {
		styleAt := strings.Index(lower[i:], "<style")
		linkAt := strings.Index(lower[i:], "<link")
		if styleAt < 0 && linkAt < 0 {
			break
		}
		// Pick the earlier tag.
		useStyle := styleAt >= 0 && (linkAt < 0 || styleAt < linkAt)
		if useStyle {
			abs := i + styleAt
			// Require a real tag start: <style or <style> or <style ...>
			if abs+6 < len(lower) {
				c := lower[abs+6]
				if c != '>' && c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != '/' {
					i = abs + 6
					continue
				}
			}
			openEnd := strings.Index(lower[abs:], ">")
			if openEnd < 0 {
				return nil, fmt.Errorf("unterminated <style> open tag")
			}
			contentStart := abs + openEnd + 1
			closeRel := strings.Index(lower[contentStart:], "</style>")
			if closeRel < 0 {
				return nil, fmt.Errorf("unterminated <style> block")
			}
			out = append(out, cssSource{
				kind: cssSourceInline,
				text: html[contentStart : contentStart+closeRel],
			})
			i = contentStart + closeRel + len("</style>")
			continue
		}

		abs := i + linkAt
		if abs+5 < len(lower) {
			c := lower[abs+5]
			if c != '>' && c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != '/' {
				i = abs + 5
				continue
			}
		}
		openEnd := strings.Index(lower[abs:], ">")
		if openEnd < 0 {
			return nil, fmt.Errorf("unterminated <link> tag")
		}
		tag := html[abs : abs+openEnd+1]
		rel, hasRel := htmlAttr(tag, "rel")
		href, hasHref := htmlAttr(tag, "href")
		if hasRel && isStylesheetRel(rel) {
			if !hasHref || href == "" {
				return nil, fmt.Errorf("stylesheet <link> missing href: %s", tag)
			}
			out = append(out, cssSource{kind: cssSourceLink, href: href})
		}
		i = abs + openEnd + 1
	}
	return out, nil
}

func isStylesheetRel(rel string) bool {
	for _, part := range strings.Fields(strings.ToLower(rel)) {
		if part == "stylesheet" {
			return true
		}
	}
	return false
}

// htmlAttr returns a quoted attribute value from a single HTML tag string.
func htmlAttr(tag, name string) (string, bool) {
	lower := strings.ToLower(tag)
	key := strings.ToLower(name)
	// Search for name= with a word boundary before the name.
	for i := 0; i < len(lower); {
		j := strings.Index(lower[i:], key+"=")
		if j < 0 {
			return "", false
		}
		abs := i + j
		if abs > 0 {
			prev := lower[abs-1]
			if prev != ' ' && prev != '\t' && prev != '\n' && prev != '\r' && prev != '<' {
				i = abs + len(key)
				continue
			}
		}
		valStart := abs + len(key) + 1
		if valStart >= len(tag) {
			return "", false
		}
		q := tag[valStart]
		if q != '"' && q != '\'' {
			// Unquoted attributes are not used in production markup; reject.
			return "", false
		}
		rest := tag[valStart+1:]
		end := strings.IndexByte(rest, q)
		if end < 0 {
			return "", false
		}
		return rest[:end], true
	}
	return "", false
}

// stylesheetEmbedPath maps a same-origin stylesheet href to its path inside the
// production embed FS (web/css/...). External URLs, relative hrefs, traversal,
// and roots other than /css/<file>.css are rejected.
func stylesheetEmbedPath(href string) (string, error) {
	if href == "" {
		return "", fmt.Errorf("empty stylesheet href")
	}
	if strings.Contains(href, "://") || strings.HasPrefix(href, "//") {
		return "", fmt.Errorf("external stylesheet href %q", href)
	}
	if strings.ContainsAny(href, "?#") {
		return "", fmt.Errorf("stylesheet href must not carry query or fragment: %q", href)
	}
	if !strings.HasPrefix(href, "/") {
		return "", fmt.Errorf("stylesheet href must be an absolute path, got %q", href)
	}
	clean := path.Clean(href)
	if clean != href {
		return "", fmt.Errorf("stylesheet href is not a clean path: %q", href)
	}
	if !strings.HasPrefix(clean, "/css/") {
		return "", fmt.Errorf("unexpected stylesheet root %q", href)
	}
	base := strings.TrimPrefix(clean, "/css/")
	if base == "" || strings.Contains(base, "/") || !strings.HasSuffix(base, ".css") {
		return "", fmt.Errorf("unexpected stylesheet path %q", href)
	}
	// path.Clean already collapses ".."; a remaining ".." segment would only
	// appear if the basename itself were "..", which is not a .css file.
	if base == ".." || strings.Contains(base, "\\") {
		return "", fmt.Errorf("traversal stylesheet href %q", href)
	}
	return "web" + clean, nil
}

// linkedStylesheetHrefs returns stylesheet hrefs in document order.
func linkedStylesheetHrefs(html string) ([]string, error) {
	sources, err := collectCSSSources(html)
	if err != nil {
		return nil, err
	}
	var hrefs []string
	for _, src := range sources {
		if src.kind == cssSourceLink {
			hrefs = append(hrefs, src.href)
		}
	}
	return hrefs, nil
}

func hasInlineStyle(html string) bool {
	sources, err := collectCSSSources(html)
	if err != nil {
		return false
	}
	for _, src := range sources {
		if src.kind == cssSourceInline {
			return true
		}
	}
	return false
}

// --- cascade helper tests ----------------------------------------------------

func TestAssembleCSSCascadeInlineOnly(t *testing.T) {
	html := `<!doctype html><head>
<link rel="icon" href="data:image/svg+xml,<svg></svg>">
<style>/* a */ body{color:red}</style>
</head><body></body>`
	css, err := assembleCSSCascade(html, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "/* a */ body{color:red}"; css != want {
		t.Fatalf("inline-only cascade =\n%q\nwant\n%q", css, want)
	}
}

func TestAssembleCSSCascadeMixedDocumentOrder(t *testing.T) {
	fsys := fstest.MapFS{
		"web/css/tokens.css": &fstest.MapFile{Data: []byte("/* tokens */")},
		"web/css/base.css":   &fstest.MapFile{Data: []byte("/* base */")},
	}
	html := `<head>
<link rel="stylesheet" href="/css/tokens.css">
<style>/* mid */</style>
<link rel="icon" href="/favicon.ico">
<link href="/css/base.css" rel="stylesheet">
</head>`
	css, err := assembleCSSCascade(html, fsys)
	if err != nil {
		t.Fatal(err)
	}
	want := "/* tokens */" + cssCascadeSeparator + "/* mid */" + cssCascadeSeparator + "/* base */"
	if css != want {
		t.Fatalf("mixed cascade =\n%q\nwant\n%q", css, want)
	}
}

func TestAssembleCSSCascadeLinkedOnlyDocumentOrder(t *testing.T) {
	fsys := fstest.MapFS{
		"web/css/tokens.css": &fstest.MapFile{Data: []byte("T")},
		"web/css/base.css":   &fstest.MapFile{Data: []byte("B")},
		"web/css/layout.css": &fstest.MapFile{Data: []byte("L")},
	}
	html := `<link rel="stylesheet" href="/css/tokens.css">
<link rel="stylesheet" href="/css/base.css">
<link rel="stylesheet" href="/css/layout.css">`
	css, err := assembleCSSCascade(html, fsys)
	if err != nil {
		t.Fatal(err)
	}
	want := "T" + cssCascadeSeparator + "B" + cssCascadeSeparator + "L"
	if css != want {
		t.Fatalf("linked-only cascade =\n%q\nwant\n%q", css, want)
	}
}

func TestAssembleCSSCascadeIgnoresNonStylesheetLinks(t *testing.T) {
	html := `<link rel="icon" href="/favicon.ico">
<link rel="preload" href="/css/tokens.css" as="style">
<link rel="apple-touch-icon" href="/icon.png">
<style>x{}</style>`
	css, err := assembleCSSCascade(html, fstest.MapFS{
		"web/css/tokens.css": &fstest.MapFile{Data: []byte("SHOULD-NOT-LOAD")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if css != "x{}" {
		t.Fatalf("non-stylesheet links must be ignored; got %q", css)
	}
	if strings.Contains(css, "SHOULD-NOT-LOAD") {
		t.Fatal("preload link without rel=stylesheet must not load CSS")
	}
}

func TestAssembleCSSCascadeRejectsInvalidHrefs(t *testing.T) {
	fsys := fstest.MapFS{
		"web/css/tokens.css": &fstest.MapFile{Data: []byte("ok")},
	}
	cases := []struct {
		name string
		html string
	}{
		{"external-https", `<link rel="stylesheet" href="https://example.com/a.css">`},
		{"external-protocol-relative", `<link rel="stylesheet" href="//example.com/a.css">`},
		{"traversal", `<link rel="stylesheet" href="/css/../tokens.css">`},
		{"unexpected-root-js", `<link rel="stylesheet" href="/js/app.css">`},
		{"unexpected-root-assets", `<link rel="stylesheet" href="/assets/x.css">`},
		{"relative", `<link rel="stylesheet" href="css/tokens.css">`},
		{"nested", `<link rel="stylesheet" href="/css/sub/tokens.css">`},
		{"missing", `<link rel="stylesheet" href="/css/missing.css">`},
		{"query", `<link rel="stylesheet" href="/css/tokens.css?v=1">`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := assembleCSSCascade(tt.html, fsys)
			if err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
		})
	}
}

func TestCSSBlockReadsAssembledCascade(t *testing.T) {
	// Synthetic production-shaped document: CSS only in a linked file, not in
	// the raw HTML body text that old cssBlock searched.
	fsys := fstest.MapFS{
		"web/css/tokens.css": &fstest.MapFile{Data: []byte(":root { --peek: clamp(44px, 12%, 60px); }\n")},
	}
	html := `<!doctype html><head>
<link rel="stylesheet" href="/css/tokens.css">
</head><body><div id="root">no css here --peek:</div></body>`
	cascade, err := assembleCSSCascade(html, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, ":root { --peek:") {
		t.Fatal("test setup broken: rule must not appear in raw HTML")
	}
	block := cssBlock(t, cascade, ":root {")
	if !strings.Contains(block, "--peek: clamp(44px, 12%, 60px);") {
		t.Fatalf("cssBlock must read the assembled cascade; got %q", block)
	}
	// Production helper path: still works for the current inline-only document.
	prod := mustProductionCSSCascade(t)
	root := cssBlock(t, prod, ":root {")
	if !strings.Contains(root, "--peek:") {
		t.Fatalf("production cascade missing --peek; got %q", root)
	}
}

// --- static CSS transition contract -----------------------------------------

func TestProductionCSSTransitionContract(t *testing.T) {
	html := mustReadIndex(t)
	cascade := mustCSSCascade(t, html, webFS)

	hrefs, err := linkedStylesheetHrefs(html)
	if err != nil {
		t.Fatal(err)
	}
	inline := hasInlineStyle(html)

	// Transition rule: linked list is a prefix of the final order while inline
	// CSS remains; once inline is gone it must equal the complete final list.
	if inline {
		if len(hrefs) > len(productionCSSFinalOrder) {
			t.Fatalf("linked stylesheets (%d) exceed final order (%d)", len(hrefs), len(productionCSSFinalOrder))
		}
		for i, href := range hrefs {
			if href != productionCSSFinalOrder[i] {
				t.Fatalf("linked[%d]=%q, want prefix of final order %q", i, href, productionCSSFinalOrder[i])
			}
		}
	} else {
		if len(hrefs) != len(productionCSSFinalOrder) {
			t.Fatalf("no inline style remains: linked count=%d, want full final order %d", len(hrefs), len(productionCSSFinalOrder))
		}
		for i, href := range hrefs {
			if href != productionCSSFinalOrder[i] {
				t.Fatalf("linked[%d]=%q, want %q", i, href, productionCSSFinalOrder[i])
			}
		}
	}

	// accessibility.css, whenever present among links, can only be last.
	for i, href := range hrefs {
		if href == "/css/accessibility.css" && i != len(hrefs)-1 {
			t.Fatalf("accessibility.css must be the final linked stylesheet; index %d of %d", i, len(hrefs))
		}
	}

	if strings.Contains(strings.ToLower(cascade), "@import") {
		t.Fatal("assembled production CSS must not contain @import")
	}

	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))

	// Currently linked sheets (none in 5A): each must serve exact embedded
	// bytes with CSS content type, no-store, and nosniff.
	for _, href := range hrefs {
		t.Run("serve"+href, func(t *testing.T) {
			embedPath, err := stylesheetEmbedPath(href)
			if err != nil {
				t.Fatal(err)
			}
			want, err := fs.ReadFile(webFS, embedPath)
			if err != nil {
				t.Fatalf("linked stylesheet %s missing from embed: %v", href, err)
			}
			rec := getCharacterization(t, h, href)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status=%d, want 200; body=%q", href, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
				t.Fatalf("GET %s Content-Type=%q, want text/css", href, got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("GET %s Cache-Control=%q, want no-store", href, got)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("GET %s X-Content-Type-Options=%q, want nosniff", href, got)
			}
			if rec.Body.String() != string(want) {
				t.Fatalf("GET %s body differs from embedded %s", href, embedPath)
			}
		})
	}

	// Target files not yet linked/embedded remain unavailable. /css/ itself
	// must not expose a directory listing.
	for _, path := range append([]string{"/css/"}, productionCSSFinalOrder...) {
		// Skip any that are currently linked and therefore must 200 above.
		linked := false
		for _, href := range hrefs {
			if href == path {
				linked = true
				break
			}
		}
		if linked {
			continue
		}
		t.Run("unavailable"+path, func(t *testing.T) {
			rec := getCharacterization(t, h, path)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s status=%d, want 404 while unlinked", path, rec.Code)
			}
			if bodyLooksLikeDirectoryListing(rec.Body.String()) {
				t.Fatalf("GET %s exposed a directory listing", path)
			}
			assertNotIndexOrSVG(t, path, rec.Body.Bytes())
		})
	}
}

// TestProductionCSSNegativeRootsPreserved keeps the existing static negative
// surface green alongside the transition contract without weakening it.
func TestProductionCSSNegativeRootsPreserved(t *testing.T) {
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))
	for _, path := range []string{
		"/css/",
		"/css/app.css",
		"/js/",
		"/js/app.js",
		"/index.html",
		"/web/index.html",
		"/package.json",
		"/web/package.json",
		"/test/smoke.test.js",
		"/web/test/smoke.test.js",
	} {
		t.Run(path, func(t *testing.T) {
			rec := getCharacterization(t, h, path)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s status=%d, want 404; body=%q", path, rec.Code, rec.Body.String())
			}
			assertNotIndexOrSVG(t, path, rec.Body.Bytes())
			if bodyLooksLikeDirectoryListing(rec.Body.String()) {
				t.Fatalf("GET %s exposed a directory listing", path)
			}
		})
	}
}

// --- Packet 5B: tokens + base extraction ------------------------------------

// TestProductionTokensAndBaseCSSOwnership locks the 5B extraction boundary:
// tokens.css and base.css are the first two linked sheets; token and reset
// rules live only in those files; the surviving inline style begins at
// statusbar; cascade order is tokens → base → statusbar.
func TestProductionTokensAndBaseCSSOwnership(t *testing.T) {
	html := mustReadIndex(t)
	hrefs, err := linkedStylesheetHrefs(html)
	if err != nil {
		t.Fatal(err)
	}
	if len(hrefs) < 2 {
		t.Fatalf("linked stylesheets = %v, want at least tokens and base", hrefs)
	}
	if hrefs[0] != "/css/tokens.css" || hrefs[1] != "/css/base.css" {
		t.Fatalf("first two linked stylesheets = %q, %q; want /css/tokens.css, /css/base.css", hrefs[0], hrefs[1])
	}
	tokens, err := fs.ReadFile(webFS, "web/css/tokens.css")
	if err != nil {
		t.Fatalf("tokens.css missing from embed: %v", err)
	}
	base, err := fs.ReadFile(webFS, "web/css/base.css")
	if err != nil {
		t.Fatalf("base.css missing from embed: %v", err)
	}
	tok := string(tokens)
	bas := string(base)

	// tokens.css: complete light/dark custom properties including --peek.
	for _, sig := range []string{
		"/* ---------- design tokens",
		":root {",
		"--peek:",
		"clamp(44px, 12%, 60px)",
		"@media (prefers-color-scheme: dark)",
		"--bg:",
		"--surface:",
		"--ink:",
		"--sbh:",
	} {
		if !strings.Contains(tok, sig) {
			t.Fatalf("tokens.css missing signature %q", sig)
		}
	}
	// base.css: only the original reset/global element rules.
	for _, sig := range []string{
		"* { box-sizing: border-box;",
		"html, body { height: 100%;",
		"body {",
		"button { font: inherit;",
	} {
		if !strings.Contains(bas, sig) {
			t.Fatalf("base.css missing signature %q", sig)
		}
	}
	// base must not absorb design tokens or later component sections.
	for _, bad := range []string{
		"/* ---------- design tokens",
		"--peek:",
		"/* ---------- statusbar ---------- */",
		"#statusbar",
		"/* ---------- search",
	} {
		if strings.Contains(bas, bad) {
			t.Fatalf("base.css must not contain %q", bad)
		}
	}
	// tokens must not absorb element reset rules or later sections.
	for _, bad := range []string{
		"* { box-sizing: border-box;",
		"button { font: inherit;",
		"/* ---------- statusbar ---------- */",
		"#statusbar",
	} {
		if strings.Contains(tok, bad) {
			t.Fatalf("tokens.css must not contain %q", bad)
		}
	}

	// Surviving inline style begins at the statusbar section.
	sources, err := collectCSSSources(html)
	if err != nil {
		t.Fatal(err)
	}
	var inline string
	for _, src := range sources {
		if src.kind == cssSourceInline {
			inline = src.text
			break
		}
	}
	// While 5B is the current two-link prefix, the surviving inline style must
	// begin exactly at statusbar. Later packets extend the prefix and eventually
	// remove the inline source without weakening the permanent tokens/base
	// ownership assertions below.
	if len(hrefs) == 2 {
		if inline == "" {
			t.Fatal("5B two-link state must retain the inline style")
		}
		trimmed := strings.TrimLeft(inline, "\n\r\t ")
		if !strings.HasPrefix(trimmed, "/* ---------- statusbar ---------- */") {
			head := trimmed
			if len(head) > 80 {
				head = head[:80]
			}
			t.Fatalf("5B inline style must begin at statusbar section; starts with %q", head)
		}
	}

	// Moved token/base signatures must no longer remain inline.
	for _, moved := range []string{
		"/* ---------- design tokens",
		"--peek:",
		"* { box-sizing: border-box;",
		"button { font: inherit;",
	} {
		if strings.Contains(inline, moved) {
			t.Fatalf("moved signature %q still present in inline style", moved)
		}
	}

	// Assembled cascade order: tokens → base → statusbar (inline).
	cascade := mustCSSCascade(t, html, webFS)
	ti := strings.Index(cascade, "/* ---------- design tokens")
	bi := strings.Index(cascade, "* { box-sizing: border-box;")
	si := strings.Index(cascade, "/* ---------- statusbar ---------- */")
	if ti < 0 || bi < 0 || si < 0 {
		t.Fatalf("cascade missing tokens/base/statusbar markers: ti=%d bi=%d si=%d", ti, bi, si)
	}
	if !(ti < bi && bi < si) {
		t.Fatalf("cascade order wrong: tokens@%d base@%d statusbar@%d (want tokens < base < statusbar)", ti, bi, si)
	}
}

// TestProductionTokensAndBaseCSSServing activates the 5A exact-byte, content-
// type, cache, nosniff, and negative-path contracts for the two linked files.
func TestProductionTokensAndBaseCSSServing(t *testing.T) {
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))

	for _, href := range []string{"/css/tokens.css", "/css/base.css"} {
		t.Run("ok"+href, func(t *testing.T) {
			embedPath, err := stylesheetEmbedPath(href)
			if err != nil {
				t.Fatal(err)
			}
			want, err := fs.ReadFile(webFS, embedPath)
			if err != nil {
				t.Fatalf("read embed %s: %v", embedPath, err)
			}
			rec := getCharacterization(t, h, href)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status=%d, want 200; body=%q", href, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
				t.Fatalf("GET %s Content-Type=%q, want %q", href, got, "text/css; charset=utf-8")
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("GET %s Cache-Control=%q, want no-store", href, got)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("GET %s X-Content-Type-Options=%q, want nosniff", href, got)
			}
			if rec.Body.String() != string(want) {
				t.Fatalf("GET %s body differs from embedded %s (%d vs %d bytes)", href, embedPath, rec.Body.Len(), len(want))
			}
		})
	}

	// Negatives: root, missing, nested, encoded traversal, non-CSS, and raw
	// /web/css/... paths. Never a directory listing or CSS body.
	for _, path := range []string{
		"/css/",
		"/css/missing.css",
		"/css/sub/tokens.css",
		"/css/%2e%2e/tokens.css",
		"/css/%2e%2e/index.html",
		"/css/tokens.txt",
		"/css/tokens.css.bak",
		"/css/readme.md",
		"/web/css/tokens.css",
		"/web/css/base.css",
		"/web/css/",
	} {
		t.Run("404"+path, func(t *testing.T) {
			rec := getCharacterization(t, h, path)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s status=%d, want 404; body=%q", path, rec.Code, rec.Body.String())
			}
			if bodyLooksLikeDirectoryListing(rec.Body.String()) {
				t.Fatalf("GET %s exposed a directory listing", path)
			}
			assertNotIndexOrSVG(t, path, rec.Body.Bytes())
		})
	}

	// Bare ".." segments are cleaned by net/http.ServeMux into a permanent
	// redirect before any route handler runs. That must never surface CSS
	// bytes; following the redirect also fails closed (no /tokens.css route).
	t.Run("traversal-dotdot-not-served", func(t *testing.T) {
		rec := getCharacterization(t, h, "/css/../tokens.css")
		if rec.Code == http.StatusOK {
			t.Fatalf("GET /css/../tokens.css status=200, want non-OK; body=%q", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "--peek:") || strings.Contains(rec.Body.String(), "box-sizing: border-box") {
			t.Fatalf("traversal path served CSS content: status=%d body=%q", rec.Code, rec.Body.String())
		}
		if bodyLooksLikeDirectoryListing(rec.Body.String()) {
			t.Fatal("traversal path exposed a directory listing")
		}
	})
}

// --- Packet 5C: layout + cards + map extraction ------------------------------

// fiveLinkCSSPrefix is the permanent linked stylesheet prefix established by
// Packet 5C (tokens → base → layout → cards → map). Later packets may extend
// the prefix further; ownership tests that require exactly these five links
// must condition on this length so they stay valid after 5D–5F.
var fiveLinkCSSPrefix = []string{
	"/css/tokens.css",
	"/css/base.css",
	"/css/layout.css",
	"/css/cards.css",
	"/css/map.css",
}

// sevenLinkCSSPrefix is the permanent linked stylesheet prefix established by
// Packet 5D (tokens → base → layout → cards → map → chat → sheets). Later
// packets may extend the prefix further; ownership tests that require exactly
// these seven links must condition on this length so they stay valid after 5E–5F.
var sevenLinkCSSPrefix = []string{
	"/css/tokens.css",
	"/css/base.css",
	"/css/layout.css",
	"/css/cards.css",
	"/css/map.css",
	"/css/chat.css",
	"/css/sheets.css",
}

// TestProductionLayoutCardsMapCSSOwnership locks the 5C extraction boundary:
// layout, cards, and map are the third–fifth linked sheets; each file owns its
// original contiguous section; moved signatures are absent from the surviving
// inline source; and cascade order is tokens → base → layout → cards → map → chat.
func TestProductionLayoutCardsMapCSSOwnership(t *testing.T) {
	html := mustReadIndex(t)
	hrefs, err := linkedStylesheetHrefs(html)
	if err != nil {
		t.Fatal(err)
	}
	if len(hrefs) < 5 {
		t.Fatalf("linked stylesheets = %v, want at least the five-link prefix %v", hrefs, fiveLinkCSSPrefix)
	}
	for i, want := range fiveLinkCSSPrefix {
		if hrefs[i] != want {
			t.Fatalf("linked[%d]=%q, want %q (five-link prefix %v)", i, hrefs[i], want, fiveLinkCSSPrefix)
		}
	}

	layout, err := fs.ReadFile(webFS, "web/css/layout.css")
	if err != nil {
		t.Fatalf("layout.css missing from embed: %v", err)
	}
	cards, err := fs.ReadFile(webFS, "web/css/cards.css")
	if err != nil {
		t.Fatalf("cards.css missing from embed: %v", err)
	}
	mapCSS, err := fs.ReadFile(webFS, "web/css/map.css")
	if err != nil {
		t.Fatalf("map.css missing from embed: %v", err)
	}
	lay := string(layout)
	car := string(cards)
	mp := string(mapCSS)

	// layout.css: statusbar, search, app-shell, pane transition, scrim, heading, tabs.
	for _, sig := range []string{
		"/* ---------- statusbar ---------- */",
		"#statusbar",
		"/* ---------- search overlay:",
		"#searchoverlay",
		"#searchscrim",
		"/* ---------- app shell:",
		"#app {",
		"#cards {",
		"#map {",
		"transform: translateX(-103%);",
		"#scrim {",
		".phead {",
		"/* ---------- browser-style tab rows",
		".tabs {",
	} {
		if !strings.Contains(lay, sig) {
			t.Fatalf("layout.css missing signature %q", sig)
		}
	}
	// cards.css: card states, pinned/attention/working, actions, fold, empty.
	for _, sig := range []string{
		"/* ---------- cards ---------- */",
		".card {",
		".card.attention {",
		".card.working {",
		".card .actions",
		".card .pinflag",
		"@keyframes attentionPulse",
		".attnfold {",
		".empty { padding: 60px 20px;",
	} {
		if !strings.Contains(car, sig) {
			t.Fatalf("cards.css missing signature %q", sig)
		}
	}
	// map.css: metro, lane, station, stop, wall-map, map-toolbar.
	for _, sig := range []string{
		"/* ---------- metro map (journeys = fork trees, stations = activities) ---------- */",
		"#mapscroll",
		".lhead {",
		".lbody {",
		".strow {",
		".strow.stoprow",
		"/* wall map:",
		"#maptoolbar",
		"body.map-full #maptoolbar.on",
		".lanechip",
	} {
		if !strings.Contains(mp, sig) {
			t.Fatalf("map.css missing signature %q", sig)
		}
	}

	// Cross-file rejection: each new file rejects section markers and
	// representative signatures owned by the other two files and by deferred
	// chat/sheets/notes concerns. (A few cross-root selectors such as
	// #chathead .dot.active historically lived inside the cards block; those
	// travel with their original contiguous section and are not rejections.)
	layoutReject := []string{
		"/* ---------- cards ---------- */",
		".card.attention {",
		"/* ---------- metro map (journeys = fork trees, stations = activities) ---------- */",
		"#maptoolbar",
		"/* ---------- chat ---------- */",
		"/* ---------- sheets ---------- */",
		".sheet {",
		"/* ---------- sticky-notes pane:",
		"/* ---------- Notes workspace:",
	}
	for _, bad := range layoutReject {
		if strings.Contains(lay, bad) {
			t.Fatalf("layout.css must not contain %q", bad)
		}
	}
	cardsReject := []string{
		"/* ---------- statusbar ---------- */",
		"/* ---------- search overlay:",
		"/* ---------- app shell:",
		"/* ---------- browser-style tab rows",
		"/* ---------- metro map (journeys = fork trees, stations = activities) ---------- */",
		"#maptoolbar",
		"/* ---------- chat ---------- */",
		"/* ---------- sheets ---------- */",
		"/* ---------- sticky-notes pane:",
		"/* ---------- Notes workspace:",
	}
	for _, bad := range cardsReject {
		if strings.Contains(car, bad) {
			t.Fatalf("cards.css must not contain %q", bad)
		}
	}
	mapReject := []string{
		"/* ---------- statusbar ---------- */",
		"/* ---------- cards ---------- */",
		".card.attention {",
		"/* ---------- chat ---------- */",
		"/* ---------- sheets ---------- */",
		"/* ---------- sticky-notes pane:",
		"/* ---------- Notes workspace:",
		"#bookmarkspane",
	}
	for _, bad := range mapReject {
		if strings.Contains(mp, bad) {
			t.Fatalf("map.css must not contain %q", bad)
		}
	}

	// Surviving inline style (while present).
	sources, err := collectCSSSources(html)
	if err != nil {
		t.Fatal(err)
	}
	var inline string
	for _, src := range sources {
		if src.kind == cssSourceInline {
			inline = src.text
			break
		}
	}

	// Five-link state: nonempty inline begins exactly at the chat section.
	// Later packets extend the prefix and eventually remove the inline source
	// without weakening the permanent ownership assertions above.
	if len(hrefs) == 5 {
		if inline == "" {
			t.Fatal("5C five-link state must retain the inline style")
		}
		trimmed := strings.TrimLeft(inline, "\n\r\t ")
		if !strings.HasPrefix(trimmed, "/* ---------- chat ---------- */") {
			head := trimmed
			if len(head) > 80 {
				head = head[:80]
			}
			t.Fatalf("5C inline style must begin at chat section; starts with %q", head)
		}
	}

	// Moved layout/cards/map signatures must no longer remain inline.
	for _, moved := range []string{
		"/* ---------- statusbar ---------- */",
		"/* ---------- search overlay:",
		"/* ---------- app shell:",
		"/* ---------- browser-style tab rows",
		"/* ---------- cards ---------- */",
		".card.attention {",
		"/* ---------- metro map (journeys = fork trees, stations = activities) ---------- */",
		"#maptoolbar {",
	} {
		if strings.Contains(inline, moved) {
			t.Fatalf("moved signature %q still present in inline style", moved)
		}
	}

	// Assembled cascade order: tokens → base → layout → cards → map → chat.
	cascade := mustCSSCascade(t, html, webFS)
	markers := []struct {
		name string
		sig  string
	}{
		{"tokens", "/* ---------- design tokens"},
		{"base", "* { box-sizing: border-box;"},
		{"layout", "/* ---------- statusbar ---------- */"},
		{"cards", "/* ---------- cards ---------- */"},
		{"map", "/* ---------- metro map (journeys = fork trees, stations = activities) ---------- */"},
		{"chat", "/* ---------- chat ---------- */"},
	}
	prev := -1
	prevName := ""
	for _, m := range markers {
		idx := strings.Index(cascade, m.sig)
		if idx < 0 {
			t.Fatalf("cascade missing %s marker %q", m.name, m.sig)
		}
		if prev >= 0 && !(prev < idx) {
			t.Fatalf("cascade order wrong: %s@%d must precede %s@%d", prevName, prev, m.name, idx)
		}
		prev = idx
		prevName = m.name
	}
}

// TestProductionLayoutCardsMapCSSServing activates the 5A exact-byte, content-
// type, cache, nosniff, and negative-path contracts for the three new linked files.
func TestProductionLayoutCardsMapCSSServing(t *testing.T) {
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))

	for _, href := range []string{"/css/layout.css", "/css/cards.css", "/css/map.css"} {
		t.Run("ok"+href, func(t *testing.T) {
			embedPath, err := stylesheetEmbedPath(href)
			if err != nil {
				t.Fatal(err)
			}
			want, err := fs.ReadFile(webFS, embedPath)
			if err != nil {
				t.Fatalf("read embed %s: %v", embedPath, err)
			}
			rec := getCharacterization(t, h, href)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status=%d, want 200; body=%q", href, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
				t.Fatalf("GET %s Content-Type=%q, want %q", href, got, "text/css; charset=utf-8")
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("GET %s Cache-Control=%q, want no-store", href, got)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("GET %s X-Content-Type-Options=%q, want nosniff", href, got)
			}
			if rec.Body.String() != string(want) {
				t.Fatalf("GET %s body differs from embedded %s (%d vs %d bytes)", href, embedPath, rec.Body.Len(), len(want))
			}
		})
	}

	for _, path := range []string{
		"/css/",
		"/css/missing.css",
		"/css/sub/layout.css",
		"/css/%2e%2e/layout.css",
		"/css/layout.txt",
		"/css/layout.css.bak",
		"/web/css/layout.css",
		"/web/css/cards.css",
		"/web/css/map.css",
		"/web/css/",
	} {
		t.Run("404"+path, func(t *testing.T) {
			rec := getCharacterization(t, h, path)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s status=%d, want 404; body=%q", path, rec.Code, rec.Body.String())
			}
			if bodyLooksLikeDirectoryListing(rec.Body.String()) {
				t.Fatalf("GET %s exposed a directory listing", path)
			}
			assertNotIndexOrSVG(t, path, rec.Body.Bytes())
		})
	}
}

// --- Packet 5D: chat + sheets extraction -------------------------------------

// TestProductionChatSheetsCSSOwnership locks the 5D extraction boundary: chat
// and sheets are the sixth–seventh linked sheets; each file owns its original
// contiguous section (including historical cross-root selectors that lived
// inside those blocks); moved signatures are absent from the surviving inline
// source; and cascade order is tokens → base → layout → cards → map → chat →
// sheets → sticky-notes.
func TestProductionChatSheetsCSSOwnership(t *testing.T) {
	html := mustReadIndex(t)
	hrefs, err := linkedStylesheetHrefs(html)
	if err != nil {
		t.Fatal(err)
	}
	if len(hrefs) < 7 {
		t.Fatalf("linked stylesheets = %v, want at least the seven-link prefix %v", hrefs, sevenLinkCSSPrefix)
	}
	for i, want := range sevenLinkCSSPrefix {
		if hrefs[i] != want {
			t.Fatalf("linked[%d]=%q, want %q (seven-link prefix %v)", i, hrefs[i], want, sevenLinkCSSPrefix)
		}
	}

	chatCSS, err := fs.ReadFile(webFS, "web/css/chat.css")
	if err != nil {
		t.Fatalf("chat.css missing from embed: %v", err)
	}
	sheetsCSS, err := fs.ReadFile(webFS, "web/css/sheets.css")
	if err != nil {
		t.Fatalf("sheets.css missing from embed: %v", err)
	}
	ch := string(chatCSS)
	sh := string(sheetsCSS)

	// chat.css: head/details, gauge, messages/turns/history, work pulse,
	// conversation tools, Markdown + attachments, peek/key row, prompt/composer,
	// attachment menu/staging. Historical .sheet validation and Notes-workspace
	// Markdown selectors that lived in this contiguous block stay here.
	for _, sig := range []string{
		"/* ---------- chat ---------- */",
		"#chathead {",
		"#chatdetails {",
		"#gauge {",
		"#msgs {",
		".turn {",
		".turn .bubble {",
		".histload {",
		".chatseam {",
		"#workpulse {",
		"#convtools {",
		".bubble p {",
		".wssecrender p, .wsrefbody p, .wsibubble p {",
		".attrow {",
		".attthumb {",
		".peekblock {",
		".peek {",
		"#keyrow {",
		"#promptbar {",
		"#sendbtn {",
		"#attadd {",
		"#attmenu {",
		"#attstage {",
		".stagechip {",
		".fielderr {",
		".sheet [aria-invalid=\"true\"]",
		"#syncwarn {",
	} {
		if !strings.Contains(ch, sig) {
			t.Fatalf("chat.css missing signature %q", sig)
		}
	}
	// sheets.css: sheet primitives, form rows/fields/CTAs, edit/new-node and
	// menu/license rules, dialog positioning, backdrop.
	for _, sig := range []string{
		"/* ---------- sheets ---------- */",
		".sheet {",
		".sheet.open {",
		".sheet .grab {",
		".sheet .sect {",
		".sheet .row2 {",
		".sheet textarea, .sheet select, .sheet input {",
		".sheet .cta {",
		".sheet .item {",
		"#newchat.editing .nc-create-only",
		"#menu .lic",
		"#m_lictext",
		"#backdrop {",
		"#backdrop.on {",
	} {
		if !strings.Contains(sh, sig) {
			t.Fatalf("sheets.css missing signature %q", sig)
		}
	}

	// Cross-file rejection. chat.css must not absorb the sheets section marker
	// or deferred sticky-notes / Notes workspace sections / unrelated layout
	// cards/map section markers. Documented historical .sheet validation
	// selectors inside chat are allowed — do not reject bare ".sheet".
	chatReject := []string{
		"/* ---------- sheets ---------- */",
		"/* ---------- sticky-notes pane:",
		"/* ---------- Notes workspace:",
		"/* ---------- statusbar ---------- */",
		"/* ---------- cards ---------- */",
		"/* ---------- metro map (journeys = fork trees, stations = activities) ---------- */",
		"#maptoolbar",
		".sheet.open {",
		"#backdrop {",
	}
	for _, bad := range chatReject {
		if strings.Contains(ch, bad) {
			t.Fatalf("chat.css must not contain %q", bad)
		}
	}
	// sheets.css must not absorb the chat marker or deferred notes sections.
	sheetsReject := []string{
		"/* ---------- chat ---------- */",
		"#chathead {",
		"#promptbar {",
		"/* ---------- sticky-notes pane:",
		"/* ---------- Notes workspace:",
		"/* ---------- statusbar ---------- */",
		"/* ---------- cards ---------- */",
		"/* ---------- metro map (journeys = fork trees, stations = activities) ---------- */",
	}
	for _, bad := range sheetsReject {
		if strings.Contains(sh, bad) {
			t.Fatalf("sheets.css must not contain %q", bad)
		}
	}

	// Surviving inline style (while present).
	sources, err := collectCSSSources(html)
	if err != nil {
		t.Fatal(err)
	}
	var inline string
	for _, src := range sources {
		if src.kind == cssSourceInline {
			inline = src.text
			break
		}
	}

	// Seven-link state only: nonempty inline begins exactly at sticky-notes.
	// Later packets extend the prefix and eventually remove the inline source
	// without weakening the permanent ownership assertions above.
	if len(hrefs) == 7 {
		if inline == "" {
			t.Fatal("5D seven-link state must retain the inline style")
		}
		trimmed := strings.TrimLeft(inline, "\n\r\t ")
		if !strings.HasPrefix(trimmed, "/* ---------- sticky-notes pane:") {
			head := trimmed
			if len(head) > 80 {
				head = head[:80]
			}
			t.Fatalf("5D inline style must begin at sticky-notes section; starts with %q", head)
		}
	}

	// Moved chat/sheets signatures must no longer remain inline.
	// Prefer section markers and rules unique to the extracted contiguous
	// blocks — later desktop override / shared-chrome rules still mention
	// .sheet/.sheet.open/.sheet .grab and must not be treated as residual
	// chat/sheets source.
	for _, moved := range []string{
		"/* ---------- chat ---------- */",
		"#chathead {",
		"#promptbar {",
		"#attmenu {",
		"#workpulse {",
		"#keyrow {",
		"/* ---------- sheets ---------- */",
		"#backdrop {",
		"#backdrop.on {",
		"#m_lictext {",
		"#m_check {",
	} {
		if strings.Contains(inline, moved) {
			t.Fatalf("moved signature %q still present in inline style", moved)
		}
	}

	// Assembled cascade order: tokens → base → layout → cards → map → chat →
	// sheets → sticky-notes.
	cascade := mustCSSCascade(t, html, webFS)
	markers := []struct {
		name string
		sig  string
	}{
		{"tokens", "/* ---------- design tokens"},
		{"base", "* { box-sizing: border-box;"},
		{"layout", "/* ---------- statusbar ---------- */"},
		{"cards", "/* ---------- cards ---------- */"},
		{"map", "/* ---------- metro map (journeys = fork trees, stations = activities) ---------- */"},
		{"chat", "/* ---------- chat ---------- */"},
		{"sheets", "/* ---------- sheets ---------- */"},
		{"sticky-notes", "/* ---------- sticky-notes pane:"},
	}
	prev := -1
	prevName := ""
	for _, m := range markers {
		idx := strings.Index(cascade, m.sig)
		if idx < 0 {
			t.Fatalf("cascade missing %s marker %q", m.name, m.sig)
		}
		if prev >= 0 && !(prev < idx) {
			t.Fatalf("cascade order wrong: %s@%d must precede %s@%d", prevName, prev, m.name, idx)
		}
		prev = idx
		prevName = m.name
	}
}

// TestProductionChatSheetsCSSServing activates the 5A exact-byte, content-
// type, cache, nosniff, and negative-path contracts for the two new linked files.
func TestProductionChatSheetsCSSServing(t *testing.T) {
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))

	for _, href := range []string{"/css/chat.css", "/css/sheets.css"} {
		t.Run("ok"+href, func(t *testing.T) {
			embedPath, err := stylesheetEmbedPath(href)
			if err != nil {
				t.Fatal(err)
			}
			want, err := fs.ReadFile(webFS, embedPath)
			if err != nil {
				t.Fatalf("read embed %s: %v", embedPath, err)
			}
			rec := getCharacterization(t, h, href)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status=%d, want 200; body=%q", href, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
				t.Fatalf("GET %s Content-Type=%q, want %q", href, got, "text/css; charset=utf-8")
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("GET %s Cache-Control=%q, want no-store", href, got)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("GET %s X-Content-Type-Options=%q, want nosniff", href, got)
			}
			if rec.Body.String() != string(want) {
				t.Fatalf("GET %s body differs from embedded %s (%d vs %d bytes)", href, embedPath, rec.Body.Len(), len(want))
			}
		})
	}

	for _, path := range []string{
		"/css/",
		"/css/missing.css",
		"/css/sub/chat.css",
		"/css/%2e%2e/chat.css",
		"/css/chat.txt",
		"/css/chat.css.bak",
		"/web/css/chat.css",
		"/web/css/sheets.css",
		"/web/css/",
	} {
		t.Run("404"+path, func(t *testing.T) {
			rec := getCharacterization(t, h, path)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s status=%d, want 404; body=%q", path, rec.Code, rec.Body.String())
			}
			if bodyLooksLikeDirectoryListing(rec.Body.String()) {
				t.Fatalf("GET %s exposed a directory listing", path)
			}
			assertNotIndexOrSVG(t, path, rec.Body.Bytes())
		})
	}
}

// --- Packet 5E: notes + accessibility extraction -----------------------------

// nineLinkCSSOrder is the permanent complete Phase 5 stylesheet list established
// by Packet 5E. Ownership tests require this exact order (not merely a prefix).
var nineLinkCSSOrder = []string{
	"/css/tokens.css",
	"/css/base.css",
	"/css/layout.css",
	"/css/cards.css",
	"/css/map.css",
	"/css/chat.css",
	"/css/sheets.css",
	"/css/notes.css",
	"/css/accessibility.css",
}

// TestProductionNotesAccessibilityCSSOwnership locks the 5E extraction
// boundary: the complete nine-link order with notes eighth and accessibility
// last; notes.css owns Bookmarks/Notes/desktop/peek rules; layout no longer
// owns #bookmarkpeek; accessibility.css owns only the cross-component pane
// reduced-motion override; cascade order and empty-inline 5E state hold.
func TestProductionNotesAccessibilityCSSOwnership(t *testing.T) {
	html := mustReadIndex(t)
	hrefs, err := linkedStylesheetHrefs(html)
	if err != nil {
		t.Fatal(err)
	}
	if len(hrefs) != len(nineLinkCSSOrder) {
		t.Fatalf("linked stylesheets = %v, want exact nine-link order %v", hrefs, nineLinkCSSOrder)
	}
	for i, want := range nineLinkCSSOrder {
		if hrefs[i] != want {
			t.Fatalf("linked[%d]=%q, want %q (exact nine-link order %v)", i, hrefs[i], want, nineLinkCSSOrder)
		}
	}
	if hrefs[len(hrefs)-1] != "/css/accessibility.css" {
		t.Fatalf("accessibility.css must be the final stylesheet; got %q", hrefs[len(hrefs)-1])
	}

	notesCSS, err := fs.ReadFile(webFS, "web/css/notes.css")
	if err != nil {
		t.Fatalf("notes.css missing from embed: %v", err)
	}
	a11yCSS, err := fs.ReadFile(webFS, "web/css/accessibility.css")
	if err != nil {
		t.Fatalf("accessibility.css missing from embed: %v", err)
	}
	layoutCSS, err := fs.ReadFile(webFS, "web/css/layout.css")
	if err != nil {
		t.Fatalf("layout.css missing from embed: %v", err)
	}
	notes := string(notesCSS)
	a11y := string(a11yCSS)
	layout := string(layoutCSS)

	// notes.css: Bookmarks pane, peek ownership, flags/tabs/list/composer,
	// desktop peek suppression and column overrides, full-cover Notes workspace,
	// scrim/panel/header/nav/editor, inbox/refs/placement, clamps, menus,
	// empty states, narrow single-zone and wider three-zone layouts.
	for _, sig := range []string{
		"/* ---------- sticky-notes pane:",
		"#bookmarkspane {",
		"body.bookmarks-open #bookmarkspane { transform: translateX(0); }",
		"/* the Bookmarks pane's left peek, as a tap-catcher:",
		"#bookmarkpeek {",
		"body.bookmarks-open #bookmarkpeek { display: block; }",
		"#notesbtn {",
		"#bookmarkflags {",
		"#bookmarktabs {",
		"#bookmarklist {",
		".bookmark {",
		".bookmark .nbubble {",
		".bookmarkactions {",
		"#bookmarkbar {",
		"#bookmarkprompt {",
		"#bookmarksend {",
		"#bookmarkpeek, body.bookmarks-open #bookmarkpeek { display: none; }",
		"/* ---------- desktop / iPad: columns ---------- */",
		"#statusbar, #cards, #map, .phead, .sheet .grab {",
		"/* ---------- Notes workspace: the synthesis surface ----------",
		"#notesworkspace {",
		"#wsscrim {",
		"#wspanel {",
		"#wstopbar {",
		"#wsnav {",
		"#wsnote {",
		"#wsinbox {",
		".wsrefs {",
		"#wsplacebar {",
		".wsibubble.clamped {",
		".wsrefbody.clamped {",
		".wsmenu {",
		"#wscards .empty {",
		"#wsnoteempty {",
		"@media (max-width: 767px) {",
		"@media (min-width: 768px) {",
		"#wsinbox, #wsnav { width: 340px; }",
	} {
		if !strings.Contains(notes, sig) {
			t.Fatalf("notes.css missing signature %q", sig)
		}
	}

	// layout.css must no longer own the bookmark-peek block.
	for _, bad := range []string{
		"#bookmarkpeek {",
		"body.bookmarks-open #bookmarkpeek { display: block; }",
		"/* the Bookmarks pane's left peek, as a tap-catcher:",
	} {
		if strings.Contains(layout, bad) {
			t.Fatalf("layout.css must not contain bookmark-peek ownership %q", bad)
		}
	}

	// accessibility.css owns the complete cross-component pane rule and,
	// apart from its explanatory comment and whitespace, nothing else.
	a11yComment := "/* the spatial-line slides are motion; honour Reduce Motion (pane-gap-design.md"
	a11yMedia := "@media (prefers-reduced-motion: reduce) {\n  #cards, #map, #bookmarkspane { transition: none; }\n}"
	if !strings.Contains(a11y, a11yComment) {
		t.Fatalf("accessibility.css missing explanatory reduced-motion comment")
	}
	if !strings.Contains(a11y, a11yMedia) {
		t.Fatalf("accessibility.css missing cross-component pane reduced-motion override")
	}
	// Strip the required comment/media and remaining whitespace; nothing may remain.
	rest := a11y
	// Drop the multi-line comment that ends before the media query.
	if i := strings.Index(rest, "/* the spatial-line slides are motion;"); i >= 0 {
		if j := strings.Index(rest[i:], "*/"); j >= 0 {
			rest = rest[:i] + rest[i+j+2:]
		}
	}
	rest = strings.Replace(rest, a11yMedia, "", 1)
	if strings.TrimSpace(rest) != "" {
		t.Fatalf("accessibility.css must contain only the pane reduced-motion comment/block; residual %q", rest)
	}
	// notes.css must not still hold the cross-component override.
	if strings.Contains(notes, "#cards, #map, #bookmarkspane { transition: none; }") {
		t.Fatal("notes.css must not contain the cross-component pane reduced-motion override")
	}

	// Component-local accessibility rules remain in their owning files.
	searchLayout := layout
	if !strings.Contains(searchLayout, "@media (prefers-reduced-transparency: reduce)") {
		t.Fatal("layout.css must retain search reduced-transparency rules")
	}
	if !strings.Contains(searchLayout, "@media (prefers-reduced-motion: reduce) {\n  #searchscrim, #searchpanel { animation: none; }\n}") &&
		!strings.Contains(searchLayout, "#searchscrim, #searchpanel { animation: none; }") {
		t.Fatal("layout.css must retain search reduced-motion rules")
	}
	if !strings.Contains(searchLayout, "#mapfullbtn { transition: none; }") {
		t.Fatal("layout.css must retain mapfull-button local reduced-motion transition rule")
	}
	if !strings.Contains(notes, "body.map-full #map { animation-duration: .12s; }") {
		t.Fatal("notes.css must own map-full reduced-motion duration with the desktop source")
	}
	if !strings.Contains(notes, "@media (prefers-reduced-transparency: reduce)") {
		t.Fatal("notes.css must retain Notes workspace reduced-transparency")
	}
	if !strings.Contains(notes, "#wsscrim, #wspanel { animation: none; }") {
		t.Fatal("notes.css must retain Notes workspace local reduced-motion animation rule")
	}
	chatCSS, err := fs.ReadFile(webFS, "web/css/chat.css")
	if err != nil {
		t.Fatalf("chat.css missing from embed: %v", err)
	}
	chat := string(chatCSS)
	for _, sig := range []string{
		"#attadd { transition: none; }",
		"#attmenu { animation: none; }",
		".stagechip .spin i { animation: none; }",
	} {
		if !strings.Contains(chat, sig) {
			t.Fatalf("chat.css must retain local reduced-motion signature %q", sig)
		}
	}

	// Cascade order: pane transitions before accessibility override; notes
	// before accessibility; phone peek before desktop suppression; accessibility last.
	cascade := mustCSSCascade(t, html, webFS)
	cardsTrans := strings.Index(cascade, "#cards {\n  position: fixed; z-index: 30;")
	if cardsTrans < 0 {
		// fall back to transition declaration on #cards block
		cardsTrans = strings.Index(cascade, "transition: transform .34s cubic-bezier(.32,.72,.34,1);")
	}
	mapTrans := strings.Index(cascade, "#map {\n  position: fixed; z-index: 31;")
	if mapTrans < 0 {
		mapTrans = strings.LastIndex(cascade, "transition: transform .34s cubic-bezier(.32,.72,.34,1);")
	}
	bmTrans := strings.Index(cascade, "transform: translateX(103%); transition: transform .25s;")
	if bmTrans < 0 {
		bmTrans = strings.Index(cascade, "#bookmarkspane { position: fixed;")
	}
	a11yOverride := strings.Index(cascade, "#cards, #map, #bookmarkspane { transition: none; }")
	if cardsTrans < 0 || mapTrans < 0 || bmTrans < 0 {
		t.Fatalf("cascade missing pane transition sources: cards@%d map@%d bookmarkspane@%d", cardsTrans, mapTrans, bmTrans)
	}
	if a11yOverride < 0 {
		t.Fatal("cascade missing cross-component pane reduced-motion override")
	}
	if !(cardsTrans < a11yOverride && mapTrans < a11yOverride && bmTrans < a11yOverride) {
		t.Fatalf("pane transitions must precede accessibility override: cards@%d map@%d bm@%d a11y@%d",
			cardsTrans, mapTrans, bmTrans, a11yOverride)
	}
	notesMarker := strings.Index(cascade, "/* ---------- sticky-notes pane:")
	a11yMarker := strings.Index(cascade, "/* the spatial-line slides are motion; honour Reduce Motion")
	if notesMarker < 0 || a11yMarker < 0 {
		t.Fatalf("cascade missing notes/accessibility markers: notes@%d a11y@%d", notesMarker, a11yMarker)
	}
	if !(notesMarker < a11yMarker) {
		t.Fatalf("notes.css must precede accessibility.css in cascade: notes@%d a11y@%d", notesMarker, a11yMarker)
	}
	if a11yMarker != strings.LastIndex(cascade, "/* the spatial-line slides are motion; honour Reduce Motion") {
		t.Fatal("accessibility reduced-motion comment must appear once at the end of the cascade")
	}
	phonePeek := strings.Index(cascade, "body.bookmarks-open #bookmarkpeek { display: block; }")
	desktopPeek := strings.Index(cascade, "#bookmarkpeek, body.bookmarks-open #bookmarkpeek { display: none; }")
	if phonePeek < 0 || desktopPeek < 0 {
		t.Fatalf("cascade missing bookmark-peek rules: phone@%d desktop@%d", phonePeek, desktopPeek)
	}
	if !(phonePeek < desktopPeek) {
		t.Fatalf("phone/tablet bookmark-peek display must precede desktop suppression: phone@%d desktop@%d", phonePeek, desktopPeek)
	}

	// Moved Notes/Bookmarks signatures must not remain inline or in layout.
	sources, err := collectCSSSources(html)
	if err != nil {
		t.Fatal(err)
	}
	var inline string
	var sawInline bool
	for _, src := range sources {
		if src.kind == cssSourceInline {
			sawInline = true
			inline = src.text
			break
		}
	}
	// Nine-link 5E state: style element remains but CSS content is empty/whitespace.
	if !sawInline {
		t.Fatal("5E nine-link state must retain the inline <style> element for Packet 5F")
	}
	if strings.TrimSpace(inline) != "" {
		t.Fatalf("5E inline style must be empty or whitespace-only; got %q", inline)
	}
	for _, moved := range []string{
		"/* ---------- sticky-notes pane:",
		"#bookmarkspane {",
		"#bookmarkpeek {",
		"#notesworkspace {",
		"#notesbtn {",
		"/* ---------- desktop / iPad: columns ---------- */",
		"#cards, #map, #bookmarkspane { transition: none; }",
	} {
		if strings.Contains(inline, moved) {
			t.Fatalf("moved signature %q still present in inline style", moved)
		}
		if moved == "#bookmarkpeek {" || moved == "/* ---------- sticky-notes pane:" {
			if strings.Contains(layout, moved) {
				t.Fatalf("moved signature %q still present in layout.css", moved)
			}
		}
	}

	// --peek remains only in tokens.css.
	tokensCSS, err := fs.ReadFile(webFS, "web/css/tokens.css")
	if err != nil {
		t.Fatalf("tokens.css missing: %v", err)
	}
	if !strings.Contains(string(tokensCSS), "--peek:") {
		t.Fatal("tokens.css must own --peek")
	}
	for _, name := range []string{"base.css", "layout.css", "cards.css", "map.css", "chat.css", "sheets.css", "notes.css", "accessibility.css"} {
		b, err := fs.ReadFile(webFS, "web/css/"+name)
		if err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
		if strings.Contains(string(b), "--peek:") {
			t.Fatalf("--peek must remain only in tokens.css; found declaration in %s", name)
		}
	}
}

// TestProductionNotesAccessibilityCSSServing activates the 5A exact-byte,
// content-type, cache, nosniff, and negative-path contracts for the two new
// linked files.
func TestProductionNotesAccessibilityCSSServing(t *testing.T) {
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))

	for _, href := range []string{"/css/notes.css", "/css/accessibility.css"} {
		t.Run("ok"+href, func(t *testing.T) {
			embedPath, err := stylesheetEmbedPath(href)
			if err != nil {
				t.Fatal(err)
			}
			want, err := fs.ReadFile(webFS, embedPath)
			if err != nil {
				t.Fatalf("read embed %s: %v", embedPath, err)
			}
			rec := getCharacterization(t, h, href)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status=%d, want 200; body=%q", href, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
				t.Fatalf("GET %s Content-Type=%q, want %q", href, got, "text/css; charset=utf-8")
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("GET %s Cache-Control=%q, want no-store", href, got)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("GET %s X-Content-Type-Options=%q, want nosniff", href, got)
			}
			if rec.Body.String() != string(want) {
				t.Fatalf("GET %s body differs from embedded %s (%d vs %d bytes)", href, embedPath, rec.Body.Len(), len(want))
			}
		})
	}

	for _, path := range []string{
		"/css/",
		"/css/missing.css",
		"/css/sub/notes.css",
		"/css/%2e%2e/notes.css",
		"/css/notes.txt",
		"/css/notes.css.bak",
		"/web/css/notes.css",
		"/web/css/accessibility.css",
		"/web/css/",
	} {
		t.Run("404"+path, func(t *testing.T) {
			rec := getCharacterization(t, h, path)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s status=%d, want 404; body=%q", path, rec.Code, rec.Body.String())
			}
			if bodyLooksLikeDirectoryListing(rec.Body.String()) {
				t.Fatalf("GET %s exposed a directory listing", path)
			}
			assertNotIndexOrSVG(t, path, rec.Body.Bytes())
		})
	}
}
