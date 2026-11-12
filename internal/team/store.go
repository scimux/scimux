package team

import (
	"encoding/json"
	"errors"
	"os"
)

// File is the server list inside the data directory.
const File = "team.json"

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

// Load reads the server file. A missing file is an empty list and is not
// created. The stub reports that it is not implemented.
func Load(dataDir string) ([]Entry, error) {
	return nil, errors.New("team: not implemented")
}

// Add inserts one server. The stub reports that it is not implemented.
func Add(dataDir string, e Entry) error {
	return errors.New("team: not implemented")
}

// Remove drops one server. The stub reports that it is not implemented.
func Remove(dataDir, serverID string) error {
	return errors.New("team: not implemented")
}
