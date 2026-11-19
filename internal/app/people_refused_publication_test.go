package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/team"
)

func TestPeoplePublicationRejectsInvalidOrForeignStageBeforeRelayWrite(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodPut || (r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments")) {
			writes++
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	entries, err := team.Load(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("server configuration unavailable: %v", err)
	}
	data := []byte("synthetic staged bytes")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(entries[0]), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	foreign := stage
	foreign.ID = "fedcba9876543210"
	foreign.ConfigHash = strings.Repeat("f", 64)
	if err := team.SavePeopleStage(dir, id, foreign); err != nil {
		t.Fatal(err)
	}
	otherOwner := stage
	otherOwner.ID = "2222222222222222"
	otherOwner.OwnerInstance = "synthetic-other-owner"
	if err := team.SavePeopleStage(dir, id, otherOwner); err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/comments"
	for _, tc := range []struct {
		name   string
		ids    []string
		status int
		code   string
	}{
		{"malformed", []string{"bad"}, http.StatusBadRequest, "bad_request"},
		{"missing", []string{"1111111111111111"}, http.StatusConflict, "stage_unavailable"},
		{"duplicate", []string{stage.ID, stage.ID}, http.StatusBadRequest, "bad_request"},
		{"foreign configuration", []string{foreign.ID}, http.StatusConflict, "stage_unavailable"},
		{"foreign native owner", []string{otherOwner.ID}, http.StatusConflict, "stage_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"text": "synthetic", "anchor": 0, "key": "0011223344556677", "attachments": tc.ids})
			if err != nil {
				t.Fatal(err)
			}
			rec := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path), string(body))
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if writes != 0 {
				t.Fatal("rejected manifest wrote to relay")
			}
		})
	}
	if got, err := team.ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, data) {
		t.Fatalf("rejection lost valid stage: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":1,"storage_min_free_bytes":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"text": "synthetic", "anchor": 0, "key": "1122334455667788", "attachments": []string{stage.ID}})
	if err != nil {
		t.Fatal(err)
	}
	rec := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path), string(body))
	if rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), "storage_full") || writes != 0 {
		t.Fatalf("local budget refusal status=%d relayWrites=%d", rec.Code, writes)
	}
	if _, err := team.ReadPeoplePending(dir, id, "1122334455667788"); err == nil {
		t.Fatal("budget refusal froze a publication")
	}
	if got, err := team.ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, data) {
		t.Fatalf("budget refusal lost staged bytes: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	prior := team.PeoplePending{Key: "aabbccddeeff0011", Author: strings.Repeat("a", 32), ConfigHash: stage.ConfigHash, Body: json.RawMessage(`{"v":1,"text":"earlier frozen publication"}`), StageIDs: []string{stage.ID}}
	if err := team.SavePeoplePending(dir, id, prior); err != nil {
		t.Fatal(err)
	}
	conflicting, err := json.Marshal(map[string]any{"text": "new synthetic caption", "anchor": 0, "key": prior.Key, "attachments": []string{stage.ID}})
	if err != nil {
		t.Fatal(err)
	}
	rec = doTeam(h, http.MethodPost, peopleTestURL(t, dir, path), string(conflicting))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "publication_conflict") || writes != 0 {
		t.Fatalf("frozen-key conflict status=%d relayWrites=%d body=%s", rec.Code, writes, rec.Body.String())
	}
	gotPending, err := team.ReadPeoplePending(dir, id, prior.Key)
	if err != nil || !bytes.Equal(gotPending.Body, prior.Body) || len(gotPending.StageIDs) != 1 || gotPending.StageIDs[0] != stage.ID {
		t.Fatalf("frozen publication changed: %v", err)
	}
	if err := team.DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("conflicting attempt released the original stage pin")
	}
}

func TestPeoplePublicationRejectsTotalOverLimitBeforeRelayWrite(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodPut || (r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments")) {
			writes++
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	entries, err := team.Load(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("server configuration unavailable: %v", err)
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		size := 4 << 20
		if i == 4 {
			size = 1
		}
		data := bytes.Repeat([]byte{'x'}, size)
		sum := sha256.Sum256(data)
		stage := team.PeopleStage{ID: fmt.Sprintf("%016x", i+1), Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(entries[0]), Data: data}
		if err := team.SavePeopleStage(dir, id, stage); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, stage.ID)
	}
	body, err := json.Marshal(map[string]any{"text": "synthetic", "anchor": 0, "key": "0011223344556677", "attachments": ids})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/comments"
	rec := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path), string(body))
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "too_large") || writes != 0 {
		t.Fatalf("oversized publication status=%d relayWrites=%d", rec.Code, writes)
	}
	for _, idValue := range ids {
		if _, err := team.ReadPeopleStage(dir, id, idValue); err != nil {
			t.Fatalf("rejection lost staged file: %v", err)
		}
	}
}

func TestPeoplePublicationCannotSucceedAfterFailedBlobUpload(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	payload := []byte("synthetic attachment")
	posts := 0
	var posted json.RawMessage
	var author string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"internal"}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			if r.Method == http.MethodPost {
				posts++
				var request struct {
					Entries []struct {
						Key  string          `json:"key"`
						Body json.RawMessage `json:"body"`
					} `json:"entries"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Entries) != 1 {
					t.Error("bad append")
					w.WriteHeader(400)
					return
				}
				posted = append([]byte(nil), request.Entries[0].Body...)
				_, _ = fmt.Fprintf(w, `{"generation":"%s","results":[{"key":"%s","seq":1,"duplicate":false}],"last":1}`, subGen, request.Entries[0].Key)
				return
			}
			reply := map[string]any{"generation": subGen, "entries": []any{map[string]any{"seq": 1, "key": "c/0123456789abcdef", "author": author, "at": "2026-11-14T09:30:00.000Z", "body": posted}}, "last": 1, "more": false}
			_ = json.NewEncoder(w).Encode(reply)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	author = installationOf(t, dir)
	readSubList(t, h)
	var fileBody bytes.Buffer
	form := multipart.NewWriter(&fileBody)
	part, err := form.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(payload)
	_ = form.Close()
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path+"/attachments"), &fileBody)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("stage status %d", rec.Code)
	}
	var staged struct {
		StageID string `json:"stage_id"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &staged) != nil || staged.StageID == "" {
		t.Fatal("unconfirmed upload did not preserve retry identity")
	}
	requestBody, _ := json.Marshal(map[string]any{"text": "", "anchor": 0, "key": "0123456789abcdef", "attachments": []string{staged.StageID}})
	comment := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path+"/comments"), string(requestBody))
	if comment.Code == http.StatusOK || posts != 0 {
		t.Fatalf("failed blob upload was reported as published: status=%d commentPosts=%d body=%s", comment.Code, posts, comment.Body.String())
	}
}

func TestPeoplePublicationUnconfirmedAppendKeepsFrozenRetry(t *testing.T) {
	data := []byte("synthetic staged bytes")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	puts, posts := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			_, _ = fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/") {
			puts++
			body, _ := io.ReadAll(r.Body)
			if !bytes.Equal(body, data) {
				t.Error("wrong synthetic upload")
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":"%s","size":%d}`, hash, len(data))
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			posts++
			http.Error(w, "synthetic unavailable", http.StatusServiceUnavailable)
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
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hash, Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), id, "")
		rec := httptest.NewRecorder()
		h.postPeopleFileComment(rec, req, id, sub, server, "synthetic caption", 0, "fedcba9876543210", []string{stage.ID})
		finish()
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "publication_unconfirmed") {
			t.Fatalf("attempt %d status=%d body=%s", attempt, rec.Code, rec.Body.String())
		}
		pending, err := team.ReadPeoplePending(dir, id, "fedcba9876543210")
		if err != nil || len(pending.StageIDs) != 1 || pending.StageIDs[0] != stage.ID || !bytes.Contains(pending.Body, []byte("synthetic caption")) {
			t.Fatalf("frozen retry changed: %v", err)
		}
		if err := team.DeletePeopleStage(dir, id, stage.ID); err == nil {
			t.Fatal("unconfirmed publication lost its pin")
		}
	}
	if puts != 2 || posts != 2 {
		t.Fatalf("explicit attempts: puts=%d posts=%d", puts, posts)
	}
}

func TestPeoplePublicationKeepsFrozenStateWhenSharingStopsBeforeUpload(t *testing.T) {
	var writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			writes.Add(1)
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
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	data := []byte("synthetic staged bytes")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var enabled, held atomic.Bool
	enabled.Store(true)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	h.peopleEnabled = func() bool {
		if _, err := team.ReadPeoplePending(dir, id, "fedcba9876543210"); err == nil && held.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return enabled.Load()
	}
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), id, "")
	defer finish()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.postPeopleFileComment(rec, req, id, sub, server, "synthetic caption", 0, "fedcba9876543210", []string{stage.ID})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("frozen publication not reached")
	}
	enabled.Store(false)
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not finish")
	}
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_active") || writes.Load() != 0 {
		t.Fatalf("disabled publication status=%d writes=%d body=%s", rec.Code, writes.Load(), rec.Body.String())
	}
	pending, err := team.ReadPeoplePending(dir, id, "fedcba9876543210")
	if err != nil || !bytes.Contains(pending.Body, []byte("synthetic caption")) || len(pending.StageIDs) != 1 || pending.StageIDs[0] != stage.ID {
		t.Fatalf("frozen state lost: %v", err)
	}
	if err := team.DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("disabled publication released its stage pin")
	}
}

func TestPeoplePublicationDoesNotFreezeAfterSharingStopsAtAdmission(t *testing.T) {
	var writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			writes.Add(1)
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
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	data := []byte("synthetic staged bytes")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var enabled atomic.Bool
	enabled.Store(true)
	var calls atomic.Int32
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	h.peopleEnabled = func() bool {
		if calls.Add(1) == 2 {
			before := enabled.Load()
			close(entered)
			<-release
			return before
		}
		return enabled.Load()
	}
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), id, "")
	defer finish()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.postPeopleFileComment(rec, req, id, sub, server, "synthetic caption", 0, "fedcba9876543210", []string{stage.ID})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("admission boundary not reached")
	}
	enabled.Store(false)
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not finish")
	}
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_active") || writes.Load() != 0 {
		t.Fatalf("disabled admission status=%d writes=%d body=%s", rec.Code, writes.Load(), rec.Body.String())
	}
	if _, err := team.ReadPeoplePending(dir, id, "fedcba9876543210"); err == nil {
		t.Fatal("disabled admission froze a publication")
	}
	got, err := team.ReadPeopleStage(dir, id, stage.ID)
	if err != nil || !bytes.Equal(got.Data, data) {
		t.Fatalf("stage changed: %v", err)
	}
}

func TestPeoplePublicationKeepsPendingWhenRetainedHistoryReadFails(t *testing.T) {
	data := []byte("synthetic retained bytes")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	puts, posts, reads := 0, 0, 0
	var stored json.RawMessage
	var author string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			_, _ = fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/") {
			puts++
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":"%s","size":%d}`, hash, len(data))
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
			_, _ = fmt.Fprintf(w, `{"generation":"%s","results":[{"key":"%s","seq":1,"duplicate":false}],"last":1}`, subGen, input.Entries[0].Key)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodGet {
			reads++
			if reads > 1 {
				http.Error(w, "synthetic history failure", http.StatusServiceUnavailable)
				return
			}
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
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), id, "")
	defer finish()
	rec := httptest.NewRecorder()
	h.postPeopleFileComment(rec, req, id, sub, server, "synthetic", 0, "fedcba9876543210", []string{stage.ID})
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "publication_unconfirmed") || puts != 1 || posts != 1 || reads != 2 {
		t.Fatalf("retention failure status=%d puts=%d posts=%d reads=%d", rec.Code, puts, posts, reads)
	}
	if _, err := team.ReadPeoplePending(dir, id, "fedcba9876543210"); err != nil {
		t.Fatalf("pending lost after retention failure: %v", err)
	}
	if err := team.DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("unconfirmed retention lost its pin")
	}
}

func TestPeoplePublicationRetriesAfterRetainedAttachmentStorageFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{{"people-records", http.StatusInsufficientStorage}, {"people-completed", http.StatusBadGateway}} {
		t.Run(tc.name, func(t *testing.T) { testPeoplePublicationStorageRetry(t, tc.name, tc.status) })
	}
}

func testPeoplePublicationStorageRetry(t *testing.T, obstacleName string, failureStatus int) {
	t.Helper()
	data := []byte("synthetic retained bytes")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	puts, posts, reads := 0, 0, 0
	var stored json.RawMessage
	var author string
	var obstacle string
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
			if posts == 1 {
				stored = append([]byte(nil), input.Entries[0].Body...)
			} else if !bytes.Equal(stored, input.Entries[0].Body) {
				t.Error("retry changed frozen body")
			}
			fmt.Fprintf(w, `{"generation":"%s","results":[{"key":"%s","seq":1,"duplicate":%t}],"last":1}`, subGen, input.Entries[0].Key, posts > 1)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodGet {
			reads++
			if obstacleName == "people-completed" && reads == 2 {
				if err := os.WriteFile(obstacle, []byte("synthetic storage obstacle"), 0600); err != nil {
					t.Error(err)
				}
			}
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
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	obstacle = filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, obstacleName)
	if obstacleName == "people-records" {
		if err := os.WriteFile(obstacle, []byte("synthetic storage obstacle"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	send := func() *httptest.ResponseRecorder {
		req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), id, "")
		defer finish()
		rec := httptest.NewRecorder()
		h.postPeopleFileComment(rec, req, id, sub, server, "synthetic", 0, "fedcba9876543210", []string{stage.ID})
		return rec
	}
	first := send()
	if first.Code != failureStatus || puts != 1 || posts != 1 || reads != 2 {
		t.Fatalf("storage failure status=%d puts=%d posts=%d reads=%d", first.Code, puts, posts, reads)
	}
	pending, err := team.ReadPeoplePending(dir, id, "fedcba9876543210")
	if err != nil || !bytes.Equal(pending.Body, stored) {
		t.Fatalf("frozen body unavailable: %v", err)
	}
	if err := team.DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("failed retention released stage pin")
	}
	if obstacleName == "people-completed" {
		retained, err := team.ReadPeoplePublished(dir, id, 1, stage.ID)
		if err != nil || !bytes.Equal(retained.Data, data) {
			t.Fatalf("receipt failure lost retained bytes: %v", err)
		}
	}
	if err := os.Remove(obstacle); err != nil {
		t.Fatal(err)
	}
	second := send()
	if second.Code != http.StatusOK || puts != 2 || posts != 2 {
		t.Fatalf("explicit retry status=%d puts=%d posts=%d body=%s", second.Code, puts, posts, second.Body.String())
	}
	retained, err := team.ReadPeoplePublished(dir, id, 1, stage.ID)
	if err != nil || !bytes.Equal(retained.Data, data) {
		t.Fatalf("retry did not retain exact bytes: %v", err)
	}
	completion, err := team.ReadPeopleCompletion(dir, id, "fedcba9876543210")
	if err != nil || completion.Seq != 1 || !bytes.Equal(completion.Pending.Body, stored) {
		t.Fatalf("retry completion mismatch: %v", err)
	}
}

func TestPeoplePublicationPreservesQuotaRefusalAndFrozenKey(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	payload := []byte("synthetic attachment")
	posts := 0
	var posted json.RawMessage
	var author string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusInsufficientStorage)
			_, _ = io.WriteString(w, `{"error":"quota"}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			if r.Method == http.MethodPost {
				posts++
				var request struct {
					Entries []struct {
						Key  string          `json:"key"`
						Body json.RawMessage `json:"body"`
					} `json:"entries"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Entries) != 1 {
					t.Error("bad append")
					w.WriteHeader(400)
					return
				}
				posted = append([]byte(nil), request.Entries[0].Body...)
				_, _ = fmt.Fprintf(w, `{"generation":"%s","results":[{"key":"%s","seq":1,"duplicate":false}],"last":1}`, subGen, request.Entries[0].Key)
				return
			}
			reply := map[string]any{"generation": subGen, "entries": []any{map[string]any{"seq": 1, "key": "c/0123456789abcdef", "author": author, "at": "2026-11-14T09:30:00.000Z", "body": posted}}, "last": 1, "more": false}
			_ = json.NewEncoder(w).Encode(reply)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	author = installationOf(t, dir)
	readSubList(t, h)
	var fileBody bytes.Buffer
	form := multipart.NewWriter(&fileBody)
	part, err := form.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(payload)
	_ = form.Close()
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path+"/attachments"), &fileBody)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("stage status %d", rec.Code)
	}
	var staged struct {
		StageID string `json:"stage_id"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &staged) != nil || staged.StageID == "" {
		t.Fatal("unconfirmed upload did not preserve retry identity")
	}
	requestBody, _ := json.Marshal(map[string]any{"text": "", "anchor": 0, "key": "0123456789abcdef", "attachments": []string{staged.StageID}})
	comment := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path+"/comments"), string(requestBody))
	if comment.Code != http.StatusInsufficientStorage || !strings.Contains(comment.Body.String(), "relay_quota") || posts != 0 {
		t.Fatalf("failed blob upload was reported as published: status=%d commentPosts=%d body=%s", comment.Code, posts, comment.Body.String())
	}
	retry := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path+"/comments"), string(requestBody))
	if retry.Code != http.StatusInsufficientStorage || posts != 0 {
		t.Fatalf("quota retry changed outcome: %d posts=%d", retry.Code, posts)
	}
}
