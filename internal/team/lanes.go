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
	"unicode"
	"unicode/utf8"
)

// LaneSharesFile is the lane share list inside the data directory.
const LaneSharesFile = "team-lane-shares.json"

const (
	maxLaneShares     = 64
	maxLaneSharesFile = 1 << 20
	maxLaneName       = 80
)

// LaneShare is one lane this seat publishes. It holds no message text and no token.
// ID is the index channel. It is never the local lane ID.
type LaneShare struct {
	ID         string         `json:"id"`
	Lane       string         `json:"lane"`
	Server     string         `json:"server"`
	Scope      string         `json:"scope"`
	Name       string         `json:"name"`
	Color      string         `json:"color"`
	Members    []Member       `json:"members"`
	Rev        int            `json:"rev"`
	Starts     map[string]int `json:"starts"`
	Excluded   []string       `json:"excluded"`
	Generation string         `json:"generation"`
	IndexRev   int            `json:"index_rev"`
	IndexKey   int            `json:"index_key"`
	IndexHash  string         `json:"index_hash"`
	CheckedAt  string         `json:"checked_at"`
	State      string         `json:"state"`
	SharedAt   string         `json:"shared_at"`
}

// Sentinel errors from the lane share file. None of them quote a name or a token.
var (
	ErrLaneSharesDamaged = errors.New("team: lane share file is damaged")
	ErrTooManyLaneShares = errors.New("team: lane share list is full")
	ErrLaneShareExists   = errors.New("team: lane already shared")
)

var marshalLaneShares = json.Marshal

type laneSharesFile struct {
	V     int         `json:"v"`
	Lanes []LaneShare `json:"lanes"`
}

type laneWire struct {
	ID         string          `json:"id"`
	Lane       string          `json:"lane"`
	Server     string          `json:"server"`
	Scope      string          `json:"scope"`
	Name       string          `json:"name"`
	Color      string          `json:"color"`
	Members    *[]Member       `json:"members"`
	Rev        *int            `json:"rev"`
	Starts     *map[string]int `json:"starts"`
	Excluded   *[]string       `json:"excluded"`
	Generation string          `json:"generation"`
	IndexRev   *int            `json:"index_rev"`
	IndexKey   *int            `json:"index_key"`
	IndexHash  *string         `json:"index_hash"`
	CheckedAt  *string         `json:"checked_at"`
	State      string          `json:"state"`
	SharedAt   string          `json:"shared_at"`
}

// LoadLaneShares reads the lane share file. A missing file is an empty list and is not created.
func LoadLaneShares(dataDir string) ([]LaneShare, error) {
	f, err := os.Open(filepath.Join(dataDir, LaneSharesFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []LaneShare{}, nil
		}
		return nil, fmt.Errorf("team: open lane share file: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxLaneSharesFile+1))
	if err != nil {
		return nil, fmt.Errorf("team: read lane share file: %w", err)
	}
	if len(raw) > maxLaneSharesFile {
		return nil, ErrLaneSharesDamaged
	}
	return parseLaneShares(raw)
}

// SaveLaneShare inserts a lane share, or replaces the one with the same ID.
// A second share of the same local lane is ErrLaneShareExists.
func SaveLaneShare(dataDir string, s LaneShare) error {
	if s.Members == nil {
		s.Members = []Member{}
	}
	if s.Excluded == nil {
		s.Excluded = []string{}
	}
	if !validLaneShare(s) {
		return ErrLaneSharesDamaged
	}
	lanes, err := LoadLaneShares(dataDir)
	if err != nil {
		return err
	}
	replaced := false
	for i := range lanes {
		if lanes[i].Lane == s.Lane && lanes[i].ID != s.ID {
			return ErrLaneShareExists
		}
		if lanes[i].ID == s.ID {
			lanes[i] = s
			replaced = true
		}
	}
	if !replaced {
		if len(lanes) >= maxLaneShares {
			return ErrTooManyLaneShares
		}
		lanes = append(lanes, s)
	}
	return writeLaneShares(dataDir, lanes)
}

// RemoveLaneShare drops the lane share with the given index-channel ID.
// ErrNotFound when the file is missing or has no such ID.
func RemoveLaneShare(dataDir, id string) error {
	lanes, err := LoadLaneShares(dataDir)
	if err != nil {
		return err
	}
	next := make([]LaneShare, 0, len(lanes))
	found := false
	for _, s := range lanes {
		if s.ID == id {
			found = true
			continue
		}
		next = append(next, s)
	}
	if !found {
		return ErrNotFound
	}
	return writeLaneShares(dataDir, next)
}

func parseLaneShares(raw []byte) ([]LaneShare, error) {
	if !utf8.Valid(raw) {
		return nil, ErrLaneSharesDamaged
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc struct {
		V     int         `json:"v"`
		Lanes *[]laneWire `json:"lanes"`
	}
	if err := dec.Decode(&doc); err != nil {
		return nil, ErrLaneSharesDamaged
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, ErrLaneSharesDamaged
	}
	if doc.V != 1 || doc.Lanes == nil {
		return nil, ErrLaneSharesDamaged
	}
	out := make([]LaneShare, 0, len(*doc.Lanes))
	seenID := make(map[string]struct{}, len(*doc.Lanes))
	seenLane := make(map[string]struct{}, len(*doc.Lanes))
	for _, w := range *doc.Lanes {
		s, ok := laneFromWire(w)
		if !ok {
			return nil, ErrLaneSharesDamaged
		}
		if _, dup := seenID[s.ID]; dup {
			return nil, ErrLaneSharesDamaged
		}
		if _, dup := seenLane[s.Lane]; dup {
			return nil, ErrLaneSharesDamaged
		}
		seenID[s.ID] = struct{}{}
		seenLane[s.Lane] = struct{}{}
		out = append(out, s)
	}
	return out, nil
}

func laneFromWire(w laneWire) (LaneShare, bool) {
	if w.Members == nil || w.Rev == nil || w.Starts == nil || w.Excluded == nil || w.IndexRev == nil || w.IndexKey == nil || w.IndexHash == nil || w.CheckedAt == nil {
		return LaneShare{}, false
	}
	members := make([]Member, len(*w.Members))
	copy(members, *w.Members)
	starts := make(map[string]int, len(*w.Starts))
	for node, n := range *w.Starts {
		starts[node] = n
	}
	excluded := make([]string, len(*w.Excluded))
	copy(excluded, *w.Excluded)
	s := LaneShare{
		ID: w.ID, Lane: w.Lane, Server: w.Server, Scope: w.Scope,
		Name: w.Name, Color: w.Color, Members: members, Rev: *w.Rev,
		Starts: starts, Excluded: excluded, Generation: w.Generation,
		IndexRev: *w.IndexRev, IndexKey: *w.IndexKey, IndexHash: *w.IndexHash,
		CheckedAt: *w.CheckedAt, State: w.State, SharedAt: w.SharedAt,
	}
	if !validLaneShare(s) {
		return LaneShare{}, false
	}
	return s, true
}

func validLaneShare(s LaneShare) bool {
	if !validHex(s.ID, 16) || !validLaneID(s.Lane) || s.ID == s.Lane || !validHex(s.Server, 32) {
		return false
	}
	if s.Scope != "from_now" && s.Scope != "all" {
		return false
	}
	if !validLaneName(s.Name) || !validLaneColor(s.Color) || !validShareMembers(s.Members) {
		return false
	}
	if s.Rev < 1 || s.IndexRev < 0 || s.IndexKey < 0 {
		return false
	}
	if !validStarts(s.Starts) || !validExcluded(s.Excluded) || !validHex(s.Generation, 16) {
		return false
	}
	if !validIndexHash(s.IndexHash) || !validCheckedAt(s.CheckedAt) {
		return false
	}
	if s.State != "active" && s.State != "stopping" && s.State != "lost" && s.State != "full" {
		return false
	}
	return validJoinedAt(s.SharedAt)
}

func validLaneID(s string) bool {
	if len(s) < 1 || len(s) > maxLaneName {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

func validLaneName(s string) bool {
	if len(s) < 1 || len(s) > maxLaneName || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validLaneColor(s string) bool {
	return len(s) == 7 && s[0] == '#' && validHex(s[1:], 6)
}

func validIndexHash(s string) bool {
	return s == "" || validHex(s, 64)
}

func validCheckedAt(s string) bool {
	return s == "" || validJoinedAt(s)
}

func validStarts(starts map[string]int) bool {
	if starts == nil {
		return false
	}
	for node, n := range starts {
		if !validNodeID(node) || n < 0 {
			return false
		}
	}
	return true
}

func validExcluded(ids []string) bool {
	if ids == nil {
		return false
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !validNodeID(id) {
			return false
		}
		if _, ok := seen[id]; ok {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

func writeLaneShares(dir string, lanes []LaneShare) error {
	body, err := marshalLaneShares(laneSharesFile{V: 1, Lanes: lanes})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if len(body) > maxLaneSharesFile {
		return ErrTooManyLaneShares
	}
	return publishFile(dir, LaneSharesFile, "lane share file", body)
}
