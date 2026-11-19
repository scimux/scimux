package main

import (
	"bytes"
	"github.com/scimux/scimux/internal/testowned"
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
