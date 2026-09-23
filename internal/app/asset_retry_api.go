package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"

	"github.com/scimux/scimux/internal/asset"
	"github.com/scimux/scimux/internal/sessionlog"
)

type assetRetryRequest struct {
	TurnRecord *int `json:"turn_record"`
	Occurrence *int `json:"occurrence"`
}

func (a *app) handleAssetImportRetry(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if n.EndedAt != "" || n.Adopted {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"status": "failed", "reason": "chat_unavailable", "message": "Chat is unavailable for this operation."})
		return
	}
	var req assetRetryRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.TurnRecord == nil || req.Occurrence == nil || *req.TurnRecord < 0 || *req.Occurrence < 0 {
		http.Error(w, "invalid recorded reference", http.StatusBadRequest)
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "invalid recorded reference", http.StatusBadRequest)
		return
	}

	a.assetImportMu.Lock()
	defer a.assetImportMu.Unlock()
	turnRecord, occurrence := *req.TurnRecord, *req.Occurrence
	logPath := a.sessionLogPath(n.ID)
	var ref *sessionlog.AssetImportEvent
	for _, candidate := range sessionlog.ReadAssetImports(logPath) {
		if candidate.TurnRecord == turnRecord && candidate.Occurrence == occurrence {
			copy := candidate
			ref = &copy
			break
		}
	}
	if ref == nil {
		http.Error(w, "recorded reference not found", http.StatusNotFound)
		return
	}
	events := sessionlog.ReadEvents(logPath)
	if turnRecord >= len(events) || (events[turnRecord].T != "user" && events[turnRecord].T != "assistant") {
		http.Error(w, "recorded reference not found", http.StatusNotFound)
		return
	}
	candidates := asset.ScanMarkdown(events[turnRecord].Text)
	if occurrence >= len(candidates) || candidates[occurrence].Ref != ref.Ref ||
		candidates[occurrence].Alt != ref.Alt || candidates[occurrence].IsImage != ref.Image {
		http.Error(w, "recorded reference not found", http.StatusNotFound)
		return
	}
	for _, bound := range sessionlog.ReadAnchoredAssets(logPath) {
		if bound.Anchor == turnRecord && bound.Asset.AnchorOccurrence != nil &&
			*bound.Asset.AnchorOccurrence == occurrence && bound.Asset.SourcePath == ref.Ref {
			writeJSON(w, map[string]any{"status": "already_imported", "asset_id": bound.Asset.ID, "name": bound.Asset.Name})
			return
		}
	}
	if ref.Reason == "outside_workspace" && !a.settings().AllowExternalAttachments {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"status": "failed", "reason": "outside_workspace", "message": "External attachments are disabled."})
		return
	}

	f, resolved, size, err := asset.Open(ref.Ref, n.Dir, agentAssetRoots(n.Dir))
	if errors.Is(err, asset.ErrOutsideRoot) {
		if !a.settings().AllowExternalAttachments {
			writeJSONStatus(w, http.StatusConflict, map[string]any{"status": "failed", "reason": "outside_workspace", "message": "External attachments are disabled."})
			return
		}
		f, resolved, size, err = asset.OpenExternal(ref.Ref, n.Dir)
	}
	if err != nil {
		retryFailure(w, assetImportReason(err))
		return
	}
	defer f.Close()
	if size > agentAssetMaxBytes {
		retryFailure(w, "too_large")
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, agentAssetMaxBytes+1))
	if err != nil {
		retryFailure(w, "unreadable")
		return
	}
	if int64(len(data)) > agentAssetMaxBytes {
		retryFailure(w, "too_large")
		return
	}
	bySHA := map[string]sessionlog.AssetEvent{}
	for _, rec := range sessionlog.ReadAssets(logPath) {
		if _, exists := bySHA[rec.SHA256]; !exists {
			bySHA[rec.SHA256] = rec
		}
	}
	havePath := map[string]bool{}
	// The exact turn+occurrence binding is new even when this path or these
	// bytes were imported elsewhere. Force the dedup branch to append its
	// zero-copy alias; the idempotency check above prevents duplicates.
	anchor := turnRecord
	ev, err := a.ingestAgentPathAssetAt(n.ID, ref.Ref, filepath.Base(resolved), data, bySHA, havePath, &anchor, &occurrence, true)
	if err != nil {
		retryFailure(w, "storage")
		return
	}
	// Drop the projection object so the next chat read cannot retain a snapshot
	// from before the worker-owned append.
	a.mu.Lock()
	delete(a.logCache, n.ID)
	a.mu.Unlock()
	writeJSON(w, map[string]any{
		"status": "imported", "asset_id": ev.ID, "name": ev.Name,
		"turn_record": turnRecord, "occurrence": occurrence,
		"current_bytes": true,
	})
}

func retryFailure(w http.ResponseWriter, reason string) {
	messages := map[string]string{
		"not_found":   "File no longer exists.",
		"unreadable":  "File cannot be read.",
		"not_regular": "Unsupported filesystem object.",
		"too_large":   "File exceeds the import limit.",
		"storage":     "Storage budget, free-space, or history write refused the import.",
	}
	message := messages[reason]
	if message == "" {
		message = "Attachment could not be imported."
	}
	writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"status": "failed", "reason": reason, "message": message})
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
