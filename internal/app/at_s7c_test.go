package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/remote"
)

// TestAT_S7c_HTTPPairingRegistersLiveDevice: a completed pairing must
// become a DeviceRecord the wait loop will poll, under the same RID.
func TestAT_S7c_HTTPPairingRegistersLiveDevice(t *testing.T) {
	const at = "AT-S7c-pairing-live-device"
	_, h, c := s7bPairingApp(t)

	minted := s7bMint(t, h)
	_, yPub := s7bMustP256(t)
	signPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AcceptPairingOffer(context.Background(), remote.PairingOffer{
		Code:       minted.Code,
		DeviceID:   "phone",
		Label:      "Phone",
		DevicePub:  yPub,
		SignPub:    signPub,
		OfferNonce: bytesRepeat(0x41, 32),
	}); err != nil {
		t.Fatalf("%s: AcceptPairingOffer: %v", at, err)
	}

	confirm := routeRequest(h, http.MethodPost, "/api/remote/pairing/"+minted.Code+"/confirm",
		`{"computer_confirm":true,"device_confirm":true}`, true)
	if confirm.Code != http.StatusOK {
		t.Fatalf("%s: both-sides confirm status = %d, want 200; body=%q", at, confirm.Code, confirm.Body.String())
	}

	list, err := c.Devices()
	if err != nil {
		t.Fatalf("%s: Devices: %v", at, err)
	}
	var found *remote.DeviceRecord
	for i := range list {
		if list[i].RID == minted.RID {
			found = &list[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("%s: paired RID %s is not in the client's device registry: %+v", at, minted.RID, list)
	}
	if found.ID == "" {
		t.Fatalf("%s: live device for RID %s has empty id", at, minted.RID)
	}
	if !bytes.Equal(found.PubKey, signPub) {
		t.Fatalf("%s: live device PubKey does not match the offered ed25519 identity", at)
	}
}

// The API reference must stay public and say who asserts device_confirm.
func TestAT_S7c_PairingAPIDocTrackedAndNamesWhoConfirms(t *testing.T) {
	const at = "AT-S7c-api-doc"
	root := repoRootFromTest(t)
	path := filepath.Join(root, "docs", "http-api.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: read %s: %v", at, path, err)
	}
	// Prose may wrap across lines without changing the documented contract.
	doc := strings.Join(strings.Fields(string(raw)), " ")
	for _, phrase := range []string{
		"operator asserts the device's half of the confirmation",
		"server cannot tell",
	} {
		if !strings.Contains(doc, phrase) {
			t.Fatalf("%s: %s is missing %q; FR-12 is never a password, never auto-confirmed — the doc must not imply the server verified device_confirm", at, path, phrase)
		}
	}

	gi, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("%s: read .gitignore: %v", at, err)
	}
	allow := "!docs/http-api.md"
	foundAllow := false
	for _, line := range strings.Split(string(gi), "\n") {
		if strings.TrimSpace(line) == allow {
			foundAllow = true
			break
		}
	}
	if !foundAllow {
		t.Fatalf("%s: .gitignore missing allowlist line %q", at, allow)
	}

	if _, err := exec.LookPath("git"); err == nil {
		chk := exec.Command("git", "check-ignore", "--no-index", "-q", "docs/http-api.md")
		chk.Dir = root
		if err := chk.Run(); err == nil {
			t.Fatalf("%s: docs/http-api.md is matched by a .gitignore pattern", at)
		}
	}
}
