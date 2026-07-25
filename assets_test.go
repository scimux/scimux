package main

import (
	"encoding/base64"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
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
func TestServeAssetHeadersActiveContentForcedDownload(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	for i, tc := range []struct{ name, mime string }{
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
