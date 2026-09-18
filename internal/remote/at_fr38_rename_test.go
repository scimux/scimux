package remote

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// FR-38: the paired-device list is what the human revokes from, so it has
// to be a list they can read. The name in it arrives from the device
// itself (the pair-offer's label), which makes it a claim rather than a
// fact: two phones can call themselves the same thing, and a device is
// free to name itself after one the operator trusts. A local rename is
// the answer — the row then says what the operator asserted, over a hash
// the device cannot forge.
//
// Renaming is a label change and nothing else. It must not touch the
// keys, the RID or the paired-at, because those are what signalling and
// sealing read; a rename that dropped ECDHPub would quietly end the
// device's ability to receive a sealed answer.

func TestFR38_RenamePairedDeviceIsDurable(t *testing.T) {
	cfg := fr38Cfg(t)
	rid := strings.Repeat("44", 32)
	y := s7MustP256Pub(t)
	at := "2026-09-04T12:18:55Z"
	writeState(t, NewClient(cfg), fr38EnrolledState(t, []PersistedDevice{
		{ID: rid, RID: rid, ECDHPub: hex.EncodeToString(y), Label: "iPhone", PairedAt: at},
	}))

	c := NewClient(cfg)
	if _, err := c.State(); err != nil {
		t.Fatalf("State: %v", err)
	}
	dev, err := c.RenamePairedDevice(context.Background(), rid, "Test User's Phone")
	if err != nil {
		t.Fatalf("RenamePairedDevice: %v", err)
	}
	if dev.Label != "Test User's Phone" {
		t.Errorf("returned label = %q, want the new name", dev.Label)
	}
	if dev.RID != rid || hex.EncodeToString(dev.PubKey) != hex.EncodeToString(y) {
		t.Errorf("rename changed more than the label: %+v", dev)
	}

	list, err := c.PairedDevices()
	if err != nil {
		t.Fatalf("PairedDevices: %v", err)
	}
	if len(list) != 1 || list[0].Label != "Test User's Phone" {
		t.Fatalf("list after rename = %+v, want the new name", list)
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
	if st.Devices[0].Label != "Test User's Phone" {
		t.Errorf("persisted label = %q, want the new name", st.Devices[0].Label)
	}
	if st.Devices[0].ECDHPub != hex.EncodeToString(y) || st.Devices[0].PairedAt != at {
		t.Errorf("rename disturbed the signalling record: %+v", st.Devices[0])
	}

	fresh := NewClient(cfg)
	if _, err := fresh.State(); err != nil {
		t.Fatalf("State: %v", err)
	}
	after, err := fresh.PairedDevices()
	if err != nil {
		t.Fatalf("PairedDevices: %v", err)
	}
	if len(after) != 1 || after[0].Label != "Test User's Phone" {
		t.Fatalf("after restart = %+v, want the new name", after)
	}
}

// An empty name is a clear, not an error: the row falls back to the
// device's identity, which is the only name that was ever a fact.
func TestFR38_RenameToEmptyClearsTheLabel(t *testing.T) {
	cfg := fr38Cfg(t)
	rid := strings.Repeat("55", 32)
	writeState(t, NewClient(cfg), fr38EnrolledState(t, []PersistedDevice{
		{ID: rid, RID: rid, ECDHPub: hex.EncodeToString(s7MustP256Pub(t)), Label: "iPhone"},
	}))
	c := NewClient(cfg)
	if _, err := c.State(); err != nil {
		t.Fatalf("State: %v", err)
	}
	dev, err := c.RenamePairedDevice(context.Background(), rid, "   ")
	if err != nil {
		t.Fatalf("RenamePairedDevice: %v", err)
	}
	if dev.Label != "" {
		t.Errorf("label = %q, want it cleared", dev.Label)
	}
}

func TestFR38_RenameUnknownDeviceIsNotFound(t *testing.T) {
	cfg := fr38Cfg(t)
	writeState(t, NewClient(cfg), fr38EnrolledState(t, nil))
	c := NewClient(cfg)
	if _, err := c.State(); err != nil {
		t.Fatalf("State: %v", err)
	}
	_, err := c.RenamePairedDevice(context.Background(), "nobody", "whatever")
	requireClass(t, err, ClassNotFound)
}

// Every label this computer stores is bounded, whoever supplied it. The
// operator's own typing is bounded because a row has to stay a row; the
// device's is bounded because the device is not trusted to be reasonable.
func TestFR38_DeviceLabelsAreBounded(t *testing.T) {
	cfg := fr38Cfg(t)
	rid := strings.Repeat("66", 32)
	writeState(t, NewClient(cfg), fr38EnrolledState(t, []PersistedDevice{
		{ID: rid, RID: rid, ECDHPub: hex.EncodeToString(s7MustP256Pub(t))},
	}))
	c := NewClient(cfg)
	if _, err := c.State(); err != nil {
		t.Fatalf("State: %v", err)
	}
	dev, err := c.RenamePairedDevice(context.Background(), rid, strings.Repeat("e", 400)+"\ntail")
	if err != nil {
		t.Fatalf("RenamePairedDevice: %v", err)
	}
	if n := len([]rune(dev.Label)); n > maxDeviceLabel {
		t.Errorf("stored label is %d runes, want at most %d", n, maxDeviceLabel)
	}
	if strings.ContainsAny(dev.Label, "\n\r\t") {
		t.Errorf("stored label kept control characters: %q", dev.Label)
	}
}

func TestFR38_OfferSuppliedLabelIsBounded(t *testing.T) {
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()
	dev := s7cPair(t, c, ctx, "phone", strings.Repeat("A", 400)+" ")
	if n := len([]rune(dev.Label)); n > maxDeviceLabel {
		t.Errorf("offer label stored as %d runes, want at most %d", n, maxDeviceLabel)
	}
	if strings.HasSuffix(dev.Label, " ") {
		t.Errorf("offer label kept its trailing space: %q", dev.Label)
	}
}
