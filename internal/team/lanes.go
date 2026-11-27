package team

import "encoding/json"

// LaneSharesFile is the lane share list inside the data directory.
const LaneSharesFile = "team-lane-shares.json"

const (
	maxLaneShares     = 64
	maxLaneSharesFile = 1 << 20
)

// LaneShare is one lane this seat publishes.
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

var (
	ErrLaneSharesDamaged = ErrSharesDamaged
	ErrTooManyLaneShares = ErrTooManyShares
	ErrLaneShareExists   = ErrSharesDamaged
)

var marshalLaneShares = json.Marshal

type laneSharesFile struct {
	V     int         `json:"v"`
	Lanes []LaneShare `json:"lanes"`
}

// LoadLaneShares reads the lane share file.
func LoadLaneShares(string) ([]LaneShare, error) { return []LaneShare{}, nil }

// SaveLaneShare inserts or replaces a lane share.
func SaveLaneShare(string, LaneShare) error { return ErrLaneSharesDamaged }

// RemoveLaneShare drops one lane share.
func RemoveLaneShare(string, string) error { return ErrNotFound }

func parseLaneShares([]byte) ([]LaneShare, error) { return nil, ErrLaneSharesDamaged }
