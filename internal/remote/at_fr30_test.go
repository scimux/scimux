package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// AT-FR-30-a: every named enrollment-state row, including injected write
// failures and two processes sharing a state root (Go test helper process).
func TestAT_FR_30_a_StateTable(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()

	t.Run("no-identity", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		c := NewClient(cfg)
		err := c.Start(ctx)
		requireClass(t, err, ClassNeedInvite)
		if fake.RequestCount() != 0 {
			t.Fatalf("absent state contacted rendezvous without invite: %v", fake.Paths())
		}
		if _, err := os.Stat(c.StatePath()); !os.IsNotExist(err) {
			t.Fatalf("absent state wrote a state file: %v", err)
		}
		g := strings.ToLower(guidanceOf(err))
		for _, w := range []string{"invite", "--invite-file", "--invite-stdin"} {
			if !strings.Contains(g, w) {
				t.Errorf("setup guidance missing %q: %q", w, guidanceOf(err))
			}
		}
	})

	t.Run("valid-enrolled", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
		c, err := startClient(ctx, cfg)
		if err != nil {
			t.Fatalf("enroll: %v", err)
		}
		st, err := c.State()
		if err != nil || st != StateEnrolled {
			t.Fatalf("state = %q %v", st, err)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("close enrolled client: %v", err)
		}
		cfg2 := cfg
		cfg2.InviteFile = ""
		c2, err := startClient(ctx, cfg2)
		if err != nil {
			t.Fatalf("restart enrolled: %v", err)
		}
		st2, err := c2.State()
		if err != nil || st2 != StateEnrolled {
			t.Fatalf("restart state = %q %v", st2, err)
		}
	})

	t.Run("key-missing", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		c := NewClient(cfg)
		fix := enrolledFixture(t, "ih_041061050R3GG28A")
		fix.PrivateKey = ""
		fix.Status = StateKeyMissing
		if fix.Status == "" {
			fix.Status = StateEnrolled
		}
		writeState(t, c, PersistedState{
			V:         1,
			Status:    StateEnrolled,
			Handle:    fix.Handle,
			PublicKey: fix.PublicKey,
			Origin:    DefaultOrigin,
		})
		before, err := os.ReadFile(c.StatePath())
		if err != nil {
			t.Fatal(err)
		}
		err = c.Start(ctx)
		requireClass(t, err, ClassKeyMissing)
		after, err := os.ReadFile(c.StatePath())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("key-missing state was overwritten")
		}
		if fake.RequestCount() != 0 {
			t.Fatalf("key-missing contacted rendezvous: %v", fake.Paths())
		}
	})

	t.Run("partial-identity", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		c := NewClient(cfg)
		if err := os.MkdirAll(c.PrivateDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		tmp := c.StatePath() + ".tmp"
		if err := os.WriteFile(tmp, []byte(`{"v":1,"status":"enrolled","public_key":"ab"`), 0o600); err != nil {
			t.Fatal(err)
		}
		err := c.Start(ctx)
		requireClass(t, err, ClassPartialIdentity)
		if _, err := os.Stat(c.StatePath()); err == nil {
			raw, _ := os.ReadFile(c.StatePath())
			var st PersistedState
			if json.Unmarshal(raw, &st) == nil && st.Status == StateEnrolled && st.Handle != "" {
				t.Fatal("partial identity was completed automatically")
			}
		}
		if fake.RequestCount() != 0 {
			t.Fatalf("partial identity contacted rendezvous: %v", fake.Paths())
		}
	})

	t.Run("corrupt-identity", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		c := NewClient(cfg)
		if err := os.MkdirAll(c.PrivateDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		garbage := []byte("this is not json {{{")
		if err := os.WriteFile(c.StatePath(), garbage, 0o600); err != nil {
			t.Fatal(err)
		}
		err := c.Start(ctx)
		requireClass(t, err, ClassCorruptIdentity)
		after, err := os.ReadFile(c.StatePath())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, garbage) {
			t.Fatal("corrupt identity was overwritten")
		}
		if fake.RequestCount() != 0 {
			t.Fatalf("corrupt identity contacted rendezvous: %v", fake.Paths())
		}
	})

	t.Run("enrollment-pending-ambiguous", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		c := NewClient(cfg)
		pub, priv := newEd25519(t)
		writeState(t, c, PersistedState{
			V:          1,
			Status:     StateAmbiguous,
			PublicKey:  fmt.Sprintf("%x", pub),
			PrivateKey: fmt.Sprintf("%x", priv),
			Pending:    &PendingEnrollment{PublicKey: fmt.Sprintf("%x", pub)},
			Origin:     DefaultOrigin,
		})
		before, _ := os.ReadFile(c.StatePath())
		err := c.Start(ctx)
		requireClass(t, err, ClassAmbiguousEnrollment)
		after, _ := os.ReadFile(c.StatePath())
		if !bytes.Equal(before, after) {
			t.Fatal("ambiguous state was overwritten on implicit start")
		}
		if fake.RequestCount() != 0 {
			t.Fatalf("ambiguous restart contacted rendezvous: %v", fake.Paths())
		}
	})

	t.Run("different-invite-does-not-rotate", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
		c, err := startClient(ctx, cfg)
		if err != nil {
			t.Fatalf("enroll: %v", err)
		}
		pub, err := c.PublicKey()
		if err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(c.StatePath())
		if err := c.Close(); err != nil {
			t.Fatalf("close enrolled client: %v", err)
		}
		fake.Issue(otherInvite())
		cfg2 := cfg
		cfg2.InviteFile = writeInviteFile(t, t.TempDir(), otherInvite(), 0o600)
		c2 := NewClient(cfg2)
		err = c2.Start(ctx)
		requireClass(t, err, ClassInviteConflict)
		after, _ := os.ReadFile(c.StatePath())
		if !bytes.Equal(before, after) {
			t.Fatal("different invite silently overwrote enrolled identity")
		}
		pub2, err := NewClient(cfg).PublicKey()
		if err == nil && pub2 != nil && !bytes.Equal(pub, pub2) {
			t.Fatal("different invite rotated the public key")
		}
	})

	t.Run("revoked-installation", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
		c, err := startClient(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		h, err := c.Handle()
		if err != nil {
			t.Fatal(err)
		}
		fake.RevokeHandle(h)
		if err := c.Close(); err != nil {
			t.Fatalf("close enrolled client: %v", err)
		}
		cfg2 := cfg
		cfg2.InviteFile = ""
		c2 := NewClient(cfg2)
		err = c2.Start(ctx)
		requireClass(t, err, ClassRevoked)
		st, err := c2.State()
		if err != nil {
			t.Fatalf("revoked-installation: State: %v", err)
		}
		if st != StateRevoked {
			t.Fatalf("revoked-installation: state = %q, want %s", st, StateRevoked)
		}
	})

	t.Run("persisted-disable-all", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
		c, err := startClient(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.DisableAll(ctx); err != nil {
			t.Fatalf("disable-all: %v", err)
		}
		st, err := c.State()
		if err != nil || st != StateDisabled {
			t.Fatalf("disabled state = %q %v", st, err)
		}
		assertCompleteValidState(t, mustState(t, c.StatePath()))
		if err := c.Close(); err != nil {
			t.Fatalf("close enrolled client: %v", err)
		}
		cfg2 := cfg
		cfg2.InviteFile = ""
		c2 := NewClient(cfg2)
		err = c2.Start(ctx)
		requireClass(t, err, ClassDisabled)
		if err := c2.Reenable(ctx); err != nil {
			t.Fatalf("reenable: %v", err)
		}
		if err := c2.Start(ctx); err != nil {
			t.Fatalf("start after explicit reenable: %v", err)
		}
	})

	t.Run("injected-write-failures", func(t *testing.T) {
		for _, step := range AllWriteSteps() {
			t.Run(step.String(), func(t *testing.T) {
				fake := newFakeRV(t)
				cfg := clientCfg(t, fake)
				cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
				step := step
				cfg.FailWrite = &WriteFault{Step: step}
				inviteBytes, err := os.ReadFile(cfg.InviteFile)
				if err != nil {
					t.Fatal(err)
				}
				c := NewClient(cfg)
				err = c.Start(ctx)
				if err == nil {
					t.Fatalf("Start succeeded despite injected failure at %s", step)
				}
				if errors.Is(err, ErrUnimplemented) {
					t.Fatalf("unimplemented (injected %s)", step)
				}
				if fake.EnrollCount() != 0 {
					t.Fatalf("injected %s contacted enroll (%d); invite must be preserved until an enrollment request can occur", step, fake.EnrollCount())
				}
				gotInvite, err := os.ReadFile(cfg.InviteFile)
				if err != nil {
					t.Fatalf("injected %s: original invite file missing: %v", step, err)
				}
				if !bytes.Equal(gotInvite, inviteBytes) {
					t.Fatalf("injected %s: invite file mutated before any enrollment request", step)
				}
				_, raw, ok := readStateFile(t, c.StatePath())
				if ok {
					var st PersistedState
					if json.Unmarshal(raw, &st) != nil {
						t.Fatalf("mixed/truncated state after %s: %q", step, raw)
					}
					assertCompleteValidState(t, st)
				}
				scanRootForInvite(t, c.PrivateDir(), vectorInviteGrouped)
			})
		}
	})
}

// State() projects load failures onto the same FR-30 state vocabulary used by
// successfully loaded records. Keep the projection exhaustive: callers use the
// state even when the accompanying diagnostic error is non-nil.
func TestEnrollmentErrorClassStateProjection(t *testing.T) {
	tests := []struct {
		class Class
		want  EnrollmentState
	}{
		{ClassCorruptIdentity, StateCorrupt},
		{ClassPartialIdentity, StatePartial},
		{ClassKeyMissing, StateKeyMissing},
		{ClassAmbiguousEnrollment, StateAmbiguous},
		{ClassRevoked, StateRevoked},
		{ClassDisabled, StateDisabled},
		{Class("future-class"), ""},
	}
	for _, tt := range tests {
		t.Run(string(tt.class), func(t *testing.T) {
			if got := eClassState(tt.class); got != tt.want {
				t.Fatalf("eClassState(%q) = %q, want %q", tt.class, got, tt.want)
			}
		})
	}
}

func TestAT_FR_30_a_TwoProcessesShareStateRoot(t *testing.T) {
	role := os.Getenv("SCIMUX_S5_HELPER")
	if role == "A" || role == "B" {
		runStateLockHelper(role)
		return
	}

	fake := newFakeRV(t)
	dir := t.TempDir()
	invitePath := writeInviteFile(t, dir, vectorInviteGrouped, 0o600)
	held := filepath.Join(t.TempDir(), "lock-held")
	release := filepath.Join(t.TempDir(), "lock-release")

	startHelper := func(role, heldPath, releasePath string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAT_FR_30_a_TwoProcessesShareStateRoot$", "-test.v=false", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"SCIMUX_S5_HELPER="+role,
			"SCIMUX_S5_STATE="+dir,
			"SCIMUX_S5_INVITE_FILE="+invitePath,
			"SCIMUX_S5_RV="+fake.URL(),
			"SCIMUX_S5_LOCK_HELD="+heldPath,
			"SCIMUX_S5_LOCK_RELEASE="+releasePath,
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd
	}
	code := func(err error) int {
		if err == nil {
			return 0
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return -1
	}

	a := startHelper("A", held, release)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	doneA := make(chan error, 1)
	go func() { doneA <- a.Wait() }()

	announced := make(chan struct{})
	go func() {
		for {
			if _, err := os.Stat(held); err == nil {
				close(announced)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	select {
	case err := <-doneA:
		t.Fatalf("process A exited before announcing lock ownership: exit %d", code(err))
	case <-announced:
	case <-time.After(3 * time.Second):
		t.Fatal("diagnostic timeout: process A neither announced lock ownership nor exited")
	}

	b := startHelper("B", "", "")
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	errB := b.Wait()
	if got := code(errB); got != 3 {
		t.Fatalf("process B exit %d, want ClassStateLock (3); dual lock-fail is not accepted", got)
	}

	if err := os.WriteFile(release, []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	errA := waitHelper(t, doneA, "process A")
	if got := code(errA); got != 0 {
		t.Fatalf("process A exit %d, want 0 (completed while holding the lock)", got)
	}

	st, raw, ok := readStateFile(t, filepath.Join(dir, PrivateDirName, StateFileName))
	if !ok {
		t.Fatal("missing final state after A completed")
	}
	if json.Unmarshal(raw, new(PersistedState)) != nil {
		t.Fatalf("corrupt final state: %q", raw)
	}
	assertCompleteValidState(t, st)
	if st.Status != StateEnrolled {
		t.Fatalf("final status = %q, want enrolled", st.Status)
	}
}

func waitHelper(t *testing.T, done <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("diagnostic timeout: %s did not finish after release", name)
		return nil
	}
}

func runStateLockHelper(role string) {
	dir := os.Getenv("SCIMUX_S5_STATE")
	invite := os.Getenv("SCIMUX_S5_INVITE_FILE")
	url := os.Getenv("SCIMUX_S5_RV")
	cfg := Config{
		DataDir:         dir,
		Remote:          true,
		Origin:          DefaultOrigin,
		InviteFile:      invite,
		RendezvousURL:   url,
		LockHeldFile:    os.Getenv("SCIMUX_S5_LOCK_HELD"),
		LockReleaseFile: os.Getenv("SCIMUX_S5_LOCK_RELEASE"),
	}
	if role == "B" {
		cfg.LockHeldFile = ""
		cfg.LockReleaseFile = ""
	}
	c := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := c.Start(ctx)
	if errors.Is(err, ErrUnimplemented) {
		os.Exit(2)
	}
	if classOf(err) == ClassStateLock {
		os.Exit(3)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper %s: %v\n", role, err)
		os.Exit(4)
	}
	os.Exit(0)
}
