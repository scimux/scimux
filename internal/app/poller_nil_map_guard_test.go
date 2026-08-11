package app

// Packet 10 Stage B: production poller.go must not carry nil-map guards whose
// comments blame tests. Maps are initialized in newApp via initMaps; tests that
// reach poll() call initMaps on their fixtures instead of shipping guards.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPollerHasNoTestBlamedNilMapGuards(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(thisFile), "poller.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read poller.go: %v", err)
	}
	// Match the deleted production shape exactly: a same-line
	// `== nil { // …test…` guard. Behavioral short-circuits with a next-line
	// comment (e.g. sessionSnapshot's bare-server path) are out of scope.
	var hits []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, "== nil {") {
			continue
		}
		commentIdx := strings.Index(line, "//")
		if commentIdx < 0 {
			continue
		}
		if strings.Contains(strings.ToLower(line[commentIdx:]), "test") {
			hits = append(hits, strings.TrimSpace(line))
		}
	}
	if len(hits) > 0 {
		t.Fatalf("poller.go reintroduced nil-map guard(s) blaming tests:\n  %s\n"+
			"use app.initMaps() in fixtures instead", strings.Join(hits, "\n  "))
	}
}
