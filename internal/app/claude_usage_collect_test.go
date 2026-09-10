package app

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A Claude probe spends an API turn, so it is only ever run for a user who
// actually has a Claude session open. Any other agent's prompt must not buy a
// Claude reading.
func TestCollectClaudeUsageNeedsALiveClaudeNode(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if _, err := a.collectUsage(context.Background(), "claude"); err == nil {
		t.Fatal("probed with no Claude node at all")
	}
	// A codex node is not a reason to probe Claude.
	n := &Node{ID: "n-codex", Agent: "codex", Title: "codex"}
	a.mu.Lock()
	a.nodes = append(a.nodes, n)
	a.live[n.ID] = "live"
	a.mu.Unlock()
	if _, err := a.collectUsage(context.Background(), "claude"); err == nil {
		t.Fatal("probed for a codex-only session list")
	}
}

// An ended or exited Claude node is not an open session either.
func TestHasLiveClaudeNodeIgnoresDeadNodes(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	ended := &Node{ID: "n-1", Agent: "claude", EndedAt: "2026-09-10T00:00:00Z"}
	exited := &Node{ID: "n-2", Agent: "claude"}
	a.mu.Lock()
	a.nodes = append(a.nodes, ended, exited)
	a.live[ended.ID] = "live"
	a.live[exited.ID] = "exited"
	a.mu.Unlock()
	if a.hasLiveClaudeNode() {
		t.Fatal("ended/exited Claude nodes counted as open sessions")
	}
	open := &Node{ID: "n-3", Agent: "claude"}
	a.mu.Lock()
	a.nodes = append(a.nodes, open)
	a.live[open.ID] = "live"
	a.mu.Unlock()
	if !a.hasLiveClaudeNode() {
		t.Fatal("open Claude node not counted")
	}
}

// The credential read is gone: no code path may name the OAuth endpoint or the
// CLI's credentials file again. This is the point of the whole change.
func TestNoClaudeCredentialOrOAuthPathRemains(t *testing.T) {
	for _, banned := range []string{
		".credentials.json",
		"api/oauth/usage",
		"oauth-2025-04-20",
		"claudeAiOauth",
	} {
		if hits := grepPackageGo(t, banned); len(hits) > 0 {
			t.Fatalf("%q still referenced in %v", banned, hits)
		}
	}
}

// grepPackageGo reports the package's non-test Go files containing needle.
func grepPackageGo(t *testing.T, needle string) []string {
	t.Helper()
	var hits []string
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), needle) {
			hits = append(hits, name)
		}
	}
	return hits
}
