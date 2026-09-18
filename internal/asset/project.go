package asset

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/scimux/scimux/internal/sessionlog"
)

// attachRefRE matches the plain-text attachment marker extendPrompt appends
// to a delivered user turn ("[attached image: /abs/path]" / "[attached
// file: /abs/path]") — the literal text mirrored/recorded turns carry (see
// extendPrompt in attachment_api.go), not Markdown, so it needs its own pattern
// rather than reusing ScanMarkdown.
var attachRefRE = regexp.MustCompile(`\[attached (image|file): ([^\]]+)\]`)

// Project rewrites a turn's attachment markers into scimux-asset:<id>
// Markdown, using byPath (asset events indexed by SourcePath, see
// sessionlog.ReadAssetsByPath) to resolve each marker to its ingested
// asset. A marker whose path was never ingested — a log predating this
// mechanism, or a failed ingest — is left exactly as written: there is no
// separate legacy-rendering path (hard cut; see upload-design.md
// "Migration And Compatibility"). Never mutates the stored log; this runs
// at read time only.
func Project(text string, byPath map[string]sessionlog.AssetEvent) string {
	if len(byPath) == 0 || !attachRefRE.MatchString(text) {
		return text
	}
	return attachRefRE.ReplaceAllStringFunc(text, func(m string) string {
		sub := attachRefRE.FindStringSubmatch(m)
		kind, path := sub[1], sub[2]
		ev, ok := byPath[path]
		if !ok {
			return m
		}
		name := ev.Name
		if name == "" {
			name = "asset"
		}
		if kind == "image" {
			return fmt.Sprintf("![%s](scimux-asset:%s)", name, ev.ID)
		}
		return fmt.Sprintf("[%s](scimux-asset:%s)", name, ev.ID)
	})
}

var assetRefRE = regexp.MustCompile(`scimux-asset:([A-Za-z0-9_]+)`)

// ReferencedIDs extracts every scimux-asset:<id> reference from already
// projected text, in first-seen order with duplicates removed — used to
// build a chat response's asset map from only what the returned turns
// actually reference.
func ReferencedIDs(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range assetRefRE.FindAllStringSubmatch(text, -1) {
		id := m[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// ProjectAgentPaths rewrites a turn's general Markdown image/link references
// — ![alt](path) / [text](path), as ScanMarkdown detects them, distinct from
// the [attached ...] marker syntax Project handles above — into
// scimux-asset:<id> Markdown, using byPath (asset events indexed by
// SourcePath; the same map Project uses, since ingestAgentPathAsset records
// the literal candidate ref text there too). Fenced code blocks are left
// untouched, matching ScanMarkdown's exclusion. Non-path targets — URLs
// (http, mailto, data, already-projected scimux-asset:) and in-document
// fragments (#heading) — are left alone via the same isLocalPathRef filter
// ScanMarkdown uses, so ordinary agent prose with anchor/reference links is
// never turned into an "unavailable" chip.
//
// A reference whose path was never ingested — outside the allowed root,
// unreadable, too large, or scanned from a log written before this
// mechanism existed — is left exactly as written, same as Project's
// unmatched markers above: these paths were never going to be servable
// (most commonly an agent like codex pointing at a source file outside its
// sandbox root), so turning every one into an inert "unavailable" chip just
// clutters the chat with dead file affordances for prose that was never an
// attachment in the first place. Plain path text degrades gracefully; a
// chip implies a broken feature.
func ProjectAgentPaths(text string, byPath map[string]sessionlog.AssetEvent) string {
	if !mdLinkRE.MatchString(text) {
		return text
	}
	lines := strings.Split(text, "\n")
	inFence := false
	for i, ln := range lines {
		if fenceRE.MatchString(ln) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		lines[i] = mdLinkRE.ReplaceAllStringFunc(ln, func(m string) string {
			sub := mdLinkRE.FindStringSubmatch(m)
			bang, ref := sub[1], sub[3]
			if !isLocalPathRef(ref) {
				return m
			}
			ev, ok := byPath[ref]
			if !ok {
				return m
			}
			name := ev.Name
			if name == "" {
				name = "asset"
			}
			return fmt.Sprintf("%s[%s](scimux-asset:%s)", bang, name, ev.ID)
		})
	}
	return strings.Join(lines, "\n")
}
