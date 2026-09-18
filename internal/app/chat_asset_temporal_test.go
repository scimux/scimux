package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
)

// The reported bug: an agent regenerates a file under the same path with new
// content and re-references it. Projection must anchor each reference to the
// content that was current when that turn was written — the earlier turn keeps
// the old asset, the later turn shows the new one — so the two stay comparable
// side by side instead of both collapsing to one version.
func TestProjectTurns_AnchorsEachReferenceToItsOwnContent(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "c1", Title: "c1", Agent: "claude"}
	a.nodes, a.byID["c1"] = []*Node{n}, n

	w := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, "c1.jsonl")}
	must(t, w.Append(sessionlog.NewMeta("c1", "claude", "", "", a.home)))
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "first: ![p](preview.png)"}))
	must(t, w.Append(sessionlog.NewAsset(sessionlog.AssetEvent{
		ID: "a_old", Name: "preview.png", Storage: "inline", Bytes: "b2xk",
		SourceKind: "agent_path", SourcePath: "preview.png",
	})))
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "regenerated: ![p](preview.png)"}))
	must(t, w.Append(sessionlog.NewAsset(sessionlog.AssetEvent{
		ID: "a_new", Name: "preview.png", Storage: "inline", Bytes: "bmV3",
		SourceKind: "agent_path", SourcePath: "preview.png",
	})))

	seg := sessionlog.ReadSegment(w.Path)
	turns, assets := a.projectTurns("c1", seg.Turns)
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(turns))
	}
	if !strings.Contains(turns[0].Text, "scimux-asset:a_old") {
		t.Errorf("earlier turn must keep the old asset, got %q", turns[0].Text)
	}
	if strings.Contains(turns[0].Text, "a_new") {
		t.Errorf("earlier turn was retroactively changed to the new asset: %q", turns[0].Text)
	}
	if !strings.Contains(turns[1].Text, "scimux-asset:a_new") {
		t.Errorf("later turn must show the new asset, got %q", turns[1].Text)
	}
	if _, ok := assets["a_old"]; !ok {
		t.Errorf("assets map missing a_old (needed to render the earlier turn)")
	}
	if _, ok := assets["a_new"]; !ok {
		t.Errorf("assets map missing a_new")
	}
}
