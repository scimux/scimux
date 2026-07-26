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
	logPath := a.sessionLogPath(nodeID)
	// Read the node's asset indexes once per hook call, not once per candidate:
	// a single turn can carry many candidates, and both dedup (bySHA) and the
	// idempotent path-alias check (havePath) consult them. Kept current
	// in-memory as we append, so a later candidate still sees an earlier one
	// ingested in the same turn.
	bySHA := map[string]sessionlog.AssetEvent{}
	for _, rec := range sessionlog.ReadAssets(logPath) {
		if _, ok := bySHA[rec.SHA256]; !ok {
			bySHA[rec.SHA256] = rec
		}
	}
	havePath := map[string]bool{}
	for p := range sessionlog.ReadAssetsByPath(logPath) {
		havePath[p] = true
	}
	for _, c := range cands {
		resolved, size, err := asset.Resolve(c.Ref, dir, roots)
		if err != nil || size > agentAssetMaxBytes {
			continue
		}
		data, err := os.ReadFile(resolved)
		// Re-check the length after the read, not just the pre-read Stat size:
		// agentAssetMaxBytes is a hard memory ceiling, and a file that grew
		// between Resolve's Stat and this read must not slip past it.
		if err != nil || int64(len(data)) > agentAssetMaxBytes {
			continue
		}
		name := c.Alt
		if name == "" {
			name = filepath.Base(resolved)
		}
		_, _ = a.ingestAgentPathAsset(nodeID, c.Ref, name, data, bySHA, havePath)
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
//
// bySHA/havePath are the caller's once-read, kept-current indexes (see
// ingestAssetHook). On a dedup hit whose stored SourcePath differs from THIS
// ref — the same bytes referenced under a different spelling, or an upload of
// the same file — a path-alias record is appended: same id and storage (so no
// second copy of the bytes is served; ReadAssets keeps the first record as
// durable), carrying this ref as SourcePath. Without it, render-time
// projection (keyed on SourcePath) would miss the ref and paint an
// "unavailable" chip for a file that is present. The havePath guard keeps a
// re-poll of the same turn idempotent — the alias is written at most once per
// distinct ref.
func (a *app) ingestAgentPathAsset(nodeID, ref, name string, data []byte, bySHA map[string]sessionlog.AssetEvent, havePath map[string]bool) (sessionlog.AssetEvent, error) {
	sha := sessionlog.SHA256Hex(data)
	if rec, ok := bySHA[sha]; ok {
		if !havePath[ref] {
			alias := rec
			alias.SourceKind = "agent_path"
			alias.SourcePath = ref
			w := &sessionlog.Writer{Path: a.sessionLogPath(nodeID)}
			if err := w.Append(sessionlog.NewAsset(alias)); err != nil {
				return sessionlog.AssetEvent{}, err
			}
			havePath[ref] = true
		}
		return rec, nil
	}
	ev, err := a.ingestAssetBytes(nodeID, name, "", "agent_path", ref, data)
	if err != nil {
		return sessionlog.AssetEvent{}, err
	}
	bySHA[sha] = ev
	havePath[ev.SourcePath] = true
	return ev, nil
}
