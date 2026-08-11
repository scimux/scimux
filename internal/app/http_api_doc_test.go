package app

// Packet 10: docs/http-api.md must document every registered /api/... route
// from characterizationAPIRoutes(), and must not list routes that are no
// longer registered. One inventory — do not invent a second table.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// httpAPIDocPath resolves docs/http-api.md from this test file's location,
// not from the process working directory (go test CWD is the package dir).
func httpAPIDocPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "http-api.md")
}

// headingRouteRe captures each `METHOD /path…` segment inside a ### heading.
// A heading may document two routes (`GET /api/ui` / `PUT /api/ui`) and may
// carry a query suffix (`?q=<query>` or `[?mode=visible]`).
var headingRouteRe = regexp.MustCompile("`([^`]+)`")

// parseDocumentedAPIRoutes extracts method+pattern pairs from
// "### `METHOD /path`" headings in docs/http-api.md.
func parseDocumentedAPIRoutes(doc string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "### ") {
			continue
		}
		for _, m := range headingRouteRe.FindAllStringSubmatch(trimmed, -1) {
			method, pattern, ok := normalizeDocRoute(m[1])
			if !ok {
				continue
			}
			out[method+" "+pattern] = true
		}
	}
	return out
}

// normalizeDocRoute turns a heading segment into method + ServeMux-style
// pattern: strip query suffixes and optional-bracket tails.
func normalizeDocRoute(seg string) (method, pattern string, ok bool) {
	seg = strings.TrimSpace(seg)
	method, rest, found := strings.Cut(seg, " ")
	if !found || method == "" || rest == "" {
		return "", "", false
	}
	// Drop ?query and [?optional] suffixes; keep /api/.../{id} paths intact.
	if i := strings.IndexAny(rest, "?["); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "/api/") {
		return "", "", false
	}
	return method, rest, true
}

func TestHTTPAPIDocCoversCharacterizationRoutes(t *testing.T) {
	path := httpAPIDocPath(t)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	documented := parseDocumentedAPIRoutes(string(b))

	registered := map[string]bool{}
	for _, r := range characterizationAPIRoutes() {
		registered[r.method+" "+r.pattern] = true
	}

	var undocumented, stale []string
	for key := range registered {
		if !documented[key] {
			undocumented = append(undocumented, key)
		}
	}
	for key := range documented {
		if !registered[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(stale)

	if len(undocumented) > 0 {
		t.Errorf("routes registered but not documented in docs/http-api.md:\n  %s",
			strings.Join(undocumented, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("routes documented in docs/http-api.md but not registered:\n  %s",
			strings.Join(stale, "\n  "))
	}
}
