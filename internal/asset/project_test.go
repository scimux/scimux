package asset

import (
	"reflect"
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

func TestProjectAgentPaths_RewritesImageLink(t *testing.T) {
	byPath := map[string]sessionlog.AssetEvent{
		"results/sketch.png": {ID: "a_5", Name: "sketch.png"},
	}
	got := ProjectAgentPaths("here: ![a sketch](results/sketch.png)", byPath)
	want := "here: ![sketch.png](scimux-asset:a_5)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestProjectAgentPaths_RewritesFileLink(t *testing.T) {
	byPath := map[string]sessionlog.AssetEvent{
		"/tmp/gen/report.pdf": {ID: "a_6", Name: "report.pdf"},
	}
	got := ProjectAgentPaths("see [the report](/tmp/gen/report.pdf)", byPath)
	want := "see [report.pdf](scimux-asset:a_6)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A reference that was scanned but never ingested — outside the allowed
// root, unreadable, too large — is left exactly as written, never rewritten
// into an "unavailable" chip: these were never going to be servable, so a
// chip would just clutter the chat with a dead file affordance.
func TestProjectAgentPaths_UnresolvedLeftAsIs(t *testing.T) {
	text := "see [notes](scratch/notes.md)"
	got := ProjectAgentPaths(text, nil)
	if got != text {
		t.Fatalf("got %q, want unchanged %q", got, text)
	}
	if assetRefRE.MatchString(got) {
		t.Fatalf("got %q, unexpected scimux-asset: reference for unresolved ref", got)
	}
}

func TestProjectAgentPaths_SkipsHTTPLinks(t *testing.T) {
	text := "see [docs](https://example.com/readme.md)"
	if got := ProjectAgentPaths(text, nil); got != text {
		t.Fatalf("got %q, want unchanged %q", got, text)
	}
}

func TestProjectAgentPaths_SkipsAlreadyProjectedRefs(t *testing.T) {
	text := "![photo.png](scimux-asset:a_1)"
	if got := ProjectAgentPaths(text, nil); got != text {
		t.Fatalf("got %q, want unchanged %q", got, text)
	}
}

func TestProjectAgentPaths_SkipsFencedCode(t *testing.T) {
	text := "```\n![img](local/file.png)\n```"
	if got := ProjectAgentPaths(text, nil); got != text {
		t.Fatalf("got %q, want unchanged (fenced) %q", got, text)
	}
}

func TestProjectAgentPaths_NoLinksNoOp(t *testing.T) {
	text := "plain assistant reply, no links"
	if got := ProjectAgentPaths(text, nil); got != text {
		t.Fatalf("got %q, want unchanged %q", got, text)
	}
}

// Regression: in-document anchor links and other non-path URL schemes are not
// local paths and must be left exactly as written — never rewritten into an
// "unavailable" asset chip. This is ordinary agent prose (doc headings,
// footnotes, mailto links), not file references.
func TestProjectAgentPaths_LeavesNonPathTargetsAlone(t *testing.T) {
	cases := []string{
		"See [Overview](#overview) and [Details](#details).",
		"Mail [us](mailto:team@example.com) about it.",
		"Inline [data](data:text/plain;base64,aGk=) blob.",
	}
	for _, text := range cases {
		if got := ProjectAgentPaths(text, nil); got != text {
			t.Errorf("ProjectAgentPaths(%q) = %q, want unchanged", text, got)
		}
	}
}

// The same filter must hold at ingestion time: non-path targets are never
// scanned as candidates.
func TestScanMarkdown_SkipsFragmentAndSchemeTargets(t *testing.T) {
	text := "[Overview](#overview) [mail](mailto:x@y.z) [real](./out.png)"
	got := ScanMarkdown(text)
	want := []Candidate{{Ref: "./out.png", Alt: "real", IsImage: false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want only the real local path %+v", got, want)
	}
}
