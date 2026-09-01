package app

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// flushRecorder is an http.ResponseWriter that also implements http.Flusher.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushRecorder) Flush() {
	f.flushes++
	f.ResponseRecorder.Flush()
}

// errWriter fails Writes after headers are committed.
type errWriter struct {
	header http.Header
	code   int
	err    error
}

func (w *errWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *errWriter) WriteHeader(code int) { w.code = code }
func (w *errWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return 0, w.err
}

// TestGzipResponseWriterInterfaces covers Unwrap and Flush delegation.
func TestGzipResponseWriterInterfaces(t *testing.T) {
	t.Run("Unwrap returns exact underlying writer", func(t *testing.T) {
		rec := httptest.NewRecorder()
		gw := &gzipResponseWriter{ResponseWriter: rec}
		if got := gw.Unwrap(); got != rec {
			t.Fatalf("Unwrap() = %T %p, want exact recorder %p", got, got, rec)
		}
	})

	t.Run("ResponseController reaches underlying Flusher via Unwrap", func(t *testing.T) {
		rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
		gw := &gzipResponseWriter{ResponseWriter: rec}
		rc := http.NewResponseController(gw)
		if err := rc.Flush(); err != nil {
			t.Fatalf("ResponseController.Flush: %v", err)
		}
		if rec.flushes != 1 {
			t.Fatalf("underlying flushes = %d, want 1", rec.flushes)
		}
		if gw.gz != nil {
			t.Fatal("Flush before body must not create a gzip writer")
		}
		if ce := rec.Header().Get("Content-Encoding"); ce != "" {
			t.Fatalf("Content-Encoding = %q before body", ce)
		}
	})

	t.Run("Flush before body delegates without creating gzip", func(t *testing.T) {
		rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
		gw := &gzipResponseWriter{ResponseWriter: rec}
		gw.Flush()
		if rec.flushes != 1 {
			t.Fatalf("flushes = %d, want 1", rec.flushes)
		}
		if gw.gz != nil {
			t.Fatal("Flush created gzip writer")
		}
	})

	t.Run("Flush after compressed write flushes gzip and underlying", func(t *testing.T) {
		rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
		gw := &gzipResponseWriter{ResponseWriter: rec}
		gw.Header().Set("Content-Type", "text/plain")
		if _, err := gw.Write([]byte("hello-flush")); err != nil {
			t.Fatal(err)
		}
		if gw.gz == nil {
			t.Fatal("expected gzip writer after compressible write")
		}
		before := rec.Body.Len()
		gw.Flush()
		if rec.flushes != 1 {
			t.Fatalf("underlying flushes = %d, want 1", rec.flushes)
		}
		if rec.Body.Len() <= before {
			t.Fatal("gzip Flush did not push bytes to the underlying writer")
		}
		gw.close()
	})

	t.Run("Flush without Flusher does not panic", func(t *testing.T) {
		rec := httptest.NewRecorder() // ResponseRecorder implements Flusher;
		// wrap in a non-flusher adapter.
		type bare struct{ http.ResponseWriter }
		gw := &gzipResponseWriter{ResponseWriter: bare{rec}}
		gw.Flush() // must not panic
	})
}

// TestGzipResponseWriterDirectPaths covers skip/passthrough and close seams
// not exercised by the full-handler gzip tests.
func TestGzipResponseWriterDirectPaths(t *testing.T) {
	t.Run("non-compressible content passes through without Content-Encoding", func(t *testing.T) {
		rec := httptest.NewRecorder()
		gw := &gzipResponseWriter{ResponseWriter: rec}
		gw.Header().Set("Content-Type", "image/png")
		payload := []byte{0x89, 0x50, 0x4e, 0x47}
		if _, err := gw.Write(payload); err != nil {
			t.Fatal(err)
		}
		gw.close()
		if ce := rec.Header().Get("Content-Encoding"); ce != "" {
			t.Fatalf("Content-Encoding = %q, want empty", ce)
		}
		if !bytes.Equal(rec.Body.Bytes(), payload) {
			t.Fatalf("body = %v, want passthrough", rec.Body.Bytes())
		}
	})

	t.Run("pre-existing Content-Encoding is not double-compressed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		gw := &gzipResponseWriter{ResponseWriter: rec}
		gw.Header().Set("Content-Type", "text/plain")
		gw.Header().Set("Content-Encoding", "br")
		payload := []byte("already-encoded")
		if _, err := gw.Write(payload); err != nil {
			t.Fatal(err)
		}
		gw.close()
		if ce := rec.Header().Get("Content-Encoding"); ce != "br" {
			t.Fatalf("Content-Encoding = %q, want br", ce)
		}
		if !bytes.Equal(rec.Body.Bytes(), payload) {
			t.Fatal("body was recompressed")
		}
		if gw.gz != nil {
			t.Fatal("gzip writer created despite existing Content-Encoding")
		}
	})

	t.Run("passthrough Write error is returned", func(t *testing.T) {
		want := errors.New("write failed")
		gw := &gzipResponseWriter{ResponseWriter: &errWriter{err: want}}
		gw.Header().Set("Content-Type", "image/png") // force passthrough
		_, err := gw.Write([]byte("x"))
		if !errors.Is(err, want) {
			t.Fatalf("Write err = %v, want %v", err, want)
		}
	})

	t.Run("header-only close commits status without gzip body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		gw := &gzipResponseWriter{ResponseWriter: rec}
		gw.WriteHeader(http.StatusAccepted)
		gw.close()
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("body len = %d, want 0", rec.Body.Len())
		}
		if ce := rec.Header().Get("Content-Encoding"); ce != "" {
			t.Fatalf("Content-Encoding = %q on header-only", ce)
		}
		if gw.gz != nil {
			t.Fatal("gzip writer created on header-only close")
		}
	})

	t.Run("repeated WriteHeader preserves first status", func(t *testing.T) {
		rec := httptest.NewRecorder()
		gw := &gzipResponseWriter{ResponseWriter: rec}
		gw.WriteHeader(http.StatusCreated)
		gw.WriteHeader(http.StatusBadRequest)
		gw.close()
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (first WriteHeader)", rec.Code)
		}
	})

	t.Run("default status on header-only close is 200", func(t *testing.T) {
		rec := httptest.NewRecorder()
		gw := &gzipResponseWriter{ResponseWriter: rec}
		gw.close()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("compressed write round-trips through close", func(t *testing.T) {
		rec := httptest.NewRecorder()
		gw := &gzipResponseWriter{ResponseWriter: rec}
		gw.Header().Set("Content-Type", "text/plain")
		payload := []byte("compressible-direct")
		if _, err := gw.Write(payload); err != nil {
			t.Fatal(err)
		}
		gw.close()
		if ce := rec.Header().Get("Content-Encoding"); ce != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", ce)
		}
		r, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("gunzip = %q, want %q", got, payload)
		}
	})
}
