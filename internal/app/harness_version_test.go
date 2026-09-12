package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The `--version` strings below are the real output shapes of the five
// harnesses, captured 2026-09-09 by hand. Only the strings are real: no test
// in this package may invoke an agent CLI, so every probe here runs against a
// shell stub on a private PATH.
func TestParseHarnessVersion(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"claude", "2.1.236 (Claude Code)\n", "2.1.236"},
		{"codex", "codex-cli 0.147.0\n", "0.147.0"},
		{"pi", "0.84.3\n", "0.84.3"},
		{"opencode", "1.18.23\n", "1.18.23"},
		{"grok", "grok 1.0.3 (1a29d5bc12) [stable]\n", "1.0.3"},
		{"prerelease suffix", "codex-cli 0.148.0-alpha.2\n", "0.148.0-alpha.2"},
		{"leading v", "grok v1.2.0\n", "1.2.0"},
		{"two segments", "opencode 1.18\n", "1.18"},
		{"noise before version", "warning: config unreadable\n2.1.236 (Claude Code)\n", "2.1.236"},
		{"no version at all", "command not found\n", ""},
		{"empty", "", ""},
		// A bare build hash must not be mistaken for a version.
		{"hash only", "1a29d5bc12\n", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseHarnessVersion(tt.out); got != tt.want {
				t.Fatalf("parseHarnessVersion(%q) = %q, want %q", tt.out, got, tt.want)
			}
		})
	}
}

// Segment-wise numeric comparison, not string comparison: grok went 1.0.3 →
// 1.0.24, where "3" sorts after "24" lexically. Getting this wrong would tell
// a user 21 releases behind that they are up to date.
func TestHarnessNewer(t *testing.T) {
	cases := []struct {
		installed, latest string
		want              bool
	}{
		{"1.0.3", "1.0.24", true},
		{"0.147.0", "0.153.4", true},
		{"2.1.236", "2.1.236", false},
		{"2.1.266", "2.1.236", false}, // native stable channel behind npm: not an update
		{"1.18", "1.18.30", true},
		{"1.18.30", "1.18", false},
		{"0.84.3", "0.85.1", true},
		{"1.0.0-beta.1", "1.0.0", false}, // equal numeric prefix: no claim
		{"", "1.0.0", false},             // unknown installed version: never claim
		{"1.0.0", "", false},
		{"garbage", "also garbage", false},
	}
	for _, tt := range cases {
		if got := harnessNewer(tt.installed, tt.latest); got != tt.want {
			t.Errorf("harnessNewer(%q, %q) = %v, want %v", tt.installed, tt.latest, got, tt.want)
		}
	}
}

func TestProbeHarnessInventory(t *testing.T) {
	binDir := t.TempDir()
	writeScript(t, binDir, "claude", `printf '%s\n' '2.1.236 (Claude Code)'`)
	writeScript(t, binDir, "codex", `printf '%s\n' 'codex-cli 0.147.0'`)
	writeScript(t, binDir, "pi", `printf '%s\n' '0.84.3'`)
	// pi-acp deliberately absent: pi is installed but not launchable.
	writeScript(t, binDir, "grok", `printf '%s\n' 'grok 1.0.3 (1a29d5bc12) [stable]'`)
	// opencode absent entirely.
	t.Setenv("PATH", binDir)

	inv := probeHarnessVersions(harnesses)

	// Every supported harness is reported, installed or not: a silently
	// missing row cannot be told apart from a broken probe.
	if len(inv) != len(harnesses) {
		t.Fatalf("inventory has %d rows, want %d (one per supported harness)", len(inv), len(harnesses))
	}
	byAgent := map[string]harnessRow{}
	for _, row := range inv {
		byAgent[row.Agent] = row
	}
	if got := byAgent["claude"]; got.Installed != "2.1.236" || !got.Present || !got.Launchable {
		t.Errorf("claude = %+v, want 2.1.236 present and launchable", got)
	}
	if got := byAgent["codex"]; got.Installed != "0.147.0" {
		t.Errorf("codex = %+v, want 0.147.0", got)
	}
	if got := byAgent["grok"]; got.Installed != "1.0.3" {
		t.Errorf("grok = %+v, want 1.0.3", got)
	}
	// pi lists models via `pi` but launches via `pi-acp`, so an installed pi
	// with no pi-acp is present-but-not-launchable — the same distinction the
	// new-activity dialog makes.
	if got := byAgent["pi"]; got.Installed != "0.84.3" || !got.Present || got.Launchable {
		t.Errorf("pi = %+v, want 0.84.3 present but not launchable", got)
	}
	if got := byAgent["opencode"]; got.Present || got.Installed != "" {
		t.Errorf("opencode = %+v, want absent with no version", got)
	}
}

func TestProbeHarnessVersionsSurvivesABrokenCLI(t *testing.T) {
	binDir := t.TempDir()
	writeScript(t, binDir, "claude", `echo boom >&2; exit 3`)
	writeScript(t, binDir, "codex", `printf '%s\n' 'not a version'`)
	t.Setenv("PATH", binDir)

	inv := probeHarnessVersions(harnesses)
	byAgent := map[string]harnessRow{}
	for _, row := range inv {
		byAgent[row.Agent] = row
	}
	// Present, but with no version to show. An installed harness must never
	// vanish because `--version` broke.
	if got := byAgent["claude"]; !got.Present || got.Installed != "" {
		t.Errorf("claude = %+v, want present with an empty version", got)
	}
	if got := byAgent["codex"]; !got.Present || got.Installed != "" {
		t.Errorf("codex = %+v, want present with an empty version", got)
	}
}

func TestFetchHarnessLatest(t *testing.T) {
	npm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"@openai/codex","version":"0.153.4","dist":{}}`))
	}))
	defer npm.Close()
	text := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("1.0.24\n"))
	}))
	defer text.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", 500)
	}))
	defer broken.Close()
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>an error page</html>"))
	}))
	defer junk.Close()

	if got, err := fetchHarnessLatest(context.Background(), harnessSource{URL: npm.URL, Kind: "npm"}); err != nil || got != "0.153.4" {
		t.Errorf("npm source = %q, %v; want 0.153.4", got, err)
	}
	if got, err := fetchHarnessLatest(context.Background(), harnessSource{URL: text.URL, Kind: "text"}); err != nil || got != "1.0.24" {
		t.Errorf("text source = %q, %v; want 1.0.24", got, err)
	}
	if _, err := fetchHarnessLatest(context.Background(), harnessSource{URL: broken.URL, Kind: "text"}); err == nil {
		t.Error("500 response: want an error")
	}
	if _, err := fetchHarnessLatest(context.Background(), harnessSource{URL: junk.URL, Kind: "text"}); err == nil {
		t.Error("unparseable body: want an error")
	}
	museOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("muse-stable request carried identity headers: %v", r.Header)
		}
		_, _ = w.Write([]byte(`{"something":"undocumented"}`))
	}))
	defer museOK.Close()
	if _, err := fetchHarnessLatest(context.Background(), harnessSource{URL: museOK.URL, Kind: "muse-stable"}); err == nil {
		t.Error("undocumented muse-stable metadata must fail closed")
	}
	muse500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", 500)
	}))
	defer muse500.Close()
	if _, err := fetchHarnessLatest(context.Background(), harnessSource{URL: muse500.URL, Kind: "muse-stable"}); err == nil {
		t.Error("non-2xx muse-stable must fail closed")
	}
	museHuge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 2<<20))
	}))
	defer museHuge.Close()
	if _, err := fetchHarnessLatest(context.Background(), harnessSource{URL: museHuge.URL, Kind: "muse-stable"}); err == nil {
		t.Error("oversized muse-stable body must fail closed")
	}
}

func TestHarnessSourcesIncludesMuseStableChannel(t *testing.T) {
	src := harnessSources()["muse"]
	if src.URL != museStableChannel || src.Kind != "muse-stable" {
		t.Fatalf("muse source = %+v", src)
	}
	if src.URL != "https://api.meta.ai/muse-code/channels/muse-stable" {
		t.Fatalf("muse stable URL drifted: %s", src.URL)
	}
}

// The native installer tracks its own `stable` channel, which trails the npm
// dist-tag on purpose (2.1.236 vs 2.1.266 on 2026-09-09). Comparing a native
// install against npm would advertise an update the installer will not hand
// over, so the source follows the install channel.
func TestClaudeSourceFollowsInstallChannel(t *testing.T) {
	native := claudeHarnessSource("/home/someone/.local/share/claude/versions/2.1.236")
	if !strings.Contains(native.URL, "downloads.claude.ai") || native.Kind != "text" {
		t.Errorf("native install source = %+v, want the downloads.claude.ai channel", native)
	}
	if !strings.Contains(native.Label, "native") {
		t.Errorf("native label = %q, want it to name the channel", native.Label)
	}
	npm := claudeHarnessSource("/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js")
	if !strings.Contains(npm.URL, "registry.npmjs.org") || npm.Kind != "npm" {
		t.Errorf("npm install source = %+v, want the npm registry", npm)
	}
	// An unresolvable path is not evidence of either channel; npm is the
	// published default, so that is the safe assumption.
	if unknown := claudeHarnessSource(""); unknown.Kind != "npm" {
		t.Errorf("unknown path source = %+v, want the npm default", unknown)
	}
}

func TestHandleHarnessesIsLocalOnly(t *testing.T) {
	binDir := t.TempDir()
	writeScript(t, binDir, "claude", `printf '%s\n' '2.1.236 (Claude Code)'`)
	t.Setenv("PATH", binDir)
	// Any network read here would be a bug: the inventory is what this
	// computer has, and opening the menu must not phone home.
	restore := setHarnessSourcesForTest(map[string]harnessSource{
		"claude": {URL: "http://127.0.0.1:0/must-not-be-called", Kind: "text"},
	})
	defer restore()

	a := newTestApp(t, &fakeTmux{})
	rec := httptest.NewRecorder()
	a.handleHarnesses(rec, httptest.NewRequest(http.MethodGet, "/api/harnesses", nil))

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Harnesses []harnessRow `json:"harnesses"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Harnesses) != len(harnesses) {
		t.Fatalf("harnesses = %d rows, want %d", len(got.Harnesses), len(harnesses))
	}
	for _, row := range got.Harnesses {
		if row.Latest != "" {
			t.Errorf("%s carries a latest version %q; the local inventory must not check upstream", row.Agent, row.Latest)
		}
	}
}

func TestHandleHarnessLatestDegradesPerAgent(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"0.153.4"}`))
	}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", 500)
	}))
	defer bad.Close()
	restore := setHarnessSourcesForTest(map[string]harnessSource{
		"codex":  {URL: ok.URL, Kind: "npm", Label: "npm @openai/codex"},
		"claude": {URL: bad.URL, Kind: "text", Label: "native stable"},
	})
	defer restore()

	a := newTestApp(t, &fakeTmux{})
	rec := httptest.NewRecorder()
	a.handleHarnessLatest(rec, httptest.NewRequest(http.MethodGet, "/api/harnesses/latest", nil))

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 — one dead source is not a failed check", rec.Code)
	}
	var got struct {
		Latest map[string]struct {
			Version string `json:"version"`
			Source  string `json:"source"`
		} `json:"latest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v := got.Latest["codex"]; v.Version != "0.153.4" || v.Source != "npm @openai/codex" {
		t.Errorf("codex = %+v, want 0.153.4 from the labelled source", v)
	}
	// A source that failed is absent, never a zero version: "" would render
	// as an answer, and "unknown" is what actually happened.
	if _, present := got.Latest["claude"]; present {
		t.Errorf("claude present = %+v, want it omitted after a failed fetch", got.Latest["claude"])
	}
}

func TestHarnessSourcesCoverEverySupportedHarness(t *testing.T) {
	// A harness with no upstream source would show a permanent blank next to
	// its version with nothing to explain it.
	src := harnessSources()
	for _, h := range harnesses {
		s, ok := src[h.bin]
		if !ok {
			t.Errorf("harness %q has no upstream source", h.bin)
			continue
		}
		if s.URL == "" || s.Kind == "" || s.Label == "" {
			t.Errorf("harness %q source = %+v, want url, kind and label", h.bin, s)
		}
		if s.Kind != "npm" && s.Kind != "text" && s.Kind != "muse-stable" {
			t.Errorf("harness %q source kind = %q, want npm, text, or muse-stable", h.bin, s.Kind)
		}
	}
}

// writeScript's stubs are the only agent binaries this package may run; this
// guards the harness probe against ever reaching a real CLI on the host.
func TestProbeHarnessVersionsRunsNoRealCLI(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	for _, row := range probeHarnessVersions(harnesses) {
		if row.Present || row.Installed != "" {
			t.Errorf("row %+v on an empty PATH; the probe found a binary outside PATH", row)
		}
	}
	if _, err := os.Stat(filepath.Join(empty, "claude")); !os.IsNotExist(err) {
		t.Fatal("test PATH is not empty")
	}
}

func TestMuseHarnessPresentButPolicyClosedIsNotLaunchable(t *testing.T) {
	binDir := t.TempDir()
	writeScript(t, binDir, "muse", `printf '%s\n' 'muse 1.2.3'`)
	t.Setenv("PATH", binDir)
	rows := probeHarnessVersions([]harness{{bin: "muse"}})
	if len(rows) != 1 || !rows[0].Present {
		t.Fatalf("Muse inventory = %+v, want one present row", rows)
	}
	if rows[0].Launchable {
		t.Fatalf("production-policy-closed Muse was advertised launchable: %+v", rows[0])
	}
}

func TestMuseStableChannelIsExplicitCheckOnly(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Errorf("muse update request carried credentials: %v", r.Header)
		}
		if r.URL.Query().Get("provider") != "" {
			t.Error("muse update request carried provider information")
		}
		_, _ = w.Write([]byte(`{"undocumented":true}`))
	}))
	defer srv.Close()
	restore := setHarnessSourcesForTest(map[string]harnessSource{
		"muse": {URL: srv.URL, Kind: "muse-stable", Label: "Meta stable channel"},
	})
	defer restore()

	a := newTestApp(t, &fakeTmux{})
	a.agentCatalog = func() map[string]agentInfo { return map[string]agentInfo{} }
	a.handleHarnesses(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/harnesses", nil))
	a.handleAgents(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	a.poll()
	if hits.Load() != 0 {
		t.Fatalf("stable channel contacted %d times outside an explicit check", hits.Load())
	}

	rec := httptest.NewRecorder()
	a.handleHarnessLatest(rec, httptest.NewRequest(http.MethodGet, "/api/harnesses/latest", nil))
	if rec.Code != 200 {
		t.Fatalf("explicit check status = %d", rec.Code)
	}
	if hits.Load() != 1 {
		t.Fatalf("explicit check hits = %d, want 1", hits.Load())
	}
	var got struct {
		Latest map[string]struct {
			Version string `json:"version"`
		} `json:"latest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Latest["muse"]; ok {
		t.Fatalf("unresolved muse-stable shape leaked a version: %+v", got.Latest["muse"])
	}
}
