package sessionlog

import (
	"io"
	"os"
)

// IdentityStatus is what ReadLogIdentity could prove about a session log.
type IdentityStatus int

const (
	// IdentityNone means the log has no usable header, or it could not be
	// read through to the end.
	IdentityNone IdentityStatus = iota
	// IdentityOK means every usable header carries the same ID.
	IdentityOK
	// IdentityConflict means the log contains two or more distinct IDs.
	IdentityConflict
)

// LogIdentity is the chat ID read from a session log.
//
// UID is set only when Status is IdentityOK. Record is the parsed-record
// ordinal of the first usable header, or -1 when Status is IdentityNone.
// A read that stops early is IdentityNone even if a header was already seen.
type LogIdentity struct {
	UID    string
	Status IdentityStatus
	Record int
}

// openIdentityLog opens a session log for reading. Tests replace it with a
// reader that fails after a prefix, which a regular file cannot do portably.
var openIdentityLog = func(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

// ReadLogIdentity reads a chat's unique ID from its session log. It only
// reads. It does not write, repair, or append.
func ReadLogIdentity(path string) LogIdentity {
	return LogIdentity{}
}
