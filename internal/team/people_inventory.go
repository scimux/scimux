package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/scimux/scimux/internal/storagebudget"
)

// PeopleInventoryEntry is a bounded local review projection. Revision hashes the
// exact immutable value; it is a concurrency precondition, not a credential.
type PeopleInventoryEntry struct {
	Kind     string   `json:"kind"`
	ID       string   `json:"id"`
	Revision string   `json:"revision"`
	Text     string   `json:"text,omitempty"`
	StageIDs []string `json:"stage_ids,omitempty"`
	Name     string   `json:"name,omitempty"`
	MIME     string   `json:"mime,omitempty"`
	Size     int      `json:"size,omitempty"`
	Hash     string   `json:"hash,omitempty"`
}

type PeopleInventory struct {
	Entries []PeopleInventoryEntry `json:"entries"`
	Next    string                 `json:"next_cursor,omitempty"`
}

func peopleInventoryRevision(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// PeopleStageRevision identifies the immutable stage value used for removal.
func PeopleStageRevision(stage PeopleStage) string { return peopleInventoryRevision(stage) }

// ReadPeopleInventory reads only the selected copy, without establishing an identity.
// A cursor is a kind prefix (p or s) followed by a lowercase record ID.
func ReadPeopleInventory(dataDir string, id CopyID, instance, author, cursor string) (PeopleInventory, error) {
	result := PeopleInventory{Entries: []PeopleInventoryEntry{}}
	if !validHex(instance, 32) || !validHex(author, 32) || (cursor != "" && (len(cursor) != 17 || (cursor[0] != 'p' && cursor[0] != 's') || !validHex(cursor[1:], 16))) {
		return result, ErrPeopleStage
	}
	path, err := copyPath(dataDir, id)
	if err != nil {
		return result, err
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	release, err := storagebudget.Lock(dataDir)
	if err != nil {
		return result, err
	}
	defer release()
	actual, err := readPeopleCopyInstanceLocked(dataDir, id)
	if err != nil || actual != instance {
		return result, ErrPeopleStage
	}
	// Bound directory traversal even for damaged or externally populated copies.
	var keys []string
	for _, tree := range []struct{ dir, prefix string }{{"people-pending", "p"}, {"people-stages", "s"}} {
		root, err := peopleRecordDir(dataDir, id, false, tree.dir)
		if errors.Is(err, ErrPeopleStageMissing) {
			continue
		}
		if err != nil {
			return result, err
		}
		dir, err := peopleRecordIO.openDir(root)
		if err != nil {
			root.Close()
			return result, ErrPeopleStage
		}
		entries, err := dir.ReadDir(4097)
		dir.Close()
		root.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return result, ErrPeopleStage
		}
		if len(entries) > 4096 {
			return result, ErrPeopleStage
		}
		for _, entry := range entries {
			name := entry.Name()
			if peopleStageTemp(name) {
				continue
			}
			if tree.prefix == "p" && strings.HasSuffix(name, ".retired") {
				key := strings.TrimSuffix(name, ".retired")
				if !validHex(key, 16) || !entry.Type().IsRegular() {
					return result, ErrPeopleStage
				}
				keys = append(keys, "p"+key)
				continue
			}
			if !strings.HasSuffix(name, ".json") || !validHex(strings.TrimSuffix(name, ".json"), 16) || !entry.Type().IsRegular() {
				return result, ErrPeopleStage
			}
			keys = append(keys, tree.prefix+strings.TrimSuffix(name, ".json"))
		}
	}
	sort.Strings(keys)
	for i := 1; i < len(keys); i++ {
		if keys[i] == keys[i-1] {
			return result, ErrPeopleStage
		}
	}
	scanned := 0
	last := ""
	for _, key := range keys {
		if key <= cursor {
			continue
		}
		if scanned == 8 {
			result.Next = last
			break
		}
		scanned++
		last = key
		recordID := key[1:]
		tree := "people-stages"
		if key[0] == 'p' {
			tree = "people-pending"
		}
		root, err := peopleRecordDir(dataDir, id, false, tree)
		if err != nil {
			return PeopleInventory{}, err
		}
		var record PeopleStage
		if key[0] == 'p' {
			record, err = readPendingEnvelopeLocked(dataDir, id, root, recordID)
		} else {
			record, err = readDurablePeopleRecordFile(root, recordID)
		}
		root.Close()
		if key[0] == 'p' && errors.Is(err, ErrPeopleStageMissing) {
			completed, completionErr := peopleRecordDir(dataDir, id, false, "people-completed")
			if completionErr == nil {
				_, completionErr = readDurablePeopleRecordFile(completed, recordID)
				completed.Close()
			}
			if !errors.Is(completionErr, ErrPeopleStageMissing) {
				return PeopleInventory{}, ErrPeopleStage
			}
			continue
		}
		if err != nil {
			return PeopleInventory{}, err
		}
		if key[0] == 's' {
			result.Entries = append(result.Entries, PeopleInventoryEntry{Kind: "stage", ID: recordID, Revision: PeopleStageRevision(record), Name: record.Name, MIME: record.MIME, Size: len(record.Data), Hash: record.Hash})
			continue
		}
		pending, err := decodePeoplePending(record)
		if err != nil || pending.Author != author {
			return PeopleInventory{}, ErrPeopleStage
		}
		completed, err := peopleRecordDir(dataDir, id, false, "people-completed")
		if err == nil {
			receipt, readErr := readDurablePeopleRecordFile(completed, recordID)
			completed.Close()
			if readErr == nil {
				completion, decodeErr := decodePeopleCompletion(receipt, recordID)
				if decodeErr != nil || !samePendingValue(pending, completion.Pending) {
					return PeopleInventory{}, ErrPeopleStage
				}
				continue
			}
			if !errors.Is(readErr, ErrPeopleStageMissing) {
				return PeopleInventory{}, readErr
			}
		} else if !errors.Is(err, ErrPeopleStageMissing) {
			return PeopleInventory{}, err
		}
		var body struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(pending.Body, &body); err != nil {
			return PeopleInventory{}, ErrPeopleStage
		}
		result.Entries = append(result.Entries, PeopleInventoryEntry{Kind: "pending", ID: recordID, Revision: peopleInventoryRevision(pending), Text: body.Text, StageIDs: pending.StageIDs})
	}
	return result, nil
}
