package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
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
	"github.com/scimux/scimux/internal/team"
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

func TestReferenceMediaTeamCaptureUsesExactRetainedAssociation(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	srv := httptest.NewServer(script)
	defer srv.Close()
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	a := newTestApp(t, &fakeTmux{})
	a.teamDataDir = dir
	a.teamHandler = h
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	var raster bytes.Buffer
	if err := png.Encode(&raster, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	data := raster.Bytes()
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.png", MIME: "image/png", ConfigHash: strings.Repeat("a", 64), Data: data}
	if err := team.SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": "Synthetic\n" + peopleAttachmentNotice, "content_text": "Synthetic", "anchor": 0, "people_attachments_version": 1, "people_attachments": []peopleAttachmentDescriptor{{ID: stage.ID, SHA256: stage.Hash, Name: stage.Name, MIME: stage.MIME, Size: int64(len(data))}}})
	record := team.MirrorRecord{Seq: 1, Key: "c/0123456789abcdef", Author: strings.Repeat("b", 32), At: "2026-01-01T00:00:00.000Z", Body: body}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{record}); err != nil {
		t.Fatal(err)
	}
	source := map[string]any{"team": map[string]any{"server": id.Server, "owner": id.Owner, "chat": id.Chat, "generation": id.Generation, "log": "comments", "seq": 1, "attachment_ids": []string{stage.ID}}}
	raw, _ := json.Marshal(map[string]any{"source": source})
	response := postReferenceCapture(t, a, string(raw))
	if response.Code != http.StatusOK {
		t.Fatalf("team capture: %d %s", response.Code, response.Body.String())
	}
	var out struct {
		Media referencemedia.Media `json:"media"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Media.Items) != 1 || out.Media.Items[0].State != "ready" {
		t.Fatalf("missing retained raster: %#v", out.Media)
	}
	wrong := map[string]any{"source": map[string]any{"team": map[string]any{"server": id.Server, "owner": id.Owner, "chat": id.Chat, "generation": id.Generation, "log": "comments", "seq": 2, "attachment_ids": []string{stage.ID}}}}
	wrongRaw, _ := json.Marshal(wrong)
	if got := postReferenceCapture(t, a, string(wrongRaw)); got.Code == http.StatusOK {
		t.Fatal("an attachment ID authorized another sequence")
	}
	wrong["source"].(map[string]any)["team"].(map[string]any)["seq"] = 1
	wrong["source"].(map[string]any)["team"].(map[string]any)["attachment_ids"] = []string{"fedcba9876543210"}
	wrongRaw, _ = json.Marshal(wrong)
	if got := postReferenceCapture(t, a, string(wrongRaw)); got.Code == http.StatusOK {
		t.Fatal("an unassociated attachment was captured")
	}
	if err := team.RemoveCopy(dir, id); err != nil {
		t.Fatal(err)
	}
	got, _, err := a.referenceMedia.ReadAsset(out.Media.CaptureID, "0")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("capture did not survive copy removal: %v", err)
	}
}

func TestReferenceMediaTeamSourceValidationAndUnavailableRetention(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	seedSub(t, dir, baseSub())
	a := newTestApp(t, &fakeTmux{})
	a.teamHandler, a.teamDataDir = h, dir
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	attachment := peopleAttachmentDescriptor{ID: "0123456789abcdef", SHA256: strings.Repeat("a", 64), Size: 1, Name: "synthetic.png", MIME: "image/png"}
	body, err := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": "Synthetic\n" + peopleAttachmentNotice, "content_text": "Synthetic", "anchor": 0, "people_attachments_version": 1, "people_attachments": []peopleAttachmentDescriptor{attachment}})
	if err != nil {
		t.Fatal(err)
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/0123456789abcdef", Author: strings.Repeat("b", 32), At: "2026-01-01T00:00:00.000Z", Body: body}}); err != nil {
		t.Fatal(err)
	}
	source := map[string]any{"server": id.Server, "owner": id.Owner, "chat": id.Chat, "generation": id.Generation, "log": "comments", "seq": 1, "attachment_ids": []string{attachment.ID}}
	request := func(fields map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"source": fields})
		if err != nil {
			t.Fatal(err)
		}
		return postReferenceCapture(t, a, string(raw))
	}
	for _, native := range []map[string]any{{"uid": "synthetic"}, {"segment": 0}, {"record": 0}} {
		native["team"] = source
		if got := request(native); got.Code != http.StatusBadRequest {
			t.Fatalf("mixed native/team source accepted: %d", got.Code)
		}
	}
	for _, invalid := range []map[string]any{
		{"server": id.Server, "owner": id.Owner, "chat": id.Chat, "generation": id.Generation, "log": "other", "seq": 1, "attachment_ids": []string{attachment.ID}},
		{"server": id.Server, "owner": id.Owner, "chat": id.Chat, "generation": id.Generation, "log": "comments", "seq": 0, "attachment_ids": []string{attachment.ID}},
		{"server": id.Server, "owner": id.Owner, "chat": id.Chat, "generation": id.Generation, "log": "comments", "seq": 1, "attachment_ids": []string{}},
	} {
		if got := request(map[string]any{"team": invalid}); got.Code != http.StatusBadRequest {
			t.Fatalf("invalid source accepted: %d", got.Code)
		}
	}
	foreign := map[string]any{"server": id.Server, "owner": strings.Repeat("f", 32), "chat": id.Chat, "generation": id.Generation, "log": "comments", "seq": 1, "attachment_ids": []string{attachment.ID}}
	if got := request(map[string]any{"team": foreign}); got.Code != http.StatusNotFound {
		t.Fatalf("foreign subscription accepted: %d", got.Code)
	}
	got := request(map[string]any{"team": source})
	if got.Code != http.StatusOK {
		t.Fatalf("retained association could not describe missing media: %d %s", got.Code, got.Body.String())
	}
	var result struct {
		Media referencemedia.Media `json:"media"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &result); err != nil || len(result.Media.Items) != 1 || result.Media.Items[0].State != referencemedia.StateUnavailable {
		t.Fatalf("missing retained bytes not reported: %v %#v", err, result.Media)
	}
}

func TestReferenceMediaTeamCaptureRejectsUnusableHistoryAndDamagedSubscriptions(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	seedSub(t, dir, baseSub())
	a := newTestApp(t, &fakeTmux{})
	a.teamHandler, a.teamDataDir = h, dir
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	for i, body := range []string{`{"v":2,"t":"comment","text":"synthetic","anchor":0}`, `{"v":1,"t":"comment","text":"synthetic text","anchor":0}`} {
		record := team.MirrorRecord{Seq: uint64(i + 1), Key: fmt.Sprintf("c/%016x", i+1), Author: strings.Repeat("b", 32), At: "2026-01-01T00:00:00.000Z", Body: json.RawMessage(body)}
		if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{record}); err != nil {
			t.Fatal(err)
		}
	}
	request := func(seq int) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]any{"source": map[string]any{"team": map[string]any{"server": id.Server, "owner": id.Owner, "chat": id.Chat, "generation": id.Generation, "log": "comments", "seq": seq, "attachment_ids": []string{"0123456789abcdef"}}}})
		if err != nil {
			t.Fatal(err)
		}
		return postReferenceCapture(t, a, string(body))
	}
	for _, seq := range []int{1, 2} {
		if rec := request(seq); rec.Code != http.StatusConflict {
			t.Fatalf("unusable source %d: status=%d", seq, rec.Code)
		}
	}
	attachment := peopleAttachmentDescriptor{ID: "0123456789abcdef", SHA256: strings.Repeat("a", 64), Size: 1, Name: "synthetic.png", MIME: "image/png"}
	body, err := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": "synthetic\n" + peopleAttachmentNotice, "content_text": "synthetic", "anchor": 0, "people_attachments_version": 1, "people_attachments": []peopleAttachmentDescriptor{attachment}})
	if err != nil {
		t.Fatal(err)
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 3, Key: "c/0000000000000003", Author: strings.Repeat("b", 32), At: "2026-01-01T00:00:00.000Z", Body: body}}); err != nil {
		t.Fatal(err)
	}
	blockedRoot := filepath.Join(t.TempDir(), "synthetic-media-root")
	if err := os.WriteFile(blockedRoot, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	a.referenceMedia = referencemedia.New(blockedRoot)
	if rec := request(3); rec.Code != http.StatusInternalServerError {
		t.Fatalf("capture store failure: status=%d", rec.Code)
	}
	path := filepath.Join(dir, team.SubsFile)
	if err := os.WriteFile(path, []byte("synthetic damaged subscriptions"), 0600); err != nil {
		t.Fatal(err)
	}
	if rec := request(1); rec.Code != http.StatusInternalServerError {
		t.Fatalf("damaged subscription store: status=%d", rec.Code)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "synthetic damaged subscriptions" {
		t.Fatalf("damaged data was changed: %v", err)
	}
}

func TestReferenceMediaTeamCaptureDoesNotTreatOtherFilesAsRasters(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	seedSub(t, dir, baseSub())
	a := newTestApp(t, &fakeTmux{})
	a.teamHandler, a.teamDataDir = h, dir
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		name string
		data []byte
	}{{"synthetic.png", []byte("not a raster")}, {"synthetic.txt", pngBytes.Bytes()}} {
		seq := uint64(i + 1)
		sum := sha256.Sum256(tc.data)
		stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: tc.name, MIME: "application/octet-stream", ConfigHash: strings.Repeat("a", 64), Data: tc.data}
		if err := team.SavePeoplePublished(dir, id, seq, stage); err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": peopleAttachmentNotice, "content_text": "", "anchor": 0, "people_attachments_version": 1, "people_attachments": []peopleAttachmentDescriptor{{ID: stage.ID, SHA256: stage.Hash, Name: stage.Name, MIME: stage.MIME, Size: int64(len(stage.Data))}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: seq, Key: fmt.Sprintf("c/%016x", seq), Author: strings.Repeat("b", 32), At: "2026-01-01T00:00:00.000Z", Body: body}}); err != nil {
			t.Fatal(err)
		}
		source := map[string]any{"team": map[string]any{"server": id.Server, "owner": id.Owner, "chat": id.Chat, "generation": id.Generation, "log": "comments", "seq": seq, "attachment_ids": []string{stage.ID}}}
		raw, err := json.Marshal(map[string]any{"source": source})
		if err != nil {
			t.Fatal(err)
		}
		rec := postReferenceCapture(t, a, string(raw))
		if rec.Code != http.StatusOK {
			t.Fatalf("capture status %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Media referencemedia.Media `json:"media"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Media.Items) != 1 || out.Media.Items[0].State != referencemedia.StateUnavailable {
			t.Fatalf("unsafe or misnamed raster captured: %v %#v", err, out.Media)
		}
	}
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
