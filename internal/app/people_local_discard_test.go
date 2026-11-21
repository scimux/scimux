package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

func TestLocalDiscardRetiresPendingWithoutRelay(t *testing.T) {
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
	raw, _ := json.Marshal(pending)
	revision := sha256.Sum256(raw)
	path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/" + pending.Key + "?copy_instance=" + instance + "&revision=" + hex.EncodeToString(revision[:])
	handler := h.(*teamHandler)
	handler.peopleEnabled = func() bool { return false }
	if denied := doTeam(h, http.MethodDelete, path, ""); denied.Code != http.StatusConflict || !strings.Contains(denied.Body.String(), "not_active") {
		t.Fatalf("disabled feature: %d %s", denied.Code, denied.Body.String())
	}
	calls := 0
	handler.peopleEnabled = func() bool { calls++; return calls == 1 }
	if denied := doTeam(h, http.MethodDelete, path, ""); denied.Code != http.StatusConflict || !strings.Contains(denied.Body.String(), "copy_changed") {
		t.Fatalf("feature disabled during request: %d %s", denied.Code, denied.Body.String())
	}
	handler.peopleEnabled = nil
	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	handler.peopleEnabled = func() bool {
		calls++
		if calls == 2 {
			cancel()
		}
		return true
	}
	request := httptest.NewRequest(http.MethodDelete, path, nil).WithContext(ctx)
	cancelled := httptest.NewRecorder()
	h.ServeHTTP(cancelled, request)
	if cancelled.Code != http.StatusConflict || !strings.Contains(cancelled.Body.String(), "copy_changed") {
		t.Fatalf("cancelled operation: %d %s", cancelled.Code, cancelled.Body.String())
	}
	handler.peopleEnabled = nil
	malformedQuery := httptest.NewRequest(http.MethodDelete, path, nil)
	malformedQuery.URL.RawQuery += "&broken=%ZZ"
	badQuery := httptest.NewRecorder()
	h.ServeHTTP(badQuery, malformedQuery)
	if badQuery.Code != http.StatusConflict || !strings.Contains(badQuery.Body.String(), "revision_required") {
		t.Fatalf("malformed query: %d %s", badQuery.Code, badQuery.Body.String())
	}
	identityPath := filepath.Join(dir, identity.InstallationFile)
	if err := os.Rename(identityPath, identityPath+".saved"); err != nil {
		t.Fatal(err)
	}
	if denied := doTeam(h, http.MethodDelete, path, ""); denied.Code != http.StatusConflict || !strings.Contains(denied.Body.String(), "identity_unavailable") {
		t.Fatalf("missing identity: %d %s", denied.Code, denied.Body.String())
	}
	if err := os.Rename(identityPath+".saved", identityPath); err != nil {
		t.Fatal(err)
	}
	missing := strings.Replace(path, pending.Key, "0011223344556677", 1)
	malformed := strings.Replace(path, pending.Key, "invalid", 1)
	if denied := doTeam(h, http.MethodDelete, malformed, ""); denied.Code != http.StatusNotFound {
		t.Fatalf("malformed key: %d %s", denied.Code, denied.Body.String())
	}
	if denied := doTeam(h, http.MethodDelete, missing, ""); denied.Code != http.StatusConflict || !strings.Contains(denied.Body.String(), "publication_unavailable") {
		t.Fatalf("missing key: %d %s", denied.Code, denied.Body.String())
	}
	other := pending
	other.Key = "0011223344556677"
	other.Author = strings.Repeat("0", 32)
	if err := team.SavePeoplePending(dir, id, other, []team.PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	if denied := doTeam(h, http.MethodDelete, missing, ""); denied.Code != http.StatusConflict || !strings.Contains(denied.Body.String(), "identity_unavailable") {
		t.Fatalf("foreign author: %d %s", denied.Code, denied.Body.String())
	}
	for _, bad := range []string{
		strings.Replace(path, instance, strings.Repeat("0", 32), 1),
		strings.Replace(path, hex.EncodeToString(revision[:]), strings.Repeat("0", 64), 1),
		strings.Replace(path, hex.EncodeToString(revision[:]), "invalid", 1),
		strings.Replace(path, "&revision="+hex.EncodeToString(revision[:]), "", 1),
		path + "&revision=" + hex.EncodeToString(revision[:]),
	} {
		refused := doTeam(h, http.MethodDelete, bad, "")
		if refused.Code != http.StatusConflict {
			t.Fatalf("forged precondition accepted: %d %s", refused.Code, refused.Body.String())
		}
		if _, err := team.ReadPeoplePending(dir, id, pending.Key); err != nil {
			t.Fatalf("refusal changed pending: %v", err)
		}
	}
	stageOperation, finishStage := handler.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/?copy_instance="+instance, nil), id, stage.ID)
	defer finishStage()
	if !handler.requirePeopleCopy(httptest.NewRecorder(), stageOperation) {
		t.Fatal("stage operation not bound")
	}
	matching, finishMatching := handler.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/?copy_instance="+instance, nil), id, "")
	defer finishMatching()
	if !handler.requirePeopleCopy(httptest.NewRecorder(), matching, pending.Key) {
		t.Fatal("matching operation not bound")
	}
	wrongPath := strings.Replace(path, hex.EncodeToString(revision[:]), strings.Repeat("0", 64), 1)
	if refused := doTeam(h, http.MethodDelete, wrongPath, ""); refused.Code != http.StatusConflict || matching.Context().Err() != nil {
		t.Fatal("refused discard cancelled matching work")
	}
	var unrelated []struct {
		request *http.Request
		finish  func()
	}
	for _, otherID := range []team.CopyID{
		{Server: id.Server, Owner: id.Owner, Chat: "abcdefabcdefabcd", Generation: id.Generation},
		{Server: strings.Repeat("f", 32), Owner: id.Owner, Chat: id.Chat, Generation: id.Generation},
	} {
		otherInstance, err := team.EnsurePeopleCopyInstance(dir, otherID)
		if err != nil {
			t.Fatal(err)
		}
		otherRequest, finish := handler.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/?copy_instance="+otherInstance, nil), otherID, "")
		if !handler.requirePeopleCopy(httptest.NewRecorder(), otherRequest, pending.Key) {
			t.Fatal("unrelated operation not bound")
		}
		unrelated = append(unrelated, struct {
			request *http.Request
			finish  func()
		}{otherRequest, finish})
		defer finish()
	}
	rec := doTeam(h, http.MethodDelete, path, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"locally_discarded"`) {
		t.Fatalf("discard: %d %s", rec.Code, rec.Body.String())
	}
	if matching.Context().Err() != context.Canceled {
		t.Fatal("successful discard left matching operation active")
	}
	if _, err := team.ReadPeoplePending(dir, id, pending.Key); err == nil {
		t.Fatal("pending remains")
	}
	if _, err := team.ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("stage lost: %v", err)
	}
	for _, other := range unrelated {
		if other.request.Context().Err() != nil || !handler.peopleCopyCurrent(other.request) {
			t.Fatal("discard affected another copy")
		}
	}
	if stageOperation.Context().Err() != nil || !handler.peopleCopyCurrent(stageOperation) {
		t.Fatal("discard affected unrelated stage operation")
	}
	wrongRetry := doTeam(h, http.MethodDelete, strings.Replace(path, hex.EncodeToString(revision[:]), strings.Repeat("0", 64), 1), "")
	if wrongRetry.Code != http.StatusConflict {
		t.Fatalf("retired revision mismatch: %d %s", wrongRetry.Code, wrongRetry.Body.String())
	}
	retry := doTeam(h, http.MethodDelete, path, "")
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `"locally_discarded"`) {
		t.Fatalf("discard retry: %d %s", retry.Code, retry.Body.String())
	}
}

func TestLocalDiscardReportsExistingCompletion(t *testing.T) {
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
	if err := team.SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	if err := team.ConfirmPeoplePending(dir, id, pending.Key, 1, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(pending)
	revision := sha256.Sum256(raw)
	path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/" + pending.Key + "?copy_instance=" + instance + "&revision=" + hex.EncodeToString(revision[:])
	bad := doTeam(h, http.MethodDelete, strings.Replace(path, hex.EncodeToString(revision[:]), strings.Repeat("0", 64), 1), "")
	if bad.Code != http.StatusConflict {
		t.Fatalf("completed revision mismatch: %d %s", bad.Code, bad.Body.String())
	}
	handler := h.(*teamHandler)
	inflight, finish := handler.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/?copy_instance="+instance, nil), id, "")
	defer finish()
	if !handler.requirePeopleCopy(httptest.NewRecorder(), inflight, pending.Key) {
		t.Fatal("could not bind in-flight publication")
	}
	for i := 0; i < 2; i++ {
		rec := doTeam(h, http.MethodDelete, path, "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"already_completed"`) || !strings.Contains(rec.Body.String(), `"seq":1`) {
			t.Fatalf("completed discard: %d %s", rec.Code, rec.Body.String())
		}
	}
	if inflight.Context().Err() != context.Canceled {
		t.Fatal("completed discard did not disarm matching work")
	}
}

func TestLocalDiscardRefusesReplacementAtStorageDecision(t *testing.T) {
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
	raw, _ := json.Marshal(pending)
	revision := sha256.Sum256(raw)
	path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/" + pending.Key + "?copy_instance=" + instance + "&revision=" + hex.EncodeToString(revision[:])
	replacement := pending
	replacement.Body = json.RawMessage(`{"v":2}`)
	handler := h.(*teamHandler)
	old, finish := handler.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/?copy_instance="+instance, nil), id, "")
	defer finish()
	if !handler.requirePeopleCopy(httptest.NewRecorder(), old, pending.Key) {
		t.Fatal("old operation not bound")
	}
	var nextInstance string
	handler.localDiscardBeforeDecision = func() {
		if err := team.RemoveCopy(dir, id); err != nil {
			t.Fatal(err)
		}
		next, err := team.EnsurePeopleCopyInstance(dir, id)
		if err != nil || next == instance {
			t.Fatalf("replacement copy: %v", err)
		}
		nextInstance = next
		if err := team.SavePeopleStage(dir, id, stage); err != nil {
			t.Fatal(err)
		}
		if err := team.SavePeoplePending(dir, id, replacement, []team.PeopleStage{stage}); err != nil {
			t.Fatal(err)
		}
	}
	rec := doTeam(h, http.MethodDelete, path, "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "publication_conflict") {
		t.Fatalf("stale discard: %d %s", rec.Code, rec.Body.String())
	}
	got, err := team.ReadPeoplePending(dir, id, pending.Key)
	if err != nil || string(got.Body) != string(replacement.Body) {
		t.Fatalf("replacement lost: %v", err)
	}
	handler.localDiscardBeforeDecision = nil
	newRaw, _ := json.Marshal(replacement)
	newRevision := sha256.Sum256(newRaw)
	newPath := strings.Replace(strings.Replace(path, instance, nextInstance, 1), hex.EncodeToString(revision[:]), hex.EncodeToString(newRevision[:]), 1)
	if result := doTeam(h, http.MethodDelete, newPath, ""); result.Code != http.StatusOK {
		t.Fatalf("replacement discard: %d %s", result.Code, result.Body.String())
	}
	if old.Context().Err() != nil || handler.peopleCopyCurrent(old) {
		t.Fatal("replacement discard affected old copy operation or old copy stayed current")
	}
}
