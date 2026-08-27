package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestS5R4_F1_WaitProtocolSignedRIDAndChallengeChain(t *testing.T) {
	srv := newStrictWaitRV(t)
	cfg := r3ClientCfg(t, srv.URL, srv.Client)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	sched := newDelaySched()
	cfg.Scheduler = sched
	cfg.Clock = newClock(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))
	cfg.RNG = constRNG{v: 0.5}
	cfg.Backoff = BackoffConfig{Initial: 50 * time.Millisecond, Max: 800 * time.Millisecond, Factor: 2, Jitter: 0.2, SuccessFor: 5 * time.Second}
	ctx, cancel := ctxTO(t)
	defer cancel()
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("F1: start: %v", err)
	}
	defer c.Close()
	d, err := c.RegisterDevice(ctx, DeviceRecord{ID: "phone", PubKey: append([]byte(nil), testPhonePubKey...)})
	if err != nil {
		t.Fatalf("F1: register: %v", err)
	}

	waitForSched(t, sched, 2*time.Second)
	sched.FireAll()
	waitForWaits(t, srv, 1, 2*time.Second)
	first := srv.LastWait()
	if first.Reject != "" {
		t.Fatalf("F1: first wait rejected: %s body=%s", first.Reject, first.Body)
	}
	if first.ID != d.RID {
		t.Fatalf("F1: wait id = %q, want registered RID %q", first.ID, d.RID)
	}
	if first.Sig == "" || first.Challenge == "" {
		t.Fatal("F1: wait missing challenge or signature")
	}

	waitForSched(t, sched, 2*time.Second)
	sched.FireAll()
	waitForWaits(t, srv, 2, 2*time.Second)
	second := srv.LastWait()
	if second.Reject != "" {
		t.Fatalf("F1: second wait rejected: %s", second.Reject)
	}
	if second.Challenge != first.NextChallenge {
		t.Fatalf("F1: challenge chain broken: second=%q want %q", second.Challenge, first.NextChallenge)
	}
	if second.ID != d.RID {
		t.Fatalf("F1: second wait id = %q, want %q", second.ID, d.RID)
	}
}

func TestS5R4_F1_NoDeviceDoesNotEmitHandleOnlyWait(t *testing.T) {
	srv := newStrictWaitRV(t)
	cfg := r3ClientCfg(t, srv.URL, srv.Client)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	sched := newDelaySched()
	cfg.Scheduler = sched
	cfg.Backoff = BackoffConfig{Initial: 50 * time.Millisecond, Max: 800 * time.Millisecond, Factor: 2, Jitter: 0.2, SuccessFor: 5 * time.Second}
	ctx, cancel := ctxTO(t)
	defer cancel()
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("F1: start: %v", err)
	}
	defer c.Close()
	waitForSched(t, sched, 2*time.Second)
	sched.FireAll()
	time.Sleep(30 * time.Millisecond)
	if srv.WaitCount() != 0 {
		t.Fatalf("F1: newly enrolled install with no device emitted %d /v1/wait", srv.WaitCount())
	}
}

func TestS5R4_F1_WaitCancellationPreserved(t *testing.T) {
	srv := newStrictWaitRV(t)
	cfg := r3ClientCfg(t, srv.URL, srv.Client)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	sched := newDelaySched()
	cfg.Scheduler = sched
	cfg.Backoff = BackoffConfig{Initial: 50 * time.Millisecond, Max: 800 * time.Millisecond, Factor: 2, Jitter: 0.2, SuccessFor: 5 * time.Second}
	ctx, cancel := ctxTO(t)
	defer cancel()
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("F1: start: %v", err)
	}
	srv.FailWaits(true)
	d, err := c.RegisterDevice(ctx, DeviceRecord{ID: "phone", PubKey: append([]byte(nil), testPhonePubKey...)})
	if err != nil {
		t.Fatal(err)
	}
	_ = d
	waitForSched(t, sched, 2*time.Second)
	n := srv.WaitCount()
	if err := c.StopRendezvous(); err != nil {
		t.Fatal(err)
	}
	sched.FireAll()
	time.Sleep(20 * time.Millisecond)
	if srv.WaitCount() > n+1 {
		t.Fatal("F1: wait retried after StopRendezvous")
	}
}

func TestS5R4_F1_SustainedSuccessResetInRVLoop(t *testing.T) {
	srv := newStrictWaitRV(t)
	cfg := r3ClientCfg(t, srv.URL, srv.Client)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	sched := newDelaySched()
	clock := newClock(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))
	cfg.Scheduler = sched
	cfg.Clock = clock
	cfg.RNG = constRNG{v: 0.5}
	cfg.Backoff = BackoffConfig{Initial: 100 * time.Millisecond, Max: 1600 * time.Millisecond, Factor: 2, Jitter: 0.2, SuccessFor: 5 * time.Second}
	ctx, cancel := ctxTO(t)
	defer cancel()
	srv.FailWaits(true)
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("F1: start: %v", err)
	}
	defer c.Close()
	waitForSched(t, sched, 2*time.Second)
	d, err := c.RegisterDevice(ctx, DeviceRecord{ID: "phone", PubKey: append([]byte(nil), testPhonePubKey...)})
	if err != nil {
		t.Fatal(err)
	}
	_ = d

	for i := 0; i < 3; i++ {
		waitForSched(t, sched, 2*time.Second)
		sched.FireAll()
		waitForWaits(t, srv, i+1, 2*time.Second)
	}
	afterFail := sched.LastDelay()
	if afterFail != 400*time.Millisecond {
		t.Fatalf("F1: delay after 3 failures = %s, want 400ms", afterFail)
	}

	srv.FailWaits(false)
	waitForSched(t, sched, 2*time.Second)
	sched.FireAll()
	waitForWaits(t, srv, 4, 2*time.Second)

	clock.Advance(cfg.Backoff.SuccessFor)
	srv.FailWaits(true)
	waitForSched(t, sched, 2*time.Second)
	sched.FireAll()
	waitForWaits(t, srv, 5, 2*time.Second)
	got := sched.LastDelay()
	if got != cfg.Backoff.Initial {
		t.Fatalf("F1: retry after sustained success = %s, want Initial %s", got, cfg.Backoff.Initial)
	}
}

func TestS5R4_F2_MutationRequiresOwnership(t *testing.T) {
	role := os.Getenv("SCIMUX_S5_R4_HELPER")
	if role != "" {
		runR4LockHelper(role)
		return
	}
	fake := newFakeRV(t)
	dir := t.TempDir()
	invitePath := writeInviteFile(t, dir, vectorInviteGrouped, 0o600)
	started := filepath.Join(t.TempDir(), "started")
	release := filepath.Join(t.TempDir(), "release")
	startHelper := func(role string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestS5R4_F2_MutationRequiresOwnership$", "-test.v=false", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"SCIMUX_S5_R4_HELPER="+role,
			"SCIMUX_S5_STATE="+dir,
			"SCIMUX_S5_INVITE_FILE="+invitePath,
			"SCIMUX_S5_RV="+fake.URL(),
			"SCIMUX_S5_STARTED="+started,
			"SCIMUX_S5_RELEASE="+release,
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
	a := startHelper("A")
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	doneA := make(chan error, 1)
	go func() { doneA <- a.Wait() }()
	announced := make(chan struct{})
	go func() {
		for {
			if _, err := os.Stat(started); err == nil {
				close(announced)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	select {
	case err := <-doneA:
		t.Fatalf("F2: process A exited before reporting Start complete: exit %d", code(err))
	case <-announced:
	case <-time.After(8 * time.Second):
		t.Fatal("F2: diagnostic timeout waiting for A Start")
	}

	statePath := filepath.Join(dir, PrivateDirName, StateFileName)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"B-register", "B-revoke", "B-disable", "B-reenable"} {
		b := startHelper(op)
		errB := b.Run()
		if code(errB) != 3 {
			t.Fatalf("F2: %s while A held lock exit %d, want ClassStateLock (3)", op, code(errB))
		}
		after, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("F2: %s mutated state while A held the lock", op)
		}
	}
	if err := os.WriteFile(release, []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitHelper(t, doneA, "process A"); code(err) != 0 {
		t.Fatalf("F2: A exit %d, want 0", code(err))
	}
	b2 := startHelper("B-disable-after")
	if err := b2.Run(); err != nil {
		t.Fatalf("F2: DisableAll after A close: %v", err)
	}
	st, _, ok := readStateFile(t, statePath)
	if !ok || st.Status != StateDisabled {
		t.Fatalf("F2: after A close DisableAll status = %+v", st)
	}
}

func TestS5R4_F3_MatchingKeysAccepted(t *testing.T) {
	pub, priv := newEd25519(t)
	pubHex := hex.EncodeToString(pub)
	privHex := hex.EncodeToString(priv)
	rows := []struct {
		name   string
		status EnrollmentState
		want   Class
	}{
		{"enrolled", StateEnrolled, ""},
		{"disabled", StateDisabled, ClassDisabled},
		{"revoked", StateRevoked, ClassRevoked},
		{"pending", StateAmbiguous, ClassAmbiguousEnrollment},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cfg := clientCfg(t, newFakeRV(t))
			if row.status == StateEnrolled {
				cfg.HTTPClient = unreachableClient()
				cfg.RendezvousURL = "http://127.0.0.1:1"
			}
			c := NewClient(cfg)
			st := PersistedState{
				V:          1,
				Status:     row.status,
				Handle:     "ih_041061050R3GG28A",
				PublicKey:  pubHex,
				PrivateKey: privHex,
				Origin:     DefaultOrigin,
			}
			if row.status == StateAmbiguous {
				st.Handle = ""
				st.Pending = &PendingEnrollment{PublicKey: pubHex}
			}
			writeState(t, c, st)
			before, _ := os.ReadFile(c.StatePath())
			err := c.Start(context.Background())
			after, _ := os.ReadFile(c.StatePath())
			if row.want == "" {
				if err != nil {
					t.Fatalf("F3: matching %s keys rejected: %v", row.name, err)
				}
			} else {
				requireClass(t, err, row.want)
			}
			if row.want != "" && !bytes.Equal(before, after) {
				t.Fatalf("F3: matching %s keys rewrote evidence", row.name)
			}
		})
	}
}

func TestS5R4_F4_DeviceMutationValidation(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	c, _, d1, _, _ := enrollTwoDevices(t)
	goodKey := append([]byte(nil), testPhonePubKey...)

	t.Run("empty-id", func(t *testing.T) {
		assertUnchanged(t, c, func() error {
			_, err := c.RegisterDevice(ctx, DeviceRecord{ID: "", PubKey: goodKey})
			return err
		}, ClassUnauthorized)
	})
	t.Run("duplicate-id", func(t *testing.T) {
		assertUnchanged(t, c, func() error {
			_, err := c.RegisterDevice(ctx, DeviceRecord{ID: d1.ID, PubKey: goodKey})
			return err
		}, ClassUnauthorized)
	})
	t.Run("malformed-key", func(t *testing.T) {
		assertUnchanged(t, c, func() error {
			_, err := c.RegisterDevice(ctx, DeviceRecord{ID: "watch", PubKey: []byte("short")})
			return err
		}, ClassUnauthorized)
	})
	t.Run("unknown-rid", func(t *testing.T) {
		assertUnchanged(t, c, func() error {
			return c.AddWaiter(&fakeWaiter{id: strings.Repeat("ef", 32)})
		}, ClassNotFound)
	})
	t.Run("revoked-rid", func(t *testing.T) {
		if err := c.RevokeDevice(ctx, d1.ID); err != nil {
			t.Fatal(err)
		}
		assertUnchanged(t, c, func() error {
			return c.AddWaiter(&fakeWaiter{id: d1.RID})
		}, ClassRevoked)
	})
	t.Run("revoked-installation", func(t *testing.T) {
		fake := newFakeRV(t)
		cfg := clientCfg(t, fake)
		cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
		c2, err := startClient(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		h, err := c2.Handle()
		if err != nil {
			t.Fatal(err)
		}
		fake.RevokeHandle(h)
		_ = c2.Close()
		cfg.InviteFile = ""
		c3 := NewClient(cfg)
		if err := c3.Start(ctx); err == nil || classOf(err) != ClassRevoked {
			t.Fatalf("F4: want revoked start, got %v", err)
		}
		assertUnchanged(t, c3, func() error {
			_, err := c3.RegisterDevice(ctx, DeviceRecord{ID: "phone", PubKey: goodKey})
			return err
		}, ClassRevoked)
	})
}

func TestS5R4_F5_InviteConflictPreservesInvite(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	other := otherInvite()
	path := writeInviteFile(t, t.TempDir(), other, 0o600)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fake.Issue(other)
	cfg2 := cfg
	cfg2.InviteFile = path
	err = NewClient(cfg2).Start(ctx)
	requireClass(t, err, ClassInviteConflict)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("F5: invite-conflict removed the invite: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("F5: invite-conflict mutated invite bytes: %q -> %q", before, after)
	}
}

// enrollNew persists the new identity before it posts, so a disk that
// fails at that moment must leave the credential unspent. The reachable
// installation this happens on is a revoked one being lifted by a new
// invite: the write fault fires on the ambiguous pre-post commit, and the
// invite has to survive it.
func TestS5R4_F5_LiftPersistFailurePreservesInvite(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	seedRevoked(t, NewClient(cfg))

	other := otherInvite()
	fake.Issue(other)
	path := writeInviteFile(t, t.TempDir(), other, 0o600)
	before, _ := os.ReadFile(path)
	cfg2 := cfg
	cfg2.InviteFile = path
	cfg2.FailWrite = &WriteFault{Step: WriteTempCreate, When: StateAmbiguous}
	nEnroll := fake.EnrollCount()

	err := NewClient(cfg2).Start(ctx)
	if err == nil {
		t.Fatal("F5: lift persist failure succeeded")
	}
	// The pre-flight probe runs before this, so the network was contacted;
	// what must not have happened is the post that spends the code.
	if fake.EnrollCount() != nEnroll {
		t.Fatalf("F5: lift posted the invite before persisting the identity (enrolls %d -> %d)", nEnroll, fake.EnrollCount())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("F5: lift failure removed invite: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("F5: lift failure mutated invite: %q -> %q", before, after)
	}
}

func TestS5R4_F5_CouldHaveRedeemedErasesInvite(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("F5: successful enrollment left invite at %s", path)
	}
}

func TestS5R4_F5_CouldHaveRedeemedLostResponseErasesInvite(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	fake.DropEnrollResponse(true)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	err := NewClient(cfg).Start(ctx)
	if err == nil {
		t.Fatal("F5: lost enroll response succeeded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("F5: could-have-redeemed enrollment left the invite")
	}
}

func TestS5R4_F5_ReplacementAtEraseBoundaryPreserved(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	replacement := []byte("unrelated-replacement-file\n")
	cfg.BeforeInviteUnlink = func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Errorf("replace remove: %v", err)
		}
		if err := os.WriteFile(path, replacement, 0o600); err != nil {
			t.Errorf("replace write: %v", err)
		}
	}
	_, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("F5: enroll: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("F5: replacement removed: %v", err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("F5: replacement overwritten: %q", got)
	}
}

func TestS5R4_F6_TTYFailures(t *testing.T) {
	rows := []struct {
		name string
		tty  *failTTY
	}{
		{"disable", &failTTY{disableErr: errors.New("disable failed")}},
		{"prompt", &failTTY{line: vectorInviteGrouped, promptErr: errors.New("prompt failed")}},
		{"read", &failTTY{readErr: errors.New("read failed")}},
		{"restore", &failTTY{line: vectorInviteGrouped, restoreErr: errors.New("restore failed")}},
		{"read-plus-restore", &failTTY{readErr: errors.New("read failed"), restoreErr: errors.New("restore failed")}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cfg := clientCfg(t, newFakeRV(t))
			cfg.Terminal = row.tty
			err := NewClient(cfg).Start(context.Background())
			if err == nil {
				t.Fatalf("F6: %s succeeded", row.name)
			}
			if errors.Is(err, ErrUnimplemented) {
				t.Fatal("F6: unimplemented")
			}
			g := strings.ToLower(err.Error() + guidanceOf(err))
			for _, w := range []string{"invite", "--invite-file", "--invite-stdin"} {
				if !strings.Contains(g, w) {
					t.Errorf("F6: %s guidance missing %q: %v", row.name, w, err)
				}
			}
			if row.tty.restoreCount == 0 {
				t.Fatalf("F6: %s did not attempt restore", row.name)
			}
			if row.name == "read-plus-restore" {
				if !strings.Contains(g, "read failed") || !strings.Contains(g, "restore failed") {
					t.Fatalf("F6: read-plus-restore did not surface both errors: %v", err)
				}
			}
		})
	}
}

func TestS5R4_F6_RestoreEchoNotMarkedBeforeIoctl(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	term := &ownerTerminal{f: w}
	err1 := term.RestoreEcho()
	if err1 == nil {
		t.Fatal("F6: RestoreEcho succeeded on a pipe")
	}
	if term.restored {
		t.Fatal("F6: RestoreEcho marked restored before ioctl succeeded")
	}
	_ = w.Close()
}

func TestS5R4_F6_InviteFileAndStdinBypassTTY(t *testing.T) {
	t.Run("invite-file", func(t *testing.T) {
		cfg := clientCfg(t, newFakeRV(t))
		cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
		opened := 0
		cfg.NewTerminal = func() (Terminal, error) {
			opened++
			return &failTTY{readErr: errors.New("tty should not open")}, nil
		}
		if _, err := startClient(context.Background(), cfg); err != nil {
			t.Fatalf("F6: --invite-file: %v", err)
		}
		if opened != 0 {
			t.Fatal("F6: --invite-file opened TTY")
		}
	})
	t.Run("invite-stdin", func(t *testing.T) {
		cfg := clientCfg(t, newFakeRV(t))
		cfg.InviteStdin = true
		cfg.Stdin = strings.NewReader(vectorInviteGrouped + "\n")
		opened := 0
		cfg.NewTerminal = func() (Terminal, error) {
			opened++
			return &failTTY{readErr: errors.New("tty should not open")}, nil
		}
		if _, err := startClient(context.Background(), cfg); err != nil {
			t.Fatalf("F6: --invite-stdin: %v", err)
		}
		if opened != 0 {
			t.Fatal("F6: --invite-stdin opened TTY")
		}
	})
}

func TestS5R4_F7_HostileAdmissionResponses(t *testing.T) {
	handle := "ih_041061050R3GG28A"
	okBody := fmt.Sprintf("{\"handle\":%q,\"v\":1}\n", handle)
	rows := []struct {
		name string
		path string
		fn   func(http.ResponseWriter, *http.Request)
	}{
		{"enroll-wrong-v", "/v1/enroll", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, fmt.Sprintf("{\"handle\":%q,\"v\":2}\n", handle))
		}},
		{"enroll-missing-v", "/v1/enroll", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, fmt.Sprintf("{\"handle\":%q}\n", handle))
		}},
		{"enroll-malformed-handle", "/v1/enroll", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"handle":"not-a-handle","v":1}`+"\n")
		}},
		{"enroll-wrong-content-type", "/v1/enroll", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, okBody)
		}},
		{"enroll-oversized", "/v1/enroll", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"handle":"`+handle+`","v":1,"pad":"`+strings.Repeat("a", 3000)+`"}`)
		}},
		{"verify-nonempty-204", "/v1/verify", func(w http.ResponseWriter, r *http.Request) {
			writeRawStatus(t, w, http.StatusNoContent, "not empty")
		}},
		{"verify-oversized-204", "/v1/verify", func(w http.ResponseWriter, r *http.Request) {
			writeRawStatus(t, w, http.StatusNoContent, strings.Repeat("x", 128))
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/enroll" && row.path == "/v1/enroll":
					row.fn(w, r)
				case r.URL.Path == "/v1/enroll":
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, okBody)
				case r.URL.Path == "/v1/challenge":
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"challenge":"`+strings.Repeat("22", 32)+`","v":1}`+"\n")
				case r.URL.Path == "/v1/verify" && row.path == "/v1/verify":
					row.fn(w, r)
				case r.URL.Path == "/v1/verify":
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			cfg := r3ClientCfg(t, srv.URL, srv.Client())
			cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
			c := NewClient(cfg)
			err := c.Start(context.Background())
			if err == nil {
				t.Fatalf("F7: %s accepted", row.name)
			}
			st, _ := c.State()
			if st == StateRevoked || classOf(err) == ClassRevoked {
				t.Fatalf("F7: %s persisted/classified revoked", row.name)
			}
			if strings.HasPrefix(row.name, "enroll-") && st == StateEnrolled {
				t.Fatalf("F7: %s persisted enrolled", row.name)
			}
		})
	}
}

func TestS5R4_F8_DeliverLinearizableIndependentOfChannel(t *testing.T) {
	c, _, d1, _, _ := enrollTwoDevices(t)
	ch := &alwaysOKChannel{started: make(chan struct{}), release: make(chan struct{})}
	if err := c.AttachChannel(d1.ID, ch); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := ctxTO(t)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- c.Deliver(ctx, d1.ID, []byte("payload")) }()
	select {
	case <-ch.started:
	case err := <-errc:
		t.Fatalf("Deliver returned before send boundary: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Deliver never reached send boundary")
	}
	if err := c.RevokeDevice(ctx, d1.ID); err != nil {
		t.Fatal(err)
	}
	close(ch.release)
	if err := <-errc; err == nil {
		t.Fatal("F8: Deliver reported success after RevokeDevice returned")
	}
}

func runR4LockHelper(role string) {
	dir := os.Getenv("SCIMUX_S5_STATE")
	cfg := Config{
		DataDir:       dir,
		Remote:        true,
		Origin:        DefaultOrigin,
		InviteFile:    os.Getenv("SCIMUX_S5_INVITE_FILE"),
		RendezvousURL: os.Getenv("SCIMUX_S5_RV"),
	}
	c := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	switch role {
	case "A":
		if err := c.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "r4 helper A start: %v\n", err)
			os.Exit(4)
		}
		if _, err := c.RegisterDevice(ctx, DeviceRecord{ID: "phone", PubKey: bytes.Repeat([]byte{0x11}, ed25519.PublicKeySize)}); err != nil {
			fmt.Fprintf(os.Stderr, "r4 helper A register: %v\n", err)
			os.Exit(4)
		}
		_ = os.WriteFile(os.Getenv("SCIMUX_S5_STARTED"), []byte("ok\n"), 0o600)
		release := os.Getenv("SCIMUX_S5_RELEASE")
		for {
			if _, err := os.Stat(release); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		_ = c.Close()
		os.Exit(0)
	}
	cfg.InviteFile = ""
	c = NewClient(cfg)
	var err error
	switch role {
	case "B-register":
		_, err = c.RegisterDevice(ctx, DeviceRecord{ID: "tablet", PubKey: bytes.Repeat([]byte{0x22}, ed25519.PublicKeySize)})
	case "B-revoke":
		err = c.RevokeDevice(ctx, "phone")
	case "B-disable", "B-disable-after":
		err = c.DisableAll(ctx)
	case "B-reenable":
		err = c.Reenable(ctx)
	default:
		os.Exit(4)
	}
	if classOf(err) == ClassStateLock {
		os.Exit(3)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "r4 helper %s: %v\n", role, err)
		os.Exit(4)
	}
	os.Exit(0)
}

func assertUnchanged(t *testing.T, c *Client, op func() error, want Class) {
	t.Helper()
	beforeDev, err := c.Devices()
	if err != nil {
		t.Fatal(err)
	}
	beforeDisk, _ := os.ReadFile(c.StatePath())
	err = op()
	requireClass(t, err, want)
	afterDev, err := c.Devices()
	if err != nil {
		t.Fatal(err)
	}
	if !deviceListEqual(beforeDev, afterDev) {
		t.Fatalf("F4: memory devices changed: before=%+v after=%+v", beforeDev, afterDev)
	}
	afterDisk, _ := os.ReadFile(c.StatePath())
	if !bytes.Equal(beforeDisk, afterDisk) {
		t.Fatalf("F4: disk state changed:\n%s\n---\n%s", beforeDisk, afterDisk)
	}
}

func deviceListEqual(a, b []DeviceRecord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].RID != b[i].RID || !bytes.Equal(a[i].PubKey, b[i].PubKey) {
			return false
		}
	}
	return true
}

type failTTY struct {
	line                  string
	disableErr, promptErr error
	readErr, restoreErr   error
	restoreCount          int
}

func (t *failTTY) DisableEcho() error { return t.disableErr }
func (t *failTTY) RestoreEcho() error {
	t.restoreCount++
	return t.restoreErr
}
func (t *failTTY) EchoEnabled() bool { return false }
func (t *failTTY) ReadLine() (string, error) {
	if t.readErr != nil {
		return "", t.readErr
	}
	return t.line, nil
}
func (t *failTTY) WritePrompt([]byte) error { return t.promptErr }

type alwaysOKChannel struct {
	started, release chan struct{}
	closed           atomic.Bool
	once             sync.Once
}

func (c *alwaysOKChannel) Close() error { c.closed.Store(true); return nil }
func (c *alwaysOKChannel) Closed() bool { return c.closed.Load() }
func (c *alwaysOKChannel) Send([]byte) error {
	c.once.Do(func() { close(c.started) })
	<-c.release
	return nil
}

type delaySched struct {
	mu      sync.Mutex
	pending []func()
	delays  []time.Duration
}

func newDelaySched() *delaySched { return &delaySched{} }

func (s *delaySched) After(d time.Duration, fn func()) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delays = append(s.delays, d)
	s.pending = append(s.pending, fn)
	idx := len(s.pending) - 1
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if idx < len(s.pending) {
			s.pending[idx] = nil
		}
	}
}

func (s *delaySched) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, fn := range s.pending {
		if fn != nil {
			n++
		}
	}
	return n
}

func (s *delaySched) LastDelay() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.delays) == 0 {
		return 0
	}
	return s.delays[len(s.delays)-1]
}

func (s *delaySched) FireAll() {
	s.mu.Lock()
	fns := make([]func(), len(s.pending))
	copy(fns, s.pending)
	s.pending = nil
	s.mu.Unlock()
	for _, fn := range fns {
		if fn != nil {
			fn()
		}
	}
}

func writeRawStatus(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("ResponseWriter is not a Hijacker")
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		t.Fatalf("hijack: %v", err)
	}
	fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
	buf.Flush()
	conn.Close()
}

func waitForSched(t *testing.T, s *delaySched, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for s.Pending() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Pending() == 0 {
		t.Fatal("no scheduled wait")
	}
}

func waitForWaits(t *testing.T, srv *strictWaitRV, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for srv.WaitCount() < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.WaitCount() < n {
		t.Fatalf("wait count = %d, want >= %d", srv.WaitCount(), n)
	}
}

type waitRec struct {
	Body          string
	ID            string
	Challenge     string
	Sig           string
	NextChallenge string
	Reject        string
	Status        int
	MaxMS         int
}

type strictWaitRV struct {
	URL    string
	Client *http.Client

	mu        sync.Mutex
	pub       ed25519.PublicKey
	handle    string
	chal      []byte
	allowed   map[string]struct{}
	failWaits bool
	waits     []waitRec
	origin    string
}

func newStrictWaitRV(t *testing.T) *strictWaitRV {
	t.Helper()
	s := &strictWaitRV{
		origin:  DefaultOrigin,
		chal:    bytes.Repeat([]byte{0x22}, 32),
		allowed: map[string]struct{}{},
	}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	s.Client = srv.Client()
	return s
}

func (s *strictWaitRV) AllowRID(rid string) {
	s.mu.Lock()
	s.allowed[rid] = struct{}{}
	s.mu.Unlock()
}

func (s *strictWaitRV) FailWaits(v bool) {
	s.mu.Lock()
	s.failWaits = v
	s.mu.Unlock()
}

func (s *strictWaitRV) WaitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.waits)
}

func (s *strictWaitRV) LastWait() waitRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.waits) == 0 {
		return waitRec{Reject: "no-wait"}
	}
	return s.waits[len(s.waits)-1]
}

func (s *strictWaitRV) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	switch r.URL.Path {
	case "/v1/hello":
		helloOK(w)
	case "/v1/enroll":
		var req struct {
			PubKey string `json:"pubkey"`
		}
		_ = json.Unmarshal(body, &req)
		raw, _ := hex.DecodeString(req.PubKey)
		s.mu.Lock()
		s.pub = raw
		s.handle = "ih_041061050R3GG28A"
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fmt.Fprintf(w, "{\"handle\":%q,\"v\":1}\n", "ih_041061050R3GG28A")
	case "/v1/challenge":
		s.mu.Lock()
		chal := hex.EncodeToString(s.chal)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fmt.Fprintf(w, "{\"challenge\":%q,\"v\":1}\n", chal)
	case "/v1/verify":
		w.WriteHeader(http.StatusNoContent)
	case "/v1/wait":
		s.handleWait(w, body)
	default:
		http.NotFound(w, r)
	}
}

func (s *strictWaitRV) handleWait(w http.ResponseWriter, body []byte) {
	rec := waitRec{Body: string(body)}
	s.mu.Lock()
	fail := s.failWaits
	s.mu.Unlock()
	if fail {
		rec.Reject = "injected-fail"
		rec.Status = http.StatusInternalServerError
		s.mu.Lock()
		s.waits = append(s.waits, rec)
		s.mu.Unlock()
		http.Error(w, "fail", http.StatusInternalServerError)
		return
	}
	var req struct {
		V         json.Number `json:"v"`
		Handle    string      `json:"handle"`
		Challenge string      `json:"challenge"`
		Sig       string      `json:"sig"`
		ID        string      `json:"id"`
		MaxMS     json.Number `json:"max_ms"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		s.rejectWait(w, rec, "malformed")
		return
	}
	if req.Sig == "" || req.Challenge == "" || req.ID == "" {
		s.rejectWait(w, rec, "handle-only-or-incomplete")
		return
	}
	if v, err := req.V.Int64(); err != nil || v < 1 {
		s.rejectWait(w, rec, "bad-v")
		return
	}
	if !validRID(req.ID) {
		s.rejectWait(w, rec, "bad-rid")
		return
	}
	chal, err := hex.DecodeString(req.Challenge)
	if err != nil || len(chal) != 32 {
		s.rejectWait(w, rec, "bad-challenge")
		return
	}
	sig, err := hex.DecodeString(req.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		s.rejectWait(w, rec, "bad-sig")
		return
	}
	msg := authMessage(s.origin, ProtocolVersion, "/v1/wait", req.Handle, chal)
	s.mu.Lock()
	pub := s.pub
	wantChal := append([]byte(nil), s.chal...)
	s.mu.Unlock()
	if !bytes.Equal(chal, wantChal) {
		s.rejectWait(w, rec, "challenge-mismatch")
		return
	}
	if !ed25519.Verify(pub, msg, sig) {
		s.rejectWait(w, rec, "verify-failed")
		return
	}
	next := make([]byte, 32)
	copy(next, wantChal)
	next[0]++
	s.mu.Lock()
	s.chal = next
	s.mu.Unlock()
	rec.ID = req.ID
	rec.Challenge = req.Challenge
	rec.Sig = req.Sig
	rec.NextChallenge = hex.EncodeToString(next)
	rec.Status = http.StatusNoContent
	s.mu.Lock()
	s.waits = append(s.waits, rec)
	s.mu.Unlock()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Rv-Challenge", rec.NextChallenge)
	w.WriteHeader(http.StatusNoContent)
}

func (s *strictWaitRV) rejectWait(w http.ResponseWriter, rec waitRec, why string) {
	rec.Reject = why
	rec.Status = http.StatusNotFound
	s.mu.Lock()
	s.waits = append(s.waits, rec)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	io.WriteString(w, "not found\n")
}
