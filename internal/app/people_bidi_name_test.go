package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/team"
)

func TestPeopleBidiFilenameAdmission(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	var puts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			puts++
			body, _ := io.ReadAll(r.Body)
			sum := sha256.Sum256(body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":"%s","size":%d}`, hex.EncodeToString(sum[:]), len(body))
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments"
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	post := func(stageID, name string) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, err := form.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, base+"?stage_id="+stageID), &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, tc := range []struct{ name, stageID, file string }{
		{"embedded override", "0123456789abcdef", "report\u202etxt.exe"},
		{"only bidi", "fedcba9876543210", "\u202e"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := puts
			rec := post(tc.stageID, tc.file)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad_request") || puts != before {
				t.Fatalf("bidi upload status=%d puts=%d body=%s", rec.Code, puts-before, rec.Body.String())
			}
			if _, err := team.ReadPeopleStage(dir, id, tc.stageID); !errors.Is(err, team.ErrPeopleStageMissing) {
				t.Fatalf("rejected upload stored a stage: %v", err)
			}
		})
	}
	const unicodeName = "文件 café.txt"
	before := puts
	accepted := post("0011223344556677", unicodeName)
	if accepted.Code != http.StatusOK || !strings.Contains(accepted.Body.String(), unicodeName) || puts != before+1 {
		t.Fatalf("unicode upload status=%d puts=%d body=%s", accepted.Code, puts-before, accepted.Body.String())
	}
	staged, err := team.ReadPeopleStage(dir, id, "0011223344556677")
	if err != nil || staged.Name != unicodeName {
		t.Fatalf("unicode stage name=%q err=%v", staged.Name, err)
	}
}

func TestPeopleBidiFilenameProjectionAndDownload(t *testing.T) {
	for _, tc := range []struct{ name, stored, want string }{
		{"alef mark", "a\u061cb", "ab"},
		{"left to right", "a\u200eb", "ab"},
		{"right to left", "a\u200fb", "ab"},
		{"embed start", "a\u202ab", "ab"},
		{"embed end", "a\u202eb", "ab"},
		{"isolate start", "a\u2066b", "ab"},
		{"isolate end", "a\u2069b", "ab"},
		{"override spoof", "report\u202etxt.exe", "reporttxt.exe"},
		{"only bidi", "\u202e", "file"},
		{"dot", ".\u200e", "file"},
		{"dot dot", "..\u200e", "file"},
		{"dot bidi dot", ".\u200e.", "file"},
		{"zwj kept", "a\u200db", "a\u200db"},
		{"before isolate", "a\u2065b", "a\u2065b"},
		{"after override", "a\u202fb", "a\u202fb"},
		{"after isolate", "a\u206ab", "a\u206ab"},
		{"quoted unicode", "报告 \"café\".txt", "报告 \"café\".txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{
				"v": 1, "t": "comment", "text": peopleAttachmentNotice, "content_text": "", "anchor": 0,
				"people_attachments_version": 1,
				"people_attachments": []map[string]any{{
					"id": "0123456789abcdef", "sha256": strings.Repeat("a", 64), "size": 1, "name": tc.stored, "mime": "text/plain",
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			entry, _, ok := projectCommentRecord(team.MirrorRecord{Seq: 1, Author: "synthetic", At: "2026-01-01T00:00:00.000Z", Body: raw}, "")
			files, _ := entry["attachments"].([]map[string]any)
			if !ok || len(files) != 1 || files[0]["name"] != tc.want {
				t.Fatalf("projection name=%#v ok=%v entry=%#v", files, ok, entry)
			}
		})
	}

	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	server := labEntry(teamServerID, "http://127.0.0.1:1", teamToken)
	seedTeam(t, dir, server)
	seedSub(t, dir, baseSub())
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct{ stored, want string }{
		{"report\u202etxt.exe", "reporttxt.exe"},
		{"\u061c\u200e\u200f\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069", "file"},
		{"文件 café.txt", "文件 café.txt"},
	} {
		data := []byte("synthetic retained bytes")
		sum := sha256.Sum256(data)
		stage := team.PeopleStage{ID: []string{"0123456789abcdef", "fedcba9876543210", "0011223344556677"}[i], Hash: hex.EncodeToString(sum[:]), Name: tc.stored, MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
		if err := team.SavePeoplePublished(dir, id, uint64(i+1), stage); err != nil {
			t.Fatal(err)
		}
		req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, "/?copy_instance="+instance, nil), id, "")
		rec := httptest.NewRecorder()
		h.readPeopleAttachment(rec, req, id, uint64(i+1), stage.ID)
		finish()
		kind, params, err := mime.ParseMediaType(rec.Header().Get("Content-Disposition"))
		if rec.Code != http.StatusOK || err != nil || kind != "attachment" || params["filename"] != tc.want || strings.ContainsRune(rec.Header().Get("Content-Disposition"), '\u202e') {
			t.Fatalf("download %q: status=%d disposition=%q err=%v", tc.stored, rec.Code, rec.Header().Get("Content-Disposition"), err)
		}
		stored, err := team.ReadPeoplePublished(dir, id, uint64(i+1), stage.ID)
		if err != nil || stored.Name != tc.stored || !bytes.Equal(stored.Data, data) {
			t.Fatalf("stored name rewritten: got %q err=%v", stored.Name, err)
		}
	}
}

func TestPeopleBidiStageMetadataShowsVisibleName(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	server := labEntry(teamServerID, "http://127.0.0.1:1", teamToken)
	seedTeam(t, dir, server)
	sub := baseSub()
	sub.Access = "write"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic staged file")
	sum := sha256.Sum256(data)
	const stored = "report\u202etxt.exe"
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: stored, MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments/stage/" + stage.ID + "?copy_instance=" + instance
	rec := doTeam(h, http.MethodGet, path, "")
	var body struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || body.Name != "reporttxt.exe" || strings.ContainsRune(rec.Body.String(), '\u202e') {
		t.Fatalf("stage metadata name=%q status=%d body=%s", body.Name, rec.Code, rec.Body.String())
	}
	got, err := team.ReadPeopleStage(dir, id, stage.ID)
	if err != nil || got.Name != stored || !bytes.Equal(got.Data, data) {
		t.Fatalf("stored stage name=%q err=%v", got.Name, err)
	}
}
