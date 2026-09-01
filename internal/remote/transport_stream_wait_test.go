package remote

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// TestDCStreamWaitOpen exercises waitOpen against a channel-only dcStream —
// no pion DataChannel and no real WebRTC session.
func TestDCStreamWaitOpen(t *testing.T) {
	t.Run("already open returns nil", func(t *testing.T) {
		s := &dcStream{
			openCh:  make(chan struct{}),
			closeCh: make(chan struct{}),
			lowCh:   make(chan struct{}, 1),
		}
		close(s.openCh)
		if err := s.waitOpen(context.Background()); err != nil {
			t.Fatalf("waitOpen: %v", err)
		}
	})

	t.Run("closed before open returns ClassHandshake", func(t *testing.T) {
		s := &dcStream{
			openCh:  make(chan struct{}),
			closeCh: make(chan struct{}),
			lowCh:   make(chan struct{}, 1),
		}
		close(s.closeCh)
		err := s.waitOpen(context.Background())
		requireClass(t, err, ClassHandshake)
		if errors.Is(err, context.Canceled) {
			t.Fatal("close-before-open must not report context.Canceled")
		}
	})

	t.Run("canceled context returns ClassHandshake wrapping Canceled", func(t *testing.T) {
		s := &dcStream{
			openCh:  make(chan struct{}),
			closeCh: make(chan struct{}),
			lowCh:   make(chan struct{}, 1),
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := s.waitOpen(ctx)
		requireClass(t, err, ClassHandshake)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("errors.Is(err, context.Canceled) = false; err=%v", err)
		}
	})

	t.Run("close before open never reports successful opening", func(t *testing.T) {
		s := &dcStream{
			openCh:  make(chan struct{}),
			closeCh: make(chan struct{}),
			lowCh:   make(chan struct{}, 1),
		}
		close(s.closeCh)
		if err := s.waitOpen(context.Background()); err == nil {
			t.Fatal("close-before-open returned nil (successful open)")
		}
	})
}

// TestAwaitBufferedDrain pins the drain state machine without a real
// DataChannel or the 30s write timer.
func TestAwaitBufferedDrain(t *testing.T) {
	t.Run("below high water returns immediately", func(t *testing.T) {
		var calls atomic.Uint64
		amount := func() uint64 {
			calls.Add(1)
			return dcHighWater - 1
		}
		if err := awaitBufferedDrain(amount, dcHighWater, nil, nil, nil); err != nil {
			t.Fatalf("awaitBufferedDrain: %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("amount calls = %d, want 1 (fast path)", calls.Load())
		}
	})

	t.Run("exactly at high water returns immediately", func(t *testing.T) {
		amount := func() uint64 { return dcHighWater }
		done := make(chan error, 1)
		go func() {
			done <- awaitBufferedDrain(amount, dcHighWater, nil, nil, nil)
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("awaitBufferedDrain: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("exactly at high water must return immediately (no wait)")
		}
	})

	t.Run("above high water waits then succeeds on low", func(t *testing.T) {
		var cur atomic.Uint64
		cur.Store(dcHighWater + 1)
		lowCh := make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() {
			done <- awaitBufferedDrain(cur.Load, dcHighWater, lowCh, nil, nil)
		}()
		select {
		case err := <-done:
			t.Fatalf("returned before low signal: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		cur.Store(dcHighWater)
		lowCh <- struct{}{}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("after low: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("diagnostic timeout: drain did not finish after low")
		}
	})

	t.Run("stale low while still above keeps waiting", func(t *testing.T) {
		var cur atomic.Uint64
		cur.Store(dcHighWater + 10)
		lowCh := make(chan struct{}, 2)
		done := make(chan error, 1)
		go func() {
			done <- awaitBufferedDrain(cur.Load, dcHighWater, lowCh, nil, nil)
		}()
		lowCh <- struct{}{} // coalesced/stale: amount still above
		select {
		case err := <-done:
			t.Fatalf("returned while still above after stale low: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		cur.Store(dcHighWater - 1)
		lowCh <- struct{}{}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("after real low: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("diagnostic timeout: drain did not finish after amount fell")
		}
	})

	t.Run("close while waiting returns ErrClosedPipe", func(t *testing.T) {
		amount := func() uint64 { return dcHighWater + 1 }
		closeCh := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- awaitBufferedDrain(amount, dcHighWater, nil, closeCh, nil)
		}()
		close(closeCh)
		select {
		case err := <-done:
			if !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("err = %v, want io.ErrClosedPipe", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("diagnostic timeout: close did not unblock drain")
		}
	})

	t.Run("expiry while waiting returns ClassLost", func(t *testing.T) {
		amount := func() uint64 { return dcHighWater + 1 }
		expiry := make(chan time.Time, 1)
		expiry <- time.Time{}
		err := awaitBufferedDrain(amount, dcHighWater, nil, nil, expiry)
		requireClass(t, err, ClassLost)
		if got := guidanceOf(err); got != "the data channel stopped draining" {
			t.Fatalf("guidance = %q", got)
		}
		var e *Error
		if !errors.As(err, &e) || e.Op != "channel" {
			t.Fatalf("op = %q, want channel; err=%v", e.Op, err)
		}
	})
}
