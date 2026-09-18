package acp

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
)

func TestClearRollbackOnSeamAppendFailure(t *testing.T) {
	agent := &fakeAgent{}
	dir := t.TempDir()
	m := NewManagerWithRunner(dir, fakeRunner(agent))
	if _, err := m.Launch("n1", "opencode", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn to finish", func() bool { return m.Live("n1") == "quiet" })

	logPath := filepath.Join(dir, "n1.jsonl")
	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("log empty before clear")
	}

	m.mu.Lock()
	s := m.sessions["n1"]
	m.mu.Unlock()
	if s == nil {
		t.Fatal("missing session")
	}
	good := s.logw.Path
	s.logw.Path = filepath.Join(dir, "no-such-dir", "n1.jsonl")

	err = m.Clear("n1")
	s.logw.Path = good

	if err == nil || !strings.Contains(err.Error(), "record clear seam") {
		t.Fatalf("want record-clear-seam error, got %v", err)
	}

	after, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("log mutated after failed clear:\nbefore=%s\nafter=%s", before, after)
	}

	if err := m.Send("n1", "after-failed-clear"); err != nil {
		t.Fatalf("send after failed clear: %v", err)
	}
	waitFor(t, "turn after failed clear", func() bool { return m.Live("n1") == "quiet" })

	seg := sessionlog.ReadSegment(logPath)
	if len(seg.Turns) != 2 || seg.PriorTurns != 0 || seg.StartTime == "" {
		t.Fatalf("unbroken segment: turns=%d prior=%d start=%q",
			len(seg.Turns), seg.PriorTurns, seg.StartTime)
	}
}
