package app

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

// noteUsage is the reverse index row needed by chat bubble markers. It omits
// note bodies and frozen snapshots: those are fetched only after the user
// chooses a destination.
type noteUsage struct {
	NoteID       string           `json:"note_id"`
	NoteTitle    string           `json:"note_title"`
	SectionID    string           `json:"section_id"`
	SectionTitle string           `json:"section_title"`
	ReferenceID  string           `json:"reference_id"`
	Source       notestore.Source `json:"source"`
}

func (a *app) handleNoteList(w http.ResponseWriter, r *http.Request) {
	list, err := a.notes.List()
	if err != nil {
		http.Error(w, "list notes: "+err.Error(), 500)
		return
	}
	if r.URL.Query().Get("usages") == "1" {
		writeJSON(w, map[string]any{"usages": projectNoteUsages(list)})
		return
	}
	out := make([]noteSummary, 0, len(list))
	for _, sh := range list {
		out = append(out, summarize(sh))
	}
	writeJSON(w, map[string]any{"notes": out})
}

func projectNoteUsages(list []notestore.Note) []noteUsage {
	out := []noteUsage{}
	seen := map[string]bool{}
	for _, sh := range list {
		for _, sec := range sh.Sections {
			for _, ref := range sec.References {
				key := sh.ID + "\x00" + sec.ID + "\x00" + noteSourceKey(ref)
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, noteUsage{
					NoteID: sh.ID, NoteTitle: sh.Title,
					SectionID: sec.ID, SectionTitle: sec.Title,
					ReferenceID: ref.ID, Source: ref.Source,
				})
			}
		}
	}
	return out
}

func noteSourceKey(ref notestore.Reference) string {
	if ref.Source.UID != "" {
		return fmt.Sprintf("u\x00%s\x00%d\x00%d", ref.Source.UID, ref.Source.Segment, ref.Source.Record)
	}
	return "n\x00" + ref.Source.Node + "\x00" + ref.Source.TurnTime + "\x00" + ref.Snapshot.Text
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
	id := r.PathValue("id")
	if !notestore.ValidID(id) {
		http.Error(w, "invalid note id", 400)
		return
	}
	sh, err := a.notes.Get(id)
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
// only the provided (non-nil) fields are changed. Commit is the client signal
// that a body (or other non-structural) edit has completed — mid-edit
// debounced autosaves omit it so optional git versioning does not snapshot
// every keystroke; the server commits only when Commit is true or the patch
// is structural (see notePatchWantsCommit).
type sectionEdit struct {
	ID     string  `json:"id"`
	Title  *string `json:"title"`
	Body   *string `json:"body"`
	Order  *int    `json:"order"`
	Delete bool    `json:"delete"`
	Commit bool    `json:"commit"`
}

func (a *app) handleNotePatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !notestore.ValidID(id) {
		http.Error(w, "invalid note id", 400)
		return
	}
	var body notePatch
	if err := decodeJSON(w, r, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), 400)
		return
	}
	// Section id is a route/body identity: reject malformed before mutation or git.
	if body.Section != nil && body.Section.ID != "" && !notestore.ValidID(body.Section.ID) {
		http.Error(w, "invalid section id", 400)
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
		a.noteError(w, err)
		return
	}
	// Optional per-note git: best-effort after a successful Save. Never fails
	// the HTTP write — note.json is the source of truth.
	if notePatchWantsCommit(body) {
		a.notes.TryCommit(id, notePatchCommitMessage(body))
	}
	writeJSON(w, sh)
}

// notePatchWantsCommit reports whether this mutation is a versioning boundary.
// Structural changes commit immediately; body-only edits commit only when the
// client sets section.commit (edit completion). Mid-edit autosaves omit it.
func notePatchWantsCommit(p notePatch) bool {
	if p.Title != nil || p.Order != nil || p.AddSection != nil {
		return true
	}
	if p.Section == nil {
		return false
	}
	sec := p.Section
	if sec.Delete || sec.Order != nil || sec.Commit {
		return true
	}
	return false
}

func notePatchCommitMessage(p notePatch) string {
	switch {
	case p.AddSection != nil:
		return "add section"
	case p.Title != nil:
		return "rename note"
	case p.Order != nil:
		return "reorder note"
	case p.Section != nil && p.Section.Delete:
		return "delete section"
	case p.Section != nil && p.Section.Order != nil:
		return "reorder section"
	case p.Section != nil && p.Section.Commit:
		return "edit section"
	default:
		return "update"
	}
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
	id := r.PathValue("id")
	sectionID := r.PathValue("sectionID")
	if !notestore.ValidID(id) {
		http.Error(w, "invalid note id", 400)
		return
	}
	if !notestore.ValidID(sectionID) {
		http.Error(w, "invalid section id", 400)
		return
	}
	var ref notestore.Reference
	if err := decodeJSON(w, r, &ref); err != nil {
		http.Error(w, "bad request: "+err.Error(), 400)
		return
	}
	ref.ID = "" // force a server-minted id; ignore anything the client sent
	a.noteMu.Lock()
	defer a.noteMu.Unlock()
	sh, err := a.notes.Get(id)
	if err != nil {
		a.noteError(w, err)
		return
	}
	if _, err := sh.AddReference(sectionID, ref); err != nil {
		a.noteError(w, err)
		return
	}
	if err := a.notes.Save(sh); err != nil {
		a.noteError(w, err)
		return
	}
	// Reference add is structural: one commit per successful save.
	a.notes.TryCommit(id, "add reference")
	writeJSON(w, sh)
}

// handleNoteTrashReference removes one embedded reference from a section. It
// removes only that reference — never the source chat bubble and never the
// capture-layer sticky note (removal from capture stays explicit).
func (a *app) handleNoteTrashReference(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sectionID := r.PathValue("sectionID")
	refID := r.PathValue("refID")
	if !notestore.ValidID(id) {
		http.Error(w, "invalid note id", 400)
		return
	}
	if !notestore.ValidID(sectionID) {
		http.Error(w, "invalid section id", 400)
		return
	}
	if !notestore.ValidID(refID) {
		http.Error(w, "invalid reference id", 400)
		return
	}
	a.noteMu.Lock()
	defer a.noteMu.Unlock()
	sh, err := a.notes.Get(id)
	if err != nil {
		a.noteError(w, err)
		return
	}
	if !sh.RemoveReference(sectionID, refID) {
		http.Error(w, "not found", 404)
		return
	}
	if err := a.notes.Save(sh); err != nil {
		a.noteError(w, err)
		return
	}
	// Reference remove is structural: one commit per successful save.
	a.notes.TryCommit(id, "remove reference")
	writeJSON(w, sh)
}

// noteError maps a store error to the right HTTP status:
//   - ErrInvalidID → 400 (malformed note/section/reference identity)
//   - ErrNotFound  → 404 (well-formed but unknown)
//   - anything else → 500
//
// Path values that never reach a handler (ServeMux non-match) are 404 at the
// router; cleaned literal ".." segments may 301 before the handler. Documented
// in docs/http-api.md.
func (a *app) noteError(w http.ResponseWriter, err error) {
	if errors.Is(err, notestore.ErrInvalidID) {
		http.Error(w, "invalid id", 400)
		return
	}
	if errors.Is(err, notestore.ErrNotFound) {
		http.Error(w, "not found", 404)
		return
	}
	http.Error(w, err.Error(), 500)
}

func (a *app) handleNoteDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !notestore.ValidID(id) {
		http.Error(w, "invalid note id", 400)
		return
	}
	a.noteMu.Lock()
	defer a.noteMu.Unlock()
	if err := a.notes.Delete(id); err != nil {
		a.noteError(w, err)
		return
	}
	writeJSON(w, map[string]string{"ok": "deleted"})
}
