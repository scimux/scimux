// attachment_api.go — attachment/upload storage, archive helpers, and
// session-asset HTTP handlers.
//
// Ownership (existing boundaries; this file does not change behavior):
//   - Uploads stage beneath the node's attachment directory (storeAttachment).
//   - Successful staging is read and passed to ingestAttachmentAsset
//     (upload_assets.go owns blob/session-log ingestion and its own rollback).
//   - Post-staging failures remove the staged attachment file.
//   - Serving is live-node-scoped and accepts only node-contained paths or
//     opaque asset IDs (handleAttachment / handleAsset).
//   - node_api.go retains delete sequencing and calls the best-effort
//     archiveAttachments/archiveAssets helpers only after removeNodeLocked.
//   - Router registration stays in router.go; outer CSRF/origin/content-type
//     multipart decisions live in security.go.
//   - node/refuseEnded, send/chat projection stay in conversation_api.go;
//     agent-generated path scanning stays in agent_asset.go/internal/asset.
package app

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	mimepkg "mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/scimux/scimux/internal/asset"
	"github.com/scimux/scimux/internal/sessionlog"
)

// attachUploadMax bounds a multipart attachment upload. Files blow past
// jsonBodyMax, so uploads have their own, larger cap and never ride the prompt
// JSON. Bytes land on disk; the session log only ever stores a text reference.
const attachUploadMax = 25 << 20

// Attachment references an uploaded file stored on disk. The bytes live under
// ~/.scimux/attachments/<node-id>/; only this text reference is ever recorded
// in a store — nodes.jsonl and the session log stay grep-able, never binary.
type Attachment struct {
	Path string `json:"path"`
	Mime string `json:"mime"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	// AssetID is the durable session-asset backing this upload (see
	// ingestAttachmentAsset, upload_assets.go). Populated by
	// handleUploadAttachments; never sent by the client.
	AssetID string `json:"assetId,omitempty"`
}

func (a *app) attachmentDir(id string) string { return filepath.Join(a.attachmentsDir, id) }

// storeAttachment writes one uploaded file into the node's attachment directory
// and returns its reference. The stored leaf is prefixed with a random token so
// re-uploading a name never collides or overwrites, and so a crafted filename
// can never escape the directory: filepath.Base strips any separators and the
// token guarantees a non-empty, traversal-free leaf. Written O_EXCL 0600.
func (a *app) storeAttachment(id, name, mime string, src io.Reader) (Attachment, error) {
	base := filepath.Base(name)
	if base == "." || base == ".." || base == "" || base == string(filepath.Separator) {
		base = "file"
	}
	var tok [4]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return Attachment{}, err
	}
	dir := a.attachmentDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Attachment{}, err
	}
	dst := filepath.Join(dir, hex.EncodeToString(tok[:])+"-"+base)
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Attachment{}, err
	}
	n, err := io.Copy(f, src)
	cerr := f.Close()
	if err != nil || cerr != nil {
		os.Remove(dst)
		if err != nil {
			return Attachment{}, err
		}
		return Attachment{}, cerr
	}
	if mime == "" || mime == "application/octet-stream" {
		if t := mimepkg.TypeByExtension(filepath.Ext(base)); t != "" {
			mime = t
		}
	}
	return Attachment{Path: dst, Mime: mime, Name: base, Size: n}, nil
}

// archiveAttachments mirrors archiveSessionLog for a deleted node's uploaded
// files: it moves ~/.scimux/attachments/<id> into attachments/archive/ so a
// reissued slug can never inherit a dead node's files. Best-effort — retention
// never blocks a delete.
func (a *app) archiveAttachments(id string) {
	if a.attachmentsDir == "" {
		return
	}
	src := a.attachmentDir(id)
	if _, err := os.Stat(src); err != nil {
		return
	}
	dir := filepath.Join(a.attachmentsDir, "archive")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: archive attachments for %s: %v\n", id, err)
		return
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(src, filepath.Join(dir, id+"."+stamp)); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: archive attachments for %s: %v\n", id, err)
	}
}

// archiveAssets mirrors archiveSessionLog/archiveAttachments for a deleted
// node's blob-stored session assets (upload-design.md Phase 6): it moves
// ~/.scimux/assets/<id> into assets/archive/ so a reissued slug can never
// inherit a dead node's asset blobs, and a deleted node's images/files don't
// dangle as orphaned-but-still-servable files (the download endpoint checks
// n.ID against a.byID, so once removeNodeLocked has run the live path is
// already unreachable — this only prevents the blobs themselves from
// lingering under the live directory). Best-effort — retention never blocks
// a delete. Inline assets need no such move: their bytes live in the
// already-archived session log, not in assetsDir.
func (a *app) archiveAssets(id string) {
	if a.assetsDir == "" {
		return
	}
	src := asset.NodeDir(a.assetsDir, id)
	if _, err := os.Stat(src); err != nil {
		return
	}
	dir := filepath.Join(a.assetsDir, "archive")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: archive assets for %s: %v\n", id, err)
		return
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(src, filepath.Join(dir, id+"."+stamp)); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: archive assets for %s: %v\n", id, err)
	}
}

// extendPrompt appends a plain-text reference to each attachment so the agent
// reads the local file — proven across every transport to trigger image
// ingestion (see attic/multimodal-input-design.md), so one mechanism serves all three
// with no per-transport image plumbing. Delivery-time only: never stored on
// n.Prompt, so cards and the research question stay clean. The reference left in
// the recorded user turn (mirror echo for tmux; the manager's own user event
// for ACP/codex) is also what the chat view renders (P3).
func extendPrompt(text string, atts []Attachment) string {
	if len(atts) == 0 {
		return text
	}
	lines := make([]string, 0, len(atts))
	for _, at := range atts {
		kind := "file"
		if strings.HasPrefix(at.Mime, "image/") {
			kind = "image"
		}
		lines = append(lines, fmt.Sprintf("[attached %s: %s]", kind, at.Path))
	}
	ref := strings.Join(lines, "\n")
	if strings.TrimSpace(text) == "" {
		return ref
	}
	return text + "\n\n" + ref
}

// resolveAttachments validates each posted attachment reference: it must point
// at a file inside THIS node's attachment directory (the only legitimate source
// — the client uploaded it via POST …/attachments). This closes off a send that
// would otherwise make the agent read an arbitrary local path. Metadata (name,
// size, mime) is refreshed from disk.
func (a *app) resolveAttachments(id string, refs []Attachment) ([]Attachment, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	nodeDir := a.attachmentDir(id) + string(filepath.Separator)
	out := make([]Attachment, 0, len(refs))
	for _, r := range refs {
		clean := filepath.Clean(r.Path)
		if !strings.HasPrefix(clean, nodeDir) {
			return nil, fmt.Errorf("attachment %q is not one of this activity's uploads", r.Name)
		}
		fi, err := os.Stat(clean)
		if err != nil || fi.IsDir() {
			return nil, fmt.Errorf("attachment %q not found", r.Name)
		}
		at := Attachment{Path: clean, Name: r.Name, Mime: r.Mime, Size: fi.Size()}
		if at.Name == "" {
			at.Name = filepath.Base(clean)
		}
		if at.Mime == "" {
			at.Mime = mimepkg.TypeByExtension(filepath.Ext(clean))
		}
		out = append(out, at)
	}
	return out, nil
}

// handleUploadAttachments stores one uploaded file for a node and returns its
// reference (as a one-element list, matching the multipart field). It is
// deliberately separate from the JSON send path: files exceed jsonBodyMax, so
// uploads get their own larger cap (attachUploadMax) and never ride the prompt
// JSON. The prompt references the returned path (P2).
func (a *app) handleUploadAttachments(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if a.refuseEnded(w, n) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, attachUploadMax)
	if err := r.ParseMultipartForm(attachUploadMax); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, fmt.Sprintf("upload too large: the total is capped at %d MiB", attachUploadMax>>20), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad multipart request", 400)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		http.Error(w, "no files in upload (expected multipart field \"files\")", 400)
		return
	}
	// One file per request. The composer already uploads files individually
	// (each staged file is its own POST), so a single-file contract loses
	// nothing — and it makes a partial-batch failure structurally impossible:
	// there is never an earlier file already committed to the append-only
	// session log when a later one fails. A caller that batches several files
	// is rejected outright rather than silently half-ingested.
	if len(files) > 1 {
		http.Error(w, "one file per upload request", 400)
		return
	}
	fh := files[0]
	src, err := fh.Open()
	if err != nil {
		http.Error(w, "open upload: "+err.Error(), 400)
		return
	}
	att, err := a.storeAttachment(n.ID, fh.Filename, fh.Header.Get("Content-Type"), src)
	src.Close()
	if err != nil {
		http.Error(w, "store upload: "+err.Error(), 500)
		return
	}
	// Past this point any failure removes the staged file so a rejected upload
	// leaves nothing behind; ingestAttachmentAsset removes its own blob on a
	// failed log append (see ingestAssetBytes).
	data, err := os.ReadFile(att.Path)
	if err != nil {
		os.Remove(att.Path)
		http.Error(w, "read upload: "+err.Error(), 500)
		return
	}
	ev, err := a.ingestAttachmentAsset(n.ID, att.Name, att.Mime, att.Path, data)
	if err != nil {
		os.Remove(att.Path)
		http.Error(w, "ingest upload: "+err.Error(), 500)
		return
	}
	att.AssetID = ev.ID
	writeJSON(w, map[string]any{"attachments": []Attachment{att}})
}

// handleAttachment serves one uploaded file's bytes for the chat view. Guarded:
// only a bare filename inside THIS node's attachment directory is served — no
// traversal, no cross-node reach. That scoping is also what keeps the delivered
// path-marker spoof-safe: a marker pointing outside the node's own uploads has
// no servable URL, so a crafted "[attached image: /etc/passwd]" renders nothing.
// Read-only; live nodes only (a deleted node's files are archived away).
func (a *app) handleAttachment(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	name := filepath.Base(r.PathValue("name")) // strips any path separators
	if name == "." || name == ".." || name == "" {
		http.Error(w, "not found", 404)
		return
	}
	dir := a.attachmentDir(n.ID)
	full := filepath.Join(dir, name)
	if !strings.HasPrefix(full, dir+string(filepath.Separator)) { // defense in depth
		http.Error(w, "not found", 404)
		return
	}
	fi, err := os.Stat(full)
	if err != nil || fi.IsDir() {
		http.Error(w, "not found", 404)
		return
	}
	// Serve uploads as inert content: an uploaded .html/.svg opened from the
	// app origin would otherwise run as same-origin active content (and, once
	// the CSRF token exists, read it). nosniff pins the type we set; only a
	// whitelist of safe raster images is served inline, everything else — SVG,
	// HTML, unknown — is forced to download. We set Content-Type explicitly so
	// ServeFile does not sniff its own.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if raster := inlineImageTypes[strings.ToLower(filepath.Ext(name))]; raster != "" {
		w.Header().Set("Content-Type", raster)
	} else {
		// Anything not on the raster whitelist downloads as an opaque blob:
		// octet-stream + nosniff + attachment leaves no path for an uploaded
		// .html/.svg to run as same-origin active content, whatever its name.
		w.Header().Set("Content-Type", "application/octet-stream")
		disp := name // strip the storage token for the download name (as the UI does)
		if i := strings.IndexByte(disp, '-'); i == 8 {
			disp = disp[i+1:]
		}
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(disp))
	}
	http.ServeFile(w, r, full)
}

// handleAsset serves one session asset's bytes: GET /api/nodes/{id}/assets/{assetID}.
// The URL carries only an opaque asset ID, never a path — BlobPath lives in
// the node's own session log and is resolved (with cross-node/traversal
// containment) via internal/asset.ResolveBlobPath before anything is served.
// Same inert-serving contract as handleAttachment: nosniff always, inline
// only for the safe raster whitelist, everything else forced to download.
func (a *app) handleAsset(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	id := r.PathValue("assetID")
	if id == "" || a.sessionsDir == "" {
		http.Error(w, "not found", 404)
		return
	}
	idx := sessionlog.ReadAssets(a.sessionLogPath(n.ID))
	rec, ok := idx[id]
	if !ok {
		http.Error(w, "not found", 404)
		return
	}

	var data []byte
	var servePath string
	switch rec.Storage {
	case "inline":
		b, err := base64.StdEncoding.DecodeString(rec.Bytes)
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		data = b
	case "blob":
		full, err := asset.ResolveBlobPath(a.assetsDir, n.ID, rec.BlobPath)
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		servePath = full
	default:
		http.Error(w, "not found", 404)
		return
	}

	name := rec.Name
	if name == "" {
		name = "asset"
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if raster := inlineImageTypes[strings.ToLower(filepath.Ext(name))]; raster != "" {
		w.Header().Set("Content-Type", raster)
	} else {
		// Same reasoning as handleAttachment: never trust the recorded MIME
		// for inline rendering — force an opaque download so an agent- or
		// user-supplied .html/.svg can never run as same-origin content.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	}
	if servePath != "" {
		http.ServeFile(w, r, servePath)
		return
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// inlineImageTypes maps the file extensions served inline (as an <img> or a
// new-tab open) to their content type: safe raster formats with no active
// content. SVG is excluded on purpose — it can carry script — as is everything
// else, which downloads. Keyed by extension so the whitelist does not depend on
// the host's mime table knowing webp.
var inlineImageTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}
