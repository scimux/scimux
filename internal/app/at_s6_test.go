package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/remote"
)

// AT-int-a: Two pion peers in-process drive a real /api/state through
// the tunnel and assert the JSON. The tunnel boundary is used, not the
// browser boundary; gzip is forbidden on the data channel.
func TestAT_int_a_InProcessPeersDriveAPIState(t *testing.T) {
	const at = "AT-int-a"
	a := newTestApp(t, &fakeTmux{})
	_, th, err := newBoundaries(a, webFS, s3Peer())
	if err != nil {
		t.Fatalf("%s: newBoundaries: %v", at, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := remote.InProcessTunnel(ctx, th)
	if err != nil {
		t.Fatalf("%s: in-process pion peers: %v", at, err)
	}
	if s == nil {
		t.Fatalf("%s: session is nil", at)
	}
	t.Cleanup(func() { _ = s.Close() })

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/api/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.RoundTrip(req)
	if err != nil {
		t.Fatalf("%s: GET /api/state over data channel: %v", at, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: GET /api/state status %d body %q", at, resp.StatusCode, body)
	}
	if ce := resp.Header.Get("Content-Encoding"); strings.EqualFold(ce, "gzip") {
		t.Fatalf("%s: gzip over the data channel is forbidden, got Content-Encoding %q", at, ce)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("%s: /api/state is not JSON: %v (%q)", at, err, raw)
	}
	for _, key := range []string{"nodes", "sys", "socket", "hostname", "version"} {
		if _, ok := top[key]; !ok {
			t.Errorf("%s: top-level missing %q", at, key)
		}
	}
	if top["version"] != version {
		t.Errorf("%s: version = %v, want %q (same local renderer and version)", at, top["version"], version)
	}
}

// AT-FR-14-b: The index is served by the same local renderer and version,
// with per-process CSRF/template substitutions declared as allowed
// differences. Byte-identity is the wrong assertion.
func TestAT_FR_14_b_TunnelIndexSameRenderer(t *testing.T) {
	const at = "AT-FR-14-b"
	a := newTestApp(t, &fakeTmux{})
	local := newTestHandler(t, a)
	localRec := routeRequest(local, http.MethodGet, "/", "", false)
	if localRec.Code != http.StatusOK {
		t.Fatalf("%s: local GET / status %d", at, localRec.Code)
	}
	if bytes.Contains(localRec.Body.Bytes(), []byte(csrfPlaceholder)) {
		t.Fatalf("%s: local index still contains CSRF placeholder", at)
	}

	_, th, err := newBoundaries(a, webFS, s3Peer())
	if err != nil {
		t.Fatalf("%s: newBoundaries: %v", at, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := remote.InProcessTunnel(ctx, th)
	if err != nil {
		t.Fatalf("%s: in-process tunnel: %v", at, err)
	}
	if s == nil {
		t.Fatalf("%s: session is nil", at)
	}
	t.Cleanup(func() { _ = s.Close() })

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.RoundTrip(req)
	if err != nil {
		t.Fatalf("%s: GET / over data channel: %v", at, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: tunnel GET / status %d body %q", at, resp.StatusCode, body)
	}
	remoteBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(remoteBody, []byte(csrfPlaceholder)) {
		t.Fatalf("%s: tunnel index still contains CSRF placeholder", at)
	}
	if !bytes.Contains(remoteBody, []byte(`name="scimux-csrf"`)) {
		t.Fatalf("%s: tunnel index is not the csrfIndex renderer", at)
	}

	// Allowed body differences: the per-process CSRF token value. Nothing
	// else in the index body may diverge — that would be a second renderer.
	if !bytes.Equal(s6NormalizeIndex(localRec.Body.Bytes()), s6NormalizeIndex(remoteBody)) {
		t.Fatalf("%s: tunnel index is not the same local renderer (after declaring CSRF as an allowed difference)\n local %d bytes\nremote %d bytes",
			at, localRec.Body.Len(), len(remoteBody))
	}
}

var s6CSRFValue = regexp.MustCompile(`content="[0-9a-f]+"`)

func s6NormalizeIndex(b []byte) []byte {
	return s6CSRFValue.ReplaceAll(b, []byte(`content="CSRF"`))
}
