package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

func TestIngestAssetHook_IngestsRelativePathUnderDir(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("# hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: "report.md", Alt: "the report"}})
	byPath := sessionlog.ReadAssetsByPath(a.sessionLogPath("n1"))
	ev, ok := byPath["report.md"]
	if !ok {
		t.Fatal("candidate not ingested")
	}
	if ev.SourceKind != "agent_path" {
		t.Errorf("sourceKind = %q, want agent_path", ev.SourceKind)
	}
	if ev.Name != "the report" {
		t.Errorf("name = %q, want %q (alt text)", ev.Name, "the report")
	}
}

func TestIngestAssetHook_RejectsPathOutsideRoot(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: filepath.Join(outside, "secret.txt")}})
	if len(sessionlog.ReadAssets(a.sessionLogPath("n1"))) != 0 {
		t.Fatal("path outside root must not be ingested")
	}
}

func TestIngestAssetHook_RejectsMissingFile(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: "does-not-exist.png"}})
	if len(sessionlog.ReadAssets(a.sessionLogPath("n1"))) != 0 {
		t.Fatal("missing file must not be ingested")
	}
}

func TestIngestAssetHook_OversizeFileSkipped(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	big := make([]byte, agentAssetMaxBytes+1)
	if err := os.WriteFile(filepath.Join(dir, "huge.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: "huge.bin"}})
	if len(sessionlog.ReadAssets(a.sessionLogPath("n1"))) != 0 {
		t.Fatal("oversize file must not be ingested")
	}
}

// Deduplication: the same bytes referenced twice within a node must reuse
// the existing asset id, not mint a second record (upload-design.md
// "Deduplication").
func TestIngestAssetHook_DedupsIdenticalBytes(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.png"), []byte("PNGBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.png"), []byte("PNGBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: "a.png"}})
	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: "b.png"}})
	idx := sessionlog.ReadAssets(a.sessionLogPath("n1"))
	if len(idx) != 1 {
		t.Fatalf("got %d asset records, want 1 (dedup by sha256)", len(idx))
	}
}

// Changed content at a reused path is not a dedup hit: it must mint a new
// asset with its own id.
func TestIngestAssetHook_ChangedContentMintsNewAsset(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: "out.txt"}})
	if err := os.WriteFile(path, []byte("v2 different content"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: "out.txt"}})
	idx := sessionlog.ReadAssets(a.sessionLogPath("n1"))
	if len(idx) != 2 {
		t.Fatalf("got %d asset records, want 2 (changed bytes must not dedup)", len(idx))
	}
}

// Repeated ingestion of the exact same path+bytes (e.g. re-polling the same
// turn) must stay idempotent — never duplicate the asset record.
func TestIngestAssetHook_RepeatedCallsIdempotent(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("same bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: "x.txt"}})
	}
	idx := sessionlog.ReadAssets(a.sessionLogPath("n1"))
	if len(idx) != 1 {
		t.Fatalf("got %d asset records after 3 identical calls, want 1", len(idx))
	}
}

// End-to-end: a turn-append ingest followed by projectTurns must turn an
// agent-generated Markdown image reference into a rendered asset entry, and
// an assistant Markdown reference to a real (non-image) doc file into a
// downloadable asset entry, exercising both the ingestion hook and
// asset.ProjectAgentPaths together, as handleChat exercises them.
func TestProjectTurns_RewritesAgentGeneratedImageAndDoc(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sketch.png"), []byte("PNGDATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ingestAssetHook("n1", dir, []asset.Candidate{
		{Ref: "sketch.png", Alt: "a sketch", IsImage: true},
		{Ref: "notes.md", Alt: "my notes"},
	})
	turns := []transcript.Turn{{
		Role: "assistant",
		Text: "here: ![a sketch](sketch.png) and [my notes](notes.md)",
	}}
	out, assets := a.projectTurns("n1", turns)
	if len(assets) != 2 {
		t.Fatalf("got %d assets, want 2: %#v", len(assets), assets)
	}
	for _, raw := range []string{"sketch.png", "notes.md"} {
		if strings.Contains(out[0].Text, raw) {
			t.Errorf("projected text still contains raw path %q: %q", raw, out[0].Text)
		}
	}
}

// Full loop, end to end: ingest an agent-generated image from disk at
// turn-append time, project the turn that mentions it at chat-read time, and
// download the asset the projected reference points at — the download must
// be byte-identical to the original file (mirrors the Phase 5 verification
// done for user uploads).
func TestAgentPathAsset_FullLoopIngestProjectDownload(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	a.byID["n1"] = &Node{ID: "n1"}
	dir := t.TempDir()
	original := []byte("\x89PNG-fake-bytes-for-test")
	if err := os.WriteFile(filepath.Join(dir, "chart.png"), original, 0o600); err != nil {
		t.Fatal(err)
	}

	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: "chart.png", Alt: "a chart", IsImage: true}})

	turns := []transcript.Turn{{Role: "assistant", Text: "generated: ![a chart](chart.png)"}}
	out, assets := a.projectTurns("n1", turns)
	ids := asset.ReferencedIDs(out[0].Text)
	if len(ids) != 1 {
		t.Fatalf("got %d referenced ids, want 1: %q", len(ids), out[0].Text)
	}
	if _, ok := assets[ids[0]]; !ok {
		t.Fatalf("asset %q missing from response assets map", ids[0])
	}

	rec := serveAsset(a, "n1", ids[0])
	if rec.Code != 200 {
		t.Fatalf("download status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(original) {
		t.Fatalf("downloaded bytes differ from original")
	}
}

// A reference scanned from a turn but never ingested (rejected or simply
// never scanned) must still reach a defined "unavailable" outcome at render
// time — never leave the raw local path exposed in chat text.
func TestProjectTurns_UnavailableForUnresolvedAgentPath(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	turns := []transcript.Turn{{Role: "assistant", Text: "see [gone](/tmp/does/not/exist.txt)"}}
	out, assets := a.projectTurns("n1", turns)
	if strings.Contains(out[0].Text, "/tmp/does/not/exist.txt") {
		t.Fatalf("raw path leaked into rendered text: %q", out[0].Text)
	}
	ids := asset.ReferencedIDs(out[0].Text)
	if len(ids) != 1 {
		t.Fatalf("got %d referenced ids, want 1: %q", len(ids), out[0].Text)
	}
	if _, ok := assets[ids[0]]; ok {
		t.Fatalf("unresolved reference must not resolve to a real asset")
	}
}
