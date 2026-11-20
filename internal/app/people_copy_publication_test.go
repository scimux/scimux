package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/team"
)

func TestPeoplePublicationCannotContinueAfterCopyRecreation(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	data := []byte("synthetic file")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	entered, release := make(chan struct{}), make(chan struct{})
	cancelled := make(chan struct{})
	var released sync.Once
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			_, _ = io.Copy(io.Discard, r.Body)
			close(entered)
			select {
			case <-r.Context().Done():
				close(cancelled)
				return
			case <-release:
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":"%s","size":%d}`, hash, len(data))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	entries, subs, err := h.(*teamHandler).loadSubState()
	if err != nil || len(entries) != 1 || len(subs) != 1 {
		t.Fatalf("synthetic setup: %v", err)
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hash, Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(entries[0]), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	body := `{"text":"","anchor":0,"key":"fedcba9876543210","attachments":["0123456789abcdef"]}`
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodPost, peopleTestURL(t, dir, base+"/comments"), body) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not reach held upload")
	}
	if removed := doTeam(h, http.MethodDelete, base, ""); removed.Code != http.StatusNoContent {
		t.Fatalf("copy removal status %d", removed.Code)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("copy removal did not cancel the old in-flight relay request")
	}
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	released.Do(func() { close(release) })
	select {
	case result := <-done:
		if result.Code == http.StatusOK {
			t.Fatal("old publication claimed success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not finish")
	}
	if posts.Load() != 0 {
		t.Fatalf("old publication posted %d comments after copy recreation", posts.Load())
	}
	if _, err := team.ReadPeoplePending(dir, id, "fedcba9876543210"); err == nil {
		t.Fatal("old pending publication remained in recreated copy")
	}
}

func TestPeoplePublicationStopsAfterAccessChangesDuringBlobUpload(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	data := []byte("synthetic file")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			_, _ = io.Copy(io.Discard, r.Body)
			close(entered)
			<-release
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":"%s","size":%d}`, hash, len(data))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	handler, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, handler)
	h := handler.(*teamHandler)
	entries, subs, err := h.loadSubState()
	if err != nil || len(entries) != 1 || len(subs) != 1 {
		t.Fatalf("synthetic setup: %v", err)
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hash, Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(entries[0]), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	body := `{"text":"","anchor":0,"key":"fedcba9876543210","attachments":["0123456789abcdef"]}`
	bound := peopleTestURL(t, dir, base+"/comments")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodPost, bound, body) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publication upload did not start")
	}
	h.mu.Lock()
	subs[0].Access = "read"
	err = team.SaveSubs(dir, subs)
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	released.Do(func() { close(release) })
	select {
	case result := <-done:
		if result.Code != http.StatusConflict || !strings.Contains(result.Body.String(), "not_active") {
			t.Fatalf("changed access status=%d body=%s", result.Code, result.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not finish")
	}
	if posts.Load() != 0 {
		t.Fatal("publication continued after access changed")
	}
	if _, err := team.ReadPeoplePending(dir, id, "fedcba9876543210"); err != nil {
		t.Fatalf("frozen retry lost: %v", err)
	}
	if err := team.DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("pending file lost its pin")
	}
}

func TestPeoplePublicationStopsAfterAccessChangesDuringAppendReconciliation(t *testing.T) {
	data := []byte("synthetic file")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	var puts, posts, reads atomic.Int32
	var stored json.RawMessage
	var author string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			_, _ = fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/") {
			puts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":"%s","size":%d}`, hash, len(data))
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			posts.Add(1)
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
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments") {
			reads.Add(1)
			close(entered)
			<-release
			_ = json.NewEncoder(w).Encode(map[string]any{"generation": subGen, "entries": []any{map[string]any{"seq": 1, "key": "c/fedcba9876543210", "author": author, "at": "2026-01-01T00:00:00.000Z", "body": stored}}, "last": 1, "more": false})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	server := labEntry(teamServerID, srv.URL, teamToken)
	seedTeam(t, dir, server)
	sub := baseSub()
	sub.Access = "write"
	seedSub(t, dir, sub)
	author = installationOf(t, dir)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hash, Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	body := `{"text":"","anchor":0,"key":"fedcba9876543210","attachments":["0123456789abcdef"]}`
	bound := peopleTestURL(t, dir, base+"/comments")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodPost, bound, body) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("append reconciliation did not start")
	}
	h.mu.Lock()
	subs, err := team.LoadSubs(dir)
	if err == nil && len(subs) == 1 {
		subs[0].Access = "read"
		err = team.SaveSubs(dir, subs)
	}
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	released.Do(func() { close(release) })
	select {
	case result := <-done:
		if result.Code != http.StatusConflict || !strings.Contains(result.Body.String(), "not_active") {
			t.Fatalf("changed access status=%d body=%s", result.Code, result.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not finish")
	}
	if puts.Load() != 1 || posts.Load() != 1 || reads.Load() != 1 {
		t.Fatalf("unexpected relay requests: puts=%d posts=%d reads=%d", puts.Load(), posts.Load(), reads.Load())
	}
	if _, err := team.ReadPeoplePending(dir, id, "fedcba9876543210"); err != nil {
		t.Fatalf("pending lost: %v", err)
	}
	if err := team.DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("pending file lost pin")
	}
}

func TestPeoplePublicationStopsWhenSharingEndsAfterHistoryRetention(t *testing.T) {
	for _, boundary := range []string{"history", "completion"} {
		t.Run(boundary, func(t *testing.T) { testPeoplePublicationSharingStopsAtBoundary(t, boundary) })
	}
}

func testPeoplePublicationSharingStopsAtBoundary(t *testing.T, boundary string) {
	t.Helper()
	data := []byte("synthetic file")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	var puts, posts atomic.Int32
	var stored json.RawMessage
	var author string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/") {
			puts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":"%s","size":%d}`, hash, len(data))
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			posts.Add(1)
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
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments") {
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
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hash, Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var held, enabled atomic.Bool
	enabled.Store(true)
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	h.peopleEnabled = func() bool {
		reached := false
		if boundary == "history" {
			_, last, err := team.ReadCopy(dir, id, team.CommentsLog, 0, 1, 4<<20)
			reached = err == nil && last == 1
		} else {
			_, err := team.ReadPeopleCompletion(dir, id, "fedcba9876543210")
			reached = err == nil
		}
		if reached && held.CompareAndSwap(false, true) {
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
		t.Fatal("retained history boundary not reached")
	}
	enabled.Store(false)
	released.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not finish")
	}
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_active") || puts.Load() != 1 || posts.Load() != 1 {
		t.Fatalf("status=%d puts=%d posts=%d body=%s", rec.Code, puts.Load(), posts.Load(), rec.Body.String())
	}
	if boundary == "history" {
		pending, err := team.ReadPeoplePending(dir, id, "fedcba9876543210")
		if err != nil || !bytes.Equal(pending.Body, stored) {
			t.Fatalf("frozen pending changed: %v", err)
		}
		if err := team.DeletePeopleStage(dir, id, stage.ID); err == nil {
			t.Fatal("stage pin was released")
		}
		if _, err := team.ReadPeoplePublished(dir, id, 1, stage.ID); err == nil {
			t.Fatal("disabled completion retained attachment")
		}
	} else {
		completed, err := team.ReadPeopleCompletion(dir, id, "fedcba9876543210")
		if err != nil || completed.Seq != 1 || !bytes.Equal(completed.Pending.Body, stored) {
			t.Fatalf("verified completion changed: %v", err)
		}
		retained, err := team.ReadPeoplePublished(dir, id, 1, stage.ID)
		if err != nil || !bytes.Equal(retained.Data, data) {
			t.Fatalf("verified bytes changed: %v", err)
		}
	}
}
