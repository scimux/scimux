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

// SubsFile is the subscription list inside the data directory.
const SubsFile = "team-subs.json"

const (
	maxSubs = 4096
	// maxSubsFile is above the largest valid document. 4096 items with
	// the longest names marshal to under 10 MiB.
	maxSubsFile = 16 << 20
)

// Subscription is one generation of a channel this seat is a member of.
// It holds no chat content and no token.
type Subscription struct {
	Server     string `json:"server"`
	Owner      string `json:"owner"`
	Chat       string `json:"chat"`
	Generation string `json:"generation"`
	ServerName string `json:"server_name"`
	OwnerName  string `json:"owner_name"`
	Title      string `json:"title"`
	Access     string `json:"access"`
	State      string `json:"state"`
	MirrorLast uint64 `json:"mirror_last"`
	SeenMirror uint64 `json:"seen_mirror"`
	FirstSeen  string `json:"first_seen"`
}

// Sentinel errors from the subscription file. None of them quote a title or a token.
var (
	ErrSubsDamaged = errors.New("team: subscription file is damaged")
	ErrTooManySubs = errors.New("team: subscription list is full")
)

// marshalSubs encodes the subscription file. Tests replace it.
var marshalSubs = json.Marshal

type subsFile struct {
	V             int            `json:"v"`
	Subscriptions []Subscription `json:"subscriptions"`
}

// subWire keeps fields whose zero value is valid as pointers, so a missing
// or null value is damage and an empty name or a zero count is not.
type subWire struct {
	Server     string  `json:"server"`
	Owner      string  `json:"owner"`
	Chat       string  `json:"chat"`
	Generation string  `json:"generation"`
	ServerName string  `json:"server_name"`
	OwnerName  *string `json:"owner_name"`
	Title      *string `json:"title"`
	Access     string  `json:"access"`
	State      string  `json:"state"`
	MirrorLast *uint64 `json:"mirror_last"`
	SeenMirror *uint64 `json:"seen_mirror"`
	FirstSeen  string  `json:"first_seen"`
}

type subID struct {
	server, owner, chat, generation string
}

// LoadSubs reads the subscription file. A missing file is an empty list and is not created.
func LoadSubs(dataDir string) ([]Subscription, error) {
	f, err := os.Open(filepath.Join(dataDir, SubsFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Subscription{}, nil
		}
		return nil, fmt.Errorf("team: open subscription file: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxSubsFile+1))
	if err != nil {
		return nil, fmt.Errorf("team: read subscription file: %w", err)
	}
	if len(raw) > maxSubsFile {
		return nil, ErrSubsDamaged
	}
	return parseSubs(raw)
}

// SaveSubs replaces the subscription list.
// A damaged or unreadable file is left as it is.
func SaveSubs(dataDir string, subs []Subscription) error {
	if subs == nil {
		subs = []Subscription{}
	}
	if _, err := LoadSubs(dataDir); err != nil {
		return err
	}
	if !validSubs(subs) {
		return ErrSubsDamaged
	}
	if len(subs) > maxSubs {
		return ErrTooManySubs
	}
	return writeSubs(dataDir, subs)
}

func parseSubs(raw []byte) ([]Subscription, error) {
	// encoding/json replaces an invalid byte inside a string.
	if !utf8.Valid(raw) {
		return nil, ErrSubsDamaged
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc struct {
		V             int        `json:"v"`
		Subscriptions *[]subWire `json:"subscriptions"`
	}
	if err := dec.Decode(&doc); err != nil {
		return nil, ErrSubsDamaged
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, ErrSubsDamaged
	}
	if doc.V != 1 || doc.Subscriptions == nil {
		return nil, ErrSubsDamaged
	}
	if len(*doc.Subscriptions) > maxSubs {
		return nil, ErrSubsDamaged
	}
	out := make([]Subscription, 0, len(*doc.Subscriptions))
	seen := make(map[subID]struct{}, len(*doc.Subscriptions))
	for _, w := range *doc.Subscriptions {
		s, ok := subFromWire(w)
		if !ok {
			return nil, ErrSubsDamaged
		}
		id := subID{s.Server, s.Owner, s.Chat, s.Generation}
		if _, dup := seen[id]; dup {
			return nil, ErrSubsDamaged
		}
		seen[id] = struct{}{}
		out = append(out, s)
	}
	return out, nil
}

func subFromWire(w subWire) (Subscription, bool) {
	if w.OwnerName == nil || w.Title == nil || w.MirrorLast == nil || w.SeenMirror == nil {
		return Subscription{}, false
	}
	s := Subscription{
		Server: w.Server, Owner: w.Owner, Chat: w.Chat, Generation: w.Generation,
		ServerName: w.ServerName, OwnerName: *w.OwnerName, Title: *w.Title,
		Access: w.Access, State: w.State,
		MirrorLast: *w.MirrorLast, SeenMirror: *w.SeenMirror,
		FirstSeen: w.FirstSeen,
	}
	if !validSub(s) {
		return Subscription{}, false
	}
	return s, true
}

func validSubs(subs []Subscription) bool {
	seen := make(map[subID]struct{}, len(subs))
	for _, s := range subs {
		if !validSub(s) {
			return false
		}
		id := subID{s.Server, s.Owner, s.Chat, s.Generation}
		if _, dup := seen[id]; dup {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

// validSub checks one subscription. Chat is 16 lower-case hex so the copy
// path can use it as a directory name. The protocol allows a wider chat id;
// those channels are not stored.
func validSub(s Subscription) bool {
	if !validHex(s.Server, 32) || !validHex(s.Owner, 32) || !validHex(s.Chat, 16) || !validHex(s.Generation, 16) {
		return false
	}
	if !utf8.ValidString(s.ServerName) || !validName(s.ServerName) || !validOwnerName(s.OwnerName) || !validChannelTitle(s.Title) {
		return false
	}
	if s.Access != "read" && s.Access != "write" {
		return false
	}
	if s.State != "live" && s.State != "ended" {
		return false
	}
	return validJoinedAt(s.FirstSeen)
}

// validOwnerName allows an empty name. A roster that does not list the
// owner leaves the stored name blank.
func validOwnerName(name string) bool {
	if name == "" {
		return true
	}
	return validStoredName(name, true)
}

func writeSubs(dir string, subs []Subscription) error {
	body, err := marshalSubs(subsFile{V: 1, Subscriptions: subs})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return publishSubs(dir, body)
}

// publishSubs writes team-subs.json. publish is fixed to team.json.
func publishSubs(dir string, body []byte) error {
	f, err := storeOps.createTemp(dir)
	if err != nil {
		return fmt.Errorf("team: write subscription file: %w", err)
	}
	tmpName := f.Name()
	closed := false
	defer func() {
		if !closed {
			_ = storeOps.close(f)
		}
		if tmpName != "" {
			_ = storeOps.remove(tmpName)
		}
	}()
	if err := storeOps.chmod(f, 0o600); err != nil {
		return fmt.Errorf("team: write subscription file: %w", err)
	}
	n, err := storeOps.write(f, body)
	if err != nil {
		return fmt.Errorf("team: write subscription file: %w", err)
	}
	if n != len(body) {
		return fmt.Errorf("team: write subscription file: %w", io.ErrShortWrite)
	}
	if err := storeOps.syncFile(f); err != nil {
		return fmt.Errorf("team: write subscription file: %w", err)
	}
	if err := storeOps.close(f); err != nil {
		return fmt.Errorf("team: write subscription file: %w", err)
	}
	closed = true
	if err := storeOps.rename(tmpName, filepath.Join(dir, SubsFile)); err != nil {
		return fmt.Errorf("team: write subscription file: %w", err)
	}
	tmpName = ""
	return nil
}
