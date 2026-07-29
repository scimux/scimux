// Package notestore owns scimux's synthesis documents ("Notes"): the
// researcher-composed surface that sits above the append-only capture layer.
// Each sheet is one mutable JSON file under <data>/sheets/<id>.json, with its
// sections and their embedded chat references nested inside it. A deleted sheet
// is moved to sheets/archive/ rather than erased, mirroring the session-log
// store's delete-is-archive pattern so a reissued id can never collide with
// live data.
//
// This is deliberately NOT an append-only event store. A sheet is a mutable
// word-processor document: the current text is the truth and edit history is
// incidental, so the append-only replay/seam/dedupe machinery that is
// load-bearing for nodes.jsonl and the session log buys nothing here and would
// cost a full-body copy per autosave plus a compaction pass. Notes are
// documents, so they are stored as documents (see notes-design.md "Storage
// Model"). Crash safety comes from tmp-write + rename atomicity, the same
// pattern ui.json uses; the files stay stdlib-only and greppable
// (`grep sheets/*.json`).
package notestore

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Snapshot is an embedded chat reference's display copy, frozen at reference
// time so the reference renders with no dependency on the source node still
// existing or on live capture-layer state (see notes-design.md "Embedded Chat
// References"). It is the orphan fallback when the source log is unavailable.
type Snapshot struct {
	Lane    string `json:"lane,omitempty"`
	Station string `json:"station,omitempty"`
	Speaker string `json:"speaker,omitempty"`
	Time    string `json:"time,omitempty"`
	Text    string `json:"text,omitempty"`
}

// Source is the durable address of the chat turn an embedded reference came
// from. (UID, Segment, Record) is the positional address search already trusts
// — slug-reuse-proof and stable across malformed-line skips — and resolves
// jump-back through /clear seams, rotation, and node deletion. Node and
// TurnTime are convenience hints for live rendering only, never the address of
// record.
type Source struct {
	UID      string `json:"uid,omitempty"`
	Segment  int    `json:"segment,omitempty"`
	Record   int    `json:"record,omitempty"`
	Node     string `json:"node,omitempty"`
	TurnTime string `json:"turnTime,omitempty"`
}

// Reference is one embedded chat bubble inside a section: a durable source
// address plus the self-contained display snapshot. The same bubble embedded in
// N sections is N references to one source, never a move.
type Reference struct {
	ID       string   `json:"id"`
	Source   Source   `json:"source"`
	Snapshot Snapshot `json:"snapshot"`
}

// Section is one editable block of a sheet: a title, a Markdown body, an
// explicit order position, and its embedded references. Fold/unfold is view
// state and lives in ui.json, not here.
type Section struct {
	ID         string      `json:"id"`
	Title      string      `json:"title"`
	Body       string      `json:"body"`
	Order      int         `json:"order"`
	Created    string      `json:"created_at,omitempty"`
	References []Reference `json:"references,omitempty"`
}

// Note is one synthesis document. Ordering (sheet Order and each section's
// Order) is a stored field the server persists on reorder, never derived from
// events. IDs are opaque, stable strings; UI sorting uses Order, never lexical
// id order. Display titles are labels, not identities — two sheets may share a
// title.
type Note struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Created  string    `json:"created_at"`
	Edited   string    `json:"edited_at"`
	Order    int       `json:"order"`
	Sections []Section `json:"sections"`
}

// Store is a directory of sheet documents.
type Store struct {
	Dir string
}

// New returns a store rooted at dir (created lazily on first write).
func New(dir string) *Store { return &Store{Dir: dir} }

// ErrNotFound is returned by Get/Delete for an unknown or archived sheet id.
var ErrNotFound = errors.New("notestore: sheet not found")

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// newID mints an opaque, stable, filename-safe id. A sortable time prefix aids
// human debugging of the directory; the random suffix makes two sheets created
// in the same second (or minute) distinct, since display titles are not
// identities. UI order comes from the Order field, never from this id.
func newID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// A collision-proof id must not silently degrade to a fixed value;
		// nanoseconds within this process cannot collide.
		return fmt.Sprintf("%s-t%x", time.Now().UTC().Format("20060102T150405"), time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%x", time.Now().UTC().Format("20060102T150405"), b)
}

func (s *Store) path(id string) string { return filepath.Join(s.Dir, id+".json") }

// Create writes a fresh sheet: auto title YYYY-MM-DD HH:MM, a single starter
// section titled "Section 1", and an order that appends it after existing
// sheets. The file is persisted before returning.
func (s *Store) Create() (Note, error) {
	now := nowStamp()
	sh := Note{
		ID:      newID(),
		Title:   time.Now().Format("2006-01-02 15:04"),
		Created: now,
		Edited:  now,
		Order:   s.nextOrder(),
		Sections: []Section{{
			ID:      newID(),
			Title:   "Section 1",
			Order:   0,
			Created: now,
		}},
	}
	if err := s.Save(sh); err != nil {
		return Note{}, err
	}
	return sh, nil
}

// nextOrder returns one past the highest existing sheet Order, so a new sheet
// appends to the end of the navigator. Best-effort: a listing error yields 0.
func (s *Store) nextOrder() int {
	list, err := s.List()
	if err != nil || len(list) == 0 {
		return 0
	}
	max := list[0].Order
	for _, sh := range list[1:] {
		if sh.Order > max {
			max = sh.Order
		}
	}
	return max + 1
}

// AddSection appends a new empty section after the sheet's current sections and
// returns it. Pure in-memory mutation — the caller persists with Save.
func (sh *Note) AddSection(title string) *Section {
	order := 0
	for _, sec := range sh.Sections {
		if sec.Order >= order {
			order = sec.Order + 1
		}
	}
	sh.Sections = append(sh.Sections, Section{
		ID:      newID(),
		Title:   title,
		Order:   order,
		Created: nowStamp(),
	})
	return &sh.Sections[len(sh.Sections)-1]
}

// AddReference appends ref to the named section. Default placement is the
// bottom of the section — the semantic default in the design: the researcher
// writes the claim, the embedded bubble sits under it as supporting evidence. A
// missing ref.ID is minted here so every stored reference has a stable identity
// for jump-back and (later) Find usages. Pure in-memory mutation — the caller
// persists with Save. ErrNotFound if no section matches.
func (sh *Note) AddReference(sectionID string, ref Reference) (*Reference, error) {
	for i := range sh.Sections {
		if sh.Sections[i].ID != sectionID {
			continue
		}
		if ref.ID == "" {
			ref.ID = newID()
		}
		sh.Sections[i].References = append(sh.Sections[i].References, ref)
		return &sh.Sections[i].References[len(sh.Sections[i].References)-1], nil
	}
	return nil, ErrNotFound
}

// RemoveReference trashes exactly one reference from the named section and
// reports whether it was found. It removes only that reference — never the
// source chat bubble and never the capture-layer sticky note (removal from the
// capture layer stays explicit). The same bubble embedded in N sections is N
// references; this trashes one.
func (sh *Note) RemoveReference(sectionID, refID string) bool {
	for i := range sh.Sections {
		if sh.Sections[i].ID != sectionID {
			continue
		}
		for j := range sh.Sections[i].References {
			if sh.Sections[i].References[j].ID == refID {
				sh.Sections[i].References = append(sh.Sections[i].References[:j], sh.Sections[i].References[j+1:]...)
				return true
			}
		}
	}
	return false
}

// Get reads and parses one sheet. A missing file is ErrNotFound; a malformed
// file is a hard error here (unlike List, a caller asking for a specific id
// wants to know the file is corrupt rather than silently get an empty sheet).
func (s *Store) Get(id string) (Note, error) {
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return Note{}, ErrNotFound
		}
		return Note{}, err
	}
	var sh Note
	if err := json.Unmarshal(b, &sh); err != nil {
		return Note{}, err
	}
	return sh, nil
}

// Save rewrites the sheet's file atomically (tmp-write + rename), overwriting
// rather than appending, and stamps edited_at. Write cost is bounded by this
// one sheet's size, so autosave frequency never grows the store.
func (s *Store) Save(sh Note) error {
	if sh.ID == "" {
		return errors.New("notestore: cannot save a sheet with an empty id")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	sh.Edited = nowStamp()
	// Indented but still a single greppable JSON object per file; a sheet is a
	// document, not a JSONL stream, so readability wins over one-line-per-record.
	b, err := json.MarshalIndent(sh, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := s.path(sh.ID) + ".tmp"
	// 0600: section bodies and captured pane snapshots are as sensitive as the
	// session log's prompt text.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path(sh.ID)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Delete moves the sheet file into sheets/archive/ with a timestamp suffix.
// Nothing is erased; a reissued id can never append onto dead content because
// the live file is gone. Missing sheet is ErrNotFound.
func (s *Store) Delete(id string) error {
	src := s.path(id)
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	dir := filepath.Join(s.Dir, "archive")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	return os.Rename(src, filepath.Join(dir, id+"."+stamp+".json"))
}

// List returns every parseable sheet in the directory, sorted by the stored
// Order field (ties broken by id for a stable order). Defensive like the rest
// of the corpus readers: non-.json entries, the archive subdir, tmp files, and
// malformed/unparseable files are skipped, never a hard error — a corrupt file
// must not 500 the whole list.
func (s *Store) List() ([]Note, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Note
	for _, e := range entries {
		if e.IsDir() {
			continue // skips archive/
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.Dir, name))
		if err != nil {
			continue
		}
		var sh Note
		if json.Unmarshal(b, &sh) != nil || sh.ID == "" {
			continue // malformed or headerless: skip, don't fail the list
		}
		out = append(out, sh)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
