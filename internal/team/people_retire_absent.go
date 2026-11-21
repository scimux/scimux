package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/storagebudget"
)

var ErrPeopleReviewRequired = errors.New("team: existing publication requires review")

type absentRetirement struct {
	Version      int    `json:"version"`
	Key          string `json:"key"`
	Author       string `json:"author"`
	CopyInstance string `json:"copy_instance"`
}

// decodePeopleRetirement recognizes only the two supported terminal envelopes.
func decodePeopleRetirement(record PeopleStage) (*absentRetirement, *PeoplePending, error) {
	if record.Name == "retired-absent.json" {
		var marker absentRetirement
		dec := json.NewDecoder(bytes.NewReader(record.Data))
		dec.DisallowUnknownFields()
		if dec.Decode(&marker) != nil || dec.Decode(new(any)) != io.EOF || marker.Version != 1 || !validHex(marker.Key, 16) || marker.Key != record.ID || !validHex(marker.Author, 32) || !validHex(marker.CopyInstance, 32) || record.ConfigHash != strings.Repeat("0", 64) || record.OwnerInstance != "" || record.MIME != "application/json" {
			return nil, nil, ErrPeopleStage
		}
		canonical, _ := json.Marshal(marker)
		if !bytes.Equal(record.Data, canonical) {
			return nil, nil, ErrPeopleStage
		}
		return &marker, nil, nil
	}
	pending, err := decodePeoplePending(record)
	if err != nil {
		return nil, nil, err
	}
	return nil, &pending, nil
}

// RetireAbsentPeopleKey creates a durable terminal fence only for an absent key.
func RetireAbsentPeopleKey(dataDir string, id CopyID, instance, author, key string) error {
	if !validHex(instance, 32) || !validHex(author, 32) || !validHex(key, 16) {
		return ErrPeopleStage
	}
	marker := absentRetirement{Version: 1, Key: key, Author: author, CopyInstance: instance}
	raw, _ := json.Marshal(marker)
	sum := sha256.Sum256(raw)
	envelope := PeopleStage{ID: key, Hash: hex.EncodeToString(sum[:]), Name: "retired-absent.json", MIME: "application/json", ConfigHash: strings.Repeat("0", 64), Data: raw}
	encoded, _ := json.Marshal(envelope)
	path, err := copyPath(dataDir, id)
	if err != nil {
		return err
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	already := errors.New("durable absent retirement")
	release, err := storagebudget.ReserveSerializedAfter(dataDir, "", int64(2*len(encoded)), func() error {
		current, err := readPeopleCopyInstanceLocked(dataDir, id)
		if err != nil || current != instance {
			return ErrPeopleStage
		}
		currentAuthor, err := identity.LoadInstallation(dataDir)
		if err != nil || string(currentAuthor) != author {
			return ErrPeopleStage
		}
		root, err := peopleRecordDir(dataDir, id, false, "people-pending")
		if err != nil && !errors.Is(err, ErrPeopleStageMissing) {
			return err
		}
		if err == nil {
			defer root.Close()
			retired, rerr := readPeopleNamedRecord(root, key, key+".retired")
			if rerr != nil && !errors.Is(rerr, ErrPeopleStageMissing) {
				return rerr
			}
			_, perr := readPeopleRecordFile(root, key)
			if perr != nil && !errors.Is(perr, ErrPeopleStageMissing) {
				return perr
			}
			if rerr == nil {
				value, old, err := decodePeopleRetirement(retired)
				if err != nil || perr == nil {
					return ErrPeopleStage
				}
				if old != nil {
					return ErrPeopleReviewRequired
				}
				if *value != marker {
					return ErrPeopleStage
				}
				completed, e := peopleRecordDir(dataDir, id, false, "people-completed")
				if e == nil {
					_, e = readDurablePeopleRecordFile(completed, key)
					completed.Close()
				}
				if !errors.Is(e, ErrPeopleStageMissing) {
					return ErrPeopleStage
				}
				dir, e := peopleRecordIO.openDir(root)
				if e != nil {
					return ErrPeopleStage
				}
				e = peopleRecordIO.sync(dir)
				dir.Close()
				if e != nil {
					return ErrPeopleStage
				}
				return already
			}
			if perr == nil {
				return ErrPeopleReviewRequired
			}
		}
		completed, e := peopleRecordDir(dataDir, id, false, "people-completed")
		if e == nil {
			_, e = readDurablePeopleRecordFile(completed, key)
			completed.Close()
		}
		if e == nil {
			return ErrPeopleReviewRequired
		}
		if !errors.Is(e, ErrPeopleStageMissing) {
			return e
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
	return savePeopleNamedRecordLocked(dataDir, id, envelope, encoded, true, peopleChargedTrees("people-pending"), "people-pending")
}

// readValidRetirement checks a terminal envelope and its local binding.
func readValidRetirement(dataDir string, id CopyID, record PeopleStage) (*absentRetirement, *PeoplePending, error) {
	marker, pending, err := decodePeopleRetirement(record)
	if err != nil || marker == nil {
		return marker, pending, err
	}
	instance, err := readPeopleCopyInstanceLocked(dataDir, id)
	if err != nil || instance != marker.CopyInstance {
		return nil, nil, ErrPeopleStage
	}
	author, err := identity.LoadInstallation(dataDir)
	if err != nil || string(author) != marker.Author {
		return nil, nil, ErrPeopleStage
	}
	return marker, nil, nil
}

func retirementIsValid(dataDir string, id CopyID, record PeopleStage) error {
	_, _, err := readValidRetirement(dataDir, id, record)
	return err
}
