package sessionlog

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestAssetEventRoundTripInline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	if err := w.Append(NewMeta("n", "claude", "", "/wd")); err != nil {
		t.Fatal(err)
	}
	a := AssetEvent{
		ID: "a_1", Name: "sketch.png", Mime: "image/png", Size: 5,
		SHA256: "deadbeef", Storage: "inline", Bytes: "aGVsbG8=",
		SourceKind: "agent_path", SourcePath: "/tmp/gen/sketch.png",
	}
	if err := w.Append(NewAsset(a)); err != nil {
		t.Fatal(err)
	}
	evs := ReadEvents(path)
	if len(evs) != 2 || evs[1].T != "asset" || evs[1].Asset == nil {
		t.Fatalf("want asset record, got %+v", evs)
	}
	got := *evs[1].Asset
	if got != a {
		t.Fatalf("asset round-trip mismatch: got %+v, want %+v", got, a)
	}
}

func TestAssetEventRoundTripBlob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	a := AssetEvent{
		ID: "a_2", Name: "report.pdf", Mime: "application/pdf", Size: 99123,
		SHA256: "cafef00d", Storage: "blob", BlobPath: "n/a_2-report.pdf",
		SourceKind: "upload",
	}
	if err := w.Append(NewAsset(a)); err != nil {
		t.Fatal(err)
	}
	evs := ReadEvents(path)
	if len(evs) != 1 || evs[0].Asset == nil || *evs[0].Asset != a {
		t.Fatalf("got %+v, want %+v", evs, a)
	}
}

func TestReadAssetsIndexesByID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	w.Append(NewMeta("n", "claude", "", ""))
	w.Append(NewAsset(AssetEvent{ID: "a_1", Name: "one.png", Storage: "inline"}))
	w.Append(NewAsset(AssetEvent{ID: "a_2", Name: "two.png", Storage: "inline"}))
	idx := ReadAssets(path)
	if len(idx) != 2 {
		t.Fatalf("index len = %d, want 2: %+v", len(idx), idx)
	}
	if idx["a_1"].Name != "one.png" || idx["a_2"].Name != "two.png" {
		t.Fatalf("index contents wrong: %+v", idx)
	}
}

// The first asset event for a given ID is the durable record; a later
// record sharing the same ID (should never normally happen, but replay must
// not let one corrupt or replace the original identity) is ignored.
func TestReadAssetsFirstRecordWinsOnIDCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	w.Append(NewAsset(AssetEvent{ID: "a_1", Name: "original.png", Storage: "inline"}))
	w.Append(NewAsset(AssetEvent{ID: "a_1", Name: "corrupted.png", Storage: "inline"}))
	idx := ReadAssets(path)
	if idx["a_1"].Name != "original.png" {
		t.Fatalf("got %+v, want first record to win", idx["a_1"])
	}
}

func TestReadAssetsByPathIndexesBySourcePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	w.Append(NewAsset(AssetEvent{ID: "a_1", Name: "one.png", Storage: "inline", SourcePath: "/tmp/n1/one.png"}))
	w.Append(NewAsset(AssetEvent{ID: "a_2", Name: "two.png", Storage: "inline", SourcePath: "/tmp/n1/two.png"}))
	idx := ReadAssetsByPath(path)
	if len(idx) != 2 {
		t.Fatalf("index len = %d, want 2: %+v", len(idx), idx)
	}
	if idx["/tmp/n1/one.png"].ID != "a_1" || idx["/tmp/n1/two.png"].ID != "a_2" {
		t.Fatalf("index contents wrong: %+v", idx)
	}
}

// Assets with no recorded SourcePath (should not occur for uploads, but the
// replay must stay defensive) are simply omitted, not indexed under "".
func TestReadAssetsByPathOmitsRecordsWithNoSourcePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	w.Append(NewAsset(AssetEvent{ID: "a_1", Name: "one.png", Storage: "inline"}))
	idx := ReadAssetsByPath(path)
	if len(idx) != 0 {
		t.Fatalf("got %+v, want empty index", idx)
	}
}

func TestReadAssetsIgnoresNonAssetRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	w.Append(NewMeta("n", "claude", "", ""))
	w.Append(Event{T: "user", Text: "hi"})
	w.Append(Event{T: "tool", Tool: &ToolEvent{ID: "t1"}})
	idx := ReadAssets(path)
	if len(idx) != 0 {
		t.Fatalf("got %+v, want empty index", idx)
	}
}

func TestReadAssetsDefensiveToMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	content := `{"t":"asset","asset":{"id":"a_1","name":"ok.png","storage":"inline"}}` + "\n" +
		"not json at all\n" +
		`{"t":"asset","asset":{"id":"a_2","name":"future.png","storage":"inline","futureField":123}}` + "\n" +
		`{"t":"asset","asset":{"id":"a_3","name":"torn` // truncated, no trailing newline
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	idx := ReadAssets(path)
	if len(idx) != 2 {
		t.Fatalf("index len = %d, want 2 (torn line skipped): %+v", len(idx), idx)
	}
	if idx["a_1"].Name != "ok.png" || idx["a_2"].Name != "future.png" {
		t.Fatalf("unexpected index contents: %+v", idx)
	}
}

func TestReadAssetsMissingFile(t *testing.T) {
	idx := ReadAssets(filepath.Join(t.TempDir(), "missing.jsonl"))
	if len(idx) != 0 {
		t.Fatalf("got %+v, want empty", idx)
	}
}

func TestNewAssetIDUniqueAndNonEmpty(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := NewAssetID()
		if id == "" {
			t.Fatal("empty asset id")
		}
		if seen[id] {
			t.Fatalf("collision on id %q", id)
		}
		seen[id] = true
	}
}

func TestSanitizeAssetName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sketch.png", "sketch.png"},
		{"/tmp/gen/sketch.png", "sketch.png"},
		{"../../etc/passwd", "passwd"},
		{"results/report.md", "report.md"},
		{"", "asset"},
		{".", "asset"},
		{"..", "asset"},
		{"/", "asset"},
	}
	for _, c := range cases {
		if got := SanitizeAssetName(c.in); got != c.want {
			t.Errorf("SanitizeAssetName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDetectMIME(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n" + "rest of file")
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"sketch.png", png, "image/png"},
		{"report.pdf", []byte("%PDF-1.4"), "application/pdf"},
		{"notes.md", []byte("# hi"), "text/markdown; charset=utf-8"},
		{"unknownext.xyz123", []byte("\x00\x01binary"), "application/octet-stream"},
	}
	for _, c := range cases {
		got := DetectMIME(c.name, c.data)
		if got != c.want {
			t.Errorf("DetectMIME(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSHA256Hex(t *testing.T) {
	data := []byte("hello")
	want := sha256.Sum256(data)
	if got := SHA256Hex(data); got != hex.EncodeToString(want[:]) {
		t.Fatalf("SHA256Hex = %q, want %q", got, hex.EncodeToString(want[:]))
	}
}
