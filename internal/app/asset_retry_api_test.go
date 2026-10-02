package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/sessionworker"
)

// legacyAssetWorkerHarness reproduces the pre-backingId worker's real JSON
// boundary: unknown asset fields are discarded before the event is appended.
type legacyAssetWorkerHarness struct {
	syntheticSessionHarness
	path string
}

func (h *legacyAssetWorkerHarness) AppendSessionEvent(_ context.Context, event sessionlog.Event) error {
	type legacyAsset struct {
		ID               string `json:"id"`
		Name             string `json:"name,omitempty"`
		Mime             string `json:"mime,omitempty"`
		Size             int64  `json:"size,omitempty"`
		SHA256           string `json:"sha256,omitempty"`
		Storage          string `json:"storage"`
		Bytes            string `json:"bytes,omitempty"`
		BlobPath         string `json:"blobPath,omitempty"`
		SourceKind       string `json:"sourceKind,omitempty"`
		SourcePath       string `json:"sourcePath,omitempty"`
		AnchorRecord     *int   `json:"anchorRecord,omitempty"`
		AnchorOccurrence *int   `json:"anchorOccurrence,omitempty"`
		Retried          bool   `json:"retried,omitempty"`
	}
	var legacy struct {
		T     string       `json:"t"`
		Time  string       `json:"time"`
		Asset *legacyAsset `json:"asset,omitempty"`
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return err
	}
	stripped, err := json.Marshal(legacy)
	if err != nil {
		return err
	}
	var persisted sessionlog.Event
	if err := json.Unmarshal(stripped, &persisted); err != nil {
		return err
	}
	return (&sessionlog.Writer{Path: h.path}).Append(persisted)
}

func TestAssetImportRetryBindsEarlierReferenceAndIsIdempotent(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "n1", Title: "n1", Agent: "codex", Dir: t.TempDir()}
	a.nodes, a.byID[n.ID] = []*Node{n}, n
	outside := t.TempDir()
	path := filepath.Join(outside, "current.png")
	if err := os.WriteFile(path, []byte("original bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "![chart](" + path + ") and [same path](" + path + ")"}))
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{
		TurnRecord: 0, Occurrence: 0, Ref: path, Alt: "chart", Image: true, Reason: "outside_workspace",
	})))
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{
		TurnRecord: 0, Occurrence: 1, Ref: path, Alt: "same path", Reason: "outside_workspace",
	})))
	must(t, w.Append(sessionlog.Event{T: "source"}))
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "a later [same path](" + path + ")"}))
	if err := os.WriteFile(path, []byte("current bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileForSettingsTest(a.settingsPath, `{"allow_external_attachments":true}`); err != nil {
		t.Fatal(err)
	}

	h := newTestHandler(t, a)
	request := func(turn, occurrence int) (int, map[string]any) {
		body := `{"turn_record":` + jsonNumber(turn) + `,"occurrence":` + jsonNumber(occurrence) + `}`
		rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/asset-imports/retry", body, true)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	code, first := request(0, 0)
	if code != http.StatusOK || first["status"] != "imported" {
		t.Fatalf("first retry = %d %#v, want imported", code, first)
	}
	anchored := sessionlog.ReadAnchoredAssets(a.sessionLogPath(n.ID))
	if len(anchored) != 1 || anchored[0].Anchor != 0 || anchored[0].Asset.AnchorOccurrence == nil ||
		*anchored[0].Asset.AnchorOccurrence != 0 || !anchored[0].Asset.Retried || anchored[0].Asset.Name != "current.png" {
		t.Fatalf("retry binding = %+v, want explicit earlier-turn binding", anchored)
	}
	bytes, err := base64.StdEncoding.DecodeString(anchored[0].Asset.Bytes)
	if err != nil || string(bytes) != "current bytes" {
		t.Fatalf("retried bytes = %q, %v; want current contents", bytes, err)
	}
	turns := sessionlog.ReadHistory(a.sessionLogPath(n.ID))[0].Turns
	projected, _ := a.projectTurns(n.ID, turns)
	if len(projected) != 1 || strings.Count(projected[0].Text, "scimux-asset:") != 1 ||
		!strings.Contains(projected[0].Text, "scimux-import:0:1:outside_workspace") {
		t.Fatalf("earlier turn projection changed unrelated occurrence: %+v", projected)
	}
	code, second := request(0, 0)
	if code != http.StatusOK || second["status"] != "already_imported" || second["asset_id"] != first["asset_id"] {
		t.Fatalf("second retry = %d %#v, first %#v", code, second, first)
	}
	if code, _ := request(99, 0); code != http.StatusNotFound {
		t.Fatalf("forged recorded reference = %d, want 404", code)
	}
}

func TestMissingImportReappearsOutsideWorkspace(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	must(t, os.MkdirAll(a.sessionsDir, 0o700))
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "report.md")
	must(t, os.WriteFile(outside, []byte("ready"), 0o600))
	linked := filepath.Join(workspace, "linked.md")
	must(t, os.Symlink(outside, linked))
	inside := filepath.Join(workspace, "inside.md")
	must(t, os.WriteFile(inside, []byte("ready"), 0o600))
	n := &Node{ID: "n1", Title: "n1", Agent: "codex", Dir: workspace}
	a.nodes, a.byID[n.ID] = []*Node{n}, n
	w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "see [outside](" + outside + ") and [linked](linked.md) and [inside](inside.md)"}))
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{TurnRecord: 0, Occurrence: 0, Ref: outside, Reason: "not_found"})))
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{TurnRecord: 0, Occurrence: 1, Ref: "linked.md", Reason: "not_found"})))
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{TurnRecord: 0, Occurrence: 2, Ref: "inside.md", Reason: "not_found"})))
	project := func() string {
		turns := sessionlog.ReadHistory(a.sessionLogPath(n.ID))[0].Turns
		out, _ := a.projectTurns(n.ID, turns)
		return out[0].Text
	}
	if got := project(); strings.Count(got, "retry_ready") != 1 || strings.Count(got, "outside_workspace") != 2 {
		t.Fatalf("external attachments off = %q, want one retryable and two policy-blocked refs", got)
	}
	must(t, writeFileForSettingsTest(a.settingsPath, `{"allow_external_attachments":true}`))
	if got := project(); strings.Count(got, "retry_ready") != 3 {
		t.Fatalf("external attachments on = %q, want three retryable refs", got)
	}
}

func TestAssetImportRetryDedupPreservesRetriedFilename(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	must(t, os.MkdirAll(a.sessionsDir, 0o700))
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.txt")
	secondPath := filepath.Join(dir, "second.md")
	data := []byte("same retry bytes")
	must(t, os.WriteFile(firstPath, data, 0o600))
	must(t, os.WriteFile(secondPath, data, 0o600))
	n := &Node{ID: "n1", Title: "n1", Agent: "codex", Dir: t.TempDir()}
	a.nodes, a.byID[n.ID] = []*Node{n}, n
	w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "[first](" + firstPath + ")"}))
	first, err := a.ingestAssetBytes(n.ID, "first.txt", "", "agent_path", firstPath, data)
	must(t, err)
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "[second](" + secondPath + ")"}))
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{
		TurnRecord: 2, Occurrence: 0, Ref: secondPath, Alt: "second", Reason: "outside_workspace",
	})))
	must(t, writeFileForSettingsTest(a.settingsPath, `{"allow_external_attachments":true}`))

	rec := routeRequest(newTestHandler(t, a), http.MethodPost, "/api/nodes/n1/asset-imports/retry", `{"turn_record":2,"occurrence":0}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body.String())
	}
	var response map[string]any
	must(t, json.Unmarshal(rec.Body.Bytes(), &response))
	if response["name"] != "second.md" {
		t.Fatalf("retry response name = %q, want second.md", response["name"])
	}
	secondID, _ := response["asset_id"].(string)
	if secondID == "" || secondID == first.ID {
		t.Fatalf("retry identity = %q, original = %q", secondID, first.ID)
	}
	second := sessionlog.ReadAssets(a.sessionLogPath(n.ID))[secondID]
	if second.Name != "second.md" || second.Mime != "text/markdown; charset=utf-8" {
		t.Fatalf("retry metadata = %q, %q", second.Name, second.Mime)
	}
	if second.BackingID != first.ID || second.Bytes != "" || second.BlobPath != "" {
		t.Fatalf("retry did not reuse content backing: first=%+v second=%+v", first, second)
	}
}

func TestAssetImportRetryFallsBackForOlderWorkerAndDownloadsBytes(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	must(t, os.MkdirAll(a.sessionsDir, 0o700))
	outside := t.TempDir()
	firstPath := filepath.Join(outside, "first.txt")
	secondPath := filepath.Join(outside, "second.md")
	data := []byte("bytes shared across the update")
	must(t, os.WriteFile(firstPath, data, 0o600))
	must(t, os.WriteFile(secondPath, data, 0o600))
	n := &Node{ID: "n1", Title: "n1", Agent: "codex", Dir: t.TempDir()}
	a.nodes, a.byID[n.ID] = []*Node{n}, n
	w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "[first](" + firstPath + ")"}))
	_, err := a.ingestAssetBytes(n.ID, "first.txt", "", "agent_path", firstPath, data)
	must(t, err)
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "[second](" + secondPath + ")"}))
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{
		TurnRecord: 2, Occurrence: 0, Ref: secondPath, Alt: "second", Reason: "outside_workspace",
	})))
	must(t, writeFileForSettingsTest(a.settingsPath, `{"allow_external_attachments":true}`))

	identity := sessionworker.Identity{WorkerID: "worker-old", Agent: "codex", Build: "previous"}
	legacy := &legacyAssetWorkerHarness{path: a.sessionLogPath(n.ID)}
	server, err := sessionworker.Listen("", identity, legacy)
	must(t, err)
	t.Cleanup(func() { _ = server.Close() })
	client, err := sessionworker.NewClient(server.Link())
	must(t, err)
	t.Cleanup(func() { _ = client.Close() })
	// The reconnect handshake recorded no asset-backing capability, as a
	// worker from the previous release would advertise.
	a.workers = &workerManager{entries: map[string]*workerEntry{
		n.ID: {client: client, identity: identity, capabilities: map[string]bool{}},
	}, dataDir: t.TempDir()}

	rec := routeRequest(newTestHandler(t, a), http.MethodPost, "/api/nodes/n1/asset-imports/retry", `{"turn_record":2,"occurrence":0}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry through old worker = %d %s", rec.Code, rec.Body.String())
	}
	var response map[string]any
	must(t, json.Unmarshal(rec.Body.Bytes(), &response))
	id, _ := response["asset_id"].(string)
	persisted := sessionlog.ReadAssets(a.sessionLogPath(n.ID))[id]
	if persisted.BackingID != "" || (persisted.Bytes == "" && persisted.BlobPath == "") {
		t.Fatalf("old-worker fallback was not self-contained: %+v", persisted)
	}
	download := serveAsset(a, n.ID, id)
	if download.Code != http.StatusOK || download.Body.String() != string(data) {
		t.Fatalf("download after old-worker write = %d %q", download.Code, download.Body.String())
	}
}

func TestAssetImportRetryFailsClosedWhenSettingOff(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "n1", Title: "n1", Agent: "codex", Dir: t.TempDir()}
	a.nodes, a.byID[n.ID] = []*Node{n}, n
	w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "[file](/outside/file.txt)"}))
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{
		TurnRecord: 0, Occurrence: 0, Ref: "/outside/file.txt", Alt: "file", Reason: "outside_workspace",
	})))
	h := newTestHandler(t, a)
	rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/asset-imports/retry", `{"turn_record":0,"occurrence":0}`, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("setting-off retry = %d body %q, want 409", rec.Code, rec.Body.String())
	}
}

func TestAssetImportRetryRequiresRequestSecurity(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	h := newTestHandler(t, a)
	rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/asset-imports/retry", `{"turn_record":0,"occurrence":0}`, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("retry without request security = %d, want 403", rec.Code)
	}
}

func TestAssetImportRetryRejectsClientPathAndTrailingJSON(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.nodes = []*Node{{ID: "n1"}}
	a.byID["n1"] = a.nodes[0]
	h := newTestHandler(t, a)
	for _, body := range []string{
		`{"turn_record":0,"occurrence":0,"path":"/etc/passwd"}`,
		`{"turn_record":0,"occurrence":0}{"turn_record":1,"occurrence":0}`,
	} {
		if rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/asset-imports/retry", body, true); rec.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", body, rec.Code)
		}
	}
}

func TestAssetImportRetryValidatesNodeBodyAndRecordedTurn(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	must(t, os.MkdirAll(a.sessionsDir, 0o700))
	h := newTestHandler(t, a)
	if rec := routeRequest(h, http.MethodPost, "/api/nodes/missing/asset-imports/retry", `{"turn_record":0,"occurrence":0}`, true); rec.Code != http.StatusNotFound {
		t.Fatalf("missing node = %d", rec.Code)
	}
	n := &Node{ID: "n1", Dir: t.TempDir()}
	a.nodes, a.byID[n.ID] = []*Node{n}, n
	for _, body := range []string{``, `{}`, `{"turn_record":-1,"occurrence":0}`, `{"turn_record":0,"occurrence":-1}`} {
		if rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/asset-imports/retry", body, true); rec.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", body, rec.Code)
		}
	}
	w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "no recorded file"}))
	if rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/asset-imports/retry", `{"turn_record":0,"occurrence":0}`, true); rec.Code != http.StatusNotFound {
		t.Fatalf("missing import record = %d", rec.Code)
	}
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{TurnRecord: 1, Occurrence: 0, Ref: "file.txt", Alt: "file"})))
	if rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/asset-imports/retry", `{"turn_record":1,"occurrence":0}`, true); rec.Code != http.StatusNotFound {
		t.Fatalf("non-turn record = %d", rec.Code)
	}
}

func TestAssetImportRetryReportsStorageAppendFailure(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	must(t, os.MkdirAll(a.sessionsDir, 0o700))
	dir := t.TempDir()
	path := filepath.Join(dir, "retry.txt")
	must(t, os.WriteFile(path, []byte("retry"), 0o600))
	n := &Node{ID: "n1", Title: "n1", Agent: "codex", Dir: t.TempDir()}
	a.nodes, a.byID[n.ID] = []*Node{n}, n
	w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "[retry](" + path + ")"}))
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{TurnRecord: 0, Occurrence: 0, Ref: path, Alt: "retry", Reason: "outside_workspace"})))
	must(t, writeFileForSettingsTest(a.settingsPath, `{"allow_external_attachments":true}`))
	attachSyntheticWorker(t, a, n.ID, &syntheticSessionHarness{launched: true, appendErr: errors.New("append refused")})

	rec := routeRequest(newTestHandler(t, a), http.MethodPost, "/api/nodes/n1/asset-imports/retry", `{"turn_record":0,"occurrence":0}`, true)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"reason":"storage"`) {
		t.Fatalf("append failure = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAssetImportRetryConcurrentClicksAppendOneBinding(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "n1", Title: "n1", Agent: "codex", Dir: t.TempDir()}
	a.nodes, a.byID[n.ID] = []*Node{n}, n
	outside := t.TempDir()
	path := filepath.Join(outside, "report.txt")
	if err := os.WriteFile(path, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "[report](" + path + ")"}))
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{TurnRecord: 0, Occurrence: 0, Ref: path, Alt: "report", Reason: "outside_workspace"})))
	must(t, writeFileForSettingsTest(a.settingsPath, `{"allow_external_attachments":true}`))
	h := newTestHandler(t, a)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- routeRequest(h, http.MethodPost, "/api/nodes/n1/asset-imports/retry", `{"turn_record":0,"occurrence":0}`, true).Code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("concurrent retry status = %d", code)
		}
	}
	if got := len(sessionlog.ReadAnchoredAssets(a.sessionLogPath(n.ID))); got != 1 {
		t.Fatalf("concurrent retries appended %d bindings, want 1", got)
	}
}

func TestAssetImportRetryUsesOwningWorkerForSessionWrite(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "n1", Title: "n1", Agent: "opencode", Dir: t.TempDir()}
	a.nodes, a.byID[n.ID] = []*Node{n}, n
	outside := t.TempDir()
	path := filepath.Join(outside, "worker.txt")
	must(t, os.WriteFile(path, []byte("worker-owned"), 0o600))
	w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
	must(t, w.Append(sessionlog.Event{T: "assistant", Text: "[worker](" + path + ")"}))
	must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{TurnRecord: 0, Occurrence: 0, Ref: path, Alt: "worker", Reason: "outside_workspace"})))
	must(t, writeFileForSettingsTest(a.settingsPath, `{"allow_external_attachments":true}`))
	harness := &syntheticSessionHarness{launched: true}
	attachSyntheticWorker(t, a, n.ID, harness)
	rec := routeRequest(newTestHandler(t, a), http.MethodPost, "/api/nodes/n1/asset-imports/retry", `{"turn_record":0,"occurrence":0}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body.String())
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if len(harness.events) != 1 || harness.events[0].Asset == nil || harness.events[0].Asset.AnchorOccurrence == nil {
		t.Fatalf("worker events = %#v", harness.events)
	}
	if got := len(sessionlog.ReadEvents(a.sessionLogPath(n.ID))); got != 2 {
		t.Fatalf("muxer bypassed worker and appended locally: %d records", got)
	}
}

func TestAssetImportRetryFailureResponses(t *testing.T) {
	for reason, message := range map[string]string{
		"not_found": "File no longer exists", "unreadable": "File cannot be read",
		"not_regular": "Unsupported filesystem object", "too_large": "File exceeds the import limit",
		"storage": "Storage budget", "future": "Attachment could not be imported",
	} {
		rec := httptest.NewRecorder()
		retryFailure(rec, reason)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), message) {
			t.Errorf("%s = %d %q", reason, rec.Code, rec.Body.String())
		}
	}
}

func TestAssetImportRetryUnavailableMismatchAndFilesystemFailures(t *testing.T) {
	makeRetry := func(t *testing.T, path string) (*app, http.Handler) {
		t.Helper()
		a := newTestApp(t, &fakeTmux{})
		must(t, os.MkdirAll(a.sessionsDir, 0o700))
		n := &Node{ID: "n1", Title: "n1", Agent: "codex", Dir: t.TempDir()}
		a.nodes, a.byID[n.ID] = []*Node{n}, n
		w := &sessionlog.Writer{Path: a.sessionLogPath(n.ID)}
		must(t, w.Append(sessionlog.Event{T: "assistant", Text: "[file](" + path + ")"}))
		must(t, w.Append(sessionlog.NewAssetImport(sessionlog.AssetImportEvent{TurnRecord: 0, Occurrence: 0, Ref: path, Alt: "file", Reason: "outside_workspace"})))
		must(t, writeFileForSettingsTest(a.settingsPath, `{"allow_external_attachments":true}`))
		return a, newTestHandler(t, a)
	}

	t.Run("unavailable", func(t *testing.T) {
		a, h := makeRetry(t, filepath.Join(t.TempDir(), "missing"))
		a.byID["n1"].EndedAt = "now"
		if rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/asset-imports/retry", `{"turn_record":0,"occurrence":0}`, true); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "chat_unavailable") {
			t.Fatalf("unavailable = %d %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("forged persisted reference", func(t *testing.T) {
		a, h := makeRetry(t, "/outside/original.txt")
		events := sessionlog.ReadEvents(a.sessionLogPath("n1"))
		events[1].AssetImport.Ref = "/outside/forged.txt"
		path := a.sessionLogPath("n1")
		must(t, os.Remove(path))
		w := &sessionlog.Writer{Path: path}
		for _, event := range events {
			must(t, w.Append(event))
		}
		if rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/asset-imports/retry", `{"turn_record":0,"occurrence":0}`, true); rec.Code != http.StatusNotFound {
			t.Fatalf("forged persisted ref = %d", rec.Code)
		}
	})
	t.Run("missing", func(t *testing.T) {
		_, h := makeRetry(t, filepath.Join(t.TempDir(), "missing.txt"))
		if rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/asset-imports/retry", `{"turn_record":0,"occurrence":0}`, true); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not_found") {
			t.Fatalf("missing = %d %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("nonregular", func(t *testing.T) {
		dir := t.TempDir()
		_, h := makeRetry(t, dir)
		if rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/asset-imports/retry", `{"turn_record":0,"occurrence":0}`, true); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not_regular") {
			t.Fatalf("nonregular = %d %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "large.bin")
		must(t, os.WriteFile(path, nil, 0o600))
		must(t, os.Truncate(path, agentAssetMaxBytes+1))
		_, h := makeRetry(t, path)
		if rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/asset-imports/retry", `{"turn_record":0,"occurrence":0}`, true); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "too_large") {
			t.Fatalf("oversized = %d %q", rec.Code, rec.Body.String())
		}
	})
}

func jsonNumber(v int) string {
	b, _ := json.Marshal(v)
	return string(b)
}
