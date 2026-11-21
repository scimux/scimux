package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/scimux/scimux/internal/team"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPeopleRelayQuotaRefusalIsActionableAndKeepsStage(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			puts++
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusInsufficientStorage)
			_, _ = io.WriteString(w, `{"error":"quota"}`)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("synthetic")); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path+"/attachments?stage_id=0123456789abcdef"), &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if puts != 1 || rec.Code == http.StatusOK {
		t.Fatalf("quota refusal not exercised: puts=%d status=%d", puts, rec.Code)
	}
	var result struct {
		Error   string `json:"error"`
		StageID string `json:"stage_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.StageID != "0123456789abcdef" {
		t.Fatalf("retry identity lost: %q", result.StageID)
	}
	saved := doTeam(h, http.MethodGet, peopleTestURL(t, dir, path+"/attachments/stage/"+result.StageID), "")
	if saved.Code != http.StatusOK {
		t.Fatalf("staged retry file unavailable: %d", saved.Code)
	}
	if !strings.Contains(strings.ToLower(result.Error), "quota") && !strings.Contains(strings.ToLower(result.Error), "storage") {
		t.Fatalf("known relay quota refusal hidden behind non-actionable error %q", result.Error)
	}
}

func TestPeopleBlobRefusalClassification(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{team.ErrQuota, 507, "relay_quota"}, {team.ErrTooLarge, 413, "file_too_large"},
		{team.ErrUnauthorized, 401, "relay_unauthorized"}, {team.ErrForbidden, 403, "relay_forbidden"},
		{team.ErrStaleGeneration, 409, "stale_generation"}, {team.ErrNoChannel, 404, "channel_unavailable"},
		{team.ErrBlobUnsupported, 409, "attachments_unsupported"}, {errors.New("sensitive synthetic network detail"), 502, "upload_unconfirmed"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			status, code := peopleBlobFailure(tc.err)
			if status != tc.status || code != tc.code {
				t.Fatalf("got %d %q", status, code)
			}
		})
	}
}
