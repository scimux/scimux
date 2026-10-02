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
	"sync/atomic"
	"testing"

	"github.com/scimux/scimux/internal/acp"
	"github.com/scimux/scimux/internal/backend"
)

// TestLegacyAndSplitVibeInspectPostMatch compares the monolith and the split
// server for the one POST the frozen HandleFunc inventory does not list.
// OPTIONS/Allow, a missing CSRF token, and the accepted and rejected bodies
// must agree. Ordinary GET stays on the refresh path.
func TestLegacyAndSplitVibeInspectPostMatch(t *testing.T) {
	binDir := t.TempDir()
	writeScript(t, binDir, "vibe-acp", `exit 99`)
	t.Setenv("PATH", binDir)
	var probes atomic.Int32
	previousProbe := probeVibeCatalog
	probeVibeCatalog = func(context.Context, string) (acp.VibeCatalog, error) {
		probes.Add(1)
		return acp.VibeCatalog{
			Models:  []string{"synthetic-model"},
			Efforts: map[string][]string{"synthetic-model": {"low"}},
		}, nil
	}
	t.Cleanup(func() { probeVibeCatalog = previousProbe })
	quietSources := func() map[string]harnessSource { return map[string]harnessSource{} }

	legacyApp := newTestApp(t, &fakeTmux{})
	legacyApp.harnessLatestSources = quietSources
	legacy, err := NewHandler(legacyApp, webFS)
	if err != nil {
		t.Fatal(err)
	}
	legacyServer := httptest.NewServer(legacy)
	defer legacyServer.Close()

	splitApp := newTestApp(t, &fakeTmux{})
	splitApp.harnessLatestSources = quietSources
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
	split, err := newWebBackend(webBackendConfig{Web: webFS, Core: client.Proxy()})
	if err != nil {
		t.Fatal(err)
	}
	splitServer := httptest.NewServer(split.local)
	defer splitServer.Close()

	cases := []struct {
		name, method, body string
		csrf               bool
		want               int
		allow              []string
	}{
		{name: "accepted inspect", method: http.MethodPost, body: `{"inspect_vibe":true}`, csrf: true, want: http.StatusOK},
		{name: "accepted ordinary false", method: http.MethodPost, body: `{"inspect_vibe":false}`, csrf: true, want: http.StatusOK},
		{name: "rejected malformed", method: http.MethodPost, body: `{`, csrf: true, want: http.StatusBadRequest},
		{name: "rejected string", method: http.MethodPost, body: `{"inspect_vibe":"true"}`, csrf: true, want: http.StatusBadRequest},
		{name: "rejected csrf", method: http.MethodPost, body: `{"inspect_vibe":true}`, csrf: false, want: http.StatusForbidden},
		{name: "options", method: http.MethodOptions, want: http.StatusMethodNotAllowed, allow: []string{http.MethodGet, http.MethodPost}},
		{name: "ordinary get", method: http.MethodGet, want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := probes.Load()
			old := vibeParityRequest(t, legacyServer.URL, tc.method, tc.body, tc.csrf)
			fresh := vibeParityRequest(t, splitServer.URL, tc.method, tc.body, tc.csrf)
			if old.status != tc.want || fresh.status != tc.want {
				t.Fatalf("status old=%d split=%d, want %d\nold=%q\nsplit=%q", old.status, fresh.status, tc.want, old.body, fresh.body)
			}
			if string(old.body) != string(fresh.body) {
				t.Fatalf("body differs\nold=%q\nsplit=%q", old.body, fresh.body)
			}
			if old.header.Get("Allow") != fresh.header.Get("Allow") {
				t.Fatalf("Allow old=%q split=%q", old.header.Get("Allow"), fresh.header.Get("Allow"))
			}
			for _, method := range tc.allow {
				if !allowContains(old.header.Get("Allow"), method) {
					t.Fatalf("Allow = %q, missing %s", old.header.Get("Allow"), method)
				}
			}
			wantProbes := int32(0)
			if tc.name == "accepted inspect" {
				wantProbes = 2
				var body struct {
					Agents map[string]agentInfo `json:"agents"`
				}
				if err := json.Unmarshal(old.body, &body); err != nil {
					t.Fatalf("decode inspected catalog: %v", err)
				}
				vibe := body.Agents["vibe"]
				if len(vibe.Models) != 1 || vibe.Models[0] != "synthetic-model" ||
					len(vibe.Efforts["synthetic-model"].Levels) != 1 || vibe.Efforts["synthetic-model"].Levels[0] != "low" {
					t.Fatalf("POST lost inspected catalog: %+v", vibe)
				}
			}
			if got := probes.Load() - before; got != wantProbes {
				t.Fatalf("probe calls = %d, want %d", got, wantProbes)
			}
		})
	}
}

func vibeParityRequest(t *testing.T, base, method, body string, csrf bool) parityResponse {
	t.Helper()
	req, err := http.NewRequest(method, base+"/api/harnesses/latest", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		req.Header.Set("Content-Type", "application/json")
		if csrf {
			req.Header.Set("X-Scimux-CSRF", csrfToken)
		}
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
