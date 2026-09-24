// Package referencemedia owns immutable, content-addressed image captures used
// by bookmarks and embedded note references. It is deliberately a leaf: the
// application resolves source turns and supplies only bytes already managed by
// scimux; this package never reads a workspace path or fetches a URL.
package referencemedia

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/scimux/scimux/internal/privatefs"
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/storagebudget"
)

const (
	Version          = 1
	StateReady       = "ready"
	StateUnavailable = "unavailable"
)

var (
	ErrInvalid  = errors.New("reference media: invalid input")
	ErrTooLarge = errors.New("reference media: capture too large")
	ErrNotFound = errors.New("reference media: not found")
	ErrCorrupt  = errors.New("reference media: corrupt immutable entry")
	hexID       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	keyRE       = regexp.MustCompile(`^(asset:[A-Za-z0-9_]+|attachment:[^/\\]+)$`)
	createTemp  = os.CreateTemp
	secureFile  = privatefs.SecureOpenedFile
	writeFile   = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	syncFile    = func(f *os.File) error { return f.Sync() }
	closeFile   = func(f *os.File) error { return f.Close() }
	linkFile    = os.Link
	syncParent  = sessionlog.SyncParentDir
	openRoot    = os.OpenRoot
	openAtRoot  = func(root *os.Root, name string) (*os.File, error) { return root.Open(name) }
	openRegular = os.Open
	statOpened  = func(f *os.File) (os.FileInfo, error) { return f.Stat() }
	readStream  = ReadBounded
)

type Source struct {
	UID     string `json:"uid"`
	Segment int    `json:"segment"`
	Record  int    `json:"record"`
}

type Item struct {
	ID    string `json:"id"`
	Key   string `json:"key"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type Media struct {
	Version   int    `json:"version"`
	CaptureID string `json:"capture_id"`
	Items     []Item `json:"items"`
}

type CaptureItem struct {
	Key   string
	Name  string
	MIME  string
	State string
	Data  []byte
}

type Limits struct {
	MaxItems         int
	MaxImageBytes    int64
	MaxTotalBytes    int64
	MaxManifestBytes int64
}

func DefaultLimits() Limits {
	return Limits{MaxItems: 32, MaxImageBytes: 50 << 20, MaxTotalBytes: 50 << 20, MaxManifestBytes: 64 << 10}
}

type Store struct {
	Root   string
	Limits Limits
	mu     sync.Mutex
}

func New(root string) *Store { return &Store{Root: root, Limits: DefaultLimits()} }

type manifestItem struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	State  string `json:"state"`
	MIME   string `json:"mime,omitempty"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

type manifest struct {
	Version int            `json:"version"`
	Source  Source         `json:"source"`
	Text    string         `json:"text"`
	Items   []manifestItem `json:"items"`
}

func (s *Store) Capture(source Source, text string, input []CaptureItem) (Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limits := s.Limits
	if limits.MaxItems <= 0 || limits.MaxImageBytes <= 0 || limits.MaxTotalBytes <= 0 || limits.MaxManifestBytes <= 0 {
		return Media{}, fmt.Errorf("%w: invalid limits", ErrInvalid)
	}
	if source.Segment < 0 || source.Record < 0 || strings.TrimSpace(source.UID) == "" {
		return Media{}, fmt.Errorf("%w: source", ErrInvalid)
	}
	seen := make(map[string]bool)
	items := make([]manifestItem, 0, len(input))
	var total int64
	for _, in := range input {
		if seen[in.Key] {
			continue
		}
		seen[in.Key] = true
		if len(items) == limits.MaxItems {
			return Media{}, ErrTooLarge
		}
		mi, err := normalizeItem(in, limits.MaxImageBytes)
		if err != nil {
			return Media{}, err
		}
		total += mi.Size
		if total > limits.MaxTotalBytes {
			return Media{}, ErrTooLarge
		}
		items = append(items, mi)
	}
	m := manifest{Version: Version, Source: source, Text: text, Items: items}
	canonical := marshalManifest(m)
	if int64(len(canonical)) > limits.MaxManifestBytes {
		return Media{}, ErrTooLarge
	}
	captureID := digest(canonical)
	if err := s.ensureDirs(); err != nil {
		return Media{}, err
	}

	// Publication hard-links each temporary file into place, so the temporary
	// and final names share one allocation. Reserve each unique missing blob and
	// the missing manifest once. The shared lock remains held through publication.
	var incoming int64
	missingBlobs := make(map[string]bool)
	for _, mi := range items {
		if mi.State != StateReady {
			continue
		}
		path := filepath.Join(s.Root, "blobs", mi.SHA256)
		if err := validateExisting(path, inputDataForKey(input, mi.Key), mi.SHA256); errors.Is(err, os.ErrNotExist) {
			if !missingBlobs[mi.SHA256] {
				incoming += mi.Size
				missingBlobs[mi.SHA256] = true
			}
		} else if err != nil {
			return Media{}, err
		}
	}
	manifestPath := filepath.Join(s.Root, "captures", captureID+".json")
	if err := validateExisting(manifestPath, canonical, captureID); err == nil {
		return publicMedia(captureID, m.Items), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Media{}, err
	}
	incoming += int64(len(canonical))
	release, err := storagebudget.Reserve(filepath.Dir(s.Root), "", incoming)
	if err != nil {
		return Media{}, err
	}
	defer release()
	for _, mi := range items {
		if mi.State != StateReady {
			continue
		}
		if err := publishImmutable(filepath.Join(s.Root, "blobs", mi.SHA256), inputDataForKey(input, mi.Key), mi.SHA256); err != nil {
			return Media{}, err
		}
	}
	if err := publishImmutable(manifestPath, canonical, captureID); err != nil {
		return Media{}, err
	}
	return publicMedia(captureID, m.Items), nil
}

func normalizeItem(in CaptureItem, max int64) (manifestItem, error) {
	name := sessionlog.SanitizeAssetName(in.Name)
	if !keyRE.MatchString(in.Key) || name != in.Name || strings.ContainsAny(name, "\x00\r\n") {
		return manifestItem{}, fmt.Errorf("%w: descriptor", ErrInvalid)
	}
	if in.State == StateUnavailable {
		if len(in.Data) != 0 || in.MIME != "" {
			return manifestItem{}, fmt.Errorf("%w: unavailable payload", ErrInvalid)
		}
		return manifestItem{Key: in.Key, Name: name, State: StateUnavailable}, nil
	}
	if in.State != "" && in.State != StateReady {
		return manifestItem{}, fmt.Errorf("%w: state", ErrInvalid)
	}
	if int64(len(in.Data)) > max {
		return manifestItem{}, ErrTooLarge
	}
	if !validRaster(in.MIME, in.Data) || mimeForName(name) != in.MIME {
		return manifestItem{}, fmt.Errorf("%w: raster type", ErrInvalid)
	}
	return manifestItem{Key: in.Key, Name: name, State: StateReady, MIME: in.MIME, Size: int64(len(in.Data)), SHA256: digest(in.Data)}, nil
}

func validRaster(mime string, b []byte) bool {
	switch mime {
	case "image/png":
		return len(b) >= 8 && bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n"))
	case "image/jpeg":
		return len(b) >= 3 && bytes.Equal(b[:3], []byte{0xff, 0xd8, 0xff})
	case "image/gif":
		return len(b) >= 6 && (bytes.Equal(b[:6], []byte("GIF87a")) || bytes.Equal(b[:6], []byte("GIF89a")))
	case "image/webp":
		return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP"
	default:
		return false
	}
}

func mimeForName(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return ""
	}
}

func inputDataForKey(in []CaptureItem, key string) []byte {
	for _, v := range in {
		if v.Key == key {
			return v.Data
		}
	}
	return nil
}

func (s *Store) ensureDirs() error {
	if s.Root == "" {
		return fmt.Errorf("%w: empty root", ErrInvalid)
	}
	if err := privatefs.EnsureDir(s.Root, 0o700); err != nil {
		return err
	}
	if err := privatefs.EnsureDir(filepath.Join(s.Root, "blobs"), 0o700); err != nil {
		return err
	}
	return privatefs.EnsureDir(filepath.Join(s.Root, "captures"), 0o700)
}

func publishImmutable(path string, data []byte, wantDigest string) error {
	if err := validateExisting(path, data, wantDigest); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := createTemp(dir, ".capture-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmpName)
		}
	}()
	if err := secureFile(tmpName, tmp, 0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err = writeFile(tmp, data); err == nil {
		err = syncFile(tmp)
	}
	if closeErr := closeFile(tmp); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// Link publishes without replacing an immutable entry. A concurrent winner
	// is accepted only after byte/digest validation.
	err = linkFile(tmpName, path)
	if errors.Is(err, os.ErrExist) {
		return validateExisting(path, data, wantDigest)
	}
	if err != nil {
		return err
	}
	if err := syncParent(path); err != nil {
		return err
	}
	remove = true
	return nil
}

func validateExisting(path string, expected []byte, wantDigest string) error {
	b, err := readRegularFile(path, int64(len(expected)))
	if err != nil {
		return err
	}
	if digest(b) != wantDigest || !bytes.Equal(b, expected) {
		return ErrCorrupt
	}
	return nil
}

func (s *Store) Load(captureID string) (Media, string, Source, error) {
	m, err := s.loadManifest(captureID)
	if err != nil {
		return Media{}, "", Source{}, err
	}
	return publicMedia(captureID, m.Items), m.Text, m.Source, nil
}

func (s *Store) loadManifest(captureID string) (manifest, error) {
	if !hexID.MatchString(captureID) {
		return manifest{}, ErrInvalid
	}
	b, err := readRooted(s.Root, filepath.Join("captures", captureID+".json"), s.Limits.MaxManifestBytes)
	if errors.Is(err, os.ErrNotExist) {
		return manifest{}, ErrNotFound
	}
	if err != nil {
		return manifest{}, err
	}
	if digest(b) != captureID {
		return manifest{}, ErrCorrupt
	}
	var m manifest
	if json.Unmarshal(b, &m) != nil || m.Version != Version || m.Source.UID == "" {
		return manifest{}, ErrCorrupt
	}
	canonical := marshalManifest(m)
	if !bytes.Equal(canonical, b) {
		return manifest{}, ErrCorrupt
	}
	if len(m.Items) > s.Limits.MaxItems {
		return manifest{}, ErrCorrupt
	}
	var total int64
	for _, item := range m.Items {
		if item.State == StateReady {
			if !hexID.MatchString(item.SHA256) || item.Size < 0 || item.Size > s.Limits.MaxImageBytes || mimeForName(item.Name) != item.MIME {
				return manifest{}, ErrCorrupt
			}
			total += item.Size
			if total > s.Limits.MaxTotalBytes {
				return manifest{}, ErrCorrupt
			}
		} else if item.State != StateUnavailable {
			return manifest{}, ErrCorrupt
		}
	}
	return m, nil
}

// marshalManifest is infallible because manifest is a closed structure of
// primitive JSON values. Keeping that fact in the type-specific encoder avoids
// a fictitious json.Marshal error path while retaining deterministic field and
// item order for the capture hash.
func marshalManifest(m manifest) []byte {
	b := append([]byte(`{"version":`), strconv.Itoa(m.Version)...)
	b = append(b, `,"source":{"uid":`...)
	b = appendJSONString(b, m.Source.UID)
	b = append(b, `,"segment":`...)
	b = strconv.AppendInt(b, int64(m.Source.Segment), 10)
	b = append(b, `,"record":`...)
	b = strconv.AppendInt(b, int64(m.Source.Record), 10)
	b = append(b, `},"text":`...)
	b = appendJSONString(b, m.Text)
	b = append(b, `,"items":[`...)
	for i, item := range m.Items {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"key":`...)
		b = appendJSONString(b, item.Key)
		b = append(b, `,"name":`...)
		b = appendJSONString(b, item.Name)
		b = append(b, `,"state":`...)
		b = appendJSONString(b, item.State)
		if item.MIME != "" {
			b = append(b, `,"mime":`...)
			b = appendJSONString(b, item.MIME)
		}
		if item.Size != 0 {
			b = append(b, `,"size":`...)
			b = strconv.AppendInt(b, item.Size, 10)
		}
		if item.SHA256 != "" {
			b = append(b, `,"sha256":`...)
			b = appendJSONString(b, item.SHA256)
		}
		b = append(b, '}')
	}
	return append(b, ']', '}')
}

func appendJSONString(dst []byte, value string) []byte {
	dst = append(dst, '"')
	for _, r := range value {
		switch r {
		case '\\', '"':
			dst = append(dst, '\\', byte(r))
		case '\b':
			dst = append(dst, `\b`...)
		case '\f':
			dst = append(dst, `\f`...)
		case '\n':
			dst = append(dst, `\n`...)
		case '\r':
			dst = append(dst, `\r`...)
		case '\t':
			dst = append(dst, `\t`...)
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				dst = append(dst, '\\', 'u', '0', '0', hex[byte(r)>>4], hex[byte(r)&15])
			} else {
				dst = utf8.AppendRune(dst, r)
			}
		}
	}
	return append(dst, '"')
}

func (s *Store) ReadAsset(captureID, itemID string) ([]byte, string, error) {
	if !hexID.MatchString(captureID) {
		return nil, "", ErrInvalid
	}
	i, err := strconv.Atoi(itemID)
	if err != nil || i < 0 || strconv.Itoa(i) != itemID {
		return nil, "", ErrInvalid
	}
	m, err := s.loadManifest(captureID)
	if err != nil {
		return nil, "", err
	}
	if i >= len(m.Items) || m.Items[i].State != StateReady {
		return nil, "", ErrNotFound
	}
	item := m.Items[i]
	data, err := readRooted(s.Root, filepath.Join("blobs", item.SHA256), s.Limits.MaxImageBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) != item.Size || digest(data) != item.SHA256 || !validRaster(item.MIME, data) {
		return nil, "", ErrCorrupt
	}
	return data, item.MIME, nil
}

func readRooted(rootDir, name string, max int64) ([]byte, error) {
	if max < 0 || !filepath.IsLocal(name) {
		return nil, ErrCorrupt
	}
	rootInfo, err := os.Lstat(rootDir)
	if err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, ErrCorrupt
	}
	root, err := openRoot(rootDir)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(filepath.Clean(name), string(filepath.Separator))
	for i := range parts {
		part := filepath.Join(parts[:i+1]...)
		info, statErr := root.Lstat(part)
		if statErr != nil {
			_ = root.Close()
			return nil, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !info.IsDir()) || (i == len(parts)-1 && !info.Mode().IsRegular()) {
			_ = root.Close()
			return nil, ErrCorrupt
		}
	}
	f, err := openAtRoot(root, name)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	data, err := readStream(f, max)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if closeErr := root.Close(); err == nil {
		err = closeErr
	}
	if errors.Is(err, ErrTooLarge) {
		return nil, ErrCorrupt
	}
	return data, err
}

func readRegularFile(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > max {
		return nil, ErrCorrupt
	}
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	opened, statErr := statOpened(f)
	if statErr != nil || !os.SameFile(info, opened) {
		_ = f.Close()
		return nil, ErrCorrupt
	}
	data, err := readStream(f, max)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if errors.Is(err, ErrTooLarge) {
		return nil, ErrCorrupt
	}
	return data, err
}

func publicMedia(id string, items []manifestItem) Media {
	out := Media{Version: Version, CaptureID: id, Items: make([]Item, len(items))}
	for i, item := range items {
		out.Items[i] = Item{ID: strconv.Itoa(i), Key: item.Key, Name: item.Name, State: item.State}
	}
	return out
}

func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

// ReadBounded reads at most max bytes, rejecting a stream with one byte more.
func ReadBounded(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, ErrTooLarge
	}
	return b, nil
}
