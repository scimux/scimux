package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIndependentSupportedTextLimitWithAttachment(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	payload := []byte("synthetic attachment")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	var posted json.RawMessage
	var author string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			w.Header().Set("Scimux-Blob-Generation", "1")
			script.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
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
	legacyBody, _ := json.Marshal(map[string]any{"text": "A" + strings.Repeat("\n", 16383), "anchor": 0, "key": "2222222222222222"})
	legacy := doTeam(h, http.MethodPost, path+"/comments", string(legacyBody))
	if legacy.Code != http.StatusOK {
		t.Fatalf("preexisting text-only path rejected reference text: status=%d body=%s", legacy.Code, legacy.Body.String())
	}
	requestBody, _ := json.Marshal(map[string]any{"text": "A" + strings.Repeat("\n", 16383), "anchor": 0, "key": "0123456789abcdef", "attachments": []string{staged.Attachments[0].ID}})
	comment := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path+"/comments"), string(requestBody))
	if comment.Code != http.StatusOK {
		t.Fatalf("file-only publication status=%d body=%s", comment.Code, comment.Body.String())
	}
}
