package muse

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if runTransportHelper() {
		return
	}
	os.Exit(m.Run())
}

func runTransportHelper() bool {
	switch os.Getenv("SCIMUX_MUSE_HELPER") {
	case "":
		return false
	case "echo":
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "stderr":
		_, _ = io.WriteString(os.Stderr, "diag-line\n")
	case "exit0":
	case "sleep":
		time.Sleep(time.Hour)
	case "child":
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "SCIMUX_MUSE_HELPER=sleep")
		if err := c.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "start child: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%d\n", c.Process.Pid)
		_ = os.Stdout.Close()
		select {}
	case "ignore-term":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			for range ch {
			}
		}()
		fmt.Printf("ready\n")
		_ = os.Stdout.Close()
		select {}
	case "child-ignore-term":
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "SCIMUX_MUSE_HELPER=ignore-term")
		if err := c.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "start child: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%d\n", c.Process.Pid)
		_ = os.Stdout.Close()
		select {}
	case "ignore-term-with-child":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		go func() {
			for range ch {
			}
		}()
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "SCIMUX_MUSE_HELPER=ignore-term")
		if err := c.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "start child: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%d\n", c.Process.Pid)
		_ = os.Stdout.Close()
		select {}
	default:
		os.Exit(2)
	}
	os.Exit(0)
	return true
}

func TestSpawnRequestsExactServeArgv(t *testing.T) {
	restore := installHelper("exit0")
	defer restore()
	var name string
	var args []string
	orig := commandFn
	commandFn = func(ctx context.Context, n string, a ...string) *exec.Cmd {
		name, args = n, append([]string(nil), a...)
		return orig(ctx, n, a...)
	}
	defer func() { commandFn = orig }()

	tr, err := Spawn(context.Background(), "/opt/does-not-run-muse")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	if name != "/opt/does-not-run-muse" {
		t.Fatalf("bin=%q", name)
	}
	if len(args) != 1 || args[0] != "serve" {
		t.Fatalf("args=%v, want [serve]", args)
	}
}

func TestSpawnStdinStdoutEcho(t *testing.T) {
	defer installHelper("echo")()
	tr, err := Spawn(context.Background(), "unused-bin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	if _, err := io.WriteString(tr.Stdin(), "hello-wire\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := tr.Stdout().Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "hello-wire\n" {
		t.Fatalf("stdout=%q", got)
	}
}

func TestSpawnStderrIsDiagnostics(t *testing.T) {
	var stderr bytes.Buffer
	restore := installHelper("stderr")
	defer restore()
	orig := commandFn
	commandFn = func(ctx context.Context, n string, a ...string) *exec.Cmd {
		cmd := orig(ctx, n, a...)
		cmd.Stderr = &stderr
		return cmd
	}
	defer func() { commandFn = orig }()

	tr, err := Spawn(context.Background(), "unused-bin")
	if err != nil {
		t.Fatal(err)
	}
	et := tr.(*execTransport)
	select {
	case <-et.waited:
	case <-time.After(2 * time.Second):
		t.Fatal("stderr helper did not exit")
	}
	_ = tr.Close()
	if !strings.Contains(stderr.String(), "diag-line") {
		t.Fatalf("stderr=%q, want diagnostic line", stderr.String())
	}
}

func TestSpawnNaturalExitReaped(t *testing.T) {
	defer installHelper("exit0")()
	tr, err := Spawn(context.Background(), "unused-bin")
	if err != nil {
		t.Fatal(err)
	}
	et, ok := tr.(*execTransport)
	if !ok {
		t.Fatalf("transport type %T", tr)
	}
	select {
	case <-et.waited:
	case <-time.After(5 * time.Second):
		t.Fatal("natural exit was not reaped")
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSpawnCloseBeforeExit(t *testing.T) {
	defer installHelper("sleep")()
	tr, err := Spawn(context.Background(), "unused-bin")
	if err != nil {
		t.Fatal(err)
	}
	et := tr.(*execTransport)
	pid := et.cmd.Process.Pid
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-et.waited:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not reap")
	}
	if pidAlive(pid) {
		t.Fatalf("pid %d still alive after Close", pid)
	}
	if _, err := tr.Stdin().Write([]byte("x")); err == nil {
		t.Fatal("write to closed stdin succeeded")
	}
}

func TestSpawnConcurrentClose(t *testing.T) {
	defer installHelper("sleep")()
	tr, err := Spawn(context.Background(), "unused-bin")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errc := make(chan error, 8)
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			errc <- tr.Close()
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestSpawnKillsProcessGroupDescendants(t *testing.T) {
	defer installHelper("child")()
	tr, err := Spawn(context.Background(), "unused-bin")
	if err != nil {
		t.Fatal(err)
	}
	et := tr.(*execTransport)
	parent := et.cmd.Process.Pid
	line := make([]byte, 32)
	n, err := tr.Stdout().Read(line)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(line[:n])))
	if err != nil {
		t.Fatalf("child pid %q: %v", line[:n], err)
	}
	if !pidAlive(child) {
		t.Fatalf("child %d not alive before Close", child)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(parent) && !pidAlive(child) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("after Close parent alive=%v child alive=%v", pidAlive(parent), pidAlive(child))
}

func TestSpawnKillsSIGTERMIgnoringDescendant(t *testing.T) {
	old := processKillGrace
	processKillGrace = 50 * time.Millisecond
	defer installHelper("child-ignore-term")()
	tr, err := Spawn(context.Background(), "unused-bin")
	processKillGrace = old
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	line := make([]byte, 32)
	n, err := tr.Stdout().Read(line)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(line[:n])))
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(child) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("SIGTERM-ignoring descendant %d still alive", child)
}

func TestSpawnContextCancelKillsGroup(t *testing.T) {
	old := processKillGrace
	processKillGrace = 50 * time.Millisecond
	defer installHelper("child-ignore-term")()
	ctx, cancel := context.WithCancel(context.Background())
	tr, err := Spawn(ctx, "unused-bin")
	processKillGrace = old
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	et := tr.(*execTransport)
	parent := et.cmd.Process.Pid
	line := make([]byte, 32)
	n, err := tr.Stdout().Read(line)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(line[:n])))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(parent) && !pidAlive(child) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("after ctx cancel parent=%v child=%v", pidAlive(parent), pidAlive(child))
}

func TestSpawnFailedStart(t *testing.T) {
	orig := commandFn
	defer func() { commandFn = orig }()
	commandFn = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/no/such/muse-binary-scimux-test", args...)
	}
	tr, err := Spawn(context.Background(), "/no/such/muse-binary-scimux-test")
	if err == nil {
		if tr != nil {
			_ = tr.Close()
		}
		t.Fatal("want start error")
	}
	if tr != nil {
		t.Fatal("failed start must not return a transport")
	}
}

func TestSpawnEmptyBinary(t *testing.T) {
	tr, err := Spawn(context.Background(), "")
	if err == nil {
		_ = tr.Close()
		t.Fatal("empty bin must fail")
	}
	if tr != nil {
		t.Fatal("empty bin returned a transport")
	}
}

func TestSpawnCloseIdempotent(t *testing.T) {
	defer installHelper("sleep")()
	tr, err := Spawn(context.Background(), "unused-bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSpawnLeaderIgnoresSIGTERMEscalates(t *testing.T) {
	old := processKillGrace
	processKillGrace = 80 * time.Millisecond
	defer installHelper("ignore-term")()
	tr, err := Spawn(context.Background(), "unused-bin")
	processKillGrace = old
	if err != nil {
		t.Fatal(err)
	}
	et := tr.(*execTransport)
	pid := et.cmd.Process.Pid
	line := make([]byte, 32)
	n, err := tr.Stdout().Read(line)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(line[:n])) != "ready" {
		t.Fatalf("helper=%q", line[:n])
	}
	if !pidAlive(pid) {
		t.Fatal("leader died before Close; ignore-term helper did not stay alive")
	}
	select {
	case <-et.waited:
		t.Fatal("leader reaped before Close; helper exited on its own")
	default:
	}
	done := make(chan error, 1)
	go func() { done <- tr.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked in Wait; SIGKILL path never reached")
	}
	if pidAlive(pid) {
		t.Fatalf("leader %d still alive after Close", pid)
	}
}

func TestSpawnLeaderExitsDescendantIgnoresSIGTERM(t *testing.T) {
	old := processKillGrace
	processKillGrace = 80 * time.Millisecond
	defer installHelper("child-ignore-term")()
	tr, err := Spawn(context.Background(), "unused-bin")
	processKillGrace = old
	if err != nil {
		t.Fatal(err)
	}
	et := tr.(*execTransport)
	parent := et.cmd.Process.Pid
	line := make([]byte, 32)
	n, err := tr.Stdout().Read(line)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(line[:n])))
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if pidAlive(parent) {
		t.Fatalf("leader %d still alive", parent)
	}
	if pidAlive(child) {
		t.Fatalf("descendant %d still alive after Close", child)
	}
}

func TestSpawnProcessGroupLeaderAndDescendantIgnoreSIGTERM(t *testing.T) {
	old := processKillGrace
	processKillGrace = 80 * time.Millisecond
	defer installHelper("ignore-term-with-child")()
	tr, err := Spawn(context.Background(), "unused-bin")
	processKillGrace = old
	if err != nil {
		t.Fatal(err)
	}
	et := tr.(*execTransport)
	parent := et.cmd.Process.Pid
	line := make([]byte, 32)
	n, err := tr.Stdout().Read(line)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(line[:n])))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- tr.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked in Wait on SIGTERM-ignoring leader")
	}
	if pidAlive(parent) || pidAlive(child) {
		t.Fatalf("after Close parent alive=%v child alive=%v", pidAlive(parent), pidAlive(child))
	}
}

func TestSpawnContextCancelCloseCleansGroup(t *testing.T) {
	old := processKillGrace
	processKillGrace = 80 * time.Millisecond
	defer installHelper("ignore-term-with-child")()
	ctx, cancel := context.WithCancel(context.Background())
	tr, err := Spawn(ctx, "unused-bin")
	processKillGrace = old
	if err != nil {
		t.Fatal(err)
	}
	et := tr.(*execTransport)
	parent := et.cmd.Process.Pid
	line := make([]byte, 32)
	n, err := tr.Stdout().Read(line)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(line[:n])))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(parent) && !pidAlive(child) {
			_ = tr.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = tr.Close()
	t.Fatalf("after ctx cancel parent=%v child=%v", pidAlive(parent), pidAlive(child))
}

func TestSpawnNaturalExitWithoutSIGKILL(t *testing.T) {
	defer installHelper("exit0")()
	tr, err := Spawn(context.Background(), "unused-bin")
	if err != nil {
		t.Fatal(err)
	}
	et := tr.(*execTransport)
	select {
	case <-et.waited:
	case <-time.After(5 * time.Second):
		t.Fatal("natural exit was not reaped")
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSpawnFailedStdoutPipeClosesStdin(t *testing.T) {
	orig := commandFn
	t.Cleanup(func() { commandFn = orig })
	commandFn = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "SCIMUX_MUSE_HELPER=sleep")
		cmd.Stdout = &bytes.Buffer{}
		return cmd
	}
	tr, err := Spawn(context.Background(), "unused-bin")
	if err == nil {
		if tr != nil {
			_ = tr.Close()
		}
		t.Fatal("StdoutPipe should fail when Stdout is already set")
	}
	if tr != nil {
		t.Fatal("failed StdoutPipe must not return a transport")
	}
}

func TestSpawnConcurrentCloseAllReturn(t *testing.T) {
	defer installHelper("sleep")()
	tr, err := Spawn(context.Background(), "unused-bin")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errc := make(chan error, 8)
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			errc <- tr.Close()
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestSignalGroupIgnoresInvalidPgid(t *testing.T) {
	et := &execTransport{pgid: 0, killGrace: 0, waited: make(chan struct{})}
	et.signalGroup()
	et.pgid = 1
	et.signalGroup()
}

func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func installHelper(kind string) func() {
	orig := commandFn
	commandFn = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0])
		cmd.Env = append(os.Environ(), "SCIMUX_MUSE_HELPER="+kind)
		return cmd
	}
	return func() { commandFn = orig }
}
