package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// FR-38 over HTTP: the burger's Remote-access list must be renameable.
// The name a row shows today came from the device in its pair-offer, so
// it is a claim; PATCH is how the operator replaces it with something
// they asserted themselves. The hash stays in the row either way, which
// is why a rename can be free-form without becoming a way to impersonate
// another device.
func s7bRenamedDevice(t *testing.T, h http.Handler, id, body string) (int, s7bDeviceJSON) {
	t.Helper()
	rec := routeRequest(h, http.MethodPatch, "/api/remote/devices/"+id, body, true)
	var dev s7bDeviceJSON
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &dev); err != nil {
			t.Fatalf("rename JSON: %v (%q)", err, rec.Body.String())
		}
	}
	return rec.Code, dev
}

func TestAT_S7b_RenameDeviceViaHTTP(t *testing.T) {
	const at = "AT-S7b-rename"
	_, h, c := s7bPairingApp(t)
	minted := s7bMint(t, h)
	_, yPub := s7bMustP256(t)
	s7bOffer(t, c, minted.Code, "phone", "Phone", yPub, bytesRepeat(0x33, 32))
	confirm := routeRequest(h, http.MethodPost, "/api/remote/pairing/"+minted.Code+"/confirm",
		`{"computer_confirm":true,"device_confirm":true}`, true)
	if confirm.Code != http.StatusOK {
		t.Fatalf("%s: confirm status = %d; body=%q", at, confirm.Code, confirm.Body.String())
	}
	var dev s7bDeviceJSON
	if err := json.Unmarshal(confirm.Body.Bytes(), &dev); err != nil {
		t.Fatalf("%s: confirm JSON: %v", at, err)
	}

	code, renamed := s7bRenamedDevice(t, h, dev.ID, `{"label":"my iPhone"}`)
	if code != http.StatusOK {
		t.Fatalf("%s: rename status = %d, want 200", at, code)
	}
	if renamed.Label != "my iPhone" {
		t.Fatalf("%s: renamed label = %q, want %q", at, renamed.Label, "my iPhone")
	}
	if renamed.ID != dev.ID {
		t.Fatalf("%s: rename returned a different device: %+v", at, renamed)
	}

	list := routeRequest(h, http.MethodGet, "/api/remote/devices", "", false)
	var devices s7bDeviceListJSON
	if err := json.Unmarshal(list.Body.Bytes(), &devices); err != nil {
		t.Fatalf("%s: devices JSON: %v", at, err)
	}
	if len(devices.Devices) != 1 || devices.Devices[0].Label != "my iPhone" {
		t.Fatalf("%s: list after rename = %+v, want the new name", at, devices.Devices)
	}

	// A name is not a way to say anything unbounded. The row has to stay
	// a row, and the label reaches the list of things a human revokes from.
	long, over := s7bRenamedDevice(t, h, dev.ID, `{"label":"`+strings.Repeat("x", 4000)+`"}`)
	if long != http.StatusOK {
		t.Fatalf("%s: long rename status = %d, want 200 with a bounded label", at, long)
	}
	if len([]rune(over.Label)) > 64 {
		t.Fatalf("%s: stored label is %d runes, want it bounded", at, len([]rune(over.Label)))
	}
}

func TestAT_S7b_RenameUnknownDeviceIs404(t *testing.T) {
	const at = "AT-S7b-rename-unknown"
	_, h, _ := s7bPairingApp(t)
	code, _ := s7bRenamedDevice(t, h, "nobody", `{"label":"my iPhone"}`)
	if code != http.StatusNotFound {
		t.Fatalf("%s: rename of an unknown device = %d, want 404", at, code)
	}
}

func TestAT_S7b_RenameRejectsAMalformedBody(t *testing.T) {
	const at = "AT-S7b-rename-malformed"
	_, h, _ := s7bPairingApp(t)
	rec := routeRequest(h, http.MethodPatch, "/api/remote/devices/phone", "{not json", true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%s: malformed rename body = %d, want 400", at, rec.Code)
	}
}
