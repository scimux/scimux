package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/scimux/scimux/internal/asset"
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/transcript"
)

// The read-only window a search hit opens onto its surrounding conversation —
// for EVERY hit, live or deleted. The overlay used to split them: a live hit
// jumped straight into the chat, a past hit landed here, and the only way to
// learn which you were about to get was to tap and read the action bar that
// appeared. One surface removes the guess. The difference that actually matters
// — whether the chat still exists — is reported as Node and shows up in the head
// as the presence or absence of "Open chat".
//
// It stays deliberately NOT /api/nodes/{id}/chat even for a live node: no
// composer, no polling, no history, no mechanics — just a photo of the turns
// around the hit. Acting on the conversation is what "Open chat" is for.
const (
	previewWindowBefore = 8 // turns kept before the anchor
	previewWindowAfter  = 8 // turns kept after the anchor
)

type previewResponse struct {
	UID string `json:"uid"`
	// Node is the live node owning this log, empty when the chat is deleted.
	// The client needs nothing else to decide whether the head can offer a jump.
	Node            string            `json:"node,omitempty"`
	Title           string            `json:"title"`
	Agent           string            `json:"agent,omitempty"`
	Model           string            `json:"model,omitempty"`
	Turns           []transcript.Turn `json:"turns"`
	Assets          map[string]any    `json:"assets,omitempty"`
	Anchor          int               `json:"anchor"` // index into Turns of the hit turn
	BeforeTruncated bool              `json:"before_truncated"`
	AfterTruncated  bool              `json:"after_truncated"`
}

// handlePreview serves GET /api/preview?uid=<uid>&seg=<n>&rec=<n>&at=<turnTime>.
// uid is the log's on-disk identity (a meta UID, or "legacy:<ref>" for a
// header-less archived log) — one identifier for live and deleted alike, because
// a bookmark's stamped address only ever carries a uid. The hit is anchored by
// its stable (seg, rec) ordinal pair —
// the same identity ScanLog emits — resolved to a bounded turn window by
// ReadTurnWindow, so a duplicate or empty timestamp can never pick the wrong turn
// and a click never materializes the whole archived log. `at` remains a defensive
// fallback for a caller that has only the time. Every failure degrades to 404,
// never a 500 or a leak.
func (a *app) handlePreview(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	at := r.URL.Query().Get("at")
	seg, haveSeg := atoiOK(r.URL.Query().Get("seg"))
	rec, haveRec := atoiOK(r.URL.Query().Get("rec"))
	if uid == "" {
		http.Error(w, "uid required", 400)
		return
	}
	path, meta, nodeID, title, ok := a.resolvePreview(uid)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}

	// Prefer the stable (seg, rec) ordinal via a single bounded pass that keeps
	// only the window — never the whole archived log. Fall back to a full read +
	// timestamp scan only when no ordinal was supplied (a caller with just `at`).
	var (
		window                          []transcript.Turn
		anchor                          int
		beforeTruncated, afterTruncated bool
		resolved                        bool
	)
	if haveSeg && haveRec {
		if w, aIdx, bt, af, ok := sessionlog.ReadTurnWindow(path, seg, rec, previewWindowBefore, previewWindowAfter); ok {
			window, anchor, beforeTruncated, afterTruncated, resolved = w, aIdx, bt, af, true
		}
	}
	if !resolved {
		turns := sessionlog.ReadTurns(path)
		a := 0
		if at != "" {
			for i, t := range turns {
				if t.Time == at {
					a = i
					break
				}
			}
		}
		if a >= len(turns) {
			a = 0
		}
		lo := a - previewWindowBefore
		if lo < 0 {
			lo = 0
		}
		hi := a + previewWindowAfter + 1
		if hi > len(turns) {
			hi = len(turns)
		}
		window = turns[lo:hi]
		anchor = a - lo
		beforeTruncated = lo > 0
		afterTruncated = hi < len(turns)
	}
	if resolved && haveSeg && haveRec && anchor >= 0 && anchor < len(window) {
		window[anchor].UID = uid
		window[anchor].Segment = seg
		window[anchor].Record = rec
	}

	// Assets. A live node's blobs are still served, so the window gets the same
	// projection the chat read path applies — otherwise the identical turn would
	// read "unavailable" in the preview and fine one tap later, which teaches the
	// user that the preview lies. A deleted node's assets were archived away with
	// it, so there the marker degrades to an inert chip, as before.
	var assets map[string]any
	if nodeID != "" {
		window, assets = a.projectTurns(nodeID, window)
	} else {
		empty := map[string]sessionlog.AssetEvent{}
		for i := range window {
			window[i].Text = asset.Project(window[i].Text, empty)
			window[i].Text = asset.ProjectAgentPaths(window[i].Text, empty)
		}
	}

	resp := previewResponse{
		UID:             uid,
		Node:            nodeID,
		Title:           title,
		Turns:           window,
		Assets:          assets,
		Anchor:          anchor,
		BeforeTruncated: beforeTruncated,
		AfterTruncated:  afterTruncated,
	}
	if meta != nil {
		resp.Agent = meta.Agent
		resp.Model = meta.Model
	}
	if resp.Title == "" {
		resp.Title = "Deleted chat"
	}
	writeJSON(w, resp)
}

// resolvePreview maps a uid to the log file behind it, live logs first and the
// archive second, and reports which of the two it found.
//
// Live logs are matched by walking a.nodes rather than by scanning sessionsDir:
// the node list is the authority on what still exists, so a stray leftover file
// can never advertise "Open chat" for a node that is gone. A live chat's title
// comes from the node, not from the meta header — the header froze the slug at
// creation and chats get renamed.
//
// A "legacy:<ref>" uid carries a path *relative* to the archive directory
// (header-less logs have no UID to match on) — never an absolute path, which
// would leak the local data dir. The ref is never trusted raw: it is joined onto
// the archive dir and must resolve back inside it, or the log is treated as not
// found (traversal guard).
func (a *app) resolvePreview(uid string) (path string, meta *sessionlog.MetaEvent, nodeID, title string, ok bool) {
	if a.sessionsDir == "" {
		return "", nil, "", "", false
	}
	archiveDir := filepath.Join(a.sessionsDir, "archive")

	if ref, cut := strings.CutPrefix(uid, "legacy:"); cut {
		clean := filepath.Join(archiveDir, filepath.Clean("/"+ref))
		if !withinDir(archiveDir, clean) {
			return "", nil, "", "", false
		}
		if st, err := os.Stat(clean); err != nil || st.IsDir() {
			return "", nil, "", "", false
		}
		m := readLogMeta(clean)
		return clean, m, "", metaTitle(m), true
	}

	// Snapshot id+title under the lock; readLogMeta touches the disk and must not
	// hold it.
	a.mu.Lock()
	live := make([][2]string, 0, len(a.nodes))
	for _, n := range a.nodes {
		live = append(live, [2]string{n.ID, n.Title})
	}
	a.mu.Unlock()
	for _, n := range live {
		p := a.sessionLogPath(n[0])
		if m := readLogMeta(p); m != nil && m.UID == uid {
			t := n[1]
			if t == "" {
				t = metaTitle(m)
			}
			return p, m, n[0], t, true
		}
	}

	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return "", nil, "", "", false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(archiveDir, e.Name())
		if m := readLogMeta(p); m != nil && m.UID == uid {
			return p, m, "", metaTitle(m), true
		}
	}
	return "", nil, "", "", false
}

// metaTitle is the only name a deleted chat has left: the title slug its meta
// header froze at creation.
func metaTitle(m *sessionlog.MetaEvent) string {
	if m == nil {
		return ""
	}
	return m.Node
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
