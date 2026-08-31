package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// AT-FR-17-a: A tunnel request carrying no browser headers succeeds through
// the tunnel handler. Reaching the boundary is the authority; nothing is
// derived from Host, Origin, Sec-Fetch-Site or the CSRF token.
func TestAT_FR_17_a_ChannelRequestSucceedsThroughTunnel(t *testing.T) {
	const at = "AT-FR-17-a"
	_, tunnel := s3Boundaries(t)

	rec := s3Serve(tunnel, s3ChannelRequest(http.MethodGet, "/api/state", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: GET /api/state over the tunnel = %d, want 200 (%s)", at, rec.Code, rec.Body.String())
	}

	// Unsafe, and deliberately with no X-Scimux-CSRF: the tunnel boundary
	// neither requires nor inspects it (maintainer decision, 2026-08-17).
	rec = s3Serve(tunnel, s3ChannelRequest(http.MethodPost, "/api/nodes/node-1/send/interrupt", "{}"))
	if s3Rejected(rec) {
		t.Fatalf("%s: unsafe tunnel request rejected at the boundary = %d (%s)", at, rec.Code, rec.Body.String())
	}
}

// AT-FR-17-b: The very same request is rejected by the HTTP boundary. This is
// the other half of the two-sided assertion the plan requires: dropped over
// the tunnel, still enforced on localhost, so a later "fix" that restores the
// dropped header cannot pass unnoticed.
func TestAT_FR_17_b_SameRequestRejectedByHTTPBoundary(t *testing.T) {
	const at = "AT-FR-17-b"
	local, _ := s3Boundaries(t)

	rec := s3Serve(local, s3ChannelRequest(http.MethodGet, "/api/state", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%s: header-less GET through the browser boundary = %d, want 403", at, rec.Code)
	}

	rec = s3Serve(local, s3ChannelRequest(http.MethodPost, "/api/nodes/node-1/send/interrupt", "{}"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%s: header-less unsafe request through the browser boundary = %d, want 403", at, rec.Code)
	}
}

// AT-FR-17-c: The tunnel handler is not reachable from the TCP listener, and
// the HTTP boundary is not reachable from the channel. Neither boundary can
// be talked into becoming the other by anything in the request.
func TestAT_FR_17_c_BoundariesAreNotCrossReachable(t *testing.T) {
	const at = "AT-FR-17-c"
	local, tunnel := s3Boundaries(t)

	// No header may buy peer authority on the local path. A request that
	// claims to be a proven peer is still Host-checked and still rejected.
	claim := s3ChannelRequest(http.MethodGet, "/api/state", "")
	claim.Header.Set("X-Scimux-Peer", s3Peer().DeviceID)
	claim.Header.Set("X-Scimux-Tunnel", "1")
	if rec := s3Serve(local, claim); rec.Code != http.StatusForbidden {
		t.Fatalf("%s: a peer-claiming header reached the local mux = %d, want 403", at, rec.Code)
	}

	// Conversely the tunnel does not run the Host guard, so a Host that the
	// local policy would reject is simply irrelevant there — it is never
	// consulted, not merely tolerated.
	hostile := s3ChannelRequest(http.MethodGet, "/api/state", "")
	hostile.Host = "evil.example.com"
	if rec := s3Serve(tunnel, hostile); rec.Code != http.StatusOK {
		t.Fatalf("%s: tunnel consulted Host (= %d, want 200); the tunnel has no dialed authority", at, rec.Code)
	}
	if rec := s3Serve(local, hostile); rec.Code != http.StatusForbidden {
		t.Fatalf("%s: hostile Host accepted by the browser boundary = %d, want 403", at, rec.Code)
	}
}

// AT-FR-17-d: Content-type validation still applies on the tunnel path. The
// tunnel drops the browser guards; it does not drop defense in depth.
func TestAT_FR_17_d_ContentTypeStillValidatedOnTunnel(t *testing.T) {
	const at = "AT-FR-17-d"
	_, tunnel := s3Boundaries(t)

	req := s3ChannelRequest(http.MethodPost, "/api/nodes/node-1/send", "{}")
	req.Header.Set("Content-Type", "text/plain")
	if rec := s3Serve(tunnel, req); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("%s: text/plain over the tunnel = %d, want 415", at, rec.Code)
	}

	// multipart stays confined to the attachment route on both boundaries.
	req = s3ChannelRequest(http.MethodPost, "/api/nodes/node-1/send", "{}")
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	if rec := s3Serve(tunnel, req); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("%s: multipart on a non-attachment route over the tunnel = %d, want 415", at, rec.Code)
	}
}

// AT-FR-17-e: The behaviour is asserted at the two constructors — the browser
// boundary rejects a header-less request, the tunnel boundary accepts it and
// derives authority from the channel. Deliberately not a source grep: a grep
// test is bypassed by any alias or helper and breaks on comments.
func TestAT_FR_17_e_AssertedAtTheTwoConstructors(t *testing.T) {
	const at = "AT-FR-17-e"
	a := newTestApp(t, &fakeTmux{})

	local, tunnel, err := newBoundaries(a, webFS, s3Peer())
	if err != nil {
		t.Fatalf("%s: newBoundaries: %v", at, err)
	}
	if local == nil || tunnel == nil {
		t.Fatalf("%s: newBoundaries returned nil (local=%v tunnel=%v)", at, local, tunnel)
	}

	req := s3ChannelRequest(http.MethodGet, "/api/state", "")
	if rec := s3Serve(local, req); rec.Code != http.StatusForbidden {
		t.Fatalf("%s: browser constructor accepted a header-less request = %d, want 403", at, rec.Code)
	}
	if rec := s3Serve(tunnel, s3ChannelRequest(http.MethodGet, "/api/state", "")); rec.Code != http.StatusOK {
		t.Fatalf("%s: tunnel constructor rejected a header-less request = %d, want 200", at, rec.Code)
	}

	// The prohibition itself, asserted at the boundary rather than in source:
	// whatever the tunnel forwards must not have acquired any browser header.
	var got *http.Request
	spy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	withTunnelBoundary(s3Peer(), spy).ServeHTTP(rec, s3ChannelRequest(http.MethodPost, "/api/nodes/node-1/send", "{}"))
	if got == nil {
		t.Fatalf("%s: tunnel boundary forwarded nothing (status %d: %s)", at, rec.Code, rec.Body.String())
	}
	if got.Host != "" {
		t.Fatalf("%s: tunnel boundary synthesised Host = %q", at, got.Host)
	}
	for _, h := range tunnelForbiddenHeaders() {
		if v := got.Header.Get(h); v != "" {
			t.Fatalf("%s: tunnel boundary synthesised %s = %q; FR-17 forbids forging exactly what the browser guards check", at, h, v)
		}
	}
}

// AT-FR-17-f: A route matrix generated from the owned mux exercises every
// route through both boundaries, asserting only the intended differences —
// not two hand-picked examples. The inventory is reused, never re-invented.
func TestAT_FR_17_f_EveryRouteThroughBothBoundaries(t *testing.T) {
	const at = "AT-FR-17-f"
	local, tunnel := s3Boundaries(t)

	routes := characterizationAPIRoutes()
	if len(routes) == 0 {
		t.Fatalf("%s: empty route inventory", at)
	}

	for _, route := range routes {
		t.Run(route.method+" "+route.pattern, func(t *testing.T) {
			body := s3UnsafeBody(route.method)

			// Over the channel: no browser headers at all, and never a
			// boundary rejection. The handler's own status is not our
			// business here.
			if rec := s3Serve(tunnel, s3ChannelRequest(route.method, route.path, body)); s3Rejected(rec) {
				t.Fatalf("%s: %s %s rejected at the tunnel boundary = %d (%s)",
					at, route.method, route.path, rec.Code, rec.Body.String())
			}

			// The same header-less request on the browser boundary is
			// always refused, safe method or not, because Host fails first.
			if rec := s3Serve(local, s3ChannelRequest(route.method, route.path, body)); rec.Code != http.StatusForbidden {
				t.Fatalf("%s: %s %s header-less on the browser boundary = %d, want 403",
					at, route.method, route.path, rec.Code)
			}

			// The intended difference is exactly the CSRF token on unsafe
			// methods: still required locally, never consulted remotely.
			switch route.method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				return
			}
			untokened := httptest.NewRequest(route.method, "http://127.0.0.1:8787"+route.path, nil)
			untokened.Header.Set("Content-Type", "application/json")
			if rec := s3Serve(local, untokened); rec.Code != http.StatusForbidden {
				t.Fatalf("%s: %s %s without a CSRF token on the browser boundary = %d, want 403",
					at, route.method, route.path, rec.Code)
			}
		})
	}
}
