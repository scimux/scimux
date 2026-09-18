package app

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/transcript"
)

// TestHandleChatProjectsUploadedAttachmentAsset covers the P3 render-time
// projection: a user turn recorded with the plain-text attachment marker
// extendPrompt appends ("[attached image: /path]") is rewritten to
// scimux-asset:<id> Markdown, and the chat response carries the matching
// asset summary — without ever rewriting the stored log line.
func TestHandleChatProjectsUploadedAttachmentAsset(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "c1", Title: "c1", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes, a.byID["c1"] = []*Node{n}, n

	w := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, "c1.jsonl")}
	must(t, w.Append(sessionlog.NewMeta("c1", "claude", "", "", a.home)))
	must(t, w.Append(sessionlog.NewAsset(sessionlog.AssetEvent{
		ID: "a_1", Name: "photo.png", Mime: "image/png", Size: 3,
		Storage: "inline", Bytes: "aGk=", SourceKind: "upload",
		SourcePath: "/tmp/n1/photo.png",
	})))
	must(t, w.Append(sessionlog.Event{
		T: "user", Time: "2026-07-14T01:00:00Z",
		Text: "check this out\n\n[attached image: /tmp/n1/photo.png]",
	}))
	must(t, w.Append(sessionlog.Event{T: "assistant", Time: "2026-07-14T01:01:00Z", Text: "nice photo"}))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/c1/chat", nil)
	r.SetPathValue("id", "c1")
	a.handleChat(rec, r)
	if rec.Code != 200 {
		t.Fatalf("chat code = %d body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Turns  []transcript.Turn         `json:"turns"`
		Assets map[string]map[string]any `json:"assets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Turns) != 2 {
		t.Fatalf("turns = %+v, want 2", body.Turns)
	}
	if strings.Contains(body.Turns[0].Text, "[attached") {
		t.Errorf("turn text still carries the raw marker: %q", body.Turns[0].Text)
	}
	if !strings.Contains(body.Turns[0].Text, "![photo.png](scimux-asset:a_1)") {
		t.Errorf("turn text not projected: %q", body.Turns[0].Text)
	}
	as, ok := body.Assets["a_1"]
	if !ok {
		t.Fatalf("assets map missing a_1: %+v", body.Assets)
	}
	if as["name"] != "photo.png" || as["mime"] != "image/png" || as["inline"] != true {
		t.Errorf("asset summary = %+v", as)
	}
	if _, ok := as["data"]; ok {
		t.Errorf("asset summary still carries inline data: %+v", as)
	}
	if url, _ := as["url"].(string); url != "/api/nodes/c1/assets/a_1" {
		t.Errorf("asset url = %q, want canonical endpoint", url)
	}

	// The stored log line is never rewritten: replaying it raw still shows
	// the original marker text.
	raw := sessionlog.ReadEvents(w.Path)
	var rawText string
	for _, ev := range raw {
		if ev.T == "user" {
			rawText = ev.Text
		}
	}
	if !strings.Contains(rawText, "[attached image: /tmp/n1/photo.png]") {
		t.Errorf("stored log line was rewritten: %q", rawText)
	}
}

// A marker whose path was never ingested as an asset (an old log written
// before this mechanism existed) renders exactly as stored — there is no
// separate legacy-rendering path (hard cut, upload-design.md).
func TestHandleChatLeavesUnknownAttachmentMarkerAsIs(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "c1", Title: "c1", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes, a.byID["c1"] = []*Node{n}, n

	w := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, "c1.jsonl")}
	must(t, w.Append(sessionlog.NewMeta("c1", "claude", "", "", a.home)))
	must(t, w.Append(sessionlog.Event{
		T: "user", Time: "2026-07-14T01:00:00Z",
		Text: "old-style upload\n\n[attached image: /tmp/n1/old.png]",
	}))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/nodes/c1/chat", nil)
	r.SetPathValue("id", "c1")
	a.handleChat(rec, r)

	var body struct {
		Turns  []transcript.Turn `json:"turns"`
		Assets map[string]any    `json:"assets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Turns) != 1 || !strings.Contains(body.Turns[0].Text, "[attached image: /tmp/n1/old.png]") {
		t.Fatalf("turns = %+v, want unrewritten legacy marker", body.Turns)
	}
	if len(body.Assets) != 0 {
		t.Errorf("assets = %+v, want none", body.Assets)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestAssetSummaryNeverInterpolatesMIMEIntoURL(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sum := a.assetSummary("n1", sessionlog.AssetEvent{
		ID: "a_1", Name: "x.png", Mime: `image/png" onerror="alert(1)`,
		Size: 3, Storage: "inline", Bytes: "aGk=",
	})
	if _, ok := sum["data"]; ok {
		t.Fatalf("inline asset still carries a data field: %+v", sum)
	}
	url, _ := sum["url"].(string)
	if url == "" {
		t.Fatalf("asset summary missing canonical url: %+v", sum)
	}
	if strings.Contains(url, "onerror") || strings.Contains(url, `"`) || strings.Contains(url, "image/png") {
		t.Fatalf("url interpolates recorded MIME: %q", url)
	}
	if url != "/api/nodes/n1/assets/a_1" {
		t.Fatalf("url = %q, want /api/nodes/n1/assets/a_1", url)
	}
}

func TestAssetSummaryAlwaysUsesCanonicalURL(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	inline := a.assetSummary("node/id", sessionlog.AssetEvent{
		ID: "a/1", Name: "x.png", Mime: "image/png", Storage: "inline", Bytes: "aGk=",
	})
	blob := a.assetSummary("node/id", sessionlog.AssetEvent{
		ID: "a/1", Name: "x.bin", Mime: "application/octet-stream", Storage: "blob",
	})
	want := "/api/nodes/node%2Fid/assets/a%2F1"
	for _, sum := range []map[string]any{inline, blob} {
		if _, ok := sum["data"]; ok {
			t.Fatalf("summary still carries data: %+v", sum)
		}
		if got, _ := sum["url"].(string); got != want {
			t.Fatalf("url = %q, want %q (summary=%+v)", got, want, sum)
		}
	}
}
