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
	"unicode/utf8"
)

// SharesFile is the share list inside the data directory.
const SharesFile = "team-shares.json"

const (
	maxShares       = 256
	maxSharesFile   = 1 << 20
	maxShareMembers = 200
)

// Share is one chat this seat publishes. It holds no message text and no token.
type Share struct {
	UID        string   `json:"uid"`
	Node       string   `json:"node"`
	Server     string   `json:"server"`
	Scope      string   `json:"scope"`
	Start      int      `json:"start"`
	Next       int      `json:"next"`
	StartSent  bool     `json:"start_sent"`
	Title      string   `json:"title"`
	Generation string   `json:"generation"`
	Members    []Member `json:"members"`
	Lane       string   `json:"-"`
	LaneRev    int      `json:"-"`
	State      string   `json:"state"`
	SharedAt   string   `json:"shared_at"`
}

// Sentinel errors from the share file. None of them quote a title or a token.
var (
	ErrSharesDamaged = errors.New("team: share file is damaged")
	ErrTooManyShares = errors.New("team: share list is full")
)

// marshalShares encodes the share file. Tests replace it.
var marshalShares = json.Marshal

type sharesFile struct {
	V      int     `json:"v"`
	Shares []Share `json:"shares"`
}

type shareWire struct {
	UID        string    `json:"uid"`
	Node       string    `json:"node"`
	Server     string    `json:"server"`
	Scope      string    `json:"scope"`
	Start      *int      `json:"start"`
	Next       *int      `json:"next"`
	StartSent  *bool     `json:"start_sent"`
	Title      string    `json:"title"`
	Generation string    `json:"generation"`
	Members    *[]Member `json:"members"`
	State      string    `json:"state"`
	SharedAt   string    `json:"shared_at"`
}

// LoadShares reads the share file. A missing file is an empty list and is not created.
func LoadShares(dataDir string) ([]Share, error) {
	f, err := os.Open(filepath.Join(dataDir, SharesFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Share{}, nil
		}
		return nil, fmt.Errorf("team: open share file: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxSharesFile+1))
	if err != nil {
		return nil, fmt.Errorf("team: read share file: %w", err)
	}
	if len(raw) > maxSharesFile {
		return nil, ErrSharesDamaged
	}
	return parseShares(raw)
}

// SaveShare inserts a share, or replaces the one with the same UID.
func SaveShare(dataDir string, s Share) error {
	if s.Members == nil {
		s.Members = []Member{}
	}
	if !validShare(s) {
		return ErrSharesDamaged
	}
	shares, err := LoadShares(dataDir)
	if err != nil {
		return err
	}
	replaced := false
	for i := range shares {
		if shares[i].UID == s.UID {
			shares[i] = s
			replaced = true
			break
		}
	}
	if !replaced {
		if len(shares) >= maxShares {
			return ErrTooManyShares
		}
		shares = append(shares, s)
	}
	return writeShares(dataDir, shares)
}

// RemoveShare drops the share with the given UID.
// ErrNotFound when the file is missing or has no such UID.
func RemoveShare(dataDir, uid string) error {
	shares, err := LoadShares(dataDir)
	if err != nil {
		return err
	}
	next := make([]Share, 0, len(shares))
	found := false
	for _, s := range shares {
		if s.UID == uid {
			found = true
			continue
		}
		next = append(next, s)
	}
	if !found {
		return ErrNotFound
	}
	return writeShares(dataDir, next)
}

func parseShares(raw []byte) ([]Share, error) {
	// encoding/json replaces an invalid byte inside a string.
	if !utf8.Valid(raw) {
		return nil, ErrSharesDamaged
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc struct {
		V      int          `json:"v"`
		Shares *[]shareWire `json:"shares"`
	}
	if err := dec.Decode(&doc); err != nil {
		return nil, ErrSharesDamaged
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, ErrSharesDamaged
	}
	if doc.V != 1 || doc.Shares == nil {
		return nil, ErrSharesDamaged
	}
	out := make([]Share, 0, len(*doc.Shares))
	seen := make(map[string]struct{}, len(*doc.Shares))
	for _, w := range *doc.Shares {
		s, ok := shareFromWire(w)
		if !ok {
			return nil, ErrSharesDamaged
		}
		if _, dup := seen[s.UID]; dup {
			return nil, ErrSharesDamaged
		}
		seen[s.UID] = struct{}{}
		out = append(out, s)
	}
	return out, nil
}

func shareFromWire(w shareWire) (Share, bool) {
	if w.Start == nil || w.Next == nil || w.StartSent == nil || w.Members == nil {
		return Share{}, false
	}
	members := make([]Member, len(*w.Members))
	copy(members, *w.Members)
	s := Share{
		UID: w.UID, Node: w.Node, Server: w.Server, Scope: w.Scope,
		Start: *w.Start, Next: *w.Next, StartSent: *w.StartSent,
		Title: w.Title, Generation: w.Generation, Members: members,
		State: w.State, SharedAt: w.SharedAt,
	}
	if !validShare(s) {
		return Share{}, false
	}
	return s, true
}

func validShare(s Share) bool {
	if !validChatID(s.UID) || !validNodeID(s.Node) || !validHex(s.Server, 32) {
		return false
	}
	if s.Scope != "from_now" && s.Scope != "all" {
		return false
	}
	if s.Start < 0 || s.Next < s.Start {
		return false
	}
	if !validChannelTitle(s.Title) || !validHex(s.Generation, 16) {
		return false
	}
	if !validShareMembers(s.Members) {
		return false
	}
	if s.State != "active" && s.State != "stopping" && s.State != "lost" && s.State != "full" {
		return false
	}
	return validJoinedAt(s.SharedAt)
}

func validNodeID(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	c0 := s[0]
	if !((c0 >= 'A' && c0 <= 'Z') || (c0 >= 'a' && c0 <= 'z') || (c0 >= '0' && c0 <= '9')) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validShareMembers(members []Member) bool {
	if len(members) > maxShareMembers {
		return false
	}
	seen := make(map[string]struct{}, len(members))
	for _, m := range members {
		if !validHex(m.Seat, 32) || (m.Access != "read" && m.Access != "write") {
			return false
		}
		if _, ok := seen[m.Seat]; ok {
			return false
		}
		seen[m.Seat] = struct{}{}
	}
	return true
}

func writeShares(dir string, shares []Share) error {
	body, err := marshalShares(sharesFile{V: 1, Shares: shares})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if len(body) > maxSharesFile {
		return ErrTooManyShares
	}
	return publishFile(dir, SharesFile, "share file", body)
}
