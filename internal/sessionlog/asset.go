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

// AnchoredAsset is an asset event tagged with Anchor — the record index of the
// turn it was ingested from (the nearest preceding visible turn in the log, or
// -1 if it precedes every turn). Render-time projection resolves each turn
// against only the assets anchored at or before it, so a path re-ingested with
// new bytes in a later turn does not retroactively change what an earlier turn
// shows: the earlier reference keeps its own content, the later one shows the
// new content, and the two stay comparable.
type AnchoredAsset struct {
	Anchor int
	Asset  AssetEvent
}

// ReadAnchoredAssets replays a session log into its asset events in log order,
// each tagged with its owning turn's record index (see AnchoredAsset). The
// record index matches transcript.Turn.Record (both are indices into
// ReadEvents), which is what projection compares against. Assets with no
// SourcePath are omitted — only path markers are projected. Defensive like the
// other readers: unreadable files and malformed lines yield nothing.
func ReadAnchoredAssets(path string) []AnchoredAsset {
	var out []AnchoredAsset
	lastTurn := -1
	for i, ev := range ReadEvents(path) {
		switch ev.T {
		case "user", "assistant":
			// Match segmentOf's turn filter: whitespace-only records are not
			// rendered turns, so they must not anchor an asset either.
			if strings.TrimSpace(ev.Text) != "" {
				lastTurn = i
			}
		case "asset":
			if ev.Asset != nil && ev.Asset.SourcePath != "" {
				out = append(out, AnchoredAsset{Anchor: lastTurn, Asset: *ev.Asset})
			}
		}
	}
	return out
}

// ReadAssetsByPath replays a session log into an asset index keyed by
// SourcePath. Used by ingestion (internal/app) only as a path-presence set for
// its idempotency check — NOT by render-time projection, which needs the
// per-turn anchoring of ReadAnchoredAssets: a path can carry different content
// across turns, and a single first-wins map cannot represent that. Assets with
// no recorded SourcePath are omitted; first-wins if two records share a path.
func ReadAssetsByPath(path string) map[string]AssetEvent {
	idx := make(map[string]AssetEvent)
	for _, ev := range ReadEvents(path) {
		if ev.T != "asset" || ev.Asset == nil || ev.Asset.SourcePath == "" {
			continue
		}
		if _, exists := idx[ev.Asset.SourcePath]; exists {
			continue
		}
		idx[ev.Asset.SourcePath] = *ev.Asset
	}
	return idx
}
