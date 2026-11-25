package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

func TestTeamReactionsReadRetainedEventsSeparately(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	seedSub(t, dir, baseSub())
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	rows := []team.MirrorRecord{
		{Seq: 1, Key: "c/0000000000000001", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"hello","anchor":0}`)},
		{Seq: 2, Key: "r/0000000000000001", Author: "22222222222222222222222222222222", At: "2026-11-14T09:31:00.000Z", Body: json.RawMessage(`{"v":1,"t":"reaction","target":1,"emoji":"thumbs_up","active":true}`)},
		{Seq: 3, Key: "c/0000000000000002", Author: subOwner, At: "2026-11-14T09:32:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"next","anchor":0}`)},
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, rows); err != nil {
		t.Fatal(err)
	}
	instance, _ := team.ReadPeopleCopyInstance(dir, id)
	rec := doTeam(h, http.MethodGet, subReadPath()+"/reactions?after=0&copy_instance="+instance, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reaction read = %d %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Events []team.ReactionEvent `json:"events"`
		Next   uint64               `json:"next"`
		Last   uint64               `json:"last"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Author != "22222222222222222222222222222222" || page.Events[0].Seq != 2 || page.Next != 3 || page.Last != 3 {
		t.Fatalf("reaction page = %+v", page)
	}
	comments := doTeam(h, http.MethodGet, subReadPath()+"/comments?after=0", "")
	var messagePage struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(comments.Body.Bytes(), &messagePage); err != nil || len(messagePage.Entries) != 2 {
		t.Fatalf("ordinary comments changed: %s", comments.Body.String())
	}
	clamped := doTeam(h, http.MethodGet, subReadPath()+"/reactions?after=9", "")
	var clampedPage struct {
		Last uint64 `json:"last"`
		Next uint64 `json:"next"`
	}
	if clamped.Code != http.StatusOK || json.Unmarshal(clamped.Body.Bytes(), &clampedPage) != nil || clampedPage.Last != 3 || clampedPage.Next != 3 {
		t.Fatalf("cursor should clamp to retained tail: %d %s %+v", clamped.Code, clamped.Body.String(), clampedPage)
	}
}

func TestTeamReactionReadSkipsMalformedReactionTargets(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	seedSub(t, dir, baseSub())
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	rows := []team.MirrorRecord{
		{Seq: 1, Key: "c/0000000000000001", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"hello","anchor":0}`)},
		{Seq: 2, Key: "r/0000000000000001", Author: "22222222222222222222222222222222", At: "2026-11-14T09:31:00.000Z", Body: json.RawMessage(`{"v":1,"t":"reaction"}`)},
		{Seq: 3, Key: "r/0000000000000002", Author: "22222222222222222222222222222222", At: "2026-11-14T09:32:00.000Z", Body: json.RawMessage(`{"v":1,"t":"reaction","target":0}`)},
		{Seq: 4, Key: "r/0000000000000003", Author: "22222222222222222222222222222222", At: "2026-11-14T09:33:00.000Z", Body: json.RawMessage(`{"v":1,"t":"reaction","target":4}`)},
		{Seq: 5, Key: "r/0000000000000004", Author: subOwner, At: "2026-11-14T09:34:00.000Z", Body: json.RawMessage(`{"v":1,"t":"reaction","target":1,"emoji":"thumbs_up","active":true}`)},
		{Seq: 6, Key: "r/0000000000000005", Author: "22222222222222222222222222222222", At: "2026-11-14T09:35:00.000Z", Body: json.RawMessage(`{"v":1,"t":"reaction","target":1,"emoji":"thumbs_up","active":true}`)},
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, rows); err != nil {
		t.Fatal(err)
	}
	page := doTeam(h, http.MethodGet, subReadPath()+"/reactions", "")
	if page.Code != http.StatusOK {
		t.Fatalf("reaction read = %d %s", page.Code, page.Body.String())
	}
	var got struct {
		Events []team.ReactionEvent `json:"events"`
	}
	if err := json.Unmarshal(page.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 || got.Events[0].Seq != 6 {
		t.Fatalf("malformed reaction targets were retained: %+v", got.Events)
	}
}

func TestTeamReactionReadInitializesCopyFenceWithoutFileCapability(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	seedSub(t, dir, baseSub())
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	page := doTeam(h, http.MethodGet, subReadPath()+"/reactions?after=0", "")
	if page.Code != http.StatusOK {
		t.Fatalf("reaction read = %d %s", page.Code, page.Body.String())
	}
	var got struct {
		CopyInstance string `json:"copy_instance"`
		Last         uint64 `json:"last"`
	}
	if err := json.Unmarshal(page.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !subHex(got.CopyInstance, 32) || got.Last != 0 {
		t.Fatalf("reaction fence/page = %+v", got)
	}
	if _, err := team.ReadPeopleCopyInstance(dir, id); err != nil {
		t.Fatalf("reaction route did not establish its local fence: %v", err)
	}
}

func TestTeamReactionPostValidatesTargetPermissionAndFencesCopy(t *testing.T) {
	var appended string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Synthetic"))
			return
		}
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			appended = string(body)
			_, _ = io.WriteString(w, `{"generation":"`+subGen+`","results":[{"key":"r/0123456789abcdef","seq":2,"duplicate":false}],"last":2}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	sub := baseSub()
	sub.Access = "write"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/0000000000000001", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"hello","anchor":0}`)}}); err != nil {
		t.Fatal(err)
	}
	instance, _ := team.ReadPeopleCopyInstance(dir, id)
	path := subReadPath() + "/reactions?copy_instance=" + instance
	good := `{"target":1,"emoji":"thumbs_up","active":true,"key":"0123456789abcdef"}`
	rec := doTeam(h, http.MethodPost, path, good)
	if rec.Code != http.StatusOK {
		t.Fatalf("post = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(appended, `"t":"reaction"`) || strings.Contains(appended, `"actor"`) || !strings.Contains(appended, `"active":true`) {
		t.Fatalf("relay payload = %s", appended)
	}
	for _, body := range []string{
		`{"target":2,"emoji":"heart","active":true,"key":"0123456789abcdef"}`,
		`{"target":1,"emoji":"heart","active":true,"key":"0123456789abcdef","actor":"` + subOwner + `"}`,
		`{"target":1,"emoji":"thumbsup","active":true,"key":"0123456789abcdef"}`,
	} {
		if got := doTeam(h, http.MethodPost, path, body).Code; got != http.StatusBadRequest {
			t.Errorf("invalid post status=%d body=%s", got, body)
		}
	}
	read := sub
	read.Access = "read"
	if err := team.SaveSubs(dir, []team.Subscription{read}); err != nil {
		t.Fatal(err)
	}
	if got := doTeam(h, http.MethodPost, path, good).Code; got != http.StatusForbidden {
		t.Errorf("read-only post status=%d", got)
	}
	if got := doTeam(h, http.MethodPost, subReadPath()+"/reactions?copy_instance="+strings.Repeat("f", 32), good).Code; got != http.StatusConflict {
		t.Errorf("stale-copy post status=%d", got)
	}
}

func TestTeamReactionPostRejectsCopyRemovedAfterTargetRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Synthetic"))
			return
		}
		_, _ = io.WriteString(w, `{"generation":"`+subGen+`","results":[{"key":"r/0123456789abcdef","seq":2,"duplicate":false}],"last":2}`)
	}))
	defer srv.Close()
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	sub := baseSub()
	sub.Access = "write"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/0000000000000001", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"hello","anchor":0}`)}}); err != nil {
		t.Fatal(err)
	}
	instance, err := team.ReadPeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	originalRead := readReactionCopy
	t.Cleanup(func() { readReactionCopy = originalRead })
	readReactionCopy = func(dataDir string, copyID team.CopyID, log string, after uint64, count int, maxBytes int) ([]team.MirrorRecord, uint64, error) {
		rows, last, readErr := team.ReadCopy(dataDir, copyID, log, after, count, maxBytes)
		if readErr == nil && log == team.CommentsLog {
			if removeErr := team.RemoveCopy(dataDir, copyID); removeErr != nil {
				t.Errorf("remove copy after target read: %v", removeErr)
			}
		}
		return rows, last, readErr
	}
	path := subReadPath() + "/reactions?copy_instance=" + instance
	rec := doTeam(h, http.MethodPost, path, `{"target":1,"emoji":"thumbs_up","active":true,"key":"0123456789abcdef"}`)
	assertAPIError(t, rec, http.StatusConflict, "copy_changed")
}

func TestTeamReactionReadRejectsBadCursorsAndUsesBoundedPages(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	seedSub(t, dir, baseSub())
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		subReadPath() + "/reactions?after=01",
		subReadPath() + "/reactions?after=-1",
		subReadPath() + "/reactions?after=0&after=1",
		subReadPath() + "/reactions?unknown=0",
		subReadPath() + "/reactions?after=%ZZ",
		subReadPath() + "/reactions?after=0&copy_instance=bad",
	} {
		if rec := doTeam(h, http.MethodGet, path, ""); rec.Code != http.StatusBadRequest && rec.Code != http.StatusConflict {
			t.Errorf("GET %s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	rows := make([]team.MirrorRecord, 300)
	for i := range rows {
		seq := uint64(i + 1)
		rows[i] = team.MirrorRecord{Seq: seq, Key: "x/" + strings.Repeat("0", 15) + "1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"other"}`)}
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, rows); err != nil {
		t.Fatal(err)
	}
	var first struct {
		Last uint64 `json:"last"`
		Next uint64 `json:"next"`
		More bool   `json:"more"`
	}
	rec := doTeam(h, http.MethodGet, subReadPath()+"/reactions?after=0", "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &first) != nil || first.Last != 300 || first.Next != 256 || !first.More {
		t.Fatalf("first bounded page: %d %s %+v", rec.Code, rec.Body.String(), first)
	}
	var second struct {
		Last uint64 `json:"last"`
		Next uint64 `json:"next"`
		More bool   `json:"more"`
	}
	rec = doTeam(h, http.MethodGet, subReadPath()+"/reactions?after=256", "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &second) != nil || second.Last != 300 || second.Next != 300 || second.More {
		t.Fatalf("second bounded page: %d %s %+v", rec.Code, rec.Body.String(), second)
	}
}

func TestTeamReactionReadRejectsDuplicateCopyInstances(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	seedSub(t, dir, baseSub())
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	path := subReadPath() + "/reactions?copy_instance=" + instance + "&copy_instance=" + instance
	assertAPIError(t, doTeam(h, http.MethodGet, path, ""), http.StatusConflict, "copy_instance_required")
}

func TestTeamReactionReadReportsStorageFailures(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*testing.T, string, team.CopyID)
		status int
		code   string
	}{
		{name: "subscription store", setup: func(t *testing.T, dir string, _ team.CopyID) {
			if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
		}, status: http.StatusInternalServerError, code: "store_unavailable"},
		{name: "missing subscription", setup: func(*testing.T, string, team.CopyID) {}, status: http.StatusNotFound, code: "not_found"},
		{name: "copy identity", setup: func(t *testing.T, dir string, id team.CopyID) {
			if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
				t.Fatal(err)
			}
			identityRecord := filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-identity", "0000000000000000.json")
			if err := os.WriteFile(identityRecord, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
		}, status: http.StatusConflict, code: "copy_changed"},
		{name: "comments copy", setup: func(t *testing.T, dir string, id team.CopyID) {
			if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
				t.Fatal(err)
			}
			comments := filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "comments.jsonl")
			if err := os.WriteFile(comments, []byte("{\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}, status: http.StatusInternalServerError, code: "copy_damaged"},
		{name: "installation identity", setup: func(t *testing.T, dir string, id team.CopyID) {
			if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, identity.InstallationFile)); err != nil {
				t.Fatal(err)
			}
		}, status: http.StatusInternalServerError, code: "identity_unavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, dir := testTeamSurface(t, team.NewClient())
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			if tc.name != "missing subscription" {
				seedSub(t, dir, baseSub())
			}
			tc.setup(t, dir, id)
			assertAPIError(t, doTeam(h, http.MethodGet, subReadPath()+"/reactions", ""), tc.status, tc.code)
		})
	}
}

func TestTeamReactionReadTargetStorageFailureAndMissingTarget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		readError bool
	}{
		{name: "damaged target copy", readError: true},
		{name: "target no longer present"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, dir := testTeamSurface(t, team.NewClient())
			seedSub(t, dir, baseSub())
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
				t.Fatal(err)
			}
			rows := []team.MirrorRecord{
				{Seq: 1, Key: "c/0000000000000001", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"hello","anchor":0}`)},
				{Seq: 2, Key: "r/0000000000000001", Author: "22222222222222222222222222222222", At: "2026-11-14T09:31:00.000Z", Body: json.RawMessage(`{"v":1,"t":"reaction","target":1,"emoji":"thumbs_up","active":true}`)},
			}
			if err := team.AppendCopy(dir, id, team.CommentsLog, rows); err != nil {
				t.Fatal(err)
			}
			originalRead := readReactionCopy
			defer func() { readReactionCopy = originalRead }()
			calls := 0
			readReactionCopy = func(dataDir string, copyID team.CopyID, log string, after uint64, count int, maxBytes int) ([]team.MirrorRecord, uint64, error) {
				calls++
				if calls == 2 {
					if tc.readError {
						return nil, 0, errors.New("synthetic changed copy")
					}
					return nil, 0, nil
				}
				return team.ReadCopy(dataDir, copyID, log, after, count, maxBytes)
			}
			page := doTeam(h, http.MethodGet, subReadPath()+"/reactions", "")
			if tc.readError {
				assertAPIError(t, page, http.StatusInternalServerError, "copy_damaged")
				return
			}
			if page.Code != http.StatusOK {
				t.Fatalf("reaction read = %d %s", page.Code, page.Body.String())
			}
			var got struct {
				Events []team.ReactionEvent `json:"events"`
			}
			if err := json.Unmarshal(page.Body.Bytes(), &got); err != nil || len(got.Events) != 0 {
				t.Fatalf("missing target should be omitted: events=%+v err=%v", got.Events, err)
			}
		})
	}
}

func TestReactionCopyFencesCoverRetiredAndChangedOperations(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	owner := asTeamHandler(t, h)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	plain := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	if _, err := owner.ensureReactionCopy(plain, id); err == nil {
		t.Fatal("reaction copy initialized without an active operation")
	}
	if owner.bindReactionCopy(httptest.NewRecorder(), plain, id, strings.Repeat("a", 32)) {
		t.Fatal("copy identity bound without an active operation")
	}

	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil).WithContext(ctx)
	request, done := owner.beginPeopleCopyOperation(request, id, "")
	defer done()
	if op, ok := request.Context().Value(peopleCopyOperationKey{}).(*peopleCopyOperation); !ok || bindReactionCopyInstance(request, op, instance) != nil {
		t.Fatal("current copy binding failed")
	}
	cancel()
	op := request.Context().Value(peopleCopyOperationKey{}).(*peopleCopyOperation)
	if err := bindReactionCopyInstance(request, op, instance); err == nil {
		t.Fatal("retired operation accepted a copy binding")
	}

	originalEnsure := ensureReactionCopyInstance
	t.Cleanup(func() { ensureReactionCopyInstance = originalEnsure })
	ctx, cancel = context.WithCancel(context.Background())
	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil).WithContext(ctx)
	request, done = owner.beginPeopleCopyOperation(request, id, "")
	defer done()
	ensureReactionCopyInstance = func(dataDir string, copyID team.CopyID) (string, error) {
		value, err := team.EnsurePeopleCopyInstance(dataDir, copyID)
		cancel()
		return value, err
	}
	if _, err := owner.ensureReactionCopy(request, id); err == nil {
		t.Fatal("operation retired while establishing its copy fence was accepted")
	}
}

func TestReactionAuthorityRechecksServerMembershipAndPermission(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	owner := asTeamHandler(t, h)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	original := baseSub()
	original.Access = "write"
	request, done := owner.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil), id, "")
	defer done()
	entry := team.Entry{ID: teamServerID, Name: "Lab One", URL: "http://127.0.0.1:1", Token: teamToken, JoinedAt: "2026-11-12T09:30:00Z"}

	if current, err := owner.reactionAuthorityCurrent(httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil), id, original, entry); err != nil || current {
		t.Fatalf("missing operation authority = %t, %v", current, err)
	}
	if err := os.WriteFile(filepath.Join(dir, team.File), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.reactionAuthorityCurrent(request, id, original, entry); err == nil {
		t.Fatal("damaged server store accepted")
	}
	if err := os.Remove(filepath.Join(dir, team.File)); err != nil {
		t.Fatal(err)
	}
	if current, err := owner.reactionAuthorityCurrent(request, id, original, entry); err != nil || current {
		t.Fatalf("missing server authority = %t, %v", current, err)
	}
	joinLab(t, dir, teamServerID, entry.URL)
	if current, err := owner.reactionAuthorityCurrent(request, id, original, entry); err != nil || current {
		t.Fatalf("missing subscription authority = %t, %v", current, err)
	}
	if err := team.SaveSubs(dir, []team.Subscription{original}); err != nil {
		t.Fatal(err)
	}
	if err := team.SaveSubs(dir, []team.Subscription{original}); err != nil {
		t.Fatal(err)
	}
	if current, err := owner.reactionAuthorityCurrent(request, id, original, entry); err != nil || !current {
		t.Fatalf("current authority = %t, %v", current, err)
	}
	changed := original
	changed.Access = "read"
	if err := team.SaveSubs(dir, []team.Subscription{changed}); err != nil {
		t.Fatal(err)
	}
	if current, err := owner.reactionAuthorityCurrent(request, id, original, entry); err != nil || current {
		t.Fatalf("changed permission authority = %t, %v", current, err)
	}
}

func TestTeamReactionPostRejectsMalformedAndInactiveRequests(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	sub := baseSub()
	sub.Access = "write"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	instance, err := team.ReadPeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	path := subReadPath() + "/reactions?copy_instance=" + instance
	for _, body := range []string{"{", `{}`, `{"target":0,"emoji":"heart","active":true,"key":"0123456789abcdef"}`, `{"target":1,"emoji":"heart","active":true,"key":"0123456789abcdeg"}`, `{"target":1,"emoji":"heart","active":true,"key":"0123456789abcdef"} {}`, strings.Repeat("x", 5000)} {
		if rec := doTeam(h, http.MethodPost, path, body); rec.Code != http.StatusBadRequest {
			t.Errorf("POST body %q status=%d body=%s", body, rec.Code, rec.Body.String())
		}
	}
	if rec := doTeam(h, http.MethodPost, subReadPath()+"/reactions", `{}`); rec.Code != http.StatusConflict {
		t.Fatalf("POST without copy identity = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doTeam(h, http.MethodPut, subReadPath()+"/reactions", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("PUT reactions = %d %s", rec.Code, rec.Body.String())
	}
	ended := sub
	ended.State = "ended"
	if err := team.SaveSubs(dir, []team.Subscription{ended}); err != nil {
		t.Fatal(err)
	}
	good := `{"target":1,"emoji":"heart","active":true,"key":"0123456789abcdef"}`
	if rec := doTeam(h, http.MethodPost, path, good); rec.Code != http.StatusConflict {
		t.Fatalf("ended share POST = %d %s", rec.Code, rec.Body.String())
	}
}

func TestTeamReactionPostFencesPermissionChanges(t *testing.T) {
	for _, phase := range []string{"before append", "after append", "store damage", "final store damage"} {
		t.Run(phase, func(t *testing.T) {
			var dir string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/hello" {
					if phase == "before append" {
						updated := baseSub()
						updated.Access = "read"
						if err := team.SaveSubs(dir, []team.Subscription{updated}); err != nil {
							t.Errorf("save permission change: %v", err)
						}
					}
					if phase == "store damage" {
						if err := os.WriteFile(filepath.Join(dir, team.File), []byte("{"), 0600); err != nil {
							t.Errorf("damage server store: %v", err)
						}
					}
					_, _ = io.WriteString(w, helloJSON(teamServerID, "Synthetic"))
					return
				}
				if phase == "after append" {
					updated := baseSub()
					updated.Access = "read"
					if err := team.SaveSubs(dir, []team.Subscription{updated}); err != nil {
						t.Errorf("save permission change: %v", err)
					}
				}
				if phase == "final store damage" {
					if err := os.WriteFile(filepath.Join(dir, team.File), []byte("{"), 0600); err != nil {
						t.Errorf("damage server store after append: %v", err)
					}
				}
				body, _ := io.ReadAll(r.Body)
				var request struct {
					Entries []struct {
						Key string `json:"key"`
					} `json:"entries"`
				}
				_ = json.Unmarshal(body, &request)
				_, _ = io.WriteString(w, `{"generation":"`+subGen+`","results":[{"key":"`+request.Entries[0].Key+`","seq":2,"duplicate":false}],"last":2}`)
			}))
			defer srv.Close()
			h, dataDir := testTeamSurface(t, clientFor(srv))
			dir = dataDir
			joinLab(t, dir, teamServerID, srv.URL)
			sub := baseSub()
			sub.Access = "write"
			seedSub(t, dir, sub)
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
				t.Fatal(err)
			}
			if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/0000000000000001", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"hello","anchor":0}`)}}); err != nil {
				t.Fatal(err)
			}
			instance, err := team.ReadPeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			path := subReadPath() + "/reactions?copy_instance=" + instance
			rec := doTeam(h, http.MethodPost, path, `{"target":1,"emoji":"thumbs_up","active":true,"key":"0123456789abcdef"}`)
			want := "copy_changed"
			wantStatus := http.StatusConflict
			if phase == "before append" {
				want = "not_active"
			}
			if phase == "store damage" || phase == "final store damage" {
				want = "store_unavailable"
				wantStatus = http.StatusInternalServerError
			}
			assertAPIError(t, rec, wantStatus, want)
		})
	}
}

func TestTeamReactionPostMapsRelayFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		failAppend bool
		wantStatus int
		wantCode   string
	}{
		{name: "missing channel", status: http.StatusNotFound, wantStatus: http.StatusConflict, wantCode: "not_active"},
		{name: "stale generation", status: http.StatusConflict, wantStatus: http.StatusConflict, wantCode: "not_active"},
		{name: "forbidden", status: http.StatusForbidden, wantStatus: http.StatusForbidden, wantCode: "read_only"},
		{name: "unauthorized", status: http.StatusUnauthorized, wantStatus: http.StatusBadGateway, wantCode: "refused"},
		{name: "offline", failAppend: true, wantStatus: http.StatusBadGateway, wantCode: "offline"},
		{name: "protocol failure", status: http.StatusInternalServerError, wantStatus: http.StatusBadGateway, wantCode: "changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/hello" {
					_, _ = io.WriteString(w, helloJSON(teamServerID, "Synthetic"))
					return
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			client := clientFor(srv)
			if tc.failAppend {
				client.Transport = reactionFailAppendRoundTripper{base: client.Transport}
			}
			h, dir := testTeamSurface(t, client)
			joinLab(t, dir, teamServerID, srv.URL)
			sub := baseSub()
			sub.Access = "write"
			seedSub(t, dir, sub)
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
				t.Fatal(err)
			}
			if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/0000000000000001", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"hello","anchor":0}`)}}); err != nil {
				t.Fatal(err)
			}
			instance, err := team.ReadPeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			path := subReadPath() + "/reactions?copy_instance=" + instance
			rec := doTeam(h, http.MethodPost, path, `{"target":1,"emoji":"thumbs_up","active":true,"key":"0123456789abcdef"}`)
			assertAPIError(t, rec, tc.wantStatus, tc.wantCode)
		})
	}
}

func TestTeamReactionPostReportsStorageAndTargetFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*testing.T, string, team.CopyID)
		status int
		code   string
	}{
		{name: "missing subscription", setup: func(*testing.T, string, team.CopyID) {}, status: http.StatusNotFound, code: "not_found"},
		{name: "subscription store", setup: func(t *testing.T, dir string, _ team.CopyID) {
			if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
		}, status: http.StatusInternalServerError, code: "store_unavailable"},
		{name: "not joined", setup: func(t *testing.T, dir string, id team.CopyID) {
			seedSub(t, dir, writableReactionSub())
			seedReactionTarget(t, dir, id, subOwner)
			if err := team.Remove(dir, teamServerID); err != nil {
				t.Fatal(err)
			}
		}, status: http.StatusConflict, code: "not_active"},
		{name: "installation identity", setup: func(t *testing.T, dir string, id team.CopyID) {
			seedSub(t, dir, writableReactionSub())
			seedReactionTarget(t, dir, id, subOwner)
			if err := os.Remove(filepath.Join(dir, identity.InstallationFile)); err != nil {
				t.Fatal(err)
			}
		}, status: http.StatusInternalServerError, code: "identity_unavailable"},
		{name: "comments copy", setup: func(t *testing.T, dir string, id team.CopyID) {
			seedSub(t, dir, writableReactionSub())
			seedReactionTarget(t, dir, id, subOwner)
			path := filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "comments.jsonl")
			if err := os.WriteFile(path, []byte("{\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}, status: http.StatusInternalServerError, code: "copy_damaged"},
		{name: "self reaction", setup: func(t *testing.T, dir string, id team.CopyID) {
			seedSub(t, dir, writableReactionSub())
			viewer, err := identity.LoadInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}
			seedReactionTarget(t, dir, id, string(viewer))
		}, status: http.StatusBadRequest, code: "bad_request"},
		{name: "non People target", setup: func(t *testing.T, dir string, id team.CopyID) {
			seedSub(t, dir, writableReactionSub())
			if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
				t.Fatal(err)
			}
			if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/0000000000000001", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"agent","text":"not a People comment"}`)}}); err != nil {
				t.Fatal(err)
			}
		}, status: http.StatusBadRequest, code: "bad_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, dir := testTeamSurface(t, team.NewClient())
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			joinLab(t, dir, teamServerID, "http://127.0.0.1:1")
			tc.setup(t, dir, id)
			if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
				t.Fatal(err)
			}
			instance, err := team.ReadPeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			path := subReadPath() + "/reactions?copy_instance=" + instance
			rec := doTeam(h, http.MethodPost, path, `{"target":1,"emoji":"thumbs_up","active":true,"key":"0123456789abcdef"}`)
			assertAPIError(t, rec, tc.status, tc.code)
		})
	}
}

func writableReactionSub() team.Subscription {
	sub := baseSub()
	sub.Access = "write"
	return sub
}

func seedReactionTarget(t *testing.T, dir string, id team.CopyID, author string) {
	t.Helper()
	if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/0000000000000001", Author: author, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"hello","anchor":0}`)}}); err != nil {
		t.Fatal(err)
	}
}

type reactionFailAppendRoundTripper struct{ base http.RoundTripper }

func (rt reactionFailAppendRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
		return nil, errors.New("synthetic relay offline")
	}
	return rt.base.RoundTrip(r)
}
