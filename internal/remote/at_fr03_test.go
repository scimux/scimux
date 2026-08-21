package remote

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"testing"
)

// AT-FR-03-a: first enablement writes a keypair; second start reuses it.
func TestAT_FR_03_a_IdentityGeneratedOnceAndReused(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	hooks := &countHooks{}
	cfg.Hooks = hooks.hooks()

	ctx, cancel := ctxTO(t)
	defer cancel()
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("AT-FR-03-a: first enablement: %v", err)
	}
	pub, err := c.PublicKey()
	if err != nil {
		t.Fatalf("AT-FR-03-a: public key: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("AT-FR-03-a: public key size %d, want %d", len(pub), ed25519.PublicKeySize)
	}
	hooks.mu.Lock()
	n := hooks.identities
	hooks.mu.Unlock()
	if n != 1 {
		t.Fatalf("AT-FR-03-a: identities generated = %d, want 1", n)
	}

	bodies := enrollBodies(fake.Requests())
	if len(bodies) == 0 {
		t.Fatal("AT-FR-03-a: no enroll request")
	}
	pk, _ := bodies[0]["pubkey"].(string)
	if pk == "" {
		t.Fatal("AT-FR-03-a: enroll missing pubkey")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("AT-FR-03-a: close first client: %v", err)
	}

	cfg2 := Config{
		DataDir:       cfg.DataDir,
		Remote:        true,
		Origin:        DefaultOrigin,
		RendezvousURL: fake.URL(),
		HTTPClient:    fake.Client(),
		Stdout:        cfg.Stdout,
		Stderr:        cfg.Stderr,
		Hooks:         hooks.hooks(),
	}
	c2, err := startClient(ctx, cfg2)
	if err != nil {
		t.Fatalf("AT-FR-03-a: second start: %v", err)
	}
	pub2, err := c2.PublicKey()
	if err != nil {
		t.Fatalf("AT-FR-03-a: second public key: %v", err)
	}
	if !bytes.Equal(pub, pub2) {
		t.Fatal("AT-FR-03-a: second start generated a different public key")
	}
	hooks.mu.Lock()
	n2 := hooks.identities
	hooks.mu.Unlock()
	if n2 != 1 {
		t.Fatalf("AT-FR-03-a: second start generated another identity (count %d)", n2)
	}
}

// AT-FR-03-b: identity file 0600, containing private directory 0700.
func TestAT_FR_03_b_IdentityFileMode0600Dir0700(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	ctx, cancel := ctxTO(t)
	defer cancel()
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("AT-FR-03-b: %v", err)
	}
	if got := modePerm(t, c.PrivateDir()); got != 0o700 {
		t.Fatalf("AT-FR-03-b: private dir mode = %o, want 0700", got)
	}
	if got := modePerm(t, c.StatePath()); got != 0o600 {
		t.Fatalf("AT-FR-03-b: state file mode = %o, want 0600", got)
	}
	info, err := os.Stat(c.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		t.Fatal("AT-FR-03-b: state path is a directory")
	}
}
