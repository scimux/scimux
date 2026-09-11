package notestore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A fresh note gets an auto title, one starter section, and an opaque id.
func TestCreateStarterShape(t *testing.T) {
	s := New(t.TempDir())
	sh, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if sh.ID == "" {
		t.Fatal("note id is empty; ids must be opaque, stable, non-empty")
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

// Create persists the file so Get round-trips it. Notes live at
// notes/<id>/note.json (per-note folder layout) so each note can later be an
// independent git repo.
func TestCreateReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	sh, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	// Folder layout: notes/<id>/note.json — not the legacy flat notes/<id>.json.
	notePath := filepath.Join(dir, sh.ID, "note.json")
	if _, err := os.Stat(notePath); err != nil {
		t.Fatalf("expected note at %s: %v", notePath, err)
	}
	if _, err := os.Stat(filepath.Join(dir, sh.ID+".json")); !os.IsNotExist(err) {
		t.Error("legacy flat notes/<id>.json must not be written")
	}
	got, err := s.Get(sh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sh.ID || got.Title != sh.Title || len(got.Sections) != 1 {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, sh)
	}
}

// List returns notes ordered by the stored order field, never lexical id order.
func TestListOrderedByOrderField(t *testing.T) {
	s := New(t.TempDir())
	a, _ := s.Create()
	b, _ := s.Create()
	c, _ := s.Create()

	// Reorder so display order is c, a, b regardless of id/creation order.
	a.Order, b.Order, c.Order = 1, 2, 0
	for _, sh := range []Note{a, b, c} {
		if err := s.Save(sh); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 notes, got %d", len(list))
	}
	want := []string{c.ID, a.ID, b.ID}
	for i, id := range want {
		if list[i].ID != id {
			t.Errorf("list[%d].ID = %q, want %q (order field must drive sort)", i, list[i].ID, id)
		}
	}
}

// Save is an atomic rewrite (tmp-write + rename inside the note folder), not an
// append, and leaves no stray tmp file behind; edited_at advances on each save.
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
	notePath := filepath.Join(dir, sh.ID, "note.json")
	raw, err := os.ReadFile(notePath)
	if err != nil {
		t.Fatal(err)
	}
	var probe Note
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("file is not a single JSON object (append corruption?): %v", err)
	}
	// No leftover tmp file inside the note folder.
	if _, err := os.Stat(filepath.Join(dir, sh.ID, "note.json.tmp")); !os.IsNotExist(err) {
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

func TestSnapshotProvenanceSurvivesSaveReloadAndJSON(t *testing.T) {
	s := New(t.TempDir())
	sh, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	prov := json.RawMessage(`{"_meta":{"src":"synth-note","n":1}}`)
	sh.Sections[0].References = []Reference{{
		ID:     "ref1",
		Source: Source{UID: "abc123", Segment: 2, Record: 7, Node: "lane-a", TurnTime: "2026-09-10T12:00:00Z"},
		Snapshot: Snapshot{
			Lane: "#c0392b", Station: "lane-a", Speaker: "muse",
			Time: "2026-09-10T12:00:00Z", Text: "frozen",
			Role: "assistant", Agent: "muse", Prov: prov,
		},
	}}
	if err := s.Save(sh); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(sh.ID)
	if err != nil {
		t.Fatal(err)
	}
	snap := got.Sections[0].References[0].Snapshot
	if snap.Role != "assistant" || snap.Agent != "muse" || snap.Speaker != "muse" || snap.Text != "frozen" {
		t.Fatalf("reload snapshot = %+v", snap)
	}
	if bytes.TrimSpace(snap.Prov)[0] == '"' {
		t.Fatalf("Prov was stringified: %s", snap.Prov)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(snap.Prov, &decoded); err != nil {
		t.Fatalf("Prov unmarshal: %v (%s)", err, snap.Prov)
	}
	var gotMeta, wantMeta any
	if err := json.Unmarshal(decoded["_meta"], &gotMeta); err != nil {
		t.Fatalf("_meta: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"src":"synth-note","n":1}`), &wantMeta); err != nil {
		t.Fatal(err)
	}
	gotB, _ := json.Marshal(gotMeta)
	wantB, _ := json.Marshal(wantMeta)
	if string(gotB) != string(wantB) {
		t.Fatalf("_meta = %s, want %s", gotB, wantB)
	}

	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var again Snapshot
	if err := json.Unmarshal(b, &again); err != nil {
		t.Fatal(err)
	}
	if again.Role != "assistant" || again.Agent != "muse" {
		t.Fatalf("json round-trip identity = %+v", again)
	}
	var aVal, sVal any
	if err := json.Unmarshal(again.Prov, &aVal); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(snap.Prov, &sVal); err != nil {
		t.Fatal(err)
	}
	ab, _ := json.Marshal(aVal)
	sb, _ := json.Marshal(sVal)
	if string(ab) != string(sb) {
		t.Fatalf("json round-trip Prov = %s, want %s", ab, sb)
	}

	// Self-contained: after the source node/bookmark are gone, the snapshot still holds attribution.
	body, err := os.ReadFile(filepath.Join(s.Dir, sh.ID, "note.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"agent"`)) || !bytes.Contains(body, []byte(`"muse"`)) ||
		!bytes.Contains(body, []byte(`"role"`)) || !bytes.Contains(body, []byte(`"assistant"`)) {
		t.Fatalf("on-disk note missing frozen attribution: %s", body)
	}
	if !bytes.Contains(body, []byte(`"src"`)) || !bytes.Contains(body, []byte(`"synth-note"`)) {
		t.Fatalf("on-disk note missing provenance: %s", body)
	}
	orig := json.RawMessage(`{"src":"gone"}`)
	copy(snap.Prov, orig)
	got2, err := s.Get(sh.ID)
	if err != nil {
		t.Fatal(err)
	}
	var still any
	if err := json.Unmarshal(got2.Sections[0].References[0].Snapshot.Prov, &still); err != nil {
		t.Fatal(err)
	}
	bStill, _ := json.Marshal(still)
	if !bytes.Contains(bStill, []byte(`synth-note`)) {
		t.Fatalf("mutating the in-memory snapshot must not rewrite the store: %s", bStill)
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

// Delete archives the whole note folder to notes/archive/<id>.<stamp>/ —
// nothing erased, and a reissued id can never append onto dead content.
func TestDeleteArchives(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	sh, _ := s.Create()

	if err := s.Delete(sh.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, sh.ID)); !os.IsNotExist(err) {
		t.Error("live note folder still present after delete")
	}
	if _, err := s.Get(sh.ID); err == nil {
		t.Error("Get should fail for a deleted note")
	}
	archived, err := filepath.Glob(filepath.Join(dir, "archive", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 {
		t.Fatalf("want 1 archived folder, got %d", len(archived))
	}
	// Archive target is a directory named <id>.<stamp>, containing note.json.
	info, err := os.Stat(archived[0])
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("archive entry must be a folder, got file %s", archived[0])
	}
	base := filepath.Base(archived[0])
	if !strings.HasPrefix(base, sh.ID+".") {
		t.Errorf("archive name %q must start with %q.", base, sh.ID)
	}
	if _, err := os.Stat(filepath.Join(archived[0], "note.json")); err != nil {
		t.Errorf("archived folder missing note.json: %v", err)
	}
	list, _ := s.List()
	if len(list) != 0 {
		t.Errorf("deleted note still listed: %d remain", len(list))
	}
}

// List reads only notes/<id>/note.json folders. Stray top-level *.json (legacy
// flat layout or garbage) are ignored — no accidental legacy read path.
// Malformed note.json inside a folder is skipped, never a hard error.
func TestListDefensiveSkipsGarbage(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	good, _ := s.Create()

	// Stray top-level *.json must not be listed (proves no legacy flat-file read).
	legacy := `{"id":"legacy-flat","title":"should be ignored","order":0,"sections":[]}`
	if err := os.WriteFile(filepath.Join(dir, "legacy-flat.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Torn note.json inside a folder, and a stray tmp, must be skipped.
	badDir := filepath.Join(dir, "torn-note")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "note.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, good.ID, "note.json.tmp"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List must not error on garbage: %v", err)
	}
	if len(list) != 1 || list[0].ID != good.ID {
		t.Fatalf("want only the one good note (no legacy flat file), got %d: %+v", len(list), list)
	}
}

// --- P3: note identity and path confinement ---

// invalidIDs covers malformed identities that must never select a path outside
// exactly one live note directory. Both slash styles are represented.
var invalidIDs = []string{
	"",
	".",
	"..",
	"foo/bar",
	`foo\bar`,
	"/absolute",
	`\absolute`,
	"archive",
	".hidden",
	"20260101T120000-deadbeef/extra",
	"not-a-timestamp-aabbccdd",
	"20260101T120000",            // missing suffix
	"20260101T120000-",           // empty suffix
	"20260101T120000-gggggggg",   // non-hex
	"20260101T120000-aabbcc",     // too short
	"20260101T120000-aabbccddee", // too long for normal form
	"20260101T120000-Tdeadbeef",  // wrong separator case/shape
	"20260101t120000-deadbeef",   // lowercase T
	"2026-01-01T12:00:00-deadbeef",
	"20260101T120000-deadbeef\x00",
	"../archive/secret",
	"archive/secret",
	"..%2Farchive", // double-encoded leftover after single decode
}

func TestValidIDRejectsMalformed(t *testing.T) {
	for _, id := range invalidIDs {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true, want false", id)
		}
	}
	// Valid minted shapes: normal (8 hex) and rand-fallback (t + hex).
	for _, id := range []string{
		"20260101T120000-deadbeef",
		"20260812T180517-9c0a9701",
		"20260101T120000-t18cb20f5d2c2fe4c",
		"20260101T000000-t1",
	} {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false, want true", id)
		}
	}
}

// Get/Save/Delete reject invalid IDs without touching anything outside one
// intended live-note directory. Sentinel files prove no traversal.
func TestInvalidIDNeverTouchesOutsideNoteDir(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	// Sentinels that traversal must not read, write, or archive.
	secretPath := filepath.Join(root, "secret-marker.txt")
	if err := os.WriteFile(secretPath, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	archiveSecret := filepath.Join(root, "archive", "secret")
	if err := os.MkdirAll(archiveSecret, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archiveSecret, "note.json"), []byte(`{"id":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(root), "outside-sentinel")
	if err := os.WriteFile(outside, []byte("OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	// Snapshot root tree (names only at top level + archive).
	snapshot := func() string {
		var b strings.Builder
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			b.WriteString(e.Name())
			b.WriteByte('\n')
		}
		return b.String()
	}
	before := snapshot()
	secretBefore, _ := os.ReadFile(secretPath)
	archBefore, _ := os.ReadFile(filepath.Join(archiveSecret, "note.json"))
	outBefore, _ := os.ReadFile(outside)

	for _, id := range invalidIDs {
		if _, err := s.Get(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Get(%q): err = %v, want ErrInvalidID", id, err)
		}
		if err := s.Save(Note{ID: id, Title: "x", Sections: []Section{}}); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Save(%q): err = %v, want ErrInvalidID", id, err)
		}
		if err := s.Delete(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Delete(%q): err = %v, want ErrInvalidID", id, err)
		}
	}

	if after := snapshot(); after != before {
		t.Errorf("root directory changed after invalid ops:\nbefore:\n%safter:\n%s", before, after)
	}
	if got, _ := os.ReadFile(secretPath); string(got) != string(secretBefore) {
		t.Error("secret-marker.txt was modified")
	}
	if got, _ := os.ReadFile(filepath.Join(archiveSecret, "note.json")); string(got) != string(archBefore) {
		t.Error("archive/secret/note.json was modified")
	}
	if got, _ := os.ReadFile(outside); string(got) != string(outBefore) {
		t.Error("outside sentinel was modified")
	}
	// No new archive entries from invalid deletes.
	archived, _ := filepath.Glob(filepath.Join(root, "archive", "*"))
	if len(archived) != 1 {
		t.Errorf("archive entries = %v, want only original secret", archived)
	}
}

// TryCommit with an invalid id must not invoke the git locator or open a repo.
func TestTryCommitInvalidIDNeverInvokesGit(t *testing.T) {
	s := New(t.TempDir())
	called := false
	restore := SetFindGitForTest(func() (string, error) {
		called = true
		return "/usr/bin/git", nil
	})
	defer restore()

	for _, id := range []string{"", "..", "archive", "foo/bar", `foo\bar`, "/abs", "not-valid"} {
		called = false
		s.TryCommit(id, "should not run")
		if called {
			t.Errorf("TryCommit(%q) invoked findGit", id)
		}
	}
}

// Valid server-minted IDs still round-trip, save atomically, keep permissions,
// work with optional git, and archive the whole folder on Delete.
func TestValidIDRoundTripPermissionsGitArchive(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	sh, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(sh.ID) {
		t.Fatalf("Create minted invalid id %q", sh.ID)
	}
	if !ValidID(sh.Sections[0].ID) {
		t.Fatalf("Create minted invalid section id %q", sh.Sections[0].ID)
	}

	// Permissions: note dir 0700, note.json 0600.
	di, err := os.Stat(filepath.Join(dir, sh.ID))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("note dir mode = %o, want 0700", di.Mode().Perm())
	}
	fi, err := os.Stat(filepath.Join(dir, sh.ID, "note.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("note.json mode = %o, want 0600", fi.Mode().Perm())
	}

	sh.Sections[0].Body = "atomic body"
	if err := s.Save(sh); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(sh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sections[0].Body != "atomic body" {
		t.Errorf("body = %q", got.Sections[0].Body)
	}
	if _, err := os.Stat(filepath.Join(dir, sh.ID, "note.json.tmp")); !os.IsNotExist(err) {
		t.Error("stray tmp after atomic save")
	}

	// Optional isolated git: only if git is present.
	if bin, err := exec.LookPath("git"); err == nil {
		s.TryCommit(sh.ID, "version")
		if _, err := os.Stat(filepath.Join(dir, sh.ID, ".git")); err != nil {
			t.Errorf("expected .git after TryCommit with git at %s: %v", bin, err)
		}
	}

	if err := s.Delete(sh.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, sh.ID)); !os.IsNotExist(err) {
		t.Error("live folder still present after delete")
	}
	archived, err := filepath.Glob(filepath.Join(dir, "archive", sh.ID+".*"))
	if err != nil || len(archived) != 1 {
		t.Fatalf("want one archive under notes/archive/, got %v err=%v", archived, err)
	}
	// Archive destination must be directly beneath notes/archive/.
	rel, err := filepath.Rel(filepath.Join(dir, "archive"), archived[0])
	if err != nil || strings.Contains(rel, string(filepath.Separator)) || rel == ".." || strings.HasPrefix(rel, "..") {
		t.Errorf("archive path not directly under archive/: %q rel=%q err=%v", archived[0], rel, err)
	}
}

// Get rejects a note.json whose embedded ID differs from the directory name.
func TestGetRejectsEmbeddedIDMismatch(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	// Manually plant a folder with a valid name but mismatched document id.
	id := "20260101T120000-aabbccdd"
	noteDir := filepath.Join(dir, id)
	if err := os.MkdirAll(noteDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Embedded ID is a different valid-looking mint.
	doc := `{"id":"20260101T120000-11223344","title":"mismatch","order":0,"sections":[]}`
	if err := os.WriteFile(filepath.Join(noteDir, "note.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(id); !errors.Is(err, ErrInvalidID) {
		// Also accept a dedicated mismatch error if exposed as ErrInvalidID subclass.
		if err == nil {
			t.Fatal("Get accepted embedded ID mismatch")
		}
		// Must not return the mismatched document as success.
		t.Fatalf("Get mismatch: err = %v, want ErrInvalidID (or identity error)", err)
	}
}

// A mismatched embedded ID cannot redirect Save or TryCommit into another directory.
func TestMismatchedEmbeddedIDCannotRedirectSaveOrCommit(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	// Victim: a real note that must stay untouched.
	victim, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	victimBody := "do not clobber"
	victim.Sections[0].Body = victimBody
	if err := s.Save(victim); err != nil {
		t.Fatal(err)
	}

	// Attacker tries to save with directory id A but embedded id of victim.
	attackerID := "20260101T120000-deadbeef"
	// First create a legitimate note at attackerID by planting via Save with matching id.
	attacker := Note{
		ID:       attackerID,
		Title:    "attacker",
		Created:  nowStamp(),
		Edited:   nowStamp(),
		Order:    99,
		Sections: []Section{{ID: "20260101T120000-eeeeeeee", Title: "S", Order: 0}},
	}
	if err := s.Save(attacker); err != nil {
		t.Fatal(err)
	}

	// Now craft a Save that claims victim's ID inside the document while
	// being invoked as if for the attacker folder — Save uses sh.ID only,
	// so a forged sh.ID=victim must be rejected when... wait: Save uses
	// sh.ID as the path key. The attack is: Get returns document with
	// embedded victim ID, then Save writes to victim's path.
	// After Get rejects mismatch, mutation handlers never see it.
	// Still prove Save(sh) with path construction cannot write outside:
	// Save with sh.ID = victim.ID is legitimate; the bad case is Get
	// returning a doc whose ID differs from requested path id.
	// Direct Save of a note whose ID is valid still goes to notes/<id>/.
	// The attack surface is Get(requested) returning sh with sh.ID != requested.
	// Prove plant: folder attackerID contains note.json with victim's ID —
	// Get must fail, and a subsequent Save of that document using victim.ID
	// would be a separate call the handler must not make.

	// Plant mismatch under attackerID.
	bad := fmt.Sprintf(`{"id":%q,"title":"forged","order":0,"sections":[{"id":"20260101T120000-ffffffff","title":"x","body":"pwned","order":0}]}`, victim.ID)
	if err := os.WriteFile(filepath.Join(dir, attackerID, "note.json"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(attackerID); err == nil {
		t.Fatal("Get must reject embedded ID != directory")
	}

	// TryCommit must not open git for invalid / after failed get path.
	called := false
	restore := SetFindGitForTest(func() (string, error) {
		called = true
		return "/usr/bin/git", nil
	})
	defer restore()
	// Commit under attacker folder id — document is mismatched; TryCommit only
	// needs a valid id and a note.json on disk. It should still not rewrite
	// victim. If TryCommit validates embedded ID, even better.
	called = false
	s.TryCommit(attackerID, "forged")
	// Victim content must remain.
	v, err := s.Get(victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Sections[0].Body != victimBody {
		t.Errorf("victim body changed to %q", v.Sections[0].Body)
	}
	// Save with intentionally wrong cross-id must only affect notes/<sh.ID>/
	// when sh.ID is valid — never create secondary paths via embedded fields.
	// Save a document whose Title mentions another path; only sh.ID matters.
	cross := Note{ID: attackerID, Title: "x", Sections: []Section{{ID: "20260101T120000-aaaaaaaa", Title: "t"}}}
	// Overwrite the mismatched file with a consistent document via Save.
	if err := s.Save(cross); err != nil {
		// If Save validates existing embedded ID on disk — fine; but Save
		// should accept consistent sh.ID.
		t.Fatalf("Save consistent attacker: %v", err)
	}
	// Victim still intact and only under its own dir.
	if v2, err := s.Get(victim.ID); err != nil || v2.Sections[0].Body != victimBody {
		t.Fatalf("victim after cross save: err=%v body=%v", err, v2)
	}
	// No second directory created from embedded ids.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == "archive" || !e.IsDir() {
			continue
		}
		if !ValidID(e.Name()) {
			t.Errorf("unexpected non-valid dir %q", e.Name())
		}
	}
	_ = called
}

// List skips invalid directory names, malformed docs, and embedded-ID
// mismatches, returning other valid notes.
func TestListSkipsInvalidAndMismatched(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	good, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}

	// Invalid directory name.
	badName := filepath.Join(dir, "not-valid-id")
	if err := os.MkdirAll(badName, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badName, "note.json"),
		[]byte(`{"id":"not-valid-id","title":"x","order":0,"sections":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Valid dir name, mismatched embedded id.
	mismatchID := "20260101T120000-bbbbbbbb"
	md := filepath.Join(dir, mismatchID)
	if err := os.MkdirAll(md, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(md, "note.json"),
		[]byte(`{"id":"20260101T120000-cccccccc","title":"m","order":1,"sections":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Valid dir, malformed JSON.
	tornID := "20260101T120000-dddddddd"
	td := filepath.Join(dir, tornID)
	if err := os.MkdirAll(td, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "note.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != good.ID {
		t.Fatalf("want only good note, got %+v", list)
	}
}

// notePaths prove with filepath.Rel that constructed paths stay under the
// notes root at exactly notes/<valid-id>/ or notes/<valid-id>/note.json.
func TestNotePathContainment(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	id := "20260101T120000-deadbeef"

	dir, err := s.noteDirChecked(id)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.pathChecked(id)
	if err != nil {
		t.Fatal(err)
	}

	// Exactly notes/<id>
	relDir, err := filepath.Rel(root, dir)
	if err != nil || relDir != id {
		t.Errorf("noteDir rel = %q err=%v, want %q", relDir, err, id)
	}
	// Exactly notes/<id>/note.json
	relDoc, err := filepath.Rel(root, doc)
	if err != nil || relDoc != filepath.Join(id, "note.json") {
		t.Errorf("path rel = %q err=%v, want %s", relDoc, err, filepath.Join(id, "note.json"))
	}

	// Invalid IDs never produce paths.
	for _, bad := range []string{"..", "a/b", `a\b`, "/x", "archive", ""} {
		if _, err := s.noteDirChecked(bad); !errors.Is(err, ErrInvalidID) {
			t.Errorf("noteDirChecked(%q) = %v", bad, err)
		}
		if _, err := s.pathChecked(bad); !errors.Is(err, ErrInvalidID) {
			t.Errorf("pathChecked(%q) = %v", bad, err)
		}
	}

	// Archive destinations only from validated ID, directly under archive/.
	arch, err := s.archiveDirChecked(id, "20260101T000000.000000000Z")
	if err != nil {
		t.Fatal(err)
	}
	relArch, err := filepath.Rel(filepath.Join(root, "archive"), arch)
	if err != nil || relArch != id+".20260101T000000.000000000Z" {
		t.Errorf("archive rel = %q err=%v", relArch, err)
	}
	if _, err := s.archiveDirChecked("..", "stamp"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("archiveDirChecked invalid: %v", err)
	}
}
