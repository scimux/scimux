package remote

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// loadedEnrolledWithDevice builds the smallest already-loaded Client that
// can authorize BeginRequest: enrolled state, one device, no rendezvous
// server and no WebRTC session.
func loadedEnrolledWithDevice(t *testing.T) (*Client, string) {
	t.Helper()
	cfg := Config{
		DataDir: t.TempDir(),
		Origin:  DefaultOrigin,
		Stdout:  nil,
		Stderr:  nil,
	}
	c := NewClient(cfg)
	st := enrolledFixture(t, "ih_041061050R3GG28A")
	st.Devices = []PersistedDevice{{
		ID:     "phone",
		RID:    strings.Repeat("ab", RendezvousIDHexLen/2),
		PubKey: hex.EncodeToString(testPhonePubKey),
	}}
	if err := c.applyState(st); err != nil {
		t.Fatalf("applyState: %v", err)
	}
	return c, "phone"
}

// TestBeginRequestAdmission pins the authorization gate and OnAdmission
// instrumentation contract. It does not invent an active-request registry.
func TestBeginRequestAdmission(t *testing.T) {
	t.Run("authorized device succeeds and calls OnAdmission once", func(t *testing.T) {
		c, id := loadedEnrolledWithDevice(t)
		var calls atomic.Int32
		var gotID atomic.Value
		c.cfg.Hooks = Hooks{OnAdmission: func(deviceID string) {
			calls.Add(1)
			gotID.Store(deviceID)
		}}
		if err := c.BeginRequest(context.Background(), id, nil); err != nil {
			t.Fatalf("BeginRequest: %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("OnAdmission calls = %d, want 1", calls.Load())
		}
		if gotID.Load() != id {
			t.Fatalf("OnAdmission device = %v, want %s", gotID.Load(), id)
		}
	})

	t.Run("nil OnAdmission succeeds", func(t *testing.T) {
		c, id := loadedEnrolledWithDevice(t)
		c.cfg.Hooks = Hooks{}
		if err := c.BeginRequest(context.Background(), id, nil); err != nil {
			t.Fatalf("BeginRequest: %v", err)
		}
	})

	t.Run("nil context uses background", func(t *testing.T) {
		c, id := loadedEnrolledWithDevice(t)
		var calls atomic.Int32
		c.cfg.Hooks = Hooks{OnAdmission: func(string) { calls.Add(1) }}
		if err := c.BeginRequest(nil, id, nil); err != nil {
			t.Fatalf("BeginRequest(nil ctx): %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("OnAdmission calls = %d, want 1", calls.Load())
		}
	})

	t.Run("Arrived before admission; hook only after Release", func(t *testing.T) {
		c, id := loadedEnrolledWithDevice(t)
		var calls atomic.Int32
		c.cfg.Hooks = Hooks{OnAdmission: func(string) { calls.Add(1) }}
		gate := newGate()
		errc := make(chan error, 1)
		go func() { errc <- c.BeginRequest(context.Background(), id, gate) }()

		waitBoundary(t, errc, gate, "BeginRequest")
		if calls.Load() != 0 {
			t.Fatal("OnAdmission called before Gate.Release")
		}

		close(gate.Release)
		if err := waitResult(t, errc, "BeginRequest"); err != nil {
			t.Fatalf("BeginRequest after release: %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("OnAdmission calls = %d, want 1 after release", calls.Load())
		}
	})

	t.Run("cancel after Arrived before Release never calls hook", func(t *testing.T) {
		c, id := loadedEnrolledWithDevice(t)
		var calls atomic.Int32
		c.cfg.Hooks = Hooks{OnAdmission: func(string) { calls.Add(1) }}
		gate := newGate()
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- c.BeginRequest(ctx, id, gate) }()

		waitBoundary(t, errc, gate, "BeginRequest")
		cancel()
		err := waitResult(t, errc, "BeginRequest")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		// OnAdmission is synchronous inside BeginRequest. Once BeginRequest
		// returned, no later callback can be pending.
		if calls.Load() != 0 {
			t.Fatalf("OnAdmission calls = %d after cancel-before-release, want 0", calls.Load())
		}
	})

	t.Run("unknown device never calls hook", func(t *testing.T) {
		c, _ := loadedEnrolledWithDevice(t)
		var calls atomic.Int32
		c.cfg.Hooks = Hooks{OnAdmission: func(string) { calls.Add(1) }}
		err := c.BeginRequest(context.Background(), "no-such-device", nil)
		requireClass(t, err, ClassNotFound)
		if calls.Load() != 0 {
			t.Fatalf("OnAdmission calls = %d for unknown device, want 0", calls.Load())
		}
	})
}
