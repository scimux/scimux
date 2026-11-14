package team

import (
	"encoding/json"
	"errors"
	"os"
)

// CopiesDir is the directory of local mirror copies inside the data directory.
const CopiesDir = "team-copies"

const (
	mirrorFile = "mirror.jsonl"
	copyStride = 256
)

// CopyID is the path of one generation's local copy.
type CopyID struct {
	Server     string
	Owner      string
	Chat       string
	Generation string
}

// Sentinel errors from a local copy. None of them quote chat text or a token.
var (
	ErrCopyDamaged = errors.New("team: mirror copy is damaged")
	ErrCopyRange   = errors.New("team: mirror entry does not continue the copy")
	ErrCopyPath    = errors.New("team: mirror copy path is refused")
)

// copyIO is the filesystem seam. Tests replace individual functions.
type copyIO struct {
	readAt  func(*os.File, []byte, int64) (int, error)
	write   func(*os.File, []byte) (int, error)
	chmod   func(string, os.FileMode) error
	open    func(string, int, os.FileMode) (*os.File, error)
	mkdir   func(string, os.FileMode) error
	marshal func(any) ([]byte, error)
}

var copyOps = copyIO{
	readAt:  func(f *os.File, p []byte, off int64) (int, error) { return f.ReadAt(p, off) },
	write:   func(f *os.File, p []byte) (int, error) { return f.Write(p) },
	chmod:   os.Chmod,
	open:    os.OpenFile,
	mkdir:   os.MkdirAll,
	marshal: json.Marshal,
}

// AppendCopy stores entries that continue the copy. Anything else stores nothing.
func AppendCopy(dataDir string, id CopyID, entries []MirrorRecord) error {
	return ErrCopyRange
}

// ReadCopy returns the entries with a number above after. A missing copy is empty.
func ReadCopy(dataDir string, id CopyID, after uint64) ([]MirrorRecord, uint64, error) {
	return nil, 0, ErrCopyDamaged
}
