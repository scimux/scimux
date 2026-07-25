package asset

import (
	"fmt"
	"regexp"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// attachRefRE matches the plain-text attachment marker extendPrompt appends
// to a delivered user turn ("[attached image: /abs/path]" / "[attached
// file: /abs/path]") — the literal text mirrored/recorded turns carry (see
// extendPrompt in main.go), not Markdown, so it needs its own pattern
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
