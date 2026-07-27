package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// The read-only surface for a DELETED chat. A search hit on a live node opens
// the normal chat (that read path already exists); only a deleted node — dropped
// from a.byID, its log moved to sessions/archive/ — needs this bounded window.
// It is deliberately NOT /api/nodes/{id}/chat: there is no node, no process, no
// composer, no polling — just a photo of the turns around the hit so the
// supervisor can read what was said and, if the dir survives, fork from it.
const (
	archivedWindowBefore = 8 // turns kept before the anchor
	archivedWindowAfter  = 8 // turns kept after the anchor
)

type archivedResponse struct {
	UID             string            `json:"uid"`
	Title           string            `json:"title"`
	Agent           string            `json:"agent,omitempty"`
	Model           string            `json:"model,omitempty"`
	Effort          string            `json:"effort,omitempty"`
	Dir             string            `json:"dir,omitempty"`
	Forkable        bool              `json:"forkable"`
	Turns           []transcript.Turn `json:"turns"`
	Anchor          int               `json:"anchor"` // index into Turns of the hit turn
	BeforeTruncated bool              `json:"before_truncated"`
	AfterTruncated  bool              `json:"after_truncated"`
}

// handleArchived serves GET /api/archived?uid=<uid>&seg=<n>&rec=<n>&at=<turnTime>.
// uid is the deleted log's on-disk identity (a meta UID, or "legacy:<path>" for a
// header-less log). The hit is anchored by its stable (seg, rec) ordinal pair —
// the same identity ScanLog emits — resolved to a turn index by ResolveTurn, so a
// duplicate or empty timestamp can never pick the wrong turn. `at` remains a
// defensive fallback for a caller that has only the time. Every failure degrades
// to 404, never a 500 or a leak.
func (a *app) handleArchived(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	at := r.URL.Query().Get("at")
	seg, haveSeg := atoiOK(r.URL.Query().Get("seg"))
	rec, haveRec := atoiOK(r.URL.Query().Get("rec"))
	if uid == "" {
		http.Error(w, "uid required", 400)
		return
	}
	path, meta, ok := a.resolveArchived(uid)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}

	turns := sessionlog.ReadTurns(path)
	// Neutralize asset markers: archived blobs are not served in v1 (a deleted
	// node's assets are archived away), so any scimux-asset marker or raw agent
	// path degrades to an inert "unavailable" chip — same projection the live
	// chat applies, with an empty asset set.
	empty := map[string]sessionlog.AssetEvent{}
	for i := range turns {
		turns[i].Text = asset.Project(turns[i].Text, empty)
		turns[i].Text = asset.ProjectAgentPaths(turns[i].Text, empty)
	}

	// Prefer the stable (seg, rec) ordinal; fall back to the first turn matching
	// the timestamp only when no ordinal was supplied. Anchor 0 if neither resolves.
	anchor := 0
	if haveSeg && haveRec {
		if idx, ok := sessionlog.ResolveTurn(path, seg, rec); ok {
			anchor = idx
		}
	} else if at != "" {
		for i, t := range turns {
			if t.Time == at {
				anchor = i
				break
			}
		}
	}
	if anchor >= len(turns) {
		anchor = 0
	}
	lo := anchor - archivedWindowBefore
	if lo < 0 {
		lo = 0
	}
	hi := anchor + archivedWindowAfter + 1
	if hi > len(turns) {
		hi = len(turns)
	}

	resp := archivedResponse{
		UID:             uid,
		Turns:           turns[lo:hi],
		Anchor:          anchor - lo,
		BeforeTruncated: lo > 0,
		AfterTruncated:  hi < len(turns),
	}
	if meta != nil {
		resp.Title = meta.Node
		resp.Agent = meta.Agent
		resp.Model = meta.Model
		resp.Effort = meta.Effort
		resp.Dir = meta.Dir
		resp.Forkable = meta.Agent != "" && meta.Dir != "" && dirExists(meta.Dir)
	}
	if resp.Title == "" {
		resp.Title = "Deleted chat"
	}
	writeJSON(w, resp)
}

// resolveArchived maps a uid to its archive file. A "legacy:<path>" uid embeds
// the on-disk path (header-less logs have no UID to match on) — that path is
// never trusted raw: it must resolve inside the archive directory, or the log is
// treated as not found (traversal guard). A hex/tN uid is matched against the
// meta header of each archived log.
func (a *app) resolveArchived(uid string) (string, *sessionlog.MetaEvent, bool) {
	if a.sessionsDir == "" {
		return "", nil, false
	}
	archiveDir := filepath.Join(a.sessionsDir, "archive")

	if p, ok := strings.CutPrefix(uid, "legacy:"); ok {
		clean := filepath.Clean(p)
		if !withinDir(archiveDir, clean) {
			return "", nil, false
		}
		if st, err := os.Stat(clean); err != nil || st.IsDir() {
			return "", nil, false
		}
		return clean, readLogMeta(clean), true
	}

	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return "", nil, false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(archiveDir, e.Name())
		if m := readLogMeta(p); m != nil && m.UID == uid {
			return p, m, true
		}
	}
	return "", nil, false
}

// atoiOK parses a non-negative decimal query param. ok is false for an empty or
// malformed value, so a missing seg/rec cleanly falls back to time anchoring.
func atoiOK(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// withinDir reports whether p resolves inside dir (both cleaned). It rejects the
// dir itself and any path that climbs out via "..".
func withinDir(dir, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), p)
	if err != nil {
		return false
	}
	return rel != ".." && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
