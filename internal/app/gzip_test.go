package app

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
)

// gzipGET issues a GET through the full NewHandler stack (mutation guard + gzip).
func gzipGET(t *testing.T, h http.Handler, path string, hdrs map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787"+path, nil)
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func gunzipBody(t *testing.T, b []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v (body len %d)", err, len(b))
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("gunzip read: %v", err)
	}
	return out
}

// TestGzip304StateNoContentEncoding is the trap that matters: a conditional
// GET with a matching ETag and Accept-Encoding: gzip must stay a true 304 —
// zero body, no Content-Encoding. A naive middleware that opens a gzip.Writer
// for every response emits a ~20-byte header/footer and sets Content-Encoding
// on the empty 304, breaking every idle poll.
func TestGzip304StateNoContentEncoding(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	base := gzipGET(t, h, "/api/state", nil)
	if base.Code != http.StatusOK {
		t.Fatalf("baseline status = %d, want 200; body=%q", base.Code, base.Body.String())
	}
	etag := base.Header().Get("ETag")
	if etag == "" {
		t.Fatal("baseline missing ETag")
	}

	rec := gzipGET(t, h, "/api/state", map[string]string{
		"If-None-Match":   etag,
		"Accept-Encoding": "gzip",
	})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 body len = %d, want 0; body=%q", rec.Body.Len(), rec.Body.String())
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want empty on 304", ce)
	}
}

// TestGzip304ChatNoContentEncoding is the same trap on the chat poll path
// (P2 ETag/304). Matching If-None-Match + gzip must not invent a body.
func TestGzip304ChatNoContentEncoding(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const id = "gz-chat"
	n := &Node{ID: id, Title: id, Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes = append(a.nodes, n)
	a.byID[id] = n
	a.live[id] = "quiet"
	w := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, id+".jsonl")}
	for _, ev := range []sessionlog.Event{
		sessionlog.NewMeta(id, "claude", "", "", a.home),
		{T: "user", Text: "hello", Time: "2026-07-14T01:00:00Z"},
		{T: "assistant", Text: "hi", Time: "2026-07-14T01:01:00Z"},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	h := newTestHandler(t, a)

	base := gzipGET(t, h, "/api/nodes/"+id+"/chat", nil)
	if base.Code != http.StatusOK {
		t.Fatalf("baseline status = %d, want 200; body=%q", base.Code, base.Body.String())
	}
	etag := base.Header().Get("ETag")
	if etag == "" {
		t.Fatal("baseline missing ETag")
	}

	rec := gzipGET(t, h, "/api/nodes/"+id+"/chat", map[string]string{
		"If-None-Match":   etag,
		"Accept-Encoding": "gzip",
	})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 body len = %d, want 0; body=%q", rec.Body.Len(), rec.Body.String())
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want empty on 304", ce)
	}
}

func TestGzipStateRoundTrip(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	plain := gzipGET(t, h, "/api/state", nil)
	if plain.Code != http.StatusOK {
		t.Fatalf("plain status = %d", plain.Code)
	}
	gz := gzipGET(t, h, "/api/state", map[string]string{"Accept-Encoding": "gzip"})
	if gz.Code != http.StatusOK {
		t.Fatalf("gzip status = %d", gz.Code)
	}
	if ce := gz.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", ce)
	}
	got := gunzipBody(t, gz.Body.Bytes())
	if !bytes.Equal(got, plain.Body.Bytes()) {
		t.Fatalf("decompressed body differs from plain\n plain=%s\n  gunzip=%s", plain.Body.Bytes(), got)
	}
}

func TestGzipStaticJSRoundTrip(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	const path = "/js/app.js"
	plain := gzipGET(t, h, path, nil)
	if plain.Code != http.StatusOK {
		t.Fatalf("plain status = %d", plain.Code)
	}
	gz := gzipGET(t, h, path, map[string]string{"Accept-Encoding": "gzip"})
	if gz.Code != http.StatusOK {
		t.Fatalf("gzip status = %d", gz.Code)
	}
	if ce := gz.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", ce)
	}
	got := gunzipBody(t, gz.Body.Bytes())
	if !bytes.Equal(got, plain.Body.Bytes()) {
		t.Fatalf("decompressed JS differs from plain (plain %d bytes, gunzip %d)", plain.Body.Len(), len(got))
	}
}

func TestGzipNoAcceptEncodingUnchanged(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	// Two plain GETs must be byte-identical and free of Content-Encoding.
	a := gzipGET(t, h, "/api/state", nil)
	b := gzipGET(t, h, "/api/state", nil)
	if a.Code != http.StatusOK || b.Code != http.StatusOK {
		t.Fatalf("status a=%d b=%d", a.Code, b.Code)
	}
	if ce := a.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q without Accept-Encoding", ce)
	}
	if !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
		t.Fatal("plain responses not byte-identical")
	}
	// Body must look like raw JSON, not a gzip stream.
	if !strings.HasPrefix(a.Body.String(), "{") {
		t.Fatalf("plain body does not look like JSON: %q", a.Body.String()[:min(40, a.Body.Len())])
	}
}
