// gzip.go — response compression middleware for the HTTP stack.
//
// Ownership:
//   - NewHandler wraps the mux as
//     withGzip(withRequestBoundary(policy, guardMutations(mux))). Compression
//     is outermost so every completed response can opt in; the request
//     boundary and mutation guard still run first on the request path.
//   - Only responses whose Content-Type is known-compressible are gzipped.
//     Already-compressed binary types (PNG/JPEG/PDF/zip/…) and anything that
//     already carries Content-Encoding are left alone — handleAsset and
//     handleAttachment serve user files that are often pre-compressed.
//   - 304/204 never initialise a compressor: a gzip.Writer opened and closed
//     with no payload still emits a ~20-byte header/footer, which would break
//     ETag short-circuits on /api/state, /api/ui, and /api/nodes/{id}/chat.
//   - Content-Length from the inner handler is dropped when compressing; the
//     compressed size is not known up front.
//   - http.Flusher is preserved when the underlying writer supports it.
package app

import (
	"compress/gzip"
	"mime"
	"net/http"
	"strings"
)

// withGzip compresses eligible responses when the client offers gzip.
func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

func acceptsGzip(ae string) bool {
	for _, part := range strings.Split(ae, ",") {
		coding := strings.TrimSpace(part)
		if i := strings.IndexByte(coding, ';'); i >= 0 {
			coding = strings.TrimSpace(coding[:i])
		}
		if strings.EqualFold(coding, "gzip") {
			return true
		}
	}
	return false
}

// compressibleType reports whether a Content-Type is worth gzipping. Binary
// and already-compressed types return false so we never double-compress PNG,
// JPEG, PDF, zip, or similar payloads served by handleAsset/handleAttachment
// or the /assets/ tree.
func compressibleType(ct string) bool {
	if ct == "" {
		// Handlers that never set Content-Type (rare) still get a chance;
		// empty bodies never open the compressor anyway.
		return true
	}
	media, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(media, "text/"):
		return true
	case media == "application/json",
		media == "application/javascript",
		media == "application/xml",
		media == "application/xhtml+xml",
		media == "image/svg+xml":
		return true
	case strings.HasPrefix(media, "image/"),
		strings.HasPrefix(media, "audio/"),
		strings.HasPrefix(media, "video/"):
		return false
	case media == "application/zip",
		media == "application/gzip",
		media == "application/x-gzip",
		media == "application/x-tar",
		media == "application/pdf",
		media == "application/octet-stream",
		media == "application/wasm":
		return false
	case strings.HasPrefix(media, "application/") &&
		(strings.Contains(media, "json") ||
			strings.Contains(media, "xml") ||
			strings.Contains(media, "javascript") ||
			strings.Contains(media, "text")):
		return true
	default:
		return false
	}
}

// gzipResponseWriter lazily wraps the body in gzip. The compressor is not
// created until the first non-empty Write of a compressible, non-empty-body
// status — so 304/204 pass through untouched.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	status      int
	wroteHeader bool
	skip        bool // never compress this response
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	// Empty-body statuses must never open a compressor. Write the header now
	// so a subsequent accidental Write cannot retrofit Content-Encoding.
	if code == http.StatusNoContent || code == http.StatusNotModified {
		w.skip = true
		w.ResponseWriter.WriteHeader(code)
		w.wroteHeader = true
		return
	}
	// Delay the real WriteHeader until the first Write so Content-Encoding
	// and Content-Length can still be adjusted.
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if err := w.ensureHeader(len(p) > 0); err != nil {
		return 0, err
	}
	if w.gz != nil {
		return w.gz.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

// ensureHeader commits the status line and decides whether to compress.
// startCompress is true when this call is about to write a non-empty body.
func (w *gzipResponseWriter) ensureHeader(startCompress bool) error {
	if w.wroteHeader {
		return nil
	}
	code := w.status
	if code == 0 {
		code = http.StatusOK
	}
	if !w.skip && startCompress && w.Header().Get("Content-Encoding") == "" &&
		compressibleType(w.Header().Get("Content-Type")) {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Encoding", "gzip")
		// Clients that cache must key on Accept-Encoding so a gzip body is
		// never served to a client that did not ask for it.
		w.Header().Add("Vary", "Accept-Encoding")
		w.gz = gzip.NewWriter(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(code)
	w.wroteHeader = true
	return nil
}

func (w *gzipResponseWriter) close() {
	// Handler wrote headers only (e.g. 200 with no body) — commit them.
	if !w.wroteHeader {
		code := w.status
		if code == 0 {
			code = http.StatusOK
		}
		w.ResponseWriter.WriteHeader(code)
		w.wroteHeader = true
	}
	if w.gz != nil {
		_ = w.gz.Close()
		w.gz = nil
	}
}

// Flush preserves http.Flusher when the underlying writer supports it. No
// handler currently relies on it; the seam is kept so a future streaming
// response is not silently broken by compression.
func (w *gzipResponseWriter) Flush() {
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *gzipResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
