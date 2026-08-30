package remote

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Four tests characterize the invite-consume no-replace guarantee across
// the RENAME_NOREPLACE → os.Root.Link rebuild (occupied name, restore,
// retry, and the InviteLink seam). A fifth refuses a non-regular
// quarantine name: os.Root follows in-root symlinks, so that case is
// an anomaly to report, not an ordinary identity mismatch.

// repeatByte yields the same byte forever. Setting cfg.Rand to
// repeatByte(0xAB) makes every quarantine candidate the same name,
// independent of how many bytes anything else drew first.
type repeatByte byte

func (b repeatByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

// countByte fills each Read with a byte that increments per call: first
// call all 0x01, second all 0x02, and so on. consumeInviteName draws
// inviteQuarantineEntropy bytes per attempt through a single io.ReadFull,
// and this reader returns a full buffer every time, so one Read is one
// attempt. If that ever stops being true the names shift and this test
// is the thing that notices.
type countByte struct{ n byte }

func (c *countByte) Read(p []byte) (int, error) {
	c.n++
	for i := range p {
		p[i] = c.n
	}
	return len(p), nil
}

func consumeOwnedInvite(t *testing.T, cfg Config, path string) error {
	t.Helper()
	c := NewClient(cfg)
	if _, err := c.readInviteFile(path); err != nil {
		t.Fatalf("readInviteFile: %v", err)
	}
	return c.eraseInvite(inviteSrc{file: path}, "")
}

func TestInviteQuarantineDoesNotReplaceExistingName(t *testing.T) {
	dir := t.TempDir()
	path := writeInviteFile(t, dir, "invite", 0o600)
	name := inviteQuarantinePrefix + hex.EncodeToString(bytes.Repeat([]byte{0xAB}, inviteQuarantineEntropy))
	qpath := filepath.Join(dir, name)
	bystander := []byte("quarantine-bystander-must-survive\n")
	if err := os.WriteFile(qpath, bystander, 0o600); err != nil {
		t.Fatal(err)
	}

	err := consumeOwnedInvite(t, Config{Rand: repeatByte(0xAB)}, path)
	requireClass(t, err, ClassInviteFile)
	if !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("error does not unwrap to EEXIST: %v", err)
	}

	got, rerr := os.ReadFile(qpath)
	if rerr != nil {
		t.Fatalf("bystander missing after consume: %v", rerr)
	}
	if !bytes.Equal(got, bystander) {
		t.Fatalf("bystander overwritten: got %q, want %q", got, bystander)
	}
}

func TestInviteRestoreDoesNotReplaceExistingName(t *testing.T) {
	dir := t.TempDir()
	path := writeInviteFile(t, dir, "invite", 0o600)
	occupant := []byte("restore-occupant-must-survive\n")

	cfg := Config{}
	cfg.BeforeInviteUnlinkFinal = func() {
		tmp := filepath.Join(dir, "replacement")
		if err := os.WriteFile(tmp, []byte("replacement-inode\n"), 0o600); err != nil {
			t.Errorf("replacement write: %v", err)
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Errorf("replacement rename: %v", err)
		}
	}
	cfg.BeforeInviteRestore = func() {
		if err := os.WriteFile(path, occupant, 0o600); err != nil {
			t.Errorf("restore occupant write: %v", err)
		}
	}

	err := consumeOwnedInvite(t, cfg, path)
	if err == nil {
		t.Fatal("eraseInvite succeeded; restore overwrote an occupied name or never reached the restore path")
	}
	if !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("error does not unwrap to EEXIST: %v", err)
	}

	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("restore occupant missing: %v", rerr)
	}
	if !bytes.Equal(got, occupant) {
		t.Fatalf("restore occupant overwritten: got %q, want %q", got, occupant)
	}
}

func TestInviteQuarantineRetriesPastAnOccupiedName(t *testing.T) {
	dir := t.TempDir()
	path := writeInviteFile(t, dir, "invite", 0o600)
	first := inviteQuarantinePrefix + hex.EncodeToString(bytes.Repeat([]byte{0x01}, inviteQuarantineEntropy))
	second := inviteQuarantinePrefix + hex.EncodeToString(bytes.Repeat([]byte{0x02}, inviteQuarantineEntropy))
	qpath := filepath.Join(dir, first)
	bystander := []byte("retry-bystander-must-survive\n")
	if err := os.WriteFile(qpath, bystander, 0o600); err != nil {
		t.Fatal(err)
	}

	err := consumeOwnedInvite(t, Config{Rand: &countByte{}}, path)
	if err != nil {
		t.Fatalf("eraseInvite after collision retry: %v", err)
	}

	got, rerr := os.ReadFile(qpath)
	if rerr != nil {
		t.Fatalf("bystander missing after retry: %v", rerr)
	}
	if !bytes.Equal(got, bystander) {
		t.Fatalf("bystander overwritten: got %q, want %q", got, bystander)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invite file still at original path")
	}
	if _, err := os.Stat(filepath.Join(dir, second)); !os.IsNotExist(err) {
		t.Fatal("second candidate still exists; retry did not claim and clean it")
	}
}

// TestInviteLinkFaultSeamIsHonoured pins that Config.InviteLink is
// actually consulted during invite consume. A declared fault-injection
// seam that no production code reads is not a seam, and two existing
// tests set this one believing it does something.
func TestInviteLinkFaultSeamIsHonoured(t *testing.T) {
	dir := t.TempDir()
	path := writeInviteFile(t, dir, "invite", 0o600)
	sentinel := errors.New("injected invite link failure")

	err := consumeOwnedInvite(t, Config{
		InviteLink: func(string, string) error { return sentinel },
	}, path)
	if err == nil {
		t.Fatal("eraseInvite succeeded; cfg.InviteLink was not consulted")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("eraseInvite error %v does not wrap the InviteLink sentinel", err)
	}
}

func TestInviteQuarantineRefusesASymlinkedName(t *testing.T) {
	dir := t.TempDir()
	path := writeInviteFile(t, dir, "invite", 0o600)
	target := filepath.Join(dir, "other")
	payload := []byte("symlink-target-must-survive\n")
	if err := os.WriteFile(target, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	err := consumeOwnedInvite(t, Config{
		InviteLink: func(_, newname string) error {
			return os.Symlink("other", filepath.Join(dir, newname))
		},
	}, path)
	if err == nil {
		t.Fatal("eraseInvite succeeded; a symlink at the quarantine name was treated as an ordinary mismatch")
	}
	if !errors.Is(err, errInviteQuarantineNotRegular) {
		t.Fatalf("error does not unwrap to errInviteQuarantineNotRegular: %v", err)
	}

	got, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatalf("symlink target missing: %v", rerr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("symlink target overwritten: got %q, want %q", got, payload)
	}
}
