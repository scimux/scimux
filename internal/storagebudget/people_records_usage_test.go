package storagebudget

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedUsageSkipsOnlyExactPeopleRecords(t *testing.T) {
	data := t.TempDir()
	server := strings.Repeat("a", 32)
	owner := strings.Repeat("b", 32)
	chat := strings.Repeat("c", 16)
	generation := strings.Repeat("d", 16)
	copy := filepath.Join(data, "team-copies", server, owner, chat, generation)
	writeManaged(t, filepath.Join(copy, "people-records", "comments", "1", "0123456789abcdef.json"), "12345")
	writeManaged(t, filepath.Join(copy, "people-stages", "people-records", "nested"), "1234")
	writeManaged(t, filepath.Join(copy, "mirror.jsonl"), "12")
	writeManaged(t, filepath.Join(data, "team-copies", "people-records", "shallow"), "123")
	writeManaged(t, filepath.Join(data, "sessions", "people-records", "elsewhere"), "1")
	// A file at the same path shape is not the kept directory, so it still counts.
	writeManaged(t, filepath.Join(data, "team-copies", server, owner, chat, strings.Repeat("e", 16), "people-records"), "ab")
	status, err := Inspect(data)
	if err != nil {
		t.Fatal(err)
	}
	// 4 nested + 2 mirror + 3 shallow + 1 sessions + 2 file. The kept directory is excluded.
	if status.UsedBytes != 12 {
		t.Fatalf("managed usage = %d, want 12", status.UsedBytes)
	}
}

func TestExactPeopleRecordsRejectsIncomparablePaths(t *testing.T) {
	root := t.TempDir()
	kept := filepath.Join(root, "a", "b", "c", "d", "people-records")
	other := filepath.Join(root, "a", "b", "c", "d", "other")
	if !exactPeopleRecords(root, kept) {
		t.Fatal("exact kept directory was not recognized")
	}
	if exactPeopleRecords(root, other) {
		t.Fatal("a different directory was excluded")
	}
	if exactPeopleRecords("relative-root", kept) {
		t.Fatal("incomparable paths were treated as kept records")
	}
}

func TestReserveFreeChecksMinFreeAndIgnoresGlobalLimit(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, `{"storage_min_free_bytes":0,"storage_global_limit_bytes":1}`)
	release, err := ReserveFreeSerializedAfter(dir, 100, nil)
	if err != nil || release == nil {
		t.Fatalf("global limit blocked a free-only reserve: %v", err)
	}
	release()
	writePolicy(t, dir, `{"storage_min_free_bytes":9223372036854775807,"storage_global_limit_bytes":0}`)
	if release, err = ReserveFreeSerializedAfter(dir, 1, nil); !errors.Is(err, ErrBudget) || release != nil {
		t.Fatal("min-free did not refuse the free-only reserve")
	}
	called := false
	if release, err = ReserveFreeSerializedAfter(dir, -1, func() error { called = true; return nil }); err == nil || release != nil || called {
		t.Fatal("negative free-only reserve reached recovery")
	}
	release, err = ReserveFreeSerializedAfter("", 1, func() error { called = true; return nil })
	if err != nil || release == nil || called {
		t.Fatal("empty root invoked free-only recovery")
	}
	release()
	if err := os.Remove(filepath.Join(dir, ".storage.lock")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".storage.lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if release, err = ReserveFreeSerializedAfter(dir, 1, func() error { called = true; return nil }); err == nil || release != nil || called {
		t.Fatal("free-only reserve entered recovery without the lock")
	}
}
