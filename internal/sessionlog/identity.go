package sessionlog

import (
	"encoding/json"
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
//
// A header is a meta record with a non-empty uid. The first one anywhere in
// the log supplies the ID. A later header with a different ID is a conflict.
// A missing file, an unreadable file, a log with no header, or a read that
// stops before the end is IdentityNone: a header already seen is discarded,
// because the rest of the log was not proven free of a second ID.
func ReadLogIdentity(path string) LogIdentity {
	rc, err := openIdentityLog(path)
	if err != nil {
		return LogIdentity{Record: -1}
	}
	defer rc.Close()

	var uid string
	record := -1
	seen := false
	conflict := false
	ordinal := 0
	_, err = forEachRecord(rc, func(rec []byte, _ int64) bool {
		var ev Event
		if json.Unmarshal(rec, &ev) != nil {
			return true
		}
		if ev.T == "meta" && ev.Meta != nil && ev.Meta.UID != "" {
			if !seen {
				seen = true
				uid = ev.Meta.UID
				record = ordinal
			} else if ev.Meta.UID != uid {
				conflict = true
			}
		}
		ordinal++
		return true
	})
	if err != nil {
		return LogIdentity{Record: -1}
	}
	if conflict {
		return LogIdentity{Status: IdentityConflict, Record: record}
	}
	if !seen {
		return LogIdentity{Record: -1}
	}
	return LogIdentity{UID: uid, Status: IdentityOK, Record: record}
}
