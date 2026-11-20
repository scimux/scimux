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
)

func TestPeopleInventoryPagesAndTerminal(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	for i := 0; i < 18; i++ {
		next := stage
		next.ID = fmt.Sprintf("%016x", i+1)
		if err := SavePeopleStage(dir, id, next); err != nil {
			t.Fatal(err)
		}
	}
	cursor := ""
	seen := map[string]bool{}
	pages := 0
	for {
		page, err := ReadPeopleInventory(dir, id, instance, pending.Author, cursor)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(page.Entries) > 8 {
			t.Fatal("page exceeds bound")
		}
		for _, e := range page.Entries {
			if seen[e.Kind+e.ID] {
				t.Fatal("duplicate")
			}
			seen[e.Kind+e.ID] = true
			if len(e.Revision) != 64 {
				t.Fatal("revision")
			}
		}
		if page.Next == "" {
			break
		}
		if page.Next <= cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = page.Next
	}
	if pages < 3 || len(seen) != 20 || !seen["pending"+pending.Key] {
		t.Fatalf("pages=%d entries=%d", pages, len(seen))
	}
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
		t.Fatal(err)
	}
	page, err := ReadPeopleInventory(dir, id, instance, pending.Author, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range page.Entries {
		if e.Kind == "pending" {
			t.Fatal("retired pending returned")
		}
	}
}

func TestPeopleInventoryValidationAndDamage(t *testing.T) {
	dir, id, instance, _, pending := setupDiscard(t)
	for _, tc := range []struct {
		id                       CopyID
		instance, author, cursor string
	}{
		{CopyID{}, instance, pending.Author, ""}, {id, "bad", pending.Author, ""}, {id, strings.Repeat("0", 32), pending.Author, ""},
		{id, instance, "bad", ""}, {id, instance, strings.Repeat("0", 32), ""}, {id, instance, pending.Author, "x"},
		{id, instance, pending.Author, "x" + strings.Repeat("0", 16)}, {id, instance, pending.Author, "p" + strings.Repeat("z", 16)},
	} {
		if _, err := ReadPeopleInventory(dir, tc.id, tc.instance, tc.author, tc.cursor); err == nil {
			t.Fatalf("accepted invalid input: %+v", tc)
		}
	}
	root, err := peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(pending.Key+".json", []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if _, err := ReadPeopleInventory(dir, id, instance, pending.Author, ""); err == nil {
		t.Fatal("corruption hidden")
	}
	identityPath := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-identity", "0000000000000000.json")
	if err := os.Remove(identityPath); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleInventory(dir, id, instance, pending.Author, ""); err == nil {
		t.Fatal("missing identity accepted")
	}
	if _, err := os.Stat(identityPath); !os.IsNotExist(err) {
		t.Fatal("identity recreated")
	}
}

func TestPeopleInventoryEmptyCopy(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	page, err := ReadPeopleInventory(dir, id, instance, strings.Repeat("a", 32), "")
	if err != nil || len(page.Entries) != 0 || page.Next != "" {
		t.Fatalf("empty: %+v %v", page, err)
	}
}

func TestPeopleInventoryCompletionAndCorruption(t *testing.T) {
	for _, mode := range []string{"matching", "different", "broken", "missing", "retired_conflict", "bad_body"} {
		t.Run(mode, func(t *testing.T) {
			dir, id, instance, _, pending := setupDiscard(t)
			if mode == "retired_conflict" {
				if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "bad_body" {
				pending.Body = json.RawMessage(`{"text":7}`)
				raw, _ := json.Marshal(pending)
				sum := sha256.Sum256(raw)
				envelope := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(sum[:]), Name: "pending.json", MIME: "application/json", ConfigHash: pending.ConfigHash, OwnerInstance: pending.OwnerInstance, Data: raw}
				encoded, _ := json.Marshal(envelope)
				root, err := peopleRecordDir(dir, id, false, "people-pending")
				if err != nil {
					t.Fatal(err)
				}
				if err := root.WriteFile(pending.Key+".json", encoded, 0600); err != nil {
					t.Fatal(err)
				}
				root.Close()
			} else {
				root, err := peopleRecordDir(dir, id, true, "people-completed")
				if err != nil {
					t.Fatal(err)
				}
				if mode != "missing" {
					completion := PeopleCompletion{Pending: pending, Seq: 1}
					if mode == "different" {
						completion.Pending.Author = strings.Repeat("b", 32)
					}
					raw, _ := json.Marshal(completion)
					sum := sha256.Sum256(raw)
					envelope := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(sum[:]), Name: "completion.json", MIME: "application/json", ConfigHash: pending.ConfigHash, OwnerInstance: pending.OwnerInstance, Data: raw}
					encoded, _ := json.Marshal(envelope)
					if mode == "broken" {
						encoded = []byte("broken")
					}
					if err := root.WriteFile(pending.Key+".json", encoded, 0600); err != nil {
						t.Fatal(err)
					}
				}
				root.Close()
			}
			page, err := ReadPeopleInventory(dir, id, instance, pending.Author, "")
			if mode == "matching" || mode == "missing" {
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range page.Entries {
					if mode == "matching" && entry.Kind == "pending" {
						t.Fatal("completed pending returned")
					}
				}
			} else if err == nil {
				t.Fatal("damaged state accepted")
			}
		})
	}
}

func TestPeopleInventoryDirectoryFailures(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "file")
		if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPeopleInventory(file, sampleCopyID(), strings.Repeat("a", 32), strings.Repeat("b", 32), ""); err == nil {
			t.Fatal("budget failure accepted")
		}
	})
	for _, mode := range []string{"invalid_tree", "open", "read", "invalid_name", "invalid_retired", "ambiguous", "temporary", "too_many", "second_open", "completed_tree"} {
		t.Run(mode, func(t *testing.T) {
			dir, id, instance, _, pending := setupDiscard(t)
			path := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-pending")
			original := peopleRecordIO
			t.Cleanup(func() { peopleRecordIO = original })
			switch mode {
			case "invalid_tree":
				if err := os.Chmod(path, 0755); err != nil {
					t.Fatal(err)
				}
			case "open", "read":
				peopleRecordIO.openDir = func(root *os.Root) (*os.File, error) {
					if strings.HasSuffix(root.Name(), "people-pending") {
						if mode == "open" {
							return nil, errors.New("synthetic")
						}
						return os.Open(os.DevNull)
					}
					return original.openDir(root)
				}
			case "invalid_name", "invalid_retired", "temporary":
				name := map[string]string{"invalid_name": "bad.json", "invalid_retired": "bad.retired", "temporary": "0000000000000000-0000000000000000.tmp"}[mode]
				if err := os.WriteFile(filepath.Join(path, name), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			case "ambiguous":
				data, err := os.ReadFile(filepath.Join(path, pending.Key+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, pending.Key+".retired"), data, 0600); err != nil {
					t.Fatal(err)
				}
			case "too_many":
				for i := 0; i < 4097; i++ {
					if err := os.WriteFile(filepath.Join(path, fmt.Sprintf("%016x.json", i)), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "second_open":
				prior := copyOps
				t.Cleanup(func() { copyOps = prior })
				n := 0
				copyOps.openChild = func(root *os.Root, name string) (*os.Root, error) {
					if name == "people-pending" {
						n++
						if n == 2 {
							return nil, errors.New("synthetic")
						}
					}
					return prior.openChild(root, name)
				}
			case "completed_tree":
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), "people-completed"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := ReadPeopleInventory(dir, id, instance, pending.Author, "")
			if mode == "temporary" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatalf("%s accepted", mode)
			}
		})
	}
}

func TestPeopleInventoryExactRevisionsAndRecreatedCopy(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	page, err := ReadPeopleInventory(dir, id, instance, pending.Author, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(pending)
	sum := sha256.Sum256(raw)
	wantPending := hex.EncodeToString(sum[:])
	raw, _ = json.Marshal(stage)
	sum = sha256.Sum256(raw)
	wantStage := hex.EncodeToString(sum[:])
	if len(page.Entries) != 2 || page.Entries[0].Revision != wantPending || page.Entries[1].Revision != wantStage || page.Entries[1].Size != len(stage.Data) {
		t.Fatalf("wrong projection: %+v", page)
	}
	if err := RemoveCopy(dir, id); err != nil {
		t.Fatal(err)
	}
	replacement, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == instance {
		t.Fatal("copy identity reused")
	}
	if _, err := ReadPeopleInventory(dir, id, instance, pending.Author, ""); err == nil {
		t.Fatal("old copy accepted")
	}
	page, err = ReadPeopleInventory(dir, id, replacement, pending.Author, "")
	if err != nil || len(page.Entries) != 0 {
		t.Fatalf("new copy: %+v %v", page, err)
	}
}

func TestPeopleInventoryLegacyTextOnly(t *testing.T) {
	dir, id := t.TempDir(), sampleCopyID()
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/0123456789abcdef", Author: id.Owner, At: "2026-01-01T00:00:00.000Z", Body: []byte(`{"v":1}`)}}); err != nil {
		t.Fatal(err)
	}
	instance, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	page, err := ReadPeopleInventory(dir, id, instance, id.Owner, "")
	if err != nil || len(page.Entries) != 0 {
		t.Fatalf("legacy: %+v %v", page, err)
	}
}

func TestPeopleInventoryDecodedMetadataConditions(t *testing.T) {
	for _, mode := range []string{"invalid_pending", "foreign_author", "invalid_receipt", "mismatched_receipt", "matching_receipt"} {
		t.Run(mode, func(t *testing.T) {
			dir, id, instance, _, pending := setupDiscard(t)
			author := pending.Author
			tree := "people-pending"
			value := any(pending)
			name := "pending.json"
			switch mode {
			case "invalid_pending":
				pending.StageIDs = nil
				value = pending
			case "foreign_author":
				pending.Author = strings.Repeat("b", 32)
				value = pending
			default:
				tree = "people-completed"
				name = "completion.json"
				receipt := PeopleCompletion{Pending: pending, Seq: 1}
				if mode == "invalid_receipt" {
					receipt.Seq = 0
				}
				if mode == "mismatched_receipt" {
					receipt.Pending.Author = strings.Repeat("b", 32)
				}
				value = receipt
			}
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(data)
			envelope := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(digest[:]), Name: name, MIME: "application/json", ConfigHash: pending.ConfigHash, OwnerInstance: pending.OwnerInstance, Data: data}
			if !validPeopleStage(envelope) {
				t.Fatal("test envelope is not valid")
			}
			if mode == "invalid_pending" {
				if _, err := decodePeoplePending(envelope); err == nil {
					t.Fatal("invalid pending decoded")
				}
			}
			if mode == "invalid_receipt" {
				if _, err := decodePeopleCompletion(envelope, pending.Key); err == nil {
					t.Fatal("invalid receipt decoded")
				}
			}
			raw, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			root, err := peopleRecordDir(dir, id, true, tree)
			if err != nil {
				t.Fatal(err)
			}
			if err := root.WriteFile(pending.Key+".json", raw, 0600); err != nil {
				t.Fatal(err)
			}
			root.Close()
			page, err := ReadPeopleInventory(dir, id, instance, author, "")
			if mode == "matching_receipt" {
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range page.Entries {
					if entry.Kind == "pending" {
						t.Fatal("completed publication returned")
					}
				}
			} else if err == nil {
				t.Fatalf("%s accepted", mode)
			}
		})
	}
}

func TestPeopleInventoryEntryTypes(t *testing.T) {
	for _, mode := range []string{"pending_json_symlink", "pending_retired_symlink", "stage_json_symlink", "pending_json_directory", "pending_retired_directory", "unsupported_suffix", "temporary"} {
		t.Run(mode, func(t *testing.T) {
			dir, id, instance, _, pending := setupDiscard(t)
			tree := "people-pending"
			if mode == "stage_json_symlink" {
				tree = "people-stages"
			}
			base := filepath.Join(dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, tree)
			name := "0011223344556677.json"
			if strings.Contains(mode, "retired") {
				name = "0011223344556677.retired"
			}
			if mode == "unsupported_suffix" {
				name = "0011223344556677.other"
			}
			if mode == "temporary" {
				name = "0011223344556677-0011223344556677.tmp"
			}
			path := filepath.Join(base, name)
			switch {
			case strings.HasSuffix(mode, "symlink"):
				if err := os.Symlink("missing", path); err != nil {
					t.Fatal(err)
				}
			case strings.HasSuffix(mode, "directory"):
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := ReadPeopleInventory(dir, id, instance, pending.Author, "")
			if mode == "temporary" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid entry accepted")
			}
		})
	}
}

func TestPeopleInventoryEmptyDirectoriesAndTerminalPages(t *testing.T) {
	t.Run("empty_directories", func(t *testing.T) {
		dir, id := t.TempDir(), sampleCopyID()
		instance, err := EnsurePeopleCopyInstance(dir, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"people-pending", "people-stages"} {
			root, err := peopleRecordDir(dir, id, true, name)
			if err != nil {
				t.Fatal(err)
			}
			root.Close()
		}
		page, err := ReadPeopleInventory(dir, id, instance, strings.Repeat("a", 32), "")
		if err != nil || len(page.Entries) != 0 || page.Next != "" {
			t.Fatalf("empty directories: %+v %v", page, err)
		}
	})
	t.Run("terminal_pages", func(t *testing.T) {
		dir, id, instance, stage, pending := setupDiscard(t)
		for i := 0; i < 17; i++ {
			old := pending
			old.Key = fmt.Sprintf("%016x", i+1)
			if err := SavePeoplePending(dir, id, old, []PeopleStage{stage}); err != nil {
				t.Fatal(err)
			}
			if _, err := DiscardPeoplePending(dir, id, instance, old); err != nil {
				t.Fatal(err)
			}
		}
		cursor := ""
		empty := 0
		seen := map[string]bool{}
		for {
			page, err := ReadPeopleInventory(dir, id, instance, pending.Author, cursor)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Entries) == 0 && page.Next != "" {
				empty++
			}
			for _, entry := range page.Entries {
				key := entry.Kind + entry.ID
				if seen[key] {
					t.Fatal("duplicate entry")
				}
				seen[key] = true
			}
			if page.Next == "" {
				break
			}
			if page.Next <= cursor {
				t.Fatal("cursor stalled")
			}
			cursor = page.Next
		}
		if empty < 2 || len(seen) != 2 || !seen["pending"+pending.Key] || !seen["stage"+stage.ID] {
			t.Fatalf("empty pages=%d entries=%v", empty, seen)
		}
	})
}
