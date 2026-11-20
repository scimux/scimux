package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	var calls atomic.Int32
	h.(*teamHandler).peopleEnabled = func() bool {
		if calls.Add(1) == 2 {
			close(entered)
			<-release
		}
		return true
	}
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
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, base+"/attachments?stage_id=0123456789abcdef"), &body)
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
	var calls atomic.Int32
	var enabled atomic.Bool
	enabled.Store(true)
	h.peopleEnabled = func() bool {
		if calls.Add(1) == 2 {
			before := enabled.Load()
			close(entered)
			<-release
			return before
		}
		return enabled.Load()
	}
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
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, base+"/attachments?stage_id=0123456789abcdef"), &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); handler.ServeHTTP(rec, req) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("final admission not reached")
	}
	enabled.Store(false)
	released.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not finish")
	}
	if rec.Code != http.StatusConflict || !bytes.Contains(rec.Body.Bytes(), []byte("not_active")) || puts.Load() != 0 {
		t.Fatalf("disabled admission status=%d PUTs=%d body=%s", rec.Code, puts.Load(), rec.Body.String())
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if _, err := team.ReadPeopleStage(dir, id, "0123456789abcdef"); err == nil {
		t.Fatal("disabled upload created a stage")
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
	entered, release := make(chan struct{}), make(chan struct{})
	var held, enabled atomic.Bool
	enabled.Store(true)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	h.peopleEnabled = func() bool {
		if _, err := team.ReadPeopleStage(dir, id, "0123456789abcdef"); err == nil && held.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return enabled.Load()
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
	done := make(chan struct{})
	go func() { defer close(done); handler.ServeHTTP(rec, req) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("staged upload did not reach relay precheck")
	}
	enabled.Store(false)
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not finish")
	}
	if rec.Code != http.StatusConflict || !bytes.Contains(rec.Body.Bytes(), []byte("not_active")) || puts.Load() != 0 {
		t.Fatalf("disabled precheck status=%d PUTs=%d body=%s", rec.Code, puts.Load(), rec.Body.String())
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
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var gateCalls atomic.Int32
	h.peopleEnabled = func() bool {
		if n := gateCalls.Add(1); n <= 2 {
			if n == 1 {
				close(entered)
			}
			<-release
		}
		return true
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	oldPath := peopleTestURL(t, dir, base+"/attachments/stage/0123456789abcdef") + "&revision=" + localStageRevision(makeStage("old bytes"))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, oldPath, nil))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old stage removal did not reach authorization")
	}
	second := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		h.ServeHTTP(second, httptest.NewRequest(http.MethodDelete, oldPath, nil))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		state := h.copyRemovalEpoch[id]
		ready := state != nil && state.active == 2
		h.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second old stage removal did not enter lifecycle")
		}
		time.Sleep(time.Millisecond)
	}
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
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old stage removal did not finish")
	}
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("second old stage removal did not finish")
	}
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

func TestPeopleStageRemovalRechecksPermission(t *testing.T) {
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
	if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic retained stage")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(entries[0]), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce atomic.Bool
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	h.peopleEnabled = func() bool {
		if enteredOnce.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return true
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, peopleTestURL(t, dir, base+"/attachments/stage/"+stage.ID), nil))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stage removal did not reach authorization")
	}
	subs[0].Access = "read"
	h.mu.Lock()
	err = team.SaveSubs(dir, subs)
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stage removal did not finish")
	}
	if rec.Code == http.StatusNoContent {
		t.Fatal("revoked write permission still allowed removal")
	}
	if _, err := team.ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("permission change lost stage: %v", err)
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
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	h.peopleEnabled = func() bool {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return true
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments/stage/0123456789abcdef"
	oldPath := peopleTestURL(t, dir, base) + "&revision=" + localStageRevision(makeStage("old bytes"))
	rec, done := httptest.NewRecorder(), make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, oldPath, nil))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old stage removal did not reach authorization")
	}
	removed := doTeam(h, http.MethodDelete, oldPath, "")
	if removed.Code != http.StatusNoContent {
		t.Fatalf("current stage removal: %d %s", removed.Code, removed.Body.String())
	}
	if err := team.SavePeopleStage(dir, id, makeStage("replacement bytes")); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old stage removal did not finish")
	}
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
	var calls atomic.Int32
	h.(*teamHandler).peopleEnabled = func() bool {
		if calls.Add(1) == 2 {
			close(entered)
			<-release
		}
		return true
	}
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
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, base+"/attachments?stage_id=0123456789abcdef"), &body)
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
	var calls atomic.Int32
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	h.peopleEnabled = func() bool {
		if calls.Add(1) == 2 {
			close(entered)
			<-release
		}
		return true
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	_ = form.Close()
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, base+"/attachments?stage_id="+stage.ID), &body)
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
