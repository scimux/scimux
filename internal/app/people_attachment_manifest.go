package app

import (
	"bytes"
	"encoding/json"
	"mime"
	"strings"
	"unicode"
	"unicode/utf8"
)

const peopleAttachmentNotice = "[Attachments unavailable in this version of scimux.]"

type peopleAttachmentDescriptor struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Name   string `json:"name"`
	MIME   string `json:"mime"`
}

// projectPeopleAttachments never turns malformed or unknown wire data into
// attachment authority. The original legacy text remains visible on error.
func projectPeopleAttachments(raw json.RawMessage, fallback string, entry map[string]any) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return
	}
	versionRaw, hasVersion := fields["people_attachments_version"]
	_, hasFiles := fields["people_attachments"]
	_, hasText := fields["content_text"]
	if !hasVersion && !hasFiles && !hasText {
		return
	}
	entry["attachments_status"] = "corrupt"
	var version int
	if !hasVersion || json.Unmarshal(versionRaw, &version) != nil {
		return
	}
	if version != 1 {
		entry["attachments_status"] = "unsupported"
		return
	}
	var text string
	if !hasText || bytes.Equal(bytes.TrimSpace(fields["content_text"]), []byte("null")) || json.Unmarshal(fields["content_text"], &text) != nil || len(text) > 16384 || !utf8.ValidString(text) {
		return
	}
	expected := peopleAttachmentNotice
	if text != "" {
		expected = text + "\n" + expected
	}
	if fallback != expected || !hasFiles {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(fields["people_attachments"]))
	dec.DisallowUnknownFields()
	var files []peopleAttachmentDescriptor
	if dec.Decode(&files) != nil || len(files) == 0 || len(files) > 16 {
		return
	}
	seen := make(map[string]bool, len(files))
	var total int64
	out := make([]map[string]any, 0, len(files))
	for _, f := range files {
		if !subHex(f.ID, 16) || !subHex(f.SHA256, 64) || seen[f.ID] || f.Size < 1 || f.Size > 4<<20 || !peopleSafeName(f.Name) || len(f.MIME) > 255 {
			return
		}
		if _, _, err := mime.ParseMediaType(f.MIME); err != nil {
			return
		}
		seen[f.ID] = true
		total += f.Size
		if total > 16<<20 {
			return
		}
		out = append(out, map[string]any{"id": f.ID, "sha256": f.SHA256, "size": f.Size, "name": peopleVisibleName(f.Name), "mime": f.MIME})
	}
	entry["text"] = text
	entry["attachments"] = out
	delete(entry, "attachments_status")
}

func peopleSafeName(name string) bool {
	if name == "" || len(name) > 255 || !utf8.ValidString(name) || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func peopleBidiControl(r rune) bool {
	return r == '\u061c' || r == '\u200e' || r == '\u200f' || r >= '\u202a' && r <= '\u202e' || r >= '\u2066' && r <= '\u2069'
}

// peopleVisibleName strips bidi controls for display. Stored names stay unchanged.
// A name that would become empty, ".", or ".." is shown as "file".
func peopleVisibleName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		if peopleBidiControl(r) {
			return -1
		}
		return r
	}, name)
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "file"
	}
	return cleaned
}
