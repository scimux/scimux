package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/storagebudget"
)

func syntheticPeopleStage() PeopleStage {
	data := []byte("synthetic bytes")
	hash := sha256.Sum256(data)
	return PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(hash[:]), Name: "file.txt", MIME: "text/plain", ConfigHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Data: data}
}

// readPeopleRecord inspects a visible record without asserting durability.
func readPeopleRecord(dataDir string, id CopyID, stageID string, parts ...string) (PeopleStage, error) {
	if !validHex(stageID, 16) {
		return PeopleStage{}, ErrPeopleStage
	}
	path, err := copyPath(dataDir, id)
	if err != nil {
		return PeopleStage{}, err
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	root, err := peopleRecordDir(dataDir, id, false, parts...)
	if err != nil {
		return PeopleStage{}, err
	}
	defer root.Close()
	return readPeopleRecordFile(root, stageID)
}

func TestPeopleStageAtomicWriteFailuresKeepRecoverableState(t *testing.T) {
	for _, fault := range []string{"open", "write", "short write", "file sync", "file close", "link", "temp remove", "directory open", "directory sync"} {
		t.Run(fault, func(t *testing.T) {
			dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			failure := errors.New("synthetic I/O failure")
			switch fault {
			case "open":
				peopleRecordIO.openFile = func(*os.Root, string, int, os.FileMode) (*os.File, error) { return nil, failure }
			case "write":
				peopleRecordIO.write = func(f *os.File, p []byte) (int, error) {
					n, err := f.Write(p[:len(p)/2])
					if err != nil {
						return n, err
					}
					return n, failure
				}
			case "short write":
				peopleRecordIO.write = func(f *os.File, p []byte) (int, error) { return f.Write(p[:len(p)/2]) }
			case "file sync":
				peopleRecordIO.sync = func(*os.File) error { return failure }
			case "file close":
				peopleRecordIO.close = func(f *os.File) error { _ = f.Close(); return failure }
			case "link":
				peopleRecordIO.link = func(*os.Root, string, string) error { return failure }
			case "temp remove":
				peopleRecordIO.remove = func(*os.Root, string) error { return failure }
			case "directory open":
				peopleRecordIO.openDir = func(*os.Root) (*os.File, error) { return nil, failure }
			case "directory sync":
				calls := 0
				peopleRecordIO.sync = func(f *os.File) error {
					calls++
					if calls == 2 {
						return failure
					}
					return f.Sync()
				}
			}
			if err := SavePeopleStage(dir, id, stage); err == nil {
				t.Fatal("failed operation reported success")
			}
			peopleRecordIO = original
			got, err := ReadPeopleStage(dir, id, stage.ID)
			postLink := fault == "temp remove" || fault == "directory open" || fault == "directory sync"
			if postLink && (err != nil || !reflect.DeepEqual(got, stage)) {
				t.Fatalf("post-link record missing or changed: %v", err)
			}
			if !postLink && !errors.Is(err, ErrPeopleStageMissing) {
				t.Fatalf("pre-link failure published a record: %v", err)
			}
			// After link, an uncertain result can already have a complete record;
			// the caller reconciles it instead of overwriting the existing ID.
			if errors.Is(err, ErrPeopleStageMissing) {
				if err := SavePeopleStage(dir, id, stage); err != nil {
					t.Fatalf("explicit retry: %v", err)
				}
			}
			got, err = ReadPeopleStage(dir, id, stage.ID)
			if err != nil || !bytes.Equal(got.Data, stage.Data) {
				t.Fatalf("retry lost staged bytes: %v", err)
			}
			stageDir := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages")
			entries, err := os.ReadDir(stageDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if peopleStageTemp(entry.Name()) {
					t.Fatal("temporary file survived cleanup")
				}
			}
		})
	}
}

func TestPeopleStageReadFailuresDoNotDamageRetainedBytes(t *testing.T) {
	for _, fault := range []string{"stat entry", "open", "stat file", "read"} {
		t.Run(fault, func(t *testing.T) {
			dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			failure := errors.New("synthetic read failure")
			switch fault {
			case "stat entry":
				peopleRecordIO.lstat = func(*os.Root, string) (os.FileInfo, error) { return nil, failure }
			case "open":
				peopleRecordIO.openRead = func(*os.Root, string) (*os.File, error) { return nil, failure }
			case "stat file":
				peopleRecordIO.stat = func(*os.File) (os.FileInfo, error) { return nil, failure }
			case "read":
				peopleRecordIO.read = func(io.Reader) ([]byte, error) { return nil, failure }
			}
			if _, err := ReadPeopleStage(dir, id, stage.ID); !errors.Is(err, ErrPeopleStage) {
				t.Fatalf("read failure not reported: %v", err)
			}
			peopleRecordIO = original
			got, err := ReadPeopleStage(dir, id, stage.ID)
			if err != nil || !bytes.Equal(got.Data, stage.Data) {
				t.Fatalf("retry lost staged bytes: %v", err)
			}
		})
	}
}

func TestPeopleStageDirectoryOpenAndStatFailuresPreserveBytes(t *testing.T) {
	for _, fault := range []string{"open child", "stat child"} {
		t.Run(fault, func(t *testing.T) {
			dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			original := copyOps
			t.Cleanup(func() { copyOps = original })
			failure := errors.New("synthetic directory failure")
			if fault == "open child" {
				copyOps.openChild = func(*os.Root, string) (*os.Root, error) { return nil, failure }
			} else {
				copyOps.stat = func(*os.Root, string) (os.FileInfo, error) { return nil, failure }
			}
			if root, err := peopleStageDir(dir, id, false); err == nil {
				root.Close()
				t.Fatal("directory failure not reported")
			}
			copyOps = original
			got, err := ReadPeopleStage(dir, id, stage.ID)
			if err != nil || !bytes.Equal(got.Data, stage.Data) {
				t.Fatalf("read after transient failure: %v", err)
			}
		})
	}
}

func TestPeopleStageCommitRecheckFailureAllowsExactRetry(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	root, err := peopleRecordDir(dir, id, true, "people-stages")
	if err != nil {
		t.Fatal(err)
	}
	root.Close()
	encoded, err := json.Marshal(stage)
	if err != nil {
		t.Fatal(err)
	}
	original := copyOps
	t.Cleanup(func() { copyOps = original })
	visits := 0
	copyOps.openChild = func(r *os.Root, name string) (*os.Root, error) {
		if name == CopiesDir {
			visits++
			if visits == 2 {
				return nil, errors.New("synthetic copy recheck failure")
			}
		}
		return r.OpenRoot(name)
	}
	path, err := copyPath(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	unlock := lockCopy(filepath.Dir(path))
	release, err := storagebudget.Lock(dir)
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	err = savePeopleRecordLocked(dir, id, stage, encoded, "people-stages")
	release()
	unlock()
	copyOps = original
	if err == nil || visits != 2 {
		t.Fatalf("copy recheck failure ignored: visits=%d err=%v", visits, err)
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("failed admission left a stage: %v", err)
	}
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	got, err := ReadPeopleStage(dir, id, stage.ID)
	if err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("retry changed bytes: %v", err)
	}
}

func TestPeopleStageCommitTreeReadFailureAllowsExactRetry(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	peopleRecordIO.walk = func(fsys fs.FS, root string, visit fs.WalkDirFunc) error {
		if root == "people-records" {
			return visit(root, nil, errors.New("synthetic directory read failure"))
		}
		return fs.WalkDir(fsys, root, visit)
	}
	if err := SavePeopleStage(dir, id, stage); err == nil {
		t.Fatal("failed budget scan admitted stage")
	}
	peopleRecordIO = original
	if _, err := ReadPeopleStage(dir, id, stage.ID); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("failed admission left a stage: %v", err)
	}
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	got, err := ReadPeopleStage(dir, id, stage.ID)
	if err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("retry changed bytes: %v", err)
	}
}

func TestPeopleStagePersistsNativeUUIDOwnerIdentity(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	stage := syntheticPeopleStage()
	stage.OwnerInstance = "aabbccdd-1122-4abc-8abc-0123456789ab"
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPeopleStage(dir, id, stage.ID)
	if err != nil || got.OwnerInstance != stage.OwnerInstance {
		t.Fatalf("native identity lost: %v", err)
	}
}

func TestPeopleCopyInstanceSurvivesRestartAndChangesAfterRemoval(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	first, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil || !validHex(first, 32) {
		t.Fatalf("copy identity unavailable: %v", err)
	}
	if next, err := EnsurePeopleCopyInstance(dir, id); err != nil || next != first {
		t.Fatalf("copy identity was not retained: %v", err)
	}
	if err := RemoveCopy(dir, id); err != nil {
		t.Fatal(err)
	}
	if next, err := EnsurePeopleCopyInstance(dir, id); err != nil || !validHex(next, 32) || next == first {
		t.Fatalf("replacement did not get a new identity: %v", err)
	}
}

func TestPeopleCopyInstanceRequiresDirectoryDurability(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	fail := true
	peopleRecordIO.sync = func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if fail && info.IsDir() {
			return errors.New("synthetic directory sync failure")
		}
		return original.sync(f)
	}
	if value, err := EnsurePeopleCopyInstance(dir, id); err == nil || value != "" {
		t.Fatalf("undurable creation confirmed: %q %v", value, err)
	}
	unconfirmed, err := readPeopleRecord(dir, id, "0000000000000000", "people-identity")
	if err != nil || !validHex(string(unconfirmed.Data), 32) {
		t.Fatalf("unconfirmed identity record unavailable: %v", err)
	}
	if value, err := EnsurePeopleCopyInstance(dir, id); err == nil || value != "" {
		t.Fatalf("poll confirmed undurable identity: %q %v", value, err)
	}
	if value, err := ReadPeopleCopyInstance(dir, id); err == nil || value != "" {
		t.Fatalf("read confirmed undurable identity: %q %v", value, err)
	}
	fail = false
	first, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil || !validHex(first, 32) || first != string(unconfirmed.Data) {
		t.Fatalf("repair failed: %q %v", first, err)
	}
	if next, err := EnsurePeopleCopyInstance(dir, id); err != nil || next != first {
		t.Fatalf("identity changed after repair: %q %v", next, err)
	}
}

func TestPeopleCopyInstanceReconcilesTransientSyncAndConcurrentReaders(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	failures := 0
	peopleRecordIO.sync = func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.IsDir() && failures == 0 {
			failures++
			return errors.New("synthetic transient sync failure")
		}
		return original.sync(f)
	}
	first, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil || !validHex(first, 32) || failures != 1 {
		t.Fatalf("durable reconciliation failed: %q %v failures=%d", first, err, failures)
	}
	peopleRecordIO = original
	const workers = 8
	values := make(chan string, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() { value, err := EnsurePeopleCopyInstance(dir, id); values <- value; errs <- err }()
	}
	for i := 0; i < workers; i++ {
		if value, err := <-values, <-errs; err != nil || value != first {
			t.Fatalf("concurrent identity changed: %q %v", value, err)
		}
	}
	if err := RemoveCopy(dir, id); err != nil {
		t.Fatal(err)
	}
	next, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil || !validHex(next, 32) || next == first {
		t.Fatalf("recreated copy identity: %q %v", next, err)
	}
}

func TestPeopleStageReadRequiresDurableDirectoryAfterFailedAdmission(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	fail := true
	failures := 0
	peopleRecordIO.sync = func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if fail && info.IsDir() {
			failures++
			return errors.New("synthetic directory sync failure")
		}
		return original.sync(f)
	}
	if err := SavePeopleStage(dir, id, stage); err == nil {
		t.Fatal("unsynced admission succeeded")
	}
	visible, err := readPeopleRecord(dir, id, stage.ID, "people-stages")
	if err != nil || !bytes.Equal(visible.Data, stage.Data) {
		t.Fatalf("expected complete visible record: %v", err)
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("visible stage was accepted without durable directory sync")
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("retry accepted stage while sync still fails")
	}
	if failures < 3 {
		t.Fatalf("directory sync faults not exercised: %d", failures)
	}
	fail = false
	got, err := ReadPeopleStage(dir, id, stage.ID)
	if err != nil || got.ID != stage.ID || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("explicit durability retry lost identity or bytes: %v", err)
	}
}

func TestPeopleStageReadRepairsTransientDirectorySyncFailure(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	failures := 0
	peopleRecordIO.sync = func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.IsDir() && failures == 0 {
			failures++
			return errors.New("synthetic transient directory sync failure")
		}
		return original.sync(f)
	}
	if err := SavePeopleStage(dir, id, stage); err == nil {
		t.Fatal("first unsynced admission succeeded")
	}
	got, err := ReadPeopleStage(dir, id, stage.ID)
	if err != nil || failures != 1 || got.ID != stage.ID || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("durability retry changed stage: %v", err)
	}
	const readers = 8
	results := make(chan PeopleStage, readers)
	errs := make(chan error, readers)
	for i := 0; i < readers; i++ {
		go func() { result, err := ReadPeopleStage(dir, id, stage.ID); results <- result; errs <- err }()
	}
	for i := 0; i < readers; i++ {
		result, err := <-results, <-errs
		if err != nil || result.ID != stage.ID || !bytes.Equal(result.Data, stage.Data) {
			t.Fatalf("concurrent durable read changed stage: %v", err)
		}
	}
}

func TestPeopleStageReadFailsWhenDurabilityDirectoryUnavailable(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	peopleRecordIO.openDir = func(*os.Root) (*os.File, error) { return nil, errors.New("synthetic directory open failure") }
	if _, err := ReadPeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("stage read accepted without durability directory")
	}
	peopleRecordIO = original
	got, err := ReadPeopleStage(dir, id, stage.ID)
	if err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("stage did not survive directory recovery: %v", err)
	}
}

func TestPeopleStageReadRejectsInvalidIdentityWithoutStorage(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	if _, err := ReadPeopleStage(dir, id, "invalid"); err == nil {
		t.Fatal("invalid stage identity accepted")
	}
	id.Generation = "invalid"
	if _, err := ReadPeopleStage(dir, id, "0123456789abcdef"); err == nil {
		t.Fatal("invalid copy identity accepted")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid read touched storage: %v", err)
	}
}

func TestPeopleStageConcurrentProcessWinnersRemainDurable(t *testing.T) {
	if dir := os.Getenv("SCIMUX_TEST_STAGE_RACE_DIR"); dir != "" {
		stage := syntheticPeopleStage()
		stage.Data = []byte(os.Getenv("SCIMUX_TEST_STAGE_RACE_DATA"))
		sum := sha256.Sum256(stage.Data)
		stage.Hash = hex.EncodeToString(sum[:])
		if err := os.WriteFile(os.Getenv("SCIMUX_TEST_STAGE_RACE_MARKER"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		err := SavePeopleStage(dir, sampleCopyID(), stage)
		result := "created"
		if err != nil {
			if !errors.Is(err, ErrPeopleStage) {
				t.Fatal(err)
			}
			result = "conflict"
		}
		if err := os.WriteFile(os.Getenv("SCIMUX_TEST_STAGE_RACE_RESULT"), []byte(result), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, mode := range []string{"same bytes", "different bytes"} {
		t.Run(mode, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
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
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			var workers sync.WaitGroup
			defer func() { cancel(); workers.Wait() }()
			markers := []string{filepath.Join(dir, "marker-one"), filepath.Join(dir, "marker-two")}
			results := []string{filepath.Join(dir, "result-one"), filepath.Join(dir, "result-two")}
			values := []string{"synthetic first", "synthetic first"}
			if mode == "different bytes" {
				values[1] = "synthetic second"
			}
			done := make([]chan error, 2)
			for i := range markers {
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPeopleStageConcurrentProcessWinnersRemainDurable$", "-test.count=1")
				cmd.Env = append(os.Environ(), "SCIMUX_TEST_STAGE_RACE_DIR="+dir, "SCIMUX_TEST_STAGE_RACE_DATA="+values[i], "SCIMUX_TEST_STAGE_RACE_MARKER="+markers[i], "SCIMUX_TEST_STAGE_RACE_RESULT="+results[i])
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done[i] = make(chan error, 1)
				workers.Add(1)
				go func() { defer workers.Done(); done[i] <- cmd.Wait() }()
			}
			for _, marker := range markers {
				for {
					if _, err := os.Stat(marker); err == nil {
						break
					} else if !os.IsNotExist(err) {
						t.Fatal(err)
					}
					select {
					case <-ctx.Done():
						t.Fatal("stage contenders did not start")
					case <-time.After(5 * time.Millisecond):
					}
				}
			}
			for _, ch := range done {
				select {
				case err := <-ch:
					t.Fatalf("contender finished while budget locked: %v", err)
				default:
				}
			}
			release()
			locked = false
			for _, ch := range done {
				if err := <-ch; err != nil {
					t.Fatalf("stage contender failed: %v", err)
				}
			}
			winner := -1
			for i, path := range results {
				result, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				switch string(result) {
				case "created":
					if winner != -1 {
						t.Fatal("multiple stage winners")
					}
					winner = i
				case "conflict":
				default:
					t.Fatalf("unexpected result %q", result)
				}
			}
			if winner < 0 {
				t.Fatal("no durable stage winner")
			}
			got, err := ReadPeopleStage(dir, id, "0123456789abcdef")
			if err != nil || string(got.Data) != values[winner] {
				t.Fatalf("winner was not retained exactly: %v", err)
			}
		})
	}
}

func TestPeopleCopyInstanceReadFailsWhenDurabilityDirectoryUnavailable(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	first, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	peopleRecordIO.openDir = func(*os.Root) (*os.File, error) { return nil, errors.New("synthetic directory open failure") }
	if value, err := ReadPeopleCopyInstance(dir, id); err == nil || value != "" {
		t.Fatalf("unverified identity returned: %q %v", value, err)
	}
	peopleRecordIO = original
	if value, err := ReadPeopleCopyInstance(dir, id); err != nil || value != first {
		t.Fatalf("identity changed after recovery: %q %v", value, err)
	}
}

func TestPeopleCopyInstanceRejectsInvalidCopyWithoutCreatingData(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	id.Generation = "invalid"
	if value, err := ReadPeopleCopyInstance(dir, id); err == nil || value != "" {
		t.Fatalf("invalid copy read: %q %v", value, err)
	}
	if value, err := EnsurePeopleCopyInstance(dir, id); err == nil || value != "" {
		t.Fatalf("invalid copy created identity: %q %v", value, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid copy touched storage: %v", err)
	}
}

func TestPeopleCopyInstanceReconcilesCommittedWinnerAfterMissingRead(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	winner, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	missed := false
	peopleRecordIO.lstat = func(r *os.Root, name string) (os.FileInfo, error) {
		if name == "0000000000000000.json" && !missed {
			missed = true
			return nil, os.ErrNotExist
		}
		return r.Lstat(name)
	}
	got, err := EnsurePeopleCopyInstance(dir, id)
	peopleRecordIO = original
	if err != nil || got != winner || !missed {
		t.Fatalf("committed winner not reconciled: %q %v", got, err)
	}
	if got, err := ReadPeopleCopyInstance(dir, id); err != nil || got != winner {
		t.Fatalf("winner changed on reload: %q %v", got, err)
	}
}

func TestPeopleCopyInstanceConcurrentProcessWinner(t *testing.T) {
	if dir := os.Getenv("SCIMUX_TEST_IDENTITY_RACE_DIR"); dir != "" {
		marker, result := os.Getenv("SCIMUX_TEST_IDENTITY_RACE_MARKER"), os.Getenv("SCIMUX_TEST_IDENTITY_RACE_RESULT")
		original := peopleRecordIO
		peopleRecordIO.lstat = func(r *os.Root, name string) (os.FileInfo, error) {
			info, err := r.Lstat(name)
			if name == "0000000000000000.json" && errors.Is(err, os.ErrNotExist) {
				if writeErr := os.WriteFile(marker, []byte("observed missing"), 0600); writeErr != nil {
					return nil, writeErr
				}
			}
			return info, err
		}
		value, err := EnsurePeopleCopyInstance(dir, sampleCopyID())
		peopleRecordIO = original
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(result, []byte(value), 0600); err != nil {
			t.Fatal(err)
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	markers := []string{filepath.Join(dir, "marker-one"), filepath.Join(dir, "marker-two")}
	results := []string{filepath.Join(dir, "result-one"), filepath.Join(dir, "result-two")}
	done := make([]chan error, 2)
	for i := range markers {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPeopleCopyInstanceConcurrentProcessWinner$", "-test.count=1")
		cmd.Env = append(os.Environ(), "SCIMUX_TEST_IDENTITY_RACE_DIR="+dir, "SCIMUX_TEST_IDENTITY_RACE_MARKER="+markers[i], "SCIMUX_TEST_IDENTITY_RACE_RESULT="+results[i])
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done[i] = make(chan error, 1)
		workers.Add(1)
		go func() { defer workers.Done(); done[i] <- cmd.Wait() }()
	}
	for _, marker := range markers {
		for {
			if _, err := os.Stat(marker); err == nil {
				break
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
				t.Fatal("contenders did not reach missing identity")
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	for _, ch := range done {
		select {
		case err := <-ch:
			t.Fatalf("contender finished before budget release: %v", err)
		default:
		}
	}
	release()
	locked = false
	for _, ch := range done {
		if err := <-ch; err != nil {
			t.Fatalf("identity contender failed: %v", err)
		}
	}
	first, err := os.ReadFile(results[0])
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(results[1])
	if err != nil {
		t.Fatal(err)
	}
	if !validHex(string(first), 32) || !bytes.Equal(first, second) {
		t.Fatal("concurrent contenders did not reconcile one durable identity")
	}
	value, err := ReadPeopleCopyInstance(dir, id)
	if err != nil || value != string(first) {
		t.Fatalf("durable winner changed: %v", err)
	}
}

func TestPeopleCopyInstanceDamageIsNotReplaced(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	if _, err := ReadPeopleCopyInstance(dir, id); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("missing identity did not remain unavailable: %v", err)
	}
	if _, err := EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-identity", "0000000000000000.json")
	if err := os.WriteFile(path, []byte("synthetic damage"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsurePeopleCopyInstance(dir, id); err == nil {
		t.Fatal("damaged identity was silently replaced")
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "synthetic damage" {
		t.Fatal("damaged identity was not preserved for inspection")
	}
}

func TestPeopleCopyInstanceRejectsValidEnvelopeWithInvalidIdentity(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	data := []byte("not-an-identity")
	sum := sha256.Sum256(data)
	record := PeopleStage{ID: "0000000000000000", Hash: hex.EncodeToString(sum[:]), Name: "identity", MIME: "text/plain", ConfigHash: strings.Repeat("0", 64), Data: data}
	if err := savePeopleRecord(dir, id, record, "people-identity"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleCopyInstance(dir, id); !errors.Is(err, ErrPeopleStage) {
		t.Fatalf("invalid identity accepted: %v", err)
	}
	if _, err := EnsurePeopleCopyInstance(dir, id); !errors.Is(err, ErrPeopleStage) {
		t.Fatalf("invalid identity replaced: %v", err)
	}
	got, err := readPeopleRecord(dir, id, record.ID, "people-identity")
	if err != nil || !bytes.Equal(got.Data, data) {
		t.Fatalf("damaged identity not preserved: %v", err)
	}
}

func TestPeopleCopyInstanceRejectsMismatchedEnvelopeMetadata(t *testing.T) {
	for _, field := range []string{"name", "mime", "configuration", "owner"} {
		t.Run(field, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			data := []byte(strings.Repeat("1", 32))
			sum := sha256.Sum256(data)
			record := PeopleStage{ID: "0000000000000000", Hash: hex.EncodeToString(sum[:]), Name: "identity", MIME: "text/plain", ConfigHash: strings.Repeat("0", 64), Data: data}
			switch field {
			case "name":
				record.Name = "other"
			case "mime":
				record.MIME = "application/octet-stream"
			case "configuration":
				record.ConfigHash = strings.Repeat("a", 64)
			case "owner":
				record.OwnerInstance = "synthetic-instance"
			}
			if err := savePeopleRecord(dir, id, record, "people-identity"); err != nil {
				t.Fatal(err)
			}
			if value, err := EnsurePeopleCopyInstance(dir, id); value != "" || !errors.Is(err, ErrPeopleStage) {
				t.Fatalf("mismatched identity was accepted: %q %v", value, err)
			}
			got, err := readPeopleRecord(dir, id, record.ID, "people-identity")
			if err != nil || got.Name != record.Name || got.MIME != record.MIME || got.ConfigHash != record.ConfigHash || got.OwnerInstance != record.OwnerInstance {
				t.Fatalf("refusal altered identity metadata: %v", err)
			}
		})
	}
}

func TestPeopleStageRejectsUnsafeRecordTreeAndMissingRoot(t *testing.T) {
	id := sampleCopyID()
	stage := syntheticPeopleStage()
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := peopleRecordDir(missing, id, false); !errors.Is(err, ErrPeopleStage) {
		t.Fatalf("missing root accepted: %v", err)
	}
	bad := id
	bad.Server = "invalid"
	if _, err := peopleRecordDir(t.TempDir(), bad, true); err == nil {
		t.Fatal("invalid copy accepted")
	}
	dir := t.TempDir()
	stages := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages")
	if err := os.MkdirAll(stages, 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	link := filepath.Join(stages, "0123456789abcdef-aaaaaaaaaaaaaaaa.tmp")
	if err := os.Symlink(filepath.Join(outside, "target"), link); err != nil {
		t.Fatal(err)
	}
	if err := SavePeopleStage(dir, id, stage); err == nil {
		t.Fatal("symlink in managed tree accepted")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("unrelated symlink removed: %v", err)
	}
}

func TestPeopleRecordDirectoryCannotEscapeItsCopyRoot(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	if root, err := peopleRecordDir(dir, id, true, "../escaped"); err == nil {
		root.Close()
		t.Fatal("record directory escaped its copy root")
	}
	escaped := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, "escaped")
	if _, err := os.Lstat(escaped); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escape created a sibling: %v", err)
	}
}

func TestPeopleCopyInstanceBudgetFailureDoesNotInventIdentity(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":1,"storage_min_free_bytes":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	value, err := EnsurePeopleCopyInstance(dir, id)
	if value != "" || !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("budget failure yielded identity %q: %v", value, err)
	}
	if value, err := ReadPeopleCopyInstance(dir, id); value != "" || !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("refused identity persisted: %q %v", value, err)
	}
}

func TestPeopleStageRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{"../secret", "a/b", "a\\b", "bad\nname", "."} {
		t.Run(name, func(t *testing.T) {
			s := syntheticPeopleStage()
			s.Name = name
			if err := SavePeopleStage(t.TempDir(), sampleCopyID(), s); err == nil {
				t.Fatal("unsafe name was accepted")
			}
		})
	}
}

func TestPeopleStageValidationRejectsUnverifiableMetadata(t *testing.T) {
	valid := syntheticPeopleStage()
	for _, tc := range []struct {
		name   string
		change func(*PeopleStage)
	}{
		{"bad logical id", func(s *PeopleStage) { s.ID = "not-an-id" }},
		{"bad hash", func(s *PeopleStage) { s.Hash = strings.Repeat("z", 64) }},
		{"hash mismatch", func(s *PeopleStage) { s.Data = []byte("different bytes") }},
		{"bad server binding", func(s *PeopleStage) { s.ConfigHash = "invalid" }},
		{"empty file", func(s *PeopleStage) { s.Data = nil }},
		{"oversize file", func(s *PeopleStage) { s.Data = make([]byte, 4<<20+1) }},
		{"empty filename", func(s *PeopleStage) { s.Name = "" }},
		{"oversize filename", func(s *PeopleStage) { s.Name = strings.Repeat("n", 256) }},
		{"invalid utf8 filename", func(s *PeopleStage) { s.Name = string([]byte{0xff}) }},
		{"invalid mime", func(s *PeopleStage) { s.MIME = "not a MIME type" }},
		{"oversize mime", func(s *PeopleStage) { s.MIME = strings.Repeat("m", 256) }},
		{"control in instance", func(s *PeopleStage) { s.OwnerInstance = "old\nnew" }},
		{"invalid utf8 instance", func(s *PeopleStage) { s.OwnerInstance = string([]byte{0xff}) }},
		{"oversize instance", func(s *PeopleStage) { s.OwnerInstance = strings.Repeat("i", 257) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage := valid
			tc.change(&stage)
			if err := SavePeopleStage(t.TempDir(), sampleCopyID(), stage); err == nil {
				t.Fatal("unsafe or unverifiable staged attachment was accepted")
			}
		})
	}
	if ValidPeopleOwnerInstance("") {
		t.Fatal("empty native owner identity accepted")
	}
	valid.Name = strings.Repeat("n", 255)
	valid.OwnerInstance = strings.Repeat("i", 256)
	if err := SavePeopleStage(t.TempDir(), sampleCopyID(), valid); err != nil {
		t.Fatalf("valid boundary metadata rejected: %v", err)
	}
}

func TestPeopleStageScopedDurabilityAndCorruption(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	s := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, s); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPeopleStage(dir, id, s.ID)
	if err != nil || string(got.Data) != string(s.Data) {
		t.Fatalf("stage unavailable: %v", err)
	}
	other := id
	other.Generation = "dddddddddddddddd"
	if _, err := ReadPeopleStage(dir, other, s.ID); err == nil {
		t.Fatal("foreign generation found stage")
	}
	if err := SavePeopleStage(dir, id, s); err == nil {
		t.Fatal("stage ID overwritten")
	}
	path := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages", s.ID+".json")
	if err := os.WriteFile(path, []byte(`{"id":"broken"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleStage(dir, id, s.ID); err == nil {
		t.Fatal("corrupt stage returned bytes")
	}
}

func TestPeopleStageRejectsPublicFilePermissions(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	s := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, s); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages", s.ID+".json")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleStage(dir, id, s.ID); err == nil {
		t.Fatal("public stage file accepted")
	}
}

func TestPeopleStageRejectsSymlinkedCopyRoot(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	id := sampleCopyID()
	s := syntheticPeopleStage()
	if err := os.Symlink(outside, filepath.Join(dir, CopiesDir)); err != nil {
		t.Fatal(err)
	}
	if err := SavePeopleStage(dir, id, s); err == nil {
		t.Fatal("symlinked copy root was accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside directory modified: %v", err)
	}
}

func TestPeopleStageRejectsSymlinkedStageDirectory(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	id := sampleCopyID()
	if _, err := EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	stages := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages")
	if err := os.Symlink(outside, stages); err != nil {
		t.Fatal(err)
	}
	if err := SavePeopleStage(dir, id, syntheticPeopleStage()); err == nil {
		t.Fatal("symlinked stage directory was accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside directory changed: %v", err)
	}
	if _, err := ReadPeopleCopyInstance(dir, id); err != nil {
		t.Fatalf("refusal lost existing copy identity: %v", err)
	}
}

func TestPeopleStageCommitRechecksManagedRootAndRemovesOrphans(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		t.Run(map[bool]string{false: "orphan", true: "symlink root"}[unsafe], func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			stage := syntheticPeopleStage()
			encoded, err := json.Marshal(stage)
			if err != nil {
				t.Fatal(err)
			}
			root, err := peopleRecordDir(dir, id, true)
			if err != nil {
				t.Fatal(err)
			}
			root.Close()
			base := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages")
			outside := t.TempDir()
			if unsafe {
				if err := os.Symlink(outside, base); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(base, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(base, "fedcba9876543210-aaaaaaaaaaaaaaaa.tmp"), []byte("partial"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			path, err := copyPath(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			unlock := lockCopy(filepath.Dir(path))
			release, err := storagebudget.Lock(dir)
			if err != nil {
				unlock()
				t.Fatal(err)
			}
			err = savePeopleRecordLocked(dir, id, stage, encoded, "people-stages")
			release()
			unlock()
			if unsafe {
				if err == nil {
					t.Fatal("commit followed unsafe stage root")
				}
				entries, readErr := os.ReadDir(outside)
				if readErr != nil || len(entries) != 0 {
					t.Fatalf("unsafe target modified: %v", readErr)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(filepath.Join(base, "fedcba9876543210-aaaaaaaaaaaaaaaa.tmp")); !os.IsNotExist(err) {
					t.Fatalf("orphan remained: %v", err)
				}
				if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, stage.Data) {
					t.Fatalf("commit lost bytes: %v", err)
				}
			}
		})
	}
}

func TestPeopleStageCommitRefusesUnremovableOrphan(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	encoded, err := json.Marshal(stage)
	if err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, true, "people-records")
	if err != nil {
		t.Fatal(err)
	}
	root.Close()
	records := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-records")
	orphan := filepath.Join(records, "fedcba9876543210-aaaaaaaaaaaaaaaa.tmp")
	if err := os.WriteFile(orphan, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(records, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(records, 0700) })
	path, err := copyPath(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	commit := func() error {
		unlock := lockCopy(filepath.Dir(path))
		defer unlock()
		release, err := storagebudget.Lock(dir)
		if err != nil {
			return err
		}
		defer release()
		return savePeopleRecordLocked(dir, id, stage, encoded, "people-stages")
	}
	if err := commit(); err == nil {
		t.Fatal("unremovable orphan allowed stage commit")
	}
	if err := os.Chmod(records, 0700); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(orphan); err != nil || string(data) != "partial" {
		t.Fatalf("failed cleanup changed orphan: %v", err)
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("failed commit published a stage: %v", err)
	}
	if err := commit(); err != nil {
		t.Fatalf("explicit retry after repair failed: %v", err)
	}
	if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("retry lost stage bytes: %v", err)
	}
}

func TestPeopleStageRemovalIsScopedAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	other := id
	other.Generation = "dddddddddddddddd"
	s := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, s); err != nil {
		t.Fatal(err)
	}
	if err := SavePeopleStage(dir, other, s); err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStage(dir, id, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleStage(dir, id, s.ID); err == nil {
		t.Fatal("deleted stage remains readable")
	}
	if _, err := ReadPeopleStage(dir, other, s.ID); err != nil {
		t.Fatal("removal affected another generation")
	}
	if err := DeletePeopleStage(dir, id, s.ID); err != nil {
		t.Fatalf("repeat removal: %v", err)
	}
}

func TestPeopleRecordsRejectInvalidAddressesWithoutChangingStoredData(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	invalidCopy := id
	invalidCopy.Generation = "invalid"
	if err := DeletePeopleStage(dir, id, "invalid"); err == nil {
		t.Fatal("invalid stage identity was accepted")
	}
	if err := DeletePeopleStage(dir, invalidCopy, stage.ID); err == nil {
		t.Fatal("invalid copy identity was accepted")
	}
	if err := SavePeoplePublished(dir, id, 0, stage); err == nil {
		t.Fatal("zero sequence was accepted")
	}
	if err := SavePeoplePublished(dir, invalidCopy, 1, stage); err == nil {
		t.Fatal("invalid copy accepted published bytes")
	}
	invalidStage := stage
	invalidStage.ID = "invalid"
	if err := SavePeoplePublished(dir, id, 1, invalidStage); err == nil {
		t.Fatal("invalid published identity was accepted")
	}
	if _, err := ReadPeoplePublished(dir, id, 0, stage.ID); err == nil {
		t.Fatal("zero sequence was readable")
	}
	if _, err := ReadPeoplePublished(dir, id, 1, "../invalid"); err == nil {
		t.Fatal("invalid attachment identity was readable")
	}
	if _, err := ReadPeoplePublished(dir, invalidCopy, 1, stage.ID); err == nil {
		t.Fatal("invalid copy identity was readable")
	}
	stored, err := ReadPeopleStage(dir, id, stage.ID)
	if err != nil || !bytes.Equal(stored.Data, stage.Data) {
		t.Fatalf("invalid address changed staged bytes: %v", err)
	}
	stored, err = ReadPeoplePublished(dir, id, 1, stage.ID)
	if err != nil || !bytes.Equal(stored.Data, stage.Data) {
		t.Fatalf("invalid address changed retained bytes: %v", err)
	}
}

func TestPeopleStageRemovedOnlyWithItsCopy(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	other := id
	other.Generation = "dddddddddddddddd"
	s := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, s); err != nil {
		t.Fatal(err)
	}
	if err := SavePeopleStage(dir, other, s); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCopy(dir, id); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleStage(dir, id, s.ID); err == nil {
		t.Fatal("removed copy retained its stage")
	}
	if _, err := ReadPeopleStage(dir, other, s.ID); err != nil {
		t.Fatal("other copy was removed")
	}
}

func TestPeopleStageNotPublishedBeforeDurableCommit(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	s := syntheticPeopleStage()
	ready := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	old := peopleStageAfterWrite
	peopleStageAfterWrite = func() { close(ready); <-release }
	t.Cleanup(func() { peopleStageAfterWrite = old })
	go func() { done <- SavePeopleStage(dir, id, s) }()
	<-ready
	path := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages", s.ID+".json")
	_, err := os.Lstat(path)
	close(release)
	saveErr := <-done
	if !os.IsNotExist(err) {
		t.Errorf("stage visible before durable commit: %v", err)
	}
	if saveErr != nil {
		t.Fatal(saveErr)
	}
}

func TestPeopleStageInterruptedTemporaryCannotPublishPartialRecord(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	stageDir := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages")
	previous := peopleStageAfterWrite
	peopleStageAfterWrite = func() {
		entries, err := os.ReadDir(stageDir)
		if err != nil {
			t.Error(err)
			return
		}
		for _, entry := range entries {
			if peopleStageTemp(entry.Name()) {
				if err := os.Remove(filepath.Join(stageDir, entry.Name())); err != nil {
					t.Error(err)
				}
			}
		}
	}
	t.Cleanup(func() { peopleStageAfterWrite = previous })
	if err := SavePeopleStage(dir, id, stage); err == nil {
		t.Fatal("interrupted temporary was published")
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("interrupted record became available: %v", err)
	}
	peopleStageAfterWrite = previous
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatalf("explicit retry after interruption failed: %v", err)
	}
	got, err := ReadPeopleStage(dir, id, stage.ID)
	if err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("retry did not retain exact bytes: %v", err)
	}
}

func TestPeoplePublishedSameLogicalIDIsScopedBySequence(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	first := syntheticPeopleStage()
	second := syntheticPeopleStage()
	second.Name = "second.txt"
	second.Data = []byte("other synthetic bytes")
	sum := sha256.Sum256(second.Data)
	second.Hash = hex.EncodeToString(sum[:])
	if err := SavePeoplePublished(dir, id, 1, first); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePublished(dir, id, 2, second); err != nil {
		t.Fatal(err)
	}
	one, err := ReadPeoplePublished(dir, id, 1, first.ID)
	if err != nil || string(one.Data) != string(first.Data) || one.Name != first.Name {
		t.Fatalf("first changed: %v", err)
	}
	two, err := ReadPeoplePublished(dir, id, 2, second.ID)
	if err != nil || string(two.Data) != string(second.Data) || two.Name != second.Name {
		t.Fatalf("second wrong: %v", err)
	}
	other := id
	other.Generation = "dddddddddddddddd"
	if _, err := ReadPeoplePublished(dir, other, 1, first.ID); err == nil {
		t.Fatal("foreign generation read retained bytes")
	}
}

func TestConcurrentPeoplePublishedRetriesKeepOneExactAssociation(t *testing.T) {
	for round := 0; round < 12; round++ {
		dir, id := t.TempDir(), sampleCopyID()
		stage := syntheticPeopleStage()
		start := make(chan struct{})
		errs := make(chan error, 8)
		var ready sync.WaitGroup
		ready.Add(8)
		for i := 0; i < 8; i++ {
			go func() { ready.Done(); <-start; errs <- SavePeoplePublished(dir, id, 1, stage) }()
		}
		ready.Wait()
		close(start)
		for i := 0; i < 8; i++ {
			if err := <-errs; err != nil {
				t.Fatalf("identical retry failed: %v", err)
			}
		}
		got, err := ReadPeoplePublished(dir, id, 1, stage.ID)
		if err != nil || !bytes.Equal(got.Data, stage.Data) || got.Hash != stage.Hash {
			t.Fatalf("concurrent publication changed retained association: %v", err)
		}
	}
}

func TestPeopleCopyQuotaCountsAllSequencesAndStages(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	base := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-records", "comments", "1")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4096; i++ {
		if err := os.WriteFile(filepath.Join(base, fmt.Sprintf("%016x.json", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := SavePeopleStage(dir, id, syntheticPeopleStage()); !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("copy quota did not include published sequences: %v", err)
	}
}

func TestPeopleStageRecoversAbandonedTemp(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	s := syntheticPeopleStage()
	stages := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages")
	if err := os.MkdirAll(stages, 0700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(stages, "0123456789abcdef-aaaaaaaaaaaaaaaa.tmp")
	if err := os.WriteFile(orphan, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(stages, "unrelated.tmp")
	if err := os.WriteFile(unrelated, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SavePeopleStage(dir, id, s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("abandoned temporary file remains: %v", err)
	}
	if _, err := os.Lstat(unrelated); err != nil {
		t.Fatalf("unrelated file removed: %v", err)
	}
	if _, err := ReadPeopleStage(dir, id, s.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPeopleStageOrphanCleanupFailureKeepsExistingBytes(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	stages := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages")
	orphan := filepath.Join(stages, "fedcba9876543210-aaaaaaaaaaaaaaaa.tmp")
	if err := os.WriteFile(orphan, []byte("synthetic partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stages, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stages, 0700) })
	other := stage
	other.ID = "fedcba9876543210"
	if err := SavePeopleStage(dir, id, other); err == nil {
		t.Fatal("orphan cleanup failure admitted another stage")
	}
	if err := os.Chmod(stages, 0700); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("cleanup failure lost prior data: %v", err)
	}
	if err := SavePeopleStage(dir, id, other); err != nil {
		t.Fatalf("explicit retry after repair: %v", err)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("abandoned temporary not recovered: %v", err)
	}
}

func TestPeopleStageRecoveryRejectsUnreadableManagedTree(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-records")
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
	other := stage
	other.ID = "fedcba9876543210"
	if err := SavePeopleStage(dir, id, other); err == nil {
		t.Fatal("unreadable managed tree allowed a new stage")
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(path, "synthetic")); err != nil || string(data) != "keep" {
		t.Fatalf("failed recovery changed retained data: %v", err)
	}
	if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("failed recovery lost prior stage: %v", err)
	}
	if err := SavePeopleStage(dir, id, other); err != nil {
		t.Fatalf("explicit retry after repair failed: %v", err)
	}
}

func TestPeopleStageRemovalKeepsBytesWhenBudgetLockUnavailable(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, ".storage.lock")
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("stage removed without the shared storage lock")
	}
	got, err := ReadPeopleStage(dir, id, stage.ID)
	if err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("lock failure lost staged bytes: %v", err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("explicit removal after lock repair failed: %v", err)
	}
}

func TestPeopleCopyRemovalKeepsBytesWhenBudgetLockUnavailable(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, ".storage.lock")
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCopy(dir, id); err == nil {
		t.Fatal("copy removed without the shared storage lock")
	}
	got, err := ReadPeopleStage(dir, id, stage.ID)
	if err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("copy data lost after lock failure: %v", err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCopy(dir, id); err != nil {
		t.Fatalf("explicit removal after repair failed: %v", err)
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("copy survived explicit removal: %v", err)
	}
}

func TestPeopleStageRemovalReportsMissingStageDirectory(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	root, err := peopleRecordDir(dir, id, true)
	if err != nil {
		t.Fatal(err)
	}
	root.Close()
	if err := DeletePeopleStage(dir, id, "0123456789abcdef"); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("missing stage directory: %v", err)
	}
}

func TestPeopleStageRemovalDoesNotDeleteNonemptyRecordDirectory(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages", stage.ID+".json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "synthetic"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("unsafe nonempty directory was removed as a stage")
	}
	if data, err := os.ReadFile(filepath.Join(path, "synthetic")); err != nil || string(data) != "keep" {
		t.Fatalf("refusal changed directory contents: %v", err)
	}
}

func TestPeopleCopyRemovalJoinsBudgetLock(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	if err := SavePeopleStage(dir, id, syntheticPeopleStage()); err != nil {
		t.Fatal(err)
	}
	release, err := storagebudget.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- RemoveCopy(dir, id) }()
	select {
	case err := <-done:
		release()
		t.Fatalf("copy removal bypassed budget lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("copy removal did not resume")
	}
}

func TestPeoplePublishedRetryIsIdempotentAndNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	s := syntheticPeopleStage()
	if err := SavePeoplePublished(dir, id, 1, s); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePublished(dir, id, 1, s); err != nil {
		t.Fatalf("same publication retry: %v", err)
	}
	changed := s
	changed.Name = "other.txt"
	if err := SavePeoplePublished(dir, id, 1, changed); err == nil {
		t.Fatal("different descriptor overwrote retained association")
	}
	got, err := ReadPeoplePublished(dir, id, 1, s.ID)
	if err != nil || got.Name != s.Name {
		t.Fatalf("retained association changed: %v", err)
	}
}

func TestPeoplePublishedRetainsBytesAcrossConfigurationChange(t *testing.T) {
	dir := t.TempDir()
	id := sampleCopyID()
	s := syntheticPeopleStage()
	if err := SavePeoplePublished(dir, id, 1, s); err != nil {
		t.Fatal(err)
	}
	changed := s
	changed.ConfigHash = strings.Repeat("b", 64)
	if err := SavePeoplePublished(dir, id, 1, changed); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPeoplePublished(dir, id, 1, s.ID)
	if err != nil || string(got.Data) != string(s.Data) || got.ConfigHash != s.ConfigHash {
		t.Fatalf("retained data lost or rewritten: %v", err)
	}
}

func TestPeoplePublishedMatchingRetryAfterTransientDurabilityFailure(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	data := []byte("synthetic retained attachment")
	digest := sha256.Sum256(data)
	originalStage := PeopleStage{ID: "1234567890abcdef", Hash: hex.EncodeToString(digest[:]), Name: "example.txt", MIME: "text/plain", ConfigHash: strings.Repeat("c", 64), OwnerInstance: "synthetic-owner", Data: data}
	history := MirrorRecord{Seq: 1, Key: "synthetic-entry", Author: strings.Repeat("1", 32), At: "2000-01-01T00:00:00Z", Body: json.RawMessage(`{"v":1,"text":"Synthetic history"}`)}
	if err := AppendCopy(dir, id, MirrorLog, []MirrorRecord{history}); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePublished(dir, id, 1, originalStage); err != nil {
		t.Fatal(err)
	}
	historyPath := copyPathOf(dir, id)
	beforeHistory, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	recordDir := filepath.Join(filepath.Dir(historyPath), "people-records", "comments", "1")
	recordPath := filepath.Join(recordDir, originalStage.ID+".json")
	beforeRecord, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	retry := originalStage
	retry.ConfigHash = strings.Repeat("d", 64)
	retry.OwnerInstance = "synthetic-retry"
	originalIO := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = originalIO })
	failedSync, existingLink := false, false
	peopleRecordIO.sync = func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.IsDir() && !failedSync {
			failedSync = true
			return errors.New("synthetic one-time directory sync failure")
		}
		return originalIO.sync(f)
	}
	peopleRecordIO.link = func(root *os.Root, old, name string) error {
		err := originalIO.link(root, old, name)
		if errors.Is(err, fs.ErrExist) {
			existingLink = true
		}
		return err
	}
	if err := SavePeoplePublished(dir, id, 1, retry); err != nil {
		t.Fatalf("matching retry did not recover: %v", err)
	}
	peopleRecordIO = originalIO
	if !failedSync || !existingLink {
		t.Fatalf("expected transient failure and existing association: sync=%t link=%t", failedSync, existingLink)
	}
	got, err := ReadPeoplePublished(dir, id, 1, originalStage.ID)
	if err != nil || !reflect.DeepEqual(got, originalStage) {
		t.Fatalf("original association changed: %+v %v", got, err)
	}
	afterInfo, err := os.Stat(recordPath)
	if err != nil || !os.SameFile(beforeInfo, afterInfo) {
		t.Fatalf("published record identity changed: %v", err)
	}
	afterRecord, err := os.ReadFile(recordPath)
	if err != nil || !bytes.Equal(afterRecord, beforeRecord) {
		t.Fatalf("published record bytes changed: %v", err)
	}
	entries, err := os.ReadDir(recordDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != originalStage.ID+".json" {
		t.Fatalf("duplicate or missing association: %v %v", entries, err)
	}
	afterHistory, err := os.ReadFile(historyPath)
	if err != nil || !bytes.Equal(beforeHistory, afterHistory) {
		t.Fatalf("history changed: %v", err)
	}
}
