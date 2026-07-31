package main

import (
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
