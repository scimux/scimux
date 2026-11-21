package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scimux/scimux/internal/storagebudget"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExactStageDeletionRetryAndReplacement(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	revision := peopleInventoryRevision(stage)
	if err := DeletePeopleStageExact(dir, id, stage.ID, instance, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong revision accepted")
	}
	if err := DeletePeopleStageExact(dir, id, stage.ID, strings.Repeat("0", 32), revision); err == nil {
		t.Fatal("wrong copy accepted")
	}
	for i := 0; i < 2; i++ {
		if err := DeletePeopleStageExact(dir, id, stage.ID, instance, revision); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	next := stage
	next.Data = []byte("synthetic replacement")
	hash := sha256.Sum256(next.Data)
	next.Hash = hex.EncodeToString(hash[:])
	if err := SavePeopleStage(dir, id, next); err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStageExact(dir, id, stage.ID, instance, revision); err == nil {
		t.Fatal("stale retry removed replacement")
	}
	if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || got.Hash != next.Hash {
		t.Fatalf("replacement lost: %v", err)
	}
}

func TestExactStageDeletionPinned(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	revision := peopleInventoryRevision(stage)
	if err := DeletePeopleStageExact(dir, id, stage.ID, instance, revision); err == nil {
		t.Fatal("pinned stage removed")
	}
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStageExact(dir, id, stage.ID, instance, revision); err != nil {
		t.Fatal(err)
	}
}

func TestExactStageDeletionFailuresAndRetry(t *testing.T) {
	for _, mode := range []string{"remove", "open", "sync"} {
		t.Run(mode, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			stage := syntheticPeopleStage()
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			revision := peopleInventoryRevision(stage)
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			failed := false
			switch mode {
			case "remove":
				peopleRecordIO.remove = func(root *os.Root, name string) error {
					if name == stage.ID+".json" && strings.HasSuffix(root.Name(), "people-stages") {
						failed = true
						return errors.New("synthetic")
					}
					return original.remove(root, name)
				}
			case "open":
				peopleRecordIO.openDir = func(root *os.Root) (*os.File, error) {
					if strings.HasSuffix(root.Name(), "people-stages") {
						failed = true
						return nil, errors.New("synthetic")
					}
					return original.openDir(root)
				}
			case "sync":
				peopleRecordIO.sync = func(f *os.File) error {
					if strings.HasSuffix(filepath.Clean(f.Name()), "people-stages") {
						failed = true
						return errors.New("synthetic")
					}
					return original.sync(f)
				}
			}
			if err := DeletePeopleStageExact(dir, id, stage.ID, instance, revision); err == nil || !failed {
				t.Fatalf("fault not reported: %v", err)
			}
			peopleRecordIO = original
			stagePath := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages", stage.ID+".json")
			_, statErr := os.Stat(stagePath)
			if mode == "remove" && statErr != nil {
				t.Fatalf("remove failure lost stage: %v", statErr)
			}
			if mode != "remove" && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("barrier failure did not follow removal: %v", statErr)
			}
			if err := DeletePeopleStageExact(dir, id, stage.ID, instance, revision); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if _, err := ReadPeopleStage(dir, id, stage.ID); !errors.Is(err, ErrPeopleStageMissing) {
				t.Fatalf("stage remains: %v", err)
			}
		})
	}
}

func TestExactStageDeletionAcrossProcesses(t *testing.T) {
	if dir := os.Getenv("SCIMUX_TEST_EXACT_STAGE_DIR"); dir != "" {
		lock, err := os.OpenFile(filepath.Join(dir, ".storage.lock"), os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		probe := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(probe, syscall.EWOULDBLOCK) {
			if probe == nil {
				_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			}
			t.Fatalf("budget lock not held: %v", probe)
		}
		lock.Close()
		if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		err = DeletePeopleStageExact(dir, sampleCopyID(), syntheticPeopleStage().ID, os.Getenv("SCIMUX_TEST_EXACT_STAGE_COPY"), os.Getenv("SCIMUX_TEST_EXACT_STAGE_REV"))
		if err == nil {
			t.Fatal("stale cross-process revision accepted")
		}
		return
	}
	dir, id := t.TempDir(), sampleCopyID()
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	release, err := storagebudget.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			release()
		}
	}()
	child := exec.Command(os.Args[0], "-test.run=^TestExactStageDeletionAcrossProcesses$", "-test.timeout=15s")
	child.Env = append(os.Environ(), "SCIMUX_TEST_EXACT_STAGE_DIR="+dir, "SCIMUX_TEST_EXACT_STAGE_COPY="+instance, "SCIMUX_TEST_EXACT_STAGE_REV="+peopleInventoryRevision(stage))
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	ready := filepath.Join(dir, "ready")
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not observe storage lock")
		}
		time.Sleep(time.Millisecond)
	}
	replacement := stage
	replacement.Data = []byte("synthetic replacement")
	sum := sha256.Sum256(replacement.Data)
	replacement.Hash = hex.EncodeToString(sum[:])
	raw, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	root, err := peopleStageDir(dir, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(stage.ID+".json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	root.Close()
	release()
	held = false
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	finished = true
	got, err := ReadPeopleStage(dir, id, stage.ID)
	if err != nil || got.Hash != replacement.Hash {
		t.Fatalf("replacement lost: %v", err)
	}
}

func TestExactStageDeletionRejectsMalformedAndDamaged(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	revision := peopleInventoryRevision(stage)
	for _, tc := range []struct{ stageID, copy, revision string }{{stage.ID, "bad", revision}, {stage.ID, instance, "bad"}, {"bad", instance, revision}} {
		if err := DeletePeopleStageExact(dir, id, tc.stageID, tc.copy, tc.revision); err == nil {
			t.Fatal("malformed precondition accepted")
		}
	}
	root, err := peopleStageDir(dir, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(stage.ID+".json", []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if err := DeletePeopleStageExact(dir, id, stage.ID, instance, revision); err == nil {
		t.Fatal("damaged stage removed")
	}
}

func TestExactStageDeletionAtLegacyCapacity(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	root, err := peopleStageDir(dir, id, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4095; i++ {
		name := fmt.Sprintf("%016x.json", i+1)
		f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := f.Truncate(256 << 20); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	root.Close()
	revision := peopleInventoryRevision(stage)
	if err := DeletePeopleStageExact(dir, id, stage.ID, instance, revision); err != nil {
		t.Fatalf("at capacity: %v", err)
	}
	if err := DeletePeopleStageExact(dir, id, stage.ID, instance, revision); err != nil {
		t.Fatalf("retry at capacity: %v", err)
	}
}

func TestExactStageDeletionCannotCrossCopyRecreation(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	old, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	revision := peopleInventoryRevision(stage)
	if err := RemoveCopy(dir, id); err != nil {
		t.Fatal(err)
	}
	current, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil || current == old {
		t.Fatalf("replacement identity: %v", err)
	}
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStageExact(dir, id, stage.ID, old, revision); err == nil {
		t.Fatal("old copy removed replacement")
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("replacement lost: %v", err)
	}
}

func TestExactStageDeletionRejectsLostIdentityAndDamagedPin(t *testing.T) {
	t.Run("lost_identity", func(t *testing.T) {
		dir, id, instance, stage, _ := setupDiscard(t)
		path := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-identity", "0000000000000000.json")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := DeletePeopleStageExact(dir, id, stage.ID, instance, peopleInventoryRevision(stage)); err == nil {
			t.Fatal("lost identity accepted")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("identity recreated")
		}
		if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("damaged_pending", func(t *testing.T) {
		dir, id, instance, stage, pending := setupDiscard(t)
		root, err := peopleRecordDir(dir, id, false, "people-pending")
		if err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(pending.Key+".json", []byte("damaged"), 0600); err != nil {
			t.Fatal(err)
		}
		root.Close()
		if err := DeletePeopleStageExact(dir, id, stage.ID, instance, peopleInventoryRevision(stage)); err == nil {
			t.Fatal("damaged pending ignored")
		}
		if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
			t.Fatal(err)
		}
	})
}
