package main

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
	"testing"
)

// productionJSURLs is the flat /js/<file>.js surface served from the embed.
var productionJSURLs = []string{
	"/js/api.js",
	"/js/cards.js",
	"/js/format.js",
	"/js/lanes.js",
	"/js/map-model.js",
	"/js/navigation.js",
	"/js/state.js",
}

// TestProductionJSServeHeadersAndBytes locks exact embedded JS bytes plus
// JavaScript content type, no-store, and nosniff for every production module.
func TestProductionJSServeHeadersAndBytes(t *testing.T) {
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))
	for _, href := range productionJSURLs {
		t.Run("ok"+href, func(t *testing.T) {
			embedPath := "web" + href // /js/foo.js -> web/js/foo.js
			want, err := fs.ReadFile(webFS, embedPath)
			if err != nil {
				t.Fatalf("read embed %s: %v", embedPath, err)
			}
			rec := getCharacterization(t, h, href)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status=%d, want 200; body=%q", href, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
				t.Fatalf("GET %s Content-Type=%q, want text/javascript; charset=utf-8", href, got)
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
}

// TestProductionJSNegativeRoots rejects directory listing, missing, nested,
// traversal/encoded traversal, non-JS, raw-source, test, and package paths.
func TestProductionJSNegativeRoots(t *testing.T) {
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))
	for _, pathURL := range []string{
		"/js/",
		"/js/missing.js",
		"/js/sub/format.js",
		"/js/%2e%2e/format.js",
		"/js/%2e%2e/index.html",
		"/js/format.txt",
		"/js/format.js.bak",
		"/js/readme.md",
		"/js/app.js",
		"/web/js/format.js",
		"/web/js/",
		"/web/js/api.js",
		"/package.json",
		"/web/package.json",
		"/test/smoke.test.js",
		"/web/test/smoke.test.js",
		"/web/test/format.test.js",
	} {
		t.Run("404"+pathURL, func(t *testing.T) {
			rec := getCharacterization(t, h, pathURL)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s status=%d, want 404; body=%q", pathURL, rec.Code, rec.Body.String())
			}
			if bodyLooksLikeDirectoryListing(rec.Body.String()) {
				t.Fatalf("GET %s exposed a directory listing", pathURL)
			}
			assertNotIndexOrSVG(t, pathURL, rec.Body.Bytes())
			// Never serve a production module body on a negative path.
			if strings.Contains(rec.Body.String(), "export function") ||
				strings.Contains(rec.Body.String(), "Packet 6") {
				t.Fatalf("GET %s leaked JS module content", pathURL)
			}
		})
	}

	// Bare ".." segments are cleaned by net/http.ServeMux into a permanent
	// redirect before any route handler runs. That must never surface JS bytes.
	t.Run("traversal-dotdot-not-served", func(t *testing.T) {
		rec := getCharacterization(t, h, "/js/../format.js")
		if rec.Code == http.StatusOK {
			t.Fatalf("GET /js/../format.js status=200, want non-OK; body=%q", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "export function esc") {
			t.Fatalf("traversal path served JS content: status=%d", rec.Code)
		}
		if bodyLooksLikeDirectoryListing(rec.Body.String()) {
			t.Fatal("traversal path exposed a directory listing")
		}
	})
}

// TestProductionJSInventoryIsFlat complements the constructor's missing/not-dir
// cases in TestNewHandlerValidationErrors with the accepted production names.
func TestProductionJSInventoryIsFlat(t *testing.T) {
	h, err := NewHandler(newTestApp(t, &fakeTmux{}), webFS)
	if err != nil {
		t.Fatalf("NewHandler(webFS) = %v", err)
	}
	if h == nil {
		t.Fatal("NewHandler returned nil handler")
	}
	// Positive: inventory files are clean base names.
	for _, href := range productionJSURLs {
		base := path.Base(href)
		if base != href[len("/js/"):] {
			t.Fatalf("non-flat production JS URL %s", href)
		}
		if !strings.HasSuffix(base, ".js") {
			t.Fatalf("production JS URL missing .js: %s", href)
		}
	}
}
