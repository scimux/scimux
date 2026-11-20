package team

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/identity"
)

func TestLocalPublicationStateAbsentAuthor(t *testing.T) {
	dir, id, instance, _, pending := setupDiscard(t)
	author, err := identity.LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "0011223344556677"
	if err := RetireAbsentPeopleKey(dir, id, instance, string(author), key); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleLocalPublicationState(dir, id, instance, strings.Repeat("0", 32), key); err == nil {
		t.Fatal("foreign installation accepted for absent retirement")
	}
	got, err := ReadPeopleLocalPublicationState(dir, id, instance, string(author), key)
	if err != nil || got.State != "absent_retired" {
		t.Fatalf("marker: %+v %v", got, err)
	}
	if _, err := ReadPeopleLocalPublication(dir, id, instance, key); !errors.Is(err, ErrPeopleStageMissing) {
		t.Fatalf("old reader semantics: %v", err)
	}
	if _, err := ReadPeopleLocalPublicationState(dir, id, instance, strings.Repeat("0", 32), pending.Key); err == nil {
		t.Fatal("foreign pending author accepted")
	}
}

func TestLocalPublicationStatePendingRetiredAndMissing(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	read := func(key string) (PeopleLocalPublicationState, error) {
		return ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, key)
	}
	got, err := read(pending.Key)
	if err != nil || got.State != "pending" || got.Revision != peopleInventoryRevision(pending) || len(got.StageIDs) != 1 || got.StageIDs[0] != stage.ID || got.Seq != 0 {
		t.Fatalf("pending: %+v %v", got, err)
	}
	missing, err := read("0011223344556677")
	if err != nil || missing.State != "missing" {
		t.Fatalf("missing: %+v %v", missing, err)
	}
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
		t.Fatal(err)
	}
	got, err = read(pending.Key)
	if err != nil || got.State != "retired" || got.Revision != peopleInventoryRevision(pending) {
		t.Fatalf("retired: %+v %v", got, err)
	}
	if _, err := ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatal("read removed stage")
	}
}

func TestLocalPublicationStateRefusesInvalidAddressesAndUnreadableBody(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	for _, tc := range []struct {
		id                    CopyID
		instance, author, key string
	}{
		{id, instance, "bad", pending.Key}, {id, "bad", pending.Author, pending.Key}, {id, instance, pending.Author, "bad"}, {CopyID{}, instance, pending.Author, pending.Key},
		{id, strings.Repeat("0", 32), pending.Author, pending.Key},
	} {
		if _, err := ReadPeopleLocalPublicationState(dir, tc.id, tc.instance, tc.author, tc.key); err == nil {
			t.Fatal("invalid address accepted")
		}
	}
	other := pending
	other.Key = "0011223344556677"
	other.Body = []byte(`"synthetic"`)
	if err := SavePeoplePending(dir, id, other, []PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleLocalPublicationState(dir, id, instance, other.Author, other.Key); err == nil {
		t.Fatal("unprojectable body accepted")
	}
}

func TestLocalPublicationStateRetirementBarrierFailureAndRetry(t *testing.T) {
	dir, id, instance, _, pending := setupDiscard(t)
	if _, err := DiscardPeoplePending(dir, id, instance, pending); err != nil {
		t.Fatal(err)
	}
	original := peopleRecordIO
	t.Cleanup(func() { peopleRecordIO = original })
	peopleRecordIO.sync = func(file *os.File) error {
		if strings.Contains(file.Name(), "people-pending") {
			return errors.New("synthetic sync failure")
		}
		return original.sync(file)
	}
	if _, err := ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key); err == nil {
		t.Fatal("failed barrier accepted")
	}
	peopleRecordIO = original
	if got, err := ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key); err != nil || got.State != "retired" {
		t.Fatalf("retry: %+v %v", got, err)
	}
}

func TestLocalPublicationStateRefusesAmbiguousAndCorruptRecords(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	root, err := peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	raw, err := root.ReadFile(pending.Key + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(pending.Key+".retired", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key); err == nil {
		t.Fatal("ambiguous pending and retirement accepted")
	}
	if err := root.Remove(pending.Key + ".retired"); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(pending.Key+".json", []byte("synthetic corruption"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key); err == nil {
		t.Fatal("corrupt pending accepted")
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("corrupt pending allowed unpin")
	}
}

func TestLocalPublicationStateValidatesReceiptWithoutReleasingPins(t *testing.T) {
	dir, id, instance, stage, pending := setupDiscard(t)
	root, err := peopleRecordDir(dir, id, false, "people-pending")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	raw, err := root.ReadFile(pending.Key + ".json")
	if err != nil {
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
	if err := root.WriteFile(pending.Key+".json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key)
	if err != nil || got.State != "completed" || got.Seq != 1 || !got.PendingPinned {
		t.Fatalf("pinned completion: %+v %v", got, err)
	}
	if err := DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("state read released pin")
	}
	retained, err := peopleRecordDir(dir, id, false, "people-records", "comments", "1")
	if err != nil {
		t.Fatal(err)
	}
	defer retained.Close()
	saved, err := retained.ReadFile(stage.ID + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if err := retained.Remove(stage.ID + ".json"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key); err == nil {
		t.Fatal("pinned bytes missing")
	}
	if err := root.Remove(pending.Key + ".json"); err != nil {
		t.Fatal(err)
	}
	got, err = ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key)
	if err != nil || got.State != "completed" || got.Seq != 1 || got.PendingPinned {
		t.Fatalf("unpinned missing bytes: %+v %v", got, err)
	}
	if err := retained.WriteFile(stage.ID+".json", []byte("synthetic corruption"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key); err == nil {
		t.Fatal("corrupt retained record accepted")
	}
	if err := retained.WriteFile(stage.ID+".json", saved, 0600); err != nil {
		t.Fatal(err)
	}
	got, err = ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key)
	if err != nil || got.Seq != 1 {
		t.Fatalf("valid retained record: %+v %v", got, err)
	}
	path, err := copyLogPath(dir, id, CommentsLog)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key); err == nil {
		t.Fatal("missing retained comment accepted")
	}
}

func TestLocalPublicationStateRejectsRecreatedCopy(t *testing.T) {
	dir, id, instance, _, pending := setupDiscard(t)
	if err := RemoveCopy(dir, id); err != nil {
		t.Fatal(err)
	}
	replacement, err := EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == instance {
		t.Fatal("replacement copy reused identity")
	}
	if _, err := ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key); err == nil {
		t.Fatal("old copy identity accepted")
	}
	got, err := ReadPeopleLocalPublicationState(dir, id, replacement, pending.Author, pending.Key)
	if err != nil || got.State != "missing" {
		t.Fatalf("replacement: %+v %v", got, err)
	}
}

func TestLocalPublicationStateRefusesUnreadablePendingDirectory(t *testing.T) {
	dir, id, instance, _, pending := setupDiscard(t)
	root, err := peopleRecordDir(dir, id, false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Rename("people-pending", "saved-pending"); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("people-pending", []byte("synthetic corruption"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeopleLocalPublicationState(dir, id, instance, pending.Author, pending.Key); err == nil {
		t.Fatal("unreadable pending directory accepted")
	}
}
