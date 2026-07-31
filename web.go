package main

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web/index.html web/assets
var webFS embed.FS

type webHandlers struct {
	index  http.Handler
	assets http.Handler
}

func newWebHandlers(web fs.FS) (webHandlers, error) {
	assetsInfo, err := fs.Stat(web, "web/assets")
	if err != nil {
		return webHandlers{}, fmt.Errorf("embedded web/assets missing: %w", err)
	}
	if !assetsInfo.IsDir() {
		return webHandlers{}, fmt.Errorf("embedded web/assets is not a directory")
	}
	assets, err := fs.Sub(web, "web/assets")
	if err != nil {
		return webHandlers{}, fmt.Errorf("embedded web/assets missing: %w", err)
	}
	indexHTML, err := csrfIndex(web)
	if err != nil {
		return webHandlers{}, err
	}

	return webHandlers{
		assets: http.StripPrefix("/assets/", http.FileServer(http.FS(assets))),
		index: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b := indexHTML
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			// The UI is embedded in the binary and changes with every build;
			// a cached copy after a scimux upgrade is a recurring dogfooding
			// trap (especially iPad Safari). It's one small local page: always
			// fetch fresh.
			w.Header().Set("Cache-Control", "no-store")
			w.Write(b)
		}),
	}, nil
}

// csrfIndex reads the embedded index page once and substitutes the per-process
// CSRF token into its placeholder meta tag. The token is hex, so it is inert in
// an HTML attribute; the page is served verbatim thereafter.
func csrfIndex(fsys fs.FS) ([]byte, error) {
	b, err := fs.ReadFile(fsys, "web/index.html")
	if err != nil {
		return nil, fmt.Errorf("embedded web/index.html missing: %w", err)
	}
	if !strings.Contains(string(b), csrfPlaceholder) {
		return nil, fmt.Errorf("web/index.html is missing the %s placeholder", csrfPlaceholder)
	}
	return []byte(strings.Replace(string(b), csrfPlaceholder, csrfToken, 1)), nil
}

const csrfPlaceholder = "__SCIMUX_CSRF__"
