package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// AT-FR-15-a: S3 splits one boundary into two; it does not weaken the local
// guard for remote's benefit. This is
// the localhost half of the two-sided CSRF assertion — the tunnel half lives
// in AT-FR-17-a/f.
func TestAT_FR_15_a_LocalBoundaryUnchanged(t *testing.T) {
	const at = "AT-FR-15-a"
	local, _ := s3Boundaries(t)

	// An ordinary browser request with the real token still works.
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/state", nil)
	if rec := s3Serve(local, req); rec.Code != http.StatusOK {
		t.Fatalf("%s: ordinary local GET = %d, want 200", at, rec.Code)
	}

	// The CSRF token is still the write-side boundary locally.
	unsafe := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/nodes/node-1/send/interrupt", nil)
	if rec := s3Serve(local, unsafe); rec.Code != http.StatusForbidden {
		t.Fatalf("%s: local unsafe request without a token = %d, want 403", at, rec.Code)
	}
	unsafe = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/nodes/node-1/send/interrupt", nil)
	unsafe.Header.Set("X-Scimux-CSRF", csrfToken)
	if rec := s3Serve(local, unsafe); rec.Code == http.StatusForbidden {
		t.Fatalf("%s: local unsafe request with the real token was rejected = %d", at, rec.Code)
	}

	// Browser defenses and the Fetch Metadata guard remain local-boundary
	// responsibilities.
	fetchMeta := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/state", nil)
	fetchMeta.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := s3Serve(local, fetchMeta); rec.Code != http.StatusForbidden {
		t.Fatalf("%s: cross-site Fetch Metadata on the local path = %d, want 403", at, rec.Code)
	}
	rec := s3Serve(local, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/state", nil))
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("%s: local X-Frame-Options = %q, want DENY", at, got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != localCSP {
		t.Fatalf("%s: local CSP = %q, want %q", at, got, localCSP)
	}
}

// FR-28 (server half): the remote response-side defenses are stamped by the
// tunnel boundary, not inherited from the browser one, so the two policies
// cannot be confused. Asserted per route, not once.
func TestAT_FR_28_ServerHalfTunnelStampsRemoteDefenses(t *testing.T) {
	const at = "AT-FR-28"
	_, tunnel := s3Boundaries(t)

	want := tunnelResponseHeaders()
	if len(want) == 0 {
		t.Fatalf("%s: tunnelResponseHeaders() is empty; FR-28 owes an explicit remote policy", at)
	}
	for _, required := range []string{
		"Content-Security-Policy",
		"X-Content-Type-Options",
		"Referrer-Policy",
		"Permissions-Policy",
	} {
		if want[required] == "" {
			t.Fatalf("%s: no remote %s defined", at, required)
		}
	}
	if got := want["X-Content-Type-Options"]; got != "nosniff" {
		t.Fatalf("%s: X-Content-Type-Options = %q, want nosniff", at, got)
	}
	pp := strings.ToLower(want["Permissions-Policy"])
	if !strings.Contains(pp, "camera=()") || !strings.Contains(pp, "microphone=()") {
		t.Fatalf("%s: Permissions-Policy %q must deny camera and microphone", at, want["Permissions-Policy"])
	}
	if strings.EqualFold(want["Content-Security-Policy"], "frame-ancestors 'none'") {
		t.Fatalf("%s: the remote CSP is the localhost policy verbatim; FR-28 wants an explicit remote policy", at)
	}
	if ref := strings.ToLower(want["Referrer-Policy"]); ref != "no-referrer" && ref != "same-origin" {
		t.Fatalf("%s: Referrer-Policy = %q is not restrictive enough to keep rendezvous IDs out of referrers", at, want["Referrer-Policy"])
	}

	for _, route := range characterizationAPIRoutes() {
		t.Run(route.method+" "+route.pattern, func(t *testing.T) {
			rec := s3Serve(tunnel, s3ChannelRequest(route.method, route.path, s3UnsafeBody(route.method)))
			for name, value := range want {
				if got := rec.Header().Get(name); got != value {
					t.Fatalf("%s: %s %s response %s = %q, want %q",
						at, route.method, route.path, name, got, value)
				}
			}
		})
	}
}

// FR-28 / decided 2026-08-17: no gzip over the data channel. The browser's
// HTTP stack gunzips transparently; our own JS reassembles channel frames and
// nothing would decompress them, so the wrapper must not be inherited.
func TestAT_FR_28_TunnelNeverCompresses(t *testing.T) {
	const at = "AT-FR-28-gzip"
	_, tunnel := s3Boundaries(t)

	req := s3ChannelRequest(http.MethodGet, "/api/state", "")
	req.Header.Set("Accept-Encoding", "gzip")
	rec := s3Serve(tunnel, req)
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("%s: tunnel response Content-Encoding = %q, want none (the codec carries identity bytes)", at, got)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: tunnel GET /api/state = %d, want 200", at, rec.Code)
	}
}
