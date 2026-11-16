package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"unicode/utf8"
)

const PeopleFile = "team-people.json"
const maxPeople = 4096
const maxPeopleFile = 1 << 20

var ErrPeopleDamaged = errors.New("team: people file is damaged")

type Person struct {
	Server string `json:"server"`
	Seat   string `json:"seat"`
	Name   string `json:"name"`
}

func LoadPeople(dir string) ([]Person, error) {
	f, err := os.Open(filepath.Join(dir, PeopleFile))
	if errors.Is(err, fs.ErrNotExist) {
		return []Person{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("team: open people file: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxPeopleFile+1))
	if err != nil {
		return nil, fmt.Errorf("team: read people file: %w", err)
	}
	if len(raw) > maxPeopleFile || !utf8.Valid(raw) {
		return nil, ErrPeopleDamaged
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc struct {
		V      *int      `json:"v"`
		People *[]Person `json:"people"`
	}
	if err := dec.Decode(&doc); err != nil {
		return nil, ErrPeopleDamaged
	}
	var extra json.RawMessage
	if dec.Decode(&extra) != io.EOF {
		return nil, ErrPeopleDamaged
	}
	if doc.V == nil || *doc.V != 1 || doc.People == nil || !validPeople(*doc.People) {
		return nil, ErrPeopleDamaged
	}
	return *doc.People, nil
}

func validPeople(people []Person) bool {
	if len(people) > maxPeople {
		return false
	}
	seen := make(map[[2]string]struct{}, len(people))
	for _, p := range people {
		if !validHex(p.Server, 32) || !validHex(p.Seat, 32) || !validStoredName(p.Name, true) {
			return false
		}
		key := [2]string{p.Server, p.Seat}
		if _, ok := seen[key]; ok {
			return false
		}
		seen[key] = struct{}{}
	}
	return true
}

func SavePeople(dir string, people []Person) error {
	if _, err := LoadPeople(dir); err != nil {
		return err
	}
	if !validPeople(people) {
		return ErrPeopleDamaged
	}
	sorted := append([]Person{}, people...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Server != sorted[j].Server {
			return sorted[i].Server < sorted[j].Server
		}
		return sorted[i].Seat < sorted[j].Seat
	})
	body, err := json.Marshal(struct {
		V      int      `json:"v"`
		People []Person `json:"people"`
	}{1, sorted})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if len(body) > maxPeopleFile {
		return ErrPeopleDamaged
	}
	return publishFile(dir, PeopleFile, "people file", body)
}
