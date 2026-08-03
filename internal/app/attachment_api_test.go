package app

// Packet 4C public-route coverage for attachment/upload and session-asset HTTP
// bindings through NewHandler. Complements — does not replace — the detailed
// direct-handler matrices in attachments_test.go, assets_test.go,
// upload_assets_test.go, chat_assets_test.go, or Packet 2E/4A delete-archive
// ordering tests.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/tmuxsession"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// attachmentAPIHandler builds the production router for public-route assertions.
func attachmentAPIHandler(t *testing.T, a *app) http.Handler {
	t.Helper()
	h, err := NewHandler(a, webFS)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// seedAttachNode registers a live-looking tmux node without going through create.
func seedAttachNode(a *app, id string) *Node {
	n := &Node{ID: id, Title: id, Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	a.nodes = append(a.nodes, n)
	a.byID[id] = n
	return n
}

// routeMultipart posts a multipart "files" upload through the outer mutation
// guard (CSRF + content-type). When files is empty, no form file field is
// written — that exercises the missing-file 400 path.
func routeMultipart(t *testing.T, h http.Handler, path string, files []struct {
	name string
	data []byte
}, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range files {
		fw, err := mw.CreateFormFile("files", f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787"+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if csrf {
		req.Header.Set("X-Scimux-CSRF", csrfToken)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func multipartOne(t *testing.T, h http.Handler, path, name string, data []byte, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	return routeMultipart(t, h, path, []struct {
		name string
		data []byte
	}{{name, data}}, csrf)
}

// ---------- multipart guard + upload success/failure ----------

func TestPublicRouteUploadMultipartGuardAndSuccess(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seedAttachNode(a, "n1")
	h := attachmentAPIHandler(t, a)

	// Missing CSRF → 403 before the handler runs.
	if rec := multipartOne(t, h, "/api/nodes/n1/attachments", "a.txt", []byte("x"), false); rec.Code != http.StatusForbidden {
		t.Fatalf("no CSRF: status = %d, want 403; body=%q", rec.Code, rec.Body.String())
	}

	// Multipart is accepted only on the attachment route — elsewhere 415.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("files", "x.txt")
	fw.Write([]byte("x"))
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/nodes/n1/send", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Scimux-CSRF", csrfToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("multipart on send: status = %d, want 415; body=%q", rec.Code, rec.Body.String())
	}

	// Unknown node → 404.
	if rec := multipartOne(t, h, "/api/nodes/ghost/attachments", "a.txt", []byte("x"), true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown node: status = %d, want 404", rec.Code)
	}

	// Ended node → 409.
	a.byID["n1"].EndedAt = "2026-07-23T00:00:00Z"
	if rec := multipartOne(t, h, "/api/nodes/n1/attachments", "a.txt", []byte("x"), true); rec.Code != http.StatusConflict {
		t.Fatalf("ended node: status = %d, want 409", rec.Code)
	}
	a.byID["n1"].EndedAt = ""

	// Missing files field → 400.
	if rec := routeMultipart(t, h, "/api/nodes/n1/attachments", nil, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing files: status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}

	// Multiple files → 400, nothing staged.
	if rec := routeMultipart(t, h, "/api/nodes/n1/attachments", []struct {
		name string
		data []byte
	}{{"a.txt", []byte("a")}, {"b.txt", []byte("b")}}, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("multi-file: status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	if entries, err := os.ReadDir(a.attachmentDir("n1")); err == nil && len(entries) != 0 {
		t.Errorf("multi-file left staged attachments: %v", entries)
	}

	// Traversal filename is normalized beneath the node directory.
	rec = multipartOne(t, h, "/api/nodes/n1/attachments", "../../../etc/evil", []byte("evil"), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("traversal upload: status = %d body %q", rec.Code, rec.Body.String())
	}
	var trav struct{ Attachments []Attachment }
	if err := json.Unmarshal(rec.Body.Bytes(), &trav); err != nil {
		t.Fatal(err)
	}
	if len(trav.Attachments) != 1 {
		t.Fatalf("traversal attachments = %d, want 1", len(trav.Attachments))
	}
	nodeDir := a.attachmentDir("n1") + string(filepath.Separator)
	if !strings.HasPrefix(trav.Attachments[0].Path, nodeDir) {
		t.Errorf("traversal escaped: path %q not under %q", trav.Attachments[0].Path, nodeDir)
	}

	// Successful upload: metadata, assetId, exact stored/served bytes.
	payload := []byte("hello public upload")
	rec = multipartOne(t, h, "/api/nodes/n1/attachments", "hello.txt", payload, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: status = %d body %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var resp struct{ Attachments []Attachment }
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(resp.Attachments))
	}
	at := resp.Attachments[0]
	if at.Name != "hello.txt" || at.AssetID == "" || at.Size != int64(len(payload)) {
		t.Fatalf("attachment = %+v", at)
	}
	if !strings.HasPrefix(at.Path, nodeDir) {
		t.Errorf("path %q not under node dir", at.Path)
	}
	stored, err := os.ReadFile(at.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, payload) {
		t.Errorf("stored bytes = %q, want %q", stored, payload)
	}
	// Attachment GET serves exact bytes.
	arec := routeRequest(h, http.MethodGet, "/api/nodes/n1/attachments/"+filepath.Base(at.Path), "", false)
	if arec.Code != http.StatusOK || !bytes.Equal(arec.Body.Bytes(), payload) {
		t.Fatalf("attachment GET: code=%d body=%q", arec.Code, arec.Body.String())
	}
	// Asset GET serves exact bytes.
	srec := routeRequest(h, http.MethodGet, "/api/nodes/n1/assets/"+at.AssetID, "", false)
	if srec.Code != http.StatusOK || !bytes.Equal(srec.Body.Bytes(), payload) {
		t.Fatalf("asset GET: code=%d body=%q", srec.Code, srec.Body.String())
	}
}

func TestPublicRouteUploadSizeCapAndIngestRollback(t *testing.T) {
	// Total body over attachUploadMax → 413 through the outer guard + handler.
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seedAttachNode(a, "n1")
	h := attachmentAPIHandler(t, a)

	// Body larger than the cap (file payload alone exceeds attachUploadMax).
	big := make([]byte, attachUploadMax+1)
	rec := multipartOne(t, h, "/api/nodes/n1/attachments", "huge.bin", big, true)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: status = %d, want 413; body=%q", rec.Code, rec.Body.String())
	}
	if entries, err := os.ReadDir(a.attachmentDir("n1")); err == nil && len(entries) != 0 {
		t.Errorf("oversized upload left staged files: %v", entries)
	}

	// Ingest failure after staging: force assetsDir to a plain file so blob
	// writes fail; upload must roll back the staged attachment and leave no
	// session-log asset reference.
	if err := os.RemoveAll(a.assetsDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.assetsDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, assetInlineCap+1)
	rec = multipartOne(t, h, "/api/nodes/n1/attachments", "fail.bin", blob, true)
	if rec.Code == http.StatusOK {
		t.Fatalf("want ingest failure, got 200: %s", rec.Body.String())
	}
	if entries, err := os.ReadDir(a.attachmentDir("n1")); err == nil && len(entries) != 0 {
		t.Errorf("staged attachment left after ingest failure: %v", entries)
	}
	if a.sessionLogExists("n1") {
		// A false asset reference would appear in the log; any log creation
		// for this node on a failed upload is a contract break.
		idx := sessionlog.ReadAssets(a.sessionLogPath("n1"))
		if len(idx) != 0 {
			t.Errorf("session-log asset refs after failed ingest: %v", idx)
		}
	}
}

// ---------- prompt attachment expansion / rejection via public send ----------

func TestPublicRouteSendAttachmentResolveAndExpand(t *testing.T) {
	// Capture load-buffer stdin so we can assert extendPrompt ran on the
	// public send path without changing the shared fakeTmux.
	f := &fakeTmux{alive: map[string]bool{"n1": true}, capture: "static"}
	var pasted string
	root := t.TempDir()
	home := filepath.Join(root, "home")
	data := filepath.Join(root, "data")
	for _, d := range []string{home, data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	run := func(ctx context.Context, stdin string, args ...string) (string, error) {
		if len(args) >= 3 && args[2] == "load-buffer" {
			pasted = stdin
		}
		return f.run(ctx, stdin, args...)
	}
	a, err := newApp(Config{
		Home: home, DataDir: data,
		LaunchGrace: 40 * time.Millisecond, LaunchPoll: 5 * time.Millisecond,
	}, appDeps{Server: tmuxsession.NewServerWithRunner("testsock", run)})
	if err != nil {
		t.Fatal(err)
	}
	a.server.PasteDelay, a.server.AckPoll = time.Millisecond, time.Millisecond
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seedAttachNode(a, "n1")
	seedAttachNode(a, "n2")
	h := attachmentAPIHandler(t, a)

	// Stage a real upload for n1 and a sibling for n2.
	rec := multipartOne(t, h, "/api/nodes/n1/attachments", "ok.png", []byte("PNG"), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload n1: status = %d body %q", rec.Code, rec.Body.String())
	}
	var up struct{ Attachments []Attachment }
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	att := up.Attachments[0]

	rec = multipartOne(t, h, "/api/nodes/n2/attachments", "sneak.png", []byte("Y"), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload n2: status = %d", rec.Code)
	}
	var other struct{ Attachments []Attachment }
	if err := json.Unmarshal(rec.Body.Bytes(), &other); err != nil {
		t.Fatal(err)
	}

	// Directory under the node attachment dir must be rejected.
	dirPath := filepath.Join(a.attachmentDir("n1"), "subdir")
	if err := os.MkdirAll(dirPath, 0o700); err != nil {
		t.Fatal(err)
	}

	// Valid attachment expands the delivered prompt.
	body, _ := json.Marshal(map[string]any{
		"text":        "look",
		"attachments": []Attachment{att},
	})
	rec = routeRequest(h, http.MethodPost, "/api/nodes/n1/send", string(body), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("send with attachment: status = %d body %q", rec.Code, rec.Body.String())
	}
	wantMark := "[attached image: " + att.Path + "]"
	if !strings.Contains(pasted, "look") || !strings.Contains(pasted, wantMark) {
		t.Fatalf("paste missing expanded prompt: %q", pasted)
	}
	// Clear unconfirmed send state for subsequent rejection probes.
	routeRequest(h, http.MethodPost, "/api/nodes/n1/send/resolve", "", true)

	// Rejection matrix: cross-node, absolute, traversal, missing, directory.
	bads := []Attachment{
		{Path: other.Attachments[0].Path, Name: "sneak.png"},
		{Path: "/etc/passwd", Name: "passwd"},
		{Path: a.attachmentDir("n1") + "/../n2/x", Name: "x"},
		{Path: a.attachmentDir("n1") + "/ghost.png", Name: "ghost.png"},
		{Path: dirPath, Name: "subdir"},
	}
	for _, bad := range bads {
		pasted = ""
		b, _ := json.Marshal(map[string]any{
			"text":        "nope",
			"attachments": []Attachment{bad},
		})
		rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/send", string(b), true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("ref %q: status = %d, want 400; body=%q", bad.Path, rec.Code, rec.Body.String())
		}
		if pasted != "" {
			t.Errorf("ref %q: paste ran despite rejection: %q", bad.Path, pasted)
		}
	}
}

// ---------- attachment + asset GET serving / headers / containment ----------

func TestPublicRouteAttachmentAndAssetGET(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true, "n2": true}}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seedAttachNode(a, "n1")
	seedAttachNode(a, "n2")
	h := attachmentAPIHandler(t, a)

	// Raster attachment: nosniff + pinned image type, inline (no disposition).
	rec := multipartOne(t, h, "/api/nodes/n1/attachments", "pic.png", []byte("PNGDATA"), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload png: %d %s", rec.Code, rec.Body.String())
	}
	var up struct{ Attachments []Attachment }
	json.Unmarshal(rec.Body.Bytes(), &up)
	png := up.Attachments[0]
	leaf := filepath.Base(png.Path)

	grec := routeRequest(h, http.MethodGet, "/api/nodes/n1/attachments/"+leaf, "", false)
	if grec.Code != http.StatusOK || grec.Body.String() != "PNGDATA" {
		t.Fatalf("serve png: code=%d body=%q", grec.Code, grec.Body.String())
	}
	if grec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("png attachment missing nosniff")
	}
	if ct := grec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("png Content-Type = %q, want image/png", ct)
	}
	if cd := grec.Header().Get("Content-Disposition"); cd != "" {
		t.Errorf("raster should be inline, got Content-Disposition %q", cd)
	}

	// Active/unknown content forced to opaque download.
	for _, tc := range []struct{ name, data string }{
		{"evil.svg", "<svg/>"},
		{"evil.html", "<html>"},
		{"data.bin", "\x00\x01"},
	} {
		rec := multipartOne(t, h, "/api/nodes/n1/attachments", tc.name, []byte(tc.data), true)
		if rec.Code != http.StatusOK {
			t.Fatalf("upload %s: %d", tc.name, rec.Code)
		}
		var r struct{ Attachments []Attachment }
		json.Unmarshal(rec.Body.Bytes(), &r)
		g := routeRequest(h, http.MethodGet, "/api/nodes/n1/attachments/"+filepath.Base(r.Attachments[0].Path), "", false)
		if g.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing nosniff", tc.name)
		}
		if !strings.HasPrefix(g.Header().Get("Content-Disposition"), "attachment") {
			t.Errorf("%s: not forced download (CD=%q)", tc.name, g.Header().Get("Content-Disposition"))
		}
		if ct := g.Header().Get("Content-Type"); strings.Contains(ct, "html") || strings.Contains(ct, "svg") {
			t.Errorf("%s: active content type %q", tc.name, ct)
		}
		if ct := g.Header().Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("%s: Content-Type = %q, want application/octet-stream", tc.name, ct)
		}
	}

	// Attachment: unknown node, cross-node, missing, traversal → 404.
	if r := routeRequest(h, http.MethodGet, "/api/nodes/ghost/attachments/"+leaf, "", false); r.Code != http.StatusNotFound {
		t.Errorf("unknown node attachment: %d", r.Code)
	}
	if r := routeRequest(h, http.MethodGet, "/api/nodes/n2/attachments/"+leaf, "", false); r.Code != http.StatusNotFound {
		t.Errorf("cross-node attachment: %d", r.Code)
	}
	if r := routeRequest(h, http.MethodGet, "/api/nodes/n1/attachments/ghost.png", "", false); r.Code != http.StatusNotFound {
		t.Errorf("missing attachment: %d", r.Code)
	}
	if r := routeRequest(h, http.MethodGet, "/api/nodes/n1/attachments/..%2f..%2fetc%2fpasswd", "", false); r.Code == http.StatusOK {
		t.Errorf("traversal attachment served: %d", r.Code)
	}

	// Inline asset (raster) + blob asset (pdf) through public GET.
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_inline", Name: "sketch.png", Mime: "image/png", Size: 5,
		Storage: "inline", Bytes: base64.StdEncoding.EncodeToString([]byte("PNGDA")),
	})
	irec := routeRequest(h, http.MethodGet, "/api/nodes/n1/assets/a_inline", "", false)
	if irec.Code != http.StatusOK || irec.Body.String() != "PNGDA" {
		t.Fatalf("inline asset: code=%d body=%q", irec.Code, irec.Body.String())
	}
	if irec.Header().Get("X-Content-Type-Options") != "nosniff" || irec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("inline headers: nosniff=%q ct=%q", irec.Header().Get("X-Content-Type-Options"), irec.Header().Get("Content-Type"))
	}
	if irec.Header().Get("Content-Disposition") != "" {
		t.Errorf("inline raster disposition = %q", irec.Header().Get("Content-Disposition"))
	}

	relPath, err := asset.WriteBlob(a.assetsDir, "n1", "a_blob", "report.pdf", []byte("PDFDATA"))
	if err != nil {
		t.Fatal(err)
	}
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_blob", Name: "report.pdf", Mime: "application/pdf", Size: 7,
		Storage: "blob", BlobPath: relPath,
	})
	brec := routeRequest(h, http.MethodGet, "/api/nodes/n1/assets/a_blob", "", false)
	if brec.Code != http.StatusOK || brec.Body.String() != "PDFDATA" {
		t.Fatalf("blob asset: code=%d body=%q", brec.Code, brec.Body.String())
	}
	if !strings.HasPrefix(brec.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("pdf not forced download: %q", brec.Header().Get("Content-Disposition"))
	}
	if ct := brec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("pdf Content-Type = %q", ct)
	}

	// Active content assets forced download with nosniff.
	for i, tc := range []struct{ name, mime string }{
		{"evil.svg", "image/svg+xml"},
		{"evil.html", "text/html"},
		{"data.bin", "application/octet-stream"},
	} {
		id := "a_x" + strconv.Itoa(i)
		appendAsset(t, a, "n1", sessionlog.AssetEvent{
			ID: id, Name: tc.name, Mime: tc.mime, Storage: "inline",
			Bytes: base64.StdEncoding.EncodeToString([]byte("<x>")),
		})
		r := routeRequest(h, http.MethodGet, "/api/nodes/n1/assets/"+id, "", false)
		if r.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s asset: missing nosniff", tc.name)
		}
		if !strings.HasPrefix(r.Header().Get("Content-Disposition"), "attachment") {
			t.Errorf("%s asset: not forced download", tc.name)
		}
		if ct := r.Header().Get("Content-Type"); strings.Contains(ct, "html") || strings.Contains(ct, "svg") {
			t.Errorf("%s asset: active type %q", tc.name, ct)
		}
	}

	// Unknown node/ID, cross-node, traversal blob, missing blob, corrupt inline.
	if r := routeRequest(h, http.MethodGet, "/api/nodes/ghost/assets/a_inline", "", false); r.Code != http.StatusNotFound {
		t.Errorf("unknown node asset: %d", r.Code)
	}
	if r := routeRequest(h, http.MethodGet, "/api/nodes/n1/assets/ghost", "", false); r.Code != http.StatusNotFound {
		t.Errorf("unknown asset id: %d", r.Code)
	}
	if r := routeRequest(h, http.MethodGet, "/api/nodes/n2/assets/a_inline", "", false); r.Code != http.StatusNotFound {
		t.Errorf("cross-node asset: %d", r.Code)
	}
	if _, err := asset.WriteBlob(a.assetsDir, "n2", "a_sneak", "sneak.png", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_trav", Name: "sneak.png", Storage: "blob", BlobPath: "n2/a_sneak-sneak.png",
	})
	if r := routeRequest(h, http.MethodGet, "/api/nodes/n1/assets/a_trav", "", false); r.Code != http.StatusNotFound {
		t.Errorf("traversal blobPath: %d", r.Code)
	}
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_miss", Name: "gone.pdf", Storage: "blob", BlobPath: "n1/a_miss-gone.pdf",
	})
	if r := routeRequest(h, http.MethodGet, "/api/nodes/n1/assets/a_miss", "", false); r.Code != http.StatusNotFound {
		t.Errorf("missing blob: %d", r.Code)
	}
	appendAsset(t, a, "n1", sessionlog.AssetEvent{
		ID: "a_bad", Name: "x.png", Storage: "inline", Bytes: "not-valid-base64!!",
	})
	if r := routeRequest(h, http.MethodGet, "/api/nodes/n1/assets/a_bad", "", false); r.Code != http.StatusNotFound {
		t.Errorf("corrupt inline: %d", r.Code)
	}
}

// ---------- session-log asset references + chat projection (public) ----------

func TestPublicRouteChatProjectsUploadAsset(t *testing.T) {
	// Complements chat_assets_test.go: same projection contract, through
	// NewHandler's public GET /api/nodes/{id}/chat binding.
	f := &fakeTmux{}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seedAttachNode(a, "c1")
	w := &sessionlog.Writer{Path: a.sessionLogPath("c1")}
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
	h := attachmentAPIHandler(t, a)

	rec := routeRequest(h, http.MethodGet, "/api/nodes/c1/chat", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: status = %d body %q", rec.Code, rec.Body.String())
	}
	var body struct {
		Turns  []transcript.Turn         `json:"turns"`
		Assets map[string]map[string]any `json:"assets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(body.Turns))
	}
	if strings.Contains(body.Turns[0].Text, "[attached") {
		t.Errorf("raw marker still present: %q", body.Turns[0].Text)
	}
	if !strings.Contains(body.Turns[0].Text, "![photo.png](scimux-asset:a_1)") {
		t.Errorf("projection missing: %q", body.Turns[0].Text)
	}
	as, ok := body.Assets["a_1"]
	if !ok {
		t.Fatalf("assets missing a_1: %+v", body.Assets)
	}
	if as["name"] != "photo.png" || as["inline"] != true {
		t.Errorf("asset summary = %+v", as)
	}
}

// ---------- delete-driven archive + live routes go dark ----------

func TestPublicRouteDeleteArchivesAttachmentsAndAssets(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seedAttachNode(a, "n1")
	h := attachmentAPIHandler(t, a)

	// Upload a small (inline) and force a blob-sized asset so both archive
	// helpers have live material under attachments/ and assets/.
	rec := multipartOne(t, h, "/api/nodes/n1/attachments", "note.txt", []byte("hi"), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload note: %d %s", rec.Code, rec.Body.String())
	}
	var up struct{ Attachments []Attachment }
	json.Unmarshal(rec.Body.Bytes(), &up)
	att := up.Attachments[0]
	leaf := filepath.Base(att.Path)
	assetID := att.AssetID

	// Live GETs work before delete.
	if r := routeRequest(h, http.MethodGet, "/api/nodes/n1/attachments/"+leaf, "", false); r.Code != http.StatusOK {
		t.Fatalf("pre-delete attachment GET: %d", r.Code)
	}
	if r := routeRequest(h, http.MethodGet, "/api/nodes/n1/assets/"+assetID, "", false); r.Code != http.StatusOK {
		t.Fatalf("pre-delete asset GET: %d", r.Code)
	}

	// Ensure a blob directory exists so archiveAssets has something to move
	// (inline-only uploads leave no assetsDir entry).
	if _, err := a.ingestAttachmentAsset("n1", "big.bin", "application/octet-stream", "/tmp/u", make([]byte, assetInlineCap+1)); err != nil {
		t.Fatal(err)
	}

	// Delete through the public binding.
	drec := routeRequest(h, http.MethodDelete, "/api/nodes/n1", "", true)
	if drec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d body %q", drec.Code, drec.Body.String())
	}
	if _, ok := a.byID["n1"]; ok {
		t.Fatal("node still registered")
	}
	if _, err := os.Stat(a.attachmentDir("n1")); !os.IsNotExist(err) {
		t.Errorf("live attachments still present: %v", err)
	}
	if _, err := os.Stat(asset.NodeDir(a.assetsDir, "n1")); !os.IsNotExist(err) {
		t.Errorf("live assets still present: %v", err)
	}
	attArch, _ := filepath.Glob(filepath.Join(a.attachmentsDir, "archive", "n1.*"))
	if len(attArch) != 1 {
		t.Errorf("attachment archive = %v, want 1", attArch)
	}
	assetArch, _ := filepath.Glob(filepath.Join(a.assetsDir, "archive", "n1.*"))
	if len(assetArch) != 1 {
		t.Errorf("asset archive = %v, want 1", assetArch)
	}

	// Live routes become unavailable once the node is gone.
	if r := routeRequest(h, http.MethodGet, "/api/nodes/n1/attachments/"+leaf, "", false); r.Code != http.StatusNotFound {
		t.Errorf("post-delete attachment GET: %d, want 404", r.Code)
	}
	if r := routeRequest(h, http.MethodGet, "/api/nodes/n1/assets/"+assetID, "", false); r.Code != http.StatusNotFound {
		t.Errorf("post-delete asset GET: %d, want 404", r.Code)
	}
}
