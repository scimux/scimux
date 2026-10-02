package acp

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStartGroupedNamesAMissingBinaryAndReapsANonAgent(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing-vibe-acp")

	_, err := startGrouped([]string{missing}, dir, "")
	if err == nil {
		t.Fatal("missing binary started")
	}
	if strings.Contains(err.Error(), "start ") {
		t.Fatalf("empty label wrapped the start error: %v", err)
	}

	_, err = startGrouped([]string{missing}, dir, "vibe")
	if err == nil || !strings.Contains(err.Error(), "start vibe ACP:") {
		t.Fatalf("labeled start error = %v", err)
	}

	// An empty PATH keeps execRunner from resolving a vibe-acp that happens
	// to be installed on the machine running the test.
	t.Setenv("PATH", dir)
	_, err = execRunner("n1", "vibe", dir, "synthetic-model", "high")
	if err == nil || !strings.Contains(err.Error(), "start vibe ACP:") {
		t.Fatalf("execRunner error = %v", err)
	}

	proc, err := startGrouped([]string{"/bin/true"}, dir, "vibe")
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = proc.Wait()
}

func TestExecRunnerRejectsAnUnknownAgentBeforeStarting(t *testing.T) {
	_, err := execRunner("n1", "not-an-agent", t.TempDir(), "", "")
	if err == nil || !strings.Contains(err.Error(), "no ACP transport") {
		t.Fatalf("execRunner error = %v", err)
	}
}
