package team

import (
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/scimux/scimux/internal/storagebudget"
)

type localPublicationRecord struct {
	state        string
	pending      PeoplePending
	seq          uint64
	pinned       bool
	markerAuthor string
}

// ReadPeopleLocalPublication returns the immutable value for an exact local
// pending, retired, or completed key. It never creates a copy identity.
func ReadPeopleLocalPublication(dataDir string, id CopyID, instance, key string) (PeoplePending, error) {
	result, err := readPeopleLocalPublication(dataDir, id, instance, key, false)
	if err != nil {
		return PeoplePending{}, err
	}
	return result.pending, nil
}

// PeopleLocalPublicationState is a bounded local review projection.
type PeopleLocalPublicationState struct {
	State         string   `json:"state"`
	Revision      string   `json:"revision,omitempty"`
	Text          string   `json:"text,omitempty"`
	StageIDs      []string `json:"stage_ids,omitempty"`
	Seq           uint64   `json:"seq,omitempty"`
	PendingPinned bool     `json:"pending_pinned,omitempty"`
}

// ReadPeopleLocalPublicationState inspects an exact copy without retiring it.
func ReadPeopleLocalPublicationState(dataDir string, id CopyID, instance, author, key string) (PeopleLocalPublicationState, error) {
	if !validHex(author, 32) {
		return PeopleLocalPublicationState{}, ErrPeopleStage
	}
	result, err := readPeopleLocalPublication(dataDir, id, instance, key, true)
	if err != nil {
		return PeopleLocalPublicationState{}, err
	}
	if result.state == "absent_retired" && result.markerAuthor != author {
		return PeopleLocalPublicationState{}, ErrPeopleStage
	}
	if result.state == "missing" || result.state == "absent_retired" {
		return PeopleLocalPublicationState{State: result.state}, nil
	}
	if result.pending.Author != author {
		return PeopleLocalPublicationState{}, ErrPeopleStage
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(result.pending.Body, &body); err != nil {
		return PeopleLocalPublicationState{}, ErrPeopleStage
	}
	return PeopleLocalPublicationState{State: result.state, Revision: peopleInventoryRevision(result.pending), Text: body.Text, StageIDs: result.pending.StageIDs, Seq: result.seq, PendingPinned: result.pinned}, nil
}

func readPeopleLocalPublication(dataDir string, id CopyID, instance, key string, inspect bool) (localPublicationRecord, error) {
	if !validHex(instance, 32) || !validHex(key, 16) {
		return localPublicationRecord{}, ErrPeopleStage
	}
	path, err := copyPath(dataDir, id)
	if err != nil {
		return localPublicationRecord{}, err
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	release, err := storagebudget.Lock(dataDir)
	if err != nil {
		return localPublicationRecord{}, err
	}
	defer release()
	actual, err := readPeopleCopyInstanceLocked(dataDir, id)
	if err != nil || actual != instance {
		return localPublicationRecord{}, ErrPeopleStage
	}
	root, err := peopleRecordDir(dataDir, id, false, "people-pending")
	if err != nil && (!inspect || !errors.Is(err, ErrPeopleStageMissing)) {
		return localPublicationRecord{}, err
	}
	var retired, pending PeopleStage
	retiredErr, pendingErr := ErrPeopleStageMissing, ErrPeopleStageMissing
	if root != nil {
		defer root.Close()
		retired, retiredErr = readPeopleNamedRecord(root, key, key+".retired")
		if retiredErr != nil && !errors.Is(retiredErr, ErrPeopleStageMissing) {
			return localPublicationRecord{}, retiredErr
		}
		pending, pendingErr = readDurablePeopleRecordFile(root, key)
		if pendingErr != nil && !errors.Is(pendingErr, ErrPeopleStageMissing) {
			return localPublicationRecord{}, pendingErr
		}
	}
	completedRoot, completedErr := peopleRecordDir(dataDir, id, false, "people-completed")
	var completion PeopleCompletion
	if completedErr == nil {
		record, readErr := readDurablePeopleRecordFile(completedRoot, key)
		completedRoot.Close()
		if readErr == nil {
			completion, completedErr = decodePeopleCompletion(record, key)
		} else {
			completedErr = readErr
		}
	}
	if completedErr != nil && !errors.Is(completedErr, ErrPeopleStageMissing) {
		return localPublicationRecord{}, completedErr
	}
	if retiredErr == nil {
		if pendingErr == nil || completedErr == nil {
			return localPublicationRecord{}, ErrPeopleStage
		}
		dir, err := peopleRecordIO.openDir(root)
		if err != nil {
			return localPublicationRecord{}, ErrPeopleStage
		}
		err = peopleRecordIO.sync(dir)
		dir.Close()
		if err != nil {
			return localPublicationRecord{}, ErrPeopleStage
		}
		marker, value, err := readValidRetirement(dataDir, id, retired)
		if err != nil {
			return localPublicationRecord{}, err
		}
		if marker != nil {
			if !inspect {
				return localPublicationRecord{}, ErrPeopleStageMissing
			}
			return localPublicationRecord{state: "absent_retired", markerAuthor: marker.Author}, nil
		}
		return localPublicationRecord{state: "retired", pending: *value}, nil
	}
	if pendingErr == nil {
		value, err := decodePeoplePending(pending)
		if err != nil {
			return localPublicationRecord{}, err
		}
		if completedErr == nil && !samePendingValue(value, completion.Pending) {
			return localPublicationRecord{}, ErrPeopleStage
		}
		if completedErr == nil && inspect {
			if err := validateCompletionRetained(dataDir, id, completion, true, true); err != nil {
				return localPublicationRecord{}, err
			}
			return localPublicationRecord{state: "completed", pending: value, seq: completion.Seq, pinned: true}, nil
		}
		return localPublicationRecord{state: "pending", pending: value}, nil
	}
	if completedErr == nil {
		if inspect {
			if err := validateCompletionRetained(dataDir, id, completion, false, true); err != nil {
				return localPublicationRecord{}, err
			}
		}
		return localPublicationRecord{state: "completed", pending: completion.Pending, seq: completion.Seq}, nil
	}
	if inspect {
		return localPublicationRecord{state: "missing"}, nil
	}
	return localPublicationRecord{}, ErrPeopleStageMissing
}
