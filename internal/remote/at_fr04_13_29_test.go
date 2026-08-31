package remote

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Distinct, non-empty deterministic device public keys for AT-FR-13-a.
// Revocation must remove these exact values; empty keys would let a
// store that never records device keys pass "key removed".
var (
	testPhonePubKey  = bytes.Repeat([]byte{0x11}, ed25519.PublicKeySize)
	testTabletPubKey = bytes.Repeat([]byte{0x22}, ed25519.PublicKeySize)
)

func enrollTwoDevices(t *testing.T) (*Client, *fakeRV, DeviceRecord, DeviceRecord, Config) {
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
	d1, err := c.RegisterDevice(ctx, DeviceRecord{ID: "phone", PubKey: append([]byte(nil), testPhonePubKey...)})
	if err != nil {
		t.Fatalf("register phone: %v", err)
	}
	d2, err := c.RegisterDevice(ctx, DeviceRecord{ID: "tablet", PubKey: append([]byte(nil), testTabletPubKey...)})
	if err != nil {
		t.Fatalf("register tablet: %v", err)
	}
	return c, fake, d1, d2, cfg
}

func ridBits(t *testing.T, rid string) {
	t.Helper()
	if len(rid) != RendezvousIDHexLen {
		t.Fatalf("rid length = %d, want %d hex chars (256 bits)", len(rid), RendezvousIDHexLen)
	}
	if rid != strings.ToLower(rid) {
		t.Fatalf("rid %q is not canonical lowercase hex", rid)
	}
	raw, err := hex.DecodeString(rid)
	if err != nil {
		t.Fatalf("rid %q: %v", rid, err)
	}
	if len(raw)*8 != RendezvousIDBits {
		t.Fatalf("rid decodes to %d bits, want %d", len(raw)*8, RendezvousIDBits)
	}
}

// AT-FR-04-a: two devices → two distinct 256-bit canonical RIDs.
func TestAT_FR_04_a_TwoDevicesDistinct256BitIDs(t *testing.T) {
	c, _, d1, d2, _ := enrollTwoDevices(t)
	if d1.RID == "" || d2.RID == "" {
		t.Fatalf("empty RID: %q %q", d1.RID, d2.RID)
	}
	if d1.RID == d2.RID {
		t.Fatalf("devices share RID %s", d1.RID)
	}
	ridBits(t, d1.RID)
	ridBits(t, d2.RID)
	list, err := c.Devices()
	if err != nil {
		t.Fatalf("devices: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("devices = %d, want 2", len(list))
	}
}

// AT-FR-13-a: revocation removes device key and RID from persisted state.
func TestAT_FR_13_a_RevokeRemovesKeyAndRID(t *testing.T) {
	c, _, d1, d2, _ := enrollTwoDevices(t)
	ctx, cancel := ctxTO(t)
	defer cancel()

	phoneHex := hex.EncodeToString(testPhonePubKey)
	tabletHex := hex.EncodeToString(testTabletPubKey)
	if phoneHex == "" || tabletHex == "" || phoneHex == tabletHex {
		t.Fatal("AT-FR-13-a: test device public keys must be distinct and non-empty")
	}

	list, err := c.Devices()
	if err != nil {
		t.Fatalf("devices before revoke: %v", err)
	}
	mem1, ok1 := memDevice(list, "phone")
	mem2, ok2 := memDevice(list, "tablet")
	if !ok1 || !ok2 {
		t.Fatalf("AT-FR-13-a: devices before revoke missing phone/tablet: %+v", list)
	}
	if mem1.ID != d1.ID || mem1.RID != d1.RID || !bytes.Equal(mem1.PubKey, testPhonePubKey) {
		t.Fatalf("AT-FR-13-a: phone not stored in memory with expected key and RID: %+v (rid %q key %x)", mem1, d1.RID, testPhonePubKey)
	}
	if mem2.ID != d2.ID || mem2.RID != d2.RID || !bytes.Equal(mem2.PubKey, testTabletPubKey) {
		t.Fatalf("AT-FR-13-a: tablet not stored in memory with expected key and RID: %+v (rid %q key %x)", mem2, d2.RID, testTabletPubKey)
	}

	st := mustState(t, c.StatePath())
	p1, ok1 := persistedDevice(st.Devices, "phone")
	p2, ok2 := persistedDevice(st.Devices, "tablet")
	if !ok1 || !ok2 {
		t.Fatalf("AT-FR-13-a: persisted devices before revoke missing phone/tablet: %+v", st.Devices)
	}
	if p1.ID != d1.ID || p1.RID != d1.RID || p1.PubKey != phoneHex {
		t.Fatalf("AT-FR-13-a: phone not persisted with expected key and RID: %+v", p1)
	}
	if p2.ID != d2.ID || p2.RID != d2.RID || p2.PubKey != tabletHex {
		t.Fatalf("AT-FR-13-a: tablet not persisted with expected key and RID: %+v", p2)
	}
	wantTablet := p2

	if err := c.RevokeDevice(ctx, d1.ID); err != nil {
		t.Fatalf("AT-FR-13-a: %v", err)
	}
	list, err = c.Devices()
	if err != nil {
		t.Fatalf("devices: %v", err)
	}
	foundTablet := false
	for _, d := range list {
		if d.ID == d1.ID || d.RID == d1.RID {
			t.Fatalf("revoked device still present: %+v", d)
		}
		if bytes.Equal(d.PubKey, testPhonePubKey) {
			t.Fatalf("revoked device public key still present: %+v", d)
		}
		if d.ID == d2.ID && d.RID != d2.RID {
			t.Fatal("revoke mutated the other device's RID")
		}
		if d.ID == d2.ID {
			foundTablet = true
			if d.RID != d2.RID || !bytes.Equal(d.PubKey, testTabletPubKey) {
				t.Fatalf("AT-FR-13-a: other device mutated in memory: %+v", d)
			}
		}
	}
	if !foundTablet {
		t.Fatal("AT-FR-13-a: other device missing from memory after revoke")
	}
	st = mustState(t, c.StatePath())
	foundTabletP := false
	for _, d := range st.Devices {
		if d.ID == d1.ID || d.RID == d1.RID || d.PubKey != "" && d.ID == d1.ID {
			t.Fatalf("revoked device still persisted: %+v", d)
		}
		if d.PubKey == phoneHex {
			t.Fatalf("revoked device public key still persisted: %+v", d)
		}
		if d.ID == d2.ID {
			foundTabletP = true
			if d != wantTablet {
				t.Fatalf("AT-FR-13-a: other device persisted record mutated or recreated: got %+v want %+v", d, wantTablet)
			}
		}
	}
	if !foundTabletP {
		t.Fatal("AT-FR-13-a: other device missing from persisted state after revoke")
	}
	_, raw, ok := readStateFile(t, c.StatePath())
	if ok && (bytes.Contains(raw, []byte(phoneHex)) || bytes.Contains(raw, testPhonePubKey)) {
		t.Fatal("AT-FR-13-a: revoked public key still in state file")
	}
}

func memDevice(list []DeviceRecord, id string) (DeviceRecord, bool) {
	for _, d := range list {
		if d.ID == id {
			return d, true
		}
	}
	return DeviceRecord{}, false
}

func persistedDevice(list []PersistedDevice, id string) (PersistedDevice, bool) {
	for _, d := range list {
		if d.ID == id {
			return d, true
		}
	}
	return PersistedDevice{}, false
}

// AT-FR-13-b: revoked device cannot reconnect even when the hub cooperates fully.
func TestAT_FR_13_b_HostileHubCannotReconnectRevokedDevice(t *testing.T) {
	c, fake, d1, d2, _ := enrollTwoDevices(t)
	ch1 := &fakeChannel{}
	ch2 := &fakeChannel{}
	ctx, cancel := ctxTO(t)
	defer cancel()
	if err := c.AttachChannel(d1.ID, ch1); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := c.AttachChannel(d2.ID, ch2); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := c.RevokeDevice(ctx, d1.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	handle, err := c.Handle()
	if err != nil || handle == "" {
		t.Fatalf("handle: %q %v", handle, err)
	}
	fake.SetHostile(true)
	coop := fake.CooperateWait(handle)
	defer coop.Body.Close()
	if coop.StatusCode != http.StatusNoContent && coop.StatusCode != http.StatusOK {
		t.Fatalf("hostile hub did not cooperate for revoked handle: status %d", coop.StatusCode)
	}
	g := newGate()
	errc := make(chan error, 1)
	go func() { errc <- c.Handshake(ctx, d1.ID, g) }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("AT-FR-13-b: revoked device handshake succeeded against a hostile hub")
		}
		if classOf(err) != ClassNotFound && classOf(err) != ClassUnauthorized && classOf(err) != ClassRevoked {
			t.Fatalf("AT-FR-13-b: handshake error %v, want revoked/unknown class", err)
		}
	case <-g.Published:
		t.Fatal("AT-FR-13-b: revoked handshake published a live channel")
	case <-time.After(3 * time.Second):
		t.Fatal("diagnostic timeout: revoked handshake neither failed nor published")
	}
	if err := c.PublishChannel(ctx, d1.ID, &fakeChannel{}, newGate()); err == nil {
		t.Fatal("AT-FR-13-b: published a channel for a revoked device")
	}
	if err := c.BeginRequest(ctx, d1.ID, newGate()); err == nil {
		t.Fatal("AT-FR-13-b: admitted a request from a revoked device")
	}
	if ch2.Closed() {
		t.Fatal("AT-FR-13-b: other device's channel was closed")
	}
	list, _ := c.Devices()
	found := false
	for _, d := range list {
		if d.ID == d2.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("AT-FR-13-b: revoke of phone removed tablet")
	}
}

// AT-FR-13-c: revocation terminates the established channel for that device.
func TestAT_FR_13_c_RevokeTerminatesChannel(t *testing.T) {
	c, _, d1, d2, _ := enrollTwoDevices(t)
	ch1 := &fakeChannel{}
	ch2 := &fakeChannel{}
	ctx, cancel := ctxTO(t)
	defer cancel()
	if err := c.AttachChannel(d1.ID, ch1); err != nil {
		t.Fatal(err)
	}
	if err := c.AttachChannel(d2.ID, ch2); err != nil {
		t.Fatal(err)
	}
	if err := c.RevokeDevice(ctx, d1.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !ch1.Closed() {
		t.Fatal("AT-FR-13-c: revoked device's channel still open")
	}
	if ch2.Closed() {
		t.Fatal("AT-FR-13-c: other device's channel was closed")
	}
}

// AT-FR-13-d: revocation succeeds while the rendezvous is unreachable.
func TestAT_FR_13_d_RevokeWithRendezvousUnreachable(t *testing.T) {
	c, fake, d1, _, _ := enrollTwoDevices(t)
	fake.Close()
	if err := c.Close(); err != nil {
		t.Fatalf("close enrolled client: %v", err)
	}
	c2 := NewClient(Config{
		DataDir:       c.cfg.DataDir,
		Remote:        true,
		Origin:        DefaultOrigin,
		RendezvousURL: fake.URL(),
		HTTPClient:    unreachableClient(),
	})
	if err := c2.Start(t.Context()); err != nil {
		t.Fatalf("reload enrolled client: %v", err)
	}
	ctx, cancel := ctxTO(t)
	defer cancel()
	if err := c2.RevokeDevice(ctx, d1.ID); err != nil {
		t.Fatalf("AT-FR-13-d: revoke with rendezvous down: %v", err)
	}
	list, err := c2.Devices()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range list {
		if d.ID == d1.ID {
			t.Fatal("AT-FR-13-d: device still listed after unreachable revoke")
		}
	}
}

func TestAT_FR_13_PendingStateErased(t *testing.T) {
	c, _, d1, d2, _ := enrollTwoDevices(t)
	ctx, cancel := ctxTO(t)
	defer cancel()
	pending1 := PendingWork{
		Handshake: []byte("hs-phone"),
		Envelope:  []byte("env-phone"),
		Reply:     []byte("rep-phone"),
	}
	pending2 := PendingWork{
		Handshake: []byte("hs-tablet"),
		Envelope:  []byte("env-tablet"),
	}
	if err := c.SetPending(d1.ID, pending1); err != nil {
		t.Fatalf("SetPending phone: %v", err)
	}
	if err := c.SetPending(d2.ID, pending2); err != nil {
		t.Fatalf("SetPending tablet: %v", err)
	}
	got1, err := c.DevicePending(d1.ID)
	if err != nil {
		t.Fatalf("DevicePending phone before revoke: %v", err)
	}
	if got1.Empty() || string(got1.Handshake) != "hs-phone" || string(got1.Envelope) != "env-phone" || string(got1.Reply) != "rep-phone" {
		t.Fatalf("pending phone not established: %+v", got1)
	}
	got2, err := c.DevicePending(d2.ID)
	if err != nil {
		t.Fatalf("DevicePending tablet before revoke: %v", err)
	}
	if got2.Empty() || string(got2.Handshake) != "hs-tablet" {
		t.Fatalf("pending tablet not established: %+v", got2)
	}

	if err := c.RevokeDevice(ctx, d1.ID); err != nil {
		t.Fatal(err)
	}

	erased, err := c.DevicePending(d1.ID)
	if errors.Is(err, ErrUnimplemented) {
		t.Fatal("AT-FR-13: DevicePending after revoke is unimplemented")
	}
	if err == nil && !erased.Empty() {
		t.Fatalf("AT-FR-13: pending state remains for revoked device: %+v", erased)
	}
	if err != nil && classOf(err) != ClassNotFound {
		t.Fatalf("AT-FR-13: revoked DevicePending: %v, want empty or class not-found", err)
	}

	kept, err := c.DevicePending(d2.ID)
	if err != nil {
		t.Fatalf("AT-FR-13: other device pending lookup: %v", err)
	}
	if string(kept.Handshake) != "hs-tablet" || string(kept.Envelope) != "env-tablet" || len(kept.Reply) != 0 {
		t.Fatalf("AT-FR-13: other device pending changed: %+v", kept)
	}
}

// AT-FR-29-a: revoke linearizable against in-flight request and opening handshake.
func TestAT_FR_29_a_RevokeLinearizable(t *testing.T) {
	c, _, d1, _, _ := enrollTwoDevices(t)
	ctx, cancel := ctxTO(t)
	defer cancel()

	reqGate := newGate()
	hsGate := newGate()
	pubGate := newGate()
	reqErr := make(chan error, 1)
	hsErr := make(chan error, 1)
	pubErr := make(chan error, 1)

	go func() { reqErr <- c.BeginRequest(ctx, d1.ID, reqGate) }()
	waitBoundary(t, reqErr, reqGate, "in-flight request")

	go func() { hsErr <- c.Handshake(ctx, d1.ID, hsGate) }()
	waitBoundary(t, hsErr, hsGate, "opening handshake")

	go func() { pubErr <- c.PublishChannel(ctx, d1.ID, &fakeChannel{}, pubGate) }()
	waitBoundary(t, pubErr, pubGate, "channel publication")

	if err := c.RevokeDevice(ctx, d1.ID); err != nil {
		t.Fatalf("AT-FR-29-a: revoke: %v", err)
	}

	close(reqGate.Release)
	close(hsGate.Release)
	close(pubGate.Release)

	if err := waitResult(t, reqErr, "in-flight request"); err == nil {
		t.Fatal("AT-FR-29-a: in-flight request began after revoke returned")
	}
	if err := waitResult(t, hsErr, "handshake"); err == nil {
		t.Fatal("AT-FR-29-a: handshake proceeded after revoke returned")
	}
	if err := waitResult(t, pubErr, "publish"); err == nil {
		t.Fatal("AT-FR-29-a: channel published after revoke returned")
	}

	select {
	case <-pubGate.Published:
		t.Fatal("AT-FR-29-a: publication barrier fired after revoke returned")
	default:
	}

	if err := c.BeginRequest(ctx, d1.ID, newGate()); err == nil {
		t.Fatal("AT-FR-29-a: new request admitted after revoke")
	}
}

func TestAT_FR_29_a_RevokeLinearizableConcurrent(t *testing.T) {
	// Same barriers, started together under -race. No stopwatch races.
	c, _, d1, _, _ := enrollTwoDevices(t)
	ctx, cancel := ctxTO(t)
	defer cancel()
	var wg sync.WaitGroup
	reqGate := newGate()
	hsGate := newGate()
	reqErr := make(chan error, 1)
	hsErr := make(chan error, 1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		reqErr <- c.BeginRequest(ctx, d1.ID, reqGate)
	}()
	go func() {
		defer wg.Done()
		hsErr <- c.Handshake(ctx, d1.ID, hsGate)
	}()
	waitBoundary(t, reqErr, reqGate, "in-flight request")
	waitBoundary(t, hsErr, hsGate, "opening handshake")
	if err := c.RevokeDevice(ctx, d1.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	close(reqGate.Release)
	close(hsGate.Release)
	wg.Wait()
	if err := <-reqErr; err == nil {
		t.Fatal("request succeeded after revoke")
	}
	if err := <-hsErr; err == nil {
		t.Fatal("handshake succeeded after revoke")
	}
}
