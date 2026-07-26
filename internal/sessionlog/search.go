package sessionlog

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"unicode"
)

// Hit is one search match within a log. It is addressed by the log's UID plus
// the (Segment, Record) ordinal pair — the stable on-disk identity that survives
// slug reuse and does not shift when malformed lines are skipped. Before/Match/
// After are the raw excerpt window around the match (never HTML-escaped — the
// client escapes and wraps only Match in <mark>).
type Hit struct {
	UID     string `json:"uid"`
	Segment int    `json:"segment"`
	Record  int    `json:"record"`
	Role    string `json:"role"` // "user" | "assistant" | "asset"
	Time    string `json:"time"`
	Before  string `json:"before"`
	Match   string `json:"match"`
	After   string `json:"after"`
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

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	rec, seg := 0, 0
	var bytesRead int64
	for sc.Scan() {
		line := sc.Bytes()
		bytesRead += int64(len(line)) + 1 // +1 for the stripped newline
		if len(strings.TrimSpace(string(line))) == 0 {
			// blank line: not a record, does not advance ordinals
			if opt.MaxBytes > 0 && bytesRead > opt.MaxBytes {
				res.Partial = true
				break
			}
			continue
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil {
			// malformed/torn line: skipped like ReadEvents, no ordinal shift
			if opt.MaxBytes > 0 && bytesRead > opt.MaxBytes {
				res.Partial = true
				break
			}
			continue
		}

		// This is parsed record `rec`, in segment `seg`.
		role, text := searchable(ev)
		if role != "" {
			if b, m, a, ok := excerpt(text, q, opt.Before, opt.After); ok {
				res.Hits = append(res.Hits, Hit{
					Segment: seg, Record: rec, Role: role, Time: ev.Time,
					Before: b, Match: m, After: a,
				})
				if opt.MaxHits > 0 && len(res.Hits) >= opt.MaxHits {
					res.Partial = true
					break
				}
			}
		}
		if ev.T == "meta" && res.UID == "" && ev.Meta != nil {
			res.UID = ev.Meta.UID
		}
		if ev.T == "source" {
			seg++
		}
		rec++

		if opt.MaxBytes > 0 && bytesRead > opt.MaxBytes {
			res.Partial = true
			break
		}
	}

	for i := range res.Hits {
		res.Hits[i].UID = res.UID
	}
	return res
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
