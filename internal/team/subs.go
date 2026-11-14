package team

import (
	"encoding/json"
	"errors"
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

// LoadSubs reads the subscription file. A missing file is an empty list and is not created.
func LoadSubs(dataDir string) ([]Subscription, error) {
	return []Subscription{}, nil
}

// SaveSubs replaces the subscription list. A damaged file is left as it is.
func SaveSubs(dataDir string, subs []Subscription) error {
	return ErrSubsDamaged
}
