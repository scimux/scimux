package app

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/notestore"
)

// patchNote drives handleNotePatch with the id path value set (the mux would
// normally supply it).
func patchNote(a *app, id, bodyJSON string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PATCH", "/api/notes/"+id, strings.NewReader(bodyJSON))
	req.SetPathValue("id", id)
	a.handleNotePatch(rec, req)
	return rec
}

func getNote(a *app, id string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/notes/"+id, nil)
	req.SetPathValue("id", id)
	a.handleNoteGet(rec, req)
	return rec
}

func createNote(t *testing.T, a *app) notestore.Note {
	t.Helper()
	rec := httptest.NewRecorder()
	a.handleNoteCreate(rec, httptest.NewRequest("POST", "/api/notes", nil))
	if rec.Code != 200 {
		t.Fatalf("create: code = %d, body %s", rec.Code, rec.Body.String())
	}
	var sh notestore.Note
	if err := json.Unmarshal(rec.Body.Bytes(), &sh); err != nil {
		t.Fatal(err)
	}
	return sh
}

// POST creates a note with the starter shape; GET reads it back.
func TestNoteCreateAndGet(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createNote(t, a)
	if sh.ID == "" || len(sh.Sections) != 1 {
		t.Fatalf("bad created note: %+v", sh)
	}

	rec := getNote(a, sh.ID)
	if rec.Code != 200 {
		t.Fatalf("get: code = %d", rec.Code)
	}
	var got notestore.Note
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != sh.ID || got.Sections[0].Title != "Section 1" {
		t.Errorf("get mismatch: %+v", got)
	}

	// Well-formed but unknown id is a 404; malformed is 400 (see confinement tests).
	if r := getNote(a, "20260101T120000-00000000"); r.Code != 404 {
		t.Errorf("unknown id: code = %d, want 404", r.Code)
	}
	if r := getNote(a, "nope"); r.Code != 400 {
		t.Errorf("malformed id: code = %d, want 400", r.Code)
	}
}

// GET /api/notes is a sparse list: title, edited, section count, lane colors —
// never the full section bodies.
func TestNoteListSparse(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createNote(t, a)
	// Give the note a reference so a lane color is represented.
	sh.Sections[0].References = []notestore.Reference{{
		ID:       "r1",
		Snapshot: notestore.Snapshot{Lane: "#c0392b", Text: "secret body text"},
	}}
	sh.Sections[0].Body = "secret body text"
	if err := a.notes.Save(sh); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	a.handleNoteList(rec, httptest.NewRequest("GET", "/api/notes", nil))
	if rec.Code != 200 {
		t.Fatalf("list: code = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "secret body text") {
		t.Error("sparse list leaked section body text")
	}
	var resp struct {
		Notes []struct {
			ID           string   `json:"id"`
			Title        string   `json:"title"`
			Edited       string   `json:"edited_at"`
			SectionCount int      `json:"section_count"`
			Lanes        []string `json:"lanes"`
		} `json:"notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Notes) != 1 {
		t.Fatalf("want 1 note, got %d", len(resp.Notes))
	}
	s := resp.Notes[0]
	if s.ID != sh.ID || s.SectionCount != 1 || s.Edited == "" {
		t.Errorf("sparse fields wrong: %+v", s)
	}
	if len(s.Lanes) != 1 || s.Lanes[0] != "#c0392b" {
		t.Errorf("represented lanes = %v, want [#c0392b]", s.Lanes)
	}
}

// PATCH renames a note, adds a section, and edits section fields.
func TestNotePatchTitleAndSections(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createNote(t, a)

	if rec := patchNote(a, sh.ID, `{"title":"Install Guide"}`); rec.Code != 200 {
		t.Fatalf("rename: code = %d, body %s", rec.Code, rec.Body.String())
	}
	// Add a section, capture its id from the returned note.
	rec := patchNote(a, sh.ID, `{"add_section":"Findings"}`)
	if rec.Code != 200 {
		t.Fatalf("add_section: code = %d", rec.Code)
	}
	var afterAdd notestore.Note
	json.Unmarshal(rec.Body.Bytes(), &afterAdd)
	if afterAdd.Title != "Install Guide" || len(afterAdd.Sections) != 2 {
		t.Fatalf("after add: %+v", afterAdd)
	}
	newSec := afterAdd.Sections[1]
	if newSec.Title != "Findings" {
		t.Errorf("added section title = %q", newSec.Title)
	}

	// Edit the new section's body + title.
	body := `{"section":{"id":"` + newSec.ID + `","title":"Results","body":"# Results\nok"}}`
	if rec := patchNote(a, sh.ID, body); rec.Code != 200 {
		t.Fatalf("section edit: code = %d, body %s", rec.Code, rec.Body.String())
	}
	got := createGet(t, a, sh.ID)
	if got.Sections[1].Title != "Results" || got.Sections[1].Body != "# Results\nok" {
		t.Errorf("section edit not persisted: %+v", got.Sections[1])
	}
}

func createGet(t *testing.T, a *app, id string) notestore.Note {
	t.Helper()
	rec := getNote(a, id)
	var sh notestore.Note
	if err := json.Unmarshal(rec.Body.Bytes(), &sh); err != nil {
		t.Fatal(err)
	}
	return sh
}

// Autosave (PATCH section body) rewrites the file atomically — the on-disk file
// stays a single valid JSON object, never a growing append log, and edited_at
// advances.
func TestNoteAutosaveOverwrites(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createNote(t, a)
	sid := sh.Sections[0].ID

	patchNote(a, sh.ID, `{"section":{"id":"`+sid+`","body":"one"}}`)
	afterFirst := createGet(t, a, sh.ID)

	patchNote(a, sh.ID, `{"section":{"id":"`+sid+`","body":"two"}}`)
	afterSecond := createGet(t, a, sh.ID)

	if afterSecond.Sections[0].Body != "two" {
		t.Errorf("body = %q, want overwrite to %q", afterSecond.Sections[0].Body, "two")
	}
	if afterSecond.Edited <= afterFirst.Edited {
		t.Errorf("edited_at did not advance: %q then %q", afterFirst.Edited, afterSecond.Edited)
	}
	// The store's List parses each file as a single JSON object; a corrupt
	// (appended) file would drop out of the list entirely.
	list, _ := a.notes.List()
	if len(list) != 1 {
		t.Fatalf("file no longer a single valid document: %d in list", len(list))
	}
}

// A section-body autosave must not clobber a concurrent title rename: PATCH is a
// partial read-modify-write over untouched fields, not a whole-document replace.
func TestNotePatchIsPartial(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createNote(t, a)
	sid := sh.Sections[0].ID

	// Rename the note, then autosave a section body. The body PATCH carries no
	// title, so the earlier rename must survive.
	patchNote(a, sh.ID, `{"title":"Kept Title"}`)
	patchNote(a, sh.ID, `{"section":{"id":"`+sid+`","body":"edited"}}`)

	got := createGet(t, a, sh.ID)
	if got.Title != "Kept Title" {
		t.Errorf("title clobbered by section autosave: %q", got.Title)
	}
	if got.Sections[0].Body != "edited" {
		t.Errorf("body not saved: %q", got.Sections[0].Body)
	}
}

// PATCH can reorder notes and delete a section.
func TestNoteReorderAndSectionDelete(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	first := createNote(t, a)
	second := createNote(t, a)

	// Move `second` in front of `first`.
	patchNote(a, second.ID, `{"order":-1}`)
	list, _ := a.notes.List()
	if list[0].ID != second.ID {
		t.Errorf("reorder failed: list[0] = %q, want %q", list[0].ID, second.ID)
	}

	// Add then delete a section on `first`.
	rec := patchNote(a, first.ID, `{"add_section":"Temp"}`)
	var withTemp notestore.Note
	json.Unmarshal(rec.Body.Bytes(), &withTemp)
	tempID := withTemp.Sections[1].ID
	patchNote(a, first.ID, `{"section":{"id":"`+tempID+`","delete":true}}`)
	got := createGet(t, a, first.ID)
	if len(got.Sections) != 1 {
		t.Errorf("section delete failed: %d sections remain", len(got.Sections))
	}
}

func addReference(a *app, noteID, sectionID, bodyJSON string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/notes/"+noteID+"/sections/"+sectionID+"/references", strings.NewReader(bodyJSON))
	req.SetPathValue("id", noteID)
	req.SetPathValue("sectionID", sectionID)
	a.handleNoteAddReference(rec, req)
	return rec
}

func trashReference(a *app, noteID, sectionID, refID string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/notes/"+noteID+"/sections/"+sectionID+"/references/"+refID, nil)
	req.SetPathValue("id", noteID)
	req.SetPathValue("sectionID", sectionID)
	req.SetPathValue("refID", refID)
	a.handleNoteTrashReference(rec, req)
	return rec
}

// Posting a capture into (note, section) stores a self-contained reference:
// the durable triple in source, the display copy in snapshot, with a
// server-minted id (a client-sent id is never trusted).
func TestNoteAddReference(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createNote(t, a)
	sid := sh.Sections[0].ID

	body := `{"id":"client-forged","source":{"uid":"u1","segment":2,"record":7,"node":"lane-a","turnTime":"2026-07-28T10:00:00Z"},` +
		`"snapshot":{"lane":"#c0392b","station":"lane-a","speaker":"assistant","time":"2026-07-28T10:00:00Z","text":"evidence"}}`
	rec := addReference(a, sh.ID, sid, body)
	if rec.Code != 200 {
		t.Fatalf("add reference: code = %d, body %s", rec.Code, rec.Body.String())
	}
	var got notestore.Note
	json.Unmarshal(rec.Body.Bytes(), &got)
	refs := got.Sections[0].References
	if len(refs) != 1 {
		t.Fatalf("want 1 reference, got %d", len(refs))
	}
	r := refs[0]
	if r.ID == "" || r.ID == "client-forged" {
		t.Errorf("reference id must be server-minted, got %q", r.ID)
	}
	if r.Source.UID != "u1" || r.Source.Segment != 2 || r.Source.Record != 7 {
		t.Errorf("source triple did not persist: %+v", r.Source)
	}
	if r.Snapshot.Text != "evidence" || r.Snapshot.Lane != "#c0392b" {
		t.Errorf("snapshot did not persist: %+v", r.Snapshot)
	}

	// Well-formed unknown section/note are 404; malformed ids are 400.
	if rec := addReference(a, sh.ID, "20260101T120000-00000000", `{}`); rec.Code != 404 {
		t.Errorf("unknown section: code = %d, want 404", rec.Code)
	}
	if rec := addReference(a, "20260101T120000-00000000", sid, `{}`); rec.Code != 404 {
		t.Errorf("unknown note: code = %d, want 404", rec.Code)
	}
	if rec := addReference(a, sh.ID, "nope", `{}`); rec.Code != 400 {
		t.Errorf("malformed section id: code = %d, want 400", rec.Code)
	}
}

// The same bubble added to two sections is two references to one source.
func TestNoteReferenceMultipleUsages(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createNote(t, a)
	secA := sh.Sections[0].ID
	rec := patchNote(a, sh.ID, `{"add_section":"Two"}`)
	var withTwo notestore.Note
	json.Unmarshal(rec.Body.Bytes(), &withTwo)
	secB := withTwo.Sections[1].ID

	src := `{"source":{"uid":"u1","record":3},"snapshot":{"text":"shared"}}`
	addReference(a, sh.ID, secA, src)
	addReference(a, sh.ID, secB, src)

	got := createGet(t, a, sh.ID)
	if len(got.Sections[0].References) != 1 || len(got.Sections[1].References) != 1 {
		t.Fatalf("want one reference in each section")
	}
	a1, b1 := got.Sections[0].References[0], got.Sections[1].References[0]
	if a1.ID == b1.ID {
		t.Error("two usages must have distinct reference ids")
	}
	if a1.Source.UID != b1.Source.UID {
		t.Error("both references must point at the same source")
	}
}

// Trashing a reference removes only that reference from the section.
func TestNoteTrashReference(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createNote(t, a)
	sid := sh.Sections[0].ID
	addReference(a, sh.ID, sid, `{"source":{"uid":"keep"}}`)
	rec := addReference(a, sh.ID, sid, `{"source":{"uid":"drop"}}`)
	var withTwo notestore.Note
	json.Unmarshal(rec.Body.Bytes(), &withTwo)
	dropID := withTwo.Sections[0].References[1].ID

	if rec := trashReference(a, sh.ID, sid, dropID); rec.Code != 200 {
		t.Fatalf("trash: code = %d", rec.Code)
	}
	got := createGet(t, a, sh.ID)
	if len(got.Sections[0].References) != 1 || got.Sections[0].References[0].Source.UID != "keep" {
		t.Errorf("wrong reference trashed: %+v", got.Sections[0].References)
	}
	// Trashing a well-formed absent reference is a 404; malformed is 400.
	if rec := trashReference(a, sh.ID, sid, "20260101T120000-00000000"); rec.Code != 404 {
		t.Errorf("trash absent: code = %d, want 404", rec.Code)
	}
	if rec := trashReference(a, sh.ID, sid, "gone"); rec.Code != 400 {
		t.Errorf("trash malformed ref id: code = %d, want 400", rec.Code)
	}
}

// Deleting the source node must not rewrite or remove the note reference: the
// reference is self-contained (triple + snapshot survive intact).
func TestReferenceSurvivesNodeDelete(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	a.nodes = []*Node{{ID: "n1", Title: "n1", Agent: "claude"}}
	a.byID["n1"] = a.nodes[0]

	sh := createNote(t, a)
	sid := sh.Sections[0].ID
	addReference(a, sh.ID, sid, `{"source":{"uid":"u-n1","record":5,"node":"n1"},"snapshot":{"text":"kept evidence","speaker":"assistant"}}`)

	del := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/nodes/n1", nil)
	r.SetPathValue("id", "n1")
	a.handleDeleteNode(del, r)
	if del.Code != 200 {
		t.Fatalf("node delete: code = %d", del.Code)
	}

	got := createGet(t, a, sh.ID)
	refs := got.Sections[0].References
	if len(refs) != 1 {
		t.Fatalf("note reference lost on node delete: %d remain", len(refs))
	}
	if refs[0].Source.UID != "u-n1" || refs[0].Snapshot.Text != "kept evidence" {
		t.Errorf("reference rewritten by node delete: %+v", refs[0])
	}
}

// DELETE archives the note; it disappears from the list and GET 404s.
func TestNoteDeleteArchives(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createNote(t, a)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/notes/"+sh.ID, nil)
	req.SetPathValue("id", sh.ID)
	a.handleNoteDelete(rec, req)
	if rec.Code != 200 {
		t.Fatalf("delete: code = %d", rec.Code)
	}
	if r := getNote(a, sh.ID); r.Code != 404 {
		t.Errorf("get after delete: code = %d, want 404", r.Code)
	}
	list, _ := a.notes.List()
	if len(list) != 0 {
		t.Errorf("deleted note still listed: %d", len(list))
	}
	// Deleting a well-formed unknown id is a 404; malformed is 400.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("DELETE", "/api/notes/20260101T120000-00000000", nil)
	req2.SetPathValue("id", "20260101T120000-00000000")
	a.handleNoteDelete(rec2, req2)
	if rec2.Code != 404 {
		t.Errorf("delete unknown: code = %d, want 404", rec2.Code)
	}
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("DELETE", "/api/notes/nope", nil)
	req3.SetPathValue("id", "nope")
	a.handleNoteDelete(rec3, req3)
	if rec3.Code != 400 {
		t.Errorf("delete malformed: code = %d, want 400", rec3.Code)
	}
}

// --- P3: public-handler note path confinement ---

// noteSentinelTree plants markers that traversal must never read, write,
// archive, or git-init. Returns paths for later assertions.
func noteSentinelTree(t *testing.T, a *app) (secretFile, archiveNote, outsideFile string) {
	t.Helper()
	root := a.notes.Dir
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	secretFile = filepath.Join(root, "secret-marker.txt")
	if err := os.WriteFile(secretFile, []byte("SECRET-LIVE"), 0o600); err != nil {
		t.Fatal(err)
	}
	archDir := filepath.Join(root, "archive", "secret-target")
	if err := os.MkdirAll(archDir, 0o700); err != nil {
		t.Fatal(err)
	}
	archiveNote = filepath.Join(archDir, "note.json")
	if err := os.WriteFile(archiveNote, []byte(`{"id":"secret-target"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideFile = filepath.Join(filepath.Dir(root), "outside-sentinel.txt")
	if err := os.WriteFile(outsideFile, []byte("OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outsideFile) })
	return secretFile, archiveNote, outsideFile
}

func assertSentinelsIntact(t *testing.T, secretFile, archiveNote, outsideFile string) {
	t.Helper()
	if b, _ := os.ReadFile(secretFile); string(b) != "SECRET-LIVE" {
		t.Errorf("secret marker changed: %q", b)
	}
	if b, _ := os.ReadFile(archiveNote); string(b) != `{"id":"secret-target"}` {
		t.Errorf("archive sentinel changed: %q", b)
	}
	if b, _ := os.ReadFile(outsideFile); string(b) != "OUTSIDE" {
		t.Errorf("outside sentinel changed: %q", b)
	}
	// No git repo next to sentinels.
	for _, p := range []string{
		filepath.Join(filepath.Dir(secretFile), "archive", "secret-target", ".git"),
		filepath.Join(filepath.Dir(outsideFile), ".git"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("unexpected git at %s", p)
		}
	}
}

// Status contract (documented in docs/http-api.md):
//   - Invalid decoded route ids that reach a handler → 400
//   - Paths not owned by ServeMux → 404
//   - No traversal request is redirected into a valid mutation route
//   - Double-encoded input is decoded only once (cannot become traversal)
func TestNoteHTTPPathConfinement(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	h := newTestHandler(t, a)
	secretFile, archiveNote, outsideFile := noteSentinelTree(t, a)

	// Create a real note so valid paths still work through the public router.
	sh := createNote(t, a)
	sid := sh.Sections[0].ID

	// Encoded slash and dot-segment note IDs for each applicable route.
	// ServeMux decodes once; PathValue yields "../archive/secret" etc.
	traversalNoteIDs := []string{
		"..%2Farchive%2Fsecret-target",
		"archive%2Fsecret-target",
		"%2e%2e",
		"%2e%2e%2farchive%2Fsecret-target",
		"..%5Csecret-target",
		"%2Fetc%2Fpasswd",
	}

	type route struct {
		method string
		path   func(noteID string) string
		body   string
		csrf   bool
	}
	routes := []route{
		{"GET", func(id string) string { return "/api/notes/" + id }, "", false},
		{"PATCH", func(id string) string { return "/api/notes/" + id }, `{"title":"pwn"}`, true},
		{"DELETE", func(id string) string { return "/api/notes/" + id }, "", true},
		{"POST", func(id string) string {
			return "/api/notes/" + id + "/sections/" + sid + "/references"
		}, `{"source":{"uid":"x"},"snapshot":{"text":"y"}}`, true},
		{"DELETE", func(id string) string {
			return "/api/notes/" + id + "/sections/" + sid + "/references/20260101T120000-ffffffff"
		}, "", true},
	}

	// Documented status matrix: 400 (invalid id reached handler), 404 (unowned
	// pattern), 301 (ServeMux path clean). Never 200, never 5xx.
	assertTraversalStatus := func(t *testing.T, method, path string, rec *httptest.ResponseRecorder) {
		t.Helper()
		switch rec.Code {
		case 400, 404, 301:
			// ok
		default:
			t.Errorf("%s %s: status %d body %q, want 400/404/301 (never 200 or 5xx)",
				method, path, rec.Code, rec.Body)
		}
	}

	for _, id := range traversalNoteIDs {
		for _, rt := range routes {
			path := rt.path(id)
			rec := routeRequest(h, rt.method, path, rt.body, rt.csrf)
			assertTraversalStatus(t, rt.method, path, rec)
		}
	}

	// Section/reference identity confinement with a valid note id → 400
	// (handler reached with malformed section/ref PathValue).
	sectionTraversal := []string{
		"..%2Fbad",
		"bad%2Fid",
		"%2e%2e",
	}
	for _, sec := range sectionTraversal {
		path := "/api/notes/" + sh.ID + "/sections/" + sec + "/references"
		rec := routeRequest(h, "POST", path, `{"source":{"uid":"x"},"snapshot":{"text":"y"}}`, true)
		assertTraversalStatus(t, "POST", path, rec)
		path = "/api/notes/" + sh.ID + "/sections/" + sec + "/references/20260101T120000-aaaaaaaa"
		rec = routeRequest(h, "DELETE", path, "", true)
		assertTraversalStatus(t, "DELETE", path, rec)
	}
	refTraversal := []string{"..%2Fbad", "bad%2Fid", "%2e%2e"}
	for _, ref := range refTraversal {
		path := "/api/notes/" + sh.ID + "/sections/" + sid + "/references/" + ref
		rec := routeRequest(h, "DELETE", path, "", true)
		assertTraversalStatus(t, "DELETE", path, rec)
	}

	// Double-encoded: single decode yields literal "%2F", not a slash → 400.
	doubleEnc := "/api/notes/..%252Farchive%252Fsecret-target"
	for _, method := range []string{"GET", "DELETE"} {
		rec := routeRequest(h, method, doubleEnc, "", method != "GET")
		if rec.Code != 400 {
			t.Errorf("%s double-encoded: status %d, want 400 (body %q)", method, rec.Code, rec.Body)
		}
	}

	// Multi-segment path not owned by ServeMux → 404 (not rewritten to mutation).
	rec := routeRequest(h, "GET", "/api/notes/foo/bar", "", false)
	if rec.Code != 404 {
		t.Errorf("unowned path: status %d, want 404", rec.Code)
	}
	rec = routeRequest(h, "PATCH", "/api/notes/foo/bar", `{"title":"x"}`, true)
	if rec.Code != 404 {
		t.Errorf("unowned PATCH: status %d, want 404", rec.Code)
	}

	// Valid generated note/section/reference ids retain existing behavior.
	rec = routeRequest(h, "GET", "/api/notes/"+sh.ID, "", false)
	if rec.Code != 200 {
		t.Fatalf("valid GET: %d %s", rec.Code, rec.Body)
	}
	rec = routeRequest(h, "PATCH", "/api/notes/"+sh.ID, `{"title":"Kept Safe"}`, true)
	if rec.Code != 200 {
		t.Fatalf("valid PATCH: %d %s", rec.Code, rec.Body)
	}
	refBody := `{"source":{"uid":"u1","segment":0,"record":1},"snapshot":{"text":"ok","lane":"#c0392b"}}`
	rec = routeRequest(h, "POST",
		"/api/notes/"+sh.ID+"/sections/"+sid+"/references", refBody, true)
	if rec.Code != 200 {
		t.Fatalf("valid add ref: %d %s", rec.Code, rec.Body)
	}
	var withRef notestore.Note
	if err := json.Unmarshal(rec.Body.Bytes(), &withRef); err != nil {
		t.Fatal(err)
	}
	if len(withRef.Sections[0].References) != 1 {
		t.Fatalf("want 1 ref, got %d", len(withRef.Sections[0].References))
	}
	refID := withRef.Sections[0].References[0].ID
	if !notestore.ValidID(refID) {
		t.Fatalf("minted ref id invalid: %q", refID)
	}
	rec = routeRequest(h, "DELETE",
		"/api/notes/"+sh.ID+"/sections/"+sid+"/references/"+refID, "", true)
	if rec.Code != 200 {
		t.Fatalf("valid trash ref: %d %s", rec.Code, rec.Body)
	}

	assertSentinelsIntact(t, secretFile, archiveNote, outsideFile)

	// Archive dir still has only the original secret-target (no traversal archive).
	entries, err := os.ReadDir(filepath.Join(a.notes.Dir, "archive"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "secret-target" {
		t.Errorf("archive entries = %v, want only secret-target", entries)
	}

	// Live note still present and titled.
	got := createGet(t, a, sh.ID)
	if got.Title != "Kept Safe" {
		t.Errorf("title = %q", got.Title)
	}
}

// Documented status matrix for malformed vs unknown note identities.
func TestNoteIDStatusContract(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	h := newTestHandler(t, a)

	// Malformed decoded ids → 400 when the handler is reached.
	for _, id := range []string{"..", "archive", "foo/bar", ".hidden", "not-minted"} {
		// Use SetPathValue path via direct helper for exact decoded values,
		// and also the public router where encoding applies.
		if rec := getNote(a, id); rec.Code != 400 {
			t.Errorf("getNote(%q): %d, want 400", id, rec.Code)
		}
		if rec := patchNote(a, id, `{"title":"x"}`); rec.Code != 400 {
			t.Errorf("patchNote(%q): %d, want 400", id, rec.Code)
		}
	}
	// Well-formed missing → 404.
	missing := "20260101T120000-00000000"
	if rec := getNote(a, missing); rec.Code != 404 {
		t.Errorf("missing get: %d", rec.Code)
	}
	// Public router: encoded traversal that reaches the handler → 400.
	rec := routeRequest(h, "GET", "/api/notes/..%2Farchive%2Fsecret", "", false)
	if rec.Code != 400 {
		t.Errorf("encoded traversal GET: %d, want 400", rec.Code)
	}
}
