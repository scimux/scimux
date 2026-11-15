package team

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/scimux/scimux/internal/storagebudget"
)

// CopiesDir is the directory of local mirror copies inside the data directory.
const CopiesDir = "team-copies"

const (
	mirrorFile = "mirror.jsonl"
	// copyStride is the spacing of the in-memory index. For each open copy
	// this process keeps the byte offset of lines 1, 257, 513, and so on.
	copyStride = 256
)

// CopyID selects one generation's local copy.
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
	openRoot  func(string) (*os.Root, error)
	openChild func(*os.Root, string) (*os.Root, error)
	lstat     func(*os.Root, string) (os.FileInfo, error)
	stat      func(*os.Root, string) (os.FileInfo, error)
	remove    func(*os.Root, string) error
	removeAll func(*os.Root, string) error
	readAt    func(*os.File, []byte, int64) (int, error)
	write     func(*os.File, []byte) (int, error)
	chmod     func(string, os.FileMode) error
	open      func(string, int, os.FileMode) (*os.File, error)
	mkdir     func(string, os.FileMode) error
	marshal   func(any) ([]byte, error)
}

var copyOps = copyIO{
	openRoot:  os.OpenRoot,
	openChild: func(root *os.Root, name string) (*os.Root, error) { return root.OpenRoot(name) },
	lstat:     func(root *os.Root, name string) (os.FileInfo, error) { return root.Lstat(name) },
	stat:      func(root *os.Root, name string) (os.FileInfo, error) { return root.Stat(name) },
	remove:    func(root *os.Root, name string) error { return root.Remove(name) },
	removeAll: func(root *os.Root, name string) error { return root.RemoveAll(name) },
	readAt:    func(f *os.File, p []byte, off int64) (int, error) { return f.ReadAt(p, off) },
	write:     func(f *os.File, p []byte) (int, error) { return f.Write(p) },
	chmod:     os.Chmod,
	open:      os.OpenFile,
	mkdir:     os.MkdirAll,
	// encoding/json escapes < inside a raw body. The line is built so the body
	// is copied unchanged.
	marshal: marshalCopyLine,
}

func marshalCopyLine(v any) ([]byte, error) {
	line := v.(copyLine)
	var buf bytes.Buffer
	buf.WriteString(`{"seq":`)
	buf.WriteString(strconv.FormatUint(line.Seq, 10))
	buf.WriteString(`,"key":`)
	buf.WriteString(strconv.Quote(line.Key))
	buf.WriteString(`,"author":`)
	buf.WriteString(strconv.Quote(line.Author))
	buf.WriteString(`,"at":`)
	buf.WriteString(strconv.Quote(line.At))
	buf.WriteString(`,"body":`)
	buf.Write(line.Body)
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// copyMeta is the index of one copy in this process.
type copyMeta struct {
	mu      sync.Mutex
	ready   bool
	damaged bool
	last    uint64
	size    int64
	offsets []int64
}

var copyMetas sync.Map

func metaFor(path string) *copyMeta {
	if v, ok := copyMetas.Load(path); ok {
		return v.(*copyMeta)
	}
	actual, _ := copyMetas.LoadOrStore(path, &copyMeta{})
	return actual.(*copyMeta)
}

type copyLine struct {
	Seq    uint64          `json:"seq"`
	Key    string          `json:"key"`
	Author string          `json:"author"`
	At     string          `json:"at"`
	Body   json.RawMessage `json:"body"`
}

// AppendCopy stores the entries that continue the copy.
// A repeated suffix, a gap, a damaged copy, or a refused reservation stores nothing.
func AppendCopy(dataDir string, id CopyID, entries []MirrorRecord) error {
	path, err := copyPath(dataDir, id)
	if err != nil {
		return err
	}
	m := metaFor(path)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.sync(path); err != nil {
		return err
	}
	fresh, err := freshEntries(m.last, entries)
	if err != nil || len(fresh) == 0 {
		return err
	}
	body, marks, end, err := encodeEntries(fresh, m.size)
	if err != nil {
		return err
	}
	release, err := storagebudget.Reserve(dataDir, "", int64(len(body)))
	if err != nil {
		return err
	}
	defer release()
	if err := writeCopy(path, body); err != nil {
		return err
	}
	m.offsets = append(m.offsets, marks...)
	m.size = end
	m.last = fresh[len(fresh)-1].Seq
	m.ready = true
	m.damaged = false
	return nil
}

// ReadCopy returns the stored entries whose number is above after.
// A missing copy is an empty result and is not created.
func ReadCopy(dataDir string, id CopyID, after uint64, maxEntries int, maxBodyBytes int) ([]MirrorRecord, uint64, error) {
	path, err := copyPath(dataDir, id)
	if err != nil {
		return nil, 0, err
	}
	m := metaFor(path)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.sync(path); err != nil {
		return nil, 0, err
	}
	if after >= m.last {
		return []MirrorRecord{}, m.last, nil
	}
	off := m.offsets[int(after/copyStride)]
	f, err := copyOps.open(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(io.NewSectionReader(copyReaderAt{f}, off, m.size-off), 4096)
	seq := (after/copyStride)*copyStride + 1
	entries := []MirrorRecord{}
	bodyBytes := 0
	for seq <= m.last {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				m.damaged = true
				return nil, 0, ErrCopyDamaged
			}
			return nil, 0, err
		}
		rec, ok := acceptLine(line[:len(line)-1], seq)
		if !ok {
			m.damaged = true
			return nil, 0, ErrCopyDamaged
		}
		seq++
		if rec.Seq <= after {
			continue
		}
		entries = append(entries, rec)
		bodyBytes += len(rec.Body)
		if len(entries) >= maxEntries || bodyBytes > maxBodyBytes {
			break
		}
	}
	return entries, m.last, nil
}

type copyReaderAt struct{ f *os.File }

func (r copyReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := copyOps.readAt(r.f, p, off)
	if n < len(p) && err == nil {
		return n, ErrCopyDamaged
	}
	return n, err
}

func copyPath(dataDir string, id CopyID) (string, error) {
	if !validHex(id.Server, 32) || !validHex(id.Owner, 32) || !validHex(id.Chat, 16) || !validHex(id.Generation, 16) {
		return "", ErrCopyPath
	}
	return filepath.Join(dataDir, CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, mirrorFile), nil
}

func (m *copyMeta) sync(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			m.ready = true
			m.damaged = false
			m.last = 0
			m.size = 0
			m.offsets = nil
			return nil
		}
		return err
	}
	if m.ready && !m.damaged && m.size == info.Size() {
		return nil
	}
	return m.scan(path)
}

func (m *copyMeta) scan(path string) error {
	f, err := copyOps.open(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	r := bufio.NewReaderSize(io.NewSectionReader(copyReaderAt{f}, 0, info.Size()), 4096)
	m.ready = false
	m.damaged = false
	m.offsets = nil
	m.last = 0
	m.size = info.Size()
	var off int64
	for off < m.size {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				m.damaged = true
				return ErrCopyDamaged
			}
			return err
		}
		seq := m.last + 1
		if _, ok := acceptLine(line[:len(line)-1], seq); !ok {
			m.damaged = true
			return ErrCopyDamaged
		}
		if (seq-1)%copyStride == 0 {
			m.offsets = append(m.offsets, off)
		}
		off += int64(len(line))
		m.last = seq
	}
	m.ready = true
	return nil
}

func freshEntries(last uint64, entries []MirrorRecord) ([]MirrorRecord, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	for i, e := range entries {
		if e.Seq == 0 || len(e.Body) == 0 || !json.Valid(e.Body) {
			return nil, ErrCopyRange
		}
		if i > 0 && e.Seq != entries[i-1].Seq+1 {
			return nil, ErrCopyRange
		}
	}
	if entries[0].Seq > last+1 {
		return nil, ErrCopyRange
	}
	if entries[len(entries)-1].Seq <= last {
		return nil, nil
	}
	skip := 0
	for skip < len(entries) && entries[skip].Seq <= last {
		skip++
	}
	return entries[skip:], nil
}

func encodeEntries(entries []MirrorRecord, start int64) ([]byte, []int64, int64, error) {
	var buf bytes.Buffer
	var marks []int64
	off := start
	for _, e := range entries {
		if (e.Seq-1)%copyStride == 0 {
			marks = append(marks, off)
		}
		line, err := copyOps.marshal(copyLine{Seq: e.Seq, Key: e.Key, Author: e.Author, At: e.At, Body: e.Body})
		if err != nil {
			return nil, nil, 0, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
		off += int64(len(line) + 1)
	}
	return buf.Bytes(), marks, off, nil
}

func writeCopy(path string, body []byte) error {
	if err := ensureCopyDirs(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := copyOps.open(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := copyOps.chmod(path, 0o600); err != nil {
		return err
	}
	n, err := copyOps.write(f, body)
	if err != nil {
		return err
	}
	if n != len(body) {
		return fmt.Errorf("team: write mirror copy: %w", io.ErrShortWrite)
	}
	return nil
}

func ensureCopyDirs(dir string) error {
	if err := copyOps.mkdir(dir, 0o700); err != nil {
		return err
	}
	for p := dir; ; p = filepath.Dir(p) {
		if err := copyOps.chmod(p, 0o700); err != nil {
			return err
		}
		if filepath.Base(p) == CopiesDir {
			return nil
		}
	}
}

func acceptLine(line []byte, expect uint64) (MirrorRecord, bool) {
	rec, ok := parseCopyLine(line)
	if !ok || rec.Seq != expect {
		return MirrorRecord{}, false
	}
	return rec, true
}

func parseCopyLine(line []byte) (MirrorRecord, bool) {
	if !utf8.Valid(line) {
		return MirrorRecord{}, false
	}
	var doc struct {
		Seq    *uint64         `json:"seq"`
		Key    string          `json:"key"`
		Author string          `json:"author"`
		At     string          `json:"at"`
		Body   json.RawMessage `json:"body"`
	}
	if err := decodeJSON(line, &doc); err != nil || doc.Seq == nil {
		return MirrorRecord{}, false
	}
	if *doc.Seq == 0 || !validEntryKey(doc.Key) || !validHex(doc.Author, 32) || !validProtocolTime(doc.At) || len(doc.Body) == 0 {
		return MirrorRecord{}, false
	}
	return MirrorRecord{
		Seq: *doc.Seq, Key: doc.Key, Author: doc.Author, At: doc.At,
		Body: append(json.RawMessage(nil), doc.Body...),
	}, true
}

// RemoveCopy removes a guest copy.
func RemoveCopy(dataDir string, id CopyID) error { return ErrCopyPath }
