package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// s3Boundaries builds the two S3 boundaries over one owned mux for a fake
// proven peer. S3 has no network and no codec: the tunnel handler is driven
// directly with a fake identity.
func s3Boundaries(t *testing.T) (local, tunnel http.Handler) {
	t.Helper()
	a := newTestApp(t, &fakeTmux{})
	local, tunnel, err := newBoundaries(a, webFS, s3Peer())
	if err != nil {
		t.Fatalf("newBoundaries: %v", err)
	}
	if local == nil || tunnel == nil {
		t.Fatalf("newBoundaries returned a nil boundary: local=%v tunnel=%v", local, tunnel)
	}
	return local, tunnel
}

func s3Peer() tunnelPeer {
	return tunnelPeer{DeviceID: "device-1", RID: strings.Repeat("ab", 32)}
}

// s3ChannelRequest is a request as it arrives over the data channel: no Host,
// no Origin, no Referer, no Sec-Fetch-Site, no CSRF token. Nothing may be
// synthesised to make it acceptable (FR-17).
func s3ChannelRequest(method, path, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, "http://tunnel.invalid"+path, nil)
	} else {
		req = httptest.NewRequest(method, "http://tunnel.invalid"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Host = ""
	req.Header.Del("Origin")
	req.Header.Del("Referer")
	req.Header.Del("Sec-Fetch-Site")
	req.Header.Del("X-Scimux-CSRF")
	return req
}

func s3Serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// s3UnsafeBody is a minimal acceptable JSON body for an unsafe route, so a
// rejection is attributable to a boundary guard rather than to a malformed
// payload.
func s3UnsafeBody(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return ""
	}
	return "{}"
}

// s3Rejected reports whether the recorder holds a boundary rejection, i.e. a
// 403/415 produced before the mux. Handler-level statuses (404 for a missing
// node, 400 for a bad payload) are not boundary rejections and must not be
// read as one.
func s3Rejected(rec *httptest.ResponseRecorder) bool {
	return rec.Code == http.StatusForbidden || rec.Code == http.StatusUnsupportedMediaType
}
