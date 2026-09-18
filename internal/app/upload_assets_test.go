package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/scimux/scimux/internal/asset"
	"github.com/scimux/scimux/internal/sessionlog"
)

func TestIngestAttachmentAssetInline(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte("small file bytes")
	ev, err := a.ingestAttachmentAsset("n1", "note.txt", "text/plain", "/tmp/uploads/note.txt", data)
	if err != nil {
		t.Fatalf("ingestAttachmentAsset: %v", err)
	}
	if ev.Storage != "inline" {
		t.Fatalf("storage = %q, want inline", ev.Storage)
	}
	if ev.SourcePath != "/tmp/uploads/note.txt" {
		t.Errorf("sourcePath = %q, want /tmp/uploads/note.txt", ev.SourcePath)
	}
	if ev.Size != int64(len(data)) {
		t.Errorf("size = %d, want %d", ev.Size, len(data))
	}
	got, err := base64.StdEncoding.DecodeString(ev.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("bytes = %q, want %q", got, data)
	}
	idx := sessionlog.ReadAssets(a.sessionLogPath("n1"))
	if _, ok := idx[ev.ID]; !ok {
		t.Fatal("asset event not recorded in session log")
	}
}

func TestIngestAttachmentAssetBlob(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, assetInlineCap+1)
	for i := range data {
		data[i] = byte(i)
	}
	ev, err := a.ingestAttachmentAsset("n1", "big.bin", "application/octet-stream", "/tmp/uploads/big.bin", data)
	if err != nil {
		t.Fatalf("ingestAttachmentAsset: %v", err)
	}
	if ev.Storage != "blob" {
		t.Fatalf("storage = %q, want blob", ev.Storage)
	}
	if ev.BlobPath == "" {
		t.Fatal("blob path not set")
	}
	got, err := asset.ReadBlob(a.assetsDir, "n1", ev.BlobPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Error("blob bytes mismatch")
	}
	idx := sessionlog.ReadAssets(a.sessionLogPath("n1"))
	rec, ok := idx[ev.ID]
	if !ok || rec.Storage != "blob" {
		t.Fatal("blob asset event not recorded")
	}
}

func TestIngestAttachmentAssetDetectsMimeWhenEmpty(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ev, err := a.ingestAttachmentAsset("n1", "photo.png", "", "/tmp/uploads/photo.png", []byte("PNGDATA"))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Mime != "image/png" {
		t.Errorf("mime = %q, want image/png", ev.Mime)
	}
}

func TestIngestAttachmentAssetSHA256Recorded(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ev, err := a.ingestAttachmentAsset("n1", "x.txt", "text/plain", "/tmp/uploads/x.txt", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	want := sessionlog.SHA256Hex([]byte("hello"))
	if ev.SHA256 != want {
		t.Errorf("sha256 = %q, want %q", ev.SHA256, want)
	}
}

func postUpload(t *testing.T, a *app, nodeID, name string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("files", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	req := httptest.NewRequest("POST", "/api/nodes/"+nodeID+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", nodeID)
	rec := httptest.NewRecorder()
	a.handleUploadAttachments(rec, req)
	return rec
}

// End-to-end: a multipart upload must return an assetId, and the referenced
// asset must be servable and byte-identical to what was uploaded.
func TestHandleUploadAttachmentsReturnsServableAsset(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	a.byID["n1"] = &Node{ID: "n1"}
	rec := postUpload(t, a, "n1", "hello.txt", []byte("hello world"))
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct{ Attachments []Attachment }
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(resp.Attachments))
	}
	at := resp.Attachments[0]
	if at.AssetID == "" {
		t.Fatal("attachment missing assetId")
	}
	arec := serveAsset(a, "n1", at.AssetID)
	if arec.Code != 200 || arec.Body.String() != "hello world" {
		t.Fatalf("code=%d body=%q", arec.Code, arec.Body.String())
	}
}

// If asset ingestion fails, the whole upload must fail and leave no orphaned
// attachment file behind — a partially-ingested upload would violate the
// guaranteed-rendering principle (an attachment reference with no backing
// asset, silently unsearchable/undownloadable elsewhere).
func TestHandleUploadAttachmentsRollsBackOnAssetIngestFailure(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	a.byID["n1"] = &Node{ID: "n1"}
	// Force asset ingestion to fail by making assetsDir a plain file, so
	// MkdirAll(assetsDir/n1, ...) errors regardless of process UID. The
	// upload must be large enough to pick the blob storage path (inline
	// uploads never touch assetsDir).
	if err := os.RemoveAll(a.assetsDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.assetsDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, assetInlineCap+1)
	rec := postUpload(t, a, "n1", "hello.txt", big)
	if rec.Code == 200 {
		t.Fatalf("want failure, got 200: %s", rec.Body.String())
	}
	entries, err := os.ReadDir(a.attachmentDir("n1"))
	if err == nil && len(entries) != 0 {
		t.Errorf("attachment left behind after rollback: %v", entries)
	}
}

// Var 1: a blob written for an upload whose session-log append then FAILS must
// not be left orphaned under assets/<node>/. The append-only log can't be
// rolled back, but the blob write can, so ingestAssetBytes removes it. (The
// test above covers the pre-append blob-write failure; this covers the
// post-write append failure, which was previously untested and leaked.)
func TestIngestAttachmentAsset_RemovesBlobOnAppendFailure(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Force the log append to fail while the blob write succeeds: make the
	// node's session-log path a directory so Writer.Append's file open errors.
	if err := os.Mkdir(a.sessionLogPath("n1"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Over the inline cap so a blob is actually written before the append.
	big := make([]byte, assetInlineCap+1)
	if _, err := a.ingestAttachmentAsset("n1", "big.bin", "application/octet-stream", "/staged/big.bin", big); err == nil {
		t.Fatal("want append failure, got nil error")
	}
	if entries, err := os.ReadDir(asset.NodeDir(a.assetsDir, "n1")); err == nil && len(entries) != 0 {
		t.Fatalf("orphaned blob left behind after failed append: %v", entries)
	}
}

// Var 2.a: the endpoint accepts one file per request. A request carrying more
// than one file is rejected outright rather than partially ingested — which is
// what makes a partial-batch leak against the append-only log impossible.
func TestHandleUploadAttachments_RejectsMultipleFiles(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	a.byID["n1"] = &Node{ID: "n1"}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, name := range []string{"a.txt", "b.txt"} {
		fw, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte("data-" + name)); err != nil {
			t.Fatal(err)
		}
	}
	mw.Close()
	req := httptest.NewRequest("POST", "/api/nodes/n1/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", "n1")
	rec := httptest.NewRecorder()
	a.handleUploadAttachments(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400 for a multi-file request; body=%s", rec.Code, rec.Body.String())
	}
	// The batch is rejected before any file is stored — nothing durable created.
	if a.sessionLogExists("n1") {
		t.Error("rejected multi-file upload created a session log")
	}
	if entries, err := os.ReadDir(a.attachmentDir("n1")); err == nil && len(entries) != 0 {
		t.Errorf("rejected multi-file upload staged files: %v", entries)
	}
}
