package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/team"
)

func TestPeopleOldHistoryFetchCannotPopulateRecreatedCopy(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "read", 0))}
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodGet {
			close(entered)
			<-release
			_, _ = fmt.Fprintf(w, `{"generation":"%s","entries":[{"seq":1,"key":"c/1111111111111111","author":"%s","at":"2026-11-14T09:30:00.000Z","body":{"v":1,"t":"comment","text":"Synthetic old comment","anchor":0}}],"last":1,"more":false}`, subGen, subOwner)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	subs, err := team.LoadSubs(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, base+"/comments?after=0", "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old history request did not reach relay")
	}
	if removed := doTeam(h, http.MethodDelete, base, ""); removed.Code != http.StatusNoContent {
		t.Fatalf("copy removal status %d", removed.Code)
	}
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	released.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old history request did not finish")
	}
	records, _, err := team.ReadCopy(dir, id, team.CommentsLog, 0, 10, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatal("old history fetch populated the recreated copy")
	}
}

func TestPeopleRetiredHistoryReadDuringRelayHelloCannotAlterReplacement(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var armed, held atomic.Bool
	var releaseOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			if armed.Load() && held.CompareAndSwap(false, true) {
				close(entered)
				<-release
			}
			fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/membership") && r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	sub := baseSub()
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	old, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, subReadPath()+"/comments", "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("history hello not reached")
	}
	if rec := doTeam(h, http.MethodDelete, subReadPath(), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("copy removal status=%d", rec.Code)
	}
	seedSub(t, dir, sub)
	current, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil || current == old {
		t.Fatalf("replacement identity: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case rec := <-done:
		if rec.Code != http.StatusNotFound {
			t.Fatalf("retired history status=%d", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("history read did not finish")
	}
	got, err := team.ReadPeopleCopyInstance(dir, id)
	if err != nil || got != current {
		t.Fatalf("replacement identity changed: %v", err)
	}
	records, last, err := team.ReadCopy(dir, id, team.CommentsLog, 0, 1, 4<<20)
	if err != nil || last != 0 || len(records) != 0 {
		t.Fatalf("replacement history changed: last=%d err=%v", last, err)
	}
}

func TestPeopleDelayedRemovalCannotRemoveReplacementCopy(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var leaves atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			_, _ = fmt.Fprint(w, helloJSON(teamServerID, "Synthetic"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/membership") && r.Method == http.MethodDelete {
			if leaves.Add(1) == 1 {
				close(entered)
				<-release
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	sub := baseSub()
	sub.Access = "write"
	sub.State = "live"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	old, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodDelete, subReadPath(), "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old removal did not reach relay")
	}
	if rec := doTeam(h, http.MethodDelete, subReadPath(), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("fresh removal: %d", rec.Code)
	}
	if err := team.SaveSubs(dir, []team.Subscription{sub}); err != nil {
		t.Fatal(err)
	}
	current, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil || current == old {
		t.Fatalf("replacement identity: %v", err)
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old removal did not complete")
	}
	got, err := team.ReadPeopleCopyInstance(dir, id)
	if err != nil || got != current {
		t.Fatalf("old removal deleted replacement identity: %v", err)
	}
	subs, err := team.LoadSubs(dir)
	if err != nil || len(subs) != 1 {
		t.Fatalf("replacement subscription removed: %v", err)
	}
}

func TestPeopleRemovalPreservesCopyWhenServerStoreBecomesDamaged(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/membership") && r.Method == http.MethodDelete {
			close(entered)
			<-release
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	joinLab(t, dir, teamServerID, srv.URL)
	sub := baseSub()
	sub.Access = "write"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodDelete, subReadPath(), "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("relay removal not reached")
	}
	h.mu.Lock()
	err = os.WriteFile(filepath.Join(dir, team.File), []byte("synthetic damaged store"), 0600)
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case rec := <-done:
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "store_unavailable") {
			t.Fatalf("removal status=%d body=%s", rec.Code, rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("removal did not finish")
	}
	got, err := team.ReadPeopleCopyInstance(dir, id)
	if err != nil || got != instance {
		t.Fatalf("copy identity changed: %v", err)
	}
	subs, err := team.LoadSubs(dir)
	if err != nil || len(subs) != 1 || !reflect.DeepEqual(subs[0], sub) {
		t.Fatalf("subscription changed: %+v err=%v", subs, err)
	}
}

func TestPeopleRemovalReportsDamagedSubscriptionStoreAfterCopyDeletion(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	sub := baseSub()
	sub.State = "ended"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	originalRemove := removeSubscriptionCopy
	removeSubscriptionCopy = func(dataDir string, copyID team.CopyID) error {
		if err := originalRemove(dataDir, copyID); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dataDir, team.SubsFile), []byte("synthetic damaged store"), 0600)
	}
	t.Cleanup(func() { removeSubscriptionCopy = originalRemove })
	rec := doTeam(handler, http.MethodDelete, subReadPath(), "")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "store_unavailable") {
		t.Fatalf("removal status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := team.ReadPeopleCopyInstance(dir, id); err == nil {
		t.Fatal("failed removal restored the copy")
	}
	if _, err := team.LoadSubs(dir); err == nil {
		t.Fatal("damaged metadata was reported as valid")
	}
}

func TestPeopleOldCopyRemovalPreconditionCannotRemoveReplacement(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	sub := baseSub()
	sub.State = "ended"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	old, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec := doTeam(h, http.MethodDelete, subReadPath(), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("initial removal: %d", rec.Code)
	}
	seedSub(t, dir, sub)
	current, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil || current == old {
		t.Fatalf("replacement: %v", err)
	}
	rec := doTeam(h, http.MethodDelete, subReadPath()+"?copy_instance="+old, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale removal status: %d", rec.Code)
	}
	got, err := team.ReadPeopleCopyInstance(dir, id)
	if err != nil || got != current {
		t.Fatalf("replacement lost: %v", err)
	}
	if rec := doTeam(h, http.MethodDelete, subReadPath()+"?copy_instance="+current, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("current removal: %d", rec.Code)
	}
}

func TestPeopleHistoryCleanupDoesNotRemoveReplacementCopy(t *testing.T) {
	entered, release, cleanupEntered, cleanupRelease := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce, cleanupOnce sync.Once
	defer func() { releaseOnce.Do(func() { close(release) }); cleanupOnce.Do(func() { close(cleanupRelease) }) }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			_, _ = fmt.Fprint(w, helloJSON(teamServerID, "Synthetic"))
		case "/v1/channels/" + subOwner + "/" + subChat + "/comments":
			close(entered)
			<-release
			_, _ = fmt.Fprint(w, subPage(subGen, 1, false, subRecord(1, `{}`)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	joinLab(t, dir, teamServerID, srv.URL)
	seedSub(t, dir, baseSub())
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	originalRemove := removeSubscriptionCopy
	removeSubscriptionCopy = func(dataDir string, copyID team.CopyID) error {
		close(cleanupEntered)
		<-cleanupRelease
		return originalRemove(dataDir, copyID)
	}
	t.Cleanup(func() { removeSubscriptionCopy = originalRemove })
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, subReadPath()+"/comments", "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("history read did not reach the relay")
	}
	h.mu.Lock()
	err := team.SaveSubs(dir, nil)
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-cleanupEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("old history cleanup was not reached")
	}
	recreated := make(chan error, 1)
	go func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if err := team.SaveSubs(dir, []team.Subscription{baseSub()}); err != nil {
			recreated <- err
			return
		}
		data := []byte("synthetic replacement")
		sum := sha256.Sum256(data)
		recreated <- team.SavePeopleStage(dir, id, team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: strings.Repeat("a", 64), Data: data})
	}()
	select {
	case err := <-recreated:
		if err != nil {
			t.Fatal(err)
		}
		cleanupOnce.Do(func() { close(cleanupRelease) })
	case <-time.After(100 * time.Millisecond):
		cleanupOnce.Do(func() { close(cleanupRelease) })
		if err := <-recreated; err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("history cleanup did not finish")
	}
	stage, err := team.ReadPeopleStage(dir, id, "0123456789abcdef")
	if err != nil || string(stage.Data) != "synthetic replacement" {
		t.Fatalf("old history cleanup removed replacement: %v", err)
	}
}

func TestPeopleOldCopyCannotAdvanceReplacementReadPosition(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "read", 0))}
	h, dir, _ := subSurface(t, script)
	readSubList(t, h)
	subs, err := team.LoadSubs(dir)
	if err != nil || len(subs) != 1 {
		t.Fatalf("synthetic subscription: %v", err)
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	old, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	if result := doTeam(h, http.MethodDelete, base, ""); result.Code != http.StatusNoContent {
		t.Fatalf("remove status %d", result.Code)
	}
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/1111111111111111", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: []byte(`{"v":1,"t":"comment","text":"synthetic incoming","anchor":0}`)}}); err != nil {
		t.Fatal(err)
	}
	current, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil || current == old {
		t.Fatalf("replacement identity: %v", err)
	}
	if result := doTeam(h, http.MethodPut, base+"/seen?copy_instance="+old, `{"comments":1}`); result.Code == http.StatusNoContent {
		t.Fatal("old copy advanced replacement read position")
	}
	updated, err := team.LoadSubs(dir)
	if err != nil || updated[0].SeenComments != 0 {
		t.Fatalf("replacement read position changed: %v", err)
	}
	if result := doTeam(h, http.MethodPut, base+"/seen?copy_instance="+current, `{"comments":1}`); result.Code != http.StatusNoContent {
		t.Fatalf("current copy could not mark read: %d", result.Code)
	}
}

type heldLegacySeenBody struct {
	entered chan struct{}
	release chan struct{}
	body    *strings.Reader
	once    sync.Once
}

func (b *heldLegacySeenBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.body.Read(p)
}
func (b *heldLegacySeenBody) Close() error { return nil }

func TestPeopleAdmittedLegacySeenCannotAdvanceReplacement(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "read", 0))}
	h, dir, _ := subSurface(t, script)
	readSubList(t, h)
	subs, err := team.LoadSubs(dir)
	if err != nil || len(subs) != 1 {
		t.Fatalf("synthetic setup: %v", err)
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	old, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	body := &heldLegacySeenBody{entered: entered, release: release, body: strings.NewReader(`{"comments":1}`)}
	req := httptest.NewRequest(http.MethodPut, base+"/seen", body)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { rec := httptest.NewRecorder(); h.ServeHTTP(rec, req); done <- rec }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old seen body was not reached")
	}
	if rec := doTeam(h, http.MethodDelete, base, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove status %d", rec.Code)
	}
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/1111111111111111", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: []byte(`{"v":1,"t":"comment","text":"synthetic incoming","anchor":0}`)}}); err != nil {
		t.Fatal(err)
	}
	current, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil || current == old {
		t.Fatalf("replacement identity: %v", err)
	}
	released.Do(func() { close(release) })
	var status int
	select {
	case rec := <-done:
		status = rec.Code
	case <-time.After(5 * time.Second):
		t.Fatal("old seen request did not finish")
	}
	updated, err := team.LoadSubs(dir)
	if err != nil || len(updated) != 1 {
		t.Fatalf("replacement state: %v", err)
	}
	if updated[0].SeenComments != 0 {
		t.Fatalf("admitted legacy request advanced replacement read cursor to %d (status %d)", updated[0].SeenComments, status)
	}
	if rec := doTeam(h, http.MethodPut, base+"/seen", `{"comments":1}`); rec.Code != http.StatusNoContent {
		t.Fatalf("fresh legacy request must retain normal behavior: %d", rec.Code)
	}
}

func TestPeopleOldCredentialReplyCannotEndCurrentServerSubscriptions(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			close(entered)
			<-release
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	joinLab(t, dir, teamServerID, srv.URL)
	first := baseSub()
	second := first
	second.Owner = subOwnerB
	second.Chat = subChatB
	if err := team.SaveSubs(dir, []team.Subscription{first, second}); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, subReadPath()+"/comments", "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old credential read not reached")
	}
	h.mu.Lock()
	entries, err := team.Load(dir)
	if err == nil && len(entries) == 1 {
		next := entries[0]
		next.Token = strings.Repeat("d", 64)
		if next.Token == entries[0].Token {
			next.Token = strings.Repeat("e", 64)
		}
		err = team.Remove(dir, teamServerID)
		if err == nil {
			err = team.Add(dir, next)
		}
	}
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case rec := <-done:
		if rec.Code != http.StatusConflict {
			t.Fatalf("stale credential response status %d; retained copy must remain retryable", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old credential read did not finish")
	}
	subs, err := team.LoadSubs(dir)
	if err != nil || len(subs) != 2 {
		t.Fatalf("current subscriptions: %v", err)
	}
	for _, sub := range subs {
		if sub.State != "live" {
			t.Fatal("late response for earlier credentials ended current server subscriptions")
		}
	}
}

func TestPeopleOldCredentialWrongGenerationCannotEndCurrentCopy(t *testing.T) {
	for _, reply := range []string{"wrong generation", "missing channel"} {
		t.Run(reply, func(t *testing.T) { testPeopleOldCredentialRetirementReply(t, reply) })
	}
}

func testPeopleOldCredentialRetirementReply(t *testing.T, reply string) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			close(entered)
			<-release
			if reply == "missing channel" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, `{"generation":"%s","entries":[],"last":0,"more":false}`, strings.Repeat("e", 16))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	joinLab(t, dir, teamServerID, srv.URL)
	sub := baseSub()
	seedSub(t, dir, sub)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, subReadPath()+"/comments", "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old credential read not reached")
	}
	h.mu.Lock()
	entries, err := team.Load(dir)
	if err == nil && len(entries) == 1 {
		next := entries[0]
		next.Token = strings.Repeat("d", 64)
		if next.Token == entries[0].Token {
			next.Token = strings.Repeat("e", 64)
		}
		err = team.Remove(dir, teamServerID)
		if err == nil {
			err = team.Add(dir, next)
		}
	}
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case rec := <-done:
		if rec.Code != http.StatusConflict {
			t.Fatalf("stale generation response status=%d", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old credential read did not finish")
	}
	subs, err := team.LoadSubs(dir)
	if err != nil || len(subs) != 1 || subs[0].State != "live" {
		t.Fatalf("current copy changed: %+v err=%v", subs, err)
	}
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	_, last, err := team.ReadCopy(dir, id, team.CommentsLog, 0, 1, 4<<20)
	if err != nil || last != 0 {
		t.Fatalf("stale page wrote history: last=%d err=%v", last, err)
	}
}

func TestPeopleOldCredentialCommentFailureCannotEndCurrentSubscriptions(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusConflict} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/hello" {
					_, _ = fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
					return
				}
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
					close(entered)
					<-release
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			t.Cleanup(srv.Close)
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			handler, dir := testTeamSurface(t, clientFor(srv))
			h := handler.(*teamHandler)
			joinLab(t, dir, teamServerID, srv.URL)
			first := baseSub()
			first.Access = "write"
			second := first
			second.Owner, second.Chat = subOwnerB, subChatB
			if err := team.SaveSubs(dir, []team.Subscription{first, second}); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- doTeam(h, http.MethodPost, subReadPath()+"/comments", `{"text":"synthetic","anchor":0,"key":"0123456789abcdef"}`)
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("synthetic comment did not reach relay")
			}
			h.mu.Lock()
			entries, err := team.Load(dir)
			if err == nil && len(entries) == 1 {
				next := entries[0]
				next.Token = strings.Repeat("d", 64)
				if next.Token == entries[0].Token {
					next.Token = strings.Repeat("e", 64)
				}
				err = team.Remove(dir, teamServerID)
				if err == nil {
					err = team.Add(dir, next)
				}
			}
			h.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			once.Do(func() { close(release) })
			select {
			case rec := <-done:
				if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_active") {
					t.Fatalf("stale response status=%d body=%s", rec.Code, rec.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stale synthetic comment did not finish")
			}
			subs, err := team.LoadSubs(dir)
			if err != nil || len(subs) != 2 {
				t.Fatalf("subscriptions: %v", err)
			}
			for _, sub := range subs {
				if sub.State != "live" {
					t.Fatal("old credentials ended a current subscription")
				}
			}
		})
	}
}

func TestPeopleOldCredentialRemovalCannotDeleteCurrentCopy(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			_, _ = fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/membership") && r.Method == http.MethodDelete {
			close(entered)
			<-release
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	joinLab(t, dir, teamServerID, srv.URL)
	sub := baseSub()
	sub.Access = "write"
	sub.State = "live"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodDelete, subReadPath()+"?copy_instance="+instance, "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old removal not reached")
	}
	h.mu.Lock()
	entries, err := team.Load(dir)
	if err == nil && len(entries) == 1 {
		next := entries[0]
		next.Token = strings.Repeat("d", 64)
		err = team.Remove(dir, teamServerID)
		if err == nil {
			err = team.Add(dir, next)
		}
	}
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old removal did not finish")
	}
	got, err := team.ReadPeopleCopyInstance(dir, id)
	if err != nil || got != instance {
		t.Fatalf("old server response deleted current copy: %v", err)
	}
	subs, err := team.LoadSubs(dir)
	if err != nil || len(subs) != 1 || subs[0].State != "live" {
		t.Fatalf("current subscription lost: %v", err)
	}
}

func TestPeopleOldServerHistoryCannotPopulateCurrentCopy(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			_, _ = fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			close(entered)
			<-release
			_, _ = fmt.Fprint(w, subPage(subGen, 1, false, subRecord(1, `{"v":1,"t":"comment","text":"old synthetic","anchor":0}`)))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	joinLab(t, dir, teamServerID, srv.URL)
	seedSub(t, dir, baseSub())
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, subReadPath()+"/comments", "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old history not reached")
	}
	h.mu.Lock()
	entries, err := team.Load(dir)
	if err == nil && len(entries) == 1 {
		next := entries[0]
		next.Token = strings.Repeat("d", 64)
		err = team.Remove(dir, teamServerID)
		if err == nil {
			err = team.Add(dir, next)
		}
	}
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old history did not finish")
	}
	records, _, err := team.ReadCopy(dir, id, team.CommentsLog, 0, 10, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatal("old server response populated current copy")
	}
}

func TestPeopleHeldOfflineHistoryReportsDamagedCurrentSubscriptionStore(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			_, _ = fmt.Fprint(w, helloJSON(teamServerID, "Synthetic"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			close(entered)
			<-release
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	handler, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	seedSub(t, dir, baseSub())
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/1111111111111111", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: []byte(`{"v":1,"t":"comment","text":"saved synthetic","anchor":0}`)}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(handler, http.MethodGet, subReadPath()+"/comments", "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("history not reached")
	}
	if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("{damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case rec := <-done:
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "store_unavailable") || strings.Contains(rec.Body.String(), "saved synthetic") {
			t.Fatalf("damaged subscription store returned history: status=%d", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("history did not finish")
	}
	records, _, err := team.ReadCopy(dir, id, team.CommentsLog, 0, 256, 4<<20)
	if err != nil || len(records) != 1 {
		t.Fatalf("damaged subscription metadata changed retained history: %v", err)
	}
}

func TestPeopleReadPermissionChangeDoesNotReportRemovedCopy(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			_, _ = fmt.Fprint(w, helloJSON(teamServerID, "Synthetic"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			close(entered)
			<-release
			_, _ = fmt.Fprint(w, subPage(subGen, 1, false, subRecord(1, `{"v":1,"t":"comment","text":"synthetic","anchor":0}`)))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	joinLab(t, dir, teamServerID, srv.URL)
	sub := baseSub()
	sub.Access = "write"
	seedSub(t, dir, sub)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, subReadPath()+"/comments", "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("history not reached")
	}
	h.mu.Lock()
	sub.Access = "read"
	err := team.SaveSubs(dir, []team.Subscription{sub})
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case rec := <-done:
		if rec.Code != http.StatusConflict {
			t.Fatalf("stale read status %d; retained copy must remain retryable", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old read did not finish")
	}
}

func TestPeopleOldServerPollCannotRetireCurrentSubscriptions(t *testing.T) {
	for _, lane := range []string{"channels", "seats"} {
		t.Run(lane, func(t *testing.T) {
			handler, dir := testTeamSurface(t, team.NewClient())
			h := handler.(*teamHandler)
			h.subAsks = make(map[string]subAsk)
			joinLab(t, dir, teamServerID, "http://127.0.0.1:1")
			seedSub(t, dir, baseSub())
			entries, err := team.Load(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("setup: %v", err)
			}
			old := entries[0]
			next := old
			next.Token = strings.Repeat("d", 64)
			if err = team.Remove(dir, teamServerID); err != nil {
				t.Fatal(err)
			}
			if err = team.Add(dir, next); err != nil {
				t.Fatal(err)
			}
			var outcomes []subOutcome
			var seats []seatOutcome
			if lane == "channels" {
				outcomes = []subOutcome{{entry: old, delivery: "refused"}}
			} else {
				seats = []seatOutcome{{id: old.ID, entry: old, err: team.ErrUnauthorized}}
			}
			if _, err = h.commitSubs("", outcomes, seats, time.Now()); err != nil {
				t.Fatal(err)
			}
			subs, err := team.LoadSubs(dir)
			if err != nil || len(subs) != 1 || subs[0].State != "live" {
				t.Fatalf("old server poll retired current subscription: %v", err)
			}
		})
	}
}

func TestPeopleOldServerRosterCannotReplaceCurrentNames(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/seats" {
			close(entered)
			<-release
			_, _ = fmt.Fprint(w, seatsOf(subOwner, "Old synthetic name"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	h.subAsks = make(map[string]subAsk)
	joinLab(t, dir, teamServerID, srv.URL)
	entries, err := team.Load(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("setup: %v", err)
	}
	old := entries[0]
	done := make(chan struct{})
	go func() { h.askSeats(context.Background(), old); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("roster not reached")
	}
	h.mu.Lock()
	next := old
	next.Token = strings.Repeat("d", 64)
	err = team.Remove(dir, teamServerID)
	if err == nil {
		err = team.Add(dir, next)
	}
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("roster did not finish")
	}
	h.mu.Lock()
	name := h.subAsks[teamServerID].names[subOwner]
	h.mu.Unlock()
	if name != "" {
		t.Fatal("old server roster replaced current names")
	}
}

func TestPeopleOldCommentRosterCannotPersistNamesAfterConfigurationChange(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			_, _ = fmt.Fprint(w, helloJSON(teamServerID, "Synthetic"))
			return
		}
		if r.URL.Path == "/v1/seats" {
			close(entered)
			<-release
			_, _ = fmt.Fprint(w, seatsOf(subOwner, "Old synthetic name"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	joinLab(t, dir, teamServerID, srv.URL)
	seedSub(t, dir, baseSub())
	entries, err := team.Load(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("setup: %v", err)
	}
	old := entries[0]
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, subReadPath()+"/comments", nil), id, "")
	defer finish()
	done := make(chan map[string]string, 1)
	go func() {
		done <- h.commentNames(req.Context(), teamServerID, old, true, "", []map[string]any{{"author": subOwner}})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old roster not reached")
	}
	h.mu.Lock()
	next := old
	next.Token = strings.Repeat("d", 64)
	err = team.Remove(dir, teamServerID)
	if err == nil {
		err = team.Add(dir, next)
	}
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case names := <-done:
		if names[subOwner] != "" {
			t.Fatal("old name returned")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old roster did not finish")
	}
	people, err := team.LoadPeople(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, person := range people {
		if person.Server == teamServerID && person.Seat == subOwner {
			t.Fatal("old name persisted")
		}
	}
}

func TestPeopleCurrentConfigurationRefreshesEarlierRoster(t *testing.T) {
	var seats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			_, _ = fmt.Fprint(w, helloJSON(teamServerID, "Synthetic"))
			return
		}
		if r.URL.Path == "/v1/seats" {
			if seats.Add(1) == 1 {
				_, _ = fmt.Fprint(w, seatsOf(subOwner, "Earlier name"))
			} else {
				_, _ = fmt.Fprint(w, seatsOf(subOwner, "Current name"))
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	h.subAsks = make(map[string]subAsk)
	joinLab(t, dir, teamServerID, srv.URL)
	entries, err := team.Load(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("setup: %v", err)
	}
	old := entries[0]
	h.askSeats(context.Background(), old)
	next := old
	next.Token = strings.Repeat("d", 64)
	if err = team.Remove(dir, teamServerID); err != nil {
		t.Fatal(err)
	}
	if err = team.Add(dir, next); err != nil {
		t.Fatal(err)
	}
	names := h.commentNames(context.Background(), teamServerID, next, true, "", []map[string]any{{"author": subOwner}})
	if names[subOwner] != "Current name" || seats.Load() != 2 {
		t.Fatalf("current server did not refresh roster: calls=%d", seats.Load())
	}
}

func TestPeopleRetiredCopyCannotCommitReadOrRetirement(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	sub := baseSub()
	sub.State = "ended"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, subReadPath(), nil), id, "")
	defer finish()
	if rec := doTeam(h, http.MethodDelete, subReadPath(), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("removal: %d", rec.Code)
	}
	if current, err := h.endSubscription(req, id, team.Entry{}, true); current || err != nil {
		t.Fatalf("retired operation admitted: current=%v err=%v", current, err)
	}
	h.mu.Lock()
	current, err := h.subscriptionReadCurrentLocked(req, id, sub, team.Entry{})
	h.mu.Unlock()
	if current || err != nil {
		t.Fatalf("retired read admitted: current=%v err=%v", current, err)
	}
	rec := httptest.NewRecorder()
	h.retiredSubscriptionRead(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("retired read status=%d", rec.Code)
	}
}

func TestPeopleLegacyMirrorReadPreservesEndedHistoryWithoutFileCapability(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	h.peopleCapabilitiesReady = false
	sub := baseSub()
	sub.State = "ended"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	record := team.MirrorRecord{Seq: 1, Key: "1/1", Author: subOwner, At: "2026-01-01T00:00:00.000Z", Body: []byte(`{"v":1,"t":"user","text":"synthetic retained text"}`)}
	if err := team.AppendCopy(dir, id, team.MirrorLog, []team.MirrorRecord{record}); err != nil {
		t.Fatal(err)
	}
	rec := doTeam(h, http.MethodGet, subReadPath(), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "synthetic retained text") || strings.Contains(rec.Body.String(), "people_copy_instance") || strings.Contains(rec.Body.String(), instance) {
		t.Fatalf("legacy mirror response status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got, err := team.ReadPeopleCopyInstance(dir, id); err != nil || got != instance {
		t.Fatalf("copy identity changed: %v", err)
	}
}

func TestPeopleSubscriptionPageTransactionChecksCurrentAuthority(t *testing.T) {
	for _, mode := range []string{"current", "cancelled", "retired", "permission", "damaged", "invalid page"} {
		t.Run(mode, func(t *testing.T) {
			handler, dir := testTeamSurface(t, team.NewClient())
			h := handler.(*teamHandler)
			server := labEntry(teamServerID, "http://127.0.0.1:1", teamToken)
			seedTeam(t, dir, server)
			sub := baseSub()
			sub.Access = "write"
			seedSub(t, dir, sub)
			id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), id, "")
			defer finish()
			switch mode {
			case "cancelled":
				cancel()
			case "retired":
				h.mu.Lock()
				h.copyRemovalEpoch[id].epoch++
				h.mu.Unlock()
			case "permission":
				changed := sub
				changed.Access = "read"
				if err := team.SaveSubs(dir, []team.Subscription{changed}); err != nil {
					t.Fatal(err)
				}
			case "damaged":
				if err := os.WriteFile(filepath.Join(dir, team.File), []byte("synthetic damaged store"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			record := team.MirrorRecord{Seq: 1, Key: "c/0123456789abcdef", Author: subOwner, At: "2026-01-01T00:00:00.000Z", Body: []byte(`{"v":1,"t":"comment","text":"synthetic","anchor":0}`)}
			if mode == "invalid page" {
				record.Seq = 2
			}
			copyCurrent, current, currentErr, appendErr := h.appendSubscriptionPage(req, id, sub, server, team.CommentsLog, []team.MirrorRecord{record})
			switch mode {
			case "current":
				if !copyCurrent || !current || currentErr != nil || appendErr != nil {
					t.Fatalf("current transaction rejected: %v %v", currentErr, appendErr)
				}
			case "cancelled", "retired":
				if copyCurrent || current || currentErr != nil || appendErr != nil {
					t.Fatal("retired transaction admitted")
				}
			case "permission":
				if !copyCurrent || current || currentErr != nil || appendErr != nil {
					t.Fatal("changed permission admitted")
				}
			case "damaged":
				if !copyCurrent || current || currentErr == nil || appendErr != nil {
					t.Fatal("damaged metadata admitted")
				}
			case "invalid page":
				if !copyCurrent || !current || currentErr != nil || appendErr == nil {
					t.Fatal("invalid page admitted")
				}
			}
			if mode == "cancelled" || mode == "retired" || mode == "permission" {
				rec := httptest.NewRecorder()
				writeRetiredSubscriptionPage(rec, copyCurrent)
				if mode == "permission" {
					if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_active") {
						t.Fatalf("changed permission status=%d body=%s", rec.Code, rec.Body.String())
					}
				} else if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not_found") {
					t.Fatalf("retired status=%d body=%s", rec.Code, rec.Body.String())
				}
			}
			records, last, err := team.ReadCopy(dir, id, team.CommentsLog, 0, 2, 4<<20)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "current" {
				if last != 1 || len(records) != 1 || !bytes.Equal(records[0].Body, record.Body) {
					t.Fatal("current page not retained exactly")
				}
			} else if last != 0 || len(records) != 0 {
				t.Fatal("rejected transaction wrote history")
			}
		})
	}
}

func TestPeopleMissingSubscriptionCleanupRechecksBeforeDeletion(t *testing.T) {
	for _, mode := range []string{"missing", "reappeared", "unrelated", "damaged", "cancelled", "retired"} {
		t.Run(mode, func(t *testing.T) {
			handler, dir := testTeamSurface(t, team.NewClient())
			h := handler.(*teamHandler)
			sub := baseSub()
			id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
			instance, err := team.EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			var subs []team.Subscription
			var otherID team.CopyID
			var otherInstance string
			if mode == "reappeared" {
				subs = []team.Subscription{sub}
			}
			if mode == "unrelated" {
				other := sub
				other.Owner, other.Chat = subOwnerB, subChatB
				subs = []team.Subscription{other}
				otherID = team.CopyID{Server: other.Server, Owner: other.Owner, Chat: other.Chat, Generation: other.Generation}
				otherInstance, err = team.EnsurePeopleCopyInstance(dir, otherID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := team.SaveSubs(dir, subs); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), id, "")
			defer finish()
			if mode == "damaged" {
				if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("synthetic damaged store"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "cancelled" {
				cancel()
			}
			if mode == "retired" {
				h.mu.Lock()
				h.copyRemovalEpoch[id].epoch++
				h.mu.Unlock()
			}
			h.cleanupMissingSubscription(req, id)
			got, err := team.ReadPeopleCopyInstance(dir, id)
			if mode == "missing" || mode == "unrelated" {
				if err == nil {
					t.Fatal("missing copy was not removed")
				}
			} else if err != nil || got != instance {
				t.Fatalf("current copy was removed: %v", err)
			}
			if mode == "unrelated" {
				gotSubs, err := team.LoadSubs(dir)
				if err != nil || len(gotSubs) != 1 || !reflect.DeepEqual(gotSubs[0], subs[0]) {
					t.Fatalf("unrelated copy changed: %v", err)
				}
				got, err := team.ReadPeopleCopyInstance(dir, otherID)
				if err != nil || got != otherInstance {
					t.Fatalf("unrelated copy identity changed: %v", err)
				}
			}
		})
	}
}

func TestPeopleCurrentRetirementIsIdempotent(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	server := labEntry(teamServerID, "http://127.0.0.1:1", teamToken)
	seedTeam(t, dir, server)
	sub := baseSub()
	sub.State = "live"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, subReadPath(), nil), id, "")
	defer finish()
	for attempt := 0; attempt < 2; attempt++ {
		current, err := h.endSubscription(req, id, server, false)
		if !current || err != nil {
			t.Fatalf("attempt %d: current=%v err=%v", attempt, current, err)
		}
		subs, err := team.LoadSubs(dir)
		if err != nil || len(subs) != 1 || subs[0].State != "ended" {
			t.Fatalf("attempt %d did not retain the ended copy: %+v err=%v", attempt, subs, err)
		}
	}
}

func TestPeopleSingleCopyRetirementPreservesAnotherSubscription(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	server := labEntry(teamServerID, "http://127.0.0.1:1", teamToken)
	seedTeam(t, dir, server)
	first := baseSub()
	second := first
	second.Owner, second.Chat = subOwnerB, subChatB
	if err := team.SaveSubs(dir, []team.Subscription{second, first}); err != nil {
		t.Fatal(err)
	}
	id := team.CopyID{Server: first.Server, Owner: first.Owner, Chat: first.Chat, Generation: first.Generation}
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, subReadPath(), nil), id, "")
	defer finish()
	current, err := h.endSubscription(req, id, server, false)
	if !current || err != nil {
		t.Fatalf("retirement: current=%v err=%v", current, err)
	}
	subs, err := team.LoadSubs(dir)
	if err != nil || len(subs) != 2 {
		t.Fatalf("subscriptions: %+v err=%v", subs, err)
	}
	byID := make(map[team.CopyID]team.Subscription, len(subs))
	for _, sub := range subs {
		key := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
		if _, exists := byID[key]; exists {
			t.Fatal("duplicate copy identity")
		}
		byID[key] = sub
	}
	otherID := team.CopyID{Server: second.Server, Owner: second.Owner, Chat: second.Chat, Generation: second.Generation}
	first.State = "ended"
	gotFirst, firstFound := byID[id]
	gotSecond, secondFound := byID[otherID]
	if !firstFound || !secondFound || !reflect.DeepEqual(gotFirst, first) || !reflect.DeepEqual(gotSecond, second) {
		t.Fatalf("retirement changed the wrong copies: %+v", subs)
	}
}

func TestPeopleRetirementRequiresCurrentServerAndCopy(t *testing.T) {
	for _, missing := range []string{"server", "other server", "copy"} {
		t.Run(missing, func(t *testing.T) {
			handler, dir := testTeamSurface(t, team.NewClient())
			h := handler.(*teamHandler)
			server := labEntry(teamServerID, "http://127.0.0.1:1", teamToken)
			if missing == "other server" {
				seedTeam(t, dir, labEntry(strings.Repeat("f", 32), "http://127.0.0.1:2", teamToken))
			} else if missing != "server" {
				seedTeam(t, dir, server)
			}
			sub := baseSub()
			if missing != "copy" {
				seedSub(t, dir, sub)
			}
			id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
			req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, subReadPath(), nil), id, "")
			defer finish()
			current, err := h.endSubscription(req, id, server, true)
			if current || err != nil {
				t.Fatalf("retirement with missing %s: current=%v err=%v", missing, current, err)
			}
			subs, err := team.LoadSubs(dir)
			if err != nil {
				t.Fatal(err)
			}
			if missing != "copy" && (len(subs) != 1 || subs[0].State != sub.State) {
				t.Fatal("missing server changed the copy")
			}
			if missing == "copy" && len(subs) != 0 {
				t.Fatal("missing copy was recreated")
			}
		})
	}
}

func TestPeopleRemovalPreservesOtherConfiguredServer(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	other := labEntry(strings.Repeat("f", 32), "http://127.0.0.1:2", teamToken)
	current := labEntry(teamServerID, "http://127.0.0.1:1", teamToken)
	seedTeam(t, dir, other)
	seedTeam(t, dir, current)
	sub := baseSub()
	sub.State = "ended"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	rec := doTeam(handler, http.MethodDelete, subReadPath(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("removal status=%d body=%s", rec.Code, rec.Body.String())
	}
	entries, err := team.Load(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("configured servers changed: %v", err)
	}
	seen := map[string]team.Entry{}
	for _, entry := range entries {
		seen[entry.ID] = entry
	}
	if len(seen) != 2 || !reflect.DeepEqual(seen[other.ID], other) || !reflect.DeepEqual(seen[current.ID], current) {
		t.Fatal("configured server changed during copy removal")
	}
	if subs, err := team.LoadSubs(dir); err != nil || len(subs) != 0 {
		t.Fatalf("copy remains after removal: %+v err=%v", subs, err)
	}
	if _, err := team.ReadPeopleCopyInstance(dir, id); err == nil {
		t.Fatal("removed copy identity remains")
	}
}

func TestPeopleRosterRejectsDamagedConfigurationAndRetiredCopy(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	h.subAsks = make(map[string]subAsk)
	joinLab(t, dir, teamServerID, "http://127.0.0.1:1")
	entries, err := team.Load(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("setup: %v", err)
	}
	server := entries[0]
	if err := os.WriteFile(filepath.Join(dir, team.File), []byte("{damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	current := h.serverCurrentLocked(server)
	h.mu.Unlock()
	if current {
		t.Fatal("damaged server configuration was accepted")
	}
	h.storeRosterCurrent(context.Background(), server, []team.Seat{{ID: subOwner, Name: "synthetic"}})
	if len(h.subAsks[teamServerID].names) != 0 {
		t.Fatal("damaged configuration updated roster")
	}
	if err := os.Remove(filepath.Join(dir, team.File)); err != nil {
		t.Fatal(err)
	}
	joinLab(t, dir, teamServerID, "http://127.0.0.1:1")
	sub := baseSub()
	sub.State = "ended"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, subReadPath(), nil), id, "")
	defer finish()
	if rec := doTeam(h, http.MethodDelete, subReadPath(), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("removal: %d", rec.Code)
	}
	h.storeRosterCurrent(req.Context(), server, []team.Seat{{ID: subOwner, Name: "synthetic"}})
	if len(h.subAsks[teamServerID].names) != 0 {
		t.Fatal("retired copy updated roster")
	}
}

func TestPeopleRosterCannotAdoptRetiredOperationWithoutContextCancellation(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	joinLab(t, dir, teamServerID, "http://127.0.0.1:1")
	servers, err := team.Load(dir)
	if err != nil || len(servers) != 1 {
		t.Fatalf("synthetic server: %v", err)
	}
	server := servers[0]
	sub := baseSub()
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	h.subAsks = make(map[string]subAsk)
	h.storeRosterCurrent(context.Background(), server, []team.Seat{{ID: subOwner, Name: "current synthetic name"}})
	if h.subAsks[teamServerID].names[subOwner] != "current synthetic name" {
		t.Fatal("current roster was not stored")
	}
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, "/", nil), id, "")
	defer finish()
	h.mu.Lock()
	h.copyRemovalEpoch[id].epoch++
	h.mu.Unlock()
	if req.Context().Err() != nil {
		t.Fatal("test operation was cancelled instead of retired")
	}
	h.storeRosterCurrent(req.Context(), server, []team.Seat{{ID: subOwner, Name: "stale synthetic name"}})
	if h.subAsks[teamServerID].names[subOwner] != "current synthetic name" {
		t.Fatal("retired operation changed the roster")
	}
	if names := h.commentNames(req.Context(), teamServerID, server, false, "", []map[string]any{{"author": subOwner}}); len(names) != 0 {
		t.Fatal("retired operation returned current roster names")
	}
}

func TestPeopleRosterAndCommentNamesRespectCancellation(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	joinLab(t, dir, teamServerID, "http://127.0.0.1:1")
	servers, err := team.Load(dir)
	if err != nil || len(servers) != 1 {
		t.Fatalf("setup: %v", err)
	}
	server := servers[0]
	h.subAsks = map[string]subAsk{teamServerID: {names: map[string]string{subOwner: "Synthetic name"}}}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	h.storeRosterCurrent(canceled, server, []team.Seat{{ID: subOwner, Name: "Should not be saved"}})
	if got := h.subAsks[teamServerID].names[subOwner]; got != "Synthetic name" {
		t.Fatalf("canceled roster changed name: %q", got)
	}
	entries := []map[string]any{{"author": subOwner}}
	if names := h.commentNames(canceled, teamServerID, server, false, "", entries); len(names) != 0 {
		t.Fatal("canceled request returned names")
	}
	ctx, cancelMidflight := context.WithCancel(context.Background())
	defer cancelMidflight()
	trigger := &cancelOnSecondCopyCheck{Context: ctx, cancel: cancelMidflight}
	if names := h.commentNames(trigger, teamServerID, server, false, "", entries); len(names) != 0 || ctx.Err() != context.Canceled {
		t.Fatal("midflight canceled request returned names")
	}
	stale := server
	stale.Token = strings.Repeat("f", 64)
	if names := h.commentNames(context.Background(), teamServerID, stale, true, "", entries); len(names) != 0 {
		t.Fatal("stale server returned names")
	}
}
