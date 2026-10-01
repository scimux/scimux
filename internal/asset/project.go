package asset

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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

// ProjectAgentPathBindings projects occurrence-specific retry successes and
// persisted failures in one pass over the original turn. Combining them keeps
// occurrence numbering stable when an earlier reference has already become a
// successful asset. Ordinary links, citations, and fenced examples remain
// unchanged.
func ProjectAgentPathBindings(text string, bound map[int]sessionlog.AssetEvent, imports []sessionlog.AssetImportEvent) string {
	if len(bound) == 0 && len(imports) == 0 || !mdLinkRE.MatchString(text) {
		return text
	}
	byOccurrence := map[int]sessionlog.AssetImportEvent{}
	for _, ref := range imports {
		byOccurrence[ref.Occurrence] = ref
	}
	lines := strings.Split(text, "\n")
	inFence := false
	occurrence := 0
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
			if !isLocalPathRef(sub[3]) {
				return m
			}
			current := occurrence
			occurrence++
			if ev, ok := bound[current]; ok && ev.SourcePath == sub[3] {
				name := ev.Name
				if name == "" {
					name = "asset"
				}
				return fmt.Sprintf("%s[%s](scimux-asset:%s)", sub[1], name, ev.ID)
			}
			ref, ok := byOccurrence[current]
			if !ok || ref.Ref != sub[3] {
				return m
			}
			// Keep the scimux-import marker. Restoring the original path would
			// let ProjectAgentPaths bind this occurrence to an older asset
			// recorded at the same path.
			label := sub[2]
			if ref.Reason == "not_found" {
				// Encode delimiter characters before putting the filename in
				// Markdown marker syntax; the browser decodes this label.
				label = url.PathEscape(missingRefLabel(sub[2], ref.Ref))
			}
			return fmt.Sprintf("%s[%s](scimux-import:%d:%d:%s)", sub[1], label, ref.TurnRecord, ref.Occurrence, ref.Reason)
		})
	}
	return strings.Join(lines, "\n")
}

// ProjectVisibleBlocked projects occurrence bindings and blocked imports.
// A blocked reference whose source is currently absent is labeled not_found
// in the returned text only; imports is not modified. The marker stays a
// scimux-import reference so a later path projection cannot attach an older
// asset that used the same path.
func ProjectVisibleBlocked(text string, bound map[int]sessionlog.AssetEvent, imports []sessionlog.AssetImportEvent, dir string) string {
	if len(imports) == 0 {
		return ProjectAgentPathBindings(text, bound, nil)
	}
	adjusted := make([]sessionlog.AssetImportEvent, len(imports))
	copy(adjusted, imports)
	for i := range adjusted {
		switch currentPathState(adjusted[i].Ref, dir) {
		case pathAbsent:
			adjusted[i].Reason = "not_found"
		case pathRegular:
			if adjusted[i].Reason == "not_found" {
				adjusted[i].Reason = "retry_ready"
			}
		}
	}
	return ProjectAgentPathBindings(text, bound, adjusted)
}

type pathState uint8

const (
	pathUnknown pathState = iota
	pathAbsent
	pathRegular
)

// PathAbsent reports whether ref currently names no filesystem object.
// Relative refs resolve against dir. A dangling symlink is absent. Any other
// error, including a permission failure, is not absence.
func PathAbsent(ref, dir string) bool {
	return currentPathState(ref, dir) == pathAbsent
}

func currentPathState(ref, dir string) pathState {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return pathUnknown
	}
	p := ref
	if !filepath.IsAbs(p) {
		if strings.TrimSpace(dir) == "" {
			return pathUnknown
		}
		p = filepath.Join(dir, p)
	}
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return pathUnknown
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pathAbsent
		}
		return pathUnknown
	}
	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(abs)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return pathAbsent
			}
			return pathUnknown
		}
	}
	if info.Mode().IsRegular() {
		return pathRegular
	}
	return pathUnknown
}

func missingRefLabel(alt, ref string) string {
	base := filepath.Base(strings.TrimSpace(ref))
	switch base {
	case "", ".", "..", string(filepath.Separator):
		if strings.TrimSpace(alt) != "" {
			return alt
		}
		return "file"
	default:
		return base
	}
}

func ProjectBlockedAgentPaths(text string, imports []sessionlog.AssetImportEvent) string {
	return ProjectAgentPathBindings(text, nil, imports)
}

var projectedImportRE = regexp.MustCompile(`!?\[([^\]]*)\]\(scimux-import:[0-9]+:[0-9]+:([a-z_]+)\)`)

// PlainBlockedRefs removes projection-only import markers from text that will
// be frozen in a reference-media capture. The session log's stored reason is
// used for that capture; current filesystem state must not alter its text.
func PlainBlockedRefs(text string) string {
	return projectedImportRE.ReplaceAllStringFunc(text, func(marker string) string {
		match := projectedImportRE.FindStringSubmatch(marker)
		label := match[1]
		if match[2] == "not_found" {
			if decoded, err := url.PathUnescape(label); err == nil {
				label = decoded
			}
		}
		if label == "" {
			return "file"
		}
		return label
	})
}
