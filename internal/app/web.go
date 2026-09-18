package app

import (
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"

	webassets "github.com/scimux/scimux/web"
)

// webFS preserves the historic web/... paths at the application boundary even
// though the assets now live in their own package at the repository root.
var webFS = webPrefixFS{fsys: webassets.Files}

type webHandlers struct {
	index  http.Handler
	assets http.Handler
	css    http.Handler
	js     http.Handler
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

	cssInfo, err := fs.Stat(web, "web/css")
	if err != nil {
		return webHandlers{}, fmt.Errorf("embedded web/css missing: %w", err)
	}
	if !cssInfo.IsDir() {
		return webHandlers{}, fmt.Errorf("embedded web/css is not a directory")
	}
	cssRoot, err := fs.Sub(web, "web/css")
	if err != nil {
		return webHandlers{}, fmt.Errorf("embedded web/css missing: %w", err)
	}

	jsInfo, err := fs.Stat(web, "web/js")
	if err != nil {
		return webHandlers{}, fmt.Errorf("embedded web/js missing: %w", err)
	}
	if !jsInfo.IsDir() {
		return webHandlers{}, fmt.Errorf("embedded web/js is not a directory")
	}
	jsRoot, err := fs.Sub(web, "web/js")
	if err != nil {
		return webHandlers{}, fmt.Errorf("embedded web/js missing: %w", err)
	}

	indexHTML, err := csrfIndex(web)
	if err != nil {
		return webHandlers{}, err
	}

	return webHandlers{
		assets: http.StripPrefix("/assets/", http.FileServer(http.FS(assets))),
		css:    cssFileHandler(cssRoot),
		js:     jsFileHandler(jsRoot),
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

// cssFileHandler serves only flat *.css files from an embedded web/css root.
// It never uses http.FileServer: directory requests, nested paths, traversal,
// and non-CSS names all 404 with no listing.
func cssFileHandler(cssRoot fs.FS) http.Handler {
	return flatStaticHandler(cssRoot, "/css/", ".css", "text/css; charset=utf-8")
}

// jsFileHandler serves only flat *.js files from an embedded web/js root.
// Parallel to cssFileHandler: no directory listing, no nested/traversal paths,
// no non-JS names, exact embedded bytes with no-store + nosniff.
func jsFileHandler(jsRoot fs.FS) http.Handler {
	return flatStaticHandler(jsRoot, "/js/", ".js", "text/javascript; charset=utf-8")
}

// flatStaticHandler serves only flat files with a required extension under
// prefix from an embedded subdirectory root. Directory, nested, traversal,
// and wrong-extension requests 404 with no listing.
func flatStaticHandler(root fs.FS, prefix, ext, contentType string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !strings.HasPrefix(p, prefix) {
			http.NotFound(w, r)
			return
		}
		name := p[len(prefix):]
		if name == "" {
			http.NotFound(w, r)
			return
		}
		// Flat files only: reject nested paths, backslashes, and unclean names.
		if strings.Contains(name, "/") || strings.Contains(name, `\`) {
			http.NotFound(w, r)
			return
		}
		if path.Base(name) != name || name == "." || name == ".." {
			http.NotFound(w, r)
			return
		}
		if path.Clean("/"+name) != "/"+name {
			http.NotFound(w, r)
			return
		}
		if !strings.HasSuffix(name, ext) {
			http.NotFound(w, r)
			return
		}
		data, err := fs.ReadFile(root, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(data)
	})
}

// csrfIndex reads the embedded index page once and substitutes the runtime
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
