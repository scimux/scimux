package app

// Packet 2C characterization: pure node-lifecycle matrix covering the gaps
// left by the existing lifecycle, API, and agents_test.go coverage.
// No production code is moved or changed here; Packet 2D extracts
// node_lifecycle.go against this matrix.
//
// Intentionally not re-covered (already characterized elsewhere):
// - newUUID / randSource failure (TestNewUUID, TestNewUUIDRNGFailure)
// - sessionArgFromCmdline (TestSessionArgFromCmdline)
// - Claude model cache / probe (agents_test.go)
// - handler-level create validation, wrapper use, launch failure surfacing
//   (node_api_test.go)
// - session-log dead-slug blocking and post-archive reuse
//   (TestArchiveSessionLogNamingAndSlugReuse in store_test.go — Packet 2A)
// - protocol-specific ACP/Codex argv (internal/acp, internal/codex)

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"

	"codeberg.org/chrberger/scimux/internal/tmuxsession"
)

// ---------- 1. ID allocation matrix ----------

func TestUniqueIDNormalizationMatrix(t *testing.T) {
	a := &app{byID: map[string]*Node{}}
	// Exact normalization: punctuation, whitespace, repeated separators, dots,
	// Unicode, empty/all-invalid titles. Underscores are allowed and preserved.
	cases := map[string]string{
		"a   b":                  "a-b",
		"a--b":                   "a-b",
		"  hello  world  ":       "hello-world",
		"foo/bar:baz":            "foo-bar-baz",
		"foo___bar":              "foo___bar",
		"foo...bar":              "foo-bar",
		"Review notes-design.md": "Review-notes-design-md",
		"v2.0 baseline":          "v2-0-baseline",
		"häßliche Umlaute":       "h-liche-Umlaute",
		"ünïcode 🚗 notes":        "n-code-notes",
		"":                       "chat",
		"!!!":                    "chat",
		"---":                    "chat",
		"  .  ":                  "chat",
		"--leading-dashes":       "leading-dashes",
		"trailing-dashes--":      "trailing-dashes",
		"mix___and---seps":       "mix___and-seps",
	}
	for title, want := range cases {
		got := a.uniqueID(title, nil)
		if got != want {
			t.Errorf("uniqueID(%q) = %q, want %q", title, got, want)
		}
		if !tmuxsession.ValidName(got) {
			t.Errorf("uniqueID(%q) = %q is not tmux-safe", title, got)
		}
	}
}

func TestUniqueIDTruncationBoundary(t *testing.T) {
	a := &app{byID: map[string]*Node{}}
	exact40 := strings.Repeat("x", 40)
	if got := a.uniqueID(exact40, nil); got != exact40 {
		t.Errorf("exact-40 base = %q (len %d), want unchanged", got, len(got))
	}
	over := strings.Repeat("x", 41)
	if got := a.uniqueID(over, nil); got != exact40 {
		t.Errorf("41-char base = %q (len %d), want first 40 x's", got, len(got))
	}
	// Boundary is applied to the slug, not the title: punctuation-expanded
	// titles still truncate at 40 after normalization.
	// "a.b" -> "a-b"; build a long dotted title whose slug exceeds 40.
	longDots := strings.Repeat("ab.", 20) // "ab.ab...." -> slug "ab-ab-..." length > 40
	got := a.uniqueID(longDots, nil)
	if len(got) != 40 {
		t.Errorf("normalized long title id len = %d, want exactly 40; id=%q", len(got), got)
	}
	if !tmuxsession.ValidName(got) {
		t.Errorf("truncated id %q is not tmux-safe", got)
	}
}

// Current behavior: the 40-character limit applies only to the base slug.
// A collision suffix is appended after truncation, so the returned id can
// exceed 40 characters. Characterize rather than "fix" this.
func TestUniqueIDCollisionSuffixAfter40CharBase(t *testing.T) {
	base := strings.Repeat("y", 40)
	a := &app{byID: map[string]*Node{base: {}}}
	got := a.uniqueID(base, nil)
	want := base + "-2"
	if got != want {
		t.Fatalf("collision after 40-char base = %q, want %q", got, want)
	}
	if len(got) != 42 {
		t.Errorf("suffixed id len = %d, want 42 (current: suffix is not re-capped at 40)", len(got))
	}
	// Suffixed form remains a valid tmux session name (alphanumeric + hyphen).
	if !tmuxsession.ValidName(got) {
		t.Errorf("suffixed id %q is not tmux-safe", got)
	}
}

func TestUniqueIDCollisionSourcesAndChains(t *testing.T) {
	// Combined chain: byID, taken (tmux sessions), and reserved (in-flight create)
	// all count as occupied; the first free numeric suffix wins.
	a := &app{
		byID:     map[string]*Node{"slot": {}},
		reserved: map[string]bool{"slot-2": true},
	}
	taken := map[string]bool{"slot-3": true}
	if got := a.uniqueID("slot", taken); got != "slot-4" {
		t.Errorf("combined collision chain = %q, want slot-4", got)
	}

	// In-flight reserved id alone is skipped even when byID and taken are clear.
	b := &app{
		byID:     map[string]*Node{},
		reserved: map[string]bool{"inflight": true},
	}
	if got := b.uniqueID("inflight", nil); got != "inflight-2" {
		t.Errorf("reserved-only collision = %q, want inflight-2", got)
	}

	// Taken set alone (unadopted tmux session names).
	c := &app{byID: map[string]*Node{}}
	if got := c.uniqueID("live", map[string]bool{"live": true, "live-2": true}); got != "live-3" {
		t.Errorf("taken chain = %q, want live-3", got)
	}

	// byID chain alone advances past occupied suffixes.
	d := &app{byID: map[string]*Node{
		"chain":   {},
		"chain-2": {},
		"chain-3": {},
	}}
	if got := d.uniqueID("chain", nil); got != "chain-4" {
		t.Errorf("byID chain = %q, want chain-4", got)
	}
}

func TestUniqueIDEveryMatrixIDIsTmuxSafe(t *testing.T) {
	a := &app{byID: map[string]*Node{}}
	titles := []string{
		"Sweep thresholds for RQ2",
		"häßliche Umlaute",
		"",
		"!!!",
		"--leading-dashes",
		"Review notes-design.md - 2",
		"v2.0 baseline",
		"a   b",
		"foo/bar:baz",
		"foo___bar",
		strings.Repeat("z", 100),
		"  spaced\ttitle\n",
		"end.",
		"...start",
	}
	// Occupy a few so collision suffixes also get checked.
	a.byID["Sweep-thresholds-for-RQ2"] = &Node{}
	a.reserved = map[string]bool{"chat": true}
	for _, title := range titles {
		got := a.uniqueID(title, map[string]bool{"v2-0-baseline": true})
		if !tmuxsession.ValidName(got) {
			t.Errorf("uniqueID(%q) = %q is not a valid tmux session name", title, got)
		}
		if strings.Contains(got, ".") {
			t.Errorf("uniqueID(%q) = %q contains '.', which breaks pane targeting", title, got)
		}
		if strings.HasPrefix(got, "-") {
			t.Errorf("uniqueID(%q) = %q has leading '-', which parses as a flag", title, got)
		}
	}
}

// ---------- 2. Transport and fork inheritance matrix ----------

func TestNodeTransportEmptyIsTmuxForAllAgents(t *testing.T) {
	// Legacy records with an absent Transport migrate to tmux regardless of
	// agent name — the field predates pi/opencode/codex structured transports.
	for _, agent := range []string{"claude", "codex", "pi", "opencode", "anything"} {
		n := &Node{Agent: agent, Transport: ""}
		if got := n.transport(); got != "tmux" {
			t.Errorf("empty Transport on agent %q = %q, want tmux", agent, got)
		}
	}
	// Explicit values pass through unchanged.
	for _, tr := range []string{"tmux", "acp", "codex"} {
		if got := (&Node{Transport: tr}).transport(); got != tr {
			t.Errorf("explicit transport %q = %q", tr, got)
		}
	}
}

func TestResolveNodeRootTransportByAgent(t *testing.T) {
	dir := t.TempDir()
	a := &app{home: dir, byID: map[string]*Node{}}
	cases := []struct {
		agent, want string
	}{
		{"claude", "tmux"},
		{"codex", "codex"},
		{"pi", "acp"},
		{"opencode", "acp"},
	}
	for _, c := range cases {
		n := Node{Title: "T", Prompt: "p", Agent: c.agent, Dir: dir}
		if status, err := a.resolveNode(&n); err != nil {
			t.Fatalf("resolveNode agent=%s: %d %v", c.agent, status, err)
		}
		if n.Transport != c.want {
			t.Errorf("root agent %q: Transport = %q, want %q", c.agent, n.Transport, c.want)
		}
	}
}

func TestResolveNodeForkInheritanceMatrix(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	a := &app{home: dir, byID: map[string]*Node{
		"cx": {
			ID: "cx", Agent: "codex", Model: "gpt-5.5", Effort: "high",
			Dir: dir, LaneID: "lane-a", Transport: "codex",
			// Conversation / identity fields that must never transfer.
			SessionID: "thread-1", Transcript: "/tmp/rollout.jsonl",
			EndedAt: "2026-01-01T00:00:00Z", Adopted: true,
			Rationale: "parent reason", CreatedAt: "2026-01-01T00:00:00Z",
			Prompt: "parent prompt", Description: "parent desc", Title: "Parent",
		},
		// Legacy pi parent: empty Transport means tmux after migration.
		"pi-legacy": {
			ID: "pi-legacy", Agent: "pi", Model: "mistral/devstral-latest",
			Dir: dir, LaneID: "lane-pi", Transport: "",
			SessionID: "acp-sess", Transcript: "",
		},
		"cl": {
			ID: "cl", Agent: "claude", Model: "opus", Effort: "medium",
			Dir: dir, LaneID: "lane-cl", Transport: "tmux",
		},
	}}

	// Same-agent fork inherits model, effort, transport, directory, and lane.
	same := Node{Title: "Fork", Prompt: "fresh", Parent: "cx"}
	if status, err := a.resolveNode(&same); err != nil {
		t.Fatalf("same-agent fork: %d %v", status, err)
	}
	if same.Agent != "codex" || same.Model != "gpt-5.5" || same.Effort != "high" {
		t.Errorf("same-agent fork agent/model/effort = %q/%q/%q", same.Agent, same.Model, same.Effort)
	}
	if same.Transport != "codex" || same.Dir != dir || same.LaneID != "lane-a" {
		t.Errorf("same-agent fork transport/dir/lane = %q/%q/%q", same.Transport, same.Dir, same.LaneID)
	}
	// No conversation identity / history fields are inherited.
	if same.SessionID != "" || same.Transcript != "" || same.EndedAt != "" || same.Adopted {
		t.Errorf("fork inherited conversation identity: session=%q transcript=%q ended=%q adopted=%v",
			same.SessionID, same.Transcript, same.EndedAt, same.Adopted)
	}
	if same.Rationale != "" || same.CreatedAt != "" {
		t.Errorf("fork inherited rationale/created_at: %q / %q", same.Rationale, same.CreatedAt)
	}
	// Child keeps its own prompt/title, not the parent's.
	if same.Prompt != "fresh" || same.Title != "Fork" {
		t.Errorf("fork overwrote prompt/title: %q / %q", same.Prompt, same.Title)
	}

	// Same-agent fork from a legacy empty-Transport parent inherits migrated tmux.
	legacy := Node{Title: "T", Prompt: "p", Parent: "pi-legacy"}
	if status, err := a.resolveNode(&legacy); err != nil {
		t.Fatalf("legacy fork: %d %v", status, err)
	}
	if legacy.Transport != "tmux" || legacy.Agent != "pi" || legacy.Model != "mistral/devstral-latest" {
		t.Errorf("legacy empty-Transport fork = agent=%q model=%q transport=%q",
			legacy.Agent, legacy.Model, legacy.Transport)
	}

	// Explicit same-agent overrides remain respected.
	over := Node{
		Title: "T", Prompt: "p", Parent: "cx",
		Model: "gpt-other", Effort: "low", Transport: "codex", Dir: other, LaneID: "lane-b",
	}
	if status, err := a.resolveNode(&over); err != nil {
		t.Fatalf("override fork: %d %v", status, err)
	}
	if over.Model != "gpt-other" || over.Effort != "low" || over.Dir != other || over.LaneID != "lane-b" {
		t.Errorf("explicit overrides not kept: model=%q effort=%q dir=%q lane=%q",
			over.Model, over.Effort, over.Dir, over.LaneID)
	}

	// Switching agent does not inherit model, effort, or transport; derives the
	// selected agent's transport; retains cross-agent fields (dir/lane).
	sw := Node{Title: "T", Prompt: "p", Parent: "cx", Agent: "claude"}
	if status, err := a.resolveNode(&sw); err != nil {
		t.Fatalf("agent-switch fork: %d %v", status, err)
	}
	if sw.Agent != "claude" {
		t.Errorf("agent-switch agent = %q, want claude", sw.Agent)
	}
	if sw.Model != "" || sw.Effort != "" {
		t.Errorf("agent-switch must not inherit model/effort, got %q/%q", sw.Model, sw.Effort)
	}
	if sw.Transport != "tmux" {
		t.Errorf("agent-switch transport = %q, want claude's tmux", sw.Transport)
	}
	if sw.Dir != dir || sw.LaneID != "lane-a" {
		t.Errorf("agent-switch should retain dir/lane, got %q/%q", sw.Dir, sw.LaneID)
	}

	// Switch onto pi derives acp, still without the parent's codex model.
	swPi := Node{Title: "T", Prompt: "p", Parent: "cx", Agent: "pi"}
	if status, err := a.resolveNode(&swPi); err != nil {
		t.Fatalf("switch-to-pi: %d %v", status, err)
	}
	if swPi.Transport != "acp" || swPi.Model != "" || swPi.Effort != "" {
		t.Errorf("switch-to-pi transport/model/effort = %q/%q/%q", swPi.Transport, swPi.Model, swPi.Effort)
	}

	// Switch from claude to codex derives codex transport.
	swCx := Node{Title: "T", Prompt: "p", Parent: "cl", Agent: "codex"}
	if status, err := a.resolveNode(&swCx); err != nil {
		t.Fatalf("switch-to-codex: %d %v", status, err)
	}
	if swCx.Transport != "codex" || swCx.Model != "" || swCx.Effort != "" {
		t.Errorf("switch-to-codex transport/model/effort = %q/%q/%q", swCx.Transport, swCx.Model, swCx.Effort)
	}
}

func TestResolveNodeIdempotent(t *testing.T) {
	dir := t.TempDir()
	a := &app{home: dir, byID: map[string]*Node{
		"p": {ID: "p", Agent: "codex", Model: "gpt-5.5", Effort: "high", Dir: dir, LaneID: "L", Transport: "codex"},
	}}
	n := Node{Title: "  Fork  ", Prompt: "  hi  ", Description: "  d  ", Parent: "p"}
	if status, err := a.resolveNode(&n); err != nil {
		t.Fatalf("first resolve: %d %v", status, err)
	}
	// Snapshot every field resolveNode mutates (or leaves alone).
	snap := n
	if status, err := a.resolveNode(&n); err != nil {
		t.Fatalf("second resolve: %d %v", status, err)
	}
	if n != snap {
		t.Errorf("resolveNode is not idempotent:\n first  = %+v\n second = %+v", snap, n)
	}
}

// ---------- 3. Request-resolution validation / default matrix ----------

func TestResolveNodeTrimmingAndFallbacks(t *testing.T) {
	dir := t.TempDir()
	a := &app{home: dir, byID: map[string]*Node{}}

	// Title / prompt / description / lane trimming.
	n := Node{
		Title: "  My Title  ", Prompt: "  the prompt  ",
		Description: "  the desc  ", LaneID: "  lane-1  ", Dir: dir, Agent: "claude",
	}
	if status, err := a.resolveNode(&n); err != nil {
		t.Fatalf("trim: %d %v", status, err)
	}
	if n.Title != "My Title" || n.Prompt != "the prompt" || n.Description != "the desc" || n.LaneID != "lane-1" {
		t.Errorf("trimming failed: title=%q prompt=%q desc=%q lane=%q",
			n.Title, n.Prompt, n.Description, n.LaneID)
	}

	// Prompt falls back from description when prompt is empty/whitespace.
	fromDesc := Node{Title: "T", Description: "  use me  ", Dir: dir}
	if status, err := a.resolveNode(&fromDesc); err != nil {
		t.Fatalf("prompt from description: %d %v", status, err)
	}
	if fromDesc.Prompt != "use me" {
		t.Errorf("prompt fallback from description = %q, want %q", fromDesc.Prompt, "use me")
	}
	// Description already set (after trim) is kept; prompt was empty so took description.
	if fromDesc.Description != "use me" {
		t.Errorf("description = %q after prompt-from-desc path", fromDesc.Description)
	}

	// Prompt falls back from title when both prompt and description are empty.
	fromTitle := Node{Title: "Only Title", Dir: dir}
	if status, err := a.resolveNode(&fromTitle); err != nil {
		t.Fatalf("prompt from title: %d %v", status, err)
	}
	if fromTitle.Prompt != "Only Title" {
		t.Errorf("prompt fallback from title = %q", fromTitle.Prompt)
	}
	// Description falls back from the resolved prompt.
	if fromTitle.Description != "Only Title" {
		t.Errorf("description fallback from prompt = %q", fromTitle.Description)
	}

	// Description falls back from an explicit prompt.
	fromPrompt := Node{Title: "T", Prompt: "explicit prompt", Dir: dir}
	if status, err := a.resolveNode(&fromPrompt); err != nil {
		t.Fatalf("desc from prompt: %d %v", status, err)
	}
	if fromPrompt.Description != "explicit prompt" {
		t.Errorf("description fallback = %q, want explicit prompt", fromPrompt.Description)
	}

	// Defaults: agent claude, directory home.
	def := Node{Title: "T", Prompt: "p"}
	if status, err := a.resolveNode(&def); err != nil {
		t.Fatalf("defaults: %d %v", status, err)
	}
	if def.Agent != "claude" {
		t.Errorf("default agent = %q, want claude", def.Agent)
	}
	absHome, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if def.Dir != absHome {
		t.Errorf("default dir = %q, want home %q", def.Dir, absHome)
	}
}

func TestResolveNodeRelativeDirectory(t *testing.T) {
	// Relative directories become absolute according to filepath.Abs (cwd-based).
	// Isolate under t.TempDir + t.Chdir so the test never writes into the repo
	// worktree. Not parallel: Chdir is process-global.
	base := t.TempDir()
	t.Chdir(base)
	relName := "project"
	if err := os.Mkdir(relName, 0o755); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(relName)
	if err != nil {
		t.Fatal(err)
	}

	a := &app{home: t.TempDir(), byID: map[string]*Node{}}
	n := Node{Title: "T", Prompt: "p", Dir: relName, Agent: "claude"}
	if status, err := a.resolveNode(&n); err != nil {
		t.Fatalf("relative dir: %d %v", status, err)
	}
	if n.Dir != want {
		t.Errorf("relative dir resolved to %q, want %q", n.Dir, want)
	}
	if !filepath.IsAbs(n.Dir) {
		t.Errorf("resolved dir is not absolute: %q", n.Dir)
	}
}

func TestResolveNodeValidationFailuresAre400(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{home: dir, byID: map[string]*Node{
		"exists": {ID: "exists", Agent: "claude", Dir: dir},
	}}

	bad := []struct {
		name string
		n    Node
	}{
		{"empty title", Node{Prompt: "p", Dir: dir}},
		{"whitespace-only title", Node{Title: "   \t  ", Prompt: "p", Dir: dir}},
		{"missing parent", Node{Title: "T", Prompt: "p", Parent: "nope", Dir: dir}},
		{"missing parent with explicit agent", Node{Title: "T", Prompt: "p", Parent: "nope", Agent: "codex", Dir: dir}},
		{"unknown agent", Node{Title: "T", Prompt: "p", Agent: "gemini", Dir: dir}},
		{"missing directory", Node{Title: "T", Prompt: "p", Dir: filepath.Join(dir, "missing")}},
		{"file as directory", Node{Title: "T", Prompt: "p", Dir: filePath}},
	}
	for _, c := range bad {
		n := c.n // copy
		status, err := a.resolveNode(&n)
		if err == nil || status != 400 {
			t.Errorf("%s: status/err = %d, %v; want 400 and error", c.name, status, err)
		}
		// Pure validation: no side effects on the app maps.
		if len(a.byID) != 1 {
			t.Errorf("%s: resolveNode mutated byID", c.name)
		}
	}

	// Valid root and fork combinations still succeed (sanity against the matrix).
	root := Node{Title: "Root", Prompt: "p", Agent: "claude", Dir: dir}
	if status, err := a.resolveNode(&root); err != nil || status != 0 {
		t.Fatalf("valid root: %d %v", status, err)
	}
	fork := Node{Title: "Child", Prompt: "p", Parent: "exists"}
	if status, err := a.resolveNode(&fork); err != nil || status != 0 {
		t.Fatalf("valid fork: %d %v", status, err)
	}
}

// ---------- 4. Agent launch-command matrix ----------

func TestShellQuoteMatrix(t *testing.T) {
	// Extends TestShellQuote with empty string and additional metacharacters.
	// Existing cases in this file remain the primary coverage for the rest.
	cases := map[string]string{
		"":                 "''",
		"plain":            "'plain'",
		"two words":        "'two words'",
		"don't":            `'don'\''t'`,
		"a'b'c":            `'a'\''b'\''c'`,
		"line1\nline2":     "'line1\nline2'",
		"ünïcode 🚗":        "'ünïcode 🚗'",
		"$HOME `id` \"x\"": "'$HOME `id` \"x\"'",
		"a;b|c&d":          "'a;b|c&d'",
		"$(rm -rf /)":      "'$(rm -rf /)'",
		"`echo hi`":        "'`echo hi`'",
		"end\\slash":       "'end\\slash'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestAgentCommandClaudeMatrix(t *testing.T) {
	// Exact Claude command with and without title/model/effort. Every owned
	// Claude launch carries exactly one --ax-screen-reader (screen-reader default).
	full := &Node{
		Agent: "claude", SessionID: "uuid-1", Title: "My Session",
		Model: "opus", Effort: "high", Prompt: "hello world",
	}
	got, err := agentCommand(full)
	if err != nil {
		t.Fatal(err)
	}
	want := `claude --session-id uuid-1 --ax-screen-reader --remote-control 'My Session' --model 'opus' --effort 'high' 'hello world'`
	if got != want {
		t.Errorf("full claude =\n  %s\nwant\n  %s", got, want)
	}
	if strings.Count(got, "--ax-screen-reader") != 1 {
		t.Errorf("full claude must contain exactly one --ax-screen-reader, got %s", got)
	}

	// Title only (no model, no effort).
	titleOnly := &Node{Agent: "claude", SessionID: "u", Title: "T", Prompt: "p"}
	if got, _ := agentCommand(titleOnly); got != `claude --session-id u --ax-screen-reader --remote-control 'T' 'p'` {
		t.Errorf("title-only claude = %s", got)
	}

	// Model only (empty title omits the remote-control argument value slot's
	// extra quoting path — title empty means the --remote-control flag still
	// appears but with no following quoted title; characterize current argv).
	modelOnly := &Node{Agent: "claude", SessionID: "u", Model: "sonnet", Prompt: "p"}
	got, _ = agentCommand(modelOnly)
	// Current: parts = claude --session-id u --ax-screen-reader --remote-control --model 'sonnet' 'p'
	// because empty Title skips the shellQuote(title) append only.
	wantModel := `claude --session-id u --ax-screen-reader --remote-control --model 'sonnet' 'p'`
	if got != wantModel {
		t.Errorf("model-only claude =\n  %s\nwant\n  %s", got, wantModel)
	}

	bare := &Node{Agent: "claude", SessionID: "uuid-2", Prompt: "p"}
	if got, _ := agentCommand(bare); got != `claude --session-id uuid-2 --ax-screen-reader --remote-control 'p'` {
		t.Errorf("bare claude = %s", got)
	}
}

func TestAgentCommandPiOpencodeMatrix(t *testing.T) {
	// Exact pi / opencode fallback commands; effort is not a supported flag on
	// either harness via agentCommand (characterize: Effort is ignored).
	// --ax-screen-reader is Claude-only and must never appear here.
	piFull := &Node{Agent: "pi", Model: "mistral/devstral-latest", Effort: "high", Prompt: "hello"}
	if got, err := agentCommand(piFull); err != nil {
		t.Fatal(err)
	} else if got != `pi --model 'mistral/devstral-latest' 'hello'` {
		t.Errorf("pi+model+effort = %s (effort must not add a flag)", got)
	}
	if got, _ := agentCommand(piFull); strings.Contains(got, "--effort") {
		t.Errorf("pi command must not pass --effort, got %s", got)
	}
	if got, _ := agentCommand(piFull); strings.Contains(got, "--ax-screen-reader") {
		t.Errorf("pi command must not pass --ax-screen-reader, got %s", got)
	}

	piBare := &Node{Agent: "pi", Effort: "xhigh", Prompt: "p"}
	if got, _ := agentCommand(piBare); got != `pi 'p'` {
		t.Errorf("bare pi = %s", got)
	}

	ocFull := &Node{Agent: "opencode", Model: "openai/gpt-5.5", Effort: "medium", Prompt: "hello"}
	if got, _ := agentCommand(ocFull); got != `opencode --model 'openai/gpt-5.5' --prompt 'hello'` {
		t.Errorf("opencode+model = %s", got)
	}
	if got, _ := agentCommand(ocFull); strings.Contains(got, "--effort") {
		t.Errorf("opencode command must not pass --effort, got %s", got)
	}
	if got, _ := agentCommand(ocFull); strings.Contains(got, "--ax-screen-reader") {
		t.Errorf("opencode command must not pass --ax-screen-reader, got %s", got)
	}

	ocBare := &Node{Agent: "opencode", Prompt: "p"}
	if got, _ := agentCommand(ocBare); got != `opencode --prompt 'p'` {
		t.Errorf("bare opencode = %s", got)
	}

	// grok legacy/forced-tmux fallback: -m and --reasoning-effort like ACP argv.
	gFull := &Node{Agent: "grok", Model: "grok-4.5", Effort: "low", Prompt: "hello"}
	if got, _ := agentCommand(gFull); got != `grok -m 'grok-4.5' --reasoning-effort 'low' 'hello'` {
		t.Errorf("grok+model+effort = %s", got)
	}
	if got, _ := agentCommand(gFull); strings.Contains(got, "--ax-screen-reader") {
		t.Errorf("grok command must not pass --ax-screen-reader, got %s", got)
	}
	gBare := &Node{Agent: "grok", Prompt: "p"}
	if got, _ := agentCommand(gBare); got != `grok 'p'` {
		t.Errorf("bare grok = %s", got)
	}

	// Prompt quoting for the fallback path.
	tricky := &Node{Agent: "pi", Prompt: `don't run $(rm -rf /)`}
	if got, _ := agentCommand(tricky); !strings.Contains(got, `'don'\''t run $(rm -rf /)'`) {
		t.Errorf("pi tricky prompt not inert: %s", got)
	}
}

func TestAgentCommandRejectsCodexAndUnknown(t *testing.T) {
	// codex uses app-server, not tmux agentCommand.
	if _, err := agentCommand(&Node{Agent: "codex", Prompt: "p"}); err == nil {
		t.Error("agentCommand must reject codex")
	}
	for _, agent := range []string{"gemini", "", "Claude"} {
		if _, err := agentCommand(&Node{Agent: agent, Prompt: "p"}); err == nil {
			t.Errorf("agentCommand must reject unknown agent %q", agent)
		}
	}
}

// ---------- 5. Launch-failure formatting / parsing matrix ----------

func TestLaunchFailReasonMatrix(t *testing.T) {
	const fallback = "the process exited immediately (no output); check the agent and --model options"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", fallback},
		{"only blanks", "  \n\t\n  ", fallback},
		{"one line", "API Error: model not found", "API Error: model not found"},
		{"one line padded", "  API Error: model not found  \n", "API Error: model not found"},
		{"two lines", "line-a\nline-b", "line-a / line-b"},
		{"three lines", "a\nb\nc", "a / b / c"},
		{"more than three keeps last three", "one\ntwo\nthree\nfour\nfive", "three / four / five"},
		{"blank lines ignored", "a\n\n  \nb\n\nc", "a / b / c"},
		{"surrounding whitespace", "\n  first  \n\n  second  \n", "first / second"},
		{"four with blanks", "keep1\n\nkeep2\n\nkeep3\n\nkeep4\n", "keep2 / keep3 / keep4"},
	}
	for _, c := range cases {
		if got := launchFailReason(c.in); got != c.want {
			t.Errorf("%s: launchFailReason(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestWrapLaunchExactShape(t *testing.T) {
	cmd := `claude --session-id u --remote-control 'T' --model 'opus' 'hi'`
	w := wrapLaunch(cmd)

	// Original command verbatim first, then status capture and conditional hold.
	want := fmt.Sprintf(
		`%s; __ec=$?; if [ "$__ec" != 0 ]; then printf '\n%s (status %%d)\n' "$__ec"; sleep %d; fi`,
		cmd, launchFailSentinel, launchHoldSeconds,
	)
	if w != want {
		t.Errorf("wrapLaunch exact shape mismatch:\n got  %q\n want %q", w, want)
	}
	if !strings.HasPrefix(w, cmd+";") {
		t.Error("wrapper must run the original command first")
	}
	if !strings.Contains(w, `__ec=$?`) {
		t.Error("wrapper must capture exit status")
	}
	if !strings.Contains(w, `!= 0`) {
		t.Error("sentinel must be gated on non-zero status")
	}
	if !strings.Contains(w, launchFailSentinel) {
		t.Error("wrapper must include launchFailSentinel")
	}
	if launchHoldSeconds != 30 {
		t.Errorf("launchHoldSeconds = %d, want 30 (current hold window)", launchHoldSeconds)
	}
	if !strings.Contains(w, fmt.Sprintf("sleep %d", launchHoldSeconds)) {
		t.Errorf("wrapper must sleep launchHoldSeconds (%d)", launchHoldSeconds)
	}
	// No real shell execution — this is pure string characterization.
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":            "'plain'",
		"two words":        "'two words'",
		"don't":            `'don'\''t'`,
		"a'b'c":            `'a'\''b'\''c'`,
		"line1\nline2":     "'line1\nline2'",
		"ünïcode 🚗":        "'ünïcode 🚗'",
		"$HOME `id` \"x\"": "'$HOME `id` \"x\"'", // no expansion inside single quotes
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestAgentCommand(t *testing.T) {
	claude := &Node{Agent: "claude", SessionID: "uuid-1", Title: "My Session", Model: "opus", Prompt: "hello world"}
	got, err := agentCommand(claude)
	if err != nil {
		t.Fatal(err)
	}
	if got != `claude --session-id uuid-1 --ax-screen-reader --remote-control 'My Session' --model 'opus' 'hello world'` {
		t.Errorf("claude cmd = %s", got)
	}
	if strings.Count(got, "--ax-screen-reader") != 1 {
		t.Errorf("claude cmd must contain exactly one --ax-screen-reader, got %s", got)
	}

	claudeBare := &Node{Agent: "claude", SessionID: "uuid-2", Prompt: "p"}
	if got, _ := agentCommand(claudeBare); got != `claude --session-id uuid-2 --ax-screen-reader --remote-control 'p'` {
		t.Errorf("bare claude cmd = %s", got)
	}

	// codex no longer launches over tmux — it is supervised through the codex
	// app-server bridge — so it has no tmux launch command line.
	codex := &Node{Agent: "codex", Model: "gpt-5.5", Effort: "high", Prompt: "sweep thresholds"}
	if _, err := agentCommand(codex); err == nil {
		t.Error("agentCommand should reject codex (no tmux launch path)")
	}

	// A prompt containing quotes and shell metacharacters must stay inert.
	tricky := &Node{Agent: "claude", SessionID: "u", Prompt: `don't run $(rm -rf /); echo "done"`}
	got, _ = agentCommand(tricky)
	if !strings.Contains(got, `'don'\''t run $(rm -rf /); echo "done"'`) {
		t.Errorf("tricky prompt not quoted inertly: %s", got)
	}
	if strings.Count(got, "--ax-screen-reader") != 1 {
		t.Errorf("tricky claude must still carry exactly one --ax-screen-reader, got %s", got)
	}

	if _, err := agentCommand(&Node{Agent: "gemini", Prompt: "p"}); err == nil {
		t.Error("unknown agent must error")
	}
}

// The claude CLI accepts --effort <level> (low/medium/high/xhigh/max); scimux
// passes it when set, and omits it entirely otherwise (so a node with no effort
// launches exactly as before).
func TestAgentCommandClaudeEffort(t *testing.T) {
	n := &Node{Agent: "claude", SessionID: "u", Title: "T", Model: "claude-opus-4-8", Effort: "medium", Prompt: "hi"}
	got, _ := agentCommand(n)
	if got != `claude --session-id u --ax-screen-reader --remote-control 'T' --model 'claude-opus-4-8' --effort 'medium' 'hi'` {
		t.Errorf("claude+effort cmd = %s", got)
	}
	// No effort -> no --effort flag.
	n2 := &Node{Agent: "claude", SessionID: "u", Prompt: "hi"}
	if got, _ := agentCommand(n2); strings.Contains(got, "--effort") {
		t.Errorf("effort-less claude cmd must omit --effort, got %s", got)
	}
	if got, _ := agentCommand(n2); got != `claude --session-id u --ax-screen-reader --remote-control 'hi'` {
		t.Errorf("effort-less claude cmd = %s", got)
	}
}

// A tmux launch is wrapped so that a process which exits before its interface
// is ready (a rejected --model, a bad flag) leaves its error on the pane long
// enough for awaitLaunch to read it, instead of the session vanishing into an
// unexplained dead node. The wrapper must run the original command verbatim
// first (so the first prompt still rides the command line) and must not touch
// the clean-exit path.
func TestWrapLaunchDiagnostics(t *testing.T) {
	cmd := `claude --session-id u --remote-control 'T' --model 'opus' 'hi'`
	w := wrapLaunch(cmd)
	if !strings.HasPrefix(w, cmd+";") {
		t.Errorf("wrapper must run the original command first, got %q", w)
	}
	if !strings.Contains(w, launchFailSentinel) {
		t.Errorf("wrapper must emit the launch-failed sentinel, got %q", w)
	}
	// The sentinel is only printed on a non-zero exit — a clean exit must fall
	// through untouched (pane closes, session dies, exactly as before).
	if !strings.Contains(w, `!= 0`) {
		t.Errorf("wrapper must guard the sentinel behind a non-zero exit, got %q", w)
	}
}

// A fork of an old or adopted pi/opencode node whose stored Transport predates
// the field (empty → tmux) must stay on tmux, not silently flip to the ACP
// default derived from the agent name (finding 54).
func TestForkInheritsMigratedTransport(t *testing.T) {
	a := &app{byID: map[string]*Node{}, home: t.TempDir()}
	parent := &Node{ID: "root", Agent: "pi", Dir: a.home, Transport: ""}
	a.byID["root"] = parent

	child := &Node{Title: "T", Parent: "root", Prompt: "keep supervising over tmux"}
	if status, err := a.resolveNode(child); err != nil {
		t.Fatalf("resolveNode: %d %v", status, err)
	}
	if got := child.transport(); got != "tmux" {
		t.Errorf("fork of empty-Transport pi parent = %q, want tmux", got)
	}

	// A parent explicitly on ACP is inherited as ACP.
	a.byID["acproot"] = &Node{ID: "acproot", Agent: "pi", Dir: a.home, Transport: "acp"}
	acpChild := &Node{Title: "T", Parent: "acproot", Prompt: "p"}
	if status, err := a.resolveNode(acpChild); err != nil {
		t.Fatalf("resolveNode acp: %d %v", status, err)
	}
	if got := acpChild.transport(); got != "acp" {
		t.Errorf("fork of acp parent = %q, want acp", got)
	}
}

func TestUniqueID(t *testing.T) {
	a := &app{byID: map[string]*Node{}}
	cases := map[string]string{
		"Sweep thresholds for RQ2": "Sweep-thresholds-for-RQ2",
		"häßliche Umlaute":         "h-liche-Umlaute",
		"":                         "chat",
		"!!!":                      "chat",
		"--leading-dashes":         "leading-dashes",
		// A dot is tmux's window.pane target separator: a session named with one
		// is created but then unaddressable (capture/send/liveness all fail), so
		// the slug must never contain it. The title itself keeps the dot.
		"Review notes-design.md - 2": "Review-notes-design-md-2",
		"v2.0 baseline":              "v2-0-baseline",
	}
	for title, want := range cases {
		got := a.uniqueID(title, nil)
		if got != want {
			t.Errorf("uniqueID(%q) = %q, want %q", title, got, want)
		}
		if !tmuxsession.ValidName(got) {
			t.Errorf("uniqueID(%q) = %q is not a valid tmux session name", title, got)
		}
	}
	// Long titles are truncated.
	long := a.uniqueID(strings.Repeat("x", 100), nil)
	if len(long) > 40 {
		t.Errorf("long id not truncated: %d chars", len(long))
	}
	// Collisions get numeric suffixes.
	a.byID["demo"] = &Node{}
	if got := a.uniqueID("demo", nil); got != "demo-2" {
		t.Errorf("collision id = %q, want demo-2", got)
	}
	a.byID["demo-2"] = &Node{}
	if got := a.uniqueID("demo", nil); got != "demo-3" {
		t.Errorf("second collision id = %q, want demo-3", got)
	}
	// Unadopted tmux sessions reserve their names too (finding 11).
	if got := a.uniqueID("adopted", map[string]bool{"adopted": true}); got != "adopted-2" {
		t.Errorf("session-name collision id = %q, want adopted-2", got)
	}
}

func TestFirstWords(t *testing.T) {
	if got := firstWords("one two three four", 2); got != "one two" {
		t.Errorf("got %q", got)
	}
	if got := firstWords("short", 6); got != "short" {
		t.Errorf("got %q", got)
	}
	if got := firstWords("  spaced\n\tout  words ", 3); got != "spaced out words" {
		t.Errorf("got %q", got)
	}
}

func TestNewUUID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := newUUID()
		if err != nil {
			t.Fatalf("newUUID: %v", err)
		}
		if !re.MatchString(id) {
			t.Fatalf("not a v4 uuid: %s", id)
		}
		if seen[id] {
			t.Fatalf("duplicate uuid: %s", id)
		}
		seen[id] = true
	}
}

// A failing RNG must surface as an error, not a zero/partial session id.
func TestNewUUIDRNGFailure(t *testing.T) {
	orig := randSource
	randSource = iotest.ErrReader(errors.New("rng down"))
	defer func() { randSource = orig }()
	if _, err := newUUID(); err == nil {
		t.Fatal("expected error when the RNG fails")
	}
}

func TestSessionArgFromCmdline(t *testing.T) {
	id := "12345678-1234-4234-8234-123456789abc"
	cases := [][]string{
		{"claude", "--resume", id},
		{"claude", "-r", id},
		{"claude", "--session-id", id, "hello"},
		{"sh", "-c", "claude --resume " + id},
		{"/bin/sh", "-c", "claude --model opus --session-id " + id + " 'prompt'"},
	}
	for _, args := range cases {
		if got := sessionArgFromCmdline(args); got != id {
			t.Errorf("sessionArgFromCmdline(%v) = %q, want %q", args, got, id)
		}
	}
	for _, args := range [][]string{
		{"claude", "--resume"},               // missing value
		{"claude", "--resume", "not-a-uuid"}, // bad value
		{"bash", "--norc"},                   // unrelated
		{"vim", "notes--resume plan.md"},     // substring red herring
		nil,
	} {
		if got := sessionArgFromCmdline(args); got != "" {
			t.Errorf("sessionArgFromCmdline(%v) = %q, want empty", args, got)
		}
	}
}

// resolveNode is the single validation/resolution step shared by
// handleNewNode's snapshot decision and createNode, so the two can never
// diverge; a request that fails any validation — including an unknown
// parent combined with an explicit codex agent — resolves to an error
// before any codex-history walk (findings 22, 25).
func TestResolveNode(t *testing.T) {
	dir := t.TempDir()
	a := &app{home: dir, byID: map[string]*Node{
		"cx": {ID: "cx", Agent: "codex", Model: "gpt-5.5", Effort: "high", Dir: dir},
		"cl": {ID: "cl", Agent: "claude", Dir: dir},
	}}
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644) // a file, not a dir

	ok := []struct {
		n        Node
		agent, d string
	}{
		{Node{Title: "T", Prompt: "p", Agent: "codex", Dir: dir}, "codex", dir},
		{Node{Title: "T", Prompt: "p", Parent: "cx"}, "codex", dir},                   // full inheritance
		{Node{Title: "T", Prompt: "p", Agent: "claude", Parent: "cx"}, "claude", dir}, // explicit beats inherited
		{Node{Title: "T"}, "claude", dir},                                             // defaults: claude, home, prompt from title
	}
	for _, c := range ok {
		if status, err := a.resolveNode(&c.n); err != nil {
			t.Errorf("resolveNode(%+v) failed: %d %v", c.n, status, err)
			continue
		}
		if c.n.Agent != c.agent || c.n.Dir != c.d {
			t.Errorf("resolved agent/dir = %q/%q, want %q/%q", c.n.Agent, c.n.Dir, c.agent, c.d)
		}
		if c.n.Description == "" || c.n.Prompt == "" {
			t.Errorf("resolved prompt/description must be populated, got %q/%q", c.n.Prompt, c.n.Description)
		}
	}
	// Inherited launch config, never the conversation (fresh-context fork).
	forked := Node{Title: "T", Prompt: "p", Parent: "cx"}
	a.resolveNode(&forked)
	if forked.Model != "gpt-5.5" || forked.Effort != "high" {
		t.Errorf("fork must inherit model/effort, got %q/%q", forked.Model, forked.Effort)
	}

	bad := []Node{
		{Prompt: "p"}, // empty title
		{Title: "T", Prompt: "p", Parent: "missing"},                           // unknown parent
		{Title: "T", Prompt: "p", Parent: "missing", Agent: "codex", Dir: dir}, // finding 25: must fail before any walk
		{Title: "T", Prompt: "p", Agent: "gemini"},                             // unknown agent
		{Title: "T", Prompt: "p", Dir: filepath.Join(dir, "nope")},             // missing dir
		{Title: "T", Prompt: "p", Dir: filepath.Join(dir, "f")},                // dir is a file
	}
	for _, n := range bad {
		if status, err := a.resolveNode(&n); err == nil || status != 400 {
			t.Errorf("resolveNode(%+v) = %d, %v; want 400 and error", n, status, err)
		}
	}
}

func TestResolveNodeTransport(t *testing.T) {
	dir := t.TempDir()
	a := &app{home: dir, byID: map[string]*Node{
		// A pi node adopted onto tmux (Transport pinned to "tmux"): its forks
		// must stay tmux, not flip to the agent-derived default.
		"pi-tmux": {ID: "pi-tmux", Agent: "pi", Transport: "tmux", Dir: dir},
	}}
	cases := []struct {
		n    Node
		want string
	}{
		{Node{Title: "T", Prompt: "p", Agent: "pi", Dir: dir}, "acp"},
		{Node{Title: "T", Prompt: "p", Agent: "opencode", Dir: dir}, "acp"},
		{Node{Title: "T", Prompt: "p", Agent: "grok", Dir: dir}, "acp"},
		{Node{Title: "T", Prompt: "p", Agent: "claude", Dir: dir}, "tmux"},
		{Node{Title: "T", Prompt: "p", Agent: "codex", Dir: dir}, "codex"},
		{Node{Title: "T", Prompt: "p", Parent: "pi-tmux"}, "tmux"}, // fork inherits parent transport
	}
	for _, c := range cases {
		if status, err := a.resolveNode(&c.n); err != nil {
			t.Fatalf("resolveNode(%+v) failed: %d %v", c.n, status, err)
		}
		if c.n.Transport != c.want {
			t.Errorf("agent %q parent %q: transport = %q, want %q", c.n.Agent, c.n.Parent, c.n.Transport, c.want)
		}
	}
	// Migration: a stored record with no Transport field is treated as tmux.
	if got := (&Node{Agent: "pi"}).transport(); got != "tmux" {
		t.Errorf("absent transport = %q, want tmux (back-compat migration)", got)
	}
}

// Owned Claude launches always carry exactly one --ax-screen-reader, and the
// durable node marker is set only after a successful launch (before persist).
// Non-Claude agentCommand paths never receive the flag.
func TestAgentCommandClaudeAXScreenReaderOnce(t *testing.T) {
	variants := []*Node{
		{Agent: "claude", SessionID: "uuid-1", Title: "My Session", Model: "opus", Effort: "high", Prompt: "hello world"},
		{Agent: "claude", SessionID: "u", Title: "T", Prompt: "p"},
		{Agent: "claude", SessionID: "u", Model: "sonnet", Prompt: "p"},
		{Agent: "claude", SessionID: "uuid-2", Prompt: "p"},
		{Agent: "claude", SessionID: "u", Title: "T", Model: "claude-opus-4-8", Effort: "medium", Prompt: "hi"},
		{Agent: "claude", SessionID: "u", Prompt: `don't run $(rm -rf /); echo "done"`},
	}
	for _, n := range variants {
		got, err := agentCommand(n)
		if err != nil {
			t.Fatalf("agentCommand(%+v): %v", n, err)
		}
		if c := strings.Count(got, "--ax-screen-reader"); c != 1 {
			t.Errorf("claude cmd must contain exactly one --ax-screen-reader (got %d): %s", c, got)
		}
		// Flag is a fixed option, not a remote-control value: it always sits
		// between --session-id and --remote-control.
		if !strings.Contains(got, "--session-id "+n.SessionID+" --ax-screen-reader --remote-control") {
			t.Errorf("flag placement wrong: %s", got)
		}
	}
	for _, n := range []*Node{
		{Agent: "pi", Prompt: "p"},
		{Agent: "opencode", Prompt: "p"},
		{Agent: "grok", Prompt: "p"},
	} {
		got, err := agentCommand(n)
		if err != nil {
			t.Fatalf("agentCommand(%s): %v", n.Agent, err)
		}
		if strings.Contains(got, "--ax-screen-reader") {
			t.Errorf("%s command must not carry --ax-screen-reader: %s", n.Agent, got)
		}
	}
}

// launchNode is the authoritative seam: a successful owned Claude launch sets
// AXScreenReader before persist, and the spawned command carries the flag.
func TestLaunchNodeSetsAXScreenReaderForClaude(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	n := &Node{
		ID: "ax-claude", Title: "AX", Prompt: "hi", Agent: "claude",
		Dir: a.home, SessionID: "sess-ax", CreatedAt: "2026-08-10T00:00:00Z",
	}
	if status, err := a.launchNode(n, nil); err != nil || status != 0 {
		t.Fatalf("launchNode: status=%d err=%v", status, err)
	}
	if !n.AXScreenReader {
		t.Fatal("successful Claude launch must set AXScreenReader=true before return")
	}
	// Persist recorded the marker (authoritative owned-Claude <=> ax true).
	var saw bool
	for _, r := range keyRecords(t, a.storePath) {
		if r.Type == "node" && r.Node != nil && r.Node.ID == "ax-claude" {
			saw = true
			if !r.Node.AXScreenReader {
				t.Fatal("persisted Claude node must have ax_screen_reader:true")
			}
		}
	}
	if !saw {
		t.Fatal("launchNode did not persist the node record")
	}
	// The tmux new-session command includes exactly one --ax-screen-reader.
	var launch string
	f.mu.Lock()
	for _, c := range f.calls {
		if len(c) >= 3 && c[2] == "new-session" {
			launch = strings.Join(c, " ")
		}
	}
	f.mu.Unlock()
	if c := strings.Count(launch, "--ax-screen-reader"); c != 1 {
		t.Errorf("new-session argv must contain exactly one --ax-screen-reader (got %d): %q", c, launch)
	}
}

// A launch that dies during awaitLaunch must not leave a published or persisted
// AX-marked node (marker is set only after the await gate).
func TestLaunchNodeFailureDoesNotMarkAX(t *testing.T) {
	f := &fakeTmux{capture: "API Error: bad flag\n" + launchFailSentinel + " (status 1)\n"}
	a := newTestApp(t, f)
	n := &Node{
		ID: "ax-fail", Title: "Fail", Prompt: "hi", Agent: "claude",
		Dir: a.home, SessionID: "sess-fail", CreatedAt: "2026-08-10T00:00:00Z",
	}
	if _, err := a.launchNode(n, nil); err == nil {
		t.Fatal("expected launch failure")
	}
	if n.AXScreenReader {
		t.Error("failed launch must not set AXScreenReader on the in-memory node")
	}
	for _, r := range keyRecords(t, a.storePath) {
		if r.Type == "node" {
			t.Fatalf("failed launch must not persist a node record, got %+v", r.Node)
		}
	}
}
