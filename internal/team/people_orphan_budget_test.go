package team

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/scimux/scimux/internal/storagebudget"
)

func TestIndependentOrphanRecoveryUnderFullGlobalBudget(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	stages := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages")
	if err := os.MkdirAll(stages, 0700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(stages, "0123456789abcdef-aaaaaaaaaaaaaaaa.tmp")
	if err := os.WriteFile(orphan, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":4096,"storage_min_free_bytes":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SavePeopleStage(dir, id, syntheticPeopleStage()); err != nil {
		t.Fatalf("abandoned temporary bytes block bounded recovery: %v", err)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan remains: %v", err)
	}
}

func TestPeopleOrphanRecoveryDoesNotBypassBudgetOrRemoveOtherFiles(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	stages := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages")
	if err := os.MkdirAll(stages, 0700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(stages, "0123456789abcdef-aaaaaaaaaaaaaaaa.tmp")
	other := filepath.Join(stages, "unrelated.tmp")
	if err := os.WriteFile(orphan, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, make([]byte, 4000), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":4096,"storage_min_free_bytes":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SavePeopleStage(dir, id, syntheticPeopleStage()); !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("expected budget error: %v", err)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan remains: %v", err)
	}
	if info, err := os.Stat(other); err != nil || info.Size() != 4000 {
		t.Fatalf("unrelated data changed: %v", err)
	}
}
