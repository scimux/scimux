package storagebudget

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestSerializedQuotaRefusalReleasesLockAndPreservesManagedData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions", "synthetic.jsonl")
	writeManaged(t, path, "1234567890")
	writePolicy(t, dir, `{"storage_min_free_bytes":0,"storage_global_limit_bytes":10}`)
	if release, err := ReserveSerializedAfter(dir, "", 1, nil); !errors.Is(err, ErrBudget) || release != nil {
		t.Fatalf("quota refusal: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		release, err := ReserveSerializedAfter(dir, "", 0, nil)
		if err == nil {
			release()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("lock not reusable: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("quota refusal retained the storage lock")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "1234567890" {
		t.Fatalf("managed data changed: %q %v", data, err)
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

func TestReferenceMediaCountsGloballyButNeverAsNodeUsage(t *testing.T) {
	data := t.TempDir()
	writeManaged(t, filepath.Join(data, "reference-media", "blobs", strings.Repeat("a", 64)), "1234")
	writeManaged(t, filepath.Join(data, "reference-media", "captures", strings.Repeat("b", 64)+".json"), "123")
	status, err := Inspect(data)
	if err != nil {
		t.Fatal(err)
	}
	if status.UsedBytes != 7 {
		t.Fatalf("global reference-media bytes = %d, want 7", status.UsedBytes)
	}
	if len(status.Nodes) != 0 {
		t.Fatalf("reference media acquired node charge: %#v", status.Nodes)
	}
}

func TestTeamCopiesCountGloballyButNeverAsNodeUsage(t *testing.T) {
	data := t.TempDir()
	server := strings.Repeat("a", 32)
	owner := strings.Repeat("b", 32)
	chat := strings.Repeat("c", 16)
	generation := strings.Repeat("d", 16)
	writeManaged(t, filepath.Join(data, "team-copies", server, owner, chat, generation, "mirror.jsonl"), "1234567")
	status, err := Inspect(data)
	if err != nil {
		t.Fatal(err)
	}
	if status.UsedBytes != 7 {
		t.Fatalf("global team copy bytes = %d, want 7", status.UsedBytes)
	}
	if len(status.Nodes) != 0 {
		t.Fatalf("team copy acquired node charge: %#v", status.Nodes)
	}
}

func TestReserveSerializedLocksEvenWhenGlobalLimitsDisabled(t *testing.T) {
	data := t.TempDir()
	writePolicy(t, data, `{"storage_min_free_bytes":0,"storage_global_limit_bytes":0,"storage_node_limit_bytes":0}`)
	release, err := ReserveSerialized(data, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan func(), 1)
	go func() {
		unlock, err := Lock(data)
		if err == nil {
			done <- unlock
		} else {
			done <- nil
		}
	}()
	select {
	case unlock := <-done:
		if unlock != nil {
			unlock()
		}
		release()
		t.Fatal("serialized reservation did not hold the lock")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case unlock := <-done:
		if unlock == nil {
			t.Fatal("second lock failed")
		}
		unlock()
	case <-time.After(2 * time.Second):
		t.Fatal("lock did not release")
	}
}

func TestReserveSerializedAfterRejectsInvalidOrUnavailableStorage(t *testing.T) {
	called := false
	recoverFn := func() error { called = true; return nil }
	if release, err := ReserveSerializedAfter(t.TempDir(), "", -1, recoverFn); err == nil || release != nil || called {
		t.Fatal("negative reservation reached recovery")
	}
	release, err := ReserveSerializedAfter("", "", 1, recoverFn)
	if err != nil || release == nil || called {
		t.Fatal("empty unmanaged root invoked recovery")
	}
	release()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".storage.lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if release, err := ReserveSerializedAfter(dir, "", 1, recoverFn); err == nil || release != nil || called {
		t.Fatal("reservation entered recovery without the storage lock")
	}
	if err := os.Remove(filepath.Join(dir, ".storage.lock")); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("synthetic recovery failure")
	if release, err := ReserveSerializedAfter(dir, "", 1, func() error { return failure }); !errors.Is(err, failure) || release != nil {
		t.Fatal("recovery failure did not abort reservation")
	}
	release, err = Lock(dir)
	if err != nil {
		t.Fatalf("failed recovery did not release lock: %v", err)
	}
	release()
}
