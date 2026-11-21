package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/sessionworker"
	"github.com/scimux/scimux/internal/testowned/workerfixture"
)

// These REDs require a private supervised namespace and leave its evidence in
// that namespace for the owner; they never inspect historical worker paths.
func TestReattachedFixturePreservesReplacementRED(t *testing.T) {
	root := os.Getenv("SCIMUX_WORKER_FIXTURE_ROOT")
	if root == "" {
		t.Skip("requires private supervised fixture root")
	}
	t.Setenv("TMPDIR", root)
	neighbor, err := os.MkdirTemp(root, "neighbor-")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(neighbor, "neighbor.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var replacement string
	var fixture *reattachedFixture
	if !t.Run("reattached worker", func(t *testing.T) {
		data, pid, client, owned := startUnwaitedSyntheticWorker(t, "replacement-red")
		fixture = owned
		locator, err := sessionworker.Discover(data, "replacement-red")
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(locator.Link.Socket)
		if filepath.Dir(filepath.Dir(dir)) != root {
			t.Fatal("worker outside private root")
		}
		killUnwaitedWorkerAndAwaitZombie(t, pid)
		m := newWorkerManager("", data, "test")
		m.reapObserver = fixture.observe
		m.entries["replacement-red"] = &workerEntry{client: client, pid: pid}
		if err := m.Kill("replacement-red"); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(dir, dir+"-original"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		replacement = filepath.Join(dir, "unknown")
		if err := os.WriteFile(replacement, []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
		fixture.expectRefusal = workerfixture.ErrIdentity
	}) {
		t.Fatal("fixture setup failed")
	}
	c, err := net.DialTimeout("unix", filepath.Join(neighbor, "neighbor.sock"), time.Second)
	if err != nil {
		t.Fatal("neighbor unusable")
	}
	_ = c.Close()
	if data, err := os.ReadFile(replacement); err != nil || string(data) != "synthetic" {
		t.Error("fixture removed replacement after reattached worker wait")
	}
	if fixture == nil || fixture.cleanupErr == nil {
		t.Error("replacement cleanup was not refused")
	}
}

func TestReattachedFixturePreservesUnknownRED(t *testing.T) {
	root := os.Getenv("SCIMUX_WORKER_FIXTURE_ROOT")
	if root == "" {
		t.Skip("requires private supervised fixture root")
	}
	t.Setenv("TMPDIR", root)
	var marker string
	var fixture *reattachedFixture
	if !t.Run("reattached worker", func(t *testing.T) {
		data, pid, client, owned := startUnwaitedSyntheticWorker(t, "unknown-red")
		fixture = owned
		locator, err := sessionworker.Discover(data, "unknown-red")
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(locator.Link.Socket)
		if filepath.Dir(filepath.Dir(dir)) != root {
			t.Fatal("worker outside private root")
		}
		killUnwaitedWorkerAndAwaitZombie(t, pid)
		m := newWorkerManager("", data, "test")
		m.reapObserver = fixture.observe
		m.entries["unknown-red"] = &workerEntry{client: client, pid: pid}
		if err := m.Kill("unknown-red"); err != nil {
			t.Fatal(err)
		}
		var status syscall.WaitStatus
		if _, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
			t.Fatal("reattached manager did not reap child")
		}
		marker = filepath.Join(dir, "unknown")
		if err := os.WriteFile(marker, []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
		fixture.expectRefusal = workerfixture.ErrContents
	}) {
		t.Fatal("fixture setup failed")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "synthetic" {
		t.Error("fixture removed unknown entry after unrecorded reattached wait")
	}
	if fixture == nil || fixture.cleanupErr == nil {
		t.Error("unknown entry cleanup was not refused")
	}
}

func TestSocketBeforeBadReadinessHelper(t *testing.T) {
	if os.Getenv("SCIMUX_SOCKET_BEFORE_BAD_READY") != "1" {
		return
	}
	config, ready, err := readSessionWorkerStartup()
	if err != nil {
		t.Fatal(err)
	}
	server, err := sessionworker.Listen("", config.Identity, &syntheticSessionHarness{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	witness, err := net.DialTimeout("unix", os.Getenv("SCIMUX_WORKER_WITNESS"), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer witness.Close()
	if err := witness.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(witness).Encode(server.Link()); err != nil {
		t.Fatal(err)
	}
	var acknowledged bool
	if err := json.NewDecoder(witness).Decode(&acknowledged); err != nil || !acknowledged {
		t.Fatal("witness not acknowledged")
	}
	locator := sessionworker.Locator{Identity: config.Identity, NodeID: config.NodeID, PID: os.Getpid() + 1, Link: server.Link()}
	if err := json.NewEncoder(ready).Encode(locator); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Second)
}

func TestStartupIdentityFailureAfterSocketRED(t *testing.T) {
	root := os.Getenv("SCIMUX_WORKER_FIXTURE_ROOT")
	if root == "" {
		t.Skip("requires private supervised fixture root")
	}
	data := t.TempDir()
	if err := os.Chmod(data, 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := sessionWorkerConfig{DataDir: data, NodeID: "startup-red", Identity: sessionworker.Identity{WorkerID: "startup-red", Agent: "pi"}}
	parent, tmp, err := workerfixture.New(filepath.Clean(root))
	if err != nil {
		t.Fatal(err)
	}
	witnessPath := filepath.Join(tmp, "w.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: witnessPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(witnessPath, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := listener.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	type startedResult struct {
		process *sessionWorkerProcess
		err     error
	}
	started := make(chan startedResult, 1)
	type outcome struct {
		process *sessionWorkerProcess
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		p, err := startSessionWorker(context.Background(), exe, cfg, sessionWorkerStartOptions{args: []string{"-test.run=^TestSocketBeforeBadReadinessHelper$"}, env: []string{"SCIMUX_SOCKET_BEFORE_BAD_READY=1", "TMPDIR=" + tmp, "SCIMUX_WORKER_WITNESS=" + witnessPath}, started: func(p *sessionWorkerProcess) { started <- startedResult{p, parent.BindStarted(p.pid)} }})
		done <- outcome{p, err}
	}()
	var owned *sessionWorkerProcess
	select {
	case observed := <-started:
		if observed.err != nil {
			t.Fatal(observed.err)
		}
		owned = observed.process
	case <-time.After(3 * time.Second):
		t.Fatal("started worker not observed")
	}
	witness, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer witness.Close()
	if err := witness.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var link sessionworker.Link
	if err := json.NewDecoder(witness).Decode(&link); err != nil {
		t.Fatal(err)
	}
	client, err := sessionworker.NewClient(link)
	if err != nil {
		t.Fatal(err)
	}
	helloCtx, cancelHello := context.WithTimeout(context.Background(), 3*time.Second)
	hello, err := client.Hello(helloCtx)
	cancelHello()
	if err != nil || hello.Identity != cfg.Identity {
		t.Fatal("witness identity mismatch")
	}
	if err := parent.Register(link.Socket, owned.pid); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(witness).Encode(true); err != nil {
		t.Fatal(err)
	}
	var result outcome
	select {
	case result = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("startup response missing")
	}
	if result.err == nil || result.process != nil {
		t.Fatal("identity failure did not reject startup")
	}
	select {
	case <-owned.wait:
	case <-time.After(3 * time.Second):
		t.Fatal("startup child wait missing")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	waitResult := func() error { owned.mu.Lock(); defer owned.mu.Unlock(); return owned.err }
	if err := parent.CleanupWait(owned.wait, waitResult, client.Close); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(link.Socket)
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("started worker socket parent remains without returned wait handle")
	}
}
