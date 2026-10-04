package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/backend"
	"github.com/scimux/scimux/internal/storagebudget"
)

// TestLegacyAndSplitPublicSurfacesMatch runs the characterized monolith and
// the Unix-linked split side by side. The same inventory that guards today's
// web UI routes drives the comparison, so adding a UI-callable REST path adds
// a parity case automatically rather than relying on a hand-maintained list.
func TestLegacyAndSplitPublicSurfacesMatch(t *testing.T) {
	release := fakeGitHub(t, "v1.0.0", nil)
	withUpdateSeams(t, release.URL, "v1.0.0")

	legacyApp := newTestApp(t, &fakeTmux{})
	legacyApp.hostedPairing = &fakePairingClient{hosted: "enrolled"}
	legacy, err := NewHandler(legacyApp, webFS)
	if err != nil {
		t.Fatal(err)
	}
	legacyServer := httptest.NewServer(legacy)
	defer legacyServer.Close()

	splitApp := newTestApp(t, &fakeTmux{})
	// Live disk space can differ between the apps' samples: body normalization
	// masks that difference, but the raw-body ETags still differ. Give both
	// transports the same synthetic metrics and keep their caches fresh for
	// the test, including across the normal 30-second refresh boundary.
	sampleAt := time.Now().Add(time.Hour)
	for _, a := range []*app{legacyApp, splitApp} {
		a.storageMu.Lock()
		a.storageSnapshot = storagebudget.Status{
			FreeBytes:    1 << 30,
			MinFreeBytes: storagebudget.DefaultMinFreeBytes,
			Writable:     true,
			Nodes:        map[string]int64{},
		}
		a.storageAt = sampleAt
		a.storageMu.Unlock()
	}
	sysMu.Lock()
	previousSys, previousSysAt := sysCache, sysCacheAt
	sysCache = sysInfo{Load1: 0.5, NCPU: 2, MemPct: 25, MemTotalGB: 8}
	sysCacheAt = sampleAt
	sysMu.Unlock()
	t.Cleanup(func() {
		sysMu.Lock()
		sysCache, sysCacheAt = previousSys, previousSysAt
		sysMu.Unlock()
	})
	coreHandler, err := newCoreMux(splitApp)
	if err != nil {
		t.Fatal(err)
	}
	core, err := backend.Listen(t.TempDir(), coreHandler)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	client, err := backend.NewClient(core.Link())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Hello(context.Background()); err != nil {
		t.Fatal(err)
	}
	split, err := newWebBackend(webBackendConfig{
		Web: webFS, Core: client.Proxy(), Pairing: &fakePairingClient{hosted: "enrolled"},
	})
	if err != nil {
		t.Fatal(err)
	}
	splitServer := httptest.NewServer(split.local)
	defer splitServer.Close()

	type requestCase struct{ name, method, path, body string }
	cases := []requestCase{
		{"index", http.MethodGet, "/", ""},
		{"javascript", http.MethodGet, "/js/app.js", ""},
		{"stylesheet", http.MethodGet, "/css/base.css", ""},
		{"agent-asset", http.MethodGet, "/assets/agents/openai.svg", ""},
		{"missing-static", http.MethodGet, "/not-a-route", ""},
		{"missing-api", http.MethodGet, "/api/not-a-route", ""},
	}
	for _, route := range characterizationAPIRoutes() {
		method, body := route.method, ""
		if method != http.MethodGet {
			body = "{not-json"
		}
		// This route intentionally contacts several upstream registries. Its
		// exact route/Allow behavior is compared without spending that network.
		if route.path == "/api/harnesses/latest" {
			method = http.MethodOptions
		}
		cases = append(cases, requestCase{route.method + " " + route.pattern, method, route.path, body})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := parityRequest(t, legacyServer.URL, tc.method, tc.path, tc.body)
			fresh := parityRequest(t, splitServer.URL, tc.method, tc.path, tc.body)
			if old.status != fresh.status {
				t.Fatalf("status old=%d split=%d\nold=%q\nsplit=%q", old.status, fresh.status, old.body, fresh.body)
			}
			oldBody := canonicalParityBody(tc.method, tc.path, old.body)
			freshBody := canonicalParityBody(tc.method, tc.path, fresh.body)
			if !bytes.Equal(oldBody, freshBody) {
				t.Fatalf("body differs\nold=%q\nsplit=%q", old.body, fresh.body)
			}
			for _, header := range []string{
				"Allow", "Cache-Control", "Content-Encoding", "Content-Security-Policy",
				"Content-Type", "ETag", "Location", "X-Content-Type-Options", "X-Frame-Options",
			} {
				if old.header.Get(header) != fresh.header.Get(header) {
					t.Errorf("%s old=%q split=%q", header, old.header.Get(header), fresh.header.Get(header))
				}
			}
		})
	}
}

type parityResponse struct {
	status int
	header http.Header
	body   []byte
}

func parityRequest(t *testing.T, base, method, path, body string) parityResponse {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scimux-CSRF", csrfToken)
	}
	transport := &http.Transport{DisableCompression: true}
	defer transport.CloseIdleConnections()
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		b, err = io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return parityResponse{status: resp.StatusCode, header: resp.Header.Clone(), body: b}
}

func canonicalParityBody(method, path string, body []byte) []byte {
	state := method == http.MethodGet && path == "/api/state"
	if !state && (method != http.MethodPost || path != "/api/notes") {
		return body
	}
	var value any
	if json.Unmarshal(body, &value) != nil {
		return body
	}
	if state {
		// These are sequential requests against a live machine: disk space
		// and system load can change between them. Preserve field presence,
		// types, storage policy, and all other response values in the comparison.
		if obj, ok := value.(map[string]any); ok {
			for section, keys := range map[string][]string{
				"storage": {"free_bytes"},
				"sys":     {"load1", "mem_pct", "swap_pct"},
			} {
				if fields, ok := obj[section].(map[string]any); ok {
					for _, key := range keys {
						if _, ok := fields[key].(float64); ok {
							fields[key] = 0
						}
					}
				}
			}
		}
		b, _ := json.Marshal(value)
		return b
	}
	var scrub func(any)
	scrub = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for key, child := range x {
				if key == "id" || strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_at") {
					x[key] = "<dynamic>"
					continue
				}
				scrub(child)
			}
		case []any:
			for _, child := range x {
				scrub(child)
			}
		}
	}
	scrub(value)
	b, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return b
}

func TestCanonicalParityStatePreservesContracts(t *testing.T) {
	base := `{"storage":{"free_bytes":100,"used_bytes":0,"writable":true},"sys":{"load1":1,"ncpu":4}}`
	want := canonicalParityBody(http.MethodGet, "/api/state", []byte(base))
	for _, tc := range []struct {
		name, old, replacement string
		equal                  bool
	}{
		{"free space changes", `"free_bytes":100`, `"free_bytes":90`, true},
		{"load changes", `"load1":1`, `"load1":2`, true},
		{"missing free space", `"free_bytes":100,`, ``, false},
		{"wrong free space type", `"free_bytes":100`, `"free_bytes":"100"`, false},
		{"usage changes", `"used_bytes":0`, `"used_bytes":1`, false},
		{"writability changes", `"writable":true`, `"writable":false`, false},
		{"CPU count changes", `"ncpu":4`, `"ncpu":2`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(base, tc.old, tc.replacement, 1)
			got := canonicalParityBody(http.MethodGet, "/api/state", []byte(body))
			if bytes.Equal(got, want) != tc.equal {
				t.Fatalf("equal = %v, want %v: %s", bytes.Equal(got, want), tc.equal, got)
			}
		})
	}
}
