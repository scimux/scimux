package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/storagebudget"
)

func peopleGroupRoot(dir string, id CopyID, tree string) string {
	return filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, tree)
}

func quietPeopleFloors(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_min_free_bytes":0}`), 0600); err != nil {
		t.Fatal(err)
	}
}

func writePeopleFillers(t *testing.T, dir string, n int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%016x.json", i+1)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func truncatePeopleFill(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestKeptCapFullDoesNotBlockStage(t *testing.T) {
	for _, tc := range []struct {
		name string
		fill func(t *testing.T, dir string, id CopyID)
	}{
		{"old byte cap", func(t *testing.T, dir string, id CopyID) {
			truncatePeopleFill(t, filepath.Join(peopleGroupRoot(dir, id, "people-records"), "fill"), 256<<20)
		}},
		{"old file cap", func(t *testing.T, dir string, id CopyID) {
			writePeopleFillers(t, filepath.Join(peopleGroupRoot(dir, id, "people-records"), "comments", "1"), 4096)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			quietPeopleFloors(t, dir)
			tc.fill(t, dir, id)
			if err := SavePeopleStage(dir, id, syntheticPeopleStage()); err != nil {
				t.Fatalf("kept files blocked a working stage: %v", err)
			}
		})
	}
}

func TestWorkCapFullDoesNotBlockKeptWrite(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	quietPeopleFloors(t, dir)
	writePeopleFillers(t, peopleGroupRoot(dir, id, "people-stages"), 4096)
	if err := SavePeoplePublished(dir, id, 1, syntheticPeopleStage()); err != nil {
		t.Fatalf("working files blocked a kept copy: %v", err)
	}
}

func TestPeopleGroupFileCap(t *testing.T) {
	stage := syntheticPeopleStage()
	for _, tc := range []struct {
		name  string
		tree  string
		files int
		save  func(dir string, id CopyID) error
		allow bool
	}{
		{"kept under", "people-records", 2047, func(dir string, id CopyID) error {
			return SavePeoplePublished(dir, id, 1, stage)
		}, true},
		{"kept full", "people-records", 2048, func(dir string, id CopyID) error {
			return SavePeoplePublished(dir, id, 1, stage)
		}, false},
		{"work under", "people-stages", 2047, func(dir string, id CopyID) error {
			return SavePeopleStage(dir, id, stage)
		}, true},
		{"work full", "people-stages", 2048, func(dir string, id CopyID) error {
			return SavePeopleStage(dir, id, stage)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			quietPeopleFloors(t, dir)
			writePeopleFillers(t, peopleGroupRoot(dir, id, tc.tree), tc.files)
			err := tc.save(dir, id)
			if tc.allow && err != nil {
				t.Fatalf("write under the group cap: %v", err)
			}
			if !tc.allow && !errors.Is(err, storagebudget.ErrBudget) {
				t.Fatalf("full group cap: %v", err)
			}
		})
	}
}

func TestPeopleGroupByteCap(t *testing.T) {
	stage := syntheticPeopleStage()
	encoded, _ := json.Marshal(stage)
	for _, tc := range []struct {
		name  string
		extra int64
		kept  bool
	}{
		{"kept exact", 0, true},
		{"kept over", 1, true},
		{"work exact", 0, false},
		{"work over", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			quietPeopleFloors(t, dir)
			tree := "people-stages"
			if tc.kept {
				tree = "people-records"
			}
			truncatePeopleFill(t, filepath.Join(peopleGroupRoot(dir, id, tree), "fill"), (128<<20)-int64(len(encoded))+tc.extra)
			var err error
			if tc.kept {
				err = SavePeoplePublished(dir, id, 1, stage)
			} else {
				err = SavePeopleStage(dir, id, stage)
			}
			if tc.extra == 0 && err != nil {
				t.Fatalf("exact group byte cap: %v", err)
			}
			if tc.extra == 1 && !errors.Is(err, storagebudget.ErrBudget) {
				t.Fatalf("over group byte cap: %v", err)
			}
		})
	}
}

func TestRetiredWorkCapBoundaries(t *testing.T) {
	for _, total := range []int{2046, 2047} {
		t.Run(fmt.Sprintf("files-%d", total), func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			quietPeopleFloors(t, dir)
			author, err := identity.LoadOrCreateInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			writePeopleFillers(t, peopleGroupRoot(dir, id, "people-stages"), total-1)
			err = RetireAbsentPeopleKey(dir, id, instance, string(author), "fedcba9876543210")
			if total == 2046 && err != nil {
				t.Fatalf("retired count under the cap: %v", err)
			}
			if total == 2047 && !errors.Is(err, storagebudget.ErrBudget) {
				t.Fatalf("retired count at the cap: %v", err)
			}
		})
	}
	for _, extra := range []int64{0, 1} {
		t.Run(fmt.Sprintf("bytes-%d", extra), func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			quietPeopleFloors(t, dir)
			author, err := identity.LoadOrCreateInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			key := "0123456789abcdef"
			identityPath := filepath.Join(peopleGroupRoot(dir, id, "people-identity"), "0000000000000000.json")
			info, err := os.Stat(identityPath)
			if err != nil {
				t.Fatal(err)
			}
			canonical := retiredPeakBytes(t, key, string(author), instance)
			truncatePeopleFill(t, filepath.Join(peopleGroupRoot(dir, id, "people-stages"), "fill"), (128<<20)-info.Size()-canonical+extra)
			err = RetireAbsentPeopleKey(dir, id, instance, string(author), key)
			if extra == 0 && err != nil {
				t.Fatalf("exact retired byte peak: %v", err)
			}
			if extra == 1 && !errors.Is(err, storagebudget.ErrBudget) {
				t.Fatalf("over retired byte peak: %v", err)
			}
		})
	}
}

func retiredPeakBytes(t *testing.T, key, author, instance string) int64 {
	t.Helper()
	marker := absentRetirement{Version: 1, Key: key, Author: author, CopyInstance: instance}
	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	encoded, err := json.Marshal(PeopleStage{ID: key, Hash: hex.EncodeToString(sum[:]), Name: "retired-absent.json", MIME: "application/json", ConfigHash: strings.Repeat("0", 64), Data: raw})
	if err != nil {
		t.Fatal(err)
	}
	return int64(2 * len(encoded))
}

func TestUnknownPeopleTreeIsRefused(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	quietPeopleFloors(t, dir)
	if err := savePeopleRecord(dir, id, syntheticPeopleStage(), "people-other"); !errors.Is(err, ErrPeopleStage) {
		t.Fatalf("unknown tree: %v", err)
	}
	if err := savePeopleRecordLocked(dir, id, syntheticPeopleStage(), []byte("{}")); !errors.Is(err, ErrPeopleStage) {
		t.Fatalf("empty tree: %v", err)
	}
}

func TestUnreadableKeptTreeDoesNotBlockStage(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	quietPeopleFloors(t, dir)
	if err := SavePeopleStage(dir, id, syntheticPeopleStage()); err != nil {
		t.Fatal(err)
	}
	path := peopleGroupRoot(dir, id, "people-records")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "synthetic"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0700) })
	other := syntheticPeopleStage()
	other.ID = "fedcba9876543210"
	if err := SavePeopleStage(dir, id, other); err != nil {
		t.Fatalf("unreadable kept tree blocked a working stage: %v", err)
	}
}

func TestWorkingFilesReachGlobalBudgetKeptFilesDoNot(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_min_free_bytes":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	before, err := storagebudget.Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	const filler = int64(8192)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(fmt.Sprintf(`{"storage_min_free_bytes":0,"storage_global_limit_bytes":%d}`, before.UsedBytes+filler)), 0600); err != nil {
		t.Fatal(err)
	}
	stage := syntheticPeopleStage()
	truncatePeopleFill(t, filepath.Join(peopleGroupRoot(dir, id, "people-stages"), "fill"), filler)
	if err := SavePeopleStage(dir, id, stage); !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("working files did not consume the global budget: %v", err)
	}
	if err := os.Remove(filepath.Join(peopleGroupRoot(dir, id, "people-stages"), "fill")); err != nil {
		t.Fatal(err)
	}
	truncatePeopleFill(t, filepath.Join(peopleGroupRoot(dir, id, "people-records"), "fill"), filler)
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatalf("kept files consumed the global budget: %v", err)
	}
}

func TestKeptWriteIgnoresGlobalLimitAndStillChecksMinFree(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_min_free_bytes":0,"storage_global_limit_bytes":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatalf("global limit blocked a kept copy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_min_free_bytes":9223372036854775807,"storage_global_limit_bytes":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	other := stage
	other.ID = "fedcba9876543210"
	if err := SavePeoplePublished(dir, id, 2, other); !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("min-free allowed a kept copy: %v", err)
	}
	if _, err := ReadPeoplePublished(dir, id, 2, other.ID); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("min-free refusal left a kept file: %v", err)
	}
}
