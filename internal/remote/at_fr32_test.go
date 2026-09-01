package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"
)

func enrollMany(t *testing.T, n int) (*Client, *fakeRV, []DeviceRecord, []*fakeChannel, []*fakeWaiter) {
	t.Helper()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	ctx, cancel := ctxTO(t)
	t.Cleanup(cancel)
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	var devices []DeviceRecord
	var chans []*fakeChannel
	var waits []*fakeWaiter
	for i := 0; i < n; i++ {
		d, err := c.RegisterDevice(ctx, DeviceRecord{
			ID:     "dev-" + string(rune('a'+i)),
			PubKey: bytes.Repeat([]byte{byte(0xa1 + i)}, ed25519.PublicKeySize),
		})
		if err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
		devices = append(devices, d)
		ch := &fakeChannel{}
		if err := c.AttachChannel(d.ID, ch); err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
		chans = append(chans, ch)
		w := &fakeWaiter{id: d.RID}
		if err := c.AddWaiter(w); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
		waits = append(waits, w)
	}
	return c, fake, devices, chans, waits
}

// AT-FR-32-a: disable-all terminates every live channel and deregisters every
// wait/ID. Agents and localhost are asserted from internal/app.
func TestAT_FR_32_a_DisableAllClosesChannelsAndWaits(t *testing.T) {
	c, fake, devices, chans, waits := enrollMany(t, 3)
	ctx, cancel := ctxTO(t)
	defer cancel()
	hooks := &countHooks{}
	c.cfg.Hooks = hooks.hooks()

	if err := c.DisableAll(ctx); err != nil {
		t.Fatalf("AT-FR-32-a: %v", err)
	}
	for i, ch := range chans {
		if !ch.Closed() {
			t.Fatalf("AT-FR-32-a: channel %d still live", i)
		}
	}
	for i, w := range waits {
		if !w.Cancelled() {
			t.Fatalf("AT-FR-32-a: waiter %d still registered", i)
		}
	}
	left, err := c.Waiters()
	if err != nil {
		t.Fatalf("AT-FR-32-a: Waiters after disable-all: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("AT-FR-32-a: waiters remain: %d", len(left))
	}
	for _, d := range devices {
		if err := c.BeginRequest(ctx, d.ID, newGate()); err == nil {
			t.Fatalf("AT-FR-32-a: admission still open for %s", d.ID)
		} else {
			requireClass(t, err, ClassDisabled)
		}
		pending, err := c.DevicePending(d.ID)
		if errors.Is(err, ErrUnimplemented) {
			t.Fatalf("AT-FR-32-a: DevicePending unimplemented for %s", d.ID)
		}
		if err != nil && classOf(err) != ClassNotFound && classOf(err) != ClassDisabled {
			t.Fatalf("AT-FR-32-a: DevicePending %s: %v", d.ID, err)
		}
		if err == nil && !pending.Empty() {
			t.Fatalf("AT-FR-32-a: pending remote work remains for %s: %+v", d.ID, pending)
		}
	}
	nreq := fake.RequestCount()
	_ = c.BeginRequest(ctx, devices[0].ID, newGate())
	if fake.RequestCount() != nreq {
		t.Fatal("AT-FR-32-a: rendezvous client still sending after disable-all")
	}
	st, err := c.State()
	if err != nil || st != StateDisabled {
		t.Fatalf("AT-FR-32-a: persisted state = %q %v", st, err)
	}
	assertCompleteValidState(t, mustState(t, c.StatePath()))
	if hooks.agentTouch != 0 {
		t.Fatal("AT-FR-32-a: disable-all touched agent processes")
	}
}

// AT-FR-32-b: disable-all linearizable against in-flight request and handshake.
func TestAT_FR_32_b_DisableAllLinearizable(t *testing.T) {
	c, _, devices, _, _ := enrollMany(t, 2)
	ctx, cancel := ctxTO(t)
	defer cancel()
	d1, d2 := devices[0], devices[1]

	reqGate := newGate()
	hsGate := newGate()
	pubGate := newGate()
	reqErr := make(chan error, 1)
	hsErr := make(chan error, 1)
	pubErr := make(chan error, 1)

	go func() { reqErr <- c.BeginRequest(ctx, d1.ID, reqGate) }()
	waitBoundary(t, reqErr, reqGate, "in-flight request")
	go func() { hsErr <- c.Handshake(ctx, d2.ID, hsGate) }()
	waitBoundary(t, hsErr, hsGate, "opening handshake")
	go func() { pubErr <- c.PublishChannel(ctx, d1.ID, &fakeChannel{}, pubGate) }()
	waitBoundary(t, pubErr, pubGate, "channel publication")

	if err := c.DisableAll(ctx); err != nil {
		t.Fatalf("AT-FR-32-b: %v", err)
	}
	close(reqGate.Release)
	close(hsGate.Release)
	close(pubGate.Release)

	if err := waitResult(t, reqErr, "in-flight request"); err == nil {
		t.Fatal("AT-FR-32-b: in-flight request began after disable-all returned")
	}
	if err := waitResult(t, hsErr, "handshake"); err == nil {
		t.Fatal("AT-FR-32-b: handshake proceeded after disable-all returned")
	}
	if err := waitResult(t, pubErr, "publish"); err == nil {
		t.Fatal("AT-FR-32-b: channel published after disable-all returned")
	}
	select {
	case <-pubGate.Published:
		t.Fatal("AT-FR-32-b: publication barrier fired after disable-all returned")
	default:
	}
	if err := c.BeginRequest(ctx, d1.ID, newGate()); err == nil {
		t.Fatal("AT-FR-32-b: new request admitted after disable-all")
	}
	if err := c.Handshake(ctx, d2.ID, newGate()); err == nil {
		t.Fatal("AT-FR-32-b: new handshake admitted after disable-all")
	}
}

func TestPublishChannelSignalsAfterSuccessfulPublication(t *testing.T) {
	c, _, devices, _, _ := enrollMany(t, 1)
	gate := newGate()
	close(gate.Release)
	want := &fakeChannel{}
	if err := c.PublishChannel(context.Background(), devices[0].ID, want, gate); err != nil {
		t.Fatalf("publish channel: %v", err)
	}
	select {
	case <-gate.Published:
	default:
		t.Fatal("successful publication did not signal its barrier")
	}
	got, err := c.Channel(devices[0].ID)
	if err != nil {
		t.Fatalf("published channel lookup: %v", err)
	}
	if got != want {
		t.Fatalf("published channel = %p, want %p", got, want)
	}
	// Production does not pass a test gate. Its success path must still publish
	// the channel rather than treating the absent instrumentation as a barrier.
	next := &fakeChannel{}
	if err := c.PublishChannel(context.Background(), devices[0].ID, next, nil); err != nil {
		t.Fatalf("publish channel without gate: %v", err)
	}
	got, err = c.Channel(devices[0].ID)
	if err != nil || got != next {
		t.Fatalf("ungated published channel = %p, %v; want %p", got, err, next)
	}
	if _, err := c.Channel("missing-device"); classOfErr(err) != ClassNotFound {
		t.Fatalf("missing channel class = %q, want %q", classOfErr(err), ClassNotFound)
	}
}

// AT-FR-32-c: disable-all succeeds with the rendezvous unreachable, no restart.
func TestAT_FR_32_c_DisableAllUnreachableNoRestart(t *testing.T) {
	c, fake, devices, chans, waits := enrollMany(t, 2)
	fake.Close()
	c.cfg.HTTPClient = unreachableClient()
	ctx, cancel := ctxTO(t)
	defer cancel()
	if err := c.DisableAll(ctx); err != nil {
		t.Fatalf("AT-FR-32-c: %v", err)
	}
	for _, ch := range chans {
		if !ch.Closed() {
			t.Fatal("AT-FR-32-c: channel still live")
		}
	}
	for _, w := range waits {
		if !w.Cancelled() {
			t.Fatal("AT-FR-32-c: waiter still live")
		}
	}
	if err := c.BeginRequest(ctx, devices[0].ID, newGate()); err == nil {
		t.Fatal("AT-FR-32-c: admission still open")
	}
	st, err := c.State()
	if err != nil || st != StateDisabled {
		t.Fatalf("AT-FR-32-c: state = %q %v", st, err)
	}
}

// AT-FR-32-d: disabled state survives restart; --remote does not re-enable.
func TestAT_FR_32_d_DisabledSurvivesRestart(t *testing.T) {
	c, fake, _, _, _ := enrollMany(t, 2)
	ctx, cancel := ctxTO(t)
	defer cancel()
	if err := c.DisableAll(ctx); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close disabled client: %v", err)
	}
	data := c.cfg.DataDir
	c2 := NewClient(Config{
		DataDir:       data,
		Remote:        true,
		Origin:        DefaultOrigin,
		RendezvousURL: fake.URL(),
		HTTPClient:    fake.Client(),
	})
	err := c2.Start(ctx)
	requireClass(t, err, ClassDisabled)
	st, err := c2.State()
	if err != nil {
		t.Fatalf("AT-FR-32-d: State after disabled restart: %v", err)
	}
	if st != StateDisabled {
		t.Fatalf("AT-FR-32-d: restart state = %q, want disabled", st)
	}
	if err := c2.Start(ctx); err == nil || classOf(err) != ClassDisabled {
		t.Fatalf("AT-FR-32-d: second --remote re-enabled: %v", err)
	}
	if err := c2.Reenable(ctx); err != nil {
		t.Fatalf("AT-FR-32-d: explicit reenable: %v", err)
	}
	if err := c2.Start(ctx); err != nil {
		t.Fatalf("AT-FR-32-d: start after reenable: %v", err)
	}
	st, err = c2.State()
	if err != nil {
		t.Fatalf("AT-FR-32-d: State after reenable: %v", err)
	}
	if st != StateEnrolled {
		t.Fatalf("AT-FR-32-d: after reenable state = %q, want enrolled", st)
	}
}

func TestAT_FR_32_a_DisableAllIdempotentOnEmpty(t *testing.T) {
	c, _, _, _, _ := enrollMany(t, 1)
	ctx, cancel := ctxTO(t)
	defer cancel()
	if err := c.DisableAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.DisableAll(ctx); err != nil {
		t.Fatalf("second disable-all: %v", err)
	}
}

func TestAT_FR_32_b_DisableAllDoesNotHang(t *testing.T) {
	c, _, devices, _, _ := enrollMany(t, 1)
	ctx, cancel := ctxTO(t)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.DisableAll(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("disable-all: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("diagnostic timeout: disable-all hung")
	}
	_ = devices
}
