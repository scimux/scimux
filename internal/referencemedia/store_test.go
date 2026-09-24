package referencemedia

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/scimux/scimux/internal/storagebudget"
)

var png = []byte("\x89PNG\r\n\x1a\nsynthetic")
var jpeg = []byte("\xff\xd8\xff\xe0synthetic")
var gif = []byte("GIF89asynthetic")
var webp = []byte("RIFF\x04\x00\x00\x00WEBPsynthetic")

func testStore(t *testing.T) *Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "reference-media"))
}

func TestCaptureIsCanonicalDurableAndIdempotent(t *testing.T) {
	s := testStore(t)
	src := Source{UID: "u1", Segment: 0, Record: 0}
	in := []CaptureItem{{Key: "asset:a_1", Name: "chart.png", MIME: "image/png", Data: png}}
	one, err := s.Capture(src, "before ![chart](scimux-asset:a_1) after", in)
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.Capture(src, "before ![chart](scimux-asset:a_1) after", in)
	if err != nil {
		t.Fatal(err)
	}
	if one.CaptureID != two.CaptureID || len(one.Items) != 1 || one.Items[0].ID != "0" {
		t.Fatalf("non-canonical captures: %#v %#v", one, two)
	}
	got, mime, err := s.ReadAsset(one.CaptureID, "0")
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/png" || !bytes.Equal(got, png) {
		t.Fatalf("asset = %q %q", got, mime)
	}

	// Restart reconstruction reads the immutable manifest and blob.
	loaded, text, source, err := New(s.Root).Load(one.CaptureID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CaptureID != one.CaptureID || text == "" || source != src {
		t.Fatalf("load = %#v %q %#v", loaded, text, source)
	}
}

func TestManifestCanonicalJSONEscapesAllStringForms(t *testing.T) {
	s := testStore(t)
	special := "quote\" slash\\ back\b form\f line\n return\r tab\t control\x01 snow雪"
	source := Source{UID: special, Segment: 1, Record: 2}
	m, err := s.Capture(source, special, []CaptureItem{{Key: "asset:a", Name: "a.png", MIME: "image/png", Data: png}, {Key: "asset:b", Name: "old.png", State: StateUnavailable}})
	if err != nil {
		t.Fatal(err)
	}
	_, text, gotSource, err := s.Load(m.CaptureID)
	if err != nil || text != special || gotSource != source {
		t.Fatalf("round trip = %q %#v %v", text, gotSource, err)
	}
}

func TestCaptureOrderUnavailableDedupAndChangedBytes(t *testing.T) {
	s := testStore(t)
	src := Source{UID: "u", Segment: 2, Record: 7}
	one, err := s.Capture(src, "x", []CaptureItem{
		{Key: "asset:a", Name: "a.png", MIME: "image/png", Data: png},
		{Key: "asset:a", Name: "ignored.png", MIME: "image/png", Data: png},
		{Key: "attachment:old.jpg", Name: "old.jpg", State: StateUnavailable},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Items) != 2 || one.Items[0].Key != "asset:a" || one.Items[1].State != StateUnavailable {
		t.Fatalf("items = %#v", one.Items)
	}
	two, err := s.Capture(src, "x", []CaptureItem{{Key: "asset:a", Name: "a.png", MIME: "image/png", Data: append(append([]byte{}, png...), 'x')}})
	if err != nil {
		t.Fatal(err)
	}
	if one.CaptureID == two.CaptureID {
		t.Fatal("changed bytes reused capture id")
	}
}

func TestCaptureSupportedTypesAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, mime string
		data       []byte
	}{
		{"x.png", "image/png", png}, {"x.jpg", "image/jpeg", jpeg}, {"x.gif", "image/gif", gif}, {"x.webp", "image/webp", webp},
	} {
		t.Run(tc.mime, func(t *testing.T) {
			s := testStore(t)
			m, err := s.Capture(Source{UID: "u"}, "x", []CaptureItem{{Key: "asset:x", Name: tc.name, MIME: tc.mime, Data: tc.data}})
			if err != nil {
				t.Fatal(err)
			}
			if _, got, err := s.ReadAsset(m.CaptureID, "0"); err != nil || got != tc.mime {
				t.Fatalf("read = %q, %v", got, err)
			}
		})
	}
	s := testStore(t)
	for _, bad := range []CaptureItem{
		{Key: "asset:x", Name: "x.svg", MIME: "image/svg+xml", Data: png},
		{Key: "../x", Name: "x.png", MIME: "image/png", Data: png},
		{Key: "asset:x", Name: "x.png", MIME: "text/html", Data: png},
	} {
		if _, err := s.Capture(Source{UID: "u"}, "x", []CaptureItem{bad}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad item %#v: %v", bad, err)
		}
	}
}

func TestCaptureLimitsAreWholeTransaction(t *testing.T) {
	s := testStore(t)
	s.Limits = Limits{MaxItems: 1, MaxImageBytes: int64(len(png)), MaxTotalBytes: int64(len(png)), MaxManifestBytes: 64 << 10}
	if _, err := s.Capture(Source{UID: "u"}, "x", []CaptureItem{{Key: "asset:a", Name: "a.png", MIME: "image/png", Data: png}}); err != nil {
		t.Fatal(err)
	}
	for name, items := range map[string][]CaptureItem{
		"items": {{Key: "asset:a", Name: "a.png", MIME: "image/png", Data: png}, {Key: "asset:b", Name: "b.png", MIME: "image/png", Data: png}},
		"image": {{Key: "asset:a", Name: "a.png", MIME: "image/png", Data: append(png, 0)}},
	} {
		t.Run(name, func(t *testing.T) {
			before, _ := filepath.Glob(filepath.Join(s.Root, "captures", "*.json"))
			if _, err := s.Capture(Source{UID: "v"}, "x", items); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("err = %v", err)
			}
			after, _ := filepath.Glob(filepath.Join(s.Root, "captures", "*.json"))
			if len(after) != len(before) {
				t.Fatal("published partial manifest")
			}
		})
	}
}

func TestConcurrentCaptureAndCorruptionFailClosed(t *testing.T) {
	s := testStore(t)
	in := []CaptureItem{{Key: "asset:a", Name: "a.png", MIME: "image/png", Data: png}}
	ids := make(chan string, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := s.Capture(Source{UID: "u"}, "x", in)
			if err != nil {
				ids <- "ERR:" + err.Error()
				return
			}
			ids <- m.CaptureID
		}()
	}
	wg.Wait()
	close(ids)
	var id string
	for got := range ids {
		if id == "" {
			id = got
		}
		if got != id {
			t.Fatalf("capture ids differ: %q %q", id, got)
		}
	}
	if err := os.WriteFile(filepath.Join(s.Root, "blobs", digest(png)), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReadAsset(id, "0"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt read = %v", err)
	}
	if _, err := s.Capture(Source{UID: "u"}, "x", in); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt reuse = %v", err)
	}
}

func TestStrictIDsAndUnavailableRead(t *testing.T) {
	s := testStore(t)
	m, err := s.Capture(Source{UID: "u"}, "x", []CaptureItem{{Key: "asset:x", Name: "x.png", State: StateUnavailable}})
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"BAD", "0"}, {m.CaptureID, "00"}, {m.CaptureID, "1"}, {m.CaptureID, "0"}} {
		_, _, err := s.ReadAsset(pair[0], pair[1])
		if err == nil {
			t.Fatalf("ReadAsset(%q,%q) succeeded", pair[0], pair[1])
		}
	}
}

func TestReadsRejectSymlinksAndOversizedManagedFiles(t *testing.T) {
	s := testStore(t)
	m, err := s.Capture(Source{UID: "u"}, "x", []CaptureItem{{Key: "asset:a", Name: "a.png", MIME: "image/png", Data: png}})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(s.Root, "captures", m.CaptureID+".json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, manifestPath); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Load(m.CaptureID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("symlink manifest read = %v", err)
	}

	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, make([]byte, s.Limits.MaxManifestBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Load(m.CaptureID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("oversized manifest read = %v", err)
	}
}

func TestReadAssetRejectsBlobSymlink(t *testing.T) {
	s := testStore(t)
	m, err := s.Capture(Source{UID: "u"}, "x", []CaptureItem{{Key: "asset:a", Name: "a.png", MIME: "image/png", Data: png}})
	if err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(s.Root, "blobs", digest(png))
	outside := filepath.Join(t.TempDir(), "outside.png")
	if err := os.WriteFile(outside, png, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blobPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, blobPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReadAsset(m.CaptureID, "0"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("symlink blob read = %v", err)
	}
}

func TestStoreValidationAndFilesystemErrorBranches(t *testing.T) {
	s := testStore(t)
	s.Limits.MaxItems = 0
	if _, err := s.Capture(Source{UID: "u"}, "x", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid limits = %v", err)
	}
	s.Limits = DefaultLimits()
	for _, source := range []Source{{}, {UID: "u", Segment: -1}, {UID: "u", Record: -1}} {
		if _, err := s.Capture(source, "x", nil); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid source %#v = %v", source, err)
		}
	}
	for _, item := range []CaptureItem{
		{Key: "asset:x", Name: "bad/name.png", MIME: "image/png", Data: png},
		{Key: "asset:x", Name: "x.png", State: StateUnavailable, Data: png},
		{Key: "asset:x", Name: "x.png", State: "future"},
	} {
		if _, err := s.Capture(Source{UID: "u"}, "x", []CaptureItem{item}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid item %#v = %v", item, err)
		}
	}
	s.Limits.MaxManifestBytes = 1
	if _, err := s.Capture(Source{UID: "u"}, "x", nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("manifest limit = %v", err)
	}

	empty := New("")
	if _, err := empty.Capture(Source{UID: "u"}, "x", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty root = %v", err)
	}
	rootFile := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(rootFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(rootFile).Capture(Source{UID: "u"}, "x", nil); err == nil {
		t.Fatal("file root succeeded")
	}

	root := t.TempDir()
	if _, err := readRooted(root, "../outside", 1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("traversal = %v", err)
	}
	if _, err := readRooted(root, "missing/file", 1); err == nil {
		t.Fatal("missing rooted read succeeded")
	}
	if err := os.WriteFile(filepath.Join(root, "parent"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRooted(root, filepath.Join("parent", "child"), 1); err == nil {
		t.Fatal("non-directory parent succeeded")
	}
	if _, err := readRegularFile(root, 1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("directory regular read = %v", err)
	}
}

func TestManifestSemanticCorruptionIsRejected(t *testing.T) {
	write := func(t *testing.T, s *Store, m manifest) string {
		t.Helper()
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		id := digest(b)
		if err := os.MkdirAll(filepath.Join(s.Root, "captures"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.Root, "captures", id+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		return id
	}
	base := manifest{Version: Version, Source: Source{UID: "u"}, Text: "x"}
	for name, mutate := range map[string]func(*manifest){
		"version": func(m *manifest) { m.Version = 2 },
		"source":  func(m *manifest) { m.Source.UID = "" },
		"state":   func(m *manifest) { m.Items = []manifestItem{{Key: "asset:x", Name: "x.png", State: "future"}} },
		"digest": func(m *manifest) {
			m.Items = []manifestItem{{Key: "asset:x", Name: "x.png", State: StateReady, MIME: "image/png", Size: 1, SHA256: "bad"}}
		},
		"mime": func(m *manifest) {
			m.Items = []manifestItem{{Key: "asset:x", Name: "x.jpg", State: StateReady, MIME: "image/png", Size: 1, SHA256: strings.Repeat("a", 64)}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			m := base
			mutate(&m)
			id := write(t, s, m)
			if _, _, _, err := s.Load(id); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Load = %v", err)
			}
		})
	}
	s := testStore(t)
	m := base
	m.Items = make([]manifestItem, DefaultLimits().MaxItems+1)
	id := write(t, s, m)
	if _, _, _, err := s.Load(id); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("too many items = %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestReadBoundedPropagatesReaderFailure(t *testing.T) {
	if _, err := ReadBounded(failingReader{}, 1); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadBounded = %v", err)
	}
}

func TestRemainingManagedReadAndPublicationFailures(t *testing.T) {
	s := testStore(t)
	s.Limits.MaxItems = 2
	s.Limits.MaxTotalBytes = int64(len(png))
	if _, err := s.Capture(Source{UID: "u"}, "x", []CaptureItem{
		{Key: "asset:a", Name: "a.png", MIME: "image/png", Data: png},
		{Key: "asset:b", Name: "b.png", MIME: "image/png", Data: png},
	}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("aggregate limit = %v", err)
	}
	if mimeForName("x.bin") != "" {
		t.Fatal("unsafe extension accepted")
	}
	if inputDataForKey(nil, "missing") != nil {
		t.Fatal("missing input returned data")
	}

	dir := t.TempDir()
	data := []byte("immutable")
	id := digest(data)
	path := filepath.Join(dir, id)
	if err := publishImmutable(path, data, id); err != nil {
		t.Fatal(err)
	}
	if err := publishImmutable(path, data, id); err != nil {
		t.Fatalf("reuse = %v", err)
	}
	if err := publishImmutable(path, []byte("other"), digest([]byte("other"))); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt reuse = %v", err)
	}
	if err := publishImmutable(filepath.Join(dir, "missing", id), data, id); err == nil {
		t.Fatal("missing publication dir succeeded")
	}

	if _, _, _, err := s.Load("bad"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid id = %v", err)
	}
	if _, _, _, err := s.Load(strings.Repeat("a", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id = %v", err)
	}
	rootFile := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(rootFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRooted(rootFile, "x", 10); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("file root = %v", err)
	}
	if _, err := readRooted(filepath.Join(t.TempDir(), "missing"), "x", 10); err == nil {
		t.Fatal("missing root succeeded")
	}
	if _, err := readRooted(dir, id, -1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("negative bound = %v", err)
	}
	if _, err := readRooted(dir, ".", 10); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("directory leaf = %v", err)
	}
	if _, err := readRooted(dir, id, 1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("oversized root read = %v", err)
	}
	if _, err := readRegularFile(path, 1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("oversized regular read = %v", err)
	}

	corruptStore := testStore(t)
	media, err := corruptStore.Capture(Source{UID: "u"}, "same", nil)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(corruptStore.Root, "captures", media.CaptureID+".json")
	if err := os.WriteFile(manifestPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := corruptStore.Capture(Source{UID: "u"}, "same", nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt manifest reuse = %v", err)
	}
	if _, _, err := corruptStore.ReadAsset(strings.Repeat("b", 64), "0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown asset manifest = %v", err)
	}
}

func TestManifestDigestCanonicalTotalAndMissingBlobFailures(t *testing.T) {
	s := testStore(t)
	if err := os.MkdirAll(filepath.Join(s.Root, "captures"), 0o700); err != nil {
		t.Fatal(err)
	}
	badID := strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(s.Root, "captures", badID+".json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Load(badID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("digest mismatch = %v", err)
	}

	m := manifest{Version: Version, Source: Source{UID: "u"}, Text: "x"}
	canonical, _ := json.Marshal(m)
	noncanonical := append(append([]byte{}, canonical...), '\n')
	id := digest(noncanonical)
	if err := os.WriteFile(filepath.Join(s.Root, "captures", id+".json"), noncanonical, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Load(id); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("noncanonical = %v", err)
	}

	sha := digest(png)
	m.Items = []manifestItem{
		{Key: "asset:a", Name: "a.png", State: StateReady, MIME: "image/png", Size: int64(len(png)), SHA256: sha},
		{Key: "asset:b", Name: "b.png", State: StateReady, MIME: "image/png", Size: int64(len(png)), SHA256: sha},
	}
	b, _ := json.Marshal(m)
	id = digest(b)
	if err := os.WriteFile(filepath.Join(s.Root, "captures", id+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	s.Limits.MaxTotalBytes = int64(len(png))
	if _, _, _, err := s.Load(id); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("manifest total = %v", err)
	}

	m.Items = m.Items[:1]
	b, _ = json.Marshal(m)
	id = digest(b)
	if err := os.WriteFile(filepath.Join(s.Root, "captures", id+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	s.Limits = DefaultLimits()
	if _, _, err := s.ReadAsset(id, "0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing blob = %v", err)
	}
}

func TestPublishImmutableInjectedFailuresCleanTemporaryFiles(t *testing.T) {
	origCreate, origSecure := createTemp, secureFile
	origWrite, origSync, origClose := writeFile, syncFile, closeFile
	origLink, origParent := linkFile, syncParent
	t.Cleanup(func() {
		createTemp, secureFile = origCreate, origSecure
		writeFile, syncFile, closeFile = origWrite, origSync, origClose
		linkFile, syncParent = origLink, origParent
	})
	fail := errors.New("injected")
	for _, tc := range []struct {
		name    string
		install func()
	}{
		{"create", func() { createTemp = func(string, string) (*os.File, error) { return nil, fail } }},
		{"secure", func() { secureFile = func(string, *os.File, os.FileMode) error { return fail } }},
		{"write", func() { writeFile = func(*os.File, []byte) (int, error) { return 0, fail } }},
		{"sync", func() { syncFile = func(*os.File) error { return fail } }},
		{"close", func() { closeFile = func(f *os.File) error { _ = f.Close(); return fail } }},
		{"link", func() { linkFile = func(string, string) error { return fail } }},
		{"parent-sync", func() { syncParent = func(string) error { return fail } }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			createTemp, secureFile = origCreate, origSecure
			writeFile, syncFile, closeFile = origWrite, origSync, origClose
			linkFile, syncParent = origLink, origParent
			dir := t.TempDir()
			tc.install()
			data := []byte("x")
			if err := publishImmutable(filepath.Join(dir, digest(data)), data, digest(data)); !errors.Is(err, fail) {
				t.Fatalf("error = %v", err)
			}
			matches, err := filepath.Glob(filepath.Join(dir, ".capture-*"))
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != 0 {
				t.Fatalf("temporary files remain: %v", matches)
			}
		})
	}
}

func TestCaptureMapsStorageBudgetRejection(t *testing.T) {
	s := testStore(t)
	settings := filepath.Join(filepath.Dir(s.Root), "settings.json")
	if err := os.WriteFile(settings, []byte(`{"storage_global_limit_bytes":1,"storage_min_free_bytes":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Capture(Source{UID: "u"}, "manifest", nil); !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("budget rejection = %v", err)
	}
}

func TestCaptureBudgetDeduplicatesIdenticalBlobDescriptors(t *testing.T) {
	input := []CaptureItem{
		{Key: "asset:a", Name: "a.png", MIME: "image/png", Data: png},
		{Key: "asset:b", Name: "b.png", MIME: "image/png", Data: png},
	}
	items := make([]manifestItem, 0, len(input))
	for _, in := range input {
		item, err := normalizeItem(in, DefaultLimits().MaxImageBytes)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	canonical := marshalManifest(manifest{Version: Version, Source: Source{UID: "u"}, Text: "x", Items: items})
	needed := int64(len(png) + len(canonical))

	makeLimited := func(t *testing.T, limit int64) *Store {
		t.Helper()
		s := testStore(t)
		settings := filepath.Join(filepath.Dir(s.Root), "settings.json")
		body := fmt.Sprintf(`{"storage_global_limit_bytes":%d,"storage_min_free_bytes":0}`, limit)
		if err := os.WriteFile(settings, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return s
	}
	near := makeLimited(t, needed)
	if _, err := near.Capture(Source{UID: "u"}, "x", input); err != nil {
		t.Fatalf("deduplicated near-quota capture = %v", err)
	}
	usage, err := storagebudget.Inspect(filepath.Dir(near.Root))
	if err != nil {
		t.Fatal(err)
	}
	if usage.UsedBytes != needed {
		t.Fatalf("managed bytes = %d, want %d", usage.UsedBytes, needed)
	}

	insufficient := makeLimited(t, needed-1)
	if _, err := insufficient.Capture(Source{UID: "u"}, "x", input); !errors.Is(err, storagebudget.ErrBudget) {
		t.Fatalf("insufficient capture = %v", err)
	}
}

func TestInjectedOpenFailuresAndConcurrentPublishWinner(t *testing.T) {
	origRoot, origAt, origOpen, origStat, origLink := openRoot, openAtRoot, openRegular, statOpened, linkFile
	t.Cleanup(func() {
		openRoot, openAtRoot, openRegular, statOpened, linkFile = origRoot, origAt, origOpen, origStat, origLink
	})
	fail := errors.New("injected open")
	dir := t.TempDir()
	path := filepath.Join(dir, "x")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	openRoot = func(string) (*os.Root, error) { return nil, fail }
	if _, err := readRooted(dir, "x", 1); !errors.Is(err, fail) {
		t.Fatalf("open root = %v", err)
	}
	openRoot = origRoot
	openAtRoot = func(*os.Root, string) (*os.File, error) { return nil, fail }
	if _, err := readRooted(dir, "x", 1); !errors.Is(err, fail) {
		t.Fatalf("open rooted file = %v", err)
	}
	openAtRoot = origAt
	openRegular = func(string) (*os.File, error) { return nil, fail }
	if _, err := readRegularFile(path, 1); !errors.Is(err, fail) {
		t.Fatalf("open regular = %v", err)
	}
	openRegular = origOpen
	statOpened = func(*os.File) (os.FileInfo, error) { return nil, fail }
	if _, err := readRegularFile(path, 1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("stat regular = %v", err)
	}
	statOpened = origStat

	data := []byte("winner")
	winner := filepath.Join(dir, digest(data))
	linkFile = func(_ string, newname string) error {
		if err := os.WriteFile(newname, data, 0o600); err != nil {
			return err
		}
		return os.ErrExist
	}
	if err := publishImmutable(winner, data, digest(data)); err != nil {
		t.Fatalf("concurrent winner = %v", err)
	}
}

func TestCapturePublicationFailuresAndCaptureDirectoryFailure(t *testing.T) {
	origLink := linkFile
	t.Cleanup(func() { linkFile = origLink })
	fail := errors.New("publish fail")
	linkFile = func(string, string) error { return fail }
	s := testStore(t)
	if _, err := s.Capture(Source{UID: "u"}, "x", []CaptureItem{{Key: "asset:a", Name: "a.png", MIME: "image/png", Data: png}}); !errors.Is(err, fail) {
		t.Fatalf("blob publication = %v", err)
	}
	s = testStore(t)
	if _, err := s.Capture(Source{UID: "u"}, "x", nil); !errors.Is(err, fail) {
		t.Fatalf("manifest publication = %v", err)
	}
	linkFile = origLink
	s = testStore(t)
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.Root, "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, "captures"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Capture(Source{UID: "u"}, "x", nil); err == nil {
		t.Fatal("capture directory file succeeded")
	}
	s = testStore(t)
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, "blobs"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Capture(Source{UID: "u"}, "x", nil); err == nil {
		t.Fatal("blob directory file succeeded")
	}
}

func TestReadRegularMapsRacingGrowthToCorruption(t *testing.T) {
	orig := readStream
	t.Cleanup(func() { readStream = orig })
	dir := t.TempDir()
	path := filepath.Join(dir, "x")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	readStream = func(io.Reader, int64) ([]byte, error) { return nil, ErrTooLarge }
	if _, err := readRegularFile(path, 1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("racing growth = %v", err)
	}
}
