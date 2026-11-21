package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/scimux/scimux/internal/storagebudget"
)

func syntheticPeoplePending(stage PeopleStage) PeoplePending {
	return PeoplePending{Key: "fedcba9876543210", Author: "11111111111111111111111111111111", ConfigHash: stage.ConfigHash, Body: json.RawMessage(`{"v":1,"text":"Synthetic"}`), StageIDs: []string{stage.ID}}
}

func TestPeoplePublicationRecordsRequireDirectoryDurability(t *testing.T) {
	for _, tree := range []string{"people-pending", "people-records", "people-completed"} {
		t.Run(tree, func(t *testing.T) {
			dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
			pending := syntheticPeoplePending(stage)
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			if tree == "people-completed" {
				if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
					t.Fatal(err)
				}
				if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
					t.Fatal(err)
				}
				if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
					t.Fatal(err)
				}
			}
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			faults := 0
			peopleRecordIO.sync = func(f *os.File) error {
				info, err := f.Stat()
				if err != nil {
					return err
				}
				if info.IsDir() && strings.Contains(f.Name(), tree) {
					faults++
					return errors.New("synthetic directory sync failure")
				}
				return original.sync(f)
			}
			var first, read, retry error
			switch tree {
			case "people-pending":
				first = SavePeoplePending(dir, id, pending, []PeopleStage{stage})
				_, read = ReadPeoplePending(dir, id, pending.Key)
				retry = SavePeoplePending(dir, id, pending, []PeopleStage{stage})
			case "people-records":
				first = SavePeoplePublished(dir, id, 1, stage)
				_, read = ReadPeoplePublished(dir, id, 1, stage.ID)
				retry = SavePeoplePublished(dir, id, 1, stage)
			case "people-completed":
				first = ConfirmPeoplePending(dir, id, pending.Key, 1, false)
				_, read = ReadPeopleCompletion(dir, id, pending.Key)
				retry = ConfirmPeoplePending(dir, id, pending.Key, 1, false)
			}
			if faults == 0 {
				t.Fatal("directory sync fault was not injected")
			}
			if first == nil || read == nil || retry == nil {
				t.Errorf("undurable record accepted: first=%v read=%v retry=%v", first, read, retry)
			}
			if tree == "people-completed" && DeletePeopleStage(dir, id, stage.ID) == nil {
				t.Error("undurable completion released the pin")
			}
			peopleRecordIO = original
			switch tree {
			case "people-pending":
				if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
					t.Fatal(err)
				}
				got, err := ReadPeoplePending(dir, id, pending.Key)
				if err != nil || !reflect.DeepEqual(got, pending) {
					t.Fatalf("pending recovery: %v", err)
				}
			case "people-records":
				if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
					t.Fatal(err)
				}
				got, err := ReadPeoplePublished(dir, id, 1, stage.ID)
				if err != nil || !bytes.Equal(got.Data, stage.Data) {
					t.Fatalf("retained recovery: %v", err)
				}
			case "people-completed":
				if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err != nil {
					t.Fatal(err)
				}
				got, err := ReadPeopleCompletion(dir, id, pending.Key)
				if err != nil || got.Seq != 1 || !reflect.DeepEqual(got.Pending, pending) {
					t.Fatalf("completion recovery: %v", err)
				}
			}
		})
	}
}

func TestPeopleConfirmationKeepsPinWhenRetainedDirectoryCannotSync(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	faults := 0
	peopleRecordIO.sync = func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.IsDir() && strings.Contains(f.Name(), "people-records") {
			faults++
			return errors.New("synthetic retained directory sync failure")
		}
		return original.sync(f)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err == nil || faults == 0 {
		t.Fatalf("confirmation accepted without durable retained bytes: %v, faults=%d", err, faults)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("failed confirmation released the pin")
	}
	if _, err := ReadPeopleCompletion(dir, id, pending.Key); err == nil {
		t.Fatal("failed confirmation wrote a completion")
	}
	peopleRecordIO = original
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPeopleCompletion(dir, id, pending.Key)
	if err != nil || got.Seq != 1 || !reflect.DeepEqual(got.Pending, pending) {
		t.Fatalf("repair did not preserve completion: %v", err)
	}
}

func TestPeopleCompletionParentSyncPrecedesPendingRemoval(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	copyDir := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation)
	parent, err := os.Stat(copyDir)
	if err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	faults := 0
	peopleRecordIO.sync = func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if os.SameFile(parent, info) {
			if _, err := os.Stat(filepath.Join(copyDir, "people-completed")); err == nil {
				faults++
				return errors.New("synthetic completion parent sync failure")
			}
		}
		return original.sync(f)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err == nil || faults == 0 {
		t.Fatalf("undurable completion accepted: %v faults=%d", err, faults)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err == nil {
		t.Fatal("retry released pending before repair")
	}
	peopleRecordIO = original
	if _, err := ReadPeoplePending(dir, id, pending.Key); err != nil {
		t.Fatalf("pending lost: %v", err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("pin released")
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err != nil {
		t.Fatalf("repair: %v", err)
	}
	completion, err := ReadPeopleCompletion(dir, id, pending.Key)
	if err != nil || completion.Seq != 1 || !reflect.DeepEqual(completion.Pending, pending) {
		t.Fatalf("receipt changed: %v", err)
	}
}

func TestPeoplePendingTransientSyncRepairKeepsConcurrentIdentity(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	faults := 0
	peopleRecordIO.sync = func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.IsDir() && strings.Contains(f.Name(), "people-pending") && faults == 0 {
			faults++
			return errors.New("synthetic transient sync failure")
		}
		return original.sync(f)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil || faults != 1 {
		t.Fatalf("expected transient sync failure: %v, faults=%d", err, faults)
	}
	peopleRecordIO = original
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := ReadPeoplePending(dir, id, pending.Key)
			if err != nil || !reflect.DeepEqual(got, pending) {
				t.Errorf("concurrent repair: %v", err)
			}
		}()
	}
	wg.Wait()
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	changed := pending
	changed.Body = json.RawMessage(`{"v":1,"text":"different"}`)
	if err := SavePeoplePending(dir, id, changed, []PeopleStage{stage}); err == nil {
		t.Fatal("different retry replaced frozen publication")
	}
	got, err := ReadPeoplePending(dir, id, pending.Key)
	if err != nil || !reflect.DeepEqual(got, pending) {
		t.Fatalf("frozen publication changed: %v", err)
	}
}

func TestPeoplePendingValidatedEncodingAndCompletionRoundTrip(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	pending.Body = json.RawMessage(`{"text":"synthetic <>&","number":9007199254740993}`)
	for _, invalid := range []PeoplePending{func() PeoplePending { p := pending; p.Body = json.RawMessage(`{`); return p }(), func() PeoplePending { p := pending; p.Key = "invalid"; return p }()} {
		missing := filepath.Join(dir, "not-created")
		if err := SavePeoplePending(missing, id, invalid, []PeopleStage{stage}); !errors.Is(err, ErrPeopleStage) {
			t.Fatalf("invalid manifest: %v", err)
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Fatalf("invalid manifest touched storage: %v", err)
		}
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	// encoding/json normalizes HTML escapes while retaining the integer exactly.
	expected := pending
	expected.Body, _ = json.Marshal(pending.Body)
	got, err := ReadPeoplePending(dir, id, pending.Key)
	if err != nil || !reflect.DeepEqual(got, expected) {
		t.Fatalf("pending round trip changed content: %#v %v", got, err)
	}
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err != nil {
		t.Fatal(err)
	}
	completed, err := ReadPeopleCompletion(dir, id, pending.Key)
	if err != nil || completed.Seq != 1 || !reflect.DeepEqual(completed.Pending, expected) {
		t.Fatalf("completion round trip changed content: %#v %v", completed, err)
	}
	retained, err := ReadPeoplePublished(dir, id, 1, stage.ID)
	if err != nil || !reflect.DeepEqual(retained, stage) {
		t.Fatalf("retained stage changed: %#v %v", retained, err)
	}
}

func TestPeoplePendingWriteFailuresPreserveStagesAndRetryIdentity(t *testing.T) {
	for _, fault := range []string{"file sync", "directory sync"} {
		t.Run(fault, func(t *testing.T) {
			dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			pending := syntheticPeoplePending(stage)
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			linked := false
			peopleRecordIO.link = func(r *os.Root, old, name string) error {
				err := original.link(r, old, name)
				if err == nil {
					linked = true
				}
				return err
			}
			peopleRecordIO.sync = func(f *os.File) error {
				info, err := f.Stat()
				if err != nil {
					return err
				}
				if (fault == "file sync" && !info.IsDir()) || (fault == "directory sync" && linked && info.IsDir() && strings.Contains(f.Name(), "people-pending")) {
					return errors.New("synthetic sync failure")
				}
				return f.Sync()
			}
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
				t.Fatal("sync failure reported success")
			}
			peopleRecordIO = original
			retained, err := ReadPeopleStage(dir, id, stage.ID)
			if err != nil || !bytes.Equal(retained.Data, stage.Data) {
				t.Fatalf("staged bytes lost: %v", err)
			}
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
				t.Fatalf("same-key retry failed: %v", err)
			}
			got, err := ReadPeoplePending(dir, id, pending.Key)
			if err != nil || !reflect.DeepEqual(got, pending) {
				t.Fatalf("retry changed frozen publication: %v", err)
			}
			if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
				t.Fatal("retry did not pin stage")
			}
		})
	}
}

func TestPeopleConfirmationCleanupFailuresPreserveReceiptAndBytes(t *testing.T) {
	for _, fault := range []string{"receipt write", "pending removal", "pending directory open", "pending directory sync"} {
		t.Run(fault, func(t *testing.T) {
			dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			pending := syntheticPeoplePending(stage)
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
				t.Fatal(err)
			}
			if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
				t.Fatal(err)
			}
			if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
				t.Fatal(err)
			}
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			failure := errors.New("synthetic cleanup failure")
			removed, injected := false, false
			if fault == "pending directory open" || fault == "pending directory sync" {
				peopleRecordIO.remove = func(r *os.Root, name string) error {
					err := original.remove(r, name)
					if name == pending.Key+".json" && err == nil {
						removed = true
					}
					return err
				}
			}
			switch fault {
			case "receipt write":
				peopleRecordIO.write = func(*os.File, []byte) (int, error) { return 0, failure }
			case "pending removal":
				peopleRecordIO.remove = func(r *os.Root, name string) error {
					if name == pending.Key+".json" {
						return failure
					}
					return r.Remove(name)
				}
			case "pending directory open":
				peopleRecordIO.openDir = func(r *os.Root) (*os.File, error) {
					if removed {
						injected = true
						return nil, failure
					}
					return original.openDir(r)
				}
			case "pending directory sync":
				peopleRecordIO.sync = func(f *os.File) error {
					if removed {
						injected = true
						return failure
					}
					return original.sync(f)
				}
			}
			if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err == nil {
				t.Fatal("cleanup failure reported success")
			}
			if (fault == "pending directory open" || fault == "pending directory sync") && (!removed || !injected) {
				t.Fatal("post-removal fault was not injected")
			}
			peopleRecordIO = original
			stored, err := ReadPeoplePublished(dir, id, 1, stage.ID)
			if err != nil || !bytes.Equal(stored.Data, stage.Data) {
				t.Fatalf("retained bytes lost: %v", err)
			}
			if _, err := ReadPeoplePending(dir, id, pending.Key); err == nil {
				if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err != nil {
					t.Fatalf("same-key retry: %v", err)
				}
			}
			receipt, err := ReadPeopleCompletion(dir, id, pending.Key)
			if err != nil || receipt.Seq != 1 || !reflect.DeepEqual(receipt.Pending, pending) {
				t.Fatalf("receipt lost or changed: %v", err)
			}
		})
	}
}

func TestPeoplePendingInspectionFailuresKeepStagePinned(t *testing.T) {
	for _, fault := range []string{"stat pending", "open directory", "read directory"} {
		t.Run(fault, func(t *testing.T) {
			dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			pending := syntheticPeoplePending(stage)
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
				t.Fatal(err)
			}
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			failure := errors.New("synthetic inspection failure")
			switch fault {
			case "stat pending":
				peopleRecordIO.lstat = func(r *os.Root, name string) (os.FileInfo, error) {
					if name == "people-pending" {
						return nil, failure
					}
					return r.Lstat(name)
				}
			case "open directory":
				peopleRecordIO.openDir = func(r *os.Root) (*os.File, error) {
					if filepath.Base(r.Name()) == "people-pending" {
						return nil, failure
					}
					return original.openDir(r)
				}
			case "read directory":
				peopleRecordIO.readDir = func(*os.File) ([]os.DirEntry, error) { return nil, failure }
			}
			if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
				t.Fatal("inspection failure allowed stage deletion")
			}
			peopleRecordIO = original
			if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
				t.Fatal("pending publication lost its pin")
			}
			got, err := ReadPeopleStage(dir, id, stage.ID)
			if err != nil || !bytes.Equal(got.Data, stage.Data) {
				t.Fatalf("pinned stage lost: %v", err)
			}
		})
	}
}

func TestPeoplePendingAdmissionStatFailurePreservesStageForRetry(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	root, err := peopleRecordDir(dir, id, true, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	root.Close()
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	calls := 0
	peopleRecordIO.lstat = func(r *os.Root, name string) (os.FileInfo, error) {
		if name == pending.Key+".json" {
			calls++
			if calls == 1 {
				return nil, errors.New("synthetic pending stat failure")
			}
		}
		return r.Lstat(name)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
		t.Fatal("failed pending inspection was accepted")
	}
	peopleRecordIO = original
	if calls != 1 {
		t.Fatalf("expected admission lookup, got %d", calls)
	}
	if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("stage changed: %v", err)
	}
	if _, err := ReadPeoplePending(dir, id, pending.Key); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("unexpected pending publication: %v", err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatalf("same-key retry: %v", err)
	}
}

func TestPeoplePendingRetriesTransientInitialReadWithoutReplacingFrozenPublication(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	failures := 0
	peopleRecordIO.openRead = func(r *os.Root, name string) (*os.File, error) {
		if name == pending.Key+".json" && failures == 0 {
			failures++
			return nil, errors.New("synthetic transient pending read")
		}
		return r.Open(name)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
		t.Fatal("failed pending read was acknowledged")
	}
	peopleRecordIO = original
	if failures != 1 {
		t.Fatalf("transient read not exercised: %d", failures)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatalf("exact retry after transient read: %v", err)
	}
	got, err := ReadPeoplePending(dir, id, pending.Key)
	if err != nil || !reflect.DeepEqual(got, pending) {
		t.Fatalf("frozen publication changed: %v", err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("publication lost its stage pin")
	}
	stored, err := ReadPeopleStage(dir, id, stage.ID)
	if err != nil || !bytes.Equal(stored.Data, stage.Data) {
		t.Fatalf("stage bytes lost: %v", err)
	}
}

func TestPeoplePendingAdmissionDirectoryFailurePreservesStageForRetry(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	original := copyOps
	t.Cleanup(func() { copyOps = original })
	copyOps.openChild = func(r *os.Root, name string) (*os.Root, error) {
		if name == "people-pending" {
			return nil, errors.New("synthetic pending directory failure")
		}
		return r.OpenRoot(name)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
		t.Fatal("failed pending directory admission was accepted")
	}
	copyOps = original
	if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("stage changed: %v", err)
	}
	if _, err := ReadPeoplePending(dir, id, pending.Key); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("unexpected pending publication: %v", err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatalf("same-key retry: %v", err)
	}
}

func TestPeoplePendingFreezesManifestAndPinsStage(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	changed := pending
	changed.Body = json.RawMessage(`{"v":1,"text":"Different"}`)
	if err := SavePeoplePending(dir, id, changed, []PeopleStage{stage}); err == nil {
		t.Fatal("same key accepted different body")
	}
	got, err := ReadPeoplePending(dir, id, pending.Key)
	if err != nil || string(got.Body) != string(pending.Body) {
		t.Fatalf("frozen body not retained: %v", err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("pending publication did not pin stage")
	}
}

func TestPeoplePendingRefusesBudgetWithoutLosingStage(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":1,"storage_min_free_bytes":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("pending bypassed budget: %v", err)
	}
	if _, err := ReadPeoplePending(dir, id, pending.Key); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("refused pending exists: %v", err)
	}
	if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || string(got.Data) != string(stage.Data) {
		t.Fatalf("budget failure lost stage: %v", err)
	}
}

func TestPeoplePendingRejectsUnsafeCompletionDirectory(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	id := sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation)
	if err := os.Symlink(outside, filepath.Join(base, "people-completed")); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, syntheticPeoplePending(stage), []PeopleStage{stage}); err == nil {
		t.Fatal("unsafe completion directory accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside directory changed: %v", err)
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("stage lost after refusal: %v", err)
	}
}

func TestPeoplePendingRejectsUnsafePendingDirectory(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	id := sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation)
	if err := os.Symlink(outside, filepath.Join(base, "people-pending")); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, syntheticPeoplePending(stage), []PeopleStage{stage}); err == nil {
		t.Fatal("unsafe pending directory accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside directory changed: %v", err)
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("stage lost after refusal: %v", err)
	}
}

func TestPeopleStageRemovalFailsClosedForAbsentCopyOrUnsafePendingRoot(t *testing.T) {
	id := sampleCopyID()
	if err := DeletePeopleStage(t.TempDir(), id, "0123456789abcdef"); err == nil {
		t.Fatal("missing copy accepted stage removal")
	}
	dir, outside := t.TempDir(), t.TempDir()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation)
	if err := os.Symlink(outside, filepath.Join(base, "people-pending")); err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("unsafe pending root permitted removing a potentially pinned stage")
	}
	if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("stage lost after refusal: %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unsafe target was modified: %v", err)
	}
}

func TestPeopleConfirmationRejectsInvalidCopyBeforeFilesystemAccess(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	id.Generation = "invalid"
	if err := ConfirmPeoplePending(dir, id, "0123456789abcdef", 1, false); err == nil {
		t.Fatal("invalid copy was confirmed")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid copy touched storage: %v", err)
	}
}

func TestPeoplePendingRejectsInvalidCopyAndAllowsOtherCompletionKeys(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	invalid := id
	invalid.Generation = "invalid"
	if err := SavePeoplePending(dir, invalid, pending, []PeopleStage{stage}); err == nil {
		t.Fatal("invalid copy accepted pending publication")
	}
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, true, "people-completed")
	if err != nil {
		t.Fatal(err)
	}
	root.Close()
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatalf("an empty completion directory blocked a new key: %v", err)
	}
	if _, err := ReadPeoplePending(dir, id, pending.Key); err != nil {
		t.Fatalf("valid pending publication missing: %v", err)
	}
}

func TestPeoplePendingDirectoryRejectsUnknownRecordsButIgnoresOtherFiles(t *testing.T) {
	for _, unknownRecord := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-record", true: "unknown record"}[unknownRecord], func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			stage := syntheticPeopleStage()
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			root, err := peopleRecordDir(dir, id, true, "people-pending")
			if err != nil {
				t.Fatal(err)
			}
			name := "unrelated.txt"
			if unknownRecord {
				name = "invalid.json"
			}
			err = root.WriteFile(name, []byte("synthetic"), 0600)
			root.Close()
			if err != nil {
				t.Fatal(err)
			}
			err = DeletePeopleStage(dir, id, stage.ID)
			if unknownRecord {
				if err == nil {
					t.Fatal("unknown pending record allowed unpinning")
				}
				if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
					t.Fatalf("refusal lost staged bytes: %v", err)
				}
			} else if err != nil {
				t.Fatalf("unrelated non-record blocked removal: %v", err)
			}
		})
	}
}

func TestPeoplePendingPinBlocksStageRemoval(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	raw, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	record := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(sum[:]), Name: "pending.json", MIME: "application/json", ConfigHash: pending.ConfigHash, Data: raw}
	if err := savePeopleRecord(dir, id, record, "people-pending"); err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("existing pending association did not prevent deletion")
	}
}

func TestPeopleRetiredPublicationCannotBeAdmittedAgain(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Rename(pending.Key+".json", pending.Key+".retired"); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
		t.Fatal("retired publication was admitted again")
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("retired publication still pins stage: %v", err)
	}
}

func TestPeoplePendingRetryRejectsFailedDurabilityBarrier(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	faults := 0
	peopleRecordIO.sync = func(f *os.File) error {
		if strings.Contains(f.Name(), "people-pending") && faults == 0 {
			faults++
			return errors.New("synthetic barrier failure")
		}
		return original.sync(f)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
		t.Fatal("retry accepted after its pending barrier failed")
	}
	peopleRecordIO = original
	if faults != 1 {
		t.Fatalf("barrier fault not exercised: %d", faults)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatalf("retry did not repair barrier: %v", err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("pending stage became unpinned")
	}
}

func TestPeopleDamagedRetirementCannotReleaseStage(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Rename(pending.Key+".json", pending.Key+".retired"); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(pending.Key+".retired", []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("damaged retired record released the stage")
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("stage bytes lost: %v", err)
	}
}

func TestPeoplePendingPinsOnlyItsOwnStages(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	other := stage
	other.ID = "0011223344556677"
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	if err := SavePeopleStage(dir, id, other); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, syntheticPeoplePending(stage), []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStage(dir, id, other.ID); err != nil {
		t.Fatalf("unrelated stage was pinned: %v", err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("referenced stage was unpinned")
	}
	if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("pinned data lost: %v", err)
	}
}

func TestPeopleConfirmationRejectsDamagedPendingAndReceipt(t *testing.T) {
	for _, damage := range []string{"pending envelope", "pending content", "completion envelope", "missing retained"} {
		t.Run(damage, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			stage := syntheticPeopleStage()
			pending := syntheticPeoplePending(stage)
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
				t.Fatal(err)
			}
			if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
				t.Fatal(err)
			}
			if damage != "missing retained" {
				if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
					t.Fatal(err)
				}
			}
			base := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation)
			switch damage {
			case "pending envelope":
				if err := os.WriteFile(filepath.Join(base, "people-pending", pending.Key+".json"), []byte("synthetic damage"), 0600); err != nil {
					t.Fatal(err)
				}
			case "pending content":
				data := []byte(`{"key":"invalid"}`)
				sum := sha256.Sum256(data)
				envelope := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(sum[:]), Name: "pending.json", MIME: "application/json", ConfigHash: pending.ConfigHash, Data: data}
				raw, err := json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(base, "people-pending", pending.Key+".json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "completion envelope":
				if err := os.MkdirAll(filepath.Join(base, "people-completed"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(base, "people-completed", pending.Key+".json"), []byte("synthetic damage"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err == nil {
				t.Fatal("damaged or unavailable publication was confirmed")
			}
			if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
				t.Fatal("failed confirmation released staged bytes")
			}
			if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, stage.Data) {
				t.Fatalf("failed confirmation lost bytes: %v", err)
			}
		})
	}
}

func TestPeopleConfirmationFailureKeepsPendingForExplicitRetry(t *testing.T) {
	for _, failure := range []string{"budget", "missing stage directory", "unsafe completion directory"} {
		t.Run(failure, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			stage := syntheticPeopleStage()
			pending := syntheticPeoplePending(stage)
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
				t.Fatal(err)
			}
			if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
				t.Fatal(err)
			}
			if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation)
			switch failure {
			case "budget":
				if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":1,"storage_min_free_bytes":0}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing stage directory":
				if err := os.RemoveAll(filepath.Join(base, "people-stages")); err != nil {
					t.Fatal(err)
				}
			case "unsafe completion directory":
				if err := os.Symlink(t.TempDir(), filepath.Join(base, "people-completed")); err != nil {
					t.Fatal(err)
				}
			}
			if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err == nil {
				t.Fatal("confirmation ignored failed storage boundary")
			}
			if got, err := ReadPeoplePending(dir, id, pending.Key); err != nil || !bytes.Equal(got.Body, pending.Body) {
				t.Fatalf("pending identity lost: %v", err)
			}
			switch failure {
			case "budget":
				if err := os.Remove(filepath.Join(dir, "settings.json")); err != nil {
					t.Fatal(err)
				}
			case "missing stage directory":
				if err := SavePeopleStage(dir, id, stage); err != nil {
					t.Fatal(err)
				}
			case "unsafe completion directory":
				if err := os.Remove(filepath.Join(base, "people-completed")); err != nil {
					t.Fatal(err)
				}
			}
			if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err != nil {
				t.Fatalf("explicit retry after repair failed: %v", err)
			}
			if _, err := ReadPeopleCompletion(dir, id, pending.Key); err != nil {
				t.Fatalf("completion was not retained: %v", err)
			}
		})
	}
}

func TestPeopleConfirmationAllowsOtherCompletionRecords(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, true, "people-completed")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("unrelated.txt", []byte("synthetic"), 0600); err != nil {
		root.Close()
		t.Fatal(err)
	}
	root.Close()
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err != nil {
		t.Fatalf("unrelated completion data blocked confirmation: %v", err)
	}
	if got, err := ReadPeopleCompletion(dir, id, pending.Key); err != nil || got.Seq != 1 {
		t.Fatalf("exact completion missing: %v", err)
	}
}

func TestPeoplePendingCannotUnpinBeforeRetainedAssociation(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err == nil {
		t.Fatal("unconfirmed bytes unpinned")
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("pending stage deleted")
	}
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err != nil {
		t.Fatal(err)
	}
	completion, err := ReadPeopleCompletion(dir, id, pending.Key)
	if err != nil || completion.Seq != 1 || string(completion.Pending.Body) != string(pending.Body) {
		t.Fatalf("completed exact publication was not retained: %v", err)
	}
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	changed := pending
	changed.Body = json.RawMessage(`{"v":1,"text":"different"}`)
	if err := SavePeoplePending(dir, id, changed, []PeopleStage{stage}); err == nil {
		t.Fatal("completed key was reused for a different body")
	}
	retained, err := ReadPeoplePublished(dir, id, 1, stage.ID)
	if err != nil || string(retained.Data) != string(stage.Data) {
		t.Fatalf("retained file missing: %v", err)
	}
}

func TestPeoplePendingNeedsExactRetainedCommentBeforeUnpin(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	stage := syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err == nil {
		t.Fatal("missing durable comment unpinned the publication")
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("stage was unpinned without history")
	}
	wrong := MirrorRecord{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: json.RawMessage(`{"v":1,"text":"different"}`)}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{wrong}); err != nil {
		t.Fatal(err)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err == nil {
		t.Fatal("different retained body unpinned the publication")
	}
}

func TestPeopleConfirmationRejectsForeignHistoryAndDifferentRetainedBytes(t *testing.T) {
	for _, kind := range []string{"author", "key", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			stage := syntheticPeopleStage()
			pending := syntheticPeoplePending(stage)
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
				t.Fatal(err)
			}
			retained := stage
			if kind == "bytes" {
				retained.Data = []byte("other synthetic bytes")
				sum := sha256.Sum256(retained.Data)
				retained.Hash = hex.EncodeToString(sum[:])
			}
			if err := SavePeoplePublished(dir, id, 1, retained); err != nil {
				t.Fatal(err)
			}
			entry := MirrorRecord{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}
			if kind == "author" {
				entry.Author = strings.Repeat("b", 32)
			}
			if kind == "key" {
				entry.Key = "c/" + strings.Repeat("b", 16)
			}
			if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{entry}); err != nil {
				t.Fatal(err)
			}
			if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err == nil {
				t.Fatal("mismatched publication confirmed")
			}
			if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
				t.Fatal("mismatched publication released its pin")
			}
			if _, err := ReadPeopleCompletion(dir, id, pending.Key); err == nil {
				t.Fatal("mismatched publication acquired a receipt")
			}
		})
	}
}

func TestPeopleConfirmationKeepsPendingWhenStoredFileIsDamaged(t *testing.T) {
	for _, target := range []string{"stage", "retained"} {
		t.Run(target, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			stage := syntheticPeopleStage()
			pending := syntheticPeoplePending(stage)
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
				t.Fatal(err)
			}
			if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
				t.Fatal(err)
			}
			if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
				t.Fatal(err)
			}
			parts := []string{"people-stages"}
			if target == "retained" {
				parts = []string{"people-records", "comments", "1"}
			}
			root, err := peopleRecordDir(dir, id, false, parts...)
			if err != nil {
				t.Fatal(err)
			}
			err = root.WriteFile(stage.ID+".json", []byte("synthetic damage"), 0600)
			root.Close()
			if err != nil {
				t.Fatal(err)
			}
			if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err == nil {
				t.Fatal("damaged file allowed confirmation")
			}
			if _, err := ReadPeoplePending(dir, id, pending.Key); err != nil {
				t.Fatalf("damaged file lost pending retry: %v", err)
			}
			if _, err := ReadPeopleCompletion(dir, id, pending.Key); err == nil {
				t.Fatal("damaged file acquired a completion receipt")
			}
		})
	}
}

func TestPeoplePendingRejectsInvalidOrUnboundFrozenManifests(t *testing.T) {
	stage := syntheticPeopleStage()
	valid := syntheticPeoplePending(stage)
	for _, tc := range []struct {
		name   string
		change func(*PeoplePending)
	}{
		{"key", func(p *PeoplePending) { p.Key = "bad" }}, {"author", func(p *PeoplePending) { p.Author = "bad" }}, {"configuration", func(p *PeoplePending) { p.ConfigHash = "bad" }},
		{"instance", func(p *PeoplePending) { p.OwnerInstance = "bad\ninstance" }}, {"json", func(p *PeoplePending) { p.Body = json.RawMessage(`{`) }},
		{"empty", func(p *PeoplePending) { p.StageIDs = nil }}, {"duplicate", func(p *PeoplePending) { p.StageIDs = []string{stage.ID, stage.ID} }}, {"invalid-stage", func(p *PeoplePending) { p.StageIDs = []string{"bad"} }},
		{"too-many", func(p *PeoplePending) { p.StageIDs = make([]string, 17) }}, {"too-large-body", func(p *PeoplePending) { p.Body = json.RawMessage(`"` + strings.Repeat("x", 256<<10) + `"`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			tc.change(&p)
			if validPeoplePending(p) {
				t.Fatal("invalid pending accepted")
			}
			if err := SavePeoplePending(t.TempDir(), sampleCopyID(), p, []PeopleStage{stage}); err == nil {
				t.Fatal("invalid pending saved")
			}
		})
	}
	dir := t.TempDir()
	id := sampleCopyID()
	if err := SavePeoplePending(dir, id, valid, []PeopleStage{stage}); err == nil {
		t.Fatal("missing stage accepted")
	}
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	wrong := valid
	wrong.ConfigHash = strings.Repeat("b", 64)
	if err := SavePeoplePending(dir, id, wrong, []PeopleStage{stage}); err == nil {
		t.Fatal("stage from another configuration pinned")
	}
	wrong = valid
	wrong.OwnerInstance = "synthetic-instance"
	if err := SavePeoplePending(dir, id, wrong, []PeopleStage{stage}); err == nil {
		t.Fatal("stage from another native identity pinned")
	}
	for _, tc := range []struct {
		key string
		seq uint64
	}{{"bad", 1}, {valid.Key, 0}, {valid.Key, 1}} {
		if err := ConfirmPeoplePending(dir, id, tc.key, tc.seq, false); err == nil {
			t.Fatal("unrecorded pending confirmed")
		}
	}
}

func TestDamagedPeoplePendingCannotUnpinOrRemoveStage(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, true, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("not-a-key.json", []byte("synthetic damage"), 0600); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("damaged pending directory allowed stage removal")
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatal("stage was lost after refused removal")
	}
}

func TestPeoplePendingDoesNotOverwriteDamagedExistingRecord(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, true, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	damaged := []byte("synthetic damaged pending record")
	if err := root.WriteFile(pending.Key+".json", damaged, 0600); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
		t.Fatal("damaged pending record was replaced")
	}
	got, err := root.ReadFile(pending.Key + ".json")
	if err != nil || !bytes.Equal(got, damaged) {
		t.Fatalf("damaged record was changed: %v", err)
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("refusal lost staged file: %v", err)
	}
}

func TestPeoplePendingDoesNotReplaceSymlinkedRecord(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, true, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	root.Close()
	outside := filepath.Join(t.TempDir(), "synthetic-record")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-pending", pending.Key+".json")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
		t.Fatal("symlinked pending record was replaced")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "untouched" {
		t.Fatalf("outside target changed: %v", err)
	}
	if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("refusal lost staged bytes: %v", err)
	}
}

func TestPeopleCompletionRejectsDamagedOrMismatchedReceipt(t *testing.T) {
	stage := syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	valid, err := json.Marshal(PeopleCompletion{Pending: pending, Seq: 1})
	if err != nil {
		t.Fatal(err)
	}
	wrongKey := pending
	wrongKey.Key = "0011223344556677"
	wrongConfig := pending
	wrongConfig.ConfigHash = strings.Repeat("b", 64)
	wrongOwner := pending
	wrongOwner.OwnerInstance = "synthetic-instance"
	invalidPending := pending
	invalidPending.Author = "invalid"
	encode := func(p PeoplePending, seq uint64) []byte {
		data, err := json.Marshal(PeopleCompletion{Pending: p, Seq: seq})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"malformed", []byte("{")},
		{"unknown field", append(append([]byte(nil), valid[:len(valid)-1]...), []byte(`,"extra":true}`)...)},
		{"trailing value", append(append([]byte(nil), valid...), []byte(` {}`)...)},
		{"zero sequence", encode(pending, 0)},
		{"invalid pending", encode(invalidPending, 1)},
		{"wrong key", encode(wrongKey, 1)},
		{"wrong configuration", encode(wrongConfig, 1)},
		{"wrong owner", encode(wrongOwner, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			sum := sha256.Sum256(tc.data)
			record := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(sum[:]), Name: "completion.json", MIME: "application/json", ConfigHash: pending.ConfigHash, Data: tc.data}
			if err := savePeopleRecord(dir, id, record, "people-completed"); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadPeopleCompletion(dir, id, pending.Key); err == nil {
				t.Fatal("damaged completion accepted as a receipt")
			}
		})
	}
	if _, err := ReadPeopleCompletion(t.TempDir(), sampleCopyID(), pending.Key); err == nil {
		t.Fatal("missing completion accepted")
	}
}

func TestDamagedPendingRecordsCannotReleaseStagePins(t *testing.T) {
	for _, malformed := range []bool{true, false} {
		t.Run(map[bool]string{true: "invalid envelope", false: "invalid pending body"}[malformed], func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			stage := syntheticPeopleStage()
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			key := syntheticPeoplePending(stage).Key
			if malformed {
				root, err := peopleRecordDir(dir, id, true, "people-pending")
				if err != nil {
					t.Fatal(err)
				}
				err = root.WriteFile(key+".json", []byte("synthetic damage"), 0600)
				root.Close()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				data := []byte(`{"key":"invalid"}`)
				sum := sha256.Sum256(data)
				record := PeopleStage{ID: key, Hash: hex.EncodeToString(sum[:]), Name: "pending.json", MIME: "application/json", ConfigHash: stage.ConfigHash, Data: data}
				if err := savePeopleRecord(dir, id, record, "people-pending"); err != nil {
					t.Fatal(err)
				}
			}
			if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
				t.Fatal("damaged pending record allowed a potentially pinned stage to be removed")
			}
			if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
				t.Fatalf("refused deletion lost stage: %v", err)
			}
		})
	}
}

func TestPeopleConfirmationRecoversAfterCompletionWrittenBeforeUnpin(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact receipt", true: "conflicting receipt"}[conflict], func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			stage := syntheticPeopleStage()
			pending := syntheticPeoplePending(stage)
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
				t.Fatal(err)
			}
			if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
				t.Fatal(err)
			}
			if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
				t.Fatal(err)
			}
			seq := uint64(1)
			if conflict {
				seq = 2
			}
			data, err := json.Marshal(PeopleCompletion{Pending: pending, Seq: seq})
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			record := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(sum[:]), Name: "completion.json", MIME: "application/json", ConfigHash: pending.ConfigHash, Data: data}
			if err := savePeopleRecord(dir, id, record, "people-completed"); err != nil {
				t.Fatal(err)
			}
			err = ConfirmPeoplePending(dir, id, pending.Key, 1, false)
			if conflict {
				if err == nil {
					t.Fatal("conflicting receipt accepted")
				}
				if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
					t.Fatal("conflict released the pending stage")
				}
			} else {
				if err != nil {
					t.Fatalf("exact receipt recovery failed: %v", err)
				}
				if err := DeletePeopleStage(dir, id, stage.ID); err != nil {
					t.Fatalf("completed stage remained pinned: %v", err)
				}
			}
		})
	}
}
func TestPeopleCompletionRestartBeforePendingRemoval(t *testing.T) {
	if dir := os.Getenv("SCIMUX_TEST_COMPLETION_RESTART_DIR"); dir != "" {
		id, stage := sampleCopyID(), syntheticPeopleStage()
		pending := syntheticPeoplePending(stage)
		receipt, err := ReadPeopleCompletion(dir, id, pending.Key)
		if err != nil || receipt.Seq != 1 || !reflect.DeepEqual(receipt.Pending, pending) {
			t.Fatalf("receipt: %v", err)
		}
		if _, err := ReadPeoplePending(dir, id, pending.Key); err != nil {
			t.Fatalf("pending: %v", err)
		}
		if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
			t.Fatal("premature unpin")
		}
		if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err != nil {
			t.Fatal(err)
		}
		if err := DeletePeopleStage(dir, id, stage.ID); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	peopleRecordIO.remove = func(root *os.Root, name string) error {
		if name == pending.Key+".json" {
			return errors.New("synthetic interruption")
		}
		return original.remove(root, name)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err == nil {
		t.Fatal("interruption accepted")
	}
	peopleRecordIO = original
	cmd := exec.Command(os.Args[0], "-test.run=^TestPeopleCompletionRestartBeforePendingRemoval$", "-test.v")
	cmd.Env = append(os.Environ(), "SCIMUX_TEST_COMPLETION_RESTART_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
}

func TestPeoplePendingComparesReviewedStage(t *testing.T) {
	cases := []struct {
		name   string
		change func(*PeopleStage)
	}{
		{"id", func(s *PeopleStage) { s.ID = "fedcba9876543210" }},
		{"hash", func(s *PeopleStage) { s.Hash = strings.Repeat("0", 64) }},
		{"name", func(s *PeopleStage) { s.Name = "different.txt" }},
		{"mime", func(s *PeopleStage) { s.MIME = "application/octet-stream" }},
		{"config", func(s *PeopleStage) { s.ConfigHash = strings.Repeat("0", 64) }},
		{"owner", func(s *PeopleStage) { s.OwnerInstance = "different" }},
		{"bytes", func(s *PeopleStage) { s.Data = []byte("different synthetic data") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			reviewed := stage
			tc.change(&reviewed)
			pending := syntheticPeoplePending(stage)
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{reviewed}); !errors.Is(err, ErrPeopleStage) {
				t.Fatalf("mismatch accepted: %v", err)
			}
			if _, err := ReadPeoplePending(dir, id, pending.Key); !errors.Is(err, ErrPeopleStageMissing) {
				t.Fatalf("pending created: %v", err)
			}
			if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, stage.Data) {
				t.Fatalf("stage changed: %v", err)
			}
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
				t.Fatalf("retry: %v", err)
			}
		})
	}
	t.Run("count", func(t *testing.T) {
		dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
		if err := SavePeopleStage(dir, id, stage); err != nil {
			t.Fatal(err)
		}
		if err := SavePeoplePending(dir, id, syntheticPeoplePending(stage), nil); !errors.Is(err, ErrPeopleStage) {
			t.Fatalf("missing reviewed stage: %v", err)
		}
	})
}

func TestPeoplePendingConcurrentPinDeleteAndRetry(t *testing.T) {
	for i := 0; i < 20; i++ {
		dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
		if err := SavePeopleStage(dir, id, stage); err != nil {
			t.Fatal(err)
		}
		pending := syntheticPeoplePending(stage)
		start := make(chan struct{})
		pin := make(chan error, 1)
		remove := make(chan error, 1)
		go func() { <-start; pin <- SavePeoplePending(dir, id, pending, []PeopleStage{stage}) }()
		go func() { <-start; remove <- DeletePeopleStage(dir, id, stage.ID) }()
		close(start)
		pinErr, removeErr := <-pin, <-remove
		switch {
		case pinErr == nil && removeErr != nil:
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
				t.Fatalf("pinned retry: %v", err)
			}
			if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
				t.Fatalf("pinned stage lost: %v", err)
			}
		case pinErr != nil && removeErr == nil:
			if _, err := ReadPeoplePending(dir, id, pending.Key); !errors.Is(err, ErrPeopleStageMissing) {
				t.Fatalf("deleted stage pinned: %v", err)
			}
			replacement := stage
			replacement.Name = "replacement.txt"
			if err := SavePeopleStage(dir, id, replacement); err != nil {
				t.Fatal(err)
			}
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
				t.Fatal("stale retry pinned replacement")
			}
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{replacement}); err != nil {
				t.Fatalf("replacement retry: %v", err)
			}
		default:
			t.Fatalf("inconsistent pin=%v delete=%v", pinErr, removeErr)
		}
	}
}

func TestPeoplePendingConcurrentIdenticalRetry(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; results <- SavePeoplePending(dir, id, pending, []PeopleStage{stage}) }()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatalf("identical concurrent retry: %v", err)
		}
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("concurrent retry lost pin")
	}
}

func TestPeoplePendingRejectsRecreatedReviewedStage(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	reviewed, err := ReadPeopleStage(dir, id, stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err != nil {
		t.Fatal(err)
	}
	replacement := reviewed
	replacement.Name = "replacement.txt"
	if err := SavePeopleStage(dir, id, replacement); err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(reviewed)
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{reviewed}); !errors.Is(err, ErrPeopleStage) {
		t.Fatalf("replaced stage accepted: %v", err)
	}
	got, err := ReadPeopleStage(dir, id, stage.ID)
	if err != nil || got.Name != replacement.Name {
		t.Fatalf("replacement lost: %v", err)
	}
	if _, err := ReadPeoplePending(dir, id, pending.Key); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("unexpected pending: %v", err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{replacement}); err != nil {
		t.Fatalf("explicit retry: %v", err)
	}
}
