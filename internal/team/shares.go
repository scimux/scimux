package team

import (
	"encoding/json"
	"errors"
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
	State      string   `json:"state"`
	SharedAt   string   `json:"shared_at"`
}

// Sentinel errors from the share file. None of them quote a title or a token.
var (
	ErrSharesDamaged = errors.New("team: share file is damaged")
	ErrTooManyShares = errors.New("team: share list is full")
)

// errShareStub is the red-state answer. The implementation replaces it.
var errShareStub = errors.New("team: share stub")

// marshalShares encodes the share file. Tests replace it.
var marshalShares = json.Marshal

type sharesFile struct {
	V      int     `json:"v"`
	Shares []Share `json:"shares"`
}

// LoadShares reads the share file. A missing file is an empty list and is not created.
func LoadShares(string) ([]Share, error) { return nil, errShareStub }

// SaveShare inserts a share, or replaces the one with the same UID.
func SaveShare(string, Share) error { return errShareStub }

// RemoveShare drops the share with the given UID.
func RemoveShare(string, string) error { return errShareStub }

func parseShares([]byte) ([]Share, error) { return nil, errShareStub }
