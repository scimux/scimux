package codec

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func tryRoundTrip(t *testing.T, req *Request) error {
	t.Helper()
	client := startPair(t, HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
		t.Errorf("handler ran for a request that must be rejected (id=%s method=%s path=%q)", req.ID, req.Method, req.Path)
		if req.Body != nil {
			_, _ = io.Copy(io.Discard, req.Body)
			_ = req.Body.Close()
		}
		return &Response{ID: req.ID, Status: 200, Body: bodyOf("")}, nil
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resp, err := client.RoundTrip(ctx, req)
	if err == nil && resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return err
}

func validGET(id, path string) *Request {
	return &Request{
		ID:     id,
		Method: http.MethodGet,
		Path:   path,
		Body:   bodyOf(""),
	}
}

func tryRoundTripResponse(t *testing.T, req *Request, hdr http.Header) error {
	t.Helper()
	client := startPair(t, HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
		if req.Body != nil {
			_, _ = io.Copy(io.Discard, req.Body)
			_ = req.Body.Close()
		}
		return &Response{
			ID:      req.ID,
			Status:  http.StatusOK,
			Headers: hdr,
			Body:    bodyOf(""),
		}, nil
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resp, err := client.RoundTrip(ctx, req)
	if err == nil && resp != nil && resp.Body != nil {
		_, copyErr := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if copyErr != nil {
			err = copyErr
		}
	}
	return err
}

// AT-FR-27-a: a rejection table drives every named class. Each is rejected;
// none is sanitised into acceptance.
func TestATFR27A_RejectionTable(t *testing.T) {
	type row struct {
		name    string
		class   Class
		field   string
		req     *Request
		respHdr http.Header // if set, the request is valid; the handler returns this
	}
	rows := []row{
		{
			name:  "absolute-form-http",
			class: ClassAbsoluteURI,
			field: "path",
			req:   validGET("rej-abs-http", "http://evil.example/api/state"),
		},
		{
			name:  "absolute-form-https",
			class: ClassAbsoluteURI,
			field: "path",
			req:   validGET("rej-abs-https", "https://evil.example/api/state"),
		},
		{
			name:  "absolute-form-uppercase-scheme",
			class: ClassAbsoluteURI,
			field: "path",
			req:   validGET("rej-abs-HTTP", "HTTP://evil.example/api/state"),
		},
		{
			name:  "authority-host-header",
			class: ClassAuthority,
			field: "header-name",
			req: &Request{
				ID:      "rej-host",
				Method:  http.MethodGet,
				Path:    "/api/state",
				Headers: http.Header{"Host": {"evil.example"}},
				Body:    bodyOf(""),
			},
		},
		{
			name:  "authority-protocol-relative-path",
			class: ClassAuthority,
			field: "path",
			req:   validGET("rej-proto-rel", "//evil.example/api/state"),
		},
		{
			name:  "hop-by-hop-connection",
			class: ClassHopByHop,
			field: "header-name",
			req: &Request{
				ID:      "rej-conn",
				Method:  http.MethodGet,
				Path:    "/api/state",
				Headers: http.Header{"Connection": {"close"}},
				Body:    bodyOf(""),
			},
		},
		{
			name:  "hop-by-hop-keep-alive",
			class: ClassHopByHop,
			field: "header-name",
			req: &Request{
				ID:      "rej-ka",
				Method:  http.MethodGet,
				Path:    "/api/state",
				Headers: http.Header{"Keep-Alive": {"timeout=5"}},
				Body:    bodyOf(""),
			},
		},
		{
			name:  "hop-by-hop-proxy-authenticate",
			class: ClassHopByHop,
			field: "header-name",
			req: &Request{
				ID:      "rej-pa",
				Method:  http.MethodGet,
				Path:    "/api/state",
				Headers: http.Header{"Proxy-Authenticate": {"Basic"}},
				Body:    bodyOf(""),
			},
		},
		{
			name:  "hop-by-hop-proxy-authorization",
			class: ClassHopByHop,
			field: "header-name",
			req: &Request{
				ID:      "rej-paz",
				Method:  http.MethodGet,
				Path:    "/api/state",
				Headers: http.Header{"Proxy-Authorization": {"Basic z"}},
				Body:    bodyOf(""),
			},
		},
		{
			name:  "hop-by-hop-te",
			class: ClassHopByHop,
			field: "header-name",
			req: &Request{
				ID:      "rej-te",
				Method:  http.MethodGet,
				Path:    "/api/state",
				Headers: http.Header{"Te": {"trailers"}},
				Body:    bodyOf(""),
			},
		},
		{
			name:  "hop-by-hop-transfer-encoding",
			class: ClassHopByHop,
			field: "header-name",
			req: &Request{
				ID:     "rej-te-enc",
				Method: http.MethodPost,
				Path:   "/api/nodes/n1/send",
				Headers: http.Header{
					"Content-Type":      {"application/json"},
					"Transfer-Encoding": {"chunked"},
				},
				Body: bodyOf(`{}`),
			},
		},
		{
			name:  "hop-by-hop-connection-listed",
			class: ClassHopByHop,
			field: "header-name",
			req: &Request{
				ID:     "rej-conn-listed",
				Method: http.MethodGet,
				Path:   "/api/state",
				Headers: http.Header{
					"Connection":  {"X-Hop-Extra"},
					"X-Hop-Extra": {"1"},
				},
				Body: bodyOf(""),
			},
		},
		{
			name:  "upgrade",
			class: ClassUpgrade,
			field: "header-name",
			req: &Request{
				ID:     "rej-upgrade",
				Method: http.MethodGet,
				Path:   "/api/state",
				Headers: http.Header{
					"Upgrade": {"websocket"},
				},
				Body: bodyOf(""),
			},
		},
		{
			name:  "trailers",
			class: ClassTrailer,
			field: "header-name",
			req: &Request{
				ID:     "rej-trailer",
				Method: http.MethodPost,
				Path:   "/api/nodes/n1/send",
				Headers: http.Header{
					"Content-Type": {"application/json"},
					"Trailer":      {"X-Checksum"},
				},
				Body: bodyOf(`{}`),
			},
		},
		{
			name:  "content-length-and-transfer-encoding",
			class: ClassLengthConflict,
			field: "header-name",
			req: &Request{
				ID:     "rej-cl-te",
				Method: http.MethodPost,
				Path:   "/api/nodes/n1/send",
				Headers: http.Header{
					"Content-Type":      {"application/json"},
					"Content-Length":    {"2"},
					"Transfer-Encoding": {"chunked"},
				},
				Body: bodyOf(`{}`),
			},
		},
		{
			name:  "crlf-in-path",
			class: ClassCRLF,
			field: "path",
			req:   validGET("rej-crlf-path", "/api/state\r\nHost: evil.example"),
		},
		{
			name:  "lf-in-path",
			class: ClassCRLF,
			field: "path",
			req:   validGET("rej-lf-path", "/api/state\nX-Injected: 1"),
		},
		{
			name:  "cr-in-path",
			class: ClassCRLF,
			field: "path",
			req:   validGET("rej-cr-path", "/api/state\rX-Injected: 1"),
		},
		{
			name:  "crlf-in-query",
			class: ClassCRLF,
			field: "query",
			req: &Request{
				ID:     "rej-crlf-query",
				Method: http.MethodGet,
				Path:   "/api/search",
				Query:  "q=1\r\nHost: evil.example",
				Body:   bodyOf(""),
			},
		},
		{
			name:  "lf-in-query",
			class: ClassCRLF,
			field: "query",
			req: &Request{
				ID:     "rej-lf-query",
				Method: http.MethodGet,
				Path:   "/api/search",
				Query:  "q=1\nX-Injected: 1",
				Body:   bodyOf(""),
			},
		},
		{
			name:  "crlf-in-header-name",
			class: ClassCRLF,
			field: "header-name",
			req: &Request{
				ID:     "rej-crlf-hname",
				Method: http.MethodGet,
				Path:   "/api/state",
				Headers: http.Header{
					"X-Foo\r\nX-Injected": {"1"},
				},
				Body: bodyOf(""),
			},
		},
		{
			name:  "lf-in-header-name",
			class: ClassCRLF,
			field: "header-name",
			req: &Request{
				ID:     "rej-lf-hname",
				Method: http.MethodGet,
				Path:   "/api/state",
				Headers: http.Header{
					"X-Foo\nX-Injected": {"1"},
				},
				Body: bodyOf(""),
			},
		},
		{
			name:  "crlf-in-header-value",
			class: ClassCRLF,
			field: "header-value",
			req: &Request{
				ID:     "rej-crlf-hval",
				Method: http.MethodGet,
				Path:   "/api/state",
				Headers: http.Header{
					"If-None-Match": {"\"e1\"\r\nX-Injected: 1"},
				},
				Body: bodyOf(""),
			},
		},
		{
			name:  "lf-in-header-value",
			class: ClassCRLF,
			field: "header-value",
			req: &Request{
				ID:     "rej-lf-hval",
				Method: http.MethodGet,
				Path:   "/api/state",
				Headers: http.Header{
					"If-None-Match": {"\"e1\"\nX-Injected: 1"},
				},
				Body: bodyOf(""),
			},
		},
		{
			name:  "crlf-in-response-header-value",
			class: ClassCRLF,
			field: "header-value",
			req:   validGET("rej-crlf-resp-hval", "/api/nodes/n1/assets/a1"),
			respHdr: http.Header{
				"Content-Type":        {"application/octet-stream"},
				"Content-Disposition": {"attachment; filename=\"x\r\nX-Injected: 1\""},
			},
		},
		{
			name:  "crlf-in-response-header-name",
			class: ClassCRLF,
			field: "header-name",
			req:   validGET("rej-crlf-resp-hname", "/api/nodes/n1/assets/a1"),
			respHdr: http.Header{
				"Content-Type":        {"application/octet-stream"},
				"X-Foo\r\nX-Injected": {"1"},
			},
		},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.respHdr != nil {
				err = tryRoundTripResponse(t, tc.req, tc.respHdr)
			} else {
				err = tryRoundTrip(t, tc.req)
			}
			if tc.field != "" {
				wantRejectField(t, err, tc.class, tc.field)
			} else {
				wantReject(t, err, tc.class)
			}
		})
	}
}

// methodUniverse is the RFC 9110 set plus the common extensions. AT-FR-27-b
// subtracts MethodAllowlist() from this and from synthetic names — it does
// not enumerate the rejected methods by hand.
var methodUniverse = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodConnect,
	http.MethodOptions, http.MethodTrace,
	"COPY", "LOCK", "MKCOL", "MOVE", "PROPFIND", "PROPPATCH", "UNLOCK",
	"REPORT", "SEARCH", "PURGE", "LINK", "UNLINK", "VIEW", "PRI",
	"BIND", "REBIND", "UNBIND", "ACL", "MKACTIVITY", "CHECKOUT",
	"MERGE", "NOTIFY", "SUBSCRIBE", "UNSUBSCRIBE", "M-SEARCH",
	"get", "Get", "post", "PATCH ",
}

// headerUniverse is a closed IANA/common-header set used to generate the
// unlisted complement. Names that are request-level rejections (hop-by-hop,
// Upgrade, Trailer, Host) are skipped here so drop vs reject stay distinct.
var headerUniverse = []string{
	"Accept", "Accept-Charset", "Accept-Encoding", "Accept-Language",
	"Accept-Ranges", "Age", "Allow", "Authorization", "Cache-Control",
	"Content-Disposition", "Content-Encoding", "Content-Language",
	"Content-Length", "Content-Location", "Content-MD5", "Content-Range",
	"Content-Type", "Cookie", "Date", "ETag", "Expect", "Expires",
	"From", "If-Match", "If-Modified-Since", "If-None-Match",
	"If-Range", "If-Unmodified-Since", "Last-Modified", "Link",
	"Location", "Max-Forwards", "Origin", "Pragma", "Range", "Referer",
	"Retry-After", "Server", "Set-Cookie", "User-Agent", "Vary", "Via",
	"Warning", "WWW-Authenticate", "X-Content-Type-Options",
	"X-Frame-Options", "X-Scimux-CSRF", "X-Forwarded-For",
	"X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP", "Forwarded",
	"Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest",
	"Sec-WebSocket-Key", "Access-Control-Allow-Origin",
	"Access-Control-Request-Method", "DNT", "Tk", "X-Requested-With",
	"X-CSRF-Token", "Referrer-Policy", "Permissions-Policy",
	"Content-Security-Policy",
}

var rejectOnSightHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Proxy-Connection":    {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
	"Host":                {},
}

func isRejectOnSightHeader(name string) bool {
	_, ok := rejectOnSightHeaders[http.CanonicalHeaderKey(name)]
	return ok
}

// AT-FR-27-b: method and header allowlists are closed. Unlisted methods and
// headers are generated as the complement of the package allowlists, so a
// future addition cannot pass by default.
func TestATFR27B_AllowlistsClosedByGeneration(t *testing.T) {
	t.Run("forbidden-names-absent-from-allowlists", func(t *testing.T) {
		methods := MethodAllowlist()
		for _, m := range []string{http.MethodConnect, http.MethodTrace} {
			if _, ok := methods[m]; ok {
				t.Errorf("method allowlist contains %s", m)
			}
		}
		for _, allow := range []map[string]struct{}{RequestHeaderAllowlist(), ResponseHeaderAllowlist()} {
			for name := range rejectOnSightHeaders {
				if headerAllowed(allow, name) {
					t.Errorf("header allowlist contains reject-on-sight name %s", name)
				}
			}
			for _, name := range []string{"X-Scimux-CSRF", "Accept-Encoding", "Content-Encoding"} {
				if headerAllowed(allow, name) {
					t.Errorf("header allowlist contains %s", name)
				}
			}
		}
		if !headerAllowed(RequestHeaderAllowlist(), "Range") {
			t.Error("Range missing from request allowlist")
		}
		if !headerAllowed(ResponseHeaderAllowlist(), "Content-Range") {
			t.Error("Content-Range missing from response allowlist")
		}
		if !headerAllowed(ResponseHeaderAllowlist(), "Accept-Ranges") {
			t.Error("Accept-Ranges missing from response allowlist")
		}
	})

	t.Run("unlisted-methods-rejected", func(t *testing.T) {
		allow := MethodAllowlist()
		if len(allow) == 0 {
			t.Fatal("MethodAllowlist is empty; generation would treat every method as unlisted")
		}
		seen := 0
		for _, m := range methodUniverse {
			if _, ok := allow[m]; ok {
				continue
			}
			seen++
			t.Run(m, func(t *testing.T) {
				err := tryRoundTrip(t, &Request{
					ID:     "rej-method-" + m,
					Method: m,
					Path:   "/api/state",
					Body:   bodyOf(""),
				})
				wantReject(t, err, ClassMethod)
			})
		}
		for i := 0; i < 8; i++ {
			m := "M" + strconv.Itoa(i)
			if _, ok := allow[m]; ok {
				t.Fatalf("synthetic method %s collided with the allowlist", m)
			}
			seen++
			t.Run("synthetic/"+m, func(t *testing.T) {
				err := tryRoundTrip(t, &Request{
					ID:     "rej-method-" + m,
					Method: m,
					Path:   "/api/state",
					Body:   bodyOf(""),
				})
				wantReject(t, err, ClassMethod)
			})
		}
		if seen == 0 {
			t.Fatal("generated zero unlisted methods; the complement is empty")
		}
	})

	t.Run("unlisted-request-headers-dropped", func(t *testing.T) {
		allow := RequestHeaderAllowlist()
		if len(allow) == 0 {
			t.Fatal("RequestHeaderAllowlist is empty")
		}
		var names []string
		for _, name := range headerUniverse {
			if headerAllowed(allow, name) || isRejectOnSightHeader(name) {
				continue
			}
			names = append(names, name)
		}
		for i := 0; i < 8; i++ {
			name := "X-Unlisted-" + strconv.Itoa(i)
			if headerAllowed(allow, name) {
				t.Fatalf("synthetic header %s collided with the allowlist", name)
			}
			names = append(names, name)
		}
		if len(names) == 0 {
			t.Fatal("generated zero unlisted request headers")
		}

		headers := make(http.Header)
		headers.Set("If-None-Match", `"keep"`)
		for _, name := range names {
			headers[http.CanonicalHeaderKey(name)] = []string{"must-drop"}
		}

		seen := make(chan collectedReq, 1)
		client := startPair(t, collectingHandler(seen, &Response{
			Status:  http.StatusOK,
			Headers: http.Header{"Content-Type": {"application/json"}},
			Body:    bodyOf(`{}`),
		}))
		_ = mustRoundTrip(t, client, &Request{
			ID:      "req-gen-drop",
			Method:  http.MethodGet,
			Path:    "/api/state",
			Headers: headers,
			Body:    bodyOf(""),
		})
		var got collectedReq
		select {
		case got = <-seen:
		case <-time.After(2 * time.Second):
			t.Fatal("handler did not receive the request")
		}
		if got.Headers.Get("If-None-Match") != `"keep"` {
			t.Errorf("allowlisted If-None-Match dropped: %q", got.Headers.Get("If-None-Match"))
		}
		for _, name := range names {
			if v := got.Headers.Values(name); len(v) > 0 {
				t.Errorf("unlisted request header %s was forwarded: %q", name, v)
			}
		}
	})

	t.Run("unlisted-response-headers-dropped", func(t *testing.T) {
		allow := ResponseHeaderAllowlist()
		if len(allow) == 0 {
			t.Fatal("ResponseHeaderAllowlist is empty")
		}
		respHeaders := make(http.Header)
		respHeaders.Set("Content-Type", "application/json")
		var names []string
		for _, name := range headerUniverse {
			if headerAllowed(allow, name) || isRejectOnSightHeader(name) {
				continue
			}
			names = append(names, name)
			respHeaders[http.CanonicalHeaderKey(name)] = []string{"must-drop"}
		}
		for i := 0; i < 8; i++ {
			name := "X-Unlisted-Resp-" + strconv.Itoa(i)
			if headerAllowed(allow, name) {
				t.Fatalf("synthetic header %s collided with the allowlist", name)
			}
			names = append(names, name)
			respHeaders[http.CanonicalHeaderKey(name)] = []string{"must-drop"}
		}
		if len(names) == 0 {
			t.Fatal("generated zero unlisted response headers")
		}

		client := startPair(t, HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
			return &Response{
				ID:      req.ID,
				Status:  http.StatusOK,
				Headers: respHeaders,
				Body:    bodyOf(`{}`),
			}, nil
		}))
		resp := mustRoundTrip(t, client, validGET("req-gen-resp-drop", "/api/state"))
		if resp.Headers.Get("Content-Type") != "application/json" {
			t.Errorf("allowlisted Content-Type dropped: %q", resp.Headers.Get("Content-Type"))
		}
		for _, name := range names {
			if v := resp.Headers.Values(name); len(v) > 0 {
				t.Errorf("unlisted response header %s was forwarded: %q", name, v)
			}
		}
	})
}

// AT-FR-27-c: cancellation propagates — a cancelled remote request releases
// the local handler's context and leaks no goroutine.
func TestATFR27C_CancelReleasesHandlerAndLeaksNoGoroutine(t *testing.T) {
	before := snapshotGoroutines()
	// Register before startPair: cleanups run LIFO, so the leak check runs
	// after the pair is torn down and Serve has returned.
	t.Cleanup(func() { assertNoGoroutineLeak(t, before) })

	started := make(chan struct{})
	released := make(chan struct{})
	client := startPair(t, HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
		close(started)
		select {
		case <-ctx.Done():
			close(released)
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			t.Error("handler context was not cancelled")
			return &Response{ID: req.ID, Status: 200, Body: bodyOf("")}, nil
		}
	}))

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		_, err := client.RoundTrip(ctx, validGET("req-cancel", "/api/state"))
		errc <- err
	}()

	select {
	case <-started:
	case err := <-errc:
		t.Fatalf("RoundTrip returned before the handler started: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not invoked")
	}

	cancel()

	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("handler context was not released")
	}

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("cancelled RoundTrip succeeded")
		}
		if !isCancelErr(err) {
			t.Fatalf("cancelled RoundTrip err = %v, want a cancellation error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RoundTrip did not return after cancel")
	}
}

func isCancelErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
