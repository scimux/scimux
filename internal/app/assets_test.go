package app

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/asset"
	"github.com/scimux/scimux/internal/sessionlog"
)

func appendAsset(t *testing.T, a *app, nodeID string, ev sessionlog.AssetEvent) {
	t.Helper()
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: a.sessionLogPath(nodeID)}
	if err := w.Append(sessionlog.NewAsset(ev)); err != nil {
		t.Fatal(err)
	}
}

func serveAsset(a *app, nodeID, assetID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/nodes/"+nodeID+"/assets/"+assetID, nil)
	req.SetPathValue("id", nodeID)
	req.SetPathValue("assetID", assetID)
	rec := httptest.NewRecorder()
	a.handleAsset(rec, req)
	return rec
}

func TestServeAssetInline(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_1", Name: "sketch.png", Mime: "image/png", Size: 5,
		Storage: "inline", Bytes: base64.StdEncoding.EncodeToString([]byte("PNGDA")),
	})
	rec := serveAsset(a, "n1", "a_1")
	if rec.Code != 200 || rec.Body.String() != "PNGDA" {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("content-type = %q, want image/png", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "" {
		t.Errorf("raster should render inline, got Content-Disposition %q", cd)
	}
}

func TestServeAssetDedupBackingUsesAliasServingMetadata(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	encoded := base64.StdEncoding.EncodeToString([]byte("shared"))
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_content", Name: "first.txt", Mime: "text/plain", Size: 6, SHA256: "same",
		Storage: "inline", Bytes: encoded,
	})
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_alias", Name: "second.png", Mime: "image/png", Size: 6, SHA256: "same",
		Storage: "inline", BackingID: "a_content",
	})
	rec := serveAsset(a, "n1", "a_alias")
	if rec.Code != http.StatusOK || rec.Body.String() != "shared" {
		t.Fatalf("alias response = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("alias content type = %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != "" {
		t.Fatalf("alias unexpectedly forced download: %q", got)
	}
}

func TestServeAssetRejectsInvalidDedupBacking(t *testing.T) {
	for _, backing := range []sessionlog.AssetEvent{
		{ID: "a_alias", Name: "x.txt", Size: 1, SHA256: "x", Storage: "inline", BackingID: "missing"},
		{ID: "a_alias", Name: "x.txt", Size: 1, SHA256: "x", Storage: "inline", BackingID: "a_alias"},
	} {
		a := newTestApp(t, &fakeTmux{})
		a.byID["n1"] = &Node{ID: "n1"}
		appendAsset(t, a, "n1", backing)
		if rec := serveAsset(a, "n1", backing.ID); rec.Code != http.StatusNotFound {
			t.Errorf("backing %q = %d, want 404", backing.BackingID, rec.Code)
		}
	}
}

func TestServeAssetSafeRasterExtensionMatrix(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	for i, tc := range []struct{ name, contentType string }{
		{"one.png", "image/png"}, {"two.jpg", "image/jpeg"}, {"three.jpeg", "image/jpeg"},
		{"four.gif", "image/gif"}, {"five.webp", "image/webp"},
	} {
		id := "a_r" + string(rune('0'+i))
		appendAsset(t, a, "n1", sessionlog.AssetEvent{ID: id, Name: tc.name, Mime: "text/html", Storage: "inline", Bytes: base64.StdEncoding.EncodeToString([]byte("bytes"))})
		rec := serveAsset(a, "n1", id)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != tc.contentType || rec.Header().Get("Content-Disposition") != "" {
			t.Errorf("%s headers = %d %q %q", tc.name, rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Content-Disposition"))
		}
	}
}

func TestServeAssetDownloadFilenamePreservesUnicodeSpacesAndExtension(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	appendAsset(t, a, "n1", sessionlog.AssetEvent{ID: "a_name", Name: "résumé final.csv", Mime: "image/png", Storage: "inline", Bytes: base64.StdEncoding.EncodeToString([]byte("a,b"))})
	rec := serveAsset(a, "n1", "a_name")
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "r%C3%A9sum%C3%A9%20final.csv") {
		t.Fatalf("download filename = %q", got)
	}
}

func TestServeAssetBlob(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	relPath, err := asset.WriteBlob(a.assetsDir, "n1", "a_2", "report.pdf", []byte("PDFDATA"))
	if err != nil {
		t.Fatal(err)
	}
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_2", Name: "report.pdf", Mime: "application/pdf", Size: 7,
		Storage: "blob", BlobPath: relPath,
	})
	rec := serveAsset(a, "n1", "a_2")
	if rec.Code != 200 || rec.Body.String() != "PDFDATA" {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("pdf should force download, got Content-Disposition %q", rec.Header().Get("Content-Disposition"))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("content-type = %q, want application/octet-stream (forced download)", ct)
	}
}

// Only the raster whitelist renders inline; SVG/HTML/unknown always force a
// download with an inert content type, mirroring the attachment endpoint.
func TestServeAssetHeadersNonRasterForcedDownload(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	for i, tc := range []struct{ name, mime string }{
		{"readme.md", "text/markdown"},
		{"notes.txt", "text/plain"},
		{"report.pdf", "application/pdf"},
		{"table.csv", "text/csv"},
		{"archive.zip", "application/zip"},
		{"evil.svg", "image/svg+xml"},
		{"evil.html", "text/html"},
		{"data.bin", "application/octet-stream"},
	} {
		id := "a_x" + string(rune('1'+i))
		appendAsset(t, a, "n1", sessionlog.AssetEvent{
			ID: id, Name: tc.name, Mime: tc.mime, Storage: "inline",
			Bytes: base64.StdEncoding.EncodeToString([]byte("<x>")),
		})
		rec := serveAsset(a, "n1", id)
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing nosniff", tc.name)
		}
		if !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
			t.Errorf("%s: not forced to download (Content-Disposition %q)", tc.name, rec.Header().Get("Content-Disposition"))
		}
		if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "html") || strings.Contains(ct, "svg") {
			t.Errorf("%s: served active content type %q", tc.name, ct)
		}
	}
}

func TestServeAssetCrossNodeRejected(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	a.byID["n2"] = &Node{ID: "n2"}
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_1", Name: "secret.png", Storage: "inline",
		Bytes: base64.StdEncoding.EncodeToString([]byte("x")),
	})
	if rec := serveAsset(a, "n2", "a_1"); rec.Code != 404 {
		t.Errorf("cross-node leak: code=%d", rec.Code)
	}
}

func TestServeAssetUnknownIDOrNode(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_1", Name: "x.png", Storage: "inline",
		Bytes: base64.StdEncoding.EncodeToString([]byte("x")),
	})
	if rec := serveAsset(a, "n1", "ghost"); rec.Code != 404 {
		t.Errorf("unknown asset id: code=%d", rec.Code)
	}
	if rec := serveAsset(a, "ghost-node", "a_1"); rec.Code != 404 {
		t.Errorf("unknown node: code=%d", rec.Code)
	}
}

func TestServeAssetMissingBlobFile(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_1", Name: "gone.pdf", Storage: "blob", BlobPath: "n1/a_1-gone.pdf",
	})
	if rec := serveAsset(a, "n1", "a_1"); rec.Code != 404 {
		t.Errorf("missing blob bytes: code=%d", rec.Code)
	}
}

// A record whose BlobPath was somehow made to point outside this node's own
// blob directory must never be served — defense in depth even though
// BlobPath is only ever written by scimux itself.
func TestServeAssetTraversalBlobPathRejected(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	if _, err := asset.WriteBlob(a.assetsDir, "n2", "a_2", "sneak.png", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_1", Name: "sneak.png", Storage: "blob", BlobPath: "n2/a_2-sneak.png",
	})
	if rec := serveAsset(a, "n1", "a_1"); rec.Code != 404 {
		t.Errorf("cross-node blobPath escape: code=%d", rec.Code)
	}
}

func TestServeAssetRecordedMIMEDoesNotAuthorizeInline(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_html", Name: "evil.html", Mime: "image/png",
		Storage: "inline", Bytes: base64.StdEncoding.EncodeToString([]byte("<html>")),
	})
	rec := serveAsset(a, "n1", "a_html")
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("HTML named asset served inline despite image/png MIME: %q", rec.Header().Get("Content-Disposition"))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("content-type = %q, want application/octet-stream (extension wins, not MIME)", ct)
	}
}

func TestServeAssetCorruptInlineBytesRejected(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_1", Name: "x.png", Storage: "inline", Bytes: "not-valid-base64!!",
	})
	if rec := serveAsset(a, "n1", "a_1"); rec.Code != 404 {
		t.Errorf("corrupt inline bytes: code=%d", rec.Code)
	}
}

// Phase 6: a deleted node's blob-stored assets must not dangle under the
// live assets directory — archiveAssets moves them out, mirroring
// archiveAttachments/archiveSessionLog, so a reissued slug can never inherit
// a dead node's blobs and the download endpoint (already unreachable once
// the node record is gone) has nothing live left to leak.
func TestArchiveAssetsMovesNodeDir(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	if _, err := asset.WriteBlob(a.assetsDir, "n1", "a_1", "big.bin", []byte("blob bytes")); err != nil {
		t.Fatal(err)
	}
	src := asset.NodeDir(a.assetsDir, "n1")
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("precondition: node asset dir missing: %v", err)
	}
	a.archiveAssets("n1")
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("node asset dir still present after archive: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(a.assetsDir, "archive"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("archive has %d entries, want 1", len(entries))
	}
	if !strings.HasPrefix(entries[0].Name(), "n1.") {
		t.Errorf("archived dir %q not prefixed n1.", entries[0].Name())
	}
	got, err := os.ReadFile(filepath.Join(a.assetsDir, "archive", entries[0].Name(), "a_1-big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "blob bytes" {
		t.Errorf("archived blob bytes = %q, want %q", got, "blob bytes")
	}
}

// A node with only inline assets (bytes live in the already-archived
// session log, not assetsDir) has no per-node asset directory at all — this
// must be a no-op, not an error.
func TestArchiveAssetsNoDirIsNoop(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.archiveAssets("no-such-node") // must not panic or error
	if _, err := os.Stat(filepath.Join(a.assetsDir, "archive")); !os.IsNotExist(err) {
		t.Errorf("archive dir created for a node with no assets")
	}
}
