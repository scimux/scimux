package storagebudget

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePolicy(t *testing.T, data string, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(data, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeManaged(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPolicyDefaultsAndExplicitDisable(t *testing.T) {
	data := t.TempDir()
	if got := LoadPolicy(data).MinFreeBytes; got != DefaultMinFreeBytes {
		t.Fatalf("default minimum free bytes = %d, want %d", got, DefaultMinFreeBytes)
	}
	writePolicy(t, data, `{"storage_min_free_bytes":0,"storage_global_limit_bytes":123,"storage_node_limit_bytes":45}`)
	got := LoadPolicy(data)
	if got.MinFreeBytes != 0 || got.GlobalLimitBytes != 123 || got.NodeLimitBytes != 45 {
		t.Fatalf("policy = %+v", got)
	}
}

func TestGlobalBudgetRejectsWithoutDeletingHistory(t *testing.T) {
	data := t.TempDir()
	path := filepath.Join(data, "sessions", "n1.jsonl")
	writeManaged(t, path, "1234567890")
	writePolicy(t, data, `{"storage_min_free_bytes":0,"storage_global_limit_bytes":12}`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(data, "n1", 3); !errors.Is(err, ErrBudget) {
		t.Fatalf("Reserve error = %v, want ErrBudget", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("rejected reservation changed durable history: %q -> %q", before, after)
	}
}

func TestNodeBudgetCountsLogAttachmentsAndAssets(t *testing.T) {
	data := t.TempDir()
	writeManaged(t, filepath.Join(data, "sessions", "n1.jsonl"), "1234")
	writeManaged(t, filepath.Join(data, "attachments", "n1", "a"), "123")
	writeManaged(t, filepath.Join(data, "assets", "n1", "b"), "12")
	writeManaged(t, filepath.Join(data, "sessions", "n2.jsonl"), "unrelated")
	writePolicy(t, data, `{"storage_min_free_bytes":0,"storage_node_limit_bytes":10}`)
	if _, err := Reserve(data, "n1", 2); !errors.Is(err, ErrBudget) {
		t.Fatalf("n1 reservation = %v, want ErrBudget", err)
	}
	release, err := Reserve(data, "n2", 1)
	if err != nil {
		t.Fatalf("n2 reservation: %v", err)
	}
	release()
}

func TestMinFreeOnlyReservationsAreSerialized(t *testing.T) {
	data := t.TempDir()
	writePolicy(t, data, `{"storage_min_free_bytes":1}`)

	releaseFirst, err := Reserve(data, "n1", 1)
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	defer releaseFirst()

	type result struct {
		release func()
		err     error
	}
	started := make(chan struct{})
	done := make(chan result, 1)
	go func() {
		close(started)
		release, err := Reserve(data, "n2", 1)
		done <- result{release: release, err: err}
	}()
	<-started

	select {
	case got := <-done:
		if got.release != nil {
			got.release()
		}
		t.Fatalf("second min-free-only reservation completed before release: %v", got.err)
	case <-time.After(100 * time.Millisecond):
	}

	releaseFirst()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("second Reserve after release: %v", got.err)
		}
		got.release()
	case <-time.After(time.Second):
		t.Fatal("second min-free-only reservation remained blocked after release")
	}
}

func TestLockAllowsNonGrowingMutationWhenBudgetIsAlreadyFull(t *testing.T) {
	data := t.TempDir()
	writeManaged(t, filepath.Join(data, "sessions", "n1.jsonl"), "already over")
	writePolicy(t, data, `{"storage_min_free_bytes":0,"storage_global_limit_bytes":1}`)
	if _, err := Reserve(data, "n1", 0); !errors.Is(err, ErrBudget) {
		t.Fatalf("Reserve at full budget = %v, want ErrBudget", err)
	}
	release, err := Lock(data)
	if err != nil {
		t.Fatalf("Lock at full budget: %v", err)
	}
	release()
}

func TestInspectReportsAggregateAndPerNodeUsage(t *testing.T) {
	data := t.TempDir()
	writeManaged(t, filepath.Join(data, "sessions", "n1.jsonl"), "1234")
	writeManaged(t, filepath.Join(data, "attachments", "n1", "a"), "12")
	writeManaged(t, filepath.Join(data, "sessions", "archive", "old.jsonl"), "123")
	writePolicy(t, data, `{"storage_min_free_bytes":0,"storage_global_limit_bytes":100,"storage_node_limit_bytes":20}`)
	status, err := Inspect(data)
	if err != nil {
		t.Fatal(err)
	}
	if status.UsedBytes != 9 || status.Nodes["n1"] != 6 {
		t.Fatalf("status usage = total %d nodes %#v", status.UsedBytes, status.Nodes)
	}
	if !status.Writable || status.GlobalLimitBytes != 100 || status.NodeLimitBytes != 20 {
		t.Fatalf("status = %+v", status)
	}
}
