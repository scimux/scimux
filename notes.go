package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"codeberg.org/chrberger/scimux/internal/notestore"
)

// Notes HTTP API (phase 1b of the notes/notes feature). Thin handlers over
// internal/notestore: the server owns read/write of the note documents, while
// ui.json keeps only view state (the note-list index and fold state). See
// notes-design.md and notes-impl-plan.md.
//
// Every mutating handler serializes its read-modify-write under a.noteMu so a
// section autosave and a rename racing on the same file cannot clobber each
// other's untouched fields; the response always carries the fresh document
// (with the advanced edited_at) so the client can reconcile a stale save.

// noteSummary is the sparse list row: enough to render a note card
// (title, edit time, section count, represented lane colors) without shipping
// any section body. Keeping the list sparse is a deliberate density rule — the
// navigator is navigation, not an inspector.
type noteSummary struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Created      string   `json:"created_at"`
	Edited       string   `json:"edited_at"`
	Order        int      `json:"order"`
	SectionCount int      `json:"section_count"`
	Lanes        []string `json:"lanes"`
}

func (a *app) handleNoteList(w http.ResponseWriter, r *http.Request) {
	list, err := a.notes.List()
	if err != nil {
		http.Error(w, "list notes: "+err.Error(), 500)
		return
	}
	out := make([]noteSummary, 0, len(list))
	for _, sh := range list {
		out = append(out, summarize(sh))
	}
	writeJSON(w, map[string]any{"notes": out})
}

// summarize projects a full note to its sparse card row. Represented lane
// colors are the deduped set of embedded-reference lane colors across all
// sections, in first-seen order.
func summarize(sh notestore.Note) noteSummary {
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
	return noteSummary{
		ID:           sh.ID,
		Title:        sh.Title,
		Created:      sh.Created,
		Edited:       sh.Edited,
		Order:        sh.Order,
		SectionCount: len(sh.Sections),
		Lanes:        lanes,
	}
}

func (a *app) handleNoteGet(w http.ResponseWriter, r *http.Request) {
	sh, err := a.notes.Get(r.PathValue("id"))
	if err != nil {
		a.noteError(w, err)
		return
	}
	writeJSON(w, sh)
}

func (a *app) handleNoteCreate(w http.ResponseWriter, r *http.Request) {
	a.noteMu.Lock()
	defer a.noteMu.Unlock()
	sh, err := a.notes.Create()
	if err != nil {
		http.Error(w, "create note: "+err.Error(), 500)
		return
	}
	writeJSON(w, sh)
}

// notePatch is the mutation envelope. Every field is optional; a request
// carries only what it changes, so a body autosave (Section.Body alone) never
// disturbs the title or another section — that is the partial-update contract
// that makes concurrent rename + autosave safe.
type notePatch struct {
	Title      *string      `json:"title"`       // rename the note
	Order      *int         `json:"order"`       // reposition the note among notes
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

func (a *app) handleNotePatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body notePatch
	if err := decodeJSON(w, r, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), 400)
		return
	}
	a.noteMu.Lock()
	defer a.noteMu.Unlock()
	sh, err := a.notes.Get(id)
	if err != nil {
		a.noteError(w, err)
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
	if err := a.notes.Save(sh); err != nil {
		http.Error(w, "save note: "+err.Error(), 500)
		return
	}
	writeJSON(w, sh)
}

// applySectionEdit mutates the named section in place: delete removes it,
// otherwise the provided title/body/order fields are set. An unknown section id
// is a client error, not a silent no-op.
func applySectionEdit(sh *notestore.Note, sec *sectionEdit) error {
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

// handleNoteAddReference appends an embedded chat reference to a section (phase
// 1c). The client sends the durable source triple and the display snapshot it
// already has in hand; the reference is self-contained, so it renders and
// jumps back with no dependency on the source node still existing. The server
// mints the reference id — a client-supplied id is never trusted, so a caller
// cannot forge or collide reference identities that Find usages (phase 2) will
// key on. Default placement is the bottom of the section.
func (a *app) handleNoteAddReference(w http.ResponseWriter, r *http.Request) {
	var ref notestore.Reference
	if err := decodeJSON(w, r, &ref); err != nil {
		http.Error(w, "bad request: "+err.Error(), 400)
		return
	}
	ref.ID = "" // force a server-minted id; ignore anything the client sent
	a.noteMu.Lock()
	defer a.noteMu.Unlock()
	sh, err := a.notes.Get(r.PathValue("id"))
	if err != nil {
		a.noteError(w, err)
		return
	}
	if _, err := sh.AddReference(r.PathValue("sectionID"), ref); err != nil {
		a.noteError(w, err)
		return
	}
	if err := a.notes.Save(sh); err != nil {
		http.Error(w, "save note: "+err.Error(), 500)
		return
	}
	writeJSON(w, sh)
}

// handleNoteTrashReference removes one embedded reference from a section. It
// removes only that reference — never the source chat bubble and never the
// capture-layer sticky note (removal from capture stays explicit).
func (a *app) handleNoteTrashReference(w http.ResponseWriter, r *http.Request) {
	a.noteMu.Lock()
	defer a.noteMu.Unlock()
	sh, err := a.notes.Get(r.PathValue("id"))
	if err != nil {
		a.noteError(w, err)
		return
	}
	if !sh.RemoveReference(r.PathValue("sectionID"), r.PathValue("refID")) {
		http.Error(w, "not found", 404)
		return
	}
	if err := a.notes.Save(sh); err != nil {
		http.Error(w, "save note: "+err.Error(), 500)
		return
	}
	writeJSON(w, sh)
}

// noteError maps a store read error to the right HTTP status: a missing note
// or section is a 404, anything else a 500.
func (a *app) noteError(w http.ResponseWriter, err error) {
	if errors.Is(err, notestore.ErrNotFound) {
		http.Error(w, "not found", 404)
		return
	}
	http.Error(w, err.Error(), 500)
}

func (a *app) handleNoteDelete(w http.ResponseWriter, r *http.Request) {
	a.noteMu.Lock()
	defer a.noteMu.Unlock()
	if err := a.notes.Delete(r.PathValue("id")); err != nil {
		a.noteError(w, err)
		return
	}
	writeJSON(w, map[string]string{"ok": "deleted"})
}
