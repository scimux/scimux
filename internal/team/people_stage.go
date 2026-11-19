package team

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/scimux/scimux/internal/storagebudget"
)

// PeopleStage is immutable local staging data, scoped by CopyID and ID.
// ConfigHash binds the captured server configuration without retaining a token.
type PeopleStage struct {
	ID            string `json:"id"`
	Hash          string `json:"hash"`
	Name          string `json:"name"`
	MIME          string `json:"mime"`
	ConfigHash    string `json:"config_hash"`
	OwnerInstance string `json:"owner_instance,omitempty"`
	Data          []byte `json:"data"`
}

var ErrPeopleStage = errors.New("team: invalid or unavailable attachment stage")
var ErrPeopleStageMissing = errors.New("team: attachment record unavailable")

// ValidPeopleOwnerInstance accepts the native server's opaque instance label.
// Its authority comes from the app's exact current-node check, not its shape.
func ValidPeopleOwnerInstance(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Tests hold a staged write before publication to check crash visibility.
var peopleStageAfterWrite = func() {}

// peopleRecordIO preserves descriptor-rooted operations while allowing the
// existing atomic-write failure paths to be exercised with synthetic faults.
var peopleRecordIO = struct {
	mkdir    func(*os.Root, string, os.FileMode) error
	openFile func(*os.Root, string, int, os.FileMode) (*os.File, error)
	write    func(*os.File, []byte) (int, error)
	sync     func(*os.File) error
	close    func(*os.File) error
	link     func(*os.Root, string, string) error
	remove   func(*os.Root, string) error
	openDir  func(*os.Root) (*os.File, error)
	lstat    func(*os.Root, string) (os.FileInfo, error)
	openRead func(*os.Root, string) (*os.File, error)
	stat     func(*os.File) (os.FileInfo, error)
	read     func(io.Reader) ([]byte, error)
	readDir  func(*os.File) ([]os.DirEntry, error)
	walk     func(fs.FS, string, fs.WalkDirFunc) error
}{
	mkdir: func(r *os.Root, name string, mode os.FileMode) error { return r.Mkdir(name, mode) },
	openFile: func(r *os.Root, name string, flag int, mode os.FileMode) (*os.File, error) {
		return r.OpenFile(name, flag, mode)
	},
	write:    func(f *os.File, p []byte) (int, error) { return f.Write(p) },
	sync:     func(f *os.File) error { return f.Sync() },
	close:    func(f *os.File) error { return f.Close() },
	link:     func(r *os.Root, old, name string) error { return r.Link(old, name) },
	remove:   func(r *os.Root, name string) error { return r.Remove(name) },
	openDir:  func(r *os.Root) (*os.File, error) { return r.Open(".") },
	lstat:    func(r *os.Root, name string) (os.FileInfo, error) { return r.Lstat(name) },
	openRead: func(r *os.Root, name string) (*os.File, error) { return r.Open(name) },
	stat:     func(f *os.File) (os.FileInfo, error) { return f.Stat() },
	read:     io.ReadAll,
	readDir:  func(f *os.File) ([]os.DirEntry, error) { return f.ReadDir(-1) },
	walk:     fs.WalkDir,
}

func validPeopleStage(s PeopleStage) bool {
	if !validHex(s.ID, 16) || !validHex(s.Hash, 64) || !validHex(s.ConfigHash, 64) || len(s.Data) == 0 || len(s.Data) > 4<<20 || s.Name == "" || len(s.Name) > 255 || !utf8.ValidString(s.Name) || s.Name == "." || s.Name == ".." || strings.ContainsAny(s.Name, "/\\") || len(s.MIME) > 255 {
		return false
	}
	for _, r := range s.Name {
		if unicode.IsControl(r) {
			return false
		}
	}
	if _, _, err := mime.ParseMediaType(s.MIME); err != nil {
		return false
	}
	if s.OwnerInstance != "" && !ValidPeopleOwnerInstance(s.OwnerInstance) {
		return false
	}
	sum := sha256.Sum256(s.Data)
	return hex.EncodeToString(sum[:]) == s.Hash
}

// peopleStageDir walks descriptor-backed roots and rejects symlink components.
// No network call occurs while the caller holds the copy or budget lock.
func peopleStageDir(dataDir string, id CopyID, create bool) (*os.Root, error) {
	return peopleRecordDir(dataDir, id, create, "people-stages")
}

func peopleRecordDir(dataDir string, id CopyID, create bool, parts ...string) (*os.Root, error) {
	if _, err := copyPath(dataDir, id); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, ErrPeopleStage
	}
	for _, name := range append([]string{CopiesDir, id.Server, id.Owner, id.Chat, id.Generation}, parts...) {
		if create {
			if err := peopleRecordIO.mkdir(root, name, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
				root.Close()
				return nil, ErrPeopleStage
			}
		}
		before, err := root.Lstat(name)
		if !create && errors.Is(err, fs.ErrNotExist) {
			root.Close()
			return nil, ErrPeopleStageMissing
		}
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm() != 0700 {
			root.Close()
			return nil, ErrPeopleStage
		}
		child, err := copyOps.openChild(root, name)
		if err != nil {
			root.Close()
			return nil, ErrPeopleStage
		}
		after, err := copyOps.stat(child, ".")
		if err != nil || !os.SameFile(before, after) {
			child.Close()
			root.Close()
			return nil, ErrPeopleStage
		}
		// A visible directory may be left by a failed sync or an interrupted
		// creation. Repair each parent link on reads as well as writes.
		dir, err := peopleRecordIO.openDir(root)
		if err != nil {
			child.Close()
			root.Close()
			return nil, ErrPeopleStage
		}
		err = peopleRecordIO.sync(dir)
		_ = dir.Close()
		if err != nil {
			child.Close()
			root.Close()
			return nil, ErrPeopleStage
		}
		root.Close()
		root = child
	}
	return root, nil
}

// SavePeopleStage atomically admits one bounded, verified local stage. A stage
// identifier cannot overwrite a prior stage. Budgets include its encoded size.
func SavePeopleStage(dataDir string, id CopyID, stage PeopleStage) error {
	return savePeopleRecord(dataDir, id, stage, "people-stages")
}

// ReadPeopleCopyInstance reads the identity without creating missing metadata.
func ReadPeopleCopyInstance(dataDir string, id CopyID) (string, error) {
	const recordID = "0000000000000000"
	path, err := copyPath(dataDir, id)
	if err != nil {
		return "", err
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	root, err := peopleRecordDir(dataDir, id, false, "people-identity")
	if err != nil {
		return "", err
	}
	defer root.Close()
	record, err := readPeopleRecordFile(root, recordID)
	if err != nil {
		return "", err
	}
	value := string(record.Data)
	if !validHex(value, 32) || record.Name != "identity" || record.MIME != "text/plain" || record.ConfigHash != strings.Repeat("0", 64) || record.OwnerInstance != "" {
		return "", ErrPeopleStage
	}
	// A visible name can follow a failed directory sync. Re-establish its
	// durability before returning it, including after a restart or retry.
	dir, err := peopleRecordIO.openDir(root)
	if err != nil {
		return "", ErrPeopleStage
	}
	err = peopleRecordIO.sync(dir)
	_ = dir.Close()
	if err != nil {
		return "", ErrPeopleStage
	}
	return value, nil
}

// EnsurePeopleCopyInstance returns the durable identity of this local copy.
// The identity is removed with the copy and is not an authorization credential.
func EnsurePeopleCopyInstance(dataDir string, id CopyID) (string, error) {
	const recordID = "0000000000000000"
	value, err := ReadPeopleCopyInstance(dataDir, id)
	if err == nil || !errors.Is(err, ErrPeopleStageMissing) {
		return value, err
	}
	var random [16]byte
	_, _ = rand.Read(random[:]) // crypto/rand.Read fills the buffer or terminates the process.
	data := []byte(hex.EncodeToString(random[:]))
	sum := sha256.Sum256(data)
	record := PeopleStage{ID: recordID, Hash: hex.EncodeToString(sum[:]), Name: "identity", MIME: "text/plain", ConfigHash: strings.Repeat("0", 64), Data: data}
	if err := savePeopleRecord(dataDir, id, record, "people-identity"); err != nil {
		if value, readErr := ReadPeopleCopyInstance(dataDir, id); readErr == nil {
			return value, nil
		}
		return "", err
	}
	return string(data), nil
}

func savePeopleRecord(dataDir string, id CopyID, stage PeopleStage, parts ...string) error {
	if !validPeopleStage(stage) {
		return ErrPeopleStage
	}
	path, err := copyPath(dataDir, id)
	if err != nil {
		return err
	}
	// PeopleStage has only strings and a byte slice, which always encode.
	encoded, _ := json.Marshal(stage)
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	release, err := storagebudget.ReserveSerializedAfter(dataDir, "", int64(len(encoded)), func() error { return recoverPeopleTemps(dataDir, id) })
	if err != nil {
		return err
	}
	defer release()
	return savePeopleRecordLocked(dataDir, id, stage, encoded, parts...)
}

// recoverPeopleTemps runs only while both the copy and global storage locks
// are held, so no active writer can own a matching temporary in this copy.
func recoverPeopleTemps(dataDir string, id CopyID) error {
	root, err := peopleRecordDir(dataDir, id, false)
	if errors.Is(err, ErrPeopleStageMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	for _, tree := range []string{"people-stages", "people-records", "people-pending", "people-completed", "people-identity"} {
		err = fs.WalkDir(root.FS(), tree, func(name string, entry fs.DirEntry, walkErr error) error {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			if walkErr != nil {
				return ErrPeopleStage
			}
			if entry.Type().IsRegular() && peopleStageTemp(entry.Name()) {
				if err := root.Remove(name); err != nil {
					return ErrPeopleStage
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// savePeopleRecordLocked requires the copy lock and serialized budget reserve.
func savePeopleRecordLocked(dataDir string, id CopyID, stage PeopleStage, encoded []byte, parts ...string) error {
	root, err := peopleRecordDir(dataDir, id, true, parts...)
	if err != nil {
		return err
	}
	defer root.Close()
	copyRoot, err := peopleRecordDir(dataDir, id, false)
	if err != nil {
		return err
	}
	var used int64
	count := 0
	for _, tree := range []string{"people-stages", "people-records", "people-pending", "people-completed", "people-identity"} {
		err = peopleRecordIO.walk(copyRoot.FS(), tree, func(name string, entry fs.DirEntry, walkErr error) error {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			if walkErr != nil {
				return ErrPeopleStage
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return ErrPeopleStage
			}
			if peopleStageTemp(entry.Name()) {
				if err := copyRoot.Remove(name); err != nil {
					return ErrPeopleStage
				}
				return nil
			}
			used += info.Size()
			count++
			if count >= 4096 || used+int64(len(encoded)) > 256<<20 {
				return storagebudget.ErrBudget
			}
			return nil
		})
		if err != nil {
			copyRoot.Close()
			return err
		}
	}
	_ = copyRoot.Close()
	name := stage.ID + ".json"
	var nonce [8]byte
	_, _ = rand.Read(nonce[:]) // Go 1.26 fills the buffer or terminates the process.
	temp := stage.ID + "-" + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := peopleRecordIO.openFile(root, temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrPeopleStage
	}
	defer func() {
		f.Close()
		_ = root.Remove(temp)
	}()
	n, err := peopleRecordIO.write(f, encoded)
	if err != nil || n != len(encoded) {
		return ErrPeopleStage
	}
	peopleStageAfterWrite()
	if err := peopleRecordIO.sync(f); err != nil {
		return ErrPeopleStage
	}
	if err := peopleRecordIO.close(f); err != nil {
		return ErrPeopleStage
	}
	if err := peopleRecordIO.link(root, temp, name); err != nil {
		return ErrPeopleStage
	}
	if err := peopleRecordIO.remove(root, temp); err != nil {
		return ErrPeopleStage
	}
	dir, err := peopleRecordIO.openDir(root)
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

func peopleStageTemp(name string) bool {
	return len(name) == 37 && name[16] == '-' && strings.HasSuffix(name, ".tmp") && validHex(name[:16], 16) && validHex(name[17:33], 16)
}

// ReadPeopleStage reads a specified stage only after its directory is durable.
// The caller must still authorize that stage for the current operation.
func ReadPeopleStage(dataDir string, id CopyID, stageID string) (PeopleStage, error) {
	if !validHex(stageID, 16) {
		return PeopleStage{}, ErrPeopleStage
	}
	path, err := copyPath(dataDir, id)
	if err != nil {
		return PeopleStage{}, err
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	root, err := peopleRecordDir(dataDir, id, false, "people-stages")
	if err != nil {
		return PeopleStage{}, err
	}
	defer root.Close()
	return readDurablePeopleRecordFile(root, stageID)
}

// readDurablePeopleRecordFile verifies the record and makes its directory entry durable.
// The caller retains the copy lock and the descriptor-rooted directory.
func readDurablePeopleRecordFile(root *os.Root, stageID string) (PeopleStage, error) {
	record, err := readPeopleRecordFile(root, stageID)
	if err != nil {
		return PeopleStage{}, err
	}
	dir, err := peopleRecordIO.openDir(root)
	if err != nil {
		return PeopleStage{}, ErrPeopleStage
	}
	err = peopleRecordIO.sync(dir)
	_ = dir.Close()
	if err != nil {
		return PeopleStage{}, ErrPeopleStage
	}
	return record, nil
}

func readDurablePeopleRecord(dataDir string, id CopyID, stageID string, parts ...string) (PeopleStage, error) {
	if !validHex(stageID, 16) {
		return PeopleStage{}, ErrPeopleStage
	}
	path, err := copyPath(dataDir, id)
	if err != nil {
		return PeopleStage{}, err
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	root, err := peopleRecordDir(dataDir, id, false, parts...)
	if err != nil {
		return PeopleStage{}, err
	}
	defer root.Close()
	return readDurablePeopleRecordFile(root, stageID)
}

func readPeopleRecordFile(root *os.Root, stageID string) (PeopleStage, error) {
	name := stageID + ".json"
	before, err := peopleRecordIO.lstat(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return PeopleStage{}, ErrPeopleStageMissing
	}
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0600 || before.Size() > 6<<20 {
		return PeopleStage{}, ErrPeopleStage
	}
	f, err := peopleRecordIO.openRead(root, name)
	if err != nil {
		return PeopleStage{}, ErrPeopleStage
	}
	defer f.Close()
	after, err := peopleRecordIO.stat(f)
	if err != nil || !os.SameFile(before, after) {
		return PeopleStage{}, ErrPeopleStage
	}
	raw, err := peopleRecordIO.read(io.LimitReader(f, (6<<20)+1))
	if err != nil || len(raw) > 6<<20 {
		return PeopleStage{}, ErrPeopleStage
	}
	var stage PeopleStage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&stage) != nil || dec.Decode(new(any)) != io.EOF || stage.ID != stageID || !validPeopleStage(stage) {
		return PeopleStage{}, ErrPeopleStage
	}
	return stage, nil
}

// DeletePeopleStage removes a staged file after its association is cancelled.
func DeletePeopleStage(dataDir string, id CopyID, stageID string) error {
	if !validHex(stageID, 16) {
		return ErrPeopleStage
	}
	path, err := copyPath(dataDir, id)
	if err != nil {
		return err
	}
	unlock := lockCopy(filepath.Dir(path))
	defer unlock()
	release, err := storagebudget.Lock(dataDir)
	if err != nil {
		return err
	}
	defer release()
	pinned, err := pendingReferencesLocked(dataDir, id, stageID)
	if err != nil || pinned {
		return ErrPeopleStage
	}
	root, err := peopleStageDir(dataDir, id, false)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Remove(stageID + ".json"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ErrPeopleStage
	}
	return nil
}

// SavePeoplePublished retains bytes under a comment sequence and logical ID.
func SavePeoplePublished(dataDir string, id CopyID, seq uint64, stage PeopleStage) error {
	if seq == 0 || !validPeopleStage(stage) {
		return ErrPeopleStage
	}
	matching := func() (bool, bool) {
		prior, err := ReadPeoplePublished(dataDir, id, seq, stage.ID)
		if err != nil {
			return false, false
		}
		return true, prior.Hash == stage.Hash && prior.Name == stage.Name && prior.MIME == stage.MIME && bytes.Equal(prior.Data, stage.Data)
	}
	if exists, same := matching(); exists {
		if same {
			return nil
		}
		return ErrPeopleStage
	}
	err := savePeopleRecord(dataDir, id, stage, "people-records", "comments", strconv.FormatUint(seq, 10))
	if err != nil {
		if exists, same := matching(); exists && same {
			return nil
		}
	}
	return err
}

// ReadPeoplePublished reads an already-authorized retained copy association.
func ReadPeoplePublished(dataDir string, id CopyID, seq uint64, attachmentID string) (PeopleStage, error) {
	if seq == 0 {
		return PeopleStage{}, ErrPeopleStage
	}
	return readDurablePeopleRecord(dataDir, id, attachmentID, "people-records", "comments", strconv.FormatUint(seq, 10))
}
