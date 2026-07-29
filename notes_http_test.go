package main

import (
	"encoding/json"
	"net/http/httptest"
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

	// Unknown id is a 404, never a 500.
	if r := getNote(a, "nope"); r.Code != 404 {
		t.Errorf("unknown id: code = %d, want 404", r.Code)
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

	// Unknown section 404, unknown note 404.
	if rec := addReference(a, sh.ID, "nope", `{}`); rec.Code != 404 {
		t.Errorf("unknown section: code = %d, want 404", rec.Code)
	}
	if rec := addReference(a, "nope", sid, `{}`); rec.Code != 404 {
		t.Errorf("unknown note: code = %d, want 404", rec.Code)
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
	// Trashing an absent reference is a 404.
	if rec := trashReference(a, sh.ID, sid, "gone"); rec.Code != 404 {
		t.Errorf("trash absent: code = %d, want 404", rec.Code)
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
	// Deleting an unknown id is a 404, not a 500.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("DELETE", "/api/notes/nope", nil)
	req2.SetPathValue("id", "nope")
	a.handleNoteDelete(rec2, req2)
	if rec2.Code != 404 {
		t.Errorf("delete unknown: code = %d, want 404", rec2.Code)
	}
}
