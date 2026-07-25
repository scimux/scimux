package main

import (
	"encoding/base64"

	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// assetInlineCap is the provisional inline-storage size cap noted as an open
// question in upload-design.md: files at or under this size are inlined as
// base64 in the session log; larger files still ingest, as a blob (see
// asset.StorageMode — the cap selects storage mode, never gates ingestion).
const assetInlineCap = 256 << 10

// ingestAttachmentAsset converts one already-validated upload into a durable
// session asset: it decides storage mode, writes inline or blob bytes, and
// appends the AssetEvent to the node's session log immediately (P3 — user
// uploads get the same durability/search/download guarantees as
// agent-generated assets, see upload-design.md). Ingestion happens at upload
// time, synchronously, so a failure here fails the whole upload rather than
// leaving an attachment with no backing asset. sourcePath is the staged
// attachment file's local path — the same path extendPrompt later embeds in
// the delivered/mirrored turn text — recorded so render-time projection
// (internal/asset.Project, via sessionlog.ReadAssetsByPath) can resolve that
// marker back to this asset without a second, send-time-only mechanism.
func (a *app) ingestAttachmentAsset(nodeID, name, mime, sourcePath string, data []byte) (sessionlog.AssetEvent, error) {
	ev := sessionlog.AssetEvent{
		ID:         sessionlog.NewAssetID(),
		Name:       sessionlog.SanitizeAssetName(name),
		Size:       int64(len(data)),
		SHA256:     sessionlog.SHA256Hex(data),
		SourceKind: "upload",
		SourcePath: sourcePath,
	}
	if mime == "" {
		mime = sessionlog.DetectMIME(ev.Name, data)
	}
	ev.Mime = mime
	ev.Storage = asset.StorageMode(ev.Size, assetInlineCap)
	if ev.Storage == "inline" {
		ev.Bytes = base64.StdEncoding.EncodeToString(data)
	} else {
		relPath, err := asset.WriteBlob(a.assetsDir, nodeID, ev.ID, ev.Name, data)
		if err != nil {
			return sessionlog.AssetEvent{}, err
		}
		ev.BlobPath = relPath
	}
	w := &sessionlog.Writer{Path: a.sessionLogPath(nodeID)}
	if err := w.Append(sessionlog.NewAsset(ev)); err != nil {
		return sessionlog.AssetEvent{}, err
	}
	return ev, nil
}
