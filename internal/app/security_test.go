package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// guardMutations is the write-side security boundary: every unsafe method must
// be same-origin and carry the process CSRF token, with a content-type gate
// behind that. Safe methods pass untouched.
func TestGuardMutations(t *testing.T) {
	reached := false
	h := guardMutations(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(200)
	}))

	call := func(method string, mut func(*http.Request)) (int, bool) {
		reached = false
		req := httptest.NewRequest(method, "http://127.0.0.1:8787/api/nodes/n1/key", strings.NewReader(`{"key":"y"}`))
		req.Header.Set("Content-Type", "application/json")
		if mut != nil {
			mut(req)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, reached
	}

	token := func(r *http.Request) { r.Header.Set("X-Scimux-CSRF", csrfToken) }

	// GET is always allowed and never needs a token.
	if code, ok := call("GET", nil); code != 200 || !ok {
		t.Errorf("GET blocked: code=%d reached=%v", code, ok)
	}

	// Unsafe with no token → 403, handler never runs (this is the CSRF case:
	// a blind {"key":"y"} from a foreign page must not approve a dialog).
	if code, ok := call("POST", nil); code != 403 || ok {
		t.Errorf("POST without token: code=%d reached=%v, want 403/false", code, ok)
	}

	// Wrong token → 403.
	if code, ok := call("POST", func(r *http.Request) { r.Header.Set("X-Scimux-CSRF", "nope") }); code != 403 || ok {
		t.Errorf("POST wrong token: code=%d reached=%v", code, ok)
	}

	// Correct token, same-origin (no Origin header = non-browser) → allowed.
	if code, ok := call("POST", token); code != 200 || !ok {
		t.Errorf("POST valid token: code=%d reached=%v", code, ok)
	}

	// Correct token but cross-origin Origin → 403 (a leaked token still cannot
	// be driven from another origin).
	if code, ok := call("POST", func(r *http.Request) {
		token(r)
		r.Header.Set("Origin", "https://evil.example")
	}); code != 403 || ok {
		t.Errorf("cross-origin: code=%d reached=%v, want 403", code, ok)
	}

	// Correct token and matching same-origin Origin → allowed.
	if code, ok := call("POST", func(r *http.Request) {
		token(r)
		r.Header.Set("Origin", "http://127.0.0.1:8787")
	}); code != 200 || !ok {
		t.Errorf("same-origin: code=%d reached=%v", code, ok)
	}

	// Token + origin ok, but a form content type → 415 (defense behind the token).
	if code, ok := call("POST", func(r *http.Request) {
		token(r)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}); code != 415 || ok {
		t.Errorf("form content type: code=%d reached=%v, want 415", code, ok)
	}

	// A bodyless unsafe request (interrupt/resolve/delete) carries no
	// content type and must still pass with a valid token.
	if code, ok := call("POST", func(r *http.Request) {
		token(r)
		r.Header.Del("Content-Type")
		r.Body = http.NoBody
	}); code != 200 || !ok {
		t.Errorf("bodyless POST: code=%d reached=%v", code, ok)
	}
}

// The multipart upload route is the one place a non-JSON content type is allowed.
func TestValidContentTypeMultipart(t *testing.T) {
	upload := httptest.NewRequest("POST", "/api/nodes/n1/attachments", nil)
	upload.Header.Set("Content-Type", "multipart/form-data; boundary=abc")
	if !validContentType(upload) {
		t.Error("multipart rejected on the upload route")
	}
	// multipart anywhere else is rejected.
	other := httptest.NewRequest("POST", "/api/nodes/n1/send", nil)
	other.Header.Set("Content-Type", "multipart/form-data; boundary=abc")
	if validContentType(other) {
		t.Error("multipart accepted off the upload route")
	}
}

// The served index page must carry the live token, not the placeholder.
func TestCSRFIndexEmbedsToken(t *testing.T) {
	b, err := csrfIndex(webFS)
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	if strings.Contains(page, csrfPlaceholder) {
		t.Error("placeholder was not substituted")
	}
	if !strings.Contains(page, `name="scimux-csrf" content="`+csrfToken+`"`) {
		t.Error("token not embedded in the page")
	}
}

// ---------- Packet 4E complementary characterization ----------
// Complements TestGuardMutations / TestValidContentTypeMultipart /
// TestCSRFIndexEmbedsToken, router mutation-outermost coverage, attachment
// multipart public routes, and TestHandleSendOversizedPrompt413. Does not
// re-walk every unsafe API path.

// securityCall exercises the production wrapper. body "" leaves Content-Type
// unset unless setCT is non-empty.
func securityCall(t *testing.T, h http.Handler, method, path, body, setCT string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, rdr)
	if setCT != "" {
		req.Header.Set("Content-Type", setCT)
	} else if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPublicSecuritySafeMethodsBypass(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	// GET is already covered by TestGuardMutations; characterize HEAD/OPTIONS
	// through the production wrapper (index / known API).
	for _, method := range []string{http.MethodHead, http.MethodOptions} {
		// HEAD / is a safe method; the index handler may or may not implement
		// HEAD, but the mutation guard must not return 403.
		rec := securityCall(t, h, method, "/", "", "", nil)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("%s / blocked by mutation guard: status=%d body=%q", method, rec.Code, rec.Body.String())
		}
		// OPTIONS on a registered API path yields 405 + Allow from the mux,
		// not a CSRF 403 — same safe-method bypass.
		rec = securityCall(t, h, method, "/api/state", "", "", nil)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("%s /api/state blocked by mutation guard: status=%d body=%q", method, rec.Code, rec.Body.String())
		}
	}
}

// Rejection order: same-origin → CSRF → content type → routed handler.
// Probe by combining failing earlier checks with later ones and asserting the
// earlier error text wins.
func TestPublicSecurityUnsafeRejectionOrder(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	path := "/api/nodes" // registered POST; also try unknown later

	// 1) Cross-origin wins over missing/wrong CSRF and bad content type.
	rec := securityCall(t, h, http.MethodPost, path, `{}`, "application/x-www-form-urlencoded", map[string]string{
		"Origin":        "https://evil.example",
		"X-Scimux-CSRF": "wrong-token",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin: status=%d, want 403; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cross-origin request rejected") {
		t.Fatalf("cross-origin body = %q, want origin rejection", rec.Body.String())
	}

	// 2) With origin ok (or absent), CSRF wins over bad content type.
	rec = securityCall(t, h, http.MethodPost, path, `{}`, "application/x-www-form-urlencoded", map[string]string{
		"X-Scimux-CSRF": "wrong-token",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong CSRF: status=%d, want 403; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing or invalid CSRF token") {
		t.Fatalf("wrong CSRF body = %q, want CSRF rejection", rec.Body.String())
	}

	// 3) Valid CSRF + bad content type → 415 (content type before handler).
	rec = securityCall(t, h, http.MethodPost, path, `{}`, "text/plain", map[string]string{
		"X-Scimux-CSRF": csrfToken,
	})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("bad content type: status=%d, want 415; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unsupported content type") {
		t.Fatalf("bad content type body = %q", rec.Body.String())
	}

	// 4) Valid CSRF + JSON on an unknown unsafe path → router 404 (handler stage).
	rec = securityCall(t, h, http.MethodPost, "/api/not-a-route", `{}`, "application/json", map[string]string{
		"X-Scimux-CSRF": csrfToken,
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown route with CSRF: status=%d, want 404; body=%q", rec.Code, rec.Body.String())
	}
}

func TestPublicSecurityOriginAndReferer(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	// Use a bodyless registered unsafe route so content-type is not in play.
	path := "/api/nodes/ghost/exit"

	// Matching Origin allowed.
	rec := securityCall(t, h, http.MethodPost, path, "", "", map[string]string{
		"X-Scimux-CSRF": csrfToken,
		"Origin":        "http://127.0.0.1:8787",
	})
	if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "cross-origin") {
		t.Fatalf("matching Origin rejected as cross-origin: body=%q", rec.Body.String())
	}

	// Mismatching Origin rejected.
	rec = securityCall(t, h, http.MethodPost, path, "", "", map[string]string{
		"X-Scimux-CSRF": csrfToken,
		"Origin":        "http://evil.example",
	})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "cross-origin request rejected") {
		t.Fatalf("mismatch Origin: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Referer fallback when Origin is absent: matching host allowed.
	rec = securityCall(t, h, http.MethodPost, path, "", "", map[string]string{
		"X-Scimux-CSRF": csrfToken,
		"Referer":       "http://127.0.0.1:8787/some/page",
	})
	if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "cross-origin") {
		t.Fatalf("matching Referer rejected as cross-origin: body=%q", rec.Body.String())
	}

	// Referer mismatch rejected when Origin is absent.
	rec = securityCall(t, h, http.MethodPost, path, "", "", map[string]string{
		"X-Scimux-CSRF": csrfToken,
		"Referer":       "https://evil.example/page",
	})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "cross-origin request rejected") {
		t.Fatalf("mismatch Referer: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Origin takes precedence over Referer: bad Origin + good Referer → reject.
	rec = securityCall(t, h, http.MethodPost, path, "", "", map[string]string{
		"X-Scimux-CSRF": csrfToken,
		"Origin":        "https://evil.example",
		"Referer":       "http://127.0.0.1:8787/",
	})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "cross-origin request rejected") {
		t.Fatalf("Origin precedence (bad Origin): status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Good Origin + bad Referer → allow past the origin gate (Referer ignored).
	rec = securityCall(t, h, http.MethodPost, path, "", "", map[string]string{
		"X-Scimux-CSRF": csrfToken,
		"Origin":        "http://127.0.0.1:8787",
		"Referer":       "https://evil.example/",
	})
	if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "cross-origin") {
		t.Fatalf("Origin precedence (good Origin): rejected via Referer; body=%q", rec.Body.String())
	}

	// Missing Origin and Referer: supported non-browser case (token is the gate).
	rec = securityCall(t, h, http.MethodPost, path, "", "", map[string]string{
		"X-Scimux-CSRF": csrfToken,
	})
	if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "cross-origin") {
		t.Fatalf("non-browser (no Origin/Referer) rejected as cross-origin: body=%q", rec.Body.String())
	}
	// Missing token still fails (CSRF remains the gate).
	rec = securityCall(t, h, http.MethodPost, path, "", "", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "missing or invalid CSRF token") {
		t.Fatalf("non-browser without token: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestPublicSecurityCSRFTokenCases(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	path := "/api/ui"
	body := `{}`

	// Valid token reaches the handler (PUT /api/ui without If-Match → 428).
	rec := securityCall(t, h, http.MethodPut, path, body, "application/json", map[string]string{
		"X-Scimux-CSRF": csrfToken,
	})
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("valid CSRF: status=%d, want 428; body=%q", rec.Code, rec.Body.String())
	}

	// Missing token → 403.
	rec = securityCall(t, h, http.MethodPut, path, body, "application/json", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "missing or invalid CSRF token") {
		t.Fatalf("missing CSRF: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Wrong token → 403.
	rec = securityCall(t, h, http.MethodPut, path, body, "application/json", map[string]string{
		"X-Scimux-CSRF": strings.Repeat("0", 64),
	})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "missing or invalid CSRF token") {
		t.Fatalf("wrong CSRF: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestCSRFTokenShape(t *testing.T) {
	// mustToken: 32 random bytes → 64 lowercase hex characters.
	if len(csrfToken) != 64 {
		t.Fatalf("csrfToken len = %d, want 64", len(csrfToken))
	}
	for i, c := range csrfToken {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("csrfToken[%d]=%q is not lowercase hex; token=%q", i, c, csrfToken)
		}
	}
}

func TestPublicSecurityBodylessAndContentTypes(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))

	// Bodyless unsafe request (no Content-Type) with valid token passes the
	// guard; registered exit on unknown node → 404 from the handler.
	rec := securityCall(t, h, http.MethodPost, "/api/nodes/ghost/exit", "", "", map[string]string{
		"X-Scimux-CSRF": csrfToken,
	})
	if rec.Code == http.StatusUnsupportedMediaType || rec.Code == http.StatusForbidden {
		t.Fatalf("bodyless unsafe: status=%d body=%q (guard should pass)", rec.Code, rec.Body.String())
	}

	// application/json with media-type parameters is accepted.
	rec = securityCall(t, h, http.MethodPut, "/api/ui", `{}`, "application/json; charset=utf-8", map[string]string{
		"X-Scimux-CSRF": csrfToken,
	})
	if rec.Code == http.StatusUnsupportedMediaType {
		t.Fatalf("json with charset param rejected: body=%q", rec.Body.String())
	}
	if rec.Code != http.StatusPreconditionRequired {
		// Reached handler: missing If-Match is the expected semantic response.
		t.Fatalf("json with charset: status=%d, want 428; body=%q", rec.Code, rec.Body.String())
	}

	// Malformed content type → 415.
	rec = securityCall(t, h, http.MethodPut, "/api/ui", `{}`, "application/json;;", map[string]string{
		"X-Scimux-CSRF": csrfToken,
	})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("malformed content type: status=%d, want 415; body=%q", rec.Code, rec.Body.String())
	}

	// Form content type → 415 (also covered for guardMutations; pin public path).
	rec = securityCall(t, h, http.MethodPost, "/api/nodes", `title=x`, "application/x-www-form-urlencoded", map[string]string{
		"X-Scimux-CSRF": csrfToken,
	})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form content type: status=%d, want 415; body=%q", rec.Code, rec.Body.String())
	}

	// Multipart accepted only for the attachment-path suffix.
	// Off-path rejection is also covered by TestPublicRouteUploadMultipartGuardAndSuccess;
	// characterize on-path acceptance at the guard via a path that ends in
	// /attachments (unknown node → 404 past the content-type gate).
	rec = securityCall(t, h, http.MethodPost, "/api/nodes/ghost/attachments", "x", "multipart/form-data; boundary=abc", map[string]string{
		"X-Scimux-CSRF": csrfToken,
	})
	if rec.Code == http.StatusUnsupportedMediaType {
		t.Fatalf("multipart on attachments path rejected by content-type gate: body=%q", rec.Body.String())
	}
	// Non-suffix multipart still 415.
	rec = securityCall(t, h, http.MethodPost, "/api/nodes", "x", "multipart/form-data; boundary=abc", map[string]string{
		"X-Scimux-CSRF": csrfToken,
	})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("multipart off attachments: status=%d, want 415; body=%q", rec.Code, rec.Body.String())
	}
}

func TestPublicSecurityUnknownUnsafeRouteOutermost(t *testing.T) {
	// Complements TestNewHandlerMutationGuardIsOutermost with explicit error text.
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	rec := securityCall(t, h, http.MethodPost, "/api/totally-unknown", `{}`, "application/json", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "missing or invalid CSRF token") {
		t.Fatalf("unknown unsafe without CSRF: status=%d body=%q", rec.Code, rec.Body.String())
	}
	rec = securityCall(t, h, http.MethodPost, "/api/totally-unknown", `{}`, "application/json", map[string]string{
		"X-Scimux-CSRF": csrfToken,
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown unsafe with CSRF: status=%d, want 404", rec.Code)
	}
}

// decodeJSON is shared request-body infrastructure; no public route isolates
// "at jsonBodyMax succeeds" without also applying feature-level validation, so
// these assert the helper contract directly.
func TestDecodeJSONBodyBoundAndSingleDecode(t *testing.T) {
	// At the bound: a document of exactly jsonBodyMax bytes decodes.
	pad := jsonBodyMax - len(`{"x":""}`)
	exact := `{"x":"` + strings.Repeat("b", pad) + `"}`
	if len(exact) != jsonBodyMax {
		t.Fatalf("test construction: exact len=%d want %d", len(exact), jsonBodyMax)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(exact))
	rec := httptest.NewRecorder()
	var got map[string]string
	if err := decodeJSON(rec, req, &got); err != nil {
		t.Fatalf("decode at jsonBodyMax: %v", err)
	}
	if len(got["x"]) != pad {
		t.Fatalf("decoded x len=%d want %d", len(got["x"]), pad)
	}

	// Over the bound: MaxBytesReader surfaces a too-large error.
	over := `{"x":"` + strings.Repeat("c", jsonBodyMax) + `"}`
	if len(over) <= jsonBodyMax {
		t.Fatalf("test construction: over len=%d not > %d", len(over), jsonBodyMax)
	}
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(over))
	rec = httptest.NewRecorder()
	var got2 map[string]string
	err := decodeJSON(rec, req, &got2)
	if err == nil {
		t.Fatal("decode over jsonBodyMax: want error")
	}
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		t.Fatalf("over-bound error = %v (%T), want MaxBytesError", err, err)
	}
	if mbe.Limit != jsonBodyMax {
		t.Fatalf("MaxBytesError.Limit = %d, want %d", mbe.Limit, jsonBodyMax)
	}

	// Single-Decode semantics: a trailing second document is ignored, not an error.
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}{"b":2}`))
	rec = httptest.NewRecorder()
	var got3 map[string]any
	if err := decodeJSON(rec, req, &got3); err != nil {
		t.Fatalf("trailing second document must not fail single Decode: %v", err)
	}
	if got3["a"] != float64(1) || got3["b"] != nil {
		t.Fatalf("single Decode result = %#v, want only first object", got3)
	}
}

func TestWriteJSONContentTypeAndNewline(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, map[string]string{"ok": "saved"})
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	body := rec.Body.Bytes()
	if len(body) == 0 || body[len(body)-1] != '\n' {
		t.Fatalf("writeJSON body %q must end with encoder newline", body)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, body)
	}
	if got["ok"] != "saved" {
		t.Fatalf("payload = %#v", got)
	}
}

// ---------- Phase P2: request-boundary Host, Fetch Metadata, framing ----------

func TestRequestBoundaryDNSRebindingRejected(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	// Matching hostile Host + Origin plus a valid CSRF token is the
	// DNS-rebinding case: Origin.Host == r.Host would pass the old check.
	rec := securityCall(t, h, http.MethodPost, "/api/nodes/ghost/exit", `{}`, "application/json", map[string]string{
		"Host":          "evil.example:8787",
		"Origin":        "http://evil.example:8787",
		"X-Scimux-CSRF": csrfToken,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("DNS-rebind POST: status=%d, want 403; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusNotFound {
		t.Fatal("hostile Host reached the routed handler")
	}
	if !strings.Contains(rec.Body.String(), "untrusted host") {
		t.Fatalf("DNS-rebind body = %q, want untrusted host", rec.Body.String())
	}
}

func TestRequestBoundaryHostileHostCoversPublicHandler(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	headers := map[string]string{"Host": "evil.example:8787"}
	for _, path := range []string{"/", "/api/state", "/no-such-route"} {
		rec := securityCall(t, h, http.MethodGet, path, "", "", headers)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s hostile Host: status=%d, want 403; body=%q", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), csrfToken) {
			t.Fatalf("GET %s leaked the CSRF token under a hostile Host", path)
		}
		if bodyLooksLikeIndex(rec.Body.String()) {
			t.Fatalf("GET %s served the token-bearing index under a hostile Host", path)
		}
	}
}

func TestRequestBoundaryLoopbackHostsAccepted(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	// Ports are not host identity: an SSH-forwarded client port must work.
	for _, host := range []string{
		"127.0.0.1:8787",
		"127.0.0.1:9999",
		"127.0.0.2",
		"[::1]:8787",
		"[::1]:443",
		"localhost",
		"localhost:1234",
		"LOCALHOST.",
		"127.0.0.1",
	} {
		rec := securityCall(t, h, http.MethodGet, "/api/state", "", "", map[string]string{"Host": host})
		if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "untrusted host") {
			t.Fatalf("loopback Host %q rejected: status=%d body=%q", host, rec.Code, rec.Body.String())
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("loopback Host %q: status=%d, want 200; body=%q", host, rec.Code, rec.Body.String())
		}
	}
}

func TestRequestBoundaryMalformedHostsRejected(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	for _, host := range []string{
		"",
		"127.0.0.1:",
		"127.0.0.1:abc",
		"user@127.0.0.1:8787",
		"user:pass@localhost",
		"127.0.0.1,evil.example",
		"evil.example,127.0.0.1:8787",
		"[:::1]",
		"[::1",
		"::1]:8787",
		"[gggg::1]:8787",
		"host:1:2",
	} {
		rec := securityCall(t, h, http.MethodGet, "/api/state", "", "", map[string]string{"Host": host})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("malformed Host %q: status=%d, want 403; body=%q", host, rec.Code, rec.Body.String())
		}
	}
}

func TestRequestBoundaryFetchMetadataAPI(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	// Empty search is a cheap documented 200 — no corpus scan.
	for _, path := range []string{"/api/search", "/api/update/check"} {
		for _, site := range []string{"cross-site", "same-site"} {
			rec := securityCall(t, h, http.MethodGet, path, "", "", map[string]string{
				"Sec-Fetch-Site": site,
			})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s Sec-Fetch-Site=%s: status=%d, want 403; body=%q", path, site, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "cross-site request rejected") {
				t.Fatalf("%s Sec-Fetch-Site=%s body = %q", path, site, rec.Body.String())
			}
		}
		for _, site := range []string{"same-origin", "none", ""} {
			headers := map[string]string{}
			if site != "" {
				headers["Sec-Fetch-Site"] = site
			}
			rec := securityCall(t, h, http.MethodGet, "/api/search", "", "", headers)
			if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "cross-site request rejected") {
				t.Fatalf("/api/search Sec-Fetch-Site=%q rejected by fetch metadata: body=%q", site, rec.Body.String())
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("/api/search Sec-Fetch-Site=%q: status=%d, want normal 200; body=%q", site, rec.Code, rec.Body.String())
			}
		}
	}
	// Static/index is not an API path: a cross-site GET of / must not be
	// fetch-metadata-rejected (Host remains the primary boundary).
	rec := securityCall(t, h, http.MethodGet, "/", "", "", map[string]string{
		"Sec-Fetch-Site": "cross-site",
	})
	if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "cross-site request rejected") {
		t.Fatalf("GET / rejected by fetch metadata; Host is the boundary for non-API")
	}
}

func TestRequestBoundaryBrowserDefenseHeaders(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	check := func(name string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if got := rec.Header().Get("Content-Security-Policy"); got != localCSP {
			t.Fatalf("%s CSP = %q, want %q", name, got, localCSP)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Fatalf("%s X-Frame-Options = %q, want DENY", name, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s X-Content-Type-Options = %q, want nosniff", name, got)
		}
		if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Fatalf("%s Referrer-Policy = %q, want no-referrer", name, got)
		}
		if got := rec.Header().Get("Permissions-Policy"); !strings.Contains(got, "camera=()") || !strings.Contains(got, "microphone=()") {
			t.Fatalf("%s Permissions-Policy = %q, want camera/microphone denied", name, got)
		}
	}

	check("GET /", securityCall(t, h, http.MethodGet, "/", "", "", nil))
	check("GET /api/state", securityCall(t, h, http.MethodGet, "/api/state", "", "", nil))
	check("GET unknown", securityCall(t, h, http.MethodGet, "/no-such-route", "", "", nil))
	check("OPTIONS /api/state", securityCall(t, h, http.MethodOptions, "/api/state", "", "", nil))
	check("CSRF 403", securityCall(t, h, http.MethodPost, "/api/nodes", `{}`, "application/json", nil))
	check("hostile Host", securityCall(t, h, http.MethodGet, "/", "", "", map[string]string{
		"Host": "evil.example:8787",
	}))
	check("fetch metadata 403", securityCall(t, h, http.MethodGet, "/api/search", "", "", map[string]string{
		"Sec-Fetch-Site": "cross-site",
	}))
}

func handlerWithPolicy(t *testing.T, listenAddr string, extra []string) http.Handler {
	t.Helper()
	p, err := newRequestPolicy(listenAddr, extra)
	if err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, &fakeTmux{})
	a.requestPolicy = p
	return newTestHandler(t, a)
}

func TestRequestBoundaryLocaldomainNotImplicitlyTrusted(t *testing.T) {
	// localhost.localdomain is a real DNS name, not the documented localhost
	// identity. Loopback and wildcard listeners must reject it unless the
	// operator opted in with -trusted-host.
	for _, listen := range []string{"127.0.0.1:8787", "0.0.0.0:8787"} {
		h := handlerWithPolicy(t, listen, nil)
		for _, host := range []string{"localhost.localdomain", "localhost.localdomain:8787", "LOCALHOST.LOCALDOMAIN."} {
			rec := securityCall(t, h, http.MethodGet, "/api/state", "", "", map[string]string{"Host": host})
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "untrusted host") {
				t.Fatalf("%s listener Host %q: status=%d body=%q, want 403 untrusted host", listen, host, rec.Code, rec.Body.String())
			}
		}
	}
	optIn := handlerWithPolicy(t, "127.0.0.1:8787", []string{"localhost.localdomain"})
	rec := securityCall(t, optIn, http.MethodGet, "/api/state", "", "", map[string]string{
		"Host": "localhost.localdomain:9999",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("-trusted-host localhost.localdomain: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestRequestBoundaryWildcardDoesNotTrustDNS(t *testing.T) {
	h := handlerWithPolicy(t, "0.0.0.0:8787", nil)
	// IP-literal access is the documented wildcard policy; DNS names are not.
	for _, host := range []string{"192.168.1.5:8787", "10.0.0.9", "[::1]:80", "127.0.0.1:9", "localhost"} {
		rec := securityCall(t, h, http.MethodGet, "/api/state", "", "", map[string]string{"Host": host})
		if rec.Code != http.StatusOK {
			t.Fatalf("wildcard IP/localhost %q: status=%d body=%q", host, rec.Code, rec.Body.String())
		}
	}
	rec := securityCall(t, h, http.MethodGet, "/", "", "", map[string]string{"Host": "evil.example:8787"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "untrusted host") {
		t.Fatalf("wildcard DNS Host: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestRequestBoundaryConcreteHostPolicy(t *testing.T) {
	h := handlerWithPolicy(t, "192.168.1.5:8787", []string{"box.lan"})
	// Configured identity and explicit extra.
	for _, host := range []string{"192.168.1.5:8787", "192.168.1.5:9999", "box.lan", "BOX.LAN."} {
		rec := securityCall(t, h, http.MethodGet, "/api/state", "", "", map[string]string{"Host": host})
		if rec.Code != http.StatusOK {
			t.Fatalf("concrete allowed %q: status=%d body=%q", host, rec.Code, rec.Body.String())
		}
	}
	// Other IPs and unrelated DNS names are rejected — including loopback,
	// which is not the configured identity.
	for _, host := range []string{"10.0.0.9:8787", "127.0.0.1:8787", "evil.example", "other.lan"} {
		rec := securityCall(t, h, http.MethodGet, "/api/state", "", "", map[string]string{"Host": host})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("concrete rejected %q: status=%d body=%q", host, rec.Code, rec.Body.String())
		}
	}
}

func TestRequestBoundaryConcreteNameDoesNotTrustArbitraryIP(t *testing.T) {
	h := handlerWithPolicy(t, "box.lan:8787", nil)
	rec := securityCall(t, h, http.MethodGet, "/api/state", "", "", map[string]string{"Host": "box.lan:80"})
	if rec.Code != http.StatusOK {
		t.Fatalf("configured name: status=%d body=%q", rec.Code, rec.Body.String())
	}
	rec = securityCall(t, h, http.MethodGet, "/api/state", "", "", map[string]string{"Host": "192.168.1.5"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unnamed IP on named listener: status=%d, want 403; body=%q", rec.Code, rec.Body.String())
	}
}

func TestCanonicalizeHost(t *testing.T) {
	ok := func(in, want string) {
		t.Helper()
		got, err := canonicalizeHost(in)
		if err != nil {
			t.Fatalf("canonicalizeHost(%q) = %v, want %q", in, err, want)
		}
		if got != want {
			t.Fatalf("canonicalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
	bad := func(in string) {
		t.Helper()
		if _, err := canonicalizeHost(in); err == nil {
			t.Fatalf("canonicalizeHost(%q) succeeded, want error", in)
		}
	}
	ok("127.0.0.1:8787", "127.0.0.1")
	ok("127.0.0.1", "127.0.0.1")
	ok("[::1]:8787", "::1")
	ok("[::1]", "::1")
	ok("[0:0:0:0:0:0:0:1]", "::1")
	ok("[::ffff:127.0.0.1]:80", "127.0.0.1")
	ok("LOCALHOST.", "localhost")
	ok("Box.LAN:9", "box.lan")
	bad("")
	bad("user@host")
	bad("a,b")
	bad("127.0.0.1:")
	bad("127.0.0.1:abc")
	bad("[:::1]")
	bad("[::1")
	bad("::1")
	bad("host:1:2")
}
