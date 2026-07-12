package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentCommandPiOpencode(t *testing.T) {
	pi := &Node{Agent: "pi", Model: "mistral/devstral-latest", Prompt: "hello"}
	if got, _ := agentCommand(pi); got != `pi --model 'mistral/devstral-latest' 'hello'` {
		t.Errorf("pi cmd = %s", got)
	}
	// opencode takes the first prompt as a flag, not a positional argument.
	oc := &Node{Agent: "opencode", Model: "openai/gpt-5.5", Prompt: "hello"}
	if got, _ := agentCommand(oc); got != `opencode --model 'openai/gpt-5.5' --prompt 'hello'` {
		t.Errorf("opencode cmd = %s", got)
	}
	bare := &Node{Agent: "pi", Prompt: "p"}
	if got, _ := agentCommand(bare); got != `pi 'p'` {
		t.Errorf("bare pi cmd = %s", got)
	}
}

func TestPiModelsParse(t *testing.T) {
	// Header is skipped by name; provider and id join with "/" — the form
	// `pi --model` accepts back.
	out := "provider                model     context\nmistral   devstral-latest   262K\n\nanthropic  claude-fable-5  1M\n"
	var models []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] == "provider" {
			continue
		}
		models = append(models, f[0]+"/"+f[1])
	}
	want := []string{"mistral/devstral-latest", "anthropic/claude-fable-5"}
	if len(models) != len(want) || models[0] != want[0] || models[1] != want[1] {
		t.Errorf("parsed %v, want %v", models, want)
	}
}

func TestHandleUIRoundTrip(t *testing.T) {
	a := &app{uiPath: filepath.Join(t.TempDir(), "ui.json")}
	put := func(body, ifMatch string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/api/ui", strings.NewReader(body))
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		a.handleUIPut(rec, req)
		return rec
	}

	// A fresh install has no file: GET must serve an empty object, not 404,
	// and carry the revision tag every write must name.
	rec := httptest.NewRecorder()
	a.handleUIGet(rec, httptest.NewRequest("GET", "/api/ui", nil))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Errorf("empty GET = %d %q", rec.Code, rec.Body.String())
	}
	rev := rec.Header().Get("ETag")
	if rev == "" {
		t.Fatal("GET carries no ETag")
	}

	// Writes without a base revision are refused outright.
	if rec := put(`{}`, ""); rec.Code != http.StatusPreconditionRequired {
		t.Errorf("PUT without If-Match = %d, want 428", rec.Code)
	}

	body := `{"groups":[{"id":"g1","name":"automotive","lanes":["a"]}],"archived":["x"],"notes":[]}`
	if rec := put(body, rev); rec.Code != 200 {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	a.handleUIGet(rec, httptest.NewRequest("GET", "/api/ui", nil))
	if rec.Body.String() != body {
		t.Errorf("GET after PUT = %q, want %q", rec.Body.String(), body)
	}
	rev2 := rec.Header().Get("ETag")

	// An unchanged revision polls for free.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/ui", nil)
	req.Header.Set("If-None-Match", rev2)
	a.handleUIGet(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", rec.Code)
	}

	// A write based on a superseded revision must conflict, not overwrite.
	if rec := put(`{"notes":["stale"]}`, rev); rec.Code != http.StatusConflict {
		t.Errorf("stale PUT = %d, want 409", rec.Code)
	}
	// The wildcard bootstrap always wins (single supervisor, explicit intent).
	if rec := put(body, "*"); rec.Code != 200 {
		t.Errorf("wildcard PUT = %d", rec.Code)
	}

	// Invalid JSON must be rejected, and must not clobber the stored state.
	if rec := put("{broken", rev2); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid PUT = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.handleUIGet(rec, httptest.NewRequest("GET", "/api/ui", nil))
	if rec.Body.String() != body {
		t.Errorf("state clobbered by rejected PUT: %q", rec.Body.String())
	}

	// Oversized blobs are refused before touching the file.
	huge := `{"notes":["` + strings.Repeat("x", uiStateMax) + `"]}`
	if rec := put(huge, rev2); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized PUT = %d, want 413", rec.Code)
	}
}

// detectAgents shells out to whatever harnesses are installed; keep it out
// of -short runs but exercise the real probes when dogfooding.
func TestDetectAgents(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping harness probes in -short mode")
	}
	agents := detectAgents()
	for name, models := range agents {
		if models == nil {
			t.Errorf("agent %q has nil model list", name)
		}
		t.Logf("%s: %d models", name, len(models))
	}
}
