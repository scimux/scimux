// Package notestore owns scimux's synthesis documents ("Notes"): the
// researcher-composed surface that sits above the append-only capture layer.
// Each note lives in its own folder at <data>/notes/<id>/note.json, with its
// sections and their embedded chat references nested inside the document. The
// per-note folder is the unit of isolation (and, later, of optional git
// versioning). A deleted note's whole folder is moved to
// notes/archive/<id>.<stamp>/ rather than erased, mirroring the session-log
// store's delete-is-archive pattern so a reissued id can never collide with
// live data.
//
// This is deliberately NOT an append-only event store. A note is a mutable
// word-processor document: the current text is the truth and edit history is
// incidental, so the append-only replay/seam/dedupe machinery that is
// load-bearing for nodes.jsonl and the session log buys nothing here and would
// cost a full-body copy per autosave plus a compaction pass. Notes are
// documents, so they are stored as documents (see notes-design.md "Storage
// Model"). Crash safety comes from tmp-write + rename atomicity (tmp inside
// the note folder), the same pattern ui.json uses; the files stay stdlib-only
// and greppable (`grep -r notes/*/note.json`). There is no legacy flat-file
// read path: stray top-level notes/*.json are ignored by List.
//
// Optional git versioning (TryCommit) shells out to the git binary — never a
// Go module — and keeps a private repo at notes/<id>/.git. It is best-effort:
// git absent or failing never fails a Save; note.json remains the source of
// truth. Callers decide commit boundaries (structural edits and explicit
// client completion markers); mid-edit autosaves must not call TryCommit.
package notestore

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// Section is one editable block of a note: a title, a Markdown body, an
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

// Note is one synthesis document. Ordering (note Order and each section's
// Order) is a stored field the server persists on reorder, never derived from
// events. IDs are opaque, stable strings; UI sorting uses Order, never lexical
// id order. Display titles are labels, not identities — two notes may share a
// title.
type Note struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Created  string    `json:"created_at"`
	Edited   string    `json:"edited_at"`
	Order    int       `json:"order"`
	Sections []Section `json:"sections"`
}

// Store is a directory of note documents.
type Store struct {
	Dir string
}

// New returns a store rooted at dir (created lazily on first write).
func New(dir string) *Store { return &Store{Dir: dir} }

// ErrNotFound is returned by Get/Delete for an unknown or archived note id.
var ErrNotFound = errors.New("notestore: note not found")

// ErrInvalidID is returned when a note/section/reference id is not a
// server-minted filename-safe component. HTTP handlers map it to 400.
var ErrInvalidID = errors.New("notestore: invalid id")

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// idRe matches the complete newID grammar:
//   - normal:   YYYYMMDDTHHMMSS- + 8 lowercase hex (4 random bytes)
//   - fallback: YYYYMMDDTHHMMSS-t + 1..16 lowercase hex (UnixNano)
//
// Both forms are a single path component: no dots, slashes, or separators.
var idRe = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}-([0-9a-f]{8}|t[0-9a-f]{1,16})$`)

// ValidID reports whether id is exactly one server-minted filename-safe
// component. The store is the final filesystem boundary: every entry point
// that consumes an id calls this before any path join. Rejects empty, ".",
// "..", absolute paths, both slash styles, reserved names, leading dots, and
// anything that is not the complete newID grammar (not merely "../" stripping).
func ValidID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	// Reject any path separator (slash or backslash) regardless of host OS,
	// and reject absolute / volume-shaped inputs before regex matching.
	if strings.ContainsAny(id, `/\`) {
		return false
	}
	if filepath.IsAbs(id) {
		return false
	}
	// filepath.IsAbs misses bare Windows volume forms and leading separators
	// on Unix when mixed; also reject NUL and other control bytes.
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] == 0x7f {
			return false
		}
	}
	if strings.HasPrefix(id, ".") {
		return false
	}
	// Reserved live-tree name used by Delete's archive destination.
	if id == "archive" {
		return false
	}
	return idRe.MatchString(id)
}

// newID mints an opaque, stable, filename-safe id. A sortable time prefix aids
// human debugging of the directory; the random suffix makes two notes created
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

// noteDirChecked returns the absolute-cleaned path of notes/<id>/ after
// proving id is valid and the joined path is exactly one component under the
// store root (filepath.Rel depth check).
func (s *Store) noteDirChecked(id string) (string, error) {
	if !ValidID(id) {
		return "", ErrInvalidID
	}
	root := filepath.Clean(s.Dir)
	dir := filepath.Join(root, id)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel != id {
		return "", ErrInvalidID
	}
	// Defense in depth: rel must be a single component (no separators, no ..).
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		strings.ContainsRune(rel, filepath.Separator) {
		return "", ErrInvalidID
	}
	return dir, nil
}

// pathChecked returns notes/<id>/note.json after the same containment proof.
func (s *Store) pathChecked(id string) (string, error) {
	dir, err := s.noteDirChecked(id)
	if err != nil {
		return "", err
	}
	doc := filepath.Join(dir, "note.json")
	root := filepath.Clean(s.Dir)
	rel, err := filepath.Rel(root, doc)
	if err != nil || rel != filepath.Join(id, "note.json") {
		return "", ErrInvalidID
	}
	return doc, nil
}

// archiveDirChecked returns notes/archive/<id>.<stamp> for a validated id.
// The destination is exactly one entry directly beneath notes/archive/.
func (s *Store) archiveDirChecked(id, stamp string) (string, error) {
	if !ValidID(id) {
		return "", ErrInvalidID
	}
	// Stamp is server-minted (time format); reject separators so archive
	// cannot escape notes/archive/.
	if stamp == "" || strings.ContainsAny(stamp, `/\`) || strings.Contains(stamp, "..") {
		return "", ErrInvalidID
	}
	root := filepath.Clean(s.Dir)
	archRoot := filepath.Join(root, "archive")
	name := id + "." + stamp
	dest := filepath.Join(archRoot, name)
	rel, err := filepath.Rel(archRoot, dest)
	if err != nil || rel != name {
		return "", ErrInvalidID
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		strings.ContainsRune(rel, filepath.Separator) {
		return "", ErrInvalidID
	}
	return dest, nil
}

// Create writes a fresh note: auto title YYYY-MM-DD HH:MM, a single starter
// section titled "Section 1", and an order that appends it after existing
// notes. The file is persisted before returning.
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

// nextOrder returns one past the highest existing note Order, so a new note
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

// AddSection appends a new empty section after the note's current sections and
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

// Get reads and parses one note. A missing file is ErrNotFound; a malformed
// file is a hard error here (unlike List, a caller asking for a specific id
// wants to know the file is corrupt rather than silently get an empty note).
// Invalid ids are ErrInvalidID before any filesystem access. An embedded
// document id that differs from the requested/directory id is also
// ErrInvalidID — identity cannot redirect a subsequent Save into another dir.
func (s *Store) Get(id string) (Note, error) {
	doc, err := s.pathChecked(id)
	if err != nil {
		return Note{}, err
	}
	b, err := os.ReadFile(doc)
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
	if sh.ID != id {
		return Note{}, ErrInvalidID
	}
	return sh, nil
}

// Save rewrites the note's file atomically (tmp-write + rename inside the note
// folder), overwriting rather than appending, and stamps edited_at. Write cost
// is bounded by this one note's size, so autosave frequency never grows the
// store. Rejects an invalid embedded id before creating directories or temps.
func (s *Store) Save(sh Note) error {
	dir, err := s.noteDirChecked(sh.ID)
	if err != nil {
		return err
	}
	doc, err := s.pathChecked(sh.ID)
	if err != nil {
		return err
	}
	// Ensure the per-note folder exists; the data dir itself is created with it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sh.Edited = nowStamp()
	// Indented but still a single greppable JSON object per file; a note is a
	// document, not a JSONL stream, so readability wins over one-line-per-record.
	b, err := json.MarshalIndent(sh, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// Tmp lives inside the note folder so rename stays same-directory atomic.
	tmp := doc + ".tmp"
	// 0600: section bodies and captured pane snapshots are as sensitive as the
	// session log's prompt text.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, doc); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Delete moves the whole note folder into notes/archive/<id>.<stamp>/.
// Nothing is erased; a reissued id can never append onto dead content because
// the live folder is gone. Missing note is ErrNotFound. Invalid ids are
// ErrInvalidID and never touch the archive tree.
func (s *Store) Delete(id string) error {
	src, err := s.noteDirChecked(id)
	if err != nil {
		return err
	}
	doc, err := s.pathChecked(id)
	if err != nil {
		return err
	}
	// Require the note document (not just an empty dir) so a stray folder is
	// not treated as a live note.
	if _, err := os.Stat(doc); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	archRoot := filepath.Join(filepath.Clean(s.Dir), "archive")
	if err := os.MkdirAll(archRoot, 0o700); err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	dest, err := s.archiveDirChecked(id, stamp)
	if err != nil {
		return err
	}
	return os.Rename(src, dest)
}

// List returns every parseable note under notes/<id>/note.json, sorted by the
// stored Order field (ties broken by id for a stable order). Defensive like
// the rest of the corpus readers: non-directories (including stray top-level
// *.json — no legacy flat-file read), the archive subdir, invalid directory
// names, missing/malformed note.json, embedded-id mismatches, and tmp files
// are skipped, never a hard error — a corrupt file must not 500 the whole list.
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
		if !e.IsDir() {
			continue // ignores stray top-level files (legacy *.json, garbage)
		}
		name := e.Name()
		if name == "archive" || strings.HasPrefix(name, ".") {
			continue
		}
		if !ValidID(name) {
			continue // invalid directory name: never join into a path read
		}
		doc, err := s.pathChecked(name)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(doc)
		if err != nil {
			continue // missing note.json, permission, etc.
		}
		var sh Note
		if json.Unmarshal(b, &sh) != nil || sh.ID == "" {
			continue // malformed or headerless: skip, don't fail the list
		}
		if sh.ID != name {
			continue // embedded identity mismatch: skip, never rename silently
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
