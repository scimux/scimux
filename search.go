package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// Search bounds. These are the global caps the /api/search handler layers over
// ScanLog's per-file limits: a query as short as minSearchQuery is ignored (the
// field is searched live as the user types), each file yields at most
// searchHitsPerFile hits, the response carries at most searchMaxGroups groups,
// and any cap tripping sets Partial.
const (
	minSearchQuery     = 2
	maxSearchQuery     = 128     // clamp the scanned query so a pathological length can't drive the scan cost
	searchHitsPerFile  = 20      // per-file hit cap (ScanLog)
	searchHitsPerGroup = 20      // per-group hit cap after log+note merge
	searchMaxFiles     = 500     // stop scanning after this many catalog sources
	searchMaxHitsTotal = 500     // stop scanning once this many log hits accumulate
	searchMaxGroups    = 50      // response group cap
	searchBytesCap     = 8 << 20 // per-file read ceiling, matches ScanLog's buffer
	searchCtxBefore    = 60      // excerpt window, runes
	searchCtxAfter     = 60
)

// searchHitJSON is one match in the response. Log hits carry (segment, record);
// note hits carry note_id and the referenced turn_time. Role is user|assistant|
// asset|note.
type searchHitJSON struct {
	Role     string `json:"role"`
	Segment  int    `json:"segment,omitempty"`
	Record   int    `json:"record,omitempty"`
	NoteID   string `json:"note_id,omitempty"`
	Time     string `json:"time"`
	TurnTime string `json:"turn_time,omitempty"`
	Before   string `json:"before"`
	Match    string `json:"match"`
	After    string `json:"after"`
}

// searchGroupJSON is one chat's (or the Notes bucket's) matches. Kind is
// live|archived|notes; ID is the live node id (show-to-chat live path, fork);
// UID is the log's on-disk identity ("legacy:<path>" for header-less logs, empty
// for the Notes bucket).
type searchGroupJSON struct {
	Kind     string          `json:"kind"`
	ID       string          `json:"id,omitempty"`
	UID      string          `json:"uid,omitempty"`
	Title    string          `json:"title"`
	LaneID   string          `json:"lane_id,omitempty"`
	Agent    string          `json:"agent,omitempty"`
	Forkable bool            `json:"forkable"`
	Hits     []searchHitJSON `json:"hits"`

	newest time.Time // ordering key, not serialized
}

type searchResponseJSON struct {
	Query   string            `json:"query"`
	Groups  []searchGroupJSON `json:"groups"`
	Partial bool              `json:"partial"`
}

// searchSource is one log to scan plus the group header it produces. It is
// assembled per request from three places (the searchCatalog): live nodes from
// memory, archived logs from their on-disk meta header, and — separately — notes
// from ui.json.
type searchSource struct {
	kind     string // "live" | "archived"
	id       string // live node id ("" for archived)
	path     string
	title    string
	laneID   string
	agent    string
	forkable bool
}

// searchCatalog enumerates every log the corpus can match this request: the live
// nodes held in memory and the archived logs on disk. Notes (ui.json) are folded
// in later, in handleSearch. It does not touch nodes.jsonl — dead nodes live as
// their archived log file, self-described by its meta header.
func (a *app) searchCatalog() []searchSource {
	var out []searchSource

	a.mu.Lock()
	for _, n := range a.nodes {
		out = append(out, searchSource{
			kind:     "live",
			id:       n.ID,
			path:     a.sessionLogPath(n.ID),
			title:    n.Title,
			laneID:   n.LaneID,
			agent:    n.Agent,
			forkable: n.Dir != "" && dirExists(n.Dir),
		})
	}
	a.mu.Unlock()

	if a.sessionsDir == "" {
		return out
	}
	archiveDir := filepath.Join(a.sessionsDir, "archive")
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(archiveDir, e.Name())
		meta := readLogMeta(path)
		src := searchSource{kind: "archived", path: path}
		if meta != nil && meta.UID != "" {
			src.title = meta.Node
			src.agent = meta.Agent
			src.forkable = meta.Agent != "" && meta.Dir != "" && dirExists(meta.Dir)
		}
		if src.title == "" {
			src.title = strings.TrimSuffix(e.Name(), ".jsonl")
		}
		out = append(out, src)
	}
	return out
}

// readLogMeta returns a log's meta header, or nil for a header-less/unreadable
// log. It streams only the leading lines — the header is the first record by
// construction — and is defensive: a missing file or a torn/malformed line is
// skipped, never an error. It stops at the first successfully-parsed record,
// since a non-meta first record means the log has no header.
func readLogMeta(path string) *sessionlog.MetaEvent {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), searchBytesCap)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev sessionlog.Event
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.T == "meta" {
			return ev.Meta
		}
		return nil
	}
	return nil
}

// dirExists reports whether path is an existing directory — the dir-gone guard
// on fork eligibility.
func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// searchNote is the narrow, defensive projection of a ui.json note the server is
// willing to read: the client owns ui.json's shape, so the reader takes only
// these fields and ignores everything else. The note id is `t` (its ISO
// creation stamp); node/turnTime tie it back to a chat; text is the corpus.
type searchNote struct {
	T        string `json:"t"`
	Text     string `json:"text"`
	Node     string `json:"node"`
	Lane     string `json:"lane"`
	TurnTime string `json:"turnTime"`
	Anchor   string `json:"anchor"`
}

// handleSearch answers GET /api/search?q= with the corpus matches grouped by
// chat and ordered by recency. A query shorter than minSearchQuery returns an
// empty result (the field is searched live as the user types), never an error;
// everything downstream is defensive — a missing log, a torn line, an
// unparseable ui.json all degrade to fewer hits, never a 500.
func (a *app) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	// Query-length caps, in order: a too-short query returns empty (the field is
	// searched live as it's typed); a too-long one is clamped so its cost is bounded.
	if qr := []rune(q); len(qr) > maxSearchQuery {
		q = string(qr[:maxSearchQuery])
	}
	resp := searchResponseJSON{Query: q, Groups: []searchGroupJSON{}}
	if len([]rune(q)) < minSearchQuery {
		writeJSON(w, resp)
		return
	}

	opt := sessionlog.ScanOptions{
		Before: searchCtxBefore, After: searchCtxAfter,
		MaxHits: searchHitsPerFile, MaxBytes: searchBytesCap,
	}

	// groups keyed by a stable identity: live node id, archived uid/legacy path,
	// or the "notes" bucket. liveSrc lets a note fold into its owning live chat's
	// group even when that chat produced no text hit of its own.
	groups := map[string]*searchGroupJSON{}
	liveSrc := map[string]searchSource{}
	partial := false

	order := func(g *searchGroupJSON, t time.Time) {
		if t.After(g.newest) {
			g.newest = t
		}
	}

	catalog := a.searchCatalog()
	for _, src := range catalog {
		if src.kind == "live" {
			liveSrc[src.id] = src
		}
	}
	filesScanned, totalHits := 0, 0
	for _, src := range catalog {
		// Stop scanning once a global cap is met — the discipline that keeps the
		// no-cache scan bounded even against a large archive: files scanned and
		// total hits are hard ceilings, and tripping either marks the result partial.
		if filesScanned >= searchMaxFiles || totalHits >= searchMaxHitsTotal {
			partial = true
			break
		}
		filesScanned++
		res := sessionlog.ScanLog(src.path, q, opt)
		if res.Partial {
			partial = true
		}
		if len(res.Hits) == 0 {
			continue
		}
		totalHits += len(res.Hits)
		key, uid := src.id, res.UID
		if src.kind == "archived" {
			if uid == "" {
				uid = "legacy:" + src.path
			}
			key = uid
		}
		g := &searchGroupJSON{
			Kind: src.kind, ID: src.id, UID: uid,
			Title: src.title, LaneID: src.laneID, Agent: src.agent,
			Forkable: src.forkable && uid != "" && !strings.HasPrefix(uid, "legacy:"),
		}
		for _, h := range res.Hits {
			g.Hits = append(g.Hits, searchHitJSON{
				Role: h.Role, Segment: h.Segment, Record: h.Record, Time: h.Time,
				Before: h.Before, Match: h.Match, After: h.After,
			})
			order(g, parseSearchTime(h.Time))
		}
		groups[key] = g
	}

	// Notes: fold into the owning live chat when it matched or exists; otherwise
	// the "Notes" bucket. A note referencing a live node that produced no log hit
	// still needs a group, so create the live group on demand from its source.
	for _, note := range a.searchNotes() {
		b, m, af, ok := searchExcerpt(note.Text, q)
		if !ok {
			continue
		}
		hit := searchHitJSON{
			Role: "note", NoteID: note.T, Time: note.T, TurnTime: note.TurnTime,
			Before: b, Match: m, After: af,
		}
		var g *searchGroupJSON
		if src, ok := liveSrc[note.Node]; note.Node != "" && ok {
			g = groups[note.Node]
			if g == nil {
				g = &searchGroupJSON{
					Kind: "live", ID: src.id, Title: src.title,
					LaneID: src.laneID, Agent: src.agent, Forkable: src.forkable,
				}
				groups[note.Node] = g
			}
		}
		if g == nil {
			g = groups["notes"]
			if g == nil {
				g = &searchGroupJSON{Kind: "notes", Title: "Notes"}
				groups["notes"] = g
			}
		}
		g.Hits = append(g.Hits, hit)
		order(g, parseSearchTime(note.T))
	}

	for _, g := range groups {
		sort.SliceStable(g.Hits, func(i, j int) bool {
			return parseSearchTime(g.Hits[i].Time).After(parseSearchTime(g.Hits[j].Time))
		})
		// Per-group hit cap: applied after the log+note merge so a single chat can't
		// dominate the feed. The newest hits survive (the sort above is newest-first).
		if len(g.Hits) > searchHitsPerGroup {
			g.Hits = g.Hits[:searchHitsPerGroup]
			partial = true
		}
		resp.Groups = append(resp.Groups, *g)
	}
	sort.SliceStable(resp.Groups, func(i, j int) bool {
		return resp.Groups[i].newest.After(resp.Groups[j].newest)
	})
	if len(resp.Groups) > searchMaxGroups {
		resp.Groups = resp.Groups[:searchMaxGroups]
		partial = true
	}
	resp.Partial = partial
	writeJSON(w, resp)
}

// searchNotes reads ui.json through the narrow defensive reader. Any failure —
// missing file, invalid JSON, notes not an array — yields no notes, never an
// error: the server treats the client-owned document as untrusted input.
func (a *app) searchNotes() []searchNote {
	if a.uiPath == "" {
		return nil
	}
	b, err := os.ReadFile(a.uiPath)
	if err != nil {
		return nil
	}
	var doc struct {
		Notes []searchNote `json:"notes"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	return doc.Notes
}

// searchExcerpt runs ScanLog's excerpt window over a single string (a note's
// text), returning the escaped-later before/match/after spans, or ok=false when
// the query is absent.
func searchExcerpt(text, query string) (before, match, after string, ok bool) {
	res := sessionlog.ScanString(text, query, searchCtxBefore, searchCtxAfter)
	if !res.OK {
		return "", "", "", false
	}
	return res.Before, res.Match, res.After, true
}

// parseSearchTime parses an RFC3339 stamp for recency ordering, tolerating the
// nanosecond and second-precision variants both transports emit. An unparseable
// or empty stamp sorts oldest (zero time).
func parseSearchTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
