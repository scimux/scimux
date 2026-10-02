package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/acp"
)

func TestVibeResolvesToACPWhenNamed(t *testing.T) {
	dir := t.TempDir()
	a := &app{home: dir, byID: map[string]*Node{}}
	n := Node{Title: "T", Prompt: "p", Agent: "vibe", Dir: dir}
	status, err := a.resolveNode(&n)
	if err != nil {
		t.Fatalf("resolveNode(vibe) = %d %v", status, err)
	}
	if n.Transport != "acp" {
		t.Fatalf("transport = %q, want acp", n.Transport)
	}
}

func TestVibeStaysOutOfTheDialogWhenTheBinaryIsAbsent(t *testing.T) {
	binDir := t.TempDir()
	writeScript(t, binDir, "dsh", `exit 0`)
	t.Setenv("PATH", binDir)
	agents := probeAgents(harnesses)
	if _, ok := agents["vibe"]; ok {
		t.Fatal("vibe was offered without vibe-acp on PATH")
	}
}

func TestVibeIsDiscoverableWhenTheBinaryIsPresent(t *testing.T) {
	binDir := t.TempDir()
	writeScript(t, binDir, "vibe-acp", `exit 0`)
	t.Setenv("PATH", binDir)
	agents := probeAgents(harnesses)
	info, ok := agents["vibe"]
	if !ok {
		t.Fatal("installed vibe-acp was not offered as vibe")
	}
	if len(info.Models) != 0 || info.Efforts != nil {
		t.Fatalf("vibe catalog = %+v, want an empty launchable default", info)
	}
	if _, ok := agents["vibe-acp"]; ok {
		t.Fatal("vibe was published under the binary name")
	}
}

func TestVibeHasNoTmuxLaunchLine(t *testing.T) {
	got, err := agentCommand(&Node{Agent: "vibe", Model: "synthetic-model", Effort: "high", Prompt: "hello"}, nil)
	if err == nil {
		t.Fatalf("agentCommand(vibe) = %q, want a structured-only refusal", got)
	}
	if got != "" {
		t.Fatalf("agentCommand(vibe) produced a command %q", got)
	}
}

func TestVibeForkInheritsLaunchConfigOnly(t *testing.T) {
	dir := t.TempDir()
	a := &app{home: dir, byID: map[string]*Node{
		"parent": {
			ID: "parent", Agent: "vibe", Model: "synthetic-model", Effort: "high",
			Dir: dir, LaneID: "lane-v", Transport: "acp",
			SessionID: "sess-parent", Transcript: filepath.Join(dir, "not-a-transcript.jsonl"),
			EndedAt: "2026-01-01T00:00:00Z", Adopted: true,
			Rationale: "why", CreatedAt: "2026-01-01T00:00:00Z",
			Prompt: "parent prompt", Description: "parent desc", Title: "Parent",
		},
	}}
	child := Node{Title: "Child", Prompt: "fresh", Parent: "parent"}
	if status, err := a.resolveNode(&child); err != nil {
		t.Fatalf("fork: %d %v", status, err)
	}
	if child.Agent != "vibe" || child.Model != "synthetic-model" || child.Effort != "high" || child.Transport != "acp" || child.Dir != dir {
		t.Fatalf("inherited launch = agent %q model %q effort %q transport %q dir %q",
			child.Agent, child.Model, child.Effort, child.Transport, child.Dir)
	}
	if child.SessionID != "" || child.Transcript != "" || child.EndedAt != "" || child.Adopted {
		t.Fatalf("fork copied conversation identity: session=%q transcript=%q ended=%q adopted=%v",
			child.SessionID, child.Transcript, child.EndedAt, child.Adopted)
	}
	if child.Prompt != "fresh" || child.Title != "Child" || child.Rationale != "" {
		t.Fatalf("fork overwrote its own prompt or copied rationale: %+v", child)
	}
}

func TestVibeWorkerRecoveryReconnectsOnACP(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	data := filepath.Dir(a.storePath)
	first := syntheticWorkerManager(t, data)
	createdAt := time.Now().UTC().Format(time.RFC3339)
	n := &Node{
		ID: "vibe-recover", Title: "Vibe", Prompt: "hi", Agent: "vibe",
		Dir: a.home, CreatedAt: createdAt,
	}
	if _, err := first.LaunchNode(n, ""); err != nil {
		t.Fatal(err)
	}
	first.Detach()
	a = reloadApp(t, a, f)
	if _, ok := a.byID[n.ID]; ok {
		t.Fatal("unpublished vibe worker was already a node before recovery")
	}
	replacement := syntheticWorkerManager(t, data)
	if err := replacement.RecoverUnknown(a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replacement.Shutdown)
	got := a.byID[n.ID]
	if got == nil || got.Agent != "vibe" || got.Transport != "acp" || got.SessionID == "" {
		t.Fatalf("recovered vibe = %#v", got)
	}
	if !replacement.manages(n.ID) {
		t.Fatal("recovered vibe worker was not reattached")
	}
	if err := replacement.Kill(n.ID); err != nil {
		t.Fatal(err)
	}
	if replacement.manages(n.ID) {
		t.Fatal("deleted vibe node left its worker attached")
	}
}

func TestVibeCatalogIsCachedAndChatPollDoesNotProbe(t *testing.T) {
	agentsCacheMu.Lock()
	loaded, cache, gen := agentsLoaded, agentsCache, agentsGeneration
	agentsLoaded = false
	agentsCache = nil
	agentsCacheMu.Unlock()
	t.Cleanup(func() {
		agentsCacheMu.Lock()
		agentsLoaded, agentsCache, agentsGeneration = loaded, cache, gen
		agentsCacheMu.Unlock()
	})
	prev := probeVibeCatalog
	var calls atomic.Int32
	probeVibeCatalog = func(context.Context, string) (acp.VibeCatalog, error) {
		calls.Add(1)
		return acp.VibeCatalog{
			Models:  []string{"alpha", "beta"},
			Efforts: map[string][]string{"alpha": {"lvl-high"}},
		}, nil
	}
	t.Cleanup(func() { probeVibeCatalog = prev })

	binDir := t.TempDir()
	writeScript(t, binDir, "vibe-acp", `exit 0`)
	t.Setenv("PATH", binDir)

	a := newTestApp(t, &fakeTmux{})
	rec := httptest.NewRecorder()
	a.handleState(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if calls.Load() != 0 {
		t.Fatalf("chat poll probed vibe %d times", calls.Load())
	}

	first := detectAgents()
	second := detectAgents()
	if calls.Load() != 0 {
		t.Fatalf("detectAgents calls = %d, want no catalog probe", calls.Load())
	}
	if _, ok := first["vibe"]; !ok || len(first["vibe"].Models) != 0 || first["vibe"].Efforts != nil {
		t.Fatalf("startup catalog = %+v, want an installed vibe with no models", first["vibe"])
	}
	if !reflect.DeepEqual(second["vibe"], first["vibe"]) {
		t.Fatalf("cached catalog = %+v / %+v", first["vibe"], second["vibe"])
	}
}

func TestVibeCatalogFailureStaysLaunchableWithNoInventedModels(t *testing.T) {
	prev := probeVibeCatalog
	var calls atomic.Int32
	probeVibeCatalog = func(context.Context, string) (acp.VibeCatalog, error) {
		calls.Add(1)
		return acp.VibeCatalog{Models: []string{"invented-on-error"}}, errors.New("discovery would spend quota")
	}
	t.Cleanup(func() { probeVibeCatalog = prev })
	binDir := t.TempDir()
	writeScript(t, binDir, "vibe-acp", `exit 0`)
	t.Setenv("PATH", binDir)
	agents := probeAgents(harnesses)
	if calls.Load() != 0 {
		t.Fatalf("ordinary discovery probed vibe %d times", calls.Load())
	}
	info, ok := agents["vibe"]
	if !ok {
		t.Fatal("a missing catalog removed the installed harness")
	}
	if len(info.Models) != 0 || info.Efforts != nil {
		t.Fatalf("ordinary catalog = %+v, want an empty launchable default", info)
	}
}
