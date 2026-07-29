package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/sheetstore"
)

// patchSheet drives handleSheetPatch with the id path value set (the mux would
// normally supply it).
func patchSheet(a *app, id, bodyJSON string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PATCH", "/api/sheets/"+id, strings.NewReader(bodyJSON))
	req.SetPathValue("id", id)
	a.handleSheetPatch(rec, req)
	return rec
}

func getSheet(a *app, id string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/sheets/"+id, nil)
	req.SetPathValue("id", id)
	a.handleSheetGet(rec, req)
	return rec
}

func createSheet(t *testing.T, a *app) sheetstore.Sheet {
	t.Helper()
	rec := httptest.NewRecorder()
	a.handleSheetCreate(rec, httptest.NewRequest("POST", "/api/sheets", nil))
	if rec.Code != 200 {
		t.Fatalf("create: code = %d, body %s", rec.Code, rec.Body.String())
	}
	var sh sheetstore.Sheet
	if err := json.Unmarshal(rec.Body.Bytes(), &sh); err != nil {
		t.Fatal(err)
	}
	return sh
}

// POST creates a sheet with the starter shape; GET reads it back.
func TestSheetCreateAndGet(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createSheet(t, a)
	if sh.ID == "" || len(sh.Sections) != 1 {
		t.Fatalf("bad created sheet: %+v", sh)
	}

	rec := getSheet(a, sh.ID)
	if rec.Code != 200 {
		t.Fatalf("get: code = %d", rec.Code)
	}
	var got sheetstore.Sheet
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != sh.ID || got.Sections[0].Title != "Section 1" {
		t.Errorf("get mismatch: %+v", got)
	}

	// Unknown id is a 404, never a 500.
	if r := getSheet(a, "nope"); r.Code != 404 {
		t.Errorf("unknown id: code = %d, want 404", r.Code)
	}
}

// GET /api/sheets is a sparse list: title, edited, section count, lane colors —
// never the full section bodies.
func TestSheetListSparse(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createSheet(t, a)
	// Give the sheet a reference so a lane color is represented.
	sh.Sections[0].References = []sheetstore.Reference{{
		ID:       "r1",
		Snapshot: sheetstore.Snapshot{Lane: "#c0392b", Text: "secret body text"},
	}}
	sh.Sections[0].Body = "secret body text"
	if err := a.sheets.Save(sh); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	a.handleSheetList(rec, httptest.NewRequest("GET", "/api/sheets", nil))
	if rec.Code != 200 {
		t.Fatalf("list: code = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "secret body text") {
		t.Error("sparse list leaked section body text")
	}
	var resp struct {
		Sheets []struct {
			ID           string   `json:"id"`
			Title        string   `json:"title"`
			Edited       string   `json:"edited_at"`
			SectionCount int      `json:"section_count"`
			Lanes        []string `json:"lanes"`
		} `json:"sheets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Sheets) != 1 {
		t.Fatalf("want 1 sheet, got %d", len(resp.Sheets))
	}
	s := resp.Sheets[0]
	if s.ID != sh.ID || s.SectionCount != 1 || s.Edited == "" {
		t.Errorf("sparse fields wrong: %+v", s)
	}
	if len(s.Lanes) != 1 || s.Lanes[0] != "#c0392b" {
		t.Errorf("represented lanes = %v, want [#c0392b]", s.Lanes)
	}
}

// PATCH renames a sheet, adds a section, and edits section fields.
func TestSheetPatchTitleAndSections(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createSheet(t, a)

	if rec := patchSheet(a, sh.ID, `{"title":"Install Guide"}`); rec.Code != 200 {
		t.Fatalf("rename: code = %d, body %s", rec.Code, rec.Body.String())
	}
	// Add a section, capture its id from the returned sheet.
	rec := patchSheet(a, sh.ID, `{"add_section":"Findings"}`)
	if rec.Code != 200 {
		t.Fatalf("add_section: code = %d", rec.Code)
	}
	var afterAdd sheetstore.Sheet
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
	if rec := patchSheet(a, sh.ID, body); rec.Code != 200 {
		t.Fatalf("section edit: code = %d, body %s", rec.Code, rec.Body.String())
	}
	got := createGet(t, a, sh.ID)
	if got.Sections[1].Title != "Results" || got.Sections[1].Body != "# Results\nok" {
		t.Errorf("section edit not persisted: %+v", got.Sections[1])
	}
}

func createGet(t *testing.T, a *app, id string) sheetstore.Sheet {
	t.Helper()
	rec := getSheet(a, id)
	var sh sheetstore.Sheet
	if err := json.Unmarshal(rec.Body.Bytes(), &sh); err != nil {
		t.Fatal(err)
	}
	return sh
}

// Autosave (PATCH section body) rewrites the file atomically — the on-disk file
// stays a single valid JSON object, never a growing append log, and edited_at
// advances.
func TestSheetAutosaveOverwrites(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createSheet(t, a)
	sid := sh.Sections[0].ID

	patchSheet(a, sh.ID, `{"section":{"id":"`+sid+`","body":"one"}}`)
	afterFirst := createGet(t, a, sh.ID)

	patchSheet(a, sh.ID, `{"section":{"id":"`+sid+`","body":"two"}}`)
	afterSecond := createGet(t, a, sh.ID)

	if afterSecond.Sections[0].Body != "two" {
		t.Errorf("body = %q, want overwrite to %q", afterSecond.Sections[0].Body, "two")
	}
	if afterSecond.Edited <= afterFirst.Edited {
		t.Errorf("edited_at did not advance: %q then %q", afterFirst.Edited, afterSecond.Edited)
	}
	// The store's List parses each file as a single JSON object; a corrupt
	// (appended) file would drop out of the list entirely.
	list, _ := a.sheets.List()
	if len(list) != 1 {
		t.Fatalf("file no longer a single valid document: %d in list", len(list))
	}
}

// A section-body autosave must not clobber a concurrent title rename: PATCH is a
// partial read-modify-write over untouched fields, not a whole-document replace.
func TestSheetPatchIsPartial(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createSheet(t, a)
	sid := sh.Sections[0].ID

	// Rename the sheet, then autosave a section body. The body PATCH carries no
	// title, so the earlier rename must survive.
	patchSheet(a, sh.ID, `{"title":"Kept Title"}`)
	patchSheet(a, sh.ID, `{"section":{"id":"`+sid+`","body":"edited"}}`)

	got := createGet(t, a, sh.ID)
	if got.Title != "Kept Title" {
		t.Errorf("title clobbered by section autosave: %q", got.Title)
	}
	if got.Sections[0].Body != "edited" {
		t.Errorf("body not saved: %q", got.Sections[0].Body)
	}
}

// PATCH can reorder sheets and delete a section.
func TestSheetReorderAndSectionDelete(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	first := createSheet(t, a)
	second := createSheet(t, a)

	// Move `second` in front of `first`.
	patchSheet(a, second.ID, `{"order":-1}`)
	list, _ := a.sheets.List()
	if list[0].ID != second.ID {
		t.Errorf("reorder failed: list[0] = %q, want %q", list[0].ID, second.ID)
	}

	// Add then delete a section on `first`.
	rec := patchSheet(a, first.ID, `{"add_section":"Temp"}`)
	var withTemp sheetstore.Sheet
	json.Unmarshal(rec.Body.Bytes(), &withTemp)
	tempID := withTemp.Sections[1].ID
	patchSheet(a, first.ID, `{"section":{"id":"`+tempID+`","delete":true}}`)
	got := createGet(t, a, first.ID)
	if len(got.Sections) != 1 {
		t.Errorf("section delete failed: %d sections remain", len(got.Sections))
	}
}

// DELETE archives the sheet; it disappears from the list and GET 404s.
func TestSheetDeleteArchives(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	sh := createSheet(t, a)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/sheets/"+sh.ID, nil)
	req.SetPathValue("id", sh.ID)
	a.handleSheetDelete(rec, req)
	if rec.Code != 200 {
		t.Fatalf("delete: code = %d", rec.Code)
	}
	if r := getSheet(a, sh.ID); r.Code != 404 {
		t.Errorf("get after delete: code = %d, want 404", r.Code)
	}
	list, _ := a.sheets.List()
	if len(list) != 0 {
		t.Errorf("deleted sheet still listed: %d", len(list))
	}
	// Deleting an unknown id is a 404, not a 500.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("DELETE", "/api/sheets/nope", nil)
	req2.SetPathValue("id", "nope")
	a.handleSheetDelete(rec2, req2)
	if rec2.Code != 404 {
		t.Errorf("delete unknown: code = %d, want 404", rec2.Code)
	}
}
