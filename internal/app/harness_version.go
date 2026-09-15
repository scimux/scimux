// harness_version.go — which agent harnesses this computer has, at which
// version, and whether upstream has a newer one. Feeds the burger menu's
// harness section via GET /api/harnesses (local, no network) and
// GET /api/harnesses/latest (network, explicit tap only).
//
// The inventory answers a question the new-activity dialog cannot: that
// dialog offers models, so a harness appears there or it does not, and
// "installed but two months old" looks exactly like "current". Version drift
// is what explains an agent that suddenly behaves differently, so it is worth
// stating outright.
package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// harnessRow is one supported harness as this computer has it. Present and
// Launchable are separate facts: pi is installed as `pi` but launches through
// `pi-acp`, so it can be present and still not offerable (the same
// distinction probeAgents makes for the dialog).
type harnessRow struct {
	Agent      string `json:"agent"`
	Present    bool   `json:"present"`
	Launchable bool   `json:"launchable"`
	Installed  string `json:"installed,omitempty"`
	Path       string `json:"path,omitempty"`
	// Latest is never filled by the local inventory. It exists on the row so
	// the browser can merge an upstream answer into one shape.
	Latest string `json:"latest,omitempty"`
}

// harnessSource is where a harness publishes its current version. There is no
// single answer — four are npm packages, Grok is a plain-text file in a
// bucket, Muse is channel metadata, and Claude's depends on how it was
// installed — so the source is data, not a hardcoded lane. Kind is "npm"
// (JSON, read .version), "text" (the bare version, one line), or
// "muse-stable". Label is shown to the user: an update notice is only
// actionable if you know which channel it came from.
//
// Every source here is an unauthenticated public endpoint and must stay one.
// No vendor credential is ever attached to these requests, and none is read
// to build them — the harness panel learns a version the same way anyone with
// curl would. An "authenticated check for better rate limits" would cross the
// line the whole integration rests on: scimux drives the harness, it never
// reaches a vendor's service on the user's behalf.
type harnessSource struct {
	URL   string `json:"url"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

const (
	npmRegistry = "https://registry.npmjs.org/"
	// The native installer's own channel file. It trails the npm dist-tag on
	// purpose, and a native install can only ever receive what it names.
	claudeNativeStable = "https://downloads.claude.ai/claude-code-releases/stable"
	grokStable         = "https://storage.googleapis.com/grok-build-public-artifacts/cli/stable"
	// Unauthenticated public channel metadata. Contacted only from the
	// explicit harness-update-check route. The response shape is not
	// documented in this repository, so parsing fails closed.
	museStableChannel = "https://api.meta.ai/muse-code/channels/muse-stable"
)

// claudeHarnessSource picks Claude's channel from where its binary actually
// lives. The native installer keeps versions under <data>/claude/versions/,
// tracks `stable`, and cannot install the npm dist-tag; comparing a native
// install against npm reported "2.1.266 available" for a computer whose
// installer would keep handing it 2.1.236 (observed 2026-09-09). An
// unresolvable path assumes npm, which is the published default.
func claudeHarnessSource(binPath string) harnessSource {
	if strings.Contains(filepath.ToSlash(binPath), "/claude/versions/") {
		return harnessSource{URL: claudeNativeStable, Kind: "text", Label: "native installer (stable)"}
	}
	return harnessSource{URL: npmRegistry + "@anthropic-ai/claude-code/latest", Kind: "npm", Label: "npm @anthropic-ai/claude-code"}
}

// harnessSources maps each supported harness to its upstream. Claude's entry
// is resolved per call because it depends on this computer's install.
func harnessSources() map[string]harnessSource {
	claudePath, _ := exec.LookPath("claude")
	if resolved, err := filepath.EvalSymlinks(claudePath); err == nil {
		claudePath = resolved
	}
	return map[string]harnessSource{
		"claude":   claudeHarnessSource(claudePath),
		"codex":    {URL: npmRegistry + "@openai/codex/latest", Kind: "npm", Label: "npm @openai/codex"},
		"pi":       {URL: npmRegistry + "@earendil-works/pi-coding-agent/latest", Kind: "npm", Label: "npm @earendil-works/pi-coding-agent"},
		"opencode": {URL: npmRegistry + "opencode-ai/latest", Kind: "npm", Label: "npm opencode-ai"},
		"grok":     {URL: grokStable, Kind: "text", Label: "xAI stable channel"},
		"muse":     {URL: museStableChannel, Kind: "muse-stable", Label: "Meta stable channel"},
	}
}

// activeHarnessSources is the indirection tests replace; production reads
// harnessSources() so Claude's channel is re-resolved on every check.
var activeHarnessSources = harnessSources

// setHarnessSourcesForTest pins the upstream map and returns its restore func
// (register with t.Cleanup or defer). For tests only — the suite must never
// reach a real registry.
func setHarnessSourcesForTest(src map[string]harnessSource) (restore func()) {
	prev := activeHarnessSources
	activeHarnessSources = func() map[string]harnessSource { return src }
	return func() { activeHarnessSources = prev }
}

// versionPattern matches a dotted numeric version with an optional leading v
// and an optional prerelease/build suffix. Two segments is the floor on
// purpose: a bare build hash like "1a29d5bc12" carries no dot and must not be
// read as a version.
var versionPattern = regexp.MustCompile(`\bv?(\d+\.\d+(?:\.\d+)*(?:[-+][0-9A-Za-z.\-]+)?)`)

// parseHarnessVersion takes the first version-shaped token in a `--version`
// output. The six CLIs print several different shapes ("2.1.236 (Claude
// Code)", "codex-cli 0.147.0", "0.84.3", "grok 1.0.3 (…) [stable]", "Muse
// Code 1.3.0 (1.3.0-R3057.1)"), and they are free to change them; an
// unrecognised output yields "" and the row simply shows no version.
func parseHarnessVersion(out string) string {
	m := versionPattern.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}

// harnessNewer reports whether latest is a higher version than installed,
// comparing numeric segments as numbers. Lexical comparison would have said
// grok 1.0.3 was current against 1.0.24. Anything unparseable, equal, or
// lower answers false: an update notice is a claim, and we only make it on
// evidence. Prerelease suffixes are ignored — the numeric prefix is what we
// can compare honestly.
func harnessNewer(installed, latest string) bool {
	a, b := versionSegments(installed), versionSegments(latest)
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return y > x
		}
	}
	return false
}

func versionSegments(v string) []int {
	v = parseHarnessVersion(v)
	if v == "" {
		return nil
	}
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out []int
	for _, seg := range strings.Split(v, ".") {
		n, err := strconv.Atoi(seg)
		if err != nil {
			return out
		}
		out = append(out, n)
	}
	return out
}

// probeHarnessVersions runs `<bin> --version` for every supported harness, in
// the order the registry declares them. Absent harnesses are reported too:
// a row that simply is not there cannot be told apart from a probe that
// failed, and the user asked what scimux found — including what it did not.
func probeHarnessVersions(hs []harness) []harnessRow {
	rows := make([]harnessRow, 0, len(hs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	out := make([]harnessRow, len(hs))
	for i, h := range hs {
		wg.Add(1)
		go func(i int, h harness) {
			defer wg.Done()
			row := harnessRow{Agent: h.bin}
			require := h.require
			if require == "" {
				require = h.bin
			}
			if _, err := exec.LookPath(require); err == nil {
				row.Launchable = true
			}
			bin, err := exec.LookPath(h.bin)
			if err != nil {
				out[i] = row
				return
			}
			row.Present = true
			if resolved, err := filepath.EvalSymlinks(bin); err == nil {
				row.Path = resolved
			} else {
				row.Path = bin
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// CombinedOutput: some CLIs print their version on stderr, and a
			// non-zero exit is not a reason to drop a version we can read.
			b, _ := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
			row.Installed = parseHarnessVersion(string(b))
			out[i] = row
		}(i, h)
	}
	wg.Wait()
	mu.Lock()
	rows = append(rows, out...)
	mu.Unlock()
	return rows
}

var harnessInventoryOnce sync.Once
var harnessInventoryCache []harnessRow

// harnessInventory probes once per process, like detectAgents: five
// subprocesses is not something to repeat on every menu open, and a harness
// installed while scimux runs appears after a restart.
func harnessInventory() []harnessRow {
	harnessInventoryOnce.Do(func() {
		harnessInventoryCache = probeHarnessVersions(harnesses)
	})
	return harnessInventoryCache
}

// fetchHarnessLatest reads one upstream source. Bounded body, short timeout,
// and a version-shaped answer or an error — an HTML error page must not be
// reported as a release.
func fetchHarnessLatest(ctx context.Context, src harnessSource) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json, text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errHarnessSource("status " + strconv.Itoa(resp.StatusCode))
	}
	raw := string(body)
	if src.Kind == "npm" {
		var pkg struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(body, &pkg); err != nil {
			return "", errHarnessSource("unreadable registry response")
		}
		raw = pkg.Version
	}
	if src.Kind == "muse-stable" {
		// No checked-in textual findings document this channel's JSON.
		// Do not invent a schema; fail closed after a bounded unauthenticated GET.
		return "", errHarnessSource("unresolved muse-stable metadata shape")
	}
	v := parseHarnessVersion(raw)
	if v == "" {
		return "", errHarnessSource("no version in response")
	}
	return v, nil
}

type errHarnessSource string

func (e errHarnessSource) Error() string { return "harness source: " + string(e) }

// handleHarnesses serves the local inventory. No network: the menu opens on
// every tap and the answer is about this computer.
func (a *app) handleHarnesses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"harnesses": harnessInventory()})
}

// handleHarnessLatest asks each upstream what it publishes — only ever on an
// explicit tap, like the scimux update check. Sources are read concurrently
// and independently: one dead registry costs its own row, not the check.
func (a *app) handleHarnessLatest(w http.ResponseWriter, r *http.Request) {
	// Tapping "check for updates" is a user saying the installed harnesses may
	// have moved. The model list is keyed on claude's version, so this is the
	// one tap that most deserves to invalidate it.
	a.ensureClaudeModels()
	type answer struct {
		Version string `json:"version"`
		Source  string `json:"source"`
	}
	src := activeHarnessSources()
	out := map[string]answer{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for agent, s := range src {
		wg.Add(1)
		go func(agent string, s harnessSource) {
			defer wg.Done()
			v, err := fetchHarnessLatest(r.Context(), s)
			if err != nil {
				return // absent, not zero: "unknown" is what happened
			}
			mu.Lock()
			out[agent] = answer{Version: v, Source: s.Label}
			mu.Unlock()
		}(agent, s)
	}
	wg.Wait()
	writeJSON(w, map[string]any{"latest": out})
}
