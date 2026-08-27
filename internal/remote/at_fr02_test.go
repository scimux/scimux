package remote

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AT-FR-02-a: invite consumed exactly once; plaintext absent from persisted
// state; restart authenticates with stored identity and no invite.
func TestAT_FR_02_a_InviteConsumedNotPersistedRestartWithoutInvite(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	hooks := &countHooks{}
	cfg.Hooks = hooks.hooks()

	ctx, cancel := ctxTO(t)
	defer cancel()
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("AT-FR-02-a: enroll: %v", err)
	}
	st, err := c.State()
	if err != nil {
		t.Fatalf("AT-FR-02-a: state: %v", err)
	}
	if st != StateEnrolled {
		t.Fatalf("AT-FR-02-a: state = %q, want enrolled", st)
	}
	if fake.EnrollCount() != 1 {
		t.Fatalf("AT-FR-02-a: enroll requests = %d, want 1", fake.EnrollCount())
	}
	scanRootForInvite(t, cfg.DataDir, vectorInviteGrouped)

	pub, err := c.PublicKey()
	if err != nil {
		t.Fatalf("AT-FR-02-a: public key: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("AT-FR-02-a: public key length %d", len(pub))
	}
	handle, err := c.Handle()
	if err != nil || handle == "" {
		t.Fatalf("AT-FR-02-a: handle: %q %v", handle, err)
	}
	if err := c.Close(); err != nil && !errors.Is(err, ErrUnimplemented) {
		t.Fatalf("AT-FR-02-a: close: %v", err)
	}

	nreq := fake.RequestCount()
	cfg2 := Config{
		DataDir:       cfg.DataDir,
		Remote:        true,
		Origin:        DefaultOrigin,
		RendezvousURL: fake.URL(),
		HTTPClient:    fake.Client(),
		Stdout:        io.Discard,
		Stderr:        new(bytes.Buffer),
	}
	c2, err := startClient(ctx, cfg2)
	if err != nil {
		t.Fatalf("AT-FR-02-a: restart without invite: %v", err)
	}
	pub2, err := c2.PublicKey()
	if err != nil {
		t.Fatalf("AT-FR-02-a: restart public key: %v", err)
	}
	if !bytes.Equal(pub, pub2) {
		t.Fatalf("AT-FR-02-a: restart rotated the public key")
	}
	h2, err := c2.Handle()
	if err != nil || h2 != handle {
		t.Fatalf("AT-FR-02-a: restart handle = %q, want %q (%v)", h2, handle, err)
	}
	for _, r := range fake.Requests()[nreq:] {
		if r.Path == "/v1/enroll" {
			t.Fatal("AT-FR-02-a: restart re-sent enroll; invite must not be a later bearer credential")
		}
		var body map[string]any
		_ = json.Unmarshal(r.Body, &body)
		if _, ok := body["code"]; ok {
			t.Fatal("AT-FR-02-a: restart request carried invite field `code`")
		}
	}
	scanRootForInvite(t, cfg.DataDir, vectorInviteGrouped)
}

// AT-FR-02-b: malformed codes rejected locally before any request.
func TestAT_FR_02_b_MalformedInviteRejectedBeforeNetwork(t *testing.T) {
	const canon = "A1B2C3D4E5F6G7H8J9K0MNPQ"
	rows := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"too-short", canon[:inviteCodeLength-1]},
		{"too-long", canon + "A"},
		{"U-not-mapped", "A1B2C3D4E5F6G7H8J9K0MNPU"},
		{"symbol", "A1B2C3D4E5F6G7H8J9K0MNP@"},
		{"underscore-not-separator", "A1B2_C3D4_E5F6_G7H8_J9K0_MNPQ"},
		{"newline-interior", "A1B2C3D4E5F6\nG7H8J9K0MNPQ"},
		{"tab", "A1B2C3D4E5F6G7H8J9K0MNP\t"},
		{"null", "A1B2C3D4E5F6G7H8J9K0MNP\x00Q"},
		{"pairing-length", "04106105"},
		{"spaces-only", "                        "},
		{"hyphens-only", "------------------------"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if _, err := specParseInvite(row.input); err == nil && row.input != "" {
				t.Fatalf("fixture %q parses as valid under the spec; it does not belong in the malformed table", row.input)
			}
			fake := newFakeRV(t)
			cfg := clientCfg(t, fake)
			path := writeInviteFile(t, cfg.DataDir, row.input, 0o600)
			cfg.InviteFile = path
			ctx, cancel := ctxTO(t)
			defer cancel()
			_, err := startClient(ctx, cfg)
			requireClass(t, err, ClassInviteFormat)
			if fake.RequestCount() != 0 {
				t.Fatalf("AT-FR-02-b: malformed %q caused %d rendezvous requests %v", row.input, fake.RequestCount(), fake.Paths())
			}
		})
	}
}

// AT-FR-02-c: enrollment through TTY, --invite-file, --invite-stdin.
// Argv surfaces are asserted in internal/app (Run() dispatch + flag set).
func TestAT_FR_02_c_TTYInviteFileInviteStdin(t *testing.T) {
	t.Run("tty", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		var stdout, stderr bytes.Buffer
		tty := newTTY(vectorInviteGrouped)
		cfg.Terminal = tty
		cfg.Stdout = &stdout
		cfg.Stderr = &stderr
		ctx, cancel := ctxTO(t)
		defer cancel()
		c, err := startClient(ctx, cfg)
		if err != nil {
			t.Fatalf("AT-FR-02-c tty: %v", err)
		}
		disable, restore, offAtRead, onAtRead, prompts := tty.snapshot()
		if disable == 0 {
			t.Fatal("AT-FR-02-c tty: echo was never disabled")
		}
		if !offAtRead {
			t.Fatal("AT-FR-02-c tty: invite was not read with echo disabled")
		}
		if onAtRead {
			t.Fatal("AT-FR-02-c tty: invite was read while echo was on")
		}
		if restore == 0 {
			t.Fatal("AT-FR-02-c tty: echo was not restored on success")
		}
		if tty.EchoEnabled() != true {
			t.Fatal("AT-FR-02-c tty: echo not left enabled after success")
		}
		requireNoInviteLeak(t, vectorInviteGrouped, stdout.String(), stderr.String(), strings.Join(prompts, ""), errString(err))
		st, err := c.State()
		if err != nil || st != StateEnrolled {
			t.Fatalf("AT-FR-02-c tty: state = %q %v", st, err)
		}
		scanRootForInvite(t, cfg.DataDir, vectorInviteGrouped)
	})

	t.Run("tty-restore-on-failure", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		var stdout, stderr bytes.Buffer
		tty := newTTY("not-an-invite")
		cfg.Terminal = tty
		cfg.Stdout = &stdout
		cfg.Stderr = &stderr
		ctx, cancel := ctxTO(t)
		defer cancel()
		_, err := startClient(ctx, cfg)
		if err == nil {
			t.Fatal("AT-FR-02-c tty failure: expected error")
		}
		_, restore, offAtRead, _, prompts := tty.snapshot()
		if !offAtRead {
			t.Fatal("AT-FR-02-c tty failure: invite was not read with echo disabled")
		}
		if restore == 0 {
			t.Fatal("AT-FR-02-c tty failure: echo was not restored on failure")
		}
		if !tty.EchoEnabled() {
			t.Fatal("AT-FR-02-c tty failure: echo left disabled")
		}
		requireNoInviteLeak(t, "not-an-invite", stdout.String(), stderr.String(), strings.Join(prompts, ""), err.Error())
		if fake.RequestCount() != 0 {
			t.Fatalf("AT-FR-02-c tty failure: rendezvous requests %v", fake.Paths())
		}
	})

	t.Run("invite-file", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
		ctx, cancel := ctxTO(t)
		defer cancel()
		c, err := startClient(ctx, cfg)
		if err != nil {
			t.Fatalf("AT-FR-02-c invite-file: %v", err)
		}
		st, err := c.State()
		if err != nil || st != StateEnrolled {
			t.Fatalf("AT-FR-02-c invite-file: state = %q %v", st, err)
		}
		scanRootForInvite(t, cfg.DataDir, vectorInviteGrouped)
	})

	t.Run("invite-stdin", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		cfg.InviteStdin = true
		cfg.Stdin = strings.NewReader(vectorInviteGrouped + "\n")
		ctx, cancel := ctxTO(t)
		defer cancel()
		c, err := startClient(ctx, cfg)
		if err != nil {
			t.Fatalf("AT-FR-02-c invite-stdin: %v", err)
		}
		st, err := c.State()
		if err != nil || st != StateEnrolled {
			t.Fatalf("AT-FR-02-c invite-stdin: state = %q %v", st, err)
		}
		scanRootForInvite(t, cfg.DataDir, vectorInviteGrouped)
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// AT-FR-02-d: --invite-file refuses group/world readable, wrong owner, non-regular.
func TestAT_FR_02_d_InviteFileOwnershipAndMode(t *testing.T) {
	ctxRun := func(t *testing.T, cfg Config) (*fakeRV, error) {
		t.Helper()
		fake := newFakeRV(t)
		cfg.RendezvousURL = fake.URL()
		cfg.HTTPClient = fake.Client()
		cfg.Remote = true
		if cfg.DataDir == "" {
			cfg.DataDir = t.TempDir()
		}
		ctx, cancel := ctxTO(t)
		t.Cleanup(cancel)
		_, err := startClient(ctx, cfg)
		return fake, err
	}

	t.Run("group-readable", func(t *testing.T) {
		dir := t.TempDir()
		path := writeInviteFile(t, dir, vectorInviteGrouped, 0o640)
		fake, err := ctxRun(t, Config{DataDir: dir, InviteFile: path})
		requireClass(t, err, ClassInviteFile)
		if fake.RequestCount() != 0 {
			t.Fatalf("group-readable file contacted rendezvous: %v", fake.Paths())
		}
	})
	t.Run("world-readable", func(t *testing.T) {
		dir := t.TempDir()
		path := writeInviteFile(t, dir, vectorInviteGrouped, 0o644)
		fake, err := ctxRun(t, Config{DataDir: dir, InviteFile: path})
		requireClass(t, err, ClassInviteFile)
		if fake.RequestCount() != 0 {
			t.Fatalf("world-readable file contacted rendezvous: %v", fake.Paths())
		}
	})
	t.Run("wrong-owner-injected", func(t *testing.T) {
		dir := t.TempDir()
		path := writeInviteFile(t, dir, vectorInviteGrouped, 0o600)
		sys := newFakeSys(1000)
		sys.add(path, vectorInviteGrouped+"\n", 0o600, 1001, true)
		fake, err := ctxRun(t, Config{DataDir: dir, InviteFile: path, Sys: sys})
		requireClass(t, err, ClassInviteFile)
		if fake.RequestCount() != 0 {
			t.Fatalf("wrong-owner file contacted rendezvous: %v", fake.Paths())
		}
	})
	t.Run("wrong-owner-as-root", func(t *testing.T) {
		// Deterministic when the test runs as uid 0: the seam reports euid 0
		// and a file owned by 1. No skip.
		dir := t.TempDir()
		path := writeInviteFile(t, dir, vectorInviteGrouped, 0o600)
		sys := newFakeSys(0)
		sys.add(path, vectorInviteGrouped+"\n", 0o600, 1, true)
		fake, err := ctxRun(t, Config{DataDir: dir, InviteFile: path, Sys: sys})
		requireClass(t, err, ClassInviteFile)
		if fake.RequestCount() != 0 {
			t.Fatalf("root wrong-owner file contacted rendezvous: %v", fake.Paths())
		}
	})
	t.Run("non-regular-directory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "invite-dir")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		fake, err := ctxRun(t, Config{DataDir: dir, InviteFile: path})
		requireClass(t, err, ClassInviteFile)
		if fake.RequestCount() != 0 {
			t.Fatalf("directory invite-file contacted rendezvous: %v", fake.Paths())
		}
	})
}

// AT-FR-02-e: server bound the key then lost the response.
func TestAT_FR_02_e_PartialEnrollmentAmbiguity(t *testing.T) {
	fake := newFakeRV(t)
	fake.DropEnrollResponse(true)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	hooks := &countHooks{}
	cfg.Hooks = hooks.hooks()

	ctx, cancel := ctxTO(t)
	defer cancel()
	c := NewClient(cfg)
	err := c.Start(ctx)
	if err == nil {
		t.Fatal("AT-FR-02-e: Start succeeded after a dropped enroll response")
	}
	if errors.Is(err, ErrUnimplemented) {
		t.Fatal("AT-FR-02-e: unimplemented")
	}

	if !fake.Redeemed(vectorInviteGrouped) {
		t.Fatal("AT-FR-02-e: fake rendezvous did not bind the invite")
	}
	bound := fake.BoundKey(vectorInviteGrouped)
	if bound == "" {
		t.Fatal("AT-FR-02-e: bound public key missing on server")
	}

	hooks.mu.Lock()
	order := append([]string(nil), hooks.order...)
	nID := hooks.identities
	nEnroll := hooks.enroll
	hooks.mu.Unlock()
	if nID == 0 {
		t.Fatal("AT-FR-02-e: identity was not generated")
	}
	if nEnroll == 0 {
		t.Fatal("AT-FR-02-e: enroll was not attempted")
	}
	idAt, enAt := -1, -1
	for i, s := range order {
		if s == "identity" && idAt < 0 {
			idAt = i
		}
		if s == "enroll" && enAt < 0 {
			enAt = i
		}
	}
	if idAt < 0 || enAt < 0 || idAt > enAt {
		t.Fatalf("AT-FR-02-e: identity must be persisted before enroll; order=%v", order)
	}

	st, raw, ok := readStateFile(t, c.StatePath())
	if !ok {
		t.Fatal("AT-FR-02-e: identity was not durably persisted before enrollment")
	}
	if st.PublicKey == "" || st.PrivateKey == "" {
		t.Fatalf("AT-FR-02-e: persisted identity missing keys: %+v", st)
	}
	if st.PublicKey != bound {
		t.Fatalf("AT-FR-02-e: persisted pubkey %s != server-bound %s", st.PublicKey, bound)
	}
	if st.Handle != "" && st.Status == StateEnrolled {
		t.Fatal("AT-FR-02-e: client claimed enrolled after a lost response")
	}
	if st.Status != StateAmbiguous && st.Status != StatePartial {
		t.Fatalf("AT-FR-02-e: status = %q, want ambiguous/partial evidence", st.Status)
	}
	if bytes.Contains(raw, []byte(vectorInviteGrouped)) || bytes.Contains(raw, []byte(vectorInviteCanon)) {
		t.Fatal("AT-FR-02-e: invite plaintext persisted in state")
	}
	scanRootForInvite(t, cfg.DataDir, vectorInviteGrouped)

	// Restart with ambiguous state must not blindly resend, discard the key,
	// claim success, or contact the rendezvous without explicit input.
	nreq := fake.RequestCount()
	cfgRestart := Config{
		DataDir:       cfg.DataDir,
		Remote:        true,
		Origin:        DefaultOrigin,
		RendezvousURL: fake.URL(),
		HTTPClient:    fake.Client(),
		Stdout:        io.Discard,
		Stderr:        new(bytes.Buffer),
	}
	cRestart := NewClient(cfgRestart)
	err = cRestart.Start(ctx)
	requireClass(t, err, ClassAmbiguousEnrollment)
	if g := strings.ToLower(guidanceOf(err)); !strings.Contains(g, "enroll") && !strings.Contains(g, "recover") && !strings.Contains(g, "invite") {
		t.Fatalf("AT-FR-02-e: restart guidance not actionable: %q", guidanceOf(err))
	}
	if fake.RequestCount() != nreq {
		t.Fatalf("AT-FR-02-e: restart contacted the rendezvous without explicit input: %v", fake.Paths()[nreq:])
	}
	st2, _, ok := readStateFile(t, c.StatePath())
	if !ok {
		t.Fatal("AT-FR-02-e: restart discarded persisted identity")
	}
	if st2.PublicKey != st.PublicKey || st2.PrivateKey != st.PrivateKey {
		t.Fatal("AT-FR-02-e: restart mutated or substituted the identity")
	}

	// The retry is the operator running the same command again with the
	// invite they were given: a fresh process on the same data directory,
	// no recovery flag anywhere. It uses the same public key, and V1
	// rejects a redeemed invite even from the same key — no invented
	// idempotent enroll.
	fake.DropEnrollResponse(false)
	cfgRetry := cfg
	cfgRetry.InviteFile = writeInviteFile(t, t.TempDir(), vectorInviteGrouped, 0o600)
	cRetry := NewClient(cfgRetry)
	err = cRetry.Start(ctx)
	if err == nil {
		t.Fatal("AT-FR-02-e: V1 redeemed-invite retry succeeded; do not invent an idempotent enroll")
	}
	requireClass(t, err, ClassAmbiguousEnrollment)
	pubs := enrollBodies(fake.Requests())
	if len(pubs) < 2 {
		t.Fatalf("AT-FR-02-e: explicit retry did not present the key again (enroll bodies %d)", len(pubs))
	}
	first, _ := pubs[0]["pubkey"].(string)
	last, _ := pubs[len(pubs)-1]["pubkey"].(string)
	if first == "" || first != last {
		t.Fatalf("AT-FR-02-e: retry substituted a new public key: %s then %s", first, last)
	}
	st3, _, ok := readStateFile(t, c.StatePath())
	if !ok || st3.PublicKey != st.PublicKey {
		t.Fatal("AT-FR-02-e: fail-closed retry lost or replaced the identity")
	}
	if st3.Status == StateEnrolled {
		t.Fatal("AT-FR-02-e: claimed enrollment succeeded after V1 rejection")
	}
	g := strings.ToLower(guidanceOf(err))
	if g == "" {
		t.Fatal("AT-FR-02-e: fail-closed path has no recovery guidance")
	}

	t.Run("successful-handle-commit", func(t *testing.T) {
		// Fault is state-specific: only the enrolled-status commit after the
		// handle has been received, never the earlier identity persist.
		fake2 := newFakeRV(t)
		dir := t.TempDir()
		hooks2 := &countHooks{}
		cfg2 := Config{
			DataDir:       dir,
			Remote:        true,
			Origin:        DefaultOrigin,
			RendezvousURL: fake2.URL(),
			HTTPClient:    fake2.Client(),
			InviteFile:    writeInviteFile(t, dir, vectorInviteGrouped, 0o600),
			FailWrite:     &WriteFault{Step: WriteRename, When: StateEnrolled},
			Hooks:         hooks2.hooks(),
			Stdout:        io.Discard,
			Stderr:        new(bytes.Buffer),
		}
		c1 := NewClient(cfg2)
		err := c1.Start(ctx)
		if err == nil {
			t.Fatal("expected persist failure after enroll")
		}
		if errors.Is(err, ErrUnimplemented) {
			t.Fatal("successful-handle-commit: unimplemented")
		}
		if fake2.EnrollCount() != 1 {
			t.Fatalf("did not reach enrollment (enroll count = %d, want 1)", fake2.EnrollCount())
		}
		gotHandle := fake2.LastHandle()
		if gotHandle == "" {
			t.Fatal("enroll bound no handle")
		}
		hooks2.mu.Lock()
		sawHandle := hooks2.lastHandle
		nHandle := hooks2.handles
		hooks2.mu.Unlock()
		if nHandle == 0 || sawHandle != gotHandle {
			t.Fatalf("handle was not received before the enrolled commit (hook %q, server %q)", sawHandle, gotHandle)
		}
		stFail, _, ok := readStateFile(t, c1.StatePath())
		if !ok {
			t.Fatal("identity was not persisted before the enrolled commit failed")
		}
		if stFail.PublicKey == "" || stFail.PrivateKey == "" {
			t.Fatalf("identity missing after enrolled-commit fault: %+v", stFail)
		}
		if stFail.Status == StateEnrolled {
			t.Fatal("claimed enrolled after the intended final commit failed")
		}
		scanRootForInvite(t, dir, vectorInviteGrouped)

		// Recovery is a new process-equivalent Client on the same data root:
		// no invite, no in-memory handle, no flag, same identity, no second
		// enroll. The enrolled record is already on disk under the temp
		// name; replaying it costs no network and cannot spend a credential,
		// so there is nothing for an operator to authorise.
		cfgRecover := Config{
			DataDir:       dir,
			Remote:        true,
			Origin:        DefaultOrigin,
			RendezvousURL: fake2.URL(),
			HTTPClient:    fake2.Client(),
			Stdout:        io.Discard,
			Stderr:        new(bytes.Buffer),
		}
		c2 := NewClient(cfgRecover)
		if err := c2.Start(ctx); err != nil {
			t.Fatalf("successful recovery: %v", err)
		}
		st, err := c2.State()
		if err != nil || st != StateEnrolled {
			t.Fatalf("recovery state = %q %v", st, err)
		}
		h2, err := c2.Handle()
		if err != nil || h2 != gotHandle {
			t.Fatalf("recovery handle = %q, want %q (%v)", h2, gotHandle, err)
		}
		pub2, err := c2.PublicKey()
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(pub2) != stFail.PublicKey {
			t.Fatal("recovery generated or substituted a new identity")
		}
		if fake2.EnrollCount() != 1 {
			t.Fatalf("recovery resent the spent invite (enroll count %d)", fake2.EnrollCount())
		}
		assertCompleteValidState(t, mustState(t, c2.StatePath()))
	})
}

func mustState(t *testing.T, path string) PersistedState {
	t.Helper()
	st, _, ok := readStateFile(t, path)
	if !ok {
		t.Fatal("missing state file")
	}
	return st
}

func TestAT_FR_02_a_PrivateKeyNeverOnWire(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	ctx, cancel := ctxTO(t)
	defer cancel()
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	st := mustState(t, c.StatePath())
	priv, err := hex.DecodeString(st.PrivateKey)
	if err != nil || len(priv) == 0 {
		t.Fatalf("private key hex: %v %q", err, st.PrivateKey)
	}
	if bodiesContain(fake.Requests(), priv) || bodiesContain(fake.Requests(), []byte(st.PrivateKey)) {
		t.Fatal("private key left the client on the wire")
	}
	scanRootForPrivateKey(t, cfg.DataDir, ed25519.PrivateKey(priv))
}
