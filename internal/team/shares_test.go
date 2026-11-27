package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sampleShare(n int) Share {
	return Share{
		UID:        fmt.Sprintf("%016x", n),
		Node:       fmt.Sprintf("chat-%d", n),
		Server:     fmt.Sprintf("%032x", n),
		Scope:      "from_now",
		Start:      0,
		Next:       3,
		StartSent:  true,
		Title:      "Synthetic title",
		Generation: fmt.Sprintf("%016x", n),
		Members: []Member{{
			Seat:   fmt.Sprintf("%032x", n+1000),
			Access: "read",
		}},
		State:    "active",
		SharedAt: "2026-11-14T09:30:00Z",
	}
}

func richShare() Share {
	s := sampleShare(1)
	s.Node = "Chat_1"
	s.Scope = "all"
	s.Start = 4
	s.Next = 4
	s.StartSent = false
	s.Title = `North <relay> "a"`
	s.Members = []Member{
		{Seat: fmt.Sprintf("%032x", 2), Access: "read"},
		{Seat: fmt.Sprintf("%032x", 3), Access: "write"},
	}
	s.State = "stopping"
	return s
}

func snapshotShares(t *testing.T) {
	t.Helper()
	ops := storeOps
	marshal := marshalShares
	t.Cleanup(func() {
		storeOps = ops
		marshalShares = marshal
	})
}

func writeSharesRaw(t *testing.T, dir string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, SharesFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func shareDocument(shares []Share) []byte {
	raw, err := json.Marshal(sharesFile{V: 1, Shares: shares})
	if err != nil {
		panic(err)
	}
	return append(raw, '\n')
}

func TestLoadSharesMissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := LoadShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("shares = %#v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, SharesFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, File)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("team file stat = %v", err)
	}
}

func TestShareStoreRoundTripKeepsFieldsAndMode(t *testing.T) {
	dir := t.TempDir()
	teamBefore := writeTeamFile(t, dir, []Entry{sampleEntry(1)})
	first := richShare()
	second := sampleShare(2)
	second.Title = ""
	second.Members = nil
	second.State = "full"
	second.Scope = "from_now"
	if err := SaveShare(dir, first); err != nil {
		t.Fatal(err)
	}
	if err := SaveShare(dir, second); err != nil {
		t.Fatal(err)
	}
	got, err := LoadShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	second.Members = []Member{}
	if len(got) != 2 || !reflect.DeepEqual(got[0], first) || !reflect.DeepEqual(got[1], second) {
		t.Fatalf("shares = %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, SharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	// A rewrite keeps the mode and does not leave a temporary file.
	first.State = "lost"
	if err := SaveShare(dir, first); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(filepath.Join(dir, SharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode after rewrite = %o", info.Mode().Perm())
	}
	got, err = LoadShares(dir)
	if err != nil || len(got) != 2 || got[0].State != "lost" || got[1].State != "full" {
		t.Fatalf("after rewrite = %+v err = %v", got, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, SharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("token")) || bytes.Contains(raw, []byte("synthetic prompt")) {
		t.Fatalf("file holds content or a token: %s", raw)
	}
	teamAfter, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(teamBefore, teamAfter) {
		t.Fatal("share write changed team.json")
	}
	names := map[string]bool{SharesFile: true, File: true}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("files = %d", len(entries))
	}
	for _, e := range entries {
		if !names[e.Name()] {
			t.Fatalf("unexpected file %s", e.Name())
		}
	}
}

func TestSaveShareReplacesByUID(t *testing.T) {
	dir := t.TempDir()
	if err := SaveShare(dir, sampleShare(1)); err != nil {
		t.Fatal(err)
	}
	if err := SaveShare(dir, sampleShare(2)); err != nil {
		t.Fatal(err)
	}
	updated := sampleShare(1)
	updated.Title = "Renamed"
	updated.State = "active"
	updated.Next = 8
	if err := SaveShare(dir, updated); err != nil {
		t.Fatal(err)
	}
	got, err := LoadShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !reflect.DeepEqual(got[0], updated) || !reflect.DeepEqual(got[1], sampleShare(2)) {
		t.Fatalf("shares = %+v", got)
	}
}

func TestRemoveShareDropsOneAndKeepsOrder(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []int{1, 2, 3} {
		if err := SaveShare(dir, sampleShare(n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveShare(dir, sampleShare(2).UID); err != nil {
		t.Fatal(err)
	}
	got, err := LoadShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !reflect.DeepEqual(got[0], sampleShare(1)) || !reflect.DeepEqual(got[1], sampleShare(3)) {
		t.Fatalf("shares = %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, SharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if err := RemoveShare(dir, sampleShare(1).UID); err != nil {
		t.Fatal(err)
	}
	if err := RemoveShare(dir, sampleShare(3).UID); err != nil {
		t.Fatal(err)
	}
	got, err = LoadShares(dir)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("shares = %#v err = %v", got, err)
	}
	var doc struct {
		V      int     `json:"v"`
		Shares []Share `json:"shares"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, SharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.V != 1 || doc.Shares == nil || len(doc.Shares) != 0 {
		t.Fatalf("document = %+v", doc)
	}
	assertOnlyFile(t, dir, SharesFile)
}

func TestRemoveShareUnknownUID(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		dir := t.TempDir()
		if err := RemoveShare(dir, sampleShare(1).UID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, SharesFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat = %v", err)
		}
	})
	t.Run("present file", func(t *testing.T) {
		dir := t.TempDir()
		before := shareDocument([]Share{sampleShare(1)})
		writeSharesRaw(t, dir, before)
		if err := RemoveShare(dir, sampleShare(2).UID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
		after, err := os.ReadFile(filepath.Join(dir, SharesFile))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("unknown uid changed the file")
		}
		assertOnlyFile(t, dir, SharesFile)
	})
}

func TestSaveShareRejectsThe257th(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= maxShares; i++ {
		if err := SaveShare(dir, sampleShare(i)); err != nil {
			t.Fatalf("share %d: %v", i, err)
		}
	}
	before, err := os.ReadFile(filepath.Join(dir, SharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveShare(dir, sampleShare(maxShares+1)); !errors.Is(err, ErrTooManyShares) {
		t.Fatalf("error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, SharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the rejected share changed the file")
	}
	updated := sampleShare(1)
	updated.Title = "Still room to replace"
	if err := SaveShare(dir, updated); err != nil {
		t.Fatal(err)
	}
	got, err := LoadShares(dir)
	if err != nil || len(got) != maxShares || got[0].Title != updated.Title {
		t.Fatalf("len = %d title = %q err = %v", len(got), got[0].Title, err)
	}
	assertOnlyFile(t, dir, SharesFile)
}

func TestSaveShareRejectsAnOversizedDocument(t *testing.T) {
	dir := t.TempDir()
	members := make([]Member, maxShareMembers)
	for i := range members {
		members[i] = Member{Seat: fmt.Sprintf("%032x", i+1), Access: "read"}
	}
	saved := 0
	for i := 1; i <= maxShares; i++ {
		s := sampleShare(i)
		s.Title = strings.Repeat("a", 200)
		s.Members = members
		err := SaveShare(dir, s)
		if errors.Is(err, ErrTooManyShares) {
			break
		}
		if err != nil {
			t.Fatalf("share %d: %v", i, err)
		}
		saved = i
	}
	if saved == 0 || saved >= maxShares {
		t.Fatalf("saved %d before the size limit", saved)
	}
	before, err := os.ReadFile(filepath.Join(dir, SharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(before) > maxSharesFile {
		t.Fatalf("stored file = %d bytes", len(before))
	}
	overflow := sampleShare(saved + 1)
	overflow.Title = strings.Repeat("b", 200)
	overflow.Members = members
	if err := SaveShare(dir, overflow); !errors.Is(err, ErrTooManyShares) {
		t.Fatalf("error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, SharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the oversized share changed the file")
	}
	got, err := LoadShares(dir)
	if err != nil || len(got) != saved {
		t.Fatalf("len = %d err = %v", len(got), err)
	}
}

func TestDamagedShareFileIsLeftUntouched(t *testing.T) {
	secret := strings.Repeat("ab", 32)
	valid := shareDocument([]Share{sampleShare(1), sampleShare(2)})
	dup := shareDocument([]Share{sampleShare(1), sampleShare(1)})
	cases := []struct {
		name string
		body []byte
	}{
		{"empty", []byte{}},
		{"blank", []byte("\n")},
		{"not json", []byte("nope")},
		{"version 2", []byte(`{"v":2,"shares":[]}`)},
		{"missing version", []byte(`{"shares":[]}`)},
		{"missing shares", []byte(`{"v":1}`)},
		{"null shares", []byte(`{"v":1,"shares":null}`)},
		{"shares object", []byte(`{"v":1,"shares":{}}`)},
		{"unknown field", []byte(`{"v":1,"shares":[],"token":"` + secret + `"}`)},
		{"trailing value", []byte(`{"v":1,"shares":[]}{}`)},
		{"duplicate uid", dup},
		{"bad utf-8", []byte("{\"v\":1,\"shares\":[{\"title\":\"bad\xff\"}]}")},
		{"oversized", append([]byte(secret), bytes.Repeat([]byte("x"), maxSharesFile)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSharesRaw(t, dir, tc.body)
			assertShareDamageUntouched(t, dir, tc.body, secret)
		})
	}

	base := shareObject(sampleShare(1))
	fieldCases := []struct {
		name string
		edit func(map[string]any)
	}{
		{"missing uid", func(m map[string]any) { delete(m, "uid") }},
		{"missing node", func(m map[string]any) { delete(m, "node") }},
		{"missing server", func(m map[string]any) { delete(m, "server") }},
		{"missing scope", func(m map[string]any) { delete(m, "scope") }},
		{"missing start", func(m map[string]any) { delete(m, "start") }},
		{"null start", func(m map[string]any) { m["start"] = nil }},
		{"fractional start", func(m map[string]any) { m["start"] = 1.5 }},
		{"missing next", func(m map[string]any) { delete(m, "next") }},
		{"null next", func(m map[string]any) { m["next"] = nil }},
		{"missing start_sent", func(m map[string]any) { delete(m, "start_sent") }},
		{"null start_sent", func(m map[string]any) { m["start_sent"] = nil }},
		{"start_sent string", func(m map[string]any) { m["start_sent"] = "yes" }},
		{"missing generation", func(m map[string]any) { delete(m, "generation") }},
		{"missing members", func(m map[string]any) { delete(m, "members") }},
		{"null members", func(m map[string]any) { m["members"] = nil }},
		{"missing state", func(m map[string]any) { delete(m, "state") }},
		{"missing shared_at", func(m map[string]any) { delete(m, "shared_at") }},
		{"share token", func(m map[string]any) { m["token"] = secret }},
		{"negative start", func(m map[string]any) { m["start"] = -1 }},
		{"next before start", func(m map[string]any) { m["start"] = 4; m["next"] = 2 }},
		{"title newline", func(m map[string]any) { m["title"] = "bad\n" }},
		{"long title", func(m map[string]any) { m["title"] = strings.Repeat("a", 201) }},
		{"bad scope", func(m map[string]any) { m["scope"] = "now" }},
		{"bad state", func(m map[string]any) { m["state"] = "paused" }},
		{"bad time", func(m map[string]any) { m["shared_at"] = "yesterday" }},
		{"member extra", func(m map[string]any) {
			m["members"] = []any{map[string]any{"seat": fmt.Sprintf("%032x", 9), "access": "read", "token": secret}}
		}},
	}
	for _, tc := range fieldCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			obj := map[string]any{}
			for k, v := range base {
				obj[k] = v
			}
			tc.edit(obj)
			body, err := json.Marshal(map[string]any{"v": 1, "shares": []any{obj}})
			if err != nil {
				t.Fatal(err)
			}
			writeSharesRaw(t, dir, body)
			assertShareDamageUntouched(t, dir, body, secret)
		})
	}
	// A document the test itself marshaled is not damage.
	if _, err := parseShares(valid); err != nil {
		t.Fatalf("valid document: %v", err)
	}
}

func shareObject(s Share) map[string]any {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		panic(err)
	}
	return m
}

func assertShareDamageUntouched(t *testing.T, dir string, before []byte, secret string) {
	t.Helper()
	_, err := LoadShares(dir)
	if !errors.Is(err, ErrSharesDamaged) {
		t.Fatalf("Load = %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "Synthetic title") {
		t.Fatalf("Load quotes the file: %v", err)
	}
	if err := SaveShare(dir, sampleShare(4)); !errors.Is(err, ErrSharesDamaged) {
		t.Fatalf("Save = %v", err)
	}
	if err := RemoveShare(dir, sampleShare(1).UID); !errors.Is(err, ErrSharesDamaged) {
		t.Fatalf("Remove = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, SharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("damaged file was rewritten")
	}
	assertOnlyFile(t, dir, SharesFile)
}

func TestSaveShareRejectsAnInvalidShare(t *testing.T) {
	long := strings.Repeat("a", 65)
	members := make([]Member, maxShareMembers+1)
	for i := range members {
		members[i] = Member{Seat: fmt.Sprintf("%032x", i+1), Access: "read"}
	}
	cases := []struct {
		name string
		edit func(*Share)
	}{
		{"empty uid", func(s *Share) { s.UID = "" }},
		{"long uid", func(s *Share) { s.UID = long }},
		{"bad uid", func(s *Share) { s.UID = "bad uid" }},
		{"empty node", func(s *Share) { s.Node = "" }},
		{"long node", func(s *Share) { s.Node = long }},
		{"leading dash", func(s *Share) { s.Node = "-chat" }},
		{"dotted node", func(s *Share) { s.Node = "bad.name" }},
		{"short server", func(s *Share) { s.Server = "abcd" }},
		{"uppercase server", func(s *Share) { s.Server = "0123456789ABCDEF0123456789ABCDEF" }},
		{"bad scope", func(s *Share) { s.Scope = "now" }},
		{"negative start", func(s *Share) { s.Start = -1 }},
		{"next before start", func(s *Share) { s.Start = 4; s.Next = 2 }},
		{"long title", func(s *Share) { s.Title = strings.Repeat("a", 201) }},
		{"title newline", func(s *Share) { s.Title = "bad\n" }},
		{"title utf-8", func(s *Share) { s.Title = "bad\xff" }},
		{"short generation", func(s *Share) { s.Generation = "abcd" }},
		{"uppercase generation", func(s *Share) { s.Generation = "0123456789ABCDEF" }},
		{"too many members", func(s *Share) { s.Members = members }},
		{"bad seat", func(s *Share) { s.Members = []Member{{Seat: "abcd", Access: "read"}} }},
		{"bad access", func(s *Share) { s.Members = []Member{{Seat: sampleShare(1).Server, Access: "owner"}} }},
		{"duplicate seat", func(s *Share) {
			seat := sampleShare(1).Server
			s.Members = []Member{{Seat: seat, Access: "read"}, {Seat: seat, Access: "write"}}
		}},
		{"bad state", func(s *Share) { s.State = "paused" }},
		{"bad time", func(s *Share) { s.SharedAt = "yesterday" }},
		{"offset time", func(s *Share) { s.SharedAt = "2026-11-14T09:30:00+00:00" }},
		{"fractional time", func(s *Share) { s.SharedAt = "2026-11-14T09:30:00.000Z" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			before := shareDocument([]Share{sampleShare(1)})
			writeSharesRaw(t, dir, before)
			s := sampleShare(2)
			s.Title = `North <relay> "a"`
			tc.edit(&s)
			err := SaveShare(dir, s)
			if !errors.Is(err, ErrSharesDamaged) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "North") || strings.Contains(err.Error(), s.Server) {
				t.Fatalf("error quotes the share: %v", err)
			}
			after, err := os.ReadFile(filepath.Join(dir, SharesFile))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("invalid share changed the file")
			}
			assertOnlyFile(t, dir, SharesFile)
		})
	}
	t.Run("no file yet", func(t *testing.T) {
		dir := t.TempDir()
		s := sampleShare(1)
		s.UID = "bad uid"
		if err := SaveShare(dir, s); !errors.Is(err, ErrSharesDamaged) {
			t.Fatalf("error = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, SharesFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat = %v", err)
		}
	})
}

func TestShareWriteFailuresLeaveThePreviousFile(t *testing.T) {
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
			snapshotShares(t)
			dir := t.TempDir()
			before := shareDocument([]Share{sampleShare(1)})
			writeSharesRaw(t, dir, before)
			want := tc.set(storeOps)
			if err := SaveShare(dir, sampleShare(2)); !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
			after, err := os.ReadFile(filepath.Join(dir, SharesFile))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed write changed the file")
			}
			assertOnlyFile(t, dir, SharesFile)
		})
	}
	t.Run("missing directory", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "missing")
		err := SaveShare(missing, sampleShare(1))
		if err == nil || !strings.Contains(err.Error(), "no such file") {
			t.Fatalf("error = %v", err)
		}
		if _, statErr := os.Stat(missing); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("stat = %v", statErr)
		}
	})
}

func TestShareMarshalFailureWritesNothing(t *testing.T) {
	snapshotShares(t)
	boom := errors.New("marshal failed North")
	marshalShares = func(any) ([]byte, error) { return nil, boom }
	dir := t.TempDir()
	before := shareDocument([]Share{sampleShare(1)})
	writeSharesRaw(t, dir, before)
	if err := SaveShare(dir, sampleShare(2)); !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, SharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("marshal failure changed the file")
	}
	assertOnlyFile(t, dir, SharesFile)
}

func TestLoadSharesOfADirectoryIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SharesFile)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := LoadShares(dir)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("Load = %v", err)
	}
	if err := SaveShare(dir, sampleShare(1)); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("Save = %v", err)
	}
	if err := RemoveShare(dir, sampleShare(1).UID); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("Remove = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("directory was replaced")
	}
	assertOnlyFile(t, dir, SharesFile)
}

func TestLoadSharesOfAFileDataDirIsAnError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(file, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadShares(file)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Load = %v", err)
	}
	if err := SaveShare(file, sampleShare(1)); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Save = %v", err)
	}
	if err := RemoveShare(file, sampleShare(1).UID); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Remove = %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "synthetic" {
		t.Fatalf("file = %q", raw)
	}
}

func TestShareLimitsAreOneThousandAndFourMiB(t *testing.T) {
	if maxShares != 1000 || maxSharesFile != 4<<20 {
		t.Fatalf("maxShares = %d file = %d, want 1000 and %d", maxShares, maxSharesFile, 4<<20)
	}
}

func TestLaneOwnedShareStoresAnEmptyMemberList(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		dir := t.TempDir()
		s := sampleShare(1)
		s.Lane = s.UID
		s.LaneRev = 2
		s.Members = nil
		if err := SaveShare(dir, s); err != nil {
			t.Fatal(err)
		}
		got, err := LoadShares(dir)
		if err != nil || len(got) != 1 || got[0].Lane != s.Lane || got[0].LaneRev != 2 || len(got[0].Members) != 0 {
			t.Fatalf("shares = %+v err = %v", got, err)
		}
	})
	t.Run("old file", func(t *testing.T) {
		dir := t.TempDir()
		plain := sampleShare(2)
		raw, err := json.Marshal(sharesFile{V: 1, Shares: []Share{plain}})
		if err != nil {
			t.Fatal(err)
		}
		old := bytes.ReplaceAll(raw, []byte(`,"lane":"","lane_rev":0`), nil)
		writeSharesRaw(t, dir, append(old, '\n'))
		loaded, err := LoadShares(dir)
		if err != nil || len(loaded) != 1 || loaded[0].Lane != "" || loaded[0].LaneRev != 0 || loaded[0].UID != plain.UID {
			t.Fatalf("old file = %+v err = %v", loaded, err)
		}
	})
	t.Run("members", func(t *testing.T) {
		dir := t.TempDir()
		owned := sampleShare(3)
		owned.Lane = owned.UID
		owned.Members = []Member{{Seat: fmt.Sprintf("%032x", 9), Access: "read"}}
		if err := SaveShare(dir, owned); !errors.Is(err, ErrSharesDamaged) {
			t.Fatalf("members on a lane share: %v", err)
		}
	})
	t.Run("rev without lane", func(t *testing.T) {
		dir := t.TempDir()
		loose := sampleShare(4)
		loose.LaneRev = 2
		if err := SaveShare(dir, loose); !errors.Is(err, ErrSharesDamaged) {
			t.Fatalf("rev without a lane: %v", err)
		}
	})
	t.Run("lane id", func(t *testing.T) {
		dir := t.TempDir()
		bad := sampleShare(5)
		bad.Lane = "lane-1"
		bad.Members = []Member{}
		if err := SaveShare(dir, bad); !errors.Is(err, ErrSharesDamaged) {
			t.Fatalf("lane id: %v", err)
		}
	})
}
