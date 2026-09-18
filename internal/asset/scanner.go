package asset

import (
	"regexp"
	"strings"

	"github.com/scimux/scimux/internal/sessionlog"
)

// Candidate is a local-path reference found in a turn, before eligibility
// (Resolve) or storage-mode (StorageMode) decisions are applied. Alt/IsImage
// are only known for Markdown-sourced candidates; tool-sourced candidates
// carry just the path.
type Candidate struct {
	Ref     string
	Alt     string
	IsImage bool
}

var mdLinkRE = regexp.MustCompile(`(!?)\[([^\]]*)\]\(([^)\s]+)\)`)
var fenceRE = regexp.MustCompile("^\\s*```")

// urlSchemeRE matches a leading URL scheme ("http:", "mailto:", "data:", and
// our own "scimux-asset:" among them). A ref carrying a scheme is a URL, not a
// local filesystem path, and is never an ingestion candidate.
var urlSchemeRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// isLocalPathRef reports whether a Markdown link/image target is a candidate
// local filesystem path — as opposed to a URL (http, mailto, data, the
// scimux-asset: refs we mint ourselves, …) or an in-document fragment
// (#heading). Only genuine paths are eligible for ingestion and render-time
// projection; everything else is left exactly as the agent wrote it. This is
// the single filter shared by ScanMarkdown (ingestion) and ProjectAgentPaths
// (render) so the two can never drift — without it, ordinary agent prose with
// anchor or reference links renders as inert "unavailable" file chips.
func isLocalPathRef(ref string) bool {
	if ref == "" || strings.HasPrefix(ref, "#") {
		return false
	}
	return !urlSchemeRE.MatchString(ref)
}

// ScanMarkdown extracts local-path candidates from Markdown image/link
// syntax in turn text — the doc's `![alt](path)` / `[text](path)` shapes.
// Fenced code blocks are skipped so example syntax quoted in prose is never
// mistaken for a real reference. Targets that are not local paths — URLs
// (http, mailto, data, already-projected scimux-asset:) and in-document
// fragments (#heading) — are skipped via isLocalPathRef.
func ScanMarkdown(text string) []Candidate {
	stripped := stripFencedCode(text)
	var out []Candidate
	for _, m := range mdLinkRE.FindAllStringSubmatch(stripped, -1) {
		ref := m[3]
		if !isLocalPathRef(ref) {
			continue
		}
		out = append(out, Candidate{Ref: ref, Alt: m[2], IsImage: m[1] == "!"})
	}
	return out
}

func stripFencedCode(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	for _, ln := range lines {
		if fenceRE.MatchString(ln) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// writeShapedKinds are the ACP ToolCall kinds that can produce a new file on
// disk. "read"/"search"/"execute"/"fetch"/"other" are excluded as too
// ambiguous or not artifact-producing; "delete" removes rather than creates.
var writeShapedKinds = map[string]bool{"edit": true, "move": true}

// pathKeys are the argument names observed (or plausible) across agent-CLI
// write/edit tool calls. Defensive by design: an unrecognized shape is
// silently skipped, never an error, matching the transcript parser's
// contract for undocumented CLI internals.
var pathKeys = []string{"path", "file_path", "filePath", "output_path", "outputPath"}

// ScanToolOutput extracts local-path candidates from a tool call's rawInput.
// Only write-shaped kinds are inspected. rawInput is untyped JSON (map,
// slice, scalar, or nil) — any shape other than a map, or a nested "edits"
// array of maps, is ignored rather than treated as an error.
func ScanToolOutput(tool *sessionlog.ToolEvent) []Candidate {
	if tool == nil || !writeShapedKinds[tool.Kind] {
		return nil
	}
	m, ok := tool.RawInput.(map[string]any)
	if !ok {
		return nil
	}
	var out []Candidate
	for _, key := range pathKeys {
		if s, ok := m[key].(string); ok && s != "" {
			out = append(out, Candidate{Ref: s})
		}
	}
	if edits, ok := m["edits"].([]any); ok {
		for _, e := range edits {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			for _, key := range pathKeys {
				if s, ok := em[key].(string); ok && s != "" {
					out = append(out, Candidate{Ref: s})
				}
			}
		}
	}
	return out
}

// Candidates merges the Markdown-text and tool-call scan sources for one
// turn, deduplicating by Ref (first occurrence wins, so a Markdown match's
// Alt/IsImage metadata takes priority over a tool-sourced duplicate).
func Candidates(text string, tools []*sessionlog.ToolEvent) []Candidate {
	seen := make(map[string]bool)
	var out []Candidate
	add := func(c Candidate) {
		if seen[c.Ref] {
			return
		}
		seen[c.Ref] = true
		out = append(out, c)
	}
	for _, c := range ScanMarkdown(text) {
		add(c)
	}
	for _, t := range tools {
		for _, c := range ScanToolOutput(t) {
			add(c)
		}
	}
	return out
}
