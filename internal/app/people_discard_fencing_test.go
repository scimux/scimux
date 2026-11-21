package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/team"
)

func TestDiscardFencesAdmittedPublication(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var puts, posts, unexpected atomic.Int32
	client := team.NewClient()
	client.Transport = teamRoundTrip(func(r *http.Request) (*http.Response, error) {
		header := make(http.Header)
		status := http.StatusOK
		body := ""
		switch {
		case r.URL.Path == "/v1/hello":
			header.Set("Scimux-Blob-Generation", "1")
			body = helloJSON(teamServerID, "Synthetic relay")
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/"):
			puts.Add(1)
			data, _ := io.ReadAll(r.Body)
			hash := sha256.Sum256(data)
			if puts.Load() == 1 {
				close(entered)
				<-release
			}
			status = http.StatusCreated
			body = fmt.Sprintf(`{"id":"%s","size":%d}`, hex.EncodeToString(hash[:]), len(data))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			posts.Add(1)
			status = http.StatusServiceUnavailable
			body = `{}`
		default:
			unexpected.Add(1)
			status = http.StatusNotFound
			body = `{}`
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	h, dir := testTeamSurface(t, client)
	server := labEntry(teamServerID, "http://127.0.0.1", teamToken)
	seedTeam(t, dir, server)
	sub := baseSub()
	sub.Access = "write"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic staged bytes")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "file.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	other := stage
	other.ID = "0011223344556677"
	for _, s := range []team.PeopleStage{stage, other} {
		if err := team.SavePeopleStage(dir, id, s); err != nil {
			t.Fatal(err)
		}
	}
	key := "fedcba9876543210"
	base := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation
	body, _ := json.Marshal(map[string]any{"text": "synthetic caption", "anchor": 0, "key": key, "attachments": []string{stage.ID, other.ID}})
	bad := doTeam(h, http.MethodPost, base+"/comments?copy_instance="+strings.Repeat("0", 32), string(body))
	if bad.Code != http.StatusConflict || puts.Load() != 0 || posts.Load() != 0 {
		t.Fatalf("stale copy publication: %d", bad.Code)
	}
	result := make(chan int, 1)
	go func() {
		result <- doTeam(h, http.MethodPost, base+"/comments?copy_instance="+instance, string(body)).Code
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not start upload")
	}
	pending, err := team.ReadPeoplePending(dir, id, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(pending)
	rev := sha256.Sum256(raw)
	discard := doTeam(h, http.MethodDelete, base+"/pending/"+key+"?copy_instance="+instance+"&revision="+hex.EncodeToString(rev[:]), "")
	if discard.Code != http.StatusOK {
		t.Fatalf("discard: %d %s", discard.Code, discard.Body.String())
	}
	once.Do(func() { close(release) })
	select {
	case status := <-result:
		if status == http.StatusOK {
			t.Fatal("discarded publication reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not finish")
	}
	if puts.Load() != 1 || posts.Load() != 0 || unexpected.Load() != 0 {
		t.Fatalf("writes after discard: uploads=%d appends=%d unexpected=%d", puts.Load(), posts.Load(), unexpected.Load())
	}
	if _, err := team.ReadPeopleCompletion(dir, id, key); err == nil {
		t.Fatal("discarded publication acquired completion")
	}
	retry := doTeam(h, http.MethodPost, base+"/comments?copy_instance="+instance, string(body))
	if retry.Code == http.StatusOK || puts.Load() != 1 || posts.Load() != 0 {
		t.Fatalf("retired retry wrote upstream: status=%d uploads=%d appends=%d", retry.Code, puts.Load(), posts.Load())
	}
	handler := h.(*teamHandler)
	handler.mu.Lock()
	activeCopies := len(handler.copyRemovalEpoch)
	handler.mu.Unlock()
	if activeCopies != 0 {
		t.Fatalf("completed operations retained %d copy entries", activeCopies)
	}
}

func TestDiscardFencesLateAcceptedAppend(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var uploads, appends, reads, accepted, unexpected atomic.Int32
	var remoteBody json.RawMessage
	var remoteAuthor string
	client := team.NewClient()
	client.Transport = teamRoundTrip(func(r *http.Request) (*http.Response, error) {
		header := make(http.Header)
		status := http.StatusOK
		body := ""
		switch {
		case r.URL.Path == "/v1/hello":
			header.Set("Scimux-Blob-Generation", "1")
			body = helloJSON(teamServerID, "Synthetic relay")
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/"):
			uploads.Add(1)
			data, _ := io.ReadAll(r.Body)
			sum := sha256.Sum256(data)
			status = http.StatusCreated
			body = fmt.Sprintf(`{"id":"%s","size":%d}`, hex.EncodeToString(sum[:]), len(data))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			appends.Add(1)
			var request struct {
				Entries []struct {
					Body json.RawMessage `json:"body"`
				} `json:"entries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Entries) != 1 {
				t.Error("invalid synthetic append")
				return nil, fmt.Errorf("invalid append")
			}
			remoteBody = request.Entries[0].Body
			close(entered)
			<-release
			accepted.Add(1)
			body = fmt.Sprintf(`{"generation":"%s","results":[{"key":"c/fedcba9876543210","seq":1,"duplicate":false}],"last":1}`, subGen)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			reads.Add(1)
			body = fmt.Sprintf(`{"generation":"%s","entries":[{"seq":1,"key":"c/fedcba9876543210","author":%q,"at":"2026-01-01T00:00:00.000Z","body":%s}],"last":1,"more":false}`, subGen, remoteAuthor, remoteBody)
		default:
			unexpected.Add(1)
			status = http.StatusNotFound
			body = `{}`
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	h, dir := testTeamSurface(t, client)
	remoteAuthor = installationOf(t, dir)
	server := labEntry(teamServerID, "http://127.0.0.1", teamToken)
	seedTeam(t, dir, server)
	sub := baseSub()
	sub.Access = "write"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic staged bytes")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "file.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	key := "fedcba9876543210"
	base := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation
	body, _ := json.Marshal(map[string]any{"text": "synthetic caption", "anchor": 0, "key": key, "attachments": []string{stage.ID}})
	result := make(chan int, 1)
	go func() {
		result <- doTeam(h, http.MethodPost, base+"/comments?copy_instance="+instance, string(body)).Code
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("append not dispatched")
	}
	pending, err := team.ReadPeoplePending(dir, id, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(pending)
	rev := sha256.Sum256(raw)
	discard := doTeam(h, http.MethodDelete, base+"/pending/"+key+"?copy_instance="+instance+"&revision="+hex.EncodeToString(rev[:]), "")
	if discard.Code != http.StatusOK {
		t.Fatalf("discard: %d %s", discard.Code, discard.Body.String())
	}
	once.Do(func() { close(release) })
	select {
	case status := <-result:
		if status == http.StatusOK {
			t.Fatal("late append reported completion")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not finish")
	}
	if uploads.Load() != 1 || appends.Load() != 1 || accepted.Load() != 1 || reads.Load() != 0 || unexpected.Load() != 0 {
		t.Fatalf("late work uploads=%d appends=%d accepted=%d reads=%d", uploads.Load(), appends.Load(), accepted.Load(), reads.Load())
	}
	if _, err := team.ReadPeopleCompletion(dir, id, key); err == nil {
		t.Fatal("late append created local receipt")
	}
	retry := doTeam(h, http.MethodPost, base+"/comments?copy_instance="+instance, string(body))
	if retry.Code == http.StatusOK || appends.Load() != 1 || uploads.Load() != 1 || reads.Load() != 0 {
		t.Fatal("late append retry dispatched work")
	}
	page, err := client.ReadComments(context.Background(), server.URL, server.Token, id.Owner, id.Chat, 0)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Key != "c/"+key || reads.Load() != 1 {
		t.Fatalf("ordinary poll did not observe late acceptance: %v", err)
	}
}

func TestDiscardLeavesOtherPublicationRunning(t *testing.T) {
	firstIn, secondIn, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var appends atomic.Int32
	var savedMu sync.Mutex
	var saved struct {
		Key  string          `json:"key"`
		Body json.RawMessage `json:"body"`
	}
	var author string
	client := team.NewClient()
	client.Transport = teamRoundTrip(func(r *http.Request) (*http.Response, error) {
		header := make(http.Header)
		status := http.StatusOK
		body := ""
		switch {
		case r.URL.Path == "/v1/hello":
			header.Set("Scimux-Blob-Generation", "1")
			body = helloJSON(teamServerID, "Synthetic relay")
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/"):
			data, _ := io.ReadAll(r.Body)
			sum := sha256.Sum256(data)
			if string(data) == "synthetic first" {
				close(firstIn)
			} else {
				close(secondIn)
			}
			<-release
			status = http.StatusCreated
			body = fmt.Sprintf(`{"id":"%s","size":%d}`, hex.EncodeToString(sum[:]), len(data))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			appends.Add(1)
			var payload struct {
				Entries []struct {
					Key  string          `json:"key"`
					Body json.RawMessage `json:"body"`
				} `json:"entries"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload.Entries) != 1 {
				t.Error("invalid synthetic append")
				status = http.StatusBadRequest
				body = `{}`
				break
			}
			savedMu.Lock()
			saved.Key = payload.Entries[0].Key
			saved.Body = payload.Entries[0].Body
			savedMu.Unlock()
			body = fmt.Sprintf(`{"generation":"%s","results":[{"key":%q,"seq":1,"duplicate":false}],"last":1}`, subGen, payload.Entries[0].Key)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			savedMu.Lock()
			body = fmt.Sprintf(`{"generation":"%s","entries":[{"seq":1,"key":%q,"author":%q,"at":"2026-01-01T00:00:00.000Z","body":%s}],"last":1,"more":false}`, subGen, saved.Key, author, saved.Body)
			savedMu.Unlock()
		default:
			status = http.StatusNotFound
			body = `{}`
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	h, dir := testTeamSurface(t, client)
	author = installationOf(t, dir)
	server := labEntry(teamServerID, "http://127.0.0.1", teamToken)
	seedTeam(t, dir, server)
	sub := baseSub()
	sub.Access = "write"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	stages := make([]team.PeopleStage, 2)
	for i, data := range [][]byte{[]byte("synthetic first"), []byte("synthetic second")} {
		sum := sha256.Sum256(data)
		stages[i] = team.PeopleStage{ID: []string{"0123456789abcdef", "0011223344556677"}[i], Hash: hex.EncodeToString(sum[:]), Name: "file.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
		if err := team.SavePeopleStage(dir, id, stages[i]); err != nil {
			t.Fatal(err)
		}
	}
	base := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation
	keys := []string{"fedcba9876543210", "1122334455667788"}
	results := make(chan int, 2)
	for i := range keys {
		body, _ := json.Marshal(map[string]any{"text": "synthetic caption", "anchor": 0, "key": keys[i], "attachments": []string{stages[i].ID}})
		go func() {
			results <- doTeam(h, http.MethodPost, base+"/comments?copy_instance="+instance, string(body)).Code
		}()
	}
	for _, ch := range []chan struct{}{firstIn, secondIn} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("publications did not both reach upload")
		}
	}
	pending, err := team.ReadPeoplePending(dir, id, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(pending)
	rev := sha256.Sum256(raw)
	discard := doTeam(h, http.MethodDelete, base+"/pending/"+keys[0]+"?copy_instance="+instance+"&revision="+hex.EncodeToString(rev[:]), "")
	if discard.Code != http.StatusOK {
		t.Fatalf("discard: %d %s", discard.Code, discard.Body.String())
	}
	once.Do(func() { close(release) })
	successes := 0
	for range keys {
		select {
		case status := <-results:
			if status == http.StatusOK {
				successes++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("publications did not finish")
		}
	}
	completion, err := team.ReadPeopleCompletion(dir, id, keys[1])
	if err != nil || completion.Seq != 1 || successes != 1 || appends.Load() != 1 {
		t.Fatalf("unrelated publication: successes=%d appends=%d receipt=%+v error=%v", successes, appends.Load(), completion, err)
	}
}

func TestLocalDiscardRouteRejectsUnsupportedShapes(t *testing.T) {
	h, _ := testTeamSurface(t, nil)
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/pending/0123456789abcdef"
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, base}, {http.MethodPost, base}, {http.MethodDelete, base + "/extra"},
		{http.MethodDelete, strings.Replace(base, teamServerID, "invalid", 1)},
		{http.MethodDelete, strings.TrimSuffix(base, "/0123456789abcdef") + "/invalid"},
	} {
		rec := doTeam(h, tc.method, tc.path, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
