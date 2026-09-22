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

	"github.com/scimux/scimux/internal/backend"
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
	if method != http.MethodPost || path != "/api/notes" {
		return body
	}
	var value any
	if json.Unmarshal(body, &value) != nil {
		return body
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
