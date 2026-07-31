package main

// Packet 2C characterization: pure node-lifecycle matrix covering the gaps
// left by existing main_test.go / main_http_test.go / agents_test.go coverage.
// No production code is moved or changed here; Packet 2D extracts
// node_lifecycle.go against this matrix.
//
// Intentionally not re-covered (already characterized elsewhere):
// - newUUID / randSource failure (TestNewUUID, TestNewUUIDRNGFailure)
// - sessionArgFromCmdline (TestSessionArgFromCmdline)
// - Claude model cache / probe (agents_test.go)
// - handler-level create validation, wrapper use, launch failure surfacing
//   (main_http_test.go)
// - session-log dead-slug blocking and post-archive reuse
//   (TestArchiveSessionLogNamingAndSlugReuse in store_test.go — Packet 2A)
// - protocol-specific ACP/Codex argv (internal/acp, internal/codex)

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		{"unknown agent grok", Node{Title: "T", Prompt: "p", Agent: "grok", Dir: dir}},
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
	// Existing cases in main_test.go remain the primary coverage for the rest.
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
	// Exact Claude command with and without title/model/effort.
	full := &Node{
		Agent: "claude", SessionID: "uuid-1", Title: "My Session",
		Model: "opus", Effort: "high", Prompt: "hello world",
	}
	got, err := agentCommand(full)
	if err != nil {
		t.Fatal(err)
	}
	want := `claude --session-id uuid-1 --remote-control 'My Session' --model 'opus' --effort 'high' 'hello world'`
	if got != want {
		t.Errorf("full claude =\n  %s\nwant\n  %s", got, want)
	}

	// Title only (no model, no effort).
	titleOnly := &Node{Agent: "claude", SessionID: "u", Title: "T", Prompt: "p"}
	if got, _ := agentCommand(titleOnly); got != `claude --session-id u --remote-control 'T' 'p'` {
		t.Errorf("title-only claude = %s", got)
	}

	// Model only (empty title omits the remote-control argument value slot's
	// extra quoting path — title empty means the --remote-control flag still
	// appears but with no following quoted title; characterize current argv).
	modelOnly := &Node{Agent: "claude", SessionID: "u", Model: "sonnet", Prompt: "p"}
	got, _ = agentCommand(modelOnly)
	// Current: parts = claude --session-id u --remote-control --model 'sonnet' 'p'
	// because empty Title skips the shellQuote(title) append only.
	wantModel := `claude --session-id u --remote-control --model 'sonnet' 'p'`
	if got != wantModel {
		t.Errorf("model-only claude =\n  %s\nwant\n  %s", got, wantModel)
	}

	bare := &Node{Agent: "claude", SessionID: "uuid-2", Prompt: "p"}
	if got, _ := agentCommand(bare); got != `claude --session-id uuid-2 --remote-control 'p'` {
		t.Errorf("bare claude = %s", got)
	}
}

func TestAgentCommandPiOpencodeMatrix(t *testing.T) {
	// Exact pi / opencode fallback commands; effort is not a supported flag on
	// either harness via agentCommand (characterize: Effort is ignored).
	piFull := &Node{Agent: "pi", Model: "mistral/devstral-latest", Effort: "high", Prompt: "hello"}
	if got, err := agentCommand(piFull); err != nil {
		t.Fatal(err)
	} else if got != `pi --model 'mistral/devstral-latest' 'hello'` {
		t.Errorf("pi+model+effort = %s (effort must not add a flag)", got)
	}
	if got, _ := agentCommand(piFull); strings.Contains(got, "--effort") {
		t.Errorf("pi command must not pass --effort, got %s", got)
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

	ocBare := &Node{Agent: "opencode", Prompt: "p"}
	if got, _ := agentCommand(ocBare); got != `opencode --prompt 'p'` {
		t.Errorf("bare opencode = %s", got)
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
	for _, agent := range []string{"gemini", "grok", "", "Claude"} {
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
