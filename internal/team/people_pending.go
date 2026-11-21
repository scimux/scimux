package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/scimux/scimux/internal/storagebudget"
)

// PeoplePending is an immutable publication bound to one CopyID and key.
type PeoplePending struct {
	Key           string          `json:"key"`
	Author        string          `json:"author"`
	ConfigHash    string          `json:"config_hash"`
	OwnerInstance string          `json:"owner_instance,omitempty"`
	Body          json.RawMessage `json:"body"`
	StageIDs      []string        `json:"stage_ids"`
}

type PeopleCompletion struct {
	Pending PeoplePending `json:"pending"`
	Seq     uint64        `json:"seq"`
}

func ReadPeopleCompletion(dataDir string, id CopyID, key string) (PeopleCompletion, error) {
	if !validHex(key, 16) {
		return PeopleCompletion{}, ErrPeopleStage
	}
	path, err := copyPath(dataDir, id)
	if err != nil {
		return PeopleCompletion{}, err
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	release, err := storagebudget.Lock(dataDir)
	if err != nil {
		return PeopleCompletion{}, err
	}
	defer release()
	pendingRoot, pendingErr := peopleRecordDir(dataDir, id, false, "people-pending")
	if pendingErr == nil {
		retired, retiredErr := readPeopleNamedRecord(pendingRoot, key, key+".retired")
		if retiredErr == nil {
			if err := retirementIsValid(dataDir, id, retired); err != nil {
				pendingRoot.Close()
				return PeopleCompletion{}, err
			}
			dir, openErr := peopleRecordIO.openDir(pendingRoot)
			if openErr == nil {
				_ = peopleRecordIO.sync(dir)
				dir.Close()
			}
			pendingRoot.Close()
			return PeopleCompletion{}, ErrPeopleStage
		}
		pendingRoot.Close()
		if !errors.Is(retiredErr, ErrPeopleStageMissing) {
			return PeopleCompletion{}, retiredErr
		}
	} else if !errors.Is(pendingErr, ErrPeopleStageMissing) {
		return PeopleCompletion{}, pendingErr
	}
	root, err := peopleRecordDir(dataDir, id, false, "people-completed")
	if err != nil {
		return PeopleCompletion{}, err
	}
	defer root.Close()
	record, err := readDurablePeopleRecordFile(root, key)
	if err != nil {
		return PeopleCompletion{}, err
	}
	return decodePeopleCompletion(record, key)
}

func decodePeopleCompletion(record PeopleStage, key string) (PeopleCompletion, error) {
	var result PeopleCompletion
	dec := json.NewDecoder(bytes.NewReader(record.Data))
	dec.DisallowUnknownFields()
	if dec.Decode(&result) != nil || dec.Decode(new(any)) != io.EOF || result.Seq == 0 || !validPeoplePending(result.Pending) || result.Pending.Key != key || result.Pending.ConfigHash != record.ConfigHash || result.Pending.OwnerInstance != record.OwnerInstance {
		return PeopleCompletion{}, ErrPeopleStage
	}
	return result, nil
}

func validPeoplePending(p PeoplePending) bool {
	if !validHex(p.Key, 16) || !validHex(p.Author, 32) || !validHex(p.ConfigHash, 64) || (p.OwnerInstance != "" && !ValidPeopleOwnerInstance(p.OwnerInstance)) || !json.Valid(p.Body) || len(p.Body) > 256<<10 || len(p.StageIDs) == 0 || len(p.StageIDs) > 16 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range p.StageIDs {
		if !validHex(id, 16) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func decodePeoplePending(stage PeopleStage) (PeoplePending, error) {
	var p PeoplePending
	dec := json.NewDecoder(bytes.NewReader(stage.Data))
	dec.DisallowUnknownFields()
	if dec.Decode(&p) != nil || dec.Decode(new(any)) != io.EOF || !validPeoplePending(p) || p.Key != stage.ID || p.ConfigHash != stage.ConfigHash || p.OwnerInstance != stage.OwnerInstance {
		return PeoplePending{}, ErrPeopleStage
	}
	return p, nil
}

// SavePeoplePending freezes body, key and ordered stage identities. It checks
// and pins the stages under copy-lock then serialized-budget-lock ordering.
func SavePeoplePending(dataDir string, id CopyID, pending PeoplePending, reviewed []PeopleStage) error {
	if !validPeoplePending(pending) {
		return ErrPeopleStage
	}
	// The fixed fields and the JSON body validated above cannot fail encoding.
	raw, _ := json.Marshal(pending)
	sum := sha256.Sum256(raw)
	envelope := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(sum[:]), Name: "pending.json", MIME: "application/json", ConfigHash: pending.ConfigHash, OwnerInstance: pending.OwnerInstance, Data: raw}
	encoded, _ := json.Marshal(envelope) // PeopleStage contains only strings and bytes.
	path, err := copyPath(dataDir, id)
	if err != nil {
		return err
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	already := errors.New("identical pending record")
	release, err := storagebudget.ReserveSerializedAfter(dataDir, "", int64(len(encoded)), func() error {
		completed, completedErr := peopleRecordDir(dataDir, id, false, "people-completed")
		if completedErr == nil {
			_, readErr := readDurablePeopleRecordFile(completed, pending.Key)
			completed.Close()
			if readErr == nil || !errors.Is(readErr, ErrPeopleStageMissing) {
				return ErrPeopleStage
			}
		} else if !errors.Is(completedErr, ErrPeopleStageMissing) {
			return completedErr
		}
		pendingRoot, pendingErr := peopleRecordDir(dataDir, id, false, "people-pending")
		if pendingErr == nil {
			retired, readErr := readPeopleNamedRecord(pendingRoot, pending.Key, pending.Key+".retired")
			if readErr == nil {
				if err := retirementIsValid(dataDir, id, retired); err != nil {
					pendingRoot.Close()
					return err
				}
				dir, openErr := peopleRecordIO.openDir(pendingRoot)
				if openErr == nil {
					_ = peopleRecordIO.sync(dir)
					dir.Close()
				}
				pendingRoot.Close()
				return ErrPeopleStage
			}
			if errors.Is(readErr, ErrPeopleStageMissing) {
				old, oldErr := readDurablePeopleRecordFile(pendingRoot, pending.Key)
				if oldErr == nil {
					pendingRoot.Close()
					if bytes.Equal(old.Data, raw) {
						return already
					}
					return ErrPeopleStage
				}
				if !errors.Is(oldErr, ErrPeopleStageMissing) {
					pendingRoot.Close()
					return oldErr
				}
			}
			pendingRoot.Close()
			if !errors.Is(readErr, ErrPeopleStageMissing) {
				return readErr
			}
		} else if !errors.Is(pendingErr, ErrPeopleStageMissing) {
			return pendingErr
		}
		return recoverPeopleTemps(dataDir, id, peopleChargedTrees("people-stages"))
	})
	if errors.Is(err, already) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	stages, err := peopleStageDir(dataDir, id, false)
	if err != nil {
		return err
	}
	defer stages.Close()
	if len(reviewed) != len(pending.StageIDs) {
		return ErrPeopleStage
	}
	for i, stageID := range pending.StageIDs {
		stage, err := readDurablePeopleRecordFile(stages, stageID)
		want := reviewed[i]
		if err != nil || stage.ID != want.ID || stage.Hash != want.Hash || stage.Name != want.Name || stage.MIME != want.MIME || stage.ConfigHash != pending.ConfigHash || stage.ConfigHash != want.ConfigHash || stage.OwnerInstance != pending.OwnerInstance || stage.OwnerInstance != want.OwnerInstance || !bytes.Equal(stage.Data, want.Data) {
			return ErrPeopleStage
		}
	}
	return savePeopleRecordLocked(dataDir, id, envelope, encoded, "people-pending")
}

func ReadPeoplePending(dataDir string, id CopyID, key string) (PeoplePending, error) {
	if !validHex(key, 16) {
		return PeoplePending{}, ErrPeopleStage
	}
	path, err := copyPath(dataDir, id)
	if err != nil {
		return PeoplePending{}, err
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	release, err := storagebudget.Lock(dataDir)
	if err != nil {
		return PeoplePending{}, err
	}
	defer release()
	root, err := peopleRecordDir(dataDir, id, false, "people-pending")
	if err != nil {
		return PeoplePending{}, err
	}
	defer root.Close()
	stage, err := readPendingEnvelopeLocked(dataDir, id, root, key)
	if err != nil {
		return PeoplePending{}, err
	}
	return decodePeoplePending(stage)
}

func readPendingEnvelopeLocked(dataDir string, id CopyID, root *os.Root, key string) (PeopleStage, error) {
	retired, retiredErr := readPeopleNamedRecord(root, key, key+".retired")
	if retiredErr == nil {
		if err := retirementIsValid(dataDir, id, retired); err != nil {
			return PeopleStage{}, err
		}
		dir, err := peopleRecordIO.openDir(root)
		if err != nil {
			return PeopleStage{}, ErrPeopleStage
		}
		err = peopleRecordIO.sync(dir)
		dir.Close()
		if err != nil {
			return PeopleStage{}, ErrPeopleStage
		}
		if _, err := peopleRecordIO.lstat(root, key+".json"); !errors.Is(err, fs.ErrNotExist) {
			return PeopleStage{}, ErrPeopleStage
		}
		return PeopleStage{}, ErrPeopleStageMissing
	}
	if !errors.Is(retiredErr, ErrPeopleStageMissing) {
		return PeopleStage{}, retiredErr
	}
	return readDurablePeopleRecordFile(root, key)
}

// pendingReferencesLocked is called only with the copy and budget locks held.
// A damaged pending record fails closed rather than allowing stage deletion.
func pendingReferencesLocked(dataDir string, id CopyID, stageID string) (bool, error) {
	copyRoot, err := peopleRecordDir(dataDir, id, false)
	if err != nil {
		return false, err
	}
	_, err = peopleRecordIO.lstat(copyRoot, "people-pending")
	copyRoot.Close()
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, ErrPeopleStage
	}
	root, err := peopleRecordDir(dataDir, id, false, "people-pending")
	if err != nil {
		return false, err
	}
	defer root.Close()
	dir, err := peopleRecordIO.openDir(root)
	if err != nil {
		return false, ErrPeopleStage
	}
	// A rename can be visible after its directory sync failed. Repair that
	// barrier before absence of a pending name can release a pin.
	if err := peopleRecordIO.sync(dir); err != nil {
		dir.Close()
		return false, ErrPeopleStage
	}
	entries, err := peopleRecordIO.readDir(dir)
	dir.Close()
	if err != nil {
		return false, ErrPeopleStage
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".retired") {
			key := strings.TrimSuffix(entry.Name(), ".retired")
			if !validHex(key, 16) {
				return false, ErrPeopleStage
			}
			record, err := readPeopleNamedRecord(root, key, entry.Name())
			if err != nil {
				return false, err
			}
			if err := retirementIsValid(dataDir, id, record); err != nil {
				return false, err
			}
			if _, err := peopleRecordIO.lstat(root, key+".json"); !errors.Is(err, fs.ErrNotExist) {
				return false, ErrPeopleStage
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".json")
		if !validHex(key, 16) {
			return false, ErrPeopleStage
		}
		stage, err := readPeopleRecordFile(root, key)
		if err != nil {
			return false, ErrPeopleStage
		}
		p, err := decodePeoplePending(stage)
		if err != nil {
			return false, err
		}
		for _, pinned := range p.StageIDs {
			if pinned == stageID {
				return true, nil
			}
		}
	}
	return false, nil
}

// ConfirmPeoplePending releases a pin after the comment log matches the pending body.
// allowUnkept permits a missing kept file when every staged file matches its hash.
func ConfirmPeoplePending(dataDir string, id CopyID, key string, seq uint64, allowUnkept bool) error {
	if !validHex(key, 16) || seq == 0 {
		return ErrPeopleStage
	}
	path, err := copyPath(dataDir, id)
	if err != nil {
		return err
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	pendingRoot, err := peopleRecordDir(dataDir, id, false, "people-pending")
	if err != nil {
		return err
	}
	defer pendingRoot.Close()
	envelope, err := readPendingEnvelopeLocked(dataDir, id, pendingRoot, key)
	if err != nil {
		return err
	}
	pending, err := decodePeoplePending(envelope)
	if err != nil {
		return err
	}
	// decodePeoplePending has validated the fixed fields and JSON body.
	raw, _ := json.Marshal(PeopleCompletion{Pending: pending, Seq: seq})
	sum := sha256.Sum256(raw)
	completion := PeopleStage{ID: key, Hash: hex.EncodeToString(sum[:]), Name: "completion.json", MIME: "application/json", ConfigHash: pending.ConfigHash, OwnerInstance: pending.OwnerInstance, Data: raw}
	encoded, _ := json.Marshal(completion) // PeopleStage contains only strings and bytes.
	completed, rootErr := peopleRecordDir(dataDir, id, false, "people-completed")
	exists := false
	if rootErr == nil {
		prior, readErr := readDurablePeopleRecordFile(completed, key)
		completed.Close()
		if readErr == nil {
			if !bytes.Equal(prior.Data, raw) {
				return ErrPeopleStage
			}
			exists = true
		} else if !errors.Is(readErr, ErrPeopleStageMissing) {
			return readErr
		}
	} else if !errors.Is(rootErr, ErrPeopleStageMissing) {
		return rootErr
	}
	incoming := int64(len(encoded))
	if exists {
		incoming = 0
	}
	release, err := storagebudget.ReserveSerializedAfter(dataDir, "", incoming, func() error {
		current, err := readPendingEnvelopeLocked(dataDir, id, pendingRoot, key)
		if err != nil || !bytes.Equal(current.Data, envelope.Data) {
			return ErrPeopleStage
		}
		root, rootErr := peopleRecordDir(dataDir, id, false, "people-completed")
		currentExists := false
		if rootErr == nil {
			prior, readErr := readDurablePeopleRecordFile(root, key)
			root.Close()
			if readErr == nil {
				if !bytes.Equal(prior.Data, raw) {
					return ErrPeopleStage
				}
				currentExists = true
			} else if !errors.Is(readErr, ErrPeopleStageMissing) {
				return readErr
			}
		} else if !errors.Is(rootErr, ErrPeopleStageMissing) {
			return rootErr
		}
		if exists != currentExists {
			return ErrPeopleStage
		}
		return recoverPeopleTemps(dataDir, id, peopleChargedTrees("people-stages"))
	})
	if err != nil {
		return err
	}
	defer release()
	if _, err := validatePeoplePublication(dataDir, id, pending, seq, allowUnkept); err != nil {
		return err
	}
	if !exists {
		if err := savePeopleRecordLocked(dataDir, id, completion, encoded, "people-completed"); err != nil {
			return err
		}
	}
	if err := peopleRecordIO.remove(pendingRoot, key+".json"); err != nil {
		return ErrPeopleStage
	}
	dir, err := peopleRecordIO.openDir(pendingRoot)
	if err != nil {
		return ErrPeopleStage
	}
	err = peopleRecordIO.sync(dir)
	_ = dir.Close()
	if err != nil {
		return ErrPeopleStage
	}
	return nil
}

// validatePeoplePublication requires the copy lock and budget lock.
// allowUnkept permits a missing kept file after the staged bytes match their hash.
func validatePeoplePublication(dataDir string, id CopyID, pending PeoplePending, seq uint64, allowUnkept bool) ([]PeopleStage, error) {
	if err := validateRetainedPeopleComment(dataDir, id, pending, seq); err != nil {
		return nil, err
	}
	stages, err := peopleStageDir(dataDir, id, false)
	if err != nil {
		return nil, err
	}
	defer stages.Close()
	retained, err := peopleRecordDir(dataDir, id, false, "people-records", "comments", strconv.FormatUint(seq, 10))
	if err != nil && !(allowUnkept && errors.Is(err, ErrPeopleStageMissing)) {
		return nil, err
	}
	if retained != nil {
		defer retained.Close()
	}
	result := make([]PeopleStage, 0, len(pending.StageIDs))
	for _, stageID := range pending.StageIDs {
		stage, stageErr := readDurablePeopleRecordFile(stages, stageID)
		if stageErr != nil {
			return nil, ErrPeopleStage
		}
		if retained == nil {
			result = append(result, stage)
			continue
		}
		copy, copyErr := readDurablePeopleRecordFile(retained, stageID)
		if allowUnkept && errors.Is(copyErr, ErrPeopleStageMissing) {
			result = append(result, stage)
			continue
		}
		// Both records were hash-verified on read, so equal bytes also imply
		// equal hashes. Compare bytes directly to preserve the exact snapshot.
		if copyErr != nil || !bytes.Equal(stage.Data, copy.Data) || stage.Name != copy.Name || stage.MIME != copy.MIME {
			return nil, ErrPeopleStage
		}
		result = append(result, copy)
	}
	return result, nil
}

func validateRetainedPeopleComment(dataDir string, id CopyID, pending PeoplePending, seq uint64) error {
	logPath, _ := copyLogPath(dataDir, id, CommentsLog)
	entries, _, err := readCopyLocked(logPath, seq-1, 1, 4<<20)
	if err != nil || len(entries) != 1 || entries[0].Seq != seq || entries[0].Key != "c/"+pending.Key || entries[0].Author != pending.Author {
		return ErrPeopleStage
	}
	want, wantErr := json.Marshal(pending.Body)
	got, gotErr := json.Marshal(entries[0].Body)
	if wantErr != nil || gotErr != nil || !bytes.Equal(want, got) {
		return ErrPeopleStage
	}
	return nil
}
