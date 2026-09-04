package remote

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// FR-38: the paired-device list is what the human revokes from. It must
// survive a restart of the computer, because a device that cannot be seen
// cannot be revoked — and the identity file has always held the pairings
// that the wait loop polls. Before this test, applyState rehydrated
// c.devices (signalling) and left pairingRuntime.devices (the UI list)
// empty, so /api/remote/devices reported only pairings made since the
// process started.

func fr38Cfg(t *testing.T) Config {
	t.Helper()
	cfg := Config{DataDir: t.TempDir(), Origin: DefaultOrigin}
	return cfg
}

func fr38EnrolledState(t *testing.T, devices []PersistedDevice) PersistedState {
	t.Helper()
	pub, priv := newEd25519(t)
	return PersistedState{
		V:          1,
		Status:     StateEnrolled,
		Handle:     "ih_041061050R3GG28A",
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		Origin:     DefaultOrigin,
		Devices:    devices,
	}
}

func TestFR38_PairedDevicesSurviveRestart(t *testing.T) {
	cfg := fr38Cfg(t)
	rid1 := strings.Repeat("11", 32)
	rid2 := strings.Repeat("22", 32)
	at := "2026-09-04T12:18:55Z"
	y := s7MustP256Pub(t)
	writeState(t, NewClient(cfg), fr38EnrolledState(t, []PersistedDevice{
		{ID: rid1, RID: rid1, ECDHPub: hex.EncodeToString(y), Label: "hotel phone", PairedAt: at},
		{ID: rid2, RID: rid2, ECDHPub: hex.EncodeToString(y)},
	}))

	c := NewClient(cfg)
	if _, err := c.State(); err != nil {
		t.Fatalf("State: %v", err)
	}
	list, err := c.PairedDevices()
	if err != nil {
		t.Fatalf("PairedDevices: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("PairedDevices after restart = %d devices, want 2 — the revoke list is empty", len(list))
	}
	if list[0].ID != rid1 || list[0].RID != rid1 {
		t.Fatalf("device 0 = %+v, want id/rid %s", list[0], rid1)
	}
	if list[0].Label != "hotel phone" {
		t.Errorf("label = %q, want %q", list[0].Label, "hotel phone")
	}
	want, _ := time.Parse(time.RFC3339, at)
	if !list[0].PairedAt.Equal(want) {
		t.Errorf("paired_at = %v, want %v", list[0].PairedAt, want)
	}
	if hex.EncodeToString(list[0].PubKey) != hex.EncodeToString(y) {
		t.Errorf("PubKey = %x, want the persisted ECDH key %x", list[0].PubKey, y)
	}
	if !list[1].PairedAt.IsZero() {
		t.Errorf("device 1 paired_at = %v, want zero for a record written before the field existed", list[1].PairedAt)
	}
}

func TestFR38_AdoptedDevicePersistsLabelAndPairedAt(t *testing.T) {
	cfg := fr38Cfg(t)
	rid := strings.Repeat("33", 32)
	y := s7MustP256Pub(t)
	writeState(t, NewClient(cfg), fr38EnrolledState(t, nil))

	c := NewClient(cfg)
	if _, err := c.State(); err != nil {
		t.Fatalf("State: %v", err)
	}
	at := time.Date(2026, 9, 4, 12, 18, 55, 0, time.UTC)
	if err := c.adoptPairedDevice(DeviceRecord{
		ID: rid, RID: rid, ECDHPub: y, Label: "tablet", PairedAt: at,
	}); err != nil {
		t.Fatalf("adoptPairedDevice: %v", err)
	}

	raw, err := os.ReadFile(c.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	var st PersistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Devices) != 1 {
		t.Fatalf("persisted devices = %d, want 1", len(st.Devices))
	}
	if st.Devices[0].Label != "tablet" {
		t.Errorf("persisted label = %q, want %q", st.Devices[0].Label, "tablet")
	}
	if st.Devices[0].PairedAt != at.Format(time.RFC3339) {
		t.Errorf("persisted paired_at = %q, want %q", st.Devices[0].PairedAt, at.Format(time.RFC3339))
	}

	// A restart sees the same device on the revoke list.
	fresh := NewClient(cfg)
	if _, err := fresh.State(); err != nil {
		t.Fatalf("State: %v", err)
	}
	list, err := fresh.PairedDevices()
	if err != nil {
		t.Fatalf("PairedDevices: %v", err)
	}
	if len(list) != 1 || list[0].Label != "tablet" || !list[0].PairedAt.Equal(at) {
		t.Fatalf("after restart = %+v, want the adopted device with its label and paired-at", list)
	}
}
