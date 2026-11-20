package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
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
	record, err := readDurablePeopleRecord(dataDir, id, key, "people-completed")
	if err != nil {
		return PeopleCompletion{}, err
	}
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
	same := func() (bool, bool) {
		old, err := ReadPeoplePending(dataDir, id, pending.Key)
		if err != nil {
			return false, false
		}
		prior, err := json.Marshal(old)
		return true, err == nil && bytes.Equal(prior, raw)
	}
	if exists, equal := same(); exists {
		if equal {
			return nil
		}
		return ErrPeopleStage
	}
	sum := sha256.Sum256(raw)
	envelope := PeopleStage{ID: pending.Key, Hash: hex.EncodeToString(sum[:]), Name: "pending.json", MIME: "application/json", ConfigHash: pending.ConfigHash, OwnerInstance: pending.OwnerInstance, Data: raw}
	encoded, _ := json.Marshal(envelope) // PeopleStage contains only strings and bytes.
	path, err := copyPath(dataDir, id)
	if err != nil {
		return err
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	completed, completedErr := peopleRecordDir(dataDir, id, false, "people-completed")
	if completedErr == nil {
		_, readErr := readPeopleRecordFile(completed, pending.Key)
		completed.Close()
		if readErr == nil || !errors.Is(readErr, ErrPeopleStageMissing) {
			return ErrPeopleStage
		}
	} else if !errors.Is(completedErr, ErrPeopleStageMissing) {
		return completedErr
	}
	release, err := storagebudget.ReserveSerializedAfter(dataDir, "", int64(len(encoded)), func() error { return recoverPeopleTemps(dataDir, id) })
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
	pendingRoot, err := peopleRecordDir(dataDir, id, true, "people-pending")
	if err != nil {
		return err
	}
	prior, statErr := peopleRecordIO.lstat(pendingRoot, pending.Key+".json")
	if statErr == nil {
		if !prior.Mode().IsRegular() {
			pendingRoot.Close()
			return ErrPeopleStage
		}
		old, err := readDurablePeopleRecordFile(pendingRoot, pending.Key)
		pendingRoot.Close()
		if err == nil && bytes.Equal(old.Data, raw) {
			return nil
		}
		return ErrPeopleStage
	}
	pendingRoot.Close()
	if !errors.Is(statErr, fs.ErrNotExist) {
		return ErrPeopleStage
	}
	return savePeopleRecordLocked(dataDir, id, envelope, encoded, "people-pending")
}

func ReadPeoplePending(dataDir string, id CopyID, key string) (PeoplePending, error) {
	stage, err := readDurablePeopleRecord(dataDir, id, key, "people-pending")
	if err != nil {
		return PeoplePending{}, err
	}
	return decodePeoplePending(stage)
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
	entries, err := peopleRecordIO.readDir(dir)
	dir.Close()
	if err != nil {
		return false, ErrPeopleStage
	}
	for _, entry := range entries {
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

// ConfirmPeoplePending releases a pin only after all retained records exist.
func ConfirmPeoplePending(dataDir string, id CopyID, key string, seq uint64) error {
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
	envelope, err := readDurablePeopleRecordFile(pendingRoot, key)
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
	release, err := storagebudget.ReserveSerializedAfter(dataDir, "", incoming, func() error { return recoverPeopleTemps(dataDir, id) })
	if err != nil {
		return err
	}
	defer release()
	// copyPath already validated this unchanged value; CommentsLog is valid.
	logPath, _ := copyLogPath(dataDir, id, CommentsLog)
	entries, _, err := readCopyLocked(logPath, seq-1, 1, 4<<20)
	if err != nil || len(entries) != 1 || entries[0].Seq != seq || entries[0].Key != "c/"+key || entries[0].Author != pending.Author {
		return ErrPeopleStage
	}
	want, wantErr := json.Marshal(pending.Body)
	got, gotErr := json.Marshal(entries[0].Body)
	if wantErr != nil || gotErr != nil || !bytes.Equal(want, got) {
		return ErrPeopleStage
	}
	stages, err := peopleStageDir(dataDir, id, false)
	if err != nil {
		return err
	}
	defer stages.Close()
	retained, err := peopleRecordDir(dataDir, id, false, "people-records", "comments", strconv.FormatUint(seq, 10))
	if err != nil {
		return err
	}
	defer retained.Close()
	for _, stageID := range pending.StageIDs {
		stage, stageErr := readDurablePeopleRecordFile(stages, stageID)
		copy, copyErr := readDurablePeopleRecordFile(retained, stageID)
		if stageErr != nil || copyErr != nil || stage.Hash != copy.Hash || stage.Name != copy.Name || stage.MIME != copy.MIME || !bytes.Equal(stage.Data, copy.Data) {
			return ErrPeopleStage
		}
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
