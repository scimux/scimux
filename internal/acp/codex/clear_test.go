package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const clearThreadResult = `{"thread":{"id":"THREAD-2","path":"/scrubbed/r2.jsonl"},"model":"gpt-5.5","approvalPolicy":"on-request","sandbox":{"type":"workspaceWrite"},"reasoningEffort":"high"}`

func launchedManagerWithLog(t *testing.T) (*Manager, *mockTransport, *mockServer, string) {
	t.Helper()
	dir := t.TempDir()
	mt, ms := newMockTransport()
	m := NewManagerWithSpawn(dir, func(nodeID, d string) (Transport, error) { return mt, nil })
	launch(t, m, ms, "n1", "gpt-5.5", "high")
	return m, mt, ms, filepath.Join(dir, "n1.jsonl")
}

func TestManagerClearNeverLaunched(t *testing.T) {
	m := NewManagerWithSpawn(t.TempDir(), nil)
	if err := m.Clear("n1"); err != ErrNoSession {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
}

func TestManagerClearRejectsActiveTurn(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	if err := m.Send("n1", "first"); err != nil {
		t.Fatalf("send: %v", err)
	}
	r := ms.nextReq(t)
	ms.reply(t, r.ID, `{"turn":{}}`) // accepted, never completed
	if err := m.Clear("n1"); err != ErrTurnActive {
		t.Fatalf("want ErrTurnActive, got %v", err)
	}
}

func TestManagerClearDeadProcess(t *testing.T) {
	m, mt, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	_ = mt.Close()
	waitFor(t, func() bool { return m.Live("n1") == "exited" })
	if err := m.Clear("n1"); err != ErrNotAlive {
		t.Fatalf("want ErrNotAlive, got %v", err)
	}
}

func TestManagerClearStartsNewThread(t *testing.T) {
	m, _, ms, logPath := launchedManagerWithLog(t)
	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}

	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	r := ms.nextReq(t)
	if r.Method != "thread/start" {
		t.Fatalf("clear req = %q, want thread/start", r.Method)
	}
	ms.reply(t, r.ID, clearThreadResult)
	if err := <-errc; err != nil {
		t.Fatalf("clear: %v", err)
	}

	after, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(after) <= len(before) {
		t.Fatalf("log did not grow: before=%d after=%d", len(before), len(after))
	}
	if !bytes.Contains(after, []byte("THREAD-2")) {
		t.Fatalf("log missing THREAD-2: %s", after)
	}
	if got := m.LastError("n1"); got != "" {
		t.Fatalf("unexpected last error: %q", got)
	}
}

func TestManagerClearAppendsSeamOnlyAfterThreadStart(t *testing.T) {
	m, _, ms, logPath := launchedManagerWithLog(t)
	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}

	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	r := ms.nextReq(t)
	if r.Method != "thread/start" {
		t.Fatalf("clear req = %q, want thread/start", r.Method)
	}
	idb, _ := json.Marshal(r.ID)
	ms.writeRaw(t, `{"id":`+string(idb)+`,"error":{"code":-32600,"message":"bad cwd"}}`)
	err = <-errc
	if err == nil || !strings.Contains(err.Error(), "codex thread/start") {
		t.Fatalf("want thread/start error, got %v", err)
	}

	after, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("log mutated after failed thread/start:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestManagerClearSeamAppendFailure(t *testing.T) {
	m, _, ms, logPath := launchedManagerWithLog(t)
	if err := os.Remove(logPath); err != nil {
		t.Fatalf("remove log: %v", err)
	}
	if err := os.Mkdir(logPath, 0o700); err != nil {
		t.Fatalf("mkdir over log: %v", err)
	}

	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	r := ms.nextReq(t)
	if r.Method != "thread/start" {
		t.Fatalf("clear req = %q, want thread/start", r.Method)
	}
	ms.reply(t, r.ID, clearThreadResult)
	err := <-errc
	if err == nil || !strings.Contains(err.Error(), "record clear seam") {
		t.Fatalf("want seam-append error, got %v", err)
	}
}

func TestManagerClearReleasesTurnReservation(t *testing.T) {
	m, _, ms, _ := launchedManagerWithLog(t)
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	r := ms.nextReq(t)
	if r.Method != "thread/start" {
		t.Fatalf("clear req = %q, want thread/start", r.Method)
	}
	ms.reply(t, r.ID, clearThreadResult)
	if err := <-errc; err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := m.Send("n1", "after-clear"); err != nil {
		t.Fatalf("send after clear: %v", err)
	}
}
