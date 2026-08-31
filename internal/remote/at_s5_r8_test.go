package remote

import (
	"bytes"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestS5R8_F1_MoveAndReplaceDuringEnrollment(t *testing.T) {
	t.Run("enrolled", func(t *testing.T) {
		ctx, cancel := ctxTO(t)
		defer cancel()
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
		cfg.InviteFile = path
		moved := path + ".moved"
		replacement := []byte("r8-unrelated-replacement\n")
		held, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		cfg.BeforeEnrollRequest = func() {
			if err := os.Rename(path, moved); err != nil {
				t.Errorf("move original: %v", err)
			}
			if err := os.WriteFile(path, replacement, 0o600); err != nil {
				t.Errorf("install replacement: %v", err)
			}
		}
		c, err := startClient(ctx, cfg)
		if err != nil {
			t.Fatalf("F1: enroll: %v", err)
		}
		defer c.Close()
		if fake.EnrollCount() != 1 {
			t.Fatalf("F1: enroll count = %d, want 1", fake.EnrollCount())
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("F1: replacement missing: %v", err)
		}
		if !bytes.Equal(got, replacement) {
			t.Fatalf("F1: replacement mutated: %q", got)
		}
		assertNoInvitePlaintext(t, held)
		movedBytes, err := os.ReadFile(moved)
		if err != nil {
			t.Fatalf("F1: moved original missing: %v", err)
		}
		if bytes.Contains(movedBytes, []byte(vectorInviteGrouped)) || bytes.Contains(movedBytes, []byte(vectorInviteCanon)) {
			t.Fatalf("F1: moved original still contains invite plaintext: %q", movedBytes)
		}
		st, _ := c.State()
		if st != StateEnrolled {
			t.Fatalf("F1: state = %q, want enrolled", st)
		}
		pub, err := c.PublicKey()
		if err != nil {
			t.Fatal(err)
		}
		handle, err := c.Handle()
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
		cfg2 := cfg
		cfg2.InviteFile = ""
		cfg2.BeforeEnrollRequest = nil
		c2, err := startClient(ctx, cfg2)
		if err != nil {
			t.Fatalf("F1: restart without invite: %v", err)
		}
		defer c2.Close()
		pub2, err := c2.PublicKey()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(pub, pub2) {
			t.Fatal("F1: restart rotated the identity")
		}
		h2, err := c2.Handle()
		if err != nil || h2 != handle {
			t.Fatalf("F1: restart handle = %q, want %q (%v)", h2, handle, err)
		}
		if fake.EnrollCount() != 1 {
			t.Fatalf("F1: restart re-sent enroll (%d)", fake.EnrollCount())
		}
	})

	t.Run("lost-response", func(t *testing.T) {
		ctx, cancel := ctxTO(t)
		defer cancel()
		fake := newFakeRV(t)
		fake.DropEnrollResponse(true)
		cfg := clientCfg(t, fake)
		path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
		cfg.InviteFile = path
		moved := path + ".moved"
		replacement := []byte("r8-unrelated-replacement-lost\n")
		held, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		cfg.BeforeEnrollRequest = func() {
			if err := os.Rename(path, moved); err != nil {
				t.Errorf("move original: %v", err)
			}
			if err := os.WriteFile(path, replacement, 0o600); err != nil {
				t.Errorf("install replacement: %v", err)
			}
		}
		c := NewClient(cfg)
		err = c.Start(ctx)
		if err == nil {
			t.Fatal("F1: lost enroll response succeeded")
		}
		if fake.EnrollCount() != 1 {
			t.Fatalf("F1: enroll count = %d, want 1", fake.EnrollCount())
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("F1: replacement missing: %v", err)
		}
		if !bytes.Equal(got, replacement) {
			t.Fatalf("F1: replacement mutated: %q", got)
		}
		assertNoInvitePlaintext(t, held)
		st, raw, ok := readStateFile(t, c.StatePath())
		if !ok {
			t.Fatal("F1: identity missing after lost response")
		}
		if st.Status != StateAmbiguous && st.Status != StatePartial {
			t.Fatalf("F1: status = %q, want ambiguous; bytes=%s", st.Status, raw)
		}
		_ = c.Close()
		nreq := fake.RequestCount()
		cfg2 := cfg
		cfg2.InviteFile = ""
		cfg2.BeforeEnrollRequest = nil
		c2 := NewClient(cfg2)
		err = c2.Start(ctx)
		requireClass(t, err, ClassAmbiguousEnrollment)
		if fake.RequestCount() != nreq || fake.EnrollCount() != 1 {
			t.Fatalf("F1: restart contacted rendezvous or retried enroll")
		}
		st2, _, ok := readStateFile(t, c2.StatePath())
		if !ok || st2.PublicKey != st.PublicKey {
			t.Fatal("F1: restart discarded the ambiguous identity")
		}
	})
}

func TestS5R8_F1_PreNetworkClosesWithoutMutation(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	cfg.FailWrite = &WriteFault{Step: WriteTempCreate}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	err = NewClient(cfg).Start(ctx)
	if err == nil {
		t.Fatal("F1: pre-network persist failure succeeded")
	}
	if fake.EnrollCount() != 0 {
		t.Fatalf("F1: pre-network failure contacted enroll (%d)", fake.EnrollCount())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("F1: pre-network failure removed invite: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("F1: pre-network failure mutated invite: %q -> %q", before, after)
	}
	if _, err := held.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(held)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, before) {
		t.Fatalf("F1: pre-network failure shredded the validated inode: %q", got)
	}
}

func TestS5R8_F2_QuarantineCollisionRetriesWithoutOverwrite(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	collide := bytes.Repeat([]byte{0xaa}, inviteQuarantineEntropy)
	fresh := bytes.Repeat([]byte{0xbb}, inviteQuarantineEntropy)
	qpath := filepath.Join(filepath.Dir(path), inviteQuarantinePrefix+hex.EncodeToString(collide))
	sentinel := []byte("r8-preexisting-quarantine\n")
	if err := os.WriteFile(qpath, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Rand = newScriptedRand(t, 32+inviteQuarantineEntropy*inviteQuarantineAttempts+16, joinRand(bytes.Repeat([]byte{0x11}, 32), collide, fresh))
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("F2: enroll after collision retry: %v", err)
	}
	defer c.Close()
	got, err := os.ReadFile(qpath)
	if err != nil {
		t.Fatalf("F2: collision target missing: %v", err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Fatalf("F2: existing quarantine modified: %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("F2: successful retry left the invite pathname")
	}
	if invitePlaintextAt(qpath) || invitePlaintextAt(path) {
		t.Fatal("F2: invite plaintext remains after successful collision retry")
	}
	if fake.EnrollCount() != 1 {
		t.Fatalf("F2: enroll count = %d, want 1", fake.EnrollCount())
	}
}

func TestS5R8_F2_QuarantineCollisionExhaustion(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	collide := bytes.Repeat([]byte{0xaa}, inviteQuarantineEntropy)
	qpath := filepath.Join(filepath.Dir(path), inviteQuarantinePrefix+hex.EncodeToString(collide))
	sentinel := []byte("r8-exhausted-quarantine\n")
	if err := os.WriteFile(qpath, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	payload := joinRand(bytes.Repeat([]byte{0x11}, 32))
	for i := 0; i < inviteQuarantineAttempts; i++ {
		payload = append(payload, collide...)
	}
	cfg.Rand = newScriptedRand(t, 32+inviteQuarantineEntropy*inviteQuarantineAttempts+16, payload)
	c := NewClient(cfg)
	err := c.Start(ctx)
	if err == nil {
		t.Fatal("F2: exhausted quarantine collisions succeeded")
	}
	requireClass(t, err, ClassInviteFile)
	g := strings.ToLower(err.Error() + guidanceOf(err))
	for _, w := range []string{"invite", "delete"} {
		if !strings.Contains(g, w) {
			t.Errorf("F2: exhaustion guidance missing %q: %v", w, err)
		}
	}
	requireNoInviteLeak(t, vectorInviteGrouped, err.Error(), guidanceOf(err))
	got, err := os.ReadFile(qpath)
	if err != nil {
		t.Fatalf("F2: collision target missing after exhaustion: %v", err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Fatalf("F2: existing quarantine modified on exhaustion: %q", got)
	}
	if invitePlaintextAt(path) || invitePlaintextAt(qpath) {
		t.Fatal("F2: exhaustion silently left invite plaintext")
	}
	st, _ := c.State()
	if st != StateEnrolled {
		t.Fatalf("F2: exhaustion undid enrollment: state=%q", st)
	}
	if fake.EnrollCount() != 1 {
		t.Fatalf("F2: enroll count = %d, want 1", fake.EnrollCount())
	}
	pub, err := c.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	cfg2 := cfg
	cfg2.InviteFile = ""
	cfg2.Rand = nil
	c2, err := startClient(ctx, cfg2)
	if err != nil {
		t.Fatalf("F2: restart after exhaustion: %v", err)
	}
	defer c2.Close()
	pub2, err := c2.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pub, pub2) {
		t.Fatal("F2: restart after exhaustion rotated the identity")
	}
	if fake.EnrollCount() != 1 {
		t.Fatalf("F2: restart retried enroll (%d)", fake.EnrollCount())
	}
}

func assertNoInvitePlaintext(t *testing.T, f *os.File) {
	t.Helper()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte(vectorInviteGrouped)) || bytes.Contains(got, []byte(vectorInviteCanon)) {
		t.Fatalf("invite plaintext still present on held original: %q", got)
	}
}

func joinRand(chunks ...[]byte) []byte {
	var b []byte
	for _, c := range chunks {
		b = append(b, c...)
	}
	return b
}

type scriptedRand struct {
	t   *testing.T
	max int
	n   atomic.Int32
	buf []byte
	off int
}

func newScriptedRand(t *testing.T, maxReads int, buf []byte) *scriptedRand {
	t.Helper()
	return &scriptedRand{t: t, max: maxReads, buf: buf}
}

func (s *scriptedRand) Read(p []byte) (int, error) {
	n := int(s.n.Add(1))
	if s.max > 0 && n > s.max {
		s.t.Fatalf("rand Read #%d exceeds bound %d (unbounded quarantine retry)", n, s.max)
	}
	if s.off >= len(s.buf) {
		return 0, io.EOF
	}
	copied := copy(p, s.buf[s.off:])
	s.off += copied
	return copied, nil
}
