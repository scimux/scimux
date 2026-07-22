package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadAttachmentStoresFileAndRef(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	a.byID["n1"] = &Node{ID: "n1"}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("files", "shot.png")
	fw.Write([]byte("\x89PNG\r\n\x1a\nfake-image-bytes"))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/nodes/n1/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", "n1")
	rec := httptest.NewRecorder()

	a.handleUploadAttachments(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct{ Attachments []Attachment }
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(resp.Attachments))
	}
	att := resp.Attachments[0]
	if att.Name != "shot.png" {
		t.Errorf("Name = %q, want shot.png", att.Name)
	}
	if att.Mime != "image/png" {
		t.Errorf("Mime = %q, want image/png (from extension)", att.Mime)
	}
	// bytes actually landed on disk under the node's dir
	if !strings.HasPrefix(att.Path, a.attachmentDir("n1")+string(filepath.Separator)) {
		t.Errorf("path %q not under node dir %q", att.Path, a.attachmentDir("n1"))
	}
	b, err := os.ReadFile(att.Path)
	if err != nil {
		t.Fatalf("stored file unreadable: %v", err)
	}
	if att.Size != int64(len(b)) {
		t.Errorf("Size = %d, want %d", att.Size, len(b))
	}
	// token prefix means re-uploading the same name does not collide
	rec2 := httptest.NewRecorder()
	var buf2 bytes.Buffer
	mw2 := multipart.NewWriter(&buf2)
	fw2, _ := mw2.CreateFormFile("files", "shot.png")
	fw2.Write([]byte("second"))
	mw2.Close()
	req2 := httptest.NewRequest("POST", "/api/nodes/n1/attachments", &buf2)
	req2.Header.Set("Content-Type", mw2.FormDataContentType())
	req2.SetPathValue("id", "n1")
	a.handleUploadAttachments(rec2, req2)
	var resp2 struct{ Attachments []Attachment }
	json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if resp2.Attachments[0].Path == att.Path {
		t.Error("second upload of same name collided onto the first path")
	}
}

// A crafted traversal filename must never escape the node's attachment dir.
func TestUploadAttachmentRejectsTraversal(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	a.byID["n1"] = &Node{ID: "n1"}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("files", "../../../etc/evil")
	fw.Write([]byte("x"))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/nodes/n1/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", "n1")
	rec := httptest.NewRecorder()

	a.handleUploadAttachments(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct{ Attachments []Attachment }
	json.Unmarshal(rec.Body.Bytes(), &resp)
	got := resp.Attachments[0].Path
	nodeDir := a.attachmentDir("n1") + string(filepath.Separator)
	if !strings.HasPrefix(got, nodeDir) {
		t.Errorf("traversal escaped: path %q not under %q", got, nodeDir)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("stored file missing: %v", err)
	}
}

func TestUploadAttachmentUnknownNode(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, _ := mw.CreateFormFile("files", "x.txt")
	w.Write([]byte("x"))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/nodes/ghost/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", "ghost")
	rec := httptest.NewRecorder()
	a.handleUploadAttachments(rec, req)
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestArchiveAttachmentsMovesNodeDir(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	if _, err := a.storeAttachment("n1", "a.txt", "text/plain", strings.NewReader("hi")); err != nil {
		t.Fatal(err)
	}
	src := a.attachmentDir("n1")
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("precondition: node dir missing: %v", err)
	}
	a.archiveAttachments("n1")
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("node attachment dir still present after archive: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(a.attachmentsDir, "archive"))
	if len(entries) != 1 {
		t.Fatalf("archive has %d entries, want 1", len(entries))
	}
	if !strings.HasPrefix(entries[0].Name(), "n1.") {
		t.Errorf("archived dir %q not prefixed n1.", entries[0].Name())
	}
}

func TestServeAttachment(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	a.byID["n2"] = &Node{ID: "n2"}
	att, err := a.storeAttachment("n1", "pic.png", "image/png", strings.NewReader("PNGDATA"))
	if err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Base(att.Path)

	serve := func(id, name string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/nodes/"+id+"/attachments/"+name, nil)
		req.SetPathValue("id", id)
		req.SetPathValue("name", name)
		rec := httptest.NewRecorder()
		a.handleAttachment(rec, req)
		return rec
	}

	if rec := serve("n1", leaf); rec.Code != 200 || rec.Body.String() != "PNGDATA" {
		t.Fatalf("serve valid: code=%d body=%q", rec.Code, rec.Body.String())
	}
	// cross-node: n2 must not serve n1's file
	if rec := serve("n2", leaf); rec.Code != 404 {
		t.Errorf("cross-node leak: code=%d", rec.Code)
	}
	// traversal + missing + unknown node
	if rec := serve("n1", "..%2f..%2fetc%2fpasswd"); rec.Code == 200 {
		t.Errorf("traversal served: code=%d", rec.Code)
	}
	if rec := serve("n1", "ghost.png"); rec.Code != 404 {
		t.Errorf("missing file: code=%d", rec.Code)
	}
	if rec := serve("ghost", leaf); rec.Code != 404 {
		t.Errorf("unknown node: code=%d", rec.Code)
	}
}

// Uploaded content must be served inert: nosniff always, inline only for a
// whitelist of safe raster images, everything else (SVG, HTML, unknown) forced
// to download so it can never run as same-origin active content.
func TestServeAttachmentHeaders(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}

	serve := func(name, mime, data string) *httptest.ResponseRecorder {
		att, err := a.storeAttachment("n1", name, mime, strings.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		leaf := filepath.Base(att.Path)
		req := httptest.NewRequest("GET", "/api/nodes/n1/attachments/"+leaf, nil)
		req.SetPathValue("id", "n1")
		req.SetPathValue("name", leaf)
		rec := httptest.NewRecorder()
		a.handleAttachment(rec, req)
		return rec
	}

	// safe raster: inline, no Content-Disposition, pinned content type
	rec := serve("pic.png", "image/png", "PNGDATA")
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("png: missing nosniff")
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "" {
		t.Errorf("png should render inline, got Content-Disposition %q", cd)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("png content-type %q", ct)
	}

	// active/unknown content: forced download
	for _, tc := range []struct{ name, mime string }{
		{"evil.svg", "image/svg+xml"},
		{"evil.html", "text/html"},
		{"data.bin", "application/octet-stream"},
	} {
		rec := serve(tc.name, tc.mime, "<x>")
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing nosniff", tc.name)
		}
		if !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
			t.Errorf("%s: not forced to download (Content-Disposition %q)", tc.name, rec.Header().Get("Content-Disposition"))
		}
		if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") || strings.Contains(ct, "svg") {
			// A download with an active content type + nosniff is still inert,
			// but the whitelist should never serve svg/html as its own type.
			t.Errorf("%s: served active content type %q", tc.name, ct)
		}
	}
}

func TestExtendPrompt(t *testing.T) {
	if got := extendPrompt("hello", nil); got != "hello" {
		t.Errorf("no attachments should pass text through, got %q", got)
	}
	img := Attachment{Path: "/a/b/x.png", Mime: "image/png"}
	got := extendPrompt("look", []Attachment{img})
	want := "look\n\n[attached image: /a/b/x.png]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// image-only turn: no leading blank lines, just the reference
	if got := extendPrompt("   ", []Attachment{img}); got != "[attached image: /a/b/x.png]" {
		t.Errorf("image-only got %q", got)
	}
	// non-image → "file"; multiple joined by newline
	multi := extendPrompt("t", []Attachment{{Path: "/a/b/x.png", Mime: "image/png"}, {Path: "/a/b/d.pdf", Mime: "application/pdf"}})
	if !strings.Contains(multi, "[attached image: /a/b/x.png]\n[attached file: /a/b/d.pdf]") {
		t.Errorf("multi/kind wrong: %q", multi)
	}
}

func TestResolveAttachmentsContainment(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1"}
	// a real upload for n1
	att, err := a.storeAttachment("n1", "ok.png", "image/png", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	// another node's upload — must be rejected for n1
	other, _ := a.storeAttachment("n2", "sneak.png", "image/png", strings.NewReader("y"))

	if got, err := a.resolveAttachments("n1", []Attachment{att}); err != nil || len(got) != 1 {
		t.Fatalf("valid ref rejected: %v", err)
	}
	for _, bad := range []Attachment{
		{Path: other.Path, Name: "sneak.png"},                           // sibling node's dir
		{Path: "/etc/passwd", Name: "passwd"},                           // absolute escape
		{Path: a.attachmentDir("n1") + "/../n2/x", Name: "x"},           // traversal out
		{Path: a.attachmentDir("n1") + "/ghost.png", Name: "ghost.png"}, // in-dir but missing
	} {
		if _, err := a.resolveAttachments("n1", []Attachment{bad}); err == nil {
			t.Errorf("expected rejection for %q", bad.Path)
		}
	}
}
