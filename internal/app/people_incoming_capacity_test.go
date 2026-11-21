package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/team"
)

// capacityIncomingFixture serves one authenticated synthetic comment and blob.
// The download URL carries only the current copy identity. Kept conversation
// files and this seat's working files have separate per-chat caps, and kept
// files are outside global managed usage, so the fixture does not opt into a
// parked retention query.
func capacityIncomingFixture(t *testing.T, payload []byte) (http.Handler, string, team.CopyID, string, *atomic.Int64) {
	t.Helper()
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "read", 0))}
	gets := new(atomic.Int64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			if r.Method != http.MethodGet || r.URL.Query().Get("generation") != subGen {
				t.Error("blob request did not carry its generation")
			}
			gets.Add(1)
			_, _ = w.Write(payload)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	fileID := "0123456789abcdef"
	sum := sha256.Sum256(payload)
	body, err := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": peopleAttachmentNotice, "content_text": "", "anchor": 0, "people_attachments_version": 1, "people_attachments": []peopleAttachmentDescriptor{{ID: fileID, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload)), Name: "synthetic.txt", MIME: "text/plain"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/1111111111111111", Author: subOwner, At: "2026-01-01T00:00:00.000Z", Body: body}}); err != nil {
		t.Fatal(err)
	}
	records, last, err := team.ReadCopy(dir, id, team.CommentsLog, 0, 1, 4<<20)
	if err != nil || len(records) != 1 || last != 1 || records[0].Seq != 1 || records[0].Author != subOwner {
		t.Fatalf("fixture setup: retained comment invalid: records=%d last=%d err=%v", len(records), last, err)
	}
	entry, _, ok := projectCommentRecord(records[0], "")
	if !ok {
		t.Fatal("fixture setup: retained comment did not project")
	}
	attachments, ok := entry["attachments"].([]map[string]any)
	if !ok || len(attachments) != 1 || attachments[0]["id"] != fileID || attachments[0]["sha256"] != hex.EncodeToString(sum[:]) || attachments[0]["size"] != int64(len(payload)) || attachments[0]["name"] != "synthetic.txt" || attachments[0]["mime"] != "text/plain" {
		t.Fatalf("fixture setup: invalid typed attachment descriptor: %#v", entry["attachments"])
	}
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments/1/" + fileID
	return h, dir, id, peopleTestURL(t, dir, path), gets
}

func capacitySmallGlobalCap(t *testing.T, dir string) {
	t.Helper()
	// Min-free stays configured and applies; zero isolates the global cap.
	// Kept conversation bytes must not consume this headroom.
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":1048576,"storage_min_free_bytes":0}`), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPeopleIncomingCannotConsumeSessionHistoryHeadroom(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 700<<10)
	h, dir, id, path, gets := capacityIncomingFixture(t, payload)
	capacitySmallGlobalCap(t, dir)
	response := doTeam(h, http.MethodGet, path, "")
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), payload) || gets.Load() != 1 {
		t.Fatalf("authorized attachment response status=%d bytes=%d fetches=%d", response.Code, response.Body.Len(), gets.Load())
	}
	assertPeopleDownloadKept(t, response, dir, id, "0123456789abcdef")
	sessionDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		t.Fatal(err)
	}
	writer := &sessionlog.Writer{Path: filepath.Join(sessionDir, "n1.jsonl")}
	event := sessionlog.Event{T: "user", Text: strings.Repeat("synthetic history ", 8192), Time: "2026-01-01T00:00:00Z"}
	if err := writer.Append(event); err != nil {
		t.Fatalf("incoming retention consumed space needed by a valid local session append: %v", err)
	}
}

func TestPeopleIncomingCannotStarveOutboundPeopleStage(t *testing.T) {
	incoming := bytes.Repeat([]byte("x"), 700<<10)
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	var gets, puts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			if r.URL.Query().Get("generation") != subGen {
				t.Error("fixture: missing generation")
			}
			switch r.Method {
			case http.MethodGet:
				gets.Add(1)
				_, _ = w.Write(incoming)
			case http.MethodPut:
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("fixture upload read: %v", err)
					return
				}
				puts.Add(1)
				sum := sha256.Sum256(data)
				w.WriteHeader(http.StatusCreated)
				_, _ = fmt.Fprintf(w, `{"id":"%s","size":%d}`, hex.EncodeToString(sum[:]), len(data))
			default:
				t.Errorf("fixture: unexpected blob method %s", r.Method)
			}
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	copyID := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	sum := sha256.Sum256(incoming)
	attachmentID := "0123456789abcdef"
	body, err := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": peopleAttachmentNotice, "content_text": "", "anchor": 0, "people_attachments_version": 1, "people_attachments": []peopleAttachmentDescriptor{{ID: attachmentID, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(incoming)), Name: "incoming.txt", MIME: "text/plain"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := team.AppendCopy(dir, copyID, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/1111111111111111", Author: subOwner, At: "2026-01-01T00:00:00.000Z", Body: body}}); err != nil {
		t.Fatal(err)
	}
	records, _, err := team.ReadCopy(dir, copyID, team.CommentsLog, 0, 1, 4<<20)
	if err != nil || len(records) != 1 || records[0].Author != subOwner {
		t.Fatalf("fixture: cannot read authentic comment: %v", err)
	}
	entry, _, valid := projectCommentRecord(records[0], "")
	attachments, typed := entry["attachments"].([]map[string]any)
	if !valid || !typed || len(attachments) != 1 || attachments[0]["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatal("fixture: incoming descriptor invalid")
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments"
	bound := peopleTestURL(t, dir, base)
	stage := func(id string, data []byte) *httptest.ResponseRecorder {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, err := form.CreateFormFile("files", "outgoing.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, bound+"&stage_id="+id, &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		result := httptest.NewRecorder()
		h.ServeHTTP(result, req)
		return result
	}
	capacitySmallGlobalCap(t, dir)
	controlID := "2222222222222222"
	control := stage(controlID, []byte("control"))
	if control.Code != http.StatusOK || puts.Load() != 1 {
		t.Fatalf("fixture: valid outbound control failed: status=%d body=%s puts=%d", control.Code, control.Body.String(), puts.Load())
	}
	var receipt struct {
		Attachments []struct{ ID, SHA256 string }
	}
	controlSum := sha256.Sum256([]byte("control"))
	if err := json.Unmarshal(control.Body.Bytes(), &receipt); err != nil || len(receipt.Attachments) != 1 || receipt.Attachments[0].ID != controlID || receipt.Attachments[0].SHA256 != hex.EncodeToString(controlSum[:]) {
		t.Fatalf("fixture: invalid successful stage receipt: %s", control.Body.String())
	}
	if _, err := team.ReadPeopleStage(dir, copyID, controlID); err != nil {
		t.Fatalf("fixture: successful stage missing: %v", err)
	}
	download := doTeam(h, http.MethodGet, peopleTestURL(t, dir, base+"/1/"+attachmentID), "")
	if download.Code != http.StatusOK || !bytes.Equal(download.Body.Bytes(), incoming) || gets.Load() != 1 {
		t.Fatalf("fixture: authorized incoming download failed: status=%d gets=%d", download.Code, gets.Load())
	}
	assertPeopleDownloadKept(t, download, dir, copyID, attachmentID)
	outgoing := bytes.Repeat([]byte("z"), 128<<10)
	second := stage("3333333333333333", outgoing)
	if second.Code != http.StatusOK || puts.Load() != 2 {
		t.Fatalf("incoming retention starved valid outgoing People stage: status=%d body=%s puts=%d", second.Code, second.Body.String(), puts.Load())
	}
}

func assertPeopleDownloadKept(t *testing.T, response *httptest.ResponseRecorder, dir string, id team.CopyID, attachmentID string) {
	t.Helper()
	if got := response.Header().Get("X-Scimux-People-Kept"); got != "" {
		t.Fatalf("kept download set X-Scimux-People-Kept: %q", got)
	}
	if _, err := team.ReadPeoplePublished(dir, id, 1, attachmentID); err != nil {
		t.Fatalf("download was not kept: %v", err)
	}
}

func fillPeopleRecordsCap(t *testing.T, dir string, id team.CopyID) {
	t.Helper()
	path := filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-records", "fill")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(128 << 20); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPeopleKeptCapFullStillServesVerifiedBytes(t *testing.T) {
	payload := []byte("synthetic verified bytes")
	h, dir, id, path, gets := capacityIncomingFixture(t, payload)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_min_free_bytes":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	fillPeopleRecordsCap(t, dir, id)
	response := doTeam(h, http.MethodGet, path, "")
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), payload) || gets.Load() != 1 {
		t.Fatalf("full kept cap did not serve verified bytes: status=%d bytes=%d fetches=%d body=%s", response.Code, response.Body.Len(), gets.Load(), response.Body.String())
	}
	if response.Header().Get("X-Scimux-People-Kept") != "false" {
		t.Fatalf("header = %q", response.Header().Get("X-Scimux-People-Kept"))
	}
	if _, err := team.ReadPeoplePublished(dir, id, 1, "0123456789abcdef"); !errors.Is(err, team.ErrPeopleStageMissing) {
		t.Fatalf("served bytes were kept: %v", err)
	}
}

func TestPeopleKeptMinFreeStillServesUnkept(t *testing.T) {
	payload := []byte("synthetic verified bytes")
	h, dir, id, path, gets := capacityIncomingFixture(t, payload)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_min_free_bytes":9223372036854775807,"storage_global_limit_bytes":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	response := doTeam(h, http.MethodGet, path, "")
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), payload) || gets.Load() != 1 {
		t.Fatalf("min-free refusal did not serve verified bytes: status=%d bytes=%d fetches=%d body=%s", response.Code, response.Body.Len(), gets.Load(), response.Body.String())
	}
	if response.Header().Get("X-Scimux-People-Kept") != "false" {
		t.Fatalf("header = %q", response.Header().Get("X-Scimux-People-Kept"))
	}
	if _, err := team.ReadPeoplePublished(dir, id, 1, "0123456789abcdef"); !errors.Is(err, team.ErrPeopleStageMissing) {
		t.Fatalf("min-free refusal kept the file: %v", err)
	}
}

func TestPeopleFetchNonBudgetSaveDoesNotServe(t *testing.T) {
	payload := []byte("synthetic verified bytes")
	h, dir, id, path, _ := capacityIncomingFixture(t, payload)
	records := filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-records")
	if err := os.MkdirAll(records, 0700); err != nil {
		t.Fatal(err)
	}
	// A non-regular file makes the kept-copy save fail before the bytes are stored.
	// The published file is still absent, so the fetch reaches SavePeoplePublished.
	if err := os.Symlink("synthetic", filepath.Join(records, "link")); err != nil {
		t.Fatal(err)
	}
	response := doTeam(h, http.MethodGet, path, "")
	if response.Code == http.StatusOK || response.Header().Get("X-Scimux-People-Kept") == "false" || bytes.Equal(response.Body.Bytes(), payload) {
		t.Fatalf("non-budget save served fetched bytes: status=%d header=%q", response.Code, response.Header().Get("X-Scimux-People-Kept"))
	}
	if _, err := team.ReadPeoplePublished(dir, id, 1, "0123456789abcdef"); !errors.Is(err, team.ErrPeopleStageMissing) {
		t.Fatalf("non-budget save kept the file: %v", err)
	}
}

func TestPeopleKeptObstacleDoesNotServeAsUnkept(t *testing.T) {
	payload := []byte("synthetic verified bytes")
	h, dir, id, path, gets := capacityIncomingFixture(t, payload)
	obstacle := filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-records")
	if err := os.MkdirAll(filepath.Dir(obstacle), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obstacle, []byte("synthetic storage obstacle"), 0600); err != nil {
		t.Fatal(err)
	}
	response := doTeam(h, http.MethodGet, path, "")
	if response.Code == http.StatusOK || response.Header().Get("X-Scimux-People-Kept") == "false" || bytes.Equal(response.Body.Bytes(), payload) {
		t.Fatalf("non-budget keep failure was served as unkept: status=%d header=%q fetches=%d", response.Code, response.Header().Get("X-Scimux-People-Kept"), gets.Load())
	}
}

func TestPeopleOwnerPublishFullKeptCapConfirms(t *testing.T) {
	data := []byte("synthetic retained bytes")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	puts, posts := 0, 0
	var stored json.RawMessage
	var author string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/") {
			puts++
			body, _ := io.ReadAll(r.Body)
			if !bytes.Equal(body, data) {
				t.Error("unexpected synthetic upload")
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":"%s","size":%d}`, hash, len(data))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodPost {
			posts++
			var input struct {
				Entries []struct {
					Key  string          `json:"key"`
					Body json.RawMessage `json:"body"`
				} `json:"entries"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || len(input.Entries) != 1 {
				t.Error("invalid synthetic append")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			stored = append([]byte(nil), input.Entries[0].Body...)
			fmt.Fprintf(w, `{"generation":"%s","results":[{"key":"%s","seq":1,"duplicate":false}],"last":1}`, subGen, input.Entries[0].Key)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"generation": subGen, "entries": []any{map[string]any{"seq": 1, "key": "c/fedcba9876543210", "author": author, "at": "2026-01-01T00:00:00.000Z", "body": stored}}, "last": 1, "more": false})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	server := labEntry(teamServerID, srv.URL, teamToken)
	seedTeam(t, dir, server)
	sub := baseSub()
	sub.Access = "write"
	seedSub(t, dir, sub)
	author = installationOf(t, dir)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hash, Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_min_free_bytes":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	fillPeopleRecordsCap(t, dir, id)
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), id, "")
	defer finish()
	rec := httptest.NewRecorder()
	h.postPeopleFileComment(rec, req, id, sub, server, "synthetic", 0, "fedcba9876543210", []string{stage.ID})
	if rec.Code != http.StatusOK || puts != 1 || posts != 1 {
		t.Fatalf("full kept cap failed an already published comment: status=%d body=%s puts=%d posts=%d", rec.Code, rec.Body.String(), puts, posts)
	}
	completion, err := team.ReadPeopleCompletion(dir, id, "fedcba9876543210")
	if err != nil || completion.Seq != 1 || !bytes.Equal(completion.Pending.Body, stored) {
		t.Fatalf("pending record was not confirmed: %v", err)
	}
	if _, err := team.ReadPeoplePending(dir, id, "fedcba9876543210"); !errors.Is(err, team.ErrPeopleStageMissing) {
		t.Fatalf("confirmed publication left its pending pin: %v", err)
	}
	if _, err := team.ReadPeoplePublished(dir, id, 1, stage.ID); !errors.Is(err, team.ErrPeopleStageMissing) {
		t.Fatalf("budget refusal kept the published file: %v", err)
	}
}
