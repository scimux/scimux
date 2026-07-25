package asset

import (
	"testing"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

func TestProject_RewritesImageMarker(t *testing.T) {
	byPath := map[string]sessionlog.AssetEvent{
		"/tmp/n1/photo.png": {ID: "a_1", Name: "photo.png"},
	}
	got := Project("look at this\n\n[attached image: /tmp/n1/photo.png]", byPath)
	want := "look at this\n\n![photo.png](scimux-asset:a_1)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestProject_RewritesFileMarker(t *testing.T) {
	byPath := map[string]sessionlog.AssetEvent{
		"/tmp/n1/report.pdf": {ID: "a_2", Name: "report.pdf"},
	}
	got := Project("[attached file: /tmp/n1/report.pdf]", byPath)
	want := "[report.pdf](scimux-asset:a_2)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A marker whose path was never ingested — an old log from before this
// mechanism existed, or a failed ingest — must render exactly as stored.
// There is no legacy-rendering branch (hard cut, upload-design.md).
func TestProject_LeavesUnmatchedMarkerAsIs(t *testing.T) {
	text := "[attached image: /tmp/n1/unknown.png]"
	got := Project(text, map[string]sessionlog.AssetEvent{"/tmp/n1/other.png": {ID: "a_9"}})
	if got != text {
		t.Fatalf("got %q, want unchanged %q", got, text)
	}
}

func TestProject_EmptyIndexNoOp(t *testing.T) {
	text := "[attached image: /tmp/n1/photo.png]"
	if got := Project(text, nil); got != text {
		t.Fatalf("got %q, want unchanged %q", got, text)
	}
}

func TestProject_MultipleMarkersInOneTurn(t *testing.T) {
	byPath := map[string]sessionlog.AssetEvent{
		"/tmp/n1/a.png": {ID: "a_1", Name: "a.png"},
		"/tmp/n1/b.txt": {ID: "a_2", Name: "b.txt"},
	}
	text := "two files:\n[attached image: /tmp/n1/a.png]\n[attached file: /tmp/n1/b.txt]"
	got := Project(text, byPath)
	want := "two files:\n![a.png](scimux-asset:a_1)\n[b.txt](scimux-asset:a_2)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReferencedIDs(t *testing.T) {
	text := "![a.png](scimux-asset:a_1) and [b.txt](scimux-asset:a_2) and again scimux-asset:a_1"
	got := ReferencedIDs(text)
	want := []string{"a_1", "a_2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReferencedIDs_None(t *testing.T) {
	if got := ReferencedIDs("plain text, no assets"); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}
