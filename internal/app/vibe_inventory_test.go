package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestVibeInventoryDoesNotExecuteTheBinary is the version-inventory boundary.
// Startup, a cold menu open, GET /api/agents, and an ordinary refresh may see
// vibe-acp on PATH. None of them may start it. Another installed harness still
// receives its --version check. This fake Vibe has no adjacent package metadata,
// so its installed version stays unknown.
func TestVibeInventoryDoesNotExecuteTheBinary(t *testing.T) {
	binDir, vibeRecord := vibeLaunchRecorder(t)
	grokRecord := filepath.Join(binDir, "grok-invocations.txt")
	writeScript(t, binDir, "grok", `
printf '%s\n' "$*" >> `+quoteForScript(grokRecord)+`
if [ "$1" = "--version" ]; then printf '%s\n' 'grok 1.0.24'; exit 0; fi
if [ "$1" = "models" ]; then printf '%s\n' 'Default model: grok-4.6' '* grok-4.6 (default)'; exit 0; fi
exit 0
`)
	t.Setenv("PATH", binDir)
	resetAgentsCache(t)
	resetHarnessInventoryCache(t)

	t.Run("startup", func(t *testing.T) {
		clearLaunches(t, vibeRecord)
		clearLaunches(t, grokRecord)
		rows := harnessInventory()
		if got := recordedLaunches(t, vibeRecord); got != 0 {
			t.Fatalf("startup inventory executed vibe-acp %d times\n%s", got, readInvocationRecord(t, vibeRecord))
		}
		if !invocationHas(t, grokRecord, "--version") {
			t.Fatalf("startup inventory did not version-check grok\n%s", readInvocationRecord(t, grokRecord))
		}
		assertVibeVersionUnknown(t, rows)
		if got := rowByAgent(rows, "grok"); got.Installed != "1.0.24" || !got.Present {
			t.Fatalf("grok inventory = %+v, want the parsed --version", got)
		}
	})

	t.Run("menu open", func(t *testing.T) {
		clearLaunches(t, vibeRecord)
		clearLaunches(t, grokRecord)
		resetHarnessInventoryCache(t)
		a := newTestApp(t, &fakeTmux{})
		rec := httptest.NewRecorder()
		a.handleHarnesses(rec, httptest.NewRequest(http.MethodGet, "/api/harnesses", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/harnesses = %d %s", rec.Code, rec.Body.String())
		}
		if got := recordedLaunches(t, vibeRecord); got != 0 {
			t.Fatalf("menu open executed vibe-acp %d times\n%s", got, readInvocationRecord(t, vibeRecord))
		}
		if !invocationHas(t, grokRecord, "--version") {
			t.Fatalf("menu open did not version-check grok\n%s", readInvocationRecord(t, grokRecord))
		}
	})

	t.Run("agents", func(t *testing.T) {
		clearLaunches(t, vibeRecord)
		resetAgentsCache(t)
		a := newTestApp(t, &fakeTmux{})
		rec := httptest.NewRecorder()
		a.handleAgents(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/agents = %d %s", rec.Code, rec.Body.String())
		}
		if got := recordedLaunches(t, vibeRecord); got != 0 {
			t.Fatalf("GET /api/agents executed vibe-acp %d times\n%s", got, readInvocationRecord(t, vibeRecord))
		}
		var offered map[string]agentInfo
		if err := json.Unmarshal(rec.Body.Bytes(), &offered); err != nil {
			t.Fatal(err)
		}
		if len(offered["vibe"].Models) != 0 || offered["vibe"].Efforts != nil {
			t.Fatalf("dialog catalog = %+v", offered["vibe"])
		}
	})

	t.Run("ordinary refresh", func(t *testing.T) {
		clearLaunches(t, vibeRecord)
		clearLaunches(t, grokRecord)
		a := newTestApp(t, &fakeTmux{})
		a.harnessLatestSources = func() map[string]harnessSource { return map[string]harnessSource{} }
		rec := httptest.NewRecorder()
		a.handleHarnessLatest(rec, httptest.NewRequest(http.MethodGet, "/api/harnesses/latest", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET refresh = %d %s", rec.Code, rec.Body.String())
		}
		if got := recordedLaunches(t, vibeRecord); got != 0 {
			t.Fatalf("ordinary refresh executed vibe-acp %d times\n%s", got, readInvocationRecord(t, vibeRecord))
		}
		if !invocationHas(t, grokRecord, "--version") {
			t.Fatalf("ordinary refresh did not version-check grok\n%s", readInvocationRecord(t, grokRecord))
		}
		agents := decodeAgents(t, rec)
		if len(agents["vibe"].Models) != 0 || agents["vibe"].Efforts != nil {
			t.Fatalf("ordinary refresh vibe = %+v", agents["vibe"])
		}
		var body struct {
			Harnesses []harnessRow `json:"harnesses"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		assertVibeVersionUnknown(t, body.Harnesses)
		unchecked := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/harnesses/latest", bytes.NewBufferString(`{"inspect_vibe":false}`))
		req.Header.Set("Content-Type", "application/json")
		clearLaunches(t, vibeRecord)
		a.handleHarnessLatest(unchecked, req)
		if unchecked.Code != http.StatusOK {
			t.Fatalf("unchecked POST = %d %s", unchecked.Code, unchecked.Body.String())
		}
		if got := recordedLaunches(t, vibeRecord); got != 0 {
			t.Fatalf("unchecked update executed vibe-acp %d times\n%s", got, readInvocationRecord(t, vibeRecord))
		}
	})
}

func quoteForScript(path string) string {
	return "'" + filepath.Clean(path) + "'"
}

func readInvocationRecord(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func invocationHas(t *testing.T, path, arg string) bool {
	t.Helper()
	for _, line := range bytes.Split([]byte(readInvocationRecord(t, path)), []byte("\n")) {
		if bytes.Contains(line, []byte(arg)) {
			return true
		}
	}
	return false
}

func rowByAgent(rows []harnessRow, agent string) harnessRow {
	for _, row := range rows {
		if row.Agent == agent {
			return row
		}
	}
	return harnessRow{}
}

func assertVibeVersionUnknown(t *testing.T, rows []harnessRow) {
	t.Helper()
	got := rowByAgent(rows, "vibe")
	if !got.Present || !got.Launchable || got.Installed != "" || !got.HasSource || got.Path == "" {
		t.Fatalf("vibe inventory = %+v, want present and launchable with an unknown installed version and public update source", got)
	}
}
