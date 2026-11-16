package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// File is the server list inside the data directory.
const File = "team.json"

const maxTeamFile = 64 * 1024

// Entry is one joined team server. Token is a credential.
type Entry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	SeatName string `json:"seat_name"`
	Token    string `json:"token"`
	JoinedAt string `json:"joined_at"`
}

// Sentinel errors from the server file. None of them quote a token.
var (
	ErrDamaged       = errors.New("team: server file is damaged")
	ErrAlreadyJoined = errors.New("team: server already joined")
	ErrNotFound      = errors.New("team: server not joined")
	ErrTooMany       = errors.New("team: server list is full")
)

// storeIO is the filesystem seam. Tests replace individual functions.
type storeIO struct {
	createTemp func(dir string) (*os.File, error)
	chmod      func(*os.File, os.FileMode) error
	write      func(*os.File, []byte) (int, error)
	syncFile   func(*os.File) error
	close      func(*os.File) error
	rename     func(oldname, newname string) error
	remove     func(name string) error
}

var storeOps = storeIO{
	createTemp: func(dir string) (*os.File, error) {
		return os.CreateTemp(dir, ".team-*.tmp")
	},
	chmod:    func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) },
	write:    func(f *os.File, b []byte) (int, error) { return f.Write(b) },
	syncFile: func(f *os.File) error { return f.Sync() },
	close:    func(f *os.File) error { return f.Close() },
	rename:   os.Rename,
	remove:   os.Remove,
}

// marshalTeam encodes the server file. Tests replace it.
var marshalTeam = json.Marshal

const maxServers = 16

type teamDocument struct {
	V       int      `json:"v"`
	Servers *[]Entry `json:"servers"`
}

type teamFile struct {
	V       int     `json:"v"`
	Servers []Entry `json:"servers"`
}

// Load reads the server file. A missing file is an empty list and is not created.
func Load(dataDir string) ([]Entry, error) {
	f, err := os.Open(filepath.Join(dataDir, File))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Entry{}, nil
		}
		return nil, fmt.Errorf("team: open server file: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxTeamFile+1))
	if err != nil {
		return nil, fmt.Errorf("team: read server file: %w", err)
	}
	if len(raw) > maxTeamFile {
		return nil, ErrDamaged
	}
	return parseTeamFile(raw)
}

// Add inserts one server at the end of the list.
func Add(dataDir string, e Entry) error {
	if !validEntry(e) {
		return ErrDamaged
	}
	entries, err := Load(dataDir)
	if err != nil {
		return err
	}
	for _, old := range entries {
		if old.ID == e.ID {
			return ErrAlreadyJoined
		}
	}
	if len(entries) >= maxServers {
		return ErrTooMany
	}
	return writeEntries(dataDir, append(entries, e))
}

// Remove drops the server with the given ID. The last server leaves an empty list.
func Remove(dataDir, serverID string) error {
	entries, err := Load(dataDir)
	if err != nil {
		return err
	}
	next := make([]Entry, 0, len(entries))
	found := false
	for _, e := range entries {
		if e.ID == serverID {
			found = true
			continue
		}
		next = append(next, e)
	}
	if !found {
		return ErrNotFound
	}
	return writeEntries(dataDir, next)
}

func parseTeamFile(raw []byte) ([]Entry, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc teamDocument
	if err := dec.Decode(&doc); err != nil {
		return nil, ErrDamaged
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, ErrDamaged
	}
	if doc.V != 1 || doc.Servers == nil {
		return nil, ErrDamaged
	}
	seen := make(map[string]struct{}, len(*doc.Servers))
	for _, e := range *doc.Servers {
		if !validEntry(e) {
			return nil, ErrDamaged
		}
		if _, ok := seen[e.ID]; ok {
			return nil, ErrDamaged
		}
		seen[e.ID] = struct{}{}
	}
	return *doc.Servers, nil
}

func writeEntries(dir string, entries []Entry) error {
	body, err := marshalTeam(teamFile{V: 1, Servers: entries})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return publishFile(dir, File, "server file", body)
}

func validEntry(e Entry) bool {
	return validHex(e.ID, 32) &&
		validStoredName(e.Name, false) &&
		validBaseURL(e.URL) &&
		validStoredName(e.SeatName, true) &&
		validHex(e.Token, 64) &&
		validJoinedAt(e.JoinedAt)
}

func validStoredName(name string, trim bool) bool {
	if name == "" || len(name) > 64 || !utf8.ValidString(name) {
		return false
	}
	if trim && strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.HasSuffix(u.Host, ":") || u.User != nil {
		return false
	}
	if u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	return raw == u.Scheme+"://"+u.Host
}

func validJoinedAt(s string) bool {
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return false
	}
	return parsed.UTC().Format(time.RFC3339) == s
}
