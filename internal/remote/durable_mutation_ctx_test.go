package remote

// Phase 4 (F-10) — the four durable local mutations accept a context and
// deliberately do not honor it. This file is the executable half of that
// contract: a comment saying "cancellation is ignored" is a claim, and a
// revoke that a cancelled caller could suppress would be a security bug, not
// a style question.
//
// The mechanism is why it is safe. withStateLock takes the state flock with
// LOCK_EX|LOCK_NB (lock.go), so a contended lock returns ClassStateLock
// immediately rather than waiting; the body then mutates memory and writes
// one atomic file. There is nothing to cancel, which is what makes the
// contract free to keep.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
)

var testLaptopPubKey = bytes.Repeat([]byte{0x33}, ed25519.PublicKeySize)

func cancelledCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ctx.Err() == nil {
		t.Fatal("context is not cancelled")
	}
	return ctx
}

// TestDurableLocalMutationsIgnoreCancellation drives all four in one client so
// the ordering is realistic: register, revoke, disable, re-enable — each with
// a context that was already dead before the call.
func TestDurableLocalMutationsIgnoreCancellation(t *testing.T) {
	c, _, d1, _, _ := enrollTwoDevices(t)
	dead := cancelledCtx(t)

	// RegisterDevice. A caller that gave up waiting must not leave a device
	// half-paired: either the record is durable or the call failed.
	laptop, err := c.RegisterDevice(dead, DeviceRecord{ID: "laptop", PubKey: append([]byte(nil), testLaptopPubKey...)})
	if err != nil {
		t.Fatalf("RegisterDevice with a cancelled context: %v", err)
	}
	if laptop.RID == "" {
		t.Fatal("RegisterDevice minted no RID")
	}
	if _, ok := persistedDevice(mustState(t, c.StatePath()).Devices, "laptop"); !ok {
		t.Fatal("RegisterDevice did not persist the device")
	}

	// RevokeDevice is FR-13/FR-29: a durable local revoke needs no rendezvous,
	// and must not need a live context either. This is the row that matters —
	// honoring ctx here would mean a cancelled caller silently leaves a
	// revoked device paired.
	if err := c.RevokeDevice(dead, d1.ID); err != nil {
		t.Fatalf("RevokeDevice with a cancelled context: %v", err)
	}
	if _, ok := persistedDevice(mustState(t, c.StatePath()).Devices, d1.ID); ok {
		t.Fatalf("RevokeDevice left %s in persisted state", d1.ID)
	}

	// DisableAll is FR-32, the kill switch. Same argument, more so.
	if err := c.DisableAll(dead); err != nil {
		t.Fatalf("DisableAll with a cancelled context: %v", err)
	}
	if got := mustState(t, c.StatePath()).Status; got != StateDisabled {
		t.Fatalf("after DisableAll persisted status = %q, want %q", got, StateDisabled)
	}

	// Reenable is the explicit reversal. It is symmetric by design: the
	// operator's intent, not their context, decides.
	if err := c.Reenable(dead); err != nil {
		t.Fatalf("Reenable with a cancelled context: %v", err)
	}
	if got := mustState(t, c.StatePath()).Status; got == StateDisabled {
		t.Fatal("Reenable left the persisted status disabled")
	}
}
