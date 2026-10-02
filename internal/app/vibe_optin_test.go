package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/acp"
)

func resetHarnessInventoryCache(t *testing.T) {
	t.Helper()
	harnessInventoryMu.Lock()
	loaded, cache, gen := harnessInventoryLoaded, harnessInventoryCache, harnessInventoryGeneration
	harnessInventoryLoaded = false
	harnessInventoryCache = nil
	harnessInventoryMu.Unlock()
	t.Cleanup(func() {
		harnessInventoryMu.Lock()
		harnessInventoryLoaded, harnessInventoryCache, harnessInventoryGeneration = loaded, cache, gen
		harnessInventoryMu.Unlock()
	})
}

func resetAgentsCache(t *testing.T) {
	t.Helper()
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
}

// vibeLaunchRecorder is a PATH stub. Every start is one line, including a
// version check. A catalog probe has no arguments; `--version` is still an
// invocation and must not disappear from the record.
func vibeLaunchRecorder(t *testing.T) (binDir, record string) {
	t.Helper()
	binDir = t.TempDir()
	record = filepath.Join(binDir, "launches.txt")
	writeScript(t, binDir, "vibe-acp", fmt.Sprintf(`printf 'invoke %%s\n' "$*" >> %q
if [ "$1" = "--version" ]; then
  printf '%%s\n' 'vibe-acp 2.25.0'
  exit 0
fi
exit 0
`, record))
	return binDir, record
}

func recordedLaunches(t *testing.T, record string) int {
	t.Helper()
	b, err := os.ReadFile(record)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			n++
		}
	}
	return n
}

func clearLaunches(t *testing.T, record string) {
	t.Helper()
	if err := os.Remove(record); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func vibeOptInHandler(t *testing.T) http.Handler {
	t.Helper()
	resetAgentsCache(t)
	a := newTestApp(t, &fakeTmux{})
	h := newTestHandler(t, a)
	// newTestHandler substitutes an empty catalog so generic route tests never
	// execute a developer's CLIs. These tests are the discovery path.
	a.agentCatalog = nil
	return h
}

func decodeAgents(t *testing.T, rec *httptest.ResponseRecorder) map[string]agentInfo {
	t.Helper()
	var got struct {
		Agents map[string]agentInfo `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode agents: %v body=%s", err, rec.Body.String())
	}
	return got.Agents
}

func TestVibeStartupAndOrdinaryRefreshDoNotLaunchTheCatalogProbe(t *testing.T) {
	binDir, record := vibeLaunchRecorder(t)
	writeScript(t, binDir, "grok", `
if [ "$1" = "--version" ]; then printf '%s\n' 'grok 1.0.24'; exit; fi
if [ "$1" = "models" ]; then printf '%s\n' 'Default model: grok-4.6' '* grok-4.6 (default)'; fi`)
	t.Setenv("PATH", binDir)
	resetAgentsCache(t)

	first := detectAgents()
	second := detectAgents()
	if recordedLaunches(t, record) != 0 {
		t.Fatalf("startup launched vibe-acp %d times", recordedLaunches(t, record))
	}
	info, ok := first["vibe"]
	if !ok || len(info.Models) != 0 || info.Efforts != nil {
		t.Fatalf("startup vibe = %+v, want an empty model and thinking catalog", info)
	}
	if !reflect.DeepEqual(second["vibe"], first["vibe"]) {
		t.Fatalf("second startup snapshot = %+v", second["vibe"])
	}

	clearLaunches(t, record)
	a := newTestApp(t, &fakeTmux{})
	rec := httptest.NewRecorder()
	a.handleAgents(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/agents = %d %s", rec.Code, rec.Body.String())
	}
	var offered map[string]agentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &offered); err != nil {
		t.Fatal(err)
	}
	if recordedLaunches(t, record) != 0 {
		t.Fatalf("GET /api/agents launched vibe-acp %d times", recordedLaunches(t, record))
	}
	if len(offered["vibe"].Models) != 0 || offered["vibe"].Efforts != nil {
		t.Fatalf("dialog catalog = %+v", offered["vibe"])
	}

	clearLaunches(t, record)
	h := vibeOptInHandler(t)
	// vibeOptInHandler replaced PATH's cache reset; the recorder is still on PATH.
	t.Setenv("PATH", binDir)
	rec = routeRequest(h, http.MethodGet, "/api/harnesses/latest", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET refresh = %d %s", rec.Code, rec.Body.String())
	}
	agents := decodeAgents(t, rec)
	if recordedLaunches(t, record) != 0 {
		t.Fatalf("ordinary refresh launched vibe-acp %d times", recordedLaunches(t, record))
	}
	if len(agents["vibe"].Models) != 0 || agents["vibe"].Efforts != nil {
		t.Fatalf("ordinary refresh vibe = %+v", agents["vibe"])
	}
	if !reflect.DeepEqual(agents["grok"].Models, []string{"grok-4.6"}) {
		t.Fatalf("ordinary refresh grok = %v, want the read-only catalog", agents["grok"].Models)
	}

	clearLaunches(t, record)
	rec = routeRequest(h, http.MethodGet, "/api/harnesses/latest?inspect_vibe=true", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET with a query signal = %d", rec.Code)
	}
	if recordedLaunches(t, record) != 0 {
		t.Fatalf("GET query launched vibe-acp %d times", recordedLaunches(t, record))
	}
}

func TestVibeAbsentBinaryStaysAbsent(t *testing.T) {
	binDir, record := vibeLaunchRecorder(t)
	// The recorder directory contains vibe-acp. Point PATH somewhere else.
	t.Setenv("PATH", t.TempDir())
	_ = binDir
	resetAgentsCache(t)
	agents := probeAgents(harnesses)
	if _, ok := agents["vibe"]; ok {
		t.Fatal("vibe was offered without vibe-acp on PATH")
	}
	if recordedLaunches(t, record) != 0 {
		t.Fatal("an absent binary was launched")
	}
	h := vibeOptInHandler(t)
	t.Setenv("PATH", t.TempDir())
	rec := routeRequest(h, http.MethodPost, "/api/harnesses/latest", `{"inspect_vibe":true}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("opt-in without a binary = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := decodeAgents(t, rec)["vibe"]; ok {
		t.Fatal("opt-in invented a vibe row for a missing binary")
	}
	if recordedLaunches(t, record) != 0 {
		t.Fatal("opt-in launched a binary that is not on PATH")
	}
}

func TestVibeCheckedUpdateLaunchesTheBoundedProbeOnce(t *testing.T) {
	if reflect.ValueOf(probeVibeCatalog).Pointer() != reflect.ValueOf(acp.ProbeVibeCatalog).Pointer() {
		t.Fatal("catalog discovery is not the bounded ACP probe")
	}
	binDir, record := vibeLaunchRecorder(t)
	t.Setenv("PATH", binDir)
	h := vibeOptInHandler(t)
	t.Setenv("PATH", binDir)
	rec := routeRequest(h, http.MethodPost, "/api/harnesses/latest", `{"inspect_vibe":true}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("checked update = %d %s", rec.Code, rec.Body.String())
	}
	if recordedLaunches(t, record) != 1 {
		t.Fatalf("checked update launches = %d, want the one bounded probe", recordedLaunches(t, record))
	}
	info, ok := decodeAgents(t, rec)["vibe"]
	if !ok || len(info.Models) != 0 || info.Efforts != nil {
		t.Fatalf("failed probe catalog = %+v, want the launchable default", info)
	}
	for _, name := range info.Models {
		if name == "default" || name == "(default)" {
			t.Fatalf("probe failure invented model %q", name)
		}
	}
}

type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, errors.New("body unreadable") }
func (errReadCloser) Close() error             { return nil }

func TestVibeCatalogProbeRunsOnlyForAnExplicitInspectPost(t *testing.T) {
	binDir, record := vibeLaunchRecorder(t)
	t.Setenv("PATH", binDir)
	resetAgentsCache(t)
	var calls atomic.Int32
	var sawBin atomic.Bool
	var bounded atomic.Bool
	prev := probeVibeCatalog
	probeVibeCatalog = func(ctx context.Context, bin string) (acp.VibeCatalog, error) {
		calls.Add(1)
		if filepath.Base(bin) == "vibe-acp" {
			sawBin.Store(true)
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) > 0 && time.Until(dl) <= 16*time.Second {
			bounded.Store(true)
		}
		return acp.VibeCatalog{
			Models:  []string{"alpha", "beta"},
			Efforts: map[string][]string{"alpha": {"lvl-high"}},
		}, nil
	}
	t.Cleanup(func() { probeVibeCatalog = prev })

	h := vibeOptInHandler(t)
	t.Setenv("PATH", binDir)

	rec := routeRequest(h, http.MethodPost, "/api/harnesses/latest", `{"inspect_vibe":true}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("checked update = %d %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 1 || !sawBin.Load() || !bounded.Load() {
		t.Fatalf("probe calls=%d bin=%v bounded=%v", calls.Load(), sawBin.Load(), bounded.Load())
	}
	if recordedLaunches(t, record) != 0 {
		t.Fatalf("the stubbed probe still launched vibe-acp %d times", recordedLaunches(t, record))
	}
	info := decodeAgents(t, rec)["vibe"]
	if !reflect.DeepEqual(info.Models, []string{"alpha", "beta"}) {
		t.Fatalf("models = %v", info.Models)
	}
	got := info.Efforts["alpha"]
	if got.Default != "" || got.Required || !reflect.DeepEqual(got.Levels, []string{"lvl-high"}) {
		t.Fatalf("alpha thinking = %+v", got)
	}
	if _, ok := info.Efforts["beta"]; ok {
		t.Fatal("a model without a snapshot inherited thinking levels")
	}

	before := calls.Load()
	agentsRec := httptest.NewRecorder()
	newTestApp(t, &fakeTmux{}).handleAgents(agentsRec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	// The handler above built a fresh app whose catalog seam is empty. Read
	// the published snapshot through detectAgents, which is what a nil seam serves.
	offered := detectAgents()
	if calls.Load() != before {
		t.Fatalf("GET /api/agents after opt-in probed again: %d", calls.Load()-before)
	}
	if !reflect.DeepEqual(offered["vibe"].Models, []string{"alpha", "beta"}) {
		t.Fatalf("cached dialog = %+v", offered["vibe"])
	}
	_ = agentsRec

	rec = routeRequest(h, http.MethodGet, "/api/harnesses/latest", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("later ordinary refresh = %d", rec.Code)
	}
	if calls.Load() != before {
		t.Fatalf("ordinary refresh probed vibe: calls %d want %d", calls.Load(), before)
	}
	if len(decodeAgents(t, rec)["vibe"].Models) != 0 {
		t.Fatalf("ordinary refresh kept an opted-in catalog: %+v", decodeAgents(t, rec)["vibe"])
	}

	silent := []struct {
		name, method, path, body, contentType string
		csrf                                  bool
		status                                int
	}{
		{"unchecked", http.MethodPost, "/api/harnesses/latest", `{"inspect_vibe":false}`, "application/json", true, http.StatusOK},
		{"missing field", http.MethodPost, "/api/harnesses/latest", `{}`, "application/json", true, http.StatusOK},
		{"null field", http.MethodPost, "/api/harnesses/latest", `{"inspect_vibe":null}`, "application/json", true, http.StatusOK},
		{"wrong name", http.MethodPost, "/api/harnesses/latest", `{"InspectVibe":true}`, "application/json", true, http.StatusOK},
		{"case variant", http.MethodPost, "/api/harnesses/latest", `{"INSPECT_VIBE":true}`, "application/json", true, http.StatusOK},
		{"oversized", http.MethodPost, "/api/harnesses/latest", `{"inspect_vibe":true,"padding":"` + strings.Repeat("x", 1<<20) + `"}`, "application/json", true, http.StatusRequestEntityTooLarge},
		{"empty body", http.MethodPost, "/api/harnesses/latest", ``, "", true, http.StatusOK},
		{"whitespace", http.MethodPost, "/api/harnesses/latest", " \n", "application/json", true, http.StatusOK},
		{"malformed", http.MethodPost, "/api/harnesses/latest", `{`, "application/json", true, http.StatusBadRequest},
		{"string", http.MethodPost, "/api/harnesses/latest", `{"inspect_vibe":"true"}`, "application/json", true, http.StatusBadRequest},
		{"number", http.MethodPost, "/api/harnesses/latest", `{"inspect_vibe":1}`, "application/json", true, http.StatusBadRequest},
		{"array", http.MethodPost, "/api/harnesses/latest", `[]`, "application/json", true, http.StatusBadRequest},
		{"no csrf", http.MethodPost, "/api/harnesses/latest", `{"inspect_vibe":true}`, "application/json", false, http.StatusForbidden},
		{"form body", http.MethodPost, "/api/harnesses/latest", `inspect_vibe=true`, "application/x-www-form-urlencoded", true, http.StatusUnsupportedMediaType},
		{"get", http.MethodGet, "/api/harnesses/latest", `{"inspect_vibe":true}`, "application/json", false, http.StatusOK},
		{"get query", http.MethodGet, "/api/harnesses/latest?inspect_vibe=true", "", "", false, http.StatusOK},
	}
	for _, tc := range silent {
		t.Run(tc.name, func(t *testing.T) {
			mark := calls.Load()
			req := httptest.NewRequest(tc.method, "http://127.0.0.1:8787"+tc.path, strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			if tc.csrf {
				req.Header.Set("X-Scimux-CSRF", csrfToken)
			}
			got := httptest.NewRecorder()
			h.ServeHTTP(got, req)
			if got.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", got.Code, tc.status, got.Body.String())
			}
			if calls.Load() != mark {
				t.Fatalf("probe calls changed by %d", calls.Load()-mark)
			}
		})
	}

	t.Run("unreadable body", func(t *testing.T) {
		mark := calls.Load()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/harnesses/latest", nil)
		req.Body = errReadCloser{}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scimux-CSRF", csrfToken)
		got := httptest.NewRecorder()
		h.ServeHTTP(got, req)
		if got.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", got.Code, got.Body.String())
		}
		if calls.Load() != mark {
			t.Fatal("an unreadable body ran the probe")
		}
	})
}

func TestVibeInspectFailureLeavesTheDefaultAndInventsNothing(t *testing.T) {
	binDir, record := vibeLaunchRecorder(t)
	t.Setenv("PATH", binDir)
	cases := []struct {
		name string
		fn   func(context.Context, string) (acp.VibeCatalog, error)
	}{
		{"error", func(context.Context, string) (acp.VibeCatalog, error) {
			return acp.VibeCatalog{Models: []string{"invented"}}, errors.New("discovery failed")
		}},
		{"empty", func(context.Context, string) (acp.VibeCatalog, error) {
			return acp.VibeCatalog{}, nil
		}},
		{"timeout", func(ctx context.Context, _ string) (acp.VibeCatalog, error) {
			return acp.VibeCatalog{Models: []string{"late-model"}}, context.DeadlineExceeded
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAgentsCache(t)
			clearLaunches(t, record)
			prev := probeVibeCatalog
			var calls atomic.Int32
			probeVibeCatalog = func(ctx context.Context, bin string) (acp.VibeCatalog, error) {
				calls.Add(1)
				return tc.fn(ctx, bin)
			}
			t.Cleanup(func() { probeVibeCatalog = prev })
			h := vibeOptInHandler(t)
			t.Setenv("PATH", binDir)
			rec := routeRequest(h, http.MethodPost, "/api/harnesses/latest", `{"inspect_vibe":true}`, true)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatalf("probe calls = %d, want 1", calls.Load())
			}
			if recordedLaunches(t, record) != 0 {
				t.Fatal("failure path launched the real binary")
			}
			info, ok := decodeAgents(t, rec)["vibe"]
			if !ok || len(info.Models) != 0 || info.Efforts != nil {
				t.Fatalf("catalog = %+v, want the launchable default", info)
			}
		})
	}
}

func TestVibeSessionCreationFailureStaysOnTheLaunchErrorPath(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	cause := errors.New("acp new session: authentication is unusable")
	proc := &countingProc{launchErr: cause}
	n := &Node{
		ID: "vibe-broken", Title: "Vibe", Prompt: "hi", Agent: "vibe",
		Dir: a.home, CreatedAt: "2026-10-01T00:00:00Z",
	}
	status, err := a.launchNode(n, proc)
	if status != http.StatusInternalServerError || err == nil || !strings.Contains(err.Error(), "authentication is unusable") {
		t.Fatalf("status=%d err=%v, want the agent's failure on the existing 500 path", status, err)
	}
	if n.SessionID != "" {
		t.Fatalf("failed create stored session %q", n.SessionID)
	}
	for _, r := range keyRecords(t, a.storePath) {
		if r.Type == "node" {
			t.Fatalf("failed create persisted a node: %+v", r.Node)
		}
	}
	if proc.launches != 1 {
		t.Fatalf("launches = %d, want the one attempt", proc.launches)
	}
}
