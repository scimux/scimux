package app

// S0 (remote refactor) — the served-set ratchet.
//
// FR-40 requires the bootstrap manifest to name *exactly* what is fetched, and
// requires it to be derived from the served asset set rather than from a
// hand-maintained list. The trap this closes is concrete and was already in the
// tree: web_js_static_test.go's productionJSURLs named 17 of the 21 served
// modules (caret.js, insets.js, menu.js and returnto.js were absent), so a
// manifest built by copying that list would boot a page with four modules
// missing — precisely the partial boot FR-40 forbids.
//
// So this file supplies servedAssetInventory(), derived from the embedded FS
// that the handlers actually serve from, and asserts the hand-maintained lists
// agree with it. S8 consumes the function, never a list. If a module is added
// to web/js/ and nowhere else, the lists fail here rather than the bootstrap
// failing in a browser.
//
// Scope note: this ratchet speaks for the *inventory*. The lists it checks
// carry other properties too — productionCSSFinalOrder encodes cascade order,
// productionJSURLs drives byte and header locking — and those stay where they
// are. Inventory equality is order-independent by design; asserting order here
// would duplicate css_cascade_test.go and give one property two homes.

import (
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
	"testing"
)

// servedAsset is one URL the local node serves, with the embed path behind it.
type servedAsset struct {
	URL   string // origin-relative, exactly as the browser requests it
	Embed string // path within webFS
	Kind  string // "index" | "js" | "css" | "asset"
}

// servedAssetInventory derives the complete served surface from the embedded
// filesystem. This is the FR-40 manifest source: every URL here must be
// fetchable over the data channel, and nothing outside it is application
// content.
//
// It mirrors newWebHandlers exactly — flat *.js under /js/, flat *.css under
// /css/, a recursive tree under /assets/, and index.html at "/" — because a
// manifest that mirrors anything else is a manifest describing a different
// server.
func servedAssetInventory(t *testing.T) []servedAsset {
	t.Helper()
	out := []servedAsset{{URL: "/", Embed: "web/index.html", Kind: "index"}}

	// Flat roots: the handlers reject nested paths, so a nested file would be
	// embedded but unreachable. That is a packaging error, not an inventory
	// entry, and it fails below rather than being silently dropped.
	for _, r := range []struct{ dir, prefix, ext, kind string }{
		{"web/js", "/js/", ".js", "js"},
		{"web/css", "/css/", ".css", "css"},
	} {
		entries, err := fs.ReadDir(webFS, r.dir)
		if err != nil {
			t.Fatalf("read embedded %s: %v", r.dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				t.Fatalf("embedded %s/%s is a directory; %s serves flat files only, "+
					"so nothing under it can ever be fetched", r.dir, e.Name(), r.prefix)
			}
			if !strings.HasSuffix(e.Name(), r.ext) {
				t.Fatalf("embedded %s/%s does not end in %s; the handler would 404 it",
					r.dir, e.Name(), r.ext)
			}
			out = append(out, servedAsset{
				URL:   r.prefix + e.Name(),
				Embed: r.dir + "/" + e.Name(),
				Kind:  r.kind,
			})
		}
	}

	// /assets/ is an http.FileServer, so its tree is served recursively.
	err := fs.WalkDir(webFS, "web/assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		out = append(out, servedAsset{
			URL:   "/assets/" + strings.TrimPrefix(p, "web/assets/"),
			Embed: p,
			Kind:  "asset",
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded web/assets: %v", err)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

func inventoryURLs(inv []servedAsset, kind string) []string {
	var out []string
	for _, a := range inv {
		if a.Kind == kind {
			out = append(out, a.URL)
		}
	}
	sort.Strings(out)
	return out
}

// TestServedAssetInventoryIsDerivedAndNonEmpty is the anti-vacuity assertion:
// an inventory that silently came back empty would make every comparison below
// pass. The exact counts are asserted because FR-40's failure mode is a
// *missing* entry, which a "greater than zero" check cannot see.
func TestServedAssetInventoryIsDerivedAndNonEmpty(t *testing.T) {
	inv := servedAssetInventory(t)
	byKind := map[string]int{}
	for _, a := range inv {
		byKind[a.Kind]++
	}
	for kind, want := range map[string]int{
		"index": 1,
		"js":    25,
		"css":   10,
		"asset": 5,
	} {
		if byKind[kind] != want {
			t.Errorf("served inventory holds %d %s entries, want %d. If the web tree "+
				"genuinely changed, update this count and check that FR-40's manifest "+
				"consumers still name everything.", byKind[kind], kind, want)
		}
	}
	if t.Failed() {
		for _, a := range inv {
			t.Logf("  %-8s %s", a.Kind, a.URL)
		}
	}
}

// TestEveryServedAssetIsActuallyReachable proves the inventory is the *served*
// set and not merely the embedded set. A manifest derived from a file listing
// that includes something the handlers 404 produces a boot that hangs on a
// fetch which can never succeed.
func TestEveryServedAssetIsActuallyReachable(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	for _, a := range servedAssetInventory(t) {
		t.Run(a.URL, func(t *testing.T) {
			rec := getCharacterization(t, h, a.URL)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status=%d, want 200: embedded at %s but not served, "+
					"so an FR-40 manifest naming it would deadlock the boot",
					a.URL, rec.Code, a.Embed)
			}
			if rec.Body.Len() == 0 {
				t.Fatalf("GET %s served an empty body", a.URL)
			}
		})
	}
}

// TestProductionJSURLsCoversEveryServedModule is the ratchet proper, and the
// reason this file exists. It is what closes the four-module gap and keeps it
// closed.
func TestProductionJSURLsCoversEveryServedModule(t *testing.T) {
	want := inventoryURLs(servedAssetInventory(t), "js")
	got := append([]string(nil), productionJSURLs...)
	sort.Strings(got)

	if strings.Join(got, ",") == strings.Join(want, ",") {
		return
	}
	served := map[string]bool{}
	for _, u := range want {
		served[u] = true
	}
	listed := map[string]bool{}
	for _, u := range got {
		listed[u] = true
	}
	for _, u := range want {
		if !listed[u] {
			t.Errorf("module %s is served but absent from productionJSURLs: it is "+
				"neither byte- nor header-locked, and an FR-40 manifest copied from "+
				"that list would omit it (partial boot)", u)
		}
	}
	for _, u := range got {
		if !served[u] {
			t.Errorf("productionJSURLs names %s, which is not served: a manifest "+
				"naming it would deadlock the boot on a fetch that 404s", u)
		}
	}
}

// TestProductionCSSListCoversEveryServedStylesheet is the same ratchet for the
// 10 stylesheets FR-40 was amended to cover. Import maps and blob: specifier
// rewriting do not reach CSS, so the bootstrap needs its own channel-fetch path
// for these — and it needs the list to be complete before it can.
func TestProductionCSSListCoversEveryServedStylesheet(t *testing.T) {
	want := inventoryURLs(servedAssetInventory(t), "css")
	got := append([]string(nil), productionCSSFinalOrder...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("productionCSSFinalOrder holds %v, served stylesheets are %v.\n"+
			"These must be the same set (cascade order stays css_cascade_test.go's "+
			"property; this asserts only that nothing is missing or stale).", got, want)
	}
}

// TestServedAssetTreeIsOneLevelDeepForCSS pins the fact FR-40 leans on when it
// calls the stylesheet path "exactly one level deep": web/css contains no
// url() and no @import, so fetching the 10 files needs no recursive resolver.
// If a stylesheet ever gains a reference, the bootstrap needs a resolver and
// this is where that shows up — rather than as a missing background at runtime.
func TestServedAssetTreeIsOneLevelDeepForCSS(t *testing.T) {
	for _, a := range servedAssetInventory(t) {
		if a.Kind != "css" {
			continue
		}
		b, err := fs.ReadFile(webFS, a.Embed)
		if err != nil {
			t.Fatalf("read %s: %v", a.Embed, err)
		}
		body := string(b)
		if strings.Contains(body, "@import") {
			t.Errorf("%s uses @import; FR-40's stylesheet fetch is one level deep and "+
				"would not follow it", a.URL)
		}
		// url(#fragment) is an SVG filter/mask reference resolved inside the
		// document — no network fetch, so it is not a second level.
		for _, frag := range urlReferences(body) {
			if strings.HasPrefix(frag, "#") || strings.HasPrefix(frag, "data:") {
				continue
			}
			t.Errorf("%s references %q via url(); FR-40's stylesheet fetch is one "+
				"level deep and would leave it unresolved on the remote path", a.URL, frag)
		}
	}
}

// urlReferences extracts the target of every CSS url(...) in src.
func urlReferences(src string) []string {
	var out []string
	rest := src
	for {
		i := strings.Index(rest, "url(")
		if i < 0 {
			return out
		}
		rest = rest[i+len("url("):]
		j := strings.Index(rest, ")")
		if j < 0 {
			return out
		}
		ref := strings.TrimSpace(rest[:j])
		ref = strings.Trim(ref, `"'`)
		if ref != "" {
			out = append(out, ref)
		}
		rest = rest[j+1:]
	}
}

// TestIndexReferencesOnlyServedAssets closes the last inventory hole: index.html
// is the boot document, so every same-origin URL it names must be in the
// inventory. A reference the inventory does not carry is an asset the bootstrap
// would never fetch, and the page would boot missing it.
func TestIndexReferencesOnlyServedAssets(t *testing.T) {
	b, err := fs.ReadFile(webFS, "web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	inv := map[string]bool{}
	for _, a := range servedAssetInventory(t) {
		inv[a.URL] = true
	}
	for _, ref := range indexSameOriginRefs(string(b)) {
		if !inv[ref] {
			t.Errorf("index.html references %s, which is not in the served inventory; "+
				"an FR-40 manifest derived from the inventory would omit it", ref)
		}
	}
}

// indexSameOriginRefs collects origin-relative href/src values from index.html.
// Absolute URLs, data: URIs and fragments are outside the seam by definition.
func indexSameOriginRefs(html string) []string {
	var out []string
	for _, attr := range []string{`href="`, `src="`} {
		rest := html
		for {
			i := strings.Index(rest, attr)
			if i < 0 {
				break
			}
			rest = rest[i+len(attr):]
			j := strings.Index(rest, `"`)
			if j < 0 {
				break
			}
			v := rest[:j]
			rest = rest[j:]
			if !strings.HasPrefix(v, "/") {
				continue // external, data:, fragment, or relative-to-nothing
			}
			out = append(out, path.Clean(v))
		}
	}
	sort.Strings(out)
	return out
}
