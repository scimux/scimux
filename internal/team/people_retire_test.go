package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/scimux/scimux/internal/storagebudget"
)

func discardTestPending(t *testing.T, stage PeopleStage) PeoplePending {
	t.Helper()
	pending := syntheticPeoplePending(stage)
	body, err := json.Marshal(map[string]any{"v": 1, "people_attachments_version": 1, "people_attachments": []any{map[string]any{"id": stage.ID, "sha256": stage.Hash, "size": len(stage.Data), "name": stage.Name, "mime": stage.MIME}}})
	if err != nil {
		t.Fatal(err)
	}
	pending.Body = body
	return pending
}

func setupDiscard(t *testing.T) (string, CopyID, string, PeopleStage, PeoplePending) {
	t.Helper()
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := discardTestPending(t, stage)
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	return dir, id, instance, stage, pending
}

func TestDiscardPeoplePendingRetiresOnlyTheReviewedRecord(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	if seq, err := DiscardPeoplePending(dir, id, instance, pending); err != nil || seq != 0 {
		t.Fatalf("discard: seq=%d err=%v", seq, err)
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("discard removed stage: %v", err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
		t.Fatal("retired retry admitted")
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err == nil {
		t.Fatal("retired retry confirmed")
	}
	if seq, err := DiscardPeoplePending(dir, id, instance, pending); err != nil || seq != 0 {
		t.Fatalf("repeat: seq=%d err=%v", seq, err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("retired pin remains: %v", err)
	}
}

func TestAmbiguousPendingAndRetiredCannotBeReadOrConfirmed(t *testing.T) {
	dir, id, _, stage, pending := setupDiscard(t)
	other := stage
	other.ID = "0011223344556677"
	if err := SavePeopleStage(dir, id, other); err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Link(pending.Key+".json", pending.Key+".retired"); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if _, err := ReadPeoplePending(dir, id, pending.Key); err == nil {
		t.Fatal("ambiguous pending was returned")
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
		t.Fatal("ambiguous pending was admitted")
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err == nil {
		t.Fatal("ambiguous pending was confirmed")
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("ambiguous pending released stage")
	}
	if err := DeletePeopleStage(dir, id, other.ID); err == nil {
		t.Fatal("ambiguous records allowed another stage deletion")
	}
	root, err = peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, suffix := range []string{".json", ".retired"} {
		if _, err := root.Lstat(pending.Key + suffix); err != nil {
			t.Fatalf("record removed: %v", err)
		}
	}
}

func TestInvalidRetiredFilenameBlocksStageRemoval(t *testing.T) {
	dir, id, _, stage, _ := setupDiscard(t)
	root, err := peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("invalid.retired", []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	root.Close()
	other := stage
	other.ID = "0011223344556677"
	if err := SavePeopleStage(dir, id, other); err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStage(dir, id, other.ID); err == nil {
		t.Fatal("invalid retirement allowed deletion")
	}
}

func TestPeopleStageRemovalRequiresDirectorySync(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	failures := 0
	peopleRecordIO.sync = func(f *os.File) error {
		if strings.Contains(f.Name(), "people-stages") {
			failures++
			return errors.New("synthetic stage directory sync failure")
		}
		return original.sync(f)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil || failures == 0 {
		t.Fatal("stage deletion acknowledged without directory sync")
	}
	peopleRecordIO = original
	if err := DeletePeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("stage deletion retry: %v", err)
	}
}

func TestPeopleStageRemovalRejectsBadIdentityAndFailedDirectoryOpen(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStage(dir, id, "invalid"); err == nil {
		t.Fatal("invalid stage ID accepted")
	}
	if err := DeletePeopleStage(dir, CopyID{}, stage.ID); err == nil {
		t.Fatal("invalid copy accepted")
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	failed := false
	peopleRecordIO.openDir = func(root *os.Root) (*os.File, error) {
		f, err := original.openDir(root)
		if err == nil && strings.Contains(f.Name(), "people-stages") {
			failed = true
			f.Close()
			return nil, errors.New("synthetic open failure")
		}
		return f, err
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil || !failed {
		t.Fatal("failed stage barrier open accepted")
	}
	peopleRecordIO = original
	if err := DeletePeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
}

func TestCompletionReadRefusesAmbiguousRetirement(t *testing.T) {
	dir, id, _, stage, pending := setupDiscard(t)
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	peopleRecordIO.remove = func(root *os.Root, name string) error {
		if name == pending.Key+".json" {
			return errors.New("synthetic interrupted cleanup")
		}
		return original.remove(root, name)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err == nil {
		t.Fatal("cleanup fault not reached")
	}
	peopleRecordIO = original
	root, err := peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Link(pending.Key+".json", pending.Key+".retired"); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if _, err := ReadPeopleCompletion(dir, id, pending.Key); err == nil {
		t.Fatal("ambiguous terminal records accepted")
	}
}

func TestValidateRetainedPeoplePublication(t *testing.T) {
	dir, id, _, stage, pending := setupDiscard(t)
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	path, err := copyPath(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	release, err := storagebudget.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	got, err := validateRetainedPeoplePublication(dir, id, pending, 1)
	if err != nil || len(got) != 1 || !bytes.Equal(got[0].Data, stage.Data) {
		t.Fatalf("retained validation: %v", err)
	}
	wrong := pending
	wrong.Author = strings.Repeat("f", 32)
	if _, err := validateRetainedPeoplePublication(dir, id, wrong, 1); err == nil {
		t.Fatal("foreign history accepted")
	}
}

func TestConfirmationRejectsDifferentRetainedFileMetadata(t *testing.T) {
	for _, field := range []string{"name", "mime"} {
		t.Run(field, func(t *testing.T) {
			dir, id, _, stage, pending := setupDiscard(t)
			retained := stage
			if field == "name" {
				retained.Name = "different.txt"
			} else {
				retained.MIME = "application/octet-stream"
			}
			if err := SavePeoplePublished(dir, id, 1, retained); err != nil {
				t.Fatal(err)
			}
			if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
				t.Fatal(err)
			}
			if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err == nil {
				t.Fatal("different retained metadata accepted")
			}
			if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
				t.Fatal("pending stage released")
			}
		})
	}
}

func TestConfirmationRechecksCompletionAtBudgetBoundary(t *testing.T) {
	if dir := os.Getenv("SCIMUX_TEST_COMPLETION_RECHECK_DIR"); dir != "" {
		kind := os.Getenv("SCIMUX_TEST_COMPLETION_RECHECK_KIND")
		id, stage := sampleCopyID(), syntheticPeopleStage()
		pending := discardTestPending(t, stage)
		release, err := storagebudget.Lock(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		copyRoot := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation)
		completedPath := filepath.Join(copyRoot, "people-completed")
		switch kind {
		case "unsafe-directory":
			if err := os.Remove(completedPath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(completedPath, []byte("damaged"), 0600); err != nil {
				t.Fatal(err)
			}
		case "unreadable-receipt":
			if err := os.WriteFile(filepath.Join(completedPath, pending.Key+".json"), []byte("damaged"), 0600); err != nil {
				t.Fatal(err)
			}
		default:
			value := pending
			if kind == "changed-pending" || kind == "changed-receipt" {
				value.Body = []byte(`{"v":1,"text":"different"}`)
			}
			var raw []byte
			var recordName, target string
			if kind == "changed-pending" {
				raw, err = json.Marshal(value)
				recordName = "pending.json"
				target = filepath.Join(copyRoot, "people-pending", pending.Key+".json")
			} else {
				raw, err = json.Marshal(PeopleCompletion{Pending: value, Seq: 1})
				recordName = "completion.json"
				target = filepath.Join(completedPath, pending.Key+".json")
			}
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(raw)
			record := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(sum[:]), Name: recordName, MIME: "application/json", ConfigHash: pending.ConfigHash, Data: raw}
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, encoded, 0600); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	for _, kind := range []string{"changed-pending", "changed-receipt", "new-receipt", "unreadable-receipt", "unsafe-directory"} {
		t.Run(kind, func(t *testing.T) {
			dir, id, _, stage, pending := setupDiscard(t)
			if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
				t.Fatal(err)
			}
			if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
				t.Fatal(err)
			}
			completed, err := peopleRecordDir(dir, id, true, "people-completed")
			if err != nil {
				t.Fatal(err)
			}
			completed.Close()
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			started := false
			peopleRecordIO.lstat = func(root *os.Root, name string) (os.FileInfo, error) {
				info, statErr := original.lstat(root, name)
				if !started && strings.Contains(root.Name(), "people-completed") && name == pending.Key+".json" {
					started = true
					child := exec.Command(os.Args[0], "-test.run=^TestConfirmationRechecksCompletionAtBudgetBoundary$", "-test.timeout=15s")
					child.Env = append(os.Environ(), "SCIMUX_TEST_COMPLETION_RECHECK_DIR="+dir, "SCIMUX_TEST_COMPLETION_RECHECK_KIND="+kind)
					if output, err := child.CombinedOutput(); err != nil {
						return nil, fmt.Errorf("synthetic peer: %v: %s", err, output)
					}
				}
				return info, statErr
			}
			err = ConfirmPeoplePending(dir, id, pending.Key, 1)
			peopleRecordIO = original
			if err == nil || !started {
				t.Fatalf("stale completion state accepted: started=%t err=%v", started, err)
			}
			if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
				t.Fatalf("stage lost: %v", err)
			}
		})
	}
}

func TestDiscardPreconditionsPreservePending(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	wrong := pending
	wrong.Body = []byte(`{"v":1,"text":"Different"}`)
	for _, tc := range []struct {
		id       CopyID
		instance string
		pending  PeoplePending
	}{
		{id, strings.Repeat("f", 32), pending}, {id, instance, wrong}, {CopyID{}, instance, pending},
	} {
		if _, err := DiscardPeoplePending(dir, tc.id, tc.instance, tc.pending); err == nil {
			t.Fatal("wrong precondition accepted")
		}
	}
	if got, err := ReadPeoplePending(dir, id, pending.Key); err != nil || !samePendingValue(got, pending) {
		t.Fatalf("pending changed: %v", err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("stage unpinned")
	}
}

func TestDiscardRejectsMissingOrDamagedCopyIdentity(t *testing.T) {
	for _, kind := range []string{"missing", "damaged"} {
		t.Run(kind, func(t *testing.T) {
			dir, id, instance, stage, pending := setupDiscard(t)
			root, err := peopleRecordDir(dir, id, false, "people-identity")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "missing" {
				err = root.Remove("0000000000000000.json")
			} else {
				err = root.WriteFile("0000000000000000.json", []byte("damaged"), 0600)
			}
			root.Close()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DiscardPeoplePending(dir, id, instance, pending); err == nil {
				t.Fatal("unverifiable identity accepted")
			}
			if _, err := ReadPeoplePending(dir, id, pending.Key); err != nil {
				t.Fatalf("pending changed: %v", err)
			}
			if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
				t.Fatalf("stage changed: %v", err)
			}
		})
	}
}

func TestDiscardRejectsDamagedLocalState(t *testing.T) {
	for _, kind := range []string{"invalid-instance", "invalid-pending", "lock", "pending-directory", "missing-pending", "retired-mismatch", "ambiguous", "damaged-retired", "damaged-completion", "malformed-receipt", "completion-directory", "initial-open", "initial-sync"} {
		t.Run(kind, func(t *testing.T) {
			dir, id, instance, stage, pending := setupDiscard(t)
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			root, err := peopleRecordDir(dir, id, false, "people-pending")
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "invalid-instance":
				instance = "invalid"
			case "invalid-pending":
				pending.Key = "invalid"
			case "lock":
				if err := os.Remove(filepath.Join(dir, ".storage.lock")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, ".storage.lock"), 0700); err != nil {
					t.Fatal(err)
				}
			case "pending-directory":
				root.Close()
				if err := os.RemoveAll(filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-pending")); err != nil {
					t.Fatal(err)
				}
			case "missing-pending":
				if err := root.Remove(pending.Key + ".json"); err != nil {
					t.Fatal(err)
				}
			case "retired-mismatch":
				root.Close()
				if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
					t.Fatal(err)
				}
				pending.Body = []byte(`{"v":1,"text":"different"}`)
			case "ambiguous":
				if err := root.Link(pending.Key+".json", pending.Key+".retired"); err != nil {
					t.Fatal(err)
				}
			case "damaged-retired":
				if err := root.WriteFile(pending.Key+".retired", []byte("damaged"), 0600); err != nil {
					t.Fatal(err)
				}
			case "damaged-completion", "malformed-receipt":
				completed, err := peopleRecordDir(dir, id, true, "people-completed")
				if err != nil {
					t.Fatal(err)
				}
				data := []byte("damaged")
				if kind == "malformed-receipt" {
					body := []byte(`{"unexpected":true}`)
					sum := sha256.Sum256(body)
					record := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(sum[:]), Name: "completion.json", MIME: "application/json", ConfigHash: pending.ConfigHash, Data: body}
					data, err = json.Marshal(record)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := completed.WriteFile(pending.Key+".json", data, 0600); err != nil {
					t.Fatal(err)
				}
				completed.Close()
			case "completion-directory":
				copyDir := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation)
				if err := os.WriteFile(filepath.Join(copyDir, "people-completed"), []byte("damaged"), 0600); err != nil {
					t.Fatal(err)
				}
			case "initial-open":
				peopleRecordIO.openDir = func(r *os.Root) (*os.File, error) {
					f, err := original.openDir(r)
					if err == nil && strings.Contains(f.Name(), "people-pending") {
						f.Close()
						return nil, errors.New("synthetic open failure")
					}
					return f, err
				}
			case "initial-sync":
				peopleRecordIO.sync = func(f *os.File) error {
					if strings.Contains(f.Name(), "people-pending") {
						return errors.New("synthetic sync failure")
					}
					return original.sync(f)
				}
			}
			root.Close()
			if _, err := DiscardPeoplePending(dir, id, instance, pending); err == nil {
				t.Fatal("damaged state accepted")
			}
			peopleRecordIO = original
			if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
				t.Fatalf("stage lost: %v", err)
			}
		})
	}
}

func TestPeopleTerminalReadsRejectDamagedRecordsAndBarriers(t *testing.T) {
	for _, kind := range []string{"invalid-key", "invalid-copy", "lock", "pending-directory", "retired-envelope", "retired-body", "retired-open", "retired-sync", "completion-envelope"} {
		t.Run(kind, func(t *testing.T) {
			dir, id, instance, stage, pending := setupDiscard(t)
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			key := pending.Key
			switch kind {
			case "invalid-key":
				key = "invalid"
			case "invalid-copy":
				id = CopyID{}
			case "lock":
				if err := os.Remove(filepath.Join(dir, ".storage.lock")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, ".storage.lock"), 0700); err != nil {
					t.Fatal(err)
				}
			case "pending-directory":
				path := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-pending")
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("damaged"), 0600); err != nil {
					t.Fatal(err)
				}
			case "retired-envelope", "retired-body", "retired-open", "retired-sync":
				if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
					t.Fatal(err)
				}
				if kind == "retired-envelope" || kind == "retired-body" {
					root, err := peopleRecordDir(dir, id, false, "people-pending")
					if err != nil {
						t.Fatal(err)
					}
					data := []byte("damaged")
					if kind == "retired-body" {
						body := []byte(`{"key":"invalid"}`)
						sum := sha256.Sum256(body)
						record := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(sum[:]), Name: "pending.json", MIME: "application/json", ConfigHash: pending.ConfigHash, Data: body}
						data, err = json.Marshal(record)
						if err != nil {
							t.Fatal(err)
						}
					}
					err = root.WriteFile(pending.Key+".retired", data, 0600)
					root.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
				if kind == "retired-open" {
					peopleRecordIO.openDir = func(root *os.Root) (*os.File, error) {
						f, err := original.openDir(root)
						if err == nil && strings.Contains(f.Name(), "people-pending") {
							f.Close()
							return nil, errors.New("synthetic open failure")
						}
						return f, err
					}
				}
				if kind == "retired-sync" {
					peopleRecordIO.sync = func(f *os.File) error {
						if strings.Contains(f.Name(), "people-pending") {
							return errors.New("synthetic sync failure")
						}
						return original.sync(f)
					}
				}
			case "completion-envelope":
				root, err := peopleRecordDir(dir, id, true, "people-completed")
				if err != nil {
					t.Fatal(err)
				}
				err = root.WriteFile(key+".json", []byte("damaged"), 0600)
				root.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ReadPeopleCompletion(dir, id, key); err == nil {
				t.Fatal("invalid completion accepted")
			}
			if kind != "completion-envelope" {
				if _, err := ReadPeoplePending(dir, id, key); err == nil {
					t.Fatal("invalid pending accepted")
				}
			}
			if kind == "retired-envelope" || kind == "retired-body" || kind == "retired-open" || kind == "retired-sync" {
				if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
					t.Fatal("retired key accepted")
				}
				if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
					t.Fatal("invalid retirement released the stage")
				}
			}
		})
	}
}

func TestPendingEnvelopeRejectsEveryBindingMismatch(t *testing.T) {
	stage := syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	valid, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey := pending
	wrongKey.Key = "0011223344556677"
	wrongConfig := pending
	wrongConfig.ConfigHash = strings.Repeat("b", 64)
	wrongOwner := pending
	wrongOwner.OwnerInstance = "synthetic-instance"
	encode := func(p PeoplePending) []byte {
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"malformed", []byte("{")}, {"trailing", append(append([]byte(nil), valid...), []byte(" {}")...)},
		{"invalid", []byte(`{"key":"invalid"}`)}, {"key", encode(wrongKey)}, {"config", encode(wrongConfig)}, {"owner", encode(wrongOwner)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(tc.body)
			record := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(sum[:]), Name: "pending.json", MIME: "application/json", ConfigHash: pending.ConfigHash, Data: tc.body}
			if err := savePeopleRecord(dir, id, record, "people-pending"); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadPeoplePending(dir, id, pending.Key); err == nil {
				t.Fatal("mismatched pending accepted")
			}
			if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
				t.Fatal("mismatched pending released stage")
			}
		})
	}
}

func TestPeopleRecordReadRejectsReplacementAndDamagedEnvelopes(t *testing.T) {
	for _, kind := range []string{"oversize", "trailing", "wrong-id", "invalid-stage", "replacement", "growing-read"} {
		t.Run(kind, func(t *testing.T) {
			dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			root, err := peopleStageDir(dir, id, false)
			if err != nil {
				t.Fatal(err)
			}
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			name := stage.ID + ".json"
			switch kind {
			case "oversize":
				f, err := root.OpenFile(name, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate((6 << 20) + 1); err != nil {
					f.Close()
					t.Fatal(err)
				}
				f.Close()
			case "trailing", "wrong-id", "invalid-stage":
				value := stage
				if kind == "wrong-id" {
					value.ID = "0011223344556677"
				}
				if kind == "invalid-stage" {
					value.Data = []byte("different")
				}
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "trailing" {
					data = append(data, []byte(" {}")...)
				}
				if err := root.WriteFile(name, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "replacement":
				peopleRecordIO.openRead = func(r *os.Root, filename string) (*os.File, error) {
					if filename == name {
						data, err := json.Marshal(stage)
						if err != nil {
							return nil, err
						}
						if err := r.Rename(name, name+".old"); err != nil {
							return nil, err
						}
						if err := r.WriteFile(name, data, 0600); err != nil {
							return nil, err
						}
					}
					return original.openRead(r, filename)
				}
			case "growing-read":
				peopleRecordIO.read = func(io.Reader) ([]byte, error) { return make([]byte, (6<<20)+1), nil }
			}
			root.Close()
			if _, err := ReadPeopleStage(dir, id, stage.ID); err == nil {
				t.Fatal("damaged or replaced record accepted")
			}
		})
	}
}

func TestDiscardRetirementFaultRecovery(t *testing.T) {
	for _, fault := range []string{"rename", "sync-after-rename", "open-after-rename"} {
		t.Run(fault, func(t *testing.T) {
			dir, id, instance, stage, pending := setupDiscard(t)
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			renamed, injected := false, false
			peopleRecordIO.rename = func(root *os.Root, old, name string) error {
				if fault == "rename" {
					injected = true
					return errors.New("synthetic rename failure")
				}
				err := original.rename(root, old, name)
				renamed = err == nil
				return err
			}
			peopleRecordIO.openDir = func(root *os.Root) (*os.File, error) {
				if fault == "open-after-rename" && renamed && !injected {
					injected = true
					return nil, errors.New("synthetic open failure")
				}
				return original.openDir(root)
			}
			peopleRecordIO.sync = func(f *os.File) error {
				if fault == "sync-after-rename" && renamed && !injected {
					injected = true
					return errors.New("synthetic sync failure")
				}
				return original.sync(f)
			}
			if _, err := DiscardPeoplePending(dir, id, instance, pending); err == nil || !injected {
				t.Fatalf("fault not reported: %v", err)
			}
			if fault == "sync-after-rename" {
				peopleRecordIO.sync = func(f *os.File) error {
					if strings.Contains(f.Name(), "people-pending") {
						return errors.New("persistent barrier failure")
					}
					return original.sync(f)
				}
				if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
					t.Fatal("unsynced retirement released pin")
				}
			}
			if fault == "rename" && DeletePeopleStage(dir, id, stage.ID) == nil {
				t.Fatal("failed rename unpinned stage")
			}
			peopleRecordIO = original
			if seq, err := DiscardPeoplePending(dir, id, instance, pending); err != nil || seq != 0 {
				t.Fatalf("recovery: seq=%d err=%v", seq, err)
			}
			if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, stage.Data) {
				t.Fatalf("stage lost: %v", err)
			}
		})
	}
}

func TestReadPendingRepairsVisibleRetirementBarrier(t *testing.T) {
	dir, id, instance, _, pending := setupDiscard(t)
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	renamed := false
	peopleRecordIO.rename = func(root *os.Root, old, name string) error {
		err := original.rename(root, old, name)
		renamed = err == nil
		return err
	}
	peopleRecordIO.sync = func(f *os.File) error {
		if renamed && strings.Contains(f.Name(), "people-pending") {
			return errors.New("synthetic persistent barrier failure")
		}
		return original.sync(f)
	}
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err == nil {
		t.Fatal("discard accepted failed barrier")
	}
	if _, err := ReadPeoplePending(dir, id, pending.Key); !errors.Is(err, ErrPeopleStage) {
		t.Fatalf("read did not repair barrier: %v", err)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1); !errors.Is(err, ErrPeopleStage) {
		t.Fatalf("confirmation did not repair barrier: %v", err)
	}
	peopleRecordIO = original
	if _, err := ReadPeoplePending(dir, id, pending.Key); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("retired read: %v", err)
	}
}

func TestDiscardCompletionWinsAndCleansPending(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	peopleRecordIO.remove = func(root *os.Root, name string) error {
		if name == pending.Key+".json" {
			return errors.New("synthetic cleanup failure")
		}
		return original.remove(root, name)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err == nil {
		t.Fatal("cleanup fault not reached")
	}
	peopleRecordIO = original
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
		t.Fatal("durably completed publication admitted as pending")
	}
	for i := 0; i < 2; i++ {
		if seq, err := DiscardPeoplePending(dir, id, instance, pending); err != nil || seq != 1 {
			t.Fatalf("completed: seq=%d err=%v", seq, err)
		}
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("completion pin remains: %v", err)
	}
	if _, err := ReadPeoplePublished(dir, id, 1, stage.ID); err != nil {
		t.Fatalf("retained bytes lost: %v", err)
	}
}

func TestDiscardRecoversLegacyCompletionWithoutFileManifest(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
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
			return errors.New("synthetic interrupted cleanup")
		}
		return original.remove(root, name)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err == nil {
		t.Fatal("cleanup was not interrupted")
	}
	peopleRecordIO = original
	if seq, err := DiscardPeoplePending(dir, id, instance, pending); err != nil || seq != 1 {
		t.Fatalf("legacy receipt: seq=%d err=%v", seq, err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("completed pin remained: %v", err)
	}
	root, err := peopleRecordDir(dir, id, false, "people-records", "comments", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Remove(stage.ID + ".json"); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if seq, err := DiscardPeoplePending(dir, id, instance, pending); err != nil || seq != 1 {
		t.Fatalf("legacy terminal receipt: %d %v", seq, err)
	}
}

func TestDiscardRecoversLegacyNonObjectJSON(t *testing.T) {
	for _, body := range []string{`[]`, `null`, `"synthetic"`} {
		t.Run(fmt.Sprintf("length-%d", len(body)), func(t *testing.T) {
			dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			pending := syntheticPeoplePending(stage)
			pending.Body = []byte(body)
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
					return errors.New("synthetic interrupted cleanup")
				}
				return original.remove(root, name)
			}
			if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err == nil {
				t.Fatal("cleanup was not interrupted")
			}
			peopleRecordIO = original
			if seq, err := DiscardPeoplePending(dir, id, instance, pending); err != nil || seq != 1 {
				t.Fatalf("legacy receipt: %d %v", seq, err)
			}
		})
	}
}

func TestDiscardCompletedReceiptSurvivesRemovalOfRetainedBytes(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, false, "people-records", "comments", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Remove(stage.ID + ".json"); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if seq, err := DiscardPeoplePending(dir, id, instance, pending); err != nil || seq != 1 {
		t.Fatalf("terminal receipt after local bytes removed: seq=%d err=%v", seq, err)
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
		t.Fatal("completed retry readmitted")
	}
	entries, _, err := ReadCopy(dir, id, CommentsLog, 0, 1, 4<<20)
	if err != nil || len(entries) != 1 || entries[0].Key != "c/"+pending.Key {
		t.Fatalf("history changed: %v", err)
	}
}

func TestDiscardTerminalReceiptChecksHistoryAndLocalRecordDamage(t *testing.T) {
	for _, kind := range []string{"missing-record-directory", "unsafe-record-directory", "damaged-record", "damaged-history"} {
		t.Run(kind, func(t *testing.T) {
			dir, id, instance, stage, pending := setupDiscard(t)
			if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
				t.Fatal(err)
			}
			if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
				t.Fatal(err)
			}
			if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err != nil {
				t.Fatal(err)
			}
			copyRoot := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation)
			switch kind {
			case "missing-record-directory":
				if err := os.RemoveAll(filepath.Join(copyRoot, "people-records", "comments", "1")); err != nil {
					t.Fatal(err)
				}
			case "unsafe-record-directory":
				path := filepath.Join(copyRoot, "people-records")
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("damaged"), 0600); err != nil {
					t.Fatal(err)
				}
			case "damaged-record":
				root, err := peopleRecordDir(dir, id, false, "people-records", "comments", "1")
				if err != nil {
					t.Fatal(err)
				}
				err = root.WriteFile(stage.ID+".json", []byte("damaged"), 0600)
				root.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "damaged-history":
				path, err := copyLogPath(dir, id, CommentsLog)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("damaged\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			seq, err := DiscardPeoplePending(dir, id, instance, pending)
			if kind == "missing-record-directory" || kind == "damaged-record" {
				if err != nil || seq != 1 {
					t.Fatalf("terminal receipt: %d %v", seq, err)
				}
			} else if err == nil {
				t.Fatal("damaged terminal state accepted")
			}
		})
	}
}

func TestDiscardRejectsMalformedOrUnknownPresentManifestWithoutBytes(t *testing.T) {
	for _, body := range []string{
		`{"people_attachments_version":2}`,
		`{"people_attachments_version":2,"people_attachments":[]}`,
		`{"people_attachments":[]}`,
		`{"people_attachments_version":1,"people_attachments":null}`,
		`{"people_attachments_version":"1","people_attachments":[]}`,
	} {
		t.Run(fmt.Sprintf("case-%d", len(body)), func(t *testing.T) {
			dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			pending := syntheticPeoplePending(stage)
			pending.Body = []byte(body)
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
			if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err != nil {
				t.Fatal(err)
			}
			root, err := peopleRecordDir(dir, id, false, "people-records", "comments", "1")
			if err != nil {
				t.Fatal(err)
			}
			if err := root.Remove(stage.ID + ".json"); err != nil {
				t.Fatal(err)
			}
			root.Close()
			if _, err := DiscardPeoplePending(dir, id, instance, pending); err == nil {
				t.Fatal("invalid present manifest accepted")
			}
		})
	}
}

func TestDiscardRejectsUnknownManifestDescriptorFields(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	pending.Body, err = json.Marshal(map[string]any{"people_attachments_version": 1, "people_attachments": []any{map[string]any{"id": stage.ID, "sha256": stage.Hash, "size": len(stage.Data), "name": stage.Name, "mime": stage.MIME, "unexpected": true}}})
	if err != nil {
		t.Fatal(err)
	}
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
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, false, "people-records", "comments", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Remove(stage.ID + ".json"); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err == nil {
		t.Fatal("unknown descriptor field accepted")
	}
}

func TestDiscardRejectsOversizedManifestWithoutRetainedBytes(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	pending.StageIDs = nil
	var stages []PeopleStage
	var files []any
	for i := 0; i < 5; i++ {
		item := stage
		item.ID = fmt.Sprintf("%016x", i+1)
		stages = append(stages, item)
		pending.StageIDs = append(pending.StageIDs, item.ID)
		files = append(files, map[string]any{"id": item.ID, "sha256": item.Hash, "size": 4 << 20, "name": item.Name, "mime": item.MIME})
		if err := SavePeopleStage(dir, id, item); err != nil {
			t.Fatal(err)
		}
		if err := SavePeoplePublished(dir, id, 1, item); err != nil {
			t.Fatal(err)
		}
	}
	pending.Body, err = json.Marshal(map[string]any{"people_attachments_version": 1, "people_attachments": files})
	if err != nil {
		t.Fatal(err)
	}
	if err := SavePeoplePending(dir, id, pending, stages); err != nil {
		t.Fatal(err)
	}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, false, "people-records", "comments", "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range stages {
		if err := root.Remove(item.ID + ".json"); err != nil {
			t.Fatal(err)
		}
	}
	root.Close()
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err == nil {
		t.Fatal("oversized manifest accepted")
	}
}

func TestDiscardRepairsInterruptedCompletionCleanup(t *testing.T) {
	for _, fault := range []string{"remove", "open", "sync"} {
		t.Run(fault, func(t *testing.T) {
			dir, id, instance, stage, pending := setupDiscard(t)
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
					return errors.New("synthetic interrupted cleanup")
				}
				return original.remove(root, name)
			}
			if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err == nil {
				t.Fatal("completion was not interrupted")
			}
			peopleRecordIO = original
			removed, injected := false, false
			peopleRecordIO.remove = func(root *os.Root, name string) error {
				if name == pending.Key+".json" && fault == "remove" {
					injected = true
					return errors.New("synthetic remove failure")
				}
				err := original.remove(root, name)
				if name == pending.Key+".json" && err == nil {
					removed = true
				}
				return err
			}
			peopleRecordIO.openDir = func(root *os.Root) (*os.File, error) {
				if removed && fault == "open" && !injected {
					injected = true
					return nil, errors.New("synthetic open failure")
				}
				return original.openDir(root)
			}
			peopleRecordIO.sync = func(f *os.File) error {
				if removed && fault == "sync" && !injected {
					injected = true
					return errors.New("synthetic sync failure")
				}
				return original.sync(f)
			}
			if _, err := DiscardPeoplePending(dir, id, instance, pending); err == nil || !injected {
				t.Fatalf("cleanup failure not reported: %v", err)
			}
			peopleRecordIO = original
			if seq, err := DiscardPeoplePending(dir, id, instance, pending); err != nil || seq != 1 {
				t.Fatalf("cleanup recovery: %d %v", seq, err)
			}
			if _, err := ReadPeoplePublished(dir, id, 1, stage.ID); err != nil {
				t.Fatalf("retained data lost: %v", err)
			}
		})
	}
}

func TestDiscardRejectsInvalidCompletionRecovery(t *testing.T) {
	for _, kind := range []string{"completion-mismatch", "pending-mismatch", "pending-corrupt", "missing-retained", "manifest-count"} {
		t.Run(kind, func(t *testing.T) {
			dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			pending := discardTestPending(t, stage)
			if kind == "manifest-count" {
				pending.Body = []byte(`{"people_attachments_version":1,"people_attachments":[]}`)
			}
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
					return errors.New("synthetic interrupted cleanup")
				}
				return original.remove(root, name)
			}
			if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err == nil {
				t.Fatal("completion was not interrupted")
			}
			peopleRecordIO = original
			expected := pending
			switch kind {
			case "completion-mismatch":
				expected.Body = []byte(`{"v":1,"text":"other"}`)
			case "pending-corrupt", "pending-mismatch":
				root, err := peopleRecordDir(dir, id, false, "people-pending")
				if err != nil {
					t.Fatal(err)
				}
				data := []byte("damaged")
				if kind == "pending-mismatch" {
					changed := pending
					changed.Body = []byte(`{"v":1,"text":"replacement"}`)
					raw, err := json.Marshal(changed)
					if err != nil {
						t.Fatal(err)
					}
					sum := sha256.Sum256(raw)
					record := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(sum[:]), Name: "pending.json", MIME: "application/json", ConfigHash: pending.ConfigHash, OwnerInstance: pending.OwnerInstance, Data: raw}
					data, err = json.Marshal(record)
					if err != nil {
						t.Fatal(err)
					}
				}
				err = root.WriteFile(pending.Key+".json", data, 0600)
				root.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "missing-retained":
				root, err := peopleRecordDir(dir, id, false, "people-records", "comments", "1")
				if err != nil {
					t.Fatal(err)
				}
				err = root.Remove(stage.ID + ".json")
				root.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := DiscardPeoplePending(dir, id, instance, expected); err == nil {
				t.Fatal("invalid completion recovery accepted")
			}
			if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
				t.Fatalf("stage lost: %v", err)
			}
		})
	}
}

func TestDiscardRejectsCompletionWithReplacedRetainedBytes(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	peopleRecordIO.remove = func(root *os.Root, name string) error {
		if name == pending.Key+".json" {
			return errors.New("synthetic cleanup failure")
		}
		return original.remove(root, name)
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err == nil {
		t.Fatal("cleanup fault not reached")
	}
	peopleRecordIO = original
	replacement := stage
	replacement.Data = []byte("different synthetic bytes")
	digest := sha256.Sum256(replacement.Data)
	replacement.Hash = hex.EncodeToString(digest[:])
	encoded, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, false, "people-records", "comments", "1")
	if err != nil {
		t.Fatal(err)
	}
	err = root.WriteFile(stage.ID+".json", encoded, 0600)
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	stageRoot, err := peopleStageDir(dir, id, false)
	if err != nil {
		t.Fatal(err)
	}
	err = stageRoot.WriteFile(stage.ID+".json", encoded, 0600)
	stageRoot.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err == nil {
		t.Fatal("replaced retained bytes accepted")
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("invalid completion released pin")
	}
}

func TestDiscardCompletedReceiptChecksRetainedManifestAfterStageRemoval(t *testing.T) {
	dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	pending.Body, err = json.Marshal(map[string]any{"people_attachments_version": 1, "people_attachments": []any{map[string]any{"id": stage.ID, "sha256": stage.Hash, "size": len(stage.Data), "name": stage.Name, "mime": stage.MIME}}})
	if err != nil {
		t.Fatal(err)
	}
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
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err != nil {
		t.Fatal(err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err != nil {
		t.Fatal(err)
	}
	if seq, err := DiscardPeoplePending(dir, id, instance, pending); err != nil || seq != 1 {
		t.Fatalf("valid completion: %d %v", seq, err)
	}
	replacement := stage
	replacement.Data = []byte("different synthetic bytes")
	digest := sha256.Sum256(replacement.Data)
	replacement.Hash = hex.EncodeToString(digest[:])
	encoded, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, false, "people-records", "comments", "1")
	if err != nil {
		t.Fatal(err)
	}
	err = root.WriteFile(stage.ID+".json", encoded, 0600)
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err == nil {
		t.Fatal("replacement retained bytes accepted")
	}
}

func TestDiscardRejectsEachRetainedDescriptorMismatch(t *testing.T) {
	for _, field := range []string{"id", "sha256", "size", "name", "mime", "zero-size", "oversize", "bad-name", "bad-hash", "bad-mime"} {
		t.Run(field, func(t *testing.T) {
			dir, id, stage := t.TempDir(), sampleCopyID(), syntheticPeopleStage()
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			descriptor := map[string]any{"id": stage.ID, "sha256": stage.Hash, "size": len(stage.Data), "name": stage.Name, "mime": stage.MIME}
			switch field {
			case "id":
				descriptor[field] = "0011223344556677"
			case "sha256":
				descriptor[field] = strings.Repeat("f", 64)
			case "size":
				descriptor[field] = len(stage.Data) + 1
			case "name":
				descriptor[field] = "different.txt"
			case "mime":
				descriptor[field] = "application/octet-stream"
			case "zero-size":
				descriptor["size"] = 0
			case "oversize":
				descriptor["size"] = (4 << 20) + 1
			case "bad-name":
				descriptor["name"] = "../unsafe"
			case "bad-hash":
				descriptor["sha256"] = "bad"
			case "bad-mime":
				descriptor["mime"] = "bad"
			}
			pending := syntheticPeoplePending(stage)
			pending.Body, err = json.Marshal(map[string]any{"people_attachments_version": 1, "people_attachments": []any{descriptor}})
			if err != nil {
				t.Fatal(err)
			}
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
			if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := DiscardPeoplePending(dir, id, instance, pending); err == nil {
				t.Fatalf("mismatched %s accepted", field)
			}
		})
	}
}

func TestDiscardConcurrentAndOverBudget(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	root, err := peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	before, err := root.Lstat(pending.Key + ".json")
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":1,"storage_min_free_bytes":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := DiscardPeoplePending(dir, id, instance, pending); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	root, err = peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	after, err := root.Lstat(pending.Key + ".retired")
	root.Close()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() {
		t.Fatal("retirement did not reuse the pending record")
	}
	if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatalf("stage lost: %v", err)
	}
}

func TestDiscardAtAndBeyondGlobalByteLimit(t *testing.T) {
	for _, beyond := range []bool{false, true} {
		t.Run(fmt.Sprintf("over=%t", beyond), func(t *testing.T) {
			dir, id, instance, stage, pending := setupDiscard(t)
			before, err := storagebudget.Inspect(dir)
			if err != nil || before.UsedBytes < 2 {
				t.Fatalf("usage: %+v %v", before, err)
			}
			limit := before.UsedBytes
			if beyond {
				limit--
			}
			settings := fmt.Sprintf(`{"storage_global_limit_bytes":%d,"storage_min_free_bytes":0}`, limit)
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0600); err != nil {
				t.Fatal(err)
			}
			if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
				t.Fatalf("identical retry at cap: %v", err)
			}
			if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
				t.Fatalf("zero-growth discard: %v", err)
			}
			after, err := storagebudget.Inspect(dir)
			if err != nil || before.UsedBytes != after.UsedBytes {
				t.Fatalf("usage changed: before=%d after=%d err=%v", before.UsedBytes, after.UsedBytes, err)
			}
		})
	}
}

func TestDiscardAtCopyByteLimitDoesNotGrow(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	copyRoot := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation)
	var already int64
	for _, tree := range []string{"people-stages", "people-pending", "people-identity"} {
		entries, err := os.ReadDir(filepath.Join(copyRoot, tree))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			already += info.Size()
		}
	}
	base := filepath.Join(copyRoot, "people-records", "comments", "1")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(base, "1111111111111111.json"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((256 << 20) - already); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	other := stage
	other.ID = "0011223344556677"
	if err := SavePeopleStage(dir, id, other); !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("expected full copy bytes: %v", err)
	}
	before, err := storagebudget.Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
		t.Fatal(err)
	}
	after, err := storagebudget.Inspect(dir)
	if err != nil || before.UsedBytes != after.UsedBytes {
		t.Fatalf("byte accounting changed: %d %d %v", before.UsedBytes, after.UsedBytes, err)
	}
	if err := SavePeopleStage(dir, id, other); !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("retirement bypassed copy bytes: %v", err)
	}
}

func TestDiscardRetirementStillCountsAtCopyRecordLimit(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	base := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-records", "comments", "1")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4093; i++ {
		if err := os.WriteFile(filepath.Join(base, fmt.Sprintf("%016x.json", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	other := stage
	other.ID = "0011223344556677"
	if other.ID == stage.ID {
		t.Fatal("new stage must have a different ID")
	}
	if err := SavePeopleStage(dir, id, other); !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("expected full copy: %v", err)
	}
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
		t.Fatal(err)
	}
	if err := SavePeopleStage(dir, id, other); !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("retired record omitted from count: %v", err)
	}
}

func TestDiscardAndConfirmSerialize(t *testing.T) {
	for round := 0; round < 8; round++ {
		dir, id, instance, stage, pending := setupDiscard(t)
		if err := SavePeoplePublished(dir, id, 1, stage); err != nil {
			t.Fatal(err)
		}
		if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		done := make(chan error, 1)
		go func() { <-start; done <- ConfirmPeoplePending(dir, id, pending.Key, 1) }()
		close(start)
		seq, err := DiscardPeoplePending(dir, id, instance, pending)
		confirmErr := <-done
		if err != nil {
			t.Fatal(err)
		}
		if seq == 0 && confirmErr == nil {
			t.Fatal("confirmation succeeded after retirement")
		}
		if seq == 1 && confirmErr != nil {
			t.Fatalf("completion reported without confirmation: %v", confirmErr)
		}
		if seq > 1 {
			t.Fatalf("unexpected sequence: %d", seq)
		}
		if _, err := ReadPeoplePublished(dir, id, 1, stage.ID); err != nil {
			t.Fatalf("retained history lost: %v", err)
		}
	}
}

func TestDiscardSurvivesRestart(t *testing.T) {
	if dir := os.Getenv("SCIMUX_TEST_DISCARD_DIR"); dir != "" {
		id, stage := sampleCopyID(), syntheticPeopleStage()
		pending := discardTestPending(t, stage)
		if seq, err := DiscardPeoplePending(dir, id, os.Getenv("SCIMUX_TEST_DISCARD_INSTANCE"), pending); err != nil || seq != 0 {
			t.Fatalf("restart retry: %d %v", seq, err)
		}
		if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
			t.Fatal("restart admitted retired key")
		}
		return
	}
	dir, id, instance, _, pending := setupDiscard(t)
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDiscardSurvivesRestart$")
	cmd.Env = append(os.Environ(), "SCIMUX_TEST_DISCARD_DIR="+dir, "SCIMUX_TEST_DISCARD_INSTANCE="+instance)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restart: %v: %s", err, output)
	}
}
