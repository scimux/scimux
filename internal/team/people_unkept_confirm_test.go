package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func publishUnkeptFixture(t *testing.T, stages []PeopleStage, logBody json.RawMessage) (string, CopyID, PeoplePending) {
	t.Helper()
	dir, id := t.TempDir(), sampleCopyID()
	ids := make([]string, len(stages))
	for i, stage := range stages {
		ids[i] = stage.ID
		if err := SavePeopleStage(dir, id, stage); err != nil {
			t.Fatal(err)
		}
	}
	pending := syntheticPeoplePending(stages[0])
	pending.StageIDs = ids
	if err := SavePeoplePending(dir, id, pending, stages); err != nil {
		t.Fatal(err)
	}
	recorded := pending.Body
	if logBody != nil {
		recorded = logBody
	}
	if err := AppendCopy(dir, id, CommentsLog, []MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: recorded}}); err != nil {
		t.Fatal(err)
	}
	return dir, id, pending
}

func peopleCopyFile(dir string, id CopyID, parts ...string) string {
	return filepath.Join(append([]string{dir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation}, parts...)...)
}

func TestConfirmPeoplePendingUnkept(t *testing.T) {
	second := syntheticPeopleStage()
	second.ID = "0011223344556677"
	second.Data = []byte("synthetic second")
	sum := sha256.Sum256(second.Data)
	second.Hash = hex.EncodeToString(sum[:])
	for _, tc := range []struct {
		name    string
		stages  []PeopleStage
		logBody json.RawMessage
		prepare func(t *testing.T, dir string, id CopyID, stages []PeopleStage)
		confirm bool
	}{
		{name: "missing directory", confirm: true},
		{name: "missing file", confirm: true, prepare: func(t *testing.T, dir string, id CopyID, stages []PeopleStage) {
			if err := SavePeoplePublished(dir, id, 1, stages[0]); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(peopleCopyFile(dir, id, "people-records", "comments", "1", stages[0].ID+".json")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "matching kept file", confirm: true, prepare: func(t *testing.T, dir string, id CopyID, stages []PeopleStage) {
			if err := SavePeoplePublished(dir, id, 1, stages[0]); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "partial kept file", stages: []PeopleStage{syntheticPeopleStage(), second}, confirm: true, prepare: func(t *testing.T, dir string, id CopyID, stages []PeopleStage) {
			if err := SavePeoplePublished(dir, id, 1, stages[0]); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "mismatched kept file", prepare: func(t *testing.T, dir string, id CopyID, stages []PeopleStage) {
			kept := stages[0]
			kept.Name = "different.txt"
			if err := SavePeoplePublished(dir, id, 1, kept); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unreadable kept file", prepare: func(t *testing.T, dir string, id CopyID, stages []PeopleStage) {
			if err := SavePeoplePublished(dir, id, 1, stages[0]); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(peopleCopyFile(dir, id, "people-records", "comments", "1", stages[0].ID+".json"), []byte("synthetic damage"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing stage", prepare: func(t *testing.T, dir string, id CopyID, stages []PeopleStage) {
			if err := os.Remove(peopleCopyFile(dir, id, "people-stages", stages[0].ID+".json")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "stage hash mismatch", prepare: func(t *testing.T, dir string, id CopyID, stages []PeopleStage) {
			path := peopleCopyFile(dir, id, "people-stages", stages[0].ID+".json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var stage PeopleStage
			if err := json.Unmarshal(raw, &stage); err != nil {
				t.Fatal(err)
			}
			stage.Data = append(stage.Data, '!')
			raw, err = json.Marshal(stage)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "comment mismatch", logBody: json.RawMessage(`{"v":1,"text":"Other"}`)},
		{name: "records obstacle", prepare: func(t *testing.T, dir string, id CopyID, stages []PeopleStage) {
			if err := os.WriteFile(peopleCopyFile(dir, id, "people-records"), []byte("synthetic storage obstacle"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stages := tc.stages
			if stages == nil {
				stages = []PeopleStage{syntheticPeopleStage()}
			}
			dir, id, pending := publishUnkeptFixture(t, stages, tc.logBody)
			if tc.prepare != nil {
				tc.prepare(t, dir, id, stages)
			}
			if tc.name == "missing directory" {
				if err := ConfirmPeoplePending(dir, id, pending.Key, 1, false); err == nil {
					t.Fatal("strict confirm accepted a missing kept file")
				}
			}
			err := ConfirmPeoplePending(dir, id, pending.Key, 1, true)
			if tc.confirm {
				if err != nil {
					t.Fatalf("unkept confirm: %v", err)
				}
				got, readErr := ReadPeopleCompletion(dir, id, pending.Key)
				if readErr != nil || got.Seq != 1 || !bytes.Equal(got.Pending.Body, pending.Body) {
					t.Fatalf("completion: %v", readErr)
				}
				if _, readErr := ReadPeoplePending(dir, id, pending.Key); !errors.Is(readErr, ErrPeopleStageMissing) {
					t.Fatalf("pending pin: %v", readErr)
				}
				for i, stage := range stages {
					_, readErr := ReadPeoplePublished(dir, id, 1, stage.ID)
					kept := readErr == nil
					wantKept := tc.name == "matching kept file" || (tc.name == "partial kept file" && i == 0)
					if kept != wantKept {
						t.Fatalf("stage %s kept=%t err=%v", stage.ID, kept, readErr)
					}
				}
				return
			}
			if err == nil {
				t.Fatal("unkept confirm accepted an incomplete publication")
			}
			if _, readErr := ReadPeopleCompletion(dir, id, pending.Key); !errors.Is(readErr, ErrPeopleStageMissing) {
				t.Fatalf("failed confirm wrote a completion: %v", readErr)
			}
			if _, readErr := ReadPeoplePending(dir, id, pending.Key); readErr != nil {
				t.Fatalf("failed confirm released the pending pin: %v", readErr)
			}
			if err := DeletePeopleStage(dir, id, stages[0].ID); err == nil {
				t.Fatal("failed confirm released the stage pin")
			}
		})
	}
}
