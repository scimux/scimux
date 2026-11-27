package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sampleLane(n int) LaneShare {
	return LaneShare{
		ID:         fmt.Sprintf("%016x", n),
		Lane:       fmt.Sprintf("lane-%d", n),
		Server:     fmt.Sprintf("%032x", n),
		Scope:      "from_now",
		Name:       "Protein",
		Color:      "#007aff",
		Members:    []Member{{Seat: fmt.Sprintf("%032x", n+1000), Access: "read"}},
		Rev:        1,
		Starts:     map[string]int{"chat-1": 4},
		Excluded:   []string{"chat-2"},
		Generation: fmt.Sprintf("%016x", n),
		IndexRev:   1,
		IndexKey:   0,
		IndexHash:  "",
		CheckedAt:  "",
		State:      "active",
		SharedAt:   "2026-11-14T09:30:00Z",
	}
}

func richLane() LaneShare {
	s := sampleLane(1)
	s.Lane = "Protein.folding_1"
	s.Scope = "all"
	s.Name = strings.Repeat("ä", 20)
	s.Color = "#aabbcc"
	s.Members = []Member{
		{Seat: fmt.Sprintf("%032x", 2), Access: "read"},
		{Seat: fmt.Sprintf("%032x", 3), Access: "write"},
	}
	s.Rev = 3
	s.Starts = map[string]int{"Chat_1": 0, "chat-9": 12}
	s.Excluded = []string{}
	s.IndexRev = 2
	s.IndexKey = 7
	s.IndexHash = strings.Repeat("ab", 32)
	s.CheckedAt = "2026-11-14T09:31:00Z"
	s.State = "stopping"
	return s
}

func laneDocument(lanes []LaneShare) []byte {
	raw, err := json.Marshal(laneSharesFile{V: 1, Lanes: lanes})
	if err != nil {
		panic(err)
	}
	return append(raw, '\n')
}

func writeLanesRaw(t *testing.T, dir string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, LaneSharesFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadLaneSharesMissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := LoadLaneShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("lanes = %#v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, LaneSharesFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v", err)
	}
}

func TestLaneStoreRoundTripKeepsFieldsAndMode(t *testing.T) {
	dir := t.TempDir()
	first := richLane()
	second := sampleLane(2)
	second.Members = nil
	second.Excluded = nil
	second.Starts = map[string]int{}
	second.State = "full"
	second.Scope = "from_now"
	second.Name = "A"
	if err := SaveLaneShare(dir, first); err != nil {
		t.Fatal(err)
	}
	if err := SaveLaneShare(dir, second); err != nil {
		t.Fatal(err)
	}
	got, err := LoadLaneShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	second.Members = []Member{}
	second.Excluded = []string{}
	if len(got) != 2 || !reflect.DeepEqual(got[0], first) || !reflect.DeepEqual(got[1], second) {
		t.Fatalf("lanes = %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, LaneSharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	raw, err := os.ReadFile(filepath.Join(dir, LaneSharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("token")) {
		t.Fatalf("file holds a token: %s", raw)
	}
}

func TestSaveLaneShareReplacesByID(t *testing.T) {
	dir := t.TempDir()
	if err := SaveLaneShare(dir, sampleLane(1)); err != nil {
		t.Fatal(err)
	}
	if err := SaveLaneShare(dir, sampleLane(2)); err != nil {
		t.Fatal(err)
	}
	updated := sampleLane(1)
	updated.Name = "Renamed"
	updated.Rev = 4
	updated.State = "lost"
	if err := SaveLaneShare(dir, updated); err != nil {
		t.Fatal(err)
	}
	got, err := LoadLaneShares(dir)
	if err != nil || len(got) != 2 || !reflect.DeepEqual(got[0], updated) || got[1].Lane != sampleLane(2).Lane {
		t.Fatalf("lanes = %+v err = %v", got, err)
	}
}

func TestSaveLaneShareRejectsASecondShareOfTheSameLane(t *testing.T) {
	dir := t.TempDir()
	if err := SaveLaneShare(dir, sampleLane(1)); err != nil {
		t.Fatal(err)
	}
	other := sampleLane(2)
	other.Lane = sampleLane(1).Lane
	before, err := os.ReadFile(filepath.Join(dir, LaneSharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveLaneShare(dir, other); !errors.Is(err, ErrLaneShareExists) {
		t.Fatalf("error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, LaneSharesFile))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("duplicate lane changed the file")
	}
}

func TestSaveLaneShareRejectsThe65th(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= maxLaneShares; i++ {
		if err := SaveLaneShare(dir, sampleLane(i)); err != nil {
			t.Fatalf("lane %d: %v", i, err)
		}
	}
	before, err := os.ReadFile(filepath.Join(dir, LaneSharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveLaneShare(dir, sampleLane(maxLaneShares+1)); !errors.Is(err, ErrTooManyLaneShares) {
		t.Fatalf("error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, LaneSharesFile))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("the rejected lane changed the file")
	}
	updated := sampleLane(1)
	updated.Name = "Still room"
	if err := SaveLaneShare(dir, updated); err != nil {
		t.Fatal(err)
	}
	got, err := LoadLaneShares(dir)
	if err != nil || len(got) != maxLaneShares || got[0].Name != "Still room" {
		t.Fatalf("len = %d err = %v", len(got), err)
	}
}

func TestSaveLaneShareRejectsAnOversizedDocument(t *testing.T) {
	dir := t.TempDir()
	if err := SaveLaneShare(dir, sampleLane(1)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, LaneSharesFile))
	if err != nil {
		t.Fatal(err)
	}
	prev := marshalLaneShares
	t.Cleanup(func() { marshalLaneShares = prev })
	marshalLaneShares = func(any) ([]byte, error) {
		return bytes.Repeat([]byte("x"), maxLaneSharesFile), nil
	}
	if err := SaveLaneShare(dir, sampleLane(2)); !errors.Is(err, ErrTooManyLaneShares) {
		t.Fatalf("error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, LaneSharesFile))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("oversized write changed the file")
	}
}

func TestRemoveLaneShareDropsOne(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []int{1, 2, 3} {
		if err := SaveLaneShare(dir, sampleLane(n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveLaneShare(dir, sampleLane(2).ID); err != nil {
		t.Fatal(err)
	}
	got, err := LoadLaneShares(dir)
	if err != nil || len(got) != 2 || got[0].ID != sampleLane(1).ID || got[1].ID != sampleLane(3).ID {
		t.Fatalf("lanes = %+v err = %v", got, err)
	}
	if err := RemoveLaneShare(dir, sampleLane(2).ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
	empty := t.TempDir()
	if err := RemoveLaneShare(empty, sampleLane(1).ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(empty, LaneSharesFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat = %v", err)
	}
}

func TestSaveLaneShareRejectsABadValueAndWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		edit func(*LaneShare)
	}{
		{"short id", func(s *LaneShare) { s.ID = "abcd" }},
		{"id is the lane", func(s *LaneShare) { s.Lane = s.ID }},
		{"bad lane", func(s *LaneShare) { s.Lane = "lane/1" }},
		{"empty lane", func(s *LaneShare) { s.Lane = "" }},
		{"long lane", func(s *LaneShare) { s.Lane = strings.Repeat("a", 81) }},
		{"bad server", func(s *LaneShare) { s.Server = "zz" }},
		{"bad scope", func(s *LaneShare) { s.Scope = "now" }},
		{"empty name", func(s *LaneShare) { s.Name = "" }},
		{"long name", func(s *LaneShare) { s.Name = strings.Repeat("a", 81) }},
		{"name control", func(s *LaneShare) { s.Name = "bad\n" }},
		{"name bytes", func(s *LaneShare) { s.Name = "bad\xff" }},
		{"bad color", func(s *LaneShare) { s.Color = "#AABBCC" }},
		{"short color", func(s *LaneShare) { s.Color = "#abc" }},
		{"rev zero", func(s *LaneShare) { s.Rev = 0 }},
		{"negative index rev", func(s *LaneShare) { s.IndexRev = -1 }},
		{"negative index key", func(s *LaneShare) { s.IndexKey = -1 }},
		{"nil starts", func(s *LaneShare) { s.Starts = nil }},
		{"bad start node", func(s *LaneShare) { s.Starts = map[string]int{"bad node": 1} }},
		{"negative start", func(s *LaneShare) { s.Starts = map[string]int{"chat-1": -1} }},
		{"dup excluded", func(s *LaneShare) { s.Excluded = []string{"chat-1", "chat-1"} }},
		{"bad excluded", func(s *LaneShare) { s.Excluded = []string{"bad node"} }},
		{"bad generation", func(s *LaneShare) { s.Generation = "short" }},
		{"bad hash", func(s *LaneShare) { s.IndexHash = "abc" }},
		{"bad checked", func(s *LaneShare) { s.CheckedAt = "yesterday" }},
		{"bad state", func(s *LaneShare) { s.State = "paused" }},
		{"bad time", func(s *LaneShare) { s.SharedAt = "yesterday" }},
		{"dup member", func(s *LaneShare) {
			seat := fmt.Sprintf("%032x", 9)
			s.Members = []Member{{Seat: seat, Access: "read"}, {Seat: seat, Access: "write"}}
		}},
		{"too many members", func(s *LaneShare) {
			s.Members = make([]Member, maxShareMembers+1)
			for i := range s.Members {
				s.Members[i] = Member{Seat: fmt.Sprintf("%032x", i+1), Access: "read"}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := sampleLane(1)
			tc.edit(&s)
			if err := SaveLaneShare(dir, s); !errors.Is(err, ErrLaneSharesDamaged) {
				t.Fatalf("error = %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, LaneSharesFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stat = %v", err)
			}
		})
	}
	// The edges that are valid.
	dir := t.TempDir()
	ok := sampleLane(1)
	ok.Name = strings.Repeat("a", 80)
	ok.Lane = strings.Repeat("b", 80)
	ok.Members = make([]Member, maxShareMembers)
	for i := range ok.Members {
		ok.Members[i] = Member{Seat: fmt.Sprintf("%032x", i+1), Access: "write"}
	}
	ok.IndexHash = strings.Repeat("cd", 32)
	if err := SaveLaneShare(dir, ok); err != nil {
		t.Fatal(err)
	}
}

func TestDamagedLaneFileIsLeftUntouched(t *testing.T) {
	secret := strings.Repeat("ab", 32)
	valid := laneDocument([]LaneShare{sampleLane(1)})
	dupID := laneDocument([]LaneShare{sampleLane(1), sampleLane(1)})
	sameLane := sampleLane(2)
	sameLane.Lane = sampleLane(1).Lane
	dupLane := laneDocument([]LaneShare{sampleLane(1), sameLane})
	cases := []struct {
		name string
		body []byte
	}{
		{"empty", []byte{}},
		{"not json", []byte("nope")},
		{"version 2", []byte(`{"v":2,"lanes":[]}`)},
		{"missing lanes", []byte(`{"v":1}`)},
		{"null lanes", []byte(`{"v":1,"lanes":null}`)},
		{"unknown field", []byte(`{"v":1,"lanes":[],"token":"` + secret + `"}`)},
		{"trailing value", []byte(`{"v":1,"lanes":[]}{}`)},
		{"bad utf-8", []byte("{\"v\":1,\"lanes\":[{\"name\":\"bad\xff\"}]}")},
		{"duplicate id", dupID},
		{"duplicate lane", dupLane},
		{"oversized", append([]byte(secret), bytes.Repeat([]byte("x"), maxLaneSharesFile)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeLanesRaw(t, dir, tc.body)
			_, err := LoadLaneShares(dir)
			if !errors.Is(err, ErrLaneSharesDamaged) {
				t.Fatalf("Load = %v", err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "Protein") {
				t.Fatalf("Load quotes the file: %v", err)
			}
			if err := SaveLaneShare(dir, sampleLane(4)); !errors.Is(err, ErrLaneSharesDamaged) {
				t.Fatalf("Save = %v", err)
			}
			if err := RemoveLaneShare(dir, sampleLane(1).ID); !errors.Is(err, ErrLaneSharesDamaged) {
				t.Fatalf("Remove = %v", err)
			}
			after, err := os.ReadFile(filepath.Join(dir, LaneSharesFile))
			if err != nil || !bytes.Equal(tc.body, after) {
				t.Fatal("damaged file was rewritten")
			}
		})
	}
	if _, err := parseLaneShares(valid); err != nil {
		t.Fatalf("valid document: %v", err)
	}
	base := laneObject(sampleLane(1))
	edits := []struct {
		name string
		edit func(map[string]any)
	}{
		{"missing rev", func(m map[string]any) { delete(m, "rev") }},
		{"null starts", func(m map[string]any) { m["starts"] = nil }},
		{"fractional start", func(m map[string]any) { m["starts"] = map[string]any{"chat-1": 1.5} }},
		{"missing hash", func(m map[string]any) { delete(m, "index_hash") }},
		{"member extra", func(m map[string]any) {
			m["members"] = []any{map[string]any{"seat": fmt.Sprintf("%032x", 9), "access": "read", "token": secret}}
		}},
	}
	for _, tc := range edits {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			obj := map[string]any{}
			for k, v := range base {
				obj[k] = v
			}
			tc.edit(obj)
			body, err := json.Marshal(map[string]any{"v": 1, "lanes": []any{obj}})
			if err != nil {
				t.Fatal(err)
			}
			writeLanesRaw(t, dir, body)
			_, err = LoadLaneShares(dir)
			if !errors.Is(err, ErrLaneSharesDamaged) {
				t.Fatalf("Load = %v", err)
			}
			after, err := os.ReadFile(filepath.Join(dir, LaneSharesFile))
			if err != nil || !bytes.Equal(body, after) {
				t.Fatal("damaged file was rewritten")
			}
		})
	}
}

func laneObject(s LaneShare) map[string]any {
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

func TestLaneShareReadOfADirectoryIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, LaneSharesFile)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := LoadLaneShares(dir)
	if err == nil || !strings.Contains(err.Error(), "is a directory") && !strings.Contains(err.Error(), "read lane share file") {
		t.Fatalf("Load = %v", err)
	}
}

func TestLaneShareMarshalFailureWritesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := SaveLaneShare(dir, sampleLane(1)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, LaneSharesFile))
	if err != nil {
		t.Fatal(err)
	}
	prev := marshalLaneShares
	t.Cleanup(func() { marshalLaneShares = prev })
	boom := errors.New("marshal failed")
	marshalLaneShares = func(any) ([]byte, error) { return nil, boom }
	if err := SaveLaneShare(dir, sampleLane(2)); !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, LaneSharesFile))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("marshal failure changed the file")
	}
}
