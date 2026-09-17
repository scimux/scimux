package app

import (
	"io/fs"
	"net/http"
)

// S3 tunnel boundary (FR-17, FR-28 server half, FR-15).
//
// The shape is fixed by the plan: the owned mux is constructed once and
// wrapped by *two* boundary constructors — the browser/TCP boundary exactly
// as it is today, and a tunnel boundary whose authority is the proven peer.
// The tunnel boundary must never synthesise Host, Origin, Sec-Fetch-Site or
// X-Scimux-CSRF: reusing those would mean forging exactly what they check.

// tunnelPeer is a remote peer whose identity has already been proven by the
// transport (S5 pairing/enrollment). Reaching the tunnel boundary at all is
// the authority; the boundary does not re-derive it from request headers.
//
// A paired device has the same authority as the local operator: pairing is
// the owner's own call (maintainer decision, 2026-08-22). Any narrower tier
// is RBAC sharing and is deliberately out of scope for V1 — this type is the
// place such a tier would later hang, not a hint that one exists now.
type tunnelPeer struct {
	// DeviceID is the paired device's stable identity.
	DeviceID string
	// RID is the rendezvous ID the channel was established over.
	RID string
}

// tunnelForbiddenHeaders are the request headers the tunnel boundary must
// never set on a request it forwards. FR-17 is a prohibition on synthesis,
// so the test asserts over this list at the boundary rather than by grepping
// source (AT-FR-17-e: a grep test is bypassed by any alias).
func tunnelForbiddenHeaders() []string {
	return []string{"Host", "Origin", "Referer", "Sec-Fetch-Site", "X-Scimux-CSRF"}
}

// remoteCSP is the FR-28 remote content-security policy. It is deliberately
// *not* the localhost "frame-ancestors 'none'" string: the two boundaries own
// two different policies, and confusing them is exactly the failure FR-28
// exists to prevent.
//
// Every network source is 'self'. This header describes responses carried over
// the authenticated tunnel; it does not set or replace the top-level viewer
// document's HTTP CSP. The independently served trusted viewer must enforce
// that policy before its bootstrap executes. data: is allowed for images only
// (inline SVG/PNG data URIs the UI already emits); object-src, base-uri,
// form-action and frame-ancestors are closed outright.
//
// blob: is required for scripts, styles and images, and is not optional
// dressing: 'self' does NOT cover blob: URLs, which carry an opaque origin and
// must be listed explicitly. FR-40 boots the remote app by fetching the module
// graph over the channel, rewriting import specifiers to blob: URLs behind an
// import map, and giving the 10 stylesheets their own channel-fetch and blob:
// path; AT-FR-42-c requires DOM asset URLs to resolve to blob: URLs from the
// channel. Omitting blob: here does not fail any S3 test — it fails the whole
// remote app at S6, silently, as a CSP violation in the console. Adding it
// grants the rendezvous origin nothing.
const remoteCSP = "default-src 'self'; script-src 'self' blob:; " +
	"style-src 'self' blob:; img-src 'self' data: blob:; font-src 'self' blob:; " +
	"connect-src 'self'; media-src 'self' blob:; " +
	"object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// tunnelResponseHeaders is the FR-28 server half: the response-side browser
// defenses stamped by the *tunnel* boundary, never inherited from the
// localhost one, so the two policies cannot be confused.
//
// Referrer-Policy is no-referrer because a rendezvous ID must never leave in
// a Referer header. Permissions-Policy denies camera and microphone: scimux
// supervises text, and a remote surface has no business asking for either.
func tunnelResponseHeaders() map[string]string {
	return map[string]string{
		"Content-Security-Policy": remoteCSP,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Permissions-Policy":      "camera=(), microphone=(), geolocation=(), payment=(), usb=()",
		"X-Frame-Options":         "DENY",
	}
}

// newBoundaries constructs the owned mux once and returns the two boundary
// handlers over it. local is byte-for-byte today's chain (FR-15); tunnel is
// the peer-authenticated boundary (FR-17). These are the two constructors
// AT-FR-17-e asserts behaviour at. The route table is registered exactly
// once, by newMux (router.go); neither boundary owns an inventory of its own.
func newBoundaries(a *app, web fs.FS, peer tunnelPeer) (local http.Handler, tunnel http.Handler, err error) {
	local, tunnelFor, err := newBoundaryFactory(a, web)
	if err != nil {
		return nil, nil, err
	}
	return local, tunnelFor(peer), nil
}

// newBoundaryFactory is newBoundaries for a run that does not yet know its
// peers, which is every real one: a device is proven in the rendezvous wait
// loop, long after startup. The owned mux is still built exactly once — so a
// route-table error is a startup error and never a mid-session refusal the
// device would see as "unavailable" — and each proven peer gets its own
// tunnel boundary over that one mux. local is byte-for-byte NewHandler's
// chain (FR-15).
func newBoundaryFactory(a *app, web fs.FS) (local http.Handler, tunnel func(tunnelPeer) http.Handler, err error) {
	mux, err := newMux(a, web)
	if err != nil {
		return nil, nil, err
	}
	return withLocalBoundary(a, mux), func(peer tunnelPeer) http.Handler {
		return withTunnelBoundary(peer, mux)
	}, nil
}

// withTunnelBoundary wraps the owned mux for a proven peer. It stamps the
// FR-28 remote response headers, retains content-type validation, and does
// not apply the Host, Fetch Metadata, same-origin or CSRF guards — those are
// the browser boundary's, and the tunnel does not borrow them. No gzip: the
// codec carries identity-encoded bytes (maintainer decision, 2026-08-17), and
// the CSRF token is neither required nor inspected (maintainer decision,
// 2026-08-17) — a paired peer has the same authority as the local operator.
//
// The peer is carried for the handler chain's benefit only: reaching here is
// already the authority, so nothing about the request is re-derived from it
// and — crucially — none of tunnelForbiddenHeaders() is ever written onto the
// forwarded request. Forging Host/Origin/Sec-Fetch-Site/X-Scimux-CSRF to make
// the browser guards pass would be forging exactly what they check, so those
// guards are dropped rather than satisfied.
func withTunnelBoundary(peer tunnelPeer, next http.Handler) http.Handler {
	_ = peer
	headers := tunnelResponseHeaders()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stamped on every response — success, handler error and boundary
		// rejection alike (FR-28), mirroring how the browser boundary stamps
		// its own, different policy.
		for name, value := range headers {
			w.Header().Set(name, value)
		}
		// Defense in depth is retained even though the browser guards are
		// not: an unsafe request still has to carry a content type the API
		// actually accepts.
		if !safeMethod(r.Method) && !validContentType(r) {
			http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
			return
		}
		// No gzip wrapper: the codec carries identity bytes and nothing
		// downstream would decompress them (maintainer decision 2026-08-17).
		next.ServeHTTP(w, r)
	})
}
