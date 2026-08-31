package remote

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

// Vector recipient from docs/protocol testdata/vectors/constructions.json
// (envelope-seal-p256). Tests only; the seam must not read this file.
const (
	s6VectorRecipientPubHex  = "046a8620064ea5a5629fefc1762c3b84a2aba64bfacdbd50a5719f9383bb2bc8f488c9c7c4586f5c5d0523829105d90409ba6b56c208b69add3182430b9120a354"
	s6VectorRecipientPrivHex = "3232323232323232323232323232323232323232323232323232323232323200"
	s6VectorRID              = "abababababababababababababababababababababababababababababababab"
)

func s6RequireSession(t *testing.T, at string, s *Session, err error) *Session {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", at, err)
	}
	if s == nil {
		t.Fatalf("%s: session is nil", at)
	}
	return s
}

func s6Establish(t *testing.T, at string, ctx context.Context, c *Client, deviceID string) *Session {
	t.Helper()
	s, err := EstablishForDevice(ctx, c, deviceID, s6StubHandler())
	return s6RequireSession(t, at, s, err)
}

func s6StubHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"nodes":[],"unadopted":[],"sys":{},"socket":"s6","hostname":"s6","version":"dev"}`))
	})
}

func s6Recipient(t *testing.T) (pub, priv []byte) {
	t.Helper()
	pub, err := hex.DecodeString(s6VectorRecipientPubHex)
	if err != nil {
		t.Fatal(err)
	}
	priv, err = hex.DecodeString(s6VectorRecipientPrivHex)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func s6FingerprintInSDP(fp, sdp string) bool {
	if fp == "" || sdp == "" {
		return false
	}
	lowerSDP := strings.ToLower(sdp)
	if !strings.Contains(lowerSDP, "a=fingerprint") {
		return false
	}
	fields := strings.Fields(fp)
	if len(fields) == 0 {
		return false
	}
	hexPart := fields[len(fields)-1]
	return strings.Contains(strings.ToLower(strings.ReplaceAll(lowerSDP, ":", "")), strings.ToLower(strings.ReplaceAll(hexPart, ":", ""))) ||
		strings.Contains(lowerSDP, strings.ToLower(fp)) ||
		strings.Contains(lowerSDP, strings.ToLower(hexPart))
}

func s6GuidanceHasFallback(g string) bool {
	l := strings.ToLower(g)
	return strings.Contains(l, "ssh") &&
		strings.Contains(l, "wireguard") &&
		strings.Contains(l, "tailscale")
}
