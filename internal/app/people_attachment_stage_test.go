package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
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

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

type syntheticInterruptedUpload struct{}

func (syntheticInterruptedUpload) Read([]byte) (int, error) {
	return 0, fmt.Errorf("synthetic upload interruption")
}

func TestPeopleAttachmentStageUsesGuardedRelay(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	payload := []byte("synthetic attachment")
	hashBytes := sha256.Sum256(payload)
	hash := hex.EncodeToString(hashBytes[:])
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			puts++
			if r.Method != http.MethodPut || r.URL.Query().Get("generation") != subGen {
				t.Error("upload missing generation guard")
			}
			body, _ := io.ReadAll(r.Body)
			if !bytes.Equal(body, payload) {
				t.Error("wrong upload payload")
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":"%s","size":%d}`, hash, len(payload))
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
	_, _ = part.Write(payload)
	_ = form.Close()
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments"
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path+"?stage_id=0123456789abcdef"), &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stage status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result struct {
		Attachments []struct {
			ID     string `json:"id"`
			SHA256 string `json:"sha256"`
			Size   int    `json:"size"`
			Name   string `json:"name"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Attachments) != 1 || result.Attachments[0].ID != "0123456789abcdef" || result.Attachments[0].SHA256 != hash || result.Attachments[0].Size != len(payload) || result.Attachments[0].Name != "synthetic.txt" || puts != 1 {
		t.Fatalf("unexpected stage result: %+v puts=%d", result, puts)
	}
	retry := func(data []byte) *httptest.ResponseRecorder {
		var retryBody bytes.Buffer
		retryForm := multipart.NewWriter(&retryBody)
		retryPart, err := retryForm.CreateFormFile("files", "synthetic.txt")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = retryPart.Write(data)
		_ = retryForm.Close()
		retryReq := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path+"?stage_id=0123456789abcdef"), &retryBody)
		retryReq.Header.Set("Content-Type", retryForm.FormDataContentType())
		result := httptest.NewRecorder()
		h.ServeHTTP(result, retryReq)
		return result
	}
	if again := retry(payload); again.Code != http.StatusOK || puts != 2 {
		t.Fatalf("same stage retry status=%d PUTs=%d", again.Code, puts)
	}
	if collision := retry([]byte("different synthetic bytes")); collision.Code != http.StatusConflict || puts != 2 {
		t.Fatalf("different stage reused identity: status=%d PUTs=%d", collision.Code, puts)
	}
	// A local budget refusal is known before any relay PUT and leaves the
	// already staged file available for a later explicit retry.
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":1,"storage_min_free_bytes":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	var budgetBody bytes.Buffer
	budgetForm := multipart.NewWriter(&budgetBody)
	budgetPart, err := budgetForm.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = budgetPart.Write(payload)
	_ = budgetForm.Close()
	budgetReq := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path+"?stage_id=fedcba9876543210"), &budgetBody)
	budgetReq.Header.Set("Content-Type", budgetForm.FormDataContentType())
	budgetResult := httptest.NewRecorder()
	h.ServeHTTP(budgetResult, budgetReq)
	if budgetResult.Code != http.StatusInsufficientStorage || !strings.Contains(budgetResult.Body.String(), "storage_full") || puts != 2 {
		t.Fatalf("budget refusal status=%d puts=%d body=%s", budgetResult.Code, puts, budgetResult.Body.String())
	}
	if err := os.Remove(filepath.Join(dir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := team.ReadPeopleStage(dir, team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}, "fedcba9876543210"); err == nil {
		t.Fatal("budget refusal left a staged file")
	}
	t.Run("damaged stage directory", func(t *testing.T) {
		stageDir := filepath.Join(dir, team.CopiesDir, teamServerID, subOwner, subChat, subGen, "people-stages")
		backup := stageDir + "-synthetic-backup"
		if err := os.Rename(stageDir, backup); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(stageDir); _ = os.Rename(backup, stageDir) })
		if err := os.WriteFile(stageDir, []byte("synthetic damaged directory"), 0600); err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, err := form.CreateFormFile("files", "synthetic.txt")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(payload)
		_ = form.Close()
		req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path+"?stage_id=fedcba9876543210"), &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "stage_conflict") || puts != 2 {
			t.Fatalf("damaged stage admission status=%d puts=%d body=%s", rec.Code, puts, rec.Body.String())
		}
	})
	var malformedBody bytes.Buffer
	malformedForm := multipart.NewWriter(&malformedBody)
	malformedPart, err := malformedForm.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = malformedPart.Write(payload)
	_ = malformedForm.Close()
	malformedReq := httptest.NewRequest(http.MethodPost, path+"?stage_id=%ZZ", &malformedBody)
	malformedReq.Header.Set("Content-Type", malformedForm.FormDataContentType())
	malformed := httptest.NewRecorder()
	h.ServeHTTP(malformed, malformedReq)
	if malformed.Code != http.StatusBadRequest || puts != 2 {
		t.Fatalf("malformed stage identity: status=%d PUTs=%d", malformed.Code, puts)
	}
	reloaded := doTeam(h, http.MethodGet, peopleTestURL(t, dir, path+"/stage/"+result.Attachments[0].ID), "")
	if reloaded.Code != http.StatusOK || !strings.Contains(reloaded.Body.String(), result.Attachments[0].ID) || !strings.Contains(reloaded.Body.String(), "synthetic.txt") {
		t.Fatalf("stage metadata unavailable for recovery: %d %s", reloaded.Code, reloaded.Body.String())
	}
	copyID := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	stage, err := team.ReadPeopleStage(dir, copyID, result.Attachments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	pending := team.PeoplePending{Key: "0123456789abcdef", Author: installationOf(t, dir), ConfigHash: stage.ConfigHash, Body: json.RawMessage(`{"v":1}`), StageIDs: []string{stage.ID}}
	if err := team.SavePeoplePending(dir, copyID, pending); err != nil {
		t.Fatal(err)
	}
	pinned := doTeam(h, http.MethodDelete, peopleTestURL(t, dir, path+"/stage/"+stage.ID), "")
	if pinned.Code != http.StatusConflict {
		t.Fatalf("pending stage removal status=%d", pinned.Code)
	}
	if err := team.SavePeoplePublished(dir, copyID, 1, stage); err != nil {
		t.Fatal(err)
	}
	if err := team.AppendCopy(dir, copyID, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: pending.Body}}); err != nil {
		t.Fatal(err)
	}
	if err := team.ConfirmPeoplePending(dir, copyID, pending.Key, 1); err != nil {
		t.Fatal(err)
	}
	remove := doTeam(h, http.MethodDelete, peopleTestURL(t, dir, path+"/stage/"+result.Attachments[0].ID), "")
	if remove.Code != http.StatusNoContent {
		t.Fatalf("stage removal status=%d", remove.Code)
	}
}

func TestPeopleUploadHandlesStageStorageContractFailures(t *testing.T) {
	for _, mode := range []string{"persistent", "transient"} {
		t.Run(mode, func(t *testing.T) {
			data := []byte("synthetic stage bytes")
			sum := sha256.Sum256(data)
			var puts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/hello" {
					w.Header().Set("Scimux-Blob-Generation", "1")
					fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
					return
				}
				if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/") {
					puts.Add(1)
					body, _ := io.ReadAll(r.Body)
					if !bytes.Equal(body, data) {
						t.Error("upload bytes changed")
					}
					w.WriteHeader(http.StatusCreated)
					fmt.Fprintf(w, `{"id":"%s","size":%d}`, hex.EncodeToString(sum[:]), len(data))
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
			failedRead := mode == "persistent"
			h.stageRead = func(dir string, id team.CopyID, stageID string) (team.PeopleStage, error) {
				stage, err := team.ReadPeopleStage(dir, id, stageID)
				if err == nil && failedRead {
					return team.PeopleStage{}, team.ErrPeopleStage
				}
				return stage, err
			}
			h.stageSave = func(dir string, id team.CopyID, stage team.PeopleStage) error {
				if err := team.SavePeopleStage(dir, id, stage); err != nil {
					return err
				}
				return errors.New("synthetic storage contract failure")
			}
			path := subReadPath() + "/attachments?stage_id=0123456789abcdef"
			send := func() *httptest.ResponseRecorder {
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
				req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path), &body)
				req.Header.Set("Content-Type", form.FormDataContentType())
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				return rec
			}
			first := send()
			if mode == "persistent" {
				if first.Code != http.StatusConflict || !strings.Contains(first.Body.String(), "stage_conflict") || puts.Load() != 0 {
					t.Fatalf("storage failure accepted: status=%d puts=%d", first.Code, puts.Load())
				}
				stage, err := team.ReadPeopleStage(dir, id, "0123456789abcdef")
				if err != nil || stage.ID != "0123456789abcdef" || !bytes.Equal(stage.Data, data) {
					t.Fatalf("failed admission changed stage: %v", err)
				}
				second := send()
				if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), "stage_conflict") || puts.Load() != 0 {
					t.Fatalf("failed retry accepted: status=%d puts=%d", second.Code, puts.Load())
				}
				stage, err = team.ReadPeopleStage(dir, id, "0123456789abcdef")
				if err != nil || stage.ID != "0123456789abcdef" || !bytes.Equal(stage.Data, data) {
					t.Fatalf("failed retry changed stage: %v", err)
				}
				failedRead = false
				first = send()
			}
			if first.Code != http.StatusOK || puts.Load() != 1 {
				t.Fatalf("durable retry: status=%d puts=%d body=%s", first.Code, puts.Load(), first.Body.String())
			}
			stage, err := team.ReadPeopleStage(dir, id, "0123456789abcdef")
			if err != nil || stage.ID != "0123456789abcdef" || !bytes.Equal(stage.Data, data) {
				t.Fatalf("stage changed: %v", err)
			}
		})
	}
}

func TestPeopleAttachmentEmptyFileIsRejectedBeforeRelayPut(t *testing.T) {
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
			http.Error(w, "unexpected", http.StatusBadRequest)
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
	_, err := form.CreateFormFile("files", "empty.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = form.Close()
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments"
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path), &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "empty") || puts != 0 {
		t.Fatalf("empty file status=%d body=%s puts=%d", rec.Code, rec.Body.String(), puts)
	}
}

func TestPeopleStageAdmissionRefusesUnavailableLocalSubscription(t *testing.T) {
	for _, mode := range []string{"missing", "ended", "unjoined", "damaged"} {
		t.Run(mode, func(t *testing.T) {
			handler, dir := testTeamSurface(t, team.NewClient())
			h := handler.(*teamHandler)
			if mode != "unjoined" {
				seedTeam(t, dir, labEntry(teamServerID, "http://127.0.0.1:1", teamToken))
			}
			if mode != "missing" {
				sub := baseSub()
				sub.Access = "write"
				if mode == "ended" {
					sub.State = "ended"
				}
				seedSub(t, dir, sub)
			}
			if mode == "damaged" {
				if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("synthetic invalid JSON"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("unused body"))
			rec := httptest.NewRecorder()
			h.stagePeopleAttachment(rec, req, id)
			want := http.StatusConflict
			if mode == "missing" {
				want = http.StatusNotFound
			}
			if mode == "damaged" {
				want = http.StatusInternalServerError
			}
			if rec.Code != want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
			}
			if _, err := team.ReadPeopleStage(dir, id, "0123456789abcdef"); err == nil {
				t.Fatal("rejected admission created a stage")
			}
		})
	}
}

func TestPeopleCommentRetentionRefusesBadPagesAndRetiredOperations(t *testing.T) {
	for _, mode := range []string{"already retained", "retired retained", "cancelled retained", "changed access retained", "damaged local log", "relay failure", "empty page", "wrong generation", "retired copy", "retired during read", "valid page", "gap page", "two pages", "page limit", "nonprogress"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			var testHandler *teamHandler
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "retired during read" {
					testHandler.mu.Lock()
					for _, state := range testHandler.copyRemovalEpoch {
						state.epoch++
					}
					testHandler.mu.Unlock()
				}
				if mode == "relay failure" {
					http.Error(w, "synthetic failure", http.StatusServiceUnavailable)
					return
				}
				generation := subGen
				if mode == "wrong generation" {
					generation = strings.Repeat("e", 16)
				}
				entries := []any{}
				last, more := 0, false
				if mode == "retired copy" || mode == "retired during read" || mode == "valid page" || mode == "gap page" || mode == "two pages" || mode == "page limit" || mode == "nonprogress" || mode == "wrong generation" {
					seq := 1
					if mode == "page limit" {
						seq = int(calls.Load())
					}
					if mode == "gap page" || (mode == "two pages" && r.URL.Query().Get("after") == "1") {
						seq = 2
					}
					entries = append(entries, map[string]any{"seq": seq, "key": fmt.Sprintf("c/%016x", seq), "author": strings.Repeat("b", 32), "at": "2026-01-01T00:00:00.000Z", "body": map[string]any{"v": 1, "t": "comment", "text": "synthetic", "anchor": 0}})
					last = seq
					if mode == "two pages" && seq == 1 {
						last, more = 2, true
					}
					if mode == "page limit" {
						last, more = 9, seq < 9
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"generation": generation, "entries": entries, "last": last, "more": more})
			}))
			t.Cleanup(srv.Close)
			handler, dir := testTeamSurface(t, clientFor(srv))
			h := handler.(*teamHandler)
			testHandler = h
			server := labEntry(teamServerID, srv.URL, teamToken)
			seedTeam(t, dir, server)
			sub := baseSub()
			sub.Access = "write"
			seedSub(t, dir, sub)
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			if _, err := team.EnsurePeopleCopyInstance(dir, id); err != nil {
				t.Fatal(err)
			}
			if mode == "already retained" || mode == "retired retained" || mode == "cancelled retained" || mode == "changed access retained" || mode == "damaged local log" {
				record := team.MirrorRecord{Seq: 1, Key: "c/0123456789abcdef", Author: strings.Repeat("b", 32), At: "2026-01-01T00:00:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"synthetic","anchor":0}`)}
				if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{record}); err != nil {
					t.Fatal(err)
				}
				if mode == "damaged local log" {
					if err := os.WriteFile(filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "comments.jsonl"), []byte("synthetic corruption\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, "/", nil), id, "")
			defer finish()
			if mode == "retired copy" || mode == "retired retained" {
				h.mu.Lock()
				h.copyRemovalEpoch[id].epoch++
				h.mu.Unlock()
			}
			if mode == "changed access retained" {
				changed := sub
				changed.Access = "read"
				if err := team.SaveSubs(dir, []team.Subscription{changed}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "cancelled retained" {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			seq := uint64(1)
			if mode == "two pages" || mode == "nonprogress" {
				seq = 2
			}
			if mode == "page limit" {
				seq = 9
			}
			err := h.retainPeopleComment(req, id, sub, server, "", seq)
			if mode == "already retained" || mode == "valid page" || mode == "two pages" || mode == "page limit" {
				if err != nil {
					t.Fatalf("retention failed: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid or retired retention succeeded")
			}
			if mode == "already retained" || mode == "retired retained" || mode == "cancelled retained" || mode == "changed access retained" || mode == "damaged local log" || mode == "retired copy" {
				if calls.Load() != 0 {
					t.Fatal("local outcome contacted relay")
				}
			} else if mode == "page limit" && calls.Load() != 9 {
				t.Fatalf("bounded relay requests=%d", calls.Load())
			} else if (mode == "two pages" || mode == "nonprogress") && calls.Load() != 2 {
				t.Fatalf("two-page relay requests=%d", calls.Load())
			} else if mode != "two pages" && mode != "nonprogress" && mode != "page limit" && calls.Load() != 1 {
				t.Fatalf("relay requests=%d", calls.Load())
			}
			if mode == "retired copy" || mode == "retired during read" || mode == "wrong generation" || mode == "gap page" {
				_, last, readErr := team.ReadCopy(dir, id, team.CommentsLog, 0, 1, 4<<20)
				if readErr != nil || last != 0 {
					t.Fatalf("rejected page wrote history: last=%d err=%v", last, readErr)
				}
			}
			if mode == "page limit" {
				_, last, err := team.ReadCopy(dir, id, team.CommentsLog, 0, 16, 4<<20)
				if err != nil || last != 9 {
					t.Fatalf("bounded copied history: last=%d err=%v", last, err)
				}
			}
		})
	}
}

func TestPeoplePublicationAdmissionRejectsDisabledIdentityAndRelayFailure(t *testing.T) {
	for _, mode := range []string{"read only", "disabled", "relay unavailable"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, "synthetic unavailable", http.StatusServiceUnavailable)
			}))
			t.Cleanup(srv.Close)
			handler, dir := testTeamSurface(t, clientFor(srv))
			h := handler.(*teamHandler)
			server := labEntry(teamServerID, srv.URL, teamToken)
			seedTeam(t, dir, server)
			sub := baseSub()
			sub.Access = "write"
			if mode == "read only" {
				sub.Access = "read"
			}
			if mode == "disabled" {
				h.peopleEnabled = func() bool { return false }
			}
			seedSub(t, dir, sub)
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), id, "")
			defer finish()
			rec := httptest.NewRecorder()
			h.postPeopleFileComment(rec, req, id, sub, server, "synthetic", 0, "0123456789abcdef", []string{"fedcba9876543210"})
			want := "identity_unavailable"
			if mode == "relay unavailable" {
				want = "attachments_unavailable"
			}
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("admission status=%d body=%s", rec.Code, rec.Body.String())
			}
			wantCalls := int32(0)
			if mode == "relay unavailable" {
				wantCalls = 1
			}
			if calls.Load() != wantCalls {
				t.Fatalf("unexpected relay requests: %d", calls.Load())
			}
			if _, err := team.ReadPeoplePending(dir, id, "0123456789abcdef"); err == nil {
				t.Fatal("rejected publication froze a pending record")
			}
		})
	}
}

func TestPeoplePublicationDoesNotFreezeWhenInstallationIdentityIsDamaged(t *testing.T) {
	var writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			_, _ = fmt.Fprint(w, helloJSON(teamServerID, "Synthetic relay"))
			return
		}
		writes.Add(1)
		http.Error(w, "unexpected synthetic request", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	handler, dir := testTeamSurface(t, clientFor(srv))
	h := handler.(*teamHandler)
	server := labEntry(teamServerID, srv.URL, teamToken)
	seedTeam(t, dir, server)
	sub := baseSub()
	sub.Access = "write"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	data := []byte("synthetic staged bytes")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(dir, identity.InstallationFile)
	if err := os.WriteFile(identityPath, []byte("synthetic damaged identity"), 0600); err != nil {
		t.Fatal(err)
	}
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), id, "")
	defer finish()
	rec := httptest.NewRecorder()
	h.postPeopleFileComment(rec, req, id, sub, server, "synthetic", 0, "fedcba9876543210", []string{stage.ID})
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "identity_unavailable") || writes.Load() != 0 {
		t.Fatalf("damaged identity admission status=%d relay writes=%d", rec.Code, writes.Load())
	}
	if _, err := team.ReadPeoplePending(dir, id, "fedcba9876543210"); err == nil {
		t.Fatal("damaged identity froze publication")
	}
	if got, err := team.ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, data) {
		t.Fatalf("staged bytes changed: %v", err)
	}
	if got, err := os.ReadFile(identityPath); err != nil || string(got) != "synthetic damaged identity" {
		t.Fatalf("damaged identity was replaced: %v", err)
	}
}

func TestPeopleAttachmentReadRejectsUnavailableStateBeforeFileAccess(t *testing.T) {
	for _, mode := range []string{"missing subscription", "damaged subscriptions", "missing precondition"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Error(w, "unexpected synthetic request", http.StatusBadRequest)
			}))
			t.Cleanup(srv.Close)
			handler, dir := testTeamSurface(t, clientFor(srv))
			h := handler.(*teamHandler)
			server := labEntry(teamServerID, srv.URL, teamToken)
			seedTeam(t, dir, server)
			if mode != "missing subscription" {
				sub := baseSub()
				seedSub(t, dir, sub)
			}
			if mode == "damaged subscriptions" {
				if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("synthetic damage"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, "/", nil), id, "")
			defer finish()
			rec := httptest.NewRecorder()
			h.readPeopleAttachment(rec, req, id, 1, "0123456789abcdef")
			want, code := http.StatusNotFound, "not_found"
			if mode == "damaged subscriptions" {
				want, code = http.StatusInternalServerError, "store_unavailable"
			}
			if mode == "missing precondition" {
				want, code = http.StatusConflict, "copy_instance_required"
			}
			if rec.Code != want || !strings.Contains(rec.Body.String(), code) || requests.Load() != 0 {
				t.Fatalf("read admission status=%d relay requests=%d body=%s", rec.Code, requests.Load(), rec.Body.String())
			}
			if _, err := team.ReadPeoplePublished(dir, id, 1, "0123456789abcdef"); err == nil {
				t.Fatal("rejected read retained bytes")
			}
		})
	}
}

func TestPeopleCompletionRequiresCurrentFeatureAndOwnerAuthority(t *testing.T) {
	for _, mode := range []string{"disabled", "unverified owner"} {
		t.Run(mode, func(t *testing.T) {
			handler, dir := testTeamSurface(t, team.NewClient())
			h := handler.(*teamHandler)
			server := labEntry(teamServerID, "http://127.0.0.1:1", teamToken)
			seedTeam(t, dir, server)
			sub := baseSub()
			if mode == "disabled" {
				sub.Access = "read"
				h.peopleEnabled = func() bool { return false }
			} else {
				sub.Access = "owner"
			}
			seedSub(t, dir, sub)
			id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
			req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), id, "")
			defer finish()
			rec := httptest.NewRecorder()
			if handled := h.tryPeopleCompletion(rec, req, id, sub, server, "synthetic", 0, "0123456789abcdef", []string{"fedcba9876543210"}); !handled || rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "identity_unavailable") {
				t.Fatalf("%s completion: handled=%v status=%d body=%s", mode, handled, rec.Code, rec.Body.String())
			}
			if _, err := team.ReadPeopleCompletion(dir, id, "0123456789abcdef"); !errors.Is(err, team.ErrPeopleStageMissing) {
				t.Fatalf("%s created a completion: %v", mode, err)
			}
		})
	}
}

func TestPeopleCachedAttachmentDoesNotServeAfterSharingDisabled(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	server := labEntry(teamServerID, "http://127.0.0.1:1", teamToken)
	seedTeam(t, dir, server)
	sub := baseSub()
	seedSub(t, dir, sub)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	data := []byte("synthetic retained bytes")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	h.peopleEnabled = func() bool { checks++; return checks == 1 }
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, "/?copy_instance="+instance, nil), id, "")
	defer finish()
	rec := httptest.NewRecorder()
	h.readPeopleAttachment(rec, req, id, 1, stage.ID)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_active") || bytes.Contains(rec.Body.Bytes(), data) {
		t.Fatalf("disabled read status=%d body=%s", rec.Code, rec.Body.String())
	}
	retained, err := team.ReadPeoplePublished(dir, id, 1, stage.ID)
	if err != nil || !bytes.Equal(retained.Data, data) {
		t.Fatalf("disabled read changed retained bytes: %v", err)
	}
}

func TestPeopleRetainedDownloadPreservesSafeFilename(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	server := labEntry(teamServerID, "http://127.0.0.1:1", teamToken)
	seedTeam(t, dir, server)
	sub := baseSub()
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	data := []byte("synthetic retained bytes")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: `report "quoted" café.txt`, MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	req, finish := h.beginPeopleCopyOperation(httptest.NewRequest(http.MethodGet, "/?copy_instance="+instance, nil), id, "")
	defer finish()
	rec := httptest.NewRecorder()
	h.readPeopleAttachment(rec, req, id, 1, stage.ID)
	kind, params, err := mime.ParseMediaType(rec.Header().Get("Content-Disposition"))
	if rec.Code != http.StatusOK || err != nil || kind != "attachment" || params["filename"] != stage.Name || !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("download: status=%d disposition=%q err=%v", rec.Code, rec.Header().Get("Content-Disposition"), err)
	}
	if rec.Header().Get("Content-Type") != "application/octet-stream" || rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("download security headers changed")
	}
}

func TestPeopleStageReadRequiresCurrentWritableAssociation(t *testing.T) {
	for _, mode := range []string{"valid", "no subscription", "no server", "ended", "read only", "unverified owner", "missing stage", "wrong configuration", "wrong native instance", "stale copy", "damaged subscriptions"} {
		t.Run(mode, func(t *testing.T) {
			handler, dir := testTeamSurface(t, team.NewClient())
			h := handler.(*teamHandler)
			server := labEntry(teamServerID, "http://127.0.0.1:1", teamToken)
			if mode != "no server" {
				seedTeam(t, dir, server)
			}
			sub := baseSub()
			sub.Access = "write"
			if mode == "ended" {
				sub.State = "ended"
			}
			if mode == "read only" {
				sub.Access = "read"
			}
			if mode == "unverified owner" {
				sub.Access = "owner"
			}
			if mode != "no subscription" {
				seedSub(t, dir, sub)
			}
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			data := []byte("synthetic staged file")
			sum := sha256.Sum256(data)
			stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
			if mode == "wrong configuration" {
				stage.ConfigHash = strings.Repeat("f", 64)
			}
			if mode == "wrong native instance" {
				stage.OwnerInstance = "synthetic-owner"
			}
			if mode != "missing stage" {
				if err := team.SavePeopleStage(dir, id, stage); err != nil {
					t.Fatal(err)
				}
			}
			instance, err := team.EnsurePeopleCopyInstance(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "stale copy" {
				instance = strings.Repeat("f", 32)
			}
			if mode == "damaged subscriptions" {
				if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("synthetic damage"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments/stage/" + stage.ID + "?copy_instance=" + instance
			rec := doTeam(h, http.MethodGet, path, "")
			want := http.StatusOK
			switch mode {
			case "no subscription", "missing stage", "wrong configuration", "wrong native instance":
				want = http.StatusNotFound
			case "no server", "ended", "stale copy":
				want = http.StatusConflict
			case "read only", "unverified owner":
				want = http.StatusForbidden
			case "damaged subscriptions":
				want = http.StatusInternalServerError
			}
			if rec.Code != want {
				t.Fatalf("%s status=%d body=%s", mode, rec.Code, rec.Body.String())
			}
			if mode == "valid" && !strings.Contains(rec.Body.String(), "synthetic.txt") {
				t.Fatal("valid stage metadata unavailable")
			}
			if mode != "missing stage" {
				if got, err := team.ReadPeopleStage(dir, id, stage.ID); err != nil || !bytes.Equal(got.Data, data) {
					t.Fatalf("stage changed by read: %v", err)
				}
			}
			if len(h.copyRemovalEpoch) != 0 {
				t.Fatal("stage read retained operation bookkeeping")
			}
		})
	}
}

func TestPeopleStageRemovalRechecksChangedLocalStage(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	server := labEntry(teamServerID, "http://127.0.0.1:1", teamToken)
	seedTeam(t, dir, server)
	sub := baseSub()
	sub.Access = "write"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	data := []byte("original synthetic stage")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
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
	path := subReadPath() + "/attachments/stage/" + stage.ID + "?copy_instance=" + instance
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodDelete, path, "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stage removal did not reach recheck")
	}
	if err := team.DeletePeopleStage(dir, id, stage.ID); err != nil {
		t.Fatal(err)
	}
	replacement := stage
	replacement.Data = []byte("replacement synthetic stage")
	replacementHash := sha256.Sum256(replacement.Data)
	replacement.Hash = hex.EncodeToString(replacementHash[:])
	if err := team.SavePeopleStage(dir, id, replacement); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case rec := <-done:
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "stage_conflict") {
			t.Fatalf("removal status=%d body=%s", rec.Code, rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stage removal did not finish")
	}
	got, err := team.ReadPeopleStage(dir, id, stage.ID)
	if err != nil || !bytes.Equal(got.Data, replacement.Data) {
		t.Fatalf("replacement stage changed: %v", err)
	}
}

func TestPeopleConcurrentStageRemovalsKeepIndependentIdentities(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := handler.(*teamHandler)
	server := labEntry(teamServerID, "http://127.0.0.1:1", teamToken)
	seedTeam(t, dir, server)
	sub := baseSub()
	sub.Access = "write"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	data := []byte("synthetic stage")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: peopleServerFingerprint(server), Data: data}
	if err := team.SavePeopleStage(dir, id, stage); err != nil {
		t.Fatal(err)
	}
	other := stage
	other.ID = "fedcba9876543210"
	if err := team.SavePeopleStage(dir, id, other); err != nil {
		t.Fatal(err)
	}
	instance, err := team.EnsurePeopleCopyInstance(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	h.peopleEnabled = func() bool {
		if held.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return true
	}
	path := subReadPath() + "/attachments/stage/"
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodDelete, path+stage.ID+"?copy_instance="+instance, "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first removal not admitted")
	}
	second := doTeam(h, http.MethodDelete, path+other.ID+"?copy_instance="+instance, "")
	if second.Code != http.StatusNoContent {
		t.Fatalf("second removal status=%d", second.Code)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case first := <-done:
		if first.Code != http.StatusNoContent {
			t.Fatalf("first removal status=%d", first.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first removal did not finish")
	}
	for _, stageID := range []string{stage.ID, other.ID} {
		if _, err := team.ReadPeopleStage(dir, id, stageID); err == nil {
			t.Fatalf("stage %s remains", stageID)
		}
	}
	if len(h.copyRemovalEpoch) != 0 {
		t.Fatal("completed operations retained lifetime bookkeeping")
	}
}

func TestPeopleAttachmentRejectsInvalidRequestsBeforeRelayPut(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			puts.Add(1)
			w.WriteHeader(http.StatusCreated)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments"
	for _, tc := range []struct {
		name, query, field string
		data               []byte
		parts              int
		content            bool
		status             int
	}{
		{"missing copy identity", "", "files", []byte("x"), 1, true, 409},
		{"wrong field", "identity", "other", []byte("x"), 1, true, 400},
		{"missing multipart", "identity", "files", []byte("x"), 1, false, 400},
		{"empty file", "identity", "files", nil, 1, true, 400},
		{"too large", "identity", "files", bytes.Repeat([]byte{'x'}, (4<<20)+1), 1, true, 413},
		{"extra part", "identity", "files", []byte("x"), 2, true, 400},
		{"invalid stage id", "bad-stage", "files", []byte("x"), 1, true, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			for i := 0; i < tc.parts; i++ {
				part, err := form.CreateFormFile(tc.field, "synthetic.txt")
				if err != nil {
					t.Fatal(err)
				}
				if _, err = part.Write(tc.data); err != nil {
					t.Fatal(err)
				}
			}
			if err := form.Close(); err != nil {
				t.Fatal(err)
			}
			path := base
			if tc.query != "" {
				path = peopleTestURL(t, dir, path)
				if tc.query == "bad-stage" {
					path += "&stage_id=invalid"
				}
			}
			req := httptest.NewRequest(http.MethodPost, path, &body)
			if tc.content {
				req.Header.Set("Content-Type", form.FormDataContentType())
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.status, rec.Body.String())
			}
			if puts.Load() != 0 {
				t.Fatal("invalid request reached relay blob PUT")
			}
		})
	}
	t.Run("interrupted multipart body", func(t *testing.T) {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, err := form.CreateFormFile("files", "synthetic.txt")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte("synthetic interrupted payload"))
		_ = form.Close()
		data := body.Bytes()
		cut := bytes.Index(data, []byte("interrupted payload"))
		if cut < 0 {
			t.Fatal("synthetic multipart payload missing")
		}
		req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, base)+"&stage_id=0123456789abcdef", io.MultiReader(bytes.NewReader(data[:cut]), syntheticInterruptedUpload{}))
		req.Header.Set("Content-Type", form.FormDataContentType())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || puts.Load() != 0 {
			t.Fatalf("interrupted upload: status=%d relay PUTs=%d", rec.Code, puts.Load())
		}
		id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
		if _, err := team.ReadPeopleStage(dir, id, "0123456789abcdef"); err == nil {
			t.Fatal("interrupted upload left a staged file")
		}
	})
}

func TestPeopleAttachmentRejectsUnverifiedRelayBeforeBlobPut(t *testing.T) {
	for _, mode := range []string{"wrong-server", "legacy", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			var puts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/blobs/") {
					puts.Add(1)
					w.WriteHeader(http.StatusCreated)
					return
				}
				if r.URL.Path != "/v1/hello" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if mode == "unavailable" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				id := teamServerID
				if mode == "wrong-server" {
					id = teamOtherID
				}
				if mode != "legacy" {
					w.Header().Set("Scimux-Blob-Generation", "1")
				}
				_, _ = fmt.Fprint(w, helloJSON(id, "Synthetic relay"))
			}))
			t.Cleanup(srv.Close)
			h, dir := testTeamSurface(t, clientFor(srv))
			joinLab(t, dir, teamServerID, srv.URL)
			sub := baseSub()
			sub.Access = "write"
			sub.State = "live"
			seedSub(t, dir, sub)
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			part, err := form.CreateFormFile("files", "synthetic.txt")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = part.Write([]byte("synthetic"))
			_ = form.Close()
			path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments"
			req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path), &body)
			req.Header.Set("Content-Type", form.FormDataContentType())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusConflict || puts.Load() != 0 {
				t.Fatalf("unverified relay accepted: status=%d puts=%d", rec.Code, puts.Load())
			}
		})
	}
}

func TestPeopleCapabilitiesRequireVerifiedRelayAndExplicitLocalReadiness(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
		}
		script.ServeHTTP(w, r)
	}))
	defer srv.Close()
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	h.(*teamHandler).peopleCapabilitiesReady = false
	rec := doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
	if strings.Contains(rec.Body.String(), "people_attachments") {
		t.Fatal("incomplete feature was advertised")
	}
	h.(*teamHandler).peopleCapabilitiesReady = true
	rec = doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
	var response struct {
		People struct {
			Version int `json:"version"`
			MaxFile int `json:"max_file_bytes"`
		} `json:"people_attachments"`
		Servers []struct {
			ID      string `json:"id"`
			People  string `json:"people_files"`
			Binding string `json:"people_file_binding"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.People.Version != 1 || response.People.MaxFile != 4194304 || len(response.Servers) != 1 || response.Servers[0].People != "supported" || len(response.Servers[0].Binding) != 64 {
		t.Fatalf("verified capability unavailable: status=%d body=%s", rec.Code, rec.Body.String())
	}
	h.(*teamHandler).peopleEnabled = func() bool { return false }
	rec = doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
	if strings.Contains(rec.Body.String(), "people_attachments") {
		t.Fatal("disabled feature advertised attachments")
	}
	h.(*teamHandler).peopleEnabled = func() bool { return true }
	entries, err := team.Load(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("missing synthetic server")
	}
	if err := team.Remove(dir, teamServerID); err != nil {
		t.Fatal(err)
	}
	entries[0].Token = strings.Repeat("a", 64)
	if err := team.Add(dir, entries[0]); err != nil {
		t.Fatal(err)
	}
	rec = doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
	response.Servers = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Servers) != 1 || response.Servers[0].People != "unknown" || response.Servers[0].Binding != "" {
		t.Fatalf("changed configuration inherited capability: %s", rec.Body.String())
	}
}

func TestPeopleCapabilityDoesNotClaimOldRelaySupport(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	srv := httptest.NewServer(script)
	defer srv.Close()
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	h.(*teamHandler).peopleCapabilitiesReady = true
	rec := doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"people_files":"unsupported"`) {
		t.Fatalf("old relay support was not truthful: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPeopleCapabilityRejectsOlderRelayProbeAfterNewerResult(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	srv := httptest.NewServer(script)
	defer srv.Close()
	handler, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, handler)
	h := handler.(*teamHandler)
	h.peopleCapabilitiesReady = true
	entries, err := team.Load(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("missing synthetic server")
	}
	newer := time.Date(2030, 1, 1, 0, 0, 2, 0, time.UTC)
	older := newer.Add(-time.Second)
	if _, err := h.commitSubs(installationOf(t, dir), []subOutcome{{entry: entries[0], started: newer, peopleProbe: 100, delivery: "ok", peopleFiles: "supported"}}, nil, newer); err != nil {
		t.Fatal(err)
	}
	view, err := h.commitSubs(installationOf(t, dir), []subOutcome{{entry: entries[0], started: older, peopleProbe: 99, delivery: "ok", peopleFiles: "unsupported"}}, nil, newer)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Servers) != 1 || view.Servers[0].PeopleFiles != "supported" {
		t.Fatal("older probe replaced newer verified capability")
	}
}

type heldPeopleBody struct {
	reader  io.Reader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *heldPeopleBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.reader.Read(p)
}

func TestPeopleAttachmentPermissionLossDuringBodyDoesNotPut(t *testing.T) {
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
			http.Error(w, "unexpected", 400)
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
	_, _ = part.Write([]byte("synthetic bytes"))
	_ = form.Close()
	held := &heldPeopleBody{reader: bytes.NewReader(body.Bytes()), entered: make(chan struct{}), release: make(chan struct{})}
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments"
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path), held)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, req); close(done) }()
	var release sync.Once
	defer release.Do(func() { close(held.release) })
	select {
	case <-held.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("attachment body was not reached")
	}
	subs, err := team.LoadSubs(dir)
	if err != nil {
		t.Fatal(err)
	}
	subs[0].Access = "read"
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(held.release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("attachment request did not finish")
	}
	if rec.Code != http.StatusConflict || puts != 0 {
		t.Fatalf("status=%d puts=%d", rec.Code, puts)
	}
}

func TestPeopleFileOnlyPublicationRetainsLegacyFallback(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	payload := []byte("synthetic attachment")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	var posted json.RawMessage
	var author string
	var blobPuts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			blobPuts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":"%s","size":%d}`, hash, len(payload))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			if r.Method == http.MethodPost {
				var request struct {
					Entries []struct {
						Key  string          `json:"key"`
						Body json.RawMessage `json:"body"`
					} `json:"entries"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Entries) != 1 {
					t.Error("bad append")
					w.WriteHeader(400)
					return
				}
				posted = append([]byte(nil), request.Entries[0].Body...)
				_, _ = fmt.Fprintf(w, `{"generation":"%s","results":[{"key":"%s","seq":1,"duplicate":false}],"last":1}`, subGen, request.Entries[0].Key)
				return
			}
			reply := map[string]any{"generation": subGen, "entries": []any{map[string]any{"seq": 1, "key": "c/0123456789abcdef", "author": author, "at": "2026-11-14T09:30:00.000Z", "body": posted}}, "last": 1, "more": false}
			_ = json.NewEncoder(w).Encode(reply)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	author = installationOf(t, dir)
	readSubList(t, h)
	var fileBody bytes.Buffer
	form := multipart.NewWriter(&fileBody)
	part, err := form.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(payload)
	_ = form.Close()
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path+"/attachments"), &fileBody)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stage status %d", rec.Code)
	}
	var staged struct {
		Attachments []struct {
			ID string `json:"id"`
		} `json:"attachments"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &staged) != nil || len(staged.Attachments) != 1 {
		t.Fatal("missing stage")
	}
	requestBody, _ := json.Marshal(map[string]any{"text": "", "anchor": 0, "key": "0123456789abcdef", "attachments": []string{staged.Attachments[0].ID}})
	comment := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path+"/comments"), string(requestBody))
	if comment.Code != http.StatusOK {
		t.Fatalf("file-only publication status=%d body=%s", comment.Code, comment.Body.String())
	}
	if blobPuts.Load() != 2 {
		t.Fatalf("publication did not reverify staged bytes: PUTs=%d", blobPuts.Load())
	}
	var wire struct {
		Text        string            `json:"text"`
		Content     string            `json:"content_text"`
		Attachments []json.RawMessage `json:"people_attachments"`
	}
	if json.Unmarshal(posted, &wire) != nil || wire.Text != peopleAttachmentNotice || wire.Content != "" || len(wire.Attachments) != 1 {
		t.Fatalf("legacy-compatible wire missing: %s", posted)
	}
	download := doTeam(h, http.MethodGet, peopleTestURL(t, dir, path+"/attachments/1/"+staged.Attachments[0].ID), "")
	if download.Code != http.StatusOK || !bytes.Equal(download.Body.Bytes(), payload) || download.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(download.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("retained download status=%d headers=%v", download.Code, download.Header())
	}
	copyID := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if err := team.DeletePeopleStage(dir, copyID, staged.Attachments[0].ID); err != nil {
		t.Fatalf("confirmed publication still pins staging: %v", err)
	}
	retry := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path+"/comments"), string(requestBody))
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `"seq":1`) || blobPuts.Load() != 2 {
		t.Fatalf("exact completed retry failed or retransmitted: %d %s PUTs=%d", retry.Code, retry.Body.String(), blobPuts.Load())
	}
	t.Run("damaged subscription at receipt confirmation", func(t *testing.T) {
		entries, err := team.Load(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("server state: %v", err)
		}
		subs, err := team.LoadSubs(dir)
		if err != nil || len(subs) != 1 {
			t.Fatalf("subscription state: %v", err)
		}
		path := filepath.Join(dir, team.SubsFile)
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("synthetic damaged subscriptions"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(path, saved, 0600); err != nil {
				t.Error(err)
			}
		})
		th := h.(*teamHandler)
		req, finish := th.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), copyID, "")
		defer finish()
		rec := httptest.NewRecorder()
		if handled := th.tryPeopleCompletion(rec, req, copyID, subs[0], entries[0], "", 0, "0123456789abcdef", []string{staged.Attachments[0].ID}); !handled || rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "publication_conflict") {
			t.Fatalf("damaged completion: handled=%v status=%d", handled, rec.Code)
		}
		if blobPuts.Load() != 2 {
			t.Fatal("failed completion retried relay upload")
		}
	})
	t.Run("retired copy at receipt confirmation", func(t *testing.T) {
		entries, err := team.Load(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("server state: %v", err)
		}
		subs, err := team.LoadSubs(dir)
		if err != nil || len(subs) != 1 {
			t.Fatalf("subscription state: %v", err)
		}
		th := h.(*teamHandler)
		req, finish := th.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), copyID, "")
		defer finish()
		th.mu.Lock()
		th.copyRemovalEpoch[copyID].epoch++
		th.mu.Unlock()
		rec := httptest.NewRecorder()
		if handled := th.tryPeopleCompletion(rec, req, copyID, subs[0], entries[0], "", 0, "0123456789abcdef", []string{staged.Attachments[0].ID}); !handled || rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_active") {
			t.Fatalf("retired completion: handled=%v status=%d", handled, rec.Code)
		}
		if blobPuts.Load() != 2 {
			t.Fatal("retired completion retried relay upload")
		}
	})
	t.Run("damaged completion receipt", func(t *testing.T) {
		entries, err := team.Load(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("server state: %v", err)
		}
		subs, err := team.LoadSubs(dir)
		if err != nil || len(subs) != 1 {
			t.Fatalf("subscription state: %v", err)
		}
		path := filepath.Join(dir, team.CopiesDir, copyID.Server, copyID.Owner, copyID.Chat, copyID.Generation, "people-completed", "0123456789abcdef.json")
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("synthetic damaged receipt"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(path, saved, 0600); err != nil {
				t.Error(err)
			}
		})
		th := h.(*teamHandler)
		req, finish := th.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), copyID, "")
		defer finish()
		rec := httptest.NewRecorder()
		if handled := th.tryPeopleCompletion(rec, req, copyID, subs[0], entries[0], "", 0, "0123456789abcdef", []string{staged.Attachments[0].ID}); !handled || rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "publication_conflict") {
			t.Fatalf("damaged receipt: handled=%v status=%d", handled, rec.Code)
		}
		if blobPuts.Load() != 2 {
			t.Fatal("damaged receipt retried relay upload")
		}
	})
	t.Run("changed server configuration at receipt confirmation", func(t *testing.T) {
		entries, err := team.Load(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("server state: %v", err)
		}
		subs, err := team.LoadSubs(dir)
		if err != nil || len(subs) != 1 {
			t.Fatalf("subscription state: %v", err)
		}
		original := entries[0]
		changed := original
		changed.Token = strings.Repeat("f", 64)
		if changed.Token == original.Token {
			changed.Token = strings.Repeat("e", 64)
		}
		if err := team.Remove(dir, teamServerID); err != nil {
			t.Fatal(err)
		}
		if err := team.Add(dir, changed); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := team.Remove(dir, teamServerID); err != nil {
				t.Error(err)
			}
			if err := team.Add(dir, original); err != nil {
				t.Error(err)
			}
		})
		th := h.(*teamHandler)
		req, finish := th.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), copyID, "")
		defer finish()
		rec := httptest.NewRecorder()
		if handled := th.tryPeopleCompletion(rec, req, copyID, subs[0], original, "", 0, "0123456789abcdef", []string{staged.Attachments[0].ID}); !handled || rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "publication_conflict") {
			t.Fatalf("changed server completion: handled=%v status=%d", handled, rec.Code)
		}
		if blobPuts.Load() != 2 {
			t.Fatal("changed server completion retried relay upload")
		}
	})
	t.Run("changed access at receipt confirmation", func(t *testing.T) {
		entries, err := team.Load(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("server state: %v", err)
		}
		subs, err := team.LoadSubs(dir)
		if err != nil || len(subs) != 1 {
			t.Fatalf("subscription state: %v", err)
		}
		original := subs[0]
		subs[0].Access = "read"
		if err := team.SaveSubs(dir, subs); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := team.SaveSubs(dir, []team.Subscription{original}); err != nil {
				t.Error(err)
			}
		})
		th := h.(*teamHandler)
		req, finish := th.beginPeopleCopyOperation(httptest.NewRequest(http.MethodPost, "/", nil), copyID, "")
		defer finish()
		rec := httptest.NewRecorder()
		if handled := th.tryPeopleCompletion(rec, req, copyID, original, entries[0], "", 0, "0123456789abcdef", []string{staged.Attachments[0].ID}); !handled || rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "publication_conflict") {
			t.Fatalf("changed access completion: handled=%v status=%d", handled, rec.Code)
		}
		if blobPuts.Load() != 2 {
			t.Fatal("changed access completion retried relay upload")
		}
	})
	changedBody, _ := json.Marshal(map[string]any{"text": "different", "anchor": 0, "key": "0123456789abcdef", "attachments": []string{staged.Attachments[0].ID}})
	conflict := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path+"/comments"), string(changedBody))
	if conflict.Code == http.StatusOK || blobPuts.Load() != 2 {
		t.Fatal("changed completed retry was accepted")
	}
	subs, err := team.LoadSubs(dir)
	if err != nil || len(subs) != 1 {
		t.Fatal("missing synthetic subscription")
	}
	subs[0].Access = "read"
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	readRetry := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path+"/comments"), string(requestBody))
	if readRetry.Code != http.StatusOK || !strings.Contains(readRetry.Body.String(), `"seq":1`) || blobPuts.Load() != 2 {
		t.Fatalf("read-only exact receipt retry failed or uploaded: %d %s PUTs=%d", readRetry.Code, readRetry.Body.String(), blobPuts.Load())
	}
	newRequest, _ := json.Marshal(map[string]any{"text": "new", "anchor": 0, "key": "1123456789abcdef", "attachments": []string{staged.Attachments[0].ID}})
	if denied := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path+"/comments"), string(newRequest)); denied.Code != http.StatusForbidden || blobPuts.Load() != 2 {
		t.Fatalf("read-only new publication was not denied: %d PUTs=%d", denied.Code, blobPuts.Load())
	}
	subs[0].Access = "write"
	subs[0].State = "ended"
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	endedRetry := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path+"/comments"), string(requestBody))
	if endedRetry.Code != http.StatusOK || !strings.Contains(endedRetry.Body.String(), `"seq":1`) || blobPuts.Load() != 2 {
		t.Fatalf("ended-share completed retry failed: %d %s", endedRetry.Code, endedRetry.Body.String())
	}
	retainedAgain := doTeam(h, http.MethodGet, peopleTestURL(t, dir, path+"/attachments/1/"+staged.Attachments[0].ID), "")
	if retainedAgain.Code != http.StatusOK || !bytes.Equal(retainedAgain.Body.Bytes(), payload) {
		t.Fatal("retained published bytes disappeared with staging")
	}
	wrong := doTeam(h, http.MethodGet, peopleTestURL(t, dir, path+"/attachments/2/"+staged.Attachments[0].ID), "")
	if wrong.Code == http.StatusOK {
		t.Fatal("attachment ID alone authorized another sequence")
	}
	retained := filepath.Join(dir, "team-copies", teamServerID, subOwner, subChat, subGen, "people-records", "comments", "1", staged.Attachments[0].ID+".json")
	if err := os.WriteFile(retained, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := doTeam(h, http.MethodGet, peopleTestURL(t, dir, path+"/attachments/1/"+staged.Attachments[0].ID), "")
	if corrupt.Code != http.StatusInternalServerError || corrupt.Body.Len() == 0 {
		t.Fatalf("corrupt retained file status=%d", corrupt.Code)
	}
	if retry := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path+"/comments"), string(requestBody)); retry.Code == http.StatusOK {
		t.Fatal("corrupt retained publication was reported as available")
	}
}

func TestPeopleRecipientFetchRetainsAuthorizedBytesAfterShareEnds(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "read", 0))}
	payload := []byte("synthetic file")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			gets++
			if r.Method != http.MethodGet || r.URL.Query().Get("generation") != subGen {
				t.Error("unguarded fetch")
			}
			_, _ = w.Write(payload)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	attachmentID := "0123456789abcdef"
	body, _ := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": peopleAttachmentNotice, "anchor": 0, "content_text": "", "people_attachments_version": 1, "people_attachments": []any{map[string]any{"id": attachmentID, "sha256": hash, "size": len(payload), "name": "synthetic.txt", "mime": "text/plain"}}})
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/1111111111111111", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: body}}); err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments/1/" + attachmentID
	first := doTeam(h, http.MethodGet, peopleTestURL(t, dir, path), "")
	if first.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), payload) || gets != 1 {
		t.Fatalf("first fetch status=%d GETs=%d", first.Code, gets)
	}
	subs, err := team.LoadSubs(dir)
	if err != nil {
		t.Fatal(err)
	}
	subs[0].State = "ended"
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	retained := doTeam(h, http.MethodGet, peopleTestURL(t, dir, path), "")
	if retained.Code != http.StatusOK || !bytes.Equal(retained.Body.Bytes(), payload) || gets != 1 {
		t.Fatalf("retained status=%d GETs=%d", retained.Code, gets)
	}
}

func TestPeopleFetchReportsMissingCorruptAndUnsupportedBeforeRetaining(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "unsupported", "local budget"} {
		t.Run(mode, func(t *testing.T) {
			script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "read", 0))}
			payload := []byte("synthetic bytes")
			sum := sha256.Sum256(payload)
			hash := hex.EncodeToString(sum[:])
			gets := 0
			healthy := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/hello" {
					if mode != "unsupported" || healthy {
						w.Header().Set("Scimux-Blob-Generation", "1")
					}
					script.ServeHTTP(w, r)
					return
				}
				if strings.Contains(r.URL.Path, "/blobs/") {
					gets++
					if mode == "missing" && !healthy {
						http.Error(w, "not found", http.StatusNotFound)
						return
					}
					data := append([]byte(nil), payload...)
					if mode == "corrupt" && !healthy {
						data[0] ^= 1
					}
					_, _ = w.Write(data)
					return
				}
				script.ServeHTTP(w, r)
			}))
			t.Cleanup(srv.Close)
			h, dir := testTeamSurface(t, clientFor(srv))
			joinLab(t, dir, teamServerID, srv.URL)
			readSubList(t, h)
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			fileID := "0123456789abcdef"
			body, err := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": peopleAttachmentNotice, "content_text": "", "anchor": 0, "people_attachments_version": 1, "people_attachments": []peopleAttachmentDescriptor{{ID: fileID, SHA256: hash, Size: int64(len(payload)), Name: "synthetic.txt", MIME: "text/plain"}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/1111111111111111", Author: subOwner, At: "2026-01-01T00:00:00.000Z", Body: body}}); err != nil {
				t.Fatal(err)
			}
			path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments/1/" + fileID
			bound := peopleTestURL(t, dir, path)
			if mode == "local budget" {
				if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":1,"storage_min_free_bytes":0}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			first := doTeam(h, http.MethodGet, bound, "")
			want := http.StatusNotFound
			if mode == "corrupt" {
				want = http.StatusBadGateway
			}
			if mode == "unsupported" {
				want = http.StatusConflict
			}
			if mode == "local budget" {
				want = http.StatusInsufficientStorage
			}
			if first.Code != want {
				t.Fatalf("%s status=%d body=%s", mode, first.Code, first.Body.String())
			}
			if mode == "unsupported" && gets != 0 {
				t.Fatal("old relay received a blob request")
			}
			if _, err := team.ReadPeoplePublished(dir, id, 1, fileID); err == nil {
				t.Fatal("unverified bytes were retained")
			}
			if mode == "local budget" {
				if err := os.Remove(filepath.Join(dir, "settings.json")); err != nil {
					t.Fatal(err)
				}
			}
			healthy = true
			retry := doTeam(h, http.MethodGet, peopleTestURL(t, dir, path), "")
			if retry.Code != http.StatusOK || !bytes.Equal(retry.Body.Bytes(), payload) {
				t.Fatalf("explicit retry failed: status=%d", retry.Code)
			}
			stored, err := team.ReadPeoplePublished(dir, id, 1, fileID)
			if err != nil || !bytes.Equal(stored.Data, payload) {
				t.Fatalf("verified retry not retained: %v", err)
			}
		})
	}
}

func TestPeopleFetchCannotRetainAfterServerCredentialsChange(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "read", 0))}
	payload := []byte("synthetic file")
	sum := sha256.Sum256(payload)
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			close(entered)
			<-release
			_, _ = w.Write(payload)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	defer released.Do(func() { close(release) })
	handler, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, handler)
	h := handler.(*teamHandler)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	fileID := "0123456789abcdef"
	body, err := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": peopleAttachmentNotice, "content_text": "", "anchor": 0, "people_attachments_version": 1, "people_attachments": []peopleAttachmentDescriptor{{ID: fileID, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload)), Name: "synthetic.txt", MIME: "text/plain"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/1111111111111111", Author: subOwner, At: "2026-01-01T00:00:00.000Z", Body: body}}); err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments/1/" + fileID
	bound := peopleTestURL(t, dir, path)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, bound, "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic relay fetch did not start")
	}
	h.mu.Lock()
	err = team.Remove(dir, teamServerID)
	if err == nil {
		err = team.Add(dir, labEntry(teamServerID, srv.URL, strings.Repeat("f", 64)))
	}
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	released.Do(func() { close(release) })
	select {
	case result := <-done:
		if result.Code == http.StatusOK {
			t.Fatal("old credential fetch returned current file")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fetch did not finish")
	}
	if _, err := team.ReadPeoplePublished(dir, id, 1, fileID); err == nil {
		t.Fatal("old credential response retained new bytes")
	}
}

func TestPeopleFetchCannotRetainWhenSubscriptionStoreBecomesDamaged(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "read", 0))}
	payload := []byte("synthetic file")
	sum := sha256.Sum256(payload)
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			close(entered)
			<-release
			_, _ = w.Write(payload)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	defer released.Do(func() { close(release) })
	handler, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, handler)
	h := handler.(*teamHandler)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	fileID := "0123456789abcdef"
	body, err := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": peopleAttachmentNotice, "content_text": "", "anchor": 0, "people_attachments_version": 1, "people_attachments": []peopleAttachmentDescriptor{{ID: fileID, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload)), Name: "synthetic.txt", MIME: "text/plain"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/1111111111111111", Author: subOwner, At: "2026-01-01T00:00:00.000Z", Body: body}}); err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments/1/" + fileID
	bound := peopleTestURL(t, dir, path)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, bound, "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic fetch did not start")
	}
	h.mu.Lock()
	err = os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("synthetic damaged subscriptions"), 0600)
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	released.Do(func() { close(release) })
	select {
	case result := <-done:
		if result.Code == http.StatusOK {
			t.Fatal("damaged subscription admitted retained bytes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic fetch did not finish")
	}
	if _, err := team.ReadPeoplePublished(dir, id, 1, fileID); err == nil {
		t.Fatal("damaged subscription allowed retaining new bytes")
	}
}

func TestPeopleFetchPermissionLossDoesNotRetainNewBytes(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "read", 0))}
	payload := []byte("synthetic file")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			once.Do(func() { close(entered) })
			<-release
			_, _ = w.Write(payload)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	var released sync.Once
	defer released.Do(func() { close(release) })
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	attachmentID := "0123456789abcdef"
	body, _ := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": peopleAttachmentNotice, "anchor": 0, "content_text": "", "people_attachments_version": 1, "people_attachments": []any{map[string]any{"id": attachmentID, "sha256": hash, "size": len(payload), "name": "synthetic.txt", "mime": "text/plain"}}})
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/1111111111111111", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: body}}); err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments/1/" + attachmentID
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, peopleTestURL(t, dir, path), "") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("relay fetch did not start")
	}
	subs, err := team.LoadSubs(dir)
	if err != nil {
		t.Fatal(err)
	}
	subs[0].State = "ended"
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	released.Do(func() { close(release) })
	var result *httptest.ResponseRecorder
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch did not finish")
	}
	if result.Code == http.StatusOK {
		t.Fatal("fetch succeeded after permission loss")
	}
	if _, err := team.ReadPeoplePublished(dir, id, 1, attachmentID); err == nil {
		t.Fatal("new bytes retained after permission loss")
	}
}

func TestPeopleFetchCannotRetainBytesInRecreatedCopy(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "read", 0))}
	payload := []byte("synthetic file")
	sum := sha256.Sum256(payload)
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			close(entered)
			<-release
			_, _ = w.Write(payload)
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
	attachmentID := "0123456789abcdef"
	body, _ := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": peopleAttachmentNotice, "anchor": 0, "content_text": "", "people_attachments_version": 1, "people_attachments": []any{map[string]any{"id": attachmentID, "sha256": hex.EncodeToString(sum[:]), "size": len(payload), "name": "synthetic.txt", "mime": "text/plain"}}})
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/1111111111111111", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: body}}); err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doTeam(h, http.MethodGet, peopleTestURL(t, dir, base+"/attachments/1/"+attachmentID), "")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("relay fetch did not start")
	}
	if removed := doTeam(h, http.MethodDelete, base, ""); removed.Code != http.StatusNoContent {
		t.Fatalf("copy removal status %d", removed.Code)
	}
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	released.Do(func() { close(release) })
	select {
	case result := <-done:
		if result.Code == http.StatusOK {
			t.Fatal("old fetch served bytes after copy recreation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fetch did not finish")
	}
	if _, err := team.ReadPeoplePublished(dir, id, 1, attachmentID); err == nil {
		t.Fatal("old fetch retained bytes in the recreated copy")
	}
}

func TestPeopleRecipientCorruptRemoteBytesAreNotMissingOrReturned(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "read", 0))}
	expected := []byte("x")
	sum := sha256.Sum256(expected)
	hash := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			_, _ = w.Write([]byte("y"))
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	attachmentID := "0123456789abcdef"
	body, _ := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": peopleAttachmentNotice, "anchor": 0, "content_text": "", "people_attachments_version": 1, "people_attachments": []any{map[string]any{"id": attachmentID, "sha256": hash, "size": 1, "name": "synthetic.txt", "mime": "text/plain"}}})
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/1111111111111111", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: body}}); err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments/1/" + attachmentID
	rec := doTeam(h, http.MethodGet, peopleTestURL(t, dir, path), "")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "corrupt") {
		t.Fatalf("corrupt response status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := team.ReadPeoplePublished(dir, id, 1, attachmentID); err == nil {
		t.Fatal("corrupt bytes retained")
	}
}

func TestPeopleRasterPreviewRequiresVerifiedImage(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "read", 0))}
	h, dir, _ := subSurface(t, script)
	readSubList(t, h)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	makeStage := func(ident string, data []byte) team.PeopleStage {
		sum := sha256.Sum256(data)
		return team.PeopleStage{ID: ident, Hash: hex.EncodeToString(sum[:]), Name: "synthetic.png", MIME: "image/png", ConfigHash: strings.Repeat("a", 64), Data: data}
	}
	good := makeStage("0123456789abcdef", pngBytes.Bytes())
	bad := makeStage("fedcba9876543210", []byte("<html>synthetic</html>"))
	if err := team.SavePeoplePublished(dir, id, 1, good); err != nil {
		t.Fatal(err)
	}
	if err := team.SavePeoplePublished(dir, id, 1, bad); err != nil {
		t.Fatal(err)
	}
	base := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments/1/"
	preview := doTeam(h, http.MethodGet, peopleTestURL(t, dir, base+good.ID+"?preview=1"), "")
	if preview.Code != http.StatusOK || preview.Header().Get("Content-Type") != "image/png" || preview.Header().Get("Content-Disposition") != "" || preview.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("valid preview status=%d headers=%v", preview.Code, preview.Header())
	}
	refused := doTeam(h, http.MethodGet, peopleTestURL(t, dir, base+bad.ID+"?preview=1"), "")
	if refused.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("active content preview status=%d", refused.Code)
	}
}

func TestPeopleRasterPreviewAcceptsJPEGAndRefusesTruncatedRaster(t *testing.T) {
	imageData := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	var jpegBytes, pngBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, imageData, nil); err != nil {
		t.Fatal(err)
	}
	if media, ok := peopleRasterPreview(jpegBytes.Bytes()); !ok || media != "image/jpeg" {
		t.Fatalf("valid JPEG rejected: %q %v", media, ok)
	}
	if err := png.Encode(&pngBytes, imageData); err != nil {
		t.Fatal(err)
	}
	data := pngBytes.Bytes()
	var truncated []byte
	for n := 1; n < len(data); n++ {
		if _, _, err := image.DecodeConfig(bytes.NewReader(data[:n])); err != nil {
			continue
		}
		if _, _, err := image.Decode(bytes.NewReader(data[:n])); err != nil {
			truncated = data[:n]
			break
		}
	}
	if len(truncated) == 0 {
		t.Fatal("synthetic PNG did not yield a valid header with incomplete image data")
	}
	if media, ok := peopleRasterPreview(truncated); ok || media != "" {
		t.Fatal("truncated raster was exposed as a safe preview")
	}
}

func TestPeopleOwnerStageRequiresVerifiedNativeInstance(t *testing.T) {
	const instance = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	script := &subScript{}
	payload := []byte("synthetic attachment")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
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
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":"%s","size":%d}`, hash, len(payload))
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	handler, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	owner := installationOf(t, dir)
	sub := baseSub()
	sub.Owner = owner
	sub.Access = "owner"
	sub.OwnerFrontier = &team.OwnerFrontier{Node: "local", Segment: 0, Record: 0}
	writeChatLog(t, dir, "local", sub.Chat, 1)
	seedSub(t, dir, sub)
	h := asTeamHandler(t, handler)
	h.ownerInstance = func(node, got string) bool { return node == "local" && got == instance }
	post := func(query string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, err := form.CreateFormFile("files", "synthetic.txt")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(payload)
		_ = form.Close()
		path := "/api/team/subscriptions/" + teamServerID + "/" + owner + "/" + sub.Chat + "/" + sub.Generation + "/attachments" + query
		req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path), &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(""); rec.Code == http.StatusOK || puts != 0 {
		t.Fatalf("missing owner identity accepted: %d", rec.Code)
	}
	if rec := post("?chat_instance=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); rec.Code == http.StatusOK || puts != 0 {
		t.Fatalf("wrong owner identity accepted: %d", rec.Code)
	}
	if rec := post("?chat_instance=" + instance); rec.Code != http.StatusOK || puts != 1 {
		t.Fatalf("verified owner stage status=%d PUTs=%d", rec.Code, puts)
	}
}

func TestPeopleOwnerRecreatedDuringUploadCannotComplete(t *testing.T) {
	const old = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const replacement = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	script := &subScript{}
	payload := []byte("synthetic attachment")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	entered := make(chan struct{})
	release := make(chan struct{})
	var released sync.Once
	defer released.Do(func() { close(release) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			_, _ = io.Copy(io.Discard, r.Body)
			close(entered)
			<-release
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":"%s","size":%d}`, hash, len(payload))
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	handler, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	owner := installationOf(t, dir)
	sub := baseSub()
	sub.Owner = owner
	sub.Access = "owner"
	sub.OwnerFrontier = &team.OwnerFrontier{Node: "local", Segment: 0, Record: 0}
	writeChatLog(t, dir, "local", sub.Chat, 1)
	seedSub(t, dir, sub)
	var current atomic.Value
	current.Store(old)
	asTeamHandler(t, handler).ownerInstance = func(node, instance string) bool { return node == "local" && current.Load().(string) == instance }
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(payload)
	_ = form.Close()
	path := "/api/team/subscriptions/" + teamServerID + "/" + owner + "/" + sub.Chat + "/" + sub.Generation + "/attachments?chat_instance=" + old
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path), &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler.ServeHTTP(rec, req); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not start")
	}
	current.Store(replacement)
	released.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not finish")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale owner upload completion status=%d", rec.Code)
	}
}

func TestPeopleOwnerNativeUUIDInstanceCanStage(t *testing.T) {
	const instance = "aabbccdd-1122-4abc-8abc-0123456789ab"
	script := &subScript{}
	payload := []byte("synthetic attachment")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
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
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":"%s","size":%d}`, hash, len(payload))
			return
		}
		script.ServeHTTP(w, r)
	}))
	defer srv.Close()
	handler, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	owner := installationOf(t, dir)
	sub := baseSub()
	sub.Owner = owner
	sub.Access = "owner"
	sub.OwnerFrontier = &team.OwnerFrontier{Node: "local", Segment: 0, Record: 0}
	writeChatLog(t, dir, "local", sub.Chat, 1)
	seedSub(t, dir, sub)
	asTeamHandler(t, handler).ownerInstance = func(node, got string) bool { return node == "local" && got == instance }
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(payload)
	_ = form.Close()
	path := "/api/team/subscriptions/" + teamServerID + "/" + owner + "/" + sub.Chat + "/" + sub.Generation + "/attachments?chat_instance=" + instance
	req := httptest.NewRequest(http.MethodPost, peopleTestURL(t, dir, path), &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || puts != 1 {
		t.Fatalf("valid native UUID was refused: status=%d puts=%d body=%s", rec.Code, puts, rec.Body.String())
	}
}

func TestPeopleDisabledFeatureDoesNotFetchNewBytes(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "read", 0))}
	gets := 0
	payload := []byte("x")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			gets++
			_, _ = w.Write(payload)
			return
		}
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	readSubList(t, h)
	asTeamHandler(t, h).peopleEnabled = func() bool { return false }
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	attachmentID := "0123456789abcdef"
	body, _ := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": peopleAttachmentNotice, "anchor": 0, "content_text": "", "people_attachments_version": 1, "people_attachments": []any{map[string]any{"id": attachmentID, "sha256": hash, "size": 1, "name": "synthetic.txt", "mime": "text/plain"}}})
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/1111111111111111", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: body}}); err != nil {
		t.Fatal(err)
	}
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments/1/" + attachmentID
	rec := doTeam(h, http.MethodGet, peopleTestURL(t, dir, path), "")
	if rec.Code == http.StatusOK || gets != 0 {
		t.Fatalf("disabled feature status=%d GETs=%d", rec.Code, gets)
	}
}
