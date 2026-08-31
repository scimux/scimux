package app

import (
	"encoding/json"
	"net/http"
	"testing"

	"codeberg.org/chrberger/scimux/internal/remote"
)

// F1, the unlink route.
//
// internal/remote can now release an installation; this is the half that
// makes it something a person can do. The route is deliberately thin,
// because the interesting decision was already taken underneath: the
// local unlink is unconditional and the remote release is best-effort,
// so the only thing this layer owes the user is an honest report of
// which halves happened.
//
//	A1  TestRemoteUnenrollReleasesAndReportsIt
//	A2  TestRemoteUnenrollReportsAnUnreleasedInstallationHonestly
//	A3  TestRemoteUnenrollSurfacesALocalFailure
//	A4  TestRemoteUnenrollIsAbsentWhenRemoteIsNotEnabled
//	A5  TestRemoteUnenrollNeedsCSRF

func TestRemoteUnenrollReleasesAndReportsIt(t *testing.T) {
	f := &fakePairingClient{hosted: "enrolled", unenrollReleased: true}
	h := pairingHandlerWith(t, f)

	rec := routeRequest(h, http.MethodPost, "/api/remote/unenroll", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/remote/unenroll = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if f.unenrollCalls != 1 {
		t.Fatalf("Unenroll called %d times, want 1", f.unenrollCalls)
	}
	var body struct {
		Released bool   `json:"released"`
		Hosted   string `json:"hosted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if !body.Released {
		t.Fatal("released=false after the rendezvous confirmed the release")
	}
	// The status is read *after* the unlink, so the burger can retire the
	// remote section without a second round trip.
	if body.Hosted == "enrolled" {
		t.Fatalf("hosted = %q after unlink", body.Hosted)
	}
}

func TestRemoteUnenrollReportsAnUnreleasedInstallationHonestly(t *testing.T) {
	// The rendezvous was unreachable. The computer is unlinked all the same —
	// that is the point of the unconditional local half — but the user is
	// told plainly, because an installation the rendezvous still holds is
	// one only its operator can strike off.
	f := &fakePairingClient{hosted: "unavailable", unenrollReleased: false}
	h := pairingHandlerWith(t, f)

	rec := routeRequest(h, http.MethodPost, "/api/remote/unenroll", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("an unreachable rendezvous failed the unlink: %d %q", rec.Code, rec.Body.String())
	}
	var body struct {
		Released bool `json:"released"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body.Released {
		t.Fatal("claimed a release the rendezvous never confirmed")
	}
}

func TestRemoteUnenrollSurfacesALocalFailure(t *testing.T) {
	// The local half is the half that must not fail silently: an identity
	// still on disk is still an enrollment, and a UI that said otherwise
	// would leave the user believing they had unlinked.
	f := &fakePairingClient{
		hosted:      "enrolled",
		unenrollErr: &remote.Error{Class: remote.ClassStateLock, Op: "unenroll", Guidance: "identity file is locked"},
	}
	h := pairingHandlerWith(t, f)

	rec := routeRequest(h, http.MethodPost, "/api/remote/unenroll", "", true)
	if rec.Code < 400 {
		t.Fatalf("a failed unlink answered %d, want an error; body=%q", rec.Code, rec.Body.String())
	}
}

func TestRemoteUnenrollIsAbsentWhenRemoteIsNotEnabled(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	h := newTestHandler(t, a)
	rec := routeRequest(h, http.MethodPost, "/api/remote/unenroll", "", true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unlink without remote = %d, want 404", rec.Code)
	}
}

func TestRemoteUnenrollNeedsCSRF(t *testing.T) {
	f := &fakePairingClient{hosted: "enrolled", unenrollReleased: true}
	h := pairingHandlerWith(t, f)
	rec := routeRequest(h, http.MethodPost, "/api/remote/unenroll", "", false)
	if rec.Code < 400 {
		t.Fatalf("unlink without CSRF = %d, want a refusal", rec.Code)
	}
	if f.unenrollCalls != 0 {
		t.Fatalf("a CSRF-less request unlinked the computer (%d calls)", f.unenrollCalls)
	}
}
