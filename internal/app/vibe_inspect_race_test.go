package app

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/acp"
)

func TestVibeInspectionCannotReplaceANewerHarnessRefresh(t *testing.T) {
	binDir := t.TempDir()
	writeScript(t, binDir, "vibe-acp", `exit 0`)
	t.Setenv("PATH", binDir)
	agentsCacheMu.Lock()
	loaded, cache, gen := agentsLoaded, agentsCache, agentsGeneration
	agentsCacheMu.Unlock()
	t.Cleanup(func() {
		agentsCacheMu.Lock()
		agentsLoaded, agentsCache, agentsGeneration = loaded, cache, gen
		agentsCacheMu.Unlock()
	})
	storeAgentsSnapshot(map[string]agentInfo{
		"vibe": {Models: []string{}}, "cursor": {Models: []string{"old"}},
	})
	agentsCacheMu.RLock()
	observed := agentsGeneration
	agentsCacheMu.RUnlock()
	entered, release := make(chan struct{}), make(chan struct{})
	previous := probeVibeCatalog
	probeVibeCatalog = func(context.Context, string) (acp.VibeCatalog, error) {
		close(entered)
		<-release
		return acp.VibeCatalog{Models: []string{"alpha"}}, nil
	}
	t.Cleanup(func() { probeVibeCatalog = previous })
	inspected := make(chan map[string]agentInfo, 1)
	go func() { inspected <- inspectVibeCatalog() }()
	<-entered
	refreshed := make(chan map[string]agentInfo, 1)
	go func() {
		refreshed <- refreshAgentsFromGeneration(observed, func() map[string]agentInfo {
			return map[string]agentInfo{"vibe": {Models: []string{}}, "cursor": {Models: []string{"new"}}}
		})
	}()
	// Old code lets the refresh complete while inspection is blocked; the
	// serialized implementation waits. Either way the final cache must be new.
	refreshFinished := false
	select {
	case <-refreshed:
		refreshFinished = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-inspected:
	case <-time.After(time.Second):
		t.Fatal("inspection did not finish")
	}
	if !refreshFinished {
		select {
		case <-refreshed:
		case <-time.After(time.Second):
			t.Fatal("refresh did not finish")
		}
	}
	got := detectAgents()
	if !reflect.DeepEqual(got["cursor"].Models, []string{"new"}) {
		t.Fatalf("inspection reverted Cursor to %+v", got["cursor"])
	}
}

func TestOverlappingVibeInspectionsShareOneProbe(t *testing.T) {
	binDir := t.TempDir()
	writeScript(t, binDir, "vibe-acp", `exit 0`)
	t.Setenv("PATH", binDir)
	agentsCacheMu.Lock()
	loaded, cache, gen := agentsLoaded, agentsCache, agentsGeneration
	agentsCacheMu.Unlock()
	t.Cleanup(func() {
		agentsCacheMu.Lock()
		agentsLoaded, agentsCache, agentsGeneration = loaded, cache, gen
		agentsCacheMu.Unlock()
	})
	storeAgentsSnapshot(map[string]agentInfo{"vibe": {Models: []string{}}})
	entered, release := make(chan struct{}), make(chan struct{})
	previous := probeVibeCatalog
	var calls atomic.Int32
	probeVibeCatalog = func(context.Context, string) (acp.VibeCatalog, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return acp.VibeCatalog{Models: []string{"alpha"}}, nil
	}
	t.Cleanup(func() { probeVibeCatalog = previous })
	first, second := make(chan map[string]agentInfo, 1), make(chan map[string]agentInfo, 1)
	go func() { first <- inspectVibeCatalog() }()
	<-entered
	go func() { second <- inspectVibeCatalog() }()
	close(release)
	for _, ch := range []chan map[string]agentInfo{first, second} {
		select {
		case got := <-ch:
			if !reflect.DeepEqual(got["vibe"].Models, []string{"alpha"}) {
				t.Fatalf("catalog = %+v", got)
			}
		case <-time.After(time.Second):
			t.Fatal("overlapping inspection did not finish")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("overlapping inspections ran %d probes", calls.Load())
	}
}

func TestVibeInspectionKeepsSnapshotWhenBinaryDisappears(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	agentsCacheMu.Lock()
	loaded, cache, gen := agentsLoaded, agentsCache, agentsGeneration
	agentsCacheMu.Unlock()
	t.Cleanup(func() {
		agentsCacheMu.Lock()
		agentsLoaded, agentsCache, agentsGeneration = loaded, cache, gen
		agentsCacheMu.Unlock()
	})
	before := storeAgentsSnapshot(map[string]agentInfo{"vibe": {Models: []string{}}, "cursor": {Models: []string{"new"}}})
	got := inspectVibeCatalog()
	if !reflect.DeepEqual(got, before) || !reflect.DeepEqual(detectAgents(), before) {
		t.Fatalf("missing binary changed catalog: %+v", got)
	}
}
