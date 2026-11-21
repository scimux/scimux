package team

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/storagebudget"
)

func TestProjectionIdentityDoesNotReplaceProtectedOrCorruptIdentity(t *testing.T) {
	t.Run("fresh", func(t *testing.T) {
		dir, id := t.TempDir(), sampleCopyID()
		first, err := EnsurePeopleCopyInstance(dir, id)
		if err != nil || first == "" {
			t.Fatalf("fresh: %v", err)
		}
		if again, err := EnsurePeopleCopyInstance(dir, id); err != nil || first != again {
			t.Fatalf("repeat: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":1,"storage_min_free_bytes":0}`), 0600); err != nil {
			t.Fatal(err)
		}
		if again, err := EnsurePeopleCopyInstance(dir, id); err != nil || first != again {
			t.Fatalf("full-budget retry: %v", err)
		}
	})
	t.Run("protected stage", func(t *testing.T) {
		dir, id, instance, stage, _ := setupDiscard(t)
		root, err := peopleRecordDir(dir, id, false, "people-identity")
		if err != nil {
			t.Fatal(err)
		}
		if err := root.Remove("0000000000000000.json"); err != nil {
			t.Fatal(err)
		}
		root.Close()
		if got, err := EnsurePeopleCopyInstance(dir, id); err == nil || got != "" {
			t.Fatal("missing identity recreated over protected data")
		}
		if _, err := ReadPeopleCopyInstance(dir, id); err == nil {
			t.Fatal("identity recreated")
		}
		if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
			t.Fatalf("stage lost: %v", err)
		}
		_ = instance
	})
	t.Run("corrupt identity", func(t *testing.T) {
		dir, id := t.TempDir(), sampleCopyID()
		if _, err := EnsurePeopleCopyInstance(dir, id); err != nil {
			t.Fatal(err)
		}
		root, err := peopleRecordDir(dir, id, false, "people-identity")
		if err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile("0000000000000000.json", []byte("broken"), 0600); err != nil {
			t.Fatal(err)
		}
		root.Close()
		if _, err := EnsurePeopleCopyInstance(dir, id); err == nil {
			t.Fatal("corrupt identity replaced")
		}
		root, err = peopleRecordDir(dir, id, false, "people-identity")
		if err != nil {
			t.Fatal(err)
		}
		b, err := root.ReadFile("0000000000000000.json")
		root.Close()
		if err != nil || string(b) != "broken" {
			t.Fatal("corrupt identity changed")
		}
	})
	t.Run("nonregular protected entry", func(t *testing.T) {
		dir, id := t.TempDir(), sampleCopyID()
		root, err := peopleRecordDir(dir, id, true, "people-stages")
		if err != nil {
			t.Fatal(err)
		}
		if err := root.Symlink("missing", "0123456789abcdef.json"); err != nil {
			t.Fatal(err)
		}
		root.Close()
		if _, err := EnsurePeopleCopyInstance(dir, id); err == nil {
			t.Fatal("nonregular protected entry authorized identity creation")
		}
		if _, err := ReadPeopleCopyInstance(dir, id); err == nil {
			t.Fatal("identity appeared")
		}
	})
	t.Run("bad storage", func(t *testing.T) {
		dir := t.TempDir() + "/file"
		if err := os.WriteFile(dir, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := EnsurePeopleCopyInstance(dir, sampleCopyID()); err == nil {
			t.Fatal("bad storage accepted")
		}
	})
}

func TestIdentityInitializationRefusesStorageFailures(t *testing.T) {
	t.Run("protected scan failure", func(t *testing.T) {
		dir, id := t.TempDir(), sampleCopyID()
		root, err := peopleRecordDir(dir, id, true, "people-stages")
		if err != nil {
			t.Fatal(err)
		}
		root.Close()
		original := peopleRecordIO
		t.Cleanup(func() { peopleRecordIO = original })
		peopleRecordIO.walk = func(_ fs.FS, _ string, visit fs.WalkDirFunc) error {
			return visit("", nil, errors.New("synthetic walk failure"))
		}
		if _, err := EnsurePeopleCopyInstance(dir, id); err == nil {
			t.Fatal("failed protected scan authorized identity")
		}
		peopleRecordIO = original
		if _, err := ReadPeopleCopyInstance(dir, id); err == nil {
			t.Fatal("failed scan created identity")
		}
	})
	t.Run("identity changed during admission", func(t *testing.T) {
		dir, id := t.TempDir(), sampleCopyID()
		if _, err := EnsurePeopleCopyInstance(dir, id); err != nil {
			t.Fatal(err)
		}
		root, err := peopleRecordDir(dir, id, false, "people-identity")
		if err != nil {
			t.Fatal(err)
		}
		if err := root.Remove("0000000000000000.json"); err != nil {
			t.Fatal(err)
		}
		root.Close()
		original := peopleRecordIO
		t.Cleanup(func() { peopleRecordIO = original })
		calls := 0
		peopleRecordIO.lstat = func(root *os.Root, name string) (os.FileInfo, error) {
			if name == "0000000000000000.json" {
				calls++
				if calls == 2 {
					return nil, errors.New("synthetic identity read failure")
				}
			}
			return original.lstat(root, name)
		}
		if _, err := EnsurePeopleCopyInstance(dir, id); err == nil || calls < 2 {
			t.Fatal("failed locked identity recheck authorized creation")
		}
	})
	t.Run("unavailable copy root", func(t *testing.T) {
		dir := t.TempDir() + "/file"
		if err := os.WriteFile(dir, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := rejectProtectedPeopleRecords(dir, sampleCopyID()); err == nil {
			t.Fatal("unavailable root accepted")
		}
	})
}

func TestIdentityInitializationRefusesEveryProtectedTree(t *testing.T) {
	for _, tree := range []string{"people-stages", "people-pending", "people-completed", "people-records"} {
		for _, damage := range []string{"missing", "corrupt"} {
			t.Run(tree+"/"+damage, func(t *testing.T) {
				dir, id, _, stage, pending := setupDiscard(t)
				if tree == "people-completed" || tree == "people-records" {
					if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
						t.Fatal(err)
					}
					if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
						t.Fatal(err)
					}
					if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err != nil {
						t.Fatal(err)
					}
				}
				copyRoot, err := peopleRecordDir(dir, id, false)
				if err != nil {
					t.Fatal(err)
				}
				for _, other := range []string{"people-stages", "people-pending", "people-completed", "people-records"} {
					if other != tree {
						if err := copyRoot.RemoveAll(other); err != nil {
							t.Fatal(err)
						}
					}
				}
				identity, err := peopleRecordDir(dir, id, false, "people-identity")
				if err != nil {
					t.Fatal(err)
				}
				if damage == "missing" {
					err = identity.Remove("0000000000000000.json")
				} else {
					err = identity.WriteFile("0000000000000000.json", []byte("broken"), 0600)
				}
				identity.Close()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := EnsurePeopleCopyInstance(dir, id); err == nil {
					t.Fatal("protected record authorized identity replacement")
				}
				if _, err := copyRoot.Stat(tree); err != nil {
					t.Fatalf("protected tree lost: %v", err)
				}
				copyRoot.Close()
			})
		}
	}
}

func TestIdentityInitializationLegacyTextAndTemporaryRecovery(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/0123456789abcdef", Author: id.Owner, At: "2026-01-01T00:00:00.000Z", Body: []byte(`{"v":1}`)}}); err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, true, "people-stages")
	if err != nil {
		t.Fatal(err)
	}
	name := "0123456789abcdef-0123456789abcdef.tmp"
	if err := root.WriteFile(name, []byte("abandoned"), 0600); err != nil {
		t.Fatal(err)
	}
	if value, err := EnsurePeopleCopyInstance(dir, id); err != nil || value == "" {
		t.Fatalf("legacy initialization: %v", err)
	}
	if _, err := root.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("temporary not recovered: %v", err)
	}
	root.Close()
}

func TestIdentityInitializationCrossProcessProtectedAdmission(t *testing.T) {
	if dir := os.Getenv("SCIMUX_TEST_PROJECTION_IDENTITY_DIR"); dir != "" {
		original := peopleRecordIO
		peopleRecordIO.lstat = func(root *os.Root, name string) (os.FileInfo, error) {
			info, err := original.lstat(root, name)
			if name == "0000000000000000.json" && errors.Is(err, os.ErrNotExist) {
				if err := os.WriteFile(filepath.Join(dir, "identity-observed"), []byte("ready"), 0600); err != nil {
					return nil, err
				}
			}
			return info, err
		}
		if _, err := EnsurePeopleCopyInstance(dir, sampleCopyID()); err == nil {
			t.Fatal("cross-process protected stage was bypassed")
		}
		return
	}
	dir, id := t.TempDir(), sampleCopyID()
	root, err := peopleRecordDir(dir, id, true, "people-identity")
	if err != nil {
		t.Fatal(err)
	}
	root.Close()
	release, err := storagebudget.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			release()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestIdentityInitializationCrossProcessProtectedAdmission$", "-test.count=1")
	child.Env = append(os.Environ(), "SCIMUX_TEST_PROJECTION_IDENTITY_DIR="+dir)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	for {
		if _, err := os.Stat(filepath.Join(dir, "identity-observed")); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("child did not observe missing identity")
		case <-time.After(5 * time.Millisecond):
		}
	}
	stage := syntheticPeopleStage()
	encoded, err := json.Marshal(stage)
	if err != nil {
		t.Fatal(err)
	}
	if err := savePeopleRecordLocked(dir, id, stage, encoded, "people-stages"); err != nil {
		t.Fatal(err)
	}
	release()
	locked = false
	if err := child.Wait(); err != nil {
		t.Fatalf("child: %v", err)
	}
	if _, err := ReadPeopleCopyInstance(dir, id); err == nil {
		t.Fatal("identity appeared over cross-process stage")
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("stage lost: %v", err)
	}
}
