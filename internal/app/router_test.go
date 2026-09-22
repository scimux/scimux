package app

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestNewHandlerEmbeddedWebFSConstructs(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	rec := routeRequest(h, http.MethodGet, "/", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
}

func TestNewHandlerMutationGuardIsOutermost(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	withoutCSRF := routeRequest(h, http.MethodPost, "/api/not-a-route", `{}`, false)
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("unknown unsafe route without CSRF status = %d, want 403", withoutCSRF.Code)
	}
	withCSRF := routeRequest(h, http.MethodPost, "/api/not-a-route", `{}`, true)
	if withCSRF.Code != http.StatusNotFound {
		t.Fatalf("unknown unsafe route with CSRF status = %d, want 404", withCSRF.Code)
	}
}

func TestNewHandlerStaticDoesNotShadowAPI(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	rec := routeRequest(h, http.MethodGet, "/api/licenses", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/licenses status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if bodyLooksLikeIndex(rec.Body.String()) {
		t.Fatal("GET /api/licenses returned index page content")
	}
	if bodyLooksLikeDirectoryListing(rec.Body.String()) {
		t.Fatal("GET /api/licenses returned directory listing content")
	}
}

func TestNewHandlerConstructsCurrentStaticBehavior(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	index := routeRequest(h, http.MethodGet, "/", "", false)
	if got := index.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("GET / Content-Type = %q, want text/html; charset=utf-8", got)
	}
	if got := index.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("GET / Cache-Control = %q, want no-store", got)
	}
	wantIndex, err := csrfIndex(webFS)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(index.Body.Bytes(), wantIndex) {
		t.Fatalf("GET / body differs from csrfIndex(webFS)")
	}

	asset := routeRequest(h, http.MethodGet, "/assets/agents/openai.svg", "", false)
	if asset.Code != http.StatusOK {
		t.Fatalf("GET asset status = %d, want 200", asset.Code)
	}
	wantAsset, err := fs.ReadFile(webFS, "web/assets/agents/openai.svg")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(asset.Body.Bytes(), wantAsset) {
		t.Fatalf("GET asset body differs from embedded bytes")
	}
	dir := routeRequest(h, http.MethodGet, "/assets/", "", false)
	if dir.Code != http.StatusOK || !bodyLooksLikeDirectoryListing(dir.Body.String()) {
		t.Fatalf("GET /assets/ status/body = %d/%q, want current directory listing", dir.Code, dir.Body.String())
	}
	missing := routeRequest(h, http.MethodGet, "/web/index.html", "", false)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("GET /web/index.html status = %d, want 404", missing.Code)
	}
}

func TestNewHandlerValidationErrors(t *testing.T) {
	// Each fixture is complete enough to pass every check before the intended
	// failure so cases stay independently meaningful.
	tests := []struct {
		name string
		web  fs.FS
	}{
		{
			name: "missing index",
			web: fstest.MapFS{
				"web/assets": {Mode: fs.ModeDir},
				"web/css":    {Mode: fs.ModeDir},
				"web/js":     {Mode: fs.ModeDir},
			},
		},
		{
			name: "missing assets",
			web: fstest.MapFS{
				"web/index.html": {Data: []byte(csrfPlaceholder)},
				"web/css":        {Mode: fs.ModeDir},
				"web/js":         {Mode: fs.ModeDir},
			},
		},
		{
			name: "assets not directory",
			web: fstest.MapFS{
				"web/index.html": {Data: []byte(csrfPlaceholder)},
				"web/assets":     {Data: []byte("not a dir")},
				"web/css":        {Mode: fs.ModeDir},
				"web/js":         {Mode: fs.ModeDir},
			},
		},
		{
			name: "missing css",
			web: fstest.MapFS{
				"web/index.html": {Data: []byte(csrfPlaceholder)},
				"web/assets":     {Mode: fs.ModeDir},
				"web/js":         {Mode: fs.ModeDir},
			},
		},
		{
			name: "css not directory",
			web: fstest.MapFS{
				"web/index.html": {Data: []byte(csrfPlaceholder)},
				"web/assets":     {Mode: fs.ModeDir},
				"web/css":        {Data: []byte("not a dir")},
				"web/js":         {Mode: fs.ModeDir},
			},
		},
		{
			name: "missing js",
			web: fstest.MapFS{
				"web/index.html": {Data: []byte(csrfPlaceholder)},
				"web/assets":     {Mode: fs.ModeDir},
				"web/css":        {Mode: fs.ModeDir},
			},
		},
		{
			name: "js not directory",
			web: fstest.MapFS{
				"web/index.html": {Data: []byte(csrfPlaceholder)},
				"web/assets":     {Mode: fs.ModeDir},
				"web/css":        {Mode: fs.ModeDir},
				"web/js":         {Data: []byte("not a dir")},
			},
		},
		{
			name: "missing csrf placeholder",
			web: fstest.MapFS{
				"web/index.html": {Data: []byte("<html></html>")},
				"web/assets":     {Mode: fs.ModeDir},
				"web/css":        {Mode: fs.ModeDir},
				"web/js":         {Mode: fs.ModeDir},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("NewHandler panicked: %v", r)
				}
			}()
			if _, err := NewHandler(newTestApp(t, &fakeTmux{}), tt.web); err == nil {
				t.Fatal("NewHandler returned nil error")
			}
		})
	}
}

// newTestHandler builds the production router for tests. Shared across package
// app tests; fatals on construction error (call NewHandler raw when asserting failure).
func newTestHandler(t *testing.T, a *app) http.Handler {
	t.Helper()
	// Generic route and boundary tests must never execute a developer's agent
	// CLIs or contact public release feeds. Focused discovery tests call the
	// handlers directly with private fakes and therefore retain production
	// behavior.
	if a.agentCatalog == nil {
		a.agentCatalog = func() map[string]agentInfo { return map[string]agentInfo{} }
	}
	if a.harnessInventory == nil {
		a.harnessInventory = func(bool) []harnessRow { return []harnessRow{} }
	}
	if a.harnessLatestSources == nil {
		a.harnessLatestSources = func() map[string]harnessSource { return map[string]harnessSource{} }
	}
	h, err := NewHandler(a, webFS)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func routeRequest(h http.Handler, method, path, body string, csrf bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf {
		req.Header.Set("X-Scimux-CSRF", csrfToken)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
