package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// The claude CLI mis-resolves its own family aliases (opus -> claude-4-8-opus,
// wrong segment order), so scimux probes the concrete ids and maps them itself.
// Parsing must be defensive: pick model-id-shaped tokens out of arbitrary prose
// or markdown, first per family wins, and the mis-ordered form must be ignored.
func TestParseClaudeModels(t *testing.T) {
	out := "Based on the model IDs available in this environment:\n\n```\n" +
		"claude-fable-5\nclaude-opus-4-8\nclaude-sonnet-5\nclaude-haiku-4-5\n```\n" +
		"You could also try claude-4-8-opus (a bogus, mis-ordered id) — ignore it.\n"
	got := parseClaudeModels(out)
	want := map[string]string{
		"fable":  "claude-fable-5",
		"opus":   "claude-opus-4-8",
		"sonnet": "claude-sonnet-5",
		"haiku":  "claude-haiku-4-5",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for fam, id := range want {
		if got[fam] != id {
			t.Errorf("family %q = %q, want %q", fam, got[fam], id)
		}
	}
	// A first-seen id wins; a later dated duplicate for the same family is ignored.
	dup := parseClaudeModels("claude-opus-4-8\nclaude-opus-4-8-20260101\n")
	if dup["opus"] != "claude-opus-4-8" {
		t.Errorf("first-seen id must win, got %q", dup["opus"])
	}
	// No model-id tokens at all -> empty map (caller falls back to the alias).
	if m := parseClaudeModels("I'm not sure which models you have."); len(m) != 0 {
		t.Errorf("no ids should yield empty map, got %v", m)
	}

	// A numbered list with backticks and trailing prose is still parsed cleanly.
	numbered := parseClaudeModels("1. `claude-opus-4-8` — your top model\n" +
		"2. `claude-sonnet-5` (fast)\n3. `claude-haiku-4-5`\n")
	if numbered["opus"] != "claude-opus-4-8" || numbered["sonnet"] != "claude-sonnet-5" || numbered["haiku"] != "claude-haiku-4-5" {
		t.Errorf("numbered/backticked list = %v", numbered)
	}

	// A date-stamped id is a valid concrete id and must be captured whole.
	if d := parseClaudeModels("claude-sonnet-5-20260101"); d["sonnet"] != "claude-sonnet-5-20260101" {
		t.Errorf("dated id truncated: %v", d)
	}
}

// A successful probe is cached for claudeCacheTTL so the billed `claude -p` call
// runs at most weekly; a missing or stale cache is not "fresh" (the caller
// re-probes), and a stale cache still carries usable ids as a fallback.
func TestClaudeCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-models.json")

	// Missing file: zero cache — no ids, not fresh.
	if c := readClaudeCache(path); len(c.IDs) != 0 || !c.ProbedAt.IsZero() {
		t.Errorf("missing cache = %+v, want zero", c)
	}

	ids := map[string]string{"opus": "claude-opus-4-8", "sonnet": "claude-sonnet-5"}
	if err := writeClaudeCache(path, ids); err != nil {
		t.Fatal(err)
	}
	got := readClaudeCache(path)
	if got.IDs["opus"] != "claude-opus-4-8" || got.IDs["sonnet"] != "claude-sonnet-5" {
		t.Errorf("round-trip ids = %v", got.IDs)
	}
	// A just-written cache is fresh.
	if time.Since(got.ProbedAt) >= claudeCacheTTL {
		t.Errorf("fresh cache read as stale: probed_at %v", got.ProbedAt)
	}

	// Hand-write a stale cache (8 days old): ids still present, but past the TTL.
	stale := claudeCache{ProbedAt: time.Now().Add(-8 * 24 * time.Hour).UTC(), IDs: ids}
	b, _ := json.Marshal(stale)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	sc := readClaudeCache(path)
	if len(sc.IDs) == 0 {
		t.Error("stale cache dropped its ids")
	}
	if time.Since(sc.ProbedAt) < claudeCacheTTL {
		t.Error("8-day-old cache should be past the 7-day TTL")
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
