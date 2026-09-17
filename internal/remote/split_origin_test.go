package remote

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfficialInviteSeparatesViewerAndServiceOrigins(t *testing.T) {
	c, pc, _ := mintedInvite(t)
	c.cfg.Origin = ""
	link, err := c.PairingLink(pc)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != "https://my.scimux.com/p" {
		t.Fatalf("invite page = %q, want official viewer", got)
	}
	q := inviteFragment(t, link)
	if got := q.Get("v"); got != "2" {
		t.Fatalf("invite version = %q, want 2", got)
	}
	if got := q.Get("o"); got != "https://rv.scimux.com" {
		t.Fatalf("service origin = %q, want official rendezvous", got)
	}
	if u.RawQuery != "" || strings.Contains(u.Path, pc.Code) || strings.Contains(u.Path, pc.RID) {
		t.Fatalf("pairing material escaped fragment: %q", link)
	}
}

func TestCustomViewerDoesNotChangeCryptographicServiceOrigin(t *testing.T) {
	c, pc, _ := mintedInvite(t)
	setViewerOriginForTest(t, &c.cfg, "https://viewer.example/")
	link, err := c.PairingLink(pc)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != "https://viewer.example/p" {
		t.Fatalf("invite page = %q", got)
	}
	if got := inviteFragment(t, link).Get("o"); got != c.origin() {
		t.Fatalf("service origin = %q, want %q", got, c.origin())
	}
}

func TestCustomServiceDoesNotSilentlyChooseViewer(t *testing.T) {
	c, pc, _ := mintedInvite(t)
	c.cfg.Origin = "https://service.example"
	link, err := c.PairingLink(pc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "https://my.scimux.com/p#") {
		t.Fatalf("custom service changed viewer: %q", link)
	}
	if got := inviteFragment(t, link).Get("o"); got != "https://service.example" {
		t.Fatalf("service origin = %q", got)
	}
}

func TestInviteNeedsAComputerPairingKey(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewClient(Config{DataDir: blocked})
	_, err := c.PairingLink(PairingCode{
		Code: "12345678",
		RID:  strings.Repeat("a", RendezvousIDHexLen),
	})
	if err == nil {
		t.Fatal("PairingLink without a computer pairing key succeeded")
	}
}
