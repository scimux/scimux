package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// heldUploadBody pauses immediately before the multipart body's last byte,
// after the request captured its copy lifetime but before local admission.
type heldUploadBody struct {
	data    *bytes.Reader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *heldUploadBody) Read(p []byte) (int, error) {
	if r.data.Len() == 1 {
		r.once.Do(func() { close(r.entered); <-r.release })
	}
	if r.data.Len() > 1 && len(p) >= r.data.Len() {
		p = p[:r.data.Len()-1]
	}
	return r.data.Read(p)
}

func TestIndependentRemovedCopyCannotBeResurrectedByUpload(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("synthetic bytes")); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, base+"/attachments?stage_id=0123456789abcdef"), &heldUploadBody{data: bytes.NewReader(body.Bytes()), entered: entered, release: release})
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(rec, req) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("write admission was not reached")
	}
	removed := doTeam(h, http.MethodDelete, base, "")
	if removed.Code != http.StatusNoContent {
		t.Fatalf("remove copy status=%d body=%s", removed.Code, removed.Body.String())
	}
	copyPath := filepath.Join(dir, team.CopiesDir, teamServerID, subOwner, subChat, subGen)
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Fatalf("copy was not actually removed before completion: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not finish")
	}
	if rec.Code == http.StatusOK {
		t.Fatal("upload claimed success after removal")
	}
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Fatalf("late upload resurrected explicitly removed copy bytes: %v", err)
	}
}

func TestPeopleUploadRechecksSharingAtLocalAdmission(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
		}
		if r.Method == http.MethodPut {
			puts.Add(1)
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	handler, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, handler)
	h := handler.(*teamHandler)
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("synthetic bytes")); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, base+"/attachments?stage_id=0123456789abcdef"), &heldUploadBody{data: bytes.NewReader(body.Bytes()), entered: entered, release: release})
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); handler.ServeHTTP(rec, req) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("final admission not reached")
	}
	h.mu.Lock()
	subs, err := team.LoadSubs(dir)
	if err == nil {
		subs[0].State = "ended"
		err = team.SaveSubs(dir, subs)
	}
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	released.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not finish")
	}
	if rec.Code != http.StatusConflict || !bytes.Contains(rec.Body.Bytes(), []byte("not_active")) || puts.Load() != 0 {
		t.Fatalf("ended subscription admission status=%d PUTs=%d body=%s", rec.Code, puts.Load(), rec.Body.String())
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if _, err := team.ReadPeopleStage(dir, id, "0123456789abcdef"); err == nil {
		t.Fatal("ended subscription upload created a stage")
	}
}

func TestPeopleUploadPreservesStageWhenSharingStopsBeforeRelayPut(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
		}
		if r.Method == http.MethodPut {
			puts.Add(1)
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	handler, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, handler)
	h := handler.(*teamHandler)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	h.stageSave = func(dataDir string, copyID team.CopyID, stage team.PeopleStage) error {
		if err := team.SavePeopleStage(dataDir, copyID, stage); err != nil {
			return err
		}
		subs, err := team.LoadSubs(dataDir)
		if err != nil {
			return err
		}
		subs[0].State = "ended"
		return team.SaveSubs(dataDir, subs)
	}
	data := []byte("synthetic staged bytes")
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, base+"/attachments?stage_id=0123456789abcdef"), &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !bytes.Contains(rec.Body.Bytes(), []byte("not_active")) || puts.Load() != 0 {
		t.Fatalf("ended subscription precheck status=%d PUTs=%d body=%s", rec.Code, puts.Load(), rec.Body.String())
	}
	stage, err := team.ReadPeopleStage(dir, id, "0123456789abcdef")
	if err != nil || !bytes.Equal(stage.Data, data) {
		t.Fatalf("staged bytes were lost: %v", err)
	}
}

func TestPeopleStaleStageReadCannotExposeOrRestoreRecreatedCopy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/membership") {
			w.WriteHeader(http.StatusNoContent)
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
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	old, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic old stage")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "old-synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	h.stageRead = func(dir string, id team.CopyID, stageID string) (team.PeopleStage, error) {
		result, err := team.ReadPeopleStage(dir, id, stageID)
		close(entered)
		<-release
		return result, err
	}
	path := subReadPath() + "/attachments/stage/" + stage.ID + "?copy_instance=" + old
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, path, "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stage read did not reach hold")
	}
	if rec := doTeam(h, http.MethodDelete, subReadPath(), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("copy removal status=%d", rec.Code)
	}
	seedSub(t, dir, sub)
	current, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil || current == old {
		t.Fatalf("replacement identity: %v", err)
	}
	once.Do(func() { close(release) })
	select {
	case rec := <-done:
		if rec.Code != http.StatusConflict || strings.Contains(rec.Body.String(), stage.Name) {
			t.Fatalf("stale read status=%d body=%s", rec.Code, rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stage read did not finish")
	}
	got, err := team.ReadPeopleCopyInstance(dir, id)
	if err != nil || got != current {
		t.Fatalf("replacement identity changed: %v", err)
	}
	if _, err := team.ReadPeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("old stage restored in replacement")
	}
}

func TestPeopleOldStageRemovalCannotDeleteRecreatedCopyStage(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	srv := httptest.NewServer(script)
	t.Cleanup(srv.Close)
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	entries, subs, err := h.loadSubState()
	if err != nil || len(entries) != 1 || len(subs) != 1 {
		t.Fatalf("synthetic setup: entries=%d subscriptions=%d err=%v", len(entries), len(subs), err)
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	makeStage := func(content string) team.PeopleStage {
		data := []byte(content)
		sum := sha256.Sum256(data)
		return team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(entries[0]), Data: data}
	}
	if err := team.SavePeopleStage(dir, id, makeStage("old bytes")); err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	oldPath := peopleTestURL(t, dir, base+"/attachments/stage/0123456789abcdef") + "&revision=" + localStageRevision(makeStage("old bytes"))
	firstReq, firstFinish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodDelete, oldPath, nil), id, "0123456789abcdef")
	secondReq, secondFinish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodDelete, oldPath, nil), id, "0123456789abcdef")
	removed := doTeam(h, http.MethodDelete, base, "")
	if removed.Code != http.StatusNoContent {
		t.Fatalf("remove copy: %d %s", removed.Code, removed.Body.String())
	}
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	if err := team.SavePeopleStage(dir, id, makeStage("replacement bytes")); err != nil {
		t.Fatal(err)
	}
	rec, second := httptest.NewRecorder(), httptest.NewRecorder()
	h.removePeopleStage(rec, firstReq, id, "0123456789abcdef")
	h.removePeopleStage(second, secondReq, id, "0123456789abcdef")
	firstFinish()
	secondFinish()
	if rec.Code == http.StatusNoContent || second.Code == http.StatusNoContent {
		t.Fatalf("stale removals claimed success: first=%d second=%d", rec.Code, second.Code)
	}
	stage, err := team.ReadPeopleStage(dir, id, "0123456789abcdef")
	if err != nil || string(stage.Data) != "replacement bytes" {
		t.Fatalf("old removal affected replacement stage: status=%d err=%v", rec.Code, err)
	}
	h.mu.Lock()
	remaining := len(h.copyRemovalEpoch)
	h.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("completed removal retained %d lifecycle entries", remaining)
	}
}

func TestPeopleLocalStageRemovalSurvivesPermissionLoss(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	srv := httptest.NewServer(script)
	t.Cleanup(srv.Close)
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	entries, subs, err := h.loadSubState()
	if err != nil || len(entries) != 1 || len(subs) != 1 {
		t.Fatalf("synthetic setup: %v", err)
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	data := []byte("synthetic retained stage")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(entries[0]), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	neighbor := stage
	neighbor.ID = "fedcba9876543210"
	if err := team.SavePeopleStage(dir, id, neighbor); err != nil {
		t.Fatal(err)
	}
	instance, err := team.ReadPeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	revision := localStageRevision(stage)
	var relayCalls atomic.Int32
	var ownerChecks atomic.Int32
	h.ownerInstance = func(string, string) bool { ownerChecks.Add(1); return false }
	h.client.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) {
		relayCalls.Add(1)
		return nil, errors.New("unexpected network")
	})
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	rec := httptest.NewRecorder()
	app := newTestApp(t, &fakeTmux{})
	app.teamHandler = h
	proc := &stubProc{hasSession: true, live: "idle"}
	app.testProc = proc
	node := &Node{ID: "synthetic", Agent: "codex", Transport: "codex"}
	app.nodes = append(app.nodes, node)
	app.byID[node.ID] = node
	path := base + "/attachments/stage/" + stage.ID + "?copy_instance=" + instance + "&revision=" + revision
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodDelete, path, nil), id, stage.ID)
	defer finish()
	subs[0].Access = "read"
	h.mu.Lock()
	err = team.SaveSubs(dir, subs)
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.removePeopleStage(rec, req, id, stage.ID)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("local removal: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := team.ReadPeopleStage(dir, id, stage.ID); !errors.Is(err, team.ErrPeopleStageMissing) {
		t.Fatalf("stage remains: %v", err)
	}
	if got, err := team.ReadPeopleStage(dir, id, neighbor.ID); err != nil || !bytes.Equal(got.Data, neighbor.Data) {
		t.Fatalf("neighbor changed: %v", err)
	}
	if got, err := team.ReadPeopleCopyInstance(dir, id); err != nil || got != instance {
		t.Fatalf("copy changed: %v", err)
	}
	if relayCalls.Load() != 0 {
		t.Fatalf("unexpected relay calls: %d", relayCalls.Load())
	}
	if ownerChecks.Load() != 0 {
		t.Fatalf("unexpected owner checks: %d", ownerChecks.Load())
	}
	proc.mu.Lock()
	sends, prepares, deliveries := proc.sendCalls, proc.prepareCalls, proc.deliverCalls
	proc.mu.Unlock()
	if sends != 0 || prepares != 0 || deliveries != 0 || proc.shutdownCalls.Load() != 0 {
		t.Fatalf("unexpected agent operations: %d/%d/%d/%d", sends, prepares, deliveries, proc.shutdownCalls.Load())
	}
}

func TestPeopleOldStageRemovalCannotDeleteRecreatedStageInSameCopy(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	srv := httptest.NewServer(script)
	t.Cleanup(srv.Close)
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	entries, _, err := h.loadSubState()
	if err != nil || len(entries) != 1 {
		t.Fatalf("synthetic setup: %v", err)
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	makeStage := func(content string) team.PeopleStage {
		data := []byte(content)
		sum := sha256.Sum256(data)
		return team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(entries[0]), Data: data}
	}
	if err := team.SavePeopleStage(dir, id, makeStage("old bytes")); err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments/stage/0123456789abcdef"
	oldPath := peopleTestURL(t, dir, base) + "&revision=" + localStageRevision(makeStage("old bytes"))
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodDelete, oldPath, nil), id, "0123456789abcdef")
	defer finish()
	removed := doTeam(h, http.MethodDelete, oldPath, "")
	if removed.Code != http.StatusNoContent {
		t.Fatalf("current stage removal: %d %s", removed.Code, removed.Body.String())
	}
	if err := team.SavePeopleStage(dir, id, makeStage("replacement bytes")); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.removePeopleStage(rec, req, id, "0123456789abcdef")
	stage, err := team.ReadPeopleStage(dir, id, "0123456789abcdef")
	if err != nil || string(stage.Data) != "replacement bytes" || rec.Code == http.StatusNoContent {
		t.Fatalf("old removal affected replacement stage: status=%d err=%v", rec.Code, err)
	}
}

func TestPeopleRemovedRecreatedCopyCannotReceiveStaleUpload(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	_, priorSubs, setupErr := h.(*teamHandler).loadSubState()
	if setupErr != nil {
		t.Fatal(setupErr)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("synthetic bytes")); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, base+"/attachments?stage_id=0123456789abcdef"), &heldUploadBody{data: bytes.NewReader(body.Bytes()), entered: entered, release: release})
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(rec, req) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("write admission was not reached")
	}
	removed := doTeam(h, http.MethodDelete, base, "")
	if removed.Code != http.StatusNoContent {
		t.Fatalf("remove copy status=%d body=%s", removed.Code, removed.Body.String())
	}
	copyPath := filepath.Join(dir, team.CopiesDir, teamServerID, subOwner, subChat, subGen)
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Fatalf("copy was not actually removed before completion: %v", err)
	}
	if err := team.SaveSubs(dir, priorSubs); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not finish")
	}
	if rec.Code == http.StatusOK {
		t.Fatal("upload claimed success after removal")
	}
	if stage, err := team.ReadPeopleStage(dir, team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}, "0123456789abcdef"); err == nil {
		t.Fatalf("stale upload attached bytes to recreated copy: status=%d bytes=%q", rec.Code, string(stage.Data))
	}
}

func TestPeopleRemovedStageCannotBeResurrectedByOldUpload(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	entries, _, err := h.loadSubState()
	if err != nil || len(entries) != 1 {
		t.Fatalf("synthetic setup: %v", err)
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	data := []byte("synthetic bytes")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(entries[0]), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	_ = form.Close()
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, base+"/attachments?stage_id="+stage.ID), &heldUploadBody{data: bytes.NewReader(body.Bytes()), entered: entered, release: release})
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec, done := httptest.NewRecorder(), make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(rec, req) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old upload did not reach admission")
	}
	if removed := doTeam(h, http.MethodDelete, peopleTestURL(t, dir, base+"/attachments/stage/"+stage.ID)+"&revision="+localStageRevision(stage), ""); removed.Code != http.StatusNoContent {
		t.Fatalf("stage removal status %d", removed.Code)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old upload did not finish")
	}
	if _, err := team.ReadPeopleStage(dir, id, stage.ID); err == nil {
		t.Fatalf("old upload restored a removed stage: status %d", rec.Code)
	}
}
