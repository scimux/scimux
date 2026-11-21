package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestLocalPublicationReaderStateAndAmbiguity(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	read := func(key string) (PeoplePending, error) { return ReadPeopleLocalPublication(dir, id, instance, key) }
	if got, err := read(pending.Key); err != nil || !samePendingValue(got, pending) {
		t.Fatalf("pending: %v", err)
	}
	if _, err := read("0011223344556677"); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := ReadPeopleLocalPublication(dir, id, "00000000000000000000000000000000", pending.Key); err == nil {
		t.Fatal("wrong copy accepted")
	}
	if _, err := ReadPeopleLocalPublication(dir, id, instance, "bad"); err == nil {
		t.Fatal("bad key accepted")
	}
	if _, err := ReadPeopleLocalPublication(dir, id, "bad", pending.Key); err == nil {
		t.Fatal("bad copy instance accepted")
	}
	if _, err := ReadPeopleLocalPublication(dir, CopyID{}, instance, pending.Key); err == nil {
		t.Fatal("bad tuple accepted")
	}
	root, err := peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := root.ReadFile(pending.Key + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(pending.Key+".retired", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := read(pending.Key); err == nil {
		t.Fatal("ambiguous retirement accepted")
	}
	if err := root.Remove(pending.Key + ".retired"); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
		t.Fatal(err)
	}
	if got, err := read(pending.Key); err != nil || !samePendingValue(got, pending) {
		t.Fatalf("retired: %v", err)
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("stage lost: %v", err)
	}
}

func TestLocalPublicationReaderRejectsCorruptRetirement(t *testing.T) {
	dir, id, instance, _, pending := setupDiscard(t)
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
		t.Fatal(err)
	}
	root, err := peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(pending.Key+".retired", []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if _, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); err == nil {
		t.Fatal("corrupt retirement accepted")
	}
	root, err = peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"key":"invalid"}`)
	sum := sha256.Sum256(data)
	envelope := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(sum[:]), Name: "pending.json", MIME: "application/json", ConfigHash: pending.ConfigHash, OwnerInstance: pending.OwnerInstance, Data: data}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(pending.Key+".retired", raw, 0600); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if _, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); err == nil {
		t.Fatal("invalid retired payload accepted")
	}
}

func TestLocalPublicationReaderRepairsRetiredBarrier(t *testing.T) {
	dir, id, instance, _, pending := setupDiscard(t)
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	peopleRecordIO.openDir = func(root *os.Root) (*os.File, error) {
		if strings.HasSuffix(root.Name(), "people-pending") {
			return nil, errors.New("synthetic barrier failure")
		}
		return original.openDir(root)
	}
	if _, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); err == nil {
		t.Fatal("failed barrier accepted")
	}
	peopleRecordIO = original
	if _, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); err != nil {
		t.Fatalf("barrier retry: %v", err)
	}
}

func TestLocalPublicationReaderRejectsDamagedStorage(t *testing.T) {
	t.Run("budget lock", func(t *testing.T) {
		file := t.TempDir() + "/file"
		if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPeopleLocalPublication(file, sampleCopyID(), "00000000000000000000000000000000", "0123456789abcdef"); err == nil {
			t.Fatal("invalid storage accepted")
		}
	})
	t.Run("missing pending directory", func(t *testing.T) {
		dir, id, instance, _, _ := setupDiscard(t)
		root, err := peopleRecordDir(dir, id, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := root.Rename("people-pending", "other"); err != nil {
			t.Fatal(err)
		}
		root.Close()
		if _, err := ReadPeopleLocalPublication(dir, id, instance, "0123456789abcdef"); err == nil {
			t.Fatal("missing directory accepted")
		}
	})
	t.Run("missing copy identity", func(t *testing.T) {
		dir, id, instance, _, pending := setupDiscard(t)
		root, err := peopleRecordDir(dir, id, false, "people-identity")
		if err != nil {
			t.Fatal(err)
		}
		if err := root.Remove("0000000000000000.json"); err != nil {
			t.Fatal(err)
		}
		root.Close()
		if _, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); err == nil {
			t.Fatal("missing copy identity accepted")
		}
		if _, err := ReadPeopleCopyInstance(dir, id); err == nil {
			t.Fatal("reader created a copy identity")
		}
	})
	t.Run("corrupt pending", func(t *testing.T) {
		dir, id, instance, _, pending := setupDiscard(t)
		root, err := peopleRecordDir(dir, id, false, "people-pending")
		if err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(pending.Key+".json", []byte("broken"), 0600); err != nil {
			t.Fatal(err)
		}
		root.Close()
		if _, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); err == nil {
			t.Fatal("corrupt pending accepted")
		}
	})
	t.Run("retired sync", func(t *testing.T) {
		dir, id, instance, _, pending := setupDiscard(t)
		if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
			t.Fatal(err)
		}
		original := peopleRecordIO
		t.Cleanup(func() { peopleRecordIO = original })
		peopleRecordIO.sync = func(f *os.File) error {
			if strings.Contains(f.Name(), "people-pending") {
				return errors.New("synthetic sync failure")
			}
			return original.sync(f)
		}
		if _, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); err == nil {
			t.Fatal("failed barrier accepted")
		}
	})
}

func TestLocalPublicationReaderRejectsInvalidEnvelopePayload(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	root, err := peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := root.ReadFile(pending.Key + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope PeopleStage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Data = []byte(`{"key":"invalid"}`)
	sum := sha256.Sum256(envelope.Data)
	envelope.Hash = hex.EncodeToString(sum[:])
	raw, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(pending.Key+".json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if _, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); err == nil {
		t.Fatal("invalid pending payload accepted")
	}
	root, err = peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	kept, err := root.ReadFile(pending.Key + ".json")
	root.Close()
	if err != nil || string(kept) != string(raw) {
		t.Fatal("refusal changed the pending envelope")
	}
	if got, err := ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, stage.Data) {
		t.Fatal("refusal changed staged bytes")
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("corrupt pending allowed stage removal")
	}
}

func TestLocalPublicationReaderRejectsConflictingReceipt(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	pendingRoot, err := peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	pendingRaw, err := pendingRoot.ReadFile(pending.Key + ".json")
	if err != nil {
		t.Fatal(err)
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
	if got, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); err != nil || !samePendingValue(got, pending) {
		t.Fatalf("completed-only state: %v", err)
	}
	if err := pendingRoot.WriteFile(pending.Key+".retired", pendingRaw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); err == nil {
		t.Fatal("retired record and receipt accepted together")
	}
	if _, err := ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key); err == nil {
		t.Fatal("state read accepted retirement and receipt together")
	}
	if err := pendingRoot.Remove(pending.Key + ".retired"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleLocalPublication(dir, id, instance, "0011223344556677"); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("missing receipt: %v", err)
	}
	if err := pendingRoot.WriteFile(pending.Key+".json", pendingRaw, 0600); err != nil {
		t.Fatal(err)
	}
	pendingRoot.Close()
	if got, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); err != nil || !samePendingValue(got, pending) {
		t.Fatalf("matching pending and receipt: %v", err)
	}
	completedRoot, err := peopleRecordDir(dir, id, false, "people-completed")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := completedRoot.ReadFile(pending.Key + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope PeopleStage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var receipt PeopleCompletion
	if err := json.Unmarshal(envelope.Data, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Pending.Body = json.RawMessage(`{"v":2}`)
	envelope.Data, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(envelope.Data)
	envelope.Hash = hex.EncodeToString(sum[:])
	raw, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := completedRoot.WriteFile(pending.Key+".json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	completedRoot.Close()
	if _, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); err == nil {
		t.Fatal("conflicting receipt accepted")
	}
	if _, err := ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key); err == nil {
		t.Fatal("state read accepted conflicting receipt")
	}
	completedRoot, err = peopleRecordDir(dir, id, false, "people-completed")
	if err != nil {
		t.Fatal(err)
	}
	if err := completedRoot.WriteFile(pending.Key+".json", []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	completedRoot.Close()
	if _, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); err == nil {
		t.Fatal("damaged receipt accepted")
	}
	if _, err := ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key); err == nil {
		t.Fatal("state read accepted damaged receipt")
	}
	completedRoot, err = peopleRecordDir(dir, id, false, "people-completed")
	if err != nil {
		t.Fatal(err)
	}
	envelope.Data = []byte(`{"seq":0}`)
	sum = sha256.Sum256(envelope.Data)
	envelope.Hash = hex.EncodeToString(sum[:])
	raw, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := completedRoot.WriteFile(pending.Key+".json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	completedRoot.Close()
	if _, err := ReadPeopleLocalPublication(dir, id, instance, pending.Key); err == nil {
		t.Fatal("invalid receipt payload accepted")
	}
}
