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

func TestLocalInventoryFindsOrphanedPendingAndStage(t *testing.T) {
	h, dir := testTeamSurface(t, nil)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic file")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: strings.Repeat("a", 64), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := team.PeoplePending{Key: "fedcba9876543210", Author: installationOf(t, dir), ConfigHash: stage.ConfigHash, Body: json.RawMessage(`{"v":1,"text":"synthetic pending"}`), StageIDs: []string{stage.ID}}
	if err := team.SavePeoplePending(dir, id, pending, []team.PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/local?copy_instance=" + instance
	rec := doTeam(h, http.MethodGet, path, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "synthetic pending") || !strings.Contains(rec.Body.String(), "synthetic.txt") || strings.Contains(rec.Body.String(), "synthetic file") {
		t.Fatalf("inventory: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSubscriptionPollDoesNotReplaceLostProtectedCopyIdentity(t *testing.T) {
	h, dir := testTeamSurface(t, nil)
	sub := baseSub()
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic file")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: strings.Repeat("a", 64), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	pending := team.PeoplePending{Key: "fedcba9876543210", Author: installationOf(t, dir), ConfigHash: stage.ConfigHash, Body: json.RawMessage(`{"v":1}`), StageIDs: []string{stage.ID}}
	if err := team.SavePeoplePending(dir, id, pending, []team.PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-identity", "0000000000000000.json")
	if err := os.Remove(identity); err != nil {
		t.Fatal(err)
	}
	res := doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
	if res.Code != http.StatusOK {
		t.Fatalf("poll: %d %s", res.Code, res.Body.String())
	}
	if _, err := team.ReadPeopleCopyInstance(dir, id); err == nil {
		t.Fatal("poll replaced missing identity over a stage")
	}
	if _, err := os.Stat(identity); !os.IsNotExist(err) {
		t.Fatalf("identity was recreated: %v", err)
	}
	if _, err := team.ReadPeopleStage(dir, id, stage.ID); err != nil {
		t.Fatalf("stage lost: %v", err)
	}
	if _, err := team.ReadPeoplePending(dir, id, pending.Key); err != nil {
		t.Fatalf("pending lost: %v", err)
	}
}

func TestLocalInventoryQueryAndIdentity(t *testing.T) {
	h, dir := testTeamSurface(t, nil)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/local?copy_instance=" + instance
	for _, suffix := range []string{"&cursor=x", "&cursor=pzzzzzzzzzzzzzzzz", "&cursor=p0000000000000000&cursor=p0000000000000001", "&limit=99", "&copy_instance=" + instance} {
		rec := doTeam(h, http.MethodGet, base+suffix, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: %d %s", suffix, rec.Code, rec.Body.String())
		}
	}
	if rec := doTeam(h, http.MethodGet, base, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body.String())
	}
	malformed := httptest.NewRequest(http.MethodGet, base, nil)
	malformed.URL.RawQuery += "&broken=%ZZ"
	invalid := httptest.NewRecorder()
	h.ServeHTTP(invalid, malformed)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("malformed query: %d", invalid.Code)
	}
	wrong := strings.Replace(base, instance, strings.Repeat("0", 32), 1)
	if rec := doTeam(h, http.MethodGet, wrong, ""); rec.Code != http.StatusConflict {
		t.Fatalf("wrong copy: %d", rec.Code)
	}
	wrong = strings.Replace(base, "/"+id.Generation+"/local", "/"+strings.Repeat("0", 16)+"/local", 1)
	if rec := doTeam(h, http.MethodGet, wrong, ""); rec.Code != http.StatusConflict {
		t.Fatalf("wrong generation: %d", rec.Code)
	}
}

func TestLocalInventoryNeedsNoLiveAuthority(t *testing.T) {
	for _, state := range []string{"absent", "ended", "read_only", "replaced"} {
		t.Run(state, func(t *testing.T) {
			calls := 0
			client := team.NewClient()
			client.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected network") })
			h, dir := testTeamSurface(t, client)
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			instance, err := team.EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			if state == "replaced" {
				joinLab(t, dir, id.Server, "http://127.0.0.1:1234")
				if err := team.Remove(dir, id.Server); err != nil {
					t.Fatal(err)
				}
				joinLab(t, dir, id.Server, "http://127.0.0.1:5678")
			}
			if state != "absent" {
				sub := baseSub()
				if state == "ended" {
					sub.State = "ended"
				}
				seedSub(t, dir, sub)
			}
			data := []byte("synthetic file")
			sum := sha256.Sum256(data)
			stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "file.txt", MIME: "text/plain", ConfigHash: strings.Repeat("a", 64), OwnerInstance: "retired-owner", Data: data}
			if err := team.SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/local?copy_instance=" + instance
			rec := doTeam(h, http.MethodGet, path, "")
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "file.txt") || strings.Contains(rec.Body.String(), "synthetic file") {
				t.Fatalf("inventory: %d %s", rec.Code, rec.Body.String())
			}
			if calls != 0 {
				t.Fatalf("network calls: %d", calls)
			}
		})
	}
}

func TestLocalInventoryRefusesUnavailableState(t *testing.T) {
	h, dir := testTeamSurface(t, nil)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/local?copy_instance=" + instance
	installation := filepath.Join(dir, identity.InstallationFile)
	if err := os.Rename(installation, installation+".saved"); err != nil {
		t.Fatal(err)
	}
	if rec := doTeam(h, http.MethodGet, path, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "identity_unavailable") {
		t.Fatalf("installation: %d %s", rec.Code, rec.Body.String())
	}
	if err := os.Rename(installation+".saved", installation); err != nil {
		t.Fatal(err)
	}
	stageDir := filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-stages")
	if err := os.Mkdir(stageDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "bad.json"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if rec := doTeam(h, http.MethodGet, path, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "inventory_unavailable") {
		t.Fatalf("damaged: %d %s", rec.Code, rec.Body.String())
	}
}

func TestLocalInventoryQueryCompoundOutcomes(t *testing.T) {
	calls := 0
	client := team.NewClient()
	client.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected network") })
	h, dir := testTeamSurface(t, client)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/local"
	for _, tc := range []struct {
		name, query string
		status      int
	}{
		{"parse_failure", "copy_instance=" + instance + "&bad=%ZZ", http.StatusBadRequest},
		{"duplicate_cursor", "copy_instance=" + instance + "&cursor=p0000000000000000&cursor=s0000000000000000", http.StatusBadRequest},
		{"missing_copy", "cursor=p0000000000000000", http.StatusBadRequest},
		{"too_many_keys", "copy_instance=" + instance + "&cursor=p0000000000000000&extra=1", http.StatusBadRequest},
		{"unknown_second_key", "copy_instance=" + instance + "&extra=1", http.StatusBadRequest},
		{"short_cursor", "copy_instance=" + instance + "&cursor=p0", http.StatusBadRequest},
		{"invalid_kind", "copy_instance=" + instance + "&cursor=x0000000000000000", http.StatusBadRequest},
		{"invalid_cursor_id", "copy_instance=" + instance + "&cursor=pzzzzzzzzzzzzzzzz", http.StatusBadRequest},
		{"valid_pending_cursor", "copy_instance=" + instance + "&cursor=p0000000000000000", http.StatusOK},
		{"valid_stage_cursor", "copy_instance=" + instance + "&cursor=s0000000000000000", http.StatusOK},
		{"empty_cursor", "copy_instance=" + instance, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, base, nil)
			req.URL.RawQuery = tc.query
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	if calls != 0 {
		t.Fatalf("unexpected relay calls: %d", calls)
	}
	if actual, err := team.ReadPeopleCopyInstance(dir, id); err != nil || actual != instance {
		t.Fatalf("copy identity changed: %v", err)
	}
}
