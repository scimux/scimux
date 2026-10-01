package app

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/asset"
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/transcript"
)

func TestIngestAssetHookWithoutSessionStoreIsNoop(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.sessionsDir = ""
	a.ingestAssetHook("n1", t.TempDir(), []asset.Candidate{{Ref: "missing.txt"}})
}

func TestAssetImportReasonUnreadableAndUnknown(t *testing.T) {
	if got := assetImportReason(asset.ErrUnreadable); got != "unreadable" {
		t.Fatalf("unreadable reason = %q", got)
	}
	if got := assetImportReason(errors.New("unexpected")); got != "unreadable" {
		t.Fatalf("unknown reason = %q", got)
	}
}

func TestRecordAssetImportFailureKeepsFirstIdentity(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	existing := map[[2]int]bool{{3, 1}: true}
	a.recordAssetImportFailure("n1", 3, 1, asset.Candidate{Ref: "missing.txt"}, "not_found", existing)
	if events := sessionlog.ReadEvents(a.sessionLogPath("n1")); len(events) != 0 {
		t.Fatalf("duplicate import identity appended events: %+v", events)
	}
}

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
	if ev.Name != "report.md" {
		t.Errorf("name = %q, want actual basename %q (alt text must not rename it)", ev.Name, "report.md")
	}
}

func TestIngestAssetHook_ExternalReferenceDefaultsBlockedAndIsRecorded(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	outside := t.TempDir()
	path := filepath.Join(outside, "outside.pdf")
	if err := os.WriteFile(path, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: a.sessionLogPath("n1")}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "[report](" + path + ")"}))
	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: path, Alt: "report"}})
	if len(sessionlog.ReadAssets(a.sessionLogPath("n1"))) != 0 {
		t.Fatal("external path imported while the setting was off")
	}
	events := sessionlog.ReadEvents(a.sessionLogPath("n1"))
	if len(events) != 2 || events[1].T != "asset_import" {
		t.Fatalf("blocked reference was not persisted after its turn: %+v", events)
	}
	b, _ := json.Marshal(events[1])
	if !strings.Contains(string(b), `"reason":"outside_workspace"`) || !strings.Contains(string(b), path) {
		t.Fatalf("blocked reference record = %s", b)
	}
	seg := sessionlog.ReadSegment(a.sessionLogPath("n1"))
	projected, _ := a.projectTurns("n1", seg.Turns)
	if len(projected) != 1 || !strings.Contains(projected[0].Text, "scimux-import:0:0:outside_workspace") {
		t.Fatalf("blocked reference projection = %+v", projected)
	}
}

func TestIngestAssetHook_ExternalReferenceImportsWhenEnabled(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFileForSettingsTest(a.settingsPath, `{"allow_external_attachments":true}`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	outside := t.TempDir()
	path := filepath.Join(outside, "outside.txt")
	if err := os.WriteFile(path, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: a.sessionLogPath("n1")}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "[description](" + path + ")"}))
	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: path, Alt: "misleading.svg"}})
	got := sessionlog.ReadAssetsByPath(a.sessionLogPath("n1"))[path]
	if got.Name != "outside.txt" {
		t.Fatalf("external asset name = %q, want actual basename", got.Name)
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

// Deduplication: the same bytes referenced twice within a node retain separate
// attachment identities but reuse the existing content backing.
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
	byPath := sessionlog.ReadAssetsByPath(a.sessionLogPath("n1"))
	first, second := byPath["a.png"], byPath["b.png"]
	if first.ID == "" || second.ID == "" || first.ID == second.ID {
		t.Fatalf("attachment identities = %q, %q", first.ID, second.ID)
	}
	if second.BackingID != first.ID || second.Bytes != "" || second.BlobPath != "" {
		t.Fatalf("content backing differs: first=%+v second=%+v", first, second)
	}
}

func TestIngestAssetHook_DedupPreservesPerAttachmentNameAndServingClass(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	data := []byte("identical content")
	if err := os.WriteFile(filepath.Join(dir, "first.txt"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "second.md"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: "first.txt"}, {Ref: "second.md"}})

	byPath := sessionlog.ReadAssetsByPath(a.sessionLogPath("n1"))
	first, firstOK := byPath["first.txt"]
	second, secondOK := byPath["second.md"]
	if !firstOK || !secondOK {
		t.Fatalf("dedup paths missing: first=%v second=%v", firstOK, secondOK)
	}
	if first.ID == second.ID {
		t.Fatalf("attachment identities were collapsed to %q", first.ID)
	}
	if first.Name != "first.txt" || first.Mime != "text/plain; charset=utf-8" {
		t.Fatalf("first metadata = %q, %q", first.Name, first.Mime)
	}
	if second.Name != "second.md" || second.Mime != "text/markdown; charset=utf-8" {
		t.Fatalf("second metadata = %q, %q", second.Name, second.Mime)
	}
	if second.BackingID != first.ID || second.Bytes != "" || second.BlobPath != "" {
		t.Fatalf("content backing not reused: first=%+v second=%+v", first, second)
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

// Regression: identical bytes referenced under two different ref spellings
// (dedup hit) must BOTH project to the real asset — not leave the second
// reference as an "unavailable" chip. Dedup shares content storage while each
// path keeps a serving identity. See ingestAgentPathAsset.
func TestIngestAssetHook_DedupAliasesSecondPath(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// Two distinct paths, identical bytes → dedup hit on the second.
	if err := os.WriteFile(filepath.Join(dir, "a.png"), []byte("SAMEBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.png"), []byte("SAMEBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: "a.png"}, {Ref: "b.png"}})

	// Both attachment identities are durable, but their content backing is
	// shared and both paths resolve.
	if n := len(sessionlog.ReadAssets(a.sessionLogPath("n1"))); n != 2 {
		t.Fatalf("stored attachment identities = %d, want 2", n)
	}
	byPath := sessionlog.ReadAssetsByPath(a.sessionLogPath("n1"))
	evA, okA := byPath["a.png"]
	evB, okB := byPath["b.png"]
	if !okA || !okB {
		t.Fatalf("both refs must resolve: a.png=%v b.png=%v", okA, okB)
	}
	if evA.ID == evB.ID {
		t.Fatalf("aliased refs collapsed to one identity: %q", evA.ID)
	}
	if evB.BackingID != evA.ID || evB.Bytes != "" || evB.BlobPath != "" {
		t.Fatalf("aliased refs did not share content: a=%+v b=%+v", evA, evB)
	}

	// A turn referencing both must project both to the same real asset, with
	// neither rendered as a synthetic "missing_" chip.
	turns := []transcript.Turn{{Role: "assistant", Text: "![x](a.png) and ![y](b.png)"}}
	out, assets := a.projectTurns("n1", turns)
	if strings.Contains(out[0].Text, "missing_") {
		t.Fatalf("a deduped-but-present file rendered as unavailable: %q", out[0].Text)
	}
	ids := asset.ReferencedIDs(out[0].Text)
	if len(ids) != 2 || len(assets) != 2 {
		t.Fatalf("want 2 attachment ids, got ids=%v assets=%d", ids, len(assets))
	}
}

// Re-polling a turn whose ref aliases an existing asset must stay idempotent:
// the alias record is written at most once, never once per poll.
func TestIngestAssetHook_DedupAliasIdempotent(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.png"), []byte("SAMEBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.png"), []byte("SAMEBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: "a.png"}})
	for i := 0; i < 3; i++ {
		a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: "b.png"}})
	}
	// Count raw "asset" records in the log: one original + exactly one alias.
	var assetRecs int
	for _, ev := range sessionlog.ReadEvents(a.sessionLogPath("n1")) {
		if ev.T == "asset" {
			assetRecs++
		}
	}
	if assetRecs != 2 {
		t.Fatalf("asset records = %d, want 2 (one original + one alias, no per-poll growth)", assetRecs)
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
	for _, raw := range []string{"(sketch.png)", "(notes.md)"} {
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

func TestProjectTurns_MissingReferenceKeepsFileNameNotOlderAsset(t *testing.T) {
	a, work := newAssetProjectionApp(t)
	relative := "nested/report.md"
	dangling := "nested/link.md"
	if err := os.Mkdir(filepath.Join(work, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "missing-target.md"), filepath.Join(work, dangling)); err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(absolute, []byte("gone"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(absolute); err != nil {
		t.Fatal(err)
	}
	present := filepath.Join(work, "still.txt")
	if err := os.WriteFile(present, []byte("kept"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(present, 0o600) })

	w := &sessionlog.Writer{Path: a.sessionLogPath("n1")}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "old [report.md](" + relative + ")"}))
	must(t, w.Append(sessionlog.NewAsset(sessionlog.AssetEvent{
		ID: "a_old", Name: "old-name.md", Storage: "inline",
		Bytes:      base64.StdEncoding.EncodeToString([]byte("snapshot")),
		SourceKind: "agent_path", SourcePath: relative,
	})))
	later := "later [quarterly](" + relative + ") [alias](" + dangling + ") [brief](" + absolute + ") [kept](" + present + ")"
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: later}))
	imports := []sessionlog.AssetImportEvent{
		{TurnRecord: 2, Occurrence: 0, Ref: relative, Reason: "not_found"},
		{TurnRecord: 2, Occurrence: 1, Ref: dangling, Reason: "too_large"},
		{TurnRecord: 2, Occurrence: 2, Ref: absolute, Reason: "outside_workspace"},
		{TurnRecord: 2, Occurrence: 3, Ref: present, Reason: "unreadable"},
	}
	for _, ref := range imports {
		must(t, w.Append(sessionlog.NewAssetImport(ref)))
	}

	seg := sessionlog.ReadSegment(a.sessionLogPath("n1"))
	projected, assets := a.projectTurns("n1", seg.Turns)
	if len(projected) != 2 {
		t.Fatalf("turns = %d, want 2", len(projected))
	}
	if !strings.Contains(projected[0].Text, "scimux-asset:a_old") {
		t.Fatalf("earlier import lost its snapshot: %q", projected[0].Text)
	}
	got := projected[1].Text
	for _, name := range []string{"report.md", "link.md", "brief.md"} {
		if !strings.Contains(got, name) {
			t.Fatalf("projected text lost file name %s: %q", name, got)
		}
	}
	for _, marker := range []string{
		"scimux-import:2:0:not_found",
		"scimux-import:2:1:not_found",
		"scimux-import:2:2:not_found",
		"scimux-import:2:3:unreadable",
	} {
		if !strings.Contains(got, marker) {
			t.Fatalf("projected text missing %s: %q", marker, got)
		}
	}
	if strings.Contains(got, "scimux-asset:a_old") || strings.Contains(got, "old-name.md") {
		t.Fatalf("missing reference resolved to the older asset: %q", got)
	}
	if _, ok := assets["a_old"]; !ok {
		t.Fatal("older snapshot dropped from the asset map")
	}
	stored := sessionlog.ReadAssetImports(a.sessionLogPath("n1"))
	if len(stored) != len(imports) {
		t.Fatalf("stored imports = %+v", stored)
	}
	for i, want := range imports {
		if stored[i].Reason != want.Reason || stored[i].Ref != want.Ref {
			t.Fatalf("stored import %d = %+v, want %+v", i, stored[i], want)
		}
	}

	locked := filepath.Join(work, "locked")
	secret := filepath.Join(locked, "secret.md")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if _, err := os.Lstat(secret); err == nil {
		t.Skip("current user can stat a file inside a mode-000 directory")
	}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "[secret.md](" + secret + ")"}))
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{
		TurnRecord: 7, Occurrence: 0, Ref: secret, Reason: "unreadable",
	})))
	again := sessionlog.ReadSegment(a.sessionLogPath("n1"))
	projected, _ = a.projectTurns("n1", again.Turns)
	perm := ""
	for _, turn := range projected {
		if strings.Contains(turn.Text, "secret") {
			perm = turn.Text
		}
	}
	if strings.Contains(perm, "not_found") || !strings.Contains(perm, "unreadable") {
		t.Fatalf("permission error projected as absence: %q", perm)
	}
}

func TestImportedAssetRemainsDownloadableAfterSourceDisappears(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	a.byID["n1"] = &Node{ID: "n1", Dir: dir}
	original := []byte("snapshot-bytes")
	path := filepath.Join(dir, "chart.png")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: a.sessionLogPath("n1")}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "generated: ![a chart](chart.png)"}))
	a.ingestAssetHook("n1", dir, []asset.Candidate{{Ref: "chart.png", Alt: "a chart", IsImage: true}})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	seg := sessionlog.ReadSegment(a.sessionLogPath("n1"))
	out, _ := a.projectTurns("n1", seg.Turns)
	ids := asset.ReferencedIDs(out[0].Text)
	if len(ids) != 1 || strings.Contains(out[0].Text, "not_found") {
		t.Fatalf("imported attachment lost after source removal: %q", out[0].Text)
	}
	rec := serveAsset(a, "n1", ids[0])
	if rec.Code != 200 || rec.Body.String() != string(original) {
		t.Fatalf("download after source removal = %d %q", rec.Code, rec.Body.String())
	}
}

func newAssetProjectionApp(t *testing.T) (*app, string) {
	t.Helper()
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	a.byID["n1"] = &Node{ID: "n1", Dir: work}
	return a, work
}

// A reference scanned from a turn but never ingested (rejected or simply
// never scanned) is left exactly as the agent wrote it — plain path text,
// never an "unavailable" chip (ineligible references were never going to
// be servable, so a chip would just clutter the chat).
func TestProjectTurns_LeavesUnresolvedAgentPathAsPlainText(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	text := "see [gone](/tmp/does/not/exist.txt)"
	turns := []transcript.Turn{{Role: "assistant", Text: text}}
	out, _ := a.projectTurns("n1", turns)
	if out[0].Text != text {
		t.Fatalf("got %q, want unchanged %q", out[0].Text, text)
	}
	if ids := asset.ReferencedIDs(out[0].Text); len(ids) != 0 {
		t.Fatalf("got %d referenced ids, want none: %q", len(ids), out[0].Text)
	}
}
