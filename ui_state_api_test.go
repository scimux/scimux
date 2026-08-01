package main

// Packet 4D public-route coverage for the opaque UI JSON read/revision/write
// bindings through NewHandler. Complements — does not replace — the detailed
// direct-handler matrix in TestHandleUIRoundTrip (agents_test.go).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// uiStateAPIHandler builds the production router for public-route assertions.
func uiStateAPIHandler(t *testing.T, a *app) http.Handler {
	t.Helper()
	h, err := NewHandler(a, webFS)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// uiRoute issues a request through the production handler with optional CSRF
// and revision headers. Empty body leaves Content-Type unset (GET/HEAD).
func uiRoute(t *testing.T, h http.Handler, method, path, body string, csrf bool, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf {
		req.Header.Set("X-Scimux-CSRF", csrfToken)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func uiGet(t *testing.T, h http.Handler, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	hdrs := map[string]string{}
	if ifNoneMatch != "" {
		hdrs["If-None-Match"] = ifNoneMatch
	}
	return uiRoute(t, h, http.MethodGet, "/api/ui", "", false, hdrs)
}

func uiPut(t *testing.T, h http.Handler, body, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	hdrs := map[string]string{}
	if ifMatch != "" {
		hdrs["If-Match"] = ifMatch
	}
	return uiRoute(t, h, http.MethodPut, "/api/ui", body, true, hdrs)
}

// ---------- bootstrap, ETag, exact-byte preservation ----------

func TestPublicRouteUIBootstrapAndETag(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	// Ensure the store path is empty (no file) — bootstrap serves exact {}.
	if _, err := os.Stat(a.uiPath); !os.IsNotExist(err) {
		t.Fatalf("precondition: uiPath should be missing, stat err=%v", err)
	}
	h := uiStateAPIHandler(t, a)

	rec := uiGet(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap GET status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	// Exact {} bytes — not a re-encoded object, not with trailing newline.
	if !bytes.Equal(rec.Body.Bytes(), []byte("{}")) {
		t.Fatalf("bootstrap body = %q, want exact {}", rec.Body.Bytes())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("bootstrap GET carries no ETag")
	}
	// Stable ETag for the bootstrap empty document.
	rec2 := uiGet(t, h, "")
	if rec2.Header().Get("ETag") != etag {
		t.Errorf("bootstrap ETag not stable: %q vs %q", etag, rec2.Header().Get("ETag"))
	}
	// Matching If-None-Match → 304 empty body.
	rec3 := uiGet(t, h, etag)
	if rec3.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match status = %d, want 304", rec3.Code)
	}
	if rec3.Body.Len() != 0 {
		t.Errorf("304 body must be empty, got %q", rec3.Body.String())
	}
}

func TestPublicRouteUIExactBytesAndStableETag(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	// Deliberately non-normalized JSON (spaces) — server must not re-encode.
	stored := []byte(`{ "notes" : [ "a" ], "x":1 }`)
	if err := os.WriteFile(a.uiPath, stored, 0o600); err != nil {
		t.Fatal(err)
	}
	h := uiStateAPIHandler(t, a)

	rec := uiGet(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body %q", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), stored) {
		t.Fatalf("GET body not byte-for-byte: got %q want %q", rec.Body.Bytes(), stored)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	// Unchanged stored bytes → stable ETag across GETs.
	rec2 := uiGet(t, h, "")
	if rec2.Header().Get("ETag") != etag {
		t.Errorf("ETag not stable for unchanged bytes: %q vs %q", etag, rec2.Header().Get("ETag"))
	}
	rec3 := uiGet(t, h, etag)
	if rec3.Code != http.StatusNotModified || rec3.Body.Len() != 0 {
		t.Fatalf("conditional GET = %d body %q, want 304 empty", rec3.Code, rec3.Body.String())
	}
}

// ---------- storage failures (portable path shapes) ----------

func TestPublicRouteUIMalformedStoredJSON(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	malformed := []byte(`{not-json`)
	if err := os.WriteFile(a.uiPath, malformed, 0o600); err != nil {
		t.Fatal(err)
	}
	h := uiStateAPIHandler(t, a)

	rec := uiGet(t, h, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("malformed GET status = %d, want 500; body=%q", rec.Code, rec.Body.String())
	}
	// Must not be treated as bootstrap {}.
	if bytes.Equal(bytes.TrimSpace(rec.Body.Bytes()), []byte("{}")) {
		t.Fatal("malformed stored JSON must not be served as {}")
	}
	// PUT (including wildcard) must not overwrite unreadable/malformed state.
	for _, match := range []string{"*", `"deadbeef"`} {
		put := uiPut(t, h, `{"notes":[]}`, match)
		if put.Code != http.StatusInternalServerError {
			t.Fatalf("PUT If-Match %s on malformed: status = %d, want 500", match, put.Code)
		}
	}
	// Stored bytes unchanged.
	got, err := os.ReadFile(a.uiPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, malformed) {
		t.Fatalf("malformed file was overwritten: %q", got)
	}
}

func TestPublicRouteUIUnreadablePathShape(t *testing.T) {
	// Portable failure: uiPath is a directory, so ReadFile fails with an error
	// that is not os.IsNotExist (no chmod denial).
	a := newTestApp(t, &fakeTmux{})
	if err := os.Remove(a.uiPath); err != nil && !os.IsNotExist(err) {
		// may not exist yet
	}
	if err := os.Mkdir(a.uiPath, 0o700); err != nil {
		t.Fatal(err)
	}
	h := uiStateAPIHandler(t, a)

	rec := uiGet(t, h, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("directory-as-uiPath GET status = %d, want 500; body=%q", rec.Code, rec.Body.String())
	}
	put := uiPut(t, h, `{}`, "*")
	if put.Code != http.StatusInternalServerError {
		t.Fatalf("directory-as-uiPath PUT status = %d, want 500", put.Code)
	}
	// Path remains a directory — never replaced by a file via a failed PUT.
	st, err := os.Stat(a.uiPath)
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsDir() {
		t.Fatal("uiPath must remain a directory after failed writes")
	}
}

// ---------- PUT validation (before touching stored state) ----------

func TestPublicRouteUIPutValidationBeforeTouch(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	prior := []byte(`{"keep":true}`)
	if err := os.WriteFile(a.uiPath, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	h := uiStateAPIHandler(t, a)

	// No If-Match → 428, stored state untouched.
	rec := uiPut(t, h, `{"notes":[]}`, "")
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match status = %d, want 428; body=%q", rec.Code, rec.Body.String())
	}

	// Invalid JSON → 400 before lock/file touch.
	rec = uiPut(t, h, `{broken`, "*")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}

	// Oversized body → 413 before lock/file touch.
	// Body length exceeds uiStateMax by construction (prefix + payload).
	huge := `{"notes":["` + strings.Repeat("x", uiStateMax) + `"]}`
	if len(huge) <= uiStateMax {
		t.Fatalf("test construction error: huge body len %d not > uiStateMax", len(huge))
	}
	rec = uiPut(t, h, huge, "*")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, want 413; body=%q", rec.Code, rec.Body.String())
	}

	got, err := os.ReadFile(a.uiPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, prior) {
		t.Fatalf("validation failures must not touch stored state; got %q", got)
	}
}

// ---------- successful writes, permissions, concurrency ----------

func TestPublicRouteUIWildcardBootstrapAndMode(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if _, err := os.Stat(a.uiPath); !os.IsNotExist(err) {
		t.Fatalf("precondition: uiPath should be missing")
	}
	h := uiStateAPIHandler(t, a)

	body := `{"groups":[{"id":"g1"}],"notes":[]}`
	rec := uiPut(t, h, body, "*")
	if rec.Code != http.StatusOK {
		t.Fatalf("wildcard bootstrap status = %d body %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	newETag := rec.Header().Get("ETag")
	if newETag == "" {
		t.Fatal("successful PUT must return ETag")
	}
	var ok map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatal(err)
	}
	if ok["ok"] != "saved" {
		t.Errorf("response = %#v, want ok=saved", ok)
	}

	// First file creation is owner-only 0600.
	st, err := os.Stat(a.uiPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("ui.json mode = %04o, want 0600", perm)
	}
	// Stored bytes are exact (opaque, not re-encoded).
	got, err := os.ReadFile(a.uiPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(body)) {
		t.Fatalf("stored = %q, want %q", got, body)
	}
	// GET reports the same ETag.
	g := uiGet(t, h, "")
	if g.Header().Get("ETag") != newETag {
		t.Errorf("GET ETag %q != PUT ETag %q", g.Header().Get("ETag"), newETag)
	}
}

func TestPublicRouteUIMatchingUpdateAndStaleConflict(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	h := uiStateAPIHandler(t, a)

	// Bootstrap.
	if rec := uiPut(t, h, `{"v":1}`, "*"); rec.Code != http.StatusOK {
		t.Fatalf("bootstrap: %d %s", rec.Code, rec.Body.String())
	}
	g := uiGet(t, h, "")
	etag := g.Header().Get("ETag")
	prior, err := os.ReadFile(a.uiPath)
	if err != nil {
		t.Fatal(err)
	}

	// Matching ETag update succeeds and returns a new ETag.
	next := `{"v":2,"notes":["x"]}`
	rec := uiPut(t, h, next, etag)
	if rec.Code != http.StatusOK {
		t.Fatalf("matching update: %d %s", rec.Code, rec.Body.String())
	}
	newETag := rec.Header().Get("ETag")
	if newETag == "" || newETag == etag {
		t.Fatalf("new ETag = %q (old %q), want different non-empty", newETag, etag)
	}
	got, err := os.ReadFile(a.uiPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(next)) {
		t.Fatalf("after update stored = %q, want %q", got, next)
	}

	// Stale If-Match → 409, stored bytes unchanged at the new revision.
	staleBody := `{"v":"stale"}`
	rec = uiPut(t, h, staleBody, etag) // old revision
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale PUT status = %d, want 409; body=%q", rec.Code, rec.Body.String())
	}
	got, err = os.ReadFile(a.uiPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(next)) {
		t.Fatalf("stale PUT overwrote storage: %q", got)
	}
	// Sanity: prior bootstrap content is gone (we did advance once).
	if bytes.Equal(got, prior) {
		t.Fatal("expected storage to have advanced past bootstrap before stale conflict")
	}
}

func TestPublicRouteUIConcurrentSameRevision(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	h := uiStateAPIHandler(t, a)
	if rec := uiPut(t, h, `{"base":true}`, "*"); rec.Code != http.StatusOK {
		t.Fatalf("bootstrap: %d", rec.Code)
	}
	etag := uiGet(t, h, "").Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing base ETag")
	}

	const n = 2
	var (
		wg       sync.WaitGroup
		success  atomic.Int32
		conflict atomic.Int32
		other    atomic.Int32
	)
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			// Distinct valid JSON per writer so a mixed write would be visible.
			body := `{"writer":` + strings.Repeat("1", i+1) + `}`
			rec := uiPut(t, h, body, etag)
			switch rec.Code {
			case http.StatusOK:
				success.Add(1)
			case http.StatusConflict:
				conflict.Add(1)
			default:
				other.Add(1)
				t.Errorf("concurrent PUT status = %d body %q", rec.Code, rec.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()

	if success.Load() != 1 || conflict.Load() != 1 || other.Load() != 0 {
		t.Fatalf("concurrent same-revision: success=%d conflict=%d other=%d, want 1/1/0",
			success.Load(), conflict.Load(), other.Load())
	}
	// Winner's bytes are stored; loser's body is not mixed in.
	got, err := os.ReadFile(a.uiPath)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(got) {
		t.Fatalf("stored document not valid JSON: %q", got)
	}
	if !bytes.Contains(got, []byte(`"writer"`)) {
		t.Fatalf("stored document missing winner payload: %q", got)
	}
}

// ---------- atomic-write failure leaves prior valid document ----------

func TestPublicRouteUITempWriteFailureLeavesPrior(t *testing.T) {
	// Portable temp-write failure: sibling path ui.json.tmp is a directory, so
	// WriteFile(tmp) fails while ReadFile(ui.json) still returns the prior
	// valid document. No chmod denial required.
	a := newTestApp(t, &fakeTmux{})
	prior := []byte(`{"prior":true,"notes":[]}`)
	if err := os.WriteFile(a.uiPath, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := a.uiPath + ".tmp"
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	h := uiStateAPIHandler(t, a)

	// GET still serves the prior document.
	g := uiGet(t, h, "")
	if g.Code != http.StatusOK || !bytes.Equal(g.Body.Bytes(), prior) {
		t.Fatalf("GET prior: status=%d body=%q", g.Code, g.Body.Bytes())
	}
	etag := g.Header().Get("ETag")

	rec := uiPut(t, h, `{"after":true}`, etag)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("temp-write failure status = %d, want 500; body=%q", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(a.uiPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, prior) {
		t.Fatalf("temp-write failure overwrote prior document: %q", got)
	}
	// Wildcard must also fail without clobbering.
	rec = uiPut(t, h, `{"after":true}`, "*")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("wildcard temp-write failure status = %d, want 500", rec.Code)
	}
	got, err = os.ReadFile(a.uiPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, prior) {
		t.Fatalf("wildcard temp-write failure overwrote prior: %q", got)
	}
}
