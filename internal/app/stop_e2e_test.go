package app

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/backend"
)

func TestStopCommandE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real scimux process")
	}
	if runtime.GOOS == "windows" {
		t.Skip("scimux ships only Unix process targets")
	}
	root := repoRootFromTest(t)
	bin := filepath.Join(t.TempDir(), "scimux")
	build := exec.Command("go", "build", "-o", bin, "./cmd/scimux")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build scimux: %v\n%s", err, out)
	}

	home := t.TempDir()
	emptyPath := t.TempDir() // prevents every real harness CLI from being found
	logFile, err := os.Create(filepath.Join(t.TempDir(), "scimux.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	env := append(os.Environ(), "HOME="+home, "PATH="+emptyPath)
	muxer := exec.Command(bin, "-addr", "127.0.0.1:0", "-socket", "stop-e2e")
	muxer.Env = env
	muxer.Stdout, muxer.Stderr = logFile, logFile
	if err := muxer.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	processDone := make(chan struct{})
	go func() {
		exited <- muxer.Wait()
		close(processDone)
	}()
	t.Cleanup(func() {
		if muxer.Process != nil {
			_ = muxer.Process.Kill()
		}
		select {
		case <-processDone:
		case <-time.After(5 * time.Second):
		}
	})

	data := filepath.Join(home, ".scimux")
	deadline := time.Now().Add(15 * time.Second)
	var link backend.Link
	for time.Now().Before(deadline) {
		link, err = backend.Discover(data)
		if err == nil {
			client, clientErr := backend.NewClient(link)
			if clientErr == nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_, clientErr = client.Hello(ctx)
				cancel()
				_ = client.Close()
			}
			if clientErr == nil {
				break
			}
		}
		select {
		case exitErr := <-exited:
			b, _ := os.ReadFile(logFile.Name())
			t.Fatalf("muxer exited before registration: %v\n%s", exitErr, b)
		case <-time.After(25 * time.Millisecond):
		}
	}
	if link.Socket == "" {
		b, _ := os.ReadFile(logFile.Name())
		t.Fatalf("muxer did not publish its locator: %v\n%s", err, b)
	}
	duplicate := exec.Command(bin, "-addr", "127.0.0.1:0", "-socket", "stop-e2e-duplicate")
	duplicate.Env = env
	if out, err := duplicate.CombinedOutput(); err == nil || !strings.Contains(string(out), "already running") {
		t.Fatalf("duplicate muxer: err=%v output=%q", err, out)
	}

	wrong := exec.Command(bin, "stop", "-data", t.TempDir())
	wrong.Env = env
	if out, err := wrong.CombinedOutput(); err == nil || !strings.Contains(string(out), "no running muxer") {
		t.Fatalf("wrong-data stop: err=%v output=%q", err, out)
	}
	client, err := backend.NewClient(link)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if _, err := client.Hello(ctx); err != nil {
		t.Fatalf("wrong-data stop disturbed muxer: %v", err)
	}
	cancel()
	_ = client.Close()

	stop := exec.Command(bin, "stop")
	stop.Env = env
	out, err := stop.CombinedOutput()
	if err != nil || string(out) != "scimux: stopped\n" {
		t.Fatalf("stop: err=%v output=%q", err, out)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("muxer exit: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("muxer did not exit after stop completed")
	}
	if _, err := backend.Discover(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("locator survived muxer exit: %v", err)
	}
}

func TestMuxerDeathReapsWebChildE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real scimux process")
	}
	if runtime.GOOS == "windows" {
		t.Skip("scimux ships only Unix process targets")
	}
	root := repoRootFromTest(t)
	bin := filepath.Join(t.TempDir(), "scimux")
	build := exec.Command("go", "build", "-o", bin, "./cmd/scimux")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build scimux: %v\n%s", err, out)
	}
	home := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "scimux.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	muxer := exec.Command(bin, "-addr", "127.0.0.1:0", "-socket", "parent-death-e2e")
	muxer.Env = append(os.Environ(), "HOME="+home, "PATH="+t.TempDir())
	muxer.Stdout, muxer.Stderr = logFile, logFile
	// Isolate cleanup to this muxer and the web child it spawns.
	muxer.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := muxer.Start(); err != nil {
		t.Fatal(err)
	}
	pid := muxer.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	exited := make(chan error, 1)
	go func() { exited <- muxer.Wait() }()

	addr := waitForLoggedAddress(t, logPath, exited)
	resp, err := testHTTPClient.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("web child was not serving before muxer death: %v", err)
	}
	_ = resp.Body.Close()
	if err := muxer.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("killed muxer did not exit")
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(logPath)
	t.Fatalf("web child retained %s after muxer death\n%s", addr, b)
}

func TestMuxerDeathReapsWebChildBlockedOnInviteStdinE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real scimux process")
	}
	if runtime.GOOS == "windows" {
		t.Skip("scimux ships only Unix process targets")
	}
	root := repoRootFromTest(t)
	bin := filepath.Join(t.TempDir(), "scimux")
	build := exec.Command("go", "build", "-o", bin, "./cmd/scimux")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build scimux: %v\n%s", err, out)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	home := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "scimux.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	muxer := exec.Command(bin, "-addr", addr, "-socket", "blocked-invite-e2e", "--remote", "--invite-stdin")
	muxer.Env = append(os.Environ(), "HOME="+home, "PATH="+t.TempDir(), experimentalRemoteEnv+"=1")
	muxer.Stdout, muxer.Stderr = logFile, logFile
	muxer.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	inviteReader, inviteWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	muxer.Stdin = inviteReader
	defer inviteReader.Close()
	defer inviteWriter.Close()
	if err := muxer.Start(); err != nil {
		t.Fatal(err)
	}
	pid := muxer.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	exited := make(chan error, 1)
	go func() { exited <- muxer.Wait() }()

	// The web child creates this persistent lock file immediately before it
	// blocks reading the still-open stdin pipe.
	waitForTestPath(t, filepath.Join(home, ".scimux", "remote", "state.lock"))
	select {
	case err := <-exited:
		b, _ := os.ReadFile(logPath)
		t.Fatalf("muxer exited before parent-death test: %v\n%s", err, b)
	case <-time.After(100 * time.Millisecond):
	}
	if err := muxer.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("killed muxer did not exit")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(logPath)
	t.Fatalf("web child blocked on invite stdin retained %s after muxer death\n%s", addr, b)
}

func waitForLoggedAddress(t *testing.T, logPath string, exited <-chan error) string {
	t.Helper()
	const marker = "scimux: http://"
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(logPath)
		if _, rest, ok := strings.Cut(string(b), marker); ok {
			if addr, _, ok := strings.Cut(rest, "/"); ok && addr != "" {
				return addr
			}
		}
		select {
		case err := <-exited:
			t.Fatalf("muxer exited before logging its address: %v\n%s", err, b)
		case <-time.After(25 * time.Millisecond):
		}
	}
	b, _ := os.ReadFile(logPath)
	t.Fatalf("muxer did not log its address\n%s", b)
	return ""
}
