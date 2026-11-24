package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

func TestRetireAbsentFencesBeforeAdmission(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once, helloOnce sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var uploads, appends atomic.Int32
	client := team.NewClient()
	client.Transport = teamRoundTrip(func(r *http.Request) (*http.Response, error) {
		header := make(http.Header)
		status, body := http.StatusOK, `{}`
		switch {
		case r.URL.Path == "/v1/hello":
			helloOnce.Do(func() { close(entered); <-release })
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
			status = http.StatusServiceUnavailable
		default:
			status = http.StatusServiceUnavailable
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
	hash := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(hash[:]), Name: "file.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	key := "fedcba9876543210"
	base := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation
	body, _ := json.Marshal(map[string]any{"text": "synthetic caption", "anchor": 0, "key": key, "attachments": []string{stage.ID}})
	done := make(chan int, 1)
	go func() {
		done <- doTeam(h, http.MethodPost, base+"/comments?copy_instance="+instance, string(body)).Code
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach relay hello")
	}
	// A browser holds the request while no server pending record has been admitted.
	discard := doTeam(h, http.MethodPost, base+"/pending/"+key+"/retire-absent?copy_instance="+instance, "")
	if discard.Code != http.StatusOK {
		t.Errorf("cannot durably fence browser-only pending: %d %s", discard.Code, discard.Body.String())
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not finish")
	}
	if uploads.Load() != 0 {
		t.Errorf("delayed original uploaded after local discard attempt: %d", uploads.Load())
	}
	if appends.Load() != 0 {
		t.Errorf("delayed original appended after local discard: %d", appends.Load())
	}
	if _, err := team.ReadPeoplePending(dir, id, key); err == nil {
		t.Error("delayed original admitted a pending record")
	}
}

func TestRetireAbsentRequiresReviewOfExistingPending(t *testing.T) {
	h, dir := testTeamSurface(t, nil)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic file")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "file.txt", MIME: "text/plain", ConfigHash: strings.Repeat("a", 64), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := team.PeoplePending{Key: "fedcba9876543210", Author: installationOf(t, dir), ConfigHash: stage.ConfigHash, Body: json.RawMessage(`{"v":1}`), StageIDs: []string{stage.ID}}
	if err := team.SavePeoplePending(dir, id, pending, []team.PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/" + pending.Key + "/retire-absent?copy_instance=" + instance
	rec := doTeam(h, http.MethodPost, path, "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "review_required") {
		t.Errorf("existing pending must require review: %d %s", rec.Code, rec.Body.String())
	}
	got, err := team.ReadPeoplePending(dir, id, pending.Key)
	if err != nil || got.Key != pending.Key || string(got.Body) != string(pending.Body) {
		t.Fatalf("pending changed: %#v %v", got, err)
	}
	if err := team.DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Error("refused request released pending pin")
	}
}

func TestRetireAbsentIsLocalIdempotentAndCopyBound(t *testing.T) {
	var relay atomic.Int32
	client := team.NewClient()
	client.Transport = teamRoundTrip(func(r *http.Request) (*http.Response, error) {
		relay.Add(1)
		return nil, fmt.Errorf("unexpected relay call")
	})
	h, dir := testTeamSurface(t, client)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/fedcba9876543210/retire-absent?copy_instance="
	for _, suffix := range []string{strings.Repeat("0", 32), instance + "&revision=" + strings.Repeat("0", 64)} {
		rec := doTeam(h, http.MethodPost, base+suffix, "")
		if rec.Code == http.StatusOK {
			t.Fatalf("forged copy or query accepted: %s", suffix)
		}
	}
	for n := 0; n < 2; n++ {
		rec := doTeam(h, http.MethodPost, base+instance, "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"locally_discarded"`) {
			t.Errorf("retirement %d: %d %s", n, rec.Code, rec.Body.String())
		}
	}
	if relay.Load() != 0 {
		t.Errorf("local maintenance called relay %d times", relay.Load())
	}
}

type absentReadError struct{}

func (absentReadError) Read([]byte) (int, error) { return 0, fmt.Errorf("synthetic read error") }
func (absentReadError) Close() error             { return nil }

func TestRetireAbsentRejectsInvalidRequestsWithoutCancelling(t *testing.T) {
	h, dir := testTeamSurface(t, nil)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	handler := h.(*teamHandler)
	key := "fedcba9876543210"
	base := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/" + key + "/retire-absent?copy_instance=" + instance
	request, finish := handler.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/?copy_instance="+instance, nil), id, "")
	defer finish()
	if !handler.requirePeopleCopy(httptest.NewRecorder(), request, key) {
		t.Fatal("bind")
	}
	other, finishOther := handler.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/?copy_instance="+instance, nil), id, "")
	defer finishOther()
	if !handler.requirePeopleCopy(httptest.NewRecorder(), other, "0011223344556677") {
		t.Fatal("bind other key")
	}
	for _, tc := range []struct{ name, path, body string }{
		{"missing", strings.Replace(base, "?copy_instance="+instance, "", 1), ""},
		{"duplicate", base + "&copy_instance=" + instance, ""}, {"unknown", base + "&extra=x", ""}, {"badcopy", strings.Replace(base, instance, "bad", 1), ""},
		{"wrongcopy", strings.Replace(base, instance, strings.Repeat("0", 32), 1), ""}, {"badkey", strings.Replace(base, key, "bad", 1), ""}, {"body", base, "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doTeam(h, http.MethodPost, tc.path, tc.body)
			if rec.Code == http.StatusOK {
				t.Fatalf("accepted %s", tc.name)
			}
			if request.Context().Err() != nil {
				t.Fatal("refusal cancelled work")
			}
		})
	}
	bad := httptest.NewRequest(http.MethodPost, base, nil)
	bad.Body = absentReadError{}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bad)
	if rec.Code != http.StatusBadRequest || request.Context().Err() != nil {
		t.Fatalf("read failure: %d", rec.Code)
	}
	handler.peopleEnabled = func() bool { return false }
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, base, nil))
	handler.peopleEnabled = nil
	if rec.Code != http.StatusConflict || request.Context().Err() != nil {
		t.Fatalf("disabled: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, base, nil))
	if rec.Code != http.StatusOK || request.Context().Err() != context.Canceled {
		t.Fatalf("success did not cancel matching operation: %d %v", rec.Code, request.Context().Err())
	}
	if other.Context().Err() != nil {
		t.Fatal("retirement cancelled a different key")
	}
}

type heldCommentBody struct {
	entered chan struct{}
	release chan struct{}
	reader  *strings.Reader
	once    sync.Once
}

func (b *heldCommentBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.reader.Read(p)
}
func (b *heldCommentBody) Close() error { return nil }

func TestRetireAbsentFencesDelayedCommentBody(t *testing.T) {
	var uploads, appends atomic.Int32
	client := team.NewClient()
	client.Transport = teamRoundTrip(func(r *http.Request) (*http.Response, error) {
		header := make(http.Header)
		status := http.StatusOK
		body := `{}`
		switch {
		case r.URL.Path == "/v1/hello":
			header.Set("Scimux-Blob-Generation", "1")
			body = helloJSON(teamServerID, "Synthetic relay")
		case r.Method == http.MethodPut:
			uploads.Add(1)
			status = http.StatusServiceUnavailable
		case r.Method == http.MethodPost:
			appends.Add(1)
			status = http.StatusServiceUnavailable
		default:
			status = http.StatusNotFound
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
	data := []byte("synthetic file")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "file.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	key := "fedcba9876543210"
	base := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation
	body, _ := json.Marshal(map[string]any{"text": "synthetic", "anchor": 0, "key": key, "attachments": []string{stage.ID}})
	held := &heldCommentBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(string(body))}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(held.release) }) })
	req := httptest.NewRequest(http.MethodPost, base+"/comments?copy_instance="+instance, nil)
	req.Body = held
	done := make(chan int, 1)
	go func() { rec := httptest.NewRecorder(); h.ServeHTTP(rec, req); done <- rec.Code }()
	select {
	case <-held.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("body was not held")
	}
	retire := doTeam(h, http.MethodPost, base+"/pending/"+key+"/retire-absent?copy_instance="+instance, "")
	if retire.Code != http.StatusOK {
		t.Fatalf("retire: %d %s", retire.Code, retire.Body.String())
	}
	release.Do(func() { close(held.release) })
	select {
	case code := <-done:
		if code == http.StatusOK {
			t.Fatal("retired request succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request stuck")
	}
	if uploads.Load() != 0 || appends.Load() != 0 {
		t.Fatalf("writes after retirement: uploads=%d appends=%d", uploads.Load(), appends.Load())
	}
	if _, err := team.ReadPeoplePending(dir, id, key); err == nil {
		t.Fatal("pending admitted")
	}
}

func TestRetireAbsentWebSecurity(t *testing.T) {
	cmd, dir, _, _ := startTeamCommand(t, true)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	core, err := newCoreMux(cmd.application)
	if err != nil {
		t.Fatal(err)
	}
	web, err := newWebBackend(webBackendConfig{Web: webFS, Core: core, RequestPolicy: cmd.application.requestPolicy})
	if err != nil {
		t.Fatal(err)
	}
	path := "http://127.0.0.1/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/0123456789abcdef/retire-absent?copy_instance=" + instance
	for _, mode := range []string{"csrf", "host", "origin"} {
		t.Run(mode, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Host = "127.0.0.1"
			if mode != "csrf" {
				req.Header.Set("X-Scimux-CSRF", csrfToken)
			}
			if mode == "host" {
				req.Host = "untrusted.example"
			}
			if mode == "origin" {
				req.Header.Set("Origin", "http://untrusted.example")
			}
			rec := httptest.NewRecorder()
			web.local.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden && rec.Code != http.StatusBadRequest {
				t.Fatalf("boundary: %d", rec.Code)
			}
		})
	}
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Host = "127.0.0.1"
	req.Header.Set("X-Scimux-CSRF", csrfToken)
	rec := httptest.NewRecorder()
	web.local.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized request: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRetireAbsentReportsIdentityGateAndStorageFailures(t *testing.T) {
	for _, mode := range []string{"missing_identity", "second_gate", "cancelled", "full", "damaged_pending"} {
		t.Run(mode, func(t *testing.T) {
			h, dir := testTeamSurface(t, nil)
			handler := h.(*teamHandler)
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			instance, err := team.EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/0123456789abcdef/retire-absent?copy_instance=" + instance
			request := httptest.NewRequest(http.MethodPost, path, nil)
			switch mode {
			case "missing_identity":
				if err := os.Remove(filepath.Join(dir, identity.InstallationFile)); err != nil {
					t.Fatal(err)
				}
			case "second_gate":
				calls := 0
				handler.peopleEnabled = func() bool { calls++; return calls == 1 }
				defer func() { handler.peopleEnabled = nil }()
			case "cancelled":
				ctx, cancel := context.WithCancel(context.Background())
				calls := 0
				handler.peopleEnabled = func() bool {
					calls++
					if calls == 2 {
						cancel()
					}
					return true
				}
				defer func() { handler.peopleEnabled = nil }()
				request = request.WithContext(ctx)
			case "full":
				if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_min_free_bytes":0,"storage_global_limit_bytes":1}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "damaged_pending":
				p := filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-pending")
				if err := os.Mkdir(p, 0755); err != nil {
					t.Fatal(err)
				}
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, request)
			want := http.StatusConflict
			if mode == "full" {
				want = http.StatusInsufficientStorage
			}
			if rec.Code != want {
				t.Fatalf("%s: %d %s", mode, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRetireAbsentDoesNotDispatchAgentAndOwnerCanSend(t *testing.T) {
	var relay atomic.Int32
	client := team.NewClient()
	client.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) { relay.Add(1); return nil, fmt.Errorf("unexpected relay") })
	handler, dir := testTeamSurface(t, client)
	teamHandler := handler.(*teamHandler)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(t, &fakeTmux{})
	node := seedStructuredNode(t, app, "synthetic-owner", "pi", "acp")
	proc := &stubProc{live: "quiet", hasSession: true}
	app.testProc = proc
	app.teamHandler = teamHandler
	core, err := newCoreMux(app)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/0123456789abcdef/retire-absent?copy_instance=" + instance
	rec := httptest.NewRecorder()
	core.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("retirement: %d %s", rec.Code, rec.Body.String())
	}
	proc.mu.Lock()
	sends, prepares, deliveries := proc.sendCalls, proc.prepareCalls, proc.deliverCalls
	proc.mu.Unlock()
	if sends != 0 || prepares != 0 || deliveries != 0 || proc.shutdownCalls.Load() != 0 || relay.Load() != 0 {
		t.Fatalf("implicit actions: %d/%d/%d/%d relay=%d", sends, prepares, deliveries, proc.shutdownCalls.Load(), relay.Load())
	}
	send := httptest.NewRequest(http.MethodPost, "/api/nodes/"+node.ID+"/send", strings.NewReader(`{"text":"synthetic owner prompt"}`))
	send.SetPathValue("id", node.ID)
	result := httptest.NewRecorder()
	app.handleSend(result, send)
	if result.Code != http.StatusOK {
		t.Fatalf("explicit send: %d %s", result.Code, result.Body.String())
	}
	proc.mu.Lock()
	sends = proc.sendCalls
	proc.mu.Unlock()
	if sends != 1 || relay.Load() != 0 {
		t.Fatalf("explicit sends=%d relay=%d", sends, relay.Load())
	}
}

func TestRetireAbsentDoesNotCancelOldCopyOperation(t *testing.T) {
	h, dir := testTeamSurface(t, nil)
	handler := h.(*teamHandler)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	oldInstance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	key := "0123456789abcdef"
	old, finish := handler.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/?copy_instance="+oldInstance, nil), id, "")
	defer finish()
	if !handler.requirePeopleCopy(httptest.NewRecorder(), old, key) {
		t.Fatal("old operation bind")
	}
	if err := team.RemoveCopy(dir, id); err != nil {
		t.Fatal(err)
	}
	newInstance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil || newInstance == oldInstance {
		t.Fatalf("new identity: %v", err)
	}
	path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/" + key + "/retire-absent?copy_instance=" + newInstance
	for _, query := range []string{"bad=%ZZ", "other=x"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.URL.RawQuery = query
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || old.Context().Err() != nil {
			t.Fatalf("bad query: %d", rec.Code)
		}
	}
	rec := doTeam(h, http.MethodPost, path, "")
	if rec.Code != http.StatusOK || old.Context().Err() != nil {
		t.Fatalf("replacement retirement: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRetireAbsentFencesAtStageReadBeforeAdmission(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var uploads, appends atomic.Int32
	client := team.NewClient()
	client.Transport = teamRoundTrip(func(r *http.Request) (*http.Response, error) {
		header := make(http.Header)
		status, body := http.StatusOK, `{}`
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
			status = http.StatusServiceUnavailable
		default:
			status = http.StatusServiceUnavailable
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
	hash := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(hash[:]), Name: "file.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	h.(*teamHandler).stageRead = func(dataDir string, copyID team.CopyID, stageID string) (team.PeopleStage, error) {
		value, err := team.ReadPeopleStage(dataDir, copyID, stageID)
		close(entered)
		<-release
		return value, err
	}
	key := "fedcba9876543210"
	base := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation
	body, _ := json.Marshal(map[string]any{"text": "synthetic caption", "anchor": 0, "key": key, "attachments": []string{stage.ID}})
	done := make(chan int, 1)
	go func() {
		done <- doTeam(h, http.MethodPost, base+"/comments?copy_instance="+instance, string(body)).Code
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach stage read")
	}
	// A browser holds the request while no server pending record has been admitted.
	discard := doTeam(h, http.MethodPost, base+"/pending/"+key+"/retire-absent?copy_instance="+instance, "")
	if discard.Code != http.StatusOK {
		t.Errorf("cannot durably fence browser-only pending: %d %s", discard.Code, discard.Body.String())
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not finish")
	}
	if uploads.Load() != 0 {
		t.Errorf("delayed original uploaded after local discard attempt: %d", uploads.Load())
	}
	if appends.Load() != 0 {
		t.Errorf("delayed original appended after local discard: %d", appends.Load())
	}
	if _, err := team.ReadPeoplePending(dir, id, key); err == nil {
		t.Error("delayed original admitted a pending record")
	}
}

func TestRetireAbsentDoesNotRestorePublicationAuthority(t *testing.T) {
	for _, mode := range []string{"read_only", "lost_membership"} {
		t.Run(mode, func(t *testing.T) {
			var relay atomic.Int32
			client := team.NewClient()
			client.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) { relay.Add(1); return nil, fmt.Errorf("unexpected relay") })
			h, dir := testTeamSurface(t, client)
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			instance, err := team.EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "read_only" {
				joinLab(t, dir, id.Server, "http://127.0.0.1:1234")
				sub := baseSub()
				seedSub(t, dir, sub)
			}
			base := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation
			suffix := "?copy_instance=" + instance
			rec := doTeam(h, http.MethodPost, base+"/pending/0123456789abcdef/retire-absent"+suffix, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("local retirement: %d %s", rec.Code, rec.Body.String())
			}
			for _, request := range []struct{ path, body string }{{"/attachments", ""}, {"/comments", `{"text":"synthetic","anchor":0,"key":"fedcba9876543210"}`}} {
				rec := doTeam(h, http.MethodPost, base+request.path+suffix, request.body)
				if rec.Code == http.StatusOK || rec.Code == http.StatusCreated || rec.Code == http.StatusNoContent {
					t.Fatalf("publication accepted: %d", rec.Code)
				}
			}
			if relay.Load() != 0 {
				t.Fatalf("relay calls: %d", relay.Load())
			}
		})
	}
}
