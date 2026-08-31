package remote

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func testBackoffCfg() BackoffConfig {
	return BackoffConfig{
		Initial:    100 * time.Millisecond,
		Max:        1600 * time.Millisecond,
		Factor:     2,
		Jitter:     0.2,
		SuccessFor: 5 * time.Second,
	}
}

// AT-FR-22-a: delays grow exponentially and are jittered; 1000 nodes do not
// reconnect at one instant. Injected clock and RNG; no sleeps.
func TestAT_FR_22_a_ExponentialBackoffJitterSpread(t *testing.T) {
	cfg := testBackoffCfg()
	clock := newClock(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))

	t.Run("exponential-growth", func(t *testing.T) {
		// Midpoint RNG (0.5) is defined as zero jitter, so Delay equals the
		// capped exponential base: min(Max, Initial * Factor^(failures-1)).
		b := NewBackoff(cfg, clock, constRNG{v: 0.5})
		want := []time.Duration{
			100 * time.Millisecond,
			200 * time.Millisecond,
			400 * time.Millisecond,
			800 * time.Millisecond,
			1600 * time.Millisecond,
			1600 * time.Millisecond,
		}
		var delays []time.Duration
		for i := 0; i < len(want); i++ {
			if err := b.Failure(); err != nil {
				t.Fatalf("Failure: %v", err)
			}
			n, err := b.Attempt()
			if err != nil {
				t.Fatalf("Attempt: %v", err)
			}
			if n != i+1 {
				t.Fatalf("Attempt after %d failures = %d, want %d", i+1, n, i+1)
			}
			d, err := b.Delay()
			if err != nil {
				t.Fatalf("Delay: %v", err)
			}
			delays = append(delays, d)
		}
		for i := range want {
			if delays[i] != want[i] {
				t.Fatalf("AT-FR-22-a: delay sequence = %v, want exact capped exponential %v", delays, want)
			}
		}
	})

	t.Run("jitter-bounded", func(t *testing.T) {
		for seed := uint64(1); seed <= 32; seed++ {
			b := NewBackoff(cfg, clock, newSeqRNG(seed))
			if err := b.Failure(); err != nil {
				t.Fatal(err)
			}
			d, err := b.Delay()
			if err != nil {
				t.Fatal(err)
			}
			if d < 0 || d > cfg.Max {
				t.Fatalf("seed %d: delay %s outside [0, max]", seed, d)
			}
			hi := time.Duration(float64(cfg.Initial) * (1 + cfg.Jitter))
			if d > hi && d > cfg.Initial {
				// Full-jitter implementations cap at the base, which is also
				// within max. A delay above Initial*(1+Jitter) on attempt 0
				// exceeds the configured jitter bound.
				if d > cfg.Initial+time.Duration(float64(cfg.Initial)*cfg.Jitter)+time.Millisecond {
					t.Fatalf("seed %d: delay %s exceeds jitter bound around %s", seed, d, cfg.Initial)
				}
			}
		}
	})

	t.Run("thousand-nodes-not-one-instant", func(t *testing.T) {
		seen := map[time.Duration]int{}
		for i := 0; i < 1000; i++ {
			b := NewBackoff(cfg, clock, newSeqRNG(uint64(i+1)))
			if err := b.Failure(); err != nil {
				t.Fatal(err)
			}
			d, err := b.Delay()
			if err != nil {
				t.Fatal(err)
			}
			seen[d]++
		}
		if len(seen) < 2 {
			t.Fatalf("AT-FR-22-a: 1000 nodes with distinct RNG inputs reconnect at one instant (%d unique delays)", len(seen))
		}
		var maxn int
		for _, n := range seen {
			if n > maxn {
				maxn = n
			}
		}
		if maxn == 1000 {
			t.Fatal("AT-FR-22-a: every node shares the same delay")
		}
	})
}

// AT-FR-22-b: a success shorter than SuccessFor does not reset; reaching it does.
func TestAT_FR_22_b_SustainedSuccessResetsBackoff(t *testing.T) {
	cfg := testBackoffCfg()
	clock := newClock(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))
	b := NewBackoff(cfg, clock, constRNG{v: 0.5})
	for i := 0; i < 4; i++ {
		if err := b.Failure(); err != nil {
			t.Fatal(err)
		}
	}
	n, err := b.Attempt()
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("Attempt after 4 failures = %d, want 4", n)
	}
	high, err := b.Delay()
	if err != nil {
		t.Fatal(err)
	}
	if high != 800*time.Millisecond {
		t.Fatalf("precondition: delay after 4 failures = %s, want 800ms base", high)
	}

	if err := b.NotifyUp(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(cfg.SuccessFor - time.Millisecond)
	if err := b.NotifyDown(); err != nil {
		t.Fatal(err)
	}
	n, err = b.Attempt()
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("AT-FR-22-b: short success reset Attempt to %d, want 4", n)
	}
	still, err := b.Delay()
	if err != nil {
		t.Fatal(err)
	}
	if still != high {
		t.Fatalf("AT-FR-22-b: short success changed delay from %s to %s", high, still)
	}

	if err := b.NotifyUp(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(cfg.SuccessFor)
	if err := b.NotifyDown(); err != nil {
		t.Fatal(err)
	}
	n, err = b.Attempt()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("AT-FR-22-b: sustained success Attempt = %d, want 0", n)
	}
	reset, err := b.Delay()
	if err != nil {
		t.Fatal(err)
	}
	if reset != cfg.Initial {
		t.Fatalf("AT-FR-22-b: sustained success delay = %s, want initial %s", reset, cfg.Initial)
	}
}

// AT-FR-24-b: absent peer → typed current-state error; nothing queued or
// cached; bringing the peer back does not deliver the old request.
func TestAT_FR_24_b_AbsentPeerNothingQueuedOrCached(t *testing.T) {
	c, _, d1, _, _ := enrollTwoDevices(t)
	ch := &fakeChannel{}
	ctx, cancel := ctxTO(t)
	defer cancel()
	if err := c.AttachChannel(d1.ID, ch); err != nil {
		t.Fatal(err)
	}
	if err := ch.Close(); err != nil {
		t.Fatal(err)
	}

	old := []byte("old-request")
	err := c.Deliver(ctx, d1.ID, old)
	requireClass(t, err, ClassPeerAbsent)

	if got := ch.Got(); len(got) != 0 {
		t.Fatalf("AT-FR-24-b: queued or delivered while absent: %q", got)
	}
	scanRootForInvite(t, c.cfg.DataDir, string(old))
	st, raw, ok := readStateFile(t, c.StatePath())
	if ok && (bytes.Contains(raw, old) || strings.Contains(string(st.Status), "queued")) {
		t.Fatal("AT-FR-24-b: request persisted for later delivery")
	}

	live := &fakeChannel{}
	if err := c.AttachChannel(d1.ID, live); err != nil {
		t.Fatal(err)
	}
	if got := live.Got(); len(got) != 0 {
		t.Fatalf("AT-FR-24-b: bringing the peer back delivered the old request: %q", got)
	}
	fresh := []byte("new-request")
	if err := c.Deliver(ctx, d1.ID, fresh); err != nil {
		t.Fatalf("AT-FR-24-b: live deliver: %v", err)
	}
	got := live.Got()
	if len(got) != 1 || !bytes.Equal(got[0], fresh) {
		t.Fatalf("AT-FR-24-b: live channel got %q, want exactly [%q]", got, fresh)
	}
	for _, msg := range got {
		if bytes.Equal(msg, old) {
			t.Fatal("AT-FR-24-b: old request delivered after peer returned")
		}
	}
	if got := ch.Got(); len(got) != 0 {
		t.Fatalf("AT-FR-24-b: closed channel received %q", got)
	}

	live.sendErr = errors.New("injected channel send failure")
	failMsg := []byte("should-not-deliver")
	err = c.Deliver(ctx, d1.ID, failMsg)
	if err == nil {
		t.Fatal("AT-FR-24-b: channel send failure treated as successful delivery")
	}
	for _, msg := range live.Got() {
		if bytes.Equal(msg, failMsg) {
			t.Fatal("AT-FR-24-b: failed send still appeared on the live channel")
		}
		if bytes.Equal(msg, old) {
			t.Fatal("AT-FR-24-b: old request delivered after peer returned")
		}
	}
}

// AT-FR-37-b client half: version below minimum refused, never downgraded;
// a higher version is not rewritten to an older one.
func TestAT_FR_37_b_ClientRefusesDowngradeDoesNotRewrite(t *testing.T) {
	t.Run("below-minimum-refused", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
		zero := 0
		min := MinRequestV
		cfg.Version = &zero
		cfg.MinVersion = &min
		ctx, cancel := ctxTO(t)
		defer cancel()
		_, err := startClient(ctx, cfg)
		requireClass(t, err, ClassDowngrade)
		if fake.RequestCount() != 0 {
			t.Fatalf("AT-FR-37-b: below-minimum version was sent: %v", fake.Paths())
		}
		for _, body := range enrollBodies(fake.Requests()) {
			t.Fatalf("AT-FR-37-b: enroll body sent for v=0: %v", body)
		}
	})

	t.Run("higher-version-not-rewritten", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
		two := 2
		cfg.Version = &two
		ctx, cancel := ctxTO(t)
		defer cancel()
		_, err := startClient(ctx, cfg)
		if err != nil {
			t.Fatalf("AT-FR-37-b: v=2: %v", err)
		}
		bodies := enrollBodies(fake.Requests())
		if len(bodies) == 0 {
			t.Fatal("AT-FR-37-b: no enroll request")
		}
		v, _ := bodies[0]["v"]
		switch n := v.(type) {
		case float64:
			if n != 2 {
				t.Fatalf("AT-FR-37-b: rewrote v=2 to %v", n)
			}
		case json.Number:
			if n.String() != "2" {
				t.Fatalf("AT-FR-37-b: rewrote v=2 to %s", n)
			}
		default:
			t.Fatalf("AT-FR-37-b: v field %v (%T)", v, v)
		}
	})
}
