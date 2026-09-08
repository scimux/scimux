package app

// Pairing is offered only where it could actually complete.
//
// The hole this closes: `MintPairingCode` is entirely local — entropy, a
// Crockford code, a locally minted RID — and `PairingLink` is too, falling
// back to DefaultOrigin when nothing is configured. Neither touches the
// rendezvous. So an installation with no enrollment at all could mint a
// perfectly well-formed code, print a QR for it, and wait for an offer that
// can never arrive, because a computer that cannot admit itself at rv
// registers no waiter. The user reads that as an expiry — a timing problem
// — rather than as "this computer is not enrolled", which is the one thing
// they needed to be told.
//
// The reachable version of that state is not exotic: unlinking from the
// menu leaves the client wired up as the app's pairing client with an empty
// hosted status (internal/remote/unenroll.go:116 clears both the status and
// the on-disk identity), so the very next tap on "Pair a device" would have
// minted one.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/remote"
)

// notEnrolledStatuses are the hosted values that mean "there is no identity
// here that rv would admit". "" is the post-unlink value; the rest are
// FR-30's explicit broken states, which Start reports as errors that stop
// scimux — but the route must not be the thing relying on that.
var notEnrolledStatuses = []string{"", "absent", "key-missing", "partial", "corrupt", "ambiguous"}

func TestRemotePairingMintRefusedWithoutAnEnrollment(t *testing.T) {
	for _, status := range notEnrolledStatuses {
		t.Run("hosted="+status, func(t *testing.T) {
			f := &fakePairingClient{hosted: status}
			h := pairingHandlerWith(t, f)
			rec := routeRequest(h, http.MethodPost, "/api/remote/pairing", "", true)
			if rec.Code != http.StatusConflict {
				t.Fatalf("POST /api/remote/pairing hosted=%q status = %d, want 409; body=%q",
					status, rec.Code, rec.Body.String())
			}
			if f.mintCalls != 0 {
				t.Fatalf("MintPairingCode called %d times with hosted=%q, want 0: "+
					"a code minted here is a credential nothing can complete", f.mintCalls, status)
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
			if !strings.Contains(strings.ToLower(body.Error), "enrol") {
				t.Fatalf("error = %q; want a sentence naming the enrollment as the problem", body.Error)
			}
		})
	}
}

// A session that was live when the enrollment went away is over. The poll is
// the only thing still asking, so it is the only thing that can say so.
func TestRemotePairingStateFailedWithoutAnEnrollment(t *testing.T) {
	exp := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, stored := range []remote.PairingUIState{remote.PairStatePending, remote.PairStateWarning} {
		t.Run(string(stored), func(t *testing.T) {
			f := &fakePairingClient{
				hosted: "",
				session: remote.PairingStatus{
					State:     stored,
					Code:      "04106105",
					RID:       "rid-unlinked",
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
			if body.Reason != "not-enrolled" {
				t.Fatalf("reason = %q, want %q: an empty hosted status is not a reason "+
					"anyone can render", body.Reason, "not-enrolled")
			}
		})
	}
}

// The menu asks this before it offers the button. Deciding it in the browser
// instead would be a second copy of the policy, free to drift from the one
// the mint enforces.
func TestRemoteStatusReportsWhetherPairingIsPossible(t *testing.T) {
	for _, tc := range []struct {
		hosted string
		want   bool
	}{
		{"enrolled", true},
		// The mint is what starts the wait loop that clears a transient
		// outage, so offering it is the recovery path, not a lie.
		{"unavailable", true},
		{"revoked", false},
		{"disabled", false},
		{"", false},
		{"key-missing", false},
	} {
		t.Run("hosted="+tc.hosted, func(t *testing.T) {
			f := &fakePairingClient{hosted: tc.hosted}
			body, raw := getRemoteStatus(t, f)
			got, ok := body["can_pair"].(bool)
			if !ok {
				t.Fatalf("can_pair missing or not a bool: %s", raw)
			}
			if got != tc.want {
				t.Fatalf("can_pair = %v with hosted=%q, want %v", got, tc.hosted, tc.want)
			}
			refusal, _ := body["pair_refusal"].(string)
			if tc.want && refusal != "" {
				t.Fatalf("pair_refusal = %q while pairing is offered; want empty", refusal)
			}
			if !tc.want && strings.TrimSpace(refusal) == "" {
				t.Fatalf("pair_refusal is empty with hosted=%q; the menu has nothing to "+
					"put where the button was: %s", tc.hosted, raw)
			}
			assertNoRemoteStatusLeaks(t, body, raw)
		})
	}
}
