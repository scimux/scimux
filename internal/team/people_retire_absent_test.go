package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/storagebudget"
)

func TestAbsentRetirementTerminalAcrossLocalReaders(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	author, err := identity.LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		if err := RetireAbsentPeopleKey(dir, id, instance, string(author), fmt.Sprintf("%016x", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	page, err := ReadPeopleInventory(dir, id, instance, string(author), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 0 || page.Next == "" {
		t.Fatalf("empty terminal page: %+v", page)
	}
	page, err = ReadPeopleInventory(dir, id, instance, string(author), page.Next)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].ID != stage.ID {
		t.Fatalf("stage page: %+v %v", page, err)
	}
	pending := syntheticPeoplePending(stage)
	pending.Key = "0000000000000001"
	pending.Author = string(author)
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
		t.Fatal("retired key admitted")
	}
	if _, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("terminal local read: %v", err)
	}
	if _, err := ReadPeopleCompletion(dir, id, pending.Key); err == nil {
		t.Fatal("terminal completion returned")
	}
	if err := ConfirmPeoplePending(dir, id, pending.Key, 1); err == nil {
		t.Fatal("terminal key confirmed")
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("marker pinned stage: %v", err)
	}
}

func TestAbsentRetirementDecoderRejectsInvalidFields(t *testing.T) {
	marker := absentRetirement{Version: 1, Key: "0123456789abcdef", Author: strings.Repeat("a", 32), CopyInstance: strings.Repeat("b", 32)}
	raw, _ := json.Marshal(marker)
	base := PeopleStage{ID: marker.Key, Name: "retired-absent.json", MIME: "application/json", ConfigHash: strings.Repeat("0", 64), Data: raw}
	for _, tc := range []struct {
		name   string
		mutate func(*PeopleStage)
	}{
		{"valid", func(*PeopleStage) {}},
		{"invalid_json", func(s *PeopleStage) { s.Data = []byte("{") }},
		{"unknown", func(s *PeopleStage) {
			s.Data = append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"extra":1}`)...)
		}},
		{"duplicate", func(s *PeopleStage) {
			s.Data = append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"version":1}`)...)
		}},
		{"trailing", func(s *PeopleStage) { s.Data = append(append([]byte(nil), raw...), []byte(` {}`)...) }},
		{"version", func(s *PeopleStage) { m := marker; m.Version = 2; s.Data, _ = json.Marshal(m) }},
		{"different_key", func(s *PeopleStage) { m := marker; m.Key = "1111111111111111"; s.Data, _ = json.Marshal(m) }},
		{"invalid_key", func(s *PeopleStage) { m := marker; m.Key = "bad"; s.ID = "bad"; s.Data, _ = json.Marshal(m) }},
		{"author", func(s *PeopleStage) { m := marker; m.Author = "bad"; s.Data, _ = json.Marshal(m) }},
		{"copy", func(s *PeopleStage) { m := marker; m.CopyInstance = "bad"; s.Data, _ = json.Marshal(m) }},
		{"config", func(s *PeopleStage) { s.ConfigHash = strings.Repeat("a", 64) }},
		{"owner", func(s *PeopleStage) { s.OwnerInstance = "owner" }},
		{"mime", func(s *PeopleStage) { s.MIME = "text/plain" }},
		{"name", func(s *PeopleStage) { s.Name = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mutate(&s)
			_, _, err := decodePeopleRetirement(s)
			if (err == nil) != (tc.name == "valid") {
				t.Fatalf("decode %s: %v", tc.name, err)
			}
		})
	}
	stage := syntheticPeopleStage()
	pending := syntheticPeoplePending(stage)
	data, _ := json.Marshal(pending)
	legacy := PeopleStage{ID: pending.Key, ConfigHash: pending.ConfigHash, OwnerInstance: pending.OwnerInstance, Data: data}
	m, p, err := decodePeopleRetirement(legacy)
	if err != nil || m != nil || p == nil || p.Key != pending.Key {
		t.Fatalf("legacy: %v", err)
	}
}

func TestAbsentRetirementWriteFaultsRecover(t *testing.T) {
	for _, fault := range []string{"open_file", "write", "short_write", "file_sync", "close", "link", "temp_remove", "dir_open", "dir_sync", "ancestor_sync"} {
		t.Run(fault, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			author, err := identity.LoadOrCreateInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := SavePeopleStage(dir, id, syntheticPeopleStage()); err != nil {
				t.Fatal(err)
			}
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			injected, linked := false, false
			peopleRecordIO.openFile = func(r *os.Root, n string, f int, m os.FileMode) (*os.File, error) {
				if fault == "open_file" && peopleStageTemp(n) && !injected {
					injected = true
					return nil, errors.New("synthetic open")
				}
				return original.openFile(r, n, f, m)
			}
			peopleRecordIO.write = func(f *os.File, b []byte) (int, error) {
				if !injected && peopleStageTemp(filepath.Base(f.Name())) && (fault == "write" || fault == "short_write") {
					injected = true
					if fault == "write" {
						return 0, errors.New("synthetic write")
					}
					return 0, nil
				}
				return original.write(f, b)
			}
			peopleRecordIO.sync = func(f *os.File) error {
				if !injected && ((fault == "file_sync" && peopleStageTemp(filepath.Base(f.Name()))) || (fault == "dir_sync" && linked) || (fault == "ancestor_sync" && !strings.Contains(f.Name(), "people-identity"))) {
					injected = true
					return errors.New("synthetic sync")
				}
				return original.sync(f)
			}
			peopleRecordIO.close = func(f *os.File) error {
				if fault == "close" && !injected && peopleStageTemp(filepath.Base(f.Name())) {
					injected = true
					_ = original.close(f)
					return errors.New("synthetic close")
				}
				return original.close(f)
			}
			peopleRecordIO.link = func(r *os.Root, old, name string) error {
				if fault == "link" && !injected {
					injected = true
					return errors.New("synthetic link")
				}
				err := original.link(r, old, name)
				linked = err == nil
				return err
			}
			peopleRecordIO.remove = func(r *os.Root, name string) error {
				if fault == "temp_remove" && linked && !injected && peopleStageTemp(name) {
					injected = true
					return errors.New("synthetic remove")
				}
				return original.remove(r, name)
			}
			peopleRecordIO.openDir = func(r *os.Root) (*os.File, error) {
				if fault == "dir_open" && linked && !injected {
					injected = true
					return nil, errors.New("synthetic open directory")
				}
				return original.openDir(r)
			}
			key := "0123456789abcdef"
			err = RetireAbsentPeopleKey(dir, id, instance, string(author), key)
			if err == nil || !injected {
				t.Fatalf("fault not observed: %v", err)
			}
			peopleRecordIO = original
			if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err != nil {
				t.Fatalf("repeat: %v", err)
			}
			p := syntheticPeoplePending(syntheticPeopleStage())
			p.Key = key
			p.Author = string(author)
			if err := SavePeoplePending(dir, id, p, []PeopleStage{syntheticPeopleStage()}); err == nil {
				t.Fatal("retired key admitted")
			}
		})
	}
}

func TestAbsentRetirementStateAndBinding(t *testing.T) {
	for _, state := range []string{"absent", "retry", "pending", "retired", "wrong_copy", "wrong_author", "missing_installation", "corrupt_installation", "missing_copy", "corrupt_retired", "ambiguous"} {
		t.Run(state, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			author, err := identity.LoadOrCreateInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			stage := syntheticPeopleStage()
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			pending := syntheticPeoplePending(stage)
			pending.Author = string(author)
			if state == "pending" || state == "retired" {
				if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
					t.Fatal(err)
				}
			}
			if state == "retired" {
				if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
					t.Fatal(err)
				}
			}
			if state == "retry" || state == "corrupt_retired" || state == "ambiguous" {
				if err := RetireAbsentPeopleKey(dir, id, instance, string(author), pending.Key); err != nil {
					t.Fatal(err)
				}
			}
			switch state {
			case "wrong_copy":
				instance = strings.Repeat("0", 32)
			case "wrong_author":
				author = identity.InstallationID(strings.Repeat("0", 32))
			case "missing_installation":
				if err := os.Remove(filepath.Join(dir, identity.InstallationFile)); err != nil {
					t.Fatal(err)
				}
			case "corrupt_installation":
				if err := os.WriteFile(filepath.Join(dir, identity.InstallationFile), []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing_copy":
				if err := os.Remove(filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-identity", "0000000000000000.json")); err != nil {
					t.Fatal(err)
				}
			case "corrupt_retired":
				if err := os.WriteFile(filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-pending", pending.Key+".retired"), []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			case "ambiguous":
				root, err := peopleRecordDir(dir, id, false, "people-pending")
				if err != nil {
					t.Fatal(err)
				}
				data, _ := json.Marshal(stage)
				if err := root.WriteFile(pending.Key+".json", data, 0600); err != nil {
					t.Fatal(err)
				}
				root.Close()
			}
			err = RetireAbsentPeopleKey(dir, id, instance, string(author), pending.Key)
			if state == "absent" || state == "retry" {
				if err != nil {
					t.Fatal(err)
				}
			} else if state == "pending" || state == "retired" {
				if !errors.Is(err, ErrPeopleReviewRequired) {
					t.Fatalf("want review: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid state accepted")
			}
			if state == "pending" {
				if _, err := ReadPeoplePending(dir, id, pending.Key); err != nil {
					t.Fatalf("pending lost: %v", err)
				}
				if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
					t.Fatal("pin lost")
				}
			}
		})
	}
}

func TestAbsentRetirementGlobalBudgetAndLinkPeak(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	author, err := identity.LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	key := "0123456789abcdef"
	marker := absentRetirement{Version: 1, Key: key, Author: string(author), CopyInstance: instance}
	raw, _ := json.Marshal(marker)
	hash := sha256.Sum256(raw)
	envelope := PeopleStage{ID: key, Hash: hex.EncodeToString(hash[:]), Name: "retired-absent.json", MIME: "application/json", ConfigHash: strings.Repeat("0", 64), Data: raw}
	encoded, _ := json.Marshal(envelope)
	before, err := storagebudget.Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	setLimit := func(limit int64) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(fmt.Sprintf(`{"storage_min_free_bytes":0,"storage_global_limit_bytes":%d}`, limit)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	setLimit(before.UsedBytes + int64(2*len(encoded)) - 1)
	if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("peak refusal: %v", err)
	}
	setLimit(before.UsedBytes + int64(2*len(encoded)))
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	var peak int64
	peopleRecordIO.link = func(root *os.Root, old, name string) error {
		if err := original.link(root, old, name); err != nil {
			return err
		}
		status, err := storagebudget.Inspect(dir)
		if err != nil {
			return err
		}
		peak = status.UsedBytes
		return nil
	}
	if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err != nil {
		t.Fatal(err)
	}
	peopleRecordIO = original
	if peak != before.UsedBytes+int64(2*len(encoded)) {
		t.Fatalf("measured link peak %d, expected %d", peak, before.UsedBytes+int64(2*len(encoded)))
	}
	setLimit(1)
	if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err != nil {
		t.Fatalf("non-growing full retry: %v", err)
	}
	if err := RetireAbsentPeopleKey(dir, id, instance, string(author), "fedcba9876543210"); !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("full new key: %v", err)
	}
}

func TestAbsentRetirementCopyRecordPeak(t *testing.T) {
	for _, total := range []int{4094, 4095} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			author, err := identity.LoadOrCreateInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			root, err := peopleRecordDir(dir, id, true, "people-records")
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < total-1; i++ {
				if err := root.WriteFile(fmt.Sprintf("%016x.json", i), []byte("x"), 0600); err != nil {
					root.Close()
					t.Fatal(err)
				}
			}
			root.Close()
			err = RetireAbsentPeopleKey(dir, id, instance, string(author), "fedcba9876543210")
			if total == 4094 && err != nil {
				t.Fatalf("4094: %v", err)
			}
			if total == 4095 && !errors.Is(err, storagebudget.ErrBudget) {
				t.Fatalf("4095: %v", err)
			}
		})
	}
}

func TestAbsentRetirementReadersRejectChangedBindings(t *testing.T) {
	for _, mode := range []string{"missing_copy", "changed_copy", "missing_author", "changed_author", "corrupt_author"} {
		t.Run(mode, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			author, err := identity.LoadOrCreateInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			key := "0123456789abcdef"
			if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing_copy", "changed_copy":
				p := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-identity", "0000000000000000.json")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if mode == "changed_copy" {
					if _, err := EnsurePeopleCopyInstance(dir, id); err == nil {
						t.Fatal("protected identity recreated")
					}
				}
			case "missing_author":
				if err := os.Remove(filepath.Join(dir, identity.InstallationFile)); err != nil {
					t.Fatal(err)
				}
			case "changed_author":
				if err := os.WriteFile(filepath.Join(dir, identity.InstallationFile), []byte(`{"v":1,"id":"00000000000000000000000000000000"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "corrupt_author":
				if err := os.WriteFile(filepath.Join(dir, identity.InstallationFile), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ReadPeoplePending(dir, id, key); err == nil || errors.Is(err, ErrPeopleStageMissing) {
				t.Fatalf("changed binding read: %v", err)
			}
			if _, err := ReadPeopleCompletion(dir, id, key); err == nil {
				t.Fatal("changed binding completion")
			}
		})
	}
}

func TestAbsentRetirementConflictingRecords(t *testing.T) {
	for _, mode := range []string{"invalid_args", "invalid_tuple", "pending_corrupt", "retired_invalid_body", "retired_foreign", "retired_ambiguous", "completed", "completed_corrupt", "retired_completed"} {
		t.Run(mode, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			author, err := identity.LoadOrCreateInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			key := "0123456789abcdef"
			writeRecord := func(tree, suffix string, record PeopleStage) {
				t.Helper()
				root, err := peopleRecordDir(dir, id, true, tree)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				if err := root.WriteFile(key+suffix, raw, 0600); err != nil {
					t.Fatal(err)
				}
				root.Close()
			}
			makeRecord := func(name string, data []byte) PeopleStage {
				sum := sha256.Sum256(data)
				return PeopleStage{ID: key, Hash: hex.EncodeToString(sum[:]), Name: name, MIME: "application/json", ConfigHash: strings.Repeat("0", 64), Data: data}
			}
			switch mode {
			case "invalid_args":
				if err := RetireAbsentPeopleKey(dir, id, "bad", string(author), key); err == nil {
					t.Fatal("invalid instance")
				}
				if err := RetireAbsentPeopleKey(dir, id, instance, "bad", key); err == nil {
					t.Fatal("invalid author")
				}
				key = "bad"
			case "invalid_tuple":
				id = CopyID{}
			case "pending_corrupt":
				root, err := peopleRecordDir(dir, id, true, "people-pending")
				if err != nil {
					t.Fatal(err)
				}
				if err := root.WriteFile(key+".json", []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
				root.Close()
			case "retired_invalid_body":
				writeRecord("people-pending", ".retired", makeRecord("retired-absent.json", []byte("{}")))
			case "retired_foreign":
				raw, _ := json.Marshal(absentRetirement{Version: 1, Key: key, Author: strings.Repeat("0", 32), CopyInstance: instance})
				writeRecord("people-pending", ".retired", makeRecord("retired-absent.json", raw))
			case "retired_ambiguous":
				if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err != nil {
					t.Fatal(err)
				}
				writeRecord("people-pending", ".json", makeRecord("pending.json", []byte("{}")))
			case "completed", "completed_corrupt":
				if mode == "completed" {
					writeRecord("people-completed", ".json", makeRecord("completion.json", []byte("{}")))
				} else {
					root, err := peopleRecordDir(dir, id, true, "people-completed")
					if err != nil {
						t.Fatal(err)
					}
					if err := root.WriteFile(key+".json", []byte("broken"), 0600); err != nil {
						t.Fatal(err)
					}
					root.Close()
				}
			case "retired_completed":
				if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err != nil {
					t.Fatal(err)
				}
				writeRecord("people-completed", ".json", makeRecord("completion.json", []byte("{}")))
			}
			err = RetireAbsentPeopleKey(dir, id, instance, string(author), key)
			if mode == "completed" {
				if !errors.Is(err, ErrPeopleReviewRequired) {
					t.Fatalf("completion requires review: %v", err)
				}
			} else if err == nil {
				t.Fatalf("%s accepted", mode)
			}
		})
	}
}

func TestAbsentRetirementSurvivesOtherProcess(t *testing.T) {
	if dir := os.Getenv("SCIMUX_TEST_ABSENT_DIR"); dir != "" {
		id := sampleCopyID()
		author, err := identity.LoadInstallation(dir)
		if err != nil {
			t.Fatal(err)
		}
		stage := syntheticPeopleStage()
		pending := syntheticPeoplePending(stage)
		pending.Author = string(author)
		if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
			t.Fatal("retired key admitted across process")
		}
		return
	}
	dir, id := t.TempDir(), sampleCopyID()
	author, err := identity.LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	if err := RetireAbsentPeopleKey(dir, id, instance, string(author), pending.Key); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAbsentRetirementSurvivesOtherProcess$", "-test.timeout=15s")
	cmd.Env = append(os.Environ(), "SCIMUX_TEST_ABSENT_DIR="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, output)
	}
	if err := RetireAbsentPeopleKey(dir, id, instance, string(author), pending.Key); err != nil {
		t.Fatalf("retry after child: %v", err)
	}
	pending.Key = "0123456789abcdef"
	pending.Author = string(author)
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatalf("distinct key: %v", err)
	}
}

func TestAbsentRetirementRetryBarrierFailures(t *testing.T) {
	for _, mode := range []string{"pending_directory", "retry_open", "retry_sync"} {
		t.Run(mode, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			author, err := identity.LoadOrCreateInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			key := "0123456789abcdef"
			if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err != nil {
				t.Fatal(err)
			}
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			injected := false
			pendingDir := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-pending")
			if mode == "pending_directory" {
				if err := os.Chmod(pendingDir, 0755); err != nil {
					t.Fatal(err)
				}
				injected = true
				t.Cleanup(func() { _ = os.Chmod(pendingDir, 0700) })
			}

			if mode == "retry_open" {
				peopleRecordIO.openDir = func(root *os.Root) (*os.File, error) {
					if strings.HasSuffix(root.Name(), "people-pending") && !injected {
						injected = true
						return nil, errors.New("synthetic open")
					}
					return original.openDir(root)
				}
			}
			if mode == "retry_sync" {
				peopleRecordIO.sync = func(f *os.File) error {
					if strings.HasSuffix(filepath.Clean(f.Name()), "people-pending") && !injected {
						injected = true
						return errors.New("synthetic sync")
					}
					return original.sync(f)
				}
			}
			if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err == nil || !injected {
				t.Fatalf("fault not reported: %v", err)
			}
			peopleRecordIO = original
			if mode == "pending_directory" {
				if err := os.Chmod(pendingDir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err != nil {
				t.Fatalf("repair: %v", err)
			}
		})
	}
}

func TestAbsentRetirementAdmissionWinsAcrossProcesses(t *testing.T) {
	if dir := os.Getenv("SCIMUX_TEST_ABSENT_RACE_DIR"); dir != "" {
		id := sampleCopyID()
		author, err := identity.LoadInstallation(dir)
		if err != nil {
			t.Fatal(err)
		}
		instance, err := ReadPeopleCopyInstance(dir, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "race-ready"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		pending := syntheticPeoplePending(syntheticPeopleStage())
		if err := RetireAbsentPeopleKey(dir, id, instance, string(author), pending.Key); !errors.Is(err, ErrPeopleReviewRequired) {
			t.Fatalf("admission did not win: %v", err)
		}
		return
	}
	dir, id := t.TempDir(), sampleCopyID()
	author, err := identity.LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	pending.Author = string(author)
	original := peopleStageAfterWrite
	defer func() { peopleStageAfterWrite = original }()
	var child *exec.Cmd
	waited := false
	t.Cleanup(func() {
		if child != nil && !waited {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	var output bytes.Buffer
	peopleStageAfterWrite = func() {
		child = exec.Command(os.Args[0], "-test.run=^TestAbsentRetirementAdmissionWinsAcrossProcesses$", "-test.timeout=15s")
		child.Env = append(os.Environ(), "SCIMUX_TEST_ABSENT_RACE_DIR="+dir)
		child.Stdout = &output
		child.Stderr = &output
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(filepath.Join(dir, "race-ready")); err == nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("child did not reach admission race")
	}
	if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	peopleStageAfterWrite = original
	if child == nil {
		t.Fatal("child not started")
	}
	err = child.Wait()
	waited = true
	if err != nil {
		t.Fatalf("child: %v %s", err, output.String())
	}
	if _, err := ReadPeoplePending(dir, id, pending.Key); err != nil {
		t.Fatalf("pending lost: %v", err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("pending pin lost")
	}
}

func TestAbsentRetirementRequiresReviewOfCompletedReceipt(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	author, err := identity.LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := discardTestPending(t, stage)
	pending.Author = string(author)
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
	if err := RetireAbsentPeopleKey(dir, id, instance, string(author), pending.Key); !errors.Is(err, ErrPeopleReviewRequired) {
		t.Fatalf("receipt review: %v", err)
	}
	receipt, err := ReadPeopleCompletion(dir, id, pending.Key)
	if err != nil || receipt.Seq != 1 {
		t.Fatalf("receipt lost: %+v %v", receipt, err)
	}
}

func TestAbsentRetirementWinsAcrossProcesses(t *testing.T) {
	if dir := os.Getenv("SCIMUX_TEST_ABSENT_CANCEL_DIR"); dir != "" {
		id := sampleCopyID()
		author, err := identity.LoadInstallation(dir)
		if err != nil {
			t.Fatal(err)
		}
		stage := syntheticPeopleStage()
		pending := syntheticPeoplePending(stage)
		pending.Author = string(author)
		lock, err := os.OpenFile(filepath.Join(dir, ".storage.lock"), os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		probe := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if probe == nil {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			lock.Close()
			t.Fatal("retirement did not hold budget lock")
		}
		if !errors.Is(probe, syscall.EWOULDBLOCK) {
			lock.Close()
			t.Fatal(probe)
		}
		lock.Close()
		if err := os.WriteFile(filepath.Join(dir, "cancel-ready"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := SavePeoplePending(dir, id, pending, []PeopleStage{stage}); err == nil {
			t.Fatal("admission won after retirement")
		}
		return
	}
	dir, id := t.TempDir(), sampleCopyID()
	author, err := identity.LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	stage := syntheticPeopleStage()
	if err := SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := syntheticPeoplePending(stage)
	original := peopleStageAfterWrite
	defer func() { peopleStageAfterWrite = original }()
	var child *exec.Cmd
	var output bytes.Buffer
	waited := false
	t.Cleanup(func() {
		if child != nil && !waited {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	peopleStageAfterWrite = func() {
		child = exec.Command(os.Args[0], "-test.run=^TestAbsentRetirementWinsAcrossProcesses$", "-test.timeout=15s")
		child.Env = append(os.Environ(), "SCIMUX_TEST_ABSENT_CANCEL_DIR="+dir)
		child.Stdout = &output
		child.Stderr = &output
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(filepath.Join(dir, "cancel-ready")); err == nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("child did not reach admission")
	}
	if err := RetireAbsentPeopleKey(dir, id, instance, string(author), pending.Key); err != nil {
		t.Fatal(err)
	}
	peopleStageAfterWrite = original
	if child == nil {
		t.Fatal("child not started")
	}
	err = child.Wait()
	waited = true
	if err != nil {
		t.Fatalf("child: %v %s", err, output.String())
	}
	if _, err := ReadPeoplePending(dir, id, pending.Key); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("terminal read: %v", err)
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("stage lost: %v", err)
	}
}

func TestAbsentRetirementCopyBytePeak(t *testing.T) {
	for _, extra := range []int64{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			author, err := identity.LoadOrCreateInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			key := "0123456789abcdef"
			marker := absentRetirement{Version: 1, Key: key, Author: string(author), CopyInstance: instance}
			raw, _ := json.Marshal(marker)
			hash := sha256.Sum256(raw)
			encoded, _ := json.Marshal(PeopleStage{ID: key, Hash: hex.EncodeToString(hash[:]), Name: "retired-absent.json", MIME: "application/json", ConfigHash: strings.Repeat("0", 64), Data: raw})
			identityPath := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-identity", "0000000000000000.json")
			info, err := os.Stat(identityPath)
			if err != nil {
				t.Fatal(err)
			}
			root, err := peopleRecordDir(dir, id, true, "people-records")
			if err != nil {
				t.Fatal(err)
			}
			f, err := root.OpenFile("synthetic-fill", os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate((256 << 20) - info.Size() - int64(2*len(encoded)) + extra); err != nil {
				t.Fatal(err)
			}
			f.Close()
			root.Close()
			err = RetireAbsentPeopleKey(dir, id, instance, string(author), key)
			if extra == 0 && err != nil {
				t.Fatalf("exact byte peak: %v", err)
			}
			if extra == 1 && !errors.Is(err, storagebudget.ErrBudget) {
				t.Fatalf("over byte peak: %v", err)
			}
		})
	}
}

func TestAbsentRetirementRepairsVisibleMarkerAfterRestart(t *testing.T) {
	const key = "0123456789abcdef"
	if dir := os.Getenv("SCIMUX_TEST_ABSENT_REPAIR_DIR"); dir != "" {
		id := sampleCopyID()
		author, err := identity.LoadInstallation(dir)
		if err != nil {
			t.Fatal(err)
		}
		instance, err := ReadPeopleCopyInstance(dir, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir, id := t.TempDir(), sampleCopyID()
	author, err := identity.LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	linked := false
	peopleRecordIO.link = func(root *os.Root, old, name string) error {
		err := original.link(root, old, name)
		linked = err == nil
		return err
	}
	peopleRecordIO.sync = func(f *os.File) error {
		if linked {
			return errors.New("synthetic barrier failure")
		}
		return original.sync(f)
	}
	if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err == nil || !linked {
		t.Fatalf("failed barrier not observed: %v", err)
	}
	peopleRecordIO = original
	path := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-pending", key+".retired")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAbsentRetirementRepairsVisibleMarkerAfterRestart$", "-test.timeout=15s")
	cmd.Env = append(os.Environ(), "SCIMUX_TEST_ABSENT_REPAIR_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, out)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("marker changed: %v", err)
	}
}

func TestAbsentRetirementRetryWithUnrelatedCompletionDirectory(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	author, err := identity.LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	key := "0123456789abcdef"
	if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, true, "people-completed")
	if err != nil {
		t.Fatal(err)
	}
	root.Close()
	if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err != nil {
		t.Fatalf("retry with unrelated directory: %v", err)
	}
}

func TestAbsentRetirementRecoversInterruptedTemporary(t *testing.T) {
	const key = "0123456789abcdef"
	if dir := os.Getenv("SCIMUX_TEST_ABSENT_CRASH_DIR"); dir != "" {
		id := sampleCopyID()
		author, err := identity.LoadInstallation(dir)
		if err != nil {
			t.Fatal(err)
		}
		instance, err := ReadPeopleCopyInstance(dir, id)
		if err != nil {
			t.Fatal(err)
		}
		peopleStageAfterWrite = func() { os.Exit(23) }
		_ = RetireAbsentPeopleKey(dir, id, instance, string(author), key)
		t.Fatal("interruption was not reached")
	}
	dir, id := t.TempDir(), sampleCopyID()
	author, err := identity.LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAbsentRetirementRecoversInterruptedTemporary$", "-test.timeout=15s")
	cmd.Env = append(os.Environ(), "SCIMUX_TEST_ABSENT_CRASH_DIR="+dir)
	err = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("child exit: %v", err)
	}
	pendingDir := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-pending")
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !peopleStageTemp(entries[0].Name()) {
		t.Fatalf("unexpected interrupted state: %v", entries)
	}
	if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err != nil {
		t.Fatalf("retry after interruption: %v", err)
	}
	entries, err = os.ReadDir(pendingDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != key+".retired" {
		t.Fatalf("recovered entries: %v %v", entries, err)
	}
}

type absentInfoErrorEntry struct{ fs.DirEntry }

func (e absentInfoErrorEntry) Info() (fs.FileInfo, error) {
	return nil, errors.New("synthetic info failure")
}

func TestAbsentRetirementWriterRejectsUninspectableOrNonRegularRecords(t *testing.T) {
	for _, mode := range []string{"info_error", "non_regular"} {
		t.Run(mode, func(t *testing.T) {
			dir, id := t.TempDir(), sampleCopyID()
			author, err := identity.LoadOrCreateInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			stage := syntheticPeopleStage()
			if err := SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			injected := false
			if mode == "info_error" {
				peopleRecordIO.walk = func(fsys fs.FS, root string, fn fs.WalkDirFunc) error {
					return original.walk(fsys, root, func(name string, entry fs.DirEntry, err error) error {
						if err == nil && entry != nil && !entry.IsDir() && !injected {
							injected = true
							entry = absentInfoErrorEntry{entry}
						}
						return fn(name, entry, err)
					})
				}
			} else {
				root, err := peopleRecordDir(dir, id, true, "people-records")
				if err != nil {
					t.Fatal(err)
				}
				if err := root.Symlink("synthetic-missing", "synthetic-link"); err != nil {
					t.Fatal(err)
				}
				root.Close()
				injected = true
			}
			if err := RetireAbsentPeopleKey(dir, id, instance, string(author), "0123456789abcdef"); err == nil || !injected {
				t.Fatalf("unsafe record accepted: %v", err)
			}
			peopleRecordIO = original
			if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
				t.Fatalf("existing stage changed: %v", err)
			}
		})
	}
}
