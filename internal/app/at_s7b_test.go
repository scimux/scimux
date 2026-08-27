package app

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"codeberg.org/chrberger/scimux/internal/remote"
)

const s1PairingSAS = "706990"
const s1PairingXPub = "046a8620064ea5a5629fefc1762c3b84a2aba64bfacdbd50a5719f9383bb2bc8f488c9c7c4586f5c5d0523829105d90409ba6b56c208b69add3182430b9120a354"

func s7bPairingApp(t *testing.T) (*app, http.Handler, *remote.Client) {
	t.Helper()
	a := newTestApp(t, &fakeTmux{})
	c := remote.NewClient(remote.Config{
		DataDir: t.TempDir(),
		Remote:  true,
		Origin:  remote.DefaultOrigin,
	})
	a.hostedPairing = c
	return a, newTestHandler(t, a), c
}

func s7bMustP256(t *testing.T) (priv, pub []byte) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k.Bytes(), k.PublicKey().Bytes()
}

// s7bDeviceSAS is protocol §11 computed from the device's private key and
// the computer's live public key. Inlined so this AT is not a caller of
// DerivePairingSAS (that was S7's defect: the primitive was tested, the
// product path was not).
func s7bDeviceSAS(t *testing.T, yPriv, xPub, transcript []byte) string {
	t.Helper()
	curve := ecdh.P256()
	priv, err := curve.NewPrivateKey(yPriv)
	if err != nil {
		t.Fatalf("device SAS: private key: %v", err)
	}
	pub, err := curve.NewPublicKey(xPub)
	if err != nil {
		t.Fatalf("device SAS: computer pub: %v", err)
	}
	shared, err := priv.ECDH(pub)
	if err != nil {
		t.Fatalf("device SAS: ECDH: %v", err)
	}
	key, err := hkdf.Key(sha256.New, shared, []byte("scimux-rv/sas/v1"), string(transcript), 4)
	if err != nil {
		t.Fatalf("device SAS: HKDF: %v", err)
	}
	n := binary.BigEndian.Uint32(key)
	return fmt.Sprintf("%06d", n%1000000)
}

type s7bMintJSON struct {
	Code      string `json:"code"`
	RID       string `json:"rid"`
	ExpiresAt string `json:"expires_at"`
	State     string `json:"state"`
}

type s7bStateJSON struct {
	State       string `json:"state"`
	Code        string `json:"code"`
	RID         string `json:"rid"`
	SAS         string `json:"sas"`
	ExpiresAt   string `json:"expires_at"`
	ComputerPub string `json:"computer_pub"`
	ReplyNonce  string `json:"reply_nonce"`
}

type s7bDeviceListJSON struct {
	Devices []s7bDeviceJSON `json:"devices"`
}

type s7bDeviceJSON struct {
	ID       string `json:"id"`
	RID      string `json:"rid"`
	Label    string `json:"label"`
	PairedAt string `json:"paired_at"`
}

func s7bMint(t *testing.T, h http.Handler) s7bMintJSON {
	t.Helper()
	rec := routeRequest(h, http.MethodPost, "/api/remote/pairing", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/remote/pairing status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var minted s7bMintJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("mint JSON: %v (%q)", err, rec.Body.String())
	}
	if minted.Code == "" || minted.RID == "" {
		t.Fatalf("mint missing code/rid: %+v", minted)
	}
	return minted
}

func s7bOffer(t *testing.T, c *remote.Client, code, id, label string, yPub, offerN []byte) {
	t.Helper()
	// SignPub is the device's ed25519 identity, distinct from the P-256 Y
	// above: Y derives the SAS and receives the sealed answer, SignPub is what
	// the device is recorded as. Since S7c a pairing without one fails closed
	// rather than completing into a device nothing can reach, so a fixture
	// that omits it is no longer a pairing that can succeed. It is generated
	// fresh per call and deliberately not pinned — SignPub is not part of the
	// §11 transcript, so it cannot affect the SAS this file asserts over.
	signPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("device identity key: %v", err)
	}
	err = c.AcceptPairingOffer(context.Background(), remote.PairingOffer{
		Code:       code,
		DeviceID:   id,
		Label:      label,
		DevicePub:  yPub,
		OfferNonce: offerN,
		SignPub:    signPub,
	})
	if err != nil {
		t.Fatalf("AcceptPairingOffer: %v", err)
	}
}

// TestAT_S7b_SASFromLiveSessionViaHTTP is the packet closer: SAS is derived
// from the live pairing session's own key and exposed over the HTTP state
// route. An AT that only called DerivePairingSAS would not close S7b.
func TestAT_S7b_SASFromLiveSessionViaHTTP(t *testing.T) {
	const at = "AT-S7b-sas-http"
	_, h, c := s7bPairingApp(t)

	minted := s7bMint(t, h)
	yPriv, yPub := s7bMustP256(t)
	offerN := bytesRepeat(0x41, 32)
	s7bOffer(t, c, minted.Code, "phone", "Phone", yPub, offerN)

	rec := routeRequest(h, http.MethodGet, "/api/remote/pairing/"+minted.Code, "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: GET pairing state status = %d, want 200; body=%q", at, rec.Code, rec.Body.String())
	}
	var st s7bStateJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("%s: state JSON: %v (%q)", at, err, rec.Body.String())
	}
	if st.SAS == "" {
		t.Fatalf("%s: HTTP state has no SAS; FR-12 digits were never derived on the live session", at)
	}
	if len(st.SAS) != 6 {
		t.Fatalf("%s: SAS %q is not 6 digits", at, st.SAS)
	}
	if st.SAS == s1PairingSAS {
		t.Fatalf("%s: HTTP SAS is the S1 vector %s; product path must use the live session key, not the fixture", at, s1PairingSAS)
	}
	if st.ComputerPub == "" || st.ComputerPub == s1PairingXPub {
		t.Fatalf("%s: computer_pub %q is empty or the S1 pairing X; want the live ensureX key", at, st.ComputerPub)
	}
	xPub, err := hex.DecodeString(st.ComputerPub)
	if err != nil {
		t.Fatalf("%s: computer_pub hex: %v", at, err)
	}
	liveX, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatalf("%s: PairingECDHPublic: %v", at, err)
	}
	if hex.EncodeToString(liveX) != st.ComputerPub {
		t.Fatalf("%s: HTTP computer_pub %s != live PairingECDHPublic %s", at, st.ComputerPub, hex.EncodeToString(liveX))
	}
	replyN, err := hex.DecodeString(st.ReplyNonce)
	if err != nil || len(replyN) != 32 {
		t.Fatalf("%s: reply_nonce %q: %v", at, st.ReplyNonce, err)
	}
	tr, err := remote.BuildPairingTranscript(remote.DefaultOrigin, minted.Code, minted.RID, xPub, yPub, nil, offerN, replyN)
	if err != nil {
		t.Fatalf("%s: transcript: %v", at, err)
	}
	want := s7bDeviceSAS(t, yPriv, xPub, tr)
	if st.SAS != want {
		t.Fatalf("%s: HTTP SAS %q != SAS derived from live X and this session's transcript %q", at, st.SAS, want)
	}
}

// TestAT_S7b_MintDoesNotConfirm proves FR-12: confirm is a separate explicit
// call. No route may mint and confirm in one request, and mint must not
// default a confirm flag to true.
func TestAT_S7b_MintDoesNotConfirm(t *testing.T) {
	const at = "AT-S7b-mint-no-confirm"
	_, h, _ := s7bPairingApp(t)

	bodies := []string{
		"",
		`{"computer_confirm":true,"device_confirm":true}`,
		`{"confirm":true}`,
		`{"computer_confirm":true}`,
	}
	for _, body := range bodies {
		rec := routeRequest(h, http.MethodPost, "/api/remote/pairing", body, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: POST mint body %q status = %d, want 200; body=%q", at, body, rec.Code, rec.Body.String())
		}
		var minted s7bMintJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
			t.Fatalf("%s: mint JSON: %v", at, err)
		}
		if minted.State == string(remote.PairStateSucceeded) {
			t.Fatalf("%s: mint body %q completed pairing; confirm must be a separate call", at, body)
		}
		list := routeRequest(h, http.MethodGet, "/api/remote/devices", "", false)
		if list.Code != http.StatusOK {
			t.Fatalf("%s: GET devices status = %d, want 200; body=%q", at, list.Code, list.Body.String())
		}
		var devices s7bDeviceListJSON
		if err := json.Unmarshal(list.Body.Bytes(), &devices); err != nil {
			t.Fatalf("%s: devices JSON: %v", at, err)
		}
		if len(devices.Devices) != 0 {
			t.Fatalf("%s: mint body %q persisted a device: %+v", at, body, devices.Devices)
		}
	}
}

func TestAT_S7b_ConfirmRequiresExplicitBothSides(t *testing.T) {
	const at = "AT-S7b-confirm-explicit"
	_, h, c := s7bPairingApp(t)
	minted := s7bMint(t, h)
	_, yPub := s7bMustP256(t)
	s7bOffer(t, c, minted.Code, "phone", "Phone", yPub, bytesRepeat(0x11, 32))

	path := "/api/remote/pairing/" + minted.Code + "/confirm"
	for _, body := range []string{
		"",
		`{}`,
		`{"computer_confirm":true}`,
		`{"device_confirm":true}`,
		`{"computer_confirm":false,"device_confirm":false}`,
		`{"computer_confirm":true,"device_confirm":false}`,
		`{"computer_confirm":false,"device_confirm":true}`,
	} {
		rec := routeRequest(h, http.MethodPost, path, body, true)
		if rec.Code == http.StatusOK {
			t.Fatalf("%s: confirm body %q succeeded; omitted/false flags must not default to true", at, body)
		}
		list := routeRequest(h, http.MethodGet, "/api/remote/devices", "", false)
		var devices s7bDeviceListJSON
		if err := json.Unmarshal(list.Body.Bytes(), &devices); err != nil {
			t.Fatalf("%s: devices JSON: %v", at, err)
		}
		if len(devices.Devices) != 0 {
			t.Fatalf("%s: confirm body %q persisted a device", at, body)
		}
	}

	rec := routeRequest(h, http.MethodPost, path, `{"computer_confirm":true,"device_confirm":true}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: both-sides confirm status = %d, want 200; body=%q", at, rec.Code, rec.Body.String())
	}
	list := routeRequest(h, http.MethodGet, "/api/remote/devices", "", false)
	if list.Code != http.StatusOK {
		t.Fatalf("%s: GET devices after confirm status = %d; body=%q", at, list.Code, list.Body.String())
	}
	var devices s7bDeviceListJSON
	if err := json.Unmarshal(list.Body.Bytes(), &devices); err != nil {
		t.Fatalf("%s: devices JSON: %v", at, err)
	}
	if len(devices.Devices) != 1 {
		t.Fatalf("%s: device list len %d, want 1 after both-sides confirm", at, len(devices.Devices))
	}
}

func TestAT_S7b_CancelAndRevokeViaHTTP(t *testing.T) {
	const at = "AT-S7b-cancel-revoke"
	_, h, c := s7bPairingApp(t)
	minted := s7bMint(t, h)
	_, yPub := s7bMustP256(t)
	s7bOffer(t, c, minted.Code, "phone", "Phone", yPub, bytesRepeat(0x22, 32))

	cancel := routeRequest(h, http.MethodPost, "/api/remote/pairing/"+minted.Code+"/cancel", "", true)
	if cancel.Code != http.StatusOK && cancel.Code != http.StatusNoContent {
		t.Fatalf("%s: cancel status = %d, want 200/204; body=%q", at, cancel.Code, cancel.Body.String())
	}
	st := routeRequest(h, http.MethodGet, "/api/remote/pairing/"+minted.Code, "", false)
	if st.Code != http.StatusOK {
		t.Fatalf("%s: GET state after cancel status = %d; body=%q", at, st.Code, st.Body.String())
	}
	var state s7bStateJSON
	if err := json.Unmarshal(st.Body.Bytes(), &state); err != nil {
		t.Fatalf("%s: state JSON: %v", at, err)
	}
	if state.State != string(remote.PairStateCancelled) {
		t.Fatalf("%s: state after cancel = %q, want cancelled", at, state.State)
	}

	s7bOffer(t, c, minted.Code, "phone", "Phone", yPub, bytesRepeat(0x22, 32))
	confirm := routeRequest(h, http.MethodPost, "/api/remote/pairing/"+minted.Code+"/confirm",
		`{"computer_confirm":true,"device_confirm":true}`, true)
	if confirm.Code != http.StatusOK {
		t.Fatalf("%s: confirm after cancel+reoffer status = %d; body=%q", at, confirm.Code, confirm.Body.String())
	}
	var dev s7bDeviceJSON
	if err := json.Unmarshal(confirm.Body.Bytes(), &dev); err != nil {
		t.Fatalf("%s: confirm JSON: %v (%q)", at, err, confirm.Body.String())
	}
	if dev.ID == "" {
		t.Fatalf("%s: confirm returned empty device", at)
	}
	rev := routeRequest(h, http.MethodDelete, "/api/remote/devices/"+dev.ID, "", true)
	if rev.Code != http.StatusOK && rev.Code != http.StatusNoContent {
		t.Fatalf("%s: revoke status = %d, want 200/204; body=%q", at, rev.Code, rev.Body.String())
	}
	list := routeRequest(h, http.MethodGet, "/api/remote/devices", "", false)
	var devices s7bDeviceListJSON
	if err := json.Unmarshal(list.Body.Bytes(), &devices); err != nil {
		t.Fatalf("%s: devices JSON: %v", at, err)
	}
	if len(devices.Devices) != 0 {
		t.Fatalf("%s: revoke left devices: %+v", at, devices.Devices)
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
