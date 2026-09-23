package app

import (
	"errors"
	"io"
	"path/filepath"

	"github.com/scimux/scimux/internal/asset"
	"github.com/scimux/scimux/internal/sessionlog"
)

// agentAssetMaxBytes bounds how large an agent-referenced file scimux will
// actually copy into the asset store. This is a practical ceiling on the
// existing in-memory ingestion pipeline (ReadAll + WriteBlob both work on a
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
	turnRecord := latestVisibleTurnRecord(logPath)
	existingImports := map[[2]int]bool{}
	for _, ref := range sessionlog.ReadAssetImports(logPath) {
		existingImports[[2]int{ref.TurnRecord, ref.Occurrence}] = true
	}
	for occurrence, c := range cands {
		f, resolved, size, err := asset.Open(c.Ref, dir, roots)
		if errors.Is(err, asset.ErrOutsideRoot) && a.settings().AllowExternalAttachments {
			f, resolved, size, err = asset.OpenExternal(c.Ref, dir)
		}
		if err != nil {
			if f != nil {
				_ = f.Close()
			}
			a.recordAssetImportFailure(nodeID, turnRecord, occurrence, c, assetImportReason(err), existingImports)
			continue
		}
		if size > agentAssetMaxBytes {
			_ = f.Close()
			a.recordAssetImportFailure(nodeID, turnRecord, occurrence, c, "too_large", existingImports)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, agentAssetMaxBytes+1))
		_ = f.Close()
		// The descriptor size is only an early refusal. LimitReader is the hard
		// allocation ceiling if the already-open file grows while it is read.
		if err != nil || int64(len(data)) > agentAssetMaxBytes {
			a.recordAssetImportFailure(nodeID, turnRecord, occurrence, c, "unreadable", existingImports)
			continue
		}
		name := filepath.Base(resolved)
		if _, err := a.ingestAgentPathAsset(nodeID, c.Ref, name, data, bySHA, havePath); err != nil {
			a.recordAssetImportFailure(nodeID, turnRecord, occurrence, c, "storage", existingImports)
		}
	}
}

func latestVisibleTurnRecord(path string) int {
	latest := -1
	for i, ev := range sessionlog.ReadEvents(path) {
		if (ev.T == "user" || ev.T == "assistant") && ev.Text != "" {
			latest = i
		}
	}
	return latest
}

func assetImportReason(err error) string {
	switch {
	case errors.Is(err, asset.ErrOutsideRoot):
		return "outside_workspace"
	case errors.Is(err, asset.ErrNotFound):
		return "not_found"
	case errors.Is(err, asset.ErrNotRegularFile):
		return "not_regular"
	case errors.Is(err, asset.ErrUnreadable):
		return "unreadable"
	default:
		return "unreadable"
	}
}

func (a *app) recordAssetImportFailure(nodeID string, turnRecord, occurrence int, c asset.Candidate, reason string, existing map[[2]int]bool) {
	key := [2]int{turnRecord, occurrence}
	if existing[key] {
		return
	}
	ev := sessionlog.NewAssetImport(sessionlog.AssetImportEvent{
		TurnRecord: turnRecord, Occurrence: occurrence, Ref: c.Ref,
		Alt: c.Alt, Image: c.IsImage, Reason: reason,
	})
	if a.appendSessionEvent(nodeID, ev) == nil {
		existing[key] = true
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
	return a.ingestAgentPathAssetAt(nodeID, ref, name, data, bySHA, havePath, nil, nil, false)
}

func (a *app) ingestAgentPathAssetAt(nodeID, ref, name string, data []byte, bySHA map[string]sessionlog.AssetEvent, havePath map[string]bool, anchor, occurrence *int, retried bool) (sessionlog.AssetEvent, error) {
	sha := sessionlog.SHA256Hex(data)
	if rec, ok := bySHA[sha]; ok {
		if !havePath[ref] {
			alias := rec
			alias.SourceKind = "agent_path"
			alias.SourcePath = ref
			alias.AnchorRecord = anchor
			alias.AnchorOccurrence = occurrence
			alias.Retried = retried
			if err := a.appendSessionEvent(nodeID, sessionlog.NewAsset(alias)); err != nil {
				return sessionlog.AssetEvent{}, err
			}
			havePath[ref] = true
		}
		return rec, nil
	}
	ev, err := a.ingestAssetBytesAt(nodeID, name, "", "agent_path", ref, data, anchor, occurrence, retried)
	if err != nil {
		return sessionlog.AssetEvent{}, err
	}
	bySHA[sha] = ev
	havePath[ev.SourcePath] = true
	return ev, nil
}
