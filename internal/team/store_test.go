package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleEntry(n int) Entry {
	return Entry{
		ID:       fmt.Sprintf("%032x", n),
		Name:     "Lab relay",
		URL:      "http://relay.example:8080",
		SeatName: "Ada",
		Token:    fmt.Sprintf("%064x", n),
		JoinedAt: "2026-11-12T09:30:00Z",
	}
}

func snapshotStore(t *testing.T) {
	t.Helper()
	ops := storeOps
	marshal := marshalTeam
	t.Cleanup(func() {
		storeOps = ops
		marshalTeam = marshal
	})
}

func writeTeamFile(t *testing.T, dir string, entries []Entry) []byte {
	t.Helper()
	doc := struct {
		V       int     `json:"v"`
		Servers []Entry `json:"servers"`
	}{V: 1, Servers: entries}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(dir, File), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertOnlyFile(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("files = %v, want only %s", names, name)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("entries = %#v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, File)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v", err)
	}
}

func TestStoreRoundTripKeepsFieldsAndMode(t *testing.T) {
	dir := t.TempDir()
	e := sampleEntry(1)
	e.Name = " Lab "
	e.SeatName = "A&B"
	e.URL = "https://relay.example"
	if err := Add(dir, e); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != e {
		t.Fatalf("entries = %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	assertOnlyFile(t, dir, File)
}

func TestStoreAcceptsA64ByteName(t *testing.T) {
	dir := t.TempDir()
	e := sampleEntry(1)
	e.Name = strings.Repeat("a", 64)
	e.SeatName = strings.Repeat("é", 32) // 64 bytes
	if err := Add(dir, e); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != e {
		t.Fatalf("entries = %+v", got)
	}
}

func TestStoreKeepsInsertionOrder(t *testing.T) {
	dir := t.TempDir()
	want := []Entry{sampleEntry(3), sampleEntry(1), sampleEntry(2)}
	want[1].URL = "https://relay.example:8443"
	for _, e := range want {
		if err := Add(dir, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAddRejectsADuplicateID(t *testing.T) {
	dir := t.TempDir()
	e := sampleEntry(1)
	if err := Add(dir, e); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	again := e
	again.Name = "Other"
	again.Token = sampleEntry(9).Token
	err = Add(dir, again)
	if !errors.Is(err, ErrAlreadyJoined) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), again.Token) {
		t.Fatalf("error quotes the token: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, after) {
		t.Fatal("duplicate changed the file")
	}

	t.Run("full list", func(t *testing.T) {
		full := t.TempDir()
		for i := 1; i <= 16; i++ {
			if err := Add(full, sampleEntry(i)); err != nil {
				t.Fatal(err)
			}
		}
		before, err := os.ReadFile(filepath.Join(full, File))
		if err != nil {
			t.Fatal(err)
		}
		if err := Add(full, sampleEntry(1)); !errors.Is(err, ErrAlreadyJoined) {
			t.Fatalf("error = %v", err)
		}
		after, err := os.ReadFile(filepath.Join(full, File))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("duplicate changed a full file")
		}
	})
}

func TestAddRejectsASeventeenthServer(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 16; i++ {
		if err := Add(dir, sampleEntry(i)); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	extra := sampleEntry(17)
	err = Add(dir, extra)
	if !errors.Is(err, ErrTooMany) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), extra.Token) {
		t.Fatalf("error quotes the token: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a rejected server changed the file")
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 16 {
		t.Fatalf("len = %d", len(got))
	}
}

func TestRemoveKeepsTheOtherEntries(t *testing.T) {
	dir := t.TempDir()
	entries := []Entry{sampleEntry(3), sampleEntry(1), sampleEntry(2)}
	for _, e := range entries {
		if err := Add(dir, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := Remove(dir, sampleEntry(1).ID); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{entries[0], entries[2]}
	if len(got) != len(want) {
		t.Fatalf("len = %d", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %+v", i, got[i])
		}
	}
	info, err := os.Stat(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestRemoveLastLeavesAValidEmptyFile(t *testing.T) {
	dir := t.TempDir()
	if err := Add(dir, sampleEntry(1)); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir, sampleEntry(1).ID); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("entries = %#v", got)
	}
	raw, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		V       int     `json:"v"`
		Servers []Entry `json:"servers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.V != 1 || doc.Servers == nil || len(doc.Servers) != 0 {
		t.Fatalf("document = %+v", doc)
	}
	assertOnlyFile(t, dir, File)
}

func TestRemoveUnknownID(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		dir := t.TempDir()
		if err := Remove(dir, sampleEntry(1).ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, File)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat = %v", err)
		}
	})
	t.Run("present file", func(t *testing.T) {
		dir := t.TempDir()
		before := writeTeamFile(t, dir, []Entry{sampleEntry(1)})
		if err := Remove(dir, sampleEntry(2).ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
		after, err := os.ReadFile(filepath.Join(dir, File))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("unknown id changed the file")
		}
	})
}

func TestDamagedFileIsLeftUntouched(t *testing.T) {
	secret := strings.Repeat("ab", 32)
	valid := sampleEntry(1)
	dup, err := json.Marshal(struct {
		V       int     `json:"v"`
		Servers []Entry `json:"servers"`
	}{V: 1, Servers: []Entry{valid, valid}})
	if err != nil {
		t.Fatal(err)
	}
	badID := valid
	badID.ID = "zz"
	badIDRaw, err := json.Marshal(struct {
		V       int     `json:"v"`
		Servers []Entry `json:"servers"`
	}{V: 1, Servers: []Entry{badID}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"not json", "nope"},
		{"version 2", `{"v":2,"servers":[]}`},
		{"missing version", `{"servers":[]}`},
		{"missing servers", `{"v":1}`},
		{"null servers", `{"v":1,"servers":null}`},
		{"servers object", `{"v":1,"servers":{}}`},
		{"unknown field", `{"v":1,"servers":[],"token":"` + secret + `"}`},
		{"trailing value", `{"v":1,"servers":[]}{}`},
		{"duplicate id", string(dup)},
		{"bad id", string(badIDRaw)},
		{"oversized", secret + strings.Repeat("x", 64*1024)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, File)
			before := []byte(tc.body)
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(dir)
			if !errors.Is(err, ErrDamaged) {
				t.Fatalf("Load = %v", err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), valid.Token) {
				t.Fatalf("Load quotes a secret: %v", err)
			}
			if err := Add(dir, sampleEntry(4)); !errors.Is(err, ErrDamaged) {
				t.Fatalf("Add = %v", err)
			}
			if err := Remove(dir, valid.ID); !errors.Is(err, ErrDamaged) {
				t.Fatalf("Remove = %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("damaged file was rewritten")
			}
			assertOnlyFile(t, dir, File)
		})
	}
}

func TestAddRejectsAnInvalidEntry(t *testing.T) {
	long := strings.Repeat("a", 65)
	cases := []struct {
		name string
		edit func(*Entry)
	}{
		{"bad id", func(e *Entry) { e.ID = "0123456789abcdef0123456789abcdeg" }},
		{"short id", func(e *Entry) { e.ID = "abcd" }},
		{"uppercase id", func(e *Entry) { e.ID = "0123456789ABCDEF0123456789ABCDEF" }},
		{"empty name", func(e *Entry) { e.Name = "" }},
		{"long name", func(e *Entry) { e.Name = long }},
		{"control name", func(e *Entry) { e.Name = "Lab\nrelay" }},
		{"invalid name utf-8", func(e *Entry) { e.Name = "Lab\xff" }},
		{"unparsed url", func(e *Entry) { e.URL = "http://[" }},
		{"ftp url", func(e *Entry) { e.URL = "ftp://relay.example" }},
		{"empty host", func(e *Entry) { e.URL = "http:///no-host" }},
		{"userinfo", func(e *Entry) { e.URL = "http://ada:secret@relay.example" }},
		{"query", func(e *Entry) { e.URL = "http://relay.example?x=1" }},
		{"empty query", func(e *Entry) { e.URL = "http://relay.example?" }},
		{"fragment", func(e *Entry) { e.URL = "http://relay.example#code" }},
		{"path", func(e *Entry) { e.URL = "http://relay.example/join" }},
		{"trailing slash", func(e *Entry) { e.URL = "http://relay.example/" }},
		{"empty port", func(e *Entry) { e.URL = "http://relay.example:" }},
		{"uppercase scheme", func(e *Entry) { e.URL = "HTTP://relay.example" }},
		{"empty seat", func(e *Entry) { e.SeatName = "" }},
		{"long seat", func(e *Entry) { e.SeatName = long }},
		{"control seat", func(e *Entry) { e.SeatName = "A\nb" }},
		{"padded seat", func(e *Entry) { e.SeatName = " Ada " }},
		{"invalid seat utf-8", func(e *Entry) { e.SeatName = "A\xff" }},
		{"short token", func(e *Entry) { e.Token = "abcd" }},
		{"uppercase token", func(e *Entry) { e.Token = strings.Repeat("AB", 32) }},
		{"non hex token", func(e *Entry) { e.Token = strings.Repeat("g", 64) }},
		{"bad time", func(e *Entry) { e.JoinedAt = "yesterday" }},
		{"offset time", func(e *Entry) { e.JoinedAt = "2026-11-12T09:30:00+00:00" }},
		{"zoned time", func(e *Entry) { e.JoinedAt = "2026-11-12T10:30:00+01:00" }},
		{"fractional time", func(e *Entry) { e.JoinedAt = "2026-11-12T09:30:00.000Z" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			before := writeTeamFile(t, dir, []Entry{sampleEntry(1)})
			e := sampleEntry(2)
			tc.edit(&e)
			err := Add(dir, e)
			if !errors.Is(err, ErrDamaged) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), e.Token) {
				t.Fatalf("error quotes the entry: %v", err)
			}
			after, err := os.ReadFile(filepath.Join(dir, File))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("invalid entry changed the file")
			}
			assertOnlyFile(t, dir, File)
		})
	}

	t.Run("no file yet", func(t *testing.T) {
		dir := t.TempDir()
		e := sampleEntry(1)
		e.ID = "nope"
		if err := Add(dir, e); !errors.Is(err, ErrDamaged) {
			t.Fatalf("error = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, File)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat = %v", err)
		}
	})
}

func TestStoreWriteFailuresLeaveThePreviousFile(t *testing.T) {
	cases := []struct {
		name string
		set  func(real storeIO) error
	}{
		{"create", func(real storeIO) error {
			boom := errors.New("create failed")
			storeOps.createTemp = func(string) (*os.File, error) { return nil, boom }
			return boom
		}},
		{"chmod", func(real storeIO) error {
			boom := errors.New("chmod failed")
			storeOps.chmod = func(*os.File, os.FileMode) error { return boom }
			return boom
		}},
		{"write", func(real storeIO) error {
			boom := errors.New("write failed")
			storeOps.write = func(*os.File, []byte) (int, error) { return 0, boom }
			return boom
		}},
		{"short write", func(real storeIO) error {
			storeOps.write = func(*os.File, []byte) (int, error) { return 1, nil }
			return io.ErrShortWrite
		}},
		{"sync", func(real storeIO) error {
			boom := errors.New("sync failed")
			storeOps.syncFile = func(*os.File) error { return boom }
			return boom
		}},
		{"close", func(real storeIO) error {
			boom := errors.New("close failed")
			storeOps.close = func(f *os.File) error {
				_ = real.close(f)
				return boom
			}
			return boom
		}},
		{"rename", func(real storeIO) error {
			boom := errors.New("rename failed")
			storeOps.rename = func(string, string) error { return boom }
			return boom
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshotStore(t)
			dir := t.TempDir()
			before := writeTeamFile(t, dir, []Entry{sampleEntry(1)})
			want := tc.set(storeOps)
			if err := Add(dir, sampleEntry(2)); !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
			after, err := os.ReadFile(filepath.Join(dir, File))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed write changed the file")
			}
			assertOnlyFile(t, dir, File)
		})
	}
}

func TestStoreMarshalFailureWritesNothing(t *testing.T) {
	snapshotStore(t)
	boom := errors.New("marshal failed")
	marshalTeam = func(any) ([]byte, error) { return nil, boom }
	dir := t.TempDir()
	before := writeTeamFile(t, dir, []Entry{sampleEntry(1)})
	if err := Add(dir, sampleEntry(2)); !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("marshal failure changed the file")
	}
	assertOnlyFile(t, dir, File)
}

func TestLoadOfADirectoryIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, File)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("Load = %v", err)
	}
	if err := Add(dir, sampleEntry(1)); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("Add = %v", err)
	}
	if err := Remove(dir, sampleEntry(1).ID); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("Remove = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("directory was replaced")
	}
	assertOnlyFile(t, dir, File)
}

func TestLoadOfAFileDataDirIsAnError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(file, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(file)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Load = %v", err)
	}
	if err := Add(file, sampleEntry(1)); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Add = %v", err)
	}
	if err := Remove(file, sampleEntry(1).ID); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Remove = %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "synthetic" {
		t.Fatalf("file = %q", raw)
	}
	assertOnlyFile(t, dir, "not-a-directory")
}
