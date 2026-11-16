package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPeopleContractGenerated(t *testing.T) {
	const server = "11111111111111111111111111111111"
	const seat = "22222222222222222222222222222222"
	t.Run("missing and sorted", func(t *testing.T) {
		dir := t.TempDir()
		got, err := LoadPeople(dir)
		if err != nil || len(got) != 0 {
			t.Fatalf("missing: %v %v", got, err)
		}
		rows := []Person{{Server: server, Seat: seat, Name: "Ada"}, {Server: server, Seat: "00000000000000000000000000000000", Name: "Bea"}}
		if err := SavePeople(dir, rows); err != nil {
			t.Fatal(err)
		}
		got, err = LoadPeople(dir)
		if err != nil || len(got) != 2 || got[0].Name != "Bea" {
			t.Fatalf("sorted: %v %v", got, err)
		}
		info, err := os.Stat(filepath.Join(dir, PeopleFile))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("permissions")
		}
	})
	for name, raw := range map[string]string{
		"empty": "", "json": "{", "version": "{\"v\":2,\"people\":[]}", "missing version": "{\"people\":[]}",
		"missing people": "{\"v\":1}", "null people": "{\"v\":1,\"people\":null}", "unknown": "{\"v\":1,\"people\":[],\"x\":1}", "extra": "{\"v\":1,\"people\":[]} {}",
		"server":               "{\"v\":1,\"people\":[{\"server\":\"bad\",\"seat\":\"" + seat + "\",\"name\":\"Ada\"}]}",
		"seat":                 "{\"v\":1,\"people\":[{\"server\":\"" + server + "\",\"seat\":\"bad\",\"name\":\"Ada\"}]}",
		"name":                 "{\"v\":1,\"people\":[{\"server\":\"" + server + "\",\"seat\":\"" + seat + "\",\"name\":\" Ada \"}]}",
		"missing name":         "{\"v\":1,\"people\":[{\"server\":\"" + server + "\",\"seat\":\"" + seat + "\"}]}",
		"empty name":           "{\"v\":1,\"people\":[{\"server\":\"" + server + "\",\"seat\":\"" + seat + "\",\"name\":\"\"}]}",
		"control name":         "{\"v\":1,\"people\":[{\"server\":\"" + server + "\",\"seat\":\"" + seat + "\",\"name\":\"a\\nb\"}]}",
		"long name":            "{\"v\":1,\"people\":[{\"server\":\"" + server + "\",\"seat\":\"" + seat + "\",\"name\":\"" + strings.Repeat("a", 65) + "\"}]}",
		"unknown person field": "{\"v\":1,\"people\":[{\"server\":\"" + server + "\",\"seat\":\"" + seat + "\",\"name\":\"Ada\",\"extra\":1}]}",
		"duplicate":            "{\"v\":1,\"people\":[{\"server\":\"" + server + "\",\"seat\":\"" + seat + "\",\"name\":\"Ada\"},{\"server\":\"" + server + "\",\"seat\":\"" + seat + "\",\"name\":\"Ada\"}]}",
		"large":                strings.Repeat(" ", (1<<20)+1), "utf8": string([]byte{0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, PeopleFile)
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadPeople(dir); !errors.Is(err, ErrPeopleDamaged) {
				t.Fatalf("load: %v", err)
			}
			if err := SavePeople(dir, nil); err == nil {
				t.Fatal("overwrote damaged file")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, []byte(raw)) {
				t.Fatal("damage overwritten")
			}
		})
	}
	t.Run("limit", func(t *testing.T) {
		dir := t.TempDir()
		rows := make([]Person, 4097)
		for i := range rows {
			rows[i] = Person{Server: server, Seat: fmt.Sprintf("%032x", i), Name: "Ada"}
		}
		if err := SavePeople(dir, rows); err == nil {
			t.Fatal("accepted too many")
		}
		raw, err := json.Marshal(struct {
			V      int      `json:"v"`
			People []Person `json:"people"`
		}{1, rows})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, PeopleFile), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPeople(dir); !errors.Is(err, ErrPeopleDamaged) {
			t.Fatalf("loaded too many: %v", err)
		}
	})
}

func TestPeopleStorageFailuresAndSize(t *testing.T) {
	t.Run("open fails", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPeople(file); err == nil {
			t.Fatal("open succeeded")
		}
		if err := SavePeople(file, nil); err == nil {
			t.Fatal("save succeeded")
		}
	})
	t.Run("read fails", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, PeopleFile), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPeople(dir); err == nil {
			t.Fatal("directory read succeeded")
		}
	})
	t.Run("output too large", func(t *testing.T) {
		rows := make([]Person, 4096)
		for i := range rows {
			rows[i] = Person{Server: fmt.Sprintf("%032x", i), Seat: fmt.Sprintf("%032x", i), Name: strings.Repeat("<", 64)}
		}
		if err := SavePeople(t.TempDir(), rows); !errors.Is(err, ErrPeopleDamaged) {
			t.Fatalf("size: %v", err)
		}
	})
	t.Run("server sort", func(t *testing.T) {
		dir := t.TempDir()
		rows := []Person{{Server: strings.Repeat("f", 32), Seat: strings.Repeat("0", 32), Name: "First"}, {Server: strings.Repeat("0", 32), Seat: strings.Repeat("0", 32), Name: "Second"}}
		if err := SavePeople(dir, rows); err != nil {
			t.Fatal(err)
		}
		got, err := LoadPeople(dir)
		if err != nil || got[0].Name != "Second" {
			t.Fatalf("order: %v %v", got, err)
		}
	})
}

func TestPeopleMarshalFailureLeavesOldFile(t *testing.T) {
	dir := t.TempDir()
	row := Person{Server: strings.Repeat("1", 32), Seat: strings.Repeat("2", 32), Name: "Ada"}
	if err := SavePeople(dir, []Person{row}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, PeopleFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	previous := marshalTeam
	defer func() { marshalTeam = previous }()
	boom := errors.New("synthetic encoding failure")
	marshalTeam = func(any) ([]byte, error) { return nil, boom }
	if err := SavePeople(dir, []Person{row}); !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed encoding changed file")
	}
}
