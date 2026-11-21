package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strconv"

	"github.com/scimux/scimux/internal/storagebudget"
)

// DiscardPeoplePending retires an exact local publication. A nonzero result
// identifies an already completed publication; zero means locally retired.
func DiscardPeoplePending(dataDir string, id CopyID, instance string, expected PeoplePending) (uint64, error) {
	if !validPeoplePending(expected) || !validHex(instance, 32) {
		return 0, ErrPeopleStage
	}
	path, err := copyPath(dataDir, id)
	if err != nil {
		return 0, err
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	release, err := storagebudget.Lock(dataDir)
	if err != nil {
		return 0, err
	}
	defer release()
	actual, err := readPeopleCopyInstanceLocked(dataDir, id)
	if err != nil || actual != instance {
		return 0, ErrPeopleStage
	}
	root, err := peopleRecordDir(dataDir, id, false, "people-pending")
	if err != nil {
		return 0, err
	}
	defer root.Close()
	dir, err := peopleRecordIO.openDir(root)
	if err != nil {
		return 0, ErrPeopleStage
	}
	err = peopleRecordIO.sync(dir)
	dir.Close()
	if err != nil {
		return 0, ErrPeopleStage
	}
	retired, retiredErr := readPeopleNamedRecord(root, expected.Key, expected.Key+".retired")
	if retiredErr == nil {
		if !sameDiscardPending(retired, expected) {
			return 0, ErrPeopleStage
		}
		if _, err := peopleRecordIO.lstat(root, expected.Key+".json"); !errors.Is(err, fs.ErrNotExist) {
			return 0, ErrPeopleStage
		}
		return 0, nil
	}
	if !errors.Is(retiredErr, ErrPeopleStageMissing) {
		return 0, retiredErr
	}
	completedRoot, completionErr := peopleRecordDir(dataDir, id, false, "people-completed")
	if completionErr == nil {
		record, readErr := readDurablePeopleRecordFile(completedRoot, expected.Key)
		completedRoot.Close()
		if readErr == nil {
			completion, err := decodePeopleCompletion(record, expected.Key)
			if err != nil || !samePendingValue(completion.Pending, expected) {
				return 0, ErrPeopleStage
			}
			pending, readErr := readPeopleRecordFile(root, expected.Key)
			if err := discardCompletionRetained(dataDir, id, completion, readErr == nil); err != nil {
				return 0, err
			}
			if readErr == nil {
				if !sameDiscardPending(pending, expected) {
					return 0, ErrPeopleStage
				}
				if err := peopleRecordIO.remove(root, expected.Key+".json"); err != nil {
					return 0, ErrPeopleStage
				}
			} else if !errors.Is(readErr, ErrPeopleStageMissing) {
				return 0, readErr
			}
			dir, err := peopleRecordIO.openDir(root)
			if err != nil {
				return 0, ErrPeopleStage
			}
			err = peopleRecordIO.sync(dir)
			dir.Close()
			if err != nil {
				return 0, ErrPeopleStage
			}
			return completion.Seq, nil
		}
		if !errors.Is(readErr, ErrPeopleStageMissing) {
			return 0, readErr
		}
	} else if !errors.Is(completionErr, ErrPeopleStageMissing) {
		return 0, completionErr
	}
	pending, err := readPeopleRecordFile(root, expected.Key)
	if err != nil {
		return 0, err
	}
	if !sameDiscardPending(pending, expected) {
		return 0, ErrPeopleStage
	}
	if err := peopleRecordIO.rename(root, expected.Key+".json", expected.Key+".retired"); err != nil {
		return 0, ErrPeopleStage
	}
	dir, err = peopleRecordIO.openDir(root)
	if err != nil {
		return 0, ErrPeopleStage
	}
	err = peopleRecordIO.sync(dir)
	dir.Close()
	if err != nil {
		return 0, ErrPeopleStage
	}
	return 0, nil
}

func samePendingValue(a, b PeoplePending) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func sameDiscardPending(record PeopleStage, expected PeoplePending) bool {
	pending, err := decodePeoplePending(record)
	return err == nil && samePendingValue(pending, expected)
}

func discardCompletionRetained(dataDir string, id CopyID, completion PeopleCompletion, pinned bool) error {
	return validateCompletionRetained(dataDir, id, completion, pinned, false)
}

func validateCompletionRetained(dataDir string, id CopyID, completion PeopleCompletion, pinned, strict bool) error {
	pending, seq := completion.Pending, completion.Seq
	var retained []PeopleStage
	if pinned {
		var err error
		retained, err = validatePeoplePublication(dataDir, id, pending, seq, false)
		if err != nil {
			return err
		}
	} else if err := validateRetainedPeopleComment(dataDir, id, pending, seq); err != nil {
		return err
	}
	if !pinned && strict {
		var err error
		retained, err = readCompletionRetained(dataDir, id, pending, seq, true)
		if err != nil {
			return err
		}
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(pending.Body, &fields) // Non-object valid JSON is a legacy body.
	_, hasVersion := fields["people_attachments_version"]
	_, hasFiles := fields["people_attachments"]
	if !hasVersion && !hasFiles {
		return nil
	}
	var manifest struct {
		Version     int `json:"people_attachments_version"`
		Attachments []struct {
			ID   string `json:"id"`
			Hash string `json:"sha256"`
			Size int64  `json:"size"`
			Name string `json:"name"`
			MIME string `json:"mime"`
		} `json:"people_attachments"`
	}
	if !hasVersion || !hasFiles || json.Unmarshal(fields["people_attachments_version"], &manifest.Version) != nil || manifest.Version != 1 {
		return ErrPeopleStage
	}
	decoder := json.NewDecoder(bytes.NewReader(fields["people_attachments"]))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&manifest.Attachments) != nil || len(manifest.Attachments) != len(pending.StageIDs) {
		return ErrPeopleStage
	}
	if !pinned && !strict {
		var err error
		retained, err = readCompletionRetained(dataDir, id, pending, seq, false)
		if err != nil {
			return err
		}
	}
	var total int64
	for i, stageID := range pending.StageIDs {
		file := manifest.Attachments[i]
		metadata := PeopleStage{ID: file.ID, Hash: file.Hash, Name: file.Name, MIME: file.MIME, ConfigHash: pending.ConfigHash, OwnerInstance: pending.OwnerInstance}
		if file.ID != stageID || file.Size < 1 || file.Size > 4<<20 || !validPeopleStageMetadata(metadata) {
			return ErrPeopleStage
		}
		total += file.Size
		if total > 16<<20 {
			return ErrPeopleStage
		}
		if len(retained) == 0 || retained[i].ID == "" {
			continue
		}
		copy := retained[i]
		if file.Hash != copy.Hash || file.Size != int64(len(copy.Data)) || file.Name != copy.Name || file.MIME != copy.MIME {
			return ErrPeopleStage
		}
	}
	return nil
}

func readCompletionRetained(dataDir string, id CopyID, pending PeoplePending, seq uint64, strict bool) ([]PeopleStage, error) {
	root, err := peopleRecordDir(dataDir, id, false, "people-records", "comments", strconv.FormatUint(seq, 10))
	if errors.Is(err, ErrPeopleStageMissing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	retained := make([]PeopleStage, len(pending.StageIDs))
	for i, stageID := range pending.StageIDs {
		copy, readErr := readDurablePeopleRecordFile(root, stageID)
		if strict && readErr != nil && !errors.Is(readErr, ErrPeopleStageMissing) {
			return nil, readErr
		}
		if readErr == nil {
			retained[i] = copy
		}
	}
	return retained, nil
}
