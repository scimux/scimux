package team

import (
	"errors"
	"io/fs"
)

// rejectProtectedPeopleRecords runs under copy and budget locks before a
// missing identity can be initialized. Recognized temporary files are recovered
// by the common storage path, and do not establish a protected association.
func rejectProtectedPeopleRecords(dataDir string, id CopyID) error {
	root, err := peopleRecordDir(dataDir, id, false)
	if errors.Is(err, ErrPeopleStageMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	for _, tree := range []string{"people-stages", "people-records", "people-pending", "people-completed"} {
		err = peopleRecordIO.walk(root.FS(), tree, func(_ string, entry fs.DirEntry, walkErr error) error {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			if walkErr != nil {
				return ErrPeopleStage
			}
			if entry.IsDir() || (entry.Type().IsRegular() && peopleStageTemp(entry.Name())) {
				return nil
			}
			return ErrPeopleStage
		})
		if err != nil {
			return err
		}
	}
	return nil
}
