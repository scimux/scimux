package sessionlog

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"unicode"

	"github.com/scimux/scimux/internal/transcript"
)

// scanCtxCheck is how often (in parsed lines) a context-aware scan re-checks for
// cancellation — often enough to abandon a superseded search promptly, rare
// enough that the check never dominates the scan.
const scanCtxCheck = 256

// Hit is one search match within a log. It is addressed by the log's UID plus
// the (Segment, Record) ordinal pair — the stable on-disk identity that survives
// slug reuse and does not shift when malformed lines are skipped. Before/Match/
// After are the raw excerpt window around the match (never HTML-escaped — the
// client escapes and wraps only Match in <mark>).
type Hit struct {
	UID     string          `json:"uid"`
	Segment int             `json:"segment"`
	Record  int             `json:"record"`
	Role    string          `json:"role"` // "user" | "assistant" | "asset"
	Time    string          `json:"time"`
	Before  string          `json:"before"`
	Match   string          `json:"match"`
	After   string          `json:"after"`
	Agent   string          `json:"agent,omitempty"` // actual harness id for assistant matches
	Prov    json.RawMessage `json:"prov,omitempty"`  // opaque provenance on the matching turn
}

// ScanOptions bounds one file's scan. Global caps (files scanned, total hits,
// groups) live in the search handler; these are the per-file limits.
type ScanOptions struct {
	Before   int   // max runes of context kept before the match
	After    int   // max runes of context kept after the match
	MaxHits  int   // stop after this many hits from this file (0 = unlimited)
	MaxBytes int64 // stop reading after this many bytes (0 = unlimited)
}

// ScanResult is the outcome of scanning one log file.
type ScanResult struct {
	UID     string // the log's MetaEvent.UID, or "" when it has none (caller maps to legacy:)
	Hits    []Hit
	Partial bool // a per-file cap (hits or bytes) stopped the scan early
}

// ScanLog streams one log file and returns case-insensitive substring matches of
// query in user/assistant prose and asset filenames. It is defensive like
// ReadEvents (a missing file or a malformed/torn line yields no error, just no
// record) but never slurps the whole file into memory: it reads line by line,
// keeping only the hits.
//
// Ordinals mirror the defensively-parsed record stream: Record is the 0-based
// index among successfully-unmarshaled records (blank/malformed lines do not
// count), and Segment is the number of `source` seams seen before the record.
// Tool records and asset SourcePaths are never matched, but every parsed record
// — including them — advances Record, so a hit resolves to the same turn that
// ReadSegment/ReadHistory would.
func ScanLog(path, query string, opt ScanOptions) ScanResult {
	return ScanLogCtx(context.Background(), path, query, opt)
}

// ScanLogCtx is ScanLog with cancellation: it re-checks ctx every scanCtxCheck
// lines and abandons the scan (marking the result Partial) the moment the caller
// gives up — the /api/search path passes the request context so a superseded
// keystroke stops scanning server-side, not just client-side. ScanLog is the
// context-free wrapper for callers (tests) that never cancel.
func ScanLogCtx(ctx context.Context, path, query string, opt ScanOptions) ScanResult {
	var res ScanResult
	q := []rune(strings.TrimSpace(query))
	if len(q) == 0 {
		return res
	}
	for i, r := range q {
		q[i] = unicode.ToLower(r)
	}

	f, err := os.Open(path)
	if err != nil {
		return res
	}
	defer f.Close()

	rec, seg := 0, 0
	var agent string
	// lastTurnTime is the timestamp of the most recent user/assistant turn in the
	// current segment. An asset filename hit is not itself a rendered turn, so it
	// anchors on the turn that owns it (the turn that produced/uploaded the file);
	// resetting at each seam keeps that owner inside the same chat surface. Empty
	// until the segment's first turn.
	var lastTurnTime string
	// pendingAssetHits are asset hits with no preceding turn in their segment (the
	// upload flow: the asset record is appended before the prompt that references
	// it). Their owner is the *next* turn in the same segment — the same forward
	// ownership ReadTurnWindow applies — so we defer their Time until that turn
	// arrives. If the segment ends first (seam or EOF) they keep their own time.
	var pendingAssetHits []int
	var lineNo int
	// One budget check, reached from every exit: over it, the scan stops and
	// says so rather than reporting a whole-file answer it did not read.
	overBudget := func(off int64) bool {
		if opt.MaxBytes > 0 && off > opt.MaxBytes {
			res.Partial = true
			return true
		}
		return false
	}
	forEachRecord(f, func(line []byte, off int64) bool {
		lineNo++
		if lineNo%scanCtxCheck == 0 && ctx.Err() != nil {
			res.Partial = true
			return false
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil {
			// malformed/torn/oversized line: skipped like ReadEvents, no ordinal shift
			return !overBudget(off)
		}

		// This is parsed record `rec`, in segment `seg`.
		role, text := searchable(ev)
		if role != "" {
			if b, m, a, ok := excerpt(text, q, opt.Before, opt.After); ok {
				// An asset hit reports the owning turn's time (the turn it belongs
				// to), never the asset record's own time — that is a private,
				// unrendered record; anchoring on it would never match a turn.
				hitTime := ev.Time
				if role == "asset" && lastTurnTime != "" {
					hitTime = lastTurnTime
				}
				h := Hit{
					Segment: seg, Record: rec, Role: role, Time: hitTime,
					Before: b, Match: m, After: a,
				}
				if role == "assistant" {
					h.Agent = agent
				}
				if role == "user" || role == "assistant" {
					h.Prov = copyRaw(ev.Prov)
				}
				res.Hits = append(res.Hits, h)
				if role == "asset" && lastTurnTime == "" {
					pendingAssetHits = append(pendingAssetHits, len(res.Hits)-1)
				}
				if opt.MaxHits > 0 && len(res.Hits) >= opt.MaxHits {
					res.Partial = true
					return false
				}
			}
		}
		if (ev.T == "user" || ev.T == "assistant") && strings.TrimSpace(ev.Text) != "" {
			lastTurnTime = ev.Time
			// This turn owns any asset hits that preceded it in this segment.
			for _, i := range pendingAssetHits {
				res.Hits[i].Time = ev.Time
			}
			pendingAssetHits = nil
		}
		if ev.T == "meta" && ev.Meta != nil {
			if res.UID == "" {
				res.UID = ev.Meta.UID
			}
			if ev.Meta.Agent != "" {
				agent = ev.Meta.Agent
			}
		}
		if ev.T == "source" {
			seg++
			lastTurnTime = ""      // owner search never crosses a seam
			pendingAssetHits = nil // forward owner never crosses a seam either
		}
		rec++
		return !overBudget(off)
	})

	for i := range res.Hits {
		res.Hits[i].UID = res.UID
	}
	return res
}

// ReadTurnWindow maps a search hit's stable (segment, record) identity to the
// window of chat turns around the turn it owns, streaming the log in a single
// bounded pass. It never materializes every turn (the reason it replaces a
// ReadTurns+ReadEvents pair on the deleted-chat click path): it keeps a ring of
// at most `before`+1 trailing turns and collects at most `after` following ones,
// so a click in a huge archive allocates a small window, not the whole file.
//
// Records are counted exactly as ScanLog assigns them (segment = source seams
// seen, record = every successfully parsed record), so the window centers on the
// same turn the excerpt was cut from — an ordinal, not a timestamp, so duplicate
// or empty times can't mis-anchor it. A hit on a non-turn record (an asset
// filename) resolves to the turn that owns it: the nearest turn at or before the
// record within the same segment, or — for a record that precedes every turn in
// its segment (an upload referenced by the following prompt) — the next turn in
// that segment. The owner search never crosses a `source` seam: a seam reached
// while still waiting for a "next turn" owner ends the search unresolved (ok=false),
// so ownership stays inside one chat surface.
//
// Returns the window turns, the anchor turn's index within that window,
// before/after truncation flags, and ok=false when the identity resolves to no
// turn (out of range, seam-terminated, or an empty log). Not cached: a per-tap read.
func ReadTurnWindow(path string, segment, record, before, after int) (window []transcript.Turn, anchor int, beforeTrunc, afterTrunc bool, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, false, false, false
	}
	defer f.Close()
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}
	ring := make([]transcript.Turn, 0, before+1) // trailing window, newest last; the anchor is its final element
	dropped := 0                                 // turns evicted from the ring front → beforeTrunc
	var afterTurns []transcript.Turn

	rec, seg, turnCount, lastInSeg := 0, 0, 0, -1
	var agent string
	pendingNext := false // owner is the next turn in this segment
	resolved := false    // the (segment, record) ordinal has been located
	anchorOrd := -1      // absolute turn index of the owning turn, once known
	collecting := false  // past the anchor: filling afterTurns

	forEachRecord(f, func(line []byte, _ int64) bool {
		var ev Event
		if json.Unmarshal(line, &ev) != nil {
			return true // malformed/torn/oversized: skipped, no ordinal shift
		}
		isTurn := (ev.T == "user" || ev.T == "assistant") && strings.TrimSpace(ev.Text) != ""

		if !pendingNext && !resolved && seg == segment && rec == record {
			resolved = true
			switch {
			case isTurn:
				anchorOrd = turnCount // this record is the owning turn itself
			case lastInSeg >= 0:
				anchorOrd = lastInSeg // owner is the nearest preceding turn — already the ring's newest
				collecting = true
			default:
				pendingNext = true // owner is the next turn in this segment
			}
		}

		if ev.T == "meta" && ev.Meta != nil && ev.Meta.Agent != "" {
			agent = ev.Meta.Agent
		}
		if isTurn {
			if pendingNext {
				anchorOrd = turnCount
				pendingNext = false
			}
			turn := chatTurn(ev, "", agent, 0, 0)
			switch {
			case collecting:
				if len(afterTurns) < after {
					afterTurns = append(afterTurns, turn)
				} else {
					afterTrunc = true
				}
			default:
				ring = append(ring, turn)
				if len(ring) > before+1 {
					ring = ring[1:]
					dropped++
				}
				if anchorOrd == turnCount { // this turn is the anchor (own-record or next-turn owner)
					collecting = true
				}
			}
			lastInSeg = turnCount
			turnCount++
		}
		if ev.T == "source" {
			seg++
			lastInSeg = -1
			pendingNext = false // a seam terminates the owner search — never cross it
		}
		rec++
		// The window is complete once the ring, a full after-window and one
		// extra record proving truncation are in hand.
		return !(collecting && afterTrunc)
	})

	if anchorOrd < 0 {
		return nil, 0, false, false, false
	}
	window = append(ring, afterTurns...)
	return window, len(ring) - 1, dropped > 0, afterTrunc, true
}

// StringMatch is the excerpt window ScanString found for a single string.
type StringMatch struct {
	Before string
	Match  string
	After  string
	OK     bool
}

// ScanString runs the same bounded-window excerpt logic ScanLog uses on prose,
// but over a single caller-supplied string (a note's text) rather than a log
// file. The query is matched case-insensitively; spans are raw (the caller
// escapes them). OK is false when the query is absent or empty.
func ScanString(text, query string, before, after int) StringMatch {
	q := []rune(strings.TrimSpace(query))
	if len(q) == 0 {
		return StringMatch{}
	}
	for i, r := range q {
		q[i] = unicode.ToLower(r)
	}
	b, m, a, ok := excerpt(text, q, before, after)
	return StringMatch{Before: b, Match: m, After: a, OK: ok}
}

// searchable returns the role and text a record contributes to the corpus, or
// ("", "") for record types that are counted in the ordinal but never matched
// (meta, source, tool, usage, mark, stop, error). Asset records contribute their
// filename only — never SourcePath (a private local path).
func searchable(ev Event) (role, text string) {
	switch ev.T {
	case "user", "assistant":
		if strings.TrimSpace(ev.Text) != "" {
			return ev.T, ev.Text
		}
	case "asset":
		if ev.Asset != nil && strings.TrimSpace(ev.Asset.Name) != "" {
			return "asset", ev.Asset.Name
		}
	}
	return "", ""
}

// excerpt finds the first case-insensitive occurrence of the already-lowered
// query in text and returns a bounded rune window around it. Case folding is
// per-rune on the original rune slice so indices stay 1:1 (ToLower on the whole
// string can change rune counts, e.g. 'İ'). Match preserves the original casing;
// context is trimmed to a word boundary when it was cut mid-word and whitespace
// is collapsed for display. Spans are raw — the caller escapes them.
func excerpt(text string, qLower []rune, before, after int) (b, m, a string, ok bool) {
	orig := []rune(text)
	low := make([]rune, len(orig))
	for i, r := range orig {
		low[i] = unicode.ToLower(r)
	}
	idx := indexRunes(low, qLower)
	if idx < 0 {
		return "", "", "", false
	}
	end := idx + len(qLower)

	bs := idx - before
	if bs < 0 {
		bs = 0
	}
	as := end + after
	if as > len(orig) {
		as = len(orig)
	}
	bRunes := orig[bs:idx]
	aRunes := orig[end:as]
	if bs > 0 {
		bRunes = trimLeadingPartialWord(bRunes)
	}
	if as < len(orig) {
		aRunes = trimTrailingPartialWord(aRunes)
	}
	return collapseWS(bRunes), collapseWS(orig[idx:end]), collapseWS(aRunes), true
}

// indexRunes returns the index of the first occurrence of needle in hay, or -1.
func indexRunes(hay, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(hay) {
		return -1
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// trimLeadingPartialWord drops a leading run of non-space runes (a word the
// window cut through) up to the first space, but only when a space exists — a
// cheap word-boundary trim, never one that empties an all-one-word window.
func trimLeadingPartialWord(r []rune) []rune {
	for i, c := range r {
		if unicode.IsSpace(c) {
			return r[i:]
		}
	}
	return r
}

// trimTrailingPartialWord mirrors trimLeadingPartialWord at the tail.
func trimTrailingPartialWord(r []rune) []rune {
	for i := len(r) - 1; i >= 0; i-- {
		if unicode.IsSpace(r[i]) {
			return r[:i+1]
		}
	}
	return r
}

// collapseWS collapses runs of whitespace to a single space and trims the ends,
// so a multi-line excerpt reads as one line.
func collapseWS(r []rune) string {
	var sb strings.Builder
	space := false
	for _, c := range r {
		if unicode.IsSpace(c) {
			space = true
			continue
		}
		if space && sb.Len() > 0 {
			sb.WriteRune(' ')
		}
		space = false
		sb.WriteRune(c)
	}
	return sb.String()
}
