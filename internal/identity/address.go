package identity

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kind is the asset a global address names.
type Kind string

const (
	KindLane      Kind = "lane"
	KindChat      Kind = "chat"
	KindBubble    Kind = "bubble"
	KindNote      Kind = "note"
	KindSection   Kind = "section"
	KindReference Kind = "reference"
)

const addressScheme = "scimux1"

const maxAddressPartBytes = 512

var errInvalidAddress = errors.New("identity: invalid address")

// Address is one asset's global address. The zero Address is invalid.
// Addresses compare equal exactly when their canonical text is equal.
type Address struct {
	kind       Kind
	inst       InstallationID
	p0, p1, p2 string
	seg, rec   int
}

// LaneAddress addresses a lane by its existing lane ID. The lane ID is
// opaque text and may contain any character the part rules allow.
func LaneAddress(inst InstallationID, laneID string) (Address, error) {
	return newAddress(KindLane, inst, laneID, "", "", 0, 0)
}

// ChatAddress addresses a chat by its existing chat UID. The UID is not
// restricted to the current 16-hex form.
func ChatAddress(inst InstallationID, chatUID string) (Address, error) {
	return newAddress(KindChat, inst, chatUID, "", "", 0, 0)
}

// BubbleAddress addresses one bubble. segment and record are non-negative;
// zero is valid.
func BubbleAddress(inst InstallationID, chatUID string, segment, record int) (Address, error) {
	return newAddress(KindBubble, inst, chatUID, "", "", segment, record)
}

// NoteAddress addresses a note by its existing note ID.
func NoteAddress(inst InstallationID, noteID string) (Address, error) {
	return newAddress(KindNote, inst, noteID, "", "", 0, 0)
}

// SectionAddress addresses a section inside a note.
func SectionAddress(inst InstallationID, noteID, sectionID string) (Address, error) {
	return newAddress(KindSection, inst, noteID, sectionID, "", 0, 0)
}

// ReferenceAddress addresses an embedded reference inside a section.
func ReferenceAddress(inst InstallationID, noteID, sectionID, referenceID string) (Address, error) {
	return newAddress(KindReference, inst, noteID, sectionID, referenceID, 0, 0)
}

// ParseAddress accepts only the canonical text form. A successful parse
// round-trips through String byte for byte.
func ParseAddress(s string) (Address, error) {
	parts := strings.Split(s, "/")
	if len(parts) < 4 || parts[0] != addressScheme {
		return Address{}, errInvalidAddress
	}
	inst, err := ParseInstallationID(parts[1])
	if err != nil {
		return Address{}, errInvalidAddress
	}
	kind := Kind(parts[2])
	rest := parts[3:]
	switch kind {
	case KindLane, KindChat, KindNote:
		if len(rest) != 1 {
			return Address{}, errInvalidAddress
		}
		p0, err := canonicalPart(rest[0])
		if err != nil {
			return Address{}, err
		}
		return newAddress(kind, inst, p0, "", "", 0, 0)
	case KindSection:
		if len(rest) != 2 {
			return Address{}, errInvalidAddress
		}
		p0, err := canonicalPart(rest[0])
		if err != nil {
			return Address{}, err
		}
		p1, err := canonicalPart(rest[1])
		if err != nil {
			return Address{}, err
		}
		return newAddress(kind, inst, p0, p1, "", 0, 0)
	case KindBubble:
		if len(rest) != 3 {
			return Address{}, errInvalidAddress
		}
		p0, err := canonicalPart(rest[0])
		if err != nil {
			return Address{}, err
		}
		seg, err := canonicalIndex(rest[1])
		if err != nil {
			return Address{}, err
		}
		rec, err := canonicalIndex(rest[2])
		if err != nil {
			return Address{}, err
		}
		return newAddress(kind, inst, p0, "", "", seg, rec)
	case KindReference:
		if len(rest) != 3 {
			return Address{}, errInvalidAddress
		}
		decoded, err := canonicalParts(rest)
		if err != nil {
			return Address{}, err
		}
		return newAddress(kind, inst, decoded[0], decoded[1], decoded[2], 0, 0)
	default:
		return newAddress(kind, inst, "", "", "", 0, 0)
	}
}

// String returns the canonical text, or "" for the zero Address.
func (a Address) String() string {
	switch a.kind {
	case KindLane, KindChat, KindNote:
		return formatAddress(a, url.PathEscape(a.p0))
	case KindSection:
		return formatAddress(a, url.PathEscape(a.p0), url.PathEscape(a.p1))
	case KindBubble:
		return formatAddress(a, url.PathEscape(a.p0), strconv.Itoa(a.seg), strconv.Itoa(a.rec))
	case KindReference:
		return formatAddress(a, url.PathEscape(a.p0), url.PathEscape(a.p1), url.PathEscape(a.p2))
	default:
		return ""
	}
}

// Kind returns the asset kind, or "" for the zero Address.
func (a Address) Kind() Kind { return a.kind }

// Installation returns the installation label, or "" for the zero Address.
func (a Address) Installation() InstallationID { return a.inst }

// MarshalText encodes the canonical text. The zero Address is an error.
func (a Address) MarshalText() ([]byte, error) {
	s := a.String()
	if s == "" {
		return nil, errInvalidAddress
	}
	return []byte(s), nil
}

// UnmarshalText parses the canonical text. On error the receiver is unchanged.
func (a *Address) UnmarshalText(text []byte) error {
	parsed, err := ParseAddress(string(text))
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}

func newAddress(kind Kind, inst InstallationID, p0, p1, p2 string, seg, rec int) (Address, error) {
	if _, err := ParseInstallationID(string(inst)); err != nil {
		return Address{}, errInvalidAddress
	}
	switch kind {
	case KindLane, KindChat, KindNote:
		if err := validatePart(p0); err != nil {
			return Address{}, err
		}
	case KindSection:
		if err := validatePart(p0); err != nil {
			return Address{}, err
		}
		if err := validatePart(p1); err != nil {
			return Address{}, err
		}
	case KindBubble:
		if err := validatePart(p0); err != nil {
			return Address{}, err
		}
		if seg < 0 || rec < 0 {
			return Address{}, errInvalidAddress
		}
	case KindReference:
		if err := validatePart(p0); err != nil {
			return Address{}, err
		}
		if err := validatePart(p1); err != nil {
			return Address{}, err
		}
		if err := validatePart(p2); err != nil {
			return Address{}, err
		}
	default:
		return Address{}, errInvalidAddress
	}
	return Address{kind: kind, inst: inst, p0: p0, p1: p1, p2: p2, seg: seg, rec: rec}, nil
}

func validatePart(s string) error {
	if s == "" || len(s) > maxAddressPartBytes || strings.IndexByte(s, 0) >= 0 || !utf8.ValidString(s) {
		return errInvalidAddress
	}
	return nil
}

func canonicalParts(parts []string) ([]string, error) {
	out := make([]string, len(parts))
	for i, part := range parts {
		decoded, err := canonicalPart(part)
		if err != nil {
			return nil, err
		}
		out[i] = decoded
	}
	return out, nil
}

func canonicalPart(escaped string) (string, error) {
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return "", errInvalidAddress
	}
	if url.PathEscape(decoded) != escaped {
		return "", errInvalidAddress
	}
	if err := validatePart(decoded); err != nil {
		return "", err
	}
	return decoded, nil
}

func canonicalIndex(s string) (int, error) {
	if s == "" || s[0] < '0' || s[0] > '9' {
		return 0, errInvalidAddress
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, errInvalidAddress
	}
	n, err := strconv.ParseUint(s, 10, strconv.IntSize-1)
	if err != nil {
		return 0, errInvalidAddress
	}
	return int(n), nil
}

func formatAddress(a Address, parts ...string) string {
	out := make([]string, 0, 3+len(parts))
	out = append(out, addressScheme, string(a.inst), string(a.kind))
	out = append(out, parts...)
	return strings.Join(out, "/")
}
