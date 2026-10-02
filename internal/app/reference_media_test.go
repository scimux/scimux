package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/asset"
	"github.com/scimux/scimux/internal/referencemedia"
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/storagebudget"
)

var referencePNG = []byte("\x89PNG\r\n\x1a\nsynthetic-reference")

func referenceFixture(t *testing.T) (*app, string) {
	t.Helper()
	a := newTestApp(t, &fakeTmux{})
	must(t, os.MkdirAll(a.sessionsDir, 0o700))
	n := &Node{ID: "capture-node", Title: "Capture", Agent: "codex", CreatedAt: "2026-09-24T00:00:00Z"}
	a.nodes, a.byID[n.ID] = []*Node{n}, n
	w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	meta := sessionlog.NewMeta(n.ID, n.Agent, "", "", a.home)
	must(t, w.Append(meta))
	must(t, w.Append(sessionlog.NewAsset(sessionlog.AssetEvent{
		ID: "a_1", Name: "chart.png", Mime: "image/png", Size: int64(len(referencePNG)),
		SHA256: "test", Storage: "inline", Bytes: base64.StdEncoding.EncodeToString(referencePNG),
		SourceKind: "upload", SourcePath: "/managed/chart.png",
	})))
	must(t, w.Append(sessionlog.Event{T: "assistant", Time: "2026-09-24T01:00:00Z", Text: "before\n\n[attached image: /managed/chart.png]\n\nafter"}))
	return a, meta.Meta.UID
}

func postReferenceCapture(t *testing.T, a *app, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/reference-media", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	a.handleReferenceMediaCapture(rec, req)
	return rec
}

func TestReferenceMediaCaptureAndReadSurviveSourceDeletionAndRestart(t *testing.T) {
	a, uid := referenceFixture(t)
	rec := postReferenceCapture(t, a, `{"source":{"uid":"`+uid+`","segment":0,"record":2}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("capture = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Text  string               `json:"text"`
		Media referencemedia.Media `json:"media"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "scimux-asset:a_1") || len(out.Media.Items) != 1 || out.Media.Items[0].State != "ready" {
		t.Fatalf("capture = %#v", out)
	}

	// Removing the exact source and constructing a fresh store does not affect bytes.
	must(t, os.Remove(a.sessionLogPath("capture-node")))
	a.referenceMedia = referencemedia.New(filepath.Join(filepath.Dir(a.sessionsDir), "reference-media"))
	asset := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/reference-media/"+out.Media.CaptureID+"/assets/0", nil)
	req.SetPathValue("captureID", out.Media.CaptureID)
	req.SetPathValue("itemID", "0")
	a.handleReferenceMediaAsset(asset, req)
	if asset.Code != http.StatusOK || !bytes.Equal(asset.Body.Bytes(), referencePNG) {
		t.Fatalf("asset = %d %q", asset.Code, asset.Body.Bytes())
	}
	if asset.Header().Get("Content-Type") != "image/png" || asset.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers = %#v", asset.Header())
	}
}

func TestReferenceMediaCaptureValidationAndExactAddress(t *testing.T) {
	a, uid := referenceFixture(t)
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{}`, 400},
		{`{"source":{"uid":"` + uid + `","record":2}}`, 400},
		{`{"source":{"uid":"` + uid + `","segment":0}}`, 400},
		{`{"source":{"uid":"` + uid + `","segment":0,"record":99}}`, 404},
		{`{"source":{"uid":"missing","segment":0,"record":2}}`, 404},
		{`{`, 400},
	} {
		if got := postReferenceCapture(t, a, tc.body); got.Code != tc.code {
			t.Errorf("%s: code=%d body=%s", tc.body, got.Code, got.Body.String())
		}
	}
}

func TestReferenceMediaCaptureProjectsImageAtOwningTurn(t *testing.T) {
	a, uid := referenceFixture(t)
	w := &sessionlog.Writer{Path: a.sessionLogPath("capture-node")}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "![plot](/managed/generated.png)"}))
	must(t, w.Append(sessionlog.NewAsset(sessionlog.AssetEvent{
		ID: "a_generated", Name: "generated.png", Mime: "image/png", Size: int64(len(referencePNG)),
		Storage: "inline", Bytes: base64.StdEncoding.EncodeToString(referencePNG),
		SourceKind: "agent_path", SourcePath: "/managed/generated.png",
	})))
	rec := postReferenceCapture(t, a, `{"source":{"uid":"`+uid+`","segment":0,"record":3}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("capture = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Text  string               `json:"text"`
		Media referencemedia.Media `json:"media"`
	}
	must(t, json.Unmarshal(rec.Body.Bytes(), &out))
	if !strings.Contains(out.Text, "scimux-asset:a_generated") || len(out.Media.Items) != 1 || out.Media.Items[0].State != "ready" {
		t.Fatalf("owning turn image was lost: %#v", out)
	}
	data, mime, err := a.referenceMedia.ReadAsset(out.Media.CaptureID, "0")
	must(t, err)
	if mime != "image/png" || !bytes.Equal(data, referencePNG) {
		t.Fatal("captured image differs from the owning turn's asset")
	}
}

func TestReferenceMediaCaptureBlockedTextDoesNotDependOnFilesystem(t *testing.T) {
	a, uid := referenceFixture(t)
	dir := t.TempDir()
	a.byID["capture-node"].Dir = dir
	w := &sessionlog.Writer{Path: a.sessionLogPath("capture-node")}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "see [report](report.md) for details"}))
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{
		TurnRecord: 3, Occurrence: 0, Ref: "report.md", Alt: "report", Reason: "outside_workspace",
	})))
	request := `{"source":{"uid":"` + uid + `","segment":0,"record":3}}`
	missing := postReferenceCapture(t, a, request)
	if missing.Code != http.StatusOK {
		t.Fatalf("missing source capture = %d %s", missing.Code, missing.Body.String())
	}
	must(t, os.WriteFile(filepath.Join(dir, "report.md"), []byte("now present"), 0o600))
	present := postReferenceCapture(t, a, request)
	if present.Code != http.StatusOK {
		t.Fatalf("present source capture = %d %s", present.Code, present.Body.String())
	}
	var before, after struct {
		Text string `json:"text"`
	}
	must(t, json.Unmarshal(missing.Body.Bytes(), &before))
	must(t, json.Unmarshal(present.Body.Bytes(), &after))
	if before.Text != "see report for details" || after.Text != before.Text {
		t.Fatalf("capture text changed with filesystem: before=%q after=%q", before.Text, after.Text)
	}

	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "[my file.md](.)"}))
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{
		TurnRecord: 5, Occurrence: 0, Ref: ".", Alt: "my file.md", Reason: "not_found",
	})))
	encoded := postReferenceCapture(t, a, `{"source":{"uid":"`+uid+`","segment":0,"record":5}}`)
	if encoded.Code != http.StatusOK {
		t.Fatalf("encoded label capture = %d %s", encoded.Code, encoded.Body.String())
	}
	var fallback struct {
		Text string `json:"text"`
	}
	must(t, json.Unmarshal(encoded.Body.Bytes(), &fallback))
	if fallback.Text != "my file.md" {
		t.Fatalf("encoded label leaked into capture: %q", fallback.Text)
	}
}

func TestReferenceMediaCaptureDetectsSourceRaceAndStoreFailure(t *testing.T) {
	a, uid := referenceFixture(t)
	a.referenceCaptureItemsHook = func(string, string, string) ([]referencemedia.CaptureItem, error) {
		delete(a.byID, "capture-node")
		a.nodes = nil
		return nil, nil
	}
	rec := postReferenceCapture(t, a, `{"source":{"uid":"`+uid+`","segment":0,"record":2}}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("race = %d %s", rec.Code, rec.Body.String())
	}

	a, uid = referenceFixture(t)
	rootFile := filepath.Join(t.TempDir(), "media-root")
	must(t, os.WriteFile(rootFile, []byte("x"), 0o600))
	a.referenceMedia = referencemedia.New(rootFile)
	rec = postReferenceCapture(t, a, `{"source":{"uid":"`+uid+`","segment":0,"record":2}}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("store failure = %d %s", rec.Code, rec.Body.String())
	}
}

func TestReferenceMediaAssetStrictIDsAndUnavailable(t *testing.T) {
	a, _ := referenceFixture(t)
	media, err := a.referenceMedia.Capture(referencemedia.Source{UID: "u", Segment: 0, Record: 0}, "x", []referencemedia.CaptureItem{{Key: "asset:old", Name: "old.jpg", State: referencemedia.StateUnavailable}})
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"BAD", "0"}, {media.CaptureID, "00"}, {media.CaptureID, "0"}} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.SetPathValue("captureID", pair[0])
		req.SetPathValue("itemID", pair[1])
		a.handleReferenceMediaAsset(rec, req)
		want := 404
		if pair[0] == "BAD" || pair[1] == "00" {
			want = 400
		}
		if rec.Code != want {
			t.Errorf("%v = %d, want %d", pair, rec.Code, want)
		}
	}
}

func TestReferenceMediaCaptureFiltersFilesAndCapturesOrdinaryImageLinks(t *testing.T) {
	a, uid := referenceFixture(t)
	w := &sessionlog.Writer{Path: a.sessionLogPath("capture-node")}
	must(t, w.Append(sessionlog.NewAsset(sessionlog.AssetEvent{
		ID: "a_2", Name: "manual.pdf", Mime: "application/pdf", Storage: "inline",
		Bytes: base64.StdEncoding.EncodeToString([]byte("synthetic pdf")),
	})))
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "[chart](scimux-asset:a_1) and [manual](scimux-asset:a_2)"}))
	rec := postReferenceCapture(t, a, `{"source":{"uid":"`+uid+`","segment":0,"record":4}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("capture = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Text  string               `json:"text"`
		Media referencemedia.Media `json:"media"`
	}
	must(t, json.Unmarshal(rec.Body.Bytes(), &out))
	if len(out.Media.Items) != 1 || out.Media.Items[0].Key != "asset:a_1" {
		t.Fatalf("media = %#v", out.Media)
	}
	if !strings.Contains(out.Text, "scimux-asset:a_2") {
		t.Fatalf("ordinary file marker was lost: %q", out.Text)
	}
}

func TestReferenceMediaCaptureEnforcesLimitsBeforePublication(t *testing.T) {
	a, uid := referenceFixture(t)
	w := &sessionlog.Writer{Path: a.sessionLogPath("capture-node")}
	must(t, w.Append(sessionlog.NewAsset(sessionlog.AssetEvent{
		ID: "a_2", Name: "second.png", Mime: "image/png", Storage: "inline",
		Bytes: base64.StdEncoding.EncodeToString(referencePNG),
	})))
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "![one](scimux-asset:a_1) ![two](scimux-asset:a_2)"}))
	a.referenceMedia.Limits.MaxItems = 1
	rec := postReferenceCapture(t, a, `{"source":{"uid":"`+uid+`","segment":0,"record":4}}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("descriptor limit = %d %s", rec.Code, rec.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(a.referenceMedia.Root, "captures"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("published captures after rejected preflight: %v", entries)
	}

	a.referenceMedia.Limits.MaxItems = 32
	a.referenceMedia.Limits.MaxTotalBytes = int64(len(referencePNG))
	rec = postReferenceCapture(t, a, `{"source":{"uid":"`+uid+`","segment":0,"record":4}}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("aggregate limit = %d %s", rec.Code, rec.Body.String())
	}
}

func TestReferenceCaptureItemClassificationAndReadFailures(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if got, err := a.referenceCaptureItems(filepath.Join(t.TempDir(), "none"), "", "plain text"); err != nil || got != nil {
		t.Fatalf("plain = %#v %v", got, err)
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	w := &sessionlog.Writer{Path: path}
	appendAsset := func(ev sessionlog.AssetEvent) { must(t, w.Append(sessionlog.NewAsset(ev))) }
	appendAsset(sessionlog.AssetEvent{ID: "pdf", Name: "manual.pdf", Mime: "application/pdf", Storage: "inline", Bytes: base64.StdEncoding.EncodeToString([]byte("pdf"))})
	appendAsset(sessionlog.AssetEvent{ID: "nameless", Storage: "inline", Bytes: base64.StdEncoding.EncodeToString(referencePNG)})
	appendAsset(sessionlog.AssetEvent{ID: "mismatch", Name: "x.png", Mime: "text/html", Storage: "inline", Bytes: base64.StdEncoding.EncodeToString(referencePNG)})
	appendAsset(sessionlog.AssetEvent{ID: "backing", Name: "x.png", Mime: "image/png", Size: 1, SHA256: "x", BackingID: "missing"})
	appendAsset(sessionlog.AssetEvent{ID: "storage", Name: "x.png", Mime: "image/png", Storage: "future"})
	appendAsset(sessionlog.AssetEvent{ID: "base64", Name: "x.png", Mime: "image/png", Storage: "inline", Bytes: "%%%"})
	appendAsset(sessionlog.AssetEvent{ID: "blob", Name: "x.png", Mime: "image/png", Storage: "blob", BlobPath: "node/blob.png"})

	items, err := a.referenceCaptureItems(path, "", "[file](scimux-asset:missing) ![gone](scimux-asset:gone) ![again](scimux-asset:gone) [pdf](scimux-asset:pdf)")
	if err != nil || len(items) != 1 || items[0].State != referencemedia.StateUnavailable {
		t.Fatalf("classification = %#v %v", items, err)
	}
	if items, err := a.referenceCaptureItems(path, "", "![x](scimux-asset:nameless)"); err != nil || len(items) != 0 {
		t.Fatalf("nameless = %#v %v", items, err)
	}
	a.referenceMedia.Limits.MaxTotalBytes = -1
	if _, err := a.referenceCaptureItems(path, "node", "![x](scimux-asset:base64)"); !errors.Is(err, referencemedia.ErrTooLarge) {
		t.Fatalf("negative remaining = %v", err)
	}
	a.referenceMedia.Limits = referencemedia.DefaultLimits()
	for _, tc := range []struct {
		name, text string
		want       error
	}{
		{"mime", "![x](scimux-asset:mismatch)", referencemedia.ErrInvalid},
		{"backing", "![x](scimux-asset:backing)", referencemedia.ErrCorrupt},
		{"storage", "![x](scimux-asset:storage)", referencemedia.ErrCorrupt},
		{"base64", "![x](scimux-asset:base64)", base64.CorruptInputError(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.referenceCaptureItems(path, "node", tc.text); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	items, err = a.referenceCaptureItems(path, "", "![x](scimux-asset:blob)")
	if err != nil || len(items) != 1 || items[0].State != referencemedia.StateUnavailable {
		t.Fatalf("deleted blob = %#v %v", items, err)
	}
	if _, err = a.referenceCaptureItems(path, "node", "![x](scimux-asset:blob)"); err == nil {
		t.Fatal("missing live blob succeeded")
	}

	nodeDir := asset.NodeDir(a.assetsDir, "node")
	must(t, os.MkdirAll(nodeDir, 0o700))
	full := filepath.Join(nodeDir, "blob.png")
	must(t, os.WriteFile(full, referencePNG, 0o600))
	appendAsset(sessionlog.AssetEvent{ID: "blobok", Name: "ok.png", Mime: "image/png", Storage: "blob", BlobPath: "node/blob.png"})
	items, err = a.referenceCaptureItems(path, "node", "![x](scimux-asset:blobok)")
	if err != nil || len(items) != 1 || !bytes.Equal(items[0].Data, referencePNG) {
		t.Fatalf("live blob = %#v %v", items, err)
	}
}

func TestReferenceMediaErrorStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{
		{referencemedia.ErrTooLarge, http.StatusRequestEntityTooLarge},
		{referencemedia.ErrInvalid, http.StatusBadRequest},
		{storagebudget.ErrBudget, http.StatusInsufficientStorage},
		{errors.New("io"), http.StatusInternalServerError},
	} {
		rec := httptest.NewRecorder()
		referenceMediaError(rec, tc.err)
		if rec.Code != tc.code {
			t.Fatalf("%v = %d want %d", tc.err, rec.Code, tc.code)
		}
	}
}
