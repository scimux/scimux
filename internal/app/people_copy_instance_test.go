package app

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/team"
)

func TestPeopleCopyAdmissionRejectsUnavailableAndRetiredIdentity(t *testing.T) {
	dir := t.TempDir()
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	h := &teamHandler{dataDir: dir}
	check := func(raw string, admitted bool, code int, message string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/?"+raw, nil)
		if admitted {
			var done func()
			req, done = h.beginPeopleCopyOperation(req, id, "")
			defer done()
		}
		rec := httptest.NewRecorder()
		ok := h.requirePeopleCopy(rec, req)
		if ok != (code == http.StatusOK) || (!ok && (rec.Code != code || !strings.Contains(rec.Body.String(), message))) {
			t.Fatalf("admission = %v, status %d, body %s", ok, rec.Code, rec.Body.String())
		}
	}
	check("", true, http.StatusConflict, "copy_instance_required")
	check("copy_instance="+instance+"&copy_instance="+instance, true, http.StatusConflict, "copy_instance_required")
	check("copy_instance="+instance, false, http.StatusConflict, "copy_changed")
	check("copy_instance="+strings.Repeat("f", 32), true, http.StatusConflict, "copy_changed")
	check("copy_instance="+instance, true, http.StatusOK, "")
	identityPath := filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-identity", "0000000000000000.json")
	if err := os.WriteFile(identityPath, []byte("synthetic damage"), 0600); err != nil {
		t.Fatal(err)
	}
	check("copy_instance="+instance, true, http.StatusConflict, "copy_identity_unavailable")
	if data, err := os.ReadFile(identityPath); err != nil || string(data) != "synthetic damage" {
		t.Fatal("damaged identity changed")
	}
	if len(h.copyRemovalEpoch) != 0 {
		t.Fatal("completed admissions retained operation state")
	}
}

func TestPeopleCopyOperationLifetimeKeepsOverlappingWorkSeparate(t *testing.T) {
	dir := t.TempDir()
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	h := &teamHandler{dataDir: dir}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if peopleCopyCurrentLocked(req) {
		t.Fatal("an unadmitted request was current")
	}
	first, finishFirst := h.beginPeopleCopyOperation(req, id, "0123456789abcdef")
	second, finishSecond := h.beginPeopleCopyOperation(req, id, "fedcba9876543210")
	if !peopleCopyCurrentLocked(first) || !peopleCopyCurrentLocked(second) {
		t.Fatal("fresh overlapping work was not current")
	}
	h.mu.Lock()
	state := h.copyRemovalEpoch[id]
	state.stages = map[string]uint64{"0123456789abcdef": 1}
	h.mu.Unlock()
	if peopleCopyCurrentLocked(first) || !peopleCopyCurrentLocked(second) {
		t.Fatal("stage retirement affected the wrong request")
	}
	finishFirst()
	if !peopleCopyCurrentLocked(second) || len(h.copyRemovalEpoch) != 1 {
		t.Fatal("finishing older work retired newer work")
	}
	h.mu.Lock()
	state.epoch++
	h.mu.Unlock()
	if peopleCopyCurrentLocked(second) {
		t.Fatal("retired copy operation remained current")
	}
	finishSecond()
	if len(h.copyRemovalEpoch) != 0 {
		t.Fatal("operation state was not released")
	}
}

func TestPeopleAttachmentRoutesRejectNoncanonicalAddresses(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments/"
	for _, tc := range []struct{ method, suffix string }{
		{http.MethodGet, "stage/not-a-stage"},
		{http.MethodDelete, "stage/0123456789ABCDEf"},
		{http.MethodGet, "0/0123456789abcdef"},
		{http.MethodGet, "01/0123456789abcdef"},
		{http.MethodGet, "not-a-sequence/0123456789abcdef"},
		{http.MethodGet, "1/not-an-id"},
	} {
		t.Run(tc.method+"/"+tc.suffix, func(t *testing.T) {
			rec := doTeam(h, tc.method, base+tc.suffix, "")
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not_found") {
				t.Fatalf("noncanonical address status=%d body=%s", rec.Code, rec.Body.String())
			}
			if len(h.copyRemovalEpoch) != 0 {
				t.Fatal("rejected address retained operation state")
			}
		})
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if _, err := team.ReadPeopleCopyInstance(dir, id); err == nil {
		t.Fatal("invalid addresses created a local copy identity")
	}
}

func TestPeopleCommentRejectsEmptyAndExcessiveAttachmentLists(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/comments"
	for _, ids := range [][]string{{}, make([]string, 17)} {
		body, err := json.Marshal(map[string]any{"text": "synthetic", "anchor": 0, "key": "0123456789abcdef", "attachments": ids})
		if err != nil {
			t.Fatal(err)
		}
		rec := doTeam(h, http.MethodPost, path, string(body))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad_request") {
			t.Fatalf("invalid list accepted: %d %s", rec.Code, rec.Body.String())
		}
		if len(h.copyRemovalEpoch) != 0 {
			t.Fatal("invalid publication retained operation state")
		}
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if _, err := team.ReadPeopleCopyInstance(dir, id); err == nil {
		t.Fatal("invalid publication created copy metadata")
	}
}

func TestPeopleWriteIdentityRequiresTheRightAuthority(t *testing.T) {
	h := &teamHandler{dataDir: t.TempDir()}
	request := func(query string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.URL.RawQuery = query
		return r
	}
	for _, tc := range []struct {
		access, query string
		want          bool
	}{
		{"write", "", true},
		{"write", "invalid=%ZZ", false},
		{"read", "", false},
		{"owner", "", false},
		{"owner", "chat_instance=a&chat_instance=b", false},
		{"owner", "chat_instance=synthetic-owner", false},
	} {
		t.Run(tc.access+"/"+tc.query, func(t *testing.T) {
			instance, ok := h.peopleWriteInstance(request(tc.query), team.Subscription{Access: tc.access})
			if ok != tc.want || instance != "" {
				t.Fatalf("write authority accepted=%v instance=%q", ok, instance)
			}
		})
	}
	if !h.peopleWriteBound(team.Subscription{Access: "write"}, "") || h.peopleWriteBound(team.Subscription{Access: "write"}, "other") {
		t.Fatal("recipient write acquired owner identity")
	}
	if h.peopleWriteBound(team.Subscription{Access: "owner"}, "synthetic-owner") || h.peopleWriteBound(team.Subscription{Access: "read"}, "") {
		t.Fatal("unverified owner or read-only subscription was writable")
	}
}

func TestPeopleWriteCommitGuardRechecksCurrentCopyAndServer(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	server := labEntry(teamServerID, "http://127.0.0.1:1", teamToken)
	seedTeam(t, dir, server)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	sub := baseSub()
	sub.Access = "write"
	check := func(want bool) {
		t.Helper()
		h.mu.Lock()
		got := h.peopleWriteCurrentLocked(id, sub, server, "")
		h.mu.Unlock()
		if got != want {
			t.Fatalf("commit authority=%v want %v", got, want)
		}
	}
	if err := team.SaveSubs(dir, []team.Subscription{sub}); err != nil {
		t.Fatal(err)
	}
	check(true)
	stale := server
	stale.Token = "synthetic-other-token"
	h.mu.Lock()
	allowed := h.peopleWriteCurrentLocked(id, sub, stale, "")
	h.mu.Unlock()
	if allowed {
		t.Fatal("old server configuration remained writable")
	}
	for _, changed := range []team.Subscription{func() team.Subscription { x := sub; x.Access = "read"; return x }(), func() team.Subscription { x := sub; x.State = "ended"; return x }(), func() team.Subscription { x := sub; x.Generation = "eeeeeeeeeeeeeeee"; return x }()} {
		if err := team.SaveSubs(dir, []team.Subscription{changed}); err != nil {
			t.Fatal(err)
		}
		check(false)
	}
	if err := team.SaveSubs(dir, nil); err != nil {
		t.Fatal(err)
	}
	check(false)
	if err := team.SaveSubs(dir, []team.Subscription{sub}); err != nil {
		t.Fatal(err)
	}
	h.peopleEnabled = func() bool { return false }
	check(false)
	h.peopleEnabled = nil
	if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("synthetic damage"), 0600); err != nil {
		t.Fatal(err)
	}
	check(false)
}

func TestPeopleStaleCopyPreconditionRejectsHistorySeenAndRemoval(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	sub := baseSub()
	seedSub(t, dir, sub)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	stale := strings.Repeat("f", 32)
	if stale == instance {
		stale = strings.Repeat("e", 32)
	}
	before, err := os.ReadFile(filepath.Join(dir, team.SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, base, ""}, {http.MethodGet, base + "/comments", ""}, {http.MethodPut, base + "/seen", `{"comments":1}`}, {http.MethodDelete, base, ""},
		{http.MethodPost, base + "/comments", `{"text":"synthetic","anchor":0,"key":"0123456789abcdef","attachments":["fedcba9876543210"]}`},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			rec := doTeam(h, tc.method, tc.path+"?copy_instance="+stale, tc.body)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "copy_changed") {
				t.Fatalf("stale operation status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	after, err := os.ReadFile(filepath.Join(dir, team.SubsFile))
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("stale operations changed subscription: %v", err)
	}
	if current, err := team.ReadPeopleCopyInstance(dir, id); err != nil || current != instance {
		t.Fatalf("stale operations changed copy identity: %v", err)
	}
	if len(h.copyRemovalEpoch) != 0 {
		t.Fatal("stale operations retained lifetime bookkeeping")
	}
}

// peopleTestURL supplies the identity a current synthetic browser would have
// received from its subscription view. Call before starting a held request.
func peopleTestURL(t *testing.T, dataDir, path string) string {
	t.Helper()
	u, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/api/team/subscriptions/"), "/")
	if len(parts) < 4 {
		t.Fatalf("invalid synthetic subscription URL: %s", u.Path)
	}
	id := team.CopyID{Server: parts[0], Owner: parts[1], Chat: parts[2], Generation: parts[3]}
	instance, err := team.EnsurePeopleCopyInstance(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("copy_instance", instance)
	u.RawQuery = query.Encode()
	return u.String()
}

func TestPeopleCapabilityExposesDurableCopyInstance(t *testing.T) {
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
	h.peopleCapabilitiesReady = true
	joinLab(t, dir, teamServerID, srv.URL)
	if rec := doTeam(h, http.MethodGet, "/api/team/subscriptions", ""); rec.Code != http.StatusOK {
		t.Fatalf("synthetic subscription refresh status %d", rec.Code)
	}
	subs, err := team.LoadSubs(dir)
	if err != nil || len(subs) != 1 {
		t.Fatalf("synthetic subscription setup: %v", err)
	}
	instance := func() string {
		t.Helper()
		rec := doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
		var result struct {
			Subscriptions []struct {
				CopyInstance string `json:"people_copy_instance"`
			} `json:"subscriptions"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil || len(result.Subscriptions) != 1 {
			t.Fatalf("list unavailable: status %d", rec.Code)
		}
		return result.Subscriptions[0].CopyInstance
	}
	first := instance()
	if !subHex(first, 32) {
		t.Fatal("People file capability did not expose a stable local copy identity")
	}
	if second := instance(); second != first {
		t.Fatal("copy identity changed on refresh")
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	if rec := doTeam(h, http.MethodDelete, base, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("copy removal status %d", rec.Code)
	}
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	next := instance()
	if !subHex(next, 32) || next == first {
		t.Fatal("recreated copy reused the old local copy identity")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("synthetic old upload"))
	_ = form.Close()
	req := httptest.NewRequest(http.MethodPost, base+"/attachments?stage_id=0123456789abcdef&copy_instance="+first, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("old copy precondition accepted after recreation")
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if _, err := team.ReadPeopleStage(dir, id, "0123456789abcdef"); err == nil {
		t.Fatal("old request staged bytes in the replacement copy")
	}
}
