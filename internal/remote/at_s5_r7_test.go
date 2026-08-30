package remote

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestS5R7_F1_DroppedEnrollPlusCleanupFailure(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	fake.DropEnrollResponse(true)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	fail := fmt.Errorf("injected invite cleanup failure")
	cfg.InviteUnlink = func(string) error { return fail }
	cfg.InviteLink = func(string, string) error { return fail }
	c := NewClient(cfg)
	err := c.Start(ctx)
	if err == nil {
		t.Fatal("F1: dropped enroll plus cleanup failure succeeded")
	}
	if fake.EnrollCount() != 1 {
		t.Fatalf("F1: enroll count = %d, want exactly 1", fake.EnrollCount())
	}
	requireClass(t, err, ClassInviteFile)
	g := strings.ToLower(err.Error() + guidanceOf(err))
	for _, w := range []string{"invite", "delete", "ambiguous"} {
		if !strings.Contains(g, w) {
			t.Errorf("F1: ClassInviteFile guidance missing %q: %v", w, err)
		}
	}
	if !strings.Contains(g, "enroll") && !strings.Contains(g, "lost") && !strings.Contains(g, "response") {
		t.Errorf("F1: enrollment ambiguity is not inspectable in the error: %v", err)
	}
	var wrapped *Error
	if errors.As(err, &wrapped) && wrapped.Unwrap() == nil {
		t.Fatal("F1: enrollment cause/context was not preserved on the ClassInviteFile error")
	}
	requireNoInviteLeak(t, vectorInviteGrouped, err.Error(), guidanceOf(err))
	st, raw, ok := readStateFile(t, c.StatePath())
	if !ok {
		t.Fatal("F1: identity was not persisted")
	}
	if st.Status != StateAmbiguous && st.Status != StatePartial {
		t.Fatalf("F1: persisted status = %q, want ambiguous evidence; bytes=%s", st.Status, raw)
	}
	plaintextLeft := invitePlaintextAt(path)
	if classOf(err) != ClassInviteFile && plaintextLeft {
		t.Fatal("F1: silent plaintext residue after redeeming enroll and cleanup failure")
	}

	pub := st.PublicKey
	_ = c.Close()
	nreq := fake.RequestCount()
	cfg2 := cfg
	cfg2.InviteFile = ""
	cfg2.InviteUnlink = nil
	cfg2.InviteLink = nil
	c2 := NewClient(cfg2)
	err = c2.Start(ctx)
	requireClass(t, err, ClassAmbiguousEnrollment)
	if fake.RequestCount() != nreq {
		t.Fatalf("F1: restart without invite re-contacted rendezvous: %v", fake.Paths()[nreq:])
	}
	if fake.EnrollCount() != 1 {
		t.Fatalf("F1: restart retried enroll (%d)", fake.EnrollCount())
	}
	st2, _, ok := readStateFile(t, c2.StatePath())
	if !ok || st2.PublicKey != pub {
		t.Fatal("F1: restart discarded or rotated the ambiguous identity")
	}
}

func TestS5R7_F2_RestoreDoesNotOverwriteSecondPathname(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	first := []byte("r7-first-quarantine-replacement\n")
	second := []byte("r7-second-pathname-occupant\n")
	cfg.BeforeInviteUnlinkFinal = func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Errorf("first replace remove: %v", err)
		}
		if err := os.WriteFile(path, first, 0o600); err != nil {
			t.Errorf("first replace write: %v", err)
		}
	}
	cfg.BeforeInviteRestore = func() {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("original pathname occupied before second-file barrier")
		}
		if err := os.WriteFile(path, second, 0o600); err != nil {
			t.Errorf("second file write: %v", err)
		}
	}
	c := NewClient(cfg)
	err := c.Start(ctx)
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("F2: original pathname missing after restore race: %v", readErr)
	}
	if !bytes.Equal(got, second) {
		t.Fatalf("F2: original pathname overwritten: got %q, want second file %q", got, second)
	}
	if found := findConsumedNamed(t, filepath.Dir(path), first); !found {
		t.Fatal("F2: first replacement was not preserved at a quarantine pathname")
	}
	st, _ := c.State()
	if st != StateEnrolled {
		t.Fatalf("F2: restore race undid enrollment: state=%q err=%v", st, err)
	}
	if err == nil {
		t.Fatal("F2: occupied restore returned success; want actionable cleanup guidance")
	}
	requireClass(t, err, ClassInviteFile)
	g := strings.ToLower(err.Error() + guidanceOf(err))
	for _, w := range []string{"invite", "delete"} {
		if !strings.Contains(g, w) {
			t.Errorf("F2: cleanup guidance missing %q: %v", w, err)
		}
	}
	requireNoInviteLeak(t, vectorInviteGrouped, err.Error(), guidanceOf(err))
	if fake.EnrollCount() != 1 {
		t.Fatalf("F2: enroll count = %d, want 1", fake.EnrollCount())
	}
	_ = c.Close()
	cfg2 := cfg
	cfg2.InviteFile = ""
	cfg2.BeforeInviteUnlinkFinal = nil
	cfg2.BeforeInviteRestore = nil
	c2, err := startClient(ctx, cfg2)
	if err != nil {
		t.Fatalf("F2: restart without invite: %v", err)
	}
	defer c2.Close()
	if fake.EnrollCount() != 1 {
		t.Fatalf("F2: restart re-sent enroll (%d)", fake.EnrollCount())
	}
}

func TestS5R7_F3_ExactRegularFileMode0600(t *testing.T) {
	rows := []struct {
		name string
		mode os.FileMode
		ok   bool
	}{
		{"0400", 0o400, false},
		{"0200", 0o200, false},
		{"0700", 0o700, false},
		{"0600", 0o600, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeRV(t)
			cfg := clientCfg(t, fake)
			path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, row.mode)
			cfg.InviteFile = path
			ctx, cancel := ctxTO(t)
			defer cancel()
			c, err := startClient(ctx, cfg)
			if row.ok {
				if err != nil {
					t.Fatalf("F3: mode 0600 rejected: %v", err)
				}
				_ = c.Close()
				return
			}
			requireClass(t, err, ClassInviteFile)
			if fake.RequestCount() != 0 {
				t.Fatalf("F3: mode %s contacted rendezvous: %v", row.name, fake.Paths())
			}
			g := strings.ToLower(err.Error() + guidanceOf(err))
			if !strings.Contains(g, "0600") {
				t.Fatalf("F3: mode %s guidance missing 0600: %v", row.name, err)
			}
		})
	}
}

func TestS5R7_F3_ShredDoesNotUseProcSelfFd(t *testing.T) {
	root := "."
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		needle := string([]byte{'/', 'p', 'r', 'o', 'c', '/', 's', 'e', 'l', 'f', '/', 'f', 'd'})
		if bytes.Contains(data, []byte(needle)) {
			return fmt.Errorf("%s uses %s", path, needle)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func findConsumedNamed(t *testing.T, dir string, want []byte) bool {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".scimux-consumed-") {
			continue
		}
		got, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(got, want) {
			return true
		}
	}
	return false
}

func TestS5R7_F3_WritableShredWithoutProc(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	orig, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer orig.Close()
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("F3: enroll: %v", err)
	}
	defer c.Close()
	if _, err := orig.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(orig)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte(vectorInviteGrouped)) || bytes.Contains(got, []byte(vectorInviteCanon)) {
		t.Fatalf("F3: shred did not clear the validated inode through a writable descriptor: %q", got)
	}
}
