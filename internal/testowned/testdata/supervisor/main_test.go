package main

import (
	"bytes"
	"github.com/scimux/scimux/internal/testowned"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMainEntry(t *testing.T) {
	var out bytes.Buffer
	var code int = -1
	called := false
	args = []string{"--", "synthetic"}
	writer = &out
	exit = func(c int) { code = c }
	run = func(v []string) testowned.Result {
		called = true
		return testowned.Result{Category: "clean", Clean: true, RunnerExit: 7}
	}
	main()
	if !called || code != 0 || out.String() != "status clean\nrunner_exit 7\nreaped 0\n" {
		t.Fatalf("entry called=%v code=%d output=%q", called, code, out.String())
	}
}

func TestEntryForwardsExplicitStreams(t *testing.T) { testEntryForwardsStreams(t, false) }
func TestEntryForwardsBrowserStreams(t *testing.T)  { testEntryForwardsStreams(t, true) }

func testEntryForwardsStreams(t *testing.T, browser bool) {
	t.Helper()
	if os.Getenv("TESTOWNED_ENTRY_STREAMS") == "1" {
		args = []string{"--", "/bin/bash", "--norc", "-c", `IFS= read -r line; printf 'out:%s' "$line"; printf 'err' >&2`}
		main()
		return
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdoutR.Close(); stdoutW.Close(); stderrR.Close(); stderrW.Close() })
	cmd := exec.Command(os.Args[0], "-test.run=^TestEntryForwardsExplicitStreams$")
	cmd.Env = append(os.Environ(), "TESTOWNED_ENTRY_STREAMS=1", "TESTOWNED_FORWARD_STREAMS=1")
	if browser {
		cmd.Env = append(cmd.Env, "TESTOWNED_BROWSER_TEARDOWN=1")
	}
	cmd.Stdin = strings.NewReader("synthetic\n")
	cmd.ExtraFiles = []*os.File{stdoutW, stderrW}
	var proof bytes.Buffer
	cmd.Stdout = &proof
	if err := cmd.Run(); err != nil {
		t.Fatalf("supervisor: %v", err)
	}
	stdoutW.Close()
	stderrW.Close()
	out, err := io.ReadAll(stdoutR)
	if err != nil {
		t.Fatal(err)
	}
	outErr, err := io.ReadAll(stderrR)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "out:synthetic" || string(outErr) != "err" {
		t.Fatalf("forwarded streams: stdout=%q stderr=%q", out, outErr)
	}
	if proof.String() != "status clean\nrunner_exit 0\nreaped 0\n" {
		t.Fatalf("supervisor proof: %q", proof.String())
	}
}
