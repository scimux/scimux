package app

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/remote"
)

// The mint response must carry the invite link (rendezvous-v1 §11.1).
//
// V1 has no typed pairing path (§11.2): the SAS is ECDH(X, Y) and the
// transcript binds the rendezvous ID, so a device that learned only a code
// can neither derive the six digits nor address the reply. The link is
// therefore not presentation — it is the only way a pairing can start.
//
// Until now the mint route returned code, rid, expires_at and state, and
// nothing carried X. The computer could mint a pairing session that no
// browser had any way to join, which is why /p served a correct pairing
// page that could never be handed an invite.

// mintJSONWithLink is the mint response as the browser reads it.
type mintJSONWithLink struct {
	Code      string `json:"code"`
	RID       string `json:"rid"`
	ExpiresAt string `json:"expires_at"`
	State     string `json:"state"`
	Link      string `json:"link"`
}

func mintLink(t *testing.T, f *fakePairingClient) mintJSONWithLink {
	t.Helper()
	h := pairingHandlerWith(t, f)
	rec := routeRequest(h, http.MethodPost, "/api/remote/pairing", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/remote/pairing = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var body mintJSONWithLink
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("mint JSON: %v (%q)", err, rec.Body.String())
	}
	return body
}

func TestAT_S8_MintReturnsTheInviteLink(t *testing.T) {
	f := okPairingClientWithLink()
	body := mintLink(t, f)

	if body.Link == "" {
		t.Fatal("mint returned no link: the device has no way to learn X, so the pairing it just created cannot be joined")
	}
	if f.linkCalls != 1 {
		t.Fatalf("PairingLink called %d times, want 1", f.linkCalls)
	}
	if f.linkArg.Code != body.Code || f.linkArg.RID != body.RID {
		t.Fatalf("link built for %q/%q but response announced %q/%q",
			f.linkArg.Code, f.linkArg.RID, body.Code, body.RID)
	}
}

// The link is the fragment §11.1 specifies, not an opaque string. A page
// that cannot parse it fails at the digits, where there is nothing useful
// to say.
func TestAT_S8_MintLinkIsAnInviteFragment(t *testing.T) {
	body := mintLink(t, okPairingClientWithLink())

	u, err := url.Parse(body.Link)
	if err != nil {
		t.Fatalf("link %q does not parse: %v", body.Link, err)
	}
	if u.Path != "/p" {
		t.Errorf("link path = %q, want /p", u.Path)
	}
	q, err := url.ParseQuery(u.Fragment)
	if err != nil {
		t.Fatalf("fragment %q is not a query: %v", u.Fragment, err)
	}
	if got := q.Get("v"); got != remote.InviteVersion {
		t.Errorf("v = %q, want %q", got, remote.InviteVersion)
	}
	if got := q.Get("c"); got != body.Code {
		t.Errorf("c = %q, want the announced code %q", got, body.Code)
	}
	if got := q.Get("r"); got != body.RID {
		t.Errorf("r = %q, want the announced rid %q", got, body.RID)
	}
	x := q.Get("x")
	if len(x) != 130 || !strings.HasPrefix(x, "04") {
		t.Errorf("x = %q, want 04 followed by 128 hex characters", x)
	}
	if _, err := hex.DecodeString(x); err != nil {
		t.Errorf("x is not hex: %v", err)
	}
	if q.Get("o") == "" {
		t.Error("o is empty: a link opened against the wrong deployment must fail loudly")
	}
}

// §8: the rendezvous ID must never reach a query string, a Referer, or
// rv's access log. Everything rides in the fragment.
func TestAT_S8_MintLinkKeepsSecretsOutOfThePathAndQuery(t *testing.T) {
	body := mintLink(t, okPairingClientWithLink())
	u, err := url.Parse(body.Link)
	if err != nil {
		t.Fatal(err)
	}
	if u.RawQuery != "" {
		t.Errorf("link has a query string %q; §8 forbids the rid on that surface", u.RawQuery)
	}
	before, _, _ := strings.Cut(body.Link, "#")
	for _, secret := range []string{body.Code, body.RID} {
		if secret != "" && strings.Contains(before, secret) {
			t.Errorf("link leaks %q outside the fragment: %q", secret, before)
		}
	}
}

// A code with no link is a pairing session nobody can join, so the request
// fails rather than announcing one. The minted session is left to expire:
// losing it costs a rate-limit slot, whereas returning it would send the
// user to a page that can only tell them the invite is unusable.
func TestAT_S8_MintFailsWhenTheLinkCannotBeBuilt(t *testing.T) {
	f := okPairingClientWithLink()
	f.linkErr = &remote.Error{
		Class: remote.ClassUnavailable,
		Op:    "invite",
	}
	h := pairingHandlerWith(t, f)
	rec := routeRequest(h, http.MethodPost, "/api/remote/pairing", "", true)
	if rec.Code == http.StatusOK {
		t.Fatalf("mint returned 200 with no link: %q", rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
		Link string `json:"link"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Code != "" {
		t.Errorf("failed mint still announced code %q; a code the device cannot receive is not an offer", body.Code)
	}
}

// The fields the route already returned keep returning. The link is
// additive: a browser that ignores it must behave exactly as before.
func TestAT_S8_MintLinkIsAdditive(t *testing.T) {
	f := okPairingClientWithLink()
	body := mintLink(t, f)
	if body.Code != f.mintResult.Code {
		t.Errorf("code = %q, want %q", body.Code, f.mintResult.Code)
	}
	if body.RID != f.mintResult.RID {
		t.Errorf("rid = %q, want %q", body.RID, f.mintResult.RID)
	}
	if body.State != string(remote.PairStatePending) {
		t.Errorf("state = %q, want %s", body.State, remote.PairStatePending)
	}
	if body.ExpiresAt == "" {
		t.Error("expires_at is empty")
	}
}

func (f *fakePairingClient) PairingLink(pc remote.PairingCode) (string, error) {
	f.linkCalls++
	f.linkArg = pc
	if f.linkErr != nil {
		return "", f.linkErr
	}
	return f.link, nil
}

// okPairingClientWithLink is an enrolled client whose link is a §11.1
// fragment built from the same code and rid it mints, so a test that
// compares the two is comparing the route's wiring and not the fake's.
func okPairingClientWithLink() *fakePairingClient {
	const (
		code = "04106105"
		rid  = "9da98cab614a2955cbd06ae7097291a7bcaf67dc6568c929d60656ba8a138186"
		x    = "04" + "ab12cd34ef56" + "00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"
	)
	return &fakePairingClient{
		hosted: "enrolled",
		mintResult: remote.PairingCode{
			Code:      code,
			RID:       rid,
			ExpiresAt: s8LinkExpiry,
		},
		link: "http://127.0.0.1:8080/p#c=" + code +
			"&o=" + url.QueryEscape("http://127.0.0.1:8080") +
			"&r=" + rid + "&v=" + remote.InviteVersion + "&x=" + x,
	}
}

var s8LinkExpiry = mustS8Expiry()

func mustS8Expiry() time.Time {
	return time.Date(2026, 8, 26, 13, 8, 44, 716356184, time.UTC)
}
