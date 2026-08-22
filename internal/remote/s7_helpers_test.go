package remote

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func s7Enrolled(t *testing.T) (*Client, *fakeClock, context.Context, context.CancelFunc) {
	t.Helper()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	clk := newClock(time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC))
	cfg.Clock = clk
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	ctx, cancel := ctxTO(t)
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	return c, clk, ctx, cancel
}

func s7Construction(t *testing.T, id string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "vectors", "constructions.json"))
	if err != nil {
		t.Fatalf("S1 constructions.json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("constructions.json: %v", err)
	}
	for _, row := range rows {
		if row["id"] == id {
			return row
		}
	}
	t.Fatalf("vector %q missing from S1 constructions.json", id)
	return nil
}

func s7Hex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func s7PubFromPriv(t *testing.T, priv []byte) []byte {
	t.Helper()
	k, err := ecdh.P256().NewPrivateKey(priv)
	if err != nil {
		t.Fatalf("P-256 private key: %v", err)
	}
	return k.PublicKey().Bytes()
}

func s7MustP256(t *testing.T) (priv, pub []byte) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k.Bytes(), k.PublicKey().Bytes()
}
