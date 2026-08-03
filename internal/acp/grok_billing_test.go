package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseGrokBilling(t *testing.T) {
	raw := []byte(`{
		"config": {
			"creditUsagePercent": 14.0,
			"currentPeriod": {
				"type": "USAGE_PERIOD_TYPE_WEEKLY",
				"start": "2026-07-29T00:00:00+00:00",
				"end": "2026-08-05T00:00:00+00:00"
			},
			"billingPeriodEnd": "2026-08-05T00:00:00+00:00"
		},
		"subscription_tier": "SuperGrok Lite"
	}`)
	u, err := ParseGrokBilling(raw)
	if err != nil {
		t.Fatalf("ParseGrokBilling: %v", err)
	}
	if u.Plan != "SuperGrok Lite" || u.Source != "grok-acp-billing" {
		t.Fatalf("identity = %+v", u)
	}
	if u.CreditUsagePercent != 14 {
		t.Fatalf("CreditUsagePercent = %v, want 14", u.CreditUsagePercent)
	}
	wantReset, _ := time.Parse(time.RFC3339, "2026-08-05T00:00:00+00:00")
	if u.WeeklyReset == nil || !u.WeeklyReset.Equal(wantReset) {
		t.Fatalf("WeeklyReset = %v, want %v", u.WeeklyReset, wantReset)
	}
	// Clamp used > 100.
	u2, err := ParseGrokBilling([]byte(`{"config":{"creditUsagePercent":150},"subscription_tier":"x"}`))
	if err != nil || u2.CreditUsagePercent != 100 {
		t.Fatalf("clamp high: u=%+v err=%v", u2, err)
	}
	// Clamp used < 0.
	u3, err := ParseGrokBilling([]byte(`{"config":{"creditUsagePercent":-5},"subscription_tier":"x"}`))
	if err != nil || u3.CreditUsagePercent != 0 {
		t.Fatalf("clamp low: u=%+v err=%v", u3, err)
	}
	// Prefer currentPeriod.end over billingPeriodEnd.
	u4, err := ParseGrokBilling([]byte(`{
		"config": {
			"creditUsagePercent": 1,
			"currentPeriod": {"end": "2026-08-01T12:00:00Z"},
			"billingPeriodEnd": "2026-09-01T00:00:00Z"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := time.Parse(time.RFC3339, "2026-08-01T12:00:00Z")
	if u4.WeeklyReset == nil || !u4.WeeklyReset.Equal(want) {
		t.Fatalf("prefer period end: %v", u4.WeeklyReset)
	}
	// Missing percent is unavailable.
	if _, err := ParseGrokBilling([]byte(`{"config":{},"subscription_tier":"x"}`)); err == nil {
		t.Fatal("expected unavailable for missing percent")
	}
	if _, err := ParseGrokBilling(nil); err == nil {
		t.Fatal("expected unavailable for empty body")
	}
	if _, err := ParseGrokBilling([]byte(`not json`)); err == nil {
		t.Fatal("expected unavailable for malformed body")
	}
}

func TestQueryGrokBillingInjected(t *testing.T) {
	raw := json.RawMessage(`{"config":{"creditUsagePercent":25.5,"currentPeriod":{"end":"2026-08-01T12:00:00Z"}},"subscription_tier":"SuperGrok"}`)
	u, err := QueryGrokBilling(context.Background(), GrokBillingOptions{
		Query: func(ctx context.Context) (json.RawMessage, error) { return raw, nil },
	})
	if err != nil {
		t.Fatalf("QueryGrokBilling: %v", err)
	}
	if u.CreditUsagePercent != 25.5 {
		t.Fatalf("used = %v, want 25.5", u.CreditUsagePercent)
	}
	if u.Plan != "SuperGrok" {
		t.Fatalf("plan = %q", u.Plan)
	}
	// Injected errors are sanitized just like failures from the live process.
	_, err = QueryGrokBilling(context.Background(), GrokBillingOptions{
		Query: func(ctx context.Context) (json.RawMessage, error) {
			return nil, errors.New("secret account detail")
		},
	})
	if err == nil || err.Error() != "usage unavailable" {
		t.Fatalf("error = %v, want generic usage unavailable", err)
	}
	// Timeout path: query respects context cancellation by returning an error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = QueryGrokBilling(ctx, GrokBillingOptions{
		Query: func(ctx context.Context) (json.RawMessage, error) {
			if err := ctx.Err(); err != nil {
				return nil, errors.New("usage unavailable")
			}
			return nil, errors.New("should not run")
		},
	})
	if err == nil || err.Error() != "usage unavailable" {
		t.Fatalf("cancelled error = %v, want generic usage unavailable", err)
	}
}

func writeGrokBillingStub(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "grok")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestQueryGrokBillingCLIWire(t *testing.T) {
	writeGrokBillingStub(t, `
if [ "$1" != "--no-auto-update" ] || [ "$2" != "agent" ] || [ "$3" != "stdio" ]; then exit 2; fi
IFS= read -r init || exit 3
case "$init" in *'"id":1'*'"method":"initialize"'*) ;; *) exit 4 ;; esac
printf '%s\n' '{"jsonrpc":"2.0","method":"session/update","params":{}}'
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
IFS= read -r billing || exit 5
case "$billing" in *'"id":2'*'"method":"_x.ai/billing"'*) ;; *) exit 6 ;; esac
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"config":{"creditUsagePercent":12.5,"currentPeriod":{"end":"2026-08-05T00:00:00Z"}},"subscription_tier":"Stub"}}'
`)
	u, err := QueryGrokBilling(context.Background(), GrokBillingOptions{})
	if err != nil {
		t.Fatalf("QueryGrokBilling CLI: %v", err)
	}
	if u.Plan != "Stub" || u.CreditUsagePercent != 12.5 {
		t.Fatalf("billing = %+v", u)
	}
}

func TestQueryGrokBillingCLITimeout(t *testing.T) {
	writeGrokBillingStub(t, `exec /bin/sleep 5`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := QueryGrokBilling(ctx, GrokBillingOptions{})
	if err == nil || err.Error() != "usage unavailable" {
		t.Fatalf("error = %v, want generic usage unavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}
}
