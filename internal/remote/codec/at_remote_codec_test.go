package codec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// AT-remote-codec-a: Request/response frames round-trip over io.Pipe
// preserving exactly the FR-27 schema — request ID, allowed method,
// origin-relative path and query, allowlisted headers, bounded body.
func TestATRemoteCodecA_RoundTripPreservesSchema(t *testing.T) {
	seen := make(chan collectedReq, 1)
	body := `{"text":"hello"}`
	client := startPair(t, collectingHandler(seen, &Response{
		Status: http.StatusOK,
		Headers: http.Header{
			"Content-Type": {"application/json"},
			"Etag":         {`"rev-1"`},
		},
		Body: bodyOf(`{"ok":true}`),
	}))

	req := &Request{
		ID:     "req-schema-1",
		Method: http.MethodPost,
		Path:   "/api/nodes/n1/send",
		Query:  "history=1",
		Headers: http.Header{
			"Content-Type":  {"application/json"},
			"If-None-Match": {`"prev"`},
		},
		Body: bodyOf(body),
	}
	resp := mustRoundTrip(t, client, req)

	var got collectedReq
	select {
	case got = <-seen:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not receive the request")
	}

	if got.ID != req.ID {
		t.Errorf("handler request id = %q, want %q", got.ID, req.ID)
	}
	if got.Method != http.MethodPost {
		t.Errorf("handler method = %q, want POST", got.Method)
	}
	if got.Path != "/api/nodes/n1/send" {
		t.Errorf("handler path = %q, want origin-relative /api/nodes/n1/send", got.Path)
	}
	if got.Query != "history=1" {
		t.Errorf("handler query = %q, want history=1", got.Query)
	}
	if ct := got.Headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("handler Content-Type = %q, want application/json", ct)
	}
	if inm := got.Headers.Get("If-None-Match"); inm != `"prev"` {
		t.Errorf("handler If-None-Match = %q, want \"prev\"", inm)
	}
	if string(got.Body) != body {
		t.Errorf("handler body = %q, want %q", got.Body, body)
	}

	if resp.ID != req.ID {
		t.Errorf("response id = %q, want %q", resp.ID, req.ID)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("response status = %d, want 200", resp.Status)
	}
	if ct := resp.Headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("response Content-Type = %q, want application/json", ct)
	}
	if et := resp.Headers.Get("ETag"); et != `"rev-1"` {
		t.Errorf("response ETag = %q, want \"rev-1\"", et)
	}
	gotBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(gotBody) != `{"ok":true}` {
		t.Errorf("response body = %q, want {\"ok\":true}", gotBody)
	}
}

// AT-remote-codec-a: header fidelity is explicitly not the property —
// headers outside the allowlist must be dropped, and the test asserts that
// they are. X-Scimux-CSRF and the encoding headers are the known traps.
func TestATRemoteCodecA_DropsUnlistedHeaders(t *testing.T) {
	seen := make(chan collectedReq, 1)
	client := startPair(t, collectingHandler(seen, &Response{
		Status: http.StatusOK,
		Headers: http.Header{
			"Content-Type":           {"application/json"},
			"X-Internal-Debug":       {"secret"},
			"Content-Encoding":       {"gzip"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: bodyOf(`{}`),
	}))

	req := &Request{
		ID:     "req-drop-1",
		Method: http.MethodGet,
		Path:   "/api/state",
		Headers: http.Header{
			"If-None-Match":   {`"e1"`},
			"X-Scimux-CSRF":   {"token-must-drop"},
			"Accept-Encoding": {"gzip"},
			"X-Debug-Trace":   {"1"},
			"Authorization":   {"Bearer nope"},
		},
		Body: bodyOf(""),
	}
	resp := mustRoundTrip(t, client, req)

	var got collectedReq
	select {
	case got = <-seen:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not receive the request")
	}

	if got.Headers.Get("If-None-Match") != `"e1"` {
		t.Errorf("allowlisted If-None-Match was not forwarded: %q", got.Headers.Get("If-None-Match"))
	}
	for _, name := range []string{"X-Scimux-CSRF", "Accept-Encoding", "X-Debug-Trace", "Authorization"} {
		if v := got.Headers.Values(name); len(v) > 0 {
			t.Errorf("unlisted request header %s was forwarded: %q", name, v)
		}
	}

	if resp.Headers.Get("Content-Type") != "application/json" {
		t.Errorf("allowlisted Content-Type was not forwarded: %q", resp.Headers.Get("Content-Type"))
	}
	if resp.Headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("allowlisted X-Content-Type-Options was not forwarded: %q", resp.Headers.Get("X-Content-Type-Options"))
	}
	for _, name := range []string{"X-Internal-Debug", "Content-Encoding"} {
		if v := resp.Headers.Values(name); len(v) > 0 {
			t.Errorf("unlisted response header %s was forwarded: %q", name, v)
		}
	}
}

// AT-remote-codec-a / FR-27: Range must survive in both directions with
// Content-Range, Accept-Ranges and status 206 intact.
func TestATRemoteCodecA_RangeRoundTrip(t *testing.T) {
	seen := make(chan collectedReq, 1)
	payload := bytes.Repeat([]byte("R"), 64)
	client := startPair(t, collectingHandler(seen, &Response{
		Status: http.StatusPartialContent,
		Headers: http.Header{
			"Content-Type":   {"image/png"},
			"Content-Range":  {"bytes 0-63/1000"},
			"Accept-Ranges":  {"bytes"},
			"Content-Length": {"64"},
		},
		Body: io.NopCloser(bytes.NewReader(payload)),
	}))

	req := &Request{
		ID:     "req-range-1",
		Method: http.MethodGet,
		Path:   "/api/nodes/n1/assets/a1",
		Headers: http.Header{
			"Range": {"bytes=0-63"},
		},
		Body: bodyOf(""),
	}
	resp := mustRoundTrip(t, client, req)

	var got collectedReq
	select {
	case got = <-seen:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not receive the request")
	}
	if got.Headers.Get("Range") != "bytes=0-63" {
		t.Errorf("Range did not survive the request path: %q", got.Headers.Get("Range"))
	}
	if resp.Status != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", resp.Status)
	}
	if resp.Headers.Get("Content-Range") != "bytes 0-63/1000" {
		t.Errorf("Content-Range = %q, want %q", resp.Headers.Get("Content-Range"), "bytes 0-63/1000")
	}
	if resp.Headers.Get("Accept-Ranges") != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", resp.Headers.Get("Accept-Ranges"))
	}
	gotBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read ranged body: %v", err)
	}
	if !bytes.Equal(gotBody, payload) {
		t.Errorf("ranged body len=%d, want 64", len(gotBody))
	}
}

// AT-remote-codec-b: Concurrent in-flight requests do not interleave incorrectly.
// Handlers complete in reverse request order, gated by per-index events, so an
// implementation that pairs responses by arrival rather than by ID fails.
func TestATRemoteCodecB_ConcurrentRequestsDoNotInterleave(t *testing.T) {
	const n = 16
	arrived := make([]chan struct{}, n)
	release := make([]chan struct{}, n)
	done := make([]chan error, n)
	for i := 0; i < n; i++ {
		arrived[i] = make(chan struct{})
		release[i] = make(chan struct{})
		done[i] = make(chan error, 1)
	}

	client := startPair(t, HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
		const prefix = "req-conc-"
		if len(req.ID) < len(prefix) {
			return nil, fmt.Errorf("bad id %q", req.ID)
		}
		idx, err := strconv.Atoi(req.ID[len(prefix):])
		if err != nil || idx < 0 || idx >= n {
			return nil, fmt.Errorf("bad id %q", req.ID)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		close(arrived[idx])
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release[idx]:
		}
		return &Response{
			ID:     req.ID,
			Status: http.StatusOK,
			Headers: http.Header{
				"Etag": {`"` + req.ID + `"`},
			},
			Body: io.NopCloser(bytes.NewReader(body)),
		}, nil
	}))

	for i := 0; i < n; i++ {
		i := i
		id := "req-conc-" + strconv.Itoa(i)
		payload := "body-" + id
		go func() {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			resp, err := client.RoundTrip(ctx, &Request{
				ID:     id,
				Method: http.MethodPost,
				Path:   "/api/nodes/n1/send",
				Headers: http.Header{
					"Content-Type": {"application/json"},
				},
				Body: bodyOf(payload),
			})
			if err != nil {
				done[i] <- err
				return
			}
			if resp.ID != id {
				done[i] <- fmt.Errorf("response id = %q, want %q", resp.ID, id)
				return
			}
			if et := resp.Headers.Get("ETag"); et != `"`+id+`"` {
				done[i] <- fmt.Errorf("response ETag = %q, want %q", et, `"`+id+`"`)
				return
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				done[i] <- err
				return
			}
			if string(got) != payload {
				done[i] <- fmt.Errorf("response body = %q, want %q (ids interleaved)", got, payload)
				return
			}
			done[i] <- nil
		}()
	}

	for i := 0; i < n; i++ {
		select {
		case <-arrived[i]:
		case err := <-done[i]:
			t.Fatalf("request %d returned before its handler started: %v", i, err)
		case <-time.After(2 * time.Second):
			t.Fatalf("handler %d did not start", i)
		}
	}
	for i := n - 1; i >= 0; i-- {
		close(release[i])
	}
	for i := 0; i < n; i++ {
		select {
		case err := <-done[i]:
			if err != nil {
				t.Errorf("request %d: %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("request %d did not finish after reverse release", i)
		}
	}
}

// AT-remote-codec-c: Truncated and malformed frames are rejected without panic.
func TestATRemoteCodecC_TruncatedAndMalformedRejected(t *testing.T) {
	cases := []struct {
		name  string
		data  []byte
		class Class
	}{
		{name: "empty", data: nil, class: ClassTruncated},
		{name: "one-byte", data: []byte{typeRequest}, class: ClassTruncated},
		// Garbage no longer fails on the type byte. tunnel-v2 §2.1
		// makes an unrecognised type skippable rather than fatal, so
		// the verdict is deferred to the length prefix — which for
		// arbitrary bytes means the payload never arrives. Still
		// rejected without panic, which is what this AC asserts; the
		// class is truncated rather than malformed.
		{name: "three-ff", data: []byte{0xff, 0xff, 0xff}, class: ClassTruncated},
		{name: "raw-http", data: []byte("GET /api/state HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"), class: ClassTruncated},
		{name: "claimed-length-eof", data: []byte{typeRequest, 0x00, 0x10, 0x00, 0x01}, class: ClassTruncated},
		// §3.1's one exception. Type 0x00 is permanently reserved, so
		// refusing it forecloses nothing; without the refusal a
		// zero-filled buffer would decode as an endless run of valid
		// empty frames.
		{name: "reserved-type", data: []byte{0x00, 0x00, 0x00, 0x00}, class: ClassMalformed},
		{name: "reserved-type-alone", data: []byte{0x00}, class: ClassMalformed},
	}
	for _, tc := range cases {
		t.Run("decode/"+tc.name, func(t *testing.T) {
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("decodeFrame panicked: %v", rec)
				}
			}()
			_, err := decodeFrame(bytes.NewReader(tc.data))
			wantReject(t, err, tc.class)
		})
	}

	t.Run("serve/garbage", func(t *testing.T) {
		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("Serve panicked: %v", rec)
			}
		}()
		sr, cw := io.Pipe()
		cr, sw := io.Pipe()
		server := NewConn(sr, sw, RoleResponder)
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			_, _ = io.Copy(io.Discard, cr)
		}()
		t.Cleanup(func() {
			_ = cr.Close()
			<-drained
		})
		go func() {
			_, _ = cw.Write([]byte("HTTP/1.1 200 OK\r\n\r\nnot-a-frame"))
			_ = cw.Close()
		}()
		err := server.Serve(t.Context(), HandlerFunc(func(context.Context, *Request) (*Response, error) {
			t.Error("handler must not run on a malformed stream")
			return &Response{Status: 200, Body: bodyOf("")}, nil
		}))
		_ = sw.Close()
		// The verdict moved earlier, and got more specific. Garbage now
		// arrives where the preamble was due (tunnel-v2 §2.2.1), so the
		// stream is refused as "not a tunnel peer" before a single frame
		// is parsed — which is what FR-24 has to be able to say. Still
		// rejected without panic and the handler still never runs, which
		// is what this AC asserts.
		var ve *VersionError
		if !errors.As(err, &ve) || ve.Reason != VersionNoPreamble {
			t.Fatalf("err = %v (%T), want *VersionError %s", err, err, VersionNoPreamble)
		}
		if ve.HavePeer {
			t.Error("HavePeer is true for a peer whose preamble never parsed")
		}
	})
}

// AT-remote-codec-d: a body exceeding the cap applicable to that route is
// rejected. Caps differ per route, so one global cap does not cover this.
func TestATRemoteCodecD_PerRouteBodyCap(t *testing.T) {
	overJSON := JSONBodyMax + 1

	t.Run("json-send-over-cap-rejected", func(t *testing.T) {
		client := startPair(t, echoOK)
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		_, err := client.RoundTrip(ctx, &Request{
			ID:     "req-json-over",
			Method: http.MethodPost,
			Path:   "/api/nodes/n1/send",
			Headers: http.Header{
				"Content-Type": {"application/json"},
			},
			Body: limitBody(overJSON, 'j'),
		})
		if err == nil {
			t.Fatal("POST /send of jsonBodyMax+1 accepted; want per-route reject")
		}
		wantReject(t, err, ClassBodyTooLarge)
	})

	t.Run("ui-put-over-cap-rejected", func(t *testing.T) {
		client := startPair(t, echoOK)
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		_, err := client.RoundTrip(ctx, &Request{
			ID:     "req-ui-over",
			Method: http.MethodPut,
			Path:   "/api/ui",
			Headers: http.Header{
				"Content-Type": {"application/json"},
				"If-Match":     {`"r1"`},
			},
			Body: limitBody(UIStateMax+1, 'u'),
		})
		wantReject(t, err, ClassBodyTooLarge)
	})

	t.Run("same-size-accepted-on-upload-route", func(t *testing.T) {
		// The same overJSON size must succeed on the 25 MiB upload route.
		// A single global 1 MiB cap fails this; a single global 25/50 MiB
		// cap fails the send-over-cap row above.
		seen := make(chan collectedReq, 1)
		client := startPair(t, collectingHandler(seen, &Response{
			Status:  http.StatusOK,
			Headers: http.Header{"Content-Type": {"application/json"}},
			Body:    bodyOf(`{"attachments":[]}`),
		}))
		resp := mustRoundTrip(t, client, &Request{
			ID:     "req-attach-under",
			Method: http.MethodPost,
			Path:   "/api/nodes/n1/attachments",
			Headers: http.Header{
				"Content-Type": {"multipart/form-data; boundary=x"},
			},
			Body: limitBody(overJSON, 'a'),
		})
		if resp.Status != http.StatusOK {
			t.Fatalf("upload of %d bytes status = %d, want 200", overJSON, resp.Status)
		}
		select {
		case got := <-seen:
			if int64(len(got.Body)) != overJSON {
				t.Fatalf("upload handler saw %d bytes, want %d", len(got.Body), overJSON)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("upload handler did not run")
		}
	})

	t.Run("json-send-at-cap-succeeds", func(t *testing.T) {
		seen := make(chan collectedReq, 1)
		client := startPair(t, collectingHandler(seen, &Response{
			Status:  http.StatusOK,
			Headers: http.Header{"Content-Type": {"application/json"}},
			Body:    bodyOf(`{}`),
		}))
		_ = mustRoundTrip(t, client, &Request{
			ID:     "req-json-at",
			Method: http.MethodPost,
			Path:   "/api/nodes/n1/send",
			Headers: http.Header{
				"Content-Type": {"application/json"},
			},
			Body: limitBody(JSONBodyMax, 'j'),
		})
		select {
		case got := <-seen:
			if int64(len(got.Body)) != JSONBodyMax {
				t.Fatalf("at-cap send handler saw %d bytes, want %d", len(got.Body), JSONBodyMax)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("at-cap send handler did not run")
		}
	})

	t.Run("upload-over-25mib-rejected", func(t *testing.T) {
		client := startPair(t, echoOK)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		_, err := client.RoundTrip(ctx, &Request{
			ID:     "req-attach-over",
			Method: http.MethodPost,
			Path:   "/api/nodes/n1/attachments",
			Headers: http.Header{
				"Content-Type": {"multipart/form-data; boundary=x"},
			},
			Body: limitBody(AttachUploadMax+1, 'A'),
		})
		wantReject(t, err, ClassBodyTooLarge)
	})

	t.Run("asset-over-50mib-rejected", func(t *testing.T) {
		client := startPair(t, HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
			return &Response{
				ID:     req.ID,
				Status: http.StatusOK,
				Headers: http.Header{
					"Content-Type": {"application/octet-stream"},
				},
				Body: limitBody(AgentAssetMax+1, 'Z'),
			}, nil
		}))
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		resp, err := client.RoundTrip(ctx, &Request{
			ID:     "req-asset-over",
			Method: http.MethodGet,
			Path:   "/api/nodes/n1/assets/a1",
			Body:   bodyOf(""),
		})
		if err == nil && resp != nil && resp.Body != nil {
			_, err = io.Copy(io.Discard, resp.Body)
		}
		wantReject(t, err, ClassBodyTooLarge)
	})

	t.Run("unmapped-path-fail-closed-at-json-cap", func(t *testing.T) {
		client := startPair(t, echoOK)
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		_, err := client.RoundTrip(ctx, &Request{
			ID:     "req-unmapped-over",
			Method: http.MethodPost,
			Path:   "/api/not-a-recognised-route",
			Headers: http.Header{
				"Content-Type": {"application/json"},
			},
			Body: limitBody(JSONBodyMax+1, 'x'),
		})
		wantReject(t, err, ClassBodyTooLarge)
	})

	t.Run("state-over-json-cap-succeeds", func(t *testing.T) {
		const n = JSONBodyMax + 1
		client := startPair(t, HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
			return &Response{
				ID:     req.ID,
				Status: http.StatusOK,
				Headers: http.Header{
					"Content-Type": {"application/json"},
				},
				Body: limitBody(n, 's'),
			}, nil
		}))
		resp := mustRoundTrip(t, client, validGET("req-state-over-json", "/api/state"))
		got, err := countBytes(resp.Body)
		if err != nil {
			t.Fatalf("read /api/state over jsonBodyMax: %v", err)
		}
		if got != n {
			t.Fatalf("client consumed %d bytes, want %d", got, n)
		}
	})

	t.Run("state-over-asset-cap-rejected", func(t *testing.T) {
		client := startPair(t, HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
			return &Response{
				ID:     req.ID,
				Status: http.StatusOK,
				Headers: http.Header{
					"Content-Type": {"application/json"},
				},
				Body: limitBody(AgentAssetMax+1, 'S'),
			}, nil
		}))
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		resp, err := client.RoundTrip(ctx, validGET("req-state-over-asset", "/api/state"))
		if err == nil && resp != nil && resp.Body != nil {
			_, err = io.Copy(io.Discard, resp.Body)
		}
		wantReject(t, err, ClassBodyTooLarge)
	})
}

var echoOK = HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	return &Response{
		ID:      req.ID,
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": {"application/json"}},
		Body:    bodyOf(`{}`),
	}, nil
})

// AT-remote-codec-d: a 25 MiB multipart upload and a 50 MiB asset response
// both succeed, and neither is materialised whole in memory on either side.
func TestATRemoteCodecD_LargeUploadAndAssetNotMaterialized(t *testing.T) {
	const maxResident = 8 << 20

	t.Run("upload-25mib", func(t *testing.T) {
		var saw atomic.Int64
		client := startPair(t, HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
			n, err := countBytes(req.Body)
			saw.Store(n)
			if err != nil {
				return nil, err
			}
			return &Response{
				ID:      req.ID,
				Status:  http.StatusOK,
				Headers: http.Header{"Content-Type": {"application/json"}},
				Body:    bodyOf(`{"ok":true}`),
			}, nil
		}))
		watch := startHeapWatch(t)
		resp := mustRoundTrip(t, client, &Request{
			ID:     "req-25m",
			Method: http.MethodPost,
			Path:   "/api/nodes/n1/attachments",
			Headers: http.Header{
				"Content-Type": {"multipart/form-data; boundary=x"},
			},
			Body: limitBody(AttachUploadMax, 'U'),
		})
		delta := watch.Delta()
		if resp.Status != http.StatusOK {
			t.Fatalf("25 MiB upload status = %d, want 200", resp.Status)
		}
		if saw.Load() != AttachUploadMax {
			t.Fatalf("handler consumed %d bytes, want %d", saw.Load(), AttachUploadMax)
		}
		if delta > maxResident {
			t.Fatalf("heap grew by %d bytes carrying a %d-byte upload; body was materialised", delta, AttachUploadMax)
		}
	})

	t.Run("asset-50mib", func(t *testing.T) {
		client := startPair(t, HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
			return &Response{
				ID:     req.ID,
				Status: http.StatusOK,
				Headers: http.Header{
					"Content-Type":   {"application/octet-stream"},
					"Accept-Ranges":  {"bytes"},
					"Content-Length": {"52428800"},
				},
				Body: limitBody(AgentAssetMax, 'S'),
			}, nil
		}))
		watch := startHeapWatch(t)
		resp := mustRoundTrip(t, client, &Request{
			ID:     "req-50m",
			Method: http.MethodGet,
			Path:   "/api/nodes/n1/assets/a1",
			Body:   bodyOf(""),
		})
		n, err := countBytes(resp.Body)
		delta := watch.Delta()
		if err != nil {
			t.Fatalf("read 50 MiB asset: %v", err)
		}
		if n != AgentAssetMax {
			t.Fatalf("client consumed %d bytes, want %d", n, AgentAssetMax)
		}
		if delta > maxResident {
			t.Fatalf("heap grew by %d bytes carrying a %d-byte asset; body was materialised", delta, AgentAssetMax)
		}
	})

	t.Run("streamed-not-buffered-first", func(t *testing.T) {
		const first = 64 << 10
		gotFirst := make(chan struct{})
		unblock := make(chan struct{})
		client := startPair(t, HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
			buf := make([]byte, first)
			n, err := io.ReadFull(req.Body, buf)
			if n == first && err == nil {
				close(gotFirst)
			} else if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
				return nil, err
			}
			_, _ = io.Copy(io.Discard, req.Body)
			return &Response{
				ID:      req.ID,
				Status:  http.StatusOK,
				Headers: http.Header{"Content-Type": {"application/json"}},
				Body:    bodyOf(`{}`),
			}, nil
		}))

		req := &Request{
			ID:     "req-stall",
			Method: http.MethodPost,
			Path:   "/api/nodes/n1/attachments",
			Headers: http.Header{
				"Content-Type": {"multipart/form-data; boundary=x"},
			},
			Body: &stallReader{first: bytes.Repeat([]byte("s"), first), unblock: unblock},
		}
		errc := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			_, err := client.RoundTrip(ctx, req)
			errc <- err
		}()
		select {
		case err := <-errc:
			t.Fatalf("RoundTrip returned before the stalled body was released: %v", err)
		case <-gotFirst:
		case <-time.After(2 * time.Second):
			t.Fatal("handler did not observe the first body chunk; body is buffered or not carried")
		}
		close(unblock)
		select {
		case err := <-errc:
			if err != nil {
				t.Fatalf("streamed upload: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("RoundTrip did not finish after the body was released")
		}
	})
}
