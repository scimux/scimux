// security.go — request-boundary policy (trusted Host, Fetch Metadata,
// anti-framing), mutation guard (CSRF/origin/content-type), shared JSON body
// bound, and JSON response encoding.
//
// Ownership:
//   - NewHandler returns withGzip(withRequestBoundary(policy, guardMutations(mux))):
//     gzip is outermost on the response path; the request boundary wraps the
//     complete mux so Host validation, Fetch Metadata, and anti-framing
//     headers apply to static files, the token-bearing index, every API, and
//     router 404/405. Registration remains in router.go.
//   - Host validation is the primary browser/loopback boundary. Origin, Fetch
//     Metadata, CORS, and Private Network Access are not substitutes.
//   - Safe methods (GET/HEAD/OPTIONS) bypass the mutation checks but still
//     pass through the request boundary.
//   - Unsafe ordering remains same-origin → constant-time CSRF token comparison
//     → content type → routed handler.
//   - Multipart remains the attachment-path-suffix exception only.
//   - Feature handlers retain semantic decoding and status/error mapping;
//     security.go owns only the shared jsonBodyMax bound and writeJSON encoding.
//   - CSRF index substitution and static serving remain in web.go.
//   - ui_state_api.go keeps its separate opaque UI body limit (uiStateMax).
package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mimepkg "mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// jsonBodyMax bounds every JSON request body (prompts included; 1 MiB is
// generous). -addr may be bound wider than loopback, so unbounded decodes
// would be an easy memory-exhaustion hole (R18.6). /api/ui has its own,
// larger uiStateMax limit.
const jsonBodyMax = 1 << 20

// decodeJSON decodes a bounded JSON request body.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyMax)
	return json.NewDecoder(r.Body).Decode(v)
}

var (
	errEmptyHost     = errors.New("empty host")
	errMalformedHost = errors.New("malformed host")
)

// Documented localhost name trusted for a loopback or wildcard listener.
// Additional names such as localhost.localdomain are ordinary DNS and must
// be opted in with -trusted-host. Ports are not part of host identity
// (SSH forwarding may rewrite the client port), so only the canonical name
// is stored.
var localhostNames = []string{"localhost"}

type listenerKind int

const (
	listenerLoopback listenerKind = iota
	listenerConcrete
	listenerWildcard
)

// requestPolicy is the single request-boundary host policy, configured from
// -addr plus optional -trusted-host values. A nil policy is treated as the
// default loopback listener (127.0.0.1) so tests and NewHandler stay aligned
// with the shipped default.
type requestPolicy struct {
	kind              listenerKind
	trusted           map[string]struct{}
	allowAnyIPLiteral bool
}

func defaultLoopbackPolicy() *requestPolicy {
	p, err := newRequestPolicy("127.0.0.1:8787", nil)
	if err != nil {
		panic("scimux: default loopback host policy: " + err.Error())
	}
	return p
}

// newRequestPolicy builds the Host-trust policy for a listen address.
//
//   - Loopback listener (127.0.0.0/8, ::1, or localhost): trust every
//     loopback IP literal and the documented localhost name.
//   - Concrete named or IP listener: trust that configured identity only,
//     plus any explicit extras. Other IP literals and DNS names are rejected.
//   - Wildcard listener (empty host, 0.0.0.0, ::): trust any syntactically
//     valid IP literal and the documented localhost name. Arbitrary DNS
//     Host values require an explicit -trusted-host entry. Accessing a
//     wildcard bind by IP cannot be DNS-rebinding — the Host is the IP.
//
// Ports are never part of host identity.
func newRequestPolicy(listenAddr string, extra []string) (*requestPolicy, error) {
	host, err := listenHost(listenAddr)
	if err != nil {
		return nil, err
	}
	p := &requestPolicy{trusted: map[string]struct{}{}}
	switch {
	case host == "" || isUnspecifiedIP(host):
		p.kind = listenerWildcard
		p.allowAnyIPLiteral = true
		addLocalhostNames(p)
	case isLoopbackHost(host):
		p.kind = listenerLoopback
		addLocalhostNames(p)
	default:
		p.kind = listenerConcrete
		canon, err := canonicalizeHost(host)
		if err != nil {
			return nil, fmt.Errorf("listen address host %q: %w", host, err)
		}
		p.trusted[canon] = struct{}{}
	}
	for _, e := range extra {
		canon, err := canonicalizeHost(e)
		if err != nil {
			return nil, fmt.Errorf("trusted-host %q: %w", e, err)
		}
		p.trusted[canon] = struct{}{}
	}
	return p, nil
}

func addLocalhostNames(p *requestPolicy) {
	for _, name := range localhostNames {
		p.trusted[name] = struct{}{}
	}
}

func (p *requestPolicy) allow(hostHeader string) bool {
	if p == nil {
		p = defaultLoopbackPolicy()
	}
	canon, err := canonicalizeHost(hostHeader)
	if err != nil {
		return false
	}
	if _, ok := p.trusted[canon]; ok {
		return true
	}
	if ip := net.ParseIP(canon); ip != nil {
		if p.kind == listenerLoopback && ip.IsLoopback() {
			return true
		}
		if p.allowAnyIPLiteral {
			return true
		}
	}
	return false
}

// listenHost extracts the bind host from an -addr value. An empty host
// (":8787") is a wildcard. Ports are discarded.
func listenHost(addr string) (string, error) {
	if addr == "" {
		return "", errEmptyHost
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host, nil
	}
	if strings.HasPrefix(addr, "[") {
		if strings.HasSuffix(addr, "]") && len(addr) > 2 {
			return addr[1 : len(addr)-1], nil
		}
		return "", errMalformedHost
	}
	if strings.Count(addr, ":") == 0 {
		return addr, nil
	}
	return "", fmt.Errorf("malformed listen address %q", addr)
}

func isUnspecifiedIP(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

func isLoopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	canon, err := canonicalizeHost(host)
	if err != nil {
		return false
	}
	for _, name := range localhostNames {
		if canon == name {
			return true
		}
	}
	return false
}

// canonicalizeHost turns a Host header or configured name into a comparable
// identity: lowercase, optional trailing dot stripped, IPv4/IPv6 literals
// via net.ParseIP, ports discarded. Userinfo, comma-separated values, empty
// Host, and invalid bracketed IPv6 fail closed.
func canonicalizeHost(hostport string) (string, error) {
	if hostport == "" {
		return "", errEmptyHost
	}
	if strings.ContainsAny(hostport, "@,") || strings.Contains(hostport, "://") {
		return "", errMalformedHost
	}
	host, err := hostFromHostPort(hostport)
	if err != nil {
		return "", err
	}
	if host == "" {
		return "", errMalformedHost
	}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), nil
		}
		return ip.String(), nil
	}
	host = strings.ToLower(host)
	host = strings.TrimSuffix(host, ".")
	if host == "" || strings.ContainsAny(host, `/\: `) {
		return "", errMalformedHost
	}
	return host, nil
}

// hostFromHostPort splits a Host header with net.SplitHostPort. IPv6 is only
// accepted in bracketed form ([::1] or [::1]:port); unbracketed multi-colon
// values are malformed HTTP Host headers.
func hostFromHostPort(hostport string) (string, error) {
	if strings.HasPrefix(hostport, "[") {
		if host, port, err := net.SplitHostPort(hostport); err == nil {
			if err := validTCPPort(port); err != nil {
				return "", err
			}
			if net.ParseIP(host) == nil {
				return "", errMalformedHost
			}
			return host, nil
		}
		if strings.HasSuffix(hostport, "]") && !strings.Contains(hostport, "]:") && len(hostport) > 2 {
			inner := hostport[1 : len(hostport)-1]
			if net.ParseIP(inner) == nil {
				return "", errMalformedHost
			}
			return inner, nil
		}
		return "", errMalformedHost
	}
	if host, port, err := net.SplitHostPort(hostport); err == nil {
		if err := validTCPPort(port); err != nil {
			return "", err
		}
		return host, nil
	}
	if strings.Count(hostport, ":") == 0 {
		return hostport, nil
	}
	return "", errMalformedHost
}

func validTCPPort(port string) error {
	if port == "" {
		return errMalformedHost
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return errMalformedHost
	}
	return nil
}

// withRequestBoundary is the outermost *request* wrapper (gzip is outermost
// on the response path only). It stamps anti-framing headers on every
// response — success, handler error, and boundary rejection — then validates
// Host before the mux sees the request, then rejects browser /api/* fetches
// marked cross-site or same-site.
func withRequestBoundary(p *requestPolicy, next http.Handler) http.Handler {
	if p == nil {
		p = defaultLoopbackPolicy()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		if !p.allow(r.Host) {
			http.Error(w, "untrusted host", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			switch r.Header.Get("Sec-Fetch-Site") {
			case "cross-site", "same-site":
				http.Error(w, "cross-site request rejected", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// csrfToken is a per-process secret embedded in the served index page (as the
// scimux-csrf meta tag) and echoed back by the UI in the X-Scimux-CSRF header
// on every unsafe request. It is the write-side security boundary: scimux is
// intentionally unauthenticated and usually loopback-bound, but a loopback
// service is still reachable by any web page the operator happens to open, and
// the unsafe API can send prompts, answer approvals, interrupt turns, upload
// files and self-update. A cross-origin page cannot read this token (the
// same-origin policy hides the HTML body) and cannot forge the custom header on
// a simple request, so requiring it closes the CSRF surface with no dependency.
var csrfToken = mustToken()

func mustToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// A weak/empty token would silently defeat the control it exists to be;
		// fail loudly at startup instead.
		panic("scimux: generate CSRF token: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// guardMutations enforces same-origin + CSRF-token on every unsafe method
// before the request reaches a handler. Safe methods (GET/HEAD/OPTIONS) pass
// through untouched — they neither mutate state nor are readable cross-origin
// without CORS, which scimux never grants.
func guardMutations(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Scimux-CSRF")), []byte(csrfToken)) != 1 {
			http.Error(w, "missing or invalid CSRF token", http.StatusForbidden)
			return
		}
		if !validContentType(r) {
			http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin rejects a browser request whose Origin (or, absent that, Referer)
// names a different host than the one it was sent to. A missing Origin *and*
// Referer is allowed: non-browser clients (curl, scripts) send neither, and for
// them the CSRF token is the gate. Browsers always attach Origin to unsafe
// cross-origin fetches, so this catches the case the token alone would not (a
// buggy client that leaked the token can still not be driven cross-origin).
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		if ref := r.Header.Get("Referer"); ref != "" {
			if u, err := url.Parse(ref); err == nil {
				origin = u.Scheme + "://" + u.Host
			}
		}
	}
	if origin == "" {
		return true // non-browser client; the token requirement still applies
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// validContentType keeps unsafe requests to the content types the API actually
// accepts: JSON everywhere, multipart only on the attachment upload route. A
// bodyless request (interrupt, resolve, delete) carries no Content-Type and is
// fine. This is defense in depth behind the token — it also blocks the classic
// simple-request form POST (which cannot set the token header anyway).
func validContentType(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return true
	}
	mediaType, _, err := mimepkg.ParseMediaType(ct)
	if err != nil {
		return false
	}
	if mediaType == "multipart/form-data" {
		return strings.HasSuffix(r.URL.Path, "/attachments")
	}
	return mediaType == "application/json"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
