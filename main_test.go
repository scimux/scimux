package main

// Unit tests for main.go's pure logic: launch-command construction (where a
// quoting bug would be silent but nasty), id/slug handling, and the
// append-only store round-trip. Handlers and the poller are covered by the
// smoke test in the README, not here — they need tmux and real sessions.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/tmuxsession"
)

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
	claude := &Node{Agent: "claude", SessionID: "uuid-1", Model: "opus", Prompt: "hello world"}
	got, err := agentCommand(claude)
	if err != nil {
		t.Fatal(err)
	}
	if got != `claude --session-id uuid-1 --model 'opus' 'hello world'` {
		t.Errorf("claude cmd = %s", got)
	}

	claudeBare := &Node{Agent: "claude", SessionID: "uuid-2", Prompt: "p"}
	if got, _ := agentCommand(claudeBare); got != `claude --session-id uuid-2 'p'` {
		t.Errorf("bare claude cmd = %s", got)
	}

	codex := &Node{Agent: "codex", Model: "gpt-5.5", Effort: "high", Prompt: "sweep thresholds"}
	got, err = agentCommand(codex)
	if err != nil {
		t.Fatal(err)
	}
	if got != `codex --model 'gpt-5.5' -c 'model_reasoning_effort=high' 'sweep thresholds'` {
		t.Errorf("codex cmd = %s", got)
	}

	// A prompt containing quotes and shell metacharacters must stay inert.
	tricky := &Node{Agent: "claude", SessionID: "u", Prompt: `don't run $(rm -rf /); echo "done"`}
	got, _ = agentCommand(tricky)
	if !strings.Contains(got, `'don'\''t run $(rm -rf /); echo "done"'`) {
		t.Errorf("tricky prompt not quoted inertly: %s", got)
	}

	if _, err := agentCommand(&Node{Agent: "gemini", Prompt: "p"}); err == nil {
		t.Error("unknown agent must error")
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
	}
	for title, want := range cases {
		got := a.uniqueID(title)
		if got != want {
			t.Errorf("uniqueID(%q) = %q, want %q", title, got, want)
		}
		if !tmuxsession.ValidName(got) {
			t.Errorf("uniqueID(%q) = %q is not a valid tmux session name", title, got)
		}
	}
	// Long titles are truncated.
	long := a.uniqueID(strings.Repeat("x", 100))
	if len(long) > 40 {
		t.Errorf("long id not truncated: %d chars", len(long))
	}
	// Collisions get numeric suffixes.
	a.byID["demo"] = &Node{}
	if got := a.uniqueID("demo"); got != "demo-2" {
		t.Errorf("collision id = %q, want demo-2", got)
	}
	a.byID["demo-2"] = &Node{}
	if got := a.uniqueID("demo"); got != "demo-3" {
		t.Errorf("second collision id = %q, want demo-3", got)
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
		id := newUUID()
		if !re.MatchString(id) {
			t.Fatalf("not a v4 uuid: %s", id)
		}
		if seen[id] {
			t.Fatalf("duplicate uuid: %s", id)
		}
		seen[id] = true
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.jsonl")
	w := &app{byID: map[string]*Node{}, storePath: path}
	n := &Node{ID: "rq2-sweep", Parent: "rq2-root", Title: "Sweep", Prompt: "sweep it",
		Rationale: "run 14 failed on low speed", Agent: "codex", Model: "gpt-5.5",
		Effort: "high", Dir: "/tmp", CreatedAt: "2026-07-11T00:00:00Z"}
	if err := w.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		t.Fatal(err)
	}
	if err := w.appendRecord(storeRecord{Type: "transcript", ID: "rq2-sweep", Path: "/x/rollout.jsonl"}); err != nil {
		t.Fatal(err)
	}
	// Corrupt trailing line must be ignored on load.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{{{ not json\n")
	f.Close()

	r := &app{byID: map[string]*Node{}, storePath: path}
	if err := r.loadStore(); err != nil {
		t.Fatal(err)
	}
	if len(r.nodes) != 1 {
		t.Fatalf("want 1 node, got %d", len(r.nodes))
	}
	got := r.byID["rq2-sweep"]
	if got == nil || got.Rationale != n.Rationale || got.Parent != "rq2-root" || got.Effort != "high" {
		t.Fatalf("node not round-tripped: %#v", got)
	}
	if got.Transcript != "/x/rollout.jsonl" {
		t.Fatalf("transcript record not applied: %q", got.Transcript)
	}
}

// TestLoadStoreMissingFile: a fresh install has no store yet.
func TestLoadStoreMissingFile(t *testing.T) {
	a := &app{byID: map[string]*Node{}, storePath: filepath.Join(t.TempDir(), "absent.jsonl")}
	if err := a.loadStore(); err != nil {
		t.Fatalf("missing store must not error: %v", err)
	}
	if len(a.nodes) != 0 {
		t.Fatalf("want 0 nodes, got %d", len(a.nodes))
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

func TestSysloadOnLinux(t *testing.T) {
	s := sysload()
	if s["load"] == "" {
		t.Error("no loadavg read on linux")
	}
	if s["mem"] == "" || !strings.Contains(s["mem"], "%") {
		t.Errorf("mem = %q", s["mem"])
	}
}
