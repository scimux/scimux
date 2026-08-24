package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/remote"
)

// fakePairingClient is the in-process hostedPairingClient used by the S8
// revoke tests. hosted is the settable HostedStatus; mintCalls records
// whether MintPairingCode ran.
type fakePairingClient struct {
	hosted     string
	mintCalls  int
	mintResult remote.PairingCode
	session    remote.PairingStatus
}

func (f *fakePairingClient) HostedStatus() string { return f.hosted }

func (f *fakePairingClient) MintPairingCode(context.Context) (remote.PairingCode, error) {
	f.mintCalls++
	return f.mintResult, nil
}

func (f *fakePairingClient) PairingSession(string) (remote.PairingStatus, error) {
	return f.session, nil
}

func (f *fakePairingClient) CompletePairing(context.Context, string, bool, bool) (remote.PairedDevice, error) {
	return remote.PairedDevice{}, nil
}

func (f *fakePairingClient) CancelPairing(context.Context, string) error { return nil }

func (f *fakePairingClient) PairedDevices() ([]remote.PairedDevice, error) {
	return nil, nil
}

func (f *fakePairingClient) RevokePairedDevice(context.Context, string) error { return nil }

func pairingHandlerWith(t *testing.T, f *fakePairingClient) http.Handler {
	t.Helper()
	a := newTestApp(t, &fakeTmux{})
	a.hostedPairing = f
	return newTestHandler(t, a)
}

// Refusal is for durable facts about authorization only. "unavailable" is
// deliberately absent — see TestRemotePairingMintAllowedWhenTransientlyUnavailable.
func TestRemotePairingMintRefusedWhenNotEnrolled(t *testing.T) {
	for _, status := range []string{"revoked", "disabled"} {
		t.Run(status, func(t *testing.T) {
			f := &fakePairingClient{hosted: status}
			h := pairingHandlerWith(t, f)
			rec := routeRequest(h, http.MethodPost, "/api/remote/pairing", "", true)
			if rec.Code != http.StatusConflict {
				t.Fatalf("POST /api/remote/pairing hosted=%s status = %d, want 409; body=%q",
					status, rec.Code, rec.Body.String())
			}
			if f.mintCalls != 0 {
				t.Fatalf("MintPairingCode called %d times with hosted=%s, want 0", f.mintCalls, status)
			}
			var body struct {
				Hosted string `json:"hosted"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("409 body is not JSON: %v (%q)", err, rec.Body.String())
			}
			if body.Hosted != status {
				t.Fatalf("hosted = %q, want %q", body.Hosted, status)
			}
			if strings.TrimSpace(body.Error) == "" {
				t.Fatal("error is empty; want a sentence a user can read")
			}
			if !strings.Contains(strings.ToLower(body.Error), status) {
				t.Fatalf("error %q does not mention hosted status %q", body.Error, status)
			}
		})
	}
}

func TestRemotePairingMintEnrolledUnchanged(t *testing.T) {
	exp := time.Date(2026, 8, 23, 12, 0, 0, 123456789, time.UTC)
	f := &fakePairingClient{
		hosted: "enrolled",
		mintResult: remote.PairingCode{
			Code:      "04106105",
			RID:       "rid-enrolled",
			ExpiresAt: exp,
		},
	}
	h := pairingHandlerWith(t, f)
	rec := routeRequest(h, http.MethodPost, "/api/remote/pairing", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/remote/pairing enrolled status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if f.mintCalls != 1 {
		t.Fatalf("MintPairingCode called %d times, want 1", f.mintCalls)
	}
	var body s7bMintJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("mint JSON: %v (%q)", err, rec.Body.String())
	}
	if body.Code != "04106105" || body.RID != "rid-enrolled" {
		t.Fatalf("mint code/rid = %q/%q, want 04106105/rid-enrolled", body.Code, body.RID)
	}
	if body.ExpiresAt != exp.Format(time.RFC3339Nano) {
		t.Fatalf("expires_at = %q, want %q", body.ExpiresAt, exp.Format(time.RFC3339Nano))
	}
	if body.State != string(remote.PairStatePending) {
		t.Fatalf("state = %q, want %s", body.State, remote.PairStatePending)
	}
}

// TestRemotePairingMintAllowedWhenTransientlyUnavailable guards a deadlock,
// not a preference. "unavailable" is what Start() records on a *transient*
// authenticate failure (internal/remote/client.go:231); it returns nil, so
// scimux comes up normally in that state. The only thing that clears it is
// noteWaitResult, which runs solely inside waitLoopRID, which exists solely
// for a RID in liveRIDs() — paired devices plus pairing sessions. A fresh
// installation with no devices paired has neither until a code is minted
// (MintPairingCode ends in kickWaiters, internal/remote/pairing.go:255).
//
// So minting IS the recovery path. Refusing it because the installation is
// transiently unavailable is self-sustaining: one network blip at startup
// and that user can never pair until scimux restarts. Verified before this
// test existed: a device-less client at hosted="unavailable" has liveRIDs=0
// and stays "unavailable" across kickWaiters.
func TestRemotePairingMintAllowedWhenTransientlyUnavailable(t *testing.T) {
	f := &fakePairingClient{
		hosted:     "unavailable",
		mintResult: remote.PairingCode{Code: "04106105", RID: "rid-blip"},
	}
	h := pairingHandlerWith(t, f)
	rec := routeRequest(h, http.MethodPost, "/api/remote/pairing", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/remote/pairing hosted=unavailable status = %d, want 200; body=%q\n"+
			"refusing a transient state deadlocks it: the mint is what starts the "+
			"pairing wait loop that sets hosted back to enrolled", rec.Code, rec.Body.String())
	}
	if f.mintCalls != 1 {
		t.Fatalf("MintPairingCode called %d times with hosted=unavailable, want 1", f.mintCalls)
	}
}

// A live session under a transient outage is pending, not failed. FR-38's
// failed is terminal, so projecting it here would make the UI abandon a
// session that is about to succeed.
func TestRemotePairingStateNotFailedWhileTransientlyUnavailable(t *testing.T) {
	exp := time.Date(2026, 8, 23, 12, 3, 0, 0, time.UTC)
	for _, stored := range []remote.PairingUIState{remote.PairStatePending, remote.PairStateWarning} {
		t.Run(string(stored), func(t *testing.T) {
			f := &fakePairingClient{
				hosted: "unavailable",
				session: remote.PairingStatus{
					State:     stored,
					Code:      "04106105",
					RID:       "rid-blip",
					ExpiresAt: exp,
				},
			}
			h := pairingHandlerWith(t, f)
			rec := routeRequest(h, http.MethodGet, "/api/remote/pairing/04106105", "", false)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET pairing state status = %d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			var body struct {
				State  string `json:"state"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("state JSON: %v (%q)", err, rec.Body.String())
			}
			if body.State != string(stored) {
				t.Fatalf("state = %q under a transient outage, want stored %s", body.State, stored)
			}
			if body.Reason != "" {
				t.Fatalf("reason = %q, want empty while the session is still live", body.Reason)
			}
		})
	}
}

func TestRemotePairingStateFailedWhenNotEnrolled(t *testing.T) {
	exp := time.Date(2026, 8, 23, 12, 1, 0, 0, time.UTC)
	for _, status := range []string{"revoked", "disabled"} {
		for _, stored := range []remote.PairingUIState{remote.PairStatePending, remote.PairStateWarning} {
			t.Run(status+"/"+string(stored), func(t *testing.T) {
				f := &fakePairingClient{
					hosted: status,
					session: remote.PairingStatus{
						State:     stored,
						Code:      "04106105",
						RID:       "rid-live",
						ExpiresAt: exp,
					},
				}
				h := pairingHandlerWith(t, f)
				rec := routeRequest(h, http.MethodGet, "/api/remote/pairing/04106105", "", false)
				if rec.Code != http.StatusOK {
					t.Fatalf("GET pairing state status = %d, want 200; body=%q", rec.Code, rec.Body.String())
				}
				var body struct {
					State  string `json:"state"`
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("state JSON: %v (%q)", err, rec.Body.String())
				}
				if body.State != string(remote.PairStateFailed) {
					t.Fatalf("state = %q, want %s", body.State, remote.PairStateFailed)
				}
				if body.Reason != status {
					t.Fatalf("reason = %q, want hosted %q", body.Reason, status)
				}
			})
		}
	}
}

func TestRemotePairingTerminalStatesNotRewritten(t *testing.T) {
	exp := time.Date(2026, 8, 23, 12, 2, 0, 0, time.UTC)
	for _, stored := range []remote.PairingUIState{
		remote.PairStateExpired,
		remote.PairStateCancelled,
		remote.PairStateSucceeded,
	} {
		t.Run(string(stored), func(t *testing.T) {
			f := &fakePairingClient{
				hosted: "revoked",
				session: remote.PairingStatus{
					State:     stored,
					Code:      "04106105",
					RID:       "rid-done",
					ExpiresAt: exp,
				},
			}
			h := pairingHandlerWith(t, f)
			rec := routeRequest(h, http.MethodGet, "/api/remote/pairing/04106105", "", false)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET pairing state status = %d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			var body struct {
				State  string `json:"state"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("state JSON: %v (%q)", err, rec.Body.String())
			}
			if body.State != string(stored) {
				t.Fatalf("state = %q, want stored %s (terminal states are facts)", body.State, stored)
			}
			if body.Reason != "" {
				t.Fatalf("reason = %q, want empty on a terminal state", body.Reason)
			}
		})
	}
}

func TestRemoteStatusRoute(t *testing.T) {
	for _, status := range []string{"enrolled", "disabled", "revoked", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			f := &fakePairingClient{hosted: status}
			h := pairingHandlerWith(t, f)
			rec := routeRequest(h, http.MethodGet, "/api/remote/status", "", false)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /api/remote/status hosted=%s status = %d, want 200; body=%q",
					status, rec.Code, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("status JSON: %v (%q)", err, rec.Body.String())
			}
			if got, ok := body["hosted"].(string); !ok || got != status {
				t.Fatalf("hosted = %v, want %q", body["hosted"], status)
			}
			for _, key := range []string{"code", "rid", "sas", "public_key", "ecdh_public_key", "laptop_pub", "reply_nonce"} {
				if _, ok := body[key]; ok {
					t.Fatalf("status body leaked pairing field %q: %s", key, rec.Body.String())
				}
			}
			if len(body) != 1 {
				t.Fatalf("status body keys = %v, want only hosted", body)
			}
		})
	}
}

func TestRemoteStatusNeedsNoCSRF(t *testing.T) {
	f := &fakePairingClient{hosted: "enrolled"}
	h := pairingHandlerWith(t, f)
	rec := routeRequest(h, http.MethodGet, "/api/remote/status", "", false)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("GET /api/remote/status without CSRF = 403; GET must not require the header")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/remote/status without CSRF = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
}

func TestRemoteStatusWithoutPairingClientIs404(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	rec := routeRequest(h, http.MethodGet, "/api/remote/status", "", false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/remote/status with no client = %d, want 404; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "remote pairing is not enabled") {
		t.Fatalf("404 body = %q, want sibling remote-route message", rec.Body.String())
	}
}

// TestPairedDeviceJSONNamesTheECDHKeyHonestly pins a name, which is the whole
// point of it.
//
// PairedDevice.PubKey is the device's static P-256 ECDH key — the offer's
// DevicePub (internal/remote/pairing.go:449), the key a §12.2 reply envelope
// is sealed *to*. It is NOT the device's signing identity: the same commit
// puts the ed25519 SignPub on DeviceRecord.PubKey two lines later, and
// DeviceRecord's own comment says the two "cannot be the same field".
//
// Everywhere else in this codebase `public_key` means the ed25519 identity —
// PersistedDevice.PubKey, the installation's own PublicKey, the enrollment
// record. Emitting the P-256 Y under that name gives the HTTP API a field
// that collides with the on-disk record: same name, different algorithm,
// different key. Anyone comparing the API against ~/.scimux state, or
// verifying a signature with what the API called a public key, is then
// simply wrong. PersistedDevice already ships the correct name for this
// material — `ecdh_public_key` — so the API uses it too, and the two agree
// per key rather than per position.
//
// Nothing consumed the old name when it was changed; the pairing UI does not
// exist yet. That is why this is a rename and not a compatibility problem.
func TestPairedDeviceJSONNamesTheECDHKeyHonestly(t *testing.T) {
	out := pairedDeviceJSON(remote.PairedDevice{
		ID:     "phone",
		RID:    "rid-1",
		Label:  "Phone",
		PubKey: []byte{0xde, 0xad, 0xbe, 0xef},
	})

	if _, ok := out["public_key"]; ok {
		t.Errorf("pairedDeviceJSON emits %q, which means the ed25519 identity "+
			"everywhere else in this repo; this value is the P-256 ECDH Y", "public_key")
	}
	got, ok := out["ecdh_public_key"].(string)
	if !ok {
		t.Fatalf("ecdh_public_key missing or not a string: %#v", out)
	}
	if got != "deadbeef" {
		t.Fatalf("ecdh_public_key = %q, want the hex of the ECDH key", got)
	}

	// An absent key emits no field at all, matching PersistedDevice's omitempty.
	bare := pairedDeviceJSON(remote.PairedDevice{ID: "phone", RID: "rid-1"})
	for _, k := range []string{"public_key", "ecdh_public_key"} {
		if _, ok := bare[k]; ok {
			t.Errorf("device with no key still emitted %q", k)
		}
	}
}
