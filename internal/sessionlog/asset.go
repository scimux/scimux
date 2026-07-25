package sessionlog

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// AssetEvent is a durable session asset: a user upload or an agent-generated
// file that belongs to the node's conversation history. Storage is either
// "inline" (Bytes holds base64) or "blob" (BlobPath names a file under the
// node-scoped asset store) — that choice is a storage-mode decision made by
// the caller (see internal/asset.StorageMode) and never gates whether a
// file was eligible to be ingested in the first place. SourceKind/SourcePath
// are provenance only, never a serving path: rendered chat and the download
// endpoint address the asset only by ID. See upload-design.md.
type AssetEvent struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Mime   string `json:"mime,omitempty"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`

	Storage  string `json:"storage"`
	Bytes    string `json:"bytes,omitempty"`
	BlobPath string `json:"blobPath,omitempty"`

	SourceKind string `json:"sourceKind,omitempty"` // "upload" | "agent_path"
	SourcePath string `json:"sourcePath,omitempty"`
}

// NewAsset builds an "asset" record for a to-be-appended session asset.
func NewAsset(a AssetEvent) Event {
	return Event{T: "asset", Time: nowStamp(), Asset: &a}
}

// NewAssetID mints a collision-resistant asset identifier, "a_" prefixed so
// it reads unambiguously in Markdown as scimux-asset:a_xxxx.
func NewAssetID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err == nil {
		return fmt.Sprintf("a_%x", b)
	}
	// Same fallback rationale as NewMeta's UID: a nanosecond stamp can't
	// collide with another ID minted by this process even if the RNG fails.
	return fmt.Sprintf("a_t%x", time.Now().UnixNano())
}

// SanitizeAssetName reduces a path or filename to a bare display name: the
// last path component, with directory traversal and empty/root inputs
// falling back to a generic placeholder rather than an empty or "."/".."
// name reaching storage or the UI.
func SanitizeAssetName(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	switch base {
	case "", ".", "..", string(filepath.Separator):
		return "asset"
	}
	return base
}

// extMIME covers the formats upload-design.md calls out explicitly; anything
// else falls through to content sniffing and then a generic fallback.
var extMIME = map[string]string{
	".png":      "image/png",
	".jpg":      "image/jpeg",
	".jpeg":     "image/jpeg",
	".gif":      "image/gif",
	".webp":     "image/webp",
	".pdf":      "application/pdf",
	".md":       "text/markdown; charset=utf-8",
	".markdown": "text/markdown; charset=utf-8",
	".txt":      "text/plain; charset=utf-8",
	".csv":      "text/csv; charset=utf-8",
	".zip":      "application/zip",
}

// DetectMIME resolves a MIME type from the file's extension first (stable
// and cheap), falling back to content sniffing, then a generic default.
// Extension wins over sniffing so ".md" reliably reports as Markdown rather
// than whatever text/plain-ish guess the sniffer makes.
func DetectMIME(name string, data []byte) string {
	if m, ok := extMIME[strings.ToLower(filepath.Ext(name))]; ok {
		return m
	}
	if len(data) > 0 {
		if sniffed := http.DetectContentType(data); sniffed != "application/octet-stream" {
			return sniffed
		}
	}
	return "application/octet-stream"
}

// SHA256Hex returns the lowercase hex SHA-256 digest of data, used both for
// the asset record's SHA256 field and for dedup lookups.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ReadAssets replays a session log into an asset index keyed by ID.
// Defensive like ReadEvents: unreadable files and malformed lines yield
// nothing/are skipped rather than erroring. The first record for a given ID
// is the durable one (Deduplication policy in upload-design.md) — a later
// record reusing the same ID is ignored rather than overwriting it.
func ReadAssets(path string) map[string]AssetEvent {
	idx := make(map[string]AssetEvent)
	for _, ev := range ReadEvents(path) {
		if ev.T != "asset" || ev.Asset == nil || ev.Asset.ID == "" {
			continue
		}
		if _, exists := idx[ev.Asset.ID]; exists {
			continue
		}
		idx[ev.Asset.ID] = *ev.Asset
	}
	return idx
}
