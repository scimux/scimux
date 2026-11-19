package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/team"
)

// The compatibility text remains visible to an old reader; an upgraded
// reader must recover the original human text and validated descriptors.
func TestPeopleAttachmentCommentProjection(t *testing.T) {
	const notice = "[Attachments unavailable in this version of scimux.]"
	for _, tc := range []struct{ name, text string }{{"file-only", ""}, {"text-and-file", "Synthetic message"}} {
		t.Run(tc.name, func(t *testing.T) {
			fallback := notice
			if tc.text != "" {
				fallback = tc.text + "\n" + notice
			}
			body := map[string]any{"v": 1, "t": "comment", "text": fallback, "anchor": 0, "content_text": tc.text, "people_attachments_version": 1,
				"people_attachments": []any{map[string]any{"id": "0123456789abcdef", "sha256": strings.Repeat("a", 64), "size": 12, "name": "synthetic.txt", "mime": "text/plain"}}}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			var legacy struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(raw, &legacy); err != nil || !strings.Contains(legacy.Text, notice) || (tc.text != "" && !strings.Contains(legacy.Text, tc.text)) {
				t.Fatalf("legacy fallback is not truthful: %q (%v)", legacy.Text, err)
			}
			entry, _, ok := projectCommentRecord(team.MirrorRecord{Seq: 1, Author: "synthetic", Body: raw}, "another")
			if !ok {
				t.Fatal("attachment record disappeared")
			}
			if entry["text"] != tc.text {
				t.Errorf("new reader text = %#v; want %#v", entry["text"], tc.text)
			}
			attachments, ok := entry["attachments"].([]map[string]any)
			if !ok || len(attachments) != 1 {
				t.Errorf("new reader descriptors = %#v; want one validated attachment", entry["attachments"])
			}
		})
	}
}

func TestPeopleUnsupportedAttachmentVersionIsVisible(t *testing.T) {
	const fallback = "Synthetic message\n[Attachments unavailable in this version of scimux.]"
	body := []byte(`{"v":1,"t":"comment","text":"Synthetic message\n[Attachments unavailable in this version of scimux.]","anchor":0,"content_text":"Synthetic message","people_attachments_version":99,"people_attachments":[{"id":"0123456789abcdef"}]}`)
	entry, _, ok := projectCommentRecord(team.MirrorRecord{Seq: 2, Author: "synthetic", Body: body}, "another")
	if !ok || entry["text"] != fallback {
		t.Fatalf("unsupported attachment must preserve truthful fallback: %#v", entry)
	}
	if entry["attachments_status"] != "unsupported" {
		t.Errorf("unsupported descriptor status = %#v; want unsupported", entry["attachments_status"])
	}
}

func TestPeopleAttachmentMalformedManifestDoesNotAuthorize(t *testing.T) {
	cases := []struct{ name, body string }{
		{"fallback-mismatch", `{"v":1,"t":"comment","text":"Synthetic message","anchor":0,"content_text":"Synthetic message","people_attachments_version":1,"people_attachments":[{"id":"0123456789abcdef","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":12,"name":"file.txt","mime":"text/plain"}]}`},
		{"unsafe-name", `{"v":1,"t":"comment","text":"[Attachments unavailable in this version of scimux.]","anchor":0,"content_text":"","people_attachments_version":1,"people_attachments":[{"id":"0123456789abcdef","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":12,"name":"../private","mime":"text/plain"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, _, ok := projectCommentRecord(team.MirrorRecord{Seq: 3, Author: "synthetic", Body: []byte(tc.body)}, "another")
			if !ok {
				t.Fatal("legacy fallback disappeared")
			}
			if entry["attachments_status"] != "corrupt" {
				t.Errorf("status = %#v; want corrupt", entry["attachments_status"])
			}
			if _, exists := entry["attachments"]; exists {
				t.Fatal("malformed manifest authorized descriptors")
			}
		})
	}
}

func TestPeopleAttachmentRouteRejectsReadonlyBeforeBody(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "read", 0))}
	h, _, _ := subSurface(t, script)
	readSubList(t, h)
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments"
	rec := doTeam(h, http.MethodPost, path, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only attachment admission status %d; want 403", rec.Code)
	}
	for _, call := range script.snapshot() {
		if strings.Contains(call.path, "/blobs/") {
			t.Fatal("read-only attempt contacted relay blob route")
		}
	}
}

func TestPeopleAttachmentOldRelayFailsBeforeUpload(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic chat", subGen, "write", 0))}
	h, dir, _ := subSurface(t, script)
	readSubList(t, h)
	path := "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen + "/attachments"
	rec := doTeam(h, http.MethodPost, peopleTestURL(t, dir, path), "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "unsupported") {
		t.Fatalf("old relay attachment status %d body %s", rec.Code, rec.Body.String())
	}
	for _, call := range script.snapshot() {
		if strings.Contains(call.path, "/blobs/") {
			t.Fatal("old relay received a blob request")
		}
	}
}

func TestPeopleAttachmentNullContentTextIsCorrupt(t *testing.T) {
	raw := []byte(`{"v":1,"t":"comment","text":"[Attachments unavailable in this version of scimux.]","anchor":0,"content_text":null,"people_attachments_version":1,"people_attachments":[{"id":"0123456789abcdef","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":12,"name":"synthetic.txt","mime":"text/plain"}]}`)
	entry, _, ok := projectCommentRecord(team.MirrorRecord{Seq: 1, Author: "synthetic", Body: raw}, "another")
	if !ok || entry["attachments_status"] != "corrupt" {
		t.Fatalf("null content projection: %#v", entry)
	}
	if _, present := entry["attachments"]; present {
		t.Fatal("null content authorized descriptors")
	}
}

func TestPeopleAttachmentProjectionRejectsDamagedExtensions(t *testing.T) {
	validFile := `{"id":"0123456789abcdef","sha256":"` + strings.Repeat("a", 64) + `","size":1,"name":"file.txt","mime":"text/plain"}`
	for _, tc := range []struct{ name, raw string }{
		{"invalid outer JSON", `{`},
		{"missing version", `{"content_text":"","people_attachments":[]}`},
		{"invalid version", `{"content_text":"","people_attachments_version":"1","people_attachments":[]}`},
		{"missing original text", `{"people_attachments_version":1,"people_attachments":[` + validFile + `]}`},
		{"oversized original text", `{"content_text":"` + strings.Repeat("a", 16385) + `","people_attachments_version":1,"people_attachments":[` + validFile + `]}`},
		{"missing files", `{"content_text":"","people_attachments_version":1}`},
		{"empty files", `{"content_text":"","people_attachments_version":1,"people_attachments":[]}`},
		{"duplicate logical identity", `{"content_text":"","people_attachments_version":1,"people_attachments":[` + validFile + `,` + validFile + `]}`},
		{"oversized individual file", `{"content_text":"","people_attachments_version":1,"people_attachments":[` + strings.Replace(validFile, `"size":1`, `"size":4194305`, 1) + `]}`},
		{"unknown file field", `{"content_text":"","people_attachments_version":1,"people_attachments":[{"id":"0123456789abcdef","extra":true}]}`},
		{"unknown field on otherwise valid file", `{"content_text":"","people_attachments_version":1,"people_attachments":[` + strings.TrimSuffix(validFile, "}") + `,"extra":true}]}`},
		{"wrong size type on otherwise valid file", `{"content_text":"","people_attachments_version":1,"people_attachments":[` + strings.Replace(validFile, `"size":1`, `"size":"1"`, 1) + `]}`},
		{"multiple values inside file field", `{"content_text":"","people_attachments_version":1,"people_attachments":[` + validFile + `] []}`},
		{"invalid MIME", `{"content_text":"","people_attachments_version":1,"people_attachments":[` + strings.Replace(validFile, "text/plain", "bad type", 1) + `]}`},
		{"control in filename", `{"content_text":"","people_attachments_version":1,"people_attachments":[` + strings.Replace(validFile, "file.txt", `bad\u0001.txt`, 1) + `]}`},
		{"total over limit", `{"content_text":"","people_attachments_version":1,"people_attachments":[` + strings.Join([]string{
			strings.Replace(strings.Replace(validFile, `"size":1`, `"size":4194304`, 1), "0123456789abcdef", "0000000000000001", 1),
			strings.Replace(strings.Replace(validFile, `"size":1`, `"size":4194304`, 1), "0123456789abcdef", "0000000000000002", 1),
			strings.Replace(strings.Replace(validFile, `"size":1`, `"size":4194304`, 1), "0123456789abcdef", "0000000000000003", 1),
			strings.Replace(strings.Replace(validFile, `"size":1`, `"size":4194304`, 1), "0123456789abcdef", "0000000000000004", 1),
			validFile,
		}, ",") + `]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := map[string]any{"text": peopleAttachmentNotice}
			projectPeopleAttachments([]byte(tc.raw), peopleAttachmentNotice, entry)
			if entry["text"] != peopleAttachmentNotice {
				t.Fatal("legacy warning was lost")
			}
			if _, ok := entry["attachments"]; ok {
				t.Fatal("damaged extension authorized files")
			}
			if tc.name != "invalid outer JSON" && tc.name != "multiple values inside file field" && entry["attachments_status"] != "corrupt" {
				t.Fatalf("status = %#v", entry["attachments_status"])
			}
		})
	}
}

func TestPeopleCompletionMatchesExactOrderedManifest(t *testing.T) {
	ids := []string{"0123456789abcdef", "fedcba9876543210"}
	file := func(id string) map[string]any {
		return map[string]any{"id": id, "sha256": strings.Repeat("a", 64), "size": 1, "name": "synthetic.txt", "mime": "text/plain"}
	}
	makeBody := func(files []map[string]any) json.RawMessage {
		t.Helper()
		data, err := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": "caption\n" + peopleAttachmentNotice, "content_text": "caption", "anchor": uint64(9007199254740993), "people_attachments_version": 1, "people_attachments": files})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	pending := team.PeoplePending{Body: makeBody([]map[string]any{file(ids[0]), file(ids[1])}), StageIDs: ids}
	const anchor uint64 = 9007199254740993
	if !peopleCompletionMatches(pending, "caption", anchor, ids) {
		t.Fatal("exact frozen publication was not recognized")
	}
	for _, tc := range []struct {
		name   string
		body   json.RawMessage
		text   string
		anchor uint64
		ids    []string
	}{
		{"wrong text", pending.Body, "other", anchor, ids},
		{"wrong anchor", pending.Body, "caption", anchor - 1, ids},
		{"missing id", pending.Body, "caption", anchor, ids[:1]},
		{"reordered request", pending.Body, "caption", anchor, []string{ids[1], ids[0]}},
		{"reordered manifest", makeBody([]map[string]any{file(ids[1]), file(ids[0])}), "caption", anchor, ids},
		{"invalid manifest", makeBody(nil), "caption", anchor, ids},
		{"invalid JSON", json.RawMessage(`{`), "caption", anchor, ids},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := pending
			candidate.Body = tc.body
			if peopleCompletionMatches(candidate, tc.text, tc.anchor, tc.ids) {
				t.Fatal("different or invalid publication matched frozen retry")
			}
		})
	}
}

func TestPeopleFetchRequiresExactPublishedAttachmentAssociation(t *testing.T) {
	dir := t.TempDir()
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	h := &teamHandler{dataDir: dir}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	get := func(seq uint64, attachment string) error {
		t.Helper()
		_, err := h.fetchPeopleAttachment(req, id, seq, attachment, team.Entry{})
		return err
	}
	if err := get(1, "0123456789abcdef"); err == nil {
		t.Fatal("missing history authorized an attachment")
	}
	valid := map[string]any{"v": 1, "t": "comment", "text": peopleAttachmentNotice, "content_text": "", "anchor": 0, "people_attachments_version": 1, "people_attachments": []peopleAttachmentDescriptor{{ID: "0123456789abcdef", SHA256: strings.Repeat("a", 64), Size: 1, Name: "synthetic.txt", MIME: "text/plain"}}}
	for i, body := range []any{map[string]any{"v": 1, "t": "unknown", "text": "synthetic"}, map[string]any{"v": 1, "t": "comment", "text": "synthetic", "anchor": 0}, valid} {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		seq := uint64(i + 1)
		if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: seq, Key: fmt.Sprintf("c/%016x", seq), Author: strings.Repeat("b", 32), At: "2026-01-01T00:00:00.000Z", Body: raw}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		seq uint64
		id  string
	}{{1, "0123456789abcdef"}, {2, "0123456789abcdef"}, {3, "fedcba9876543210"}} {
		if err := get(tc.seq, tc.id); err == nil {
			t.Fatalf("unassociated record %d authorized bytes", tc.seq)
		}
	}
	if _, err := team.ReadPeoplePublished(dir, id, 3, "fedcba9876543210"); err == nil {
		t.Fatal("rejected foreign reference retained bytes")
	}
}

func TestPeopleCompletionRequiresMatchingHistoryAndRetainedBytes(t *testing.T) {
	dir := t.TempDir()
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	h := &teamHandler{dataDir: dir}
	data := []byte("synthetic retained file")
	sum := sha256.Sum256(data)
	stage := team.PeopleStage{ID: "0123456789abcdef", Hash: hex.EncodeToString(sum[:]), Name: "synthetic.txt", MIME: "text/plain", ConfigHash: strings.Repeat("a", 64), Data: data}
	body, err := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": peopleAttachmentNotice, "content_text": "", "anchor": 0, "people_attachments_version": 1, "people_attachments": []peopleAttachmentDescriptor{{ID: stage.ID, SHA256: stage.Hash, Size: int64(len(data)), Name: stage.Name, MIME: stage.MIME}}})
	if err != nil {
		t.Fatal(err)
	}
	pending := team.PeoplePending{Key: "fedcba9876543210", Author: strings.Repeat("b", 32), Body: body, StageIDs: []string{stage.ID}}
	completed := team.PeopleCompletion{Pending: pending, Seq: 1}
	if h.peopleCompletionRetained(id, completed) {
		t.Fatal("missing history claimed retained delivery")
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: 1, Key: "c/" + pending.Key, Author: pending.Author, At: "2026-01-01T00:00:00.000Z", Body: body}}); err != nil {
		t.Fatal(err)
	}
	if h.peopleCompletionRetained(id, completed) {
		t.Fatal("missing file claimed retained delivery")
	}
	if err := team.SavePeoplePublished(dir, id, 1, stage); err != nil {
		t.Fatal(err)
	}
	if !h.peopleCompletionRetained(id, completed) {
		t.Fatal("matching history and verified retained bytes were rejected")
	}
	wrong := completed
	wrong.Pending.Author = strings.Repeat("c", 32)
	if h.peopleCompletionRetained(id, wrong) {
		t.Fatal("different author confirmed original receipt")
	}
	wrong = completed
	wrong.Pending.Body = json.RawMessage(`{"v":1,"t":"comment","text":"different"}`)
	if h.peopleCompletionRetained(id, wrong) {
		t.Fatal("different body confirmed original receipt")
	}
	wrong = completed
	wrong.Pending.StageIDs = []string{}
	if h.peopleCompletionRetained(id, wrong) {
		t.Fatal("different manifest size confirmed original receipt")
	}
	for i, raw := range []json.RawMessage{json.RawMessage(`[]`), json.RawMessage(`{"v":1,"t":"comment","text":"synthetic","anchor":0}`)} {
		seq := uint64(i + 2)
		next := pending
		next.Key = fmt.Sprintf("%016x", seq)
		next.Body = raw
		if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{{Seq: seq, Key: "c/" + next.Key, Author: next.Author, At: "2026-01-01T00:00:00.000Z", Body: raw}}); err != nil {
			t.Fatal(err)
		}
		if h.peopleCompletionRetained(id, team.PeopleCompletion{Pending: next, Seq: seq}) {
			t.Fatal("malformed or text-only history confirmed a file receipt")
		}
	}
}
