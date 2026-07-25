package main

import (
	"os"
	"path/filepath"

	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// agentAssetMaxBytes bounds how large an agent-referenced file scimux will
// actually copy into the asset store. This is a practical ceiling on the
// existing in-memory ingestion pipeline (ReadFile + WriteBlob both work on a
// whole []byte, matching upload ingestion), not part of the storage-mode
// decision: a file under this bound but over assetInlineCap still gets
// ingested as a blob (see asset.StorageMode). A file over this bound is the
// "truly too large to copy at all" case in upload-design.md's Guaranteed
// Outcome — it renders as an "unavailable" chip rather than being ingested.
const agentAssetMaxBytes = 50 << 20

// ingestAssetHook is the Phase 4 turn-append ingestion hook (asset.IngestFunc)
// wired into every transport at startup (main(), newTestApp): mirror.go for
// tmux, and internal/acp / internal/acp/codex via SetAssetHook. It runs once
// per turn-append, synchronously, per upload-design.md's "Ingestion Timing" —
// a short-lived agent process's temp files must be copied here, before they
// can disappear; handleChat's render-time projection never touches the
// filesystem again.
//
// Ineligible candidates (outside dir, unreadable, not a regular file, over
// agentAssetMaxBytes) are simply not ingested — no asset event, no error.
// That absence is itself the durable signal internal/asset.Project reads at
// render time to produce the "unavailable" chip, so eligibility is decided
// exactly once, here, never re-derived from a later filesystem check.
func (a *app) ingestAssetHook(nodeID, dir string, cands []asset.Candidate) {
	if a.sessionsDir == "" {
		return
	}
	roots := agentAssetRoots(dir)
	for _, c := range cands {
		resolved, size, err := asset.Resolve(c.Ref, dir, roots)
		if err != nil || size > agentAssetMaxBytes {
			continue
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			continue
		}
		name := c.Alt
		if name == "" {
			name = filepath.Base(resolved)
		}
		_, _ = a.ingestAgentPathAsset(nodeID, c.Ref, name, data)
	}
}

// agentAssetRoots is the allowed-root set for agent-generated path ingestion:
// the node's own working directory subtree. upload-design.md's Path
// Resolution Rules also call for "a small explicit allow-list of known
// agent-CLI output directories" (codex/Claude Code generated-image staging
// dirs); that list is still an open question there pending a survey of where
// each CLI actually stages files, so it is deliberately not guessed at here —
// widening this is a follow-up, not a silent guess baked into eligibility.
func agentAssetRoots(dir string) []string {
	return []string{dir}
}

// ingestAgentPathAsset ingests one already-resolved local file as an
// agent_path asset. It deduplicates by SHA-256 within the node (Deduplication,
// upload-design.md): identical bytes reuse the existing asset id instead of
// minting a duplicate record, while a path reused with changed bytes (a
// different hash) is not a dedup hit and mints a new one, as designed. ref is
// recorded verbatim as SourcePath — the exact text the candidate scan found
// in the turn, not the resolved absolute path — so render-time projection
// (internal/asset.Project) can match it back by simple string equality, the
// same pattern user-upload projection already uses.
func (a *app) ingestAgentPathAsset(nodeID, ref, name string, data []byte) (sessionlog.AssetEvent, error) {
	sha := sessionlog.SHA256Hex(data)
	for _, rec := range sessionlog.ReadAssets(a.sessionLogPath(nodeID)) {
		if rec.SHA256 == sha {
			return rec, nil
		}
	}
	return a.ingestAssetBytes(nodeID, name, "", "agent_path", ref, data)
}
