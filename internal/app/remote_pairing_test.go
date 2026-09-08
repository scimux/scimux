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
	devices    []remote.PairedDevice
	causes     map[string]remote.TransportCause
	connected  map[string]bool

	// The invite link (§11.1). link is returned verbatim so a test can
	// assert over a fragment it wrote itself; linkArg records what the
	// route asked for, which is how "the link matches the code it was
	// announced with" is checked at all.
	link      string
	linkErr   error
	linkCalls int
	linkArg   remote.PairingCode

	// The unlink (F1). unenrollReleased is what the §4.4 half reported;
	// hosted flips on a successful unlink because that is what the real
	// client does — the route reads the status afterwards.
	unenrollReleased bool
	unenrollErr      error
	unenrollCalls    int
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
	if f.devices == nil {
		return nil, nil
	}
	return append([]remote.PairedDevice(nil), f.devices...), nil
}

func (f *fakePairingClient) RevokePairedDevice(context.Context, string) error { return nil }

func (f *fakePairingClient) RenamePairedDevice(_ context.Context, id, label string) (remote.PairedDevice, error) {
	for i := range f.devices {
		if f.devices[i].ID == id {
			f.devices[i].Label = label
			return f.devices[i], nil
		}
	}
	return remote.PairedDevice{}, nil
}

func (f *fakePairingClient) Unenroll(context.Context) (bool, error) {
	f.unenrollCalls++
	if f.unenrollErr != nil {
		return false, f.unenrollErr
	}
	f.hosted = ""
	return f.unenrollReleased, nil
}

func (f *fakePairingClient) TransportCause(id string) (remote.TransportCause, error) {
	if f.connected[id] {
		return "", nil
	}
	if c, ok := f.causes[id]; ok {
		return c, nil
	}
	return "", &remote.Error{Class: remote.ClassPeerAbsent, Op: "transport-cause"}
}

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

var remoteStatusLeakKeys = []string{
	"code", "rid", "sas", "public_key", "ecdh_public_key", "computer_pub", "reply_nonce",
}

func assertNoRemoteStatusLeaks(t *testing.T, body map[string]any, raw string) {
	t.Helper()
	for _, key := range remoteStatusLeakKeys {
		if _, ok := body[key]; ok {
			t.Fatalf("status body leaked pairing field %q: %s", key, raw)
		}
	}
	devs, _ := body["devices"].([]any)
	for _, d := range devs {
		dm, _ := d.(map[string]any)
		for _, key := range remoteStatusLeakKeys {
			if _, ok := dm[key]; ok {
				t.Fatalf("device leaked pairing field %q: %s", key, raw)
			}
		}
	}
}

func getRemoteStatus(t *testing.T, f *fakePairingClient) (map[string]any, string) {
	t.Helper()
	h := pairingHandlerWith(t, f)
	rec := routeRequest(h, http.MethodGet, "/api/remote/status", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/remote/status status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status JSON: %v (%q)", err, rec.Body.String())
	}
	return body, rec.Body.String()
}

func TestRemoteStatusRoute(t *testing.T) {
	for _, status := range []string{"enrolled", "disabled", "revoked", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			f := &fakePairingClient{hosted: status}
			body, raw := getRemoteStatus(t, f)
			if got, ok := body["hosted"].(string); !ok || got != status {
				t.Fatalf("hosted = %v, want %q", body["hosted"], status)
			}
			assertNoRemoteStatusLeaks(t, body, raw)
			// A closed set on purpose: this route is read by the menu and
			// must not become a place where remote state accretes. Adding a
			// key is a deliberate act, and can_pair/pair_refusal were added
			// so the menu need not re-derive the pairing policy from
			// `hosted` (TestRemoteStatusReportsWhetherPairingIsPossible).
			allowed := map[string]bool{"hosted": true, "devices": true, "can_pair": true, "pair_refusal": true}
			for k := range body {
				if !allowed[k] {
					t.Fatalf("unexpected status key %q: %s", k, raw)
				}
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

func s8StatusCauses() []remote.TransportCause {
	return []remote.TransportCause{
		remote.CauseRendezvousUnavailable,
		remote.CauseComputerOffline,
		remote.CauseSignallingRejected,
		remote.CauseICEFailed,
		remote.CauseAuthFailed,
		remote.CauseConnectedThenLost,
		// The seventh, added with tunnel 2.0.0.
		remote.CauseTunnelVersionMismatch,
	}
}

func entryHasFallback(entry string) bool {
	l := strings.ToLower(entry)
	return strings.Contains(l, "ssh") ||
		strings.Contains(l, "wireguard") ||
		strings.Contains(l, "tailscale")
}

func TestRemoteStatusEveryCauseRoundTrips(t *testing.T) {
	causes := s8StatusCauses()
	devices := make([]remote.PairedDevice, len(causes))
	causeByID := make(map[string]remote.TransportCause, len(causes))
	for i, c := range causes {
		id := "dev-" + string(c)
		devices[i] = remote.PairedDevice{ID: id}
		causeByID[id] = c
	}
	f := &fakePairingClient{hosted: "enrolled", devices: devices, causes: causeByID}
	body, raw := getRemoteStatus(t, f)
	if body["hosted"] != "enrolled" {
		t.Fatalf("hosted = %v, want enrolled", body["hosted"])
	}
	assertNoRemoteStatusLeaks(t, body, raw)
	devs, _ := body["devices"].([]any)
	if len(devs) == 0 {
		t.Fatal("anti-vacuity: devices array is empty")
	}
	seen := map[string]bool{}
	for _, d := range devs {
		dm, ok := d.(map[string]any)
		if !ok {
			t.Fatalf("device is not an object: %v", d)
		}
		id, _ := dm["id"].(string)
		want, ok := causeByID[id]
		if !ok {
			t.Fatalf("unexpected device id %q", id)
		}
		t.Run(string(want), func(t *testing.T) {
			got, _ := dm["cause"].(string)
			if got != string(want) {
				t.Fatalf("cause = %q, want exact constant %q (not a generic unreachable)", got, want)
			}
			if got == "unreachable" {
				t.Fatal("generic unreachable satisfied a row")
			}
			if dm["connected"] != false {
				t.Fatalf("connected = %v, want false", dm["connected"])
			}
			entry, err := json.Marshal(dm)
			if err != nil {
				t.Fatal(err)
			}
			if want == remote.CauseICEFailed {
				g, _ := dm["guidance"].(string)
				if g == "" {
					t.Fatal("ice-failed missing guidance")
				}
				if !strings.Contains(strings.ToLower(g), "ssh") ||
					!strings.Contains(strings.ToLower(g), "wireguard") ||
					!strings.Contains(strings.ToLower(g), "tailscale") {
					t.Fatalf("ice-failed guidance does not name SSH/WireGuard/Tailscale: %q", g)
				}
			} else if _, ok := dm["guidance"]; ok {
				t.Fatalf("%s carried guidance: %s", want, entry)
			} else if entryHasFallback(string(entry)) {
				t.Fatalf("%s entry named a fallback: %s", want, entry)
			}
		})
		if got, _ := dm["cause"].(string); got != "" {
			seen[got] = true
		}
	}
	if len(seen) != len(causes) {
		t.Fatalf("saw %d distinct cause strings, want %d: %v", len(seen), len(causes), seen)
	}
}

func TestRemoteStatusNoChannelOmitsCauseKey(t *testing.T) {
	f := &fakePairingClient{
		hosted:  "enrolled",
		devices: []remote.PairedDevice{{ID: "old"}},
	}
	body, raw := getRemoteStatus(t, f)
	if body["hosted"] != "enrolled" {
		t.Fatalf("hosted = %v, want enrolled", body["hosted"])
	}
	assertNoRemoteStatusLeaks(t, body, raw)
	devs, _ := body["devices"].([]any)
	if len(devs) == 0 {
		t.Fatal("anti-vacuity: devices array is empty")
	}
	dm, _ := devs[0].(map[string]any)
	if dm["id"] != "old" {
		t.Fatalf("id = %v, want old", dm["id"])
	}
	if dm["connected"] != false {
		t.Fatalf("connected = %v, want false", dm["connected"])
	}
	if _, ok := dm["cause"]; ok {
		t.Fatalf("cause key present on a device with no attached channel: keys=%v body=%s", keysOf(dm), raw)
	}
	if _, ok := dm["guidance"]; ok {
		t.Fatalf("guidance key present with no cause: keys=%v", keysOf(dm))
	}
}

func TestRemoteStatusConnectedOmitsCauseAndGuidance(t *testing.T) {
	f := &fakePairingClient{
		hosted:    "enrolled",
		devices:   []remote.PairedDevice{{ID: "phone"}},
		connected: map[string]bool{"phone": true},
	}
	body, raw := getRemoteStatus(t, f)
	if body["hosted"] != "enrolled" {
		t.Fatalf("hosted = %v, want enrolled", body["hosted"])
	}
	assertNoRemoteStatusLeaks(t, body, raw)
	devs, _ := body["devices"].([]any)
	if len(devs) == 0 {
		t.Fatal("anti-vacuity: devices array is empty")
	}
	dm, _ := devs[0].(map[string]any)
	if dm["connected"] != true {
		t.Fatalf("connected = %v, want true", dm["connected"])
	}
	if _, ok := dm["cause"]; ok {
		t.Fatalf("cause key present on a connected device: keys=%v body=%s", keysOf(dm), raw)
	}
	if _, ok := dm["guidance"]; ok {
		t.Fatalf("guidance key present on a connected device: keys=%v", keysOf(dm))
	}
}

func TestRemoteStatusHostedUnchangedIncludingRevoked(t *testing.T) {
	f := &fakePairingClient{
		hosted:  "revoked",
		devices: []remote.PairedDevice{{ID: "phone"}, {ID: "ipad"}},
		causes:  map[string]remote.TransportCause{"ipad": remote.CauseICEFailed},
	}
	body, raw := getRemoteStatus(t, f)
	if body["hosted"] != "revoked" {
		t.Fatalf("hosted = %v, want revoked (not rewritten into a cause)", body["hosted"])
	}
	assertNoRemoteStatusLeaks(t, body, raw)
	devs, _ := body["devices"].([]any)
	if len(devs) == 0 {
		t.Fatal("anti-vacuity: devices array is empty")
	}
	byID := map[string]map[string]any{}
	for _, d := range devs {
		dm, _ := d.(map[string]any)
		id, _ := dm["id"].(string)
		byID[id] = dm
		if c, _ := dm["cause"].(string); c == "revoked" {
			t.Fatalf("device %q used hosted revoked as a transport cause: %v", id, dm)
		}
	}
	phone := byID["phone"]
	if phone == nil {
		t.Fatalf("phone missing from devices: %s", raw)
	}
	if _, ok := phone["cause"]; ok {
		t.Fatalf("revoked installation projected a cause onto a device with none: keys=%v", keysOf(phone))
	}
	ipad := byID["ipad"]
	if ipad == nil {
		t.Fatalf("ipad missing from devices: %s", raw)
	}
	if ipad["cause"] != string(remote.CauseICEFailed) {
		t.Fatalf("ipad cause = %v, want ice-failed (hosted and cause are separate)", ipad["cause"])
	}
}
