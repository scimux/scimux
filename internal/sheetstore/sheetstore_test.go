package sheetstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// A fresh sheet gets an auto title, one starter section, and an opaque id.
func TestCreateStarterShape(t *testing.T) {
	s := New(t.TempDir())
	sh, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if sh.ID == "" {
		t.Fatal("sheet id is empty; ids must be opaque, stable, non-empty")
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$`).MatchString(sh.Title) {
		t.Errorf("auto title %q does not match YYYY-MM-DD HH:MM", sh.Title)
	}
	if sh.Created == "" || sh.Edited == "" {
		t.Errorf("created_at/edited_at unset: %q / %q", sh.Created, sh.Edited)
	}
	if len(sh.Sections) != 1 {
		t.Fatalf("want exactly one starter section, got %d", len(sh.Sections))
	}
	if sh.Sections[0].Title != "Section 1" {
		t.Errorf("starter section title = %q, want %q", sh.Sections[0].Title, "Section 1")
	}
	if sh.Sections[0].ID == "" {
		t.Error("starter section id is empty")
	}
}

// Create persists the file so Get round-trips it.
func TestCreateReadRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	sh, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(sh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sh.ID || got.Title != sh.Title || len(got.Sections) != 1 {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, sh)
	}
}

// List returns sheets ordered by the stored order field, never lexical id order.
func TestListOrderedByOrderField(t *testing.T) {
	s := New(t.TempDir())
	a, _ := s.Create()
	b, _ := s.Create()
	c, _ := s.Create()

	// Reorder so display order is c, a, b regardless of id/creation order.
	a.Order, b.Order, c.Order = 1, 2, 0
	for _, sh := range []Sheet{a, b, c} {
		if err := s.Save(sh); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 sheets, got %d", len(list))
	}
	want := []string{c.ID, a.ID, b.ID}
	for i, id := range want {
		if list[i].ID != id {
			t.Errorf("list[%d].ID = %q, want %q (order field must drive sort)", i, list[i].ID, id)
		}
	}
}

// Save is an atomic rewrite (tmp-write + rename), not an append, and leaves no
// stray tmp file behind; edited_at advances on each save.
func TestSaveAtomicRewrite(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	sh, _ := s.Create()

	sh.Sections[0].Body = "first body"
	if err := s.Save(sh); err != nil {
		t.Fatal(err)
	}
	sh.Sections[0].Body = "second body"
	if err := s.Save(sh); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(sh.ID)
	if got.Sections[0].Body != "second body" {
		t.Errorf("body = %q, want overwrite to %q (must rewrite, not append)", got.Sections[0].Body, "second body")
	}
	// The file must be a single valid JSON object (an append would corrupt it).
	raw, err := os.ReadFile(filepath.Join(dir, sh.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var probe Sheet
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("file is not a single JSON object (append corruption?): %v", err)
	}
	// No leftover tmp file.
	if _, err := os.Stat(filepath.Join(dir, sh.ID+".json.tmp")); !os.IsNotExist(err) {
		t.Error("stray .tmp file left after Save")
	}
}

// Section CRUD lives nested in the document: add, edit, delete all persist.
func TestSectionCRUDNested(t *testing.T) {
	s := New(t.TempDir())
	sh, _ := s.Create()

	sec := sh.AddSection("Findings")
	if sec.ID == "" || sec.Title != "Findings" {
		t.Fatalf("AddSection returned bad section: %+v", sec)
	}
	if len(sh.Sections) != 2 {
		t.Fatalf("want 2 sections after add, got %d", len(sh.Sections))
	}
	if sh.Sections[1].Order <= sh.Sections[0].Order {
		t.Error("added section must sort after the starter section")
	}
	if err := s.Save(sh); err != nil {
		t.Fatal(err)
	}

	// Edit a body and delete the starter, then persist.
	got, _ := s.Get(sh.ID)
	got.Sections[1].Body = "body text"
	got.Sections = append(got.Sections[:0], got.Sections[1:]...) // drop starter
	if err := s.Save(got); err != nil {
		t.Fatal(err)
	}

	final, _ := s.Get(sh.ID)
	if len(final.Sections) != 1 {
		t.Fatalf("want 1 section after delete, got %d", len(final.Sections))
	}
	if final.Sections[0].Title != "Findings" || final.Sections[0].Body != "body text" {
		t.Errorf("surviving section wrong: %+v", final.Sections[0])
	}
}

// An embedded reference round-trips its durable source address and snapshot.
func TestReferenceRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	sh, _ := s.Create()
	sh.Sections[0].References = []Reference{{
		ID:       "ref1",
		Source:   Source{UID: "abc123", Segment: 2, Record: 7, Node: "lane-a", TurnTime: "2026-07-28T10:00:00Z"},
		Snapshot: Snapshot{Lane: "#c0392b", Station: "lane-a", Speaker: "assistant", Time: "2026-07-28T10:00:00Z", Text: "hello"},
	}}
	if err := s.Save(sh); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(sh.ID)
	refs := got.Sections[0].References
	if len(refs) != 1 {
		t.Fatalf("want 1 reference, got %d", len(refs))
	}
	if refs[0].Source.UID != "abc123" || refs[0].Source.Segment != 2 || refs[0].Source.Record != 7 {
		t.Errorf("source address did not round-trip: %+v", refs[0].Source)
	}
	if refs[0].Snapshot.Text != "hello" {
		t.Errorf("snapshot text did not round-trip: %+v", refs[0].Snapshot)
	}
}

// AddReference appends to the named section (bottom placement) and mints a
// stable id; the same source added to two sections is two references, never a
// move.
func TestAddReferenceMintsIDAndAppends(t *testing.T) {
	s := New(t.TempDir())
	sh, _ := s.Create()
	secA := sh.Sections[0].ID
	secB := sh.AddSection("Two").ID

	src := Source{UID: "u1", Segment: 0, Record: 3}
	snap := Snapshot{Lane: "#c0392b", Speaker: "assistant", Text: "evidence"}

	r1, err := sh.AddReference(secA, Reference{Source: src, Snapshot: snap})
	if err != nil {
		t.Fatal(err)
	}
	if r1.ID == "" {
		t.Error("AddReference did not mint a reference id")
	}
	r2, err := sh.AddReference(secB, Reference{Source: src, Snapshot: snap})
	if err != nil {
		t.Fatal(err)
	}
	if r1.ID == r2.ID {
		t.Error("two references to one source must have distinct ids")
	}
	if len(sh.Sections[0].References) != 1 || len(sh.Sections[1].References) != 1 {
		t.Fatalf("want one reference in each section, got %d and %d",
			len(sh.Sections[0].References), len(sh.Sections[1].References))
	}

	// Bottom placement: a second reference appends after the first.
	if _, err := sh.AddReference(secA, Reference{Source: Source{UID: "u2"}}); err != nil {
		t.Fatal(err)
	}
	if got := sh.Sections[0].References[1].Source.UID; got != "u2" {
		t.Errorf("second reference not appended at bottom: %q", got)
	}

	// An unknown section is ErrNotFound, not a panic.
	if _, err := sh.AddReference("nope", Reference{}); err != ErrNotFound {
		t.Errorf("unknown section: err = %v, want ErrNotFound", err)
	}
}

// RemoveReference trashes exactly one reference and leaves the rest intact.
func TestRemoveReferenceIsScoped(t *testing.T) {
	s := New(t.TempDir())
	sh, _ := s.Create()
	sec := sh.Sections[0].ID
	keep, _ := sh.AddReference(sec, Reference{Source: Source{UID: "keep"}})
	drop, _ := sh.AddReference(sec, Reference{Source: Source{UID: "drop"}})

	if !sh.RemoveReference(sec, drop.ID) {
		t.Fatal("RemoveReference reported not-found for a present reference")
	}
	if len(sh.Sections[0].References) != 1 || sh.Sections[0].References[0].ID != keep.ID {
		t.Errorf("wrong reference removed: %+v", sh.Sections[0].References)
	}
	if sh.RemoveReference(sec, "already-gone") {
		t.Error("RemoveReference reported found for an absent reference")
	}
}

// Delete moves the file into archive/ — nothing erased, and a reissued id can
// never append onto dead content because the live file is gone.
func TestDeleteArchives(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	sh, _ := s.Create()

	if err := s.Delete(sh.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, sh.ID+".json")); !os.IsNotExist(err) {
		t.Error("live sheet file still present after delete")
	}
	if _, err := s.Get(sh.ID); err == nil {
		t.Error("Get should fail for a deleted sheet")
	}
	archived, err := filepath.Glob(filepath.Join(dir, "archive", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 {
		t.Fatalf("want 1 archived file, got %d", len(archived))
	}
	list, _ := s.List()
	if len(list) != 0 {
		t.Errorf("deleted sheet still listed: %d remain", len(list))
	}
}

// A malformed or unknown-shaped file in the directory is skipped in List, never
// a hard error (defensive-parse invariant), and the archive subdir is not read.
func TestListDefensiveSkipsGarbage(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	good, _ := s.Create()

	// Torn JSON, a non-json file, and a stray tmp file must all be ignored.
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, good.ID+".json.tmp"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List must not error on garbage: %v", err)
	}
	if len(list) != 1 || list[0].ID != good.ID {
		t.Fatalf("want only the one good sheet, got %d", len(list))
	}
}
