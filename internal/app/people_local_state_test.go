package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

func TestLocalPublicationStateReview(t *testing.T) {
	for _, state := range []string{"pending", "retired", "completed"} {
		t.Run(state, func(t *testing.T) {
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
			pending := team.PeoplePending{Key: "fedcba9876543210", Author: installationOf(t, dir), ConfigHash: stage.ConfigHash, Body: json.RawMessage(`{"v":1,"text":"synthetic pending"}`), StageIDs: []string{stage.ID}}
			if err := team.SavePeoplePending(dir, id, pending, []team.PeopleStage{stage}); err != nil {
				t.Fatal(err)
			}
			if state == "retired" {
				if _, err := team.DiscardPeoplePending(dir, id, instance, pending); err != nil {
					t.Fatal(err)
				}
			}
			if state == "completed" {
				if err := team.SavePeoplePublished(dir, id, 1, stage); err != nil {
					t.Fatal(err)
				}
				if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
					t.Fatal(err)
				}
				if err := team.ConfirmPeoplePending(dir, id, pending.Key, 1); err != nil {
					t.Fatal(err)
				}
			}
			path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/" + pending.Key + "?copy_instance=" + instance
			copyDir := filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation)
			before := localStateCopyFiles(t, copyDir)
			rec := doTeam(h, http.MethodGet, path, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("state read: %d %s", rec.Code, rec.Body.String())
			}
			var result struct {
				State    string   `json:"state"`
				Text     string   `json:"text"`
				Revision string   `json:"revision"`
				StageIDs []string `json:"stage_ids"`
				Seq      uint64   `json:"seq"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.State != state || result.Text != "synthetic pending" || len(result.Revision) != 64 || len(result.StageIDs) != 1 || result.StageIDs[0] != stage.ID {
				t.Fatalf("unexpected projection: %+v", result)
			}
			pendingJSON, err := json.Marshal(pending)
			if err != nil {
				t.Fatal(err)
			}
			revision := sha256.Sum256(pendingJSON)
			if result.Revision != hex.EncodeToString(revision[:]) {
				t.Fatal("projection revision does not match the immutable pending value")
			}
			if state == "completed" && result.Seq != 1 {
				t.Fatalf("sequence: %+v", result)
			}
			if state != "completed" && result.Seq != 0 {
				t.Fatalf("unexpected sequence: %+v", result)
			}
			if strings.Contains(rec.Body.String(), "synthetic file") || strings.Contains(rec.Body.String(), "config_hash") || strings.Contains(rec.Body.String(), "\"body\"") {
				t.Fatal("projection exposed internal data")
			}
			if !reflect.DeepEqual(before, localStateCopyFiles(t, copyDir)) {
				t.Fatal("state read changed copy files")
			}
		})
	}
}

func TestLocalPublicationStateReceiptProofAndPin(t *testing.T) {
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
	pending := team.PeoplePending{Key: "fedcba9876543210", Author: installationOf(t, dir), ConfigHash: stage.ConfigHash, Body: json.RawMessage(`{"v":1,"text":"synthetic"}`), StageIDs: []string{stage.ID}}
	if err := team.SavePeoplePending(dir, id, pending, []team.PeopleStage{stage}); err != nil {
		t.Fatal(err)
	}
	copyDir := filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation)
	pendingPath := filepath.Join(copyDir, "people-pending", pending.Key+".json")
	raw, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := team.SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	if err := team.ConfirmPeoplePending(dir, id, pending.Key, 1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/" + pending.Key + "?copy_instance=" + instance
	rec := doTeam(h, http.MethodGet, path, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"seq":1`) || !strings.Contains(rec.Body.String(), `"pending_pinned":true`) {
		t.Fatalf("pinned receipt: %d %s", rec.Code, rec.Body.String())
	}
	if got, err := os.ReadFile(pendingPath); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("GET changed pinned pending")
	}
	if err := team.DeletePeopleStage(dir, id, stage.ID); err == nil {
		t.Fatal("GET released stage pin")
	}
	retained := filepath.Join(copyDir, "people-records", "comments", "1", stage.ID+".json")
	kept, err := os.ReadFile(retained)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(retained); err != nil {
		t.Fatal(err)
	}
	if got := doTeam(h, http.MethodGet, path, ""); got.Code != http.StatusConflict {
		t.Fatalf("pinned missing bytes: %d %s", got.Code, got.Body.String())
	}
	if err := os.WriteFile(retained, kept, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pendingPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(retained); err != nil {
		t.Fatal(err)
	}
	if got := doTeam(h, http.MethodGet, path, ""); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"seq":1`) {
		t.Fatalf("unpinned missing bytes: %d %s", got.Code, got.Body.String())
	}
	if err := os.WriteFile(retained, []byte("synthetic corruption"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := doTeam(h, http.MethodGet, path, ""); got.Code != http.StatusConflict {
		t.Fatalf("corrupt retained bytes: %d %s", got.Code, got.Body.String())
	}
	if err := os.Remove(retained); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(copyDir, "comments.jsonl")); err != nil {
		t.Fatal(err)
	}
	if got := doTeam(h, http.MethodGet, path, ""); got.Code != http.StatusConflict {
		t.Fatalf("missing comment: %d %s", got.Code, got.Body.String())
	}
}

func TestLocalPublicationStateQueryAndMissing(t *testing.T) {
	calls := 0
	client := team.NewClient()
	client.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected synthetic relay call")
	})
	h, dir := testTeamSurface(t, client)
	handler := h.(*teamHandler)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	author := installationOf(t, dir)
	key := "fedcba9876543210"
	base := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/" + key
	path := base + "?copy_instance=" + instance
	if rec := doTeam(h, http.MethodGet, path, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"missing"`) {
		t.Fatalf("missing: %d %s", rec.Code, rec.Body.String())
	}
	if err := team.RetireAbsentPeopleKey(dir, id, instance, author, key); err != nil {
		t.Fatal(err)
	}
	if rec := doTeam(h, http.MethodGet, path, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"absent_retired"`) || strings.Contains(rec.Body.String(), "revision") {
		t.Fatalf("absent marker: %d %s", rec.Code, rec.Body.String())
	}
	for _, suffix := range []string{"?copy_instance=" + instance + "&copy_instance=" + instance, "?copy_instance=" + instance + "&extra=1", "?copy_instance=" + instance + "&bad=%ZZ"} {
		rec := doTeam(h, http.MethodGet, base+suffix, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := doTeam(h, http.MethodGet, base+"?copy_instance="+strings.Repeat("0", 32), ""); rec.Code != http.StatusConflict {
		t.Fatalf("wrong copy: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doTeam(h, http.MethodGet, strings.Replace(path, key, "invalid", 1), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bad key: %d", rec.Code)
	}
	if rec := doTeam(h, http.MethodPost, path, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("method: %d", rec.Code)
	}
	handler.peopleEnabled = func() bool { return false }
	if rec := doTeam(h, http.MethodGet, path, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_active") {
		t.Fatalf("disabled: %d %s", rec.Code, rec.Body.String())
	}
	handler.peopleEnabled = nil
	if calls != 0 {
		t.Fatalf("relay calls: %d", calls)
	}
}

func TestLocalPublicationStateNeedsNoLiveAuthority(t *testing.T) {
	for _, mode := range []string{"absent", "ended", "read_only", "replaced"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			client := team.NewClient()
			client.Transport = teamRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected synthetic relay call")
			})
			h, dir := testTeamSurface(t, client)
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			instance, err := team.EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "replaced" {
				joinLab(t, dir, id.Server, "http://127.0.0.1:1234")
				if err := team.Remove(dir, id.Server); err != nil {
					t.Fatal(err)
				}
				joinLab(t, dir, id.Server, "http://127.0.0.1:5678")
			}
			if mode != "absent" {
				sub := baseSub()
				if mode == "ended" {
					sub.State = "ended"
				}
				if mode == "read_only" {
					sub.Access = "read"
				}
				seedSub(t, dir, sub)
			}
			data := []byte("synthetic file")
			sum := sha256.Sum256(data)
			stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "file.txt", MIME: "text/plain", ConfigHash: strings.Repeat("a", 64), OwnerInstance: "retired-owner", Data: data}
			if err := team.SavePeopleStage(dir, id, stage); err != nil {
				t.Fatal(err)
			}
			pending := team.PeoplePending{Key: "fedcba9876543210", Author: installationOf(t, dir), ConfigHash: stage.ConfigHash, OwnerInstance: stage.OwnerInstance, Body: json.RawMessage(`{"text":"synthetic pending"}`), StageIDs: []string{stage.ID}}
			if err := team.SavePeoplePending(dir, id, pending, []team.PeopleStage{stage}); err != nil {
				t.Fatal(err)
			}
			path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/" + pending.Key + "?copy_instance=" + instance
			rec := doTeam(h, http.MethodGet, path, "")
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "synthetic pending") {
				t.Fatalf("state: %d %s", rec.Code, rec.Body.String())
			}
			if calls != 0 {
				t.Fatalf("relay calls: %d", calls)
			}
		})
	}
}

func TestLocalPublicationStateRefusesUnavailableIdentityAndMidflightGate(t *testing.T) {
	h, dir := testTeamSurface(t, nil)
	handler := h.(*teamHandler)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/fedcba9876543210?copy_instance=" + instance
	installation := filepath.Join(dir, identity.InstallationFile)
	if err := os.Rename(installation, installation+".saved"); err != nil {
		t.Fatal(err)
	}
	if rec := doTeam(h, http.MethodGet, path, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "identity_unavailable") {
		t.Fatalf("identity: %d %s", rec.Code, rec.Body.String())
	}
	if err := os.Rename(installation+".saved", installation); err != nil {
		t.Fatal(err)
	}
	calls := 0
	handler.peopleEnabled = func() bool { calls++; return calls == 1 }
	if rec := doTeam(h, http.MethodGet, path, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "copy_changed") {
		t.Fatalf("midflight gate: %d %s", rec.Code, rec.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	handler.peopleEnabled = func() bool {
		calls++
		if calls == 2 {
			cancel()
		}
		return true
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "copy_changed") {
		t.Fatalf("cancelled: %d %s", rec.Code, rec.Body.String())
	}
}

func TestLocalPublicationStateDoesNotCreateOrChangeCopyFiles(t *testing.T) {
	h, dir := testTeamSurface(t, nil)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + id.Server + "/" + id.Owner + "/" + id.Chat + "/" + id.Generation + "/pending/0123456789abcdef?copy_instance=" + instance
	copyDir := filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation)
	before := localStateCopyFiles(t, copyDir)
	if rec := doTeam(h, http.MethodGet, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("missing: %d %s", rec.Code, rec.Body.String())
	}
	after := localStateCopyFiles(t, copyDir)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("state read changed copy files")
	}
}

func localStateCopyFiles(t *testing.T, copyDir string) map[string][32]byte {
	t.Helper()
	result := map[string][32]byte{}
	err := filepath.WalkDir(copyDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(copyDir, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			result[rel] = [32]byte{}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[rel] = sha256.Sum256(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
