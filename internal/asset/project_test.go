package asset

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
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

func TestProject_UsesFallbackName(t *testing.T) {
	got := Project("[attached file: /tmp/n1/unnamed]", map[string]sessionlog.AssetEvent{
		"/tmp/n1/unnamed": {ID: "a_unnamed"},
	})
	if got != "[asset](scimux-asset:a_unnamed)" {
		t.Fatalf("got %q", got)
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

func TestProjectBlockedAgentPathsCountsOnlyLocalReferences(t *testing.T) {
	text := "[source](https://example.test) ![chart](/outside/chart.png) [doc](/outside/report.pdf)"
	imports := []sessionlog.AssetImportEvent{
		{TurnRecord: 4, Occurrence: 0, Ref: "/outside/chart.png", Reason: "outside_workspace"},
		{TurnRecord: 4, Occurrence: 1, Ref: "/outside/report.pdf", Reason: "not_found"},
	}
	got := ProjectBlockedAgentPaths(text, imports)
	if !strings.Contains(got, "[source](https://example.test)") ||
		!strings.Contains(got, "scimux-import:4:0:outside_workspace") ||
		!strings.Contains(got, "scimux-import:4:1:not_found") {
		t.Fatalf("projected = %q", got)
	}
}

func TestProjectBlockedAgentPathsLeavesFencedExample(t *testing.T) {
	text := "```md\n![example](/outside/example.png)\n```\n![real](/outside/real.png)"
	imports := []sessionlog.AssetImportEvent{{TurnRecord: 3, Occurrence: 0, Ref: "/outside/real.png", Reason: "outside_workspace"}}
	got := ProjectBlockedAgentPaths(text, imports)
	if !strings.Contains(got, "![example](/outside/example.png)") || !strings.Contains(got, "scimux-import:3:0:outside_workspace") {
		t.Fatalf("projected = %q", got)
	}
}

func TestProjectAgentPathBindingsUsesFallbackNameAndRejectsMismatchedBinding(t *testing.T) {
	text := "![one](/tmp/one.png) [two](/tmp/two.txt)"
	bound := map[int]sessionlog.AssetEvent{
		0: {ID: "a_one", SourcePath: "/tmp/one.png"},
		1: {ID: "a_wrong", SourcePath: "/tmp/other.txt", Name: "other.txt"},
	}
	got := ProjectAgentPathBindings(text, bound, nil)
	if !strings.Contains(got, "![asset](scimux-asset:a_one)") || !strings.Contains(got, "[two](/tmp/two.txt)") {
		t.Fatalf("bindings = %q", got)
	}
}

func TestProjectVisibleBlockedMissingAndRemovedSourcesKeepFileName(t *testing.T) {
	dir := t.TempDir()
	relative := "nested/report.md"
	dangling := "nested/link.md"
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing-target.md"), filepath.Join(dir, dangling)); err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(dir, "outside-brief.md")
	if err := os.WriteFile(absolute, []byte("was here"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(absolute); err != nil {
		t.Fatal(err)
	}
	present := filepath.Join(dir, "still.txt")
	if err := os.WriteFile(present, []byte("kept"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(present, 0o600) })

	older := map[string]sessionlog.AssetEvent{
		relative: {ID: "a_old", Name: "old-name.md", SourcePath: relative},
		dangling: {ID: "a_link", Name: "old-link.md", SourcePath: dangling},
		absolute: {ID: "a_abs", Name: "old-brief.md", SourcePath: absolute},
	}
	cases := []struct {
		name, text, ref, reason, want string
	}{
		{
			name: "initially missing relative",
			text: "see [quarterly](" + relative + ")", ref: relative, reason: "not_found",
			want: "report.md",
		},
		{
			name: "blocked relative removed",
			text: "see [quarterly](" + relative + ")", ref: relative, reason: "unreadable",
			want: "report.md",
		},
		{
			name: "dangling symlink",
			text: "see [alias](" + dangling + ")", ref: dangling, reason: "too_large",
			want: "link.md",
		},
		{
			name: "absolute source removed",
			text: "see [brief](" + absolute + ")", ref: absolute, reason: "outside_workspace",
			want: "outside-brief.md",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			imports := []sessionlog.AssetImportEvent{{
				TurnRecord: 5, Occurrence: 0, Ref: tc.ref, Reason: tc.reason, Alt: "ignored",
			}}
			got := ProjectVisibleBlocked(tc.text, nil, imports, dir)
			got = ProjectAgentPaths(got, older)
			if imports[0].Reason != tc.reason {
				t.Fatalf("stored reason changed to %q", imports[0].Reason)
			}
			if !strings.Contains(got, tc.want) || !strings.Contains(got, "scimux-import:5:0:not_found") {
				t.Fatalf("projected = %q, want file name %q and not_found marker", got, tc.want)
			}
			if strings.Contains(got, "scimux-asset:a_") {
				t.Fatalf("missing reference resolved to an older asset: %q", got)
			}
		})
	}

	kept := []sessionlog.AssetImportEvent{{
		TurnRecord: 5, Occurrence: 0, Ref: present, Reason: "unreadable", Alt: "quarterly",
	}}
	got := ProjectVisibleBlocked("see [quarterly]("+present+")", nil, kept, dir)
	if kept[0].Reason != "unreadable" || !strings.Contains(got, "quarterly") || !strings.Contains(got, "scimux-import:5:0:unreadable") {
		t.Fatalf("present unreadable file = %q, imports=%+v", got, kept)
	}
	if strings.Contains(got, "not_found") {
		t.Fatalf("permission or presence was treated as absence: %q", got)
	}

	t.Run("permission error", func(t *testing.T) {
		locked := filepath.Join(dir, "locked")
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
		} else if os.IsNotExist(err) {
			t.Fatalf("inaccessible file reported missing: %v", err)
		}
		denied := []sessionlog.AssetImportEvent{{
			TurnRecord: 6, Occurrence: 0, Ref: secret, Reason: "unreadable",
		}}
		deniedText := ProjectVisibleBlocked("[secret.md]("+secret+")", nil, denied, dir)
		if denied[0].Reason != "unreadable" || strings.Contains(deniedText, "not_found") {
			t.Fatalf("permission error projected as absence: %q imports=%+v", deniedText, denied)
		}
	})

	bound := map[int]sessionlog.AssetEvent{
		0: {ID: "a_keep", Name: "report.md", SourcePath: relative},
	}
	imported := []sessionlog.AssetImportEvent{{
		TurnRecord: 5, Occurrence: 0, Ref: relative, Reason: "unreadable",
	}}
	keptAsset := ProjectVisibleBlocked("[quarterly]("+relative+")", bound, imported, dir)
	if !strings.Contains(keptAsset, "scimux-asset:a_keep") || strings.Contains(keptAsset, "not_found") {
		t.Fatalf("imported snapshot was dropped after the source disappeared: %q", keptAsset)
	}
}

func TestProjectVisibleBlockedAbsenceDecisions(t *testing.T) {
	dir := t.TempDir()
	if PathAbsent(" \t", dir) || PathAbsent("gone.md", "") || PathAbsent("gone.md", "  ") {
		t.Fatal("a blank ref or a relative ref without a directory was treated as absent")
	}

	relative := "nested/gone.md"
	undirected := []sessionlog.AssetImportEvent{{
		TurnRecord: 8, Occurrence: 0, Ref: relative, Reason: "unreadable",
	}}
	got := ProjectVisibleBlocked("see [quarterly]("+relative+")", nil, undirected, "  ")
	if undirected[0].Reason != "unreadable" || !strings.Contains(got, "quarterly") || strings.Contains(got, "not_found") {
		t.Fatalf("relative ref without a directory = %q imports=%+v", got, undirected)
	}

	target := filepath.Join(dir, "target.md")
	if err := os.WriteFile(target, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	live := "live.md"
	if err := os.Symlink(target, filepath.Join(dir, live)); err != nil {
		t.Fatal(err)
	}
	liveImports := []sessionlog.AssetImportEvent{{
		TurnRecord: 2, Occurrence: 0, Ref: live, Reason: "too_large",
	}}
	liveText := ProjectVisibleBlocked("[alias]("+live+")", nil, liveImports, dir)
	liveText = ProjectAgentPaths(liveText, map[string]sessionlog.AssetEvent{
		live: {ID: "a_old", Name: "old.md", SourcePath: live},
	})
	if liveImports[0].Reason != "too_large" || !strings.Contains(liveText, "scimux-import:2:0:too_large") || strings.Contains(liveText, "not_found") || strings.Contains(liveText, "scimux-asset:") {
		t.Fatalf("live symlink = %q imports=%+v", liveText, liveImports)
	}

	loop := filepath.Join(dir, "loop.md")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	loopImports := []sessionlog.AssetImportEvent{{
		TurnRecord: 2, Occurrence: 0, Ref: "loop.md", Reason: "unreadable",
	}}
	loopText := ProjectVisibleBlocked("[alias](loop.md)", nil, loopImports, dir)
	if loopImports[0].Reason != "unreadable" || strings.Contains(loopText, "not_found") {
		t.Fatalf("symlink loop = %q imports=%+v", loopText, loopImports)
	}

	mid := filepath.Join(dir, "mid.md")
	if err := os.Symlink(filepath.Join(dir, "missing-chain.md"), mid); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mid, filepath.Join(dir, "chain.md")); err != nil {
		t.Fatal(err)
	}
	chainImports := []sessionlog.AssetImportEvent{{
		TurnRecord: 2, Occurrence: 0, Ref: "chain.md", Reason: "storage",
	}}
	chainText := ProjectVisibleBlocked("[alias](chain.md)", nil, chainImports, dir)
	chainText = ProjectAgentPaths(chainText, map[string]sessionlog.AssetEvent{
		"chain.md": {ID: "a_chain", Name: "old-chain.md", SourcePath: "chain.md"},
	})
	if chainImports[0].Reason != "storage" || !strings.Contains(chainText, "chain.md") || !strings.Contains(chainText, "scimux-import:2:0:not_found") || strings.Contains(chainText, "scimux-asset:") {
		t.Fatalf("dangling symlink chain = %q imports=%+v", chainText, chainImports)
	}

	t.Run("symlink target permission error", func(t *testing.T) {
		locked := filepath.Join(dir, "noperm")
		if err := os.Mkdir(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		hidden := filepath.Join(locked, "secret.md")
		if err := os.WriteFile(hidden, []byte("hidden"), 0o600); err != nil {
			t.Fatal(err)
		}
		deniedLink := filepath.Join(dir, "denied.md")
		if err := os.Symlink(hidden, deniedLink); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		if _, err := os.Stat(deniedLink); err == nil {
			t.Skip("current user can stat through a mode-000 directory")
		} else if os.IsNotExist(err) {
			t.Fatalf("inaccessible symlink target reported missing: %v", err)
		}
		deniedImports := []sessionlog.AssetImportEvent{{
			TurnRecord: 2, Occurrence: 0, Ref: "denied.md", Reason: "unreadable",
		}}
		deniedText := ProjectVisibleBlocked("[alias](denied.md)", nil, deniedImports, dir)
		if deniedImports[0].Reason != "unreadable" || strings.Contains(deniedText, "not_found") {
			t.Fatalf("symlink target permission error = %q imports=%+v", deniedText, deniedImports)
		}
	})

	for _, ref := range []string{".", "..", "/"} {
		labeled := []sessionlog.AssetImportEvent{{
			TurnRecord: 4, Occurrence: 0, Ref: ref, Reason: "not_found",
		}}
		text := ProjectVisibleBlocked("[notes]("+ref+")", nil, labeled, dir)
		text = ProjectAgentPaths(text, map[string]sessionlog.AssetEvent{
			ref: {ID: "a_named", Name: "old.md", SourcePath: ref},
		})
		if labeled[0].Reason != "not_found" || !strings.Contains(text, "[notes](scimux-import:4:0:not_found)") || strings.Contains(text, "scimux-asset:") {
			t.Fatalf("nameless ref %q = %q imports=%+v", ref, text, labeled)
		}
		blank := []sessionlog.AssetImportEvent{{
			TurnRecord: 4, Occurrence: 0, Ref: ref, Reason: "not_found",
		}}
		unnamed := ProjectVisibleBlocked("[]("+ref+")", nil, blank, dir)
		if !strings.Contains(unnamed, "[file](scimux-import:4:0:not_found)") {
			t.Fatalf("blank alt for %q = %q", ref, unnamed)
		}
	}

	returned := filepath.Join(dir, "returned.md")
	if err := os.WriteFile(returned, []byte("back"), 0o600); err != nil {
		t.Fatal(err)
	}
	stillMissing := []sessionlog.AssetImportEvent{{
		TurnRecord: 3, Occurrence: 0, Ref: returned, Reason: "not_found",
	}}
	returnedText := ProjectVisibleBlocked("[quarterly]("+returned+")", nil, stillMissing, dir)
	if stillMissing[0].Reason != "not_found" || !strings.Contains(returnedText, "quarterly") || !strings.Contains(returnedText, "scimux-import:3:0:retry_ready") {
		t.Fatalf("present file with a stored not_found import = %q imports=%+v", returnedText, stillMissing)
	}
	returnedLink := filepath.Join(dir, "returned-link.md")
	if err := os.Symlink(returned, returnedLink); err != nil {
		t.Fatal(err)
	}
	linked := []sessionlog.AssetImportEvent{{TurnRecord: 3, Occurrence: 0, Ref: returnedLink, Reason: "not_found"}}
	if got := ProjectVisibleBlocked("[link]("+returnedLink+")", nil, linked, dir); !strings.Contains(got, "retry_ready") {
		t.Fatalf("live symlink did not restore retry: %q", got)
	}
	directory := []sessionlog.AssetImportEvent{{TurnRecord: 3, Occurrence: 0, Ref: dir, Reason: "not_found"}}
	if got := ProjectVisibleBlocked("[folder]("+dir+")", nil, directory, dir); !strings.Contains(got, "not_found") || strings.Contains(got, "retry_ready") {
		t.Fatalf("directory was treated as an importable file: %q", got)
	}

	bound := map[int]sessionlog.AssetEvent{
		0: {ID: "a_keep", Name: "kept.png", SourcePath: "kept.png"},
	}
	if got := ProjectVisibleBlocked("![shot](kept.png)", bound, nil, dir); got != "![kept.png](scimux-asset:a_keep)" {
		t.Fatalf("empty import list = %q", got)
	}
}

func TestProjectVisibleBlockedEncodesMissingFilenameForMarker(t *testing.T) {
	dir := t.TempDir()
	ref := "report]100%.md"
	imports := []sessionlog.AssetImportEvent{{
		TurnRecord: 3, Occurrence: 0, Ref: ref, Reason: "not_found",
	}}
	got := ProjectVisibleBlocked("see [quarterly]("+ref+")", nil, imports, dir)
	if !strings.Contains(got, "[report%5D100%25.md](scimux-import:3:0:not_found)") {
		t.Fatalf("missing filename must fit the import marker: %q", got)
	}
	if imports[0].Reason != "not_found" {
		t.Fatalf("stored reason changed: %+v", imports[0])
	}
}

func TestPlainBlockedRefsDecodesMissingLabelsAndPreservesOtherText(t *testing.T) {
	text := "see [my%20file.md](scimux-import:1:0:not_found) " +
		"![chart](scimux-import:1:1:outside_workspace) " +
		"[100%broken](scimux-import:1:2:not_found) " +
		"[](scimux-import:1:3:storage)"
	if got, want := PlainBlockedRefs(text), "see my file.md chart 100%broken file"; got != want {
		t.Fatalf("plain blocked refs = %q, want %q", got, want)
	}
	if got := PlainBlockedRefs("ordinary prose"); got != "ordinary prose" {
		t.Fatalf("ordinary prose changed: %q", got)
	}
}

func TestPathAbsentDoesNotGuessWhenWorkingDirectoryIsUnresolvable(t *testing.T) {
	parent := t.TempDir()
	gone := filepath.Join(parent, "gone")
	if err := os.Mkdir(gone, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Getwd(); err == nil {
		t.Fatal("working directory still resolves")
	}
	if PathAbsent("missing.md", "rel") {
		t.Fatal("unresolvable relative ref was treated as absent")
	}
}
