package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"codeberg.org/chrberger/scimux/internal/sheetstore"
)

// Sheets HTTP API (phase 1b of the notes/sheets feature). Thin handlers over
// internal/sheetstore: the server owns read/write of the sheet documents, while
// ui.json keeps only view state (the sheet-list index and fold state). See
// notes-design.md and notes-impl-plan.md.
//
// Every mutating handler serializes its read-modify-write under a.sheetMu so a
// section autosave and a rename racing on the same file cannot clobber each
// other's untouched fields; the response always carries the fresh document
// (with the advanced edited_at) so the client can reconcile a stale save.

// sheetSummary is the sparse list row: enough to render a sheet card
// (title, edit time, section count, represented lane colors) without shipping
// any section body. Keeping the list sparse is a deliberate density rule — the
// navigator is navigation, not an inspector.
type sheetSummary struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Created      string   `json:"created_at"`
	Edited       string   `json:"edited_at"`
	Order        int      `json:"order"`
	SectionCount int      `json:"section_count"`
	Lanes        []string `json:"lanes"`
}

func (a *app) handleSheetList(w http.ResponseWriter, r *http.Request) {
	list, err := a.sheets.List()
	if err != nil {
		http.Error(w, "list sheets: "+err.Error(), 500)
		return
	}
	out := make([]sheetSummary, 0, len(list))
	for _, sh := range list {
		out = append(out, summarize(sh))
	}
	writeJSON(w, map[string]any{"sheets": out})
}

// summarize projects a full sheet to its sparse card row. Represented lane
// colors are the deduped set of embedded-reference lane colors across all
// sections, in first-seen order.
func summarize(sh sheetstore.Sheet) sheetSummary {
	var lanes []string
	seen := map[string]bool{}
	for _, sec := range sh.Sections {
		for _, ref := range sec.References {
			c := ref.Snapshot.Lane
			if c == "" || seen[c] {
				continue
			}
			seen[c] = true
			lanes = append(lanes, c)
		}
	}
	return sheetSummary{
		ID:           sh.ID,
		Title:        sh.Title,
		Created:      sh.Created,
		Edited:       sh.Edited,
		Order:        sh.Order,
		SectionCount: len(sh.Sections),
		Lanes:        lanes,
	}
}

func (a *app) handleSheetGet(w http.ResponseWriter, r *http.Request) {
	sh, err := a.sheets.Get(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sheetstore.ErrNotFound) {
			http.Error(w, "not found", 404)
			return
		}
		http.Error(w, "read sheet: "+err.Error(), 500)
		return
	}
	writeJSON(w, sh)
}

func (a *app) handleSheetCreate(w http.ResponseWriter, r *http.Request) {
	a.sheetMu.Lock()
	defer a.sheetMu.Unlock()
	sh, err := a.sheets.Create()
	if err != nil {
		http.Error(w, "create sheet: "+err.Error(), 500)
		return
	}
	writeJSON(w, sh)
}

// sheetPatch is the mutation envelope. Every field is optional; a request
// carries only what it changes, so a body autosave (Section.Body alone) never
// disturbs the title or another section — that is the partial-update contract
// that makes concurrent rename + autosave safe.
type sheetPatch struct {
	Title      *string      `json:"title"`       // rename the sheet
	Order      *int         `json:"order"`       // reposition the sheet among sheets
	AddSection *string      `json:"add_section"` // append a section with this title ("" → "Section N")
	Section    *sectionEdit `json:"section"`
}

// sectionEdit targets one existing section by id: delete removes it, otherwise
// only the provided (non-nil) fields are changed.
type sectionEdit struct {
	ID     string  `json:"id"`
	Title  *string `json:"title"`
	Body   *string `json:"body"`
	Order  *int    `json:"order"`
	Delete bool    `json:"delete"`
}

func (a *app) handleSheetPatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body sheetPatch
	if err := decodeJSON(w, r, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), 400)
		return
	}
	a.sheetMu.Lock()
	defer a.sheetMu.Unlock()
	sh, err := a.sheets.Get(id)
	if err != nil {
		if errors.Is(err, sheetstore.ErrNotFound) {
			http.Error(w, "not found", 404)
			return
		}
		http.Error(w, "read sheet: "+err.Error(), 500)
		return
	}
	if body.Title != nil {
		sh.Title = *body.Title
	}
	if body.Order != nil {
		sh.Order = *body.Order
	}
	if body.AddSection != nil {
		title := *body.AddSection
		if title == "" {
			// Mirror the starter "Section 1" naming so an unnamed add is still
			// scannable; titles are labels, not identities, so a dup is fine.
			title = "Section " + strconv.Itoa(len(sh.Sections)+1)
		}
		sh.AddSection(title)
	}
	if body.Section != nil {
		if err := applySectionEdit(&sh, body.Section); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	}
	if err := a.sheets.Save(sh); err != nil {
		http.Error(w, "save sheet: "+err.Error(), 500)
		return
	}
	writeJSON(w, sh)
}

// applySectionEdit mutates the named section in place: delete removes it,
// otherwise the provided title/body/order fields are set. An unknown section id
// is a client error, not a silent no-op.
func applySectionEdit(sh *sheetstore.Sheet, sec *sectionEdit) error {
	idx := -1
	for i := range sh.Sections {
		if sh.Sections[i].ID == sec.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("unknown section %q", sec.ID)
	}
	if sec.Delete {
		sh.Sections = append(sh.Sections[:idx], sh.Sections[idx+1:]...)
		return nil
	}
	if sec.Title != nil {
		sh.Sections[idx].Title = *sec.Title
	}
	if sec.Body != nil {
		sh.Sections[idx].Body = *sec.Body
	}
	if sec.Order != nil {
		sh.Sections[idx].Order = *sec.Order
	}
	return nil
}

func (a *app) handleSheetDelete(w http.ResponseWriter, r *http.Request) {
	a.sheetMu.Lock()
	defer a.sheetMu.Unlock()
	if err := a.sheets.Delete(r.PathValue("id")); err != nil {
		if errors.Is(err, sheetstore.ErrNotFound) {
			http.Error(w, "not found", 404)
			return
		}
		http.Error(w, "delete sheet: "+err.Error(), 500)
		return
	}
	writeJSON(w, map[string]string{"ok": "deleted"})
}
