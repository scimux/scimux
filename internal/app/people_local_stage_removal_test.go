package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

func TestLocalStageRemovalWithoutRelayAndRetry(t *testing.T) {
	calls := 0
	client := team.NewClient()
	client.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected network") })
	h, dir := testTeamSurface(t, client)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic file")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "file.txt", MIME: "text/plain", ConfigHash: strings.Repeat("a", 64), OwnerInstance: "old-owner", Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(stage)
	rev := sha256.Sum256(raw)
	path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/attachments/stage/" + stage.ID + "?copy_instance=" + instance + "&revision=" + hex.EncodeToString(rev[:])
	handler := h.(*teamHandler)
	matching, finishMatching := handler.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), id, stage.ID)
	defer finishMatching()
	unrelated, finishUnrelated := handler.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), id, "fedcba9876543210")
	defer finishUnrelated()
	for attempt := 0; attempt < 2; attempt++ {
		rec := doTeam(h, http.MethodDelete, path, "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("attempt %d: %d %s", attempt, rec.Code, rec.Body.String())
		}
	}
	if matching.Context().Err() == nil || unrelated.Context().Err() != nil {
		t.Fatal("stage operation fencing affected the wrong work")
	}
	if calls != 0 {
		t.Fatalf("relay calls %d", calls)
	}
}

func TestLocalStageRemovalLostAuthorityAndPreconditions(t *testing.T) {
	for _, mode := range []string{"ended", "read_only", "replaced", "retired_owner", "bad_revision", "bad_copy", "bad_revision_format", "duplicate_revision"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			client := team.NewClient()
			client.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected network") })
			h, dir := testTeamSurface(t, client)
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			instance, err := team.EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			sub := baseSub()
			if mode == "ended" {
				sub.State = "ended"
			}
			seedSub(t, dir, sub)
			if mode == "replaced" {
				joinLab(t, dir, id.Server, "http://127.0.0.1:1234")
				if err := team.Remove(dir, id.Server); err != nil {
					t.Fatal(err)
				}
				joinLab(t, dir, id.Server, "http://127.0.0.1:5678")
			}
			data := []byte("synthetic file")
			hash := sha256.Sum256(data)
			stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(hash[:]), Name: "file.txt", MIME: "text/plain", ConfigHash: strings.Repeat("a", 64), OwnerInstance: "old-owner", Data: data}
			if err := team.SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(stage)
			sum := sha256.Sum256(raw)
			revision := hex.EncodeToString(sum[:])
			query := "copy_instance=" + instance + "&revision=" + revision
			switch mode {
			case "bad_revision":
				query = "copy_instance=" + instance + "&revision=" + strings.Repeat("0", 64)
			case "bad_copy":
				query = "copy_instance=" + strings.Repeat("0", 32) + "&revision=" + revision
			case "bad_revision_format":
				query = "copy_instance=" + instance + "&revision=bad"
			case "duplicate_revision":
				query += "&revision=" + revision
			}
			path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/attachments/stage/" + stage.ID + "?" + query
			rec := doTeam(h, http.MethodDelete, path, "")
			invalid := strings.HasPrefix(mode, "bad_") || mode == "duplicate_revision"
			if invalid && rec.Code != http.StatusConflict {
				t.Fatalf("invalid: %d %s", rec.Code, rec.Body.String())
			}
			if !invalid && rec.Code != http.StatusNoContent {
				t.Fatalf("local removal: %d %s", rec.Code, rec.Body.String())
			}
			if invalid {
				if _, err := team.ReadPeopleStage(dir, id, stage.ID); err != nil {
					t.Fatal("refusal lost stage")
				}
			}
			if calls != 0 {
				t.Fatalf("relay calls=%d", calls)
			}
		})
	}
}

func TestLocalStageRemovalFailsClosed(t *testing.T) {
	h, dir := testTeamSurface(t, nil)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic file")
	hash := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(hash[:]), Name: "file.txt", MIME: "text/plain", ConfigHash: strings.Repeat("a", 64), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(stage)
	sum := sha256.Sum256(raw)
	revision := hex.EncodeToString(sum[:])
	base := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/attachments/stage/" + stage.ID + "?copy_instance=" + instance + "&revision=" + revision
	malformed := httptest.NewRequest(http.MethodDelete, base, nil)
	malformed.URL.RawQuery += "&bad=%ZZ"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, malformed)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "revision_required") {
		t.Fatalf("malformed: %d", rec.Code)
	}
	install := filepath.Join(dir, identity.InstallationFile)
	if err := os.Rename(install, install+".saved"); err != nil {
		t.Fatal(err)
	}
	if rec := doTeam(h, http.MethodDelete, base, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "identity_unavailable") {
		t.Fatalf("installation: %d", rec.Code)
	}
	if err := os.Rename(install+".saved", install); err != nil {
		t.Fatal(err)
	}
	pending := team.PeoplePending{Key: "fedcba9876543210", Author: installationOf(t, dir), ConfigHash: stage.ConfigHash, Body: json.RawMessage(`{"v":1}`), StageIDs: []string{stage.ID}}
	if err := team.SavePeoplePending(dir, id, pending, []team.PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	if rec := doTeam(h, http.MethodDelete, base, ""); rec.Code != http.StatusConflict {
		t.Fatalf("pinned: %d", rec.Code)
	}
	if _, err := team.ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("stage lost: %v", err)
	}
}

func TestLocalStageRemovalWebSecurity(t *testing.T) {
	cmd, dir, _, _ := startTeamCommand(t, true)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic file")
	hash := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(hash[:]), Name: "file.txt", MIME: "text/plain", ConfigHash: strings.Repeat("a", 64), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(stage)
	sum := sha256.Sum256(raw)
	core, err := newCoreMux(cmd.application)
	if err != nil {
		t.Fatal(err)
	}
	web, err := newWebBackend(webBackendConfig{Web: webFS, Core: core, RequestPolicy: cmd.application.requestPolicy})
	if err != nil {
		t.Fatal(err)
	}
	path := "http://127.0.0.1/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/attachments/stage/" + stage.ID + "?copy_instance=" + instance + "&revision=" + hex.EncodeToString(sum[:])
	for _, mode := range []string{"csrf", "host", "origin"} {
		t.Run(mode, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodDelete, path, nil)
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
			if _, err := team.ReadPeopleStage(dir, id, stage.ID); err != nil {
				t.Fatalf("stage changed: %v", err)
			}
		})
	}
}

func localStageRevision(stage team.PeopleStage) string {
	raw, _ := json.Marshal(stage)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestLocalStageCleanupDoesNotGrantRelayAuthority(t *testing.T) {
	for _, mode := range []string{"read_only", "lost_membership"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			client := team.NewClient()
			client.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected relay") })
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
			for _, tc := range []struct{ path, body string }{{"/attachments", ""}, {"/comments", `{"text":"synthetic","anchor":0,"key":"0123456789abcdef"}`}} {
				rec := doTeam(h, http.MethodPost, base+tc.path+"?copy_instance="+instance, tc.body)
				if rec.Code == http.StatusOK || rec.Code == http.StatusCreated || rec.Code == http.StatusNoContent {
					t.Fatalf("unauthorized publication: %d %s", rec.Code, rec.Body.String())
				}
			}
			if calls != 0 {
				t.Fatalf("unexpected relay calls: %d", calls)
			}
		})
	}
}
