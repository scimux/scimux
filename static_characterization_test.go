package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCharacterizationIndexRoute(t *testing.T) {
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))

	rec := getCharacterization(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("GET / Content-Type = %q, want %q", got, "text/html; charset=utf-8")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("GET / Cache-Control = %q, want %q", got, "no-store")
	}
	if strings.Contains(rec.Body.String(), csrfPlaceholder) {
		t.Fatal("GET / body still contains CSRF placeholder")
	}
	if !strings.Contains(rec.Body.String(), `name="scimux-csrf" content="`+csrfToken+`"`) {
		t.Fatal("GET / body does not contain live CSRF meta token")
	}
	want, err := csrfIndex(webFS)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("GET / body differs from csrfIndex(webFS): got %d bytes want %d bytes", rec.Body.Len(), len(want))
	}

	other := getCharacterization(t, h, "/not-root")
	if other.Code == http.StatusOK && bytes.Equal(other.Body.Bytes(), rec.Body.Bytes()) {
		t.Fatalf("GET /not-root received the index shell")
	}
	if bodyLooksLikeIndex(other.Body.String()) {
		t.Fatalf("GET /not-root body contains index shell marker; status=%d", other.Code)
	}
}

func TestCharacterizationAgentAssets(t *testing.T) {
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))

	tests := []struct {
		path      string
		embedPath string
	}{
		{"/assets/agents/claude.svg", "web/assets/agents/claude.svg"},
		{"/assets/agents/grok.svg", "web/assets/agents/grok.svg"},
		{"/assets/agents/openai.svg", "web/assets/agents/openai.svg"},
		{"/assets/agents/opencode.svg", "web/assets/agents/opencode.svg"},
		{"/assets/agents/pi.svg", "web/assets/agents/pi.svg"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := getCharacterization(t, h, tt.path)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200; body=%q", tt.path, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "image/svg+xml" {
				t.Fatalf("GET %s Content-Type = %q, want %q", tt.path, got, "image/svg+xml")
			}
			want, err := webFS.ReadFile(tt.embedPath)
			if err != nil {
				t.Fatalf("read embedded %s: %v", tt.embedPath, err)
			}
			if !bytes.Equal(rec.Body.Bytes(), want) {
				t.Fatalf("GET %s body differs from embedded %s: got %d bytes want %d bytes", tt.path, tt.embedPath, rec.Body.Len(), len(want))
			}
			if got := rec.Header().Get("Cache-Control"); got != "" {
				t.Fatalf("GET %s Cache-Control = %q, want empty", tt.path, got)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "" {
				t.Fatalf("GET %s X-Content-Type-Options = %q, want empty", tt.path, got)
			}
		})
	}
}

func TestCharacterizationAssetDirectoryBehavior(t *testing.T) {
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))

	noSlash := getCharacterization(t, h, "/assets")
	if noSlash.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /assets status = %d, want 301; body=%q", noSlash.Code, noSlash.Body.String())
	}
	if got := noSlash.Header().Get("Location"); got != "/assets/" {
		t.Fatalf("GET /assets Location = %q, want %q", got, "/assets/")
	}

	root := getCharacterization(t, h, "/assets/")
	if root.Code != http.StatusOK {
		t.Fatalf("GET /assets/ status = %d, want 200; body=%q", root.Code, root.Body.String())
	}
	if got := root.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("GET /assets/ Content-Type = %q, want text/html prefix", got)
	}
	if !strings.Contains(root.Body.String(), "agents/") {
		t.Fatalf("GET /assets/ body does not contain agents/ link: %q", root.Body.String())
	}

	agents := getCharacterization(t, h, "/assets/agents/")
	if agents.Code != http.StatusOK {
		t.Fatalf("GET /assets/agents/ status = %d, want 200; body=%q", agents.Code, agents.Body.String())
	}
	for _, name := range []string{"claude.svg", "grok.svg", "openai.svg", "opencode.svg", "pi.svg"} {
		if !strings.Contains(agents.Body.String(), name) {
			t.Fatalf("GET /assets/agents/ body missing %s: %q", name, agents.Body.String())
		}
	}
}

func TestCharacterizationStaticNotFoundAndAllowlist(t *testing.T) {
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))

	for _, path := range []string{
		"/favicon.ico",
		"/assets/missing.svg",
		"/assets/agents/missing.svg",
		"/index.html",
		"/web/index.html",
		"/web/assets/agents/openai.svg",
		"/package.json",
		"/web/package.json",
		"/test/smoke.test.js",
		"/web/test/smoke.test.js",
		"/css/",
		"/css/app.css",
		"/js/",
	} {
		t.Run(path, func(t *testing.T) {
			rec := getCharacterization(t, h, path)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s status = %d, want 404; body=%q", path, rec.Code, rec.Body.String())
			}
			assertNotIndexOrSVG(t, path, rec.Body.Bytes())
		})
	}
}

func TestCharacterizationStaticDoesNotShadowAPI(t *testing.T) {
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))

	licenses := getCharacterization(t, h, "/api/licenses")
	if licenses.Code != http.StatusOK {
		t.Fatalf("GET /api/licenses status = %d, want 200; body=%q", licenses.Code, licenses.Body.String())
	}
	if got := licenses.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("GET /api/licenses Content-Type = %q, want %q", got, "application/json")
	}
	if bodyLooksLikeIndex(licenses.Body.String()) {
		t.Fatal("GET /api/licenses returned index page content")
	}
	if bodyLooksLikeDirectoryListing(licenses.Body.String()) {
		t.Fatal("GET /api/licenses returned directory listing content")
	}

	missing := getCharacterization(t, h, "/api/not-a-route")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("GET /api/not-a-route status = %d, want 404; body=%q", missing.Code, missing.Body.String())
	}
	assertNotIndexOrSVG(t, "/api/not-a-route", missing.Body.Bytes())
	if bodyLooksLikeDirectoryListing(missing.Body.String()) {
		t.Fatal("GET /api/not-a-route returned directory listing content")
	}
}

func getCharacterization(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787"+path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func assertNotIndexOrSVG(t *testing.T, path string, body []byte) {
	t.Helper()
	if bodyLooksLikeIndex(string(body)) {
		t.Fatalf("GET %s body contains index shell marker", path)
	}
	for _, embedPath := range []string{
		"web/assets/agents/claude.svg",
		"web/assets/agents/grok.svg",
		"web/assets/agents/openai.svg",
		"web/assets/agents/opencode.svg",
		"web/assets/agents/pi.svg",
	} {
		svg, err := webFS.ReadFile(embedPath)
		if err != nil {
			t.Fatalf("read embedded %s: %v", embedPath, err)
		}
		if bytes.Contains(body, svg) {
			t.Fatalf("GET %s body contains embedded SVG %s", path, embedPath)
		}
	}
}

func bodyLooksLikeIndex(body string) bool {
	return strings.Contains(body, `name="scimux-csrf" content="`) ||
		strings.Contains(body, csrfPlaceholder)
}

func bodyLooksLikeDirectoryListing(body string) bool {
	return strings.Contains(body, "<pre>") && strings.Contains(body, "<a href=")
}
