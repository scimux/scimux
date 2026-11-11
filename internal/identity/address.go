package identity

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

// Address is one asset's global address. The zero Address is invalid.
// Addresses compare equal exactly when their canonical text is equal.
type Address struct {
	kind       Kind
	inst       InstallationID
	p0, p1, p2 string
	seg, rec   int
}

// LaneAddress addresses a lane by its existing lane ID.
func LaneAddress(inst InstallationID, laneID string) (Address, error) {
	return Address{}, nil
}

// ChatAddress addresses a chat by its existing chat UID.
func ChatAddress(inst InstallationID, chatUID string) (Address, error) {
	return Address{}, nil
}

// BubbleAddress addresses one bubble. segment and record are non-negative;
// zero is valid.
func BubbleAddress(inst InstallationID, chatUID string, segment, record int) (Address, error) {
	return Address{}, nil
}

// NoteAddress addresses a note by its existing note ID.
func NoteAddress(inst InstallationID, noteID string) (Address, error) {
	return Address{}, nil
}

// SectionAddress addresses a section inside a note.
func SectionAddress(inst InstallationID, noteID, sectionID string) (Address, error) {
	return Address{}, nil
}

// ReferenceAddress addresses an embedded reference inside a section.
func ReferenceAddress(inst InstallationID, noteID, sectionID, referenceID string) (Address, error) {
	return Address{}, nil
}

// ParseAddress accepts only the canonical text form.
func ParseAddress(s string) (Address, error) {
	return Address{}, nil
}

// String returns the canonical text, or "" for the zero Address.
func (a Address) String() string { return "" }

// Kind returns the asset kind, or "" for the zero Address.
func (a Address) Kind() Kind { return "" }

// Installation returns the installation label, or "" for the zero Address.
func (a Address) Installation() InstallationID { return "" }

// MarshalText encodes the canonical text. The zero Address is an error.
func (a Address) MarshalText() ([]byte, error) { return nil, nil }

// UnmarshalText parses the canonical text. On error the receiver is unchanged.
func (a *Address) UnmarshalText(b []byte) error { return nil }
