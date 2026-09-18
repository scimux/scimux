package asset

import (
	"reflect"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
)

func TestScanMarkdown_ImageAndLink(t *testing.T) {
	text := "Here is the sketch:\n\n![sketch](/tmp/gen/sketch.png)\n\nAnd the report: " +
		"[report](results/report.md)."
	got := ScanMarkdown(text)
	want := []Candidate{
		{Ref: "/tmp/gen/sketch.png", Alt: "sketch", IsImage: true},
		{Ref: "results/report.md", Alt: "report", IsImage: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestScanMarkdown_SkipsFencedCodeBlocks(t *testing.T) {
	text := "Example usage:\n\n```md\n![sketch](/tmp/gen/sketch.png)\n```\n\nNo real image here."
	got := ScanMarkdown(text)
	if len(got) != 0 {
		t.Fatalf("got %+v, want no candidates (fenced block should be skipped)", got)
	}
}

func TestScanMarkdown_SkipsHTTPLinks(t *testing.T) {
	text := "See [docs](https://example.com/readme.md) and ![logo](http://example.com/logo.png)."
	got := ScanMarkdown(text)
	if len(got) != 0 {
		t.Fatalf("got %+v, want no candidates (http/https are not local)", got)
	}
}

func TestScanMarkdown_SkipsAlreadyProjectedAssetRefs(t *testing.T) {
	text := "Here is the sketch:\n\n![sketch](scimux-asset:a_123)"
	got := ScanMarkdown(text)
	if len(got) != 0 {
		t.Fatalf("got %+v, want no candidates (scimux-asset refs are already resolved)", got)
	}
}

func TestScanMarkdown_NoMatches(t *testing.T) {
	got := ScanMarkdown("just some plain prose, no links here.")
	if len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
}

func TestScanMarkdown_InlineCodeSpanNotFenceConfused(t *testing.T) {
	// A single-line code span (`...`) is not a fence; content outside it
	// should still scan normally.
	text := "Run `ls` then check ![out](/tmp/gen/out.png)"
	got := ScanMarkdown(text)
	want := []Candidate{{Ref: "/tmp/gen/out.png", Alt: "out", IsImage: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestScanToolOutput_EditKindWithPathKey(t *testing.T) {
	tool := &sessionlog.ToolEvent{
		Kind:     "edit",
		RawInput: map[string]any{"path": "/tmp/gen/report.md", "content": "..."},
	}
	got := ScanToolOutput(tool)
	want := []Candidate{{Ref: "/tmp/gen/report.md"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestScanToolOutput_MoveKindWithFilePathKey(t *testing.T) {
	tool := &sessionlog.ToolEvent{
		Kind:     "move",
		RawInput: map[string]any{"file_path": "/tmp/gen/moved.png"},
	}
	got := ScanToolOutput(tool)
	want := []Candidate{{Ref: "/tmp/gen/moved.png"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestScanToolOutput_NonWriteKindIgnored(t *testing.T) {
	for _, kind := range []string{"read", "search", "execute", "fetch", "other", "delete", ""} {
		tool := &sessionlog.ToolEvent{
			Kind:     kind,
			RawInput: map[string]any{"path": "/tmp/gen/whatever.png"},
		}
		if got := ScanToolOutput(tool); len(got) != 0 {
			t.Fatalf("kind %q: got %+v, want none (only edit/move are write-shaped)", kind, got)
		}
	}
}

func TestScanToolOutput_NilOrNonMapRawInput(t *testing.T) {
	cases := []any{nil, "just a string", []any{"a", "b"}, 42.0}
	for _, raw := range cases {
		tool := &sessionlog.ToolEvent{Kind: "edit", RawInput: raw}
		if got := ScanToolOutput(tool); len(got) != 0 {
			t.Fatalf("rawInput %#v: got %+v, want none", raw, got)
		}
	}
}

func TestScanToolOutput_MissingKnownKeys(t *testing.T) {
	tool := &sessionlog.ToolEvent{
		Kind:     "edit",
		RawInput: map[string]any{"unrelated": "value"},
	}
	if got := ScanToolOutput(tool); len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
}

func TestScanToolOutput_NestedEditsArray(t *testing.T) {
	tool := &sessionlog.ToolEvent{
		Kind: "edit",
		RawInput: map[string]any{
			"edits": []any{
				map[string]any{"path": "/tmp/gen/a.png"},
				map[string]any{"filePath": "/tmp/gen/b.md"},
				map[string]any{"unrelated": "x"},
			},
		},
	}
	got := ScanToolOutput(tool)
	want := []Candidate{{Ref: "/tmp/gen/a.png"}, {Ref: "/tmp/gen/b.md"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestScanToolOutput_NilTool(t *testing.T) {
	if got := ScanToolOutput(nil); len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
}

func TestCandidates_MergesAndDedupsBySource(t *testing.T) {
	text := "![sketch](/tmp/gen/sketch.png)"
	tools := []*sessionlog.ToolEvent{
		{Kind: "edit", RawInput: map[string]any{"path": "/tmp/gen/sketch.png"}}, // same ref as markdown
		{Kind: "edit", RawInput: map[string]any{"path": "/tmp/gen/report.md"}},
	}
	got := Candidates(text, tools)
	want := []Candidate{
		{Ref: "/tmp/gen/sketch.png", Alt: "sketch", IsImage: true},
		{Ref: "/tmp/gen/report.md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
